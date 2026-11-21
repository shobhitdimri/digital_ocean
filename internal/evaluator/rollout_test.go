package evaluator_test

import (
	"context"
	"fmt"
	"math"
	"testing"

	"feature-flag-service/internal/domain"
	"feature-flag-service/internal/evaluator"
)

func rolloutFlag(name string, enabled bool, percent int, overrides map[string]bool) domain.Flag {
	f := flag(name, enabled, overrides)
	if err := f.SetRollout(percent); err != nil {
		panic(err)
	}
	return f
}

func evalWith(t *testing.T, f domain.Flag, user string) evaluator.Result {
	t.Helper()
	e, err := evaluator.NewDefault(mapLookup{f.Name: f})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Evaluate(context.Background(), f.Name, user)
	if err != nil {
		t.Fatalf("Evaluate(%s, %s): %v", f.Name, user, err)
	}
	return res
}

func users(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s-%06d", prefix, i)
	}
	return out
}

// The bucket function is part of the service's contract: changing it moves
// users in and out of every running rollout. These values were computed with an
// independent implementation (Python's hashlib) so the test cannot merely agree
// with the code under test.
func TestBucketIsPinnedToKnownValues(t *testing.T) {
	tests := []struct {
		flag, user string
		want       int
	}{
		{"checkout", "alice", 410},
		{"checkout", "bob", 5030},
		{"checkout", "user-1", 593},
		{"new-search", "alice", 7613},
		{"a", "b", 2387},
		{"checkout", "ünïcode-ユーザー", 7231},
		{"boundary", "probe-5440", 0},
		{"boundary", "probe-6820", 4999},
		{"boundary", "probe-6970", 5000},
	}
	for _, tt := range tests {
		if got := evaluator.Bucket(tt.flag, tt.user); got != tt.want {
			t.Errorf("Bucket(%q, %q) = %d, want %d: the hash changed, which would reshuffle every rollout", tt.flag, tt.user, got, tt.want)
		}
	}
}

func TestBucketIsAlwaysInRangeAndDeterministic(t *testing.T) {
	for _, u := range users("range", 20000) {
		b := evaluator.Bucket("f", u)
		if b < 0 || b >= evaluator.BucketCount {
			t.Fatalf("Bucket = %d, outside [0, %d)", b, evaluator.BucketCount)
		}
		if again := evaluator.Bucket("f", u); again != b {
			t.Fatalf("Bucket(%q) changed between calls: %d then %d", u, b, again)
		}
	}
}

// Splitting the same characters differently between flag name and user id must
// not give the same bucket: the separator keeps the two fields apart. Without
// it, "flag1"+"1" and "flag"+"11" would be one string. Chance collisions are
// expected about once per 10000 pairs, so a handful in 2000 would already
// be a red flag.
func TestBucketSeparatesFlagNameFromUserID(t *testing.T) {
	collisions := 0
	for i := 0; i < 2000; i++ {
		a := evaluator.Bucket(fmt.Sprintf("flag%d", i), "1")
		b := evaluator.Bucket("flag", fmt.Sprintf("%d1", i))
		if a == b {
			collisions++
		}
	}
	if collisions > 3 {
		t.Fatalf("%d of 2000 flag/user splits of the same characters collided; the boundary is not part of the hash", collisions)
	}
	if evaluator.Bucket("ab", "c") == evaluator.Bucket("a", "bc") {
		t.Fatal("different splits of the same text hash identically")
	}
}

func TestRolloutEdgesZeroAndHundred(t *testing.T) {
	for _, u := range users("edge", 500) {
		zero := evalWith(t, rolloutFlag("f", true, 0, nil), u)
		if zero.Enabled || zero.Reason != evaluator.ReasonRolloutExcluded || zero.DecidedBy != "rollout" {
			t.Fatalf("0%% rollout for %s = %+v, want off, rollout_excluded", u, zero)
		}
		full := evalWith(t, rolloutFlag("f", true, 100, nil), u)
		if !full.Enabled || full.Reason != evaluator.ReasonGlobalOn || full.DecidedBy != "global_on" {
			t.Fatalf("100%% rollout for %s = %+v, want on, global_on (unchanged behaviour)", u, full)
		}
	}
}

// A flag stored before rollouts existed, or built as a literal, has no rollout
// set and must still mean "on for everyone".
func TestFlagWithoutARolloutIsOnForEveryone(t *testing.T) {
	f := flag("legacy", true, nil)
	if f.RolloutPercent != nil {
		t.Fatal("test premise: the flag must have no rollout set")
	}
	for _, u := range users("legacy", 500) {
		res := evalWith(t, f, u)
		if !res.Enabled || res.Reason != evaluator.ReasonGlobalOn {
			t.Fatalf("flag without a rollout evaluated to %+v for %s", res, u)
		}
	}
}

