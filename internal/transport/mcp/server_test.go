// File server_test.go verifies MCP dependency resolution, nil-deps fallbacks,
// newline-delimited stdio JSON-RPC 2.0 sessions, and Streamable HTTP dispatch.
package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/transport/mcp"
)

type stubDeps struct {
	execCalls   int
	guideCalls  int
	digestCalls int
}

func (d *stubDeps) LanguageExecutor() mcp.LanguageExecutor {
	d.execCalls++
	return nil
}

func (d *stubDeps) LanguageGuideProvider() mcp.LanguageGuideProvider {
	d.guideCalls++
	return nil
}

func (d *stubDeps) ProjectDigestProvider() mcp.ProjectDigestProvider {
	d.digestCalls++
	return nil
}

type fakeMCPDeps struct {
	mu         sync.Mutex
	lastExec   mcp.ExecuteScriptInput
	lastGuide  mcp.GuideInput
	lastDigest mcp.DigestInput
	execErr    error
}

func (f *fakeMCPDeps) LanguageExecutor() mcp.LanguageExecutor           { return f }
func (f *fakeMCPDeps) LanguageGuideProvider() mcp.LanguageGuideProvider { return f }
func (f *fakeMCPDeps) ProjectDigestProvider() mcp.ProjectDigestProvider { return f }

func (f *fakeMCPDeps) Execute(ctx context.Context, in mcp.ExecuteScriptInput) (mcp.ExecuteScriptOutput, error) {
	if err := ctx.Err(); err != nil {
		return mcp.ExecuteScriptOutput{}, err
	}
	f.mu.Lock()
	f.lastExec = in
	execErr := f.execErr
	f.mu.Unlock()
	if execErr != nil {
		return mcp.ExecuteScriptOutput{}, execErr
	}
	return mcp.ExecuteScriptOutput{
		FormattedSummary: "ok mutations=1 FLW-42",
		AppliedCount:     1,
		DryRun:           in.DryRun,
	}, nil
}

func (f *fakeMCPDeps) Guide(ctx context.Context, in mcp.GuideInput) (mcp.GuideOutput, error) {
	if err := ctx.Err(); err != nil {
		return mcp.GuideOutput{}, err
	}
	f.mu.Lock()
	f.lastGuide = in
	f.mu.Unlock()
	return mcp.GuideOutput{
		Version:  "2.0.0",
		MIMEType: mcp.ResourceMIMETextMarkdown,
		Markdown: "# Flowsk Code Mode Guide (" + string(in.Section) + ")",
	}, nil
}

func (f *fakeMCPDeps) Digest(ctx context.Context, in mcp.DigestInput) (mcp.DigestOutput, error) {
	if err := ctx.Err(); err != nil {
		return mcp.DigestOutput{}, err
	}
	f.mu.Lock()
	f.lastDigest = in
	f.mu.Unlock()
	return mcp.DigestOutput{
		FormattedDigest: "FLW ready=3 in_progress=1 blocked=0",
		ReadyTasks:      3,
		InProgressTasks: 1,
		BlockedTasks:    0,
	}, nil
}

type jsonRPCResponseEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// TestNewServerResolvesDependenciesImmediatelyAndProtectsSlices verifies
// constructor resolution and defensive slice cloning on tools and resources.
func TestNewServerResolvesDependenciesImmediatelyAndProtectsSlices(t *testing.T) {
	t.Parallel()

	deps := &stubDeps{}
	srv := mcp.NewServer(mcp.Config{}, deps)

	if deps.execCalls != 1 || deps.guideCalls != 1 || deps.digestCalls != 1 {
		t.Fatalf("expected immediate constructor resolution (1/1/1), got %d/%d/%d",
			deps.execCalls, deps.guideCalls, deps.digestCalls)
	}
	if srv.Config().Transport != mcp.TransportStdio {
		t.Fatalf("expected default stdio transport, got %q", srv.Config().Transport)
	}

	tools := srv.RegisterTools()
	if len(tools) != 4 {
		t.Fatalf("expected 4 canonical tools, got %d", len(tools))
	}
	if tools[0].Name != mcp.ToolNameSearch ||
		tools[1].Name != mcp.ToolNameExecute ||
		tools[2].Name != mcp.ToolNameLanguageGuide ||
		tools[3].Name != mcp.ToolNameDigest {
		t.Fatalf("unexpected canonical tool ordering: %+v", tools)
	}

	// Mutate caller slice to verify defensive copy invariant.
	tools[0].Name = "mutated"
	tools[0].InputSchema[0] = 'X'
	freshTools := srv.RegisterTools()
	if freshTools[0].Name != mcp.ToolNameSearch {
		t.Fatalf("expected defensive copy of tool name, got %q", freshTools[0].Name)
	}
	var schema map[string]any
	if err := json.Unmarshal(freshTools[0].InputSchema, &schema); err != nil {
		t.Fatalf("expected intact JSON schema after caller mutation: %v", err)
	}

	resources := srv.RegisterResources()
	if len(resources) != 1 || resources[0].URI != mcp.ResourceURILanguageGuide {
		t.Fatalf("unexpected canonical resources: %+v", resources)
	}
	resources[0].URI = "mutated"
	if srv.RegisterResources()[0].URI != mcp.ResourceURILanguageGuide {
		t.Fatal("expected defensive copy of resource slice")
	}
}

// TestServerSkeletonReturnsErrNotImplemented verifies fallback behavior when
// nil dependencies are supplied to the MCP server.
func TestServerSkeletonReturnsErrNotImplemented(t *testing.T) {
	t.Parallel()

	srv := mcp.NewServer(mcp.Config{Transport: mcp.TransportHTTP}, nil)
	ctx := context.Background()

	if err := srv.Serve(ctx); !errors.Is(err, mcp.ErrNotImplemented) {
		t.Fatalf("Serve error = %v, want %v", err, mcp.ErrNotImplemented)
	}
	if _, err := srv.HandleExecute(ctx, json.RawMessage(`{"script":"TASK LIST"}`)); !errors.Is(err, mcp.ErrNotImplemented) {
		t.Fatalf("HandleExecute error = %v, want %v", err, mcp.ErrNotImplemented)
	}
	if _, err := srv.HandleLanguageGuide(ctx, json.RawMessage(`{"section":"all"}`)); !errors.Is(err, mcp.ErrNotImplemented) {
		t.Fatalf("HandleLanguageGuide error = %v, want %v", err, mcp.ErrNotImplemented)
	}
	if _, err := srv.HandleDigest(ctx, json.RawMessage(`{"limit":10}`)); !errors.Is(err, mcp.ErrNotImplemented) {
		t.Fatalf("HandleDigest error = %v, want %v", err, mcp.ErrNotImplemented)
	}
	if _, err := srv.ReadResource(ctx, mcp.ResourceURILanguageGuide); !errors.Is(err, mcp.ErrNotImplemented) {
		t.Fatalf("ReadResource error = %v, want %v", err, mcp.ErrNotImplemented)
	}

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	rec := httptest.NewRecorder()
	srv.HTTPHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("HTTP status = %d, want %d", rec.Code, http.StatusNotImplemented)
	}
}

