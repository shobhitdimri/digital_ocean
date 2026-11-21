package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestValidateName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"simple", "new-checkout", false},
		{"letters digits", "Feature123", false},
		{"dots underscores dashes", "team.a_new-ui", false},
		{"single char", "a", false},
		{"starts with digit", "1st-flag", false},
		{"exactly max length", strings.Repeat("a", MaxNameLength), false},

		{"empty", "", true},
		{"one over max length", strings.Repeat("a", MaxNameLength+1), true},
		{"contains space", "new checkout", true},
		{"leading space", " flag", true},
		{"trailing space", "flag ", true},
		{"trailing newline", "flag\n", true},
		{"tab", "fl\tag", true},
		{"starts with dash", "-flag", true},
		{"starts with dot", ".flag", true},
		{"starts with underscore", "_flag", true},
		{"slash", "a/b", true},
		{"question mark", "flag?", true},
		{"percent", "flag%20", true},
		{"unicode letter", "flagé", true},
		{"emoji", "flag🚀", true},
		{"path traversal", "../etc", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateName(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateName(%q) error = %v, wantErr %v", tc.input, err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalid) {
				t.Fatalf("error should wrap ErrInvalid, got %v", err)
			}
		})
	}
}

func TestValidateUserID(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"simple", "alice", false},
		{"email", "alice@example.com", false},
		{"uuid", "3f2b8c1e-9d4a-4b7e-8a11-0c5d2e6f7a90", false},
		{"unicode", "ユーザー1", false},
		{"max length", strings.Repeat("u", MaxUserIDLength), false},

		{"empty", "", true},
		{"too long", strings.Repeat("u", MaxUserIDLength+1), true},
		{"space", "alice smith", true},
		{"newline", "alice\n", true},
		{"control char", "ali\x00ce", true},
		{"invalid utf8", "ali\xffce", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateUserID(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateUserID(%q) error = %v, wantErr %v", tc.input, err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalid) {
				t.Fatalf("error should wrap ErrInvalid, got %v", err)
			}
		})
	}
}

func TestNewFlag(t *testing.T) {
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)

	f, err := NewFlag("new-ui", "Redesigned UI", true, now)
	if err != nil {
		t.Fatalf("NewFlag: %v", err)
	}
	if f.Version != 1 || !f.CreatedAt.Equal(now) || !f.UpdatedAt.Equal(now) || !f.Enabled || f.Overrides != nil {
		t.Fatalf("unexpected defaults: %+v", f)
	}

	if _, err := NewFlag("bad name", "", false, now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("want ErrInvalid for bad name, got %v", err)
	}
}

func TestFlagValidate(t *testing.T) {
	tooMany := make(map[string]bool, MaxOverridesPerFlag+1)
	for i := 0; i <= MaxOverridesPerFlag; i++ {
		tooMany[fmt.Sprintf("user-%d", i)] = true
	}

	tests := []struct {
		name    string
		flag    Flag
		wantErr bool
	}{
		{"minimal", Flag{Name: "f"}, false},
		{"with overrides", Flag{Name: "f", Overrides: map[string]bool{"alice": true, "bob": false}}, false},
		{"description at limit", Flag{Name: "f", Description: strings.Repeat("d", MaxDescriptionLength)}, false},
		{"multibyte description at limit", Flag{Name: "f", Description: strings.Repeat("é", MaxDescriptionLength)}, false},

		{"bad name", Flag{Name: "has space"}, true},
		{"description over limit", Flag{Name: "f", Description: strings.Repeat("d", MaxDescriptionLength+1)}, true},
		{"multibyte description over limit", Flag{Name: "f", Description: strings.Repeat("é", MaxDescriptionLength+1)}, true},
		{"override with bad user id", Flag{Name: "f", Overrides: map[string]bool{"bad user": true}}, true},
		{"override with empty user id", Flag{Name: "f", Overrides: map[string]bool{"": true}}, true},
		{"too many overrides", Flag{Name: "f", Overrides: tooMany}, true},

		{"rollout 0", Flag{Name: "f", RolloutPercent: ptr(0)}, false},
		{"rollout 50", Flag{Name: "f", RolloutPercent: ptr(50)}, false},
		{"rollout 100", Flag{Name: "f", RolloutPercent: ptr(100)}, false},
		{"rollout negative", Flag{Name: "f", RolloutPercent: ptr(-1)}, true},
		{"rollout over 100", Flag{Name: "f", RolloutPercent: ptr(101)}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.flag.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalid) {
				t.Fatalf("error should wrap ErrInvalid, got %v", err)
			}
		})
	}
}

