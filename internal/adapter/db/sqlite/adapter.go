// Package sqlite provides the pure-Go `modernc.org/sqlite` database adapter,
// WAL connection pools, Goose migration runner, serialized ACID transaction
// manager, and sqlc-backed domain repositories for local workstation and shared
// Docker volume deployments of Flowsk.
//
// SQLite Concurrency & Durability Architecture:
//   - Enables Write-Ahead Logging (`PRAGMA journal_mode = WAL`) so concurrent reader
//     connections never block behind active writers.
//   - Enforces `PRAGMA foreign_keys = ON`, `PRAGMA synchronous = NORMAL` (or `FULL`),
//     and `PRAGMA busy_timeout` on every pooled connection via driver DSN parameters.
//   - Separates writer serialization (`MaxWriteOpenConns = 1`, `_txlock=immediate`, and
//     a context-aware writer semaphore inside `tx.Manager`) from concurrent read-pool
//     connections (`MaxReadOpenConns`).
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	dbmigrations "github.com/mindmain/flowsk/db"
	"github.com/mindmain/flowsk/internal/adapter/db"
	"github.com/mindmain/flowsk/internal/adapter/db/sqlite/sqlcgen"
	"github.com/mindmain/flowsk/internal/domain/audit"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/objective"
	"github.com/mindmain/flowsk/internal/domain/project"
	"github.com/mindmain/flowsk/internal/domain/task"
	"github.com/mindmain/flowsk/internal/domain/tx"
	"github.com/mindmain/flowsk/internal/domain/workspace"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// SynchronousMode configures the SQLite `PRAGMA synchronous` durability level.
type SynchronousMode string

const (
	// SynchronousNormal balances crash safety and high throughput when paired with WAL mode.
	SynchronousNormal SynchronousMode = "NORMAL"

	// SynchronousFull forces extra fsync barriers on every WAL commit for maximum durability.
	SynchronousFull SynchronousMode = "FULL"
)

// Config defines connection, WAL pragma, busy-timeout, and connection-pool parameters
// for opening a Flowsk SQLite database file on local disk or a mounted Docker volume.
type Config struct {
	// FilePath is the filesystem path to the SQLite database file
	// (for example, `/data/flowsk/flowsk.db` inside a shared Docker volume or
	// `~/.local/share/flowsk/flowsk.db` in standalone local mode).
	FilePath string

	// EnableWAL enables `PRAGMA journal_mode = WAL` on connection initialization (recommended: true).
	EnableWAL bool

	// ForeignKeys enables `PRAGMA foreign_keys = ON` on every pooled connection (mandatory: true).
	ForeignKeys bool

	// BusyTimeout sets `PRAGMA busy_timeout` waiting duration before returning SQLITE_BUSY.
	BusyTimeout time.Duration

	// Synchronous configures `PRAGMA synchronous` (`NORMAL` or `FULL`).
	Synchronous SynchronousMode

	// WALAutoCheckpointPages sets `PRAGMA wal_autocheckpoint` page threshold to bound WAL file growth
	// on shared Docker volumes.
	WALAutoCheckpointPages int

	// MaxReadOpenConns bounds concurrent reader connections in the read `*sql.DB` pool.
	MaxReadOpenConns int
}

// QuerierBinder abstracts binding a `*sql.Tx` to sqlc-generated SQLite query structs.
type QuerierBinder interface {
	// BindSQLTx returns a transaction-scoped query executor bound to sqlTx.
	BindSQLTx(sqlTx *sql.Tx) any
}

// TxHandle wraps an open SQLite `*sql.Tx` to satisfy `tx.DialectTxHandle` without
// leaking `database/sql` into `internal/domain`.
type TxHandle struct {
	// SQLTx is the active SQLite transaction handle bound to the single writer connection.
	SQLTx *sql.Tx
}

// TxDialect identifies this handle as belonging to the SQLite adapter (`"sqlite"`).
func (h TxHandle) TxDialect() string {
	return string(db.DialectSQLite)
}

