// Package projecthandler implements the Buf ConnectRPC `ProjectServiceHandler`
// transport adapter for logical workspace projects, workflow states, and labels.
//
// Zero Server Path Footprint Invariant:
//   - Project RPC payloads carry strictly logical identifiers (`id`, `slug`, `key_prefix`,
//     and descriptions); host directory mappings live exclusively on the client.
package projecthandler

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	flowsk_project_pb "github.com/mindmain/flowsk/gen/go/flowsk/project/v1"
	"github.com/mindmain/flowsk/gen/go/flowsk/project/v1/flowsk_project_pbconnect"
	"github.com/mindmain/flowsk/internal/adapter/db"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/project"
	"github.com/mindmain/flowsk/internal/domain/tx"
	"github.com/mindmain/flowsk/internal/transport/connectrpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	maxProjectPageSize     int32 = 200
	listProjectsFetchLimit int32 = 1000
)

// DataDependencies defines the segregated persistence provider contracts consumed
// by `New` to extract concrete project repository and transaction dependencies.
type DataDependencies interface {
	db.TxManagerProvider
	db.ProjectRepoProvider
}

// ServicesDependencies defines the segregated application service provider contract
// consumed by `New` to extract the project domain service.
type ServicesDependencies interface {
	// ProjectService returns the resolved project domain service.
	ProjectService() project.Service
}

// Handler implements `flowsk_project_pbconnect.ProjectServiceHandler` with
// pre-resolved domain dependencies and zero stored provider wrappers.
type Handler struct {
	txMgr tx.Manager
	repo  project.Repository
	svc   project.Service

	mu     sync.RWMutex
	states map[uuid.UUID][]*flowsk_project_pb.WorkflowState
	labels map[uuid.UUID][]*flowsk_project_pb.Label
}

// Compile-time assertion ensuring *Handler satisfies ProjectServiceHandler.
var _ flowsk_project_pbconnect.ProjectServiceHandler = (*Handler)(nil)

// New resolves `DataDependencies` and `ServicesDependencies` immediately at startup
// and returns an immutable `ProjectService` ConnectRPC handler.
func New(dataDeps DataDependencies, svcDeps ServicesDependencies) *Handler {
	var (
		txMgr tx.Manager
		repo  project.Repository
		svc   project.Service
	)
	if dataDeps != nil {
		txMgr = dataDeps.TxManager()
		repo = dataDeps.ProjectRepository()
	}
	if svcDeps != nil {
		svc = svcDeps.ProjectService()
	}
	if svc == nil {
		svc = project.NewService(repo, txMgr)
	}
	return &Handler{
		txMgr:  txMgr,
		repo:   repo,
		svc:    svc,
		states: make(map[uuid.UUID][]*flowsk_project_pb.WorkflowState),
		labels: make(map[uuid.UUID][]*flowsk_project_pb.Label),
	}
}

// Service returns the resolved `project.Service` bound to this handler.
func (h *Handler) Service() project.Service {
	if h == nil {
		return nil
	}
	return h.svc
}

// Repository returns the resolved `project.Repository` bound to this handler.
func (h *Handler) Repository() project.Repository {
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

// CreateProject creates a new logical project and optionally seeds canonical Linear-style workflow states.
func (h *Handler) CreateProject(
	ctx context.Context,
	req *connect.Request[flowsk_project_pb.CreateProjectRequest],
) (*connect.Response[flowsk_project_pb.CreateProjectResponse], error) {
	const op = "projecthandler.CreateProject"
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
	if _, err := parseProjectStatus(op, req.Msg.GetStatus(), true); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	slug := strings.TrimSpace(req.Msg.GetSlug())
	keyPrefix := strings.TrimSpace(req.Msg.GetKeyPrefix())
	name := strings.TrimSpace(req.Msg.GetName())
	if slug == "" || keyPrefix == "" || name == "" {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "slug, key_prefix, and name are required",
			errs.FieldViolation{Field: "slug", Description: "slug must not be empty"},
			errs.FieldViolation{Field: "key_prefix", Description: "key_prefix must not be empty"},
			errs.FieldViolation{Field: "name", Description: "name must not be empty"},
		))
	}
	if err := connectrpc.RejectHostFilesystemPath(op, "description", req.Msg.GetDescription()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	created, err := h.svc.CreateProject(ctx, project.CreateProjectInput{
		WorkspaceID: workspaceID,
		Slug:        slug,
		KeyPrefix:   keyPrefix,
		Name:        name,
		Description: req.Msg.GetDescription(),
	})
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	// If caller explicitly requested PAUSED or ARCHIVED initial status, apply it cleanly.
	if requestedStatus, statusErr := parseProjectStatus(op, req.Msg.GetStatus(), true); statusErr == nil && requestedStatus != nil && *requestedStatus != project.StatusActive {
		updated, updErr := h.svc.UpdateProject(ctx, project.UpdateProjectInput{
			WorkspaceID: workspaceID,
			ProjectID:   created.ID,
			Status:      requestedStatus,
		})
		if updErr != nil {
			return nil, connectrpc.MapDomainError(updErr)
		}
		created = updated
	}

	var seeded []*flowsk_project_pb.WorkflowState
	if req.Msg.GetSeedDefaultWorkflowStates() {
		seeded = h.ensureProjectStates(workspaceID, created.ID, created.CreatedAt)
	}

	return connect.NewResponse(&flowsk_project_pb.CreateProjectResponse{
		Project:        created.ToProto(),
		WorkflowStates: seeded,
	}), nil
}

