# Flowsk Technology Stack & Directory Map (`STACK.md`)

Single source of truth mapping every language, execution topology, upper transport protocol, pure core domain package, external dependency adapter, contract generator, and verification tool in `module github.com/mindmain/flowsk`.

## 1. Runtime, Usage Modes & Entrypoint

| Component | Technology / Package | Exact Path |
| :--- | :--- | :--- |
| Language & Module | Go `1.26.0` (`GOTOOLCHAIN=go1.26.11`, `CGO_ENABLED=0` static pure-Go build) | `go.mod`, `Taskfile.yml` |
| Unified Binary Entrypoint | `flowsk` CLI (local embedded `~/.flowsk/flowsk.db` default + remote ConnectRPC client), `flowsk install` & `flowsk uninstall` (Lip Gloss + Bubble Tea 3-stage Skill + MCP wizard defaulting to Universal `~/.agents`), `flowsk doctor`, `flowsk update`, `flowsk ui` / `flowsk web` (embedded Angular SPA), `flowsk serve` (`--ui`), `flowsk mcp serve` (Cloudflare Code Mode MCP) | `cmd/flowsk/main.go`, `internal/app/`, `internal/version/` |
| Embedded Minimalist Web UI | Angular `21.1.x` (standalone `OnPush` + strict templates) + Tailwind CSS `v4.1` (`@tailwindcss/postcss`) + `@ngrx/signals` (`21.0.x` SignalStore) + `@angular/cdk/drag-drop` (`21.1.x`) + TypeScript `~5.9.2`, real-time SSE `EventSource` stream (`FlowskStreamService`), structured **Goals & Roadmap** view (`GoalsViewComponent`), built via `pnpm@12.21.0` into `embed.FS` | `web/`, `internal/adapter/webui/dist/` |
| Embedded AI Agent Skill & MCP Installer | `//go:embed embed/flowsk/SKILL.md` + SHA-256 digest & non-destructive JSON/TOML Skill + MCP install/uninstall (`agents` `~/.agents/mcp.json`, `claude`, `codex`, `antigravity`, `gemini`, `cursor`) | `internal/skill/`, `internal/skill/embed/flowsk/SKILL.md` |
| Typed Runtime Configuration | `github.com/spf13/viper` (`v1.21.0`, `FLOWSK_*` env + `~/.flowsk/config.yaml` + flags, DSN/token redaction) | `internal/config/` |
| Container & Multi-Profile Compose | 3-stage `node:22-alpine` + `golang:1.26-alpine` + non-root `alpine:3.21` (`10001:10001`, `/data/flowsk` volume) + optional `postgres:17-alpine` profile | `Dockerfile`, `docker-compose.yml`, `.env.example` |

## 2. Contracts, Migrations & Generated Stubs

| Component | Technology / Package | Exact Path |
| :--- | :--- | :--- |
| RPC & Validation Contracts | Protobuf v3 + `buf.build/bufbuild/protovalidate` (100% English `STANDARD` + `COMMENTS` lint enforced) | `proto/flowsk/{workspace,project,objective,task,language,audit}/v1/*.proto` |
| Buf Workspace & Codegen Config | Buf v2 (`protoc-gen-go`, `protoc-gen-connect-go`) | `buf.yaml`, `buf.gen.yaml`, `buf.lock` |
| Generated Go Proto & Connect Stubs | `google.golang.org/protobuf` (`v1.36.12`), `connectrpc.com/connect` (`v1.21.1`) | `gen/go/flowsk/{workspace,project,objective,task,language,audit}/v1/` |
| Reversible Embedded DDL Schemas | Goose v3 (`github.com/pressly/goose/v3` `v3.30.1`, `//go:embed` in `db/embed.go`): initial schema + `project_bindings` DAG + `has_markdown_doc` & archived workflow support + project-unique `tasks.external_id` | `db/embed.go`, `db/migrations/{sqlite,postgres}/{20261115190000_initial_schema,20261125130757_project_bindings,20261125150000_archived_and_markdown_docs,20261125155201_task_external_id}.sql` |
| Parameterized SQL Queries | SQLC v2 (`sqlc.yaml` dual-engine `sqlite` + `postgresql`: `workspaces`, `actors`, `projects`, `objectives`, `tasks`, `audit_logs`, `dsl_batches`) | `db/queries/{sqlite,postgres}/*.sql` |

## 3. Upper Transport Layer (Strictly Isolated from Domain)

