// Package config reads service configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
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
}

// Load reads configuration through getenv (normally os.Getenv). Error
// messages name the variable at fault but never include its value.
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		DatabaseURL: strings.TrimSpace(getenv("DATABASE_URL")),
		APIKeys:     strings.TrimSpace(getenv("API_KEYS")),
		Port:        8080,
		LogLevel:    slog.LevelInfo,
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

	if err := errors.Join(problems...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}
