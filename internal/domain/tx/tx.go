// Package tx defines dialect-agnostic ACID transaction boundaries and atomic
// audit logging contracts shared across Flowsk domain services, the Cloudflare
// Code Mode JavaScript sandbox, and SQLite/PostgreSQL database adapters.
//
// Architectural & Transactional Invariants:
//   - Mutations Inside Transactions Only: Every state-changing operation in any
//     domain service or Code Mode script MUST execute inside Manager.WithTransaction
//     (or Manager.WithNoAuditTransaction for explicitly exempted internal maintenance).
//   - Strict Scope Isolation: All SQL queries and mutations inside a transaction
//     callback MUST execute exclusively against the callback's Scope (via
//     repo.WithScope(scope)). Reusing unscoped database pools inside a transaction
//     callback is strictly forbidden, as it causes deadlocks under SQLite single-writer
//     WAL locks and breaks snapshot isolation under PostgreSQL.
//   - Fail-Fast Rollback: Returning any non-nil error from the transaction callback
//     immediately aborts execution and triggers an automatic rollback of both business
//     writes and audit entries.
//   - Atomic Audit Logging: Audit diffs (OldValues -> NewValues) are persisted on
//     scope.Audit().Record(ctx, entry) inside the exact same database transaction
//     prior to commit. If audit persistence fails, the entire mutation rolls back;
//     if business logic fails, no orphaned audit log is ever committed.
package tx

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// AuditAction enumerates the canonical state-mutation categories recorded in the
// immutable Flowsk audit log.
type AuditAction string

const (
	// AuditActionCreate records the creation of a new domain aggregate or relation.
	AuditActionCreate AuditAction = "Create"

	// AuditActionUpdate records field-level modifications on an existing aggregate.
	AuditActionUpdate AuditAction = "Update"

	// AuditActionDelete records a soft-delete or relation removal operation.
	AuditActionDelete AuditAction = "Delete"

	// AuditActionTransition records a workflow lifecycle status transition on a task or objective.
	AuditActionTransition AuditAction = "Transition"

	// AuditActionClaim records an actor acquiring or renewing an exclusive ClaimLease on a task.
	AuditActionClaim AuditAction = "Claim"

	// AuditActionRelease records an actor voluntarily releasing or an admin revoking a ClaimLease.
	AuditActionRelease AuditAction = "Release"

	// AuditActionBatchExec records the atomic execution summary of a Code Mode JavaScript script.
	AuditActionBatchExec AuditAction = "BatchExec"
)

// Valid reports whether a is one of the canonical AuditAction constants.
func (a AuditAction) Valid() bool {
	switch a {
	case AuditActionCreate,
		AuditActionUpdate,
		AuditActionDelete,
		AuditActionTransition,
		AuditActionClaim,
		AuditActionRelease,
		AuditActionBatchExec:
		return true
	default:
		return false
	}
}

// AuditEntry represents a single immutable state-change diff captured inside an
// active ACID transaction before commit.
//
// Defensive Copy Invariant:
//   - OldValues and NewValues carry JSON-encoded snapshots of the entity state
//     prior to and following the mutation. Callers should use Clone() or defensive
//     byte copies when passing slices across package boundaries.
type AuditEntry struct {
	// ID is the unique identifier of the audit log record. If uuid.Nil when passed
	// to AuditRecorder.Record, the recorder generates a new UUIDv7/UUIDv4 identifier.
	ID uuid.UUID

	// WorkspaceID is the mandatory tenant boundary owning the mutated resource.
	WorkspaceID uuid.UUID

	// ActorID identifies the authenticated human or autonomous agent that performed
	// the mutation.
	ActorID uuid.UUID

	// Action classifies the state change (Create, Update, Delete, Transition, Claim, Release, BatchExec).
	Action AuditAction

	// ResourceType identifies the bounded-context entity type (for example,
	// "workspace", "project", "objective", "task", "task_relation", "task_comment", "codemode_script").
	ResourceType string

	// ResourceID is the canonical UUID or composite identifier of the mutated entity.
	ResourceID string

	// OldValues contains an optional UTF-8 JSON object representing relevant fields
	// prior to mutation (nil or empty for AuditActionCreate).
	OldValues []byte

	// NewValues contains an optional UTF-8 JSON object representing relevant fields
	// after mutation (nil or empty for hard removals, populated with soft-delete timestamp for soft deletes).
	NewValues []byte

	// OccurredAt is the UTC timestamp of the mutation. If zero, the AuditRecorder
	// stamps the current transaction clock time.
	OccurredAt time.Time
}

