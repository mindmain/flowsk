# Flowsk

**Flowsk** (`github.com/mindmain/flowsk`) is a local-first, network-capable project, strategic goal, and multi-agent task coordination engine written in Go (`CGO_ENABLED=0`). A single static binary (`flowsk`) ships the terminal CLI, the embedded Angular Web UI (`flowsk ui`), the Buf Connect (`ConnectRPC`) server (`flowsk serve`), and the **Cloudflare Code Mode** Model Context Protocol (`MCP`) server (`flowsk mcp serve`).

- **Self-Contained `~/.flowsk` Memory**: By default (`FLOWSK_SERVER_URL` unset), all state lives in `~/.flowsk/` (`flowsk.db`, `config.yaml`, `local_paths.json`, and per-project/per-task Markdown docs). Zero `.flowsk` files are ever created inside your Git repositories, and host filesystem paths are never transmitted over the network.
- **Cloudflare Code Mode for AI Agents**: Instead of exposing dozens of chatty CRUD tools or a custom DSL, Flowsk's MCP server embeds a hermetic pure-Go ES6 JavaScript sandbox (`dop251/goja`) so agents query and mutate projects, goals, DAG dependencies, external tracker IDs (`externalId`), and exclusive `ClaimLease` locks inside **one atomic ACID transaction**.

---

## Quickstart

```sh
# Build the unified binary (embeds the Angular 21 Web UI + Goose SQL migrations + AI SKILL.md)
task build

# Install the Flowsk operational skill across detected AI agents (Claude, Codex, Antigravity, Gemini, Cursor)
./bin/flowsk install

# Bind the current Git repository/worktree to a logical project in ~/.flowsk
./bin/flowsk init CORE --name "Core Platform"

# Inspect global or project status in the terminal, or open the embedded Kanban/Grid Web UI
./bin/flowsk status
./bin/flowsk ui

# Verify ~/.flowsk storage, DB migrations, orphan .md hygiene, MCP config, and skill SHA-256 hashes
./bin/flowsk doctor
```

---

## MCP Server & Cloudflare Code Mode Examples

### 1. Register the MCP Server

Run `flowsk mcp guide` to print ready-to-paste client configs, or add `flowsk` directly to your MCP client configuration (`.mcp.json`, `~/.claude.json`, `~/.gemini/settings.json`, Antigravity, or `~/.codex/config.toml`):

```json
{
  "mcpServers": {
    "flowsk": {
      "command": "flowsk",
      "args": ["mcp", "serve", "--transport", "stdio", "--workspace", "default", "--actor", "ai-agent"]
    }
  }
}
```

Flowsk exposes **four token-frugal MCP tools** and one read-only Markdown specification resource (`flowsk://language/guide`):

| MCP Tool | Purpose |
| :--- | :--- |
| `flowsk_digest` | Low-token snapshot of task counts, active goals, live peer leases, ready queue, and blockers |
| `flowsk_search` | GitHub/Linear-style search across tasks, goals, or `flowsk.*` JS SDK signatures (`scope: "sdk"`) |
| `flowsk_execute` | Evaluates ES6 JavaScript against the global `flowsk` SDK in **one outer ACID transaction** |
| `flowsk_language_guide` | Returns the TypeScript `.d.ts` SDK declaration and runnable JavaScript recipes on demand |

### 2. Poll State & Search (`flowsk_digest` & `flowsk_search`)

```json
// Tool: flowsk_digest
{
  "project_id": "CORE",
  "include_blocked": true,
  "limit": 20
}
```

```json
// Tool: flowsk_search — filter unclaimed ready tasks, external IDs, or inspect SDK signatures
{
  "project_id": "CORE",
  "query": "state:ready is:unclaimed #backend",
  "scope": "tasks"
}
```

```json
// Tool: flowsk_search — progressive SDK discovery for JavaScript methods
{
  "query": "upsertExternal",
  "scope": "sdk"
}
```

### 3. Atomic Multi-Step JavaScript Execution (`flowsk_execute`)

All `flowsk.*` calls inside `flowsk_execute` (or `flowsk code -e '...'` in the CLI) run inside a single database transaction — if any statement throws or violates a domain invariant, every prior write rolls back automatically (`dry_run: true` simulates without committing):

```js
// Example A — Create a strategic goal, decompose subtasks with DAG blockers, and claim the first task:
const goal = flowsk.goals.create({
  title: "Ship Webhook Security v1",
  priority: "urgent",
  targetDate: "2026-12-31"
});

const t1 = flowsk.tasks.create({
  title: "Implement HMAC-SHA256 signature middleware",
  objective: goal.key,
  priority: "urgent",
  tags: ["security", "api"]
});

const t2 = flowsk.tasks.create({
  title: "Add replay-window integration tests",
  objective: goal.key,
  priority: "high",
  dependsOn: [t1.key],
  tags: ["security", "test"]
});

return flowsk.tasks.claim(t1.key, 1800);
```

