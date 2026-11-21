package lfu

import (
	"bytes"
	"fmt"
	"log/slog"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- helpers ---------------------------------------------------------------

type fakeClock struct{ ns atomic.Int64 }

func newFakeClock() *fakeClock {
	c := &fakeClock{}
	c.ns.Store(time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC).UnixNano())
	return c
}
func (c *fakeClock) Now() time.Time          { return time.Unix(0, c.ns.Load()).UTC() }
func (c *fakeClock) Advance(d time.Duration) { c.ns.Add(int64(d)) }

func newTestCache(t *testing.T, cfg Config) (*Cache, *fakeClock) {
	t.Helper()
	clock := newFakeClock()
	if cfg.Clock == nil {
		cfg.Clock = clock.Now
	}
	c, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, clock
}

func mustGet(t *testing.T, c *Cache, key string, want any) {
	t.Helper()
	got, ok := c.Get(key)
	if !ok || got != want {
		t.Fatalf("Get(%q) = %v, %v; want %v, true", key, got, ok, want)
	}
}

func mustMiss(t *testing.T, c *Cache, key string) {
	t.Helper()
	if got, ok := c.Get(key); ok {
		t.Fatalf("Get(%q) = %v, true; want a miss", key, got)
	}
}

// checkInvariants verifies every internal index agrees with every other one.
// It is the safety net for the concurrency tests: a race or a bookkeeping slip
// in any code path shows up as an inconsistency here.
func checkInvariants(t *testing.T, c *Cache) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.items) > c.cfg.Capacity {
		t.Errorf("size %d exceeds capacity %d", len(c.items), c.cfg.Capacity)
	}

	inBuckets, lowest := 0, 0
	for f, l := range c.freqs {
		if l.Len() == 0 {
			t.Errorf("empty bucket %d was not removed", f)
		}
		if lowest == 0 || f < lowest {
			lowest = f
		}
		for el := l.Front(); el != nil; el = el.Next() {
			e := el.Value.(*entry)
			inBuckets++
			if e.freq != f {
				t.Errorf("entry %q has freq %d but sits in bucket %d", e.key, e.freq, f)
			}
			if e.elem != el {
				t.Errorf("entry %q elem pointer does not match its bucket element", e.key)
			}
			if c.items[e.key] != e {
				t.Errorf("bucket entry %q is not the one in items", e.key)
			}
		}
	}
	if inBuckets != len(c.items) {
		t.Errorf("%d entries in buckets, %d in items", inBuckets, len(c.items))
	}
	if len(c.items) > 0 && c.minFreq > lowest {
		t.Errorf("minFreq %d is above the lowest bucket %d", c.minFreq, lowest)
	}

	expiring := 0
	for _, e := range c.items {
		if e.expiresAt.IsZero() {
			if e.heapIdx != -1 {
				t.Errorf("non-expiring entry %q has heapIdx %d", e.key, e.heapIdx)
			}
			continue
		}
		expiring++
		if e.heapIdx < 0 || e.heapIdx >= len(c.expiry) || c.expiry[e.heapIdx] != e {
			t.Errorf("expiring entry %q is not at its heapIdx %d", e.key, e.heapIdx)
		}
	}
	if expiring != len(c.expiry) {
		t.Errorf("%d expiring entries, %d in heap", expiring, len(c.expiry))
	}
	for i := 1; i < len(c.expiry); i++ {
		if c.expiry[i].expiresAt.Before(c.expiry[(i-1)/2].expiresAt) {
			t.Errorf("heap property violated at index %d", i)
		}
	}

	for tag, set := range c.tags {
		if len(set) == 0 {
			t.Errorf("empty tag set %q was not removed", tag)
		}
		for key := range set {
			e, ok := c.items[key]
			if !ok {
				t.Errorf("tag %q references missing key %q", tag, key)
				continue
			}
			found := false
			for _, et := range e.tags {
				found = found || et == tag
			}
			if !found {
				t.Errorf("tag %q lists key %q which does not carry it", tag, key)
			}
		}
	}
	for _, e := range c.items {
		for _, tag := range e.tags {
			if _, ok := c.tags[tag][e.key]; !ok {
				t.Errorf("entry %q carries tag %q missing from the index", e.key, tag)
			}
		}
	}
}

