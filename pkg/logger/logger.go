// Package logger builds the service's structured logger.
//
// Output is JSON, one object per line, written to three destinations:
//
//	logs/app.log    every record at or above the configured level
//	logs/error.log  warn and error records only, for quick triage
//	stdout          same records as app.log, for container log collectors
//
// Both files rotate by size and age.
package logger

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/natefinch/lumberjack.v2"
)

const (
	appLogFile   = "app.log"
	errorLogFile = "error.log"
	defaultName  = "feature-flag-service"
)

// Config controls logger construction.
type Config struct {
	Service    string    // value of the "service" field, defaults to feature-flag-service
	Level      string    // debug, info, warn or error
	Dir        string    // directory for app.log and error.log
	MaxSizeMB  int       // rotate a file once it reaches this size
	MaxBackups int       // rotated files to keep, 0 keeps all
	MaxAgeDays int       // days to keep rotated files, 0 keeps all
	Stdout     io.Writer // defaults to os.Stdout
}

// Logger wraps *slog.Logger and owns the underlying files.
type Logger struct {
	*slog.Logger
	closers []io.Closer
}

// New creates a Logger writing to app.log, error.log and stdout.
func New(cfg Config) (*Logger, error) {
	level, err := ParseLevel(cfg.Level)
	if err != nil {
		return nil, err
	}
	if cfg.Dir == "" {
		return nil, errors.New("logger: Dir must not be empty")
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("logger: create log dir: %w", err)
	}
	stdout := cfg.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	service := cfg.Service
	if service == "" {
		service = defaultName
	}

	appFile := rotatingFile(cfg, appLogFile)
	errFile := rotatingFile(cfg, errorLogFile)

	// error.log never goes below warn, even when LOG_LEVEL=debug, and follows
	// the configured level when that is stricter (for example error).
	errorLevel := slog.LevelWarn
	if level > errorLevel {
		errorLevel = level
	}

	h := newMultiHandler(
		newJSONHandler(appFile, level),
		newJSONHandler(stdout, level),
		newJSONHandler(errFile, errorLevel),
	)

	return &Logger{
		Logger:  slog.New(h).With("service", service),
		closers: []io.Closer{appFile, errFile},
	}, nil
}

// Close flushes and closes the log files. It is safe to call more than once.
func (l *Logger) Close() error {
	var errs []error
	for _, c := range l.closers {
		errs = append(errs, c.Close())
	}
	return errors.Join(errs...)
}

// ParseLevel converts a level name to a slog.Level.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("logger: unknown level %q (want debug, info, warn or error)", s)
	}
}

func rotatingFile(cfg Config, name string) *lumberjack.Logger {
	return &lumberjack.Logger{
		Filename:   filepath.Join(cfg.Dir, name),
		MaxSize:    cfg.MaxSizeMB,
		MaxBackups: cfg.MaxBackups,
		MaxAge:     cfg.MaxAgeDays,
		Compress:   true,
	}
}

func newJSONHandler(w io.Writer, level slog.Level) slog.Handler {
	return slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.LevelKey {
				if lvl, ok := a.Value.Any().(slog.Level); ok {
					a.Value = slog.StringValue(strings.ToLower(lvl.String()))
				}
			}
			return a
		},
	})
}

// multiHandler fans a record out to several handlers, each applying its own
// level filter.
type multiHandler struct {
	handlers []slog.Handler
}

func newMultiHandler(handlers ...slog.Handler) *multiHandler {
	return &multiHandler{handlers: handlers}
}

func (m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			errs = append(errs, h.Handle(ctx, r.Clone()))
		}
	}
	return errors.Join(errs...)
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		next[i] = h.WithAttrs(attrs)
	}
	return &multiHandler{handlers: next}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		next[i] = h.WithGroup(name)
	}
	return &multiHandler{handlers: next}
}