// GetProject retrieves a project by UUID within the caller's authorized workspace.
func (h *Handler) GetProject(
	ctx context.Context,
	req *connect.Request[flowsk_project_pb.GetProjectRequest],
) (*connect.Response[flowsk_project_pb.GetProjectResponse], error) {
	const op = "projecthandler.GetProject"
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

	proj, err := h.svc.GetProject(ctx, workspaceID, projectID)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_project_pb.GetProjectResponse{
		Project: proj.ToProto(),
	}), nil
}

// GetProjectBySlug resolves a project by its workspace-unique slug.
func (h *Handler) GetProjectBySlug(
	ctx context.Context,
	req *connect.Request[flowsk_project_pb.GetProjectBySlugRequest],
) (*connect.Response[flowsk_project_pb.GetProjectBySlugResponse], error) {
	const op = "projecthandler.GetProjectBySlug"
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
	slug := strings.TrimSpace(req.Msg.GetSlug())
	if slug == "" {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "slug is required", errs.FieldViolation{
			Field:       "slug",
			Description: "slug must not be empty",
		}))
	}
	if err := connectrpc.RejectHostFilesystemPath(op, "slug", slug); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	proj, err := h.svc.GetProjectBySlug(ctx, workspaceID, slug)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_project_pb.GetProjectBySlugResponse{
		Project: proj.ToProto(),
	}), nil
}

// ListProjects lists workspace projects matching optional status filters and pagination.
func (h *Handler) ListProjects(
	ctx context.Context,
	req *connect.Request[flowsk_project_pb.ListProjectsRequest],
) (*connect.Response[flowsk_project_pb.ListProjectsResponse], error) {
	const op = "projecthandler.ListProjects"
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
	statusFilter, err := parseProjectStatus(op, req.Msg.GetStatusFilter(), true)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	limit, offset, err := connectrpc.ParsePagination(op, req.Msg.GetPageSize(), maxProjectPageSize, req.Msg.GetPageToken())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	items, err := h.svc.ListProjects(ctx, project.ListProjectsFilter{
		WorkspaceID: workspaceID,
		Status:      statusFilter,
		Limit:       listProjectsFetchLimit,
	})
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	filtered := make([]project.Project, 0, len(items))
	for _, item := range items {
		if statusFilter == nil && !req.Msg.GetIncludeArchived() && item.Status == project.StatusArchived {
			continue
		}
		filtered = append(filtered, item)
	}

	page, nextToken := connectrpc.PaginateSlice(filtered, limit, offset)
	out := make([]*flowsk_project_pb.Project, 0, len(page))
	for i := range page {
		out = append(out, page[i].ToProto())
	}
	return connect.NewResponse(&flowsk_project_pb.ListProjectsResponse{
		Projects:      out,
		NextPageToken: nextToken,
	}), nil
}

// UpdateProject updates mutable metadata on an existing project.
func (h *Handler) UpdateProject(
	ctx context.Context,
	req *connect.Request[flowsk_project_pb.UpdateProjectRequest],
) (*connect.Response[flowsk_project_pb.UpdateProjectResponse], error) {
	const op = "projecthandler.UpdateProject"
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
	statusPtr, err := parseProjectStatus(op, req.Msg.GetStatus(), true)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req.Msg.Description != nil {
		if err := connectrpc.RejectHostFilesystemPath(op, "description", *req.Msg.Description); err != nil {
			return nil, connectrpc.MapDomainError(err)
		}
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	updated, err := h.svc.UpdateProject(ctx, project.UpdateProjectInput{
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
		Name:        req.Msg.Name,
		Description: req.Msg.Description,
		Status:      statusPtr,
	})
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_project_pb.UpdateProjectResponse{
		Project: updated.ToProto(),
	}), nil
}

