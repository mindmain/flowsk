---
name: flowsk
description: Coordinate multi-agent tasks, strategic goals, exclusive claim leases, DAG blockers, and external tracker sync (ClickUp, Linear, Jira, GitHub) using the Flowsk MCP server. Activate whenever managing tasks, parallelizing work across AI agents, or syncing external issue IDs in a Flowsk-managed project.
---

# Flowsk Operational Skill

## 1. Purpose & Role
- **Local-First State Engine (`~/.flowsk`)**: Stores projects, goals/milestones, hierarchical tasks, comments, and Markdown docs outside Git repositories. Never create Flowsk metadata files inside target repositories.
- **Parallel Multi-Agent Coordination**: Enforces exclusive time-bounded `ClaimLease` locks and DAG `blocks` preconditions so concurrent agents never duplicate work or start blocked tasks.
- **External Tracker Bridge (`external_id`)**: Links external tickets (ClickUp, Linear, Jira, GitHub) to Flowsk tasks via `externalId`, unique per `(project_id, external_id)` while keeping Flowsk's internal UUID and `KEY-N` independent.

## 2. Primary Interface — Use the Flowsk MCP Server First
Always use the **`flowsk` MCP server** tools instead of shelling out to the CLI when MCP is connected:

1. `flowsk_digest` — Fetch project state counts, active goals, live peer leases, ready queue, and blockers in one low-token call.
2. `flowsk_search` — Query tasks, goals, or SDK docs (`state:ready is:unclaimed`, `ext:CU-869abc`, `#backend`, `@agent`, `type:sdk`).
3. `flowsk_execute` — Run ES6 JavaScript against the `flowsk` SDK inside **one atomic ACID transaction** (`dry_run: true` to simulate):
   ```js
   // Import/sync an external ticket, decompose subtasks with attached .md specs, and claim the first ready step:
   const epic = flowsk.tasks.upsertExternal({
     externalId: "CU-869abc",
     title: "Implement webhook signature verifier",
     priority: "high",
     tags: ["security", "api"],
     markdown: "# Webhook Verifier Spec\n\n- Verify X-Signature HMAC-SHA256\n- Reject timestamps > 300s"
   });
   const sub = flowsk.tasks.create({
     title: "Add HMAC-SHA256 middleware tests",
     parent: epic.key,
     priority: "high",
     markdown: "## Test Matrix\n\n1. Valid signature\n2. Tampered body\n3. Expired timestamp"
   });
   return flowsk.tasks.claim(sub.key, 1800);
   ```
4. `flowsk_language_guide` — Retrieve exact `flowsk.*` TypeScript signatures (including `flowsk.tasks.getDoc` / `setDoc` and `flowsk.goals.*`) on demand.

If the `flowsk` MCP server is not available in your tool list, run `flowsk install --yes` (or inspect snippets via `flowsk mcp guide`) to register `flowsk mcp serve --transport stdio` before falling back to the CLI.

## 3. Mandatory Agent Workflow
1. **Orient**: Call `flowsk_digest` at session start to inspect active peer leases, blockers, and highest-priority ready tasks.
2. **Claim Exclusively Before Editing**: Acquire a lease with `flowsk.tasks.next({ claim: true, ttlSeconds: 1800 })` or `flowsk.tasks.claim("<KEY>", 1800)`. Never edit code for a task leased by another active actor.
3. **Preserve `externalId` on Imported Work**: When decomposing or executing tasks from an external tracker, set `externalId` via `flowsk.tasks.upsertExternal(...)` or `flowsk.tasks.getByExternal("<EXT-ID>")` so completion status maps back deterministically to the external ticket.
4. **Log Decisions & Blockers**: Record concise verification evidence, architectural trade-offs, or handoff context via `flowsk.tasks.comment("<KEY>", "<note>", "progress" | "decision" | "handoff" | "blocker")`.
5. **Verify Then Close**: Run project build/tests first; only when green, call `flowsk.tasks.done("<KEY>", "<verification summary>")` to close the task and release the lease atomically. If stopping early or blocked, release the lease via `flowsk.tasks.release("<KEY>", { state: "in_review" | "blocked", note: "<handoff>" })`.

## 4. CLI Fallback Reference (Only When MCP Is Unavailable)
```sh
flowsk init --yes                                              # Bind current repo/subpath to ~/.flowsk
flowsk status                                                  # Project or workspace task/worktree overview
flowsk task next --actor <agent-handle> --claim                # Claim next unblocked ready task
flowsk task create "<title>" --external-id <EXT-ID> --priority 1
flowsk search "ext:<EXT-ID>"                                   # Lookup task by external tracker ID
flowsk code -e 'return flowsk.tasks.done("FLW-1", "Verified");'
flowsk doctor                                                  # Verify DB, orphans, MCP, and skill hashes
```
