// Package daotest holds contract tests shared by every DAO implementation.
// An implementation is correct when it passes these, which is what makes the
// in-memory, file and (later) database stores interchangeable.
package daotest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"feature-flag-service/internal/dao"
	"feature-flag-service/internal/domain"
)

var baseTime = time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)

func newFlag(t *testing.T, name string) domain.Flag {
	t.Helper()
	f, err := domain.NewFlag(name, "description of "+name, false, baseTime)
	if err != nil {
		t.Fatalf("NewFlag(%q): %v", name, err)
	}
	return f
}

func sameFlag(a, b domain.Flag) bool {
	return a.Name == b.Name &&
		a.Description == b.Description &&
		a.Enabled == b.Enabled &&
		a.Version == b.Version &&
		a.CreatedAt.Equal(b.CreatedAt) &&
		a.UpdatedAt.Equal(b.UpdatedAt) &&
		reflect.DeepEqual(a.Overrides, b.Overrides)
}

func cancelled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// RunFlagDAOContract runs the dao.FlagDAO contract. newDAO must return a fresh,
// empty store on every call.
func RunFlagDAOContract(t *testing.T, newDAO func(t *testing.T) dao.FlagDAO) {
	ctx := context.Background()

	t.Run("create then get", func(t *testing.T) {
		d := newDAO(t)
		f := newFlag(t, "new-ui")
		_ = f.SetOverride("alice", true)
		if err := d.Create(ctx, f); err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := d.Get(ctx, "new-ui")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !sameFlag(got, f) {
			t.Fatalf("Get returned %+v, want %+v", got, f)
		}
	})

	t.Run("get missing is not found", func(t *testing.T) {
		if _, err := newDAO(t).Get(ctx, "ghost"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("create duplicate is already exists", func(t *testing.T) {
		d := newDAO(t)
		f := newFlag(t, "dup")
		if err := d.Create(ctx, f); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := d.Create(ctx, f); !errors.Is(err, domain.ErrAlreadyExists) {
			t.Fatalf("want ErrAlreadyExists, got %v", err)
		}
	})

	t.Run("create invalid flag is rejected", func(t *testing.T) {
		d := newDAO(t)
		if err := d.Create(ctx, domain.Flag{Name: "has space", Version: 1}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("want ErrInvalid, got %v", err)
		}
		if list, _ := d.List(ctx); len(list) != 0 {
			t.Fatalf("invalid flag must not be stored, list = %v", list)
		}
	})

	t.Run("list is empty non-nil then sorted by name", func(t *testing.T) {
		d := newDAO(t)
		empty, err := d.List(ctx)
		if err != nil || empty == nil || len(empty) != 0 {
			t.Fatalf("empty List = %v, %v; want non-nil empty slice", empty, err)
		}
		for _, n := range []string{"charlie", "alpha", "bravo"} {
			if err := d.Create(ctx, newFlag(t, n)); err != nil {
				t.Fatalf("Create %s: %v", n, err)
			}
		}
		list, err := d.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		var names []string
		for _, f := range list {
			names = append(names, f.Name)
		}
		if !reflect.DeepEqual(names, []string{"alpha", "bravo", "charlie"}) {
			t.Fatalf("List order = %v", names)
		}
	})

	t.Run("update replaces the flag when the version advances by one", func(t *testing.T) {
		d := newDAO(t)
		f := newFlag(t, "toggle")
		if err := d.Create(ctx, f); err != nil {
			t.Fatalf("Create: %v", err)
		}
		next := f.Clone()
		next.Enabled = true
		next.Description = "now on"
		next.Version = 2
		next.UpdatedAt = baseTime.Add(time.Hour)
		if err := d.Update(ctx, next); err != nil {
			t.Fatalf("Update: %v", err)
		}
		got, _ := d.Get(ctx, "toggle")
		if !sameFlag(got, next) {
			t.Fatalf("after Update got %+v, want %+v", got, next)
		}
	})

	t.Run("update missing is not found", func(t *testing.T) {
		f := newFlag(t, "ghost")
		f.Version = 2
		if err := newDAO(t).Update(ctx, f); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("update with a stale or skipped version conflicts and stores nothing", func(t *testing.T) {
		d := newDAO(t)
		f := newFlag(t, "guarded")
		if err := d.Create(ctx, f); err != nil {
			t.Fatalf("Create: %v", err)
		}
		for _, v := range []int64{0, 1, 3} {
			attempt := f.Clone()
			attempt.Enabled = true
			attempt.Version = v
			if err := d.Update(ctx, attempt); !errors.Is(err, domain.ErrConflict) {
				t.Fatalf("version %d: want ErrConflict, got %v", v, err)
			}
		}
		got, _ := d.Get(ctx, "guarded")
		if !sameFlag(got, f) {
			t.Fatalf("conflicting updates changed stored flag: %+v", got)
		}
	})

	t.Run("update with invalid flag is rejected", func(t *testing.T) {
		d := newDAO(t)
		f := newFlag(t, "valid")
		if err := d.Create(ctx, f); err != nil {
			t.Fatalf("Create: %v", err)
		}
		bad := f.Clone()
		bad.Version = 2
		bad.Overrides = map[string]bool{"bad user": true}
		if err := d.Update(ctx, bad); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("want ErrInvalid, got %v", err)
		}
	})

	t.Run("delete removes the flag and the name can be reused", func(t *testing.T) {
		d := newDAO(t)
		f := newFlag(t, "temp")
		if err := d.Create(ctx, f); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := d.Delete(ctx, "temp"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := d.Get(ctx, "temp"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("Get after Delete: want ErrNotFound, got %v", err)
		}
		if err := d.Create(ctx, f); err != nil {
			t.Fatalf("re-Create after Delete: %v", err)
		}
	})

	t.Run("delete missing is not found", func(t *testing.T) {
		if err := newDAO(t).Delete(ctx, "ghost"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("stored state is isolated from caller mutation", func(t *testing.T) {
		d := newDAO(t)
		f := newFlag(t, "iso")
		_ = f.SetOverride("alice", true)
		if err := d.Create(ctx, f); err != nil {
			t.Fatalf("Create: %v", err)
		}

		f.Overrides["alice"] = false
		f.Overrides["mallory"] = true

		got, _ := d.Get(ctx, "iso")
		if !got.Overrides["alice"] || len(got.Overrides) != 1 {
			t.Fatalf("mutating the input leaked into the store: %v", got.Overrides)
		}

		got.Overrides["alice"] = false
		again, _ := d.Get(ctx, "iso")
		if !again.Overrides["alice"] {
			t.Fatal("mutating a returned flag leaked into the store")
		}

		list, _ := d.List(ctx)
		list[0].Overrides["alice"] = false
		again, _ = d.Get(ctx, "iso")
		if !again.Overrides["alice"] {
			t.Fatal("mutating a listed flag leaked into the store")
		}
	})

	t.Run("cancelled context is honoured", func(t *testing.T) {
		d := newDAO(t)
		f := newFlag(t, "ctx")
		if err := d.Create(cancelled(), f); !errors.Is(err, context.Canceled) {
			t.Errorf("Create: want context.Canceled, got %v", err)
		}
		if _, err := d.Get(cancelled(), "ctx"); !errors.Is(err, context.Canceled) {
			t.Errorf("Get: want context.Canceled, got %v", err)
		}
		if _, err := d.List(cancelled()); !errors.Is(err, context.Canceled) {
			t.Errorf("List: want context.Canceled, got %v", err)
		}
		if err := d.Update(cancelled(), f); !errors.Is(err, context.Canceled) {
			t.Errorf("Update: want context.Canceled, got %v", err)
		}
		if err := d.Delete(cancelled(), "ctx"); !errors.Is(err, context.Canceled) {
			t.Errorf("Delete: want context.Canceled, got %v", err)
		}
	})

	t.Run("concurrent creates all land", func(t *testing.T) {
		d := newDAO(t)
		const n = 30
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if err := d.Create(ctx, newFlag(t, fmt.Sprintf("flag-%02d", i))); err != nil {
					t.Errorf("Create %d: %v", i, err)
				}
			}(i)
		}
		wg.Wait()
		if list, _ := d.List(ctx); len(list) != n {
			t.Fatalf("stored %d flags, want %d", len(list), n)
		}
	})

	t.Run("exactly one of many racing updates wins", func(t *testing.T) {
		d := newDAO(t)
		f := newFlag(t, "race")
		if err := d.Create(ctx, f); err != nil {
			t.Fatalf("Create: %v", err)
		}

		const writers = 25
		var wins, conflicts atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				next := f.Clone()
				next.Version = 2
				next.Description = fmt.Sprintf("writer %d", i)
				switch err := d.Update(ctx, next); {
				case err == nil:
					wins.Add(1)
				case errors.Is(err, domain.ErrConflict):
					conflicts.Add(1)
				default:
					t.Errorf("unexpected error: %v", err)
				}
			}(i)
		}
		wg.Wait()
		if wins.Load() != 1 || conflicts.Load() != writers-1 {
			t.Fatalf("wins=%d conflicts=%d, want 1 and %d", wins.Load(), conflicts.Load(), writers-1)
		}
	})
}

func auditEntry(t *testing.T, id, flag, actor string, action domain.Action) domain.AuditEntry {
	t.Helper()
	before, _ := domain.Snapshot(nil)
	f := newFlag(t, flag)
	after, err := domain.Snapshot(&f)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return domain.AuditEntry{
		ID:        id,
		FlagName:  flag,
		Actor:     actor,
		Action:    action,
		Before:    before,
		After:     after,
		RequestID: "req-" + id,
		At:        baseTime,
	}
}

// RunAuditDAOContract runs the dao.AuditDAO contract. newDAO must return a
// fresh, empty store on every call.
func RunAuditDAOContract(t *testing.T, newDAO func(t *testing.T) dao.AuditDAO) {
	ctx := context.Background()

	t.Run("append then list preserves every field", func(t *testing.T) {
		d := newDAO(t)
		e := auditEntry(t, "1", "new-ui", "alice", domain.ActionFlagCreated)
		if err := d.Append(ctx, e); err != nil {
			t.Fatalf("Append: %v", err)
		}
		got, err := d.ListByFlag(ctx, "new-ui", 0)
		if err != nil || len(got) != 1 {
			t.Fatalf("ListByFlag = %v, %v; want one entry", got, err)
		}
		g := got[0]
		if g.ID != e.ID || g.FlagName != e.FlagName || g.Actor != e.Actor || g.Action != e.Action ||
			g.RequestID != e.RequestID || !g.At.Equal(e.At) {
			t.Fatalf("entry mismatch:\n got %+v\nwant %+v", g, e)
		}
		if string(g.Before) != "null" {
			t.Fatalf("Before = %s, want null", g.Before)
		}
		var after domain.Flag
		if err := json.Unmarshal(g.After, &after); err != nil || after.Name != "new-ui" {
			t.Fatalf("After did not round trip: %s (%v)", g.After, err)
		}
	})

	t.Run("lists newest first and only for the requested flag", func(t *testing.T) {
		d := newDAO(t)
		for _, e := range []domain.AuditEntry{
			auditEntry(t, "1", "a", "alice", domain.ActionFlagCreated),
			auditEntry(t, "2", "b", "bob", domain.ActionFlagCreated),
			auditEntry(t, "3", "a", "alice", domain.ActionFlagUpdated),
			auditEntry(t, "4", "a", "carol", domain.ActionFlagDeleted),
		} {
			if err := d.Append(ctx, e); err != nil {
				t.Fatalf("Append %s: %v", e.ID, err)
			}
		}
		got, _ := d.ListByFlag(ctx, "a", 0)
		var ids []string
		for _, e := range got {
			ids = append(ids, e.ID)
		}
		if !reflect.DeepEqual(ids, []string{"4", "3", "1"}) {
			t.Fatalf("ids = %v, want [4 3 1]", ids)
		}
	})

	t.Run("limit keeps the newest entries", func(t *testing.T) {
		d := newDAO(t)
		for i := 1; i <= 5; i++ {
			if err := d.Append(ctx, auditEntry(t, fmt.Sprint(i), "a", "alice", domain.ActionFlagUpdated)); err != nil {
				t.Fatalf("Append: %v", err)
			}
		}
		got, _ := d.ListByFlag(ctx, "a", 2)
		if len(got) != 2 || got[0].ID != "5" || got[1].ID != "4" {
			t.Fatalf("limit 2 returned %v", got)
		}
		for _, limit := range []int{0, -1} {
			if all, _ := d.ListByFlag(ctx, "a", limit); len(all) != 5 {
				t.Fatalf("limit %d returned %d entries, want all 5", limit, len(all))
			}
		}
	})

	t.Run("unknown flag yields an empty non-nil slice", func(t *testing.T) {
		got, err := newDAO(t).ListByFlag(ctx, "ghost", 10)
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("ListByFlag = %v, %v; want empty non-nil slice", got, err)
		}
	})

	t.Run("invalid entry is rejected and not stored", func(t *testing.T) {
		d := newDAO(t)
		bad := auditEntry(t, "1", "a", "", domain.ActionFlagCreated)
		if err := d.Append(ctx, bad); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("want ErrInvalid, got %v", err)
		}
		if got, _ := d.ListByFlag(ctx, "a", 0); len(got) != 0 {
			t.Fatalf("invalid entry was stored: %v", got)
		}
	})

	t.Run("history outlives the flag it describes", func(t *testing.T) {
		d := newDAO(t)
		_ = d.Append(ctx, auditEntry(t, "1", "gone", "alice", domain.ActionFlagCreated))
		_ = d.Append(ctx, auditEntry(t, "2", "gone", "alice", domain.ActionFlagDeleted))
		if got, _ := d.ListByFlag(ctx, "gone", 0); len(got) != 2 {
			t.Fatalf("expected both entries to remain, got %d", len(got))
		}
	})

	t.Run("cancelled context is honoured", func(t *testing.T) {
		d := newDAO(t)
		if err := d.Append(cancelled(), auditEntry(t, "1", "a", "alice", domain.ActionFlagCreated)); !errors.Is(err, context.Canceled) {
			t.Errorf("Append: want context.Canceled, got %v", err)
		}
		if _, err := d.ListByFlag(cancelled(), "a", 0); !errors.Is(err, context.Canceled) {
			t.Errorf("ListByFlag: want context.Canceled, got %v", err)
		}
	})

	t.Run("concurrent appends are all recorded", func(t *testing.T) {
		d := newDAO(t)
		const n = 40
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if err := d.Append(ctx, auditEntry(t, fmt.Sprint(i), "busy", "alice", domain.ActionFlagUpdated)); err != nil {
					t.Errorf("Append %d: %v", i, err)
				}
			}(i)
		}
		wg.Wait()
		got, _ := d.ListByFlag(ctx, "busy", 0)
		if len(got) != n {
			t.Fatalf("recorded %d entries, want %d", len(got), n)
		}
		seen := map[string]bool{}
		for _, e := range got {
			seen[e.ID] = true
		}
		if len(seen) != n {
			t.Fatalf("entries were lost or duplicated: %d distinct ids", len(seen))
		}
	})
}