// Adapter is the production pure-Go SQLite implementation of `db.DatabaseAdapter`.
type Adapter struct {
	cfg           Config
	inMemory      bool
	writeDB       *sql.DB
	readDB        *sql.DB
	bridge        *SQLCBridge
	writeSem      chan struct{}
	closeOnce     sync.Once
	closeErr      error
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

// isInMemorySQLitePath reports whether path targets an ephemeral SQLite in-memory database.
func isInMemorySQLitePath(path string) bool {
	trimmed := strings.TrimSpace(path)
	return trimmed == ":memory:" ||
		strings.HasPrefix(trimmed, "file::memory:") ||
		strings.Contains(trimmed, "mode=memory")
}

// buildSQLiteDSN constructs a `modernc.org/sqlite` connection string with per-connection PRAGMAs.
func buildSQLiteDSN(cfg Config, inMemory, isWriter bool) string {
	params := url.Values{}

	busyMS := int64(10000)
	if cfg.BusyTimeout > 0 {
		busyMS = cfg.BusyTimeout.Milliseconds()
	}
	params.Set("_busy_timeout", strconv.FormatInt(busyMS, 10))

	if cfg.ForeignKeys {
		params.Set("_foreign_keys", "on")
	} else {
		params.Set("_foreign_keys", "off")
	}

	if !inMemory && cfg.EnableWAL {
		params.Set("_journal_mode", "WAL")
	}

	syncMode := strings.ToUpper(strings.TrimSpace(string(cfg.Synchronous)))
	switch SynchronousMode(syncMode) {
	case SynchronousFull:
		params.Set("_synchronous", "FULL")
	case SynchronousNormal, "":
		params.Set("_synchronous", "NORMAL")
	default:
		params.Set("_synchronous", "NORMAL")
	}

	if isWriter {
		params.Set("_txlock", "immediate")
	}
	if !inMemory && cfg.WALAutoCheckpointPages > 0 {
		params.Add("_pragma", fmt.Sprintf("wal_autocheckpoint(%d)", cfg.WALAutoCheckpointPages))
	}

	basePath := strings.TrimSpace(cfg.FilePath)
	sep := "?"
	if strings.Contains(basePath, "?") {
		sep = "&"
	}
	return basePath + sep + params.Encode()
}

// NewAdapter opens dedicated single-writer and concurrent multi-reader `*sql.DB`
// pools via `modernc.org/sqlite`, auto-creates missing parent directories for
// file-backed paths, configures WAL + foreign_keys + busy_timeout PRAGMAs, and
// wires Goose migrations, `tx.Manager`, and all 5 sqlc domain repositories.
func NewAdapter(cfg Config) (*Adapter, error) {
	const op = "sqlite.NewAdapter"
	if strings.TrimSpace(cfg.FilePath) == "" {
		return nil, errs.Validation(op, "sqlite file path is required", errs.FieldViolation{
			Field:       "file_path",
			Description: "sqlite database FilePath must not be empty",
		})
	}

	inMem := isInMemorySQLitePath(cfg.FilePath)
	if !inMem {
		cleanedPath := filepath.Clean(cfg.FilePath)
		cfg.FilePath = cleanedPath
		parentDir := filepath.Dir(cleanedPath)
		if parentDir != "" && parentDir != "." {
			if err := os.MkdirAll(parentDir, 0o750); err != nil {
				return nil, fmt.Errorf("%s: create parent directory %q: %w", op, parentDir, err)
			}
		}
	}

	writeDSN := buildSQLiteDSN(cfg, inMem, true)
	writeDB, err := sql.Open("sqlite", writeDSN)
	if err != nil {
		return nil, fmt.Errorf("%s: open writer pool: %w", op, err)
	}
	writeDB.SetMaxOpenConns(1)
	writeDB.SetMaxIdleConns(1)
	writeDB.SetConnMaxLifetime(0)

	var readDB *sql.DB
	if inMem {
		readDB = writeDB
	} else {
		readDSN := buildSQLiteDSN(cfg, false, false)
		readDB, err = sql.Open("sqlite", readDSN)
		if err != nil {
			_ = writeDB.Close()
			return nil, fmt.Errorf("%s: open reader pool: %w", op, err)
		}
		maxRead := cfg.MaxReadOpenConns
		if maxRead <= 0 {
			maxRead = max(4, runtime.NumCPU())
		}
		readDB.SetMaxOpenConns(maxRead)
		readDB.SetMaxIdleConns(maxRead)
		readDB.SetConnMaxLifetime(0)
	}

	sem := make(chan struct{}, 1)
	sem <- struct{}{}

	bridge := NewSQLCBridge(readDB, writeDB)
	a := &Adapter{
		cfg:      cfg,
		inMemory: inMem,
		writeDB:  writeDB,
		readDB:   readDB,
		bridge:   bridge,
		writeSem: sem,
	}
	a.txMgr = &sqliteTxManager{adapter: a}
	a.migrations = &sqliteMigrationRunner{adapter: a}
	a.workspaceRepo = &WorkspaceRepository{adapter: a}
	a.projectRepo = &ProjectRepository{adapter: a}
	a.objectiveRepo = &ObjectiveRepository{adapter: a}
	a.taskRepo = &TaskRepository{adapter: a}
	a.auditRepo = &AuditRepository{adapter: a}
	return a, nil
}

// Config returns a copy of the adapter's immutable configuration.
func (a *Adapter) Config() Config {
	return a.cfg
}

// Dialect returns db.DialectSQLite.
func (a *Adapter) Dialect() db.Dialect {
	return db.DialectSQLite
}

// acquireWriteSlot serializes single-writer transactions and DDL operations while honoring ctx cancellation.
func (a *Adapter) acquireWriteSlot(ctx context.Context, op string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	select {
	case <-a.writeSem:
		var once sync.Once
		return func() {
			once.Do(func() {
				a.writeSem <- struct{}{}
			})
		}, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("%s: %w", op, ctx.Err())
	}
}

// Ping verifies live connectivity and read/write health against the SQLite pools.
func (a *Adapter) Ping(ctx context.Context) error {
	const op = "sqlite.Adapter.Ping"
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	release, err := a.acquireWriteSlot(ctx, op)
	if err != nil {
		return err
	}
	defer release()

	if err := a.writeDB.PingContext(ctx); err != nil {
		return fmt.Errorf("%s: writer ping: %w", op, err)
	}
	if a.readDB != a.writeDB {
		if err := a.readDB.PingContext(ctx); err != nil {
			return fmt.Errorf("%s: reader ping: %w", op, err)
		}
	}
	return nil
}

// Close checkpoints WAL frames when file-backed and closes both reader and writer pools.
func (a *Adapter) Close() error {
	a.closeOnce.Do(func() {
		if a.writeDB != nil && !a.inMemory && a.cfg.EnableWAL {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = a.writeDB.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)")
			cancel()
		}
		var errsList []error
		if a.readDB != nil && a.readDB != a.writeDB {
			if err := a.readDB.Close(); err != nil {
				errsList = append(errsList, err)
			}
		}
		if a.writeDB != nil {
			if err := a.writeDB.Close(); err != nil {
				errsList = append(errsList, err)
			}
		}
		a.closeErr = errors.Join(errsList...)
	})
	return a.closeErr
}

