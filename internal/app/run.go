package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Started returns a channel that is closed once the listener is bound and the
// server is accepting connections. It is never closed if Run fails to bind.
func (a *App) Started() <-chan struct{} { return a.started }

// Addr returns the address the server is listening on, such as "[::]:8080".
// It is only meaningful after Started is closed; it is how callers learn the
// real port when the configured port is 0.
func (a *App) Addr() string { return a.addr }

// Run serves HTTP until ctx is cancelled or the server fails, then shuts down
// in order:
//
//  1. Readiness flips to false, so /readyz answers 503. If SHUTDOWN_DELAY is
//     set, the server keeps listening for that long so a load balancer can see
//     the failing probe and stop routing new requests here.
//  2. The listener closes and in-flight requests get up to SHUTDOWN_TIMEOUT to
//     finish. If they do not, the remaining connections are closed forcibly.
//  3. Resources are released in reverse creation order: the cache (stopping its
//     janitor goroutine), then the logger last so the final lines reach disk.
//
// The listener is bound before the serving goroutine starts, so an address
// already in use is reported straight away as an error. Run may only be called
// once. It returns nil after a clean shutdown.
func (a *App) Run(ctx context.Context) error {
	if !a.running.CompareAndSwap(false, true) {
		return errors.New("app: Run called more than once")
	}

	ln, err := a.listen("tcp", a.srv.Addr)
	if err != nil {
		a.log.Error("cannot listen", "addr", a.srv.Addr, "err", err)
		_ = a.closeAll()
		return fmt.Errorf("listen on %s: %w", a.srv.Addr, err)
	}
	a.addr = ln.Addr().String()
	close(a.started)

	a.log.Info("starting server",
		"addr", a.addr,
		"log_level", a.cfg.LogLevel,
		"data_dir", a.cfg.DataDir,
		"cache_capacity", a.cfg.CacheCapacity,
		"cache_ttl", a.cfg.CacheTTL.String(),
	)

	errCh := make(chan error, 1)
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		if err := a.srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		a.log.Error("server failed", "err", err)
		a.ready.Store(false)
		_ = a.closeAll()
		return fmt.Errorf("server failed: %w", err)
	case <-ctx.Done():
	}

	a.log.Info("shutdown signal received",
		"delay", a.cfg.ShutdownDelay.String(),
		"timeout", a.cfg.ShutdownTimeout.String(),
	)
	a.ready.Store(false)

	if a.cfg.ShutdownDelay > 0 {
		a.log.Info("not ready; waiting before closing the listener", "delay", a.cfg.ShutdownDelay.String())
		select {
		case <-time.After(a.cfg.ShutdownDelay):
		case err := <-errCh:
			a.log.Error("server failed while draining", "err", err)
		}
	}

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
	// net/http's Shutdown already waits for Serve to return; waiting here too
	// keeps that guarantee ours rather than an implementation detail.
	<-serveDone

	if err := a.closeAll(); err != nil && shutdownErr == nil {
		return fmt.Errorf("close resources: %w", err)
	}
	return shutdownErr
}
