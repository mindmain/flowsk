// Package postgres provides the `github.com/jackc/pgx/v5/pgxpool` pooled PostgreSQL
// database adapter, Goose schema migration runner (`stdlib.OpenDBFromPool`), ACID
// transaction manager, and sqlc-backed domain repositories for networked Flowsk
// server deployments.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	dbmigrations "github.com/mindmain/flowsk/db"
	"github.com/mindmain/flowsk/internal/adapter/db"
	"github.com/mindmain/flowsk/internal/adapter/db/postgres/sqlcgen"
	"github.com/mindmain/flowsk/internal/domain/audit"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/objective"
	"github.com/mindmain/flowsk/internal/domain/project"
	"github.com/mindmain/flowsk/internal/domain/task"
	"github.com/mindmain/flowsk/internal/domain/tx"
	"github.com/mindmain/flowsk/internal/domain/workspace"
	"github.com/pressly/goose/v3"
)

// Config defines connection DSN, `pgxpool` sizing bounds, health-check intervals,
// and statement timeout parameters for PostgreSQL deployments.
type Config struct {
	// DSN is the PostgreSQL connection URI or keyword/value DSN string.
	// Never log this field verbatim as it may embed credentials.
	DSN string

	// MaxConns sets the maximum number of connections in the pgxpool.Pool.
	MaxConns int32

	// MinConns sets the minimum warm idle connections maintained in the pgxpool.Pool.
	MinConns int32

	// MaxConnLifetime bounds the maximum duration a connection may be reused before recycling.
	MaxConnLifetime time.Duration

	// MaxConnIdleTime bounds how long an idle connection remains in the pool before closing.
	MaxConnIdleTime time.Duration

	// HealthCheckPeriod sets the interval between background pgxpool health checks.
	HealthCheckPeriod time.Duration

	// ConnectTimeout bounds TCP + TLS + authentication handshake time for new connections.
	ConnectTimeout time.Duration

	// StatementTimeout configures per-transaction/statement timeout protection against runaway queries.
	StatementTimeout time.Duration
}

// QuerierBinder abstracts binding a `pgx.Tx` to sqlc-generated PostgreSQL query structs.
type QuerierBinder interface {
	// BindPgxTx returns a transaction-scoped query executor bound to pgxTx.
	BindPgxTx(pgxTx pgx.Tx) any
}

// TxHandle wraps an open `pgx.Tx` transaction handle to satisfy `tx.DialectTxHandle`
// without leaking `jackc/pgx/v5` into `internal/domain`.
type TxHandle struct {
	// PgxTx is the active PostgreSQL transaction handle.
	PgxTx pgx.Tx
}

// TxDialect identifies this handle as belonging to the PostgreSQL adapter (`"postgres"`).
func (h TxHandle) TxDialect() string {
	return string(db.DialectPostgres)
}

// Adapter is the production pooled PostgreSQL implementation of `db.DatabaseAdapter`.
type Adapter struct {
	cfg           Config
	poolCfg       *pgxpool.Config
	mu            sync.Mutex
	pool          *pgxpool.Pool
	bridge        *SQLCBridge
	closed        bool
	txMgr         tx.Manager
	migrations    db.MigrationRunner
	workspaceRepo workspace.Repository
	projectRepo   project.Repository
	objectiveRepo objective.Repository
	taskRepo      task.Repository
	auditRepo     audit.Repository
}

// Compile-time check ensuring *Adapter satisfies db.DatabaseAdapter.
var _ db.DatabaseAdapter = (*Adapter)(nil)

