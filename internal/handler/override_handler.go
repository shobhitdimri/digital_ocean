package handler

import (
	"context"
	"net/http"

	"feature-flag-service/internal/domain"
	"feature-flag-service/pkg/httpx"
)

// OverrideManager is what OverrideHandler needs from the flag service.
type OverrideManager interface {
	SetOverride(ctx context.Context, name, userID string, enabled bool) (domain.Flag, error)
	RemoveOverride(ctx context.Context, name, userID string) (domain.Flag, error)
}

// OverrideHandler serves per-user overrides.
type OverrideHandler struct {
	overrides OverrideManager
}

// NewOverrideHandler creates an OverrideHandler.
func NewOverrideHandler(overrides OverrideManager) *OverrideHandler {
	return &OverrideHandler{overrides: overrides}
}

// Set handles PUT /v1/flags/{name}/overrides/{userId}.
func (h *OverrideHandler) Set(w http.ResponseWriter, r *http.Request) {
	var req SetOverrideRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if req.Enabled == nil {
		writeError(w, r, domain.Invalidf(`field "enabled" is required`))
		return
	}
	flag, err := h.overrides.SetOverride(r.Context(), r.PathValue("name"), r.PathValue("userId"), *req.Enabled)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toFlagResponse(flag))
}

// Remove handles DELETE /v1/flags/{name}/overrides/{userId}.
func (h *OverrideHandler) Remove(w http.ResponseWriter, r *http.Request) {
	if _, err := h.overrides.RemoveOverride(r.Context(), r.PathValue("name"), r.PathValue("userId")); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
