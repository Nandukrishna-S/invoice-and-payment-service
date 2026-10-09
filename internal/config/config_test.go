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

func TestParseRetrySchedule(t *testing.T) {
	good := map[string][]time.Duration{
		DefaultWebhookRetrySchedule: {0, 30 * time.Second, 2 * time.Minute, 10 * time.Minute, time.Hour, 6 * time.Hour, 12 * time.Hour},
		"0s":                        {0},
		" 0s , 1s ,2s ":             {0, time.Second, 2 * time.Second},
		"5s,10s":                    {5 * time.Second, 10 * time.Second}, // the first entry may be non-zero
	}
	for in, want := range good {
		got, err := ParseRetrySchedule(in)
		if err != nil || len(got) != len(want) {
			t.Errorf("%q: got %v, %v", in, got, err)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%q: entry %d = %v, want %v", in, i, got[i], want[i])
			}
		}
	}
	for _, in := range []string{"", "soon", "0s,", "0s,0s", "0s,-1s", "-5s,1s", "1s,,2s", "0s,5"} {
		if _, err := ParseRetrySchedule(in); err == nil {
			t.Errorf("%q should be rejected", in)
		}
	}
}

func TestLoadWebhookSettings(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	for _, k := range []string{"WEBHOOK_RETRY_SCHEDULE", "WEBHOOK_POLL_INTERVAL", "WEBHOOK_HTTP_TIMEOUT"} {
		t.Setenv(k, "")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.WebhookRetrySchedule) != 7 || cfg.WebhookRetrySchedule[0] != 0 || cfg.WebhookPollInterval != time.Second || cfg.WebhookHTTPTimeout != 5*time.Second {
		t.Fatalf("defaults: %+v", cfg)
	}

	t.Setenv("WEBHOOK_RETRY_SCHEDULE", "0s,1s,2s")
	t.Setenv("WEBHOOK_POLL_INTERVAL", "250ms")
	t.Setenv("WEBHOOK_HTTP_TIMEOUT", "3s")
	cfg, err = Load()
	if err != nil || len(cfg.WebhookRetrySchedule) != 3 || cfg.WebhookPollInterval != 250*time.Millisecond || cfg.WebhookHTTPTimeout != 3*time.Second {
		t.Fatalf("overrides: %+v %v", cfg, err)
	}

	t.Setenv("WEBHOOK_HTTP_TIMEOUT", "20s")
	t.Setenv("WEBHOOK_RETRY_SCHEDULE", "0s,1s")
	t.Setenv("WEBHOOK_POLL_INTERVAL", "1s")
	if _, err := Load(); err != nil {
		t.Fatalf("20s is the longest allowed timeout: %v", err)
	}
	for _, bad := range []struct{ k, v string }{{"WEBHOOK_RETRY_SCHEDULE", "0s,0s"}, {"WEBHOOK_POLL_INTERVAL", "0s"}, {"WEBHOOK_HTTP_TIMEOUT", "never"}, {"WEBHOOK_HTTP_TIMEOUT", "21s"}, {"WEBHOOK_HTTP_TIMEOUT", "1m"}} {
		t.Setenv("WEBHOOK_RETRY_SCHEDULE", "0s,1s")
		t.Setenv("WEBHOOK_POLL_INTERVAL", "1s")
		t.Setenv("WEBHOOK_HTTP_TIMEOUT", "5s")
		t.Setenv(bad.k, bad.v)
		if _, err := Load(); err == nil {
			t.Errorf("%s=%q should be rejected", bad.k, bad.v)
		}
	}
}

func TestLoadReceiver(t *testing.T) {
	t.Setenv("RECEIVER_ADDR", "")
	t.Setenv("RECEIVER_WEBHOOK_SECRET", "")
	t.Setenv("LOG_LEVEL", "")
	cfg, err := LoadReceiver()
	if err != nil || cfg.Addr != ":9000" || cfg.Secret != "" || cfg.LogLevel != slog.LevelInfo {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
	t.Setenv("RECEIVER_ADDR", ":9999")
	t.Setenv("RECEIVER_WEBHOOK_SECRET", "whsec_x")
	t.Setenv("LOG_LEVEL", "debug")
	cfg, err = LoadReceiver()
	if err != nil || cfg.Addr != ":9999" || cfg.Secret != "whsec_x" || cfg.LogLevel != slog.LevelDebug {
		t.Fatalf("overrides: %+v %v", cfg, err)
	}
	t.Setenv("LOG_LEVEL", "loud")
	if _, err := LoadReceiver(); err == nil {
		t.Fatal("a bad log level should be rejected")
	}
}
