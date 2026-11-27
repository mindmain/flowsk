// File stream_test.go verifies Go channel event fan-out, cross-tenant workspace
// isolation, SSE framing, and live audit relay delivery across workspaces.
package connectrpc_test

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	flowsk_language_pb "github.com/mindmain/flowsk/gen/go/flowsk/language/v1"
	"github.com/mindmain/flowsk/gen/go/flowsk/language/v1/flowsk_language_pbconnect"
	flowsk_workspace_pb "github.com/mindmain/flowsk/gen/go/flowsk/workspace/v1"
	"github.com/mindmain/flowsk/gen/go/flowsk/workspace/v1/flowsk_workspace_pbconnect"
	"github.com/mindmain/flowsk/internal/adapter/db/sqlite"
	"github.com/mindmain/flowsk/internal/domain/audit"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/project"
	"github.com/mindmain/flowsk/internal/domain/task"
	"github.com/mindmain/flowsk/internal/domain/workspace"
	"github.com/mindmain/flowsk/internal/transport/connectrpc"
	"github.com/mindmain/flowsk/internal/transport/connectrpc/languagehandler"
	"github.com/mindmain/flowsk/internal/transport/connectrpc/workspacehandler"
)

func TestEventBroker_TenantIsolationAndSSEStream(t *testing.T) {
	t.Parallel()

	wsA := uuid.New()
	wsB := uuid.New()
	actorA := uuid.New()

	broker := connectrpc.NewEventBroker(16)

	// 1. Verify Go channel Subscribe/Publish strict tenant isolation.
	chA, unsubA := broker.Subscribe(wsA)
	defer unsubA()
	chB, unsubB := broker.Subscribe(wsB)
	defer unsubB()

	broker.Publish(wsB, connectrpc.StreamEvent{
		ID:           uuid.NewString(),
		WorkspaceID:  wsB.String(),
		ActorID:      uuid.NewString(),
		Action:       "Create",
		ResourceType: "task",
		ResourceID:   "B-1",
	})

	select {
	case got := <-chB:
		if got.ResourceID != "B-1" {
			t.Fatalf("unexpected wsB event: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for wsB event")
	}

	select {
	case leaked := <-chA:
		t.Fatalf("cross-tenant leak into wsA channel: %+v", leaked)
	default:
	}

	// 2. Verify SSE HTTP handler rejects cross-tenant access (403) and streams authorized events.
	authFn := func(_ context.Context, r *http.Request) (authctx.Identity, uuid.UUID, error) {
		reqWS, err := uuid.Parse(r.URL.Query().Get("workspaceId"))
		if err != nil {
			return authctx.Identity{}, uuid.Nil, errs.Validation("test", "invalid workspaceId")
		}
		// Caller is authenticated ONLY for wsA.
		return authctx.Identity{
			WorkspaceID: wsA,
			ActorID:     actorA,
			Kind:        authctx.ActorKindHuman,
			Handle:      "operator-a",
		}, reqWS, nil
	}

	srv := httptest.NewServer(connectrpc.NewSSEHandler(broker, authFn, 5*time.Second))
	defer srv.Close()

	// Unauthorized foreign workspace subscription MUST fail with 403 Forbidden.
	respForbidden, err := http.Get(srv.URL + "?workspaceId=" + wsB.String())
	if err != nil {
		t.Fatalf("GET forbidden stream: %v", err)
	}
	_ = respForbidden.Body.Close()
	if respForbidden.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for foreign workspace stream, got %d", respForbidden.StatusCode)
	}

	// Authorized wsA SSE stream receives `ready` followed by real-time `mutation` event.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"?workspaceId="+wsA.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET authorized stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for authorized stream, got %d", resp.StatusCode)
	}

	reader := bufio.NewReader(resp.Body)
	var readyLine string
	for {
		line, rErr := reader.ReadString('\n')
		if rErr != nil {
			t.Fatalf("read SSE ready frame: %v", rErr)
		}
		if strings.HasPrefix(line, "event: ready") {
			readyLine = line
			break
		}
	}
	if readyLine == "" {
		t.Fatal("missing ready event")
	}

	broker.Publish(wsA, connectrpc.StreamEvent{
		ID:           uuid.NewString(),
		WorkspaceID:  wsA.String(),
		ActorID:      actorA.String(),
		ActorHandle:  "claude-agent",
		ActorKind:    "agent",
		Action:       "Transition",
		ResourceType: "task",
		ResourceID:   "FLW-99",
	})

	var foundPayload bool
	for {
		line, rErr := reader.ReadString('\n')
		if rErr != nil {
			t.Fatalf("read SSE mutation frame: %v", rErr)
		}
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, "FLW-99") {
			foundPayload = true
			break
		}
	}
	if !foundPayload {
		t.Fatal("expected FLW-99 mutation payload on SSE stream")
	}
}

