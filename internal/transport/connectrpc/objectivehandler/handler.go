// Package objectivehandler implements the Buf ConnectRPC `ObjectiveServiceHandler`
// transport adapter for project milestones, cycles, and measurable objectives.
package objectivehandler

import (
	"context"
	"strconv"
	"strings"
	"time"
	"unicode"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	flowsk_objective_pb "github.com/mindmain/flowsk/gen/go/flowsk/objective/v1"
	"github.com/mindmain/flowsk/gen/go/flowsk/objective/v1/flowsk_objective_pbconnect"
	"github.com/mindmain/flowsk/internal/adapter/db"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/objective"
	"github.com/mindmain/flowsk/internal/domain/tx"
	"github.com/mindmain/flowsk/internal/transport/connectrpc"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const maxObjectivePageSize int32 = 200

// DataDependencies defines the segregated persistence provider contracts consumed
// by `New` to extract concrete objective repository and transaction dependencies.
type DataDependencies interface {
	db.TxManagerProvider
	db.ObjectiveRepoProvider
}

// ServicesDependencies defines the segregated application service provider contract
// consumed by `New` to extract the objective domain service.
type ServicesDependencies interface {
	// ObjectiveService returns the resolved objective domain service.
	ObjectiveService() objective.Service
}

// Handler implements `flowsk_objective_pbconnect.ObjectiveServiceHandler` with
// pre-resolved domain dependencies and zero stored provider wrappers.
type Handler struct {
	txMgr tx.Manager
	repo  objective.Repository
	svc   objective.Service
}

// Compile-time assertion ensuring *Handler satisfies ObjectiveServiceHandler.
var _ flowsk_objective_pbconnect.ObjectiveServiceHandler = (*Handler)(nil)

// New resolves `DataDependencies` and `ServicesDependencies` immediately at startup
// and returns an immutable `ObjectiveService` ConnectRPC handler.
func New(dataDeps DataDependencies, svcDeps ServicesDependencies) *Handler {
	var (
		txMgr tx.Manager
		repo  objective.Repository
		svc   objective.Service
	)
	if dataDeps != nil {
		txMgr = dataDeps.TxManager()
		repo = dataDeps.ObjectiveRepository()
	}
	if svcDeps != nil {
		svc = svcDeps.ObjectiveService()
	}
	if svc == nil {
		svc = objective.NewService(repo, txMgr)
	}
	return &Handler{
		txMgr: txMgr,
		repo:  repo,
		svc:   svc,
	}
}

// Service returns the resolved `objective.Service` bound to this handler.
func (h *Handler) Service() objective.Service {
	if h == nil {
		return nil
	}
	return h.svc
}

// Repository returns the resolved `objective.Repository` bound to this handler.
func (h *Handler) Repository() objective.Repository {
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

// CreateObjective creates a new milestone, cycle, or strategic goal inside a project.
func (h *Handler) CreateObjective(
	ctx context.Context,
	req *connect.Request[flowsk_objective_pb.CreateObjectiveRequest],
) (*connect.Response[flowsk_objective_pb.CreateObjectiveResponse], error) {
	const op = "objectivehandler.CreateObjective"
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
	if _, err := connectrpc.ParseOptionalUUID(op, "owner_actor_id", req.Msg.OwnerActorId); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	domainKind, err := parseObjectiveKind(op, req.Msg.GetKind(), false)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	statusPtr, err := parseObjectiveStatus(op, req.Msg.GetStatus(), true)
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
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	initialStatus := objective.StatusDraft
	if statusPtr != nil {
		initialStatus = *statusPtr
	}
	var resolvedKind objective.Kind
	if domainKind != nil {
		resolvedKind = *domainKind
	}
	slug := strings.TrimSpace(req.Msg.GetSlug())
	if slug == "" {
		slug = slugifyTitle(title)
	} else {
		slug = slugifyTitle(slug)
	}
	priorityRank := int32(2)
	if req.Msg.PriorityRank != nil {
		priorityRank = req.Msg.GetPriorityRank()
	}

	created, err := h.svc.CreateObjective(ctx, objective.CreateObjectiveInput{
		WorkspaceID:     workspaceID,
		ProjectID:       projectID,
		Kind:            resolvedKind,
		Slug:            slug,
		Title:           title,
		Summary:         req.Msg.GetDescription(),
		SuccessCriteria: splitLines(req.Msg.GetSuccessCriteria()),
		Status:          initialStatus,
		PriorityRank:    priorityRank,
		TargetDate:      protoTimestampToTimePtr(req.Msg.GetTargetDate()),
	})
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	return connect.NewResponse(&flowsk_objective_pb.CreateObjectiveResponse{
		Objective: created.ToProto(),
	}), nil
}

// GetObjective retrieves an objective by UUID within the caller's authorized workspace.
func (h *Handler) GetObjective(
	ctx context.Context,
	req *connect.Request[flowsk_objective_pb.GetObjectiveRequest],
) (*connect.Response[flowsk_objective_pb.GetObjectiveResponse], error) {
	const op = "objectivehandler.GetObjective"
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
	objectiveID, err := connectrpc.ParseRequiredUUID(op, "objective_id", req.Msg.GetObjectiveId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	obj, err := h.svc.GetObjective(ctx, workspaceID, objectiveID)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_objective_pb.GetObjectiveResponse{
		Objective: obj.ToProto(),
	}), nil
}

// ListObjectives lists workspace or project objectives matching filter predicates.
func (h *Handler) ListObjectives(
	ctx context.Context,
	req *connect.Request[flowsk_objective_pb.ListObjectivesRequest],
) (*connect.Response[flowsk_objective_pb.ListObjectivesResponse], error) {
	const op = "objectivehandler.ListObjectives"
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
	kindPtr, err := parseObjectiveKind(op, req.Msg.GetKindFilter(), true)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	statusPtr, err := parseObjectiveStatus(op, req.Msg.GetStatusFilter(), true)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	limit, offset, err := connectrpc.ParsePagination(op, req.Msg.GetPageSize(), maxObjectivePageSize, req.Msg.GetPageToken())
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

	var (
		items         []objective.Objective
		nextPageToken string
	)
	if limit > 0 {
		items, err = h.svc.ListObjectives(ctx, objective.ListObjectivesFilter{
			WorkspaceID: workspaceID,
			ProjectID:   projectID,
			Kind:        kindPtr,
			Status:      statusPtr,
			Limit:       limit + 1,
			Offset:      offset,
		})
		if err != nil {
			return nil, connectrpc.MapDomainError(err)
		}
		if len(items) > int(limit) {
			items = items[:limit]
			nextPageToken = strconv.Itoa(int(offset + limit))
		}
	} else {
		allItems, listErr := h.svc.ListObjectives(ctx, objective.ListObjectivesFilter{
			WorkspaceID: workspaceID,
			ProjectID:   projectID,
			Kind:        kindPtr,
			Status:      statusPtr,
		})
		if listErr != nil {
			return nil, connectrpc.MapDomainError(listErr)
		}
		items, nextPageToken = connectrpc.PaginateSlice(allItems, limit, offset)
	}

	out := make([]*flowsk_objective_pb.Objective, 0, len(items))
	for i := range items {
		out = append(out, items[i].ToProto())
	}
	return connect.NewResponse(&flowsk_objective_pb.ListObjectivesResponse{
		Objectives:    out,
		NextPageToken: nextPageToken,
	}), nil
}

// UpdateObjective applies partial updates to an existing objective.
func (h *Handler) UpdateObjective(
	ctx context.Context,
	req *connect.Request[flowsk_objective_pb.UpdateObjectiveRequest],
) (*connect.Response[flowsk_objective_pb.UpdateObjectiveResponse], error) {
	const op = "objectivehandler.UpdateObjective"
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
	objectiveID, err := connectrpc.ParseRequiredUUID(op, "objective_id", req.Msg.GetObjectiveId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseOptionalUUID(op, "owner_actor_id", req.Msg.OwnerActorId); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	statusPtr, err := parseObjectiveStatus(op, req.Msg.GetStatus(), true)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	kindPtr, err := parseObjectiveKind(op, req.Msg.GetKind(), true)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	var criteria []string
	if req.Msg.SuccessCriteria != nil {
		criteria = splitLines(*req.Msg.SuccessCriteria)
		if criteria == nil {
			criteria = []string{}
		}
	}

	targetDate := protoTimestampToTimePtr(req.Msg.GetTargetDate())
	if req.Msg.GetClearTargetDate() {
		zero := time.Time{}
		targetDate = &zero
	}

	updated, err := h.svc.UpdateObjective(ctx, objective.UpdateObjectiveInput{
		WorkspaceID:     workspaceID,
		ObjectiveID:     objectiveID,
		Kind:            kindPtr,
		Title:           req.Msg.Title,
		Summary:         req.Msg.Description,
		SuccessCriteria: criteria,
		Status:          statusPtr,
		PriorityRank:    req.Msg.PriorityRank,
		TargetDate:      targetDate,
	})
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_objective_pb.UpdateObjectiveResponse{
		Objective: updated.ToProto(),
	}), nil
}

// CloseObjective transitions an objective into a terminal ACHIEVED or DROPPED state.
func (h *Handler) CloseObjective(
	ctx context.Context,
	req *connect.Request[flowsk_objective_pb.CloseObjectiveRequest],
) (*connect.Response[flowsk_objective_pb.CloseObjectiveResponse], error) {
	const op = "objectivehandler.CloseObjective"
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
	objectiveID, err := connectrpc.ParseRequiredUUID(op, "objective_id", req.Msg.GetObjectiveId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	var terminal objective.Status
	switch req.Msg.GetTerminalStatus() {
	case flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_ACHIEVED:
		terminal = objective.StatusAchieved
	case flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_DROPPED:
		terminal = objective.StatusDropped
	default:
		return nil, connectrpc.MapDomainError(errs.Validation(op, "invalid terminal_status", errs.FieldViolation{
			Field:       "terminal_status",
			Description: "terminal_status must be OBJECTIVE_STATUS_ACHIEVED or OBJECTIVE_STATUS_DROPPED",
		}))
	}

	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	closed, err := h.svc.UpdateObjective(ctx, objective.UpdateObjectiveInput{
		WorkspaceID: workspaceID,
		ObjectiveID: objectiveID,
		Status:      &terminal,
	})
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_objective_pb.CloseObjectiveResponse{
		Objective: closed.ToProto(),
	}), nil
}

// DeleteObjective soft-deletes an objective within the caller's authorized workspace.
func (h *Handler) DeleteObjective(
	ctx context.Context,
	req *connect.Request[flowsk_objective_pb.DeleteObjectiveRequest],
) (*connect.Response[flowsk_objective_pb.DeleteObjectiveResponse], error) {
	const op = "objectivehandler.DeleteObjective"
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
	objectiveID, err := connectrpc.ParseRequiredUUID(op, "objective_id", req.Msg.GetObjectiveId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	if err := h.svc.DeleteObjective(ctx, workspaceID, objectiveID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_objective_pb.DeleteObjectiveResponse{
		DeletedObjectiveId: objectiveID.String(),
		DeletedAt:          timestamppb.New(time.Now().UTC()),
	}), nil
}

func parseObjectiveKind(op string, k flowsk_objective_pb.ObjectiveKind, allowUnspecified bool) (*objective.Kind, error) {
	switch k {
	case flowsk_objective_pb.ObjectiveKind_OBJECTIVE_KIND_UNSPECIFIED:
		if allowUnspecified {
			return nil, nil
		}
		return nil, errs.Validation(op, "objective kind is required", errs.FieldViolation{
			Field:       "kind",
			Description: "kind must be MILESTONE, CYCLE, or GOAL",
		})
	case flowsk_objective_pb.ObjectiveKind_OBJECTIVE_KIND_MILESTONE:
		kd := objective.KindMilestone
		return &kd, nil
	case flowsk_objective_pb.ObjectiveKind_OBJECTIVE_KIND_CYCLE:
		kd := objective.KindCycle
		return &kd, nil
	case flowsk_objective_pb.ObjectiveKind_OBJECTIVE_KIND_GOAL:
		kd := objective.KindGoal
		return &kd, nil
	default:
		return nil, errs.Validation(op, "invalid objective kind", errs.FieldViolation{
			Field:       "kind",
			Description: "unsupported ObjectiveKind enum value",
		})
	}
}

func parseObjectiveStatus(op string, s flowsk_objective_pb.ObjectiveStatus, allowUnspecified bool) (*objective.Status, error) {
	switch s {
	case flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_UNSPECIFIED:
		if allowUnspecified {
			return nil, nil
		}
		return nil, errs.Validation(op, "objective status is required", errs.FieldViolation{
			Field:       "status",
			Description: "status must not be OBJECTIVE_STATUS_UNSPECIFIED",
		})
	case flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_PLANNED:
		st := objective.StatusDraft
		return &st, nil
	case flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_ACTIVE:
		st := objective.StatusActive
		return &st, nil
	case flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_AT_RISK:
		st := objective.StatusAtRisk
		return &st, nil
	case flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_ACHIEVED:
		st := objective.StatusAchieved
		return &st, nil
	case flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_DROPPED:
		st := objective.StatusDropped
		return &st, nil
	default:
		return nil, errs.Validation(op, "invalid objective status", errs.FieldViolation{
			Field:       "status",
			Description: "unsupported ObjectiveStatus enum value",
		})
	}
}

func slugifyTitle(title string) string {
	var b strings.Builder
	prevHyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevHyphen = false
		case unicode.IsSpace(r) || r == '-' || r == '_' || r == '.' || r == '/':
			if b.Len() > 0 && !prevHyphen {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
		if b.Len() >= 56 {
			break
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) < 2 {
		return "obj-" + uuid.NewString()[:8]
	}
	return slug
}

func splitLines(raw string) []string {
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

func protoTimestampToTimePtr(ts *timestamppb.Timestamp) *time.Time {
	if ts == nil || !ts.IsValid() {
		return nil
	}
	t := ts.AsTime().UTC()
	return &t
}
