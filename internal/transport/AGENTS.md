# Upper Transport Layer Governance (`internal/transport/AGENTS.md`)

## 1. Scope & Stack Reference
Owns upper network and agent transport adapters in `internal/transport/connectrpc/` (Buf Connect HTTP/1.1 & `h2c` HTTP/2 handlers, interceptors, and the Go-channel `EventBroker` + cross-process audit relay `GET /api/events` SSE stream) and `internal/transport/mcp/` (pure-Go JSON-RPC 2.0 `stdio` and Streamable `http` MCP servers). Refer to [STACK.md](../../STACK.md) for `connectrpc.com/connect`, `protovalidate`, and handler mappings.

## 2. Architecture & Style Invariants
- **Thin Protocol Translation Only**: Transport packages perform auth extraction (`internal/domain/authctx`), `protovalidate` input validation, DTO-to-domain translation, `ToProto()` / `formatter` serialization, and canonical error mapping (`internal/domain/errs` -> `connect.Code` or structured MCP tool errors) — zero business logic or direct SQL driver imports.
- **Immediate Constructor Resolution (`connectrpc`)**: ConnectRPC service handlers (`workspacehandler`, `projecthandler`, `objectivehandler`, `taskhandler`, `languagehandler`, `audithandler`) declare segregated `DataDependencies` / `ServicesDependencies` interfaces and resolve fine-grained domain/Code Mode interfaces immediately inside `New(...)` without retaining god-containers.
- **Cloudflare Code Mode MCP & Browser-Safe HTTP Guards (`connectrpc`, `mcp`)**: Expose `flowsk_search` (GitHub-style task/goal search and `scope="sdk"` progressive SDK discovery), `flowsk_execute` (sandboxed ES6 JavaScript via `codemode` in one ACID transaction), `flowsk_language_guide` (TypeScript `.d.ts` SDK & search specification), `flowsk_digest` (low-token workspace/project/goal/task digest), and `flowsk://language/guide` identically over both local `stdio` and networked `http` transports; enforce `Origin`/`Sec-Fetch-Site`/`Host` DNS-rebinding & CSRF guards, RFC 9110 `Bearer` token checks, and `application/json` `Content-Type` enforcement on HTTP endpoints.
- **No Server-Side Local Path Exposure & English Comments**: Never accept, return, or log client filesystem paths in any ConnectRPC handler or server-side MCP payload; document every file header, handler, interceptor, and tool schema strictly in English.

## 3. Flowsk Operational Plans & MCP Lifecycle
- Track ConnectRPC and MCP transport tasks in `~/.flowsk/` via the `flowsk` MCP tools (`flowsk_digest`, `flowsk_search`, `flowsk_execute`) without creating plan files inside `internal/transport/`.

## 4. Executive Orders & Verification Commands
1. **Ordine Esecutivo 1: Aggiorna sempre `AGENTS.md` e `STACK.md` ad ogni modifica rilevante di stack, architettura, adapter, comandi o struttura prima di concludere il task.**
2. **Direct English Commenting Mandate**: Keep all Go transport code, RPC error messages, MCP tool descriptions, and doc-comments strictly in English.
3. Verify ConnectRPC and MCP transport handlers, interceptors, and linters:
   ```sh
   go test -race -count=1 ./internal/transport/...
   task lint
   ```