// Bucket b is inside a p% rollout exactly when b < p*100. Test both sides of the
// boundary with real users whose buckets are known.
func TestRolloutBoundaryIsExact(t *testing.T) {
	const flagName = "boundary"
	tests := []struct {
		user    string // bucket in the comment
		percent int
		want    bool
	}{
		{"probe-6820", 50, true},  // bucket 4999 < 5000
		{"probe-6970", 50, false}, // bucket 5000 is not < 5000
		{"probe-6970", 51, true},  // bucket 5000 < 5100
		{"probe-5440", 0, false},  // bucket 0 is never in 0%
		{"probe-5440", 1, true},   // bucket 0 is in any positive share
		{"probe-6820", 49, false}, // bucket 4999 is not < 4900
	}
	for _, tt := range tests {
		got := evalWith(t, rolloutFlag(flagName, true, tt.percent, nil), tt.user)
		if got.Enabled != tt.want {
			t.Errorf("%s at %d%%: enabled=%v (%s), want %v", tt.user, tt.percent, got.Enabled, got.Reason, tt.want)
		}
	}
}

func TestRolloutReasonsMatchTheOutcome(t *testing.T) {
	included, excluded := 0, 0
	for _, u := range users("reason", 400) {
		res := evalWith(t, rolloutFlag("f", true, 50, nil), u)
		switch {
		case res.Enabled && res.Reason == evaluator.ReasonRolloutIncluded:
			included++
		case !res.Enabled && res.Reason == evaluator.ReasonRolloutExcluded:
			excluded++
		default:
			t.Fatalf("inconsistent result %+v", res)
		}
	}
	if included == 0 || excluded == 0 {
		t.Fatalf("a 50%% rollout over 400 users gave %d included, %d excluded", included, excluded)
	}
}

// The same flag and user always get the same answer, however often and from
// however many goroutines they are asked.
func TestRolloutIsDeterministic(t *testing.T) {
	f := rolloutFlag("stable", true, 37, nil)
	first := map[string]bool{}
	for _, u := range users("det", 1000) {
		first[u] = evalWith(t, f, u).Enabled
	}
	for round := 0; round < 3; round++ {
		for u, want := range first {
			if got := evalWith(t, f, u).Enabled; got != want {
				t.Fatalf("user %s flipped from %v to %v on round %d", u, want, got, round)
			}
		}
	}
}

// Raising the percentage must only ever add users: nobody who had the feature
// loses it, and nobody is swapped out. This is the property that makes a staged
// rollout safe.
func TestRaisingThePercentageOnlyAddsUsers(t *testing.T) {
	all := users("mono", 3000)
	var prev map[string]bool
	for pct := 0; pct <= 100; pct++ {
		cur := map[string]bool{}
		for _, u := range all {
			cur[u] = evalWith(t, rolloutFlag("staged", true, pct, nil), u).Enabled
		}
		for u, was := range prev {
			if was && !cur[u] {
				t.Fatalf("user %s had the feature at %d%% but lost it at %d%%", u, pct-1, pct)
			}
		}
		prev = cur
	}
	for u, on := range prev {
		if !on {
			t.Fatalf("user %s is off at 100%%", u)
		}
	}
}

// Over many users the included share must be close to the requested one.
func TestRolloutDistributionMatchesThePercentage(t *testing.T) {
	const n = 20000
	all := users("dist", n)
	for _, pct := range []int{1, 5, 10, 25, 33, 50, 75, 90, 99} {
		f := rolloutFlag("distribution", true, pct, nil)
		in := 0
		for _, u := range all {
			if evalWith(t, f, u).Enabled {
				in++
			}
		}
		p := float64(pct) / 100
		sd := math.Sqrt(float64(n) * p * (1 - p))
		if dev := math.Abs(float64(in) - p*n); dev > 5*sd {
			t.Errorf("%d%% rollout included %d of %d users (expected %.0f, off by %.1f standard deviations)", pct, in, n, p*n, dev/sd)
		}
	}
}

// Sequential, zero-padded and numeric ids are what real systems have. They must
// spread as evenly as random ones (FNV-1a would not pass this for padded ids).
func TestBucketsSpreadEvenlyForRealisticIdPatterns(t *testing.T) {
	patterns := map[string]func(int) string{
		"user-N":      func(i int) string { return fmt.Sprintf("user-%d", i) },
		"u000000":     func(i int) string { return fmt.Sprintf("u%06d", i) },
		"bare number": func(i int) string { return fmt.Sprintf("%d", i) },
		"email":       func(i int) string { return fmt.Sprintf("person%d@example.com", i) },
	}
	const n, bins = 100000, 100
	for name, gen := range patterns {
		var counts [bins]float64
		for i := 0; i < n; i++ {
			counts[evaluator.Bucket("checkout", gen(i))/(evaluator.BucketCount/bins)]++
		}
		expected := float64(n) / bins
		chi2 := 0.0
		for _, c := range counts {
			chi2 += (c - expected) * (c - expected) / expected
		}
		// 99 degrees of freedom: mean 99, standard deviation about 14. 160 is
		// more than 4 sigma, so a good hash essentially never trips it. FNV-1a
		// measured 218 on the zero-padded pattern.
		if chi2 > 160 {
			t.Errorf("%s: chi-square %.1f over %d buckets, the spread is visibly uneven", name, chi2, bins)
		}
	}
}