// TestStdioJSONRPCSessionLifecycle verifies end-to-end JSON-RPC 2.0 framing,
// tool calls, and resource reads over stdio.
func TestStdioJSONRPCSessionLifecycle(t *testing.T) {
	t.Parallel()

	wsID := uuid.NewString()
	actorID := uuid.NewString()
	deps := &fakeMCPDeps{}

	requests := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","clientInfo":{"name":"test-agent","version":"1.0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"flowsk_language_guide","arguments":{"section":"syntax"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"flowsk_digest","arguments":{"limit":15}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"flowsk_execute","arguments":{"script":"const t1 = flowsk.tasks.create({ title: \"Ship MCP\" }); return t1.key;","dry_run":true}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"resources/list"}`,
		`{"jsonrpc":"2.0","id":8,"method":"resources/read","params":{"uri":"flowsk://language/guide"}}`,
	}

	var stdout, stderr bytes.Buffer
	srv := mcp.NewServer(mcp.Config{
		Transport:          mcp.TransportStdio,
		Stdin:              strings.NewReader(strings.Join(requests, "\n") + "\n"),
		Stdout:             &stdout,
		Stderr:             &stderr,
		DefaultWorkspaceID: wsID,
		DefaultProjectID:   "CORE",
		DefaultActorID:     actorID,
		MaxTokensDefault:   500,
	}, deps)

	if err := srv.Serve(context.Background()); err != nil {
		t.Fatalf("stdio Serve: %v (stderr=%s)", err, stderr.String())
	}

	rawLines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	// 8 request IDs (notifications/initialized emits no response frame).
	if len(rawLines) != 8 {
		t.Fatalf("expected 8 JSON-RPC response lines on stdout, got %d:\n%s", len(rawLines), stdout.String())
	}

	var responses []jsonRPCResponseEnvelope
	for i, line := range rawLines {
		var env jsonRPCResponseEnvelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("line %d invalid JSON-RPC frame %q: %v", i, line, err)
		}
		if env.JSONRPC != "2.0" {
			t.Fatalf("line %d jsonrpc = %q, want 2.0", i, env.JSONRPC)
		}
		if env.Error != nil {
			t.Fatalf("line %d unexpected JSON-RPC error: %+v", i, env.Error)
		}
		responses = append(responses, env)
	}

	// Verify tools/list (id=3 -> index 2).
	var toolsList struct {
		Tools []mcp.ToolSpec `json:"tools"`
	}
	if err := json.Unmarshal(responses[2].Result, &toolsList); err != nil || len(toolsList.Tools) != 4 {
		t.Fatalf("tools/list decode err=%v count=%d", err, len(toolsList.Tools))
	}

	// Verify flowsk_language_guide call (id=4 -> index 3).
	var guideToolRes mcp.ToolResult
	if err := json.Unmarshal(responses[3].Result, &guideToolRes); err != nil || guideToolRes.IsError || len(guideToolRes.Content) != 1 || !strings.Contains(guideToolRes.Content[0].Text, "syntax") {
		t.Fatalf("flowsk_language_guide result: err=%v res=%+v", err, guideToolRes)
	}

	// Verify flowsk_digest call inherited session defaults and default IncludeBlocked=true (id=5 -> index 4).
	var digestToolRes mcp.ToolResult
	if err := json.Unmarshal(responses[4].Result, &digestToolRes); err != nil || digestToolRes.IsError || !strings.Contains(digestToolRes.Content[0].Text, "ready=3") {
		t.Fatalf("flowsk_digest result: err=%v res=%+v", err, digestToolRes)
	}
	if deps.lastDigest.WorkspaceID != wsID || deps.lastDigest.ProjectID != "CORE" || !deps.lastDigest.IncludeBlocked || deps.lastDigest.Limit != 15 || deps.lastDigest.MaxTokens != 500 {
		t.Fatalf("unexpected DigestInput defaults: %+v", deps.lastDigest)
	}

	// Verify flowsk_execute call (id=6 -> index 5).
	var execToolRes mcp.ToolResult
	if err := json.Unmarshal(responses[5].Result, &execToolRes); err != nil || execToolRes.IsError || !strings.Contains(execToolRes.Content[0].Text, "FLW-42") {
		t.Fatalf("flowsk_execute result: err=%v res=%+v", err, execToolRes)
	}
	if deps.lastExec.WorkspaceID != wsID || deps.lastExec.ActorID != actorID || !deps.lastExec.DryRun || deps.lastExec.Density != mcp.DensityMinimal {
		t.Fatalf("unexpected ExecuteScriptInput: %+v", deps.lastExec)
	}

	// Verify resources/read (id=8 -> index 7).
	var resRead struct {
		Contents []mcp.ResourceContents `json:"contents"`
	}
	if err := json.Unmarshal(responses[7].Result, &resRead); err != nil || len(resRead.Contents) != 1 || resRead.Contents[0].URI != mcp.ResourceURILanguageGuide {
		t.Fatalf("resources/read result: err=%v contents=%+v", err, resRead.Contents)
	}
}

// TestStreamableHTTPJSONRPCAndErrorHandling verifies Streamable HTTP JSON-RPC
// dispatch, method validation, and domain error mapping.
func TestStreamableHTTPJSONRPCAndErrorHandling(t *testing.T) {
	t.Parallel()

	deps := &fakeMCPDeps{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := mcp.NewServer(mcp.Config{
		Transport:          mcp.TransportHTTP,
		HTTPAddr:           "127.0.0.1:0",
		DefaultWorkspaceID: uuid.NewString(),
		DefaultActorID:     uuid.NewString(),
	}, deps)

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ctx)
	}()

	// Wait briefly for listener bind via HTTPHandler and live socket check.
	postRPC := func(handler http.Handler, body string) (*http.Response, jsonRPCResponseEnvelope) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8081/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		resp := rec.Result()
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		var env jsonRPCResponseEnvelope
		if len(bytes.TrimSpace(raw)) > 0 {
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatalf("unmarshal HTTP JSON-RPC response %q: %v", string(raw), err)
			}
		}
		return resp, env
	}

	h := srv.HTTPHandler()

	// 1. Valid initialize over HTTP POST.
	resp, env := postRPC(h, `{"jsonrpc":"2.0","id":"init-1","method":"initialize","params":{"protocolVersion":"2025-03-26"}}`)
	if resp.StatusCode != http.StatusOK || env.Error != nil {
		t.Fatalf("HTTP initialize: status=%d err=%+v", resp.StatusCode, env.Error)
	}

	// 2. Notification over HTTP POST returns 202 Accepted with empty body.
	notifResp, _ := postRPC(h, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if notifResp.StatusCode != http.StatusAccepted {
		t.Fatalf("HTTP notification status = %d, want %d", notifResp.StatusCode, http.StatusAccepted)
	}

	// 3. Malformed JSON -> JSON-RPC -32700 Parse error.
	_, parseEnv := postRPC(h, `{"jsonrpc":"2.0","id":1,"method":`)
	if parseEnv.Error == nil || parseEnv.Error.Code != -32700 {
		t.Fatalf("malformed JSON error = %+v, want -32700", parseEnv.Error)
	}

	// 4. Unknown method -> JSON-RPC -32601 Method not found.
	_, unknownMethodEnv := postRPC(h, `{"jsonrpc":"2.0","id":2,"method":"unknown/method"}`)
	if unknownMethodEnv.Error == nil || unknownMethodEnv.Error.Code != -32601 {
		t.Fatalf("unknown method error = %+v, want -32601", unknownMethodEnv.Error)
	}

	// 5. Malformed tool arguments / host path leakage -> JSON-RPC -32602 Invalid params.
	_, badArgsEnv := postRPC(h, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"flowsk_execute","arguments":{"project_id":"/Users/mario/Desktop/repo","script":"digest"}}}`)
	if badArgsEnv.Error == nil || badArgsEnv.Error.Code != -32602 {
		t.Fatalf("host path argument should fail with -32602, got %+v", badArgsEnv.Error)
	}

	// 6. Domain error inside flowsk_execute -> ToolResult{IsError: true} without leaking raw Cause.
	deps.mu.Lock()
	deps.execErr = errs.Wrap(errs.ErrLeaseHeld, "task.Service.ClaimTask", "task lease held by peer", errors.New("SQLITE_BUSY: secret_path"))
	deps.mu.Unlock()

	_, domainErrEnv := postRPC(h, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"flowsk_execute","arguments":{"script":"return flowsk.tasks.claim(\"FLW-1\");"}}}`)
	if domainErrEnv.Error != nil {
		t.Fatalf("expected structured MCP ToolResult{IsError:true}, got protocol error %+v", domainErrEnv.Error)
	}
	var errToolRes mcp.ToolResult
	if err := json.Unmarshal(domainErrEnv.Result, &errToolRes); err != nil || !errToolRes.IsError || strings.Contains(errToolRes.Content[0].Text, "secret_path") {
		t.Fatalf("unexpected error ToolResult: err=%v res=%+v", err, errToolRes)
	}

	// Gracefully stop background HTTP server.
	cancel()
	select {
	case serveErr := <-errCh:
		if serveErr != nil {
			t.Fatalf("HTTP Serve shutdown error: %v", serveErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for HTTP MCP server graceful shutdown")
	}
}

func TestStreamableHTTPSecurityGuardsAndJSONRPCNotifications(t *testing.T) {
	t.Parallel()

	deps := &fakeMCPDeps{}
	const token = "bearer-prefixed-secret-token"

	srv := mcp.NewServer(mcp.Config{
		Transport:          mcp.TransportHTTP,
		HTTPAddr:           "127.0.0.1:8081",
		DefaultWorkspaceID: uuid.NewString(),
		DefaultActorID:     uuid.NewString(),
		AuthToken:          token,
		AllowedOrigins:     []string{"https://studio.example.com"},
	}, deps)
	h := srv.HTTPHandler()

	sendReq := func(target string, headers map[string]string, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	pingReq := `{"jsonrpc":"2.0","id":1,"method":"ping"}`

	// 1. DNS rebinding protection on loopback listener rejects non-loopback Host.
	recRebind := sendReq("http://evil.rebind.example:8081/mcp", map[string]string{
		"Authorization": "Bearer " + token,
		"Content-Type":  "application/json",
	}, pingReq)
	if recRebind.Code != http.StatusForbidden {
		t.Fatalf("DNS rebinding Host status = %d, want 403", recRebind.Code)
	}

	// 2. Missing/wrong/scheme-less Authorization -> 401 Unauthorized.
	for _, badAuth := range []string{"", token, "Basic dXNlcjpwYXNz", "Bearer wrong-token", "Bearer "} {
		hdrs := map[string]string{"Content-Type": "application/json"}
		if badAuth != "" {
			hdrs["Authorization"] = badAuth
		}
		rec := sendReq("http://127.0.0.1:8081/mcp", hdrs, pingReq)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("bad Authorization %q status = %d, want 401", badAuth, rec.Code)
		}
	}

	// 3. Case-insensitive "bearer" scheme + token starting with "bearer" succeeds, and Content-Type charset allowed.
	recOK := sendReq("http://localhost:8081/mcp", map[string]string{
		"Authorization": "bEaReR " + token,
		"Content-Type":  "Application/JSON; charset=utf-8",
	}, pingReq)
	if recOK.Code != http.StatusOK {
		t.Fatalf("valid bearer + charset JSON status = %d, want 200 (body=%q)", recOK.Code, recOK.Body.String())
	}

	// 4. Non-JSON Content-Type (browser simple-request CSRF vectors) -> 415 Unsupported Media Type.
	for _, badCT := range []string{"", "text/plain", "text/plain; charset=utf-8", "application/x-www-form-urlencoded"} {
		hdrs := map[string]string{"Authorization": "Bearer " + token}
		if badCT != "" {
			hdrs["Content-Type"] = badCT
		}
		rec := sendReq("http://127.0.0.1:8081/mcp", hdrs, pingReq)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("Content-Type %q status = %d, want 415", badCT, rec.Code)
		}
	}

	// 5. Cross-site / cross-origin browser requests rejected (403) unless in AllowedOrigins or same-host.
	recCrossSite := sendReq("http://127.0.0.1:8081/mcp", map[string]string{
		"Authorization":  "Bearer " + token,
		"Content-Type":   "application/json",
		"Sec-Fetch-Site": "cross-site",
	}, pingReq)
	if recCrossSite.Code != http.StatusForbidden {
		t.Fatalf("Sec-Fetch-Site: cross-site status = %d, want 403", recCrossSite.Code)
	}

	recBadOrigin := sendReq("http://127.0.0.1:8081/mcp", map[string]string{
		"Authorization": "Bearer " + token,
		"Content-Type":  "application/json",
		"Origin":        "https://evil.attacker.com",
	}, pingReq)
	if recBadOrigin.Code != http.StatusForbidden {
		t.Fatalf("untrusted Origin status = %d, want 403", recBadOrigin.Code)
	}

	recNullOrigin := sendReq("http://127.0.0.1:8081/mcp", map[string]string{
		"Authorization": "Bearer " + token,
		"Content-Type":  "application/json",
		"Origin":        "null",
	}, pingReq)
	if recNullOrigin.Code != http.StatusForbidden {
		t.Fatalf("null Origin status = %d, want 403", recNullOrigin.Code)
	}

	recSameOrigin := sendReq("http://[::1]:8081/mcp", map[string]string{
		"Authorization":  "Bearer " + token,
		"Content-Type":   "application/json",
		"Origin":         "http://[::1]:8081",
		"Sec-Fetch-Site": "same-origin",
	}, pingReq)
	if recSameOrigin.Code != http.StatusOK {
		t.Fatalf("same-origin IPv6 loopback status = %d, want 200", recSameOrigin.Code)
	}

	recAllowedOrigin := sendReq("http://127.0.0.1:8081/mcp", map[string]string{
		"Authorization":  "Bearer " + token,
		"Content-Type":   "application/json",
		"Origin":         "https://studio.example.com",
		"Sec-Fetch-Site": "cross-site",
	}, pingReq)
	if recAllowedOrigin.Code != http.StatusOK {
		t.Fatalf("explicitly allowed Origin status = %d, want 200", recAllowedOrigin.Code)
	}

	// 6. JSON-RPC 2.0 Notification compliance: requests without "id" NEVER emit a JSON-RPC body, regardless of method.
	for _, notif := range []string{
		`{"jsonrpc":"2.0","method":"ping"}`,
		`{"jsonrpc":"2.0","method":"tools/list"}`,
		`{"jsonrpc":"2.0","method":"initialize"}`,
		`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progress":50}}`,
	} {
		rec := sendReq("http://127.0.0.1:8081/mcp", map[string]string{
			"Authorization": "Bearer " + token,
			"Content-Type":  "application/json",
		}, notif)
		if rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
			t.Fatalf("notification %s -> status=%d body=%q, want 202 with empty body", notif, rec.Code, rec.Body.String())
		}
	}

	// Verify stdio also emits zero frames for notifications of any method name.
	var stdout bytes.Buffer
	stdioSrv := mcp.NewServer(mcp.Config{
		Transport: mcp.TransportStdio,
		Stdin: strings.NewReader(strings.Join([]string{
			`{"jsonrpc":"2.0","method":"ping"}`,
			`{"jsonrpc":"2.0","method":"tools/list"}`,
			`{"jsonrpc":"2.0","id":99,"method":"ping"}`,
		}, "\n") + "\n"),
		Stdout: &stdout,
	}, deps)
	if err := stdioSrv.Serve(context.Background()); err != nil {
		t.Fatalf("stdio notification test Serve: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], `"id":99`) {
		t.Fatalf("expected exactly 1 response frame (for id=99), got %d: %q", len(lines), stdout.String())
	}
}
