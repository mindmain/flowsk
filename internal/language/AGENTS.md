# Search Parser & Code Mode SDK Catalog Governance (`internal/language/AGENTS.md`)

## 1. Scope & Stack Reference
Owns Flowsk's GitHub/Linear-style structured search query parser (`search.go`) and the Cloudflare Code Mode JavaScript SDK TypeScript specification, method catalog, and runnable ES6 recipes (`guide.go`) in `internal/language/`. See [STACK.md](../../STACK.md) for how `internal/adapter/codemode`, `internal/transport/{connectrpc,mcp}`, and `internal/cli` consume this package.

## 2. Architecture & Style Invariants
- **Pure Decoupled Search & SDK Specification Package**: Keep `internal/language` completely free of `connectrpc`, `mcp`, `cobra`, `goja`, SQL driver, and `os` filesystem imports; depend only on the Go standard library, `internal/domain/task` status/priority constants, and `gen/go/flowsk/language/v1` for outbound `Example.ToProto()` mapping.
- **Fault-Tolerant Search Parsing (`ParseSearchQuery`)**: Support GitHub/Linear qualifiers (`state:*`, `is:open|done|claimed|unclaimed|blocked|archived`, `#tag` / `label:*`, `@actor` / `actor:*`, `obj:*`, `parent:*`, `ext:*`, `prio:*`, `type:sdk|tasks|goals|all`) while safely treating unknown `key:value` tokens as conjunctive free-text search terms so user/agent queries never fail with syntax errors.
- **Zero Host Paths & Synchronized Code Mode SDK Guide**: `CodeModeTypeScriptSpec()`, `CodeModeSDKCatalog()`, and `DefaultLanguageGuideMarkdown` (`GuideProvider`) must stay 100% synchronized with the global `flowsk.*` ES6 JavaScript methods bound in `internal/adapter/codemode` and must never accept or reference host filesystem paths.
- **Direct Mandate — Comprehensive English Comments**: Write all Go source code, TypeScript SDK declarations, Markdown specifications, file headers, and doc-comments strictly in English.

## 3. Hierarchical `.prompts` Tree & MCP Lifecycle
- Search parser and SDK catalog plans reside in `internal/language/.prompts/`; cross-layer plans touching `internal/adapter/codemode`, `proto/flowsk/language/v1`, or MCP tools reside in `internal/.prompts/` or root `.prompts/`.
- Manage all `.prompts` plan creation, task progress, and status updates exclusively through `ocin-ai` MCP tools; never delete `.md` files in `.prompts`.

## 4. Executive Orders & Verification Commands
1. **Ordine Esecutivo 1: Aggiorna sempre `AGENTS.md` e `STACK.md` ad ogni modifica rilevante di stack, architettura, adapter, comandi o struttura prima di concludere il task.**
2. **Ordine Esecutivo 2: Non cancellare mai file `.md` in `.prompts`; posiziona `.prompts` nel perimetro minimo del lavoro (servizio, dominio padre o root) e gestisci creazione, avanzamento e stato esclusivamente tramite i tool MCP di `ocin-ai`.**
3. **Direct English Commenting Mandate**: Document every search type, SDK catalog struct/method, and guide constant strictly in English.
4. Run search and SDK guide tests with race detection and static analysis after any change:
   ```sh
   go test -race -count=1 ./internal/language/...
   task lint
   ```
