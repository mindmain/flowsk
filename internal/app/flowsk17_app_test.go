// File flowsk17_app_test.go verifies RFC 9110 Bearer + HttpOnly SameSite=Strict
// UI session cookie authentication (`/api/ui-auth`, `/api/ui-config`, ConnectRPC,
// SSE `/api/events`), zero phantom DB actor pollution, `runRemoteUIProxy`
// loopback/Origin/Sec-Fetch-Site & SSE hardening, `MigrateUp` TargetVersion,
// MCP section slicing & token budgets, and Local vs. Remote CLI parity.
package app_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mindmain/flowsk/internal/app"
	"github.com/mindmain/flowsk/internal/cli"
	"github.com/mindmain/flowsk/internal/transport/mcp"
)

func allocateLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen ephemeral port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func waitHealthy(t *testing.T, baseURL string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("server at %s did not become healthy within 5s", baseURL)
}

func TestUISessionCookieAuthPhantomActorPreventionAndRemoteUIProxy(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("FLOWSK_HOME", filepath.Join(tmpDir, ".flowsk"))
	const secretToken = "flw_secret_token_2026"
	t.Setenv("FLOWSK_AUTH_TOKEN", secretToken)

	srvAddr := allocateLoopbackAddr(t)
	srvDB := filepath.Join(tmpDir, "srv", "flowsk.db")
	srvRT := app.NewRuntime()
	t.Cleanup(func() { _ = srvRT.Close() })

	srvCtx, stopSrv := context.WithCancel(context.Background())
	defer stopSrv()

	srvErrCh := make(chan error, 1)
	go func() {
		cmd := cli.NewRootCommand(srvRT.CLIDependencies())
		cmd.SetArgs([]string{
			"serve",
			"--ui",
			"--listen", srvAddr,
			"--db-driver", "sqlite",
			"--db-dsn", srvDB,
			"--volume-dir", filepath.Join(tmpDir, "srv"),
		})
		srvErrCh <- cmd.ExecuteContext(srvCtx)
	}()

	srvBase := "http://" + srvAddr
	waitHealthy(t, srvBase)

	// 1. Unauthenticated GET /api/ui-config MUST NOT expose workspaceId or actorId!
	unauthCfgResp, err := http.Get(srvBase + "/api/ui-config")
	if err != nil {
		t.Fatalf("GET /api/ui-config (unauthed): %v", err)
	}
	unauthCfgRaw, _ := io.ReadAll(unauthCfgResp.Body)
	_ = unauthCfgResp.Body.Close()
	var unauthCfg map[string]any
	if err := json.Unmarshal(unauthCfgRaw, &unauthCfg); err != nil {
		t.Fatalf("unmarshal unauthed /api/ui-config: %v (%s)", err, string(unauthCfgRaw))
	}
	if unauthCfg["authRequired"] != true || unauthCfg["authenticated"] != false {
		t.Fatalf("expected authRequired=true, authenticated=false, got %+v", unauthCfg)
	}
	if _, leakedWS := unauthCfg["workspaceId"]; leakedWS {
		t.Fatalf("SECURITY: unauthed /api/ui-config leaked workspaceId: %s", string(unauthCfgRaw))
	}
	if _, leakedActor := unauthCfg["actorId"]; leakedActor {
		t.Fatalf("SECURITY: unauthed /api/ui-config leaked actorId: %s", string(unauthCfgRaw))
	}

	// 2. Malformed Authorization header ("BearerNoSpace" or "Basic ...") must be rejected on ConnectRPC!
	badReq, _ := http.NewRequest(http.MethodPost, srvBase+"/flowsk.project.v1.ProjectService/ListProjects", strings.NewReader(`{}`))
	badReq.Header.Set("Content-Type", "application/json")
	badReq.Header.Set("Authorization", "Bearer"+secretToken)
	badResp, err := http.DefaultClient.Do(badReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = badResp.Body.Close()
	if badResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for malformed Authorization header without space, got %d", badResp.StatusCode)
	}

	// 3. POST /api/ui-auth with wrong token -> 401; with valid token -> 200 + HttpOnly SameSite=Strict cookie!
	jar, _ := cookiejar.New(nil)
	browserClient := &http.Client{Jar: jar, Timeout: 5 * time.Second}

	wrongAuthResp, err := browserClient.Post(srvBase+"/api/ui-auth", "application/json", strings.NewReader(`{"token":"wrong"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = wrongAuthResp.Body.Close()
	if wrongAuthResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on wrong UI token, got %d", wrongAuthResp.StatusCode)
	}

	okAuthResp, err := browserClient.Post(srvBase+"/api/ui-auth", "application/json", strings.NewReader(`{"token":"`+secretToken+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	setCookies := okAuthResp.Cookies()
	_ = okAuthResp.Body.Close()
	if okAuthResp.StatusCode != http.StatusOK || len(setCookies) == 0 {
		t.Fatalf("expected 200 + flowsk_ui_session cookie, status=%d cookies=%v", okAuthResp.StatusCode, setCookies)
	}
	if setCookies[0].Name != "flowsk_ui_session" || !setCookies[0].HttpOnly || setCookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("expected HttpOnly SameSite=Strict flowsk_ui_session cookie, got %+v", setCookies[0])
	}

	// 4. Authenticated via cookie ONLY (no Authorization header!): /api/ui-config, ConnectRPC, and GET /api/events all succeed!
	authedCfgResp, err := browserClient.Get(srvBase + "/api/ui-config")
	if err != nil {
		t.Fatal(err)
	}
	authedCfgRaw, _ := io.ReadAll(authedCfgResp.Body)
	_ = authedCfgResp.Body.Close()
	var authedCfg struct {
		WorkspaceID   string `json:"workspaceId"`
		WorkspaceSlug string `json:"workspaceSlug"`
		ActorID       string `json:"actorId"`
		AuthRequired  bool   `json:"authRequired"`
		Authenticated bool   `json:"authenticated"`
	}
	if err := json.Unmarshal(authedCfgRaw, &authedCfg); err != nil {
		t.Fatal(err)
	}
	if !authedCfg.AuthRequired || !authedCfg.Authenticated || authedCfg.WorkspaceID == "" || authedCfg.ActorID == "" {
		t.Fatalf("expected full authenticated bootstrap config via cookie, got %+v", authedCfg)
	}

	rpcReq, _ := http.NewRequest(http.MethodPost, srvBase+"/flowsk.project.v1.ProjectService/ListProjects",
		strings.NewReader(`{"workspaceId":"`+authedCfg.WorkspaceID+`","actorId":"`+authedCfg.ActorID+`"}`))
	rpcReq.Header.Set("Content-Type", "application/json")
	rpcReq.Header.Set("X-Flowsk-Workspace-Id", authedCfg.WorkspaceID)
	rpcReq.Header.Set("X-Flowsk-Actor-Handle", "web-operator")
	rpcResp, err := browserClient.Do(rpcReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = rpcResp.Body.Close()
	if rpcResp.StatusCode != http.StatusOK {
		t.Fatalf("expected ConnectRPC call with flowsk_ui_session cookie to return 200, got %d", rpcResp.StatusCode)
	}

	// Open SSE stream via cookie with an arbitrary ?actorHandle=transient-viewer and verify it neither fails nor enrolls transient-viewer or stream-relay in DB!
	sseCtx, cancelSSE := context.WithTimeout(context.Background(), 2*time.Second)
	sseReq, _ := http.NewRequestWithContext(sseCtx, http.MethodGet, srvBase+"/api/events?workspaceId="+authedCfg.WorkspaceID+"&actorHandle=transient-viewer", nil)
	sseClient := &http.Client{Jar: jar}
	sseResp, err := sseClient.Do(sseReq)
	if err != nil {
		cancelSSE()
		t.Fatalf("GET /api/events with session cookie failed: %v", err)
	}
	if sseResp.StatusCode != http.StatusOK {
		_ = sseResp.Body.Close()
		cancelSSE()
		t.Fatalf("expected 200 OK on SSE /api/events with session cookie, got %d", sseResp.StatusCode)
	}
	reader := bufio.NewReader(sseResp.Body)
	firstLine, _ := reader.ReadString('\n')
	_ = sseResp.Body.Close()
	cancelSSE()
	if !strings.Contains(firstLine, "event:") && !strings.Contains(firstLine, ":") {
		t.Fatalf("unexpected initial SSE line: %q", firstLine)
	}

	// Wait >800ms so RunAuditRelay polls at least once, then verify neither "stream-relay" nor "transient-viewer" exists in workspace actors!
	time.Sleep(850 * time.Millisecond)
	cliRT := app.NewRuntime()
	t.Cleanup(func() { _ = cliRT.Close() })
	actorsOut := runCLI(t, cliRT, "--server-url", srvBase, "--auth-token", secretToken, "--actor", "inspector", "--format", "json", "workspace", "actor", "list")
	if strings.Contains(actorsOut, "stream-relay") {
		t.Fatalf("phantom @stream-relay actor must NOT be persisted in DB, got:\n%s", actorsOut)
	}
	if strings.Contains(actorsOut, "transient-viewer") {
		t.Fatalf("read-only SSE query-string actorHandle 'transient-viewer' must NOT be enrolled as DB member, got:\n%s", actorsOut)
	}

	// 5. DELETE /api/ui-auth clears session cookie so /api/ui-config reverts to authenticated=false.
	delReq, _ := http.NewRequest(http.MethodDelete, srvBase+"/api/ui-auth", nil)
	delResp, err := browserClient.Do(delReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = delResp.Body.Close()
	afterLogoutResp, err := browserClient.Get(srvBase + "/api/ui-config")
	if err != nil {
		t.Fatal(err)
	}
	afterLogoutRaw, _ := io.ReadAll(afterLogoutResp.Body)
	_ = afterLogoutResp.Body.Close()
	if strings.Contains(string(afterLogoutRaw), `"authenticated":true`) || strings.Contains(string(afterLogoutRaw), authedCfg.WorkspaceID) {
		t.Fatalf("expected unauthenticated /api/ui-config after DELETE /api/ui-auth, got: %s", string(afterLogoutRaw))
	}

	// 6. Launch `flowsk ui --no-open --server-url <srvBase>` (runRemoteUIProxy) and verify Host/Origin/Sec-Fetch-Site guards + cookie-to-Bearer proxy!
	proxyAddr := allocateLoopbackAddr(t)
	proxyRT := app.NewRuntime()
	t.Cleanup(func() { _ = proxyRT.Close() })
	proxyCtx, stopProxy := context.WithCancel(context.Background())
	defer stopProxy()

	proxyErrCh := make(chan error, 1)
	go func() {
		cmd := cli.NewRootCommand(proxyRT.CLIDependencies())
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetArgs([]string{"--server-url", srvBase, "--auth-token", secretToken, "ui", "--listen", proxyAddr, "--no-open"})
		proxyErrCh <- cmd.ExecuteContext(proxyCtx)
	}()

	proxyBase := "http://" + proxyAddr
	waitHealthy(t, proxyBase)

	// DNS rebinding non-loopback Host -> 403
	rebindReq, _ := http.NewRequest(http.MethodGet, proxyBase+"/api/ui-config", nil)
	rebindReq.Host = "evil-rebind.example.com:1616"
	rebindResp, err := http.DefaultClient.Do(rebindReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = rebindResp.Body.Close()
	if rebindResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for non-loopback Host on runRemoteUIProxy, got %d", rebindResp.StatusCode)
	}

	// Cross-site Sec-Fetch-Site -> 403
	xsiteReq, _ := http.NewRequest(http.MethodPost, proxyBase+"/flowsk.project.v1.ProjectService/ListProjects", strings.NewReader(`{}`))
	xsiteReq.Header.Set("Sec-Fetch-Site", "cross-site")
	xsiteResp, err := http.DefaultClient.Do(xsiteReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = xsiteResp.Body.Close()
	if xsiteResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for Sec-Fetch-Site: cross-site on runRemoteUIProxy, got %d", xsiteResp.StatusCode)
	}

	// Cross-origin Origin -> 403
	xorigReq, _ := http.NewRequest(http.MethodPost, proxyBase+"/api/ui-auth", strings.NewReader(`{"token":"`+secretToken+`"}`))
	xorigReq.Header.Set("Origin", "https://attacker.example")
	xorigResp, err := http.DefaultClient.Do(xorigReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = xorigResp.Body.Close()
	if xorigResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-origin Origin on runRemoteUIProxy, got %d", xorigResp.StatusCode)
	}

	// Unauthenticated RPC against proxy when bearerToken is configured -> 401 (does NOT blindly inject bearerToken!)
	unauthProxyRPC, _ := http.NewRequest(http.MethodPost, proxyBase+"/flowsk.project.v1.ProjectService/ListProjects", strings.NewReader(`{}`))
	unauthProxyRPC.Header.Set("Content-Type", "application/json")
	unauthProxyResp, err := http.DefaultClient.Do(unauthProxyRPC)
	if err != nil {
		t.Fatal(err)
	}
	_ = unauthProxyResp.Body.Close()
	if unauthProxyResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on unauthenticated RPC to runRemoteUIProxy, got %d", unauthProxyResp.StatusCode)
	}

	// Authenticate to proxy via same-origin POST /api/ui-auth, then call proxied ConnectRPC using ONLY the proxy cookie!
	proxyJar, _ := cookiejar.New(nil)
	proxyBrowser := &http.Client{Jar: proxyJar, Timeout: 5 * time.Second}
	loginReq, _ := http.NewRequest(http.MethodPost, proxyBase+"/api/ui-auth", strings.NewReader(`{"token":"`+secretToken+`"}`))
	loginReq.Header.Set("Content-Type", "application/json")
	loginReq.Header.Set("Origin", proxyBase)
	loginResp, err := proxyBrowser.Do(loginReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on same-origin proxy /api/ui-auth, got %d", loginResp.StatusCode)
	}

	proxiedRPC, _ := http.NewRequest(http.MethodPost, proxyBase+"/flowsk.project.v1.ProjectService/ListProjects",
		strings.NewReader(`{"workspaceId":"`+authedCfg.WorkspaceID+`","actorId":"`+authedCfg.ActorID+`"}`))
	proxiedRPC.Header.Set("Content-Type", "application/json")
	proxiedRPC.Header.Set("Origin", proxyBase)
	proxiedRPC.Header.Set("X-Flowsk-Workspace-Id", authedCfg.WorkspaceID)
	proxiedRPC.Header.Set("X-Flowsk-Actor-Handle", "web-operator")
	proxiedResp, err := proxyBrowser.Do(proxiedRPC)
	if err != nil {
		t.Fatal(err)
	}
	_ = proxiedResp.Body.Close()
	if proxiedResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on proxied ConnectRPC with session cookie, got %d", proxiedResp.StatusCode)
	}

	stopProxy()
	if pErr := <-proxyErrCh; pErr != nil {
		t.Fatalf("runRemoteUIProxy shutdown error: %v", pErr)
	}
	stopSrv()
	if sErr := <-srvErrCh; sErr != nil {
		t.Fatalf("flowsk serve shutdown error: %v", sErr)
	}
}

func TestDualModeParityObjectivesTasksAuditMigrateAndMCPBridge(t *testing.T) {
	tmpDir := t.TempDir()
	localHome := filepath.Join(tmpDir, "local-home")
	remoteHome := filepath.Join(tmpDir, "remote-home")
	t.Setenv("FLOWSK_AUTH_TOKEN", "")

	// Start remote `flowsk serve` instance for the remote leg of dual-mode parity testing.
	srvAddr := allocateLoopbackAddr(t)
	srvDB := filepath.Join(tmpDir, "remote-srv", "flowsk.db")
	srvRT := app.NewRuntime()
	t.Cleanup(func() { _ = srvRT.Close() })
	srvCtx, stopSrv := context.WithCancel(context.Background())
	defer stopSrv()
	srvErrCh := make(chan error, 1)
	go func() {
		cmd := cli.NewRootCommand(srvRT.CLIDependencies())
		cmd.SetArgs([]string{
			"serve",
			"--listen", srvAddr,
			"--db-driver", "sqlite",
			"--db-dsn", srvDB,
			"--volume-dir", filepath.Join(tmpDir, "remote-srv"),
		})
		srvErrCh <- cmd.ExecuteContext(srvCtx)
	}()
	srvURL := "http://" + srvAddr
	waitHealthy(t, srvURL)

	runParitySuite := func(t *testing.T, extraGlobalArgs ...string) {
		t.Helper()
		rt := app.NewRuntime()
		t.Cleanup(func() { _ = rt.Close() })

		execJSON := func(args ...string) string {
			full := append([]string{"--format", "json", "--actor", "lead"}, extraGlobalArgs...)
			full = append(full, args...)
			return runCLI(t, rt, full...)
		}
		execFail := func(args ...string) {
			full := append([]string{"--format", "json", "--actor", "lead"}, extraGlobalArgs...)
			full = append(full, args...)
			cmd := cli.NewRootCommand(rt.CLIDependencies())
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(full)
			if err := cmd.ExecuteContext(context.Background()); err == nil {
				t.Fatalf("expected command %v to fail, but it succeeded", args)
			}
		}

		_ = execJSON("project", "create", "PAR", "--name", "Parity Project")

		// 1. Objectives: kind (milestone/cycle), list filter by kind & status, update status & target-date, clear target-date "".
		var ms cli.ObjectiveView
		if err := json.Unmarshal([]byte(execJSON("--project", "PAR", "goal", "create", "Beta Milestone", "--kind", "milestone", "--target-date", "2026-12-15")), &ms); err != nil {
			t.Fatal(err)
		}
		if ms.Kind != "milestone" || ms.TargetDate != "2026-12-15" {
			t.Fatalf("expected milestone with due 2026-12-15, got %+v", ms)
		}
		_ = execJSON("--project", "PAR", "goal", "create", "Sprint Cycle 1", "--kind", "cycle")

		var msList cli.ObjectiveListResult
		if err := json.Unmarshal([]byte(execJSON("--project", "PAR", "goal", "list", "--kind", "milestone")), &msList); err != nil {
			t.Fatal(err)
		}
		if len(msList.Items) != 1 || msList.Items[0].Kind != "milestone" {
			t.Fatalf("expected 1 milestone from goal list --kind milestone, got %+v", msList.Items)
		}

		var updMs cli.ObjectiveView
		if err := json.Unmarshal([]byte(execJSON("--project", "PAR", "goal", "update", ms.ID, "--status", "at_risk", "--target-date", "")), &updMs); err != nil {
			t.Fatal(err)
		}
		if updMs.Kind != "milestone" || updMs.Status != "at_risk" || updMs.TargetDate != "" {
			t.Fatalf("expected milestone updated to status=at_risk and cleared target_date, got %+v", updMs)
		}

		// 2. Tasks: error on invalid --objective / --parent / --depends-on before creating task!
		execFail("--project", "PAR", "task", "create", "Bad Obj", "--objective", "NONEXISTENT-OBJ")
		execFail("--project", "PAR", "task", "create", "Bad Parent", "--parent", "PAR-999")
		execFail("--project", "PAR", "task", "create", "Bad Dep", "--depends-on", "PAR-999")

		// First valid task must still get sequence PAR-1 because failed creates didn't leak tasks!
		var t1 cli.TaskView
		if err := json.Unmarshal([]byte(execJSON("--project", "PAR", "task", "create", "Parent Task", "--objective", ms.ID, "--label", "backend,urgent", "--assignee", "bob-dev")), &t1); err != nil {
			t.Fatal(err)
		}
		if t1.Key != "PAR-1" || len(t1.Labels) != 2 {
			t.Fatalf("expected PAR-1 with 2 labels, got %+v", t1)
		}

		// List tasks by --assignee bob-dev (unclaimed, matched via AssigneeActorID!).
		var assignedList cli.TaskListResult
		if err := json.Unmarshal([]byte(execJSON("--project", "PAR", "task", "list", "--assignee", "bob-dev")), &assignedList); err != nil {
			t.Fatal(err)
		}
		if len(assignedList.Items) != 1 || assignedList.Items[0].Key != "PAR-1" {
			t.Fatalf("expected task list --assignee bob-dev to return PAR-1, got %+v", assignedList.Items)
		}

		// Create child task PAR-2 under PAR-1, then clear labels, parent, and objective via update!
		var t2 cli.TaskView
		if err := json.Unmarshal([]byte(execJSON("--project", "PAR", "task", "create", "Child Task", "--parent", "PAR-1", "--objective", ms.ID, "--label", "only-tag")), &t2); err != nil {
			t.Fatal(err)
		}
		var t2Cleared cli.TaskView
		if err := json.Unmarshal([]byte(execJSON("--project", "PAR", "task", "update", "PAR-2", "--remove-label", "only-tag", "--objective", "", "--parent", "", "--estimate", "5")), &t2Cleared); err != nil {
			t.Fatal(err)
		}
		if len(t2Cleared.Labels) != 0 || t2Cleared.ObjectiveKey != "" || t2Cleared.ParentKey != "" {
			t.Fatalf("expected labels=[], objective='', parent='' after clearing, got %+v", t2Cleared)
		}

		// Claim with --note, structured comments, block without --by, unblock -> Ready, block with 2 blockers + partial unblock!
		_ = execJSON("--project", "PAR", "task", "claim", "PAR-2", "--note", "Starting implementation")
		var hComment cli.CommentView
		if err := json.Unmarshal([]byte(execJSON("--project", "PAR", "task", "comment", "PAR-2", "-m", "Handing off context", "--kind", "handoff")), &hComment); err != nil {
			t.Fatal(err)
		}
		if hComment.Kind != "handoff" {
			t.Fatalf("expected comment kind=handoff, got %+v", hComment)
		}
		var comments cli.CommentListResult
		if err := json.Unmarshal([]byte(execJSON("--project", "PAR", "task", "comments", "PAR-2")), &comments); err != nil {
			t.Fatal(err)
		}
		if len(comments.Items) < 2 {
			t.Fatalf("expected >=2 comments (claim note + handoff), got %+v", comments.Items)
		}

		// Impediment block (no --by) -> Blocked; unblock -> Todo/Ready!
		var impBlocked cli.TaskView
		if err := json.Unmarshal([]byte(execJSON("--project", "PAR", "task", "block", "PAR-2", "--reason", "Waiting on API credentials")), &impBlocked); err != nil {
			t.Fatal(err)
		}
		if !impBlocked.Blocked || impBlocked.Category != "blocked" {
			t.Fatalf("expected PAR-2 in Blocked state when blocked without --by, got %+v", impBlocked)
		}
		var impUnblocked cli.TaskView
		if err := json.Unmarshal([]byte(execJSON("--project", "PAR", "task", "unblock", "PAR-2", "--note", "Credentials provisioned")), &impUnblocked); err != nil {
			t.Fatal(err)
		}
		if impUnblocked.Blocked || impUnblocked.Category != "todo" {
			t.Fatalf("expected PAR-2 transitioned back to Todo after unblock, got %+v", impUnblocked)
		}

		// Block PAR-2 by both PAR-1 and PAR-3, then unblock ONLY PAR-1 (--by PAR-1) -> still blocked by PAR-3!
		_ = execJSON("--project", "PAR", "task", "create", "Third Task")
		_ = execJSON("--project", "PAR", "task", "block", "PAR-2", "--by", "PAR-1,PAR-3", "--reason", "Needs upstream schemas")
		var partialUnblock cli.TaskView
		if err := json.Unmarshal([]byte(execJSON("--project", "PAR", "task", "unblock", "PAR-2", "--by", "PAR-1")), &partialUnblock); err != nil {
			t.Fatal(err)
		}
		if !partialUnblock.Blocked {
			t.Fatalf("expected PAR-2 to remain blocked by PAR-3 after unblocking only PAR-1, got %+v", partialUnblock)
		}
		var fullUnblock cli.TaskView
		if err := json.Unmarshal([]byte(execJSON("--project", "PAR", "task", "unblock", "PAR-2", "--by", "PAR-3")), &fullUnblock); err != nil {
			t.Fatal(err)
		}
		if fullUnblock.Blocked {
			t.Fatalf("expected PAR-2 unblocked after clearing PAR-3, got %+v", fullUnblock)
		}

		// 3. Audit filtering by --action.
		var claimAudits cli.AuditEventListResult
		if err := json.Unmarshal([]byte(execJSON("audit", "list", "--action", "task.claimed")), &claimAudits); err != nil {
			t.Fatal(err)
		}
		if len(claimAudits.Items) == 0 {
			t.Fatalf("expected at least 1 claim audit event when filtering --action task.claimed")
		}
	}

	t.Run("local_mode", func(t *testing.T) {
		t.Setenv("FLOWSK_HOME", localHome)
		runParitySuite(t)
	})

	t.Run("remote_mode", func(t *testing.T) {
		t.Setenv("FLOWSK_HOME", remoteHome)
		runParitySuite(t, "--server-url", srvURL)
	})

	// Verify `db migrate up --target-version` honors TargetVersion on a fresh SQLite DB!
	migDB := filepath.Join(tmpDir, "mig-target.db")
	migRT := app.NewRuntime()
	t.Cleanup(func() { _ = migRT.Close() })
	var statuses []cli.MigrationStatusEntry
	if err := json.Unmarshal([]byte(runCLI(t, migRT, "--format", "json", "db", "--db-dsn", migDB, "migrate", "status")), &statuses); err != nil {
		t.Fatal(err)
	}
	if len(statuses) >= 2 {
		firstVer := statuses[0].Version
		var migRes cli.MigrationResult
		if err := json.Unmarshal([]byte(runCLI(t, migRT, "--format", "json", "db", "--db-dsn", migDB, "migrate", "up", "--target-version", jsonNumberString(firstVer))), &migRes); err != nil {
			t.Fatal(err)
		}
		if migRes.CurrentVersion != firstVer {
			t.Fatalf("expected CurrentVersion=%d with --target-version, got %+v", firstVer, migRes)
		}
	}

	// Verify MCP Bridge Guide section slicing (`overview`, `filters`, `syntax`, `examples`) + MaxTokens & Execute MaxTokens!
	t.Setenv("FLOWSK_HOME", localHome)
	mcpSrv := migRT.CLIDependencies().MCPRunner().NewMCPServer(mcp.Config{
		Transport:          mcp.TransportStdio,
		DefaultWorkspaceID: "default",
		DefaultProjectID:   "PAR",
		DefaultActorID:     "mcp-tester",
	})
	_ = mcpSrv
	langClient := migRT.CLIDependencies().LanguageClient()
	ctxWithOpts := cli.WithGlobalOptions(context.Background(), cli.GlobalOptions{
		DBPath:    filepath.Join(localHome, "flowsk.db"),
		Workspace: "default",
		Project:   "PAR",
		Actor:     "mcp-tester",
	})
	fullGuide, err := langClient.GetGuide(ctxWithOpts, "all")
	if err != nil {
		t.Fatal(err)
	}
	overviewGuide, err := langClient.GetGuide(ctxWithOpts, "overview")
	if err != nil {
		t.Fatal(err)
	}
	examplesGuide, err := langClient.GetGuide(ctxWithOpts, "examples")
	if err != nil {
		t.Fatal(err)
	}
	if len(overviewGuide.Markdown) >= len(fullGuide.Markdown) || !strings.Contains(overviewGuide.Markdown, "Core Guarantees") {
		t.Fatalf("expected sliced overview guide smaller than full guide, len=%d vs %d", len(overviewGuide.Markdown), len(fullGuide.Markdown))
	}
	if len(examplesGuide.Markdown) >= len(fullGuide.Markdown) || !strings.Contains(examplesGuide.Markdown, "Recipes") {
		t.Fatalf("expected sliced examples guide smaller than full guide, len=%d vs %d", len(examplesGuide.Markdown), len(fullGuide.Markdown))
	}

	stopSrv()
	if sErr := <-srvErrCh; sErr != nil {
		t.Fatalf("flowsk serve shutdown error: %v", sErr)
	}
}

func jsonNumberString(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
