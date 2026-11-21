package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
)

// Run starts the HTTP server and blocks until ctx is cancelled or the server
// fails. On cancellation it drains in-flight requests before returning.
//
// This is the bare version from FFS-1; readiness handling, ordered closers and
// structured logging arrive in FFS-2 and FFS-8A.
func (a *App) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		log.Printf("server listening on %s", a.srv.Addr)
		if err := a.srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("server failed: %w", err)
	case <-ctx.Done():
	}

	log.Println("shutdown signal received")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
	defer cancel()
	if err := a.srv.Shutdown(shutdownCtx); err != nil {
		_ = a.srv.Close()
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	return nil
}
