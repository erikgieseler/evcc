package meter

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewPyPowerwallServerFromConfigValidation(t *testing.T) {
	tests := []struct {
		name   string
		config map[string]any
		want   string
	}{
		{
			name:   "missing usage",
			config: map[string]any{"uri": "http://localhost:8675"},
			want:   "missing usage",
		},
		{
			name:   "missing uri",
			config: map[string]any{"usage": "battery"},
			want:   "missing uri",
		},
		{
			name:   "uri without scheme",
			config: map[string]any{"usage": "battery", "uri": "localhost:8675"},
			want:   "complete URL",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewPyPowerwallServerFromConfig(tc.config)
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

func TestPyPowerwallServerNormalizeBase(t *testing.T) {
	assert.Equal(t, "http://h:8675", normalizePyPowerwallServerBase("http://h:8675"))
	assert.Equal(t, "http://h:8675", normalizePyPowerwallServerBase("http://h:8675/"))
	assert.Equal(t, "https://h/x", normalizePyPowerwallServerBase("https://h/x/"))
	assert.Equal(t, "https://h/x", normalizePyPowerwallServerBase("  https://h/x/  "))
}

// fakePyPowerwallServer is a minimal pypowerwall-server stub: proxy reads plus
// the authenticated control routes used by the meter.
type fakePyPowerwallServer struct {
	mu       sync.Mutex
	token    string
	reserve  float64
	held     int
	apparent *float64
}

func floatptr(f float64) *float64 { return &f }

func (f *fakePyPowerwallServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/meters/aggregates", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"battery": map[string]any{"instant_power": -1200},
			"load":    map[string]any{"instant_power": 2086.75, "energy_imported": 70058570.0},
			"solar":   map[string]any{"instant_power": 2243, "energy_exported": 80889089.0},
		})
	})
	mux.HandleFunc("/api/system_status/soe", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"percentage": 67.3})
	})
	mux.HandleFunc("/api/operation", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"backup_reserve_percent": f.reserve})
	})
	mux.HandleFunc("/api/system_status", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"nominal_full_pack_energy": 27000,
			"max_apparent_power":       f.apparent,
		})
	})
	for _, path := range []string{"/control/reserve", "/control/reserve_hold"} {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if r.Header.Get("Authorization") != "Bearer "+f.token {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if r.URL.Path == "/control/reserve" {
				v, ok := body["value"].(float64)
				if !ok {
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}
				f.reserve = v
			} else {
				f.held++
				f.reserve = 67
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"result": "Updated"})
		})
	}
	return mux
}

func newTestPyPowerwallServer(t *testing.T, fake *fakePyPowerwallServer) (*PyPowerwallServer, *httptest.Server) {
	t.Helper()

	srv := httptest.NewServer(fake.handler())

	m, err := NewPyPowerwallServerFromConfig(map[string]any{
		"uri":    srv.URL,
		"usage":  "battery",
		"token":  "secret",
		"minsoc": 20.0,
		"cache":  time.Millisecond,
	})
	require.NoError(t, err)

	pws, ok := m.(*PyPowerwallServer)
	require.True(t, ok, "expected *PyPowerwallServer")

	return pws, srv
}

func TestPyPowerwallServerReads(t *testing.T) {
	fake := &fakePyPowerwallServer{token: "secret", reserve: 20, apparent: floatptr(15000)}
	m, srv := newTestPyPowerwallServer(t, fake)
	defer srv.Close()

	power, err := m.CurrentPower()
	require.NoError(t, err)
	assert.Equal(t, -1200.0, power)

	soc, err := m.batterySoc()
	require.NoError(t, err)
	assert.Equal(t, 67.3, soc)
}

func TestPyPowerwallServerTotalEnergy(t *testing.T) {
	for usage, want := range map[string]float64{
		"load":  70058.570,
		"solar": 80889.089,
	} {
		srv := httptest.NewServer((&fakePyPowerwallServer{token: "secret", reserve: 20, apparent: floatptr(15000)}).handler())
		defer srv.Close()

		m, err := NewPyPowerwallServerFromConfig(map[string]any{
			"uri":   srv.URL,
			"usage": usage,
			"cache": time.Millisecond,
		})
		require.NoError(t, err)

		energy, ok := api.Cap[api.MeterEnergy](m)
		require.True(t, ok, "expected MeterEnergy for %s", usage)
		total, err := energy.TotalEnergy()
		require.NoError(t, err)
		assert.InDelta(t, want, total, 0.001)
	}
}

func TestPyPowerwallServerPowerLimits(t *testing.T) {
	newMeter := func(t *testing.T, fake *fakePyPowerwallServer, extra map[string]any) api.Meter {
		t.Helper()

		srv := httptest.NewServer(fake.handler())
		t.Cleanup(srv.Close)

		cfg := map[string]any{
			"uri":   srv.URL,
			"usage": "battery",
			"token": "secret",
			"cache": time.Millisecond,
		}
		for k, v := range extra {
			cfg[k] = v
		}

		m, err := NewPyPowerwallServerFromConfig(cfg)
		require.NoError(t, err)
		return m
	}

	limits := func(t *testing.T, m api.Meter) (float64, float64, bool) {
		t.Helper()

		lim, ok := api.Cap[api.BatteryPowerLimiter](m)
		if !ok {
			return 0, 0, false
		}
		charge, discharge := lim.GetPowerLimits()
		return charge, discharge, true
	}

	t.Run("explicit config wins over hardware", func(t *testing.T) {
		fake := &fakePyPowerwallServer{token: "secret", reserve: 20, apparent: floatptr(15000)}
		m := newMeter(t, fake, map[string]any{
			"maxchargepower": 9200.0, "maxdischargepower": 9200.0,
		})
		charge, discharge, ok := limits(t, m)
		require.True(t, ok, "expected BatteryPowerLimiter")
		assert.Equal(t, 9200.0, charge)
		assert.Equal(t, 9200.0, discharge)
	})

	t.Run("hardware fallback", func(t *testing.T) {
		fake := &fakePyPowerwallServer{token: "secret", reserve: 20, apparent: floatptr(15000)}
		m := newMeter(t, fake, nil)
		charge, discharge, ok := limits(t, m)
		require.True(t, ok, "expected BatteryPowerLimiter")
		assert.Equal(t, 15000.0, charge)
		assert.Equal(t, 15000.0, discharge)
	})

	t.Run("no limiter when hardware reports null", func(t *testing.T) {
		fake := &fakePyPowerwallServer{token: "secret", reserve: 20, apparent: nil}
		m := newMeter(t, fake, nil)
		_, _, ok := limits(t, m)
		assert.False(t, ok, "expected no BatteryPowerLimiter")
	})
}

