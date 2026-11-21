package evaluator_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"feature-flag-service/internal/dao"
	"feature-flag-service/internal/domain"
	"feature-flag-service/internal/evaluator"
)

// A store that only reads satisfies the evaluator's own interface, so the
// evaluator never needs the write half of the DAO.
var _ evaluator.FlagLookup = dao.FlagReader(nil)

// ---- fakes -----------------------------------------------------------------

type mapLookup map[string]domain.Flag

func (m mapLookup) Get(_ context.Context, name string) (domain.Flag, error) {
	f, ok := m[name]
	if !ok {
		return domain.Flag{}, domain.NotFoundf("flag %q", name)
	}
	return f.Clone(), nil
}

type failingLookup struct{ err error }

func (f failingLookup) Get(context.Context, string) (domain.Flag, error) {
	return domain.Flag{}, f.err
}

func flag(name string, enabled bool, overrides map[string]bool) domain.Flag {
	return domain.Flag{
		Name:      name,
		Enabled:   enabled,
		Overrides: overrides,
		Version:   1,
		CreatedAt: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC),
	}
}

func newDefault(t *testing.T, flags ...domain.Flag) *evaluator.Evaluator {
	t.Helper()
	store := mapLookup{}
	for _, f := range flags {
		store[f.Name] = f
	}
	e, err := evaluator.NewDefault(store)
	if err != nil {
		t.Fatalf("NewDefault: %v", err)
	}
	return e
}

// ---- required evaluation behaviour -----------------------------------------

func TestEvaluationRules(t *testing.T) {
	tests := []struct {
		name       string
		flag       domain.Flag
		userID     string
		wantOn     bool
		wantReason evaluator.Reason
		wantRule   string
	}{
		{
			name: "global on, no override", flag: flag("f", true, nil), userID: "alice",
			wantOn: true, wantReason: evaluator.ReasonGlobalOn, wantRule: "global_on",
		},
		{
			name: "global off, no override", flag: flag("f", false, nil), userID: "alice",
			wantOn: false, wantReason: evaluator.ReasonGlobalOff, wantRule: "global_off",
		},
		{
			name: "override on beats global off", flag: flag("f", false, map[string]bool{"alice": true}), userID: "alice",
			wantOn: true, wantReason: evaluator.ReasonUserOverride, wantRule: "override",
		},
		{
			name: "override off beats global on", flag: flag("f", true, map[string]bool{"alice": false}), userID: "alice",
			wantOn: false, wantReason: evaluator.ReasonUserOverride, wantRule: "override",
		},
		{
			name: "override for another user does not apply (global on)", flag: flag("f", true, map[string]bool{"bob": false}), userID: "alice",
			wantOn: true, wantReason: evaluator.ReasonGlobalOn, wantRule: "global_on",
		},
		{
			name: "override for another user does not apply (global off)", flag: flag("f", false, map[string]bool{"bob": true}), userID: "alice",
			wantOn: false, wantReason: evaluator.ReasonGlobalOff, wantRule: "global_off",
		},
		{
			name: "override matches the exact user id only", flag: flag("f", false, map[string]bool{"alice": true}), userID: "Alice",
			wantOn: false, wantReason: evaluator.ReasonGlobalOff, wantRule: "global_off",
		},
		{
			name: "override agreeing with global still reports the override", flag: flag("f", true, map[string]bool{"alice": true}), userID: "alice",
			wantOn: true, wantReason: evaluator.ReasonUserOverride, wantRule: "override",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newDefault(t, tc.flag)
			got, err := e.Evaluate(context.Background(), "f", tc.userID)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if got.Enabled != tc.wantOn || got.Reason != tc.wantReason || got.DecidedBy != tc.wantRule {
				t.Fatalf("got enabled=%v reason=%q by=%q; want enabled=%v reason=%q by=%q",
					got.Enabled, got.Reason, got.DecidedBy, tc.wantOn, tc.wantReason, tc.wantRule)
			}
			if got.Flag != "f" || got.UserID != tc.userID {
				t.Fatalf("result identity wrong: %+v", got)
			}
			if !reflect.DeepEqual(got.Consulted, []string{"f"}) {
				t.Fatalf("Consulted = %v, want [f]", got.Consulted)
			}
		})
	}
}

