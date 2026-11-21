// Package app is the composition root: it wires concrete implementations
// together and owns the process lifecycle.
package app

import (
	"fmt"
	"net/http"
	"time"

	"feature-flag-service/internal/config"
	"feature-flag-service/internal/handler"
	"feature-flag-service/internal/router"
)

// App is the assembled service.
type App struct {
	cfg config.Config
	srv *http.Server
}

// New builds the dependency graph. All concrete types are chosen here and
// nowhere else.
func New(cfg config.Config) (*App, error) {
	h := router.New(router.Deps{
		Health: handler.NewHealthHandler(),
	})

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
	}

	return &App{cfg: cfg, srv: srv}, nil
}