```js
// Example B — Sync an external tracker ticket (ClickUp / Linear / Jira / GitHub) via project-unique externalId:
const parent = flowsk.tasks.upsertExternal({
  externalId: "CU-869abc",
  title: "Audit multi-tenant query isolation",
  priority: "high",
  tags: ["db", "security"]
});

flowsk.tasks.comment(parent.key, "All SQLC queries verified with workspace_id guards", "progress");
flowsk.tasks.done(parent.key, "Verified with task verify");

// Atomically claim the next highest-priority unblocked ready task:
return flowsk.tasks.next({ claim: true, ttlSeconds: 1800 });
```

---

## Shared Network Server (Optional Docker Compose)

```sh
# Option 1: Shared SQLite WAL volume (/data/flowsk)
cp .env.example .env
docker compose up -d --build

# Option 2: PostgreSQL 17 profile for multi-node / office deployments
FLOWSK_DB_DRIVER=postgres \
FLOWSK_POSTGRES_DSN="postgres://flowsk:flowsk@postgres:5432/flowsk?sslmode=disable" \
docker compose --profile postgres up -d --build

# Point any workstation CLI or MCP server at the shared instance:
FLOWSK_SERVER_URL="http://192.168.1.50:8080" flowsk status
```

---

## Developer Workflow (`Taskfile.yml`)

| Command | Description |
| :--- | :--- |
| `task proto:lint` | Lint Protobuf v3 schemas and mandatory English comments (`buf lint`) |
| `task proto:breaking` | Check Protobuf backward compatibility (`buf breaking`) |
| `task proto:gen` | Regenerate Go Protobuf & ConnectRPC stubs (`gen/go/`) |
| `task sqlc:compile` | Validate SQLite and PostgreSQL queries against Goose schemas |
| `task sqlc:gen` | Regenerate dual-engine SQLC packages (`internal/adapter/db/*/sqlcgen`) |
| `task ui:build` | Compile Angular 21 + Tailwind CSS v4 SPA into `internal/adapter/webui/dist/` |
| `task build` | Compile Web UI assets and static `bin/flowsk` binary (`CGO_ENABLED=0`) |
| `task install` | Compile Web UI assets and install `flowsk` into `GOBIN` / `GOPATH/bin` |
| `task test` | Run race-enabled Go test suite (`go test -race -count=1 ./...`) |
| `task lint` | Run `golangci-lint` across all packages |
| `task vuln` | Run `govulncheck` vulnerability scan |
| `task verify` | Execute full pre-commit verification pipeline |

---

## Stack & Governance Catalog

- [STACK.md](./STACK.md) — Canonical technology, protocol, domain, adapter, and tooling directory map.
- [AGENTS.md](./AGENTS.md) — Root repository architecture rules, `~/.flowsk` local/network dual mode, English commenting mandate, `.prompts` lifecycle, and executive orders.
- [proto/AGENTS.md](./proto/AGENTS.md) — Protobuf v3, `protovalidate`, mandatory English docstrings, and Buf Connect contract rules.
- [db/AGENTS.md](./db/AGENTS.md) — Goose reversible migrations, English SQL comments, and dual-engine SQLC query rules.
- [internal/AGENTS.md](./internal/AGENTS.md) — Strict 3-layer internal package boundaries, English comment mandate, and dependency direction.
- [internal/domain/AGENTS.md](./internal/domain/AGENTS.md) — Pure domain models, outbound `ToProto()` mappers, `tx`, and `authctx` invariants.
- [internal/language/AGENTS.md](./internal/language/AGENTS.md) — GitHub/Linear search query parser and Cloudflare Code Mode JS SDK catalog/guide invariants.
- [internal/adapter/AGENTS.md](./internal/adapter/AGENTS.md) — SQLite/PostgreSQL, `goja` Code Mode sandbox, `mdstore`, `webui`, `volume`, client-only `localpath`, `formatter`, and RPC `client` adapters.
- [internal/transport/AGENTS.md](./internal/transport/AGENTS.md) — ConnectRPC handlers/interceptors and Cloudflare Code Mode MCP (`stdio`/`http`) server boundaries.
- [internal/cli/AGENTS.md](./internal/cli/AGENTS.md) — Cobra + Viper + Charm TUI dual-mode CLI tree, `status`/`board`/`search`/`code`, and client-local path bindings.
