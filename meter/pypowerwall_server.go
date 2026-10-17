package meter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/api/implement"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/request"
)

// PyPowerwallServer is a Tesla Powerwall meter using pypowerwall-server as backend.
//
// Reads come from pypowerwall-server's proxy-compatible endpoints, writes go to
// its authenticated /control/* routes. BatteryHold uses POST
// /control/reserve_hold (reserve = cached SoC, server-side and race-free)
// instead of a client-side read followed by a reserve write. Holding
// deliberately freezes at the actual state of charge, capped at Tesla's 80%
// reserve limit and without clamping to MinSoc the way the fleet meter's
// soc-limit controller does.
type PyPowerwallServer struct {
	implement.Caps
	usage  string
	base   string
	token  string
	client *http.Client
	log    *util.Logger
	meterG func() (pwsAggregates, error)
	opG    func() (pwsOperation, error)
	minSoc float64
	maxSoc float64
}

// pwsAggregates mirrors pypowerwall-server's /api/meters/aggregates sections
// (site/solar/battery/load), same shape as the Tesla gateway API.
// Lifetime energy is in Wh, like the gateway reports it. Fields the server
// cannot provide are null; such readings error instead of silently becoming
// zero.
type pwsAggregates map[string]struct {
	InstantPower   *float64 `json:"instant_power"`
	EnergyImported *float64 `json:"energy_imported"`
	EnergyExported *float64 `json:"energy_exported"`
}

type pwsSoe struct {
	Percentage *float64 `json:"percentage"`
}

type pwsOperation struct {
	BackupReservePercent *float64 `json:"backup_reserve_percent"`
}

// pwsSystemStatus values are optional: zero (also when the server reports
// null) means no reading, keeping the corresponding capability unregistered.
type pwsSystemStatus struct {
	NominalFullPackEnergy      float64 `json:"nominal_full_pack_energy"`
	MaxApparentPower           float64 `json:"max_apparent_power"`
	InverterNominalUsablePower float64 `json:"inverter_nominal_usable_power"`
}

type pyPowerwallServerConfig struct {
	URI, Usage, Token  string
	Cache              time.Duration
	MinSoc, MaxSoc     float64
	batteryCapacity    `mapstructure:",squash"`
	batteryPowerLimits `mapstructure:",squash"`
}

func defaultPyPowerwallServerConfig() pyPowerwallServerConfig {
	return pyPowerwallServerConfig{
		Cache:  time.Second,
		MaxSoc: 100,
	}
}

// normalizeBase trims whitespace and a trailing slash. The template always
// renders a complete http://host:port URL; direct type users must provide
// a full base URL as well.
func normalizePyPowerwallServerBase(uri string) string {
	base := strings.TrimSpace(uri)
	return strings.TrimSuffix(base, "/")
}

// validate checks required parameters and maps legacy usage names
func (cc *pyPowerwallServerConfig) validate() error {
	if cc.Usage == "" {
		return errors.New("missing usage")
	}

	if cc.URI == "" {
		return errors.New("missing uri")
	}

	if !strings.Contains(cc.URI, "://") {
		return errors.New("uri must be a complete URL, e.g. http://192.0.2.2:8675")
	}

	// support default meter names
	switch strings.ToLower(cc.Usage) {
	case "grid":
		cc.Usage = "site"
	case "pv":
		cc.Usage = "solar"
	}

	return nil
}

func init() {
	registry.Add("pypowerwall-server", NewPyPowerwallServerFromConfig)
}

// NewPyPowerwallServerFromConfig creates a Powerwall meter backed by pypowerwall-server
func NewPyPowerwallServerFromConfig(other map[string]any) (api.Meter, error) {
	cc := defaultPyPowerwallServerConfig()
	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	if err := cc.validate(); err != nil {
		return nil, err
	}

	log := util.NewLogger("pypowerwall-server").Redact(cc.Token)

	m := &PyPowerwallServer{
		Caps:   implement.New(),
		usage:  strings.ToLower(cc.Usage),
		base:   normalizePyPowerwallServerBase(cc.URI),
		token:  cc.Token,
		log:    log,
		minSoc: cc.MinSoc,
		maxSoc: cc.MaxSoc,
		client: &http.Client{
			Transport: request.NewTripper(log, http.DefaultTransport),
			Timeout:   15 * time.Second, // control writes can take a few seconds
		},
	}
	m.meterG = util.Cached(m.getAggregates, cc.Cache)
	m.opG = util.Cached(m.getOperation, cc.Cache)

	// reading validates connectivity
	if _, err := m.meterG(); err != nil {
		return nil, err
	}

	if m.usage == "load" || m.usage == "solar" {
		implement.Has(m, implement.MeterEnergy(m.totalEnergy))
	}

	if m.usage == "battery" {
		implement.Has(m, implement.Battery(m.batterySoc))

		// Capacity and inverter rating never change: read once.
		// Explicitly configured values win, the server reading is the
		// fallback in both cases.
		var sys pwsSystemStatus
		sysErr := m.get("/api/system_status", &sys)
		if sysErr != nil {
			log.DEBUG.Println("battery system status:", sysErr)
		}

		if cc.Capacity > 0 {
			capacity := cc.Capacity
			implement.Has(m, implement.BatteryCapacity(func() (float64, error) {
				return capacity, nil
			}))
		} else if sysErr == nil && sys.NominalFullPackEnergy > 0 {
			capacity := sys.NominalFullPackEnergy / 1e3
			implement.Has(m, implement.BatteryCapacity(func() (float64, error) {
				return capacity, nil
			}))
		}

		charge, discharge := cc.MaxChargePower, cc.MaxDischargePower
		if sysErr == nil {
			// the smallest positive of nameplate and usable power applies to
			// charging and discharging alike: derated sites report usable
			// power below their nameplate
			limit := minPositive(sys.MaxApparentPower, sys.InverterNominalUsablePower)
			if charge == 0 {
				charge = limit
			}
			if discharge == 0 {
				discharge = limit
			}
		}
		if charge > 0 && discharge > 0 {
			implement.Has(m, implement.BatteryPowerLimiter(func() (float64, float64, error) {
				return charge, discharge, nil
			}))
		}

		// backup reserve is the lower discharge limit, the powerwall has no upper soc limit
		implement.Has(m, implement.BatterySocLimiter(func() (float64, float64, error) {
			op, err := m.opG()
			if err != nil {
				return 0, 0, err
			}
			return *op.BackupReservePercent, 100, nil
		}))

		if cc.Token == "" {
			log.WARN.Println("no token configured, battery control disabled (read-only)")
		} else {
			implement.Has(m, implement.BatteryController(batteryModesSocLimit, m.setBatteryMode))
		}
	}

	return m, nil
}

