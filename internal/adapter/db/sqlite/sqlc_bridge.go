// File sqlc_bridge.go exposes typed sqlc query contracts bridging `internal/adapter/db/sqlite`
// and generated query executors in `internal/adapter/db/sqlite/sqlcgen`.
package sqlite

import (
	"database/sql"
	"fmt"

	"github.com/mindmain/flowsk/internal/adapter/db"
	"github.com/mindmain/flowsk/internal/adapter/db/sqlite/sqlcgen"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/tx"
)

// SQLCRepositoryContract defines the typed SQLite query surface consumed by
// concrete domain repository implementations (`workspace`, `project`, `objective`,
// `task`, and `audit`).
type SQLCRepositoryContract interface {
	sqlcgen.Querier

	// WithSQLTx returns a transaction-bound sqlc Querier executing against sqlTx.
	WithSQLTx(sqlTx *sql.Tx) sqlcgen.Querier

	// WithTxScope extracts the SQLite `*sql.Tx` from scope.TxHandle() and binds a
	// transaction-scoped sqlc Querier, rejecting nil, cross-dialect, or uninitialized handles.
	WithTxScope(scope tx.Scope) (sqlcgen.Querier, error)
}

// QuerierProvider exposes typed `sqlcgen.DBTX` connection handles and `sqlcgen.Querier`
// constructors on the SQLite adapter for wiring concrete domain repositories.
type QuerierProvider interface {
	// ReadDBTX returns the concurrent reader `sqlcgen.DBTX` pool handle.
	ReadDBTX() sqlcgen.DBTX

	// WriteDBTX returns the serialized single-writer `sqlcgen.DBTX` pool handle.
	WriteDBTX() sqlcgen.DBTX

	// ReadQueries constructs a read-pool `sqlcgen.Querier` for snapshot SELECT queries.
	ReadQueries() sqlcgen.Querier

	// WriteQueries constructs a writer-pool `sqlcgen.Querier` for mutations.
	WriteQueries() sqlcgen.Querier

	// QueriesForTx binds a new `sqlcgen.Querier` directly to an open `*sql.Tx`.
	QueriesForTx(sqlTx *sql.Tx) sqlcgen.Querier

	// QueriesForScope resolves the active `*sql.Tx` from `tx.Scope` and returns a
	// transaction-isolated `sqlcgen.Querier`.
	QueriesForScope(scope tx.Scope) (sqlcgen.Querier, error)
}

// SQLCBridge implements `QuerierProvider` and `QuerierBinder` over SQLite read/write
// `sqlcgen.DBTX` handles and `sqlcgen.Queries`.
type SQLCBridge struct {
	readDB       sqlcgen.DBTX
	writeDB      sqlcgen.DBTX
	readQueries  *sqlcgen.Queries
	writeQueries *sqlcgen.Queries
}

// Compile-time interface assertions ensuring *SQLCBridge satisfies SQLite SQLC bridge contracts.
var (
	_ QuerierProvider = (*SQLCBridge)(nil)
	_ QuerierBinder   = (*SQLCBridge)(nil)
)

// NewSQLCBridge constructs a typed SQLite `SQLCBridge` from reader and writer `sqlcgen.DBTX` handles.
func NewSQLCBridge(readDB, writeDB sqlcgen.DBTX) *SQLCBridge {
	if writeDB == nil {
		writeDB = readDB
	}
	bridge := &SQLCBridge{
		readDB:  readDB,
		writeDB: writeDB,
	}
	if readDB != nil {
		bridge.readQueries = sqlcgen.New(readDB)
	}
	if writeDB != nil {
		bridge.writeQueries = sqlcgen.New(writeDB)
	}
	return bridge
}

// ReadDBTX returns the configured reader `sqlcgen.DBTX` handle.
func (b *SQLCBridge) ReadDBTX() sqlcgen.DBTX {
	if b == nil {
		return nil
	}
	return b.readDB
}

// WriteDBTX returns the configured single-writer `sqlcgen.DBTX` handle.
func (b *SQLCBridge) WriteDBTX() sqlcgen.DBTX {
	if b == nil {
		return nil
	}
	return b.writeDB
}

// ReadQueries returns the `sqlcgen.Querier` bound to the reader pool, or nil if uninitialized.
func (b *SQLCBridge) ReadQueries() sqlcgen.Querier {
	if b == nil || b.readQueries == nil {
		return nil
	}
	return b.readQueries
}

