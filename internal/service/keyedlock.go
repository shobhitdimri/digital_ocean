package service

import (
	"context"
	"sync"
)

// keyedLock serialises work per key, so writes to one flag queue up while
// writes to other flags proceed in parallel.
//
// Concurrency rules:
//
//   - mu guards only the entries map and the ref counts. It is never held while
//     waiting for a key, so it cannot take part in a deadlock.
//   - Waiting for a key uses a channel, so a waiter can give up when its context
//     ends instead of hanging behind a slow writer.
//   - An entry is deleted when its last holder or waiter leaves, so the map
//     does not grow with the number of flag names ever seen.
type keyedLock struct {
	mu      sync.Mutex
	entries map[string]*lockEntry
}

type lockEntry struct {
	slot chan struct{} // holds one token while the key is locked
	refs int           // holders plus waiters, guarded by keyedLock.mu
}

func newKeyedLock() *keyedLock {
	return &keyedLock{entries: make(map[string]*lockEntry)}
}

// Lock waits for exclusive access to key. It returns the unlock function, or
// the context's error if the context ends first. The caller must call unlock
// exactly once.
func (k *keyedLock) Lock(ctx context.Context, key string) (unlock func(), err error) {
	// select picks at random when several cases are ready, so check first: a
	// request whose context has already ended must never proceed to write.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	k.mu.Lock()
	e, ok := k.entries[key]
	if !ok {
		e = &lockEntry{slot: make(chan struct{}, 1)}
		k.entries[key] = e
	}
	e.refs++
	k.mu.Unlock()

	select {
	case e.slot <- struct{}{}:
		return func() {
			<-e.slot
			k.release(key, e)
		}, nil
	case <-ctx.Done():
		k.release(key, e)
		return nil, ctx.Err()
	}
}

func (k *keyedLock) release(key string, e *lockEntry) {
	k.mu.Lock()
	defer k.mu.Unlock()
	e.refs--
	if e.refs == 0 {
		delete(k.entries, key)
	}
}

// size reports the number of tracked keys. It exists for tests.
func (k *keyedLock) size() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.entries)
}
