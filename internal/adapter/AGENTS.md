# External Dependency Adapters Governance (`internal/adapter/AGENTS.md`)

## 1. Scope & Stack Reference
Owns external infrastructure wrappers in `internal/adapter/{db,codemode,volume,mdstore,webui,localpath,formatter,client}/` for `module github.com/mindmain/flowsk`, including SQLC outputs in `internal/adapter/db/{sqlite,postgres}/sqlcgen/` and embedded Angular SPA assets in `internal/adapter/webui/dist/`. See [STACK.md](../../STACK.md) for driver versions (`modernc.org/sqlite`, `jackc/pgx/v5`, `dop251/goja`, `goose/v3`) and port mappings.

## 2. Architecture & Style Invariants
- **Strict Adapter Encapsulation**: Wrap all SQL drivers, Goose runners, sandboxed JavaScript VMs (`codemode`), shared-volume file locks/snapshots (`volume`), global per-project/per-task Markdown files (`mdstore` under `~/.flowsk/{projects,tasks}/<workspace_id>/<id>.md`), embedded Angular SPA assets (`webui`), client JSON path storage (`localpath`), terminal/agent output serializers (`formatter`), and outbound ConnectRPC network clients (`client`) behind domain/CLI interfaces; translate driver errors (unique constraint, `sql.ErrNoRows`, `pgx.ErrNoRows`, busy lock) into `internal/domain/errs`.
- **Dual-Engine Persistence & Shared Volumes**: `db/sqlite` (`CGO_ENABLED=0` WAL mode + `PRAGMA foreign_keys = ON` + `busy_timeout`, powering both standalone local `~/.flowsk/flowsk.db` and shared Docker `/data/flowsk`) and `db/postgres` (`pgxpool`) must pass identical domain repository contracts and propagate scoped transactions via `internal/domain/tx`.
- **Hermetic Code Mode Sandbox (`codemode`) & Zero Target-Repo Footprint (`localpath`)**: `internal/adapter/codemode` runs `dop251/goja` ES6 JavaScript (`Engine.Execute` and static `ValidateSyntax`) with zero filesystem/network/env access, preemptive timeout cancellation, single-transaction ACID semantics, and staged `DryRun`-safe `.md` document persistence (`stagedMarkdownStore` flushed to `MarkdownStore` only after SQL commit); `internal/adapter/localpath` reads/writes `~/.flowsk/local_paths.json` atomically (`0600` perms, temp-file rename) on the client machine only, inspects local `.git` presence, `git worktree list --porcelain`, and subpath commits (`git log -1`) without writing inside target repositories, and is never wired into server handlers.
- **Terminal-Readable & Token-Frugal Output (`formatter`)**: `formatter` renders workspaces, projects, goals/objectives (with task progress bars), tasks, task queues, digests, full terminal boards (`BoardView`), and audit trails across `human` (aligned terminal tables), `compact` (ultra-low-token agent lines), `tsv`, `jsonl`, `json`, and `markdown`/`md`. Never manually edit `internal/adapter/db/*/sqlcgen/**` and write all adapter comments strictly in English.

## 3. Flowsk Operational Plans & MCP Lifecycle
- Track adapter implementation tasks and Markdown specs in `~/.flowsk/` via the `flowsk` MCP tools (`flowsk_digest`, `flowsk_search`, `flowsk_execute`) without creating plan files inside `internal/adapter/`.

## 4. Executive Orders & Verification Commands
1. **Ordine Esecutivo 1: Aggiorna sempre `AGENTS.md` e `STACK.md` ad ogni modifica rilevante di stack, architettura, adapter, comandi o struttura prima di concludere il task.**
2. **Direct English Commenting Mandate**: Document every non-generated `.go` file header, exported type, field, and method strictly in English.
3. Verify SQLC parity, race-safe SQLite/PostgreSQL/codemode/volume/localpath/formatter tests, and linter checks:
   ```sh
   task sqlc:compile
   go test -race -count=1 ./internal/adapter/...
   task lint
   ```
