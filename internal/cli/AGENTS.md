# CLI Command Tree Governance (`internal/cli/AGENTS.md`)

## 1. Scope & Stack Reference
Owns the Cobra + Viper + Charm Lip Gloss / Bubble Tea command tree in `internal/cli/` powering the unified `flowsk` binary (`init`, `install` & `uninstall` [3-stage Skill + MCP wizards defaulting to Universal `~/.agents`], `doctor`, `update`, `board`/`status`, `ui`/`web` [default `127.0.0.1:1616`], `prune`, `search`/`find`/`s`, `code`/`exec`, `serve`, `mcp`, `workspace`/`ws`, `project`, `objective`/`goal`, `task`/`issue`, `audit`, `db`, `config`). Refer to [STACK.md](../../STACK.md) for `spf13/cobra`, `spf13/viper`, `charmbracelet/lipgloss`, `charmbracelet/bubbletea`, and local/remote adapter bindings.

## 2. Architecture & Style Invariants
- **Dual-Mode Execution (`~/.flowsk` Default + Optional Network Client)**: When `--server-url` / `FLOWSK_SERVER_URL` is empty (default), CLI commands execute in-process against the local SQLite WAL file (`--db-path` / `FLOWSK_DB_PATH`, default `~/.flowsk/flowsk.db`) inside the self-contained `~/.flowsk` folder (`$FLOWSK_HOME`) with zero daemon setup. When `--server-url` is configured, the exact same commands delegate over Buf Connect (`internal/adapter/client`) to a shared `flowsk serve` instance on localhost, LAN, VPN, or a remote server.
- **Clear Terminal Views, Global Multi-Project `status`, Cloudflare Code Mode & Low Tokens**: Render workspaces, projects, sub-project DAG bindings, composed Markdown blueprints (`flowsk goal bundle` / `doc` / `dossier` and `flowsk project bundle` / `project doc --bundle`), strategic goals/objectives (`flowsk goal` / `flowsk objective` with `--slug`, `--success-criteria`, and task progress bars), tasks (`flowsk task`, `--external-id`, `--done`, `--in-progress`, `-q`, `done`/`close`/`complete`), `get` aliases (`show`/`view`), searches (`flowsk search`, `--type sdk`), sandboxed ES6 JavaScript Code Mode scripts (`flowsk code` / `flowsk exec` via `-e`, `-c`, `-f`, stdin, or positional script), and boards (`flowsk board` / `flowsk status`, falling back to `WorkspaceOverviewView` with per-state task counts matching workflow columns and grouped Git worktree commits when outside any bound directory or with `-A/--all`) through `OutputFormatter` (with built-in fallback `cli.Render` / `cli.ResolveOutputFormat` in `render.go`) — defaulting via `--format auto` to aligned `human` tables on interactive TTYs and low-token `compact` output (plus `tsv`, `jsonl`, `json`, `md`, `--max-tokens`) when piped or called by AI agents, and returning non-zero exit errors with typo suggestions on unknown subcommands across all command groups.
- **Strict Client-Only Local Path Registry & `flowsk init`**: Only `internal/cli` commands (`init`, `project link|unlink|where`, and implicit working-directory/worktree resolution) may invoke `internal/adapter/localpath` (`~/.flowsk/local_paths.json` + local Git `.git`/worktree/commit inspection); host directories are never written into target Git repositories and never transmitted to any server.
- **Deterministic Stream Hygiene & English Comments**: Propagate `signal.NotifyContext` (`SIGINT`/`SIGTERM`), write command output strictly to `cmd.OutOrStdout()`, route diagnostics/errors to `cmd.ErrOrStderr()`, and keep all CLI usage text, flags, examples, file headers, and Go comments strictly in English.

## 3. Flowsk Operational Plans & MCP Lifecycle
- Coordinate CLI command and TUI tasks in `~/.flowsk/` via the `flowsk` MCP tools (`flowsk_digest`, `flowsk_search`, `flowsk_execute`) without creating plan files inside `internal/cli/`.

## 4. Executive Orders & Verification Commands
1. **Ordine Esecutivo 1: Aggiorna sempre `AGENTS.md` e `STACK.md` ad ogni modifica rilevante di stack, architettura, adapter, comandi o struttura prima di concludere il task.**
2. **Direct English Commenting Mandate**: Document every CLI file header, command help string, flag description, and exported symbol strictly in English.
3. Verify CLI compilation, command tests, and static analysis:
   ```sh
   task build
   go test -race -count=1 ./internal/cli/...
   task lint
   ```
