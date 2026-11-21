// Package router builds the HTTP routing table and middleware chain.
package router

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"feature-flag-service/internal/handler"
	"feature-flag-service/internal/middleware"
	"feature-flag-service/pkg/httpx"
)

// Defaults applied when the corresponding Deps field is not set.
const (
	DefaultMaxBodyBytes = 1 << 20 // 1 MiB
	DefaultSlowRequest  = time.Second
)

// Deps are everything the router mounts. Concrete handlers and infrastructure
// are injected so the router stays free of construction logic.
type Deps struct {
	Health    *handler.HealthHandler
	Flags     *handler.FlagHandler
	Overrides *handler.OverrideHandler
	Evaluate  *handler.EvaluateHandler
	Audit     *handler.AuditHandler

	Logger *slog.Logger
	IDs    middleware.IDGenerator

	// MaxBodyBytes caps request bodies. Zero or less uses DefaultMaxBodyBytes.
	MaxBodyBytes int64
	// SlowRequest is the duration after which an access-log line is raised to
	// WARN. Zero uses DefaultSlowRequest; a negative value disables it.
	SlowRequest time.Duration
}

// New returns the root http.Handler: the routing table wrapped in the
// middleware chain.
func New(d Deps) (http.Handler, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	maxBody := d.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = DefaultMaxBodyBytes
	}
	slow := d.SlowRequest
	switch {
	case slow == 0:
		slow = DefaultSlowRequest
	case slow < 0:
		slow = 0
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", d.Health.Liveness)
	mux.HandleFunc("GET /readyz", d.Health.Readiness)

	mux.HandleFunc("POST /v1/flags", d.Flags.Create)
	mux.HandleFunc("GET /v1/flags", d.Flags.List)
	mux.HandleFunc("GET /v1/flags/{name}", d.Flags.Get)
	mux.HandleFunc("PUT /v1/flags/{name}", d.Flags.Update)
	mux.HandleFunc("DELETE /v1/flags/{name}", d.Flags.Delete)

	mux.HandleFunc("PUT /v1/flags/{name}/overrides/{userId}", d.Overrides.Set)
	mux.HandleFunc("DELETE /v1/flags/{name}/overrides/{userId}", d.Overrides.Remove)

	mux.HandleFunc("GET /v1/flags/{name}/audit", d.Audit.History)
	mux.HandleFunc("GET /v1/evaluate", d.Evaluate.Evaluate)

	return middleware.Chain(jsonRouteErrors(mux),
		middleware.RequestID(d.Logger, d.IDs),
		middleware.Actor(d.Logger),
		middleware.Logging(slow),
		middleware.Recover(),
		middleware.BodyLimit(maxBody),
	), nil
}

func (d Deps) validate() error {
	switch {
	case d.Health == nil, d.Flags == nil, d.Overrides == nil, d.Evaluate == nil, d.Audit == nil:
		return errors.New("router: every handler is required")
	case d.Logger == nil:
		return errors.New("router: logger is required")
	case d.IDs == nil:
		return errors.New("router: request id generator is required")
	}
	return nil
}

// jsonRouteErrors makes the mux's own failures use the service's JSON error
// shape. Out of the box ServeMux answers an unknown path or a wrong method with
// a plain-text body, which would break clients that parse every error as JSON.
//
// Matched routes are served by the mux itself, so path parameters still work;
// only unmatched requests are inspected.
func jsonRouteErrors(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, pattern := mux.Handler(r)
		if pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}

		cw := &captureWriter{header: make(http.Header)}
		h.ServeHTTP(cw, r)
		switch cw.status {
		case http.StatusNotFound:
			httpx.WriteError(w, r, http.StatusNotFound, "not_found",
				fmt.Sprintf("no route for %s %s", r.Method, r.URL.Path))
		case http.StatusMethodNotAllowed:
			w.Header().Set("Allow", cw.header.Get("Allow"))
			httpx.WriteError(w, r, http.StatusMethodNotAllowed, "method_not_allowed",
				fmt.Sprintf("%s is not allowed on %s", r.Method, r.URL.Path))
		default: // for example the mux's redirect for an unclean path
			for k, v := range cw.header {
				w.Header()[k] = v
			}
			w.WriteHeader(cw.status)
		}
	})
}

// captureWriter records the status and headers the mux's fallback handler would
// send, and discards its body.
type captureWriter struct {
	header http.Header
	status int
}

func (c *captureWriter) Header() http.Header { return c.header }
func (c *captureWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}
func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return len(b), nil
}
