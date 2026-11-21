// Package lfu implements an in-memory cache with least-frequently-used
// eviction, per-entry TTL, and tag-based invalidation.
//
// # Complexity
//
// Get, Set and Delete are O(1) (O(log n) when the entry has a TTL, for the
// expiry heap). Entries are grouped in buckets by access frequency and each
// bucket is ordered by recency, so the eviction victim is the least recently
// used entry among the least frequently used ones.
//
// # Concurrency rules
//
// These rules are what keep the cache free of data races and deadlocks. Anyone
// changing this file must preserve them:
//
//  1. One mutex (mu) guards all mutable state. Get also mutates (it bumps the
//     access frequency), so there is no read-only path and no RWMutex.
//  2. Every critical section uses "defer mu.Unlock()", so a panic can never
//     leave the lock held.
//  3. mu is never held while calling code the cache does not own: the clock is
//     read before locking, and nothing is logged while locked.
//  4. Methods never call other locking methods while holding mu. Internal
//     helpers named *Locked require the caller to hold it and never lock.
//     Because there is exactly one lock and no nesting, lock-order deadlocks
//     are impossible.
//  5. Close never holds mu while waiting for the janitor goroutine, and the
//     janitor holds mu only for one small batch at a time, so neither can
//     block the other.
package lfu

import (
	"container/heap"
	"container/list"
	"context"
	"errors"
	"io"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"feature-flag-service/pkg/cache"
)

var _ cache.Cache = (*Cache)(nil)

const defaultSweepBatchSize = 256

// Config configures a Cache.
type Config struct {
	// Capacity is the maximum number of entries. Must be at least 1.
	Capacity int

	// DefaultTTL applies when Set is called with a non-positive ttl. If it is
	// also zero or less, such entries never expire.
	DefaultTTL time.Duration

	// JanitorInterval is how often the background sweeper removes expired
	// entries. Zero disables the janitor; expired entries are then removed
	// lazily on access, when the cache is full, or by SweepNow.
	JanitorInterval time.Duration

	// SweepBatchSize bounds how many entries one sweep step removes while
	// holding the lock, so a large expiry burst never blocks Get and Set for
	// long. Defaults to 256.
	SweepBatchSize int

	// Clock returns the current time. Defaults to time.Now. It is called
	// without any lock held, so it may safely block or call back into the
	// cache's caller.
	Clock func() time.Time
}

// Stats is a point-in-time view of cache activity.
type Stats struct {
	Size          int
	Hits          uint64
	Misses        uint64
	Evictions     uint64 // entries removed to make room
	Expirations   uint64 // entries removed because their TTL elapsed
	Invalidations uint64 // entries removed by InvalidateTag
}

type entry struct {
	key       string
	value     any
	freq      int
	expiresAt time.Time // zero means never
	tags      []string
	elem      *list.Element // position in freqs[freq]
	heapIdx   int           // position in the expiry heap, or -1
}

// Cache is a thread-safe LFU cache. Create it with New and release it with
// Close.
type Cache struct {
	cfg Config
	log *slog.Logger

	mu      sync.Mutex
	items   map[string]*entry
	freqs   map[int]*list.List // frequency -> entries, front is least recently used
	minFreq int                // lower bound on the lowest non-empty frequency
	expiry  expiryHeap
	tags    map[string]map[string]struct{}
	epoch   uint64

	hits, misses, evictions, expirations, invalidations atomic.Uint64

	stop      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
	sweepHook func() // test seam, called at the start of every janitor tick
}

// New creates a cache and, if cfg.JanitorInterval is positive, starts its
// janitor goroutine. Call Close to stop it. log may be nil.
func New(cfg Config, log *slog.Logger) (*Cache, error) {
	return newCache(cfg, log, nil)
}

func newCache(cfg Config, log *slog.Logger, sweepHook func()) (*Cache, error) {
	if cfg.Capacity < 1 {
		return nil, errors.New("lfu: Capacity must be at least 1")
	}
	if cfg.JanitorInterval < 0 {
		return nil, errors.New("lfu: JanitorInterval must not be negative")
	}
	if cfg.SweepBatchSize <= 0 {
		cfg.SweepBatchSize = defaultSweepBatchSize
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if log == nil {
		log = slog.New(discardHandler{})
	}

	c := &Cache{
		cfg:       cfg,
		log:       log,
		items:     make(map[string]*entry),
		freqs:     make(map[int]*list.List),
		tags:      make(map[string]map[string]struct{}),
		stop:      make(chan struct{}),
		sweepHook: sweepHook,
	}
	if cfg.JanitorInterval > 0 {
		c.wg.Add(1)
		go c.runJanitor(cfg.JanitorInterval)
	}
	return c, nil
}

// Get implements cache.Cache.
func (c *Cache) Get(key string) (any, bool) {
	now := c.cfg.Clock()

	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.items[key]
	if !ok {
		c.misses.Add(1)
		return nil, false
	}
	if e.expiredAt(now) {
		c.removeLocked(e)
		c.expirations.Add(1)
		c.misses.Add(1)
		return nil, false
	}
	c.touchLocked(e)
	c.hits.Add(1)
	return e.value, true
}

// Set implements cache.Cache.
func (c *Cache) Set(key string, value any, ttl time.Duration, tags ...string) {
	now := c.cfg.Clock()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.setLocked(now, key, value, ttl, tags)
}

// Epoch implements cache.Cache.
func (c *Cache) Epoch() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.epoch
}