func TestEventBroker_AuditRelayStreamsAgentMutations(t *testing.T) {
	t.Parallel()

	adapter, err := sqlite.NewAdapter(sqlite.Config{
		FilePath:    ":memory:",
		EnableWAL:   true,
		ForeignKeys: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adapter.Close() })

	baseCtx := context.Background()
	if _, err := adapter.Migrations().Up(baseCtx); err != nil {
		t.Fatal(err)
	}

	wsSvc := workspace.NewService(adapter.WorkspaceRepository(), adapter.TxManager())
	projSvc := project.NewService(adapter.ProjectRepository(), adapter.TxManager())
	taskSvc := task.NewService(adapter.TaskRepository(), adapter.TxManager())
	auditSvc := audit.NewService(adapter.AuditRepository(), adapter.TxManager())

	actorID := uuid.New()
	bootCtx := authctx.WithIdentity(baseCtx, authctx.Identity{
		WorkspaceID: uuid.New(),
		ActorID:     actorID,
		Kind:        authctx.ActorKindAgent,
		Handle:      "agent-worker",
	})
	ws, err := wsSvc.CreateWorkspace(bootCtx, workspace.CreateWorkspaceInput{
		Slug: "stream-ws",
		Name: "Stream Workspace",
	})
	if err != nil {
		t.Fatal(err)
	}
	authCtx := authctx.WithIdentity(baseCtx, authctx.Identity{
		WorkspaceID: ws.ID,
		ActorID:     actorID,
		Kind:        authctx.ActorKindAgent,
		Handle:      "agent-worker",
	})
	proj, err := projSvc.CreateProject(authCtx, project.CreateProjectInput{
		WorkspaceID: ws.ID,
		Slug:        "api",
		KeyPrefix:   "API",
		Name:        "API Service",
	})
	if err != nil {
		t.Fatal(err)
	}

	broker := connectrpc.NewEventBroker(16)
	relayCtx, stopRelay := context.WithCancel(baseCtx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		broker.RunAuditRelay(relayCtx, auditSvc, func(_ context.Context, wsID uuid.UUID) (authctx.Identity, bool) {
			return authctx.Identity{
				WorkspaceID: wsID,
				ActorID:     actorID,
				Kind:        authctx.ActorKindHuman,
				Handle:      "relay",
			}, true
		}, 40*time.Millisecond)
	}()
	defer func() {
		stopRelay()
		wg.Wait()
	}()

	subCh, unsub := broker.Subscribe(ws.ID)
	defer unsub()

	// Commit immediately after Subscribe returns (no sleep) to verify no initial-seed race!
	createdTask, err := taskSvc.CreateTask(authCtx, task.CreateTaskInput{
		WorkspaceID: ws.ID,
		ProjectID:   proj.ID,
		Kind:        task.KindFeature,
		Title:       "Streamed agent task",
		Status:      task.StatusReady,
		Priority:    task.PriorityHigh,
	})
	if err != nil {
		t.Fatal(err)
	}
	broker.NotifyWorkspace(ws.ID)

	select {
	case evt := <-subCh:
		if evt.ResourceType != "task" || evt.ResourceID != createdTask.ID.String() {
			t.Fatalf("expected task Create event for %s, got %+v", createdTask.ID, evt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for audit-relayed task creation event")
	}
}

func TestEventBroker_BurstOver50_UnsubscribeCleanup_AndBroadcastInterceptor(t *testing.T) {
	t.Parallel()

	adapter, err := sqlite.NewAdapter(sqlite.Config{
		FilePath:    ":memory:",
		EnableWAL:   true,
		ForeignKeys: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adapter.Close() })

	baseCtx := context.Background()
	if _, err := adapter.Migrations().Up(baseCtx); err != nil {
		t.Fatal(err)
	}

	wsSvc := workspace.NewService(adapter.WorkspaceRepository(), adapter.TxManager())
	projSvc := project.NewService(adapter.ProjectRepository(), adapter.TxManager())
	taskSvc := task.NewService(adapter.TaskRepository(), adapter.TxManager())
	auditSvc := audit.NewService(adapter.AuditRepository(), adapter.TxManager())

	actorID := uuid.New()
	bootCtx := authctx.WithIdentity(baseCtx, authctx.Identity{
		WorkspaceID: uuid.New(),
		ActorID:     actorID,
		Kind:        authctx.ActorKindAgent,
		Handle:      "burst-agent",
	})
	ws, err := wsSvc.CreateWorkspace(bootCtx, workspace.CreateWorkspaceInput{
		Slug: "burst-ws",
		Name: "Burst Workspace",
	})
	if err != nil {
		t.Fatal(err)
	}
	authCtx := authctx.WithIdentity(baseCtx, authctx.Identity{
		WorkspaceID: ws.ID,
		ActorID:     actorID,
		Kind:        authctx.ActorKindAgent,
		Handle:      "burst-agent",
	})
	proj, err := projSvc.CreateProject(authCtx, project.CreateProjectInput{
		WorkspaceID: ws.ID,
		Slug:        "burst",
		KeyPrefix:   "BRS",
		Name:        "Burst Project",
	})
	if err != nil {
		t.Fatal(err)
	}

	broker := connectrpc.NewEventBroker(256)
	relayCtx, stopRelay := context.WithCancel(baseCtx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		broker.RunAuditRelay(relayCtx, auditSvc, func(_ context.Context, wsID uuid.UUID) (authctx.Identity, bool) {
			return authctx.Identity{
				WorkspaceID: wsID,
				ActorID:     actorID,
				Kind:        authctx.ActorKindHuman,
				Handle:      "relay",
			}, true
		}, 10*time.Second) // Long poll interval so only explicit NotifyWorkspace triggers sync.
	}()
	defer func() {
		stopRelay()
		wg.Wait()
	}()

	// 1. Subscribe and burst 75 (>50) mutations before waking relay.
	subCh, unsub := broker.Subscribe(ws.ID)
	const burstCount = 75
	for i := 0; i < burstCount; i++ {
		if _, err := taskSvc.CreateTask(authCtx, task.CreateTaskInput{
			WorkspaceID: ws.ID,
			ProjectID:   proj.ID,
			Kind:        task.KindChore,
			Title:       fmt.Sprintf("Burst task %d", i),
			Status:      task.StatusReady,
			Priority:    task.PriorityMedium,
		}); err != nil {
			t.Fatalf("CreateTask %d: %v", i, err)
		}
	}
	broker.NotifyWorkspace(ws.ID)

	for i := 0; i < burstCount; i++ {
		select {
		case evt := <-subCh:
			if evt.ResourceType != "task" {
				t.Fatalf("event %d resourceType = %q, want task", i, evt.ResourceType)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out after %d/%d burst events", i, burstCount)
		}
	}

	// 2. Unsubscribe last listener -> workspace state cleaned up; offline CLI events are not replayed on reconnect.
	unsub()
	if active := broker.ActiveWorkspaces(); len(active) != 0 {
		t.Fatalf("expected 0 active workspaces after unsub, got %v", active)
	}

	offlineTask, err := taskSvc.CreateTask(authCtx, task.CreateTaskInput{
		WorkspaceID: ws.ID,
		ProjectID:   proj.ID,
		Kind:        task.KindChore,
		Title:       "Offline CLI task while disconnected",
		Status:      task.StatusReady,
		Priority:    task.PriorityLow,
	})
	if err != nil {
		t.Fatal(err)
	}

	subCh2, unsub2 := broker.Subscribe(ws.ID)
	defer unsub2()

	liveTask, err := taskSvc.CreateTask(authCtx, task.CreateTaskInput{
		WorkspaceID: ws.ID,
		ProjectID:   proj.ID,
		Kind:        task.KindFeature,
		Title:       "Fresh task after reconnect",
		Status:      task.StatusReady,
		Priority:    task.PriorityHigh,
	})
	if err != nil {
		t.Fatal(err)
	}
	broker.NotifyWorkspace(ws.ID)

	select {
	case evt := <-subCh2:
		if evt.ResourceID == offlineTask.ID.String() {
			t.Fatalf("unexpected replay of offline task %s after reconnect", offlineTask.ID)
		}
		if evt.ResourceID != liveTask.ID.String() {
			t.Fatalf("expected liveTask %s, got %+v", liveTask.ID, evt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for post-reconnect event")
	}

	// 3. Broadcast interceptor skips read-only ValidateScript and handles public CreateWorkspace cleanly.
	resolver := &connectrpc.HeaderIdentityResolver{
		PublicProcedures: map[string]bool{
			flowsk_workspace_pbconnect.WorkspaceServiceCreateWorkspaceProcedure: true,
		},
	}
	mux := connectrpc.NewMux(
		connectrpc.HandlerSet{
			Workspace: workspacehandler.New(adapter, nil),
			Language:  languagehandler.New(adapter, nil),
		},
		connect.WithInterceptors(
			connectrpc.NewAuthContextInterceptor(resolver),
			connectrpc.NewEventBroadcastInterceptor(broker),
		),
	)
	client := &inMemoryHTTPClient{handler: mux}
	langClient := flowsk_language_pbconnect.NewLanguageServiceClient(client, "http://127.0.0.1:8080")
	wsClient := flowsk_workspace_pbconnect.NewWorkspaceServiceClient(client, "http://127.0.0.1:8080")

	valReq := connect.NewRequest(&flowsk_language_pb.ValidateScriptRequest{
		WorkspaceId: ws.ID.String(),
		ActorId:     actorID.String(),
		Script:      "return 1 + 1;",
	})
	valReq.Header().Set(connectrpc.HeaderWorkspaceID, ws.ID.String())
	valReq.Header().Set(connectrpc.HeaderActorID, actorID.String())
	valReq.Header().Set(connectrpc.HeaderActorKind, "agent")
	valReq.Header().Set(connectrpc.HeaderActorHandle, "burst-agent")
	if _, err := langClient.ValidateScript(baseCtx, valReq); err != nil {
		t.Fatalf("ValidateScript through broadcast interceptor: %v", err)
	}

	if _, err := wsClient.CreateWorkspace(baseCtx, connect.NewRequest(&flowsk_workspace_pb.CreateWorkspaceRequest{
		Slug: "public-ws-broadcast",
		Name: "Public WS Broadcast",
	})); err != nil {
		t.Fatalf("public CreateWorkspace through broadcast interceptor: %v", err)
	}
}

func TestShouldSuppressSignalHeaders(t *testing.T) {
	t.Parallel()

	if connectrpc.ShouldSuppressSignalHeaders(nil) {
		t.Fatal("nil headers must not suppress task signal")
	}
	agentHeaders := make(http.Header)
	agentHeaders.Set(connectrpc.HeaderActorKind, "agent")
	if connectrpc.ShouldSuppressSignalHeaders(agentHeaders) {
		t.Fatal("standard agent headers must emit task signal")
	}

	webHeaders := make(http.Header)
	webHeaders.Set(connectrpc.HeaderClientSurface, "webui")
	if !connectrpc.ShouldSuppressSignalHeaders(webHeaders) {
		t.Fatal("X-Flowsk-Client: webui must suppress task signal")
	}

	internalHeaders := make(http.Header)
	internalHeaders.Set(connectrpc.HeaderSuppressSignal, "1")
	if !connectrpc.ShouldSuppressSignalHeaders(internalHeaders) {
		t.Fatal("X-Flowsk-Suppress-Signal: 1 must suppress task signal")
	}
}
