// Command api serves the Floci Flix backend.
//
// It discovers PostgreSQL, Valkey and OpenSearch through Floci's AWS control
// plane and connects to them. It creates none of them: the provisioning job owns
// the resources, the schema, the seed data and the search index.
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

	"github.com/okteto/app-with-floci/api/internal/config"
	"github.com/okteto/app-with-floci/api/internal/httpapi"
)

func main() { os.Exit(run()) }

func run() int {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel(),
	})))

	cfg, err := config.Load()
	if err != nil {
		slog.Error("configuration error", "error", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	app := httpapi.NewApp(cfg)
	defer app.Close()

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           app.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	// Supervise returns rather than retrying. There is nothing to retry for: this
	// process cannot create what is missing, and the job that could has already
	// run. Exiting non-zero surfaces that as a CrashLoopBackOff instead of a pod
	// that reports healthy while serving nothing.
	fatal := make(chan error, 1)
	go func() { fatal <- app.Supervise(ctx) }()

	code := 0
	select {
	case <-ctx.Done():
		slog.Info("shutting down")
	case err := <-serverErr:
		slog.Error("server failed", "error", err)
		code = 1
	case err := <-fatal:
		if err != nil {
			slog.Error("cannot serve", "error", err)
			code = 1
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed", "error", err)
	}
	return code
}

func logLevel() slog.Level {
	if os.Getenv("LOG_LEVEL") == "debug" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}
