// File board_persistence_test.go verifies that custom project workflow states
// and labels created over ConnectRPC persist durably in SQLite across handler
// and server restarts instead of living only in volatile handler memory.
package connectrpc_test

import (
	"context"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	flowsk_project_pb "github.com/mindmain/flowsk/gen/go/flowsk/project/v1"
	flowsk_task_pb "github.com/mindmain/flowsk/gen/go/flowsk/task/v1"
	"github.com/mindmain/flowsk/internal/adapter/db/sqlite"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/project"
	"github.com/mindmain/flowsk/internal/domain/task"
	"github.com/mindmain/flowsk/internal/domain/workspace"
	"github.com/mindmain/flowsk/internal/transport/connectrpc/projecthandler"
	"github.com/mindmain/flowsk/internal/transport/connectrpc/taskhandler"
)

func TestProjectHandler_DurableWorkflowStatesAndLabelsAcrossRestarts(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbFile := filepath.Join(t.TempDir(), "board_meta.db")

	openStack := func(t *testing.T) (*sqlite.Adapter, *projecthandler.Handler, *taskhandler.Handler, workspace.Service) {
		t.Helper()
		adapter, err := sqlite.NewAdapter(sqlite.Config{
			FilePath:    dbFile,
			EnableWAL:   true,
			ForeignKeys: true,
		})
		if err != nil {
			t.Fatalf("open sqlite adapter: %v", err)
		}
		if _, err := adapter.Migrations().Up(ctx); err != nil {
			_ = adapter.Close()
			t.Fatalf("migrate sqlite adapter: %v", err)
		}
		wsSvc := workspace.NewService(adapter.WorkspaceRepository(), adapter.TxManager())
		projSvc := project.NewService(adapter.ProjectRepository(), adapter.TxManager())
		taskSvc := task.NewService(adapter.TaskRepository(), adapter.TxManager())

		projHandler := projecthandler.New(adapter, testProjectSvcDeps{svc: projSvc})
		taskHandler := taskhandler.New(adapter, testTaskSvcDeps{svc: taskSvc})
		return adapter, projHandler, taskHandler, wsSvc
	}

	// Phase 1: Create workspace, project, custom workflow state, and custom label.
	adapter1, projH1, _, wsSvc1 := openStack(t)
	actorID := uuid.New()
	bootCtx := authctx.WithIdentity(ctx, authctx.Identity{
		WorkspaceID: uuid.New(),
		ActorID:     actorID,
		Kind:        authctx.ActorKindHuman,
		Handle:      "architect",
	})
	ws, err := wsSvc1.CreateWorkspace(bootCtx, workspace.CreateWorkspaceInput{
		Slug: "durable-ws",
		Name: "Durable Workspace",
	})
	if err != nil {
		_ = adapter1.Close()
		t.Fatalf("create workspace: %v", err)
	}
	authedCtx := authctx.WithIdentity(ctx, authctx.Identity{
		WorkspaceID: ws.ID,
		ActorID:     actorID,
		Kind:        authctx.ActorKindHuman,
		Handle:      "architect",
	})

	projResp, err := projH1.CreateProject(authedCtx, connect.NewRequest(&flowsk_project_pb.CreateProjectRequest{
		WorkspaceId:               ws.ID.String(),
		ActorId:                   actorID.String(),
		Slug:                      "core-api",
		KeyPrefix:                 "CORE",
		Name:                      "Core API",
		SeedDefaultWorkflowStates: true,
	}))
	if err != nil {
		_ = adapter1.Close()
		t.Fatalf("CreateProject: %v", err)
	}
	projID := projResp.Msg.GetProject().GetId()

	stateResp, err := projH1.CreateWorkflowState(authedCtx, connect.NewRequest(&flowsk_project_pb.CreateWorkflowStateRequest{
		WorkspaceId: ws.ID.String(),
		ActorId:     actorID.String(),
		ProjectId:   projID,
		Name:        "Security Gate",
		Description: "Mandatory security sign-off",
		Category:    flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_STARTED,
		ColorHex:    "#F97316",
		Position:    45,
	}))
	if err != nil {
		_ = adapter1.Close()
		t.Fatalf("CreateWorkflowState: %v", err)
	}
	customStateID := stateResp.Msg.GetWorkflowState().GetId()

	lblResp, err := projH1.CreateLabel(authedCtx, connect.NewRequest(&flowsk_project_pb.CreateLabelRequest{
		WorkspaceId: ws.ID.String(),
		ActorId:     actorID.String(),
		ProjectId:   projID,
		Name:        "zero-trust",
		ColorHex:    "#10B981",
		Description: "Security boundary work",
	}))
	if err != nil {
		_ = adapter1.Close()
		t.Fatalf("CreateLabel: %v", err)
	}
	labelID := lblResp.Msg.GetLabel().GetId()
	_ = adapter1.Close()

	// Phase 2: Re-open brand-new SQLite adapter and fresh ConnectRPC handlers (zero in-memory state).
	adapter2, projH2, taskH2, _ := openStack(t)
	defer func() { _ = adapter2.Close() }()

	statesAfter, err := projH2.ListWorkflowStates(authedCtx, connect.NewRequest(&flowsk_project_pb.ListWorkflowStatesRequest{
		WorkspaceId: ws.ID.String(),
		ActorId:     actorID.String(),
		ProjectId:   projID,
	}))
	if err != nil {
		t.Fatalf("ListWorkflowStates after restart: %v", err)
	}
	foundCustomState := false
	for _, st := range statesAfter.Msg.GetWorkflowStates() {
		if st.GetId() == customStateID && st.GetName() == "Security Gate" {
			foundCustomState = true
			break
		}
	}
	if !foundCustomState {
		t.Fatalf("expected custom state %q (%s) to survive handler restart; got %d states", "Security Gate", customStateID, len(statesAfter.Msg.GetWorkflowStates()))
	}

	// Verify a task can transition into the persisted custom workflow state ID after restart.
	createdTask, err := taskH2.CreateTask(authedCtx, connect.NewRequest(&flowsk_task_pb.CreateTaskRequest{
		WorkspaceId: ws.ID.String(),
		ActorId:     actorID.String(),
		ProjectId:   projID,
		Title:       "Verify custom state transition",
		Priority:    flowsk_task_pb.TaskPriority_TASK_PRIORITY_HIGH,
	}))
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	movedTask, err := taskH2.TransitionTaskState(authedCtx, connect.NewRequest(&flowsk_task_pb.TransitionTaskStateRequest{
		WorkspaceId:     ws.ID.String(),
		ActorId:         actorID.String(),
		TaskId:          createdTask.Msg.GetTask().GetId(),
		WorkflowStateId: customStateID,
	}))
	if err != nil {
		t.Fatalf("TransitionTaskState to persisted custom state: %v", err)
	}
	if movedTask.Msg.GetTask().GetStateCategory() != flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_STARTED {
		t.Fatalf("expected task category STARTED after custom state transition, got %v", movedTask.Msg.GetTask().GetStateCategory())
	}

	labelsAfter, err := projH2.ListLabels(authedCtx, connect.NewRequest(&flowsk_project_pb.ListLabelsRequest{
		WorkspaceId: ws.ID.String(),
		ActorId:     actorID.String(),
		ProjectId:   projID,
	}))
	if err != nil {
		t.Fatalf("ListLabels after restart: %v", err)
	}
	if len(labelsAfter.Msg.GetLabels()) != 1 || labelsAfter.Msg.GetLabels()[0].GetId() != labelID || labelsAfter.Msg.GetLabels()[0].GetName() != "zero-trust" {
		t.Fatalf("expected persisted label zero-trust (%s) after restart, got %+v", labelID, labelsAfter.Msg.GetLabels())
	}

	if _, err := projH2.DeleteLabel(authedCtx, connect.NewRequest(&flowsk_project_pb.DeleteLabelRequest{
		WorkspaceId: ws.ID.String(),
		ActorId:     actorID.String(),
		ProjectId:   projID,
		LabelId:     labelID,
	})); err != nil {
		t.Fatalf("DeleteLabel after restart: %v", err)
	}
	labelsEmpty, err := projH2.ListLabels(authedCtx, connect.NewRequest(&flowsk_project_pb.ListLabelsRequest{
		WorkspaceId: ws.ID.String(),
		ActorId:     actorID.String(),
		ProjectId:   projID,
	}))
	if err != nil {
		t.Fatalf("ListLabels post-delete: %v", err)
	}
	if len(labelsEmpty.Msg.GetLabels()) != 0 {
		t.Fatalf("expected 0 labels after DeleteLabel, got %d", len(labelsEmpty.Msg.GetLabels()))
	}
}

type testProjectSvcDeps struct {
	svc project.Service
}

func (d testProjectSvcDeps) ProjectService() project.Service { return d.svc }

type testTaskSvcDeps struct {
	svc task.Service
}

func (d testTaskSvcDeps) TaskService() task.Service { return d.svc }
