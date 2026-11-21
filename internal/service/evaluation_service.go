package service

import (
	"context"
	"errors"
	"time"

	"feature-flag-service/internal/domain"
	"feature-flag-service/internal/evaluator"
	"feature-flag-service/pkg/logger"
)

// Evaluation is the outcome of EvaluationService.Evaluate.
type Evaluation struct {
	evaluator.Result

	// Cached is true when the result came from the cache.
	Cached bool
}

// EvaluationService answers "is this flag enabled for this user?" using a
// cache-aside strategy.
//
// Cache key:  eval:{flag}:{user}. Flag names cannot contain ':', so the key is
// unambiguous even when a user id does.
//
// Tags: one FlagTag per flag the evaluation read (the flag itself plus any
// prerequisite). FlagService invalidates a flag's tag on every write, which
// drops every cached result that could have changed.
//
// Misses follow the epoch protocol (see pkg/cache): read the epoch before
// reading the flag, evaluate, then store only if no invalidation happened in
// between. This closes the race where a slow reader would otherwise store a
// result computed from the old flag after the writer already invalidated.
//
// Errors are never cached. In particular "flag not found" is not cached, so a
// caller probing random names cannot fill the cache with junk keys.
//
// The TTL is only a safety net for missed invalidations, for example after a
// manual edit of the data file; correctness comes from tag invalidation.
type EvaluationService struct {
	eval  FlagEvaluator
	cache EvalCache
	ttl   time.Duration
}

// NewEvaluationService builds an EvaluationService. A ttl of zero or less uses
// the cache's default TTL.
func NewEvaluationService(eval FlagEvaluator, cache EvalCache, ttl time.Duration) (*EvaluationService, error) {
	if eval == nil {
		return nil, errors.New("service: evaluator must not be nil")
	}
	if cache == nil {
		return nil, errors.New("service: cache must not be nil")
	}
	return &EvaluationService{eval: eval, cache: cache, ttl: ttl}, nil
}

// Evaluate returns whether flagName is enabled for userID. It returns an error
// wrapping domain.ErrInvalid for a malformed name or user id, and one wrapping
// domain.ErrNotFound when the flag does not exist.
func (s *EvaluationService) Evaluate(ctx context.Context, flagName, userID string) (Evaluation, error) {
	// Validate before touching the cache so malformed input never becomes a key.
	if err := domain.ValidateName(flagName); err != nil {
		return Evaluation{}, err
	}
	if err := domain.ValidateUserID(userID); err != nil {
		return Evaluation{}, err
	}

	log := logger.FromContext(ctx)
	key := cacheKey(flagName, userID)

	if v, ok := s.cache.Get(key); ok {
		if res, ok := v.(evaluator.Result); ok {
			log.Debug("evaluation cache hit", "flag", flagName, "user", userID)
			return Evaluation{Result: res, Cached: true}, nil
		}
		log.Warn("evaluation cache held an unexpected value type; recomputing", "key", key)
	}

	epoch := s.cache.Epoch()
	res, err := s.eval.Evaluate(ctx, flagName, userID)
	if err != nil {
		return Evaluation{}, err
	}

	tags := make([]string, len(res.Consulted))
	for i, name := range res.Consulted {
		tags[i] = FlagTag(name)
	}
	if s.cache.SetIfEpoch(key, res, s.ttl, epoch, tags...) {
		log.Debug("evaluation cache miss, stored", "flag", flagName, "user", userID, "reason", string(res.Reason))
	} else {
		log.Debug("evaluation cache miss, not stored: flag changed during evaluation", "flag", flagName, "user", userID)
	}
	return Evaluation{Result: res}, nil
}

func cacheKey(flagName, userID string) string {
	return "eval:" + flagName + ":" + userID
}