// SetIfEpoch implements cache.Cache. The epoch comparison and the store happen
// under one lock acquisition, so no invalidation can slip in between them.
func (c *Cache) SetIfEpoch(key string, value any, ttl time.Duration, epoch uint64, tags ...string) bool {
	now := c.cfg.Clock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.epoch != epoch {
		return false
	}
	c.setLocked(now, key, value, ttl, tags)
	return true
}

// Delete implements cache.Cache. It advances the epoch even if the key is
// absent, so an in-flight fill that raced with the delete is rejected.
func (c *Cache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
	if e, ok := c.items[key]; ok {
		c.removeLocked(e)
	}
}

// InvalidateTag implements cache.Cache. It advances the epoch even if no entry
// carries the tag: the writer's change may predate any cached entry, and an
// in-flight fill that read the old source must still be rejected.
func (c *Cache) InvalidateTag(tag string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++

	set := c.tags[tag]
	if len(set) == 0 {
		return
	}
	// Snapshot first: removeLocked edits the tag sets we would be ranging over.
	victims := make([]*entry, 0, len(set))
	for key := range set {
		victims = append(victims, c.items[key])
	}
	for _, e := range victims {
		c.removeLocked(e)
	}
	c.invalidations.Add(uint64(len(victims)))
}

// Len returns the number of entries, including any that have expired but not
// yet been removed.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// Stats returns a snapshot of cache activity.
func (c *Cache) Stats() Stats {
	return Stats{
		Size:          c.Len(),
		Hits:          c.hits.Load(),
		Misses:        c.misses.Load(),
		Evictions:     c.evictions.Load(),
		Expirations:   c.expirations.Load(),
		Invalidations: c.invalidations.Load(),
	}
}

// SweepNow removes expired entries and returns how many it removed. The janitor
// calls it on every tick. Work is done in batches of SweepBatchSize with the
// lock released between batches, so concurrent Get and Set calls interleave.
func (c *Cache) SweepNow() int {
	total := 0
	for {
		removed := c.sweepBatch()
		total += removed
		if removed < c.cfg.SweepBatchSize {
			return total
		}
	}
}

func (c *Cache) sweepBatch() int {
	now := c.cfg.Clock()

	c.mu.Lock()
	defer c.mu.Unlock()

	removed := 0
	for removed < c.cfg.SweepBatchSize && len(c.expiry) > 0 && c.expiry[0].expiredAt(now) {
		c.removeLocked(c.expiry[0])
		removed++
	}
	c.expirations.Add(uint64(removed))
	return removed
}

// Close stops the janitor and waits for it to exit. It is idempotent and safe
// to call concurrently. The cache stays usable afterwards; expired entries are
// then only removed lazily. Close never holds the cache lock while waiting.
func (c *Cache) Close() error {
	c.closeOnce.Do(func() { close(c.stop) })
	c.wg.Wait()
	return nil
}

func (c *Cache) runJanitor(interval time.Duration) {
	defer c.wg.Done()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			c.janitorTick()
		}
	}
}

// janitorTick runs one sweep. A panic is logged and swallowed so one bad tick
// cannot kill the janitor, or the process. The lock is already released by the
// deferred Unlock in sweepBatch by the time recover runs.
func (c *Cache) janitorTick() {
	defer func() {
		if r := recover(); r != nil {
			c.log.Error("cache janitor recovered from panic", "panic", r, "stack", string(debug.Stack()))
		}
	}()
	if c.sweepHook != nil {
		c.sweepHook()
	}

	start := time.Now()
	if removed := c.SweepNow(); removed > 0 {
		c.log.Debug("cache sweep", "expired", removed, "remaining", c.Len(), "duration_ms", time.Since(start).Milliseconds())
	}
}