// TxManager returns the ACID SQLite transaction Manager.
func (a *Adapter) TxManager() tx.Manager {
	return a.txMgr
}

// WorkspaceRepository returns the SQLite workspace.Repository implementation.
func (a *Adapter) WorkspaceRepository() workspace.Repository {
	return a.workspaceRepo
}

// ProjectRepository returns the SQLite project.Repository implementation.
func (a *Adapter) ProjectRepository() project.Repository {
	return a.projectRepo
}

// ObjectiveRepository returns the SQLite objective.Repository implementation.
func (a *Adapter) ObjectiveRepository() objective.Repository {
	return a.objectiveRepo
}

// TaskRepository returns the SQLite task.Repository implementation.
func (a *Adapter) TaskRepository() task.Repository {
	return a.taskRepo
}

// AuditRepository returns the SQLite audit.Repository implementation.
func (a *Adapter) AuditRepository() audit.Repository {
	return a.auditRepo
}

// Migrations returns the Goose SQLite MigrationRunner backed by embedded SQL files.
func (a *Adapter) Migrations() db.MigrationRunner {
	return a.migrations
}

// sqliteTxManager coordinates serialized `BEGIN IMMEDIATE` SQLite ACID transactions.
type sqliteTxManager struct {
	adapter *Adapter
}

// WithTransaction executes fn inside a serialized ACID SQLite transaction with atomic audit support.
func (m *sqliteTxManager) WithTransaction(ctx context.Context, fn tx.TxFunc) error {
	return m.runTx(ctx, "sqlite.TxManager.WithTransaction", true, fn)
}

// WithNoAuditTransaction executes fn inside a serialized ACID SQLite transaction with auditing disabled.
func (m *sqliteTxManager) WithNoAuditTransaction(ctx context.Context, fn tx.TxFunc) error {
	return m.runTx(ctx, "sqlite.TxManager.WithNoAuditTransaction", false, fn)
}

func (m *sqliteTxManager) runTx(ctx context.Context, op string, auditEnabled bool, fn tx.TxFunc) (retErr error) {
	if fn == nil {
		return errs.Validation(op, "transaction callback is required", errs.FieldViolation{
			Field:       "fn",
			Description: "tx.TxFunc callback must not be nil",
		})
	}
	release, err := m.adapter.acquireWriteSlot(ctx, op)
	if err != nil {
		return err
	}
	defer release()

	sqlTx, err := m.adapter.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: begin tx: %w", op, err)
	}

	committed := false
	defer func() {
		if r := recover(); r != nil {
			_ = sqlTx.Rollback()
			panic(r)
		}
		if !committed {
			_ = sqlTx.Rollback()
		}
	}()

	queries := m.adapter.bridge.QueriesForTx(sqlTx)
	scope := &sqliteTxScope{
		sqlTx:        sqlTx,
		auditEnabled: auditEnabled,
		recorder: &sqliteAuditRecorder{
			queries:      queries,
			auditEnabled: auditEnabled,
		},
	}

	if err := fn(ctx, scope); err != nil {
		return err
	}

	if err := sqlTx.Commit(); err != nil {
		return fmt.Errorf("%s: commit tx: %w", op, db.MapSQLDriverError(op, err))
	}
	committed = true
	return nil
}

