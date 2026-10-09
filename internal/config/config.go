// Package config reads all runtime configuration from environment variables,
// once, at startup.
package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPAddr        string
	DatabaseURL     string
	DBMaxConns      int32
	LogLevel        slog.Level
	ShutdownTimeout time.Duration
	// PPROFAddr enables pprof on a separate listener when set; keep it on localhost.
	PPROFAddr string
	// DemoAPIKey is for local development only and must never be set in production.
	DemoAPIKey string
	// FiscalLocation is where the April-to-March financial year boundary is judged.
	FiscalLocation *time.Location
	// The payment provider the pay flow and reconciler call.
	PSPBaseURL        string
	PSPConnectTimeout time.Duration
	PSPTotalTimeout   time.Duration
	// ReconcilerPollInterval is how often unresolved payments are re-checked
	// with the provider. Kept short in the demo so tok_timeout resolves quickly.
	ReconcilerPollInterval time.Duration
	// WebhookRetrySchedule lists the delay before each delivery attempt: the first
	// entry is the wait before the first attempt, and the number of entries is the
	// number of attempts. Each delay is jittered by up to 20% when it is applied.
	WebhookRetrySchedule []time.Duration
	// WebhookPollInterval is how often the delivery worker looks for due deliveries.
	WebhookPollInterval time.Duration
	// WebhookHTTPTimeout bounds one delivery request.
	WebhookHTTPTimeout time.Duration
}

// DefaultWebhookRetrySchedule is: immediate, then 30s, 2m, 10m, 1h, 6h, 12h
// (7 attempts over about 19.7 hours, before jitter).
const DefaultWebhookRetrySchedule = "0s,30s,2m,10m,1h,6h,12h"

// MaxWebhookHTTPTimeout keeps one delivery attempt comfortably inside the delivery lease.
const MaxWebhookHTTPTimeout = 20 * time.Second

// Load returns the configuration with defaults applied, or an error naming the
// first invalid variable.
func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:        envString("HTTP_ADDR", ":8080"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		DBMaxConns:      20,
		LogLevel:        slog.LevelInfo,
		ShutdownTimeout: 15 * time.Second,
		DemoAPIKey:      os.Getenv("DEMO_API_KEY"),
		PPROFAddr:       os.Getenv("PPROF_ADDR"),

		PSPBaseURL:        envString("PSP_URL", "http://localhost:8081"),
		PSPConnectTimeout: 2 * time.Second,
		PSPTotalTimeout:   5 * time.Second,

		ReconcilerPollInterval: 5 * time.Second,
		WebhookPollInterval:    time.Second,
		WebhookHTTPTimeout:     5 * time.Second,
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}

	if u, err := url.Parse(cfg.PSPBaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Config{}, fmt.Errorf("PSP_URL must be an absolute http(s) URL")
	}
	for env, dst := range map[string]*time.Duration{
		"PSP_CONNECT_TIMEOUT":      &cfg.PSPConnectTimeout,
		"PSP_TOTAL_TIMEOUT":        &cfg.PSPTotalTimeout,
		"RECONCILER_POLL_INTERVAL": &cfg.ReconcilerPollInterval,
		"WEBHOOK_POLL_INTERVAL":    &cfg.WebhookPollInterval,
		"WEBHOOK_HTTP_TIMEOUT":     &cfg.WebhookHTTPTimeout,
	} {
		if v := os.Getenv(env); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				return Config{}, fmt.Errorf("%s must be a positive duration, got %q", env, v)
			}
			*dst = d
		}
	}
	if cfg.PSPConnectTimeout > cfg.PSPTotalTimeout {
		return Config{}, fmt.Errorf("PSP_CONNECT_TIMEOUT must not exceed PSP_TOTAL_TIMEOUT")
	}

	schedule, err := ParseRetrySchedule(envString("WEBHOOK_RETRY_SCHEDULE", DefaultWebhookRetrySchedule))
	if err != nil {
		return Config{}, fmt.Errorf("WEBHOOK_RETRY_SCHEDULE: %w", err)
	}
	cfg.WebhookRetrySchedule = schedule

	// A claimed delivery is leased for 30s; an attempt that could outlive its lease
	// might be sent a second time by another worker while still in flight.
	if cfg.WebhookHTTPTimeout > MaxWebhookHTTPTimeout {
		return Config{}, fmt.Errorf("WEBHOOK_HTTP_TIMEOUT must be at most %s so an attempt cannot outlive its 30s lease", MaxWebhookHTTPTimeout)
	}

	fiscalTZ := envString("FISCAL_TIMEZONE", "Asia/Kolkata")
	loc, err := time.LoadLocation(fiscalTZ)
	if err != nil {
		return Config{}, fmt.Errorf("FISCAL_TIMEZONE must be an IANA timezone name, got %q", fiscalTZ)
	}
	cfg.FiscalLocation = loc

	if v := os.Getenv("DB_MAX_CONNS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 1 {
			return Config{}, fmt.Errorf("DB_MAX_CONNS must be a positive integer, got %q", v)
		}
		cfg.DBMaxConns = int32(n)
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(v)); err != nil {
			return Config{}, fmt.Errorf("LOG_LEVEL must be debug, info, warn or error, got %q", v)
		}
	}
	if v := os.Getenv("SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("SHUTDOWN_TIMEOUT must be a positive duration, got %q", v)
		}
		cfg.ShutdownTimeout = d
	}
	return cfg, nil
}

