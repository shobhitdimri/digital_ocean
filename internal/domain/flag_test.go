package domain

import (
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