| Protocol / Surface | Technology / Package | Exact Path |
| :--- | :--- | :--- |
| ConnectRPC & Real-Time SSE Stream (`HTTP/1.1`, `h2c` `HTTP/2`, gRPC-compatible) | `connectrpc.com/connect` (`v1.21.1`), `AuthContextInterceptor`, `ProtoValidator`, sanitized `MapDomainError`, Go-channel `EventBroker` + cross-process audit relay (`GET /api/events`), `/healthz`, and 6 bounded-context handlers | `internal/transport/connectrpc/`, `internal/transport/connectrpc/{workspace,project,objective,task,language,audit}handler/` |
| Model Context Protocol (`stdio`, Streamable `HTTP`) | Pure-Go JSON-RPC 2.0 Cloudflare Code Mode MCP server (`flowsk_search`, `flowsk_execute` ES6 JS, `flowsk_language_guide`, `flowsk_digest`, `flowsk://language/guide`) | `internal/transport/mcp/` |
| Command-Line Interface (`CLI`) | `github.com/spf13/cobra` (`v1.10.2`) + `github.com/spf13/viper` (`v1.21.0`) + `github.com/charmbracelet/lipgloss` (`v1.1.0`) + `github.com/charmbracelet/bubbletea` (`v1.3.10`): `init`, `install`, `uninstall`, `doctor`, `update`, `board`/`status` (`--all` + yellow orphan warning), `ui`/`web`, `prune`, `search`/`find`/`s`, `code`/`exec`, `serve` (`--ui`), `mcp` (`serve`/`tools`/`guide`), `workspace`/`ws` (`actor`), `project` (`link`/`unlink`/`where`/`bind`/`unbind`/`bindings`/`state`/`label`/`doc`), `objective`/`goal`, `task`/`issue` (`--external-id`, `next`/`digest`/`claim`/`release`/`move`/`block`/`unblock`/`comment`/`archive`/`purge-archived`/`doc`), `audit`, `db`, `config` | `internal/cli/` |

## 4. Pure Core Domain & Search / SDK Catalog (Zero Transport / Driver Imports)

| Package | Responsibility | Exact Path |
| :--- | :--- | :--- |
| Domain Errors | Canonical transport-agnostic error sentinels, violation descriptors & safe wrappers | `internal/domain/errs/` |
| Workspace Auth Context | Typed caller `Identity` & `RequireWorkspaceOwnership` context guards (`github.com/google/uuid` `v1.6.0`) | `internal/domain/authctx/` |
| Transaction Abstraction | ACID unit-of-work contract (`WithTransaction` / `WithNoAuditTransaction`) & atomic `AuditRecorder` decoupled from SQL drivers | `internal/domain/tx/` |
| Workspace Domain | Multi-tenant workspace & actor membership entities, `ToProto()`, `Service`, local `Repository` | `internal/domain/workspace/` |
| Project Domain | Logical project lifecycle, Markdown doc port & acyclic `project_bindings` sub-project DAG (zero host paths), `ToProto()`, `Service`, local `Repository` | `internal/domain/project/` |
| Objective Domain | Strategic goal, milestone, and cycle state machine, `ToProto()`, `Service`, local `Repository` | `internal/domain/objective/` |
| Task Domain | Hierarchical task execution, project-unique `ExternalID`, DAG dependencies, `ClaimLease` locks, orphan hygiene & digests, `ToProto()`, `Service`, local `Repository` | `internal/domain/task/` |
| Audit Domain | Append-only atomic mutation audit log, `ToProto()`, `Service`, local `Repository` | `internal/domain/audit/` |
| Search Parser & Code Mode SDK Catalog | GitHub/Linear structured search parser (`ParseSearchQuery`), `flowsk.*` TypeScript `.d.ts` specification (`CodeModeTypeScriptSpec`), progressive SDK method catalog (`SearchCodeModeSDK`), and `GuideProvider` ES6 recipes | `internal/language/` |

## 5. External Dependency Adapters

