# Pure Core Domain Governance (`internal/domain/AGENTS.md`)

## 1. Scope & Stack Reference
Owns pure core domain packages in `internal/domain/{errs,authctx,tx,workspace,project,objective,task,audit}/` for `module github.com/mindmain/flowsk`. Refer to [STACK.md](../../STACK.md) for cross-package architecture and adapter bindings.

## 2. Architecture & Style Invariants
- **Zero Transport & Infrastructure Imports**: Domain packages must never import `connectrpc.com/*`, `net/http`, `spf13/{cobra,viper}`, `dop251/goja`, `database/sql`, `modernc.org/sqlite`, `jackc/pgx/v5`, or `internal/adapter/*`.
- **Consumer-Owned Interfaces & Outbound `ToProto()` Mappers**: Each aggregate package (`workspace`, `project`, `objective`, `task`, `audit`) defines its pure Go entities, nil-safe outbound `ToProto()` methods targeting `gen/go/flowsk/*/v1` messages (no `FromProto()` on domain types), its `Service` contract, and minimal local `Repository` interfaces implemented by `internal/adapter/db/*`.
- **Mandatory Ownership, ACID Transactions & Atomic Audit**: Extract caller scope via `internal/domain/authctx`, enforce `workspace_id` ownership on every operation, return typed errors from `internal/domain/errs`, and wrap multi-write state transitions plus `audit` entries inside `internal/domain/tx` (`WithTransaction`) with fail-fast rollback.
- **Pure Logical Entities & English Comments**: `project` (including acyclic multi-binding `ProjectBinding` sub-project edges with non-destructive parent deletion), `workspace`, `objective` (goals, milestones, cycles), and `task` (including optional project-scoped unique `ExternalID` independent of `ID` and `Key`) models represent logical work graphs only and must never hold host filesystem paths; every file header, exported struct/field, interface, and method MUST be documented strictly in English.

## 3. Hierarchical `.prompts` Tree & MCP Lifecycle
- Domain-only plans reside in `internal/domain/.prompts/` (or `internal/domain/<aggregate>/.prompts/`); cross-layer plans reside in `internal/.prompts/` or root `.prompts/`.
- Manage creation, task tracking, and status updates of all `.prompts` artifacts exclusively via `ocin-ai` MCP tools; never delete `.md` files in `.prompts`.

## 4. Executive Orders & Verification Commands
1. **Ordine Esecutivo 1: Aggiorna sempre `AGENTS.md` e `STACK.md` ad ogni modifica rilevante di stack, architettura, adapter, comandi o struttura prima di concludere il task.**
2. **Ordine Esecutivo 2: Non cancellare mai file `.md` in `.prompts`; posiziona `.prompts` nel perimetro minimo del lavoro (servizio, dominio padre o root) e gestisci creazione, avanzamento e stato esclusivamente tramite i tool MCP di `ocin-ai`.**
3. **Direct English Commenting Mandate**: Keep 100% of Go domain comments, invariants, and error messages strictly in English.
4. Verify zero forbidden imports and full race-tested domain behavior:
   ```sh
   go test -race -count=1 ./internal/domain/...
   task lint
   ```