// ArchiveProject transitions a project into PROJECT_STATUS_ARCHIVED.
func (h *Handler) ArchiveProject(
	ctx context.Context,
	req *connect.Request[flowsk_project_pb.ArchiveProjectRequest],
) (*connect.Response[flowsk_project_pb.ArchiveProjectResponse], error) {
	const op = "projecthandler.ArchiveProject"
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

	archived := project.StatusArchived
	updated, err := h.svc.UpdateProject(ctx, project.UpdateProjectInput{
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
		Status:      &archived,
	})
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_project_pb.ArchiveProjectResponse{
		Project: updated.ToProto(),
	}), nil
}

// DeleteProject soft-deletes a project within the caller's workspace.
func (h *Handler) DeleteProject(
	ctx context.Context,
	req *connect.Request[flowsk_project_pb.DeleteProjectRequest],
) (*connect.Response[flowsk_project_pb.DeleteProjectResponse], error) {
	const op = "projecthandler.DeleteProject"
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

	if err := h.svc.DeleteProject(ctx, workspaceID, projectID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	h.mu.Lock()
	delete(h.states, projectID)
	delete(h.labels, projectID)
	h.mu.Unlock()
	connectrpc.UnregisterProjectWorkflowStates(projectID)

	return connect.NewResponse(&flowsk_project_pb.DeleteProjectResponse{
		DeletedProjectId: projectID.String(),
		DeletedAt:        timestamppb.New(time.Now().UTC()),
	}), nil
}

// CreateWorkflowState adds a custom Kanban workflow state column to an existing project.
func (h *Handler) CreateWorkflowState(
	ctx context.Context,
	req *connect.Request[flowsk_project_pb.CreateWorkflowStateRequest],
) (*connect.Response[flowsk_project_pb.CreateWorkflowStateResponse], error) {
	const op = "projecthandler.CreateWorkflowState"
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
	name := strings.TrimSpace(req.Msg.GetName())
	if name == "" {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "workflow state name is required", errs.FieldViolation{
			Field:       "name",
			Description: "name must not be empty",
		}))
	}
	if _, ok := connectrpc.StatusFromWorkflowCategory(req.Msg.GetCategory()); !ok {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "invalid workflow state category", errs.FieldViolation{
			Field:       "category",
			Description: "category must be a non-unspecified WorkflowStateCategory",
		}))
	}
	if req.Msg.GetPosition() < 0 {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "invalid position", errs.FieldViolation{
			Field:       "position",
			Description: "position must be >= 0",
		}))
	}
	if err := connectrpc.ValidateHexColor(op, "color_hex", req.Msg.GetColorHex()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	proj, err := h.svc.GetProject(ctx, workspaceID, projectID)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	if isCanonicalDefaultStateName(name) {
		return nil, connectrpc.MapDomainError(errs.New(errs.ErrConflict, op, "workflow state name already exists in project"))
	}

	if boardSvc, ok := h.svc.(project.BoardMetadataService); ok {
		saved, persistErr := boardSvc.CreateWorkflowState(ctx, project.CreateWorkflowStateInput{
			WorkspaceID: workspaceID,
			ProjectID:   projectID,
			Name:        name,
			Description: req.Msg.GetDescription(),
			Category:    protoWorkflowCategoryToDomain(req.Msg.GetCategory()),
			ColorHex:    req.Msg.GetColorHex(),
			Position:    req.Msg.GetPosition(),
			IsDefault:   req.Msg.GetIsDefault(),
		})
		if persistErr == nil {
			created := saved.ToProto()
			// Preserve caller description when SQL workflow_states stores name/category/color/position.
			if created.GetDescription() == "" && req.Msg.GetDescription() != "" {
				created.Description = req.Msg.GetDescription()
			}
			connectrpc.RegisterProjectWorkflowState(projectID, saved.ID, created.GetCategory())
			return connect.NewResponse(&flowsk_project_pb.CreateWorkflowStateResponse{
				WorkflowState: created,
			}), nil
		}
		if !isBoardMetadataUnsupported(persistErr) {
			return nil, connectrpc.MapDomainError(persistErr)
		}
	}

	stateID := uuid.New()
	now := timestamppb.New(time.Now().UTC())
	created := &flowsk_project_pb.WorkflowState{
		Id:          stateID.String(),
		WorkspaceId: workspaceID.String(),
		ProjectId:   projectID.String(),
		Name:        name,
		Description: req.Msg.GetDescription(),
		Category:    req.Msg.GetCategory(),
		ColorHex:    req.Msg.GetColorHex(),
		Position:    req.Msg.GetPosition(),
		IsDefault:   req.Msg.GetIsDefault(),
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	h.mu.Lock()
	existing := h.states[projectID]
	if len(existing) == 0 {
		existing = connectrpc.DefaultWorkflowStates(workspaceID, projectID, proj.CreatedAt)
	}
	for _, st := range existing {
		if strings.EqualFold(st.GetName(), name) {
			h.mu.Unlock()
			return nil, connectrpc.MapDomainError(errs.New(errs.ErrConflict, op, "workflow state name already exists in project"))
		}
	}
	if created.GetIsDefault() {
		for _, st := range existing {
			st.IsDefault = false
		}
	}
	existing = append(existing, cloneWorkflowState(created))
	sortWorkflowStates(existing)
	h.states[projectID] = existing
	h.mu.Unlock()

	connectrpc.RegisterProjectWorkflowState(projectID, stateID, created.GetCategory())

	return connect.NewResponse(&flowsk_project_pb.CreateWorkflowStateResponse{
		WorkflowState: created,
	}), nil
}

// ListWorkflowStates returns canonical Linear-style and custom workflow states ordered by position.
func (h *Handler) ListWorkflowStates(
	ctx context.Context,
	req *connect.Request[flowsk_project_pb.ListWorkflowStatesRequest],
) (*connect.Response[flowsk_project_pb.ListWorkflowStatesResponse], error) {
	const op = "projecthandler.ListWorkflowStates"
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
	catFilter := req.Msg.GetCategoryFilter()
	if catFilter != flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_UNSPECIFIED {
		if _, ok := connectrpc.StatusFromWorkflowCategory(catFilter); !ok {
			return nil, connectrpc.MapDomainError(errs.Validation(op, "invalid category_filter", errs.FieldViolation{
				Field:       "category_filter",
				Description: "unsupported WorkflowStateCategory enum value",
			}))
		}
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	proj, err := h.svc.GetProject(ctx, workspaceID, projectID)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	allStates, err := h.loadMergedWorkflowStates(ctx, workspaceID, projectID, proj.CreatedAt)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	out := make([]*flowsk_project_pb.WorkflowState, 0, len(allStates))
	for _, st := range allStates {
		if catFilter != flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_UNSPECIFIED && st.GetCategory() != catFilter {
			continue
		}
		out = append(out, st)
	}
	return connect.NewResponse(&flowsk_project_pb.ListWorkflowStatesResponse{
		WorkflowStates: out,
	}), nil
}

// UpdateWorkflowState modifies an existing workflow state column on a project.
func (h *Handler) UpdateWorkflowState(
	ctx context.Context,
	req *connect.Request[flowsk_project_pb.UpdateWorkflowStateRequest],
) (*connect.Response[flowsk_project_pb.UpdateWorkflowStateResponse], error) {
	const op = "projecthandler.UpdateWorkflowState"
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
	stateID, err := connectrpc.ParseRequiredUUID(op, "workflow_state_id", req.Msg.GetWorkflowStateId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req.Msg.ColorHex != nil {
		if err := connectrpc.ValidateHexColor(op, "color_hex", *req.Msg.ColorHex); err != nil {
			return nil, connectrpc.MapDomainError(err)
		}
	}
	if req.Msg.GetCategory() != flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_UNSPECIFIED {
		if _, ok := connectrpc.StatusFromWorkflowCategory(req.Msg.GetCategory()); !ok {
			return nil, connectrpc.MapDomainError(errs.Validation(op, "invalid category", errs.FieldViolation{
				Field:       "category",
				Description: "unsupported WorkflowStateCategory enum value",
			}))
		}
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	proj, err := h.svc.GetProject(ctx, workspaceID, projectID)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	if boardSvc, ok := h.svc.(project.BoardMetadataService); ok {
		var catPtr *string
		if req.Msg.GetCategory() != flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_UNSPECIFIED {
			c := protoWorkflowCategoryToDomain(req.Msg.GetCategory())
			catPtr = &c
		}
		saved, updErr := boardSvc.UpdateWorkflowState(ctx, project.UpdateWorkflowStateInput{
			WorkspaceID: workspaceID,
			ProjectID:   projectID,
			StateID:     stateID,
			Name:        req.Msg.Name,
			Description: req.Msg.Description,
			Category:    catPtr,
			ColorHex:    req.Msg.ColorHex,
			Position:    req.Msg.Position,
			IsDefault:   req.Msg.IsDefault,
		})
		if updErr == nil {
			updated := saved.ToProto()
			if req.Msg.Description != nil && updated.GetDescription() == "" {
				updated.Description = *req.Msg.Description
			}
			connectrpc.RegisterProjectWorkflowState(projectID, saved.ID, updated.GetCategory())
			return connect.NewResponse(&flowsk_project_pb.UpdateWorkflowStateResponse{
				WorkflowState: updated,
			}), nil
		}
		// Allow canonical UUIDv5 state updates to fall through to canonical state map;
		// propagate real validation/conflict/tenant errors immediately.
		if !isBoardMetadataUnsupported(updErr) && !errors.Is(updErr, errs.ErrNotFound) {
			return nil, connectrpc.MapDomainError(updErr)
		}
	}

	h.mu.Lock()
	existing := h.states[projectID]
	if len(existing) == 0 {
		existing = connectrpc.DefaultWorkflowStates(workspaceID, projectID, proj.CreatedAt)
	}
	var updated *flowsk_project_pb.WorkflowState
	for _, st := range existing {
		if st.GetId() == stateID.String() {
			if req.Msg.Name != nil {
				st.Name = strings.TrimSpace(*req.Msg.Name)
			}
			if req.Msg.Description != nil {
				st.Description = *req.Msg.Description
			}
			if req.Msg.GetCategory() != flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_UNSPECIFIED {
				st.Category = req.Msg.GetCategory()
			}
			if req.Msg.ColorHex != nil {
				st.ColorHex = *req.Msg.ColorHex
			}
			if req.Msg.Position != nil {
				st.Position = *req.Msg.Position
			}
			if req.Msg.IsDefault != nil {
				if *req.Msg.IsDefault {
					for _, peer := range existing {
						peer.IsDefault = false
					}
				}
				st.IsDefault = *req.Msg.IsDefault
			}
			st.UpdatedAt = timestamppb.New(time.Now().UTC())
			updated = cloneWorkflowState(st)
			break
		}
	}
	if updated != nil {
		sortWorkflowStates(existing)
		h.states[projectID] = existing
	}
	h.mu.Unlock()

	if updated == nil {
		return nil, connectrpc.MapDomainError(errs.New(errs.ErrNotFound, op, "workflow state not found"))
	}
	connectrpc.RegisterProjectWorkflowState(projectID, stateID, updated.GetCategory())
	return connect.NewResponse(&flowsk_project_pb.UpdateWorkflowStateResponse{
		WorkflowState: updated,
	}), nil
}

// CreateLabel adds a classification label to a project.
func (h *Handler) CreateLabel(
	ctx context.Context,
	req *connect.Request[flowsk_project_pb.CreateLabelRequest],
) (*connect.Response[flowsk_project_pb.CreateLabelResponse], error) {
	const op = "projecthandler.CreateLabel"
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
	name := strings.TrimSpace(req.Msg.GetName())
	if name == "" {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "label name is required", errs.FieldViolation{
			Field:       "name",
			Description: "name must not be empty",
		}))
	}
	if err := connectrpc.ValidateHexColor(op, "color_hex", req.Msg.GetColorHex()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := h.svc.GetProject(ctx, workspaceID, projectID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	if boardSvc, ok := h.svc.(project.BoardMetadataService); ok {
		saved, persistErr := boardSvc.CreateLabel(ctx, project.CreateProjectLabelInput{
			WorkspaceID: workspaceID,
			ProjectID:   projectID,
			Name:        name,
			ColorHex:    req.Msg.GetColorHex(),
			Description: req.Msg.GetDescription(),
		})
		if persistErr == nil {
			return connect.NewResponse(&flowsk_project_pb.CreateLabelResponse{
				Label: saved.ToProto(),
			}), nil
		}
		if !isBoardMetadataUnsupported(persistErr) {
			return nil, connectrpc.MapDomainError(persistErr)
		}
	}

	now := timestamppb.New(time.Now().UTC())
	created := &flowsk_project_pb.Label{
		Id:          uuid.NewString(),
		WorkspaceId: workspaceID.String(),
		ProjectId:   projectID.String(),
		Name:        name,
		Description: req.Msg.GetDescription(),
		ColorHex:    req.Msg.GetColorHex(),
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	h.mu.Lock()
	existing := h.labels[projectID]
	for _, lbl := range existing {
		if strings.EqualFold(lbl.GetName(), name) {
			h.mu.Unlock()
			return nil, connectrpc.MapDomainError(errs.New(errs.ErrConflict, op, "label name already exists in project"))
		}
	}
	existing = append(existing, cloneLabel(created))
	sort.Slice(existing, func(i, j int) bool {
		return strings.ToLower(existing[i].GetName()) < strings.ToLower(existing[j].GetName())
	})
	h.labels[projectID] = existing
	h.mu.Unlock()

	return connect.NewResponse(&flowsk_project_pb.CreateLabelResponse{
		Label: created,
	}), nil
}

// ListLabels returns all classification labels configured on a project ordered alphabetically.
func (h *Handler) ListLabels(
	ctx context.Context,
	req *connect.Request[flowsk_project_pb.ListLabelsRequest],
) (*connect.Response[flowsk_project_pb.ListLabelsResponse], error) {
	const op = "projecthandler.ListLabels"
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
	if _, err := h.svc.GetProject(ctx, workspaceID, projectID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	if boardSvc, ok := h.svc.(project.BoardMetadataService); ok {
		persisted, listErr := boardSvc.ListLabels(ctx, workspaceID, projectID)
		if listErr == nil {
			out := make([]*flowsk_project_pb.Label, 0, len(persisted))
			for i := range persisted {
				out = append(out, persisted[i].ToProto())
			}
			sort.Slice(out, func(i, j int) bool {
				return strings.ToLower(out[i].GetName()) < strings.ToLower(out[j].GetName())
			})
			return connect.NewResponse(&flowsk_project_pb.ListLabelsResponse{
				Labels: out,
			}), nil
		}
		if !isBoardMetadataUnsupported(listErr) {
			return nil, connectrpc.MapDomainError(listErr)
		}
	}

	h.mu.RLock()
	src := h.labels[projectID]
	out := make([]*flowsk_project_pb.Label, 0, len(src))
	for _, lbl := range src {
		out = append(out, cloneLabel(lbl))
	}
	h.mu.RUnlock()

	return connect.NewResponse(&flowsk_project_pb.ListLabelsResponse{
		Labels: out,
	}), nil
}

// DeleteLabel removes a classification label from a project.
func (h *Handler) DeleteLabel(
	ctx context.Context,
	req *connect.Request[flowsk_project_pb.DeleteLabelRequest],
) (*connect.Response[flowsk_project_pb.DeleteLabelResponse], error) {
	const op = "projecthandler.DeleteLabel"
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
	labelID, err := connectrpc.ParseRequiredUUID(op, "label_id", req.Msg.GetLabelId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := h.svc.GetProject(ctx, workspaceID, projectID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	if boardSvc, ok := h.svc.(project.BoardMetadataService); ok {
		delErr := boardSvc.DeleteLabel(ctx, workspaceID, projectID, labelID)
		if delErr == nil {
			return connect.NewResponse(&flowsk_project_pb.DeleteLabelResponse{
				DeletedLabelId: labelID.String(),
			}), nil
		}
		if !isBoardMetadataUnsupported(delErr) {
			return nil, connectrpc.MapDomainError(delErr)
		}
	}

	h.mu.Lock()
	existing := h.labels[projectID]
	found := false
	filtered := make([]*flowsk_project_pb.Label, 0, len(existing))
	for _, lbl := range existing {
		if lbl.GetId() == labelID.String() {
			found = true
			continue
		}
		filtered = append(filtered, lbl)
	}
	h.labels[projectID] = filtered
	h.mu.Unlock()

	if !found {
		return nil, connectrpc.MapDomainError(errs.New(errs.ErrNotFound, op, "label not found"))
	}
	return connect.NewResponse(&flowsk_project_pb.DeleteLabelResponse{
		DeletedLabelId: labelID.String(),
	}), nil
}

// BindSubproject creates or updates a parent->child sub-project edge in the multi-binding DAG.
func (h *Handler) BindSubproject(
	ctx context.Context,
	req *connect.Request[flowsk_project_pb.BindSubprojectRequest],
) (*connect.Response[flowsk_project_pb.BindSubprojectResponse], error) {
	const op = "projecthandler.BindSubproject"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}
	wsID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	parentID, err := connectrpc.ParseRequiredUUID(op, "parent_project_id", req.Msg.GetParentProjectId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	childID, err := connectrpc.ParseRequiredUUID(op, "child_project_id", req.Msg.GetChildProjectId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, wsID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	binding, err := h.svc.BindSubproject(ctx, project.BindSubprojectInput{
		WorkspaceID:     wsID,
		ParentProjectID: parentID,
		ChildProjectID:  childID,
		ScopeSubpath:    req.Msg.GetScopeSubpath(),
	})
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_project_pb.BindSubprojectResponse{
		Binding: binding.ToProto(),
	}), nil
}

// UnbindSubproject removes a parent->child project binding without deleting either project.
func (h *Handler) UnbindSubproject(
	ctx context.Context,
	req *connect.Request[flowsk_project_pb.UnbindSubprojectRequest],
) (*connect.Response[flowsk_project_pb.UnbindSubprojectResponse], error) {
	const op = "projecthandler.UnbindSubproject"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}
	wsID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	parentID, err := connectrpc.ParseRequiredUUID(op, "parent_project_id", req.Msg.GetParentProjectId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	childID, err := connectrpc.ParseRequiredUUID(op, "child_project_id", req.Msg.GetChildProjectId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, wsID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	unbound, err := h.svc.UnbindSubproject(ctx, project.UnbindSubprojectInput{
		WorkspaceID:     wsID,
		ParentProjectID: parentID,
		ChildProjectID:  childID,
	})
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_project_pb.UnbindSubprojectResponse{
		Unbound: unbound,
	}), nil
}

// ListProjectBindings returns active parent->child project bindings in the workspace.
func (h *Handler) ListProjectBindings(
	ctx context.Context,
	req *connect.Request[flowsk_project_pb.ListProjectBindingsRequest],
) (*connect.Response[flowsk_project_pb.ListProjectBindingsResponse], error) {
	const op = "projecthandler.ListProjectBindings"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}
	wsID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	parentID, err := connectrpc.ParseOptionalUUID(op, "parent_project_id", req.Msg.ParentProjectId)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	childID, err := connectrpc.ParseOptionalUUID(op, "child_project_id", req.Msg.ChildProjectId)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, wsID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	items, err := h.svc.ListBindings(ctx, project.ListBindingsFilter{
		WorkspaceID:     wsID,
		ParentProjectID: parentID,
		ChildProjectID:  childID,
	})
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	out := make([]*flowsk_project_pb.ProjectBinding, 0, len(items))
	for _, it := range items {
		out = append(out, it.ToProto())
	}
	return connect.NewResponse(&flowsk_project_pb.ListProjectBindingsResponse{
		Bindings: out,
	}), nil
}

// GetProjectMarkdown retrieves the optional extended Markdown document for a project.
func (h *Handler) GetProjectMarkdown(
	ctx context.Context,
	req *connect.Request[flowsk_project_pb.GetProjectMarkdownRequest],
) (*connect.Response[flowsk_project_pb.GetProjectMarkdownResponse], error) {
	const op = "projecthandler.GetProjectMarkdown"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}
	wsID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	projID, err := connectrpc.ParseRequiredUUID(op, "project_id", req.Msg.GetProjectId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, wsID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	md, exists, err := h.svc.GetProjectMarkdown(ctx, wsID, projID)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_project_pb.GetProjectMarkdownResponse{
		Markdown:       md,
		HasMarkdownDoc: exists,
	}), nil
}

// PutProjectMarkdown creates or overwrites the optional Markdown document for a project.
func (h *Handler) PutProjectMarkdown(
	ctx context.Context,
	req *connect.Request[flowsk_project_pb.PutProjectMarkdownRequest],
) (*connect.Response[flowsk_project_pb.PutProjectMarkdownResponse], error) {
	const op = "projecthandler.PutProjectMarkdown"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}
	wsID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	projID, err := connectrpc.ParseRequiredUUID(op, "project_id", req.Msg.GetProjectId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, wsID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	updated, err := h.svc.PutProjectMarkdown(ctx, wsID, projID, req.Msg.GetMarkdown())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_project_pb.PutProjectMarkdownResponse{
		Project: updated.ToProto(),
	}), nil
}

// DeleteProjectMarkdown deletes the optional Markdown document for a project.
func (h *Handler) DeleteProjectMarkdown(
	ctx context.Context,
	req *connect.Request[flowsk_project_pb.DeleteProjectMarkdownRequest],
) (*connect.Response[flowsk_project_pb.DeleteProjectMarkdownResponse], error) {
	const op = "projecthandler.DeleteProjectMarkdown"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}
	wsID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	projID, err := connectrpc.ParseRequiredUUID(op, "project_id", req.Msg.GetProjectId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, wsID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	updated, err := h.svc.DeleteProjectMarkdown(ctx, wsID, projID)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_project_pb.DeleteProjectMarkdownResponse{
		Project: updated.ToProto(),
	}), nil
}

func (h *Handler) ensureProjectStates(workspaceID, projectID uuid.UUID, createdAt time.Time) []*flowsk_project_pb.WorkflowState {
	h.mu.Lock()
	defer h.mu.Unlock()
	existing := h.states[projectID]
	if len(existing) == 0 {
		existing = connectrpc.DefaultWorkflowStates(workspaceID, projectID, createdAt)
		h.states[projectID] = existing
	}
	out := make([]*flowsk_project_pb.WorkflowState, 0, len(existing))
	for _, st := range existing {
		out = append(out, cloneWorkflowState(st))
	}
	return out
}

func (h *Handler) loadMergedWorkflowStates(ctx context.Context, workspaceID, projectID uuid.UUID, createdAt time.Time) ([]*flowsk_project_pb.WorkflowState, error) {
	base := h.ensureProjectStates(workspaceID, projectID, createdAt)
	boardSvc, ok := h.svc.(project.BoardMetadataService)
	if !ok {
		return base, nil
	}
	persisted, err := boardSvc.ListWorkflowStates(ctx, workspaceID, projectID)
	if err != nil {
		if isBoardMetadataUnsupported(err) {
			return base, nil
		}
		return nil, err
	}
	if len(persisted) == 0 {
		return base, nil
	}

	seenID := make(map[string]struct{}, len(base)+len(persisted))
	seenCanonical := make(map[string]struct{}, len(base))
	for _, st := range base {
		seenID[st.GetId()] = struct{}{}
		seenCanonical[strings.ToLower(strings.TrimSpace(st.GetName()))] = struct{}{}
	}

	customDefault := false
	extras := make([]*flowsk_project_pb.WorkflowState, 0, len(persisted))
	for i := range persisted {
		stProto := persisted[i].ToProto()
		connectrpc.RegisterProjectWorkflowState(projectID, persisted[i].ID, stProto.GetCategory())
		lowerName := strings.ToLower(strings.TrimSpace(stProto.GetName()))
		if _, isCanonical := seenCanonical[lowerName]; isCanonical {
			continue
		}
		if _, exists := seenID[stProto.GetId()]; exists {
			continue
		}
		if stProto.GetIsDefault() {
			customDefault = true
		}
		seenID[stProto.GetId()] = struct{}{}
		extras = append(extras, stProto)
	}
	if customDefault {
		for _, st := range base {
			st.IsDefault = false
		}
	}
	merged := append(base, extras...)
	sortWorkflowStates(merged)
	return merged, nil
}

func isCanonicalDefaultStateName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "triage", "backlog", "todo", "in progress", "in review", "blocked", "done", "canceled", "cancelled", "archived":
		return true
	default:
		return false
	}
}

func protoWorkflowCategoryToDomain(cat flowsk_project_pb.WorkflowStateCategory) string {
	switch cat {
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_TRIAGE:
		return "triage"
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_BACKLOG:
		return "backlog"
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_UNSTARTED:
		return "unstarted"
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_STARTED:
		return "started"
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_COMPLETED:
		return "completed"
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_CANCELED:
		return "canceled"
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_ARCHIVED:
		return "archived"
	default:
		return "unstarted"
	}
}

func isBoardMetadataUnsupported(err error) bool {
	return errors.Is(err, errs.ErrPreconditionFailed) && strings.Contains(err.Error(), "does not support")
}

func cloneWorkflowState(st *flowsk_project_pb.WorkflowState) *flowsk_project_pb.WorkflowState {
	if st == nil {
		return nil
	}
	cloned, _ := proto.Clone(st).(*flowsk_project_pb.WorkflowState)
	return cloned
}

func cloneLabel(lbl *flowsk_project_pb.Label) *flowsk_project_pb.Label {
	if lbl == nil {
		return nil
	}
	cloned, _ := proto.Clone(lbl).(*flowsk_project_pb.Label)
	return cloned
}

func sortWorkflowStates(states []*flowsk_project_pb.WorkflowState) {
	sort.SliceStable(states, func(i, j int) bool {
		if states[i].GetPosition() != states[j].GetPosition() {
			return states[i].GetPosition() < states[j].GetPosition()
		}
		return states[i].GetName() < states[j].GetName()
	})
}

func parseProjectStatus(op string, s flowsk_project_pb.ProjectStatus, allowUnspecified bool) (*project.Status, error) {
	switch s {
	case flowsk_project_pb.ProjectStatus_PROJECT_STATUS_UNSPECIFIED:
		if allowUnspecified {
			return nil, nil
		}
		return nil, errs.Validation(op, "project status is required", errs.FieldViolation{
			Field:       "status",
			Description: "status must not be PROJECT_STATUS_UNSPECIFIED",
		})
	case flowsk_project_pb.ProjectStatus_PROJECT_STATUS_PLANNED,
		flowsk_project_pb.ProjectStatus_PROJECT_STATUS_ACTIVE:
		st := project.StatusActive
		return &st, nil
	case flowsk_project_pb.ProjectStatus_PROJECT_STATUS_PAUSED:
		st := project.StatusPaused
		return &st, nil
	case flowsk_project_pb.ProjectStatus_PROJECT_STATUS_COMPLETED,
		flowsk_project_pb.ProjectStatus_PROJECT_STATUS_ARCHIVED:
		st := project.StatusArchived
		return &st, nil
	default:
		return nil, errs.Validation(op, "invalid project status", errs.FieldViolation{
			Field:       "status",
			Description: "unsupported ProjectStatus enum value",
		})
	}
}
