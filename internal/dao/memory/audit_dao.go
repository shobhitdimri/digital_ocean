package memory

import (
	"context"
	"encoding/json"
	"sync"

	"feature-flag-service/internal/dao"
	"feature-flag-service/internal/domain"
)

var _ dao.AuditDAO = (*AuditDAO)(nil)

// AuditDAO is a thread-safe, in-memory dao.AuditDAO.
type AuditDAO struct {
	mu      sync.RWMutex
	entries []domain.AuditEntry
}

// NewAuditDAO creates an empty audit store.
func NewAuditDAO() *AuditDAO {
	return &AuditDAO{}
}

// Append implements dao.AuditDAO.
func (d *AuditDAO) Append(ctx context.Context, e domain.AuditEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := e.Validate(); err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.entries = append(d.entries, cloneEntry(e))
	return nil
}

// ListByFlag implements dao.AuditDAO.
func (d *AuditDAO) ListByFlag(ctx context.Context, flagName string, limit int) ([]domain.AuditEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	d.mu.RLock()
	defer d.mu.RUnlock()

	out := []domain.AuditEntry{}
	for i := len(d.entries) - 1; i >= 0; i-- {
		if d.entries[i].FlagName != flagName {
			continue
		}
		out = append(out, cloneEntry(d.entries[i]))
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

func cloneEntry(e domain.AuditEntry) domain.AuditEntry {
	e.Before = append(json.RawMessage(nil), e.Before...)
	e.After = append(json.RawMessage(nil), e.After...)
	return e
}
