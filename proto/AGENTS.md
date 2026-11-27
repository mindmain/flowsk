# Protobuf & ConnectRPC Contracts Governance (`proto/AGENTS.md`)

## 1. Scope & Stack Reference
Owns all Protobuf v3 service and message schemas in `proto/flowsk/{workspace,project,objective,task,language,audit}/v1/*.proto`, governed by `buf.yaml`, `buf.gen.yaml`, and generated into `gen/go/flowsk/*/v1/`. See [STACK.md](../STACK.md) for Buf v2 plugins, `protovalidate`, and ConnectRPC versions.

## 2. Architecture & Style Invariants
- **Pure Wire Contracts for Network Sharing**: Schemas define Buf Connect (`ConnectRPC`) DTOs, enums (always prefixed with `*_UNSPECIFIED = 0`), dedicated `<RpcName>Request`/`<RpcName>Response` pairs, and cursor pagination tokens enabling remote workstations, containers, and servers to share a `flowsk serve` instance over the network; never leak SQL column internals or server storage details into `.proto` files.
- **No Host Filesystem Paths on the Wire**: Never add `local_path`, `host_dir`, or client filesystem fields to any Protobuf message (`project.v1`, `workspace.v1`, `language.v1`, etc.); local folder bindings remain strictly client-local (`internal/adapter/localpath`).
- **Direct Mandate — Comprehensive English Proto Comments & Validation**: Every `.proto` file MUST include an English package header comment and clear English doc-comments on every `service`, `rpc` method (behavior, auth, error conditions), `message`, `enum`, enum value, and `field`, enforced by `buf.yaml` (`STANDARD` + `COMMENTS`), alongside declarative `buf.validate` constraints (`buf.build/bufbuild/protovalidate`).
- **Backward Compatibility & Clean Codegen**: Never reuse field numbers or change wire types; regenerate `gen/go/` exclusively via `task proto:gen` and never manually edit generated `.pb.go` or `.connect.go` files.

## 3. Flowsk Operational Plans & MCP Lifecycle
- Track contract design specs, tasks, and cross-layer dependencies in `~/.flowsk/` via the `flowsk` MCP tools (`flowsk_digest`, `flowsk_search`, `flowsk_execute`) without creating plan files inside `proto/`.

## 4. Executive Orders & Verification Commands
1. **Ordine Esecutivo 1: Aggiorna sempre `AGENTS.md` e `STACK.md` ad ogni modifica rilevante di stack, architettura, adapter, comandi o struttura prima di concludere il task.**
2. **Direct English Commenting Mandate**: Document every `.proto` service, RPC, message, enum, enum constant, and field strictly in English; zero uncommented declarations are permitted.
3. Run Buf linting, compatibility checks, and stub generation after every `.proto` edit:
   ```sh
   task proto:lint
   task proto:breaking
   task proto:gen
   ```
