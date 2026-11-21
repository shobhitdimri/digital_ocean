// Package dao defines the data access contracts the service layer depends on.
// Concrete stores (file, in-memory, and later Postgres) live in sub-packages
// and are chosen only in the composition root.
package dao

import (
	"context"

	"feature-flag-service/internal/domain"
)

// FlagDAO stores feature flags.
//
// Contract, enforced for every implementation by the tests in package daotest:
//
//   - Methods return ctx.Err() when the context is already cancelled.
//   - Flags are copied in and out. Callers may freely mutate what they pass in
//     or receive without affecting stored state.
//   - Errors wrap the sentinels in package domain, so callers use errors.Is.
type FlagDAO interface {
	// Create stores a new flag. It returns domain.ErrInvalid when the flag
	// fails validation and domain.ErrAlreadyExists when the name is taken.
	Create(ctx context.Context, f domain.Flag) error

	// Get returns the flag, or domain.ErrNotFound.
	Get(ctx context.Context, name string) (domain.Flag, error)

	// List returns every flag ordered by name. The result is never nil.
	List(ctx context.Context) ([]domain.Flag, error)

	// Update replaces a stored flag using optimistic concurrency: f.Version
	// must equal the stored version plus one, otherwise it returns
	// domain.ErrConflict and stores nothing. This stops two concurrent writers
	// from silently overwriting each other, and keeps audit before and after
	// snapshots truthful. It returns domain.ErrNotFound for an unknown name
	// and domain.ErrInvalid for an invalid flag.
	Update(ctx context.Context, f domain.Flag) error

	// Delete removes a flag, or returns domain.ErrNotFound.
	Delete(ctx context.Context, name string) error
}
