package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"invoice-and-payment-service/internal/config"
	"invoice-and-payment-service/internal/db"
	"invoice-and-payment-service/internal/health"
	"invoice-and-payment-service/migrations"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fatal("load config", err)
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Migrate before serving so a bad migration fails the start, not a request.
	if err := db.Migrate(cfg.DatabaseURL, migrations.FS, "schema_migrations"); err != nil {
		fatal("migrate", err)
	}
	pool, err := db.NewPool(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		fatal("open database", err)
	}
	defer pool.Close()

	r := chi.NewRouter()
	health.RegisterRoutes(r, pool)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second, // above the pay path's PSP timeout
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		slog.Info("listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			fatal("http server", err)
		}
	}()

	<-ctx.Done()

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown", "error", err)
	}
}

// fatal exits immediately, so deferred cleanup is skipped; the process is dying anyway.
func fatal(msg string, err error) {
	slog.Error(msg, "error", err)
	os.Exit(1)
}
