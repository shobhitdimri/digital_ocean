package middleware

import (
	"net/http"
	"runtime/debug"

	"feature-flag-service/pkg/httpx"
	"feature-flag-service/pkg/logger"
)

// Recover turns a panic in a handler into a logged 500 so one bad request cannot
// take the process down. The stack trace goes to the log, never to the client.
//
// If the handler had already started the response, nothing more is written:
// the status line is gone, and appending an error body would corrupt it.
// http.ErrAbortHandler is re-panicked, as the net/http contract asks, so the
// server can abort the connection quietly.
func Recover() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sw := newStatusWriter(w)
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				logger.FromContext(r.Context()).Error("panic recovered",
					"panic", rec,
					"stack", string(debug.Stack()),
				)
				if !sw.wrote {
					httpx.WriteError(sw, r, http.StatusInternalServerError, "internal", "internal server error")
				}
			}()
			next.ServeHTTP(sw, r)
		})
	}
}