// withTimeout fails the test, with a full goroutine dump, if fn does not return
// in time. A deadlock therefore shows up as a clear failure rather than a hung
// test run.
func withTimeout(t *testing.T, d time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		t.Fatalf("possible deadlock: no completion within %s\n%s", d, buf)
	}
}

func eventually(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %s", d, msg)
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// ---- basic behaviour -------------------------------------------------------

func TestNewValidatesConfig(t *testing.T) {
	if _, err := New(Config{Capacity: 0}, nil); err == nil {
		t.Error("Capacity 0 should be rejected")
	}
	if _, err := New(Config{Capacity: 1, JanitorInterval: -time.Second}, nil); err == nil {
		t.Error("negative JanitorInterval should be rejected")
	}
}

func TestSetGetOverwriteDelete(t *testing.T) {
	c, _ := newTestCache(t, Config{Capacity: 10})

	mustMiss(t, c, "a")
	c.Set("a", 1, 0)
	mustGet(t, c, "a", 1)

	c.Set("a", 2, 0)
	mustGet(t, c, "a", 2)
	if c.Len() != 1 {
		t.Fatalf("overwrite must not add an entry, Len = %d", c.Len())
	}

	c.Delete("a")
	mustMiss(t, c, "a")
	c.Delete("never-existed")
	checkInvariants(t, c)
}

func TestStats(t *testing.T) {
	c, clock := newTestCache(t, Config{Capacity: 2})
	c.Set("a", 1, time.Second)
	c.Get("a")
	c.Get("missing")
	c.Set("b", 2, 0)
	c.Set("c", 3, 0) // evicts a live entry
	clock.Advance(2 * time.Second)
	c.Get("a")
	c.Get("b")

	s := c.Stats()
	if s.Hits != 1 || s.Misses < 2 || s.Evictions != 1 || s.Size > 2 {
		t.Fatalf("unexpected stats: %+v", s)
	}
}

// ---- LFU eviction ----------------------------------------------------------

func TestEvictsLeastFrequentlyUsed(t *testing.T) {
	c, _ := newTestCache(t, Config{Capacity: 3})
	c.Set("a", "A", 0)
	c.Set("b", "B", 0)
	c.Set("c", "C", 0)
	c.Get("a")
	c.Get("a")
	c.Get("b")

	c.Set("d", "D", 0) // c has the lowest frequency

	mustMiss(t, c, "c")
	mustGet(t, c, "a", "A")
	mustGet(t, c, "b", "B")
	mustGet(t, c, "d", "D")
	checkInvariants(t, c)
}

func TestFrequencyTieEvictsLeastRecentlyUsed(t *testing.T) {
	c, _ := newTestCache(t, Config{Capacity: 2})
	c.Set("a", 1, 0)
	c.Set("b", 1, 0)
	c.Set("c", 1, 0) // a and b tie on frequency 1, a is older

	mustMiss(t, c, "a")
	mustGet(t, c, "b", 1)
	mustGet(t, c, "c", 1)
	checkInvariants(t, c)
}

func TestTieAfterAccessesUsesRecency(t *testing.T) {
	c, _ := newTestCache(t, Config{Capacity: 2})
	c.Set("a", 1, 0)
	c.Set("b", 1, 0)
	c.Get("a") // both now freq 2, a touched first
	c.Get("b")
	c.Set("c", 1, 0)

	mustMiss(t, c, "a")
	mustGet(t, c, "b", 1)
	checkInvariants(t, c)
}

func TestHotEntrySurvivesAFlood(t *testing.T) {
	c, _ := newTestCache(t, Config{Capacity: 3})
	c.Set("hot", "H", 0)
	for i := 0; i < 10; i++ {
		c.Get("hot")
	}
	for i := 0; i < 50; i++ {
		c.Set(fmt.Sprintf("cold-%d", i), i, 0)
	}
	mustGet(t, c, "hot", "H")
	checkInvariants(t, c)
}

func TestEvictionAfterArbitraryRemovalRecomputesMinFreq(t *testing.T) {
	c, _ := newTestCache(t, Config{Capacity: 3})
	c.Set("a", 1, 0)
	c.Set("b", 1, 0)
	c.Set("c", 1, 0)
	for _, k := range []string{"b", "c"} {
		c.Get(k)
	}
	c.Delete("a") // empties the lowest bucket, leaving minFreq stale
	c.Set("d", 1, 0)
	c.Set("e", 1, 0) // full again: must find a victim despite the stale minFreq

	if c.Len() != 3 {
		t.Fatalf("Len = %d, want 3", c.Len())
	}
	checkInvariants(t, c)
}

// ---- TTL -------------------------------------------------------------------

func TestTTLExpiresLazily(t *testing.T) {
	c, clock := newTestCache(t, Config{Capacity: 10})
	c.Set("a", 1, 10*time.Second)

	clock.Advance(9 * time.Second)
	mustGet(t, c, "a", 1)
	clock.Advance(time.Second) // exactly at expiry counts as expired
	mustMiss(t, c, "a")

	if c.Len() != 0 {
		t.Fatalf("expired entry should have been removed on access, Len = %d", c.Len())
	}
	checkInvariants(t, c)
}

func TestDefaultTTLAndNoExpiry(t *testing.T) {
	c, clock := newTestCache(t, Config{Capacity: 10, DefaultTTL: 5 * time.Second})
	c.Set("default", 1, 0)
	c.Set("explicit", 2, 20*time.Second)
	clock.Advance(6 * time.Second)
	mustMiss(t, c, "default")
	mustGet(t, c, "explicit", 2)

	noTTL, clock2 := newTestCache(t, Config{Capacity: 10})
	noTTL.Set("forever", 1, 0)
	clock2.Advance(1000 * time.Hour)
	mustGet(t, noTTL, "forever", 1)
	checkInvariants(t, noTTL)
}

func TestOverwriteResetsTTL(t *testing.T) {
	c, clock := newTestCache(t, Config{Capacity: 10})
	c.Set("a", 1, 10*time.Second)
	clock.Advance(8 * time.Second)
	c.Set("a", 2, 10*time.Second)
	clock.Advance(8 * time.Second)
	mustGet(t, c, "a", 2)

	c.Set("a", 3, 0) // no default TTL configured: becomes non-expiring
	clock.Advance(1000 * time.Hour)
	mustGet(t, c, "a", 3)
	checkInvariants(t, c)
}

func TestFullCachePrefersEvictingExpiredEntries(t *testing.T) {
	c, clock := newTestCache(t, Config{Capacity: 2})
	c.Set("popular-but-expired", 1, time.Second)
	for i := 0; i < 5; i++ {
		c.Get("popular-but-expired")
	}
	c.Set("live", 2, 0)
	clock.Advance(2 * time.Second)

	c.Set("new", 3, 0)

	mustGet(t, c, "live", 2)
	mustGet(t, c, "new", 3)
	if s := c.Stats(); s.Evictions != 0 || s.Expirations != 1 {
		t.Fatalf("a live entry was evicted while an expired one remained: %+v", s)
	}
	checkInvariants(t, c)
}

func TestSweepNowRemovesOnlyExpiredInBatches(t *testing.T) {
	c, clock := newTestCache(t, Config{Capacity: 100, SweepBatchSize: 2})
	for i := 0; i < 7; i++ {
		c.Set(fmt.Sprintf("short-%d", i), i, time.Second)
	}
	c.Set("long", 1, time.Hour)
	c.Set("forever", 1, 0)
	clock.Advance(2 * time.Second)

	if removed := c.SweepNow(); removed != 7 {
		t.Fatalf("SweepNow removed %d, want 7", removed)
	}
	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2", c.Len())
	}
	mustGet(t, c, "long", 1)
	mustGet(t, c, "forever", 1)
	checkInvariants(t, c)
}

