package file

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"feature-flag-service/internal/dao"
	"feature-flag-service/internal/domain"
	"feature-flag-service/pkg/logger"
)

var _ dao.AuditDAO = (*AuditDAO)(nil)

// AuditDAO is a file-backed dao.AuditDAO storing one JSON object per line
// (JSON Lines). The file is append-only: entries are never rewritten.
//
// Each Append is a single write followed by an fsync, so an acknowledged entry
// survives a crash. If a crash tears the final line, the next Append starts on
// a fresh line and readers skip the damaged fragment instead of failing.
//
// ListByFlag scans the file, which is fine for a file-backed store and keeps
// the format trivial to inspect with standard tools. A database-backed DAO
// would index by flag name.
type AuditDAO struct {
	path string
	mu   sync.RWMutex
}

// NewAuditDAO opens the audit trail at path, creating the parent directory.
func NewAuditDAO(path string) (*AuditDAO, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("audit store: create directory: %w", err)
	}
	return &AuditDAO{path: path}, nil
}

// Append implements dao.AuditDAO.
func (d *AuditDAO) Append(ctx context.Context, e domain.AuditEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := e.Validate(); err != nil {
		return err
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("audit store: encode: %w", err)
	}
	line = append(line, '\n')

	d.mu.Lock()
	defer d.mu.Unlock()

	f, err := os.OpenFile(d.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("audit store: open: %w", err)
	}
	defer f.Close()

	// Start on a fresh line if a previous crash left a partial one behind.
	if torn, err := endsMidLine(f); err != nil {
		return fmt.Errorf("audit store: inspect tail: %w", err)
	} else if torn {
		line = append([]byte{'\n'}, line...)
	}

	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("audit store: write: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("audit store: sync: %w", err)
	}
	return nil
}

// ListByFlag implements dao.AuditDAO.
func (d *AuditDAO) ListByFlag(ctx context.Context, flagName string, limit int) ([]domain.AuditEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	d.mu.RLock()
	defer d.mu.RUnlock()

	f, err := os.Open(d.path)
	if errors.Is(err, os.ErrNotExist) {
		return []domain.AuditEntry{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("audit store: open: %w", err)
	}
	defer f.Close()

	var matches []domain.AuditEntry
	log := logger.FromContext(ctx)

	// bufio.Reader rather than Scanner: entries embed full flag snapshots and
	// must not be limited by a fixed token size.
	r := bufio.NewReader(f)
	for lineNo := 1; ; lineNo++ {
		line, readErr := r.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			var e domain.AuditEntry
			if err := json.Unmarshal(trimmed, &e); err != nil {
				log.Warn("skipping unreadable audit line", "path", d.path, "line", lineNo, "err", err)
			} else if e.FlagName == flagName {
				matches = append(matches, e)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("audit store: read: %w", readErr)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}

	out := make([]domain.AuditEntry, 0, len(matches))
	for i := len(matches) - 1; i >= 0; i-- {
		out = append(out, matches[i])
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

// endsMidLine reports whether the file is non-empty and its last byte is not a
// newline.
func endsMidLine(f *os.File) (bool, error) {
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if info.Size() == 0 {
		return false, nil
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, info.Size()-1); err != nil {
		return false, err
	}
	return last[0] != '\n', nil
}