func TestPyPowerwallServerRejectsNonFiniteReadings(t *testing.T) {
	// JSON cannot carry NaN/Inf (Go rejects such tokens on decode),
	// so the guard itself is unit-tested: absurd decoded values
	// must error instead of poisoning regulation math.
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		_, err := finite("test", bad)
		assert.Error(t, err)
	}

	got, err := finite("test", 67.3)
	require.NoError(t, err)
	assert.Equal(t, 67.3, got)
}

func TestPyPowerwallServerBatteryCapacity(t *testing.T) {
	fake := &fakePyPowerwallServer{token: "secret", reserve: 20, apparent: floatptr(15000)}
	m, srv := newTestPyPowerwallServer(t, fake)
	defer srv.Close()

	cap, ok := api.Cap[api.BatteryCapacity](m)
	require.True(t, ok, "expected BatteryCapacity")
	assert.Equal(t, 27.0, cap.Capacity())

	lim, ok := api.Cap[api.BatteryPowerLimiter](m)
	require.True(t, ok, "expected BatteryPowerLimiter")
	charge, discharge := lim.GetPowerLimits()
	assert.Equal(t, 15000.0, charge)
	assert.Equal(t, 15000.0, discharge)
}

func TestPyPowerwallServerExplicitCapacityWins(t *testing.T) {
	fake := &fakePyPowerwallServer{token: "secret", reserve: 20, apparent: floatptr(15000)}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	m, err := NewPyPowerwallServerFromConfig(map[string]any{
		"uri":      srv.URL,
		"usage":    "battery",
		"token":    "secret",
		"cache":    time.Millisecond,
		"capacity": 30.0,
	})
	require.NoError(t, err)

	cap, ok := api.Cap[api.BatteryCapacity](m)
	require.True(t, ok, "expected BatteryCapacity")
	assert.Equal(t, 30.0, cap.Capacity())
}

func TestPyPowerwallServerBatteryModes(t *testing.T) {
	fake := &fakePyPowerwallServer{token: "secret", reserve: 20, apparent: floatptr(15000)}
	m, srv := newTestPyPowerwallServer(t, fake)
	defer srv.Close()

	ctrl, ok := api.Cap[api.BatteryController](m)
	require.True(t, ok, "expected BatteryController")

	// normal restores the configured minimum
	require.NoError(t, ctrl.SetBatteryMode(api.BatteryNormal))
	assert.Equal(t, 20.0, fake.reserve)

	// hold freezes at the current level via the atomic server action
	require.NoError(t, ctrl.SetBatteryMode(api.BatteryHold))
	assert.Equal(t, 1, fake.held)
	assert.Equal(t, 67.0, fake.reserve)

	// charge raises to the configured maximum
	require.NoError(t, ctrl.SetBatteryMode(api.BatteryCharge))
	assert.Equal(t, 100.0, fake.reserve)

	assert.Error(t, ctrl.SetBatteryMode(api.BatteryMode(42)))
}

func TestPyPowerwallServerControlAuth(t *testing.T) {
	fake := &fakePyPowerwallServer{token: "secret", reserve: 20, apparent: floatptr(15000)}
	m, srv := newTestPyPowerwallServer(t, fake)
	defer srv.Close()

	m.token = "wrong"
	assert.Error(t, m.setReserve(30))
}

func TestPyPowerwallServerReadOnlyWithoutToken(t *testing.T) {
	srv := httptest.NewServer((&fakePyPowerwallServer{reserve: 20}).handler())
	defer srv.Close()

	m, err := NewPyPowerwallServerFromConfig(map[string]any{
		"uri":   srv.URL,
		"usage": "battery",
		"cache": time.Millisecond,
	})
	require.NoError(t, err)

	_, ok := api.Cap[api.BatteryController](m)
	assert.False(t, ok, "no token must mean no battery control")

	soc, err := m.(*PyPowerwallServer).batterySoc()
	require.NoError(t, err)
	assert.Equal(t, 67.3, soc)
}

// decode helper mirrors the decoding done by the meter constructor
func decodePyPowerwallServerConfig(t *testing.T, other map[string]any) pyPowerwallServerConfig {
	t.Helper()

	cc := defaultPyPowerwallServerConfig()
	require.NoError(t, util.DecodeOther(other, &cc))
	require.NoError(t, cc.validate())

	return cc
}

func TestPyPowerwallServerConfigDefaults(t *testing.T) {
	cc := decodePyPowerwallServerConfig(t, map[string]any{
		"uri": "http://localhost:8675", "usage": "battery",
	})
	assert.Equal(t, 0.0, cc.MinSoc)
	assert.Equal(t, 100.0, cc.MaxSoc)
	assert.Equal(t, "http://localhost:8675", normalizePyPowerwallServerBase(cc.URI))
}