// NewAdapter validates cfg.DSN syntax offline and constructs a lazy-connecting
// PostgreSQL `Adapter` wired with Goose migrations, `tx.Manager`, and all 5
// sqlc-backed domain repositories.
func NewAdapter(cfg Config) (*Adapter, error) {
	const op = "postgres.NewAdapter"
	if strings.TrimSpace(cfg.DSN) == "" {
		return nil, errs.Validation(op, "postgres DSN is required", errs.FieldViolation{
			Field:       "dsn",
			Description: "postgres connection DSN must not be empty",
		})
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, errs.Validation(op, "invalid postgres DSN configuration", errs.FieldViolation{
			Field:       "dsn",
			Description: "postgres connection DSN failed URI/keyword parsing",
		})
	}
	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns >= 0 {
		poolCfg.MinConns = cfg.MinConns
	}
	if cfg.MaxConnLifetime > 0 {
		poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	}
	if cfg.MaxConnIdleTime > 0 {
		poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	}
	if cfg.HealthCheckPeriod > 0 {
		poolCfg.HealthCheckPeriod = cfg.HealthCheckPeriod
	}
	if cfg.ConnectTimeout > 0 {
		poolCfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	}

	a := &Adapter{
		cfg:     cfg,
		poolCfg: poolCfg,
		bridge:  NewSQLCBridge(nil),
	}
	a.txMgr = &postgresTxManager{adapter: a}
	a.migrations = &postgresMigrationRunner{adapter: a}
	a.workspaceRepo = &WorkspaceRepository{adapter: a}
	a.projectRepo = &ProjectRepository{adapter: a}
	a.objectiveRepo = &ObjectiveRepository{adapter: a}
	a.taskRepo = &TaskRepository{adapter: a}
	a.auditRepo = &AuditRepository{adapter: a}
	return a, nil
}

// ensurePool lazily initializes the `*pgxpool.Pool` on first database interaction.
func (a *Adapter) ensurePool(ctx context.Context, op string) (*pgxpool.Pool, *SQLCBridge, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", op, err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return nil, nil, errs.New(errs.ErrPreconditionFailed, op, "postgres adapter has been closed")
	}
	if a.pool != nil {
		return a.pool, a.bridge, nil
	}

	clonedCfg := a.poolCfg.Copy()
	pool, err := pgxpool.NewWithConfig(ctx, clonedCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: init pgxpool: %w", op, err)
	}
	a.pool = pool
	a.bridge = NewSQLCBridge(pool)
	return a.pool, a.bridge, nil
}

// currentBridge returns the current SQLCBridge (nil-safe before pool creation).
func (a *Adapter) currentBridge() *SQLCBridge {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.bridge
}

// Dialect returns db.DialectPostgres.
func (a *Adapter) Dialect() db.Dialect {
	return db.DialectPostgres
}

// Ping verifies live connectivity against the PostgreSQL connection pool.
func (a *Adapter) Ping(ctx context.Context) error {
	const op = "postgres.Adapter.Ping"
	pool, _, err := a.ensurePool(ctx, op)
	if err != nil {
		return err
	}
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

// Close gracefully closes the underlying `pgxpool.Pool` if one was opened.
func (a *Adapter) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	a.closed = true
	if a.pool != nil {
		a.pool.Close()
		a.pool = nil
	}
	return nil
}

// TxManager returns the ACID PostgreSQL `tx.Manager`.
func (a *Adapter) TxManager() tx.Manager {
	return a.txMgr
}

// WorkspaceRepository returns the PostgreSQL `workspace.Repository` implementation.
func (a *Adapter) WorkspaceRepository() workspace.Repository {
	return a.workspaceRepo
}

// ProjectRepository returns the PostgreSQL `project.Repository` implementation.
func (a *Adapter) ProjectRepository() project.Repository {
	return a.projectRepo
}

// ObjectiveRepository returns the PostgreSQL `objective.Repository` implementation.
func (a *Adapter) ObjectiveRepository() objective.Repository {
	return a.objectiveRepo
}

// TaskRepository returns the PostgreSQL `task.Repository` implementation.
func (a *Adapter) TaskRepository() task.Repository {
	return a.taskRepo
}

// AuditRepository returns the PostgreSQL `audit.Repository` implementation.
func (a *Adapter) AuditRepository() audit.Repository {
	return a.auditRepo
}

