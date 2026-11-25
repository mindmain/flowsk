// Package db defines dialect-agnostic database lifecycle contracts, Goose schema
// migration runner interfaces, and fine-grained Dependency Injection connection
// provider interfaces for Flowsk's dual SQLite (`modernc.org/sqlite`) and
// PostgreSQL (`jackc/pgx/v5`) persistence backends.
//
// Dependency Injection Invariants (`ocinsh-go-dependency-injection`):
//   - Transport handlers (ConnectRPC, MCP, CLI) declare local segregated dependency
//     interfaces (for example, `type DataDependencies interface { db.TxManagerProvider; db.TaskRepoProvider }`)
//     embedding ONLY the single-method accessors required by that handler's domain.
//   - Handlers MUST resolve these accessors immediately inside their `New(...)`
//     constructor, pass the concrete resolved `tx.Manager` and domain `Repository`
//     instances into `domain.NewService(...)`, and NEVER leak connection provider
//     interfaces into `internal/domain` or store unresolved providers for request-time lookup.
package db

import (
	"context"
	"time"

	"github.com/mindmain/flowsk/internal/domain/audit"
	"github.com/mindmain/flowsk/internal/domain/objective"
	"github.com/mindmain/flowsk/internal/domain/project"
	"github.com/mindmain/flowsk/internal/domain/task"
	"github.com/mindmain/flowsk/internal/domain/tx"
	"github.com/mindmain/flowsk/internal/domain/workspace"
)

// Dialect identifies the active relational SQL engine backing a DatabaseAdapter.
type Dialect string

const (
	// DialectSQLite selects the embedded pure-Go `modernc.org/sqlite` engine
	// running in WAL mode on local disk or a shared local/Docker volume.
	DialectSQLite Dialect = "sqlite"

	// DialectPostgres selects the pooled `github.com/jackc/pgx/v5` PostgreSQL engine
	// for networked multi-replica server deployments.
	DialectPostgres Dialect = "postgres"
)

// Valid reports whether d is a supported SQL Dialect.
func (d Dialect) Valid() bool {
	return d == DialectSQLite || d == DialectPostgres
}

// MigrationVersionStatus describes the applied state of a single Goose SQL migration.
type MigrationVersionStatus struct {
	// Version is the numeric Goose migration version (for example, 20261115000001).
	Version int64

	// Name is the descriptive migration filename stem.
	Name string

	// IsApplied reports whether this migration has been applied to the target database.
	IsApplied bool

	// AppliedAt is the UTC timestamp when the migration ran (zero if unapplied).
	AppliedAt time.Time
}

// MigrationRunner wraps Goose (`github.com/pressly/goose/v3`) to apply, roll back,
// and inspect embedded SQL schema migrations deterministically for the active Dialect.
//
// Operational Invariants:
//   - SQLite migrations run under an exclusive advisory volume lock when backed by a
//     shared volume so concurrent processes cannot race during DDL upgrades.
//   - All Goose migrations must be strictly reversible (`-- +goose Up` and `-- +goose Down`).
type MigrationRunner interface {
	// Up applies all pending embedded Goose migrations in ascending version order,
	// returning the final applied schema version.
	Up(ctx context.Context) (currentVersion int64, err error)

	// Down rolls back the single most recently applied Goose migration, returning
	// the resulting schema version.
	Down(ctx context.Context) (currentVersion int64, err error)

	// Status inspects applied vs. pending Goose migrations without mutating schema state,
	// returning a defensive slice of MigrationVersionStatus records.
	Status(ctx context.Context) ([]MigrationVersionStatus, error)
}

// TxManagerProvider is a single-method DI accessor exposing the resolved ACID
// transaction Manager (`tx.Manager`).
type TxManagerProvider interface {
	TxManager() tx.Manager
}

// WorkspaceRepoProvider is a single-method DI accessor exposing the dialect-backed
// `workspace.Repository`.
type WorkspaceRepoProvider interface {
	WorkspaceRepository() workspace.Repository
}

// ProjectRepoProvider is a single-method DI accessor exposing the dialect-backed
// `project.Repository`.
type ProjectRepoProvider interface {
	ProjectRepository() project.Repository
}

// ObjectiveRepoProvider is a single-method DI accessor exposing the dialect-backed
// `objective.Repository`.
type ObjectiveRepoProvider interface {
	ObjectiveRepository() objective.Repository
}

// TaskRepoProvider is a single-method DI accessor exposing the dialect-backed
// `task.Repository`.
type TaskRepoProvider interface {
	TaskRepository() task.Repository
}

// AuditRepoProvider is a single-method DI accessor exposing the dialect-backed
// `audit.Repository`.
type AuditRepoProvider interface {
	AuditRepository() audit.Repository
}

// MigrationRunnerProvider is a single-method DI accessor exposing the dialect-specific
// Goose `MigrationRunner`.
type MigrationRunnerProvider interface {
	Migrations() MigrationRunner
}

// DatabaseConnection composes the fine-grained repository and transaction manager
// provider interfaces for bootstrap wiring, while individual transport handlers
// embed ONLY the narrow single-method provider interfaces they actually consume.
type DatabaseConnection interface {
	TxManagerProvider
	WorkspaceRepoProvider
	ProjectRepoProvider
	ObjectiveRepoProvider
	TaskRepoProvider
	AuditRepoProvider
	MigrationRunnerProvider
}

// DatabaseAdapter manages the full connection pool lifecycle, health probes,
// schema migrations, transaction management, and sqlc repository provisioning
// for either SQLite or PostgreSQL.
type DatabaseAdapter interface {
	DatabaseConnection

	// Dialect returns DialectSQLite or DialectPostgres for diagnostics and telemetry.
	Dialect() Dialect

	// Ping verifies live connectivity and write/read health against the underlying database.
	Ping(ctx context.Context) error

	// Close gracefully drains in-flight transactions, checkpoints WAL if applicable,
	// and releases all underlying database connections.
	Close() error
}
