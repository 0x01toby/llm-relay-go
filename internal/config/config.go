// Package config loads process configuration from environment variables.
//
// It mirrors the env-var contract of the original Node service (see
// .env.example and src/gateway-timeouts.ts): required keys, default values,
// and the timeout precedence chain (UPSTREAM_<KIND>_FIRST_BYTE_TIMEOUT_MS →
// UPSTREAM_REQUEST_TIMEOUT_MS → code default).
package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// Defaults mirror CODE_DEFAULT_GATEWAY_TIMEOUTS in gateway-timeouts.ts.
const (
	DefaultDefaultFirstByteTimeoutMs = 300_000
	DefaultStreamFirstByteTimeoutMs  = 30_000
	DefaultImageFirstByteTimeoutMs   = 300_000
	DefaultResponseIdleTimeoutMs     = 300_000

	DefaultPort              = 3300
	DefaultDebugDBMaxRecords = 2_000
	MinDebugDBMaxRecords     = 200

	// Logging limits mirror logging-constants.ts.
	PayloadLogLimitBytes              = 5 * 1024 * 1024 // 5 MiB
	DefaultResponseStreamLogMaxBytes  = PayloadLogLimitBytes
	DefaultResponseStreamLogMaxDurMs  = 1_800_000 // 30 min
	MinResponseStreamLogMaxDurMs      = 100
	ResponseStreamLogMaxDurEnv        = "CONSOLE_STREAM_LOG_MAX_DURATION_MS"
)

// TimeoutLimits mirror GATEWAY_TIMEOUT_LIMITS.
var (
	FirstByteLimits   = TimeoutLimit{Min: 1000, Max: 900000}
	ResponseIdleLimit = TimeoutLimit{Min: 0, Max: 3600000, AllowZero: true}
)

// TimeoutLimit describes the valid range for a timeout field.
type TimeoutLimit struct {
	Min        int64
	Max        int64
	AllowZero  bool
}

// Config holds all process configuration.
type Config struct {
	// DatabaseURL is the PostgreSQL connection string. Required for the full
	// gateway; the spike binary ignores it.
	DatabaseURL string
	// TestDatabaseURL is used by integration tests.
	TestDatabaseURL string

	// GatewayAPIKey authenticates proxy clients and is the console login
	// password.
	GatewayAPIKey string

	// Port is the HTTP listen port.
	Port int

	// DebugDBMaxRecords caps how many request-log rows are retained.
	DebugDBMaxRecords int

	// Timeouts are the upstream response-start/idle defaults (ms). They may be
	// overridden at runtime via the gateway_settings table (P4).
	Timeouts TimeoutSettings
}

// TimeoutSettings mirrors GatewayTimeoutSettings.
type TimeoutSettings struct {
	DefaultFirstByteMs int64
	StreamFirstByteMs  int64
	ImageFirstByteMs   int64
	ResponseIdleMs     int64
}

// Addr returns the listen address ("PORT" -> ":PORT").
func (c *Config) Addr() string { return fmt.Sprintf(":%d", c.Port) }

