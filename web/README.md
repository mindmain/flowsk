# Flowsk Web UI (`web/`)

Real-time single-page web interface for **Flowsk** (`module github.com/mindmain/flowsk`), embedded directly into the single static Go binary via `//go:embed all:dist` in `internal/adapter/webui` and served by `flowsk ui` (`flowsk web`, default `127.0.0.1:1616`) or `flowsk serve --ui` (`FLOWSK_UI=true`).

## Stack & Architecture

- **Framework & Reactivity**: Angular `21.1` standalone components with `ChangeDetectionStrategy.OnPush`, Signals, and `@ngrx/signals` `SignalStore` state management.
- **Styling**: Tailwind CSS `v4.1` (`@tailwindcss/postcss`).
- **Transport & Live Sync**:
  - **ConnectRPC** (`@connectrpc/connect` + `@connectrpc/connect-web` + `@bufbuild/protobuf`) communicating with `internal/transport/connectrpc` over HTTP/1.1 & HTTP/2.
  - **Server-Sent Events (`GET /api/events`)** streaming real-time workspace-scoped updates from both in-process ConnectRPC mutations and cross-process CLI / `stdio` MCP SQLite WAL commits.
- **Views**: Live drag-and-drop Kanban & Grid task boards, strategic **Goals & Roadmap** (`goal`, `milestone`, `cycle`), hierarchical Project/Subproject selector & unbound-folder Projects Overview, Markdown document viewer/editor, and live agent activity feed.

## Build & Development Commands

Run frontend tasks from the repository root via [Task](https://taskfile.dev) (`Taskfile.yml`):

```sh
# Install frontend dependencies via pnpm (frozen lockfile)
task ui:install

# Compile production SPA assets into ../internal/adapter/webui/dist/
task ui:build
```

The production build outputs directly to `../internal/adapter/webui/dist/` (configured in `angular.json`), which is tracked in Git and embedded by `internal/adapter/webui` so `go install`, `task build`, and `docker build` always include a complete working Web UI bundle.

For iterative local frontend development with Angular CLI (`pnpm start` inside `web/`), run a local Flowsk backend (`flowsk serve --listen 127.0.0.1:8080 --ui`) or launch the embedded UI directly with `flowsk ui`.
