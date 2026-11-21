package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func withTimeout(t *testing.T, d time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatal("deadlock suspected: test did not finish in time")
	}
}

func TestKeyedLockMutualExclusionPerKey(t *testing.T) {
	withTimeout(t, 20*time.Second, func() {
		k := newKeyedLock()
		counters := map[string]*int{"a": new(int), "b": new(int)}
		var wg sync.WaitGroup
		for _, key := range []string{"a", "b"} {
			for g := 0; g < 20; g++ {
				wg.Add(1)
				go func(key string) {
					defer wg.Done()
					for i := 0; i < 200; i++ {
						unlock, err := k.Lock(context.Background(), key)
						if err != nil {
							t.Error(err)
							return
						}
						// A plain int guarded only by the keyed lock: -race fails if
						// two holders of one key ever overlap.
						*counters[key]++
						unlock()
					}
				}(key)
			}
		}
		wg.Wait()
		if *counters["a"] != 4000 || *counters["b"] != 4000 {
			t.Errorf("counters = %d, %d", *counters["a"], *counters["b"])
		}
		if k.size() != 0 {
			t.Errorf("%d entries leaked", k.size())
		}
	})
}

func TestKeyedLockDifferentKeysDoNotBlockEachOther(t *testing.T) {
	withTimeout(t, 5*time.Second, func() {
		k := newKeyedLock()
		unlockA, _ := k.Lock(context.Background(), "a")
		defer unlockA()
		unlockB, err := k.Lock(context.Background(), "b")
		if err != nil {
			t.Fatal(err)
		}
		unlockB()
	})
}

func TestKeyedLockWaiterGivesUpWhenContextEnds(t *testing.T) {
	withTimeout(t, 5*time.Second, func() {
		k := newKeyedLock()
		unlock, _ := k.Lock(context.Background(), "a")

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		if _, err := k.Lock(ctx, "a"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want DeadlineExceeded", err)
		}

		unlock()
		if k.size() != 0 {
			t.Fatalf("entry leaked after a waiter gave up: %d", k.size())
		}
		// The abandoned waiter must not have left the key stuck.
		unlock2, err := k.Lock(context.Background(), "a")
		if err != nil {
			t.Fatal(err)
		}
		unlock2()
	})
}

func TestKeyedLockAlreadyCancelledContextNeverLocks(t *testing.T) {
	k := newKeyedLock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 100; i++ {
		unlock, err := k.Lock(ctx, "a")
		if !errors.Is(err, context.Canceled) || unlock != nil {
			t.Fatalf("iteration %d: got (%v, %v), want context.Canceled", i, unlock != nil, err)
		}
	}
	if k.size() != 0 {
		t.Fatalf("entry leaked: %d", k.size())
	}
}

func TestKeyedLockHandoffToWaiter(t *testing.T) {
	withTimeout(t, 5*time.Second, func() {
		k := newKeyedLock()
		unlock, _ := k.Lock(context.Background(), "a")

		got := make(chan struct{})
		go func() {
			u, err := k.Lock(context.Background(), "a")
			if err != nil {
				t.Error(err)
				return
			}
			close(got)
			u()
		}()
		time.Sleep(20 * time.Millisecond)
		select {
		case <-got:
			t.Fatal("waiter got the lock while it was held")
		default:
		}
		unlock()
		<-got
	})
}
