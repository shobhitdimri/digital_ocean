package file

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"feature-flag-service/internal/dao"
	"feature-flag-service/internal/dao/daotest"
	"feature-flag-service/internal/domain"
)

var ctx = context.Background()

func TestFlagDAOContract(t *testing.T) {
	daotest.RunFlagDAOContract(t, func(t *testing.T) dao.FlagDAO {
		d, err := NewFlagDAO(filepath.Join(t.TempDir(), "flags.json"))
		if err != nil {
			t.Fatalf("NewFlagDAO: %v", err)
		}
		return d
	})
}

func TestAuditDAOContract(t *testing.T) {
	daotest.RunAuditDAOContract(t, func(t *testing.T) dao.AuditDAO {
		d, err := NewAuditDAO(filepath.Join(t.TempDir(), "audit.jsonl"))
		if err != nil {
			t.Fatalf("NewAuditDAO: %v", err)
		}
		return d
	})
}

func mustFlag(t *testing.T, name string) domain.Flag {
	t.Helper()
	f, err := domain.NewFlag(name, "desc "+name, true, time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NewFlag: %v", err)
	}
	return f
}

func TestFlagsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "flags.json")

	d1, err := NewFlagDAO(path)
	if err != nil {
		t.Fatalf("NewFlagDAO: %v", err)
	}
	f := mustFlag(t, "persisted")
	_ = f.SetOverride("alice", false)
	if err := d1.Create(ctx, f); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := d1.Create(ctx, mustFlag(t, "second")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	next := f.Clone()
	next.Version = 2
	next.Enabled = false
	if err := d1.Update(ctx, next); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := d1.Delete(ctx, "second"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	d2, err := NewFlagDAO(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	list, _ := d2.List(ctx)
	if len(list) != 1 {
		t.Fatalf("after restart got %d flags, want 1: %+v", len(list), list)
	}
	got := list[0]
	if got.Name != "persisted" || got.Version != 2 || got.Enabled || got.Overrides["alice"] {
		t.Fatalf("restart lost state: %+v", got)
	}
	if v, ok := got.OverrideFor("alice"); !ok || v {
		t.Fatalf("override not preserved: %v %v", v, ok)
	}
}

func TestFlagFileFormatAndNoTempLeftovers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "flags.json")
	d, _ := NewFlagDAO(path)
	for _, n := range []string{"b", "a"} {
		if err := d.Create(ctx, mustFlag(t, n)); err != nil {
			t.Fatal(err)
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		SchemaVersion int `json:"schema_version"`
		Flags         []struct {
			Name string `json:"name"`
		} `json:"flags"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("file is not valid JSON: %v", err)
	}
	if doc.SchemaVersion != 1 || len(doc.Flags) != 2 || doc.Flags[0].Name != "a" || doc.Flags[1].Name != "b" {
		t.Fatalf("unexpected file contents: %s", raw)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "flags.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("temp files left behind: %v", names)
	}
}

func TestFlagStoreStartupValidation(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		wantErr  string
	}{
		{"corrupt json", `{"schema_version": 1, "flags": [`, "corrupt"},
		{"newer schema", `{"schema_version": 99, "flags": []}`, "schema version 99"},
		{"invalid flag", `{"schema_version":1,"flags":[{"name":"bad name","version":1}]}`, "invalid flag"},
		{"duplicate names", `{"schema_version":1,"flags":[{"name":"x","version":1},{"name":"x","version":1}]}`, "duplicate"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "flags.json")
			if err := os.WriteFile(path, []byte(tc.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := NewFlagDAO(path)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
			if after, _ := os.ReadFile(path); string(after) != tc.contents {
				t.Fatal("a refused file must be left untouched")
			}
		})
	}
}

func TestEmptyOrMissingFileIsEmptyStore(t *testing.T) {
	dir := t.TempDir()

	missing, err := NewFlagDAO(filepath.Join(dir, "missing.json"))
	if err != nil {
		t.Fatalf("missing file: %v", err)
	}
	if list, _ := missing.List(ctx); len(list) != 0 {
		t.Fatalf("want empty store, got %v", list)
	}

	emptyPath := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(emptyPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFlagDAO(emptyPath); err != nil {
		t.Fatalf("zero-byte file should open as an empty store: %v", err)
	}
}

func TestDiskFailureRollsBackAndReportsError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission checks do not apply to root")
	}
	dir := t.TempDir()
	d, err := NewFlagDAO(filepath.Join(dir, "flags.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Create(ctx, mustFlag(t, "keep")); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err = d.Create(ctx, mustFlag(t, "doomed"))
	if err == nil {
		t.Fatal("Create should fail when the directory is read-only")
	}
	if errors.Is(err, domain.ErrAlreadyExists) || errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("a disk failure must not look like a client error: %v", err)
	}
	if _, err := d.Get(ctx, "doomed"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("memory must not hold a flag the disk rejected, Get = %v", err)
	}
	if _, err := d.Get(ctx, "keep"); err != nil {
		t.Fatalf("existing flag must be untouched: %v", err)
	}
}

func newAudit(t *testing.T) (*AuditDAO, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit", "audit.jsonl")
	d, err := NewAuditDAO(path)
	if err != nil {
		t.Fatal(err)
	}
	return d, path
}

func entry(t *testing.T, id, flag string) domain.AuditEntry {
	t.Helper()
	f := mustFlag(t, flag)
	after, _ := domain.Snapshot(&f)
	before, _ := domain.Snapshot(nil)
	return domain.AuditEntry{
		ID: id, FlagName: flag, Actor: "alice", Action: domain.ActionFlagCreated,
		Before: before, After: after, At: time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC),
	}
}

func TestAuditSurvivesRestartAndIsOneJSONObjectPerLine(t *testing.T) {
	d1, path := newAudit(t)
	for i := 1; i <= 3; i++ {
		if err := d1.Append(ctx, entry(t, fmt.Sprint(i), "a")); err != nil {
			t.Fatal(err)
		}
	}

	raw, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d", len(lines))
	}
	for _, l := range lines {
		if !json.Valid([]byte(l)) {
			t.Fatalf("line is not valid JSON: %q", l)
		}
	}

	d2, _ := NewAuditDAO(path)
	got, _ := d2.ListByFlag(ctx, "a", 0)
	if len(got) != 3 || got[0].ID != "3" {
		t.Fatalf("after restart got %v", got)
	}
}

func TestAuditToleratesTornTrailingLine(t *testing.T) {
	d, path := newAudit(t)
	if err := d.Append(ctx, entry(t, "1", "a")); err != nil {
		t.Fatal(err)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"2","flag_name":"a","actor":"ali`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	got, err := d.ListByFlag(ctx, "a", 0)
	if err != nil || len(got) != 1 || got[0].ID != "1" {
		t.Fatalf("reading past a torn tail = %v, %v; want only entry 1", got, err)
	}

	if err := d.Append(ctx, entry(t, "3", "a")); err != nil {
		t.Fatalf("Append after torn tail: %v", err)
	}
	got, _ = d.ListByFlag(ctx, "a", 0)
	if len(got) != 2 || got[0].ID != "3" || got[1].ID != "1" {
		t.Fatalf("new entry must not be glued to the torn fragment, got %v", got)
	}
}

func TestAuditSkipsCorruptMiddleLine(t *testing.T) {
	d, path := newAudit(t)
	_ = d.Append(ctx, entry(t, "1", "a"))

	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString("this is not json\n")
	f.Close()

	_ = d.Append(ctx, entry(t, "2", "a"))

	got, err := d.ListByFlag(ctx, "a", 0)
	if err != nil || len(got) != 2 || got[0].ID != "2" || got[1].ID != "1" {
		t.Fatalf("got %v, %v; want entries 2 and 1 around the corrupt line", got, err)
	}
}

func TestAuditHandlesEntriesLargerThanScannerLimits(t *testing.T) {
	d, _ := newAudit(t)

	big := mustFlag(t, "big")
	for i := 0; i < 5000; i++ {
		_ = big.SetOverride(fmt.Sprintf("user-%05d-with-a-long-identifier-suffix", i), i%2 == 0)
	}
	after, _ := domain.Snapshot(&big)
	if len(after) < 200_000 {
		t.Fatalf("test snapshot too small to be meaningful: %d bytes", len(after))
	}
	e := entry(t, "1", "big")
	e.After = after
	if err := d.Append(ctx, e); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := d.Append(ctx, entry(t, "2", "big")); err != nil {
		t.Fatal(err)
	}

	got, err := d.ListByFlag(ctx, "big", 0)
	if err != nil || len(got) != 2 {
		t.Fatalf("ListByFlag = %d entries, %v; want 2", len(got), err)
	}
	var back domain.Flag
	if err := json.Unmarshal(got[1].After, &back); err != nil || len(back.Overrides) != 5000 {
		t.Fatalf("large snapshot did not round trip: %d overrides, %v", len(back.Overrides), err)
	}
}

func TestAuditMissingFileListsEmpty(t *testing.T) {
	d, path := newAudit(t)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file should not exist before the first Append: %v", err)
	}
	got, err := d.ListByFlag(ctx, "a", 0)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want empty non-nil", got, err)
	}
}
