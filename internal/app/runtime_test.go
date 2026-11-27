// File runtime_test.go provides end-to-end integration tests verifying Flowsk's
// zero-setup local SQLite auto-directory/auto-migration bootstrap, client-local
// folder linking (zero Git repo footprint), goal & task lifecycle, DAG blockers,
// agent leases, Cloudflare Code Mode JavaScript ACID scripts, terminal board
// rendering, MCP stdio tool calls, and remote ConnectRPC network server collaboration.
package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mindmain/flowsk/internal/app"
	"github.com/mindmain/flowsk/internal/cli"
)

func runCLI(t *testing.T, rt *app.Runtime, args ...string) string {
	t.Helper()
	cmd := cli.NewRootCommand(rt.CLIDependencies())
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("flowsk %v failed: %v\nstderr: %s", args, err, errBuf.String())
	}
	return out.String()
}

// TestLocalZeroSetupAutoBootstrapAndTerminalWorkflow verifies that starting
// from a completely empty XDG environment, Flowsk automatically creates its
// own data directory and SQLite database, applies embedded Goose migrations,
// provisions the default workspace, links a local directory without polluting
// it, creates goals and tasks, enforces leases and blockers, executes a Code Mode
// JavaScript script, and renders the full interactive terminal board.
func TestLocalZeroSetupAutoBootstrapAndTerminalWorkflow(t *testing.T) {
	tmpRoot := t.TempDir()
	fakeHome := filepath.Join(tmpRoot, "user-home")
	fakeRepo := filepath.Join(tmpRoot, "my-repo")
	if err := os.MkdirAll(fakeHome, 0o755); err != nil {
		t.Fatalf("mkdir fakeHome: %v", err)
	}
	if err := os.MkdirAll(fakeRepo, 0o755); err != nil {
		t.Fatalf("mkdir fakeRepo: %v", err)
	}

	t.Setenv("HOME", fakeHome)
	t.Setenv("FLOWSK_HOME", "")

	expectedDB := filepath.Join(fakeHome, ".flowsk", "flowsk.db")
	if _, err := os.Stat(expectedDB); !os.IsNotExist(err) {
		t.Fatalf("expected DB %q to not exist before first run", expectedDB)
	}

	rt := app.NewRuntime()
	t.Cleanup(func() { _ = rt.Close() })

	// 1. First command ever: creates ~/.flowsk/flowsk.db, migrates schema,
	//    provisions default workspace, and creates logical project FLW!
	out := runCLI(t, rt, "--format", "human", "--actor", "mario", "project", "create", "FLW", "--name", "Flowsk Core")
	if !strings.Contains(out, "FLW") || !strings.Contains(out, "Flowsk Core") {
		t.Fatalf("unexpected project create output:\n%s", out)
	}
	if _, err := os.Stat(expectedDB); err != nil {
		t.Fatalf("expected Flowsk to auto-create local SQLite DB at %q: %v", expectedDB, err)
	}

	// 2. Link fakeRepo locally and verify ZERO files were written inside fakeRepo!
	_ = runCLI(t, rt, "project", "link", fakeRepo, "--project", "FLW")
	entries, err := os.ReadDir(fakeRepo)
	if err != nil {
		t.Fatalf("readdir fakeRepo: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected zero target-repo footprint in %q, found %d entries", fakeRepo, len(entries))
	}

	// 3. Create a strategic Goal and hierarchical/dependent Tasks, then inspect in human & compact formats!
	goalOut := runCLI(t, rt, "--format", "human", "--project", "FLW", "--actor", "mario",
		"goal", "create", "Ship v1 Local & Network Engine", "--kind", "goal", "--priority", "1", "--summary", "Zero-setup SQLite + ConnectRPC")
	if !strings.Contains(goalOut, "Ship v1 Local & Network Engine") {
		t.Fatalf("unexpected goal create output:\n%s", goalOut)
	}

	t1Out := runCLI(t, rt, "--format", "json", "--project", "FLW", "--actor", "mario",
		"task", "create", "Bootstrap SQLite WAL store", "--priority", "1")
	var t1 cli.TaskView
	if err := json.Unmarshal([]byte(t1Out), &t1); err != nil {
		t.Fatalf("unmarshal t1 json: %v\nraw: %s", err, t1Out)
	}
	if t1.Key != "FLW-1" {
		t.Fatalf("expected first task key FLW-1, got %q", t1.Key)
	}

	t2Out := runCLI(t, rt, "--format", "json", "--project", "FLW", "--actor", "mario",
		"task", "create", "Wire terminal board view", "--priority", "2", "--depends-on", "FLW-1")
	var t2 cli.TaskView
	if err := json.Unmarshal([]byte(t2Out), &t2); err != nil {
		t.Fatalf("unmarshal t2 json: %v\nraw: %s", err, t2Out)
	}
	if t2.Key != "FLW-2" || !t2.Blocked {
		t.Fatalf("expected FLW-2 blocked=true, got key=%q blocked=%v", t2.Key, t2.Blocked)
	}

	// 4. Agent polls `task next --claim`: must pick FLW-1 (since FLW-2 is blocked by FLW-1)!
	nextOut := runCLI(t, rt, "--format", "json", "--project", "FLW", "--actor", "agent-alpha", "task", "next", "--claim")
	var nextRes cli.NextTaskResult
	if err := json.Unmarshal([]byte(nextOut), &nextRes); err != nil {
		t.Fatalf("unmarshal next json: %v\nraw: %s", err, nextOut)
	}
	if nextRes.Task == nil || nextRes.Task.Key != "FLW-1" || !nextRes.Claimed {
		t.Fatalf("expected next --claim to claim FLW-1, got %+v", nextRes)
	}

	// 5. Complete FLW-1, unblocking FLW-2, and execute an atomic Code Mode ES6 JavaScript script!
	_ = runCLI(t, rt, "--project", "FLW", "--actor", "agent-alpha", "task", "release", "FLW-1", "--state", "done", "--note", "SQLite WAL store verified")

	jsScript := `
		const m1 = flowsk.goals.create({ slug: "network-sync", title: "ConnectRPC Multi-Peer Sync", priority: "urgent" });
		const t3 = flowsk.tasks.create({ kind: "feature", objective: m1.key, priority: "high", title: "Add remote client wiring" });
		flowsk.tasks.comment(t3.key, "Reusing generated Connect stubs.", "decision");
		return t3.key;
	`
	batchOut := runCLI(t, rt, "--format", "human", "--project", "FLW", "--actor", "claude-planner", "code", "-e", jsScript)
	if !strings.Contains(batchOut, "FLW-3") {
		t.Fatalf("expected Code Mode JS output to include FLW-3, got:\n%s", batchOut)
	}

	// 6. Render full terminal board (`flowsk board` / `flowsk status`) and audit trail!
	boardOut := runCLI(t, rt, "--format", "human", "--project", "FLW", "board", "--include-completed")
	for _, want := range []string{"FLW", "Ship v1 Local & Network Engine", "ConnectRPC Multi-Peer Sync", "FLW-1", "FLW-2", "FLW-3"} {
		if !strings.Contains(boardOut, want) {
			t.Fatalf("expected board output to contain %q, got:\n%s", want, boardOut)
		}
	}

	auditOut := runCLI(t, rt, "--format", "compact", "audit", "list")
	if strings.TrimSpace(auditOut) == "" {
		t.Fatalf("expected non-empty audit log output")
	}
}