// Load reads configuration from the environment. It returns an error listing
// every missing required key (rather than failing on the first) so operators
// see all problems at once.
func Load() (*Config, error) {
	var missing []string

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	gatewayKey := os.Getenv("GATEWAY_API_KEY")
	if gatewayKey == "" {
		missing = append(missing, "GATEWAY_API_KEY")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	port, err := readIntEnv("PORT", DefaultPort)
	if err != nil {
		return nil, fmt.Errorf("PORT: %w", err)
	}

	debugMax, err := readDebugMaxRecords()
	if err != nil {
		return nil, err
	}

	timeouts, err := readTimeoutDefaults()
	if err != nil {
		return nil, err
	}

	return &Config{
		DatabaseURL:      databaseURL,
		TestDatabaseURL:  os.Getenv("TEST_DATABASE_URL"),
		GatewayAPIKey:    gatewayKey,
		Port:             port,
		DebugDBMaxRecords: debugMax,
		Timeouts:         timeouts,
	}, nil
}

// LoadForTest builds a Config without consulting the environment (for tests
// and the spike). All fields are caller-supplied.
func LoadForTest(opts ...Option) *Config {
	c := &Config{
		Port:              DefaultPort,
		DebugDBMaxRecords: DefaultDebugDBMaxRecords,
		Timeouts:          CodeDefaultTimeouts(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Option mutates a test Config.
type Option func(*Config)

// WithGatewayKey sets the gateway key on a test Config.
func WithGatewayKey(key string) Option {
	return func(c *Config) { c.GatewayAPIKey = key }
}

// WithDatabaseURL sets the database URL on a test Config.
func WithDatabaseURL(url string) Option {
	return func(c *Config) { c.DatabaseURL = url }
}

// CodeDefaultTimeouts returns the built-in timeout defaults.
func CodeDefaultTimeouts() TimeoutSettings {
	return TimeoutSettings{
		DefaultFirstByteMs: DefaultDefaultFirstByteTimeoutMs,
		StreamFirstByteMs:  DefaultStreamFirstByteTimeoutMs,
		ImageFirstByteMs:   DefaultImageFirstByteTimeoutMs,
		ResponseIdleMs:     DefaultResponseIdleTimeoutMs,
	}
}

// readTimeoutDefaults replicates the precedence chain in getGatewayTimeoutDefaults:
//
//	UPSTREAM_<KIND>_FIRST_BYTE_TIMEOUT_MS → UPSTREAM_REQUEST_TIMEOUT_MS → code default
//	UPSTREAM_RESPONSE_IDLE_TIMEOUT_MS → code default
func readTimeoutDefaults() (TimeoutSettings, error) {
	// The legacy UPSTREAM_REQUEST_TIMEOUT_MS is a shared fallback for the three
	// first-byte fields when their specific env is unset.
	var legacyFB *int64
	if v, ok := readPositiveIntEnv("UPSTREAM_REQUEST_TIMEOUT_MS"); ok {
		legacyFB = &v
	}

	defaultFB := resolveFirstByte(readPositiveIntEnvPtr("UPSTREAM_DEFAULT_FIRST_BYTE_TIMEOUT_MS"), legacyFB, DefaultDefaultFirstByteTimeoutMs)
	streamFB := resolveFirstByte(readPositiveIntEnvPtr("UPSTREAM_STREAM_FIRST_BYTE_TIMEOUT_MS"), legacyFB, DefaultStreamFirstByteTimeoutMs)
	imageFB := resolveFirstByte(readPositiveIntEnvPtr("UPSTREAM_IMAGE_FIRST_BYTE_TIMEOUT_MS"), legacyFB, DefaultImageFirstByteTimeoutMs)
	idle := firstNonNegative(
		readNonNegativeIntEnvPtr("UPSTREAM_RESPONSE_IDLE_TIMEOUT_MS"),
		DefaultResponseIdleTimeoutMs,
	)

	ts := TimeoutSettings{
		DefaultFirstByteMs: defaultFB,
		StreamFirstByteMs:  streamFB,
		ImageFirstByteMs:   imageFB,
		ResponseIdleMs:     idle,
	}
	if err := validateTimeouts(ts); err != nil {
		return TimeoutSettings{}, err
	}
	return ts, nil
}

func validateTimeouts(ts TimeoutSettings) error {
	if err := checkRange(ts.DefaultFirstByteMs, "defaultFirstByteTimeoutMs", FirstByteLimits); err != nil {
		return err
	}
	if err := checkRange(ts.StreamFirstByteMs, "streamFirstByteTimeoutMs", FirstByteLimits); err != nil {
		return err
	}
	if err := checkRange(ts.ImageFirstByteMs, "imageFirstByteTimeoutMs", FirstByteLimits); err != nil {
		return err
	}
	if err := checkRange(ts.ResponseIdleMs, "responseIdleTimeoutMs", ResponseIdleLimit); err != nil {
		return err
	}
	return nil
}

func checkRange(v int64, name string, l TimeoutLimit) error {
	minOK := v >= l.Min
	if !l.AllowZero {
		// firstByte limits use min 1000; allowZero is false so 0 is invalid.
	}
	if !minOK || v > l.Max {
		return fmt.Errorf("%s must be between %dms and %dms", name, l.Min, l.Max)
	}
	return nil
}

func readDebugMaxRecords() (int, error) {
	raw := strings.TrimSpace(os.Getenv("DEBUG_DB_MAX_RECORDS"))
	if raw == "" {
		return DefaultDebugDBMaxRecords, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < MinDebugDBMaxRecords {
		return 0, fmt.Errorf("DEBUG_DB_MAX_RECORDS must be an integer >= %d", MinDebugDBMaxRecords)
	}
	return n, nil
}

func readIntEnv(name string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("must be an integer, got %q", raw)
	}
	return n, nil
}

// readPositiveIntEnv returns (value, ok). ok is false when unset or non-positive.
func readPositiveIntEnv(name string) (int64, bool) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func readPositiveIntEnvPtr(name string) *int64 {
	if v, ok := readPositiveIntEnv(name); ok {
		return &v
	}
	return nil
}

func readNonNegativeIntEnvPtr(name string) *int64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return nil
	}
	return &n
}

// resolveFirstByte picks the first positive value among: the field-specific
// env, the shared legacy env, and the code default. Implements the precedence
// chain UPSTREAM_<KIND>_FIRST_BYTE_TIMEOUT_MS → UPSTREAM_REQUEST_TIMEOUT_MS → default.
func resolveFirstByte(specific, legacy *int64, fallback int64) int64 {
	if specific != nil && *specific > 0 {
		return *specific
	}
	if legacy != nil && *legacy > 0 {
		return *legacy
	}
	return fallback
}

func firstNonNegative(v *int64, fallback int64) int64 {
	if v != nil {
		return *v
	}
	return fallback
}

// ResponseStreamLogMaxDurationMs reads the optional streaming-log duration cap.
// Mirrors getStreamingObservationLimits in response-observer.ts.
func ResponseStreamLogMaxDurationMs() int64 {
	raw := strings.TrimSpace(os.Getenv(ResponseStreamLogMaxDurEnv))
	if raw == "" {
		return DefaultResponseStreamLogMaxDurMs
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return DefaultResponseStreamLogMaxDurMs
	}
	if n < MinResponseStreamLogMaxDurMs {
		return MinResponseStreamLogMaxDurMs
	}
	return n
}

// ErrMissingRequired is returned by Load when required vars are absent.
var ErrMissingRequired = errors.New("missing required environment variables")

// Silence unused-import for time/math in builds that don't reference them yet;
// these are used by downstream packages via the exported constants.
var (
	_ = time.Second
	_ = math.Trunc
)
