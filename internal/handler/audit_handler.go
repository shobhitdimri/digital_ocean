package handler

import (
	"context"
	"net/http"
	"strconv"

	"feature-flag-service/internal/domain"
	"feature-flag-service/pkg/httpx"
)

// AuditHistory is what AuditHandler needs from the audit service.
type AuditHistory interface {
	History(ctx context.Context, flagName string, limit int) ([]domain.AuditEntry, error)
}

// AuditHandler serves a flag's audit trail.
type AuditHandler struct {
	audit AuditHistory
}

// NewAuditHandler creates an AuditHandler.
func NewAuditHandler(audit AuditHistory) *AuditHandler {
	return &AuditHandler{audit: audit}
}

// History handles GET /v1/flags/{name}/audit?limit=N, newest entry first.
//
// A flag with no history (never existed) returns an empty list rather than 404,
// because the history of a deleted flag must stay readable.
func (h *AuditHandler) History(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, r, domain.Invalidf("query parameter %q must be an integer", "limit"))
			return
		}
		limit = n
	}
	name := r.PathValue("name")
	entries, err := h.audit.History(r.Context(), name, limit)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toAuditResponse(name, entries))
}
