// Package db_test verifies dialect validation rules on the dialect-agnostic
// persistence contracts in internal/adapter/db.
package db_test

import (
	"testing"

	"github.com/mindmain/flowsk/internal/adapter/db"
)

// TestDialectValid verifies that only supported SQLite and PostgreSQL dialect
// identifiers pass validation.
func TestDialectValid(t *testing.T) {
	t.Parallel()

	if !db.DialectSQLite.Valid() {
		t.Fatal("expected DialectSQLite to be valid")
	}
	if !db.DialectPostgres.Valid() {
		t.Fatal("expected DialectPostgres to be valid")
	}
	if db.Dialect("mysql").Valid() {
		t.Fatal("expected unsupported dialect 'mysql' to be invalid")
	}
	if db.Dialect("").Valid() {
		t.Fatal("expected empty dialect to be invalid")
	}
}
