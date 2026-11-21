package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := load(env(nil))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Port != 8080 || cfg.LogLevel != "info" || cfg.LogDir != "logs" || cfg.DataDir != "data" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.ShutdownDelay != 0 {
		t.Fatalf("SHUTDOWN_DELAY must default to 0, got %s", cfg.ShutdownDelay)
	}
	if cfg.ShutdownTimeout != 10*time.Second || cfg.CacheTTL != 60*time.Second || cfg.CacheJanitorInterval != 30*time.Second {
		t.Fatalf("unexpected duration defaults: %+v", cfg)
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := load(env(map[string]string{
		"PORT":                   "9090",
		"LOG_LEVEL":              "DEBUG",
		"CACHE_CAPACITY":         "5",
		"CACHE_JANITOR_INTERVAL": "0s",
		"SHUTDOWN_TIMEOUT":       "3s",
		"SHUTDOWN_DELAY":         "5s",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Port != 9090 || cfg.LogLevel != "debug" || cfg.CacheCapacity != 5 || cfg.CacheJanitorInterval != 0 || cfg.ShutdownTimeout != 3*time.Second || cfg.ShutdownDelay != 5*time.Second {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"port not int", map[string]string{"PORT": "abc"}, "PORT must be an integer"},
		{"port out of range", map[string]string{"PORT": "70000"}, "out of range"},
		{"bad duration", map[string]string{"SHUTDOWN_TIMEOUT": "soon"}, "SHUTDOWN_TIMEOUT must be a duration"},
		{"negative delay", map[string]string{"SHUTDOWN_DELAY": "-1s"}, "SHUTDOWN_DELAY must not be negative"},
		{"bad delay", map[string]string{"SHUTDOWN_DELAY": "later"}, "SHUTDOWN_DELAY must be a duration"},
		{"negative shutdown", map[string]string{"SHUTDOWN_TIMEOUT": "-1s"}, "must be positive"},
		{"bad log level", map[string]string{"LOG_LEVEL": "verbose"}, "LOG_LEVEL must be one of"},
		{"zero capacity", map[string]string{"CACHE_CAPACITY": "0"}, "CACHE_CAPACITY"},
		{"zero ttl", map[string]string{"CACHE_TTL": "0s"}, "CACHE_TTL"},
		{"negative janitor", map[string]string{"CACHE_JANITOR_INTERVAL": "-1s"}, "CACHE_JANITOR_INTERVAL"},
		{"zero log size", map[string]string{"LOG_MAX_SIZE_MB": "0"}, "LOG_MAX_SIZE_MB"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(env(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}
