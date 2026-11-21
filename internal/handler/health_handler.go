package handler

import (
	"net/http"

	"feature-flag-service/pkg/httpx"
)

// Readiness reports whether the service should receive traffic.
type Readiness interface {
	Ready() bool
}

// HealthHandler serves the liveness and readiness probes.
type HealthHandler struct {
	readiness Readiness
}

// NewHealthHandler creates a HealthHandler.
func NewHealthHandler(readiness Readiness) *HealthHandler {
	return &HealthHandler{readiness: readiness}
}

// Liveness handles GET /healthz. It answers 200 whenever the process can serve
// a request, and deliberately checks nothing else: a failing liveness probe
// makes an orchestrator restart the process.
func (h *HealthHandler) Liveness(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Readiness handles GET /readyz. It answers 503 once shutdown has begun, so a
// load balancer stops sending new requests while in-flight ones finish.
func (h *HealthHandler) Readiness(w http.ResponseWriter, _ *http.Request) {
	if h.readiness.Ready() {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
		return
	}
	httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "shutting_down"})
}