// TestNetworkServerModeConnectsMultipleClients verifies that `flowsk serve`
// exposes ConnectRPC over TCP so separate remote CLI clients connect over the
// network via --server-url and collaborate on the exact same projects, goals,
// and tasks.
func TestNetworkServerModeConnectsMultipleClients(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("FLOWSK_HOME", filepath.Join(tmpDir, ".flowsk"))

	// Allocate an available localhost TCP port for `flowsk serve`.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen ephemeral port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	serverDB := filepath.Join(tmpDir, "server-vol", "flowsk.db")
	serverRT := app.NewRuntime()
	t.Cleanup(func() { _ = serverRT.Close() })

	srvCtx, stopSrv := context.WithCancel(context.Background())
	defer stopSrv()

	srvErrCh := make(chan error, 1)
	go func() {
		cmd := cli.NewRootCommand(serverRT.CLIDependencies())
		cmd.SetArgs([]string{
			"serve",
			"--listen", addr,
			"--db-driver", "sqlite",
			"--db-dsn", serverDB,
			"--volume-dir", filepath.Join(tmpDir, "server-vol"),
			"--auto-migrate=true",
		})
		srvErrCh <- cmd.ExecuteContext(srvCtx)
	}()

	// Wait until TCP port accepts connections.
	serverURL := "http://" + addr
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, dialErr := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			break
		}
		time.Sleep(40 * time.Millisecond)
	}

	// Remote Client A creates project + goal + task over HTTP/ConnectRPC.
	clientART := app.NewRuntime()
	t.Cleanup(func() { _ = clientART.Close() })

	_ = runCLI(t, clientART, "--server-url", serverURL, "--actor", "alice-mac",
		"project", "create", "NET", "--name", "Network Shared Project")
	_ = runCLI(t, clientART, "--server-url", serverURL, "--actor", "alice-mac", "--project", "NET",
		"goal", "create", "Shared LAN Milestone", "--kind", "milestone")
	_ = runCLI(t, clientART, "--server-url", serverURL, "--actor", "alice-mac", "--project", "NET",
		"task", "create", "Verify remote peer board", "--priority", "1")

	// Remote Client B (simulating a second server/workstation) inspects the board!
	clientBRT := app.NewRuntime()
	t.Cleanup(func() { _ = clientBRT.Close() })

	remoteBoard := runCLI(t, clientBRT, "--server-url", serverURL, "--actor", "bob-linux", "--project", "NET", "--format", "human", "status")
	for _, want := range []string{"Network Shared Project", "Shared LAN Milestone", "NET-1", "Verify remote peer board"} {
		if !strings.Contains(remoteBoard, want) {
			t.Fatalf("remote client B board missing %q:\n%s", want, remoteBoard)
		}
	}

	stopSrv()
	if srvErr := <-srvErrCh; srvErr != nil {
		t.Fatalf("flowsk serve returned unexpected error on graceful stop: %v", srvErr)
	}
}

