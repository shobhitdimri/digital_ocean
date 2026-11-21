package logger

import (
	"context"
	"log/slog"
)

type ctxKey int

const (
	loggerKey ctxKey = iota
	requestIDKey
	actorKey
)

// AnonymousActor is used when a request carries no actor identity.
const AnonymousActor = "anonymous"

// NewContext stores l in ctx.
func NewContext(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, l)
}

// FromContext returns the request-scoped logger, or slog.Default() when ctx
// has none, so callers never need a nil check.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// WithRequest attaches request identity to ctx and derives a child logger that
// stamps request_id and actor on every record. Everything logged further down
// the call chain via FromContext is therefore correlated by request_id.
func WithRequest(ctx context.Context, base *slog.Logger, requestID, actor string) context.Context {
	if actor == "" {
		actor = AnonymousActor
	}
	ctx = context.WithValue(ctx, requestIDKey, requestID)
	ctx = context.WithValue(ctx, actorKey, actor)
	return NewContext(ctx, base.With("request_id", requestID, "actor", actor))
}

// RequestID returns the request id stored in ctx, or "".
func RequestID(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}

// Actor returns the actor stored in ctx, or "".
func Actor(ctx context.Context) string {
	v, _ := ctx.Value(actorKey).(string)
	return v
}
