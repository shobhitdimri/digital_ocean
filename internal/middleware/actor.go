package middleware

import (
	"log/slog"
	"net/http"
	"strings"

	"feature-flag-service/internal/domain"
	"feature-flag-service/pkg/httpx"
	"feature-flag-service/pkg/logger"
)

// ActorHeader names the user making a request. There is no authentication yet,
// so this is an asserted identity used for the audit trail and logs.
const ActorHeader = "X-Actor"

// Actor reads the X-Actor header and puts the identity in the context and in the
// request-scoped logger.
//
//   - A write (POST, PUT, PATCH, DELETE) without the header is rejected with
//     400, because every change must be attributable.
//   - A read without the header is allowed and recorded as "anonymous".
//   - A header that is present but not a valid identity is rejected on any
//     method, rather than silently recorded as something else.
func Actor(base *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			actor := strings.TrimSpace(r.Header.Get(ActorHeader))
			switch {
			case actor == "" && isWrite(r.Method):
				httpx.WriteError(w, r, http.StatusBadRequest, "missing_actor",
					ActorHeader+" header is required for write requests")
				return
			case actor == "":
				actor = logger.AnonymousActor
			default:
				if err := domain.ValidateActor(actor); err != nil {
					httpx.WriteError(w, r, http.StatusBadRequest, "invalid_actor", err.Error())
					return
				}
			}
			ctx := logger.WithRequest(r.Context(), base, logger.RequestID(r.Context()), actor)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func isWrite(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}