// Migrations returns the Goose PostgreSQL `db.MigrationRunner`.
func (a *Adapter) Migrations() db.MigrationRunner {
	return a.migrations
}

// postgresTxManager orchestrates ACID `pgx.Tx` transactions and atomic audit recording.
type postgresTxManager struct {
	adapter *Adapter
}

// WithTransaction runs fn inside an ACID PostgreSQL transaction with atomic audit recording enabled.
func (m *postgresTxManager) WithTransaction(ctx context.Context, fn tx.TxFunc) error {
	return m.runTx(ctx, "postgres.TxManager.WithTransaction", true, fn)
}

// WithNoAuditTransaction runs fn inside an ACID PostgreSQL transaction with audit recording bypassed.
func (m *postgresTxManager) WithNoAuditTransaction(ctx context.Context, fn tx.TxFunc) error {
	return m.runTx(ctx, "postgres.TxManager.WithNoAuditTransaction", false, fn)
}

func (m *postgresTxManager) runTx(ctx context.Context, op string, auditEnabled bool, fn tx.TxFunc) error {
	if fn == nil {
		return errs.Validation(op, "transaction callback is required", errs.FieldViolation{
			Field:       "fn",
			Description: "tx.TxFunc callback must not be nil",
		})
	}
	pool, bridge, err := m.adapter.ensurePool(ctx, op)
	if err != nil {
		return err
	}

	pgxTx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%s: begin tx: %w", op, err)
	}

	committed := false
	defer func() {
		if r := recover(); r != nil {
			_ = pgxTx.Rollback(context.Background())
			panic(r)
		}
		if !committed {
			_ = pgxTx.Rollback(context.Background())
		}
	}()

	if m.adapter.cfg.StatementTimeout > 0 {
		timeoutMS := m.adapter.cfg.StatementTimeout.Milliseconds()
		if _, err := pgxTx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", timeoutMS)); err != nil {
			return fmt.Errorf("%s: set statement_timeout: %w", op, err)
		}
	}

	queries := bridge.QueriesForTx(pgxTx)
	scope := &postgresTxScope{
		pgxTx:        pgxTx,
		auditEnabled: auditEnabled,
		recorder: &postgresAuditRecorder{
			queries:      queries,
			auditEnabled: auditEnabled,
		},
	}

	if err := fn(ctx, scope); err != nil {
		return err
	}
	if err := pgxTx.Commit(ctx); err != nil {
		return fmt.Errorf("%s: commit tx: %w", op, db.MapSQLDriverError(op, err))
	}
	committed = true
	return nil
}

// postgresTxScope implements `tx.Scope` over an open `pgx.Tx`.
type postgresTxScope struct {
	pgxTx        pgx.Tx
	auditEnabled bool
	recorder     tx.AuditRecorder
}

func (s *postgresTxScope) Audit() tx.AuditRecorder {
	return s.recorder
}

func (s *postgresTxScope) IsAuditEnabled() bool {
	return s.auditEnabled
}

func (s *postgresTxScope) TxHandle() tx.DialectTxHandle {
	return TxHandle{PgxTx: s.pgxTx}
}

// postgresAuditRecorder persists `tx.AuditEntry` records inside the enclosing `pgx.Tx`.
type postgresAuditRecorder struct {
	queries      sqlcgen.Querier
	auditEnabled bool
}

