// File repository.go defines the consumer-owned persistence port for Tasks,
// dependency edges, comments, agent claim leases, and compact project digests.
package task

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/domain/tx"
)

// Repository defines the persistence contract required exclusively by the task
// bounded context, covering Tasks, TaskRelations, TaskComments, ClaimLeases,
// and compact agent queue/digest queries.
//
// Ownership, Concurrency & Transaction Invariants:
//   - Mandatory Workspace Scoping: Every single method requires workspaceID so
//     sqlc queries always include `WHERE workspace_id = ?` and never leak cross-tenant rows.
//   - Soft-Delete Safety: Soft-deleted tasks (`deleted_at IS NOT NULL`) MUST be
//     excluded from all lookups, queues, blocker counts, and digests.
//   - Transaction Scope Isolation: All mutations (and read-modify-write checks for
//     leases, DAG cycle detection, and optimistic locking) MUST run on a transaction-bound
//     repository obtained via WithScope(scope) inside tx.Manager.WithTransaction.
type Repository interface {
	// WithScope binds this repository to an active ACID transaction Scope.
	WithScope(scope tx.Scope) Repository

	// Create inserts a new Task row scoped to t.WorkspaceID.
	// Returns errs.ErrConflict on duplicate Key or ID.
	Create(ctx context.Context, t Task) error

	// GetByID fetches a full Task aggregate (including ActiveLease and UnresolvedBlockerCount)
	// by (workspaceID, taskID). Returns errs.ErrNotFound if absent or soft-deleted.
	GetByID(ctx context.Context, workspaceID, taskID uuid.UUID) (Task, error)

	// GetByKey fetches a full Task aggregate by its human/LLM compact key (for example, "FLW-104")
	// within workspaceID. Returns errs.ErrNotFound if absent or soft-deleted.
	GetByKey(ctx context.Context, workspaceID uuid.UUID, key string) (Task, error)

	// GetByExternalID fetches a full Task aggregate by its project-scoped external tracker ID
	// (workspaceID, projectID, externalID). Returns errs.ErrNotFound if absent or soft-deleted.
	GetByExternalID(ctx context.Context, workspaceID, projectID uuid.UUID, externalID string) (Task, error)

	// Update persists field and status mutations on an active Task, incrementing Version
	// and verifying expectedVersion when non-nil (returning errs.ErrConflict on version mismatch).
	Update(ctx context.Context, t Task, expectedVersion *int64) error

	// SoftDelete marks (workspaceID, taskID) as soft-deleted and clears any active lease.
	SoftDelete(ctx context.Context, workspaceID, taskID uuid.UUID) error

	// QueryCompactQueue executes a token-optimized query returning CompactTaskSummary rows
	// ordered deterministically by Priority ascending, CreatedAt ascending, ID ascending.
	QueryCompactQueue(ctx context.Context, filter AgentWorkQueueFilter, now time.Time) ([]CompactTaskSummary, error)

	// BuildProjectDigest computes or assembles a ProjectDigest snapshot for (workspaceID, projectID).
	BuildProjectDigest(ctx context.Context, workspaceID, projectID uuid.UUID, opts DigestOptions, now time.Time) (ProjectDigest, error)

	// UpsertLease inserts or updates the exclusive ClaimLease row for lease.TaskID
	// inside an active transaction after service-level expiration/holder validation.
	UpsertLease(ctx context.Context, lease ClaimLease) error

	// GetLease retrieves the stored ClaimLease for (workspaceID, taskID), if any.
	// Returns errs.ErrNotFound when no lease row exists on the task.
	GetLease(ctx context.Context, workspaceID, taskID uuid.UUID) (ClaimLease, error)

	// DeleteLease removes the ClaimLease row for (workspaceID, taskID).
	DeleteLease(ctx context.Context, workspaceID, taskID uuid.UUID) error

	// DeleteExpiredLeases removes expired ClaimLease rows (`expires_at <= asOf`) within
	// workspaceID (or across workspaces during background maintenance when workspaceID == uuid.Nil),
	// returning the count of reaped leases.
	DeleteExpiredLeases(ctx context.Context, workspaceID uuid.UUID, asOf time.Time) (int64, error)

	// CreateRelation inserts a directed TaskRelation edge.
	// Returns errs.ErrConflict if an identical (SourceTaskID, TargetTaskID, Type) edge already exists.
	CreateRelation(ctx context.Context, rel TaskRelation) error

	// DeleteRelation removes a directed TaskRelation edge matching (workspaceID, relationID).
	DeleteRelation(ctx context.Context, workspaceID, relationID uuid.UUID) error

	// ListRelationsForTask returns all inbound and outbound TaskRelation edges involving taskID
	// within workspaceID.
	ListRelationsForTask(ctx context.Context, workspaceID, taskID uuid.UUID) ([]TaskRelation, error)

	// CreateComment appends an immutable TaskComment to (c.WorkspaceID, c.TaskID).
	CreateComment(ctx context.Context, c TaskComment) error

	// ListComments returns TaskComments for (workspaceID, taskID) ordered chronologically
	// by CreatedAt ascending, bounded by limit.
	ListComments(ctx context.Context, workspaceID, taskID uuid.UUID, limit int32) ([]TaskComment, error)

	// ListByWorkspace returns all active (non-soft-deleted) Task aggregates across every
	// non-deleted project in workspaceID.
	ListByWorkspace(ctx context.Context, workspaceID uuid.UUID) ([]Task, error)

	// SetHasMarkdownDoc toggles the persisted has_markdown_doc flag on (workspaceID, taskID)
	// and returns the updated Task aggregate.
	SetHasMarkdownDoc(ctx context.Context, workspaceID, taskID uuid.UUID, hasDoc bool) (Task, error)

	// HardDelete permanently deletes (workspaceID, taskID) and detaches any child subtasks
	// (used exclusively when purging archived tasks).
	HardDelete(ctx context.Context, workspaceID, taskID uuid.UUID) error
}
