// Package app is the composition root: it wires concrete implementations
// together and owns the process lifecycle.
package app

import (
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sync/atomic"

	"feature-flag-service/internal/config"
	"feature-flag-service/internal/dao/file"
	"feature-flag-service/internal/evaluator"
	"feature-flag-service/internal/handler"
	"feature-flag-service/internal/router"
	"feature-flag-service/internal/service"
	"feature-flag-service/pkg/cache/lfu"
	"feature-flag-service/pkg/logger"
)

// App is the assembled service.
type App struct {
	cfg     config.Config
	log     *logger.Logger
	srv     *http.Server
	closers []io.Closer // released in reverse order on shutdown
	ready   atomic.Bool
}

// Ready reports whether the service should receive traffic. It is true from
// construction until shutdown begins, and backs the /readyz probe.
func (a *App) Ready() bool { return a.ready.Load() }

// New builds the dependency graph. All concrete types are chosen here and
// nowhere else; everything below this function sees interfaces.
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

	a := &App{cfg: cfg, log: log}
	// The logger is registered first so it is closed last and the final
	// shutdown lines still reach the files.
	a.closers = append(a.closers, log)

	fail := func(what string, err error) (*App, error) {
		_ = a.closeAll()
		return nil, fmt.Errorf("%s: %w", what, err)
	}

	flagDAO, err := file.NewFlagDAO(filepath.Join(cfg.DataDir, "flags.json"))
	if err != nil {
		return fail("init flag store", err)
	}
	auditDAO, err := file.NewAuditDAO(filepath.Join(cfg.DataDir, "audit.jsonl"))
	if err != nil {
		return fail("init audit store", err)
	}

	cache, err := lfu.New(lfu.Config{
		Capacity:        cfg.CacheCapacity,
		DefaultTTL:      cfg.CacheTTL,
		JanitorInterval: cfg.CacheJanitorInterval,
	}, log.Logger)
	if err != nil {
		return fail("init cache", err)
	}
	a.closers = append(a.closers, cache)

	eval, err := evaluator.NewDefault(flagDAO)
	if err != nil {
		return fail("init evaluator", err)
	}
	flagSvc, err := service.NewFlagService(flagDAO, auditDAO, cache)
	if err != nil {
		return fail("init flag service", err)
	}
	evalSvc, err := service.NewEvaluationService(eval, cache, cfg.CacheTTL)
	if err != nil {
		return fail("init evaluation service", err)
	}
	auditSvc, err := service.NewAuditService(auditDAO)
	if err != nil {
		return fail("init audit service", err)
	}

	root, err := router.New(router.Deps{
		Health:    handler.NewHealthHandler(a),
		Flags:     handler.NewFlagHandler(flagSvc),
		Overrides: handler.NewOverrideHandler(flagSvc),
		Evaluate:  handler.NewEvaluateHandler(evalSvc),
		Audit:     handler.NewAuditHandler(auditSvc),
		Logger:    log.Logger,
		IDs:       service.RandomIDGenerator{},
	})
	if err != nil {
		return fail("init router", err)
	}

	a.srv = &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           root,
		ReadHeaderTimeout: cfg.HTTPReadHeaderTimeout,
		ReadTimeout:       cfg.HTTPReadTimeout,
		WriteTimeout:      cfg.HTTPWriteTimeout,
		IdleTimeout:       cfg.HTTPIdleTimeout,
	}
	a.ready.Store(true)
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
