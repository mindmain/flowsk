// File engine_test.go verifies the Cloudflare Code Mode pure-Go sandboxed
// JavaScript execution engine (`dop251/goja`): zero-guide ES6 scripting against
// the `flowsk` SDK, single-transaction ACID atomicity, DryRun rollback, error
// rollback guarantees, infinite-loop timeout preemption, and zero host/OS access.
package codemode_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/adapter/codemode"
	"github.com/mindmain/flowsk/internal/adapter/db/sqlite"
	"github.com/mindmain/flowsk/internal/adapter/mdstore"
	"github.com/mindmain/flowsk/internal/domain/authctx"
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
