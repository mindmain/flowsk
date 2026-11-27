# Internal Packages Governance (`internal/AGENTS.md`)

## 1. Scope & Stack Reference
Governs all private application packages under `internal/` (`app`, `config`, `version`, `skill`, `domain`, `language`, `adapter`, `transport`, `cli`) for `module github.com/mindmain/flowsk`. Refer to [STACK.md](../STACK.md) for package inventory and consult `./{domain,adapter,language,transport,cli}/AGENTS.md` for layer-specific rules.

## 2. Architecture & Style Invariants
- **Unidirectional Dependency Graph**:
  - `internal/domain/*` and `internal/language` depend only on the Go standard library, `google/uuid`, and `gen/go/flowsk/*/v1` Protobuf model types (for outbound `ToProto()` mapping) — never on `connectrpc.com/connect`, `mcp`, `cobra`, `viper`, `pgx`, `sqlite`, `goja`, or `os` filesystem adapters.
  - `internal/adapter/*` implements domain/CLI ports (`db`, `codemode`, `volume`, `mdstore`, `webui`, `localpath`, `formatter`, `client`) without importing `internal/transport/*` or `internal/cli`.
  - `internal/transport/{connectrpc,mcp}` and `internal/cli` adapt external protocols/flags into pure `domain`, `language`, and `codemode` calls (supporting both embedded local SQLite execution and remote ConnectRPC network clients); `internal/config` centralizes typed Viper `FLOWSK_*` configuration, and `internal/app` wires the composition root.
- **Constructor Dependency Injection**: Wire fine-grained interfaces in constructors (`New(...)`), fail fast on invalid config, propagate `context.Context` everywhere, and avoid package-level mutable singletons.
- **Direct Mandate — English Comments & Zero Path Leakage**: Every `.go` file MUST begin with a clear English package or file-level comment and document every exported type, struct field, constant, interface, and method/function in English; `internal/adapter/localpath` may only be imported by client-side `internal/cli` and `internal/app` (never by server handlers or domain packages).

## 3. Flowsk Operational Plans & MCP Lifecycle
- Coordinate internal package tasks, refactors, and Markdown specs in `~/.flowsk/` via the `flowsk` MCP tools (`flowsk_digest`, `flowsk_search`, `flowsk_execute`) without writing plan files inside `internal/`.

## 4. Executive Orders & Verification Commands
1. **Ordine Esecutivo 1: Aggiorna sempre `AGENTS.md` e `STACK.md` ad ogni modifica rilevante di stack, architettura, adapter, comandi o struttura prima di concludere il task.**
2. **Direct English Commenting Mandate**: Document every `.go` file header, exported symbol, and architectural invariant strictly in English.
3. Validate layer isolation, race safety, and static analysis across `internal/...`:
   ```sh
   task build
   task test
   task lint
   ```