// ---- tags ------------------------------------------------------------------

func TestInvalidateTag(t *testing.T) {
	c, _ := newTestCache(t, Config{Capacity: 10})
	c.Set("k1", 1, 0, "a")
	c.Set("k2", 2, 0, "a", "b")
	c.Set("k3", 3, 0, "b")
	c.Set("k4", 4, 0)

	c.InvalidateTag("a")

	mustMiss(t, c, "k1")
	mustMiss(t, c, "k2")
	mustGet(t, c, "k3", 3)
	mustGet(t, c, "k4", 4)
	if s := c.Stats(); s.Invalidations != 2 {
		t.Fatalf("Invalidations = %d, want 2", s.Invalidations)
	}
	checkInvariants(t, c)

	c.InvalidateTag("b")
	mustMiss(t, c, "k3")
	c.InvalidateTag("no-such-tag")
	checkInvariants(t, c)
}

func TestDuplicateTagsAreHarmless(t *testing.T) {
	c, _ := newTestCache(t, Config{Capacity: 10})
	c.Set("k", 1, 0, "a", "a", "a")
	checkInvariants(t, c)
	c.InvalidateTag("a")
	mustMiss(t, c, "k")
	checkInvariants(t, c)
}

func TestOverwriteReplacesTags(t *testing.T) {
	c, _ := newTestCache(t, Config{Capacity: 10})
	c.Set("k", 1, 0, "old")
	c.Set("k", 2, 0, "new")

	c.InvalidateTag("old")
	mustGet(t, c, "k", 2)
	c.InvalidateTag("new")
	mustMiss(t, c, "k")
	checkInvariants(t, c)
}

