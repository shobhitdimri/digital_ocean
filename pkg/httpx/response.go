// Package httpx has small, domain-agnostic HTTP helpers: JSON responses, the
// service's single error shape, and strict JSON request decoding. It knows
// nothing about flags, so any handler in the project can use it.
package httpx

import (
	"encoding/json"
	"net/http"

	"feature-flag-service/pkg/logger"
)

// ErrorBody is the one JSON shape every error response uses.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail describes a failed request. Code is a stable machine-readable
// string; Message is for humans and may change.
type ErrorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// WriteJSON writes v as JSON with the given status. The body is encoded before
// anything is sent, so an encoding failure produces a clean 500 instead of a
// truncated 200.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		status = http.StatusInternalServerError
		body = []byte(`{"error":{"code":"internal","message":"internal server error"}}`)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

// WriteError writes the standard error body. The request id is taken from the
// request context so the client can quote it when reporting a problem.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	WriteJSON(w, status, ErrorBody{Error: ErrorDetail{
		Code:      code,
		Message:   message,
		RequestID: logger.RequestID(r.Context()),
	}})
}
