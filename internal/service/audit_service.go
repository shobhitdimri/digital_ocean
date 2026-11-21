package service

import (
	"context"
	"errors"

	"feature-flag-service/internal/dao"
	"feature-flag-service/internal/domain"
)

// Audit history page sizes.
const (
	DefaultAuditLimit = 50
	MaxAuditLimit     = 1000
)

// AuditService reads the audit trail. It depends only on the read half of the
// audit store (interface segregation), so it cannot alter history.
type AuditService struct {
	reader dao.AuditReader
}

// NewAuditService builds an AuditService.
func NewAuditService(reader dao.AuditReader) (*AuditService, error) {
	if reader == nil {
		return nil, errors.New("service: audit reader must not be nil")
	}
	return &AuditService{reader: reader}, nil
}

// History returns the newest-first audit entries for a flag. A limit of zero
// uses DefaultAuditLimit, a negative limit is invalid, and a limit above
// MaxAuditLimit is capped. History of a deleted flag is still available, and an
// unknown flag simply has an empty history, so this never returns not-found.
func (s *AuditService) History(ctx context.Context, flagName string, limit int) ([]domain.AuditEntry, error) {
	if err := domain.ValidateName(flagName); err != nil {
		return nil, err
	}
	switch {
	case limit < 0:
		return nil, domain.Invalidf("limit must not be negative")
	case limit == 0:
		limit = DefaultAuditLimit
	case limit > MaxAuditLimit:
		limit = MaxAuditLimit
	}
	return s.reader.ListByFlag(ctx, flagName, limit)
}