// sqliteTxScope implements `tx.Scope` over an open SQLite `*sql.Tx`.
type sqliteTxScope struct {
	sqlTx        *sql.Tx
	auditEnabled bool
	recorder     tx.AuditRecorder
}

func (s *sqliteTxScope) Audit() tx.AuditRecorder {
	return s.recorder
}

func (s *sqliteTxScope) IsAuditEnabled() bool {
	return s.auditEnabled
}

func (s *sqliteTxScope) TxHandle() tx.DialectTxHandle {
	return TxHandle{SQLTx: s.sqlTx}
}

// sqliteAuditRecorder persists `tx.AuditEntry` rows inside the enclosing SQLite transaction.
type sqliteAuditRecorder struct {
	queries      sqlcgen.Querier
	auditEnabled bool
}

func (r *sqliteAuditRecorder) Record(ctx context.Context, entry tx.AuditEntry) error {
	const op = "sqlite.AuditRecorder.Record"
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
	actorRowID, err := ensureSQLiteActorRow(ctx, r.queries, cloned.WorkspaceID, cloned.ActorID, hintKind, hintHandle)
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}

	var oldStr, newStr *string
	if len(cloned.OldValues) > 0 {
		s := string(cloned.OldValues)
		oldStr = &s
	}
	if len(cloned.NewValues) > 0 {
		s := string(cloned.NewValues)
		newStr = &s
	}

	_, err = r.queries.InsertAuditLog(ctx, sqlcgen.InsertAuditLogParams{
		ID:            recordID.String(),
		WorkspaceID:   cloned.WorkspaceID.String(),
		ActorID:       actorRowID,
		Action:        string(cloned.Action),
		ResourceType:  cloned.ResourceType,
		ResourceID:    cloned.ResourceID,
		OldValuesJson: oldStr,
		NewValuesJson: newStr,
	})
	return db.MapSQLDriverError(op, err)
}

// sqliteMigrationRunner executes embedded Goose SQLite schema migrations without global state.
type sqliteMigrationRunner struct {
	adapter *Adapter
}

func (m *sqliteMigrationRunner) newProvider() (*goose.Provider, error) {
	subFS, err := fs.Sub(dbmigrations.SQLiteFS, dbmigrations.SQLiteDir)
	if err != nil {
		return nil, fmt.Errorf("sub embedded sqlite migrations: %w", err)
	}
	return goose.NewProvider(goose.DialectSQLite3, m.adapter.writeDB, subFS)
}

// Up applies all pending embedded SQLite Goose migrations and returns the resulting version.
func (m *sqliteMigrationRunner) Up(ctx context.Context) (int64, error) {
	const op = "sqlite.MigrationRunner.Up"
	release, err := m.adapter.acquireWriteSlot(ctx, op)
	if err != nil {
		return 0, err
	}
	defer release()

	provider, err := m.newProvider()
	if err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	ver, err := provider.GetDBVersion(ctx)
	if err != nil {
		return 0, fmt.Errorf("%s: get version: %w", op, err)
	}
	return ver, nil
}

// Down rolls back the single latest applied SQLite Goose migration and returns the resulting version.
func (m *sqliteMigrationRunner) Down(ctx context.Context) (int64, error) {
	const op = "sqlite.MigrationRunner.Down"
	release, err := m.adapter.acquireWriteSlot(ctx, op)
	if err != nil {
		return 0, err
	}
	defer release()

	provider, err := m.newProvider()
	if err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	if _, err := provider.Down(ctx); err != nil && !errors.Is(err, goose.ErrNoNextVersion) {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	ver, err := provider.GetDBVersion(ctx)
	if err != nil {
		return 0, fmt.Errorf("%s: get version: %w", op, err)
	}
	return ver, nil
}

// Status inspects applied and pending SQLite Goose migrations without mutating schema state.
func (m *sqliteMigrationRunner) Status(ctx context.Context) ([]db.MigrationVersionStatus, error) {
	const op = "sqlite.MigrationRunner.Status"
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	provider, err := m.newProvider()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	rawStatuses, err := provider.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	out := make([]db.MigrationVersionStatus, 0, len(rawStatuses))
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
	return out, nil
}
