// Package taskhandler implements the Buf ConnectRPC `TaskServiceHandler` transport
// adapter for tasks, DAG relations, exclusive agent claim leases, comments, work
// queues, and compact project digests.
package taskhandler

import (
	"context"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	flowsk_project_pb "github.com/mindmain/flowsk/gen/go/flowsk/project/v1"
	flowsk_task_pb "github.com/mindmain/flowsk/gen/go/flowsk/task/v1"
	"github.com/mindmain/flowsk/gen/go/flowsk/task/v1/flowsk_task_pbconnect"
	"github.com/mindmain/flowsk/internal/adapter/db"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/task"
	"github.com/mindmain/flowsk/internal/domain/tx"
	"github.com/mindmain/flowsk/internal/transport/connectrpc"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	maxTaskPageSize   int32 = 200
	maxQueuePageSize  int32 = 100
	maxLeaseTTLSecond int32 = 86400
)

// DataDependencies defines the segregated persistence provider contracts consumed
// by `New` to extract concrete task repository and transaction dependencies.
type DataDependencies interface {
	db.TxManagerProvider
	db.TaskRepoProvider
}

// ServicesDependencies defines the segregated application service provider contract
// consumed by `New` to extract the task domain service.
type ServicesDependencies interface {
	// TaskService returns the resolved task domain service.
	TaskService() task.Service
}

// Handler implements `flowsk_task_pbconnect.TaskServiceHandler` with pre-resolved
// domain dependencies and zero stored provider wrappers.
type Handler struct {
	txMgr tx.Manager
	repo  task.Repository
	svc   task.Service
}

// Compile-time assertion ensuring *Handler satisfies TaskServiceHandler.
var _ flowsk_task_pbconnect.TaskServiceHandler = (*Handler)(nil)

// New resolves `DataDependencies` and `ServicesDependencies` immediately at startup
// and returns an immutable `TaskService` ConnectRPC handler.
func New(dataDeps DataDependencies, svcDeps ServicesDependencies) *Handler {
	var (
		txMgr tx.Manager
		repo  task.Repository
		svc   task.Service
	)
	if dataDeps != nil {
		txMgr = dataDeps.TxManager()
		repo = dataDeps.TaskRepository()
	}
	if svcDeps != nil {
		svc = svcDeps.TaskService()
	}
	if svc == nil {
		svc = task.NewService(repo, txMgr)
	}
	return &Handler{
		txMgr: txMgr,
		repo:  repo,
		svc:   svc,
	}
}

// Service returns the resolved `task.Service` bound to this handler.
func (h *Handler) Service() task.Service {
	if h == nil {
		return nil
	}
	return h.svc
}

// Repository returns the resolved `task.Repository` bound to this handler.
func (h *Handler) Repository() task.Repository {
	if h == nil {
		return nil
	}
	return h.repo
}

// TxManager returns the resolved `tx.Manager` bound to this handler.
func (h *Handler) TxManager() tx.Manager {
	if h == nil {
		return nil
	}
	return h.txMgr
}

// CreateTask creates a new task inside an authorized workspace and project.
func (h *Handler) CreateTask(
	ctx context.Context,
	req *connect.Request[flowsk_task_pb.CreateTaskRequest],
) (*connect.Response[flowsk_task_pb.CreateTaskResponse], error) {
	const op = "taskhandler.CreateTask"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}

	workspaceID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	projectID, err := connectrpc.ParseRequiredUUID(op, "project_id", req.Msg.GetProjectId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	workflowStateID, err := connectrpc.ParseOptionalUUID(op, "workflow_state_id", req.Msg.WorkflowStateId)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	objectiveID, err := connectrpc.ParseOptionalUUID(op, "objective_id", req.Msg.ObjectiveId)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	parentTaskID, err := connectrpc.ParseOptionalUUID(op, "parent_task_id", req.Msg.ParentTaskId)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	assigneeActorID, err := connectrpc.ParseOptionalUUID(op, "assignee_actor_id", req.Msg.AssigneeActorId)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseUUIDSlice(op, "label_ids", req.Msg.GetLabelIds()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	kindPtr, err := parseTaskKind(op, req.Msg.GetKind(), true)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	priorityPtr, err := parseTaskPriority(op, req.Msg.GetPriority(), true)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	title := strings.TrimSpace(req.Msg.GetTitle())
	if title == "" {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "title is required", errs.FieldViolation{
			Field:       "title",
			Description: "title must not be empty",
		}))
	}

	initialStatus := task.StatusReady
	if workflowStateID != nil {
		resolvedStatus, ok := connectrpc.ResolveWorkflowStateStatus(projectID, *workflowStateID)
		if !ok {
			return nil, connectrpc.MapDomainError(errs.Validation(op, "unknown workflow_state_id", errs.FieldViolation{
				Field:       "workflow_state_id",
				Description: "workflow_state_id does not match any workflow state for this project",
			}))
		}
		initialStatus = resolvedStatus
	}

	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	kind := task.KindChore
	if kindPtr != nil {
		kind = *kindPtr
	}
	priority := task.PriorityMedium
	if priorityPtr != nil {
		priority = *priorityPtr
	}

	created, err := h.svc.CreateTask(ctx, task.CreateTaskInput{
		WorkspaceID:        workspaceID,
		ProjectID:          projectID,
		ObjectiveID:        objectiveID,
		ParentTaskID:       parentTaskID,
		ExternalID:         strings.TrimSpace(req.Msg.GetExternalId()),
		Kind:               kind,
		Title:              title,
		Description:        req.Msg.GetDescription(),
		AcceptanceCriteria: splitCriteria(req.Msg.GetAcceptanceCriteria()),
		Status:             initialStatus,
		Priority:           priority,
		AssigneeActorID:    assigneeActorID,
		Tags:               append([]string(nil), req.Msg.GetLabelIds()...),
	})
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	return connect.NewResponse(&flowsk_task_pb.CreateTaskResponse{
		Task: enrichTaskProto(&created),
	}), nil
}

