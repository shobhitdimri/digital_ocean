// Package config loads service configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds runtime settings. It is extended in later tickets
// (logging, data paths, cache, HTTP timeouts).
type Config struct {
	Port            int
	ShutdownTimeout time.Duration
}

const (
	defaultPort            = 8080
	defaultShutdownTimeout = 10 * time.Second
)

// Load reads configuration from environment variables, applying defaults
// and validating the result.
func Load() (Config, error) {
	port, err := intEnv("PORT", defaultPort)
	if err != nil {
		return Config{}, err
	}
	shutdown, err := durationEnv("SHUTDOWN_TIMEOUT", defaultShutdownTimeout)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{Port: port, ShutdownTimeout: shutdown}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("config: PORT %d out of range 1-65535", c.Port)
	}
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("config: SHUTDOWN_TIMEOUT must be positive, got %s", c.ShutdownTimeout)
	}
	return nil
}

func intEnv(key string, def int) (int, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be an integer, got %q", key, raw)
	}
	return v, nil
}

func durationEnv(key string, def time.Duration) (time.Duration, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def, nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be a duration like 10s, got %q", key, raw)
	}
	return v, nil
}