// WriteQueries returns the `sqlcgen.Querier` bound to the writer pool, or nil if uninitialized.
func (b *SQLCBridge) WriteQueries() sqlcgen.Querier {
	if b == nil || b.writeQueries == nil {
		return nil
	}
	return b.writeQueries
}

// QueriesForTx binds sqlTx onto `sqlcgen.Queries`, returning nil when sqlTx is nil.
func (b *SQLCBridge) QueriesForTx(sqlTx *sql.Tx) sqlcgen.Querier {
	if sqlTx == nil {
		return nil
	}
	if b != nil && b.writeQueries != nil {
		return b.writeQueries.WithTx(sqlTx)
	}
	return sqlcgen.New(sqlTx)
}

// BindSQLTx implements `QuerierBinder` by returning a typed `sqlcgen.Querier` bound to sqlTx.
func (b *SQLCBridge) BindSQLTx(sqlTx *sql.Tx) any {
	return b.QueriesForTx(sqlTx)
}

// QueriesForScope extracts the SQLite `TxHandle` from scope and binds a transaction-isolated `sqlcgen.Querier`.
func (b *SQLCBridge) QueriesForScope(scope tx.Scope) (sqlcgen.Querier, error) {
	var base *sqlcgen.Queries
	if b != nil {
		base = b.writeQueries
	}
	return ResolveScopeQuerier(base, scope)
}

// BoundContract wraps `*sqlcgen.Queries` to satisfy `SQLCRepositoryContract` for concrete SQLite domain repositories.
type BoundContract struct {
	*sqlcgen.Queries
}

// Compile-time assertion ensuring *BoundContract satisfies SQLCRepositoryContract.
var _ SQLCRepositoryContract = (*BoundContract)(nil)

// NewBoundContract wraps dbtx in a `BoundContract` satisfying `SQLCRepositoryContract`.
func NewBoundContract(dbtx sqlcgen.DBTX) *BoundContract {
	if dbtx == nil {
		return &BoundContract{}
	}
	return &BoundContract{Queries: sqlcgen.New(dbtx)}
}

// WithSQLTx returns a transaction-scoped `sqlcgen.Querier` bound to sqlTx.
func (c *BoundContract) WithSQLTx(sqlTx *sql.Tx) sqlcgen.Querier {
	if sqlTx == nil {
		return nil
	}
	if c != nil && c.Queries != nil {
		return c.WithTx(sqlTx)
	}
	return sqlcgen.New(sqlTx)
}

// WithTxScope resolves `tx.Scope` into a transaction-bound SQLite `sqlcgen.Querier`.
func (c *BoundContract) WithTxScope(scope tx.Scope) (sqlcgen.Querier, error) {
	var base *sqlcgen.Queries
	if c != nil {
		base = c.Queries
	}
	return ResolveScopeQuerier(base, scope)
}

// ResolveScopeQuerier validates that scope carries an open SQLite `TxHandle` (`*sql.Tx`)
// and binds it to `sqlcgen.Queries`.
func ResolveScopeQuerier(base *sqlcgen.Queries, scope tx.Scope) (sqlcgen.Querier, error) {
	const op = "sqlite.ResolveScopeQuerier"
	if scope == nil {
		return nil, errs.Validation(op, "transaction scope is required", errs.FieldViolation{
			Field:       "scope",
			Description: "tx.Scope must not be nil when binding SQLite sqlc queries",
		})
	}
	rawHandle := scope.TxHandle()
	if rawHandle == nil {
		return nil, errs.New(errs.ErrPreconditionFailed, op, "transaction scope carries a nil dialect handle")
	}
	var sqlTx *sql.Tx
	switch h := rawHandle.(type) {
	case TxHandle:
		sqlTx = h.SQLTx
	case *TxHandle:
		if h != nil {
			sqlTx = h.SQLTx
		}
	default:
		return nil, errs.New(
			errs.ErrPreconditionFailed,
			op,
			fmt.Sprintf("incompatible transaction dialect %q for %q sqlc bridge", rawHandle.TxDialect(), db.DialectSQLite),
		)
	}
	if sqlTx == nil {
		return nil, errs.New(errs.ErrPreconditionFailed, op, "sqlite transaction handle holds a nil *sql.Tx")
	}
	if base != nil {
		return base.WithTx(sqlTx), nil
	}
	return sqlcgen.New(sqlTx), nil
}

// SQLCBridge exposes the active typed `QuerierProvider` contract on the SQLite `Adapter`.
func (a *Adapter) SQLCBridge() QuerierProvider {
	if a != nil && a.bridge != nil {
		return a.bridge
	}
	return NewSQLCBridge(nil, nil)
}
