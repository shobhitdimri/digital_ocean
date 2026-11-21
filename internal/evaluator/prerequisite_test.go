package evaluator_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"feature-flag-service/internal/domain"
	"feature-flag-service/internal/evaluator"
)

func requiring(name string, enabled bool, requires ...string) domain.Flag {
	f := flag(name, enabled, nil)
	f.Requires = requires
	return f
}

func evalIn(t *testing.T, flags mapLookup, name, user string) evaluator.Result {
	t.Helper()
	e, err := evaluator.NewDefault(flags)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Evaluate(context.Background(), name, user)
	if err != nil {
		t.Fatalf("Evaluate(%s, %s): %v", name, user, err)
	}
	return res
}

func TestDependentIsOffWhenAPrerequisiteIsOff(t *testing.T) {
	flags := mapLookup{
		"payments": flag("payments", false, nil),
		"checkout": requiring("checkout", true, "payments"),
	}
	res := evalIn(t, flags, "checkout", "u1")
	if res.Enabled || res.Reason != evaluator.ReasonPrerequisiteUnmet || res.DecidedBy != "prerequisite" {
		t.Fatalf("got %+v, want off by prerequisite_unmet", res)
	}
}

func TestDependentFallsThroughToItsOwnRulesWhenPrerequisitesAreOn(t *testing.T) {
	flags := mapLookup{
		"payments": flag("payments", true, nil),
		"checkout": requiring("checkout", true, "payments"),
		"disabled": requiring("disabled", false, "payments"),
	}
	if res := evalIn(t, flags, "checkout", "u1"); !res.Enabled || res.Reason != evaluator.ReasonGlobalOn {
		t.Fatalf("got %+v, want on via global_on", res)
	}
	if res := evalIn(t, flags, "disabled", "u1"); res.Enabled || res.Reason != evaluator.ReasonGlobalOff {
		t.Fatalf("a met prerequisite must not turn on a disabled flag, got %+v", res)
	}
}

// A dependency is a safety property, so a per-user override cannot bypass it.
func TestOverrideCannotBypassAnUnmetPrerequisite(t *testing.T) {
	dependent := requiring("checkout", true, "payments")
	dependent.Overrides = map[string]bool{"vip": true}
	flags := mapLookup{"payments": flag("payments", false, nil), "checkout": dependent}

	res := evalIn(t, flags, "checkout", "vip")
	if res.Enabled || res.Reason != evaluator.ReasonPrerequisiteUnmet {
		t.Fatalf("got %+v, want the prerequisite to win over the override", res)
	}

	// With the prerequisite on, the override works again.
	flags["payments"] = flag("payments", true, nil)
	if res := evalIn(t, flags, "checkout", "vip"); !res.Enabled || res.Reason != evaluator.ReasonUserOverride {
		t.Fatalf("got %+v, want the override to apply once the prerequisite is met", res)
	}
}

// The prerequisite is judged for the same user, through its own full rule
// chain, so its rollout and overrides count.
func TestPrerequisiteIsEvaluatedPerUserThroughItsOwnRules(t *testing.T) {
	payments := rolloutFlag("payments", true, 50, map[string]bool{"blocked": false, "forced": true})
	flags := mapLookup{"payments": payments, "checkout": requiring("checkout", true, "payments")}

	// Known buckets for flag "payments" are not pinned, so derive who is in.
	inside, outside := "", ""
	for _, u := range users("pre", 200) {
		if evaluator.Bucket("payments", u) < 5000 && inside == "" {
			inside = u
		}
		if evaluator.Bucket("payments", u) >= 5000 && outside == "" {
			outside = u
		}
	}
	if res := evalIn(t, flags, "checkout", inside); !res.Enabled {
		t.Fatalf("user inside the prerequisite's rollout got %+v", res)
	}
	if res := evalIn(t, flags, "checkout", outside); res.Enabled || res.Reason != evaluator.ReasonPrerequisiteUnmet {
		t.Fatalf("user outside the prerequisite's rollout got %+v", res)
	}
	if res := evalIn(t, flags, "checkout", "blocked"); res.Enabled {
		t.Fatalf("a user whose override turns the prerequisite off got %+v", res)
	}
	if res := evalIn(t, flags, "checkout", "forced"); !res.Enabled {
		t.Fatalf("a user whose override turns the prerequisite on got %+v", res)
	}
}

func TestPrerequisitesChainAndAllMustHold(t *testing.T) {
	flags := mapLookup{
		"a": flag("a", true, nil),
		"b": requiring("b", true, "a"),
		"c": requiring("c", true, "b"),
		"x": flag("x", true, nil),
		"y": flag("y", false, nil),
		"z": requiring("z", true, "x", "y"),
	}
	if res := evalIn(t, flags, "c", "u"); !res.Enabled {
		t.Fatalf("a -> b -> c all on, got %+v", res)
	}
	flags["a"] = flag("a", false, nil)
	if res := evalIn(t, flags, "c", "u"); res.Enabled || res.Reason != evaluator.ReasonPrerequisiteUnmet {
		t.Fatalf("turning the root off must turn the whole chain off, got %+v", res)
	}
	if res := evalIn(t, flags, "z", "u"); res.Enabled {
		t.Fatalf("one unmet prerequisite out of two must be enough, got %+v", res)
	}
}

