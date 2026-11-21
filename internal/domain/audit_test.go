package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func validEntry() AuditEntry {
	return AuditEntry{
		ID:        "01HXYZ",
		FlagName:  "new-ui",
		Actor:     "alice",
		Action:    ActionFlagUpdated,
		Before:    json.RawMessage(`{"enabled":false}`),
		After:     json.RawMessage(`{"enabled":true}`),
		RequestID: "req-1",
		At:        time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC),
	}
}

func TestAuditEntryValidate(t *testing.T) {
	if err := validEntry().Validate(); err != nil {
		t.Fatalf("valid entry rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*AuditEntry)
	}{
		{"missing id", func(e *AuditEntry) { e.ID = "" }},
		{"bad flag name", func(e *AuditEntry) { e.FlagName = "bad name" }},
		{"missing actor", func(e *AuditEntry) { e.Actor = "" }},
		{"actor with spaces", func(e *AuditEntry) { e.Actor = "al ice" }},
		{"actor too long", func(e *AuditEntry) { e.Actor = strings.Repeat("a", MaxUserIDLength+1) }},
		{"unknown action", func(e *AuditEntry) { e.Action = "flag.exploded" }},
		{"empty action", func(e *AuditEntry) { e.Action = "" }},
		{"zero time", func(e *AuditEntry) { e.At = time.Time{} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := validEntry()
			tc.mutate(&e)
			err := e.Validate()
			if err == nil {
				t.Fatal("want validation error, got nil")
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("error should wrap ErrInvalid, got %v", err)
			}
		})
	}
}

func TestAllActionsAreValid(t *testing.T) {
	for _, a := range []Action{ActionFlagCreated, ActionFlagUpdated, ActionFlagDeleted, ActionOverrideSet, ActionOverrideRemove} {
		e := validEntry()
		e.Action = a
		if err := e.Validate(); err != nil {
			t.Errorf("action %q rejected: %v", a, err)
		}
	}
}

func TestSnapshot(t *testing.T) {
	t.Run("nil flag is JSON null", func(t *testing.T) {
		got, err := Snapshot(nil)
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if string(got) != "null" {
			t.Fatalf("Snapshot(nil) = %s, want null", got)
		}
	})

	t.Run("flag round trips", func(t *testing.T) {
		f := Flag{
			Name:        "new-ui",
			Description: "d",
			Enabled:     true,
			Overrides:   map[string]bool{"alice": false},
			Version:     3,
			CreatedAt:   time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC),
			UpdatedAt:   time.Date(2026, 11, 21, 11, 0, 0, 0, time.UTC),
		}
		raw, err := Snapshot(&f)
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		var back Flag
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatalf("snapshot is not valid JSON: %v", err)
		}
		if back.Name != f.Name || back.Version != 3 || !back.Enabled || back.Overrides["alice"] || !back.UpdatedAt.Equal(f.UpdatedAt) {
			t.Fatalf("round trip mismatch: %+v", back)
		}
	})
}

func TestAuditEntryJSONRoundTrip(t *testing.T) {
	created := AuditEntry{
		ID:       "1",
		FlagName: "new-ui",
		Actor:    "alice",
		Action:   ActionFlagCreated,
		At:       time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC),
	}
	created.Before, _ = Snapshot(nil)
	f := Flag{Name: "new-ui", Version: 1}
	created.After, _ = Snapshot(&f)

	b, err := json.Marshal(created)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back AuditEntry
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(back.Before) != "null" {
		t.Fatalf("before should stay null, got %s", back.Before)
	}
	if back.Action != ActionFlagCreated || back.Actor != "alice" || !back.At.Equal(created.At) {
		t.Fatalf("round trip mismatch: %+v", back)
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("round-tripped entry should validate: %v", err)
	}
}
