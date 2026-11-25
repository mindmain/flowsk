// Package audit models immutable audit trail query projections and read-side
// governance contracts for Workspace activity inspection.
//
// Architectural & Transactional Invariants:
//   - Zero Transport Dependencies: Pure native Go domain types only.
//   - Write-Side vs. Read-Side Separation: Audit writes occur atomically inside
//     every domain mutation transaction via `internal/domain/tx.AuditRecorder` so
//     that business mutations and audit rows share a single ACID commit boundary.
//     This package provides the read-side projection (`Record`), filtering inputs,
//     and read/query `Service` used by operators and agents inspecting history.
//   - Immutability: Audit records are append-only; no Update or Delete operations
//     exist on audit history.
package audit

import (
	"time"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/tx"
)

// Record represents a persisted, immutable audit trail entry returned by history queries.
type Record struct {
	// ID is the unique primary UUID of the audit entry.
	ID uuid.UUID

	// WorkspaceID is the mandatory tenant partition owning this event.
	WorkspaceID uuid.UUID

	// ActorID identifies the human or autonomous agent principal that executed the mutation.
	ActorID uuid.UUID

	// ActorKind classifies whether the actor was human or an autonomous agent at execution time.
	ActorKind authctx.ActorKind

	// ActorHandle captures the actor's handle for compact timeline display.
	ActorHandle string

	// Action identifies the mutation category (Create, Update, Delete, Transition, Claim, Release, BatchExec).
	Action tx.AuditAction

	// ResourceType identifies the mutated domain aggregate type (for example, "task", "project").
	ResourceType string

	// ResourceID is the canonical UUID or key of the mutated entity.
	ResourceID string

	// OldValues holds a UTF-8 JSON snapshot of relevant fields prior to mutation.
	// Callers MUST use OldValuesCopy() or Clone() when crossing package boundaries.
	OldValues []byte

	// NewValues holds a UTF-8 JSON snapshot of relevant fields after mutation.
	// Callers MUST use NewValuesCopy() or Clone() when crossing package boundaries.
	NewValues []byte

	// OccurredAt is the UTC transaction timestamp when the mutation occurred.
	OccurredAt time.Time
}

// OldValuesCopy returns a defensive copy of the OldValues JSON byte slice.
func (r Record) OldValuesCopy() []byte {
	if len(r.OldValues) == 0 {
		return nil
	}
	out := make([]byte, len(r.OldValues))
	copy(out, r.OldValues)
	return out
}

// NewValuesCopy returns a defensive copy of the NewValues JSON byte slice.
func (r Record) NewValuesCopy() []byte {
	if len(r.NewValues) == 0 {
		return nil
	}
	out := make([]byte, len(r.NewValues))
	copy(out, r.NewValues)
	return out
}

// Clone returns a deep copy of Record, duplicating both JSON snapshot byte slices.
func (r Record) Clone() Record {
	out := r
	out.OldValues = r.OldValuesCopy()
	out.NewValues = r.NewValuesCopy()
	return out
}

// QueryFilter specifies tenant-scoped filtering criteria for querying immutable audit logs.
type QueryFilter struct {
	// WorkspaceID is the mandatory tenant scope verified against caller identity.
	WorkspaceID uuid.UUID

	// ActorID optionally restricts events to a single human or agent principal when non-zero.
	ActorID uuid.UUID

	// Action optionally restricts events to a specific tx.AuditAction when non-empty.
	Action tx.AuditAction

	// ResourceType optionally restricts events to a domain entity type (for example, "task").
	ResourceType string

	// ResourceID optionally restricts events to a single entity ID.
	ResourceID string

	// Since optionally restricts events to OccurredAt >= *Since.
	Since *time.Time

	// Until optionally restricts events to OccurredAt <= *Until.
	Until *time.Time

	// Limit caps the maximum number of audit records returned (token-budget friendly).
	Limit int32

	// Offset specifies pagination offset.
	Offset int32
}