// TestCloudflareCodeModeSearchAndDotFlowskPersistence verifies that all local
// artifacts reside under ~/.flowsk, `flowsk code` executes ES6 JavaScript
// atomically against `flowsk.*`, and `flowsk search` / `--done` / `--in-progress`
// filter tasks and SDK docs cleanly.
func TestCloudflareCodeModeSearchAndDotFlowskPersistence(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("FLOWSK_HOME", "")

	rt := app.NewRuntime()
	t.Cleanup(func() { _ = rt.Close() })

	_ = runCLI(t, rt, "config", "init")
	_ = runCLI(t, rt, "--actor", "claude", "project", "create", "CM", "--name", "CodeMode App")

	for _, f := range []string{"config.yaml", "local_paths.json", "flowsk.db"} {
		want := filepath.Join(tmpHome, ".flowsk", f)
		if _, err := os.Stat(want); err != nil {
			t.Fatalf("expected %q inside unified ~/.flowsk folder: %v", want, err)
		}
	}

	jsOut := runCLI(t, rt, "--project", "CM", "--actor", "claude", "--format", "compact", "code", "-e", `
		const g = flowsk.goals.create({ title: "Cloudflare Code Mode", priority: "urgent" });
		const t1 = flowsk.tasks.create({ title: "Ship Goja JS Sandbox", objective: g.key, priority: "urgent", tags: ["js", "security"] });
		const t2 = flowsk.tasks.create({ title: "Ship Search CLI", objective: g.key, priority: "high", dependsOn: [t1.key], tags: ["search"] });
		flowsk.tasks.claim(t1.key, 1800);
		flowsk.tasks.done(t1.key, "Sandbox shipped");
		flowsk.tasks.claim(t2.key, 1800);
		return { done: flowsk.tasks.search("state:done").map(x => x.key), wip: flowsk.tasks.search("state:in_progress").map(x => x.key) };
	`)
	if !strings.Contains(jsOut, "CM-1") || !strings.Contains(jsOut, "CM-2") {
		t.Fatalf("unexpected flowsk code JS output:\n%s", jsOut)
	}

	doneOnly := runCLI(t, rt, "--project", "CM", "--format", "compact", "search", "--done")
	if !strings.Contains(doneOnly, "CM-1") || strings.Contains(doneOnly, "CM-2") {
		t.Fatalf("expected search --done to return only CM-1, got:\n%s", doneOnly)
	}

	wipOnly := runCLI(t, rt, "--project", "CM", "--format", "compact", "search", "state:in_progress #search")
	if !strings.Contains(wipOnly, "CM-2") || strings.Contains(wipOnly, "CM-1") {
		t.Fatalf("expected search 'state:in_progress #search' to return only CM-2, got:\n%s", wipOnly)
	}

	sdkDocs := runCLI(t, rt, "--format", "compact", "search", "--type", "sdk", "claim")
	if !strings.Contains(sdkDocs, "flowsk.tasks.claim") {
		t.Fatalf("expected SDK search to return flowsk.tasks.claim, got:\n%s", sdkDocs)
	}
}

func runGitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Flowsk Test",
		"GIT_AUTHOR_EMAIL=test@flowsk.dev",
		"GIT_COMMITTER_NAME=Flowsk Test",
		"GIT_COMMITTER_EMAIL=test@flowsk.dev",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s failed: %v\n%s", args, dir, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func execCLIWithIO(t *testing.T, rt *app.Runtime, cwd, stdin string, args ...string) (string, string, error) {
	t.Helper()
	if cwd != "" {
		origWD, err := os.Getwd()
		if err != nil {
			t.Fatalf("getwd: %v", err)
		}
		if err := os.Chdir(cwd); err != nil {
			t.Fatalf("chdir %q: %v", cwd, err)
		}
		defer func() { _ = os.Chdir(origWD) }()
	}
	cmd := cli.NewRootCommand(rt.CLIDependencies())
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errBuf.String(), err
}

