package domain

import (
	"encoding/json"
	"time"
)

// Action identifies the kind of change recorded by an audit entry.
type Action string

const (
	ActionFlagCreated    Action = "flag.created"
	ActionFlagUpdated    Action = "flag.updated"
	ActionFlagDeleted    Action = "flag.deleted"
	ActionOverrideSet    Action = "override.set"
	ActionOverrideRemove Action = "override.removed"
)

func (a Action) valid() bool {
	switch a {
	case ActionFlagCreated, ActionFlagUpdated, ActionFlagDeleted, ActionOverrideSet, ActionOverrideRemove:
		return true
	}
	return false
}

// AuditEntry records a single change to a flag: who made it, when, and the
// complete flag state before and after.
type AuditEntry struct {
	ID        string          `json:"id"`
	FlagName  string          `json:"flag_name"`
	Actor     string          `json:"actor"`
	Action    Action          `json:"action"`
	Before    json.RawMessage `json:"before"` // null when the flag did not exist
	After     json.RawMessage `json:"after"`  // null when the flag was deleted
	RequestID string          `json:"request_id,omitempty"`
	At        time.Time       `json:"at"`
}

// Validate checks the entry is complete enough to be a trustworthy record.
func (e AuditEntry) Validate() error {
	if e.ID == "" {
		return Invalidf("audit entry id must not be empty")
	}
	if err := ValidateName(e.FlagName); err != nil {
		return err
	}
	if err := ValidateActor(e.Actor); err != nil {
		return err
	}
	if !e.Action.valid() {
		return Invalidf("unknown audit action %q", e.Action)
	}
	if e.At.IsZero() {
		return Invalidf("audit entry time must be set")
	}
	return nil
}

// ValidateActor checks the identity recorded as the author of a change.
func ValidateActor(actor string) error {
	return validateIdentifier("actor", actor)
}

// Snapshot serialises a flag for the Before and After fields of an audit
// entry. A nil flag (no previous or no next state) becomes JSON null.
func Snapshot(f *Flag) (json.RawMessage, error) {
	if f == nil {
		return json.RawMessage("null"), nil
	}
	b, err := json.Marshal(f)
	if err != nil {
		return nil, err
	}
	return b, nil
}