func TestEvaluateFlagMatchesEvaluate(t *testing.T) {
	f := flag("f", false, map[string]bool{"alice": true})
	e := newDefault(t, f)

	viaLookup, err := e.Evaluate(context.Background(), "f", "alice")
	if err != nil {
		t.Fatal(err)
	}
	direct, err := e.EvaluateFlag(context.Background(), f, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(viaLookup, direct) {
		t.Fatalf("EvaluateFlag %+v differs from Evaluate %+v", direct, viaLookup)
	}
}

func TestMissingFlagIsNotFound(t *testing.T) {
	e := newDefault(t)
	got, err := e.Evaluate(context.Background(), "ghost", "alice")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if !reflect.DeepEqual(got, evaluator.Result{}) {
		t.Fatalf("an error must come with a zero Result, got %+v", got)
	}
}

func TestInvalidInput(t *testing.T) {
	e := newDefault(t, flag("f", true, nil))
	tests := []struct{ name, flag, user string }{
		{"flag name with space", "bad name", "alice"},
		{"empty flag name", "", "alice"},
		{"too long flag name", strings.Repeat("a", domain.MaxNameLength+1), "alice"},
		{"empty user", "f", ""},
		{"user with space", "f", "al ice"},
		{"too long user", "f", strings.Repeat("u", domain.MaxUserIDLength+1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := e.Evaluate(context.Background(), tc.flag, tc.user); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
		})
	}

	if _, err := e.EvaluateFlag(context.Background(), flag("f", true, nil), ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("EvaluateFlag with empty user: want ErrInvalid, got %v", err)
	}
}

func TestCancelledContext(t *testing.T) {
	e := newDefault(t, flag("f", true, nil))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Evaluate(ctx, "f", "alice"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Evaluate: want context.Canceled, got %v", err)
	}
	if _, err := e.EvaluateFlag(ctx, flag("f", true, nil), "alice"); !errors.Is(err, context.Canceled) {
		t.Fatalf("EvaluateFlag: want context.Canceled, got %v", err)
	}
}

func TestStorageFailureIsNotMistakenForNotFound(t *testing.T) {
	boom := errors.New("disk on fire")
	e, err := evaluator.NewDefault(failingLookup{err: boom})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Evaluate(context.Background(), "f", "alice")
	if !errors.Is(err, boom) || errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("want the storage error unchanged, got %v", err)
	}
}