// GetTask fetches a task and its dependency relations by UUID.
func (h *Handler) GetTask(
	ctx context.Context,
	req *connect.Request[flowsk_task_pb.GetTaskRequest],
) (*connect.Response[flowsk_task_pb.GetTaskResponse], error) {
	const op = "taskhandler.GetTask"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}

	workspaceID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	taskID, err := connectrpc.ParseRequiredUUID(op, "task_id", req.Msg.GetTaskId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	tsk, err := h.svc.GetTask(ctx, workspaceID, taskID)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	rels, err := h.svc.ListRelations(ctx, workspaceID, taskID)
	if err != nil && !errors.Is(err, errs.ErrNotImplemented) {
		return nil, connectrpc.MapDomainError(err)
	}

	return connect.NewResponse(&flowsk_task_pb.GetTaskResponse{
		Task:      enrichTaskProto(&tsk),
		Relations: relationsToProto(rels),
	}), nil
}

// GetTaskByIdentifier resolves a task and its dependency relations by short key (e.g. FLW-42).
func (h *Handler) GetTaskByIdentifier(
	ctx context.Context,
	req *connect.Request[flowsk_task_pb.GetTaskByIdentifierRequest],
) (*connect.Response[flowsk_task_pb.GetTaskByIdentifierResponse], error) {
	const op = "taskhandler.GetTaskByIdentifier"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}

	workspaceID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	identifier := strings.TrimSpace(req.Msg.GetIdentifier())
	if identifier == "" {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "identifier is required", errs.FieldViolation{
			Field:       "identifier",
			Description: "identifier must not be empty",
		}))
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	tsk, err := h.svc.GetTaskByKey(ctx, workspaceID, identifier)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	var rels []task.TaskRelation
	if tsk.ID != uuid.Nil {
		rels, err = h.svc.ListRelations(ctx, workspaceID, tsk.ID)
		if err != nil && !errors.Is(err, errs.ErrNotImplemented) {
			return nil, connectrpc.MapDomainError(err)
		}
	}

	return connect.NewResponse(&flowsk_task_pb.GetTaskByIdentifierResponse{
		Task:      enrichTaskProto(&tsk),
		Relations: relationsToProto(rels),
	}), nil
}

// GetTaskByExternalId resolves a task and its dependency relations by its
// project-scoped external tracker ID (e.g. CU-869abc, LIN-42).
func (h *Handler) GetTaskByExternalId(
	ctx context.Context,
	req *connect.Request[flowsk_task_pb.GetTaskByExternalIdRequest],
) (*connect.Response[flowsk_task_pb.GetTaskByExternalIdResponse], error) {
	const op = "taskhandler.GetTaskByExternalId"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}

	workspaceID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	projectID, err := connectrpc.ParseRequiredUUID(op, "project_id", req.Msg.GetProjectId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	extID := strings.TrimSpace(req.Msg.GetExternalId())
	if extID == "" {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "external_id is required", errs.FieldViolation{
			Field:       "external_id",
			Description: "external_id must not be empty",
		}))
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	tsk, err := h.svc.GetTaskByExternalID(ctx, workspaceID, projectID, extID)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	var rels []task.TaskRelation
	if tsk.ID != uuid.Nil {
		rels, err = h.svc.ListRelations(ctx, workspaceID, tsk.ID)
		if err != nil && !errors.Is(err, errs.ErrNotImplemented) {
			return nil, connectrpc.MapDomainError(err)
		}
	}

	return connect.NewResponse(&flowsk_task_pb.GetTaskByExternalIdResponse{
		Task:      enrichTaskProto(&tsk),
		Relations: relationsToProto(rels),
	}), nil
}

