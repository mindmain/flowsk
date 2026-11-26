// File stream_test.go verifies Go channel event fan-out, cross-tenant workspace
// isolation, SSE framing, and live audit relay delivery across workspaces.
package connectrpc_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/adapter/db/sqlite"
	"github.com/mindmain/flowsk/internal/domain/audit"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/project"
	"github.com/mindmain/flowsk/internal/domain/task"
	"github.com/mindmain/flowsk/internal/domain/workspace"
	"github.com/mindmain/flowsk/internal/transport/connectrpc"
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

	// Allow initial baseline seed to complete.
	time.Sleep(80 * time.Millisecond)

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