func TestEvaluationDoesNotMutateTheFlag(t *testing.T) {
	f := flag("f", false, map[string]bool{"alice": true, "bob": false})
	before := f.Clone()
	e := newDefault(t, f)

	for _, user := range []string{"alice", "bob", "carol"} {
		if _, err := e.EvaluateFlag(context.Background(), f, user); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(f, before) {
		t.Fatalf("evaluation changed the flag:\n got %+v\nwant %+v", f, before)
	}
}

// ---- construction ----------------------------------------------------------

func TestNewValidation(t *testing.T) {
	if _, err := evaluator.New(nil, evaluator.GlobalOnRule{}); err == nil {
		t.Error("nil lookup should be rejected")
	}
	if _, err := evaluator.New(mapLookup{}); err == nil {
		t.Error("an empty rule chain should be rejected")
	}
	if _, err := evaluator.New(mapLookup{}, evaluator.OverrideRule{}, nil); err == nil {
		t.Error("a nil rule should be rejected")
	}
}

func TestDefaultRuleOrder(t *testing.T) {
	var names []string
	for _, r := range evaluator.DefaultRules() {
		names = append(names, r.Name())
	}
	if !reflect.DeepEqual(names, []string{"prerequisite", "override", "global_off", "rollout", "global_on"}) {
		t.Fatalf("default chain = %v", names)
	}
}

func TestEvaluatorIsolatedFromCallerMutatingTheRuleSlice(t *testing.T) {
	rules := evaluator.DefaultRules()
	e, err := evaluator.New(mapLookup{"f": flag("f", true, nil)}, rules...)
	if err != nil {
		t.Fatal(err)
	}
	rules[0] = nil
	rules[2] = nil

	if _, err := e.Evaluate(context.Background(), "f", "alice"); err != nil {
		t.Fatalf("evaluator should hold its own copy of the chain: %v", err)
	}
}

// ---- open/closed: extending by adding rules --------------------------------

type funcRule struct {
	name string
	fn   func(ctx context.Context, in evaluator.Input) (evaluator.Decision, bool, error)
}

func (r funcRule) Name() string { return r.name }
func (r funcRule) Apply(ctx context.Context, in evaluator.Input) (evaluator.Decision, bool, error) {
	return r.fn(ctx, in)
}

func TestFirstDecidingRuleWinsAndLaterRulesAreSkipped(t *testing.T) {
	var calls []string
	record := func(name string, decide bool) evaluator.Rule {
		return funcRule{name: name, fn: func(context.Context, evaluator.Input) (evaluator.Decision, bool, error) {
			calls = append(calls, name)
			return evaluator.Decision{Enabled: decide, Reason: "test"}, decide, nil
		}}
	}
	e, err := evaluator.New(mapLookup{"f": flag("f", true, nil)},
		record("first", false), record("second", true), record("third", true))
	if err != nil {
		t.Fatal(err)
	}

	got, err := e.Evaluate(context.Background(), "f", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got.DecidedBy != "second" || !reflect.DeepEqual(calls, []string{"first", "second"}) {
		t.Fatalf("decidedBy=%q calls=%v; want second, [first second]", got.DecidedBy, calls)
	}
}

func TestNewRuleCanBeInsertedWithoutTouchingTheEvaluator(t *testing.T) {
	killSwitch := funcRule{name: "kill_switch", fn: func(_ context.Context, in evaluator.Input) (evaluator.Decision, bool, error) {
		if strings.HasPrefix(in.Flag.Name, "legacy-") {
			return evaluator.Decision{Enabled: false, Reason: "killed"}, true, nil
		}
		return evaluator.Decision{}, false, nil
	}}
	store := mapLookup{
		"legacy-ui": flag("legacy-ui", true, map[string]bool{"alice": true}),
		"new-ui":    flag("new-ui", true, nil),
	}
	e, err := evaluator.New(store, append([]evaluator.Rule{killSwitch}, evaluator.DefaultRules()...)...)
	if err != nil {
		t.Fatal(err)
	}

	killed, _ := e.Evaluate(context.Background(), "legacy-ui", "alice")
	if killed.Enabled || killed.Reason != "killed" || killed.DecidedBy != "kill_switch" {
		t.Fatalf("custom rule did not take precedence: %+v", killed)
	}
	normal, _ := e.Evaluate(context.Background(), "new-ui", "alice")
	if !normal.Enabled || normal.Reason != evaluator.ReasonGlobalOn {
		t.Fatalf("other flags must be unaffected: %+v", normal)
	}
}

func TestChainWithoutATerminalRuleFailsLoudly(t *testing.T) {
	e, err := evaluator.New(mapLookup{"f": flag("f", true, nil)}, evaluator.OverrideRule{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Evaluate(context.Background(), "f", "alice")
	if !errors.Is(err, evaluator.ErrNoDecision) {
		t.Fatalf("want ErrNoDecision, got %v", err)
	}
	for _, sentinel := range []error{domain.ErrInvalid, domain.ErrNotFound, domain.ErrConflict, domain.ErrAlreadyExists} {
		if errors.Is(err, sentinel) {
			t.Fatalf("a misconfiguration must not look like a client error (%v)", sentinel)
		}
	}
}

func TestRuleErrorStopsTheChainAndKeepsItsSentinel(t *testing.T) {
	boom := domain.Conflictf("rule says no")
	thirdRan := false
	e, err := evaluator.New(mapLookup{"f": flag("f", true, nil)},
		funcRule{name: "bad", fn: func(context.Context, evaluator.Input) (evaluator.Decision, bool, error) {
			return evaluator.Decision{}, false, boom
		}},
		funcRule{name: "never", fn: func(context.Context, evaluator.Input) (evaluator.Decision, bool, error) {
			thirdRan = true
			return evaluator.Decision{Enabled: true}, true, nil
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Evaluate(context.Background(), "f", "alice")
	if !errors.Is(err, domain.ErrConflict) || !strings.Contains(err.Error(), `"bad"`) {
		t.Fatalf("want a wrapped conflict naming the rule, got %v", err)
	}
	if thirdRan {
		t.Fatal("rules after a failing rule must not run")
	}
}

// requiresRule is a stand-in for the future prerequisite rule. It exists to
// prove that rules reaching other flags through Input.Flags are tracked.
type requiresRule struct{ required []string }

func (r requiresRule) Name() string { return "requires" }
func (r requiresRule) Apply(ctx context.Context, in evaluator.Input) (evaluator.Decision, bool, error) {
	for _, name := range r.required {
		dep, err := in.Flags.Get(ctx, name)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			return evaluator.Decision{Enabled: false, Reason: "prerequisite_failed"}, true, nil
		case err != nil:
			return evaluator.Decision{}, false, err
		case !dep.Enabled:
			return evaluator.Decision{Enabled: false, Reason: "prerequisite_failed"}, true, nil
		}
	}
	return evaluator.Decision{}, false, nil
}

func TestConsultedTracksEveryFlagARuleReads(t *testing.T) {
	store := mapLookup{
		"child":  flag("child", true, nil),
		"parent": flag("parent", true, nil),
		"off":    flag("off", false, nil),
	}
	chain := func(required ...string) *evaluator.Evaluator {
		e, err := evaluator.New(store, append([]evaluator.Rule{requiresRule{required: required}}, evaluator.DefaultRules()...)...)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}

	tests := []struct {
		name         string
		required     []string
		wantEnabled  bool
		wantConsults []string
	}{
		{"prerequisite met", []string{"parent"}, true, []string{"child", "parent"}},
		{"prerequisite disabled", []string{"off"}, false, []string{"child", "off"}},
		{"prerequisite missing is still tracked", []string{"ghost"}, false, []string{"child", "ghost"}},
		{"stops at the first failure", []string{"off", "parent"}, false, []string{"child", "off"}},
		{"repeated reads are deduplicated", []string{"parent", "parent"}, true, []string{"child", "parent"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := chain(tc.required...).Evaluate(context.Background(), "child", "alice")
			if err != nil {
				t.Fatal(err)
			}
			if got.Enabled != tc.wantEnabled {
				t.Fatalf("enabled = %v, want %v (%+v)", got.Enabled, tc.wantEnabled, got)
			}
			if !reflect.DeepEqual(got.Consulted, tc.wantConsults) {
				t.Fatalf("Consulted = %v, want %v", got.Consulted, tc.wantConsults)
			}
		})
	}
}

func TestResultsDoNotShareTheConsultedSlice(t *testing.T) {
	e := newDefault(t, flag("f", true, nil))
	a, _ := e.Evaluate(context.Background(), "f", "alice")
	b, _ := e.Evaluate(context.Background(), "f", "bob")
	a.Consulted[0] = "tampered"
	if b.Consulted[0] != "f" {
		t.Fatal("results share a backing array; a cached result could be corrupted by a caller")
	}
}

// ---- Liskov: every rule honours the same contract --------------------------

func TestEveryDefaultRuleHonoursTheRuleContract(t *testing.T) {
	flags := []domain.Flag{
		flag("on", true, nil),
		flag("off", false, nil),
		flag("mixed", true, map[string]bool{"alice": false, "bob": true}),
	}
	store := mapLookup{}
	for _, f := range flags {
		store[f.Name] = f
	}

	for _, rule := range evaluator.DefaultRules() {
		t.Run(rule.Name(), func(t *testing.T) {
			if rule.Name() == "" {
				t.Fatal("a rule must have a name")
			}
			for _, f := range flags {
				for _, user := range []string{"alice", "bob", "carol"} {
					before := f.Clone()
					in := evaluator.Input{Flag: f, UserID: user, Flags: store}

					d1, decided1, err1 := rule.Apply(context.Background(), in)
					d2, decided2, err2 := rule.Apply(context.Background(), in)

					if err1 != nil || err2 != nil {
						t.Fatalf("default rules never fail: %v %v", err1, err2)
					}
					if d1 != d2 || decided1 != decided2 {
						t.Fatal("a rule must be deterministic for the same input")
					}
					if !decided1 && d1 != (evaluator.Decision{}) {
						t.Fatalf("an undecided rule must return a zero Decision, got %+v", d1)
					}
					if decided1 && d1.Reason == "" {
						t.Fatal("a deciding rule must give a reason")
					}
					if !reflect.DeepEqual(f, before) {
						t.Fatal("a rule modified the flag it was given")
					}
				}
			}
		})
	}
}

// ---- concurrency -----------------------------------------------------------

// The evaluator is stateless and the per-call recorder is never shared, so many
// goroutines can use one instance. Run with -race.
func TestConcurrentEvaluationsAreIndependent(t *testing.T) {
	store := mapLookup{
		"child":  flag("child", true, map[string]bool{"vip": false}),
		"parent": flag("parent", true, nil),
		"off":    flag("off", false, nil),
	}
	e, err := evaluator.New(store, append([]evaluator.Rule{requiresRule{required: []string{"parent"}}}, evaluator.DefaultRules()...)...)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				user := fmt.Sprintf("user-%d", i%7)
				name, wantEnabled := "child", true
				if i%3 == 0 {
					name, wantEnabled = "off", false
				}
				got, err := e.Evaluate(context.Background(), name, user)
				if err != nil {
					t.Errorf("Evaluate: %v", err)
					return
				}
				want := []string{name}
				if name == "child" {
					want = []string{"child", "parent"}
				}
				if name == "off" {
					want = []string{"off", "parent"}
				}
				if got.Enabled != wantEnabled || !reflect.DeepEqual(got.Consulted, want) {
					t.Errorf("goroutine %d: %s for %s = %+v, want enabled=%v consulted=%v", g, name, user, got, wantEnabled, want)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}
