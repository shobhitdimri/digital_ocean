package handler

import (
	"encoding/json"
	"time"

	"feature-flag-service/internal/domain"
	"feature-flag-service/internal/service"
)

// The types in this file are the JSON wire format. They are separate from the
// domain types so the API can stay stable while the domain evolves.

// CreateFlagRequest is the body of POST /v1/flags. A flag created without
// "enabled" starts disabled, the safe default for a new feature.
type CreateFlagRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`

	// RolloutPercent is optional; leaving it out means 100.
	RolloutPercent *int `json:"rollout_percent"`

	// Requires lists flags that must be enabled for a user before this one can
	// be. Optional.
	Requires []string `json:"requires"`
}

func (r CreateFlagRequest) options() []domain.FlagOption {
	var opts []domain.FlagOption
	if r.RolloutPercent != nil {
		opts = append(opts, domain.WithRollout(*r.RolloutPercent))
	}
	if len(r.Requires) > 0 {
		opts = append(opts, domain.WithRequires(r.Requires...))
	}
	return opts
}

// UpdateFlagRequest is the body of PUT /v1/flags/{name}. Omitted fields are
// left unchanged.
type UpdateFlagRequest struct {
	Description    *string `json:"description"`
	Enabled        *bool   `json:"enabled"`
	RolloutPercent *int    `json:"rollout_percent"`

	// Requires replaces the whole prerequisite list; [] clears it and leaving
	// it out (or null) keeps it.
	Requires *[]string `json:"requires"`
}

func (r UpdateFlagRequest) toUpdate() service.FlagUpdate {
	return service.FlagUpdate{
		Description: r.Description, Enabled: r.Enabled,
		RolloutPercent: r.RolloutPercent, Requires: r.Requires,
	}
}

// SetOverrideRequest is the body of PUT /v1/flags/{name}/overrides/{userId}.
// "enabled" is required: an override with no value would be ambiguous.
type SetOverrideRequest struct {
	Enabled *bool `json:"enabled"`
}

// FlagResponse is a flag as returned by the API.
type FlagResponse struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Enabled     bool            `json:"enabled"`
	Overrides   map[string]bool `json:"overrides"`

	// RolloutPercent is always present, 100 for a flag with no rollout.
	RolloutPercent int `json:"rollout_percent"`

	// Requires is always an array, empty for a flag with no prerequisites.
	Requires []string `json:"requires"`

	Version   int64     `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toFlagResponse(f domain.Flag) FlagResponse {
	overrides := make(map[string]bool, len(f.Overrides))
	for user, enabled := range f.Overrides {
		overrides[user] = enabled
	}
	return FlagResponse{
		Name:        f.Name,
		Description: f.Description,
		Enabled:     f.Enabled,
		Overrides:   overrides,

		RolloutPercent: f.Rollout(),
		Requires:       append([]string{}, f.Requires...),
		Version:        f.Version,
		CreatedAt:      f.CreatedAt,
		UpdatedAt:      f.UpdatedAt,
	}
}

// FlagListResponse is the body of GET /v1/flags.
type FlagListResponse struct {
	Flags []FlagResponse `json:"flags"`
}

func toFlagListResponse(flags []domain.Flag) FlagListResponse {
	out := FlagListResponse{Flags: make([]FlagResponse, len(flags))}
	for i, f := range flags {
		out.Flags[i] = toFlagResponse(f)
	}
	return out
}

// Evaluation sources reported to clients.
const (
	SourceCache = "cache"
	SourceStore = "store"
)

// EvaluateResponse is the body of GET /v1/evaluate.
type EvaluateResponse struct {
	Flag    string `json:"flag"`
	UserID  string `json:"user_id"`
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason"`
	Source  string `json:"source"`
}

func toEvaluateResponse(e service.Evaluation) EvaluateResponse {
	source := SourceStore
	if e.Cached {
		source = SourceCache
	}
	return EvaluateResponse{
		Flag:    e.Flag,
		UserID:  e.UserID,
		Enabled: e.Enabled,
		Reason:  string(e.Reason),
		Source:  source,
	}
}

// AuditEntryResponse is one audit record. Before and After are the complete
// flag state as JSON, or null when the flag did not exist before or no longer
// exists after.
type AuditEntryResponse struct {
	ID        string          `json:"id"`
	FlagName  string          `json:"flag_name"`
	Actor     string          `json:"actor"`
	Action    string          `json:"action"`
	Before    json.RawMessage `json:"before"`
	After     json.RawMessage `json:"after"`
	RequestID string          `json:"request_id,omitempty"`
	At        time.Time       `json:"at"`
}

// AuditResponse is the body of GET /v1/flags/{name}/audit, newest entry first.
type AuditResponse struct {
	Flag    string               `json:"flag"`
	Entries []AuditEntryResponse `json:"entries"`
}

func toAuditResponse(flagName string, entries []domain.AuditEntry) AuditResponse {
	out := AuditResponse{Flag: flagName, Entries: make([]AuditEntryResponse, len(entries))}
	for i, e := range entries {
		out.Entries[i] = AuditEntryResponse{
			ID:        e.ID,
			FlagName:  e.FlagName,
			Actor:     e.Actor,
			Action:    string(e.Action),
			Before:    e.Before,
			After:     e.After,
			RequestID: e.RequestID,
			At:        e.At,
		}
	}
	return out
}
