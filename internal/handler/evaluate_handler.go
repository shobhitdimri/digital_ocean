package handler

import (
	"context"
	"net/http"

	"feature-flag-service/internal/domain"
	"feature-flag-service/internal/service"
	"feature-flag-service/pkg/httpx"
)

// FlagEvaluator is what EvaluateHandler needs from the evaluation service.
type FlagEvaluator interface {
	Evaluate(ctx context.Context, flagName, userID string) (service.Evaluation, error)
}

// EvaluateHandler answers "is this flag enabled for this user?".
type EvaluateHandler struct {
	eval FlagEvaluator
}

// NewEvaluateHandler creates an EvaluateHandler.
func NewEvaluateHandler(eval FlagEvaluator) *EvaluateHandler {
	return &EvaluateHandler{eval: eval}
}

// Evaluate handles GET /v1/evaluate?flag=NAME&user_id=USER.
func (h *EvaluateHandler) Evaluate(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	flag, user := q.Get("flag"), q.Get("user_id")
	if flag == "" {
		writeError(w, r, domain.Invalidf("query parameter %q is required", "flag"))
		return
	}
	if user == "" {
		writeError(w, r, domain.Invalidf("query parameter %q is required", "user_id"))
		return
	}
	result, err := h.eval.Evaluate(r.Context(), flag, user)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toEvaluateResponse(result))
}
