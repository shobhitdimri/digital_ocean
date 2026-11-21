// Package handler contains HTTP handlers. Handlers translate HTTP to service
// calls and back; they hold no business logic.
package handler

import (
	"encoding/json"
	"net/http"
)

// HealthHandler serves liveness checks.
type HealthHandler struct{}

// NewHealthHandler creates a HealthHandler.
func NewHealthHandler() *HealthHandler {
	return &HealthHandler{}
}

// Liveness reports that the process is up.
func (h *HealthHandler) Liveness(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
