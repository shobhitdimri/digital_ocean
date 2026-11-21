package domain

import (
	"time"
	"unicode"
	"unicode/utf8"
)

// Validation limits.
const (
	MaxNameLength        = 64
	MaxDescriptionLength = 256
	MaxUserIDLength      = 128
	MaxOverridesPerFlag  = 10000
)

// Flag is a feature flag with a global state and optional per-user overrides.
type Flag struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Enabled     bool            `json:"enabled"`
	Overrides   map[string]bool `json:"overrides,omitempty"` // user id -> enabled
	Version     int64           `json:"version"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// NewFlag builds a validated flag at version 1.
func NewFlag(name, description string, enabled bool, now time.Time) (Flag, error) {
	f := Flag{
		Name:        name,
		Description: description,
		Enabled:     enabled,
		Version:     1,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := f.Validate(); err != nil {
		return Flag{}, err
	}
	return f, nil
}

// Validate checks every field of the flag.
func (f Flag) Validate() error {
	if err := ValidateName(f.Name); err != nil {
		return err
	}
	if n := utf8.RuneCountInString(f.Description); n > MaxDescriptionLength {
		return Invalidf("description is %d characters, maximum is %d", n, MaxDescriptionLength)
	}
	if len(f.Overrides) > MaxOverridesPerFlag {
		return Invalidf("flag has %d overrides, maximum is %d", len(f.Overrides), MaxOverridesPerFlag)
	}
	for userID := range f.Overrides {
		if err := ValidateUserID(userID); err != nil {
			return err
		}
	}
	return nil
}

// OverrideFor reports the user's override and whether one exists.
func (f Flag) OverrideFor(userID string) (enabled, ok bool) {
	enabled, ok = f.Overrides[userID]
	return enabled, ok
}

// SetOverride sets a per-user override, replacing any existing one.
func (f *Flag) SetOverride(userID string, enabled bool) error {
	if err := ValidateUserID(userID); err != nil {
		return err
	}
	if _, exists := f.Overrides[userID]; !exists && len(f.Overrides) >= MaxOverridesPerFlag {
		return Invalidf("flag already has the maximum of %d overrides", MaxOverridesPerFlag)
	}
	if f.Overrides == nil {
		f.Overrides = make(map[string]bool)
	}
	f.Overrides[userID] = enabled
	return nil
}

// RemoveOverride deletes a user's override and reports whether it existed.
func (f *Flag) RemoveOverride(userID string) bool {
	if _, ok := f.Overrides[userID]; !ok {
		return false
	}
	delete(f.Overrides, userID)
	if len(f.Overrides) == 0 {
		f.Overrides = nil
	}
	return true
}

// Clone returns a deep copy. Flag contains a map, so a plain assignment would
// share it; DAOs, caches and audit snapshots must hold independent copies.
func (f Flag) Clone() Flag {
	if f.Overrides != nil {
		overrides := make(map[string]bool, len(f.Overrides))
		for k, v := range f.Overrides {
			overrides[k] = v
		}
		f.Overrides = overrides
	}
	return f
}

// ValidateName checks a flag name: 1 to 64 ASCII letters, digits, '_', '.' or
// '-', starting with a letter or digit. Spaces are rejected.
func ValidateName(name string) error {
	if name == "" {
		return Invalidf("flag name must not be empty")
	}
	if len(name) > MaxNameLength {
		return Invalidf("flag name is %d characters, maximum is %d", len(name), MaxNameLength)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if i == 0 && !alnum {
			return Invalidf("flag name must start with a letter or digit")
		}
		if !alnum && c != '_' && c != '.' && c != '-' {
			return Invalidf("flag name may only contain letters, digits, '_', '.' and '-' (no spaces)")
		}
	}
	return nil
}

// ValidateUserID checks a user identifier: 1 to 128 characters of valid UTF-8
// with no whitespace or control characters.
func ValidateUserID(userID string) error {
	return validateIdentifier("user id", userID)
}

func validateIdentifier(label, value string) error {
	if value == "" {
		return Invalidf("%s must not be empty", label)
	}
	if !utf8.ValidString(value) {
		return Invalidf("%s must be valid UTF-8", label)
	}
	if n := utf8.RuneCountInString(value); n > MaxUserIDLength {
		return Invalidf("%s is %d characters, maximum is %d", label, n, MaxUserIDLength)
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return Invalidf("%s must not contain whitespace or control characters", label)
		}
	}
	return nil
}
