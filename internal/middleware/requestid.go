package middleware

import (
	"log/slog"
	"net/http"

	"feature-flag-service/pkg/logger"
)

// RequestIDHeader carries the request id in both directions.
const RequestIDHeader = "X-Request-ID"

const maxRequestIDLength = 64

// IDGenerator creates request ids.
type IDGenerator interface {
	NewID() string
}

// RequestID gives every request an id, stores it (and a request-scoped logger)
// in the context, and echoes it in the response.
//
// A client-supplied X-Request-ID is reused so a caller can correlate its own
// logs with ours, but only if it is short and made of safe characters. Anything
// else is replaced, so a client cannot inject newlines or huge values into our
// logs.
func RequestID(base *slog.Logger, ids IDGenerator) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(RequestIDHeader)
			if !validRequestID(id) {
				id = ids.NewID()
			}
			w.Header().Set(RequestIDHeader, id)
			ctx := logger.WithRequest(r.Context(), base, id, logger.AnonymousActor)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLength {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.'
		if !ok {
			return false
		}
	}
	return true
}
