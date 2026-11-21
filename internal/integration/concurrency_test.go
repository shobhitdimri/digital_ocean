package integration

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// TestConcurrentWritersAndReadersLeaveAConsistentRecord hammers one flag with
// writers and readers over real files and real TCP, then checks that the audit
// trail is a gap-free replay of what happened and that the cache agrees with
// the store once the dust settles. Run it with -race.
func TestConcurrentWritersAndReadersLeaveAConsistentRecord(t *testing.T) {
	s := startServer(t, testConfig(t))
	const (
		flag            = "hot"
		writers         = 6
		writesPerWriter = 15
		readers         = 6
	)
	s.createFlag(flag, false)

	var serverErrors atomic.Int64
	check := func(r response) {
		if r.Status >= 500 {
			serverErrors.Add(1)
			t.Errorf("server error %d: %s", r.Status, r)
		}
	}

	var readersWG, writersWG sync.WaitGroup
	stop := make(chan struct{})
	for r := 0; r < readers; r++ {
		readersWG.Add(1)
		go func(r int) {
			defer readersWG.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				resp := s.callLoad("GET", fmt.Sprintf("/v1/evaluate?flag=%s&user_id=writer-%d", flag, i%writers), "", nil)
				check(resp)
				if resp.Status != 200 {
					t.Errorf("evaluate = %d: %s", resp.Status, resp)
					return
				}
			}
		}(r)
	}

	for w := 0; w < writers; w++ {
		writersWG.Add(1)
		go func(w int) {
			defer writersWG.Done()
			user := fmt.Sprintf("writer-%d", w)
			for i := 0; i < writesPerWriter; i++ {
				var resp response
				switch i % 3 {
				case 0:
					resp = s.call("PUT", "/v1/flags/"+flag, user, map[string]any{"enabled": (w+i)%2 == 0})
				case 1:
					resp = s.call("PUT", "/v1/flags/"+flag+"/overrides/"+user, user, map[string]any{"enabled": i%2 == 0})
				default:
					resp = s.call("PUT", "/v1/flags/"+flag, user, map[string]any{"description": fmt.Sprintf("by %s #%d", user, i)})
				}
				check(resp)
				if resp.Status != 200 {
					t.Errorf("write by %s #%d = %d: %s", user, i, resp.Status, resp)
					return
				}
			}
		}(w)
	}
	writersWG.Wait()
	close(stop)
	readersWG.Wait()
	if t.Failed() {
		return
	}

	// Overrides and descriptions always change something, so each must appear.
	// A global toggle that sets the value it already has is a deliberate no-op
	// (nothing written, no audit entry), so toggles may be missing, but never
	// invented. Each recorded change bumps the version by exactly one.
	entries := s.audit(flag)
	alwaysChanging := 1 + writers*(writesPerWriter*2/3)
	maxEntries := 1 + writers*writesPerWriter
	if len(entries) < alwaysChanging || len(entries) > maxEntries {
		t.Fatalf("audit has %d entries, want between %d and %d", len(entries), alwaysChanging, maxEntries)
	}
	if got := decode[flagBody](t, s.mustStatus(s.call("GET", "/v1/flags/"+flag, "", nil), 200, "get")).Version; got != int64(len(entries)) {
		t.Fatalf("final version is %d but the audit trail has %d entries: a change was not recorded, or one was recorded twice", got, len(entries))
	}

	// Oldest first, each entry must continue exactly where the last one ended.
	var prev *flagBody
	seenIDs := map[string]bool{}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if seenIDs[e.ID] {
			t.Fatalf("duplicate audit id %s", e.ID)
		}
		seenIDs[e.ID] = true
		if e.After == nil {
			t.Fatalf("entry %d has no 'after'", i)
		}
		if prev == nil {
			if e.Before != nil || e.Action != "flag.created" {
				t.Fatalf("history does not start with the creation: %+v", e)
			}
		} else {
			if e.Before == nil || e.Before.Version != prev.Version || e.After.Version != prev.Version+1 {
				t.Fatalf("version chain broken at entry %d: before=%+v after=%+v, previous after=%+v", i, e.Before, e.After, prev)
			}
			if e.Before.Enabled != prev.Enabled || len(e.Before.Overrides) != len(prev.Overrides) {
				t.Fatalf("a write started from state that was never recorded at entry %d: %+v vs %+v", i, e.Before, prev)
			}
		}
		prev = e.After
	}

	final := decode[flagBody](t, s.mustStatus(s.call("GET", "/v1/flags/"+flag, "", nil), 200, "get final"))
	if final.Version != prev.Version || final.Enabled != prev.Enabled || len(final.Overrides) != len(prev.Overrides) {
		t.Fatalf("stored flag %+v differs from the last audit 'after' %+v", final, prev)
	}
	for user, want := range prev.Overrides {
		if final.Overrides[user] != want {
			t.Fatalf("override for %s is %v in the store, %v in the audit trail", user, final.Overrides[user], want)
		}
	}

	// With writes finished, every answer, cached or not, must match the truth.
	for round := 0; round < 2; round++ {
		for w := 0; w < writers; w++ {
			user := fmt.Sprintf("writer-%d", w)
			want, hasOverride := final.Overrides[user]
			if !hasOverride {
				want = final.Enabled
			}
			if got := s.evaluate(flag, user); got.Enabled != want {
				t.Fatalf("round %d: %s evaluates to %v, truth is %v (source %s)", round, user, got.Enabled, want, got.Source)
			}
		}
		if got := s.evaluate(flag, "nobody"); got.Enabled != final.Enabled {
			t.Fatalf("round %d: a user without an override evaluates to %v, global state is %v", round, got.Enabled, final.Enabled)
		}
	}
}

