package config

import (
	"os"
	"testing"
)

// setEnv sets the given env vars and returns a cleanup that restores the
// prior state. Tests must not leak env mutations into each other.
func setEnv(t *testing.T, vars map[string]string) {
	t.Helper()
	saved := map[string]string{}
	for k, v := range vars {
		saved[k] = os.Getenv(k)
		if v == "" {
			os.Unsetenv(k)
		} else {
			os.Setenv(k, v)
		}
	}
	t.Cleanup(func() {
		for k, v := range saved {
			if v == "" && !wasPresent(saved, k) {
				os.Unsetenv(k)
			} else {
				os.Setenv(k, v)
			}
		}
	})
}

// wasPresent reports whether a key was explicitly saved (even as "").
func wasPresent(saved map[string]string, k string) bool {
	_, ok := saved[k]
	return ok
}

// clearEnv unsets all config-related env vars for a clean slate.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"DATABASE_URL", "GATEWAY_API_KEY", "PORT", "DEBUG_DB_MAX_RECORDS",
		"UPSTREAM_DEFAULT_FIRST_BYTE_TIMEOUT_MS", "UPSTREAM_STREAM_FIRST_BYTE_TIMEOUT_MS",
		"UPSTREAM_IMAGE_FIRST_BYTE_TIMEOUT_MS", "UPSTREAM_REQUEST_TIMEOUT_MS",
		"UPSTREAM_RESPONSE_IDLE_TIMEOUT_MS", "TEST_DATABASE_URL",
	} {
		orig := os.Getenv(k)
		os.Unsetenv(k)
		t.Cleanup(func() { os.Setenv(k, orig) })
	}
}

func TestLoad_RequiresDatabaseURLAndGatewayKey(t *testing.T) {
	clearEnv(t)
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing required vars")
	}
	if !contains(err.Error(), "DATABASE_URL") || !contains(err.Error(), "GATEWAY_API_KEY") {
		t.Errorf("error should name both missing vars, got: %v", err)
	}
}