| Adapter | Technology / Underlying Dependency | Exact Path |
| :--- | :--- | :--- |
| Shared DB Port & Metadata Codec | `DatabaseAdapter` port, lossless domain-to-SQL metadata envelopes, workflow state seeds & SQL driver error mapper | `internal/adapter/db/` |
| SQLite Persistence (Local Default & Shared Volume) | Pure-Go `modernc.org/sqlite` (`v1.63.0`), WAL, Goose v3 runner, SQLC (`~/.flowsk/flowsk.db` or `/data/flowsk/flowsk.db`) | `internal/adapter/db/sqlite/`, `internal/adapter/db/sqlite/sqlcgen/` |
| PostgreSQL Persistence (Network / Office Swap) | `github.com/jackc/pgx/v5` (`v5.12.0` `pgxpool`), Goose v3 runner, SQLC | `internal/adapter/db/postgres/`, `internal/adapter/db/postgres/sqlcgen/` |
| Code Mode JS Sandbox (`flowsk_execute` / `flowsk code`) | Pure-Go `github.com/dop251/goja` (`CGO_ENABLED=0` ES6 VM, `ValidateSyntax`, zero host/net globals, watchdog timeout, single-tx ACID `Engine` + staged `DryRun`-safe `.md` attachment writes) | `internal/adapter/codemode/` |
| Shared Volume Coordination | Advisory file locks (`golang.org/x/sys` `v0.50.0`) & rolling SQLite snapshots (`~/.flowsk` / `/data/flowsk`) | `internal/adapter/volume/` |
| Global Markdown Resource Store | Atomic (`0700`/`0600`) tenant-isolated `.md` store (`~/.flowsk/{projects,tasks}/<workspace_id>/<id>.md`) + orphan scanner | `internal/adapter/mdstore/` |
| Embedded Web UI Static & Bootstrap Adapter | `//go:embed all:dist` Angular SPA handler (`NewSPAHandler`) + `/api/ui-config` (`NewBootstrapHandler`) | `internal/adapter/webui/` |
| Client-Only Local Path Registry | Host JSON store (`~/.flowsk/local_paths.json`, Git `.git`/worktree/subpath commit inspection, zero repo/server footprint) | `internal/adapter/localpath/` |
| Terminal & Low-Token Formatters | 10 domain formatters across `human` (TTY tables, goal progress bars, `BoardView`) + `compact`, `tsv`, `jsonl`, `json`, `markdown`/`md` with line-safe `MaxTokens` budgets | `internal/adapter/formatter/` |
| Remote ConnectRPC Client | Typed HTTP/Connect client provider (`Workspace`, `Project`, `Objective`, `Task`, `Language`, `Audit`) when `--server-url` / `FLOWSK_SERVER_URL` is configured | `internal/adapter/client/` |

## 6. Build, Codegen & Verification Tooling

| Tool | Purpose | Canonical Command (`Taskfile.yml`) |
| :--- | :--- | :--- |
| Task v3 (`Taskfile.yml`) | Unified Web UI + Go build, codegen, migration, and CI verification runner | `task --list` / `task verify` |
| Node.js 22 + `pnpm@12.21.0` (`web/`) | Angular `21.1` + Tailwind CSS `v4.1` + SignalStore SPA build into `internal/adapter/webui/dist/` | `task ui:install`, `task ui:build` |
| Buf CLI (`buf`) | Protobuf v3 linting (`STANDARD` + `COMMENTS`), breaking-change detection, Go/Connect stub generation | `task proto:lint`, `task proto:breaking`, `task proto:gen` |
| SQLC v2 (`sqlc`) | Compile-time SQL verification & dual-engine Go repository generation | `task sqlc:compile`, `task sqlc:gen` |
| Goose v3 (`goose`) | Reversible SQLite and PostgreSQL schema migrations | `task db:migrate:sqlite`, `task db:migrate:postgres` |
| Go Toolchain (`Go 1.26.0` / `go1.26.11`) | Static binary compilation (`CGO_ENABLED=0`), installation (`GOBIN`), and race-enabled testing | `task build`, `task install`, `task test` |
| `golangci-lint` | Static analysis, style, and defensive security linting | `task lint` |
| `govulncheck` | Call-graph vulnerability scanning against the Go vulnerability database | `task vuln` |
| Docker & Compose | Hardened multi-stage container build & SQLite / PostgreSQL 17 multi-profile stack | `docker compose up -d` / `docker compose --profile postgres up -d` |

## 7. Governance & Untracked Operational Plans

| Artifact | Purpose | Exact Path |
| :--- | :--- | :--- |
| Subsystem Agent Rules | Scoped architectural invariants, mandatory English commenting mandate & executive directives (explicitly unignored in `.gitignore` via `!AGENTS.md`) | `AGENTS.md`, `{proto,db,internal}/AGENTS.md`, `internal/{domain,adapter,language,transport,cli}/AGENTS.md` |
| Hierarchical `.prompts` Tree | Untracked (`.gitignore`) Italian architectural docs & `ocin-ai` MCP plans | `.prompts/`, `<subsystem>/.prompts/` |
