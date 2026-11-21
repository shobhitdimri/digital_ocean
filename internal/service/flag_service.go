package service

import (
	"context"
	"errors"
	"fmt"

	"feature-flag-service/internal/dao"
	"feature-flag-service/internal/domain"
	"feature-flag-service/pkg/logger"
)

// maxConflictRetries bounds the optimistic-concurrency retry loop. Within one
// process the per-flag lock makes conflicts impossible, so a retry only happens
// when another process or an external edit changed the flag.
const maxConflictRetries = 3

// ErrAuditFailed means a change was applied but its audit entry could not be
// stored. The change itself is NOT rolled back: the full entry is logged at
// error level so it can be recovered, and the caller gets this error so the
// failure is never silent.
var ErrAuditFailed = errors.New("change applied but its audit entry could not be recorded")

// FlagUpdate is a partial update. A nil field is left unchanged.
type FlagUpdate struct {
	Description    *string
	Enabled        *bool
	RolloutPercent *int
}

// FlagService manages flags and records an audit entry for every change.
//
// Write ordering, for every mutation:
//
//  1. take the per-flag lock (serialises writers of one flag and keeps audit
//     order identical to version order)
//  2. read the current flag and build the next version
//  3. store it through the DAO, using the optimistic version check
//  4. invalidate cached evaluations that depend on the flag
//  5. append the audit entry
//
// Invalidation comes before the audit append so the window in which readers can
// see stale cached results is as short as possible. Steps 4 and 5 run on a
// context detached from cancellation: once the change is committed, a client
// disconnecting must not leave a stale cache or a missing audit entry.
//
// Lock ordering is service lock -> DAO and cache locks. Neither the DAOs nor the
// cache ever call back into the service, so no cycle is possible.
type FlagService struct {
	flags dao.FlagDAO
	audit dao.AuditWriter
	cache Invalidator
	clock Clock
	ids   IDGenerator
	locks *keyedLock
}

// Option customises a service.
type Option func(*FlagService)

// WithClock replaces the time source.
func WithClock(c Clock) Option { return func(s *FlagService) { s.clock = c } }

// WithIDGenerator replaces the audit entry id generator.
func WithIDGenerator(g IDGenerator) Option { return func(s *FlagService) { s.ids = g } }

