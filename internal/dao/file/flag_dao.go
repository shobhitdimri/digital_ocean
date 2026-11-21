package file

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"feature-flag-service/internal/dao"
	"feature-flag-service/internal/dao/memory"
	"feature-flag-service/internal/domain"
)

var _ dao.FlagDAO = (*FlagDAO)(nil)

// schemaVersion is bumped whenever the on-disk layout changes, so a newer file
// is refused instead of being misread by an older binary.
const schemaVersion = 1

type flagsFile struct {
	SchemaVersion int           `json:"schema_version"`
	Flags         []domain.Flag `json:"flags"`
}

// FlagDAO is a file-backed dao.FlagDAO.
//
// All flags live in memory, so reads never touch the disk, and every
// successful write persists the full state to a JSON file atomically. If the
// disk write fails, the in-memory change is rolled back and the error is
// returned, so memory and disk never diverge.
//
// A single file is fine for the expected scale (thousands of flags). The
// dao.FlagDAO interface is what lets a database replace it later.
type FlagDAO struct {
	*memory.FlagDAO
}

// NewFlagDAO opens the store at path, creating the parent directory. A missing
// or empty file is an empty store; a corrupt file is an error, because
// silently starting empty would overwrite real data on the next write.
func NewFlagDAO(path string) (*FlagDAO, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("flag store: create directory: %w", err)
	}
	initial, err := loadFlags(path)
	if err != nil {
		return nil, err
	}
	return &FlagDAO{
		FlagDAO: memory.NewFlagDAO(
			memory.WithInitial(initial),
			memory.WithPersist(func(flags []domain.Flag) error { return saveFlags(path, flags) }),
		),
	}, nil
}

func loadFlags(path string) ([]domain.Flag, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("flag store: read %s: %w", path, err)
	}
	if len(raw) == 0 {
		return nil, nil
	}

	var doc flagsFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("flag store: %s is corrupt: %w", path, err)
	}
	if doc.SchemaVersion > schemaVersion {
		return nil, fmt.Errorf("flag store: %s has schema version %d, this build supports up to %d",
			path, doc.SchemaVersion, schemaVersion)
	}

	seen := make(map[string]struct{}, len(doc.Flags))
	for _, f := range doc.Flags {
		if err := f.Validate(); err != nil {
			return nil, fmt.Errorf("flag store: %s contains an invalid flag: %w", path, err)
		}
		if _, dup := seen[f.Name]; dup {
			return nil, fmt.Errorf("flag store: %s contains duplicate flag %q", path, f.Name)
		}
		seen[f.Name] = struct{}{}
	}
	return doc.Flags, nil
}

func saveFlags(path string, flags []domain.Flag) error {
	if flags == nil {
		flags = []domain.Flag{}
	}
	data, err := json.MarshalIndent(flagsFile{SchemaVersion: schemaVersion, Flags: flags}, "", "  ")
	if err != nil {
		return fmt.Errorf("flag store: encode: %w", err)
	}
	if err := writeFileAtomic(path, append(data, '\n')); err != nil {
		return fmt.Errorf("flag store: persist: %w", err)
	}
	return nil
}