// TestAnAcknowledgedWriteIsNeverFollowedByAStaleAnswer keeps the cache hot with
// background readers while a writer toggles the flag. The moment an update is
// acknowledged, the next evaluation must reflect it. This is the cache-aside
// stale-fill race, tested end to end.
func TestAnAcknowledgedWriteIsNeverFollowedByAStaleAnswer(t *testing.T) {
	s := startServer(t, testConfig(t))
	const flag = "toggle"
	s.createFlag(flag, false)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					s.callLoad("GET", "/v1/evaluate?flag="+flag+"&user_id=shared", "", nil)
				}
			}
		}()
	}

	state := false
	for i := 0; i < 80; i++ {
		state = !state
		s.updateFlag(flag, map[string]any{"enabled": state})
		if got := s.evaluate(flag, "shared"); got.Enabled != state {
			close(stop)
			wg.Wait()
			t.Fatalf("update %d set enabled=%v, but the next evaluation said %v (source %s)", i, state, got.Enabled, got.Source)
		}
	}
	close(stop)
	wg.Wait()
}

// TestConcurrentCreatesOfOneNameHaveOneWinner checks that uniqueness holds on
// the real file store, and that losers get 409 rather than an error.
func TestConcurrentCreatesOfOneNameHaveOneWinner(t *testing.T) {
	s := startServer(t, testConfig(t))
	const contenders = 25
	var created, conflicts, other atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := s.call("POST", "/v1/flags", fmt.Sprintf("actor-%d", i), map[string]any{"name": "contested"})
			switch r.Status {
			case 201:
				created.Add(1)
			case 409:
				conflicts.Add(1)
			default:
				other.Add(1)
				t.Errorf("unexpected %d: %s", r.Status, r)
			}
		}(i)
	}
	wg.Wait()
	if created.Load() != 1 || conflicts.Load() != contenders-1 || other.Load() != 0 {
		t.Fatalf("created=%d conflicts=%d other=%d, want 1, %d, 0", created.Load(), conflicts.Load(), other.Load(), contenders-1)
	}
	if h := s.audit("contested"); len(h) != 1 || h[0].Action != "flag.created" {
		t.Fatalf("audit = %+v, want exactly one creation", h)
	}
}

// TestWritesThatChangeNothingAreNoOps checks the idempotency rule end to end: a
// repeated write is acknowledged, but changes no version, adds no audit entry
// and leaves cached answers alone. Removing an override that was never set is
// a 404, since there is nothing to remove.
func TestWritesThatChangeNothingAreNoOps(t *testing.T) {
	s := startServer(t, testConfig(t))
	const flag = "idempotent"
	s.createFlag(flag, true)
	s.setOverride(flag, "u1", false)
	s.expectEval(flag, "u1", false, "user_override", "store")
	s.expectEval(flag, "u1", false, "user_override", "cache")
	before := len(s.audit(flag))

	same := s.updateFlag(flag, map[string]any{"enabled": true})
	if same.Version != 2 {
		t.Fatalf("a no-op update moved the version to %d, want it to stay 2", same.Version)
	}
	s.setOverride(flag, "u1", false)
	if r := s.call("DELETE", "/v1/flags/"+flag+"/overrides/never-set", "tester", nil); r.Status != 404 {
		t.Fatalf("removing an override that does not exist = %d, want 404", r.Status)
	}

	if after := len(s.audit(flag)); after != before {
		t.Fatalf("no-op writes added %d audit entries", after-before)
	}
	s.expectEval(flag, "u1", false, "user_override", "cache")
}
