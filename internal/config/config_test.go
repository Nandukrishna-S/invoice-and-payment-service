package config

import (
	"log/slog"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("HTTP_ADDR", "")
	t.Setenv("DB_MAX_CONNS", "")
	t.Setenv("LOG_LEVEL", "")
	t.Setenv("SHUTDOWN_TIMEOUT", "")
	t.Setenv("DEMO_API_KEY", "")
	t.Setenv("FISCAL_TIMEZONE", "")
	t.Setenv("PSP_URL", "")
	t.Setenv("PSP_CONNECT_TIMEOUT", "")
	t.Setenv("PSP_TOTAL_TIMEOUT", "")
	t.Setenv("RECONCILER_POLL_INTERVAL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.DBMaxConns != 20 || cfg.LogLevel != slog.LevelInfo || cfg.ShutdownTimeout != 15*time.Second || cfg.DemoAPIKey != "" ||
		cfg.FiscalLocation.String() != "Asia/Kolkata" || cfg.PSPBaseURL != "http://localhost:8081" ||
		cfg.PSPConnectTimeout != 2*time.Second || cfg.PSPTotalTimeout != 5*time.Second ||
		cfg.ReconcilerPollInterval != 5*time.Second {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("HTTP_ADDR", ":9000")
	t.Setenv("DB_MAX_CONNS", "5")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("SHUTDOWN_TIMEOUT", "3s")
	t.Setenv("DEMO_API_KEY", "sk_test_x")
	t.Setenv("FISCAL_TIMEZONE", "UTC")
	t.Setenv("PSP_URL", "http://mockpsp:8081")
	t.Setenv("PSP_CONNECT_TIMEOUT", "500ms")
	t.Setenv("PSP_TOTAL_TIMEOUT", "3s")
	t.Setenv("RECONCILER_POLL_INTERVAL", "750ms")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":9000" || cfg.DBMaxConns != 5 || cfg.LogLevel != slog.LevelDebug || cfg.ShutdownTimeout != 3*time.Second || cfg.DemoAPIKey != "sk_test_x" || cfg.FiscalLocation != time.UTC ||
		cfg.PSPBaseURL != "http://mockpsp:8081" || cfg.PSPConnectTimeout != 500*time.Millisecond || cfg.PSPTotalTimeout != 3*time.Second || cfg.ReconcilerPollInterval != 750*time.Millisecond {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadInvalid(t *testing.T) {
	tests := []struct{ name, key, val string }{
		{"missing database url", "DATABASE_URL", ""},
		{"max conns not a number", "DB_MAX_CONNS", "many"},
		{"max conns zero", "DB_MAX_CONNS", "0"},
		{"bad log level", "LOG_LEVEL", "loud"},
		{"bad shutdown timeout", "SHUTDOWN_TIMEOUT", "soon"},
		{"negative shutdown timeout", "SHUTDOWN_TIMEOUT", "-1s"},
		{"unknown timezone", "FISCAL_TIMEZONE", "Mars/Olympus"},
		{"psp url without scheme", "PSP_URL", "mockpsp:8081"},
		{"psp url not http", "PSP_URL", "ftp://mockpsp"},
		{"psp url without host", "PSP_URL", "http://"},
		{"bad psp connect timeout", "PSP_CONNECT_TIMEOUT", "fast"},
		{"zero psp total timeout", "PSP_TOTAL_TIMEOUT", "0s"},
		{"connect timeout above total", "PSP_CONNECT_TIMEOUT", "10s"},
		{"bad reconciler interval", "RECONCILER_POLL_INTERVAL", "often"},
		{"zero reconciler interval", "RECONCILER_POLL_INTERVAL", "0s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://x")
			t.Setenv(tt.key, tt.val)
			if _, err := Load(); err == nil {
				t.Fatalf("expected error for %s=%q", tt.key, tt.val)
			}
		})
	}
}

func TestLoadPSPDefaultsAndOverrides(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	for _, k := range []string{"PSP_HTTP_ADDR", "LOG_LEVEL", "PSP_PROCESSING_DELAY", "PSP_FAST_DELAY", "SHUTDOWN_TIMEOUT"} {
		t.Setenv(k, "")
	}
	cfg, err := LoadPSP()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":8081" || cfg.ProcessingDelay != 30*time.Second || cfg.FastDelay != 100*time.Millisecond || cfg.ShutdownTimeout != 15*time.Second || cfg.LogLevel != slog.LevelInfo {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}

	t.Setenv("PSP_HTTP_ADDR", ":9999")
	t.Setenv("PSP_PROCESSING_DELAY", "2s")
	t.Setenv("PSP_FAST_DELAY", "0s")
	t.Setenv("SHUTDOWN_TIMEOUT", "1s")
	t.Setenv("LOG_LEVEL", "debug")
	cfg, err = LoadPSP()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":9999" || cfg.ProcessingDelay != 2*time.Second || cfg.ShutdownTimeout != time.Second || cfg.LogLevel != slog.LevelDebug {
		t.Fatalf("unexpected overrides: %+v", cfg)
	}
}

func TestLoadPSPInvalid(t *testing.T) {
	tests := []struct{ name, key, val string }{
		{"missing database url", "DATABASE_URL", ""},
		{"bad log level", "LOG_LEVEL", "loud"},
		{"bad processing delay", "PSP_PROCESSING_DELAY", "forever"},
		{"zero processing delay", "PSP_PROCESSING_DELAY", "0s"},
		{"bad fast delay", "PSP_FAST_DELAY", "quick"},
		{"negative fast delay", "PSP_FAST_DELAY", "-1ms"},
		{"negative shutdown timeout", "SHUTDOWN_TIMEOUT", "-1s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://x")
			t.Setenv(tt.key, tt.val)
			if _, err := LoadPSP(); err == nil {
				t.Fatalf("expected an error for %s=%q", tt.key, tt.val)
			}
		})
	}
}