// Each flag gets its own cohort, so being in one experiment says nothing about
// being in another.
func TestFlagsHaveIndependentCohorts(t *testing.T) {
	const n = 40000
	for _, pct := range []int{10, 50} {
		a := rolloutFlag("experiment-a", true, pct, nil)
		b := rolloutFlag("experiment-b", true, pct, nil)
		inA, both := 0, 0
		for _, u := range users("indep", n) {
			ea, eb := evalWith(t, a, u).Enabled, evalWith(t, b, u).Enabled
			if ea {
				inA++
				if eb {
					both++
				}
			}
		}
		got, want := float64(both)/float64(inA), float64(pct)/100
		if math.Abs(got-want) > 0.02 {
			t.Errorf("at %d%%, %.3f of flag A's cohort is also in flag B's; independent cohorts would give %.2f", pct, got, want)
		}
	}
}

// Override and global state still win over the rollout, in every combination.
func TestRolloutRanksBelowOverridesAndGlobalOff(t *testing.T) {
	const flagName = "ranked"
	var inUser, outUser string
	for _, u := range users("rank", 200) {
		if evaluator.Bucket(flagName, u) < 5000 && inUser == "" {
			inUser = u
		}
		if evaluator.Bucket(flagName, u) >= 5000 && outUser == "" {
			outUser = u
		}
	}

	t.Run("override on beats exclusion", func(t *testing.T) {
		res := evalWith(t, rolloutFlag(flagName, true, 50, map[string]bool{outUser: true}), outUser)
		if !res.Enabled || res.Reason != evaluator.ReasonUserOverride {
			t.Fatalf("got %+v", res)
		}
	})
	t.Run("override off beats inclusion", func(t *testing.T) {
		res := evalWith(t, rolloutFlag(flagName, true, 50, map[string]bool{inUser: false}), inUser)
		if res.Enabled || res.Reason != evaluator.ReasonUserOverride {
			t.Fatalf("got %+v", res)
		}
	})
	t.Run("override on beats a 0 percent rollout", func(t *testing.T) {
		res := evalWith(t, rolloutFlag(flagName, true, 0, map[string]bool{inUser: true}), inUser)
		if !res.Enabled || res.Reason != evaluator.ReasonUserOverride {
			t.Fatalf("got %+v", res)
		}
	})
	t.Run("globally off beats inclusion", func(t *testing.T) {
		res := evalWith(t, rolloutFlag(flagName, false, 50, nil), inUser)
		if res.Enabled || res.Reason != evaluator.ReasonGlobalOff {
			t.Fatalf("a disabled flag must be off even for a user inside the rollout, got %+v", res)
		}
	})
	t.Run("globally off with a rollout still honours an override", func(t *testing.T) {
		res := evalWith(t, rolloutFlag(flagName, false, 50, map[string]bool{outUser: true}), outUser)
		if !res.Enabled || res.Reason != evaluator.ReasonUserOverride {
			t.Fatalf("got %+v", res)
		}
	})
}

func TestRolloutRuleOnlyConsultsTheFlagItself(t *testing.T) {
	res := evalWith(t, rolloutFlag("solo", true, 50, nil), "someone")
	if len(res.Consulted) != 1 || res.Consulted[0] != "solo" {
		t.Fatalf("Consulted = %v, want just the flag, so its tag alone invalidates the answer", res.Consulted)
	}
}

func TestRolloutRuleHonoursTheRuleContract(t *testing.T) {
	f := rolloutFlag("contract", true, 50, map[string]bool{"x": true})
	before := f.Clone()
	rule := evaluator.RolloutRule{}
	for _, u := range users("contract", 50) {
		if _, _, err := rule.Apply(context.Background(), evaluator.Input{Flag: f, UserID: u}); err != nil {
			t.Fatal(err)
		}
	}
	if f.Rollout() != before.Rollout() || len(f.Overrides) != len(before.Overrides) || f.Enabled != before.Enabled {
		t.Fatal("RolloutRule modified the flag it was given")
	}
	if rule.Name() != "rollout" {
		t.Fatalf("Name = %q", rule.Name())
	}
}
