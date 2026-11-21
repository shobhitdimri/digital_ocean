// Package app is the composition root: it wires concrete implementations
// together and owns the process lifecycle.
package app

import (
	"fmt"
	"io"
	"net/http"

	"feature-flag-service/internal/config"
	"feature-flag-service/internal/handler"
	"feature-flag-service/internal/router"
	"feature-flag-service/pkg/logger"
)

// App is the assembled service.
type App struct {
	cfg     config.Config
	log     *logger.Logger
	srv     *http.Server
	closers []io.Closer // released in reverse order on shutdown
}

// New builds the dependency graph. All concrete types are chosen here and
// nowhere else.
func New(cfg config.Config) (*App, error) {
	log, err := logger.New(logger.Config{
		Level:      cfg.LogLevel,
		Dir:        cfg.LogDir,
		MaxSizeMB:  cfg.LogMaxSizeMB,
		MaxBackups: cfg.LogMaxBackups,
		MaxAgeDays: cfg.LogMaxAgeDays,
	})
	if err != nil {
		return nil, fmt.Errorf("init logger: %w", err)
	}

	h := router.New(router.Deps{
		Health: handler.NewHealthHandler(),
	})

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           h,
		ReadHeaderTimeout: cfg.HTTPReadHeaderTimeout,
		ReadTimeout:       cfg.HTTPReadTimeout,
		WriteTimeout:      cfg.HTTPWriteTimeout,
		IdleTimeout:       cfg.HTTPIdleTimeout,
	}

	a := &App{cfg: cfg, log: log, srv: srv}
	// The logger is registered first so it is closed last and the final
	// shutdown lines still reach the files.
	a.closers = append(a.closers, log)
	return a, nil
}

// closeAll releases resources in reverse creation order.
func (a *App) closeAll() error {
	var first error
	for i := len(a.closers) - 1; i >= 0; i-- {
		if err := a.closers[i].Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