// Clone returns a deep copy of AuditEntry, duplicating OldValues and NewValues
// byte slices so neither caller nor adapter can alias mutable backing arrays.
func (e AuditEntry) Clone() AuditEntry {
	out := e
	if e.OldValues != nil {
		out.OldValues = make([]byte, len(e.OldValues))
		copy(out.OldValues, e.OldValues)
	}
	if e.NewValues != nil {
		out.NewValues = make([]byte, len(e.NewValues))
		copy(out.NewValues, e.NewValues)
	}
	return out
}

// AuditRecorder persists AuditEntry records inside the active database transaction
// bound to a Scope.
type AuditRecorder interface {
	// Record inserts entry into the workspace audit log table using the enclosing
	// transaction handle.
	//
	// Error Contract:
	//   - Returns errs.ErrValidation if WorkspaceID, ActorID, Action, ResourceType, or ResourceID is invalid.
	//   - Returns errs.ErrPreconditionFailed if invoked on a no-audit transaction scope where auditing is disabled,
	//     unless the adapter implements a documented no-op recorder for WithNoAuditTransaction.
	//   - Any non-nil error MUST be propagated by the caller so Manager.WithTransaction rolls back atomically.
	Record(ctx context.Context, entry AuditEntry) error
}

// DialectTxHandle is an opaque marker interface implemented by concrete dialect-specific
// transaction wrappers (such as SQLite *sql.Tx wrappers and PostgreSQL pgx.Tx wrappers).
//
// Architectural Intent:
//   - Domain services never inspect or type-assert DialectTxHandle.
//   - Dialect-specific sqlc repository implementations type-assert Scope.TxHandle()
//     internally to bind generated sqlc Queries (`queries.WithTx(...)`) to the active
//     transaction without leaking `database/sql` or `pgx/v5` into `internal/domain`.
type DialectTxHandle interface {
	// TxDialect returns the canonical backend identifier (for example, "sqlite" or "postgres")
	// of the underlying open transaction handle.
	TxDialect() string
}

// Scope represents an active, single-flight ACID database transaction bound to a
// specific SQLite or PostgreSQL connection.
//
// Concurrency Invariant:
//   - A Scope wraps a single underlying database transaction connection and is NOT
//     safe for concurrent use by multiple goroutines simultaneously. Sequential
//     execution within the transaction callback is required.
type Scope interface {
	// Audit returns the transaction-bound AuditRecorder whose Record calls commit
	// or roll back atomically with all repository writes executed on this Scope.
	Audit() AuditRecorder

	// IsAuditEnabled reports whether this Scope enforces atomic audit recording
	// (true for WithTransaction, false for WithNoAuditTransaction).
	IsAuditEnabled() bool

	// TxHandle returns the opaque dialect-specific transaction handle consumed by
	// concrete adapter repositories when binding sqlc query executors.
	TxHandle() DialectTxHandle
}

// TxFunc is the unit-of-work callback executed inside a managed transaction Scope.
// Returning nil commits the transaction; returning any non-nil error rolls it back.
type TxFunc func(ctx context.Context, scope Scope) error

// Manager orchestrates ACID transactions, fail-fast rollbacks, and atomic audit
// enforcement across domain services and the Code Mode JavaScript sandbox.
type Manager interface {
	// WithTransaction opens a new ACID database transaction, invokes fn with a
	// transaction-bound Scope (where Audit() is active), and commits if and only if
	// fn returns nil and at least one audit entry was recorded (or read-only checks succeeded).
	//
	// Semantics & Error Contract:
	//   - Propagates ctx cancellation/deadlines prior to and during transaction lifecycle.
	//   - Automatically rolls back the transaction if fn returns a non-nil error or panics
	//     (re-panicking after rollback cleanup).
	//   - Returns any commit failure wrapped with %w so callers ALWAYS verify completion.
	WithTransaction(ctx context.Context, fn TxFunc) error

	// WithNoAuditTransaction opens an ACID database transaction with audit enforcement
	// explicitly bypassed.
	//
	// Governance Invariant:
	//   - Reserved strictly for high-throughput idempotent background workers (such as
	//     expired lease reaping), internal syncs, or snapshot maintenance.
	//   - Standard user- or agent-initiated domain mutations MUST use WithTransaction.
	WithNoAuditTransaction(ctx context.Context, fn TxFunc) error
}