// ListTasks queries and filters tasks within a workspace or project.
func (h *Handler) ListTasks(
	ctx context.Context,
	req *connect.Request[flowsk_task_pb.ListTasksRequest],
) (*connect.Response[flowsk_task_pb.ListTasksResponse], error) {
	const op = "taskhandler.ListTasks"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}

	workspaceID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	projectIDPtr, err := connectrpc.ParseOptionalUUID(op, "project_id", req.Msg.ProjectId)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	objectiveIDPtr, err := connectrpc.ParseOptionalUUID(op, "objective_id", req.Msg.ObjectiveId)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	parentTaskIDPtr, err := connectrpc.ParseOptionalUUID(op, "parent_task_id", req.Msg.ParentTaskId)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseOptionalUUID(op, "workflow_state_id", req.Msg.WorkflowStateId); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	assigneeFilter, err := connectrpc.ParseOptionalUUID(op, "assignee_actor_id", req.Msg.AssigneeActorId)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	claimedByFilter, err := connectrpc.ParseOptionalUUID(op, "claimed_by_actor_id", req.Msg.ClaimedByActorId)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	labelFilter, err := connectrpc.ParseOptionalUUID(op, "label_id", req.Msg.LabelId)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	kindFilter, err := parseTaskKind(op, req.Msg.GetKindFilter(), true)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	priorityFilter, err := parseTaskPriority(op, req.Msg.GetPriorityFilter(), true)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	limit, offset, err := connectrpc.ParsePagination(op, req.Msg.GetPageSize(), maxTaskPageSize, req.Msg.GetPageToken())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	statuses, err := statusesForCategoryFilter(op, req.Msg.GetStateCategoryFilter())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	var projectID uuid.UUID
	if projectIDPtr != nil {
		projectID = *projectIDPtr
	}
	var requiredTags []string
	if labelFilter != nil {
		requiredTags = []string{labelFilter.String()}
	}

	extFilter := strings.TrimSpace(req.Msg.GetExternalIdFilter())

	summaries, err := h.svc.PollWorkQueue(ctx, task.AgentWorkQueueFilter{
		WorkspaceID:   workspaceID,
		ProjectID:     projectID,
		ObjectiveID:   objectiveIDPtr,
		ParentTaskID:  parentTaskIDPtr,
		ExternalID:    extFilter,
		Statuses:      statuses,
		RequiredTags:  requiredTags,
		OnlyUnclaimed: req.Msg.GetUnclaimedOnly(),
		Limit:         maxTaskPageSize,
	})
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	searchQuery := strings.ToLower(strings.TrimSpace(req.Msg.GetSearchQuery()))
	matched := make([]task.Task, 0, len(summaries))
	for _, s := range summaries {
		if kindFilter != nil && s.Kind != *kindFilter {
			continue
		}
		if priorityFilter != nil && s.Priority != *priorityFilter {
			continue
		}
		full, getErr := h.svc.GetTask(ctx, workspaceID, s.ID)
		if getErr != nil {
			continue
		}
		if extFilter != "" && full.ExternalID != extFilter {
			continue
		}
		if assigneeFilter != nil && (full.AssigneeActorID == nil || *full.AssigneeActorID != *assigneeFilter) {
			continue
		}
		if claimedByFilter != nil && (full.ActiveLease == nil || full.ActiveLease.HolderActorID != *claimedByFilter) {
			continue
		}
		if searchQuery != "" {
			haystack := strings.ToLower(full.Key + " " + full.ExternalID + " " + full.Title + " " + full.Description)
			if !strings.Contains(haystack, searchQuery) {
				continue
			}
		}
		matched = append(matched, full)
	}

	page, nextToken := connectrpc.PaginateSlice(matched, limit, offset)
	out := make([]*flowsk_task_pb.Task, 0, len(page))
	for i := range page {
		out = append(out, enrichTaskProto(&page[i]))
	}
	return connect.NewResponse(&flowsk_task_pb.ListTasksResponse{
		Tasks:         out,
		NextPageToken: nextToken,
	}), nil
}

// UpdateTask applies partial metadata updates to an existing task.
func (h *Handler) UpdateTask(
	ctx context.Context,
	req *connect.Request[flowsk_task_pb.UpdateTaskRequest],
) (*connect.Response[flowsk_task_pb.UpdateTaskResponse], error) {
	const op = "taskhandler.UpdateTask"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}

	workspaceID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	taskID, err := connectrpc.ParseRequiredUUID(op, "task_id", req.Msg.GetTaskId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	objectiveID, err := connectrpc.ParseOptionalUUID(op, "objective_id", req.Msg.ObjectiveId)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	parentTaskID, err := connectrpc.ParseOptionalUUID(op, "parent_task_id", req.Msg.ParentTaskId)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	assigneeActorID, err := connectrpc.ParseOptionalUUID(op, "assignee_actor_id", req.Msg.AssigneeActorId)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseUUIDSlice(op, "label_ids", req.Msg.GetLabelIds()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	kindPtr, err := parseTaskKind(op, req.Msg.GetKind(), true)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	priorityPtr, err := parseTaskPriority(op, req.Msg.GetPriority(), true)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	if req.Msg.GetClearObjective() {
		zero := uuid.Nil
		objectiveID = &zero
	}
	if req.Msg.GetClearParentTask() {
		zero := uuid.Nil
		parentTaskID = &zero
	}
	if req.Msg.GetClearAssignee() {
		zero := uuid.Nil
		assigneeActorID = &zero
	}

	var criteria []string
	if req.Msg.AcceptanceCriteria != nil {
		criteria = splitCriteria(*req.Msg.AcceptanceCriteria)
		if criteria == nil {
			criteria = []string{}
		}
	}
	var tags []string
	if req.Msg.GetReplaceLabels() {
		tags = append([]string{}, req.Msg.GetLabelIds()...)
	}
	var extIDPtr *string
	if req.Msg.GetClearExternalId() {
		empty := ""
		extIDPtr = &empty
	} else if req.Msg.ExternalId != nil {
		trimmed := strings.TrimSpace(*req.Msg.ExternalId)
		extIDPtr = &trimmed
	}

	updated, err := h.svc.UpdateTask(ctx, task.UpdateTaskInput{
		WorkspaceID:        workspaceID,
		TaskID:             taskID,
		ObjectiveID:        objectiveID,
		ParentTaskID:       parentTaskID,
		ExternalID:         extIDPtr,
		Kind:               kindPtr,
		Title:              req.Msg.Title,
		Description:        req.Msg.Description,
		AcceptanceCriteria: criteria,
		Priority:           priorityPtr,
		AssigneeActorID:    assigneeActorID,
		Tags:               tags,
	})
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_task_pb.UpdateTaskResponse{
		Task: enrichTaskProto(&updated),
	}), nil
}

