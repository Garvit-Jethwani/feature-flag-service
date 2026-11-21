package flag

import (
	"encoding/json"
	"time"
)

// AuditAction names the kind of change an audit entry records.
type AuditAction string

// Audit actions.
const (
	ActionFlagCreated     AuditAction = "flag.created"
	ActionFlagUpdated     AuditAction = "flag.updated"
	ActionFlagDeleted     AuditAction = "flag.deleted"
	ActionOverrideSet     AuditAction = "override.set"
	ActionOverrideRemoved AuditAction = "override.removed"
)

// AuditEntry records one change to a flag: who made it, when, and the value
// before and after.
type AuditEntry struct {
	ID       int64
	FlagName string
	Action   AuditAction
	// Actor is the name bound to the API key that made the change.
	Actor string
	// Previous and New are JSON documents (a FlagState or an OverrideState).
	// Previous is nil for a creation; New is nil for a deletion.
	Previous   json.RawMessage
	New        json.RawMessage
	OccurredAt time.Time
}

// FlagState is the part of a flag recorded in the audit trail.
type FlagState struct {
	Description       string   `json:"description"`
	Enabled           bool     `json:"enabled"`
	RolloutPercentage int      `json:"rollout_percentage"`
	Prerequisites     []string `json:"prerequisites"`
}

// OverrideState is a per-user override as recorded in the audit trail.
type OverrideState struct {
	UserID  string `json:"user_id"`
	Enabled bool   `json:"enabled"`
}

// State returns the auditable state of the flag.
func (f Flag) State() FlagState {
	prerequisites := f.Prerequisites
	if prerequisites == nil {
		prerequisites = []string{} // record [] rather than null
	}
	return FlagState{
		Description:       f.Description,
		Enabled:           f.Enabled,
		RolloutPercentage: f.RolloutPercentage,
		Prerequisites:     prerequisites,
	}
}
