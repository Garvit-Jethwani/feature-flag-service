// Command server runs the feature flag HTTP API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Garvit-Jethwani/feature-flag-service/internal/cache"
	"github.com/Garvit-Jethwani/feature-flag-service/internal/config"
	"github.com/Garvit-Jethwani/feature-flag-service/internal/httpapi"
	"github.com/Garvit-Jethwani/feature-flag-service/internal/service"
	"github.com/Garvit-Jethwani/feature-flag-service/internal/store"
)

const (
	startupTimeout  = 30 * time.Second
	shutdownTimeout = 10 * time.Second
)

func main() {
	if err := run(); err != nil {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("server stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))

	// Each API key gets its own rate limiter.
	auth, err := httpapi.NewAuthenticator(cfg.APIKeys, httpapi.RateLimit{
		PerSecond: cfg.RateLimitPerSecond,
		Burst:     cfg.RateLimitBurst,
	})
	if err != nil {
		return err
	}

	// Cancelled on SIGINT or SIGTERM, which starts a graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	startCtx, cancelStart := context.WithTimeout(ctx, startupTimeout)
	defer cancelStart()

	pool, err := pgxpool.New(startCtx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("configure database pool: %w", err)
	}
	defer pool.Close()

	if err := store.Migrate(startCtx, pool); err != nil {
		return err
	}

	// Wire the layers together: store (Postgres) and cache (in memory) sit
	// behind the service, and the HTTP layer sits in front of it.
	flagStore := store.New(pool)
	resultCache := cache.NewMemory(cfg.CacheTTL, cfg.CacheMaxEntries)
	handler := httpapi.New(service.New(flagStore, resultCache, logger), auth, logger, flagStore.Ping)
	logger.Info("configured",
		"cache_ttl", cfg.CacheTTL.String(),
		"cache_max_entries", cfg.CacheMaxEntries,
		"rate_limit_rps", cfg.RateLimitPerSecond,
		"rate_limit_burst", cfg.RateLimitBurst,
	)

	server := &http.Server{
		Addr:              net.JoinHostPort("", strconv.Itoa(cfg.Port)),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "port", cfg.Port)
		serveErr <- server.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()
	if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
