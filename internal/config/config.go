// Package config loads service configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds runtime settings, all sourced from environment variables.
type Config struct {
	Port int

	// ShutdownDelay is how long the service reports "not ready" before it stops
	// listening, so a load balancer can notice /readyz failing and stop sending
	// traffic. Zero stops listening immediately.
	ShutdownDelay time.Duration
	// ShutdownTimeout bounds how long in-flight requests get to finish once the
	// service has stopped listening.
	ShutdownTimeout time.Duration

	HTTPReadHeaderTimeout time.Duration
	HTTPReadTimeout       time.Duration
	HTTPWriteTimeout      time.Duration
	HTTPIdleTimeout       time.Duration

	DataDir string

	LogDir        string
	LogLevel      string
	LogMaxSizeMB  int
	LogMaxBackups int
	LogMaxAgeDays int

	CacheCapacity        int
	CacheTTL             time.Duration
	CacheJanitorInterval time.Duration
}

var validLogLevels = map[string]struct{}{
	"debug": {}, "info": {}, "warn": {}, "warning": {}, "error": {},
}

// Load reads configuration from environment variables, applying defaults
// and validating the result.
func Load() (Config, error) {
	return load(os.LookupEnv)
}

// load is split from Load so tests can supply their own environment.
func load(lookup func(string) (string, bool)) (Config, error) {
	r := reader{lookup: lookup}

	cfg := Config{
		Port:            r.int("PORT", 8080),
		ShutdownDelay:   r.duration("SHUTDOWN_DELAY", 0),
		ShutdownTimeout: r.duration("SHUTDOWN_TIMEOUT", 10*time.Second),

		HTTPReadHeaderTimeout: r.duration("HTTP_READ_HEADER_TIMEOUT", 5*time.Second),
		HTTPReadTimeout:       r.duration("HTTP_READ_TIMEOUT", 15*time.Second),
		HTTPWriteTimeout:      r.duration("HTTP_WRITE_TIMEOUT", 15*time.Second),
		HTTPIdleTimeout:       r.duration("HTTP_IDLE_TIMEOUT", 60*time.Second),

		DataDir: r.string("DATA_DIR", "data"),

		LogDir:        r.string("LOG_DIR", "logs"),
		LogLevel:      strings.ToLower(r.string("LOG_LEVEL", "info")),
		LogMaxSizeMB:  r.int("LOG_MAX_SIZE_MB", 50),
		LogMaxBackups: r.int("LOG_MAX_BACKUPS", 5),
		LogMaxAgeDays: r.int("LOG_MAX_AGE_DAYS", 30),

		CacheCapacity:        r.int("CACHE_CAPACITY", 10000),
		CacheTTL:             r.duration("CACHE_TTL", 60*time.Second),
		CacheJanitorInterval: r.duration("CACHE_JANITOR_INTERVAL", 30*time.Second),
	}
	if r.err != nil {
		return Config{}, r.err
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	switch {
	case c.Port < 1 || c.Port > 65535:
		return fmt.Errorf("config: PORT %d out of range 1-65535", c.Port)
	case c.ShutdownTimeout <= 0:
		return fmt.Errorf("config: SHUTDOWN_TIMEOUT must be positive, got %s", c.ShutdownTimeout)
	case c.ShutdownDelay < 0:
		return fmt.Errorf("config: SHUTDOWN_DELAY must not be negative, got %s", c.ShutdownDelay)
	case c.HTTPReadHeaderTimeout <= 0 || c.HTTPReadTimeout <= 0 || c.HTTPWriteTimeout <= 0 || c.HTTPIdleTimeout <= 0:
		return fmt.Errorf("config: HTTP_* timeouts must be positive")
	case c.DataDir == "":
		return fmt.Errorf("config: DATA_DIR must not be empty")
	case c.LogDir == "":
		return fmt.Errorf("config: LOG_DIR must not be empty")
	case c.LogMaxSizeMB < 1:
		return fmt.Errorf("config: LOG_MAX_SIZE_MB must be at least 1, got %d", c.LogMaxSizeMB)
	case c.LogMaxBackups < 0 || c.LogMaxAgeDays < 0:
		return fmt.Errorf("config: LOG_MAX_BACKUPS and LOG_MAX_AGE_DAYS must not be negative")
	case c.CacheCapacity < 1:
		return fmt.Errorf("config: CACHE_CAPACITY must be at least 1, got %d", c.CacheCapacity)
	case c.CacheTTL <= 0:
		return fmt.Errorf("config: CACHE_TTL must be positive, got %s", c.CacheTTL)
	case c.CacheJanitorInterval < 0:
		return fmt.Errorf("config: CACHE_JANITOR_INTERVAL must not be negative (0 disables it), got %s", c.CacheJanitorInterval)
	}
	if _, ok := validLogLevels[c.LogLevel]; !ok {
		return fmt.Errorf("config: LOG_LEVEL must be one of debug, info, warn, error, got %q", c.LogLevel)
	}
	return nil
}

// reader accumulates the first parse error so Load stays linear to read.
type reader struct {
	lookup func(string) (string, bool)
	err    error
}

func (r *reader) string(key, def string) string {
	if v, ok := r.lookup(key); ok && v != "" {
		return v
	}
	return def
}

func (r *reader) int(key string, def int) int {
	v, ok := r.lookup(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		r.fail(fmt.Errorf("config: %s must be an integer, got %q", key, v))
		return def
	}
	return n
}

func (r *reader) duration(key string, def time.Duration) time.Duration {
	v, ok := r.lookup(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		r.fail(fmt.Errorf("config: %s must be a duration like 10s, got %q", key, v))
		return def
	}
	return d
}

func (r *reader) fail(err error) {
	if r.err == nil {
		r.err = err
	}
}
