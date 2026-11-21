// Package cache defines the caching contract the service layer depends on.
// Implementations (in-memory LFU today, Redis later) live in sub-packages and
// are chosen only in the composition root.
package cache

import "time"

// Cache is a key-value cache with expiry and tag-based invalidation.
//
// All methods are safe for concurrent use. Values are stored by reference, so
// callers must treat a value as immutable once it has been passed to Set.
//
// # Avoiding stale fills
//
// The classic cache-aside race: a reader misses, loads the old value from the
// source, a writer then updates the source and invalidates, and finally the
// reader stores its now-stale value, which stays cached until it expires.
// Plain Set cannot prevent that. Readers that fill the cache after a miss
// should use the epoch protocol instead:
//
//	epoch := c.Epoch()          // 1. before reading the source
//	v := loadFromSource()       // 2. read
//	c.SetIfEpoch(k, v, ttl, epoch, tags...) // 3. store only if nothing was invalidated since step 1
//
// Writers simply update the source and then call InvalidateTag or Delete.
type Cache interface {
	// Get returns the value for key. Expired entries are never returned.
	Get(key string) (value any, ok bool)

	// Set stores value under key and attaches it to the given tags. A ttl of
	// zero or less uses the implementation's default TTL.
	Set(key string, value any, ttl time.Duration, tags ...string)

	// Epoch returns the current invalidation epoch. It increases on every
	// Delete and InvalidateTag call, whether or not anything was removed.
	Epoch() uint64

	// SetIfEpoch behaves like Set but stores nothing, and returns false, when
	// the epoch has changed since the caller read it.
	SetIfEpoch(key string, value any, ttl time.Duration, epoch uint64, tags ...string) bool

	// Delete removes one key.
	Delete(key string)

	// InvalidateTag removes every entry that was stored with the tag.
	InvalidateTag(tag string)
}
