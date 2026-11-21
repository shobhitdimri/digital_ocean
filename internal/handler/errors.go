package handler

import (
	"context"
	"errors"
	"net/http"

	"feature-flag-service/internal/domain"
	"feature-flag-service/internal/service"
	"feature-flag-service/pkg/httpx"
	"feature-flag-service/pkg/logger"
)

// writeError answers a failed request. It is the single place where errors from
// the service layer become HTTP statuses:
//
//	httpx.RequestError (bad body)   -> its own 400, 413 or 415
//	domain.ErrInvalid               -> 400 invalid_request
//	domain.ErrNotFound              -> 404 not_found
//	domain.ErrAlreadyExists         -> 409 already_exists
//	domain.ErrConflict              -> 409 conflict
//	service.ErrAuditFailed          -> 500 audit_failed
//	context.DeadlineExceeded        -> 504 timeout
//	context.Canceled                -> 499 client_closed_request
//	anything else                   -> 500 internal
//
// Client errors carry the error's own message, which the domain writes for
// callers. An unexpected error never leaks: the client gets a fixed message and
// the real cause goes to the log with the request id.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	var reqErr *httpx.RequestError
	switch {
	case errors.As(err, &reqErr):
		httpx.WriteError(w, r, reqErr.Status, reqErr.Code, reqErr.Message)
	case errors.Is(err, domain.ErrInvalid):
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, domain.ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, domain.ErrAlreadyExists):
		httpx.WriteError(w, r, http.StatusConflict, "already_exists", err.Error())
	case errors.Is(err, domain.ErrConflict):
		httpx.WriteError(w, r, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, service.ErrAuditFailed):
		logger.FromContext(r.Context()).Error("request failed", "error", err)
		httpx.WriteError(w, r, http.StatusInternalServerError, "audit_failed",
			"the change was applied but its audit entry could not be recorded")
	case errors.Is(err, context.DeadlineExceeded):
		httpx.WriteError(w, r, http.StatusGatewayTimeout, "timeout", "the request timed out")
	case errors.Is(err, context.Canceled):
		httpx.WriteError(w, r, statusClientClosedRequest, "client_closed_request", "the request was cancelled")
	default:
		logger.FromContext(r.Context()).Error("request failed", "error", err)
		httpx.WriteError(w, r, http.StatusInternalServerError, "internal", "internal server error")
	}
}

// statusClientClosedRequest is the de facto status (from nginx) for a client
// that went away before the response was ready.
const statusClientClosedRequest = 499
