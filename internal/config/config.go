// Package config reads all runtime configuration from environment variables,
// once, at startup.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
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
}

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
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
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
	ShutdownTimeout time.Duration
}

func LoadPSP() (PSPConfig, error) {
	cfg := PSPConfig{
		HTTPAddr:        envString("PSP_HTTP_ADDR", ":8081"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		LogLevel:        slog.LevelInfo,
		ProcessingDelay: 30 * time.Second,
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
	return cfg, nil
}
