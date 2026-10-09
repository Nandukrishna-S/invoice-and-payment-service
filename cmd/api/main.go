package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // the distroless image has no zoneinfo; FISCAL_TIMEZONE needs it

	"github.com/go-chi/chi/v5"

	"invoice-and-payment-service/internal/auth"
	"invoice-and-payment-service/internal/config"
	"invoice-and-payment-service/internal/customers"
	"invoice-and-payment-service/internal/db"
	"invoice-and-payment-service/internal/health"
	"invoice-and-payment-service/internal/httpx"
	"invoice-and-payment-service/internal/invoices"
	"invoice-and-payment-service/internal/psp"
	"invoice-and-payment-service/internal/reconciler"
	"invoice-and-payment-service/internal/webhooks"
	"invoice-and-payment-service/migrations"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fatal("load config", err)
	}
	slog.SetDefault(slog.New(httpx.NewContextHandler(
		slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))))

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

	authSvc := auth.NewService(pool)
	generatedKey, err := authSvc.Seed(ctx, cfg.DemoAPIKey)
	if err != nil {
		fatal("seed", err)
	}
	if generatedKey != "" {
		// Printed, not logged: it is shown once and never stored in recoverable form.
		fmt.Printf("\n  Initial API key (shown once): %s\n\n", generatedKey)
	}

	pspClient := psp.NewClient(cfg.PSPBaseURL, cfg.PSPConnectTimeout, cfg.PSPTotalTimeout)
	invoiceSvc := invoices.NewService(pool, cfg.FiscalLocation, pspClient, webhooks.NewOutbox())

	r := chi.NewRouter()
	r.Use(httpx.RequestID, httpx.Recoverer)
	r.NotFound(httpx.NotFound)
	r.MethodNotAllowed(httpx.MethodNotAllowed)
	health.RegisterRoutes(r, pool) // outside the auth group: the only unauthenticated route

	r.Group(func(r chi.Router) {
		r.Use(auth.Middleware(authSvc))
		customers.RegisterRoutes(r, pool)
		invoices.RegisterRoutes(r, invoiceSvc)
		webhooks.RegisterRoutes(r, pool)
	})

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

	var pprofSrv *http.Server
	if cfg.PPROFAddr != "" {
		pprofSrv = startPprof(cfg.PPROFAddr)
	}

	// Unknown payment outcomes are settled by asking the provider. It stops with the
	// context, and shutdown waits for it so an in-flight resolution finishes
	// (or rolls back) before the pool closes.
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		reconciler.New(pool, pspClient, invoiceSvc, cfg.PSPTotalTimeout).Run(ctx, cfg.ReconcilerPollInterval)
	}()

	<-ctx.Done()
	stop() // restore default signal handling so a second Ctrl-C kills a stuck shutdown

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown", "error", err)
	}
	if pprofSrv != nil {
		_ = pprofSrv.Shutdown(shutdownCtx)
	}
	workers.Wait()
}

// startPprof serves profiling endpoints on their own listener, never on the
// public API port.
func startPprof(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		slog.Info("pprof listening", "addr", addr)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			slog.Error("pprof server", "error", err)
		}
	}()
	return srv
}

// fatal exits immediately, so deferred cleanup is skipped; the process is dying anyway.
func fatal(msg string, err error) {
	slog.Error(msg, "error", err)
	os.Exit(1)
}