// TransitionTaskState moves a task across workflow state columns atomically.
func (h *Handler) TransitionTaskState(
	ctx context.Context,
	req *connect.Request[flowsk_task_pb.TransitionTaskStateRequest],
) (*connect.Response[flowsk_task_pb.TransitionTaskStateResponse], error) {
	const op = "taskhandler.TransitionTaskState"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}

	workspaceID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	taskID, err := connectrpc.ParseRequiredUUID(op, "task_id", req.Msg.GetTaskId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	workflowStateID, err := connectrpc.ParseRequiredUUID(op, "workflow_state_id", req.Msg.GetWorkflowStateId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	targetStatus, ok := connectrpc.ResolveWorkflowStateStatus(uuid.Nil, workflowStateID)
	if !ok {
		existing, getErr := h.svc.GetTask(ctx, workspaceID, taskID)
		if getErr != nil {
			return nil, connectrpc.MapDomainError(getErr)
		}
		targetStatus, ok = connectrpc.ResolveWorkflowStateStatus(existing.ProjectID, workflowStateID)
		if !ok {
			return nil, connectrpc.MapDomainError(errs.Validation(op, "unknown workflow_state_id", errs.FieldViolation{
				Field:       "workflow_state_id",
				Description: "workflow_state_id does not belong to the target task's project",
			}))
		}
	}

	updated, err := h.svc.TransitionTask(ctx, task.TransitionTaskInput{
		WorkspaceID:        workspaceID,
		TaskID:             taskID,
		TargetStatus:       targetStatus,
		ResolutionComment:  req.Msg.GetResolutionNote(),
		ReleaseActiveLease: req.Msg.GetReleaseClaim(),
	})
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	protoTask := enrichTaskProto(&updated)
	if protoTask != nil {
		protoTask.WorkflowStateId = workflowStateID.String()
	}
	return connect.NewResponse(&flowsk_task_pb.TransitionTaskStateResponse{
		Task: protoTask,
	}), nil
}

// ClaimTask acquires or renews an exclusive time-bounded execution lease on a task.
func (h *Handler) ClaimTask(
	ctx context.Context,
	req *connect.Request[flowsk_task_pb.ClaimTaskRequest],
) (*connect.Response[flowsk_task_pb.ClaimTaskResponse], error) {
	const op = "taskhandler.ClaimTask"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}

	workspaceID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	taskID, err := connectrpc.ParseRequiredUUID(op, "task_id", req.Msg.GetTaskId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	ttl := req.Msg.GetLeaseTtlSeconds()
	if ttl < 0 || ttl > maxLeaseTTLSecond {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "invalid lease_ttl_seconds", errs.FieldViolation{
			Field:       "lease_ttl_seconds",
			Description: "lease_ttl_seconds must be between 0 and 86400",
		}))
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	if req.Msg.GetForceTakeover() {
		if relErr := h.svc.ReleaseTask(ctx, task.ReleaseTaskInput{
			WorkspaceID: workspaceID,
			TaskID:      taskID,
			Force:       true,
		}); relErr != nil && !errors.Is(relErr, errs.ErrNotFound) && !errors.Is(relErr, errs.ErrPreconditionFailed) {
			return nil, connectrpc.MapDomainError(relErr)
		}
	}

	lease, err := h.svc.ClaimTask(ctx, task.ClaimTaskInput{
		WorkspaceID:              workspaceID,
		TaskID:                   taskID,
		TTLSeconds:               int64(ttl),
		AutoTransitionInProgress: true,
	})
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	claimedTask, err := h.svc.GetTask(ctx, workspaceID, taskID)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if claimedTask.ActiveLease == nil {
		leaseCopy := lease.Clone()
		claimedTask.ActiveLease = &leaseCopy
	}
	return connect.NewResponse(&flowsk_task_pb.ClaimTaskResponse{
		Task: enrichTaskProto(&claimedTask),
	}), nil
}

// ReleaseTaskClaim releases an active ClaimLease and optionally records a handoff comment.
func (h *Handler) ReleaseTaskClaim(
	ctx context.Context,
	req *connect.Request[flowsk_task_pb.ReleaseTaskClaimRequest],
) (*connect.Response[flowsk_task_pb.ReleaseTaskClaimResponse], error) {
	const op = "taskhandler.ReleaseTaskClaim"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}

	workspaceID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	taskID, err := connectrpc.ParseRequiredUUID(op, "task_id", req.Msg.GetTaskId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	var leaseToken string
	if current, getErr := h.svc.GetTask(ctx, workspaceID, taskID); getErr == nil && current.ActiveLease != nil {
		leaseToken = current.ActiveLease.LeaseToken
	}

	if err := h.svc.ReleaseTask(ctx, task.ReleaseTaskInput{
		WorkspaceID:    workspaceID,
		TaskID:         taskID,
		LeaseToken:     leaseToken,
		HandoffComment: req.Msg.GetHandoffNote(),
	}); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	releasedTask, err := h.svc.GetTask(ctx, workspaceID, taskID)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_task_pb.ReleaseTaskClaimResponse{
		Task: enrichTaskProto(&releasedTask),
	}), nil
}

