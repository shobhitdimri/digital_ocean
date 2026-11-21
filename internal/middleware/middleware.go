// Package middleware holds the HTTP middleware chain: request id, actor,
// access logging, panic recovery and body size limit.
//
// The order, outermost first, is deliberate: RequestID, Actor, Logging,
// Recover, BodyLimit, then the router.
//
// RequestID and Actor run first so every later layer, including the access log
// and error bodies, can report who made which request. Logging sits outside
// Recover, so a request whose handler panicked still produces an access-log
// line, with status 500 and its request id. Recover sits outside BodyLimit and
// the handlers, so it catches panics from all of them.
package middleware

import "net/http"

// Middleware wraps a handler with extra behaviour.
type Middleware func(http.Handler) http.Handler

// Chain applies middlewares to h so that the first one listed is the
// outermost, meaning it sees the request first and the response last.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// statusWriter records the status and size of a response and whether anything
// has been sent, which the logging and recovery middleware need.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
	wrote  bool
}

func newStatusWriter(w http.ResponseWriter) *statusWriter {
	return &statusWriter{ResponseWriter: w, status: http.StatusOK}
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer for flushing
// and deadlines.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