func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// PSPConfig is the mock PSP binary's configuration.
type PSPConfig struct {
	HTTPAddr    string
	DatabaseURL string
	LogLevel    slog.Level
	// ProcessingDelay is how long tok_timeout charges stay "processing".
	ProcessingDelay time.Duration
	// FastDelay is how long success and decline answers take (0 disables it).
	FastDelay       time.Duration
	ShutdownTimeout time.Duration
}

func LoadPSP() (PSPConfig, error) {
	cfg := PSPConfig{
		HTTPAddr:        envString("PSP_HTTP_ADDR", ":8081"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		LogLevel:        slog.LevelInfo,
		ProcessingDelay: 30 * time.Second,
		FastDelay:       100 * time.Millisecond,
		ShutdownTimeout: 15 * time.Second,
	}
	if cfg.DatabaseURL == "" {
		return PSPConfig{}, fmt.Errorf("DATABASE_URL is required")
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(v)); err != nil {
			return PSPConfig{}, fmt.Errorf("LOG_LEVEL must be debug, info, warn or error, got %q", v)
		}
	}
	for env, dst := range map[string]*time.Duration{
		"PSP_PROCESSING_DELAY": &cfg.ProcessingDelay,
		"SHUTDOWN_TIMEOUT":     &cfg.ShutdownTimeout,
	} {
		if v := os.Getenv(env); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				return PSPConfig{}, fmt.Errorf("%s must be a positive duration, got %q", env, v)
			}
			*dst = d
		}
	}
	if v := os.Getenv("PSP_FAST_DELAY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return PSPConfig{}, fmt.Errorf("PSP_FAST_DELAY must be a non-negative duration, got %q", v)
		}
		cfg.FastDelay = d
	}
	return cfg, nil
}

// ParseRetrySchedule parses a comma-separated list of durations. The first may
// be zero (an immediate first attempt); every later one must be positive.
func ParseRetrySchedule(raw string) ([]time.Duration, error) {
	var out []time.Duration
	for i, part := range strings.Split(raw, ",") {
		d, err := time.ParseDuration(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("entry %d (%q) is not a duration", i+1, strings.TrimSpace(part))
		}
		if d < 0 || (d == 0 && i > 0) {
			return nil, fmt.Errorf("entry %d (%q) must be positive (only the first may be 0s)", i+1, strings.TrimSpace(part))
		}
		out = append(out, d)
	}
	return out, nil
}

// ReceiverConfig is the demo webhook receiver's configuration.
type ReceiverConfig struct {
	Addr string
	// Secret optionally seeds the secrets the receiver verifies with; more can be
	// added at runtime through its /secrets endpoint.
	Secret   string
	LogLevel slog.Level
}

func LoadReceiver() (ReceiverConfig, error) {
	cfg := ReceiverConfig{
		Addr:     envString("RECEIVER_ADDR", ":9000"),
		Secret:   os.Getenv("RECEIVER_WEBHOOK_SECRET"),
		LogLevel: slog.LevelInfo,
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(v)); err != nil {
			return ReceiverConfig{}, fmt.Errorf("LOG_LEVEL must be debug, info, warn or error, got %q", v)
		}
	}
	return cfg, nil
}