// AddTaskRelation creates a directed dependency link between two workspace tasks.
func (h *Handler) AddTaskRelation(
	ctx context.Context,
	req *connect.Request[flowsk_task_pb.AddTaskRelationRequest],
) (*connect.Response[flowsk_task_pb.AddTaskRelationResponse], error) {
	const op = "taskhandler.AddTaskRelation"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}

	workspaceID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	sourceTaskID, err := connectrpc.ParseRequiredUUID(op, "source_task_id", req.Msg.GetSourceTaskId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	targetTaskID, err := connectrpc.ParseRequiredUUID(op, "target_task_id", req.Msg.GetTargetTaskId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if sourceTaskID == targetTaskID {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "self-referential task relation is forbidden", errs.FieldViolation{
			Field:       "target_task_id",
			Description: "target_task_id must differ from source_task_id",
		}))
	}

	var relType task.RelationType
	switch req.Msg.GetRelationType() {
	case flowsk_task_pb.TaskRelationType_TASK_RELATION_TYPE_BLOCKS:
		relType = task.RelationBlocks
	case flowsk_task_pb.TaskRelationType_TASK_RELATION_TYPE_DUPLICATES:
		relType = task.RelationDuplicates
	case flowsk_task_pb.TaskRelationType_TASK_RELATION_TYPE_RELATES_TO:
		relType = task.RelationRelatesTo
	default:
		return nil, connectrpc.MapDomainError(errs.Validation(op, "invalid relation_type", errs.FieldViolation{
			Field:       "relation_type",
			Description: "relation_type must be BLOCKS, DUPLICATES, or RELATES_TO",
		}))
	}

	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	rel, err := h.svc.AddRelation(ctx, task.AddRelationInput{
		WorkspaceID:  workspaceID,
		SourceTaskID: sourceTaskID,
		TargetTaskID: targetTaskID,
		Type:         relType,
	})
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_task_pb.AddTaskRelationResponse{
		Relation: rel.ToProto(),
	}), nil
}

// RemoveTaskRelation removes a directed task relation edge by UUID.
func (h *Handler) RemoveTaskRelation(
	ctx context.Context,
	req *connect.Request[flowsk_task_pb.RemoveTaskRelationRequest],
) (*connect.Response[flowsk_task_pb.RemoveTaskRelationResponse], error) {
	const op = "taskhandler.RemoveTaskRelation"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}

	workspaceID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	relationID, err := connectrpc.ParseRequiredUUID(op, "relation_id", req.Msg.GetRelationId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	if err := h.svc.RemoveRelation(ctx, workspaceID, relationID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_task_pb.RemoveTaskRelationResponse{
		RemovedRelationId: relationID.String(),
	}), nil
}

// AddTaskComment appends a typed timeline comment to a task.
func (h *Handler) AddTaskComment(
	ctx context.Context,
	req *connect.Request[flowsk_task_pb.AddTaskCommentRequest],
) (*connect.Response[flowsk_task_pb.AddTaskCommentResponse], error) {
	const op = "taskhandler.AddTaskComment"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}

	workspaceID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	taskID, err := connectrpc.ParseRequiredUUID(op, "task_id", req.Msg.GetTaskId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	body := strings.TrimSpace(req.Msg.GetBody())
	if body == "" {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "comment body is required", errs.FieldViolation{
			Field:       "body",
			Description: "body must not be empty",
		}))
	}
	commentKind, err := parseCommentKind(op, req.Msg.GetKind())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	comment, err := h.svc.AddComment(ctx, task.AddCommentInput{
		WorkspaceID: workspaceID,
		TaskID:      taskID,
		Kind:        commentKind,
		Body:        req.Msg.GetBody(),
	})
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_task_pb.AddTaskCommentResponse{
		Comment: comment.ToProto(),
	}), nil
}

