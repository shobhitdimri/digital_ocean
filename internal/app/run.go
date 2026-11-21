package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// Run starts the HTTP server and blocks until ctx is cancelled or the server
// fails. On cancellation it drains in-flight requests before returning.
//
// Readiness handling and shutdown tests arrive in FFS-8A.
func (a *App) Run(ctx context.Context) error {
	a.log.Info("starting server",
		"addr", a.srv.Addr,
		"log_level", a.cfg.LogLevel,
		"data_dir", a.cfg.DataDir,
		"cache_capacity", a.cfg.CacheCapacity,
		"cache_ttl", a.cfg.CacheTTL.String(),
	)

	errCh := make(chan error, 1)
	go func() {
		if err := a.srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		a.log.Error("server failed", "err", err)
		_ = a.closeAll()
		return fmt.Errorf("server failed: %w", err)
	case <-ctx.Done():
	}

	a.log.Info("shutdown signal received", "timeout", a.cfg.ShutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
	defer cancel()

	shutdownErr := a.srv.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		a.log.Warn("graceful shutdown did not finish, forcing close", "err", shutdownErr)
		_ = a.srv.Close()
		shutdownErr = fmt.Errorf("graceful shutdown: %w", shutdownErr)
	} else {
		a.log.Info("server stopped cleanly")
	}

	if err := a.closeAll(); err != nil && shutdownErr == nil {
		return fmt.Errorf("close resources: %w", err)
	}
	return shutdownErr
}
