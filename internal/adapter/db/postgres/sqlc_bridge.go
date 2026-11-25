// File sqlc_bridge.go exposes typed sqlc query contracts bridging `internal/adapter/db/postgres`
// and generated `pgx/v5` query executors in `internal/adapter/db/postgres/sqlcgen`.
package postgres

import (
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/mindmain/flowsk/internal/adapter/db"
	"github.com/mindmain/flowsk/internal/adapter/db/postgres/sqlcgen"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/tx"
)

// SQLCRepositoryContract defines the typed PostgreSQL query surface consumed by
// concrete domain repository implementations (`workspace`, `project`, `objective`,
// `task`, and `audit`).
type SQLCRepositoryContract interface {
	sqlcgen.Querier

	// WithPgxTx returns a transaction-bound sqlc Querier executing against pgxTx.
	WithPgxTx(pgxTx pgx.Tx) sqlcgen.Querier

	// WithTxScope extracts the PostgreSQL `pgx.Tx` from scope.TxHandle() and binds a
	// transaction-scoped sqlc Querier, rejecting nil, cross-dialect, or uninitialized handles.
	WithTxScope(scope tx.Scope) (sqlcgen.Querier, error)
}

// QuerierProvider exposes typed `sqlcgen.DBTX` pool handles and `sqlcgen.Querier`
// constructors on the PostgreSQL adapter for wiring concrete domain repositories.
type QuerierProvider interface {
	// PoolDBTX returns the underlying pooled `sqlcgen.DBTX` handle (typically `*pgxpool.Pool`,
	// or nil before lazy connection pool creation).
	PoolDBTX() sqlcgen.DBTX

	// Queries returns a pooled `sqlcgen.Querier` for read queries outside an explicit transaction.
	Queries() sqlcgen.Querier

	// QueriesForTx binds a new `sqlcgen.Querier` directly to an open `pgx.Tx`.
	QueriesForTx(pgxTx pgx.Tx) sqlcgen.Querier

	// QueriesForScope resolves the active `pgx.Tx` from `tx.Scope` and returns a
	// transaction-isolated `sqlcgen.Querier`.
	QueriesForScope(scope tx.Scope) (sqlcgen.Querier, error)
}

// SQLCBridge implements `QuerierProvider` and `QuerierBinder` over a PostgreSQL
// `sqlcgen.DBTX` connection pool and `sqlcgen.Queries`.
type SQLCBridge struct {
	pool    sqlcgen.DBTX
	queries *sqlcgen.Queries
}

// Compile-time interface assertions ensuring *SQLCBridge satisfies PostgreSQL SQLC bridge contracts.
var (
	_ QuerierProvider = (*SQLCBridge)(nil)
	_ QuerierBinder   = (*SQLCBridge)(nil)
)

// NewSQLCBridge constructs a typed PostgreSQL `SQLCBridge` from a `sqlcgen.DBTX` handle.
func NewSQLCBridge(pool sqlcgen.DBTX) *SQLCBridge {
	bridge := &SQLCBridge{pool: pool}
	if pool != nil {
		bridge.queries = sqlcgen.New(pool)
	}
	return bridge
}

// PoolDBTX returns the configured `sqlcgen.DBTX` pool handle.
func (b *SQLCBridge) PoolDBTX() sqlcgen.DBTX {
	if b == nil {
		return nil
	}
	return b.pool
}

// Queries returns the `sqlcgen.Querier` bound to the connection pool, or nil if uninitialized.
func (b *SQLCBridge) Queries() sqlcgen.Querier {
	if b == nil || b.queries == nil {
		return nil
	}
	return b.queries
}

// QueriesForTx binds pgxTx onto `sqlcgen.Queries`, returning nil when pgxTx is nil.
func (b *SQLCBridge) QueriesForTx(pgxTx pgx.Tx) sqlcgen.Querier {
	if pgxTx == nil {
		return nil
	}
	if b != nil && b.queries != nil {
		return b.queries.WithTx(pgxTx)
	}
	return sqlcgen.New(pgxTx)
}

// BindPgxTx implements `QuerierBinder` by returning a typed `sqlcgen.Querier` bound to pgxTx.
func (b *SQLCBridge) BindPgxTx(pgxTx pgx.Tx) any {
	return b.QueriesForTx(pgxTx)
}

// QueriesForScope extracts the PostgreSQL `TxHandle` from scope and binds a transaction-isolated `sqlcgen.Querier`.
func (b *SQLCBridge) QueriesForScope(scope tx.Scope) (sqlcgen.Querier, error) {
	var base *sqlcgen.Queries
	if b != nil {
		base = b.queries
	}
	return ResolveScopeQuerier(base, scope)
}

// BoundContract wraps `*sqlcgen.Queries` to satisfy `SQLCRepositoryContract` for concrete PostgreSQL repositories.
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

// WithPgxTx returns a transaction-scoped `sqlcgen.Querier` bound to pgxTx.
func (c *BoundContract) WithPgxTx(pgxTx pgx.Tx) sqlcgen.Querier {
	if pgxTx == nil {
		return nil
	}
	if c != nil && c.Queries != nil {
		return c.WithTx(pgxTx)
	}
	return sqlcgen.New(pgxTx)
}

// WithTxScope resolves `tx.Scope` into a transaction-bound PostgreSQL `sqlcgen.Querier`.
func (c *BoundContract) WithTxScope(scope tx.Scope) (sqlcgen.Querier, error) {
	var base *sqlcgen.Queries
	if c != nil {
		base = c.Queries
	}
	return ResolveScopeQuerier(base, scope)
}

// ResolveScopeQuerier validates that scope carries an open PostgreSQL `TxHandle` (`pgx.Tx`)
// and binds it to `sqlcgen.Queries`.
func ResolveScopeQuerier(base *sqlcgen.Queries, scope tx.Scope) (sqlcgen.Querier, error) {
	const op = "postgres.ResolveScopeQuerier"
	if scope == nil {
		return nil, errs.Validation(op, "transaction scope is required", errs.FieldViolation{
			Field:       "scope",
			Description: "tx.Scope must not be nil when binding PostgreSQL sqlc queries",
		})
	}
	rawHandle := scope.TxHandle()
	if rawHandle == nil {
		return nil, errs.New(errs.ErrPreconditionFailed, op, "transaction scope carries a nil dialect handle")
	}
	var pgxTx pgx.Tx
	switch h := rawHandle.(type) {
	case TxHandle:
		pgxTx = h.PgxTx
	case *TxHandle:
		if h != nil {
			pgxTx = h.PgxTx
		}
	default:
		return nil, errs.New(
			errs.ErrPreconditionFailed,
			op,
			fmt.Sprintf("incompatible transaction dialect %q for %q sqlc bridge", rawHandle.TxDialect(), db.DialectPostgres),
		)
	}
	if pgxTx == nil {
		return nil, errs.New(errs.ErrPreconditionFailed, op, "postgres transaction handle holds a nil pgx.Tx")
	}
	if base != nil {
		return base.WithTx(pgxTx), nil
	}
	return sqlcgen.New(pgxTx), nil
}

// SQLCBridge exposes the typed `QuerierProvider` contract on the PostgreSQL `Adapter`.
func (a *Adapter) SQLCBridge() QuerierProvider {
	if a != nil {
		if br := a.currentBridge(); br != nil {
			return br
		}
	}
	return NewSQLCBridge(nil)
}
