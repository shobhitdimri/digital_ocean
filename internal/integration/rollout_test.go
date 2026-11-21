package integration

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func (s *server) createRolloutFlag(name string, enabled bool, percent int) flagBody {
	s.t.Helper()
	r := s.mustStatus(s.call("POST", "/v1/flags", "tester",
		map[string]any{"name": name, "enabled": enabled, "rollout_percent": percent}), 201, "create "+name)
	return decode[flagBody](s.t, r)
}

func (s *server) setRollout(name string, percent int) flagBody {
	s.t.Helper()
	return s.updateFlag(name, map[string]any{"rollout_percent": percent})
}

func idsFor(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s-%04d", prefix, i)
	}
	return out
}

// cohort returns the set of users for whom the flag is currently on.
func (s *server) cohort(flag string, users []string) map[string]bool {
	s.t.Helper()
	in := map[string]bool{}
	for _, u := range users {
		if s.evaluate(flag, u).Enabled {
			in[u] = true
		}
	}
	return in
}

func rolloutOf(t *testing.T, f flagBody) int {
	t.Helper()
	if f.RolloutPercent == nil {
		t.Fatalf("response has no rollout_percent: %+v", f)
	}
	return *f.RolloutPercent
}

func TestRolloutSplitsUsersAndExplainsEachAnswer(t *testing.T) {
	s := startServer(t, testConfig(t))
	created := s.createRolloutFlag("gradual", true, 30)
	if got := rolloutOf(t, created); got != 30 {
		t.Fatalf("created with rollout_percent %d, want 30", got)
	}

	users := idsFor("split", 400)
	in := 0
	for _, u := range users {
		got := s.evaluate("gradual", u)
		switch {
		case got.Enabled && got.Reason == "rollout_included":
			in++
		case !got.Enabled && got.Reason == "rollout_excluded":
		default:
			t.Fatalf("%s: enabled=%v reason=%s", u, got.Enabled, got.Reason)
		}
		if got.Source != "store" {
			t.Fatalf("%s: first answer came from %s, want store", u, got.Source)
		}
	}
	// 30% of 400 is 120 with a standard deviation of about 9: eight is a very
	// wide margin, so this fails only if the split is genuinely wrong.
	if in < 120-72 || in > 120+72 {
		t.Fatalf("%d of 400 users were included at 30%%", in)
	}

	// Asked again: same answers, now from the cache.
	for _, u := range users[:50] {
		first := s.evaluate("gradual", u)
		if first.Source != "cache" {
			t.Fatalf("%s: second answer came from %s, want cache", u, first.Source)
		}
	}
}

// These users have known buckets (see the evaluator's pinned values), so this
// checks the documented rule exactly, through HTTP: bucket < percent * 100.
func TestRolloutBoundariesAreExactEndToEnd(t *testing.T) {
	s := startServer(t, testConfig(t))
	s.createRolloutFlag("checkout", true, 4)

	// alice has bucket 410 (4.10%), bob has 5030 (50.30%).
	s.expectEval("checkout", "alice", false, "rollout_excluded", "store")
	s.setRollout("checkout", 5)
	s.expectEval("checkout", "alice", true, "rollout_included", "store")
	s.setRollout("checkout", 50)
	s.expectEval("checkout", "alice", true, "rollout_included", "store")
	s.expectEval("checkout", "bob", false, "rollout_excluded", "store")
	s.setRollout("checkout", 51)
	s.expectEval("checkout", "bob", true, "rollout_included", "store")
}

func TestARolloutOnlyEverGrowsTheAudience(t *testing.T) {
	s := startServer(t, testConfig(t))
	s.createRolloutFlag("ramp", true, 0)
	users := idsFor("ramp", 300)

	var prev map[string]bool
	for _, pct := range []int{0, 5, 10, 25, 50, 75, 90, 100} {
		if pct != 0 {
			s.setRollout("ramp", pct)
		}
		cur := s.cohort("ramp", users)
		for u := range prev {
			if !cur[u] {
				t.Fatalf("%s was in the audience before the step to %d%% and dropped out", u, pct)
			}
		}
		if pct == 0 && len(cur) != 0 {
			t.Fatalf("0%% included %d users", len(cur))
		}
		if pct == 100 && len(cur) != len(users) {
			t.Fatalf("100%% included %d of %d users", len(cur), len(users))
		}
		prev = cur
	}
	s.expectEval("ramp", users[0], true, "global_on", "")
}

