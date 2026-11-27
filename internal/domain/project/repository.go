// File repository.go defines the consumer-owned persistence port for logical
// Projects within a multi-tenant Workspace (zero host filesystem paths).
package project

import (
	"context"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/domain/tx"
)

// Repository defines the persistence contract required exclusively by the
// project bounded context.
//
// Ownership & Transaction Invariants:
//   - Every query and mutation explicitly requires workspaceID so cross-tenant
//     reads or writes are structurally impossible at the persistence boundary.
//   - Soft-deleted rows (deleted_at IS NOT NULL) are excluded from all queries.
//   - Mutations MUST run on a transaction-bound repository acquired via WithScope(scope).
type Repository interface {
	// WithScope returns a shallow copy of Repository bound to the active transaction Scope.
	WithScope(scope tx.Scope) Repository

	// Create inserts a new Project scoped to p.WorkspaceID.
	// Returns errs.ErrConflict if Slug or KeyPrefix already exists in the workspace.
	Create(ctx context.Context, p Project) error

	// GetByID retrieves an active Project by (workspaceID, projectID).
	// Returns errs.ErrNotFound if absent, soft-deleted, or owned by a different workspace.
	GetByID(ctx context.Context, workspaceID, projectID uuid.UUID) (Project, error)

	// GetBySlug retrieves an active Project by (workspaceID, slug).
	// Returns errs.ErrNotFound if absent or soft-deleted.
	GetBySlug(ctx context.Context, workspaceID uuid.UUID, slug string) (Project, error)

	// List returns active Projects matching filter within filter.WorkspaceID,
	// ordered deterministically by Slug ascending.
	List(ctx context.Context, filter ListProjectsFilter) ([]Project, error)

	// Update persists changes to an active Project strictly matching (p.WorkspaceID, p.ID).
	// Returns errs.ErrNotFound if absent or soft-deleted.
	Update(ctx context.Context, p Project) error

	// NextTaskSequence atomically increments and returns the next monotonic task
	// sequence number for (workspaceID, projectID) inside the enclosing transaction.
	NextTaskSequence(ctx context.Context, workspaceID, projectID uuid.UUID) (keyPrefix string, seq int64, err error)

	// SoftDelete marks (workspaceID, projectID) as soft-deleted.
	// Returns errs.ErrNotFound if absent or already soft-deleted.
	SoftDelete(ctx context.Context, workspaceID, projectID uuid.UUID) error

	// UpsertBinding creates or updates a parent->child sub-project binding edge.
	UpsertBinding(ctx context.Context, b ProjectBinding) (ProjectBinding, error)

	// DeleteBinding removes a single parent->child binding edge without deleting either project.
	DeleteBinding(ctx context.Context, workspaceID, parentProjectID, childProjectID uuid.UUID) (bool, error)

	// DeleteBindingsForProject removes all binding edges referencing projectID.
	DeleteBindingsForProject(ctx context.Context, workspaceID, projectID uuid.UUID) error

	// ListBindings returns active ProjectBindings where both parent and child projects are non-deleted.
	ListBindings(ctx context.Context, filter ListBindingsFilter) ([]ProjectBinding, error)

	// SetHasMarkdownDoc toggles the persisted has_markdown_doc flag on (workspaceID, projectID)
	// and returns the updated Project aggregate.
	SetHasMarkdownDoc(ctx context.Context, workspaceID, projectID uuid.UUID, hasDoc bool) (Project, error)
}

// BoardMetadataRepository is an optional segregated persistence interface implemented by
// concrete SQLite and PostgreSQL ProjectRepository adapters to persist custom per-project
// Kanban workflow states and labels without breaking lightweight Repository test doubles.
type BoardMetadataRepository interface {
	// ListWorkflowStates returns all active WorkflowStates for (workspaceID, projectID)
	// ordered deterministically by Position ascending.
	ListWorkflowStates(ctx context.Context, workspaceID, projectID uuid.UUID) ([]WorkflowState, error)

	// CreateWorkflowState inserts a new WorkflowState scoped to (st.WorkspaceID, st.ProjectID).
	CreateWorkflowState(ctx context.Context, st WorkflowState) (WorkflowState, error)

	// UpdateWorkflowState applies partial updates to an active WorkflowState in (in.WorkspaceID, in.ProjectID).
	UpdateWorkflowState(ctx context.Context, in UpdateWorkflowStateInput) (WorkflowState, error)

	// ListLabels returns all active ProjectLabels visible to (workspaceID, projectID).
	ListLabels(ctx context.Context, workspaceID, projectID uuid.UUID) ([]ProjectLabel, error)

	// CreateLabel inserts a new ProjectLabel scoped to (lbl.WorkspaceID, lbl.ProjectID).
	CreateLabel(ctx context.Context, lbl ProjectLabel) (ProjectLabel, error)

	// DeleteLabel soft-deletes an active ProjectLabel matching (workspaceID, projectID, labelID).
	DeleteLabel(ctx context.Context, workspaceID, projectID, labelID uuid.UUID) error
}