// ListTaskComments lists timeline comments on a task ordered chronologically.
func (h *Handler) ListTaskComments(
	ctx context.Context,
	req *connect.Request[flowsk_task_pb.ListTaskCommentsRequest],
) (*connect.Response[flowsk_task_pb.ListTaskCommentsResponse], error) {
	const op = "taskhandler.ListTaskComments"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}

	workspaceID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	taskID, err := connectrpc.ParseRequiredUUID(op, "task_id", req.Msg.GetTaskId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := parseCommentKind(op, req.Msg.GetKindFilter()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	limit, offset, err := connectrpc.ParsePagination(op, req.Msg.GetPageSize(), maxTaskPageSize, req.Msg.GetPageToken())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	comments, err := h.svc.ListComments(ctx, workspaceID, taskID, maxTaskPageSize)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	kindFilter := req.Msg.GetKindFilter()
	filtered := make([]task.TaskComment, 0, len(comments))
	for _, c := range comments {
		if kindFilter != flowsk_task_pb.TaskCommentKind_TASK_COMMENT_KIND_UNSPECIFIED && c.Kind.ToProto() != kindFilter {
			continue
		}
		filtered = append(filtered, c)
	}

	page, nextToken := connectrpc.PaginateSlice(filtered, limit, offset)
	out := make([]*flowsk_task_pb.TaskComment, 0, len(page))
	for i := range page {
		out = append(out, page[i].ToProto())
	}
	return connect.NewResponse(&flowsk_task_pb.ListTaskCommentsResponse{
		Comments:      out,
		NextPageToken: nextToken,
	}), nil
}

// GetAgentWorkQueue polls prioritized, unblocked, unclaimed tasks ready for execution.
func (h *Handler) GetAgentWorkQueue(
	ctx context.Context,
	req *connect.Request[flowsk_task_pb.GetAgentWorkQueueRequest],
) (*connect.Response[flowsk_task_pb.GetAgentWorkQueueResponse], error) {
	const op = "taskhandler.GetAgentWorkQueue"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}

	workspaceID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	projectIDPtr, err := connectrpc.ParseOptionalUUID(op, "project_id", req.Msg.ProjectId)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	objectiveIDPtr, err := connectrpc.ParseOptionalUUID(op, "objective_id", req.Msg.ObjectiveId)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	maxItems := req.Msg.GetMaxItems()
	if maxItems < 0 || maxItems > maxQueuePageSize {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "invalid max_items", errs.FieldViolation{
			Field:       "max_items",
			Description: "max_items must be between 0 and 100",
		}))
	}

	callerIdentity, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	var projectID uuid.UUID
	if projectIDPtr != nil {
		projectID = *projectIDPtr
	}

	ready, err := h.svc.PollWorkQueue(ctx, task.AgentWorkQueueFilter{
		WorkspaceID:   workspaceID,
		ProjectID:     projectID,
		ObjectiveID:   objectiveIDPtr,
		Statuses:      []task.Status{task.StatusReady},
		OnlyUnblocked: true,
		OnlyUnclaimed: true,
		Limit:         maxItems,
	})
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	readyProto := make([]*flowsk_task_pb.CompactTaskSummary, 0, len(ready))
	for i := range ready {
		readyProto = append(readyProto, ready[i].ToProto())
	}

	var claimedProto []*flowsk_task_pb.CompactTaskSummary
	if req.Msg.GetIncludeClaimedByCaller() {
		active, pollErr := h.svc.PollWorkQueue(ctx, task.AgentWorkQueueFilter{
			WorkspaceID:   workspaceID,
			ProjectID:     projectID,
			ObjectiveID:   objectiveIDPtr,
			Statuses:      []task.Status{task.StatusReady, task.StatusInProgress, task.StatusInReview, task.StatusBlocked},
			OnlyUnclaimed: false,
			Limit:         maxQueuePageSize,
		})
		if pollErr != nil {
			return nil, connectrpc.MapDomainError(pollErr)
		}
		for i := range active {
			if active[i].HolderHandle != "" && active[i].HolderHandle == callerIdentity.Handle {
				summaryProto := active[i].ToProto()
				if summaryProto != nil {
					callerIDStr := callerIdentity.ActorID.String()
					summaryProto.ClaimedByActorId = &callerIDStr
				}
				claimedProto = append(claimedProto, summaryProto)
			}
		}
	}

	return connect.NewResponse(&flowsk_task_pb.GetAgentWorkQueueResponse{
		ReadyTasks:      readyProto,
		ClaimedByCaller: claimedProto,
	}), nil
}

// GetProjectDigest computes a token-compact health and queue digest for a project.
func (h *Handler) GetProjectDigest(
	ctx context.Context,
	req *connect.Request[flowsk_task_pb.GetProjectDigestRequest],
) (*connect.Response[flowsk_task_pb.GetProjectDigestResponse], error) {
	const op = "taskhandler.GetProjectDigest"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}

	workspaceID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	projectID, err := connectrpc.ParseRequiredUUID(op, "project_id", req.Msg.GetProjectId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	digest, err := h.svc.GetProjectDigest(ctx, workspaceID, projectID, task.DigestOptions{})
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_task_pb.GetProjectDigestResponse{
		Digest: digest.ToProto(),
	}), nil
}

// DeleteTask soft-deletes a task within the caller's workspace.
func (h *Handler) DeleteTask(
	ctx context.Context,
	req *connect.Request[flowsk_task_pb.DeleteTaskRequest],
) (*connect.Response[flowsk_task_pb.DeleteTaskResponse], error) {
	const op = "taskhandler.DeleteTask"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}

	workspaceID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	taskID, err := connectrpc.ParseRequiredUUID(op, "task_id", req.Msg.GetTaskId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	if err := h.svc.DeleteTask(ctx, workspaceID, taskID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_task_pb.DeleteTaskResponse{
		DeletedTaskId: taskID.String(),
		DeletedAt:     timestamppb.New(time.Now().UTC()),
	}), nil
}