func TestOverrides(t *testing.T) {
	f := Flag{Name: "f"}

	if _, ok := f.OverrideFor("alice"); ok {
		t.Fatal("no override expected on a fresh flag")
	}

	if err := f.SetOverride("alice", true); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	if err := f.SetOverride("bob", false); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	if v, ok := f.OverrideFor("alice"); !ok || !v {
		t.Fatalf("alice override = %v, %v; want true, true", v, ok)
	}
	if v, ok := f.OverrideFor("bob"); !ok || v {
		t.Fatalf("bob override = %v, %v; want false, true", v, ok)
	}

	if err := f.SetOverride("alice", false); err != nil {
		t.Fatalf("replacing override: %v", err)
	}
	if v, _ := f.OverrideFor("alice"); v {
		t.Fatal("override should have been replaced with false")
	}

	if err := f.SetOverride("not valid", true); !errors.Is(err, ErrInvalid) {
		t.Fatalf("want ErrInvalid for bad user id, got %v", err)
	}

	if !f.RemoveOverride("alice") {
		t.Fatal("RemoveOverride should report an existing override")
	}
	if f.RemoveOverride("alice") {
		t.Fatal("RemoveOverride should report false for a missing override")
	}
	f.RemoveOverride("bob")
	if f.Overrides != nil {
		t.Fatalf("overrides map should be nil once empty, got %v", f.Overrides)
	}
}

func TestSetOverrideEnforcesLimit(t *testing.T) {
	f := Flag{Name: "f", Overrides: make(map[string]bool, MaxOverridesPerFlag)}
	for i := 0; i < MaxOverridesPerFlag; i++ {
		f.Overrides[fmt.Sprintf("user-%d", i)] = true
	}

	if err := f.SetOverride("one-too-many", true); !errors.Is(err, ErrInvalid) {
		t.Fatalf("want ErrInvalid at the limit, got %v", err)
	}
	if err := f.SetOverride("user-0", false); err != nil {
		t.Fatalf("updating an existing override at the limit must work: %v", err)
	}
}

func TestCloneIsDeep(t *testing.T) {
	orig := Flag{Name: "f", Overrides: map[string]bool{"alice": true}}
	clone := orig.Clone()

	clone.Overrides["alice"] = false
	clone.Overrides["bob"] = true

	if v := orig.Overrides["alice"]; !v {
		t.Fatal("mutating the clone changed the original")
	}
	if _, ok := orig.Overrides["bob"]; ok {
		t.Fatal("adding to the clone's map leaked into the original")
	}

	if empty := (Flag{Name: "f"}).Clone(); empty.Overrides != nil {
		t.Fatal("cloning a nil map should stay nil")
	}
}

