package service_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"feature-flag-service/internal/dao/memory"
	"feature-flag-service/internal/domain"
	"feature-flag-service/internal/evaluator"
	"feature-flag-service/internal/service"
	"feature-flag-service/pkg/cache/lfu"
)

func newLFU(t *testing.T) *lfu.Cache {
	t.Helper()
	c, err := lfu.New(lfu.Config{Capacity: 1000, DefaultTTL: time.Minute}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// stack wires the real evaluator, DAOs and LFU cache the way the composition
// root will, with a counting reader to observe storage reads.
type stack struct {
	flags  *service.FlagService
	eval   *service.EvaluationService
	cache  *lfu.Cache
	reader *countingReader
}

func newStack(t *testing.T) *stack {
	t.Helper()
	store := memory.NewFlagDAO()
	cache := newLFU(t)
	reader := &countingReader{FlagReader: store}

	fs, err := service.NewFlagService(store, memory.NewAuditDAO(), cache,
		service.WithClock(newFakeClock()), service.WithIDGenerator(&seqIDs{}))
	if err != nil {
		t.Fatal(err)
	}
	ev, err := evaluator.NewDefault(reader)
	if err != nil {
		t.Fatal(err)
	}
	es, err := service.NewEvaluationService(ev, cache, 0)
	if err != nil {
		t.Fatal(err)
	}
	return &stack{flags: fs, eval: es, cache: cache, reader: reader}
}

func (s *stack) mustEval(t *testing.T, flag, user string) service.Evaluation {
	t.Helper()
	got, err := s.eval.Evaluate(context.Background(), flag, user)
	if err != nil {
		t.Fatalf("Evaluate(%q,%q): %v", flag, user, err)
	}
	return got
}

func TestNewEvaluationServiceRejectsNilDependencies(t *testing.T) {
	ev, _ := evaluator.NewDefault(memory.NewFlagDAO())
	if _, err := service.NewEvaluationService(nil, newLFU(t), 0); err == nil {
		t.Error("nil evaluator accepted")
	}
	if _, err := service.NewEvaluationService(ev, nil, 0); err == nil {
		t.Error("nil cache accepted")
	}
}

func TestEvaluationRulesThroughTheService(t *testing.T) {
	s := newStack(t)
	ctx := reqCtx("admin", "r")
	s.flags.Create(ctx, "on", "", true)
	s.flags.Create(ctx, "off", "", false)
	s.flags.SetOverride(ctx, "off", "vip", true)
	s.flags.SetOverride(ctx, "on", "banned", false)

	tests := []struct {
		flag, user string
		want       bool
		reason     evaluator.Reason
	}{
		{"on", "anyone", true, evaluator.ReasonGlobalOn},
		{"off", "anyone", false, evaluator.ReasonGlobalOff},
		{"off", "vip", true, evaluator.ReasonUserOverride},
		{"on", "banned", false, evaluator.ReasonUserOverride},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s/%s", tt.flag, tt.user), func(t *testing.T) {
			got := s.mustEval(t, tt.flag, tt.user)
			if got.Enabled != tt.want || got.Reason != tt.reason || got.Cached {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestSecondEvaluationIsServedFromCacheWithoutStorageRead(t *testing.T) {
	s := newStack(t)
	s.flags.Create(reqCtx("a", "r"), "f", "", true)

	first := s.mustEval(t, "f", "u1")
	readsAfterFirst := s.reader.gets.Load()
	second := s.mustEval(t, "f", "u1")

	if first.Cached || !second.Cached {
		t.Fatalf("cached flags: first=%v second=%v", first.Cached, second.Cached)
	}
	if first.Enabled != second.Enabled || first.Reason != second.Reason {
		t.Fatalf("cached result differs: %+v vs %+v", first, second)
	}
	if got := s.reader.gets.Load(); got != readsAfterFirst {
		t.Fatalf("cache hit read storage %d more times", got-readsAfterFirst)
	}

	if third := s.mustEval(t, "f", "u2"); third.Cached {
		t.Fatal("a different user was served another user's cached result")
	}
	if other := s.mustEval(t, "f", "u1"); !other.Cached {
		t.Fatal("u1 lost its cached entry")
	}
}

func TestEveryWriteInvalidatesCachedEvaluations(t *testing.T) {
	s := newStack(t)
	ctx := reqCtx("admin", "r")
	s.flags.Create(ctx, "f", "", true)

	steps := []struct {
		name  string
		do    func() error
		want  bool
		cause evaluator.Reason
	}{
		{"disable globally", func() error {
			_, err := s.flags.Update(ctx, "f", service.FlagUpdate{Enabled: ptr(false)})
			return err
		}, false, evaluator.ReasonGlobalOff},
		{"add override", func() error { _, err := s.flags.SetOverride(ctx, "f", "u", true); return err }, true, evaluator.ReasonUserOverride},
		{"flip override", func() error { _, err := s.flags.SetOverride(ctx, "f", "u", false); return err }, false, evaluator.ReasonUserOverride},
		{"remove override", func() error { _, err := s.flags.RemoveOverride(ctx, "f", "u"); return err }, false, evaluator.ReasonGlobalOff},
		{"enable globally", func() error { _, err := s.flags.Update(ctx, "f", service.FlagUpdate{Enabled: ptr(true)}); return err }, true, evaluator.ReasonGlobalOn},
	}
	for _, st := range steps {
		s.mustEval(t, "f", "u")
		if !s.mustEval(t, "f", "u").Cached {
			t.Fatalf("%s: result was not cached before the write", st.name)
		}
		if err := st.do(); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		got := s.mustEval(t, "f", "u")
		if got.Cached {
			t.Fatalf("%s: stale cached result served after the write", st.name)
		}
		if got.Enabled != st.want || got.Reason != st.cause {
			t.Fatalf("%s: got %+v, want enabled=%v reason=%s", st.name, got, st.want, st.cause)
		}
	}
}

func TestDeleteAndRecreateAreVisibleToEvaluation(t *testing.T) {
	s := newStack(t)
	ctx := reqCtx("admin", "r")
	s.flags.Create(ctx, "f", "", true)
	s.mustEval(t, "f", "u")

	s.flags.Delete(ctx, "f")
	if _, err := s.eval.Evaluate(ctx, "f", "u"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("after delete: %v, want ErrNotFound", err)
	}

	s.flags.Create(ctx, "f", "", false)
	if got := s.mustEval(t, "f", "u"); got.Enabled {
		t.Fatalf("recreated disabled flag evaluated as enabled: %+v", got)
	}
}

func TestInvalidInputNeverTouchesTheCache(t *testing.T) {
	s := newStack(t)
	before := s.cache.Stats()
	for _, tt := range []struct{ flag, user string }{
		{"bad name", "u"}, {"", "u"}, {"ok", ""}, {"ok", "has space"},
	} {
		if _, err := s.eval.Evaluate(context.Background(), tt.flag, tt.user); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("Evaluate(%q,%q) = %v, want ErrInvalid", tt.flag, tt.user, err)
		}
	}
	if after := s.cache.Stats(); after != before {
		t.Fatalf("cache stats changed on invalid input: %+v -> %+v", before, after)
	}
}

func TestNotFoundIsNeverCached(t *testing.T) {
	s := newStack(t)
	for i := 0; i < 3; i++ {
		if _, err := s.eval.Evaluate(context.Background(), "ghost", "u"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	}
	if n := s.cache.Len(); n != 0 {
		t.Fatalf("cache holds %d entries after not-found lookups", n)
	}
	if got := s.reader.gets.Load(); got != 3 {
		t.Fatalf("storage reads = %d, want 3 (every miss must re-check)", got)
	}
}

// stubEvaluator lets a test control exactly what the evaluation returns and
// run code in the middle of it.
type stubEvaluator struct {
	result evaluator.Result
	err    error
	during func()
	calls  atomic.Int64
}

func (s *stubEvaluator) Evaluate(context.Context, string, string) (evaluator.Result, error) {
	s.calls.Add(1)
	if s.during != nil {
		s.during()
	}
	return s.result, s.err
}

func TestEvaluatorErrorsAreReturnedAndNotCached(t *testing.T) {
	cache := newLFU(t)
	boom := errors.New("storage down")
	stub := &stubEvaluator{err: boom}
	es, _ := service.NewEvaluationService(stub, cache, 0)

	for i := 0; i < 2; i++ {
		if _, err := es.Evaluate(context.Background(), "f", "u"); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the storage error", err)
		}
	}
	if stub.calls.Load() != 2 || cache.Len() != 0 {
		t.Fatalf("errors were cached: calls=%d len=%d", stub.calls.Load(), cache.Len())
	}
}

// spyCache records what the service stores.
type spyCache struct {
	*lfu.Cache
	mu      sync.Mutex
	ttls    []time.Duration
	tagSets [][]string
}

func (s *spyCache) SetIfEpoch(key string, v any, ttl time.Duration, epoch uint64, tags ...string) bool {
	s.mu.Lock()
	s.ttls = append(s.ttls, ttl)
	s.tagSets = append(s.tagSets, append([]string(nil), tags...))
	s.mu.Unlock()
	return s.Cache.SetIfEpoch(key, v, ttl, epoch, tags...)
}

func TestCachedResultIsTaggedWithEveryConsultedFlagAndUsesTheTTL(t *testing.T) {
	spy := &spyCache{Cache: newLFU(t)}
	stub := &stubEvaluator{result: evaluator.Result{Flag: "child", UserID: "u", Enabled: true, Consulted: []string{"child", "parent", "grandparent"}}}
	es, _ := service.NewEvaluationService(stub, spy, 7*time.Second)

	if _, err := es.Evaluate(context.Background(), "child", "u"); err != nil {
		t.Fatal(err)
	}
	if len(spy.tagSets) != 1 {
		t.Fatalf("SetIfEpoch calls = %d", len(spy.tagSets))
	}
	want := []string{service.FlagTag("child"), service.FlagTag("parent"), service.FlagTag("grandparent")}
	if fmt.Sprint(spy.tagSets[0]) != fmt.Sprint(want) {
		t.Fatalf("tags = %v, want %v", spy.tagSets[0], want)
	}
	if spy.ttls[0] != 7*time.Second {
		t.Fatalf("ttl = %v", spy.ttls[0])
	}

	// Changing a prerequisite must drop the dependent's cached result.
	spy.InvalidateTag(service.FlagTag("parent"))
	got, _ := es.Evaluate(context.Background(), "child", "u")
	if got.Cached {
		t.Fatal("result survived invalidation of a prerequisite it consulted")
	}
}

func TestStaleFillIsRefused(t *testing.T) {
	cache := newLFU(t)
	stub := &stubEvaluator{result: evaluator.Result{Flag: "f", UserID: "u", Enabled: true, Consulted: []string{"f"}}}
	es, _ := service.NewEvaluationService(stub, cache, 0)

	// A writer invalidates the flag while the reader is mid-evaluation, after
	// the reader loaded the old value.
	stub.during = func() { cache.InvalidateTag(service.FlagTag("f")) }

	got, err := es.Evaluate(context.Background(), "f", "u")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled {
		t.Fatal("the computed result must still be returned to the caller")
	}
	if cache.Len() != 0 {
		t.Fatal("a result computed before an invalidation was stored")
	}

	stub.during = nil
	if again, _ := es.Evaluate(context.Background(), "f", "u"); again.Cached {
		t.Fatal("served a stale entry")
	}
	if cache.Len() != 1 {
		t.Fatal("a clean evaluation should have been cached")
	}
}

func TestUnexpectedCachedValueTypeIsTreatedAsAMiss(t *testing.T) {
	cache := newLFU(t)
	cache.Set("eval:f:u", "garbage", time.Minute, service.FlagTag("f"))
	stub := &stubEvaluator{result: evaluator.Result{Flag: "f", UserID: "u", Enabled: true, Consulted: []string{"f"}}}
	es, _ := service.NewEvaluationService(stub, cache, 0)

	got, err := es.Evaluate(context.Background(), "f", "u")
	if err != nil || !got.Enabled || got.Cached {
		t.Fatalf("got %+v, %v", got, err)
	}
	if stub.calls.Load() != 1 {
		t.Fatal("corrupt entry was not recomputed")
	}
}

func TestCacheKeyIsUnambiguousForUserIDsContainingColons(t *testing.T) {
	s := newStack(t)
	ctx := reqCtx("admin", "r")
	s.flags.Create(ctx, "a", "", true)
	s.flags.Create(ctx, "a-b", "", false)

	// "a" + user "b:x" and "a-b" + user "x" must not share a key.
	s.mustEval(t, "a", "b:x")
	if got := s.mustEval(t, "a-b", "x"); got.Cached || got.Enabled {
		t.Fatalf("cache key collision: %+v", got)
	}
}

// After a write returns, every evaluation that starts later must see it, even
// while readers are racing to refill the cache. This is the property the epoch
// guard exists to protect.
func TestReadYourWritesUnderConcurrentEvaluation(t *testing.T) {
	s := newStack(t)
	ctx := reqCtx("admin", "r")
	s.flags.Create(ctx, "f", "", true)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			user := fmt.Sprintf("u%d", r%3)
			for {
				select {
				case <-stop:
					return
				default:
					if _, err := s.eval.Evaluate(context.Background(), "f", user); err != nil {
						t.Errorf("reader: %v", err)
						return
					}
				}
			}
		}(r)
	}

	for i := 0; i < 300; i++ {
		want := i%2 == 0
		if _, err := s.flags.Update(ctx, "f", service.FlagUpdate{Enabled: ptr(want)}); err != nil {
			t.Fatal(err)
		}
		for _, user := range []string{"u0", "u1", "u2"} {
			got, err := s.eval.Evaluate(context.Background(), "f", user)
			if err != nil {
				t.Fatal(err)
			}
			if got.Enabled != want {
				close(stop)
				wg.Wait()
				t.Fatalf("iteration %d: user %s read enabled=%v (cached=%v) after the flag was set to %v", i, user, got.Enabled, got.Cached, want)
			}
		}
	}
	close(stop)
	wg.Wait()
}
