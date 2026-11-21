package middleware

import (
	"log/slog"
	"net/http"
	"time"

	"feature-flag-service/pkg/logger"
)

// Logging writes one access-log line per request after it finishes.
//
// The line carries method, path, status, duration_ms, bytes and remote_addr,
// plus request_id and actor from the request-scoped logger, and user_id when the
// request asks about one (the evaluate endpoint). Query strings and bodies are
// never logged.
//
// Level by outcome: 5xx is ERROR, 4xx and requests slower than slow are WARN,
// everything else INFO. Successful health probes are DEBUG so orchestrator
// polling does not drown the log. The line is written even if a panic is
// passing through.
func Logging(slow time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := newStatusWriter(w)
			defer func() {
				elapsed := time.Since(start)
				attrs := []any{
					"method", r.Method,
					"path", r.URL.Path,
					"status", sw.status,
					"duration_ms", float64(elapsed.Microseconds()) / 1000,
					"bytes", sw.bytes,
					"remote_addr", r.RemoteAddr,
				}
				if uid := r.URL.Query().Get("user_id"); uid != "" {
					attrs = append(attrs, "user_id", uid)
				}
				log := logger.FromContext(r.Context())
				log.Log(r.Context(), levelFor(r, sw.status, elapsed, slow), "request completed", attrs...)
			}()
			next.ServeHTTP(sw, r)
		})
	}
}

func levelFor(r *http.Request, status int, elapsed, slow time.Duration) slog.Level {
	switch {
	case status >= 500:
		return slog.LevelError
	case status >= 400:
		return slog.LevelWarn
	case slow > 0 && elapsed >= slow:
		return slog.LevelWarn
	case isProbe(r.URL.Path):
		return slog.LevelDebug
	default:
		return slog.LevelInfo
	}
}

func isProbe(path string) bool {
	return path == "/healthz" || path == "/readyz"
}
