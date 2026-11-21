package service_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"feature-flag-service/internal/dao"
	"feature-flag-service/internal/dao/memory"
	"feature-flag-service/internal/domain"
	"feature-flag-service/internal/service"
	"feature-flag-service/pkg/logger"
)

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// fakeClock returns a time that advances by one second on every call, so every
// timestamp in a test is distinct and predictable.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: t0.Add(-time.Second)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(time.Second)
	return c.now
}

// seqIDs yields "id-1", "id-2", ...
type seqIDs struct{ n atomic.Int64 }

func (s *seqIDs) NewID() string { return "id-" + strconv.FormatInt(s.n.Add(1), 10) }

// spyInvalidator records invalidated tags.
type spyInvalidator struct {
	mu   sync.Mutex
	tags []string
	hook func(tag string)
}

func (s *spyInvalidator) InvalidateTag(tag string) {
	s.mu.Lock()
	s.tags = append(s.tags, tag)
	hook := s.hook
	s.mu.Unlock()
	if hook != nil {
		hook(tag)
	}
}

func (s *spyInvalidator) Tags() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.tags...)
}

// hookDAO wraps a FlagDAO to count calls and to replace Update with a test
// controlled behaviour.
type hookDAO struct {
	dao.FlagDAO
	updateCalls atomic.Int64
	updateHook  func(call int, f domain.Flag, inner dao.FlagDAO) error
	deleteErr   error
}

func (h *hookDAO) Delete(ctx context.Context, name string) error {
	if h.deleteErr != nil {
		return h.deleteErr
	}
	return h.FlagDAO.Delete(ctx, name)
}

func (h *hookDAO) Update(ctx context.Context, f domain.Flag) error {
	call := int(h.updateCalls.Add(1))
	if h.updateHook != nil {
		return h.updateHook(call, f, h.FlagDAO)
	}
	return h.FlagDAO.Update(ctx, f)
}

// countingReader counts Get calls on a FlagReader.
type countingReader struct {
	dao.FlagReader
	gets atomic.Int64
}

func (c *countingReader) Get(ctx context.Context, name string) (domain.Flag, error) {
	c.gets.Add(1)
	return c.FlagReader.Get(ctx, name)
}

// failingAudit is an AuditWriter that always fails, and records the context
// state it was called with.
type failingAudit struct {
	err        error
	ctxWasLive atomic.Bool
}

func (f *failingAudit) Append(ctx context.Context, _ domain.AuditEntry) error {
	f.ctxWasLive.Store(ctx.Err() == nil)
	return f.err
}

// recordingAudit stores entries and notes whether the context was still live.
type recordingAudit struct {
	*memory.AuditDAO
	ctxWasLive atomic.Bool
}

func (r *recordingAudit) Append(ctx context.Context, e domain.AuditEntry) error {
	r.ctxWasLive.Store(ctx.Err() == nil)
	return r.AuditDAO.Append(ctx, e)
}

// reqCtx builds a context like the HTTP middleware does.
func reqCtx(actor, requestID string) context.Context {
	base := slog.New(slog.NewTextHandler(io.Discard, nil))
	return logger.WithRequest(context.Background(), base, requestID, actor)
}

type fixture struct {
	svc     *service.FlagService
	flags   *memory.FlagDAO
	audit   *memory.AuditDAO
	cache   *spyInvalidator
	clock   *fakeClock
	hookDAO *hookDAO
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		flags: memory.NewFlagDAO(),
		audit: memory.NewAuditDAO(),
		cache: &spyInvalidator{},
		clock: newFakeClock(),
	}
	f.hookDAO = &hookDAO{FlagDAO: f.flags}
	svc, err := service.NewFlagService(f.hookDAO, f.audit, f.cache,
		service.WithClock(f.clock), service.WithIDGenerator(&seqIDs{}))
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	return f
}

// history returns audit entries oldest first.
func (f *fixture) history(t *testing.T, flag string) []domain.AuditEntry {
	t.Helper()
	newest, err := f.audit.ListByFlag(context.Background(), flag, 0)
	if err != nil {
		t.Fatal(err)
	}
	oldest := make([]domain.AuditEntry, len(newest))
	for i, e := range newest {
		oldest[len(newest)-1-i] = e
	}
	return oldest
}

func decodeFlag(t *testing.T, raw json.RawMessage) *domain.Flag {
	t.Helper()
	if string(raw) == "null" {
		return nil
	}
	var fl domain.Flag
	if err := json.Unmarshal(raw, &fl); err != nil {
		t.Fatalf("decode snapshot %s: %v", raw, err)
	}
	return &fl
}