func TestErrorHelpersWrapSentinels(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		sentinel error
	}{
		{"invalid", Invalidf("bad %s", "x"), ErrInvalid},
		{"not found", NotFoundf("flag %q", "x"), ErrNotFound},
		{"already exists", AlreadyExistsf("flag %q", "x"), ErrAlreadyExists},
		{"conflict", Conflictf("required by %q", "y"), ErrConflict},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !errors.Is(tc.err, tc.sentinel) {
				t.Fatalf("%v should wrap %v", tc.err, tc.sentinel)
			}
			for _, other := range []error{ErrInvalid, ErrNotFound, ErrAlreadyExists, ErrConflict} {
				if other != tc.sentinel && errors.Is(tc.err, other) {
					t.Fatalf("%v should not match %v", tc.err, other)
				}
			}
			if !strings.Contains(tc.err.Error(), ":") {
				t.Fatalf("message should carry detail, got %q", tc.err.Error())
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestRolloutDefaultsToEveryone(t *testing.T) {
	if got := (Flag{Name: "f"}).Rollout(); got != FullRollout {
		t.Fatalf("a flag with no rollout set reports %d, want %d", got, FullRollout)
	}
	f, err := NewFlag("f", "", true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if f.RolloutPercent != nil || f.Rollout() != 100 {
		t.Fatalf("NewFlag without options must not restrict the flag: %+v", f)
	}
}

func TestNewFlagWithRollout(t *testing.T) {
	now := time.Now()
	tests := []struct {
		percent int
		wantErr bool
		want    int
	}{
		{0, false, 0}, {1, false, 1}, {50, false, 50}, {99, false, 99}, {100, false, 100},
		{-1, true, 0}, {101, true, 0}, {1000, true, 0},
	}
	for _, tt := range tests {
		f, err := NewFlag("f", "", true, now, WithRollout(tt.percent))
		if (err != nil) != tt.wantErr {
			t.Fatalf("WithRollout(%d): err = %v, wantErr %v", tt.percent, err, tt.wantErr)
		}
		if err != nil {
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("WithRollout(%d) error must wrap ErrInvalid, got %v", tt.percent, err)
			}
			continue
		}
		if f.Rollout() != tt.want {
			t.Fatalf("WithRollout(%d) gave %d", tt.percent, f.Rollout())
		}
	}
}

func TestNewFlagStillValidatesNameWhenOptionsAreGiven(t *testing.T) {
	if _, err := NewFlag("bad name", "", true, time.Now(), WithRollout(10)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("got %v, want ErrInvalid", err)
	}
}

func TestSetRolloutStoresFullRolloutAsUnset(t *testing.T) {
	f := Flag{Name: "f"}
	if err := f.SetRollout(30); err != nil || f.Rollout() != 30 {
		t.Fatalf("SetRollout(30): %v, rollout %d", err, f.Rollout())
	}
	if err := f.SetRollout(100); err != nil {
		t.Fatal(err)
	}
	if f.RolloutPercent != nil {
		t.Fatalf("100 must be stored as unset so there is one representation of 'everyone', got %d", *f.RolloutPercent)
	}
	if err := f.SetRollout(0); err != nil || f.RolloutPercent == nil || *f.RolloutPercent != 0 {
		t.Fatalf("0 is a real restriction and must be stored: %v %v", err, f.RolloutPercent)
	}
}

func TestSetRolloutRejectsOutOfRangeAndKeepsTheOldValue(t *testing.T) {
	f := Flag{Name: "f"}
	_ = f.SetRollout(40)
	for _, bad := range []int{-1, 101, 5000} {
		if err := f.SetRollout(bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("SetRollout(%d) = %v, want ErrInvalid", bad, err)
		}
		if f.Rollout() != 40 {
			t.Fatalf("a rejected SetRollout(%d) changed the rollout to %d", bad, f.Rollout())
		}
	}
}

func TestCloneDeepCopiesTheRollout(t *testing.T) {
	orig := Flag{Name: "f"}
	_ = orig.SetRollout(25)
	clone := orig.Clone()

	*clone.RolloutPercent = 90
	if orig.Rollout() != 25 {
		t.Fatalf("writing through the clone's pointer changed the original to %d", orig.Rollout())
	}
	_ = clone.SetRollout(10)
	if orig.Rollout() != 25 {
		t.Fatalf("SetRollout on the clone changed the original to %d", orig.Rollout())
	}
	if unset := (Flag{Name: "f"}).Clone(); unset.RolloutPercent != nil {
		t.Fatal("cloning an unset rollout must stay unset")
	}
}

func TestRolloutJSONRepresentation(t *testing.T) {
	marshal := func(f Flag) string {
		b, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	unset := Flag{Name: "f"}
	if got := marshal(unset); strings.Contains(got, "rollout_percent") {
		t.Fatalf("an unrestricted flag must not write the field, so old readers see an unchanged file: %s", got)
	}
	zero := Flag{Name: "f"}
	_ = zero.SetRollout(0)
	if got := marshal(zero); !strings.Contains(got, `"rollout_percent":0`) {
		t.Fatalf("0%% must survive serialization, omitempty must not swallow it: %s", got)
	}

	for _, pct := range []int{0, 1, 50, 99} {
		f := Flag{Name: "f"}
		_ = f.SetRollout(pct)
		var back Flag
		if err := json.Unmarshal([]byte(marshal(f)), &back); err != nil {
			t.Fatal(err)
		}
		if back.Rollout() != pct {
			t.Fatalf("%d%% came back as %d%%", pct, back.Rollout())
		}
	}

	var old Flag
	if err := json.Unmarshal([]byte(`{"name":"legacy","enabled":true}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Rollout() != 100 {
		t.Fatalf("a record written before rollouts existed must read as 100%%, got %d", old.Rollout())
	}
}
