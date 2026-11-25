// File repository.go defines the read-only persistence port for immutable
// workspace audit records implemented by the SQLite and PostgreSQL adapters.
package audit

import (
	"context"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/domain/tx"
)

// Repository defines the append-and-read persistence contract for Workspace audit history.
//
// Ownership & Immutability Invariants:
//   - Audit rows are strictly append-only; no Update or Delete methods are ever exposed.
//   - Every read query MUST filter by workspaceID to prevent cross-tenant leakage.
type Repository interface {
	// WithScope binds this repository to an active ACID transaction Scope.
	WithScope(scope tx.Scope) Repository

	// GetByID retrieves a single immutable audit Record by (workspaceID, auditID).
	// Returns errs.ErrNotFound if absent or belonging to another workspace.
	GetByID(ctx context.Context, workspaceID, auditID uuid.UUID) (Record, error)

	// Query returns immutable audit Records matching filter within filter.WorkspaceID,
	// ordered deterministically by OccurredAt descending, then ID descending.
	Query(ctx context.Context, filter QueryFilter) ([]Record, error)
}