func TestMissingPrerequisiteCountsAsUnmet(t *testing.T) {
	flags := mapLookup{"checkout": requiring("checkout", true, "gone")}
	res := evalIn(t, flags, "checkout", "u")
	if res.Enabled || res.Reason != evaluator.ReasonPrerequisiteUnmet {
		t.Fatalf("got %+v", res)
	}
	// It stays tracked, so creating the flag later invalidates this answer.
	if len(res.Consulted) != 2 || res.Consulted[1] != "gone" {
		t.Fatalf("Consulted = %v, want the missing flag recorded", res.Consulted)
	}
}

// Every flag read must be a cache tag, or changing a prerequisite would leave
// stale answers for its dependents.
func TestConsultedListsEveryPrerequisiteIncludingNested(t *testing.T) {
	flags := mapLookup{
		"a": flag("a", true, nil),
		"b": requiring("b", true, "a"),
		"c": requiring("c", true, "b"),
	}
	res := evalIn(t, flags, "c", "u")
	want := map[string]bool{"a": true, "b": true, "c": true}
	if len(res.Consulted) != 3 || res.Consulted[0] != "c" {
		t.Fatalf("Consulted = %v", res.Consulted)
	}
	for _, n := range res.Consulted {
		delete(want, n)
	}
	if len(want) != 0 {
		t.Fatalf("Consulted = %v is missing %v", res.Consulted, want)
	}
}

type countingLookup struct {
	mapLookup
	gets atomic.Int64
}

func (c *countingLookup) Get(ctx context.Context, name string) (domain.Flag, error) {
	c.gets.Add(1)
	return c.mapLookup.Get(ctx, name)
}

// A shared prerequisite (a diamond) is read and evaluated once per evaluation.
func TestSharedPrerequisiteIsEvaluatedOnce(t *testing.T) {
	lookup := &countingLookup{mapLookup: mapLookup{
		"base":  flag("base", true, nil),
		"left":  requiring("left", true, "base"),
		"right": requiring("right", true, "base"),
		"top":   requiring("top", true, "left", "right"),
	}}
	e, _ := evaluator.NewDefault(lookup)
	res, err := e.Evaluate(context.Background(), "top", "u")
	if err != nil || !res.Enabled {
		t.Fatalf("got %+v, %v", res, err)
	}
	if got := lookup.gets.Load(); got != 4 {
		t.Fatalf("%d flag reads for 4 distinct flags: the shared prerequisite was evaluated more than once", got)
	}
}

// The service never stores a cycle, but hand-edited data must not hang the
// evaluator or overflow the stack.
func TestCycleInStoredDataIsAnErrorNotAHang(t *testing.T) {
	flags := mapLookup{
		"a": requiring("a", true, "b"),
		"b": requiring("b", true, "c"),
		"c": requiring("c", true, "a"),
	}
	e, _ := evaluator.NewDefault(flags)
	_, err := e.Evaluate(context.Background(), "a", "u")
	if !errors.Is(err, evaluator.ErrDependencyCycle) {
		t.Fatalf("err = %v, want ErrDependencyCycle", err)
	}
	if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrInvalid) {
		t.Fatal("a data fault must not look like bad input")
	}
}

func TestVeryDeepChainsAreRefused(t *testing.T) {
	flags := mapLookup{}
	prev := ""
	for i := 0; i < evaluator.MaxDependencyDepth+5; i++ {
		name := "f" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		if prev == "" {
			flags[name] = flag(name, true, nil)
		} else {
			flags[name] = requiring(name, true, prev)
		}
		prev = name
	}
	e, _ := evaluator.NewDefault(flags)
	if _, err := e.Evaluate(context.Background(), prev, "u"); !errors.Is(err, evaluator.ErrDependencyCycle) {
		t.Fatalf("err = %v, want ErrDependencyCycle for a chain deeper than %d", err, evaluator.MaxDependencyDepth)
	}
}

func TestPrerequisiteLookupFailureIsReturned(t *testing.T) {
	boom := errors.New("disk on fire")
	e, _ := evaluator.NewDefault(failingAfterFirst{first: requiring("a", true, "b"), err: boom})
	if _, err := e.Evaluate(context.Background(), "a", "u"); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the storage error, not a silent 'unmet'", err)
	}
}

type failingAfterFirst struct {
	first domain.Flag
	err   error
}

func (f failingAfterFirst) Get(_ context.Context, name string) (domain.Flag, error) {
	if name == f.first.Name {
		return f.first, nil
	}
	return domain.Flag{}, f.err
}

func TestRuleWithoutEvaluateFunctionFailsLoudly(t *testing.T) {
	_, _, err := evaluator.PrerequisiteRule{}.Apply(context.Background(), evaluator.Input{Flag: requiring("a", true, "b"), UserID: "u"})
	if err == nil {
		t.Fatal("want an error, not a nil dereference or a silent pass")
	}
	if _, decided, err := (evaluator.PrerequisiteRule{}).Apply(context.Background(), evaluator.Input{Flag: flag("a", true, nil), UserID: "u"}); err != nil || decided {
		t.Fatalf("a flag with no prerequisites must pass without needing Evaluate: %v %v", decided, err)
	}
}