func (r *postgresAuditRecorder) Record(ctx context.Context, entry tx.AuditEntry) error {
	const op = "postgres.AuditRecorder.Record"
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if !r.auditEnabled {
		return errs.New(errs.ErrPreconditionFailed, op, "audit recording is disabled in WithNoAuditTransaction scope")
	}
	if err := db.ValidateAuditEntry(op, entry); err != nil {
		return err
	}

	cloned := entry.Clone()
	recordID := cloned.ID
	if recordID == uuid.Nil {
		recordID = uuid.New()
	}

	var hintKind authctx.ActorKind
	var hintHandle string
	if id, err := authctx.FromContext(ctx); err == nil && id.ActorID == cloned.ActorID {
		hintKind = id.Kind
		hintHandle = id.Handle
	}
	actorRowID, err := ensurePostgresActorRow(ctx, r.queries, cloned.WorkspaceID, cloned.ActorID, hintKind, hintHandle)
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}

	_, err = r.queries.InsertAuditLog(ctx, sqlcgen.InsertAuditLogParams{
		ID:            recordID,
		WorkspaceID:   cloned.WorkspaceID,
		ActorID:       actorRowID,
		Action:        string(cloned.Action),
		ResourceType:  cloned.ResourceType,
		ResourceID:    cloned.ResourceID,
		OldValuesJson: cloned.OldValues,
		NewValuesJson: cloned.NewValues,
	})
	return db.MapSQLDriverError(op, err)
}

// postgresMigrationRunner executes embedded Goose PostgreSQL migrations via `stdlib.OpenDBFromPool`.
type postgresMigrationRunner struct {
	adapter *Adapter
}

func (m *postgresMigrationRunner) withProvider(ctx context.Context, op string, fn func(*goose.Provider) error) error {
	pool, _, err := m.adapter.ensurePool(ctx, op)
	if err != nil {
		return err
	}
	subFS, err := fs.Sub(dbmigrations.PostgresFS, dbmigrations.PostgresDir)
	if err != nil {
		return fmt.Errorf("%s: sub embedded postgres migrations: %w", op, err)
	}
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, subFS)
	if err != nil {
		return fmt.Errorf("%s: create goose provider: %w", op, err)
	}
	return fn(provider)
}

// Up applies all pending embedded PostgreSQL Goose migrations and returns the resulting version.
func (m *postgresMigrationRunner) Up(ctx context.Context) (int64, error) {
	const op = "postgres.MigrationRunner.Up"
	var currentVer int64
	err := m.withProvider(ctx, op, func(provider *goose.Provider) error {
		if _, err := provider.Up(ctx); err != nil {
			return err
		}
		var vErr error
		currentVer, vErr = provider.GetDBVersion(ctx)
		return vErr
	})
	if err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	return currentVer, nil
}

// Down rolls back the single latest applied PostgreSQL Goose migration and returns the resulting version.
func (m *postgresMigrationRunner) Down(ctx context.Context) (int64, error) {
	const op = "postgres.MigrationRunner.Down"
	var currentVer int64
	err := m.withProvider(ctx, op, func(provider *goose.Provider) error {
		if _, err := provider.Down(ctx); err != nil && !errors.Is(err, goose.ErrNoNextVersion) {
			return err
		}
		var vErr error
		currentVer, vErr = provider.GetDBVersion(ctx)
		return vErr
	})
	if err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	return currentVer, nil
}

// Status inspects applied and pending PostgreSQL Goose migrations without mutating schema state.
func (m *postgresMigrationRunner) Status(ctx context.Context) ([]db.MigrationVersionStatus, error) {
	const op = "postgres.MigrationRunner.Status"
	var out []db.MigrationVersionStatus
	err := m.withProvider(ctx, op, func(provider *goose.Provider) error {
		rawStatuses, err := provider.Status(ctx)
		if err != nil {
			return err
		}
		out = make([]db.MigrationVersionStatus, 0, len(rawStatuses))
		for _, st := range rawStatuses {
			name := ""
			var version int64
			if st.Source != nil {
				version = st.Source.Version
				name = strings.TrimSuffix(filepath.Base(st.Source.Path), filepath.Ext(st.Source.Path))
			}
			var appliedAt time.Time
			isApplied := st.State == goose.StateApplied
			if isApplied && !st.AppliedAt.IsZero() {
				appliedAt = st.AppliedAt.UTC()
			}
			out = append(out, db.MigrationVersionStatus{
				Version:   version,
				Name:      name,
				IsApplied: isApplied,
				AppliedAt: appliedAt,
			})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return out, nil
}
