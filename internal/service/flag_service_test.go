package service_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"feature-flag-service/internal/dao"
	"feature-flag-service/internal/dao/memory"
	"feature-flag-service/internal/domain"
	"feature-flag-service/internal/service"
	"feature-flag-service/pkg/logger"
)

func ptr[T any](v T) *T { return &v }

func TestNewFlagServiceRejectsNilDependencies(t *testing.T) {
	flags, audit, cache := memory.NewFlagDAO(), memory.NewAuditDAO(), &spyInvalidator{}
	tests := []struct {
		name string
		make func() (*service.FlagService, error)
	}{
		{"nil flags", func() (*service.FlagService, error) { return service.NewFlagService(nil, audit, cache) }},
		{"nil audit", func() (*service.FlagService, error) { return service.NewFlagService(flags, nil, cache) }},
		{"nil cache", func() (*service.FlagService, error) { return service.NewFlagService(flags, audit, nil) }},
		{"nil clock", func() (*service.FlagService, error) {
			return service.NewFlagService(flags, audit, cache, service.WithClock(nil))
		}},
		{"nil ids", func() (*service.FlagService, error) {
			return service.NewFlagService(flags, audit, cache, service.WithIDGenerator(nil))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if s, err := tt.make(); err == nil || s != nil {
				t.Fatalf("got (%v, %v), want an error", s, err)
			}
		})
	}
}

func TestCreate(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("alice", "req-1")

	got, err := f.svc.Create(ctx, "new-checkout", "the new checkout", true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "new-checkout" || !got.Enabled || got.Version != 1 || got.Description != "the new checkout" {
		t.Fatalf("unexpected flag: %+v", got)
	}

	stored, err := f.flags.Get(ctx, "new-checkout")
	if err != nil || stored.Version != 1 {
		t.Fatalf("flag not stored: %+v, %v", stored, err)
	}

	if tags := f.cache.Tags(); len(tags) != 1 || tags[0] != service.FlagTag("new-checkout") {
		t.Fatalf("tags invalidated = %v", tags)
	}

	h := f.history(t, "new-checkout")
	if len(h) != 1 {
		t.Fatalf("got %d audit entries, want 1", len(h))
	}
	e := h[0]
	if e.Actor != "alice" || e.RequestID != "req-1" || e.Action != domain.ActionFlagCreated || e.ID != "id-1" {
		t.Fatalf("bad audit entry: %+v", e)
	}
	if !e.At.After(t0.Add(-time.Second)) || e.At.Location() != time.UTC {
		t.Fatalf("audit time %v not from the injected clock", e.At)
	}
	if decodeFlag(t, e.Before) != nil {
		t.Fatalf("before = %s, want null", e.Before)
	}
	if after := decodeFlag(t, e.After); after == nil || after.Name != "new-checkout" || after.Version != 1 {
		t.Fatalf("after = %s", e.After)
	}
}

func TestCreateRejectsBadInputAndLeavesNoTrace(t *testing.T) {
	tests := []struct {
		name, flag, desc string
		want             error
	}{
		{"empty name", "", "", domain.ErrInvalid},
		{"name with space", "my flag", "", domain.ErrInvalid},
		{"name too long", strings.Repeat("a", domain.MaxNameLength+1), "", domain.ErrInvalid},
		{"description too long", "ok", strings.Repeat("d", domain.MaxDescriptionLength+1), domain.ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			_, err := f.svc.Create(reqCtx("a", "r"), tt.flag, tt.desc, true)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if tags := f.cache.Tags(); len(tags) != 0 {
				t.Fatalf("invalidated %v on a rejected create", tags)
			}
			if all, _ := f.flags.List(context.Background()); len(all) != 0 {
				t.Fatalf("stored %v on a rejected create", all)
			}
		})
	}
}

func TestCreateDuplicate(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("alice", "r")
	if _, err := f.svc.Create(ctx, "dup", "", true); err != nil {
		t.Fatal(err)
	}
	invalidatedBefore := len(f.cache.Tags())

	_, err := f.svc.Create(ctx, "dup", "other", false)
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("err = %v, want ErrAlreadyExists", err)
	}
	if got := len(f.history(t, "dup")); got != 1 {
		t.Fatalf("duplicate create wrote an audit entry (%d entries)", got)
	}
	if len(f.cache.Tags()) != invalidatedBefore {
		t.Fatal("duplicate create invalidated the cache")
	}
	if got, _ := f.flags.Get(ctx, "dup"); !got.Enabled || got.Description != "" {
		t.Fatalf("original flag was modified: %+v", got)
	}
}

func TestAnonymousActorWhenContextHasNone(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.Create(context.Background(), "x", "", true); err != nil {
		t.Fatal(err)
	}
	if h := f.history(t, "x"); h[0].Actor != logger.AnonymousActor || h[0].RequestID != "" {
		t.Fatalf("entry = %+v", h[0])
	}
}

func TestInvalidActorIsRejectedBeforeAnyWrite(t *testing.T) {
	f := newFixture(t)
	_, err := f.svc.Create(reqCtx("bad actor", "r"), "x", "", true)
	if !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if all, _ := f.flags.List(context.Background()); len(all) != 0 {
		t.Fatal("flag was created despite an invalid actor")
	}
}

func TestGetAndList(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("a", "r")
	for _, n := range []string{"b", "a", "c"} {
		if _, err := f.svc.Create(ctx, n, "", true); err != nil {
			t.Fatal(err)
		}
	}

	got, err := f.svc.Get(ctx, "a")
	if err != nil || got.Name != "a" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if _, err := f.svc.Get(ctx, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get missing: %v, want ErrNotFound", err)
	}
	if _, err := f.svc.Get(ctx, "bad name"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("Get bad name: %v, want ErrInvalid", err)
	}

	list, err := f.svc.List(ctx)
	if err != nil || len(list) != 3 || list[0].Name != "a" || list[2].Name != "c" {
		t.Fatalf("List = %+v, %v", list, err)
	}
}

