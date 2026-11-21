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
