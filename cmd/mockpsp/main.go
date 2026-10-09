// Command mockpsp is the demo payment provider; see package mockpsp.
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
	"invoice-and-payment-service/internal/httpx"
	"invoice-and-payment-service/internal/mockpsp"
	"invoice-and-payment-service/migrations/psp"
)

const schema = "psp"

func main() {
	cfg, err := config.LoadPSP()
	if err != nil {
		fatal("load config", err)
	}
	slog.SetDefault(slog.New(httpx.NewContextHandler(
		slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Everything the PSP touches lives in its own schema, with its own
	// bookkeeping table, so it can never collide with the API's tables.
	pspURL, err := db.WithSearchPath(cfg.DatabaseURL, schema)
	if err != nil {
		fatal("database url", err)
	}
	if err := db.EnsureSchema(ctx, cfg.DatabaseURL, schema); err != nil {
		fatal("ensure schema", err)
	}
	if err := db.Migrate(pspURL, psp.FS, "schema_migrations"); err != nil {
		fatal("migrate", err)
	}
	pool, err := db.NewPool(ctx, pspURL, 10)
	if err != nil {
		fatal("open database", err)
	}
	defer pool.Close()

	svc := mockpsp.NewService(pool, ctx, cfg.ProcessingDelay, cfg.FastDelay)

	r := chi.NewRouter()
	r.Use(httpx.RequestID, httpx.Recoverer)
	r.NotFound(httpx.NotFound)
	r.MethodNotAllowed(httpx.MethodNotAllowed)
	health.RegisterRoutes(r, pool)
	mockpsp.RegisterRoutes(r, svc)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// A slow charge holds its response for the whole processing delay.
		WriteTimeout: cfg.ProcessingDelay + 30*time.Second,
		IdleTimeout:  60 * time.Second,
	}
	go func() {
		slog.Info("mock psp listening", "addr", cfg.HTTPAddr, "processing_delay", cfg.ProcessingDelay.String(), "fast_delay", cfg.FastDelay.String())
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			fatal("http server", err)
		}
	}()

	<-ctx.Done()
	stop()

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown", "error", err)
	}
	svc.Close()
}

// fatal exits immediately, skipping deferred cleanup; only used while starting up.
func fatal(msg string, err error) {
	slog.Error(msg, "error", err)
	os.Exit(1)
}
