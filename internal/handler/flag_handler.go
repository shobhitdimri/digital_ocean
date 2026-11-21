// Package handler contains HTTP handlers. Handlers translate HTTP to service
// calls and back; they hold no business logic. Each handler depends on a small
// interface declared next to it, so it can be tested with a fake and never sees
// more of a service than it uses.
package handler

import (
	"context"
	"net/http"
	"net/url"

	"feature-flag-service/internal/domain"
	"feature-flag-service/internal/service"
	"feature-flag-service/pkg/httpx"
)

// FlagCRUD is what FlagHandler needs from the flag service.
type FlagCRUD interface {
	Create(ctx context.Context, name, description string, enabled bool) (domain.Flag, error)
	Get(ctx context.Context, name string) (domain.Flag, error)
	List(ctx context.Context) ([]domain.Flag, error)
	Update(ctx context.Context, name string, u service.FlagUpdate) (domain.Flag, error)
	Delete(ctx context.Context, name string) error
}

// FlagHandler serves flag create, read, update and delete.
type FlagHandler struct {
	flags FlagCRUD
}

// NewFlagHandler creates a FlagHandler.
func NewFlagHandler(flags FlagCRUD) *FlagHandler {
	return &FlagHandler{flags: flags}
}

// Create handles POST /v1/flags.
func (h *FlagHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req CreateFlagRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	flag, err := h.flags.Create(r.Context(), req.Name, req.Description, req.Enabled)
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/flags/"+url.PathEscape(flag.Name))
	httpx.WriteJSON(w, http.StatusCreated, toFlagResponse(flag))
}

// List handles GET /v1/flags.
func (h *FlagHandler) List(w http.ResponseWriter, r *http.Request) {
	flags, err := h.flags.List(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toFlagListResponse(flags))
}

// Get handles GET /v1/flags/{name}.
func (h *FlagHandler) Get(w http.ResponseWriter, r *http.Request) {
	flag, err := h.flags.Get(r.Context(), r.PathValue("name"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toFlagResponse(flag))
}

// Update handles PUT /v1/flags/{name}. Only the fields present are changed.
func (h *FlagHandler) Update(w http.ResponseWriter, r *http.Request) {
	var req UpdateFlagRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	flag, err := h.flags.Update(r.Context(), r.PathValue("name"), req.toUpdate())
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toFlagResponse(flag))
}

// Delete handles DELETE /v1/flags/{name}.
func (h *FlagHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if err := h.flags.Delete(r.Context(), r.PathValue("name")); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
