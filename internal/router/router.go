// Package router builds the HTTP routing table and middleware chain.
package router

import (
	"net/http"

	"feature-flag-service/internal/handler"
)

// Deps are the handlers the router mounts. Concrete handlers are injected so
// the router stays free of construction logic.
type Deps struct {
	Health *handler.HealthHandler
}

// New returns the root http.Handler.
func New(d Deps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", d.Health.Liveness)
	return mux
}