func TestRemovalPathsCleanTheTagIndex(t *testing.T) {
	c, clock := newTestCache(t, Config{Capacity: 2})
	c.Set("evicted", 1, 0, "t1")
	c.Set("expires", 2, time.Second, "t2")
	c.Get("expires")
	c.Set("deleted", 3, 0, "t3") // evicts "evicted"
	clock.Advance(2 * time.Second)
	c.SweepNow()
	c.Delete("deleted")

	c.mu.Lock()
	leftover := len(c.tags)
	c.mu.Unlock()
	if leftover != 0 {
		t.Fatalf("tag index leaked %d tags after every entry was removed", leftover)
	}
	checkInvariants(t, c)
}

// ---- epoch protocol (stale fill protection) --------------------------------

func TestEpochGuardsFills(t *testing.T) {
	c, _ := newTestCache(t, Config{Capacity: 10})

	e := c.Epoch()
	if !c.SetIfEpoch("k", 1, 0, e, "t") {
		t.Fatal("fill should succeed when nothing was invalidated")
	}
	mustGet(t, c, "k", 1)

	stale := c.Epoch()
	c.InvalidateTag("unrelated-and-empty")
	if c.SetIfEpoch("k2", 2, 0, stale, "t") {
		t.Fatal("fill must be rejected after any invalidation, even of an empty tag")
	}
	mustMiss(t, c, "k2")

	stale = c.Epoch()
	c.Delete("whatever")
	if c.SetIfEpoch("k3", 3, 0, stale) {
		t.Fatal("fill must be rejected after a Delete")
	}

	if !c.SetIfEpoch("k4", 4, 0, c.Epoch()) {
		t.Fatal("a fill using a fresh epoch must succeed")
	}
}

