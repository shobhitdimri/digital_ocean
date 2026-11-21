// Package service holds the application's use cases: flag management with an
// audit trail, and cached flag evaluation.
//
// Every dependency is an interface declared in this package, by the consumer
// (dependency inversion), and each one is as small as the service needs
// (interface segregation). Concrete DAOs, the LFU cache and the logger are
// chosen only in the composition root, so each use case can be tested with a
// fake.
package service

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"feature-flag-service/internal/evaluator"
)

// Clock supplies the current time. Injecting it makes timestamps in flags and
// audit entries deterministic in tests.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production Clock. It always reports UTC.
type SystemClock struct{}

// Now returns the current time in UTC.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// IDGenerator creates unique identifiers for audit entries.
type IDGenerator interface {
	NewID() string
}

// RandomIDGenerator is the production IDGenerator: 128 random bits as hex. It
// is safe for concurrent use. The ids are unique, not secret.
type RandomIDGenerator struct{}

// NewID returns a 32 character hex string.
func (RandomIDGenerator) NewID() string {
	return fmt.Sprintf("%016x%016x", rand.Uint64(), rand.Uint64())
}

// Invalidator removes cached evaluations that depend on a flag. It is the only
// capability FlagService needs from the cache.
type Invalidator interface {
	InvalidateTag(tag string)
}

// EvalCache is the cache capability EvaluationService needs: read, plus the
// epoch protocol for filling after a miss without storing a stale result.
type EvalCache interface {
	Get(key string) (value any, ok bool)
	Epoch() uint64
	SetIfEpoch(key string, value any, ttl time.Duration, epoch uint64, tags ...string) bool
}

// FlagEvaluator computes an evaluation without caching. *evaluator.Evaluator
// satisfies it.
type FlagEvaluator interface {
	Evaluate(ctx context.Context, flagName, userID string) (evaluator.Result, error)
}

// FlagTag is the cache tag attached to every cached evaluation that read the
// named flag. FlagService invalidates it on every write to that flag, and
// EvaluationService attaches it when storing, so both sides agree on the format
// through this one function.
func FlagTag(flagName string) string {
	return "flag:" + flagName
}