// GetTaskMarkdown retrieves the optional extended Markdown document for a task.
func (h *Handler) GetTaskMarkdown(
	ctx context.Context,
	req *connect.Request[flowsk_task_pb.GetTaskMarkdownRequest],
) (*connect.Response[flowsk_task_pb.GetTaskMarkdownResponse], error) {
	const op = "taskhandler.GetTaskMarkdown"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}
	workspaceID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	taskID, err := connectrpc.ParseRequiredUUID(op, "task_id", req.Msg.GetTaskId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	md, exists, err := h.svc.GetTaskMarkdown(ctx, workspaceID, taskID)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_task_pb.GetTaskMarkdownResponse{
		Markdown:       md,
		HasMarkdownDoc: exists,
	}), nil
}

// PutTaskMarkdown creates or overwrites the optional Markdown document for a task.
func (h *Handler) PutTaskMarkdown(
	ctx context.Context,
	req *connect.Request[flowsk_task_pb.PutTaskMarkdownRequest],
) (*connect.Response[flowsk_task_pb.PutTaskMarkdownResponse], error) {
	const op = "taskhandler.PutTaskMarkdown"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}
	workspaceID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	taskID, err := connectrpc.ParseRequiredUUID(op, "task_id", req.Msg.GetTaskId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	updated, err := h.svc.PutTaskMarkdown(ctx, workspaceID, taskID, req.Msg.GetMarkdown())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_task_pb.PutTaskMarkdownResponse{
		Task: enrichTaskProto(&updated),
	}), nil
}

// DeleteTaskMarkdown deletes the optional Markdown document associated with a task.
func (h *Handler) DeleteTaskMarkdown(
	ctx context.Context,
	req *connect.Request[flowsk_task_pb.DeleteTaskMarkdownRequest],
) (*connect.Response[flowsk_task_pb.DeleteTaskMarkdownResponse], error) {
	const op = "taskhandler.DeleteTaskMarkdown"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}
	workspaceID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	taskID, err := connectrpc.ParseRequiredUUID(op, "task_id", req.Msg.GetTaskId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	updated, err := h.svc.DeleteTaskMarkdown(ctx, workspaceID, taskID)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_task_pb.DeleteTaskMarkdownResponse{
		Task: enrichTaskProto(&updated),
	}), nil
}

// PurgeArchivedTasks permanently deletes all archived tasks across a workspace and removes their `.md` files.
func (h *Handler) PurgeArchivedTasks(
	ctx context.Context,
	req *connect.Request[flowsk_task_pb.PurgeArchivedTasksRequest],
) (*connect.Response[flowsk_task_pb.PurgeArchivedTasksResponse], error) {
	const op = "taskhandler.PurgeArchivedTasks"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}
	workspaceID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	res, err := h.svc.PurgeArchivedTasks(ctx, workspaceID)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_task_pb.PurgeArchivedTasksResponse{
		DeletedTaskCount:     res.DeletedTaskCount,
		DeletedMarkdownCount: res.DeletedMarkdownCount,
		DeletedTaskKeys:      append([]string(nil), res.DeletedTaskKeys...),
	}), nil
}

// InspectOrphanResources scans a workspace for orphan `.md` files and broken DB Markdown flags.
func (h *Handler) InspectOrphanResources(
	ctx context.Context,
	req *connect.Request[flowsk_task_pb.InspectOrphanResourcesRequest],
) (*connect.Response[flowsk_task_pb.InspectOrphanResourcesResponse], error) {
	const op = "taskhandler.InspectOrphanResources"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}
	workspaceID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	rep, err := h.svc.InspectOrphans(ctx, workspaceID)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_task_pb.InspectOrphanResourcesResponse{
		OrphanTaskFilesCount:    rep.OrphanTaskFilesCount,
		MissingTaskFilesCount:   rep.MissingTaskFilesCount,
		OrphanProjectFilesCount: rep.OrphanProjectFilesCount,
		MissingProjectFilesCount: rep.MissingProjectFilesCount,
		TotalIssues:             rep.TotalIssues,
		IssueSummaries:          append([]string(nil), rep.IssueSummaries...),
	}), nil
}

// PruneOrphanResources removes orphan `.md` files and clears broken DB Markdown flags in a workspace.
func (h *Handler) PruneOrphanResources(
	ctx context.Context,
	req *connect.Request[flowsk_task_pb.PruneOrphanResourcesRequest],
) (*connect.Response[flowsk_task_pb.PruneOrphanResourcesResponse], error) {
	const op = "taskhandler.PruneOrphanResources"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}
	workspaceID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	res, err := h.svc.PruneOrphans(ctx, workspaceID, req.Msg.GetDryRun())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_task_pb.PruneOrphanResourcesResponse{
		RemovedTaskFilesCount:    res.RemovedTaskFilesCount,
		ClearedTaskRefsCount:     res.ClearedTaskRefsCount,
		RemovedProjectFilesCount: res.RemovedProjectFilesCount,
		ClearedProjectRefsCount:  res.ClearedProjectRefsCount,
		TotalCleaned:             res.TotalCleaned,
		DryRun:                   res.DryRun,
		CleanedSummaries:         append([]string(nil), res.CleanedSummaries...),
	}), nil
}

func enrichTaskProto(t *task.Task) *flowsk_task_pb.Task {
	if t == nil {
		return nil
	}
	pb := t.ToProto()
	if pb != nil && pb.GetWorkflowStateId() == "" {
		pb.WorkflowStateId = connectrpc.WorkflowStateIDForStatus(t.ProjectID, t.Status).String()
	}
	return pb
}

