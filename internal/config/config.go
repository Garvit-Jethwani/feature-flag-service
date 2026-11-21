// Package config reads service configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"
)

// Config is the validated service configuration.
type Config struct {
	// DatabaseURL is the Postgres connection string. It contains a credential
	// and must never be logged.
	DatabaseURL string
	// APIKeys is the raw API key specification, parsed by the HTTP layer. It
	// contains credentials and must never be logged.
	APIKeys  string
	Port     int
	LogLevel slog.Level

	// CacheTTL is how long an evaluation result stays cached. Zero disables
	// the cache.
	CacheTTL time.Duration
	// CacheMaxEntries is the upper limit on cached evaluation results.
	CacheMaxEntries int

	// RateLimitPerSecond is the sustained request rate allowed per API key.
	// Zero disables rate limiting.
	RateLimitPerSecond float64
	// RateLimitBurst is how many requests a key may make at once.
	RateLimitBurst int
}

// Load reads configuration through getenv (normally os.Getenv). Error
// messages name the variable at fault but never include its value.
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		DatabaseURL: strings.TrimSpace(getenv("DATABASE_URL")),
		APIKeys:     strings.TrimSpace(getenv("API_KEYS")),
		Port:        8080,
		LogLevel:    slog.LevelInfo,

		CacheTTL:           60 * time.Second,
		CacheMaxEntries:    100_000,
		RateLimitPerSecond: 100,
		RateLimitBurst:     200,
	}

	var problems []error
	if cfg.DatabaseURL == "" {
		problems = append(problems, errors.New("DATABASE_URL is required"))
	}
	if cfg.APIKeys == "" {
		problems = append(problems, errors.New("API_KEYS is required"))
	}

	if raw := strings.TrimSpace(getenv("PORT")); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil || port < 1 || port > 65535 {
			problems = append(problems, errors.New("PORT must be a number from 1 to 65535"))
		} else {
			cfg.Port = port
		}
	}

	if raw := strings.TrimSpace(getenv("LOG_LEVEL")); raw != "" {
		switch strings.ToLower(raw) {
		case "debug":
			cfg.LogLevel = slog.LevelDebug
		case "info":
			cfg.LogLevel = slog.LevelInfo
		case "warn":
			cfg.LogLevel = slog.LevelWarn
		case "error":
			cfg.LogLevel = slog.LevelError
		default:
			problems = append(problems, errors.New("LOG_LEVEL must be debug, info, warn, or error"))
		}
	}

	if raw := strings.TrimSpace(getenv("CACHE_TTL")); raw != "" {
		ttl, err := time.ParseDuration(raw)
		if err != nil || ttl < 0 {
			problems = append(problems, errors.New("CACHE_TTL must be a duration such as 60s, or 0 to disable the cache"))
		} else {
			cfg.CacheTTL = ttl
		}
	}

	if raw := strings.TrimSpace(getenv("CACHE_MAX_ENTRIES")); raw != "" {
		entries, err := strconv.Atoi(raw)
		if err != nil || entries < 1 {
			problems = append(problems, errors.New("CACHE_MAX_ENTRIES must be a positive number"))
		} else {
			cfg.CacheMaxEntries = entries
		}
	}

	if raw := strings.TrimSpace(getenv("RATE_LIMIT_RPS")); raw != "" {
		rate, err := strconv.ParseFloat(raw, 64)
		if err != nil || rate < 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
			problems = append(problems, errors.New("RATE_LIMIT_RPS must be a number of requests per second, or 0 to disable rate limiting"))
		} else {
			cfg.RateLimitPerSecond = rate
		}
	}

	if raw := strings.TrimSpace(getenv("RATE_LIMIT_BURST")); raw != "" {
		burst, err := strconv.Atoi(raw)
		if err != nil || burst < 1 {
			problems = append(problems, errors.New("RATE_LIMIT_BURST must be a positive number"))
		} else {
			cfg.RateLimitBurst = burst
		}
	}

	if err := errors.Join(problems...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}