func TestRolloutChangeIsVisibleImmediatelyDespiteTheCache(t *testing.T) {
	s := startServer(t, testConfig(t))
	s.createRolloutFlag("cached", true, 0)
	s.createRolloutFlag("bystander", true, 0)
	users := idsFor("c", 30)

	for _, u := range users {
		s.expectEval("cached", u, false, "rollout_excluded", "store")
		s.expectEval("cached", u, false, "rollout_excluded", "cache")
		s.expectEval("bystander", u, false, "rollout_excluded", "store")
	}

	s.setRollout("cached", 100)
	for _, u := range users {
		s.expectEval("cached", u, true, "global_on", "store")
	}
	for _, u := range users {
		s.expectEval("bystander", u, false, "rollout_excluded", "cache")
	}
}

func TestOverridesAndTheGlobalSwitchStillOutrankTheRollout(t *testing.T) {
	s := startServer(t, testConfig(t))
	s.createRolloutFlag("ranked", true, 0)
	s.setOverride("ranked", "vip", true)
	s.expectEval("ranked", "vip", true, "user_override", "store")
	s.expectEval("ranked", "someone", false, "rollout_excluded", "store")

	s.setRollout("ranked", 100)
	s.setOverride("ranked", "blocked", false)
	s.expectEval("ranked", "blocked", false, "user_override", "store")

	users := idsFor("g", 100)
	before := s.cohort("ranked", users)
	if len(before) != len(users) {
		t.Fatalf("100%% included %d of %d", len(before), len(users))
	}

	s.setRollout("ranked", 50)
	half := s.cohort("ranked", users)
	s.updateFlag("ranked", map[string]any{"enabled": false})
	for _, u := range users {
		s.expectEval("ranked", u, false, "global_off", "store")
	}
	s.expectEval("ranked", "vip", true, "user_override", "")

	// Turning the flag back on must restore exactly the same audience.
	s.updateFlag("ranked", map[string]any{"enabled": true})
	again := s.cohort("ranked", users)
	if len(again) != len(half) {
		t.Fatalf("audience changed from %d to %d users across an off/on cycle", len(half), len(again))
	}
	for u := range half {
		if !again[u] {
			t.Fatalf("%s lost access across an off/on cycle", u)
		}
	}
}

func TestRolloutSurvivesARestartWithTheSameAudience(t *testing.T) {
	cfg := testConfig(t)
	first := startServer(t, cfg)
	first.createRolloutFlag("durable", true, 40)
	first.createRolloutFlag("none", true, 0)
	first.createFlag("everyone", true)
	users := idsFor("d", 200)
	before := first.cohort("durable", users)
	first.stop()

	onDisk := readFile(t, filepath.Join(cfg.DataDir, "flags.json"))
	if !strings.Contains(onDisk, `"rollout_percent": 40`) || !strings.Contains(onDisk, `"rollout_percent": 0`) {
		t.Fatalf("rollouts not in flags.json:\n%s", onDisk)
	}
	if !strings.Contains(onDisk, `"schema_version": 3`) {
		t.Fatalf("flags.json is not schema 3:\n%s", onDisk)
	}

	second := startServer(t, cfg)
	for name, want := range map[string]int{"durable": 40, "none": 0, "everyone": 100} {
		f := decode[flagBody](t, second.mustStatus(second.call("GET", "/v1/flags/"+name, "", nil), 200, "get "+name))
		if rolloutOf(t, f) != want {
			t.Fatalf("%s came back with rollout %d, want %d", name, rolloutOf(t, f), want)
		}
	}
	after := second.cohort("durable", users)
	if len(after) != len(before) {
		t.Fatalf("audience changed across the restart: %d then %d", len(before), len(after))
	}
	for u := range before {
		if !after[u] {
			t.Fatalf("%s was in the audience before the restart and not after", u)
		}
	}
}

func TestRolloutChangesAreAudited(t *testing.T) {
	s := startServer(t, testConfig(t))
	s.createRolloutFlag("audited", true, 10)
	s.mustStatus(s.call("PUT", "/v1/flags/audited", "carol", map[string]any{"rollout_percent": 55}, "X-Request-ID", "req-rollout"), 200, "update")

	h := s.audit("audited")
	if len(h) != 2 {
		t.Fatalf("%d audit entries, want 2", len(h))
	}
	e := h[0]
	if e.Actor != "carol" || e.Action != "flag.updated" || e.RequestID != "req-rollout" {
		t.Fatalf("entry = %+v", e)
	}
	if e.Before == nil || rolloutOf(t, *e.Before) != 10 || e.After == nil || rolloutOf(t, *e.After) != 55 {
		t.Fatalf("history must show 10 -> 55, got before=%+v after=%+v", e.Before, e.After)
	}
}