func relationsToProto(rels []task.TaskRelation) []*flowsk_task_pb.TaskRelation {
	if len(rels) == 0 {
		return nil
	}
	out := make([]*flowsk_task_pb.TaskRelation, 0, len(rels))
	for i := range rels {
		out = append(out, rels[i].ToProto())
	}
	return out
}

func parseTaskKind(op string, k flowsk_task_pb.TaskKind, allowUnspecified bool) (*task.Kind, error) {
	switch k {
	case flowsk_task_pb.TaskKind_TASK_KIND_UNSPECIFIED:
		if allowUnspecified {
			return nil, nil
		}
		return nil, errs.Validation(op, "task kind is required", errs.FieldViolation{
			Field:       "kind",
			Description: "kind must not be TASK_KIND_UNSPECIFIED",
		})
	case flowsk_task_pb.TaskKind_TASK_KIND_TASK:
		v := task.KindChore
		return &v, nil
	case flowsk_task_pb.TaskKind_TASK_KIND_BUG:
		v := task.KindBug
		return &v, nil
	case flowsk_task_pb.TaskKind_TASK_KIND_FEATURE:
		v := task.KindFeature
		return &v, nil
	case flowsk_task_pb.TaskKind_TASK_KIND_ISSUE:
		v := task.KindResearch
		return &v, nil
	case flowsk_task_pb.TaskKind_TASK_KIND_EPIC:
		v := task.KindEpic
		return &v, nil
	default:
		return nil, errs.Validation(op, "invalid task kind", errs.FieldViolation{
			Field:       "kind",
			Description: "unsupported TaskKind enum value",
		})
	}
}

func parseTaskPriority(op string, p flowsk_task_pb.TaskPriority, allowUnspecified bool) (*task.Priority, error) {
	switch p {
	case flowsk_task_pb.TaskPriority_TASK_PRIORITY_UNSPECIFIED:
		if allowUnspecified {
			return nil, nil
		}
		return nil, errs.Validation(op, "task priority is required", errs.FieldViolation{
			Field:       "priority",
			Description: "priority must not be TASK_PRIORITY_UNSPECIFIED",
		})
	case flowsk_task_pb.TaskPriority_TASK_PRIORITY_URGENT:
		v := task.PriorityCritical
		return &v, nil
	case flowsk_task_pb.TaskPriority_TASK_PRIORITY_HIGH:
		v := task.PriorityHigh
		return &v, nil
	case flowsk_task_pb.TaskPriority_TASK_PRIORITY_MEDIUM,
		flowsk_task_pb.TaskPriority_TASK_PRIORITY_NONE:
		v := task.PriorityMedium
		return &v, nil
	case flowsk_task_pb.TaskPriority_TASK_PRIORITY_LOW:
		v := task.PriorityLow
		return &v, nil
	default:
		return nil, errs.Validation(op, "invalid task priority", errs.FieldViolation{
			Field:       "priority",
			Description: "unsupported TaskPriority enum value",
		})
	}
}

func parseCommentKind(op string, k flowsk_task_pb.TaskCommentKind) (task.CommentKind, error) {
	switch k {
	case flowsk_task_pb.TaskCommentKind_TASK_COMMENT_KIND_UNSPECIFIED,
		flowsk_task_pb.TaskCommentKind_TASK_COMMENT_KIND_COMMENT:
		return task.CommentKindNote, nil
	case flowsk_task_pb.TaskCommentKind_TASK_COMMENT_KIND_BLOCKER_REPORT:
		return task.CommentKindBlocker, nil
	case flowsk_task_pb.TaskCommentKind_TASK_COMMENT_KIND_HANDOFF:
		return task.CommentKindHandoff, nil
	case flowsk_task_pb.TaskCommentKind_TASK_COMMENT_KIND_RESOLUTION:
		return task.CommentKindDecision, nil
	default:
		return "", errs.Validation(op, "invalid comment kind", errs.FieldViolation{
			Field:       "kind",
			Description: "unsupported TaskCommentKind enum value",
		})
	}
}

func statusesForCategoryFilter(op string, cat flowsk_project_pb.WorkflowStateCategory) ([]task.Status, error) {
	switch cat {
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_UNSPECIFIED:
		return []task.Status{
			task.StatusBacklog,
			task.StatusReady,
			task.StatusInProgress,
			task.StatusInReview,
			task.StatusBlocked,
			task.StatusDone,
			task.StatusCancelled,
			task.StatusArchived,
		}, nil
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_TRIAGE,
		flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_BACKLOG:
		return []task.Status{task.StatusBacklog}, nil
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_UNSTARTED:
		return []task.Status{task.StatusReady}, nil
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_STARTED:
		return []task.Status{task.StatusInProgress, task.StatusInReview, task.StatusBlocked}, nil
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_COMPLETED:
		return []task.Status{task.StatusDone}, nil
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_CANCELED:
		return []task.Status{task.StatusCancelled}, nil
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_ARCHIVED:
		return []task.Status{task.StatusArchived}, nil
	default:
		return nil, errs.Validation(op, "invalid state_category_filter", errs.FieldViolation{
			Field:       "state_category_filter",
			Description: "unsupported WorkflowStateCategory enum value",
		})
	}
}

func splitCriteria(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	lines := strings.Split(raw, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
