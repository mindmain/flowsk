// File repository.go defines the consumer-owned persistence port for strategic
// Objectives (goals, milestones, cycles) scoped by workspace and project.
package objective

import (
	"context"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/domain/tx"
)

// Repository defines the persistence contract required exclusively by the
// objective bounded context.
//
// Ownership & Transaction Invariants:
//   - Every method requires workspaceID explicitly so cross-tenant isolation is
//     enforced directly in SQL WHERE clauses.
//   - Soft-deleted records (deleted_at IS NOT NULL) are excluded from all queries.
//   - Mutations MUST execute on a transaction-bound Repository obtained via WithScope(scope).
type Repository interface {
	// WithScope binds this repository to an active ACID transaction Scope.
	WithScope(scope tx.Scope) Repository

	// Create inserts a new Objective scoped to (o.WorkspaceID, o.ProjectID).
	// Returns errs.ErrConflict if Slug already exists in the project.
	Create(ctx context.Context, o Objective) error

	// GetByID retrieves an active Objective by (workspaceID, objectiveID).
	// Returns errs.ErrNotFound if absent or soft-deleted.
	GetByID(ctx context.Context, workspaceID, objectiveID uuid.UUID) (Objective, error)

	// GetBySlug retrieves an active Objective by (workspaceID, projectID, slug).
	// Returns errs.ErrNotFound if absent or soft-deleted.
	GetBySlug(ctx context.Context, workspaceID, projectID uuid.UUID, slug string) (Objective, error)

	// List returns active Objectives matching filter within filter.WorkspaceID,
	// ordered by PriorityRank ascending, then CreatedAt ascending.
	List(ctx context.Context, filter ListObjectivesFilter) ([]Objective, error)

	// Update persists modifications to an active Objective matching (o.WorkspaceID, o.ID).
	// Returns errs.ErrNotFound if absent or soft-deleted.
	Update(ctx context.Context, o Objective) error

	// SoftDelete marks (workspaceID, objectiveID) as soft-deleted.
	// Returns errs.ErrNotFound if absent or already deleted.
	SoftDelete(ctx context.Context, workspaceID, objectiveID uuid.UUID) error
}
