// Package dbmigrations embeds the reversible Goose SQL schema migration files
// for both SQLite (`migrations/sqlite/*.sql`) and PostgreSQL
// (`migrations/postgres/*.sql`) directly into the static Flowsk binary so
// standalone CLI commands and `flowsk serve` initialize databases on first run
// without requiring external SQL files on disk.
package dbmigrations

import "embed"

const (
	// SQLiteDir is the relative directory path inside SQLiteFS containing Goose SQLite migrations.
	SQLiteDir = "migrations/sqlite"

	// PostgresDir is the relative directory path inside PostgresFS containing Goose PostgreSQL migrations.
	PostgresDir = "migrations/postgres"
)

// SQLiteFS holds all embedded Goose SQLite `.sql` migration scripts.
//
//go:embed migrations/sqlite/*.sql
var SQLiteFS embed.FS

// PostgresFS holds all embedded Goose PostgreSQL `.sql` migration scripts.
//
//go:embed migrations/postgres/*.sql
var PostgresFS embed.FS
