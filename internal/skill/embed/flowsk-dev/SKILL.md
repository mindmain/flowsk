---
name: flowsk-dev
description: Validate composed goal blueprints, acquire exclusive claim leases, and implement Flowsk tasks in code without mutating planned task specifications. Activate when executing engineering tasks, implementing a Flowsk goal, or picking up ready work in a Flowsk-managed project.
---

# Flowsk Dev — Plan Validation & Implementation Mandate (`flowsk-dev`)

## 1. Core Mandate — Faithful Execution & Upfront Plan Validation
- **Implementation Role**: Under `flowsk-dev`, your mandate is to **read the authored plan, validate its technical soundness against the codebase, claim ready tasks exclusively, write & verify the code, and close completed tasks**.
- **Do NOT Rewrite Task Definitions on Gaps**: If you uncover an implementation gap, contradiction, missing dependency, or broken assumption in a goal or task specification, **DO NOT silently edit, rescope, split, or overwrite task titles, descriptions, `.md` specs, or DAG plans** (`create`, `update`, `setDoc`, `deleteDoc`). Stop before shipping broken assumptions, **report the exact gap or problem clearly to the user** (optionally logging a `blocker` comment or releasing your lease cleanly), and wait for user direction or a `flowsk-plan` revision.
- **Local-First State (`~/.flowsk`)**: Never create Flowsk metadata files inside Git repositories. Preserve `externalId` on tracker-synced tasks (`0=none`, `1=P1/urgent .. 4=P4/low` unified priority).

## 2. Flowsk MCP Interface (Primary)
Always use the **`flowsk` MCP server** tools before falling back to the CLI:
1. `flowsk_digest` — Inspect project state counts, active peer leases, ready queue, and blockers in one low-token call.
2. `flowsk_search` — Filter ready/leased tasks or SDK docs (`state:ready is:unclaimed`, `obj:<goal-slug>`, `ext:CU-869abc`, `type:sdk`).
3. `flowsk_execute` — Run ES6 JavaScript against the `flowsk` SDK in **one atomic ACID transaction**:
   ```js
   // 1. Download the entire composed Goal blueprint (charter + criteria + DAG + all task .md specs) in one call:
   const blueprint = flowsk.goals.bundle("auth-session-hardening");
   return blueprint.markdown;
   ```
   ```js
   // 2. Claim a ready task (returns TaskDetail with inline `markdown` specification already populated):
   return flowsk.tasks.claim("CORE-12", 1800);
   // or pick the highest-priority unblocked ready task:
   // return flowsk.tasks.next({ claim: true, ttlSeconds: 1800 });
   ```
4. `flowsk_language_guide` — Retrieve exact `flowsk.*` TypeScript signatures on demand.

*(If MCP tools are unavailable in your session, run `flowsk install --yes` or use `flowsk goal bundle <slug>` / `flowsk code -c '...'`.)*

## 3. Mandatory 5-Step Developer Execution Workflow

### Step 1 — Download Composed Blueprint & Validate the Whole Plan First
1. Start with `flowsk_digest`. When assigned a Goal (or a task belonging to a Goal), call `flowsk.goals.bundle("<goal-slug>")` (or `flowsk.projects.bundle({ includeDone: false })` / `flowsk.tasks.get("<KEY>")`) so you have the **entire composed Markdown design** in context without hunting task by task.
2. Cross-check the blueprint against the real codebase before writing code:
   - Verify target files, types, interfaces, configs, and upstream task outputs match what the `.md` spec expects.
   - Check for architectural holes, security/ownership risks, or contradictions across sibling tasks.
3. **Escalation Rule**: If any gap, conflict, or missing decision is found, **do not alter the task specifications** — report the exact findings and affected `<KEY>` to the user immediately and pause for confirmation.

### Step 2 — Claim Exclusively Before Editing Code
- Acquire an exclusive time-bounded `ClaimLease` via `flowsk.tasks.claim("<KEY>", 1800)` or `flowsk.tasks.next({ claim: true, ttlSeconds: 1800 })`.
- Never modify code for a task leased by another active actor (`holder`), and never start a task with `blocked: true` until its upstream `dependsOn` tasks reach `done`.

### Step 3 — Implement Strictly Against `task.markdown`
- Execute the changes described in `task.markdown` (`Target Files & Symbols`, `Implementation Plan & Edge Cases`, `Acceptance & Verification`) and `acceptance` criteria while respecting repository `AGENTS.md` / `STACK.md` rules.
- Log concise factual progress or implementation notes via `flowsk.tasks.comment("<KEY>", "<note>", "progress" | "decision")` without modifying `setDoc`.

### Step 4 — Run Verification Gates, Then Mark `done`
- Run all build, test, and lint commands required by the task `.md` specification and project governance.
- Only when every gate passes cleanly, close the task and release the lease atomically:
  ```js
  return flowsk.tasks.done("CORE-12", "Implemented and verified with task verify");
  ```

### Step 5 — Clean Handoff on Blockers or Early Stop
- If an external impediment or mid-flight plan flaw blocks completion, record context via `flowsk.tasks.comment("<KEY>", "<impediment details>", "blocker")` and release the lease cleanly (`flowsk.tasks.release("<KEY>", { state: "in_review", note: "<handoff>" })`), then report to the user.

## 4. CLI Fallback Reference
```sh
flowsk status                                                  # Project board & active leases
flowsk goal bundle <goal-slug>                                 # Download composed goal .md blueprint
flowsk task next --actor <agent-handle> --claim                # Claim next unblocked ready task
flowsk task doc <KEY>                                          # Read task .md specification
flowsk task done <KEY> --note "Verified with task verify"      # Close task & release lease
```