// TestNoStaleValueSurvivesTheCacheAsideRace reproduces the classic race between
// readers that fill the cache and a writer that updates the source and then
// invalidates. Once the writer's final invalidation has completed, no cached
// value may be older than the final source value.
func TestNoStaleValueSurvivesTheCacheAsideRace(t *testing.T) {
	c, _ := newTestCache(t, Config{Capacity: 64})
	var truth atomic.Int64

	const (
		keys    = 16
		readers = 8
		writes  = 2000
	)
	stop := make(chan struct{})
	var readersWG sync.WaitGroup
	for r := 0; r < readers; r++ {
		readersWG.Add(1)
		go func(seed int64) {
			defer readersWG.Done()
			rng := rand.New(rand.NewSource(seed))
			for {
				select {
				case <-stop:
					return
				default:
				}
				key := fmt.Sprintf("k%d", rng.Intn(keys))
				if _, ok := c.Get(key); ok {
					continue
				}
				epoch := c.Epoch() // 1. before reading the source
				v := truth.Load()  // 2. read the source
				runtime.Gosched()  // widen the window for the race
				c.SetIfEpoch(key, v, 0, epoch, "flag")
			}
		}(int64(r))
	}

	withTimeout(t, 30*time.Second, func() {
		for i := 0; i < writes; i++ {
			truth.Add(1)            // update the source
			c.InvalidateTag("flag") // then invalidate
			if i%50 == 0 {
				runtime.Gosched()
			}
		}
	})
	close(stop)
	withTimeout(t, 10*time.Second, readersWG.Wait)

	final := truth.Load()
	for i := 0; i < keys; i++ {
		if v, ok := c.Get(fmt.Sprintf("k%d", i)); ok && v.(int64) != final {
			t.Fatalf("stale value %v cached for k%d, source is %d", v, i, final)
		}
	}
	checkInvariants(t, c)
}

// ---- janitor ---------------------------------------------------------------

func TestJanitorRemovesExpiredEntriesWithoutAnyAccess(t *testing.T) {
	c, clock := newTestCache(t, Config{Capacity: 100, JanitorInterval: 5 * time.Millisecond})
	for i := 0; i < 20; i++ {
		c.Set(fmt.Sprintf("k%d", i), i, time.Second, "t")
	}
	c.Set("keep", 1, time.Hour)
	clock.Advance(2 * time.Second)

	eventually(t, 3*time.Second, func() bool { return c.Len() == 1 }, "janitor should sweep the 20 expired entries")
	mustGet(t, c, "keep", 1)
	checkInvariants(t, c)
}

func TestJanitorDisabledWhenIntervalIsZero(t *testing.T) {
	before := runtime.NumGoroutine()
	c, clock := newTestCache(t, Config{Capacity: 10})
	c.Set("a", 1, time.Second)
	clock.Advance(time.Minute)

	time.Sleep(30 * time.Millisecond)
	if c.Len() != 1 {
		t.Fatal("no janitor is running, so the expired entry stays until accessed")
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("a goroutine was started despite JanitorInterval = 0 (%d -> %d)", before, after)
	}
}

func TestCloseStopsJanitorAndIsIdempotent(t *testing.T) {
	before := runtime.NumGoroutine()
	c, err := New(Config{Capacity: 10, JanitorInterval: time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, func() bool { return runtime.NumGoroutine() > before }, "janitor goroutine should be running")

	withTimeout(t, 5*time.Second, func() {
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := c.Close(); err != nil {
					t.Errorf("Close: %v", err)
				}
			}()
		}
		wg.Wait()
	})
	if err := c.Close(); err != nil {
		t.Fatalf("Close after Close: %v", err)
	}

	eventually(t, time.Second, func() bool { return runtime.NumGoroutine() <= before }, "janitor goroutine should have exited")

	c.Set("still", "works", 0)
	mustGet(t, c, "still", "works")
}

