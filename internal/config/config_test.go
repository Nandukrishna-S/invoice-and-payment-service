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

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.DBMaxConns != 20 || cfg.LogLevel != slog.LevelInfo || cfg.ShutdownTimeout != 15*time.Second {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("HTTP_ADDR", ":9000")
	t.Setenv("DB_MAX_CONNS", "5")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("SHUTDOWN_TIMEOUT", "3s")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":9000" || cfg.DBMaxConns != 5 || cfg.LogLevel != slog.LevelDebug || cfg.ShutdownTimeout != 3*time.Second {
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
