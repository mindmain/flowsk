# Database Migrations & SQLC Governance (`db/AGENTS.md`)

## 1. Scope & Stack Reference
Owns dual-engine Goose DDL migrations in `db/migrations/{sqlite,postgres}/` and parameterized SQLC queries in `db/queries/{sqlite,postgres}/*.sql`, configured via `sqlc.yaml` and generating into `internal/adapter/db/{sqlite,postgres}/sqlcgen/`. See [STACK.md](../STACK.md) for Goose v3, SQLC v2, `modernc.org/sqlite`, and `jackc/pgx/v5` mappings.

## 2. Architecture & Style Invariants
- **Dual-Engine Parity (Local SQLite Primary + Optional PostgreSQL)**: Keep `db/migrations/sqlite` and `db/migrations/postgres` (and matching `db/queries/{sqlite,postgres}/*.sql` query signatures) functionally symmetrical so Flowsk runs locally out-of-the-box on SQLite WAL and switches seamlessly to PostgreSQL for multi-server network deployments without domain changes.
- **Reversible DDL & Zero Server-Side Local Paths**: Every Goose migration (`20261115190000_initial_schema.sql`, `20261125130757_project_bindings.sql`, `20261125150000_archived_and_markdown_docs.sql`, `20261125155201_task_external_id.sql`, etc.) must define both `-- +goose Up` and `-- +goose Down` blocks with explicit foreign keys and composite/partial indexes (such as `UNIQUE (project_id, external_id) WHERE external_id IS NOT NULL AND deleted_at IS NULL`); never store host filesystem paths in any database table (`project_bindings.scope_subpath` stores only relative subpaths, and `has_markdown_doc` stores only a boolean flag).
- **Mandatory Ownership & Soft-Delete Filters**: Every tenant-scoped `SELECT`, `UPDATE`, and soft-delete query in `db/queries/{sqlite,postgres}/*.sql` must filter by `workspace_id` and guard active rows with `deleted_at IS NULL`.
- **Direct Mandate — English SQL Comments & Parameterized Queries**: Every `.sql` migration and SQLC query file MUST have an English header comment and clear English `--` comments documenting tables, indexes, and each `-- name:` query; use strict SQLC named/positional parameters (zero dynamic SQL concatenation) and regenerate code exclusively via `task sqlc:gen`.

## 3. Hierarchical `.prompts` Tree & MCP Lifecycle
- Schema- and query-scoped plans reside in `db/.prompts/`; cross-cutting persistence + domain/adapter plans reside in root `.prompts/`.
- Manage all `.prompts` plan creation, task updates, and status transitions exclusively via `ocin-ai` MCP tools; never delete `.md` files in `.prompts`.

## 4. Executive Orders & Verification Commands
1. **Ordine Esecutivo 1: Aggiorna sempre `AGENTS.md` e `STACK.md` ad ogni modifica rilevante di stack, architettura, adapter, comandi o struttura prima di concludere il task.**
2. **Ordine Esecutivo 2: Non cancellare mai file `.md` in `.prompts`; posiziona `.prompts` nel perimetro minimo del lavoro (servizio, dominio padre o root) e gestisci creazione, avanzamento e stato esclusivamente tramite i tool MCP di `ocin-ai`.**
3. **Direct English Commenting Mandate**: Write all SQL identifiers, schema notes, and `-- name:` query documentation comments strictly in English.
4. Verify schema parity, SQLC compilation, generated Go code, and reversible migrations before completing any database change:
   ```sh
   task sqlc:compile
   task sqlc:gen
   task db:migrate:sqlite
   # When PostgreSQL profile is running:
   task db:migrate:postgres
   ```
