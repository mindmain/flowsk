# Pure Core Domain Governance (`internal/domain/AGENTS.md`)

## 1. Scope & Stack Reference
Owns pure core domain packages in `internal/domain/{errs,authctx,tx,workspace,project,objective,task,audit}/` for `module github.com/mindmain/flowsk`. Refer to [STACK.md](../../STACK.md) for cross-package architecture and adapter bindings.

## 2. Architecture & Style Invariants
- **Zero Transport & Infrastructure Imports**: Domain packages must never import `connectrpc.com/*`, `net/http`, `spf13/{cobra,viper}`, `dop251/goja`, `database/sql`, `modernc.org/sqlite`, `jackc/pgx/v5`, or `internal/adapter/*`.
- **Consumer-Owned Interfaces & Outbound `ToProto()` Mappers**: Each aggregate package (`workspace`, `project`, `objective`, `task`, `audit`) defines its pure Go entities, nil-safe outbound `ToProto()` methods targeting `gen/go/flowsk/*/v1` messages (no `FromProto()` on domain types), its `Service` contract, and minimal local `Repository` interfaces implemented by `internal/adapter/db/*`.
- **Mandatory Ownership, ACID Transactions & Atomic Audit**: Extract caller scope via `internal/domain/authctx`, enforce `workspace_id` ownership on every operation, return typed errors from `internal/domain/errs`, and wrap multi-write state transitions plus `audit` entries inside `internal/domain/tx` (`WithTransaction`) with fail-fast rollback.
- **Pure Logical Entities & English Comments**: `project` (including durable `BoardMetadataService` workflow states & labels and acyclic multi-binding `ProjectBinding` sub-project edges with non-destructive parent deletion and cascading soft-delete of owned tasks/objectives), `workspace` (lowercase handle normalization & RBAC member mutation guards), `objective` (goals, milestones, cycles), and `task` (including optional project-scoped unique `ExternalID` independent of `ID` and `Key`) models represent logical work graphs only and must never hold host filesystem paths; every file header, exported struct/field, interface, and method MUST be documented strictly in English.

## 3. Flowsk Operational Plans & MCP Lifecycle
- Manage domain aggregate tasks, DAG blockers, and Markdown specs in `~/.flowsk/` via the `flowsk` MCP tools (`flowsk_digest`, `flowsk_search`, `flowsk_execute`) without creating plan files inside `internal/domain/`.

## 4. Executive Orders & Verification Commands
1. **Ordine Esecutivo 1: Aggiorna sempre `AGENTS.md` e `STACK.md` ad ogni modifica rilevante di stack, architettura, adapter, comandi o struttura prima di concludere il task.**
2. **Direct English Commenting Mandate**: Keep 100% of Go domain comments, invariants, and error messages strictly in English.
3. Verify zero forbidden imports and full race-tested domain behavior:
   ```sh
   go test -race -count=1 ./internal/domain/...
   task lint
   ```
