// File repository.go defines the consumer-owned persistence port for multi-tenant
// Workspaces and human/agent actor memberships.
package workspace

import (
	"context"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/domain/tx"
)

// Repository defines the persistence contract required exclusively by the
// workspace bounded context.
//
// Implementation & Ownership Invariants:
//   - Concrete implementations live in the database adapter layer wrapping sqlc-generated
//     queries for SQLite and PostgreSQL; manual raw SQL string concatenation is forbidden.
//   - Soft-deleted workspaces (deleted_at IS NOT NULL) MUST be excluded from all standard
//     read queries.
//   - All writes inside domain services MUST obtain a transaction-bound repository via
//     WithScope(scope) inside tx.Manager.WithTransaction.
type Repository interface {
	// WithScope binds this repository to an active ACID transaction Scope so all
	// subsequent calls execute strictly on that transaction handle.
	WithScope(scope tx.Scope) Repository

	// Create inserts a newly provisioned Workspace row.
	// Returns errs.ErrConflict if Slug or ID already exists.
	Create(ctx context.Context, ws Workspace) error

	// GetByID fetches an active (non-deleted) Workspace by its primary UUID.
	// Returns errs.ErrNotFound if no matching active workspace exists.
	GetByID(ctx context.Context, workspaceID uuid.UUID) (Workspace, error)

	// GetBySlug fetches an active (non-deleted) Workspace by its unique slug.
	// Returns errs.ErrNotFound if no matching active workspace exists.
	GetBySlug(ctx context.Context, slug string) (Workspace, error)

	// ListForActor returns all active Workspaces in which actorID holds membership,
	// ordered deterministically by Slug ascending.
	ListForActor(ctx context.Context, actorID uuid.UUID) ([]Workspace, error)

	// Update persists mutable metadata changes to an existing active Workspace.
	// Returns errs.ErrNotFound if workspaceID does not exist or is soft-deleted.
	Update(ctx context.Context, ws Workspace) error

	// SoftDelete marks the target Workspace as deleted using a UTC timestamp.
	// Returns errs.ErrNotFound if workspaceID does not exist or is already deleted.
	SoftDelete(ctx context.Context, workspaceID uuid.UUID) error

	// UpsertMember inserts or updates a WorkspaceMember scoped strictly to m.WorkspaceID.
	// Returns errs.ErrConflict if Handle collides with a different ActorID in the workspace.
	UpsertMember(ctx context.Context, m WorkspaceMember) error

	// ListMembers returns all members belonging to workspaceID ordered by Handle ascending.
	ListMembers(ctx context.Context, workspaceID uuid.UUID) ([]WorkspaceMember, error)
}