func TestInvalidRolloutsAreRejectedWithoutSideEffects(t *testing.T) {
	s := startServer(t, testConfig(t))

	for _, bad := range []any{-1, 101, 12.5, "50", true} {
		r := s.call("POST", "/v1/flags", "tester", map[string]any{"name": "nope", "rollout_percent": bad})
		if r.Status != 400 || decode[errorBody](t, r).Error.Code != "invalid_request" {
			t.Fatalf("create with %#v: %d %s, want 400 invalid_request", bad, r.Status, r.Body)
		}
	}
	if r := s.call("GET", "/v1/flags/nope", "", nil); r.Status != 404 {
		t.Fatalf("a rejected create left a flag behind: %d", r.Status)
	}

	s.createRolloutFlag("stays", true, 20)
	for _, bad := range []any{-1, 101, 12.5, "50", true} {
		r := s.call("PUT", "/v1/flags/stays", "tester", map[string]any{"rollout_percent": bad, "description": "must not apply"})
		if r.Status != 400 || decode[errorBody](t, r).Error.Code != "invalid_request" {
			t.Fatalf("update with %#v: %d %s, want 400 invalid_request", bad, r.Status, r.Body)
		}
	}
	got := decode[flagBody](t, s.mustStatus(s.call("GET", "/v1/flags/stays", "", nil), 200, "get"))
	if rolloutOf(t, got) != 20 || got.Version != 1 || got.Description != "" {
		t.Fatalf("a rejected update changed the flag: %+v", got)
	}
	if n := len(s.audit("stays")); n != 1 {
		t.Fatalf("rejected updates left %d audit entries, want only the creation", n)
	}
}

func TestRolloutWritesNeedAnActorLikeAnyOtherWrite(t *testing.T) {
	s := startServer(t, testConfig(t))
	s.createFlag("needs-actor", true)
	r := s.call("PUT", "/v1/flags/needs-actor", "", map[string]any{"rollout_percent": 10})
	if r.Status != 400 || decode[errorBody](t, r).Error.Code != "missing_actor" {
		t.Fatalf("got %d %s", r.Status, r.Body)
	}
}

func TestUnrestrictedFlagsReportFullRolloutAndNeverAlterOldBehaviour(t *testing.T) {
	s := startServer(t, testConfig(t))
	created := s.createFlag("classic", true)
	if rolloutOf(t, created) != 100 {
		t.Fatalf("a flag created without a rollout reports %d, want 100", rolloutOf(t, created))
	}
	s.expectEval("classic", "anyone", true, "global_on", "store")

	// Asking for the state it is already in changes nothing.
	same := s.setRollout("classic", 100)
	if same.Version != created.Version || len(s.audit("classic")) != 1 {
		t.Fatalf("a no-op rollout update wrote a new version or audit entry: %+v", same)
	}

	list := decode[struct {
		Flags []flagBody `json:"flags"`
	}](t, s.mustStatus(s.call("GET", "/v1/flags", "", nil), 200, "list"))
	for _, f := range list.Flags {
		if f.RolloutPercent == nil {
			t.Fatalf("listed flag %s has no rollout_percent", f.Name)
		}
	}
}

// The cache-aside stale-fill race, now with rollouts: readers keep the cache hot
// while the rollout flips between nobody and everybody. Once an update is
// acknowledged, no evaluation may answer from the old state.
func TestAnAcknowledgedRolloutChangeIsNeverFollowedByAStaleAnswer(t *testing.T) {
	s := startServer(t, testConfig(t))
	const flag = "flip"
	s.createRolloutFlag(flag, true, 0)
	users := idsFor("flip", 6)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
					s.callLoad("GET", "/v1/evaluate?flag="+flag+"&user_id="+users[(i+r)%len(users)], "", nil)
				}
			}
		}(r)
	}

	percent := 0
	for i := 0; i < 60; i++ {
		percent = 100 - percent
		s.setRollout(flag, percent)
		for _, u := range users {
			if got := s.evaluate(flag, u); got.Enabled != (percent == 100) {
				close(stop)
				wg.Wait()
				t.Fatalf("update %d set the rollout to %d%% but %s was told enabled=%v (reason %s, source %s)",
					i, percent, u, got.Enabled, got.Reason, got.Source)
			}
		}
	}
	close(stop)
	wg.Wait()
}
