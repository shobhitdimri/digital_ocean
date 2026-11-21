package dao

import (
	"context"

	"feature-flag-service/internal/domain"
)

// AuditDAO stores the append-only audit trail.
//
// Entries are never modified or removed, and they outlive the flags they
// describe, so the history of a deleted flag stays readable.
type AuditDAO interface {
	// Append durably records an entry. It returns domain.ErrInvalid when the
	// entry fails validation.
	Append(ctx context.Context, e domain.AuditEntry) error

	// ListByFlag returns the entries for one flag, newest first (most recently
	// appended first). A limit of zero or less means no limit. An unknown flag
	// yields an empty, non-nil slice and no error.
	ListByFlag(ctx context.Context, flagName string, limit int) ([]domain.AuditEntry, error)
}