func TestUpdate(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("alice", "req-create")
	created, _ := f.svc.Create(ctx, "f", "old", true)

	ctx = reqCtx("bob", "req-update")
	got, err := f.svc.Update(ctx, "f", service.FlagUpdate{Description: ptr("new"), Enabled: ptr(false)})
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "new" || got.Enabled || got.Version != 2 {
		t.Fatalf("updated flag = %+v", got)
	}
	if !got.UpdatedAt.After(created.UpdatedAt) || !got.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("timestamps wrong: created %v updated %v", got.CreatedAt, got.UpdatedAt)
	}

	h := f.history(t, "f")
	if len(h) != 2 {
		t.Fatalf("got %d entries, want 2", len(h))
	}
	e := h[1]
	if e.Actor != "bob" || e.RequestID != "req-update" || e.Action != domain.ActionFlagUpdated {
		t.Fatalf("entry = %+v", e)
	}
	before, after := decodeFlag(t, e.Before), decodeFlag(t, e.After)
	if before == nil || before.Description != "old" || !before.Enabled || before.Version != 1 {
		t.Fatalf("before = %s", e.Before)
	}
	if after == nil || after.Description != "new" || after.Enabled || after.Version != 2 {
		t.Fatalf("after = %s", e.After)
	}
}

func TestUpdateTouchesOnlyProvidedFields(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("a", "r")
	f.svc.Create(ctx, "f", "keep me", true)

	got, err := f.svc.Update(ctx, "f", service.FlagUpdate{Enabled: ptr(false)})
	if err != nil || got.Description != "keep me" || got.Enabled {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestUpdateWithNoChangeIsANoOp(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("a", "r")
	f.svc.Create(ctx, "f", "d", true)
	tagsBefore := len(f.cache.Tags())

	got, err := f.svc.Update(ctx, "f", service.FlagUpdate{Description: ptr("d"), Enabled: ptr(true)})
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 {
		t.Fatalf("version bumped to %d by a no-op update", got.Version)
	}
	if len(f.history(t, "f")) != 1 {
		t.Fatal("no-op update wrote an audit entry")
	}
	if len(f.cache.Tags()) != tagsBefore {
		t.Fatal("no-op update invalidated the cache")
	}
	if f.hookDAO.updateCalls.Load() != 0 {
		t.Fatal("no-op update hit the DAO")
	}
}

func TestUpdateErrors(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("a", "r")
	f.svc.Create(ctx, "f", "", true)

	tests := []struct {
		name string
		flag string
		u    service.FlagUpdate
		want error
	}{
		{"nothing to update", "f", service.FlagUpdate{}, domain.ErrInvalid},
		{"missing flag", "nope", service.FlagUpdate{Enabled: ptr(true)}, domain.ErrNotFound},
		{"bad name", "bad name", service.FlagUpdate{Enabled: ptr(true)}, domain.ErrInvalid},
		{"description too long", "f", service.FlagUpdate{Description: ptr(strings.Repeat("x", domain.MaxDescriptionLength+1))}, domain.ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := f.svc.Update(ctx, tt.flag, tt.u); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
	if len(f.history(t, "f")) != 1 {
		t.Fatal("rejected updates left audit entries")
	}
	if got, _ := f.flags.Get(ctx, "f"); got.Version != 1 {
		t.Fatalf("rejected updates changed the flag: %+v", got)
	}
}

func TestDelete(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("alice", "r1")
	f.svc.Create(ctx, "f", "d", true)
	f.svc.SetOverride(ctx, "f", "u1", false)

	ctx = reqCtx("carol", "r-del")
	if err := f.svc.Delete(ctx, "f"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Get(ctx, "f"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get after delete: %v", err)
	}

	tags := f.cache.Tags()
	if tags[len(tags)-1] != service.FlagTag("f") {
		t.Fatalf("delete did not invalidate: %v", tags)
	}

	h := f.history(t, "f")
	e := h[len(h)-1]
	if e.Action != domain.ActionFlagDeleted || e.Actor != "carol" || e.RequestID != "r-del" {
		t.Fatalf("entry = %+v", e)
	}
	before := decodeFlag(t, e.Before)
	if before == nil || before.Version != 2 || before.Overrides["u1"] != false {
		t.Fatalf("before = %s", e.Before)
	}
	if decodeFlag(t, e.After) != nil {
		t.Fatalf("after = %s, want null", e.After)
	}
}

func TestDeleteErrors(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("a", "r")
	if err := f.svc.Delete(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if err := f.svc.Delete(ctx, "bad name"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if len(f.cache.Tags()) != 0 {
		t.Fatal("failed deletes invalidated the cache")
	}
}

func TestOverrides(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("alice", "r")
	f.svc.Create(ctx, "f", "", false)

	ctx = reqCtx("bob", "r-set")
	got, err := f.svc.SetOverride(ctx, "f", "user-1", true)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := got.OverrideFor("user-1"); !ok || !v || got.Version != 2 {
		t.Fatalf("after set: %+v", got)
	}

	got, err = f.svc.SetOverride(ctx, "f", "user-1", false)
	if err != nil || got.Version != 3 {
		t.Fatalf("replace: %+v, %v", got, err)
	}
	if v, _ := got.OverrideFor("user-1"); v {
		t.Fatal("override not replaced")
	}

	ctx = reqCtx("carol", "r-rm")
	got, err = f.svc.RemoveOverride(ctx, "f", "user-1")
	if err != nil || got.Version != 4 {
		t.Fatalf("remove: %+v, %v", got, err)
	}
	if _, ok := got.OverrideFor("user-1"); ok {
		t.Fatal("override still present")
	}

	h := f.history(t, "f")
	wantActions := []domain.Action{
		domain.ActionFlagCreated, domain.ActionOverrideSet, domain.ActionOverrideSet, domain.ActionOverrideRemove,
	}
	if len(h) != len(wantActions) {
		t.Fatalf("got %d entries", len(h))
	}
	for i, a := range wantActions {
		if h[i].Action != a {
			t.Fatalf("entry %d action = %s, want %s", i, h[i].Action, a)
		}
	}
	rm := h[3]
	if rm.Actor != "carol" {
		t.Fatalf("actor = %s", rm.Actor)
	}
	if b := decodeFlag(t, rm.Before); b.Overrides["user-1"] != false || len(b.Overrides) != 1 {
		t.Fatalf("remove before = %s", rm.Before)
	}
	if a := decodeFlag(t, rm.After); len(a.Overrides) != 0 {
		t.Fatalf("remove after = %s", rm.After)
	}
}

func TestOverrideNoOpsAndErrors(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("a", "r")
	f.svc.Create(ctx, "f", "", true)
	f.svc.SetOverride(ctx, "f", "u", true)
	entries := len(f.history(t, "f"))

	got, err := f.svc.SetOverride(ctx, "f", "u", true)
	if err != nil || got.Version != 2 {
		t.Fatalf("repeat set: %+v, %v", got, err)
	}
	if len(f.history(t, "f")) != entries {
		t.Fatal("repeat set wrote an audit entry")
	}

	tests := []struct {
		name string
		do   func() error
		want error
	}{
		{"remove missing override", func() error { _, err := f.svc.RemoveOverride(ctx, "f", "ghost"); return err }, domain.ErrNotFound},
		{"set on missing flag", func() error { _, err := f.svc.SetOverride(ctx, "nope", "u", true); return err }, domain.ErrNotFound},
		{"remove on missing flag", func() error { _, err := f.svc.RemoveOverride(ctx, "nope", "u"); return err }, domain.ErrNotFound},
		{"set with bad user id", func() error { _, err := f.svc.SetOverride(ctx, "f", "has space", true); return err }, domain.ErrInvalid},
		{"set with empty user id", func() error { _, err := f.svc.SetOverride(ctx, "f", "", true); return err }, domain.ErrInvalid},
		{"remove with bad user id", func() error { _, err := f.svc.RemoveOverride(ctx, "f", "has space"); return err }, domain.ErrInvalid},
		{"set on bad flag name", func() error { _, err := f.svc.SetOverride(ctx, "bad name", "u", true); return err }, domain.ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.do(); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
	if len(f.history(t, "f")) != entries {
		t.Fatal("failed override calls left audit entries")
	}
}

func TestOverrideLimit(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("a", "r")
	full := domain.Flag{Name: "full", Enabled: true, Version: 1, Overrides: map[string]bool{}, CreatedAt: t0, UpdatedAt: t0}
	for i := 0; i < domain.MaxOverridesPerFlag; i++ {
		full.Overrides[fmt.Sprintf("u%d", i)] = true
	}
	if err := f.flags.Create(ctx, full); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SetOverride(ctx, "full", "one-too-many", true); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if got, err := f.svc.SetOverride(ctx, "full", "u0", false); err != nil || got.Overrides["u0"] {
		t.Fatalf("replacing an existing override at the limit failed: %v", err)
	}
}

func TestInvalidationHappensAfterTheWriteIsVisible(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("a", "r")
	f.svc.Create(ctx, "f", "", true)

	var seenAtInvalidate domain.Flag
	f.cache.hook = func(string) { seenAtInvalidate, _ = f.flags.Get(context.Background(), "f") }
	if _, err := f.svc.Update(ctx, "f", service.FlagUpdate{Enabled: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	if seenAtInvalidate.Enabled || seenAtInvalidate.Version != 2 {
		t.Fatalf("cache invalidated before the new flag was stored: %+v", seenAtInvalidate)
	}
}

func TestAuditIsWrittenAfterInvalidation(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("a", "r")
	f.svc.Create(ctx, "f", "", true)

	var auditCountAtInvalidate int
	f.cache.hook = func(string) { auditCountAtInvalidate = len(f.history(t, "f")) }
	f.svc.Update(ctx, "f", service.FlagUpdate{Enabled: ptr(false)})
	if auditCountAtInvalidate != 1 {
		t.Fatalf("audit entry written before invalidation (%d entries at that time)", auditCountAtInvalidate)
	}
}

func TestConflictIsRetriedWithFreshData(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("alice", "r")
	f.svc.Create(ctx, "f", "orig", true)

	f.hookDAO.updateHook = func(call int, fl domain.Flag, inner dao.FlagDAO) error {
		if call == 1 {
			// Another process edits the flag between our read and our write.
			cur, _ := inner.Get(ctx, "f")
			cur.Description = "external edit"
			cur.Version++
			if err := inner.Update(ctx, cur); err != nil {
				t.Errorf("external update: %v", err)
			}
		}
		return inner.Update(ctx, fl)
	}

	got, err := f.svc.Update(ctx, "f", service.FlagUpdate{Enabled: ptr(false)})
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 3 || got.Enabled || got.Description != "external edit" {
		t.Fatalf("retry lost the concurrent edit or the change: %+v", got)
	}
	if calls := f.hookDAO.updateCalls.Load(); calls != 2 {
		t.Fatalf("Update calls = %d, want 2", calls)
	}

	h := f.history(t, "f")
	last := h[len(h)-1]
	if before := decodeFlag(t, last.Before); before.Version != 2 || before.Description != "external edit" {
		t.Fatalf("audit before is stale: %s", last.Before)
	}
	if len(h) != 2 {
		t.Fatalf("got %d entries, want create + one update", len(h))
	}
}

func TestPersistentConflictGivesUpWithErrConflict(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("a", "r")
	f.svc.Create(ctx, "f", "", true)
	tagsBefore := len(f.cache.Tags())

	f.hookDAO.updateHook = func(int, domain.Flag, dao.FlagDAO) error { return domain.ErrConflict }
	_, err := f.svc.Update(ctx, "f", service.FlagUpdate{Enabled: ptr(false)})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if calls := f.hookDAO.updateCalls.Load(); calls != 3 {
		t.Fatalf("Update calls = %d, want the bounded 3", calls)
	}
	if len(f.history(t, "f")) != 1 || len(f.cache.Tags()) != tagsBefore {
		t.Fatal("failed update left an audit entry or invalidated the cache")
	}
}

func TestStorageFailureIsNotRetriedAndLeavesNoTrace(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("a", "r")
	f.svc.Create(ctx, "f", "", true)
	tagsBefore := len(f.cache.Tags())

	boom := errors.New("disk on fire")
	f.hookDAO.updateHook = func(int, domain.Flag, dao.FlagDAO) error { return boom }
	_, err := f.svc.Update(ctx, "f", service.FlagUpdate{Enabled: ptr(false)})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the storage error", err)
	}
	if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrInvalid) {
		t.Fatal("a storage failure was disguised as a client error")
	}
	if calls := f.hookDAO.updateCalls.Load(); calls != 1 {
		t.Fatalf("Update calls = %d, want 1", calls)
	}
	if len(f.history(t, "f")) != 1 || len(f.cache.Tags()) != tagsBefore {
		t.Fatal("failed update left an audit entry or invalidated the cache")
	}
}

func TestAuditFailureIsReportedButTheChangeStands(t *testing.T) {
	flags := memory.NewFlagDAO()
	cache := &spyInvalidator{}
	boom := errors.New("audit disk full")
	audit := &failingAudit{err: boom}
	svc, err := service.NewFlagService(flags, audit, cache, service.WithClock(newFakeClock()), service.WithIDGenerator(&seqIDs{}))
	if err != nil {
		t.Fatal(err)
	}

	var logs strings.Builder
	base := slog.New(slog.NewJSONHandler(&logs, nil))
	ctx := logger.WithRequest(context.Background(), base, "req-9", "alice")

	_, err = svc.Create(ctx, "f", "", true)
	if !errors.Is(err, service.ErrAuditFailed) || !errors.Is(err, boom) {
		t.Fatalf("err = %v, want ErrAuditFailed wrapping the cause", err)
	}
	if _, err := flags.Get(ctx, "f"); err != nil {
		t.Fatalf("change was rolled back: %v", err)
	}
	if tags := cache.Tags(); len(tags) != 1 {
		t.Fatalf("cache not invalidated despite the committed write: %v", tags)
	}
	out := logs.String()
	for _, want := range []string{`"level":"ERROR"`, "audit entry could not be recorded", `"flag":"f"`, `"request_id":"req-9"`, "flag.created"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

func TestInvalidationAndAuditSurviveClientCancellation(t *testing.T) {
	flags := memory.NewFlagDAO()
	cache := &spyInvalidator{}
	audit := &recordingAudit{AuditDAO: memory.NewAuditDAO()}
	hd := &hookDAO{FlagDAO: flags}
	svc, _ := service.NewFlagService(hd, audit, cache, service.WithClock(newFakeClock()), service.WithIDGenerator(&seqIDs{}))

	ctx, cancel := context.WithCancel(reqCtx("a", "r"))
	defer cancel()
	svc.Create(ctx, "f", "", true)

	hd.updateHook = func(_ int, fl domain.Flag, inner dao.FlagDAO) error {
		err := inner.Update(ctx, fl)
		cancel() // the client disconnects right after the write commits
		return err
	}
	if _, err := svc.Update(ctx, "f", service.FlagUpdate{Enabled: ptr(false)}); err != nil {
		t.Fatalf("update failed after commit: %v", err)
	}
	if !audit.ctxWasLive.Load() {
		t.Fatal("audit append ran on the cancelled request context and would have been lost")
	}
	entries, _ := audit.ListByFlag(context.Background(), "f", 0)
	if len(entries) != 2 {
		t.Fatalf("got %d audit entries, want 2", len(entries))
	}
}

func TestCancelledContextBeforeStartChangesNothing(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(reqCtx("a", "r"))
	cancel()
	if _, err := f.svc.Create(ctx, "f", "", true); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if all, _ := f.flags.List(context.Background()); len(all) != 0 {
		t.Fatal("flag created on a cancelled context")
	}
	if len(f.history(t, "f")) != 0 {
		t.Fatal("audit entry written on a cancelled context")
	}
}

func TestWaitingForAFlagLockHonoursTheContext(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("a", "r")
	f.svc.Create(ctx, "f", "", true)

	entered, release := make(chan struct{}), make(chan struct{})
	f.hookDAO.updateHook = func(_ int, fl domain.Flag, inner dao.FlagDAO) error {
		close(entered)
		<-release
		return inner.Update(ctx, fl)
	}
	slow := make(chan error, 1)
	go func() {
		_, err := f.svc.Update(ctx, "f", service.FlagUpdate{Enabled: ptr(false)})
		slow <- err
	}()
	<-entered

	waitCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err := f.svc.Update(waitCtx, "f", service.FlagUpdate{Description: ptr("blocked")})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked writer err = %v, want DeadlineExceeded", err)
	}

	// Writes to a different flag are not blocked by the busy one.
	other := make(chan error, 1)
	go func() {
		_, err := f.svc.Create(ctx, "other", "", true)
		other <- err
	}()
	select {
	case err := <-other:
		if err != nil {
			t.Fatalf("create other flag: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a write to a different flag was blocked by a busy flag")
	}

	close(release)
	if err := <-slow; err != nil {
		t.Fatalf("slow writer: %v", err)
	}
}

func TestConcurrentWritersToOneFlagNeverLoseUpdates(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("a", "r")
	f.svc.Create(ctx, "hot", "", true)

	const writers = 40
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := reqCtx(fmt.Sprintf("actor-%d", i), fmt.Sprintf("req-%d", i))
			_, err := f.svc.SetOverride(c, "hot", fmt.Sprintf("user-%d", i), i%2 == 0)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent SetOverride failed: %v", err)
		}
	}

	got, _ := f.flags.Get(ctx, "hot")
	if len(got.Overrides) != writers || got.Version != int64(writers+1) {
		t.Fatalf("lost updates: %d overrides, version %d", len(got.Overrides), got.Version)
	}

	h := f.history(t, "hot")
	if len(h) != writers+1 {
		t.Fatalf("got %d audit entries, want %d", len(h), writers+1)
	}
	for i, e := range h {
		if v := decodeFlag(t, e.After).Version; v != int64(i+1) {
			t.Fatalf("audit entry %d records version %d: audit order differs from change order", i, v)
		}
		if i > 0 {
			if pv := decodeFlag(t, e.Before).Version; pv != int64(i) {
				t.Fatalf("audit entry %d before-version = %d, want %d", i, pv, i)
			}
		}
	}
}

func TestConcurrentMixedOperationsAcrossFlags(t *testing.T) {
	f := newFixture(t)
	const flagsN, rounds = 6, 25
	var wg sync.WaitGroup
	for i := 0; i < flagsN; i++ {
		name := fmt.Sprintf("flag-%d", i)
		for w := 0; w < 3; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				ctx := reqCtx("worker", "r")
				for r := 0; r < rounds; r++ {
					switch (r + w) % 4 {
					case 0:
						f.svc.Create(ctx, name, "", true)
					case 1:
						f.svc.Update(ctx, name, service.FlagUpdate{Enabled: ptr(r%2 == 0)})
					case 2:
						f.svc.SetOverride(ctx, name, fmt.Sprintf("u%d", r), true)
					case 3:
						f.svc.Delete(ctx, name)
					}
					f.svc.List(ctx)
				}
			}(w)
		}
	}
	wg.Wait()

	// Whatever interleaving happened, the audit trail must replay to the final
	// stored state for every flag.
	for i := 0; i < flagsN; i++ {
		name := fmt.Sprintf("flag-%d", i)
		h := f.history(t, name)
		var last *domain.Flag
		for _, e := range h {
			last = decodeFlag(t, e.After)
		}
		stored, err := f.flags.Get(context.Background(), name)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			if last != nil {
				t.Fatalf("%s: audit says it exists at version %d but it is deleted", name, last.Version)
			}
		case err != nil:
			t.Fatal(err)
		default:
			if last == nil || last.Version != stored.Version {
				t.Fatalf("%s: audit replay %+v does not match stored version %d", name, last, stored.Version)
			}
		}
	}
}

func TestWriteHelpersDoNotLeakIntoCallerData(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("a", "r")
	f.svc.Create(ctx, "f", "", true)
	got, _ := f.svc.SetOverride(ctx, "f", "u", true)
	got.Overrides["u"] = false
	got.Overrides["hacker"] = true

	stored, _ := f.flags.Get(ctx, "f")
	if stored.Overrides["u"] != true || len(stored.Overrides) != 1 {
		t.Fatalf("mutating a returned flag changed stored state: %+v", stored.Overrides)
	}
}

func TestDeleteStorageFailureLeavesNoTrace(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("a", "r")
	f.svc.Create(ctx, "f", "", true)
	tagsBefore := len(f.cache.Tags())

	boom := errors.New("disk on fire")
	f.hookDAO.deleteErr = boom
	if err := f.svc.Delete(ctx, "f"); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the storage error", err)
	}
	if len(f.history(t, "f")) != 1 || len(f.cache.Tags()) != tagsBefore {
		t.Fatal("failed delete left an audit entry or invalidated the cache")
	}
	if _, err := f.svc.Get(ctx, "f"); err != nil {
		t.Fatalf("flag vanished: %v", err)
	}
}

func TestInvalidActorIsRejectedForEveryWrite(t *testing.T) {
	f := newFixture(t)
	good := reqCtx("alice", "r")
	f.svc.Create(good, "f", "", true)
	bad := reqCtx("bad actor", "r")

	tests := map[string]func() error{
		"update":          func() error { _, err := f.svc.Update(bad, "f", service.FlagUpdate{Enabled: ptr(false)}); return err },
		"set override":    func() error { _, err := f.svc.SetOverride(bad, "f", "u", true); return err },
		"remove override": func() error { _, err := f.svc.RemoveOverride(bad, "f", "u"); return err },
		"delete":          func() error { return f.svc.Delete(bad, "f") },
	}
	for name, do := range tests {
		if err := do(); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if got, _ := f.flags.Get(good, "f"); got.Version != 1 || !got.Enabled {
		t.Fatalf("flag changed despite invalid actor: %+v", got)
	}
}

func TestCancelledContextDoesNotDeleteOrMutate(t *testing.T) {
	f := newFixture(t)
	live := reqCtx("a", "r")
	f.svc.Create(live, "f", "", true)
	ctx, cancel := context.WithCancel(live)
	cancel()

	if err := f.svc.Delete(ctx, "f"); !errors.Is(err, context.Canceled) {
		t.Fatalf("delete err = %v", err)
	}
	if _, err := f.svc.Update(ctx, "f", service.FlagUpdate{Enabled: ptr(false)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("update err = %v", err)
	}
	if got, _ := f.flags.Get(live, "f"); got.Version != 1 {
		t.Fatalf("flag changed on a cancelled context: %+v", got)
	}
}

func TestProductionClockAndIDGenerator(t *testing.T) {
	now := service.SystemClock{}.Now()
	if now.Location() != time.UTC || time.Since(now) > time.Minute {
		t.Fatalf("SystemClock.Now() = %v", now)
	}

	gen := service.RandomIDGenerator{}
	const goroutines, per = 8, 2000
	ids := make(chan string, goroutines*per)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				ids <- gen.NewID()
			}
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[string]struct{}, goroutines*per)
	for id := range ids {
		if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
			t.Fatalf("malformed id %q", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = struct{}{}
	}
}

// ---- percentage rollout ----------------------------------------------------

func TestCreateWithRollout(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("alice", "req-1")

	got, err := f.svc.Create(ctx, "gradual", "d", true, domain.WithRollout(25))
	if err != nil {
		t.Fatal(err)
	}
	if got.Rollout() != 25 {
		t.Fatalf("created flag rollout = %d, want 25", got.Rollout())
	}
	stored, _ := f.flags.Get(ctx, "gradual")
	if stored.Rollout() != 25 {
		t.Fatalf("stored rollout = %d, want 25", stored.Rollout())
	}
	h := f.history(t, "gradual")
	if len(h) != 1 {
		t.Fatalf("got %d audit entries", len(h))
	}
	if after := decodeFlag(t, h[0].After); after == nil || after.Rollout() != 25 {
		t.Fatalf("the audit trail must record the starting rollout: %s", h[0].After)
	}
}

func TestCreateWithoutRolloutIsFullyRolledOut(t *testing.T) {
	f := newFixture(t)
	got, err := f.svc.Create(reqCtx("a", "r"), "plain", "", true)
	if err != nil || got.Rollout() != 100 || got.RolloutPercent != nil {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestCreateWithInvalidRolloutLeavesNoTrace(t *testing.T) {
	for _, pct := range []int{-1, 101, 1000} {
		f := newFixture(t)
		_, err := f.svc.Create(reqCtx("a", "r"), "bad", "", true, domain.WithRollout(pct))
		if !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("rollout %d: err = %v, want ErrInvalid", pct, err)
		}
		if all, _ := f.flags.List(context.Background()); len(all) != 0 {
			t.Fatalf("rollout %d stored %v", pct, all)
		}
		if len(f.cache.Tags()) != 0 || len(f.history(t, "bad")) != 0 {
			t.Fatalf("rollout %d left cache or audit side effects", pct)
		}
	}
}

func TestUpdateChangesTheRolloutAndRecordsBeforeAndAfter(t *testing.T) {
	f := newFixture(t)
	f.svc.Create(reqCtx("alice", "r0"), "gradual", "d", true, domain.WithRollout(10))

	got, err := f.svc.Update(reqCtx("bob", "r1"), "gradual", service.FlagUpdate{RolloutPercent: ptr(60)})
	if err != nil {
		t.Fatal(err)
	}
	if got.Rollout() != 60 || got.Version != 2 || !got.Enabled || got.Description != "d" {
		t.Fatalf("got %+v: only the rollout should have changed", got)
	}

	h := f.history(t, "gradual")
	if len(h) != 2 {
		t.Fatalf("got %d entries, want 2", len(h))
	}
	e := h[1]
	if e.Actor != "bob" || e.Action != domain.ActionFlagUpdated {
		t.Fatalf("entry = %+v", e)
	}
	if before := decodeFlag(t, e.Before); before == nil || before.Rollout() != 10 {
		t.Fatalf("before = %s, want rollout 10", e.Before)
	}
	if after := decodeFlag(t, e.After); after == nil || after.Rollout() != 60 {
		t.Fatalf("after = %s, want rollout 60", e.After)
	}
}

// The rollout decides what users see, so changing it must drop cached answers.
func TestRolloutChangeInvalidatesCachedEvaluations(t *testing.T) {
	f := newFixture(t)
	f.svc.Create(reqCtx("a", "r"), "gradual", "", true, domain.WithRollout(10))
	f.cache.Reset()

	if _, err := f.svc.Update(reqCtx("a", "r2"), "gradual", service.FlagUpdate{RolloutPercent: ptr(20)}); err != nil {
		t.Fatal(err)
	}
	if tags := f.cache.Tags(); len(tags) != 1 || tags[0] != "flag:gradual" {
		t.Fatalf("invalidated %v, want exactly flag:gradual", tags)
	}
}

func TestRolloutUpdateToTheSameValueIsANoOp(t *testing.T) {
	f := newFixture(t)
	f.svc.Create(reqCtx("a", "r"), "gradual", "", true, domain.WithRollout(30))
	f.cache.Reset()

	got, err := f.svc.Update(reqCtx("a", "r2"), "gradual", service.FlagUpdate{RolloutPercent: ptr(30)})
	if err != nil || got.Version != 1 {
		t.Fatalf("got version %d, %v: setting the same rollout must not write", got.Version, err)
	}
	if len(f.history(t, "gradual")) != 1 || len(f.cache.Tags()) != 0 || f.hookDAO.updateCalls.Load() != 0 {
		t.Fatal("a no-op rollout update had side effects")
	}
}

// 100 and "never set" are the same state, so asking for 100 on a flag that
// was never restricted must not create a version or an audit entry.
func TestSettingFullRolloutOnAnUnrestrictedFlagIsANoOp(t *testing.T) {
	f := newFixture(t)
	f.svc.Create(reqCtx("a", "r"), "plain", "", true)
	f.cache.Reset()

	got, err := f.svc.Update(reqCtx("a", "r2"), "plain", service.FlagUpdate{RolloutPercent: ptr(100)})
	if err != nil || got.Version != 1 {
		t.Fatalf("got version %d, %v", got.Version, err)
	}
	if len(f.history(t, "plain")) != 1 || len(f.cache.Tags()) != 0 {
		t.Fatal("asking for 100%% on an unrestricted flag had side effects")
	}
}

func TestRolloutCanGoToZeroAndBackToFull(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("a", "r")
	f.svc.Create(ctx, "kill", "", true)

	got, err := f.svc.Update(ctx, "kill", service.FlagUpdate{RolloutPercent: ptr(0)})
	if err != nil || got.Rollout() != 0 || got.Version != 2 {
		t.Fatalf("to 0: %+v, %v", got, err)
	}
	got, err = f.svc.Update(ctx, "kill", service.FlagUpdate{RolloutPercent: ptr(100)})
	if err != nil || got.Rollout() != 100 || got.RolloutPercent != nil || got.Version != 3 {
		t.Fatalf("back to 100: %+v, %v", got, err)
	}
}

func TestUpdateRejectsInvalidRolloutAndChangesNothing(t *testing.T) {
	for _, pct := range []int{-1, 101, 250} {
		f := newFixture(t)
		ctx := reqCtx("a", "r")
		f.svc.Create(ctx, "f", "d", true, domain.WithRollout(40))
		f.cache.Reset()

		_, err := f.svc.Update(ctx, "f", service.FlagUpdate{RolloutPercent: ptr(pct), Description: ptr("also changed")})
		if !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("rollout %d: err = %v, want ErrInvalid", pct, err)
		}
		got, _ := f.flags.Get(ctx, "f")
		if got.Rollout() != 40 || got.Description != "d" || got.Version != 1 {
			t.Fatalf("rollout %d: a rejected update still changed the flag: %+v", pct, got)
		}
		if len(f.history(t, "f")) != 1 || len(f.cache.Tags()) != 0 {
			t.Fatalf("rollout %d left audit or cache side effects", pct)
		}
	}
}

func TestRolloutAloneIsEnoughForAnUpdate(t *testing.T) {
	f := newFixture(t)
	f.svc.Create(reqCtx("a", "r"), "f", "d", true)
	if _, err := f.svc.Update(reqCtx("a", "r2"), "f", service.FlagUpdate{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("an empty update must stay invalid, got %v", err)
	}
	if _, err := f.svc.Update(reqCtx("a", "r3"), "f", service.FlagUpdate{RolloutPercent: ptr(5)}); err != nil {
		t.Fatalf("an update carrying only a rollout must be accepted: %v", err)
	}
}

func TestRolloutAndEnabledChangeTogetherAsOneVersion(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("a", "r")
	f.svc.Create(ctx, "f", "d", false)

	got, err := f.svc.Update(ctx, "f", service.FlagUpdate{Enabled: ptr(true), RolloutPercent: ptr(5)})
	if err != nil || got.Version != 2 || !got.Enabled || got.Rollout() != 5 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if h := f.history(t, "f"); len(h) != 2 {
		t.Fatalf("one update must be one audit entry, got %d entries in total", len(h))
	}
}

func TestOverrideChangesKeepTheRollout(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("a", "r")
	f.svc.Create(ctx, "f", "d", true, domain.WithRollout(15))

	got, err := f.svc.SetOverride(ctx, "f", "vip", true)
	if err != nil || got.Rollout() != 15 {
		t.Fatalf("setting an override changed the rollout: %+v, %v", got, err)
	}
	got, err = f.svc.RemoveOverride(ctx, "f", "vip")
	if err != nil || got.Rollout() != 15 {
		t.Fatalf("removing an override changed the rollout: %+v, %v", got, err)
	}
}

func TestConcurrentRolloutUpdatesNeverLoseAVersion(t *testing.T) {
	f := newFixture(t)
	f.svc.Create(reqCtx("a", "r"), "ramp", "", true, domain.WithRollout(0))

	const workers = 16
	var wg sync.WaitGroup
	for w := 1; w <= workers; w++ {
		wg.Add(1)
		go func(pct int) {
			defer wg.Done()
			if _, err := f.svc.Update(reqCtx("w", fmt.Sprintf("r%d", pct)), "ramp", service.FlagUpdate{RolloutPercent: ptr(pct)}); err != nil {
				t.Errorf("update to %d: %v", pct, err)
			}
		}(w)
	}
	wg.Wait()

	got, _ := f.flags.Get(context.Background(), "ramp")
	if got.Version != workers+1 {
		t.Fatalf("version = %d, want %d: a concurrent update was lost or doubled", got.Version, workers+1)
	}
	if h := f.history(t, "ramp"); len(h) != workers+1 {
		t.Fatalf("%d audit entries, want %d", len(h), workers+1)
	}
}

// Bad input is reported as bad input whether or not the flag exists, and
// without taking the flag's lock or touching the store.
func TestInvalidRolloutIsRejectedBeforeTheFlagIsLookedUp(t *testing.T) {
	f := newFixture(t)
	_, err := f.svc.Update(reqCtx("a", "r"), "does-not-exist", service.FlagUpdate{RolloutPercent: ptr(101)})
	if !errors.Is(err, domain.ErrInvalid) || errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrInvalid and not ErrNotFound", err)
	}
	if f.hookDAO.updateCalls.Load() != 0 {
		t.Fatal("an invalid request reached the store")
	}

	if _, err := f.svc.Update(reqCtx("a", "r"), "does-not-exist", service.FlagUpdate{RolloutPercent: ptr(50)}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a valid rollout on a missing flag = %v, want ErrNotFound", err)
	}
}

// ---- flag dependencies -----------------------------------------------------

func TestCreateWithPrerequisites(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("a", "r")
	f.svc.Create(ctx, "payments", "", true)

	got, err := f.svc.Create(ctx, "checkout", "", true, domain.WithRequires("payments"))
	if err != nil || len(got.Requires) != 1 || got.Requires[0] != "payments" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if h := f.history(t, "checkout"); len(h) != 1 || !strings.Contains(string(h[0].After), `"requires":["payments"]`) {
		t.Fatalf("the audit trail must record the prerequisites: %v", h)
	}
}

func TestCreateRejectsUnknownPrerequisitesAndLeavesNoTrace(t *testing.T) {
	f := newFixture(t)
	_, err := f.svc.Create(reqCtx("a", "r"), "checkout", "", true, domain.WithRequires("ghost", "phantom"))
	if !errors.Is(err, domain.ErrInvalid) || !strings.Contains(err.Error(), "ghost, phantom") {
		t.Fatalf("err = %v, want ErrInvalid naming the unknown flags", err)
	}
	if all, _ := f.flags.List(context.Background()); len(all) != 0 {
		t.Fatalf("stored %v", all)
	}
	if len(f.cache.Tags()) != 0 || len(f.history(t, "checkout")) != 0 {
		t.Fatal("a rejected create left side effects")
	}
}

func TestUpdateSetsAndClearsPrerequisites(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("a", "r")
	f.svc.Create(ctx, "payments", "", true)
	f.svc.Create(ctx, "checkout", "", true)

	got, err := f.svc.Update(ctx, "checkout", service.FlagUpdate{Requires: &[]string{"payments"}})
	if err != nil || got.Version != 2 || len(got.Requires) != 1 {
		t.Fatalf("set: %+v, %v", got, err)
	}
	h := f.history(t, "checkout")
	before, after := decodeFlag(t, h[1].Before), decodeFlag(t, h[1].After)
	if len(before.Requires) != 0 || len(after.Requires) != 1 {
		t.Fatalf("history must show none -> payments: %s -> %s", h[1].Before, h[1].After)
	}

	got, err = f.svc.Update(ctx, "checkout", service.FlagUpdate{Requires: &[]string{}})
	if err != nil || got.Version != 3 || got.Requires != nil {
		t.Fatalf("clear: %+v, %v", got, err)
	}
}

func TestSettingTheSamePrerequisitesInAnyOrderIsANoOp(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("a", "r")
	f.svc.Create(ctx, "a", "", true)
	f.svc.Create(ctx, "b", "", true)
	f.svc.Create(ctx, "top", "", true, domain.WithRequires("a", "b"))
	f.cache.Reset()

	got, err := f.svc.Update(ctx, "top", service.FlagUpdate{Requires: &[]string{"b", "a"}})
	if err != nil || got.Version != 1 {
		t.Fatalf("version %d, %v: same set in another order must not write", got.Version, err)
	}
	if len(f.history(t, "top")) != 1 || len(f.cache.Tags()) != 0 {
		t.Fatal("no-op had side effects")
	}
	if _, err := f.svc.Update(ctx, "top", service.FlagUpdate{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("an empty update must stay invalid: %v", err)
	}
}

func TestUpdateRejectsBadPrerequisites(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("a", "r")
	f.svc.Create(ctx, "a", "", true)
	f.svc.Create(ctx, "b", "", true, domain.WithRequires("a"))

	tests := []struct {
		name     string
		flag     string
		requires []string
		want     error
	}{
		{"itself", "b", []string{"b"}, domain.ErrInvalid},
		{"duplicate", "b", []string{"a", "a"}, domain.ErrInvalid},
		{"unknown", "b", []string{"ghost"}, domain.ErrInvalid},
		{"direct cycle", "a", []string{"b"}, domain.ErrConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, _ := f.flags.Get(ctx, tt.flag)
			_, err := f.svc.Update(ctx, tt.flag, service.FlagUpdate{Requires: &tt.requires})
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			after, _ := f.flags.Get(ctx, tt.flag)
			if after.Version != before.Version {
				t.Fatal("a rejected update changed the flag")
			}
		})
	}
}

func TestIndirectCycleIsRejectedWithItsPath(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("a", "r")
	f.svc.Create(ctx, "a", "", true)
	f.svc.Create(ctx, "b", "", true, domain.WithRequires("a"))
	f.svc.Create(ctx, "c", "", true, domain.WithRequires("b"))

	_, err := f.svc.Update(ctx, "a", service.FlagUpdate{Requires: &[]string{"c"}})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "a -> c -> b -> a") {
		t.Fatalf("the error should show the loop, got %v", err)
	}
	// A diamond is not a cycle.
	f.svc.Create(ctx, "d", "", true, domain.WithRequires("b", "c"))
	if _, err := f.svc.Update(ctx, "d", service.FlagUpdate{Requires: &[]string{"a", "b", "c"}}); err != nil {
		t.Fatalf("shared prerequisites must be allowed: %v", err)
	}
}

func TestDeletingARequiredFlagIsRefusedUntilItIsReleased(t *testing.T) {
	f := newFixture(t)
	ctx := reqCtx("a", "r")
	f.svc.Create(ctx, "payments", "", true)
	f.svc.Create(ctx, "checkout", "", true, domain.WithRequires("payments"))
	f.svc.Create(ctx, "invoices", "", true, domain.WithRequires("payments"))

	err := f.svc.Delete(ctx, "payments")
	if !errors.Is(err, domain.ErrConflict) || !strings.Contains(err.Error(), "checkout, invoices") {
		t.Fatalf("err = %v, want ErrConflict naming both dependents", err)
	}
	if _, err := f.flags.Get(ctx, "payments"); err != nil {
		t.Fatal("the flag was deleted despite the conflict")
	}
	if len(f.history(t, "payments")) != 1 {
		t.Fatal("a refused delete was audited")
	}

	empty := []string{}
	f.svc.Update(ctx, "checkout", service.FlagUpdate{Requires: &empty})
	f.svc.Delete(ctx, "invoices")
	if err := f.svc.Delete(ctx, "payments"); err != nil {
		t.Fatalf("delete after release: %v", err)
	}
}

func TestChangingAFlagInvalidatesOnlyItsOwnTag(t *testing.T) {
	// Dependents are invalidated through the cache tags the evaluation stored
	// (every consulted flag), so the service only has to invalidate the flag it
	// changed.
	f := newFixture(t)
	ctx := reqCtx("a", "r")
	f.svc.Create(ctx, "payments", "", true)
	f.svc.Create(ctx, "checkout", "", true, domain.WithRequires("payments"))
	f.cache.Reset()

	f.svc.Update(ctx, "payments", service.FlagUpdate{Enabled: ptr(false)})
	if tags := f.cache.Tags(); len(tags) != 1 || tags[0] != "flag:payments" {
		t.Fatalf("invalidated %v", tags)
	}
}

// Two writers, each holding only their own flag's lock, could both pass a cycle
// check and together create a cycle. The graph lock must make exactly one win.
func TestConcurrentOppositeRequirementsNeverFormACycle(t *testing.T) {
	for round := 0; round < 25; round++ {
		f := newFixture(t)
		ctx := reqCtx("a", "r")
		f.svc.Create(ctx, "a", "", true)
		f.svc.Create(ctx, "b", "", true)

		var wg sync.WaitGroup
		results := make([]error, 2)
		for i, edge := range [][2]string{{"a", "b"}, {"b", "a"}} {
			wg.Add(1)
			go func(i int, from, to string) {
				defer wg.Done()
				_, results[i] = f.svc.Update(reqCtx("w", fmt.Sprintf("r%d", i)), from, service.FlagUpdate{Requires: &[]string{to}})
			}(i, edge[0], edge[1])
		}
		wg.Wait()

		wins := 0
		for _, err := range results {
			switch {
			case err == nil:
				wins++
			case !errors.Is(err, domain.ErrConflict):
				t.Fatalf("round %d: unexpected error %v", round, err)
			}
		}
		if wins != 1 {
			t.Fatalf("round %d: %d of 2 opposite requirements were accepted (%v), want exactly 1", round, wins, results)
		}
	}
}

// Deleting a flag while another is being created that requires it must not
// leave a flag pointing at nothing.
func TestConcurrentDeleteAndRequireNeverDangle(t *testing.T) {
	for round := 0; round < 25; round++ {
		f := newFixture(t)
		ctx := reqCtx("a", "r")
		f.svc.Create(ctx, "base", "", true)

		var wg sync.WaitGroup
		var delErr, createErr error
		wg.Add(2)
		go func() { defer wg.Done(); delErr = f.svc.Delete(reqCtx("d", "r1"), "base") }()
		go func() {
			defer wg.Done()
			_, createErr = f.svc.Create(reqCtx("c", "r2"), "dep", "", true, domain.WithRequires("base"))
		}()
		wg.Wait()

		_, baseErr := f.flags.Get(ctx, "base")
		dep, depErr := f.flags.Get(ctx, "dep")
		if depErr == nil && baseErr != nil {
			t.Fatalf("round %d: %q requires %v but base is gone (delete=%v create=%v)", round, dep.Name, dep.Requires, delErr, createErr)
		}
		if delErr == nil && createErr == nil {
			t.Fatalf("round %d: both succeeded", round)
		}
	}
}