func TestCloseDoesNotWaitOnTheCacheLock(t *testing.T) {
	c, err := New(Config{Capacity: 10, JanitorInterval: time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Hold the lock the janitor needs. Close must still be able to stop it as
	// soon as the lock is free, and must itself never need the lock.
	c.mu.Lock()
	closed := make(chan struct{})
	go func() {
		_ = c.Close()
		close(closed)
	}()
	time.Sleep(20 * time.Millisecond)
	c.mu.Unlock()

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close deadlocked against the cache lock")
	}
}

func TestJanitorSurvivesAPanic(t *testing.T) {
	var ticks atomic.Int32
	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))

	hook := func() {
		if ticks.Add(1) == 1 {
			panic("boom")
		}
	}
	clock := newFakeClock()
	c, err := newCache(Config{Capacity: 10, JanitorInterval: 3 * time.Millisecond, Clock: clock.Now}, log, hook)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	c.Set("a", 1, time.Second)
	clock.Advance(time.Minute)

	eventually(t, 3*time.Second, func() bool { return c.Len() == 0 }, "janitor should keep sweeping after a panic")
	if ticks.Load() < 2 {
		t.Fatalf("janitor stopped after the panic, ticks = %d", ticks.Load())
	}
	if !bytes.Contains([]byte(logs.String()), []byte("recovered from panic")) {
		t.Fatalf("the panic was not logged: %s", logs.String())
	}

	// The lock must have been released despite the panic.
	withTimeout(t, 2*time.Second, func() { c.Set("after", 1, 0) })
}

// ---- concurrency stress ----------------------------------------------------

// TestConcurrentStress hammers every operation from many goroutines, with the
// janitor sweeping aggressively and short TTLs forcing constant expiry. Run
// with -race. The watchdog turns a deadlock into a failure, and the invariant
// check afterwards turns any lost update into one.
func TestConcurrentStress(t *testing.T) {
	c, err := New(Config{
		Capacity:        20,
		DefaultTTL:      3 * time.Millisecond,
		JanitorInterval: time.Millisecond,
		SweepBatchSize:  4,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	const (
		workers = 12
		opsEach = 4000
		keys    = 60
	)
	withTimeout(t, 60*time.Second, func() {
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(seed int64) {
				defer wg.Done()
				rng := rand.New(rand.NewSource(seed))
				for i := 0; i < opsEach; i++ {
					key := fmt.Sprintf("k%d", rng.Intn(keys))
					tag := fmt.Sprintf("t%d", rng.Intn(6))
					switch rng.Intn(10) {
					case 0, 1, 2:
						c.Set(key, i, time.Duration(rng.Intn(5))*time.Millisecond, tag, fmt.Sprintf("t%d", rng.Intn(6)))
					case 3:
						c.SetIfEpoch(key, i, 0, c.Epoch(), tag)
					case 4:
						c.Delete(key)
					case 5:
						c.InvalidateTag(tag)
					case 6:
						c.SweepNow()
					case 7:
						_ = c.Stats()
						_ = c.Len()
					default:
						c.Get(key)
					}
				}
			}(int64(w) + 1)
		}
		wg.Wait()
	})

	withTimeout(t, 5*time.Second, func() { _ = c.Close() })
	checkInvariants(t, c)
}

func TestConcurrentCloseDuringTraffic(t *testing.T) {
	c, err := New(Config{Capacity: 50, DefaultTTL: time.Millisecond, JanitorInterval: time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				k := fmt.Sprintf("k%d", (i+w)%40)
				c.Set(k, i, 0, "t")
				c.Get(k)
				if i%97 == 0 {
					c.InvalidateTag("t")
				}
			}
		}(w)
	}

	withTimeout(t, 10*time.Second, func() {
		time.Sleep(30 * time.Millisecond)
		_ = c.Close()
		time.Sleep(30 * time.Millisecond)
		close(stop)
		wg.Wait()
	})
	checkInvariants(t, c)
}

// ---- benchmarks ------------------------------------------------------------

func BenchmarkGetParallel(b *testing.B) {
	c, _ := New(Config{Capacity: 1024}, nil)
	defer c.Close()
	for i := 0; i < 1024; i++ {
		c.Set(fmt.Sprintf("k%d", i), i, 0)
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			c.Get(fmt.Sprintf("k%d", i&1023))
			i++
		}
	})
}

func BenchmarkSetParallel(b *testing.B) {
	c, _ := New(Config{Capacity: 1024, DefaultTTL: time.Minute}, nil)
	defer c.Close()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			c.Set(fmt.Sprintf("k%d", i&4095), i, 0, "t")
			i++
		}
	})
}