var _ api.Meter = (*PyPowerwallServer)(nil)

// get decodes a JSON GET response into v (short timeout: reads are cached
// server-side and must never stall the poll loop)
func (m *PyPowerwallServer) get(path string, v any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.base+path, nil)
	if err != nil {
		return err
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("get %s: %s", path, resp.Status)
	}

	return json.NewDecoder(resp.Body).Decode(v)
}

// post sends a JSON POST to a pypowerwall-server /control/* route.
// The control secret doubles as evcc's token and is sent as bearer token,
// the same scheme the server's console uses.
func (m *PyPowerwallServer) post(path string, body any) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, m.base+path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.token)

	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("post %s: %s", path, resp.Status)
	}

	return nil
}

func (m *PyPowerwallServer) getAggregates() (pwsAggregates, error) {
	var res pwsAggregates
	if err := m.get("/api/meters/aggregates", &res); err != nil {
		return nil, err
	}
	return res, nil
}

// finite rejects NaN and infinite readings from the server: invalid
// external numerics must error instead of poisoning regulation math.
func finite(label string, f float64) (float64, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("invalid %s reading: %v", label, f)
	}
	return f, nil
}

// minPositive returns the smallest positive value, or 0 when none is positive.
// Server readings use 0 for "not available", so it must not win here.
func minPositive(values ...float64) float64 {
	var min float64
	for _, v := range values {
		if v > 0 && (min == 0 || v < min) {
			min = v
		}
	}
	return min
}

// CurrentPower implements the api.Meter interface
func (m *PyPowerwallServer) CurrentPower() (float64, error) {
	res, err := m.meterG()
	if err != nil {
		return 0, err
	}

	if o, ok := res[m.usage]; ok {
		if o.InstantPower == nil {
			return 0, fmt.Errorf("server reports no power for %s", m.usage)
		}
		return finite("power", *o.InstantPower)
	}

	return 0, fmt.Errorf("invalid usage: %s", m.usage)
}

// totalEnergy implements the api.MeterEnergy interface
func (m *PyPowerwallServer) totalEnergy() (float64, error) {
	res, err := m.meterG()
	if err != nil {
		return 0, err
	}

	if o, ok := res[m.usage]; ok {
		switch m.usage {
		case "load":
			if o.EnergyImported == nil {
				return 0, fmt.Errorf("server reports no energy for %s", m.usage)
			}
			return finite("energy", *o.EnergyImported/1e3)
		case "solar":
			if o.EnergyExported == nil {
				return 0, fmt.Errorf("server reports no energy for %s", m.usage)
			}
			return finite("energy", *o.EnergyExported/1e3)
		}
	}

	return 0, fmt.Errorf("invalid usage: %s", m.usage)
}

// batterySoc implements the api.Battery interface
func (m *PyPowerwallServer) batterySoc() (float64, error) {
	var res pwsSoe
	if err := m.get("/api/system_status/soe", &res); err != nil {
		return 0, err
	}

	if res.Percentage == nil {
		return 0, errors.New("server reports no state of charge")
	}

	return finite("soc", *res.Percentage)
}

func (m *PyPowerwallServer) getOperation() (pwsOperation, error) {
	var res pwsOperation
	if err := m.get("/api/operation", &res); err != nil {
		return pwsOperation{}, err
	}

	if res.BackupReservePercent == nil {
		return pwsOperation{}, errors.New("server reports no backup reserve")
	}

	return res, nil
}

// setReserve writes an explicit backup reserve level (Tesla accepts up to
// 80 or exactly 100, like the fleet meter clamps it)
func (m *PyPowerwallServer) setReserve(limit float64) error {
	return m.post("/control/reserve", map[string]any{
		"value": int(teslaReserveLimit(limit)),
	})
}

// holdReserve freezes the battery at its current level via the server's
// atomic reserve_hold action (reserve = cached SoC, no client-side race).
// Deliberately no MinSoc clamp: hold means freezing the actual SoC, capped by
// Tesla's 80% reserve limit.
func (m *PyPowerwallServer) holdReserve() error {
	return m.post("/control/reserve_hold", map[string]any{})
}

// setBatteryMode implements the api.BatteryController interface
func (m *PyPowerwallServer) setBatteryMode(mode api.BatteryMode) error {
	switch mode {
	case api.BatteryNormal:
		return m.setReserve(m.minSoc)

	case api.BatteryHold:
		return m.holdReserve()

	case api.BatteryCharge:
		return m.setReserve(m.maxSoc)

	default:
		return errInvalidBatteryMode(mode)
	}
}