func TestLoad_Defaults(t *testing.T) {
	clearEnv(t)
	setEnv(t, map[string]string{
		"DATABASE_URL":    "postgresql://u:p@localhost:5432/db",
		"GATEWAY_API_KEY": "key",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != DefaultPort {
		t.Errorf("Port: got %d, want %d", cfg.Port, DefaultPort)
	}
	if cfg.DebugDBMaxRecords != DefaultDebugDBMaxRecords {
		t.Errorf("DebugDBMaxRecords: got %d", cfg.DebugDBMaxRecords)
	}
	want := CodeDefaultTimeouts()
	if cfg.Timeouts != want {
		t.Errorf("Timeouts: got %+v, want %+v", cfg.Timeouts, want)
	}
}

func TestLoad_UsesEnvTimeouts(t *testing.T) {
	clearEnv(t)
	setEnv(t, map[string]string{
		"DATABASE_URL":                            "postgresql://u:p@localhost:5432/db",
		"GATEWAY_API_KEY":                         "key",
		"UPSTREAM_DEFAULT_FIRST_BYTE_TIMEOUT_MS":  "120000",
		"UPSTREAM_STREAM_FIRST_BYTE_TIMEOUT_MS":   "15000",
		"UPSTREAM_IMAGE_FIRST_BYTE_TIMEOUT_MS":    "60000",
		"UPSTREAM_RESPONSE_IDLE_TIMEOUT_MS":       "0",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timeouts.DefaultFirstByteMs != 120000 {
		t.Errorf("DefaultFirstByteMs: %d", cfg.Timeouts.DefaultFirstByteMs)
	}
	if cfg.Timeouts.StreamFirstByteMs != 15000 {
		t.Errorf("StreamFirstByteMs: %d", cfg.Timeouts.StreamFirstByteMs)
	}
	if cfg.Timeouts.ImageFirstByteMs != 60000 {
		t.Errorf("ImageFirstByteMs: %d", cfg.Timeouts.ImageFirstByteMs)
	}
	if cfg.Timeouts.ResponseIdleMs != 0 {
		t.Errorf("ResponseIdleMs: %d (0 disables)", cfg.Timeouts.ResponseIdleMs)
	}
}

// TestLoad_LegacyFallback verifies the UPSTREAM_REQUEST_TIMEOUT_MS legacy
// fallback applies when the specific first-byte envs are unset.
func TestLoad_LegacyFallback(t *testing.T) {
	clearEnv(t)
	setEnv(t, map[string]string{
		"DATABASE_URL":                 "postgresql://u:p@localhost:5432/db",
		"GATEWAY_API_KEY":              "key",
		"UPSTREAM_REQUEST_TIMEOUT_MS": "120000",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	// All three first-byte fields should pick up the legacy value.
	if cfg.Timeouts.DefaultFirstByteMs != 120000 || cfg.Timeouts.StreamFirstByteMs != 120000 || cfg.Timeouts.ImageFirstByteMs != 120000 {
		t.Errorf("legacy fallback failed: %+v", cfg.Timeouts)
	}
	// Specific env beats legacy.
	setEnv(t, map[string]string{
		"UPSTREAM_STREAM_FIRST_BYTE_TIMEOUT_MS": "9000",
	})
	cfg2, _ := Load()
	if cfg2.Timeouts.StreamFirstByteMs != 9000 {
		t.Errorf("specific should beat legacy: %d", cfg2.Timeouts.StreamFirstByteMs)
	}
	if cfg2.Timeouts.DefaultFirstByteMs != 120000 {
		t.Errorf("default should still use legacy: %d", cfg2.Timeouts.DefaultFirstByteMs)
	}
}

func TestLoad_SpecificBeatsLegacy(t *testing.T) {
	clearEnv(t)
	setEnv(t, map[string]string{
		"DATABASE_URL":                            "postgresql://u:p@localhost:5432/db",
		"GATEWAY_API_KEY":                         "key",
		"UPSTREAM_REQUEST_TIMEOUT_MS":             "100000",
		"UPSTREAM_DEFAULT_FIRST_BYTE_TIMEOUT_MS":  "50000",
	})
	cfg, _ := Load()
	if cfg.Timeouts.DefaultFirstByteMs != 50000 {
		t.Errorf("specific (50000) should beat legacy (100000): %d", cfg.Timeouts.DefaultFirstByteMs)
	}
}

func TestLoad_RejectsInvalidPort(t *testing.T) {
	clearEnv(t)
	setEnv(t, map[string]string{
		"DATABASE_URL":    "postgresql://u:p@localhost:5432/db",
		"GATEWAY_API_KEY": "key",
		"PORT":            "not-a-number",
	})
	if _, err := Load(); err == nil {
		t.Fatal("expected error for non-numeric PORT")
	}
}

func TestLoad_RejectsTooSmallDebugMax(t *testing.T) {
	clearEnv(t)
	setEnv(t, map[string]string{
		"DATABASE_URL":        "postgresql://u:p@localhost:5432/db",
		"GATEWAY_API_KEY":     "key",
		"DEBUG_DB_MAX_RECORDS": "10", // below min (200)
	})
	if _, err := Load(); err == nil {
		t.Fatal("expected error for DEBUG_DB_MAX_RECORDS below minimum")
	}
}

func TestLoad_RejectsOutOfRangeTimeout(t *testing.T) {
	clearEnv(t)
	setEnv(t, map[string]string{
		"DATABASE_URL":                           "postgresql://u:p@localhost:5432/db",
		"GATEWAY_API_KEY":                        "key",
		"UPSTREAM_DEFAULT_FIRST_BYTE_TIMEOUT_MS": "100", // below 1000 min
	})
	if _, err := Load(); err == nil {
		t.Fatal("expected error for out-of-range first-byte timeout")
	}
}

func TestResponseStreamLogMaxDurationMs_DefaultAndOverride(t *testing.T) {
	clearEnv(t)
	if got := ResponseStreamLogMaxDurationMs(); got != DefaultResponseStreamLogMaxDurMs {
		t.Errorf("default: got %d", got)
	}
	setEnv(t, map[string]string{"CONSOLE_STREAM_LOG_MAX_DURATION_MS": "5000"})
	if got := ResponseStreamLogMaxDurationMs(); got != 5000 {
		t.Errorf("override: got %d", got)
	}
	setEnv(t, map[string]string{"CONSOLE_STREAM_LOG_MAX_DURATION_MS": "10"}) // below min
	if got := ResponseStreamLogMaxDurationMs(); got != MinResponseStreamLogMaxDurMs {
		t.Errorf("below-min should clamp: got %d", got)
	}
}

func TestLoadForTest(t *testing.T) {
	cfg := LoadForTest(WithGatewayKey("k"), WithDatabaseURL("u"))
	if cfg.GatewayAPIKey != "k" || cfg.DatabaseURL != "u" {
		t.Errorf("options not applied: %+v", cfg)
	}
	if cfg.Timeouts.DefaultFirstByteMs != DefaultDefaultFirstByteTimeoutMs {
		t.Errorf("test config should use code defaults")
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