// NewFlagService builds a FlagService. All three dependencies are required.
func NewFlagService(flags dao.FlagDAO, audit dao.AuditWriter, cache Invalidator, opts ...Option) (*FlagService, error) {
	if flags == nil {
		return nil, errors.New("service: flag DAO must not be nil")
	}
	if audit == nil {
		return nil, errors.New("service: audit writer must not be nil")
	}
	if cache == nil {
		return nil, errors.New("service: cache invalidator must not be nil")
	}
	s := &FlagService{
		flags: flags,
		audit: audit,
		cache: cache,
		clock: SystemClock{},
		ids:   RandomIDGenerator{},
		locks: newKeyedLock(),
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.clock == nil || s.ids == nil {
		return nil, errors.New("service: clock and id generator must not be nil")
	}
	return s, nil
}

// Create adds a new flag at version 1. It returns domain.ErrInvalid for a bad
// name or description and domain.ErrAlreadyExists when the name is taken.
func (s *FlagService) Create(ctx context.Context, name, description string, enabled bool, opts ...domain.FlagOption) (domain.Flag, error) {
	flag, err := domain.NewFlag(name, description, enabled, s.clock.Now(), opts...)
	if err != nil {
		return domain.Flag{}, err
	}
	who, err := callerOf(ctx)
	if err != nil {
		return domain.Flag{}, err
	}

	unlock, err := s.locks.Lock(ctx, name)
	if err != nil {
		return domain.Flag{}, err
	}
	defer unlock()

	if err := s.flags.Create(ctx, flag); err != nil {
		return domain.Flag{}, err
	}
	// Evaluations may have cached a result that depended on this flag being
	// absent (for example a prerequisite that did not exist yet).
	s.cache.InvalidateTag(FlagTag(name))

	if err := s.record(ctx, who, domain.ActionFlagCreated, nil, &flag); err != nil {
		return domain.Flag{}, err
	}
	return flag, nil
}

// Get returns one flag, or domain.ErrNotFound.
func (s *FlagService) Get(ctx context.Context, name string) (domain.Flag, error) {
	if err := domain.ValidateName(name); err != nil {
		return domain.Flag{}, err
	}
	return s.flags.Get(ctx, name)
}

// List returns every flag ordered by name.
func (s *FlagService) List(ctx context.Context) ([]domain.Flag, error) {
	return s.flags.List(ctx)
}

// Update changes the description, global state and/or rollout percentage.
// Setting values equal to the current ones is a no-op: nothing is written and no
// audit entry is made.
func (s *FlagService) Update(ctx context.Context, name string, u FlagUpdate) (domain.Flag, error) {
	if u.Description == nil && u.Enabled == nil && u.RolloutPercent == nil {
		return domain.Flag{}, domain.Invalidf("nothing to update: provide description, enabled and/or rollout_percent")
	}
	if u.RolloutPercent != nil {
		if err := domain.ValidateRolloutPercent(*u.RolloutPercent); err != nil {
			return domain.Flag{}, err
		}
	}
	return s.mutate(ctx, name, domain.ActionFlagUpdated, func(f *domain.Flag) (bool, error) {
		changed := false
		if u.Description != nil && *u.Description != f.Description {
			f.Description = *u.Description
			changed = true
		}
		if u.Enabled != nil && *u.Enabled != f.Enabled {
			f.Enabled = *u.Enabled
			changed = true
		}
		if u.RolloutPercent != nil && *u.RolloutPercent != f.Rollout() {
			if err := f.SetRollout(*u.RolloutPercent); err != nil {
				return false, err
			}
			changed = true
		}
		return changed, nil
	})
}

// SetOverride sets (or replaces) a per-user override. Setting the value the
// user already has is a no-op.
func (s *FlagService) SetOverride(ctx context.Context, name, userID string, enabled bool) (domain.Flag, error) {
	if err := domain.ValidateUserID(userID); err != nil {
		return domain.Flag{}, err
	}
	return s.mutate(ctx, name, domain.ActionOverrideSet, func(f *domain.Flag) (bool, error) {
		if current, ok := f.OverrideFor(userID); ok && current == enabled {
			return false, nil
		}
		if err := f.SetOverride(userID, enabled); err != nil {
			return false, err
		}
		return true, nil
	})
}

// RemoveOverride deletes a per-user override. It returns domain.ErrNotFound
// when the user has none.
func (s *FlagService) RemoveOverride(ctx context.Context, name, userID string) (domain.Flag, error) {
	if err := domain.ValidateUserID(userID); err != nil {
		return domain.Flag{}, err
	}
	return s.mutate(ctx, name, domain.ActionOverrideRemove, func(f *domain.Flag) (bool, error) {
		if !f.RemoveOverride(userID) {
			return false, domain.NotFoundf("flag %q has no override for user %q", name, userID)
		}
		return true, nil
	})
}

// Delete removes a flag. Its audit history is kept.
func (s *FlagService) Delete(ctx context.Context, name string) error {
	if err := domain.ValidateName(name); err != nil {
		return err
	}
	who, err := callerOf(ctx)
	if err != nil {
		return err
	}

	unlock, err := s.locks.Lock(ctx, name)
	if err != nil {
		return err
	}
	defer unlock()

	before, err := s.flags.Get(ctx, name)
	if err != nil {
		return err
	}
	if err := s.flags.Delete(ctx, name); err != nil {
		return err
	}
	s.cache.InvalidateTag(FlagTag(name))
	return s.record(ctx, who, domain.ActionFlagDeleted, &before, nil)
}

// mutate runs one read-modify-write cycle on a flag. apply edits the copy it is
// given and reports whether anything changed; an error from apply aborts the
// cycle untouched.
func (s *FlagService) mutate(ctx context.Context, name string, action domain.Action, apply func(*domain.Flag) (bool, error)) (domain.Flag, error) {
	if err := domain.ValidateName(name); err != nil {
		return domain.Flag{}, err
	}
	who, err := callerOf(ctx)
	if err != nil {
		return domain.Flag{}, err
	}

	unlock, err := s.locks.Lock(ctx, name)
	if err != nil {
		return domain.Flag{}, err
	}
	defer unlock()

	for attempt := 0; attempt < maxConflictRetries; attempt++ {
		before, err := s.flags.Get(ctx, name)
		if err != nil {
			return domain.Flag{}, err
		}

		next := before.Clone()
		changed, err := apply(&next)
		if err != nil {
			return domain.Flag{}, err
		}
		if !changed {
			return before, nil
		}
		next.Version = before.Version + 1
		next.UpdatedAt = s.clock.Now()
		if err := next.Validate(); err != nil {
			return domain.Flag{}, err
		}

		err = s.flags.Update(ctx, next)
		if errors.Is(err, domain.ErrConflict) {
			continue
		}
		if err != nil {
			return domain.Flag{}, err
		}

		s.cache.InvalidateTag(FlagTag(name))
		if err := s.record(ctx, who, action, &before, &next); err != nil {
			return domain.Flag{}, err
		}
		return next, nil
	}
	return domain.Flag{}, domain.Conflictf("flag %q was modified concurrently, retry the request", name)
}

// record appends the audit entry for a committed change.
func (s *FlagService) record(ctx context.Context, who caller, action domain.Action, before, after *domain.Flag) error {
	ctx = context.WithoutCancel(ctx)

	name := ""
	switch {
	case after != nil:
		name = after.Name
	case before != nil:
		name = before.Name
	}

	entry := domain.AuditEntry{
		ID:        s.ids.NewID(),
		FlagName:  name,
		Actor:     who.actor,
		Action:    action,
		RequestID: who.requestID,
		At:        s.clock.Now(),
	}
	var err error
	if entry.Before, err = domain.Snapshot(before); err != nil {
		return s.auditFailed(ctx, entry, err)
	}
	if entry.After, err = domain.Snapshot(after); err != nil {
		return s.auditFailed(ctx, entry, err)
	}
	if err := s.audit.Append(ctx, entry); err != nil {
		return s.auditFailed(ctx, entry, err)
	}
	return nil
}

func (s *FlagService) auditFailed(ctx context.Context, entry domain.AuditEntry, cause error) error {
	logger.FromContext(ctx).Error("audit entry could not be recorded; change was applied",
		"flag", entry.FlagName,
		"action", string(entry.Action),
		"audit_id", entry.ID,
		"before", string(entry.Before),
		"after", string(entry.After),
		"error", cause,
	)
	return fmt.Errorf("%w: flag %q: %w", ErrAuditFailed, entry.FlagName, cause)
}

// caller identifies who is making a request, for the audit trail.
type caller struct {
	actor     string
	requestID string
}

// callerOf reads the actor and request id that the HTTP middleware stored in
// the context. A request with no actor is recorded as anonymous.
func callerOf(ctx context.Context) (caller, error) {
	c := caller{actor: logger.Actor(ctx), requestID: logger.RequestID(ctx)}
	if c.actor == "" {
		c.actor = logger.AnonymousActor
	}
	if err := domain.ValidateActor(c.actor); err != nil {
		return caller{}, err
	}
	return c, nil
}
