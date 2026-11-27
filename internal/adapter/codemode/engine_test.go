// File engine_test.go verifies the Cloudflare Code Mode pure-Go sandboxed
// JavaScript execution engine (`dop251/goja`): zero-guide ES6 scripting against
// the `flowsk` SDK, single-transaction ACID atomicity, DryRun rollback, error
// rollback guarantees, infinite-loop timeout preemption, and zero host/OS access.
package codemode_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/adapter/codemode"
	"github.com/mindmain/flowsk/internal/adapter/db/sqlite"
	"github.com/mindmain/flowsk/internal/adapter/mdstore"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/project"
	"github.com/mindmain/flowsk/internal/domain/task"
	"github.com/mindmain/flowsk/internal/domain/workspace"
)

func setupCodeModeTestEnv(t *testing.T) (*codemode.Engine, *sqlite.Adapter, context.Context, uuid.UUID, uuid.UUID) {
	t.Helper()
	adapter, err := sqlite.NewAdapter(sqlite.Config{
		FilePath:    ":memory:",
		EnableWAL:   true,
		ForeignKeys: true,
	})
	if err != nil {
		t.Fatalf("sqlite.NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })

	ctx := context.Background()
	if _, err := adapter.Migrations().Up(ctx); err != nil {
		t.Fatalf("migrations up: %v", err)
	}

	wsSvc := workspace.NewService(adapter.WorkspaceRepository(), adapter.TxManager())
	projSvc := project.NewService(adapter.ProjectRepository(), adapter.TxManager())

	wsID := uuid.New()
	actorID := uuid.New()
	authCtx := authctx.WithIdentity(ctx, authctx.Identity{
		WorkspaceID: wsID,
		ActorID:     actorID,
		Kind:        authctx.ActorKindAgent,
		Handle:      "claude-codemode",
	})

	ws, err := wsSvc.CreateWorkspace(authCtx, workspace.CreateWorkspaceInput{
		Slug: "default",
		Name: "Default",
	})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	wsID = ws.ID
	authCtx = authctx.WithIdentity(ctx, authctx.Identity{
		WorkspaceID: wsID,
		ActorID:     actorID,
		Kind:        authctx.ActorKindAgent,
		Handle:      "claude-codemode",
	})

	proj, err := projSvc.CreateProject(authCtx, project.CreateProjectInput{
		WorkspaceID: wsID,
		Slug:        "core",
		KeyPrefix:   "CORE",
		Name:        "Core Platform",
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	eng := codemode.NewEngine(codemode.Dependencies{
		TxManager:     adapter.TxManager(),
		ProjectRepo:   adapter.ProjectRepository(),
		ObjectiveRepo: adapter.ObjectiveRepository(),
		TaskRepo:      adapter.TaskRepository(),
	})
	return eng, adapter, authCtx, wsID, proj.ID
}

// TestCodeModeEngine_EndToEndJSWorkflowAndSearch verifies that an AI agent can
// write idiomatic ES6 JavaScript using `flowsk.goals`, `flowsk.tasks`, loops,
// filtering, and `flowsk.search(...)` in a single atomic transaction.
func TestCodeModeEngine_EndToEndJSWorkflowAndSearch(t *testing.T) {
	t.Parallel()
	eng, _, ctx, wsID, projID := setupCodeModeTestEnv(t)

	script := `
		const goal = flowsk.goals.create({
			title: "Cloudflare Code Mode Rollout",
			priority: "high",
			targetDate: "2026-12-31"
		});
		const t1 = flowsk.tasks.create({
			title: "Embed pure-Go Goja sandbox",
			objective: goal.key,
			priority: "urgent",
			tags: ["codemode", "security"],
			acceptance: "Zero OS/network globals exposed"
		});
		const t2 = flowsk.tasks.create({
			title: "Add GitHub-style flowsk_search tool",
			objective: goal.key,
			priority: "high",
			dependsOn: [t1.key],
			tags: ["search", "mcp"]
		});

		flowsk.tasks.claim(t1.key, 1800);
		flowsk.tasks.comment(t1.key, "Sandbox verified with timeout interrupt", "progress");
		flowsk.tasks.done(t1.key, "All tests green");

		// Claim next ready task (should now be t2 since t1 is done!)
		const next = flowsk.tasks.next({ claim: true, ttlSeconds: 1200 });
		const doneTasks = flowsk.tasks.search("state:done #codemode");
		const inProgTasks = flowsk.tasks.search("state:in_progress");

		console.log("completed:", doneTasks.map(x => x.key).join(","));
		return {
			goalKey: goal.key,
			doneCount: doneTasks.length,
			doneKey: doneTasks[0].key,
			nextClaimedKey: next.key,
			inProgressCount: inProgTasks.length
		};
	`

	res, err := eng.Execute(ctx, codemode.ExecuteInput{
		WorkspaceID:      wsID,
		DefaultProjectID: &projID,
		Script:           script,
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !res.Committed || res.DryRun {
		t.Fatalf("expected Committed=true DryRun=false, got %+v", res)
	}
	if !strings.Contains(res.Summary, "CORE-1") || !strings.Contains(res.Summary, "CORE-2") {
		t.Fatalf("expected CORE-1 and CORE-2 in summary, got:\n%s", res.Summary)
	}
	if len(res.Console) != 1 || !strings.Contains(res.Console[0], "completed: CORE-1") {
		t.Fatalf("unexpected console logs: %+v", res.Console)
	}
}

// TestCodeModeEngine_DryRunAndErrorRollbackGuarantees verifies that DryRun
// simulates full JS execution without persisting writes, and runtime JS errors
// roll back all earlier mutations in the same script.
func TestCodeModeEngine_DryRunAndErrorRollbackGuarantees(t *testing.T) {
	t.Parallel()
	eng, adapter, ctx, wsID, projID := setupCodeModeTestEnv(t)

	// 1. DryRun creates tasks and returns keys, then rolls back cleanly.
	dryRes, err := eng.Execute(ctx, codemode.ExecuteInput{
		WorkspaceID:      wsID,
		DefaultProjectID: &projID,
		DryRun:           true,
		Script: `
			const a = flowsk.tasks.create({ title: "Simulated task", priority: "medium" });
			return a.key;
		`,
	})
	if err != nil {
		t.Fatalf("DryRun Execute failed: %v", err)
	}
	if dryRes.Committed || !dryRes.DryRun || !strings.Contains(dryRes.Summary, "CORE-1") {
		t.Fatalf("unexpected DryRun result: %+v", dryRes)
	}

	taskSvc := task.NewService(adapter.TaskRepository(), adapter.TxManager())
	remaining, err := taskSvc.PollWorkQueue(ctx, task.AgentWorkQueueFilter{WorkspaceID: wsID, ProjectID: projID, Limit: 50})
	if err != nil || len(remaining) != 0 {
		t.Fatalf("expected 0 persisted tasks after DryRun, got len=%d err=%v", len(remaining), err)
	}

	// 2. Runtime JS exception after mutating must roll back the entire ACID transaction!
	_, err = eng.Execute(ctx, codemode.ExecuteInput{
		WorkspaceID:      wsID,
		DefaultProjectID: &projID,
		Script: `
			flowsk.tasks.create({ title: "Should roll back on throw" });
			throw new Error("simulated mid-script failure");
		`,
	})
	if err == nil {
		t.Fatal("expected error when JS script throws")
	}
	remaining, err = taskSvc.PollWorkQueue(ctx, task.AgentWorkQueueFilter{WorkspaceID: wsID, ProjectID: projID, Limit: 50})
	if err != nil || len(remaining) != 0 {
		t.Fatalf("expected 0 persisted tasks after JS throw rollback, got len=%d err=%v", len(remaining), err)
	}
}

// TestCodeModeEngine_SandboxIsolationAndInfiniteLoopTimeout verifies that
// dangerous host globals (`process`, `require`, `fetch`) are undefined and
// runaway loops (`while (true) {}`) are preempted cleanly by Timeout.
func TestCodeModeEngine_SandboxIsolationAndInfiniteLoopTimeout(t *testing.T) {
	t.Parallel()
	eng, _, ctx, wsID, projID := setupCodeModeTestEnv(t)

	isoRes, err := eng.Execute(ctx, codemode.ExecuteInput{
		WorkspaceID:      wsID,
		DefaultProjectID: &projID,
		Script: `
			return {
				hasProcess: typeof process !== "undefined",
				hasRequire: typeof require !== "undefined",
				hasFetch: typeof fetch !== "undefined"
			};
		`,
	})
	if err != nil {
		t.Fatalf("isolation check failed: %v", err)
	}
	if strings.Contains(isoRes.Summary, "true") {
		t.Fatalf("expected zero Node/browser host globals in sandbox, got: %s", isoRes.Summary)
	}

	start := time.Now()
	_, err = eng.Execute(ctx, codemode.ExecuteInput{
		WorkspaceID:      wsID,
		DefaultProjectID: &projID,
		Timeout:          80 * time.Millisecond,
		Script:           `while (true) {}`,
	})
	if err == nil {
		t.Fatal("expected timeout error on infinite loop")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("expected infinite loop interrupt within 2s, took %s", elapsed)
	}

	if err := codemode.ValidateSyntax(`const t = flowsk.tasks.create({ title: "Valid ES6" }); return t.key;`); err != nil {
		t.Fatalf("expected valid ES6 syntax to pass ValidateSyntax, got: %v", err)
	}
	if err := codemode.ValidateSyntax(`const = ;`); err == nil {
		t.Fatal("expected invalid ES6 syntax to fail ValidateSyntax")
	}
}

func TestCodeModeEngine_MarkdownDocsAndGoalsLifecycle(t *testing.T) {
	t.Parallel()
	_, adapter, ctx, wsID, projID := setupCodeModeTestEnv(t)

	mdRoot := t.TempDir()
	store, err := mdstore.NewFileStore(mdRoot)
	if err != nil {
		t.Fatalf("mdstore.NewFileStore: %v", err)
	}

	eng := codemode.NewEngine(codemode.Dependencies{
		TxManager:     adapter.TxManager(),
		ProjectRepo:   adapter.ProjectRepository(),
		ObjectiveRepo: adapter.ObjectiveRepository(),
		TaskRepo:      adapter.TaskRepository(),
		MarkdownStore: store,
	})

	// 1. DryRun with markdown attachment must NOT write any .md file to disk!
	_, err = eng.Execute(ctx, codemode.ExecuteInput{
		WorkspaceID:      wsID,
		DefaultProjectID: &projID,
		DryRun:           true,
		Script: `
			const t = flowsk.tasks.create({
				title: "Simulated spec task",
				state: "backlog",
				markdown: "# Simulated Spec\nShould never hit disk"
			});
			return flowsk.tasks.getDoc(t.key);
		`,
	})
	if err != nil {
		t.Fatalf("DryRun with markdown failed: %v", err)
	}
	scanAfterDry, err := store.ScanTaskMarkdown(ctx, wsID)
	if err != nil || len(scanAfterDry.ValidIDs) != 0 {
		t.Fatalf("expected 0 disk .md files after DryRun, got %+v (err=%v)", scanAfterDry, err)
	}

	// 2. Committed script creates milestone, updates status to at_risk, creates backlog task with .md, updates .md, and filters is:md.
	commitRes, err := eng.Execute(ctx, codemode.ExecuteInput{
		WorkspaceID:      wsID,
		DefaultProjectID: &projID,
		Script: `
			const ms = flowsk.goals.create({
				title: "Real-Time Streaming v1",
				kind: "milestone",
				priority: "urgent",
				successCriteria: ["SSE hub live", "Structured Goals UI"]
			});
			const updatedMs = flowsk.goals.update(ms.key, { status: "at_risk" });

			// 1-arg flowsk.projects.setDoc(markdown) uses the active session project, and 2-arg explicit key also works.
			flowsk.projects.setDoc("# Initial Charter");
			flowsk.projects.setDoc("CORE", "# Core Architecture\n\nGo channels + SSE");
			const t1 = flowsk.tasks.create({
				title: "Implement Go channel EventBroker",
				state: "backlog",
				objective: ms.key,
				markdown: "# EventBroker Spec\n\n- Buffered per-workspace channels"
			});
			const docBefore = flowsk.tasks.getDoc(t1.key);
			flowsk.tasks.setDoc(t1.key, docBefore.markdown + "\n- Heartbeat ping frames");
			const mdTasks = flowsk.tasks.search("is:md");

			return {
				milestoneKind: updatedMs.kind,
				milestoneStatus: updatedMs.status,
				taskKey: t1.key,
				hasDoc: t1.hasMarkdownDoc,
				mdMatchCount: mdTasks.length,
				projDoc: flowsk.projects.getDoc("CORE").hasMarkdownDoc
			};
		`,
	})
	if err != nil {
		t.Fatalf("Committed Execute failed: %v", err)
	}
	for _, want := range []string{`"milestoneKind":"milestone"`, `"milestoneStatus":"at_risk"`, `"mdMatchCount":1`, `"projDoc":true`} {
		if !strings.Contains(commitRes.Summary, want) {
			t.Fatalf("expected %s in summary, got:\n%s", want, commitRes.Summary)
		}
	}

	scanAfterCommit, err := store.ScanTaskMarkdown(ctx, wsID)
	if err != nil || len(scanAfterCommit.ValidIDs) != 1 {
		t.Fatalf("expected 1 persisted task .md file on disk, got %+v (err=%v)", scanAfterCommit, err)
	}
	diskContent, err := store.ReadTaskMarkdown(ctx, wsID, scanAfterCommit.ValidIDs[0])
	if err != nil || !strings.Contains(diskContent, "Heartbeat ping frames") {
		t.Fatalf("unexpected persisted task markdown content: %q (err=%v)", diskContent, err)
	}
}

// TestCodeModeEngine_ArchivedSearchLargeProjectAndAssigneeFallback verifies:
//   - >100 tasks in a project are not truncated in tasks.list(), goals.list(), or flowsk.board()
//   - StatusArchived is excluded on unfiltered searches/boards and included when explicitly requested
//   - @actor / assignee: filters match task.AssigneeActorID via GetTask fallback when unclaimed
func TestCodeModeEngine_ArchivedSearchLargeProjectAndAssigneeFallback(t *testing.T) {
	t.Parallel()
	eng, adapter, ctx, wsID, projID := setupCodeModeTestEnv(t)

	otherAssignee := uuid.New()
	taskSvc := task.NewService(adapter.TaskRepository(), adapter.TxManager())
	assignedTask, err := taskSvc.CreateTask(ctx, task.CreateTaskInput{
		WorkspaceID:     wsID,
		ProjectID:       projID,
		Kind:            task.KindFeature,
		Title:           "Pre-assigned unclaimed task",
		Status:          task.StatusReady,
		Priority:        task.PriorityHigh,
		AssigneeActorID: &otherAssignee,
	})
	if err != nil {
		t.Fatalf("CreateTask assigned: %v", err)
	}

	res, err := eng.Execute(ctx, codemode.ExecuteInput{
		WorkspaceID:      wsID,
		DefaultProjectID: &projID,
		Timeout:          20 * time.Second,
		Script: `
			const g = flowsk.goals.create({ title: "Scale Past 100 Tasks" });
			for (let i = 0; i < 110; i++) {
				flowsk.tasks.create({
					title: "Bulk task " + i,
					objective: g.key,
					state: i < 5 ? "done" : "ready",
					tags: ["bulk"]
				});
			}
			const selfAssigned = flowsk.tasks.create({
				title: "Self-assigned task",
				assignee: "claude-codemode"
			});
			const arch = flowsk.tasks.create({ title: "Archived historical task" });
			flowsk.tasks.move(arch.key, "archived", "Done long ago");

			const openList = flowsk.tasks.list();
			const archState = flowsk.tasks.search("state:archived");
			const archIs = flowsk.tasks.search("is:archived");
			const archList = flowsk.tasks.list({ state: "archived" });
			const goals = flowsk.goals.list();
			const byUUID = flowsk.tasks.search("assignee:` + otherAssignee.String() + `");
			const byHandle = flowsk.tasks.search("@claude-codemode");

			return {
				openCount: openList.length,
				archStateCount: archState.length,
				archIsCount: archIs.length,
				archListCount: archList.length,
				goalTotal: goals[0].totalTasks,
				goalCompleted: goals[0].completedTasks,
				byUUIDKey: byUUID.length === 1 ? byUUID[0].key : "",
				byHandleKey: byHandle.length === 1 ? byHandle[0].key : "",
				selfKey: selfAssigned.key
			};
		`,
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	outMap, ok := res.Value.(map[string]any)
	if !ok {
		t.Fatalf("expected map Value, got %T: %+v", res.Value, res.Value)
	}
	// 1 pre-assigned + 110 bulk + 1 self-assigned = 112 non-archived tasks (archived excluded by default!).
	if outMap["openCount"] != int64(112) {
		t.Fatalf("expected openCount=112 (>100 without truncation, excluding archived), got %v", outMap["openCount"])
	}
	if outMap["archStateCount"] != int64(1) || outMap["archIsCount"] != int64(1) || outMap["archListCount"] != int64(1) {
		t.Fatalf("expected archived search counts to equal 1, got %+v", outMap)
	}
	if outMap["goalTotal"] != int64(110) || outMap["goalCompleted"] != int64(5) {
		t.Fatalf("expected goal rollup 5/110 without truncation, got %+v", outMap)
	}
	if outMap["byUUIDKey"] != assignedTask.Key {
		t.Fatalf("expected assignee UUID search to match %s, got %v", assignedTask.Key, outMap["byUUIDKey"])
	}
	if outMap["byHandleKey"] != outMap["selfKey"] {
		t.Fatalf("expected @claude-codemode search to match %v, got %v", outMap["selfKey"], outMap["byHandleKey"])
	}
}

// TestCodeModeEngine_CaughtDomainErrorAndCyclicReturnRollback verifies:
//   - Caught SDK errors inside JS try/catch do not leak as stale domain errors when a plain JS Error is thrown later
//   - Cyclic JS return objects fail JSON validation inside the transaction and roll back all writes
func TestCodeModeEngine_CaughtDomainErrorAndCyclicReturnRollback(t *testing.T) {
	t.Parallel()
	eng, adapter, ctx, wsID, projID := setupCodeModeTestEnv(t)

	// 1. Caught ErrNotFound followed by plain JS Error must return ErrValidation, NOT ErrNotFound.
	_, err := eng.Execute(ctx, codemode.ExecuteInput{
		WorkspaceID:      wsID,
		DefaultProjectID: &projID,
		Script: `
			try {
				flowsk.tasks.get("CORE-9999");
			} catch (ignored) {}
			throw new Error("plain custom js failure");
		`,
	})
	if err == nil {
		t.Fatal("expected error from plain JS throw")
	}
	if errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("stale ErrNotFound leaked across JS try/catch: %v", err)
	}
	if !errors.Is(err, errs.ErrValidation) || !strings.Contains(err.Error(), "plain custom js failure") {
		t.Fatalf("expected ErrValidation with plain JS message, got: %v", err)
	}

	// 2. Uncaught SDK ErrNotFound still propagates ErrNotFound.
	_, err = eng.Execute(ctx, codemode.ExecuteInput{
		WorkspaceID:      wsID,
		DefaultProjectID: &projID,
		Script:           `return flowsk.tasks.get("CORE-9999");`,
	})
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("expected uncaught tasks.get to return ErrNotFound, got: %v", err)
	}

	// 3. Cyclic return object must fail inside WithTransaction and roll back earlier mutations.
	_, err = eng.Execute(ctx, codemode.ExecuteInput{
		WorkspaceID:      wsID,
		DefaultProjectID: &projID,
		Script: `
			flowsk.tasks.create({ title: "Must roll back on cyclic return" });
			const cyc = { ok: true };
			cyc.self = cyc;
			return cyc;
		`,
	})
	if !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("expected ErrValidation for cyclic return value, got: %v", err)
	}
	taskSvc := task.NewService(adapter.TaskRepository(), adapter.TxManager())
	remaining, qErr := taskSvc.PollWorkQueue(ctx, task.AgentWorkQueueFilter{WorkspaceID: wsID, ProjectID: projID, Limit: 50})
	if qErr != nil || len(remaining) != 0 {
		t.Fatalf("expected 0 tasks after cyclic return rollback, got len=%d err=%v", len(remaining), qErr)
	}
}

// TestCodeModeEngine_SDKParameterAndErrorFidelity verifies project key normalization,
// RFC3339 & clearable targetDate, unfabricated & clearable slices, selective tasks.unblock,
// tasks.block status transition, upsertExternal kind/objective/parent/dependsOn,
// goals.close note, tasks.ready(limit), tasks.list({limit}), and flowsk.board(opts).
func TestCodeModeEngine_SDKParameterAndErrorFidelity(t *testing.T) {
	t.Parallel()
	eng, _, ctx, wsID, projID := setupCodeModeTestEnv(t)

	// Malformed targetDate must fail with ErrValidation instead of silently dropping.
	_, badDateErr := eng.Execute(ctx, codemode.ExecuteInput{
		WorkspaceID:      wsID,
		DefaultProjectID: &projID,
		Script:           `return flowsk.goals.create({ title: "Bad Date", targetDate: "not-a-date" });`,
	})
	if !errors.Is(badDateErr, errs.ErrValidation) {
		t.Fatalf("expected ErrValidation for malformed targetDate, got: %v", badDateErr)
	}

	// Missing or undefined markdown arguments in setDoc must throw a TypeError and roll back!
	for _, badDocScript := range []string{
		`return flowsk.projects.setDoc();`,
		`return flowsk.projects.setDoc("CORE", undefined);`,
		`return flowsk.projects.setDoc(123);`,
		`const t = flowsk.tasks.create({ title: "Doc Check" }); return flowsk.tasks.setDoc(t.key);`,
		`const t = flowsk.tasks.create({ title: "Doc Check" }); return flowsk.tasks.setDoc(t.key, undefined);`,
	} {
		if _, badDocErr := eng.Execute(ctx, codemode.ExecuteInput{
			WorkspaceID:      wsID,
			DefaultProjectID: &projID,
			Script:           badDocScript,
		}); !errors.Is(badDocErr, errs.ErrValidation) {
			t.Fatalf("expected ErrValidation (JS TypeError) for %q, got: %v", badDocScript, badDocErr)
		}
	}

	res, err := eng.Execute(ctx, codemode.ExecuteInput{
		WorkspaceID:      wsID,
		DefaultProjectID: &projID,
		Timeout:          20 * time.Second,
		Script: `
			// 1. Auto-generated project keys normalize safely, and priority scale (0..4) is unified across goals & tasks.
			const p1 = flowsk.projects.create({ name: "v1-api" });
			const p2 = flowsk.projects.create({ name: "123-svc" });
			const p3 = flowsk.projects.create({ name: "x" });
			const prioGoalNum = flowsk.goals.create({ title: "Numeric P1 Goal", priority: 1 });
			const prioGoalStr = flowsk.goals.create({ title: "Urgent P1 Goal", priority: "urgent" });
			const prioTaskNum = flowsk.tasks.create({ title: "Numeric P1 Task", priority: 1 });
			const prioTaskP1 = flowsk.tasks.create({ title: "Token P1 Task", priority: "p1" });
			const prioTaskUrg = flowsk.tasks.create({ title: "Urgent P1 Task", priority: "urgent" });
			const prioTaskNone = flowsk.tasks.create({ title: "Unprioritized Task", priority: 0 });

			// 2. Goals: omitted successCriteria stays empty, RFC3339 targetDate works, "" clears targetDate, [] clears criteria, close records note.
			const g1 = flowsk.goals.create({
				title: "Q4 API Stability",
				targetDate: "2026-12-31T23:59:59Z"
			});
			const g1InitialCriteriaLen = g1.successCriteria.length;
			const g1InitialTarget = g1.targetDate;
			const g1WithCriteria = flowsk.goals.update(g1.key, {
				successCriteria: ["Zero 5xx"],
				targetDate: ""
			});
			const g1Cleared = flowsk.goals.update(g1.key, {
				successCriteria: []
			});
			const g2 = flowsk.goals.create({ title: "Secondary Goal" });

			// 3. upsertExternal respects kind, objective, parent, dependsOn, and slice clearing.
			const epic = flowsk.tasks.create({ title: "Parent Epic", kind: "epic", objective: g1.key });
			const depA = flowsk.tasks.create({ title: "Blocker A", objective: g1.key });
			const depB = flowsk.tasks.create({ title: "Blocker B", objective: g2.key });

			const extCreated = flowsk.tasks.upsertExternal({
				externalId: "LIN-100",
				kind: "bug",
				title: "External bug",
				tags: ["sync", "bug"],
				acceptance: ["Check A"]
			});
			const extUpdated = flowsk.tasks.upsertExternal({
				externalId: "LIN-100",
				kind: "chore",
				title: "External chore",
				objective: g1.key,
				parent: epic.key,
				dependsOn: [depA.key],
				tags: [],
				acceptance: []
			});

			// 4. Selective dependency block & unblock (keeps workflow status "ready" with blocked=true) vs external impediment block (transitions to "blocked").
			const target = flowsk.tasks.create({ title: "Target work", objective: g1.key });
			const blockedTarget = flowsk.tasks.block(target.key, "Waiting on A and B", [depA.key, depB.key]);
			const afterPartialUnblock = flowsk.tasks.unblock(target.key, [depA.key]);
			const afterFullUnblock = flowsk.tasks.unblock(target.key, [depB.key]);
			const impedimentTarget = flowsk.tasks.create({ title: "External Impediment", objective: g1.key });
			const externallyBlocked = flowsk.tasks.block(impedimentTarget.key, "Waiting on third-party API credentials");
			const externallyUnblocked = flowsk.tasks.unblock(impedimentTarget.key);

			// 5. Ready limit, list limit, board options, and goals.close(note).
			const readyLimited = flowsk.tasks.ready(2);
			const listLimited = flowsk.tasks.list({ limit: 2 });
			flowsk.tasks.done(depB.key, "Finished B");
			const boardOpen = flowsk.board({ includeCompleted: false });
			const boardAll = flowsk.board({ includeCompleted: true });
			const boardObj = flowsk.board({ includeCompleted: true, objective: g2.key });
			const closedGoal = flowsk.goals.close(g2.key, "Shipped secondary milestone");

			return {
				p1Key: p1.key,
				p2Key: p2.key,
				p3Key: p3.key,
				prioUnifiedOk: prioGoalNum.priority === 1 && prioGoalStr.priority === 1 && prioTaskNum.priority === 1 && prioTaskP1.priority === 1 && prioTaskUrg.priority === 1 && prioTaskNone.priority === 0,
				g1InitialCriteriaLen,
				g1InitialTarget,
				g1ClearedTarget: g1WithCriteria.targetDate,
				g1ClearedCriteriaLen: g1Cleared.successCriteria.length,
				extCreatedKind: extCreated.kind,
				extUpdatedKind: extUpdated.kind,
				extUpdatedObj: extUpdated.objective,
				extUpdatedParent: extUpdated.parent,
				extUpdatedBlocked: extUpdated.blocked,
				extUpdatedTagsLen: extUpdated.tags.length,
				extUpdatedAccLen: extUpdated.acceptance.length,
				blockedStatus: blockedTarget.status,
				blockedFlag: blockedTarget.blocked,
				partialUnblockBlocked: afterPartialUnblock.blocked,
				fullUnblockBlocked: afterFullUnblock.blocked,
				fullUnblockStatus: afterFullUnblock.status,
				externalBlockedStatus: externallyBlocked.status,
				externalUnblockedStatus: externallyUnblocked.status,
				readyLen: readyLimited.length,
				listLen: listLimited.length,
				boardOpenCount: boardOpen.tasks.length,
				boardAllCount: boardAll.tasks.length,
				boardObjTasksCount: boardObj.tasks.length,
				closedStatus: closedGoal.status,
				closedSummary: closedGoal.summary
			};
		`,
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	m := res.Value.(map[string]any)
	if m["p1Key"] != "V1API" || m["p2Key"] != "P123SVC" || m["p3Key"] != "PRJ" {
		t.Fatalf("unexpected normalized project keys: p1=%v p2=%v p3=%v", m["p1Key"], m["p2Key"], m["p3Key"])
	}
	if m["g1InitialCriteriaLen"] != int64(0) || m["g1InitialTarget"] != "2026-12-31" || m["g1ClearedTarget"] != nil || m["g1ClearedCriteriaLen"] != int64(0) {
		t.Fatalf("unexpected goal targetDate/criteria lifecycle: %+v", m)
	}
	if m["extCreatedKind"] != "bug" || m["extUpdatedKind"] != "chore" || m["extUpdatedObj"] != "q4-api-stability" ||
		m["extUpdatedBlocked"] != true || m["extUpdatedTagsLen"] != int64(0) || m["extUpdatedAccLen"] != int64(0) {
		t.Fatalf("unexpected upsertExternal behavior: %+v", m)
	}
	if m["prioUnifiedOk"] != true {
		t.Fatalf("expected unified 0..4 priority scale across goals and tasks, got %+v", m)
	}
	if m["blockedStatus"] != "ready" || m["blockedFlag"] != true || m["partialUnblockBlocked"] != true ||
		m["fullUnblockBlocked"] != false || m["fullUnblockStatus"] != "ready" ||
		m["externalBlockedStatus"] != "blocked" || m["externalUnblockedStatus"] != "ready" {
		t.Fatalf("unexpected block/unblock transitions: %+v", m)
	}
	if m["readyLen"] != int64(2) || m["listLen"] != int64(2) {
		t.Fatalf("expected ready(2) and list({limit:2}) to return 2 items, got ready=%v list=%v", m["readyLen"], m["listLen"])
	}
	if m["boardAllCount"].(int64) <= m["boardOpenCount"].(int64) || m["boardObjTasksCount"] != int64(1) {
		t.Fatalf("unexpected board filtering: open=%v all=%v obj=%v", m["boardOpenCount"], m["boardAllCount"], m["boardObjTasksCount"])
	}
	if m["closedStatus"] != "achieved" || !strings.Contains(m["closedSummary"].(string), "Shipped secondary milestone") {
		t.Fatalf("unexpected goals.close result: status=%v summary=%v", m["closedStatus"], m["closedSummary"])
	}

	// Verify lastSignalAt lifecycle in Code Mode: create sets it, move to in_review updates it, done freezes it.
	sigCreateRes, err := eng.Execute(ctx, codemode.ExecuteInput{
		WorkspaceID:      wsID,
		DefaultProjectID: &projID,
		Script:           `return flowsk.tasks.create({ title: "Signal Tracked Task", state: "ready" });`,
	})
	if err != nil {
		t.Fatalf("Execute signal create failed: %v", err)
	}
	sigMap := sigCreateRes.Value.(map[string]any)
	taskKey, _ := sigMap["key"].(string)
	sig0, _ := sigMap["lastSignalAt"].(string)
	if sig0 == "" {
		t.Fatalf("expected non-empty lastSignalAt on created task, got %+v", sigMap)
	}

	time.Sleep(2 * time.Millisecond)
	sigReviewRes, err := eng.Execute(ctx, codemode.ExecuteInput{
		WorkspaceID:      wsID,
		DefaultProjectID: &projID,
		Script:           fmt.Sprintf(`return flowsk.tasks.move(%q, "in_review");`, taskKey),
	})
	if err != nil {
		t.Fatalf("Execute signal move(in_review) failed: %v", err)
	}
	sig1, _ := sigReviewRes.Value.(map[string]any)["lastSignalAt"].(string)
	if sig1 == "" || sig1 == sig0 {
		t.Fatalf("expected move(in_review) to advance lastSignalAt (was %q, got %q)", sig0, sig1)
	}

	time.Sleep(2 * time.Millisecond)
	sigDoneRes, err := eng.Execute(ctx, codemode.ExecuteInput{
		WorkspaceID:      wsID,
		DefaultProjectID: &projID,
		Script:           fmt.Sprintf(`return flowsk.tasks.done(%q, "Verified");`, taskKey),
	})
	if err != nil {
		t.Fatalf("Execute signal done failed: %v", err)
	}
	sig2, _ := sigDoneRes.Value.(map[string]any)["lastSignalAt"].(string)
	if sig2 != sig1 {
		t.Fatalf("expected tasks.done NOT to advance lastSignalAt (want %q, got %q)", sig1, sig2)
	}
}
