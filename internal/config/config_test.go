package config

import (
	"log/slog"
	"strings"
	"testing"
)

func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"DATABASE_URL": "postgres://example",
		"API_KEYS":     "placeholder",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 8080 {
		t.Errorf("Port = %d, want 8080", cfg.Port)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"DATABASE_URL": "postgres://example",
		"API_KEYS":     "placeholder",
		"PORT":         "9090",
		"LOG_LEVEL":    "DEBUG",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 9090 {
		t.Errorf("Port = %d, want 9090", cfg.Port)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want debug", cfg.LogLevel)
	}
}

func TestLoadRejectsBadSettings(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"missing database url", map[string]string{"API_KEYS": "x"}, "DATABASE_URL is required"},
		{"missing api keys", map[string]string{"DATABASE_URL": "x"}, "API_KEYS is required"},
		{"port not a number", map[string]string{"DATABASE_URL": "x", "API_KEYS": "x", "PORT": "http"}, "PORT must be"},
		{"port out of range", map[string]string{"DATABASE_URL": "x", "API_KEYS": "x", "PORT": "70000"}, "PORT must be"},
		{"unknown log level", map[string]string{"DATABASE_URL": "x", "API_KEYS": "x", "LOG_LEVEL": "loud"}, "LOG_LEVEL must be"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(env(tt.env))
			if err == nil {
				t.Fatal("Load succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestLoadErrorsDoNotLeakValues(t *testing.T) {
	const secret = "postgres://user:not-a-real-password@db/flags"
	_, err := Load(env(map[string]string{"DATABASE_URL": secret, "PORT": "bad"}))
	if err == nil {
		t.Fatal("Load succeeded, want an error")
	}
	if strings.Contains(err.Error(), "not-a-real-password") {
		t.Errorf("error leaks a configuration value: %q", err)
	}
}