// setLocked stores or replaces an entry. Callers hold c.mu.
func (c *Cache) setLocked(now time.Time, key string, value any, ttl time.Duration, tags []string) {
	expiresAt := time.Time{}
	if ttl <= 0 {
		ttl = c.cfg.DefaultTTL
	}
	if ttl > 0 {
		expiresAt = now.Add(ttl)
	}
	tags = dedupe(tags)

	if e, ok := c.items[key]; ok {
		c.detachTagsLocked(e)
		e.value = value
		e.tags = tags
		e.expiresAt = expiresAt
		c.attachTagsLocked(e)
		c.reheapLocked(e)
		c.touchLocked(e)
		return
	}

	if len(c.items) >= c.cfg.Capacity {
		c.makeRoomLocked(now)
	}

	e := &entry{key: key, value: value, freq: 1, expiresAt: expiresAt, tags: tags, heapIdx: -1}
	c.items[key] = e
	c.attachBucketLocked(e)
	c.minFreq = 1
	if !expiresAt.IsZero() {
		heap.Push(&c.expiry, e)
	}
	c.attachTagsLocked(e)
}

// makeRoomLocked frees one slot. An already-expired entry is the preferred
// victim, because evicting a live entry while dead ones remain wastes cache.
func (c *Cache) makeRoomLocked(now time.Time) {
	if len(c.expiry) > 0 && c.expiry[0].expiredAt(now) {
		c.removeLocked(c.expiry[0])
		c.expirations.Add(1)
		return
	}

	bucket := c.freqs[c.minFreq]
	if bucket == nil || bucket.Len() == 0 {
		// minFreq is only a lower bound after arbitrary removals; recompute.
		c.minFreq = 0
		for f := range c.freqs {
			if c.minFreq == 0 || f < c.minFreq {
				c.minFreq = f
			}
		}
		bucket = c.freqs[c.minFreq]
	}
	if bucket == nil {
		return
	}
	c.removeLocked(bucket.Front().Value.(*entry))
	c.evictions.Add(1)
}

// touchLocked records an access: moves the entry to the next frequency bucket
// at the most-recently-used end.
func (c *Cache) touchLocked(e *entry) {
	old := e.freq
	c.detachBucketLocked(e)
	if old == c.minFreq && c.freqs[old] == nil {
		c.minFreq = old + 1
	}
	e.freq++
	c.attachBucketLocked(e)
}

// removeLocked deletes an entry from every index.
func (c *Cache) removeLocked(e *entry) {
	delete(c.items, e.key)
	c.detachBucketLocked(e)
	if e.heapIdx >= 0 {
		heap.Remove(&c.expiry, e.heapIdx)
	}
	c.detachTagsLocked(e)
}

func (c *Cache) attachBucketLocked(e *entry) {
	l := c.freqs[e.freq]
	if l == nil {
		l = list.New()
		c.freqs[e.freq] = l
	}
	e.elem = l.PushBack(e)
}

func (c *Cache) detachBucketLocked(e *entry) {
	l := c.freqs[e.freq]
	l.Remove(e.elem)
	e.elem = nil
	if l.Len() == 0 {
		delete(c.freqs, e.freq)
	}
}

func (c *Cache) attachTagsLocked(e *entry) {
	for _, tag := range e.tags {
		set := c.tags[tag]
		if set == nil {
			set = make(map[string]struct{})
			c.tags[tag] = set
		}
		set[e.key] = struct{}{}
	}
}

func (c *Cache) detachTagsLocked(e *entry) {
	for _, tag := range e.tags {
		if set := c.tags[tag]; set != nil {
			delete(set, e.key)
			if len(set) == 0 {
				delete(c.tags, tag)
			}
		}
	}
}

// reheapLocked brings the expiry heap in line with e.expiresAt after an
// overwrite changed it.
func (c *Cache) reheapLocked(e *entry) {
	switch {
	case e.expiresAt.IsZero() && e.heapIdx >= 0:
		heap.Remove(&c.expiry, e.heapIdx)
	case !e.expiresAt.IsZero() && e.heapIdx < 0:
		heap.Push(&c.expiry, e)
	case !e.expiresAt.IsZero():
		heap.Fix(&c.expiry, e.heapIdx)
	}
}

func (e *entry) expiredAt(now time.Time) bool {
	return !e.expiresAt.IsZero() && !now.Before(e.expiresAt)
}

func dedupe(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// expiryHeap is a min-heap of entries ordered by expiresAt. It only contains
// entries that expire.
type expiryHeap []*entry

func (h expiryHeap) Len() int           { return len(h) }
func (h expiryHeap) Less(i, j int) bool { return h[i].expiresAt.Before(h[j].expiresAt) }
func (h expiryHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].heapIdx = i
	h[j].heapIdx = j
}
func (h *expiryHeap) Push(x any) {
	e := x.(*entry)
	e.heapIdx = len(*h)
	*h = append(*h, e)
}
func (h *expiryHeap) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	e.heapIdx = -1
	*h = old[:n-1]
	return e
}

type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler           { return h }

var _ io.Closer = (*Cache)(nil)