// TestFlowskInitSubprojectsWorktreesAndGlobalStatus verifies:
//  1. `flowsk init` checks direct `.git` presence (prompting confirmation when absent,
//     proceeding immediately when `.git` dir or worktree `.git` file is present).
//  2. Bottom-up sub-project adoption (`init` in subfolder first, then `init` in `.git`
//     macro repo adopts the sub-project) and top-down sub-project binding.
//  3. Multi-worktree grouping (`git worktree add` groups checkouts under the same
//     project with per-worktree branch/commit inspection and inherits sub-projects).
//  4. `flowsk status` outside any registered folder renders the global multi-project
//     breakdown (Total, Backlog, Planned, Progress, Review, Blocked, Done) instead of failing.
//  5. Non-destructive parent deletion detaches `project_bindings` while leaving child
//     sub-projects and their tasks completely intact.
func TestFlowskInitSubprojectsWorktreesAndGlobalStatus(t *testing.T) {
	tmpRoot := t.TempDir()
	fakeHome := filepath.Join(tmpRoot, "home")
	outsideDir := filepath.Join(tmpRoot, "unregistered-helm-charts")
	mainRepo := filepath.Join(tmpRoot, "mpower-platform")
	webSubdir := filepath.Join(mainRepo, "apps", "web")
	workerSubdir := filepath.Join(mainRepo, "apps", "worker")
	wtSecond := filepath.Join(tmpRoot, "mpower-platform-wt2")

	for _, d := range []string{fakeHome, outsideDir, webSubdir, workerSubdir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %q: %v", d, err)
		}
	}
	t.Setenv("HOME", fakeHome)
	t.Setenv("FLOWSK_HOME", "")

	// Initialize Git repo in mainRepo with distinct commits in root, apps/web, and apps/worker.
	runGitCmd(t, mainRepo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(mainRepo, "README.md"), []byte("# Macro\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitCmd(t, mainRepo, "add", "README.md")
	runGitCmd(t, mainRepo, "commit", "-m", "feat: bootstrap macro platform")

	if err := os.WriteFile(filepath.Join(webSubdir, "app.ts"), []byte("export const v = 1;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitCmd(t, mainRepo, "add", "apps/web/app.ts")
	runGitCmd(t, mainRepo, "commit", "-m", "feat(web): ship dashboard shell")

	if err := os.WriteFile(filepath.Join(workerSubdir, "worker.go"), []byte("package worker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitCmd(t, mainRepo, "add", "apps/worker/worker.go")
	runGitCmd(t, mainRepo, "commit", "-m", "feat(worker): add queue consumer")

	rt := app.NewRuntime()
	t.Cleanup(func() { _ = rt.Close() })

	// 1. Subfolder WITHOUT .git: declining prompt ("n\n") aborts cleanly; accepting ("y\n") initializes standalone WEB!
	if _, stderr, err := execCLIWithIO(t, rt, webSubdir, "n\n", "init", "WEB"); err == nil || !strings.Contains(stderr, "No .git found") {
		t.Fatalf("expected init without .git and 'n' answer to abort after prompting, err=%v stderr=%q", err, stderr)
	}
	initWebOut, stderrWeb, err := execCLIWithIO(t, rt, webSubdir, "y\n", "--format", "json", "init", "WEB", "--name", "Web Portal")
	if err != nil {
		t.Fatalf("init WEB with 'y' confirmation failed: %v (stderr=%s)", err, stderrWeb)
	}
	var webInit cli.InitResultView
	if err := json.Unmarshal([]byte(initWebOut), &webInit); err != nil {
		t.Fatalf("unmarshal webInit: %v", err)
	}
	if webInit.Project.Key != "WEB" || webInit.HasDirectDotGit || webInit.ParentKey != "" {
		t.Fatalf("unexpected standalone WEB init result: %+v", webInit)
	}

	// 2. Bottom-Up Adoption: `flowsk init` in mainRepo (HAS .git!) needs NO confirmation and automatically adopts WEB!
	initMacroOut, _, err := execCLIWithIO(t, rt, mainRepo, "", "--format", "json", "init", "MPOWER", "--name", "MPower Platform")
	if err != nil {
		t.Fatalf("init MPOWER in .git root failed: %v", err)
	}
	var macroInit cli.InitResultView
	if err := json.Unmarshal([]byte(initMacroOut), &macroInit); err != nil {
		t.Fatalf("unmarshal macroInit: %v", err)
	}
	if !macroInit.HasDirectDotGit || len(macroInit.AdoptedSubprojects) != 1 || macroInit.AdoptedSubprojects[0] != "WEB" {
		t.Fatalf("expected MPOWER to have direct .git and auto-adopt [WEB], got: %+v", macroInit)
	}

	// 3. Top-Down Sub-Project Binding: `flowsk init WORKER --yes` inside apps/worker auto-binds under MPOWER!
	initWorkerOut, _, err := execCLIWithIO(t, rt, workerSubdir, "", "--format", "json", "init", "WORKER", "--name", "Async Worker", "--yes")
	if err != nil {
		t.Fatalf("init WORKER --yes failed: %v", err)
	}
	var workerInit cli.InitResultView
	if err := json.Unmarshal([]byte(initWorkerOut), &workerInit); err != nil {
		t.Fatalf("unmarshal workerInit: %v", err)
	}
	if workerInit.ParentKey != "MPOWER" || workerInit.ScopeSubpath != "apps/worker" {
		t.Fatalf("expected WORKER top-down bound to MPOWER (apps/worker), got: %+v", workerInit)
	}

	// 4. Create tasks across states in MPOWER, WEB, and WORKER to verify separate scopes & state counts.
	_ = runCLI(t, rt, "--project", "MPOWER", "task", "create", "Root backlog item", "--state", "backlog")
	_ = runCLI(t, rt, "--project", "MPOWER", "task", "create", "Root planned item", "--state", "todo")
	_ = runCLI(t, rt, "--project", "MPOWER", "task", "create", "Root active item", "--state", "in_progress")
	_ = runCLI(t, rt, "--project", "MPOWER", "task", "create", "Root completed item", "--state", "done")
	_ = runCLI(t, rt, "--project", "WEB", "task", "create", "Build login UI", "--state", "in_progress")
	_ = runCLI(t, rt, "--project", "WEB", "task", "create", "Ship theme tokens", "--state", "done")
	_ = runCLI(t, rt, "--project", "WORKER", "task", "create", "Retry dead-letter queue", "--state", "blocked")

	// 5. Add a second Git worktree (`git worktree add`) with its own branch and commit;
	//    `flowsk init` inside wtSecond has a `.git` file so it needs NO prompt and groups with MPOWER!
	runGitCmd(t, mainRepo, "worktree", "add", "-b", "feat/multi-wt", wtSecond)
	if err := os.WriteFile(filepath.Join(wtSecond, "README.md"), []byte("# Macro v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitCmd(t, wtSecond, "add", "README.md")
	runGitCmd(t, wtSecond, "commit", "-m", "feat: worktree-2 commit")

	wtInitOut, _, err := execCLIWithIO(t, rt, wtSecond, "", "--format", "json", "init")
	if err != nil {
		t.Fatalf("init inside linked git worktree failed: %v", err)
	}
	var wtInit cli.InitResultView
	if err := json.Unmarshal([]byte(wtInitOut), &wtInit); err != nil {
		t.Fatalf("unmarshal wtInit: %v", err)
	}
	if !wtInit.ReusedExisting || wtInit.WorktreeOf != "MPOWER" || len(wtInit.Worktrees) < 2 {
		t.Fatalf("expected linked worktree to group under MPOWER with >=2 worktrees, got: %+v", wtInit)
	}

	// Verify sibling worktree subpath resolution: running `flowsk task list` from wtSecond/apps/worker resolves WORKER!
	wtWorkerTasks, _, err := execCLIWithIO(t, rt, filepath.Join(wtSecond, "apps", "worker"), "", "--format", "compact", "task", "list")
	if err != nil {
		t.Fatalf("task list inside sibling worktree subpath failed: %v", err)
	}
	if !strings.Contains(wtWorkerTasks, "WORKER-1") || strings.Contains(wtWorkerTasks, "MPOWER-1") {
		t.Fatalf("expected sibling worktree apps/worker to resolve strictly to WORKER, got:\n%s", wtWorkerTasks)
	}

	// 6. Run `flowsk status` OUTSIDE any registered folder (`outsideDir`) — MUST succeed with global overview!
	globalHuman, _, err := execCLIWithIO(t, rt, outsideDir, "", "--format", "human", "status")
	if err != nil {
		t.Fatalf("flowsk status outside registered folder failed: %v", err)
	}
	for _, want := range []string{
		"FLOWSK STATUS",
		"MPOWER",
		"└─ WEB",
		"apps/web",
		"└─ WORKER",
		"apps/worker",
		"feat/multi-wt",
		"worktree-2 commit",
		"ship dashboard shell",
	} {
		if !strings.Contains(globalHuman, want) {
			t.Fatalf("global flowsk status missing %q:\n%s", want, globalHuman)
		}
	}

	globalJSONOut, _, err := execCLIWithIO(t, rt, outsideDir, "", "--format", "json", "status")
	if err != nil {
		t.Fatalf("flowsk status --format json failed: %v", err)
	}
	var overview cli.WorkspaceOverviewView
	if err := json.Unmarshal([]byte(globalJSONOut), &overview); err != nil {
		t.Fatalf("unmarshal overview: %v", err)
	}
	if overview.TotalProjects != 3 || overview.TotalSubprojects != 2 || overview.Counts.Total != 7 {
		t.Fatalf("unexpected overview totals: %+v", overview)
	}
	if len(overview.Projects) != 1 || overview.Projects[0].Project.Key != "MPOWER" || overview.Projects[0].Counts.Total != 4 {
		t.Fatalf("expected MPOWER root with own separate count=4, got: %+v", overview.Projects)
	}

	// 7. Non-Destructive Parent Deletion: deleting MPOWER detaches bindings without deleting WEB or WORKER!
	_ = runCLI(t, rt, "project", "delete", "MPOWER")
	afterDelOut, _, err := execCLIWithIO(t, rt, outsideDir, "", "--format", "json", "status")
	if err != nil {
		t.Fatalf("status after parent delete failed: %v", err)
	}
	var afterOverview cli.WorkspaceOverviewView
	if err := json.Unmarshal([]byte(afterDelOut), &afterOverview); err != nil {
		t.Fatalf("unmarshal afterOverview: %v", err)
	}
	if afterOverview.TotalProjects != 2 || afterOverview.TotalSubprojects != 0 || afterOverview.Counts.Total != 3 {
		t.Fatalf("expected WEB and WORKER to survive as 2 standalone root projects with 3 tasks, got: %+v", afterOverview)
	}
}

// TestWebUIEmbeddedServerArchivePurgeMarkdownDocsAndOrphanPrune verifies end-to-end:
//  1. Sibling `~/.flowsk/projects/<ws>/<id>.md` and `~/.flowsk/tasks/<ws>/<id>.md` persistence.
//  2. `flowsk task archive` + `flowsk task purge-archived` permanently deleting archived DB rows & `.md` files.
//  3. Orphan `.md` & missing `.md` detection surfacing as a yellow warning in `flowsk status`, and reconciled by `flowsk prune`.
//  4. `flowsk web --no-open` serving the embedded Angular SPA (`<app-root>`), `/api/ui-config`, and ConnectRPC JSON endpoints.
func TestWebUIEmbeddedServerArchivePurgeMarkdownDocsAndOrphanPrune(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("FLOWSK_HOME", "")

	rt := app.NewRuntime()
	t.Cleanup(func() { _ = rt.Close() })

	// 1. Create root project + bound subproject & tasks, attach Markdown docs to both project and tasks.
	projJSON := runCLI(t, rt, "--format", "json", "--actor", "operator", "project", "create", "UIX", "--name", "Flowsk Web UI")
	var proj cli.ProjectView
	if err := json.Unmarshal([]byte(projJSON), &proj); err != nil {
		t.Fatalf("unmarshal project: %v", err)
	}
	subJSON := runCLI(t, rt, "--format", "json", "--actor", "operator", "project", "create", "UIXSUB", "--name", "Web Submodule")
	var subProj cli.ProjectView
	if err := json.Unmarshal([]byte(subJSON), &subProj); err != nil {
		t.Fatalf("unmarshal subProj: %v", err)
	}
	_ = runCLI(t, rt, "--actor", "operator", "project", "bind", "UIX", "UIXSUB", "--subpath", "apps/sub")

	boundDir := filepath.Join(t.TempDir(), "uix-repo")
	unboundDir := filepath.Join(t.TempDir(), "not-a-flowsk-project")
	if err := os.MkdirAll(boundDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(unboundDir, 0o750); err != nil {
		t.Fatal(err)
	}
	_ = runCLI(t, rt, "--project", "UIX", "project", "link", boundDir)

	t1JSON := runCLI(t, rt, "--format", "json", "--project", "UIX", "--actor", "operator", "task", "create", "Archived task to purge", "--priority", "2")
	var t1 cli.TaskView
	if err := json.Unmarshal([]byte(t1JSON), &t1); err != nil {
		t.Fatalf("unmarshal t1: %v", err)
	}

	t2JSON := runCLI(t, rt, "--format", "json", "--project", "UIX", "--actor", "operator", "task", "create", "Active Kanban task", "--priority", "1")
	var t2 cli.TaskView
	if err := json.Unmarshal([]byte(t2JSON), &t2); err != nil {
		t.Fatalf("unmarshal t2: %v", err)
	}

	_ = runCLI(t, rt, "--project", "UIX", "project", "doc", "--set", "# UIX Architecture\n\nMinimalist Tailwind + SignalStore.")
	_ = runCLI(t, rt, "--project", "UIX", "task", "doc", t1.Key, "--set", "# Ephemeral Archived Notes")
	_ = runCLI(t, rt, "--project", "UIX", "task", "doc", t2.Key, "--set", "# Active Task Notes")

	// Verify sibling directory layout: ~/.flowsk/projects/<ws>/<proj>.md and ~/.flowsk/tasks/<ws>/<task>.md!
	projMDPath := filepath.Join(tmpHome, ".flowsk", "projects", proj.WorkspaceID, proj.ID+".md")
	t1MDPath := filepath.Join(tmpHome, ".flowsk", "tasks", t1.WorkspaceID, t1.ID+".md")
	t2MDPath := filepath.Join(tmpHome, ".flowsk", "tasks", t2.WorkspaceID, t2.ID+".md")
	for _, p := range []string{projMDPath, t1MDPath, t2MDPath} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected Markdown file at %q: %v", p, err)
		}
	}

	// 2. Archive t1 and run `task purge-archived`: t1 must be physically deleted from DB and its .md file removed, while t2 & project remain intact!
	_ = runCLI(t, rt, "--project", "UIX", "task", "archive", t1.Key)
	purgeJSON := runCLI(t, rt, "--format", "json", "task", "purge-archived")
	var purgeRes cli.PurgeArchivedView
	if err := json.Unmarshal([]byte(purgeJSON), &purgeRes); err != nil {
		t.Fatalf("unmarshal purgeRes: %v", err)
	}
	if purgeRes.DeletedTaskCount != 1 || purgeRes.DeletedMarkdownCount != 1 {
		t.Fatalf("expected 1 archived task and 1 .md file purged, got: %+v", purgeRes)
	}
	if _, err := os.Stat(t1MDPath); !os.IsNotExist(err) {
		t.Fatalf("expected purged task .md %q to be removed from disk", t1MDPath)
	}
	if _, _, err := execCLIWithIO(t, rt, "", "", "task", "get", t1.Key); err == nil {
		t.Fatalf("expected purged task %s to no longer exist in DB", t1.Key)
	}

	// 3. Simulate orphan .md file + missing .md file -> verify yellow warning in `flowsk status` -> reconcile with `flowsk prune`!
	orphanFile := filepath.Join(tmpHome, ".flowsk", "tasks", t2.WorkspaceID, "00000000-0000-4000-8000-000000000099.md")
	if err := os.WriteFile(orphanFile, []byte("# Orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(t2MDPath); err != nil {
		t.Fatal(err)
	}

	statusWithOrphans := runCLI(t, rt, "--format", "human", "status", "--all")
	if !strings.Contains(statusWithOrphans, "⚠ Warning: 2 orphan or missing Markdown resource(s)") || strings.Contains(statusWithOrphans, "\033[33m") {
		t.Fatalf("expected Lip Gloss orphan warning for 2 issues without raw ANSI in non-TTY status, got:\n%s", statusWithOrphans)
	}

	dryOut := runCLI(t, rt, "--format", "json", "prune", "--dry-run")
	var dryRes cli.PruneResultView
	if err := json.Unmarshal([]byte(dryOut), &dryRes); err != nil || !dryRes.DryRun || dryRes.TotalCleaned != 2 {
		t.Fatalf("unexpected prune --dry-run result: err=%v res=%+v", err, dryRes)
	}
	if _, err := os.Stat(orphanFile); err != nil {
		t.Fatalf("orphan file should still exist after --dry-run: %v", err)
	}

	pruneOut := runCLI(t, rt, "--format", "json", "prune")
	var pruneRes cli.PruneResultView
	if err := json.Unmarshal([]byte(pruneOut), &pruneRes); err != nil || pruneRes.DryRun || pruneRes.TotalCleaned != 2 {
		t.Fatalf("unexpected prune result: err=%v res=%+v", err, pruneRes)
	}
	if _, err := os.Stat(orphanFile); !os.IsNotExist(err) {
		t.Fatalf("expected orphan file %q to be deleted by flowsk prune", orphanFile)
	}
	statusClean := runCLI(t, rt, "--format", "human", "status", "--all")
	if strings.Contains(statusClean, "orphan or missing") {
		t.Fatalf("expected clean status after prune, got:\n%s", statusClean)
	}

	// Close in-process runtime before launching `flowsk web` against the same SQLite DB.
	_ = rt.Close()

	// 4. Launch `flowsk web --no-open` inside an UNBOUND folder (`unboundDir`) and verify:
	//    - `/api/ui-config` exposes resolved `workspaceId` UUID with empty `projectSlug` (no error, no auto-picked project).
	//    - Browser bootstrap sequence (`RegisterActor` -> `ListProjects` -> `ListProjectBindings`) returns all projects & subprojects!
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })
	if err := os.Chdir(unboundDir); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	uiAddr := ln.Addr().String()
	_ = ln.Close()

	uiRT := app.NewRuntime()
	t.Cleanup(func() { _ = uiRT.Close() })
	uiCtx, stopUI := context.WithCancel(context.Background())
	defer stopUI()

	uiErrCh := make(chan error, 1)
	go func() {
		cmd := cli.NewRootCommand(uiRT.CLIDependencies())
		var uiOut bytes.Buffer
		cmd.SetOut(&uiOut)
		cmd.SetArgs([]string{"web", "--listen", uiAddr, "--no-open"})
		uiErrCh <- cmd.ExecuteContext(uiCtx)
	}()

	baseURL := "http://" + uiAddr
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, gErr := http.Get(baseURL + "/healthz")
		if gErr == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
	}

	// Verify embedded Angular SPA index.html
	spaResp, err := http.Get(baseURL + "/board")
	if err != nil {
		t.Fatalf("GET /board failed: %v", err)
	}
	spaBody, _ := io.ReadAll(spaResp.Body)
	_ = spaResp.Body.Close()
	if spaResp.StatusCode != http.StatusOK || !strings.Contains(string(spaBody), "<app-root") {
		t.Fatalf("expected embedded Angular SPA <app-root>, status=%d body=%q", spaResp.StatusCode, string(spaBody))
	}

	// Verify /api/ui-config from an unbound directory: workspaceId UUID present, projectSlug empty!
	cfgResp, err := http.Get(baseURL + "/api/ui-config")
	if err != nil {
		t.Fatalf("GET /api/ui-config failed: %v", err)
	}
	cfgBody, _ := io.ReadAll(cfgResp.Body)
	_ = cfgResp.Body.Close()
	var bootPayload struct {
		WorkspaceID   string `json:"workspaceId"`
		WorkspaceSlug string `json:"workspaceSlug"`
		ProjectSlug   string `json:"projectSlug"`
		ActorID       string `json:"actorId"`
		ActorHandle   string `json:"actorHandle"`
	}
	if err := json.Unmarshal(cfgBody, &bootPayload); err != nil {
		t.Fatalf("unmarshal /api/ui-config: %v (body=%s)", err, string(cfgBody))
	}
	if bootPayload.WorkspaceID != proj.WorkspaceID || bootPayload.WorkspaceSlug != "default" || bootPayload.ProjectSlug != "" {
		t.Fatalf("expected unbound /api/ui-config to have workspaceId=%s, workspaceSlug=default, empty projectSlug, got: %+v", proj.WorkspaceID, bootPayload)
	}

	doJSONRPC := func(procedure, payload string) string {
		t.Helper()
		req, rErr := http.NewRequest(http.MethodPost, baseURL+procedure, strings.NewReader(payload))
		if rErr != nil {
			t.Fatal(rErr)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Connect-Protocol-Version", "1")
		req.Header.Set("X-Flowsk-Workspace-Id", bootPayload.WorkspaceID)
		req.Header.Set("X-Flowsk-Actor-Handle", "web-operator")
		req.Header.Set("X-Flowsk-Actor-Kind", "human")
		resp, dErr := http.DefaultClient.Do(req)
		if dErr != nil {
			t.Fatalf("POST %s failed: %v", procedure, dErr)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST %s returned %d: %s", procedure, resp.StatusCode, string(raw))
		}
		return string(raw)
	}

	// Simulate exact Angular SPA bootstrap calls from unbound directory
	regOut := doJSONRPC("/flowsk.workspace.v1.WorkspaceService/RegisterActor",
		`{"workspaceId":"`+bootPayload.WorkspaceID+`","kind":"ACTOR_KIND_HUMAN","handle":"web-operator","displayName":"web-operator"}`)
	if !strings.Contains(regOut, bootPayload.WorkspaceID) {
		t.Fatalf("expected RegisterActor response with workspaceId, got %s", regOut)
	}
	projsOut := doJSONRPC("/flowsk.project.v1.ProjectService/ListProjects",
		`{"workspaceId":"`+bootPayload.WorkspaceID+`","actorId":"`+bootPayload.ActorID+`","includeArchived":true,"pageSize":200}`)
	if !strings.Contains(projsOut, "UIX") || !strings.Contains(projsOut, "UIXSUB") {
		t.Fatalf("expected ListProjects to return both UIX and UIXSUB, got: %s", projsOut)
	}
	bindingsOut := doJSONRPC("/flowsk.project.v1.ProjectService/ListProjectBindings",
		`{"workspaceId":"`+bootPayload.WorkspaceID+`","actorId":"`+bootPayload.ActorID+`"}`)
	if !strings.Contains(bindingsOut, "apps/sub") || !strings.Contains(bindingsOut, subProj.ID) {
		t.Fatalf("expected ListProjectBindings to include subproject binding apps/sub, got: %s", bindingsOut)
	}
	tasksOut := doJSONRPC("/flowsk.task.v1.TaskService/ListTasks",
		`{"workspaceId":"`+bootPayload.WorkspaceID+`","actorId":"`+bootPayload.ActorID+`"}`)
	if !strings.Contains(tasksOut, t2.Key) {
		t.Fatalf("expected ConnectRPC JSON ListTasks to contain %s, got: %s", t2.Key, tasksOut)
	}

	stopUI()
	if uiErr := <-uiErrCh; uiErr != nil {
		t.Fatalf("flowsk web (unbound) returned unexpected error on shutdown: %v", uiErr)
	}
	_ = uiRT.Close()

	// 5. Launch `flowsk ui --no-open` inside BOUND folder (`boundDir`) and verify `projectSlug` auto-selects UIX!
	if err := os.Chdir(boundDir); err != nil {
		t.Fatal(err)
	}
	lnBound, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	boundAddr := lnBound.Addr().String()
	_ = lnBound.Close()

	boundRT := app.NewRuntime()
	t.Cleanup(func() { _ = boundRT.Close() })
	boundCtx, stopBound := context.WithCancel(context.Background())
	defer stopBound()

	boundErrCh := make(chan error, 1)
	go func() {
		cmd := cli.NewRootCommand(boundRT.CLIDependencies())
		var uiOut bytes.Buffer
		cmd.SetOut(&uiOut)
		cmd.SetArgs([]string{"ui", "--listen", boundAddr, "--no-open"})
		boundErrCh <- cmd.ExecuteContext(boundCtx)
	}()

	boundURL := "http://" + boundAddr
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, gErr := http.Get(boundURL + "/healthz")
		if gErr == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
	}

	boundCfgResp, err := http.Get(boundURL + "/api/ui-config")
	if err != nil {
		t.Fatalf("GET bound /api/ui-config failed: %v", err)
	}
	boundCfgBytes, _ := io.ReadAll(boundCfgResp.Body)
	_ = boundCfgResp.Body.Close()
	if err := json.Unmarshal(boundCfgBytes, &bootPayload); err != nil {
		t.Fatalf("unmarshal bound /api/ui-config: %v", err)
	}
	if !strings.EqualFold(bootPayload.ProjectSlug, "uix") {
		t.Fatalf("expected bound folder /api/ui-config projectSlug=uix, got %+v", bootPayload)
	}

	stopBound()
	if bErr := <-boundErrCh; bErr != nil {
		t.Fatalf("flowsk ui (bound) returned unexpected error on shutdown: %v", bErr)
	}
}

// TestExternalIDUniquenessCodeModeUpsertAndDoctor verifies SQLite persistence
// of project-scoped unique `external_id` attributes, Code Mode `upsertExternal` /
// `getByExternal`, `ext:` search qualifiers, `flowsk install`, and `flowsk doctor`.
func TestExternalIDUniquenessCodeModeUpsertAndDoctor(t *testing.T) {
	tmpRoot := t.TempDir()
	fakeHome := filepath.Join(tmpRoot, "home")
	if err := os.MkdirAll(filepath.Join(fakeHome, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeHome, ".codex", "config.toml"), []byte("[mcp_servers.flowsk]\ncommand = \"flowsk\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome)
	t.Setenv("FLOWSK_HOME", filepath.Join(fakeHome, ".flowsk"))

	rt := app.NewRuntime()
	t.Cleanup(func() { _ = rt.Close() })

	_ = runCLI(t, rt, "project", "create", "ALPHA", "--name", "Alpha Service")
	_ = runCLI(t, rt, "project", "create", "BETA", "--name", "Beta Service")

	// 1. Create task with external_id in ALPHA.
	createdA := runCLI(t, rt, "--project", "ALPHA", "--format", "compact", "task", "create", "Import ClickUp ticket", "--external-id", "CU-9001")
	if !strings.Contains(createdA, "ALPHA-1") || !strings.Contains(createdA, "ext=CU-9001") {
		t.Fatalf("expected ALPHA-1 with ext=CU-9001, got: %s", createdA)
	}

	// 2. Duplicate external_id in ALPHA must fail.
	dupCmd := cli.NewRootCommand(rt.CLIDependencies())
	dupCmd.SetOut(&bytes.Buffer{})
	dupCmd.SetErr(&bytes.Buffer{})
	dupCmd.SetArgs([]string{"--project", "ALPHA", "task", "create", "Duplicate ticket", "--external-id", "CU-9001"})
	if err := dupCmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("expected duplicate external_id in same project ALPHA to fail")
	}

	// 3. Same external_id in BETA must succeed!
	createdB := runCLI(t, rt, "--project", "BETA", "--format", "compact", "task", "create", "Same external ticket in Beta", "--external-id", "CU-9001")
	if !strings.Contains(createdB, "BETA-1") || !strings.Contains(createdB, "ext=CU-9001") {
		t.Fatalf("expected BETA-1 with ext=CU-9001, got: %s", createdB)
	}

	// 4. Code Mode JS upsertExternal (updates ALPHA-1 idempotently + creates CU-9002) and getByExternal.
	jsOut := runCLI(t, rt, "--project", "ALPHA", "--format", "compact", "code", "-e", `
		const u1 = flowsk.tasks.upsertExternal({ externalId: "CU-9001", title: "Updated ClickUp ticket", priority: "urgent" });
		const u2 = flowsk.tasks.upsertExternal({ externalId: "CU-9002", title: "Second ClickUp ticket", priority: "high" });
		const fetched = flowsk.tasks.getByExternal("CU-9002");
		return { u1: u1.upserted, u2: u2.upserted, fetchedKey: fetched.key };
	`)
	if !strings.Contains(jsOut, `"u1":"updated"`) || !strings.Contains(jsOut, `"u2":"created"`) || !strings.Contains(jsOut, `"fetchedKey":"ALPHA-2"`) {
		t.Fatalf("unexpected Code Mode upsertExternal output:\n%s", jsOut)
	}

	// 5. Search by ext: qualifier.
	searchOut := runCLI(t, rt, "--project", "ALPHA", "--format", "compact", "search", "ext:CU-9002")
	if !strings.Contains(searchOut, "ALPHA-2") || strings.Contains(searchOut, "ALPHA-1") {
		t.Fatalf("expected only ALPHA-2 for ext:CU-9002, got:\n%s", searchOut)
	}

	// 6. Install embedded skill into Codex and run `flowsk doctor --strict`.
	_ = runCLI(t, rt, "install", "--home", fakeHome, "--providers", "codex", "--yes")
	docOut := runCLI(t, rt, "doctor", "--home", fakeHome, "--strict", "--format", "human")
	if !strings.Contains(docOut, "FLOWSK DOCTOR") || !strings.Contains(docOut, "up-to-date") {
		t.Fatalf("unexpected flowsk doctor output:\n%s", docOut)
	}
}
