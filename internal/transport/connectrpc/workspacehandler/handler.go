// Package workspacehandler implements the Buf ConnectRPC `WorkspaceServiceHandler`
// transport adapter for tenant workspaces and actor memberships.
package workspacehandler

import (
	"context"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	flowsk_workspace_pb "github.com/mindmain/flowsk/gen/go/flowsk/workspace/v1"
	"github.com/mindmain/flowsk/gen/go/flowsk/workspace/v1/flowsk_workspace_pbconnect"
	"github.com/mindmain/flowsk/internal/adapter/db"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/tx"
	"github.com/mindmain/flowsk/internal/domain/workspace"
	"github.com/mindmain/flowsk/internal/transport/connectrpc"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const maxWorkspacePageSize int32 = 200

// DataDependencies defines the segregated persistence provider contracts consumed
// by `New` to extract concrete workspace repository and transaction dependencies.
type DataDependencies interface {
	db.TxManagerProvider
	db.WorkspaceRepoProvider
}

// ServicesDependencies defines the segregated application service provider contract
// consumed by `New` to extract the workspace domain service.
type ServicesDependencies interface {
	// WorkspaceService returns the resolved workspace domain service.
	WorkspaceService() workspace.Service
}

// Handler implements `flowsk_workspace_pbconnect.WorkspaceServiceHandler` with
// pre-resolved domain dependencies and zero stored provider wrappers.
type Handler struct {
	txMgr tx.Manager
	repo  workspace.Repository
	svc   workspace.Service
}

// Compile-time assertion ensuring *Handler satisfies WorkspaceServiceHandler.
var _ flowsk_workspace_pbconnect.WorkspaceServiceHandler = (*Handler)(nil)

// New resolves `DataDependencies` and `ServicesDependencies` immediately at startup
// and returns an immutable `WorkspaceService` ConnectRPC handler.
// If `svcDeps` is nil or returns a nil service, a skeleton `workspace.Service` is
// wired from the resolved repository and transaction manager.
func New(dataDeps DataDependencies, svcDeps ServicesDependencies) *Handler {
	var (
		txMgr tx.Manager
		repo  workspace.Repository
		svc   workspace.Service
	)
	if dataDeps != nil {
		txMgr = dataDeps.TxManager()
		repo = dataDeps.WorkspaceRepository()
	}
	if svcDeps != nil {
		svc = svcDeps.WorkspaceService()
	}
	if svc == nil {
		svc = workspace.NewService(repo, txMgr)
	}
	return &Handler{
		txMgr: txMgr,
		repo:  repo,
		svc:   svc,
	}
}

// Service returns the resolved `workspace.Service` bound to this handler.
func (h *Handler) Service() workspace.Service {
	if h == nil {
		return nil
	}
	return h.svc
}

// Repository returns the resolved `workspace.Repository` bound to this handler.
func (h *Handler) Repository() workspace.Repository {
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

// CreateWorkspace provisions a new isolated workspace tenant via the domain service.
func (h *Handler) CreateWorkspace(
	ctx context.Context,
	req *connect.Request[flowsk_workspace_pb.CreateWorkspaceRequest],
) (*connect.Response[flowsk_workspace_pb.CreateWorkspaceResponse], error) {
	const op = "workspacehandler.CreateWorkspace"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}

	slug := strings.TrimSpace(req.Msg.GetSlug())
	name := strings.TrimSpace(req.Msg.GetName())
	if slug == "" || name == "" {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "slug and name are required",
			errs.FieldViolation{Field: "slug", Description: "slug must not be empty"},
			errs.FieldViolation{Field: "name", Description: "name must not be empty"},
		))
	}
	if err := connectrpc.RejectHostFilesystemPath(op, "description", req.Msg.GetDescription()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	// Support public bootstrap provisioning when CreateWorkspace is marked public
	// and caller credentials do not yet exist.
	if _, authErr := authctx.FromContext(ctx); authErr != nil {
		ctx = authctx.WithIdentity(ctx, authctx.Identity{
			WorkspaceID: uuid.New(),
			ActorID:     uuid.New(),
			Kind:        authctx.ActorKindHuman,
			Handle:      "bootstrap-owner",
		})
	}

	ws, err := h.svc.CreateWorkspace(ctx, workspace.CreateWorkspaceInput{
		Slug:        slug,
		Name:        name,
		Description: req.Msg.GetDescription(),
	})
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_workspace_pb.CreateWorkspaceResponse{
		Workspace: ws.ToProto(),
	}), nil
}

// GetWorkspace fetches a workspace by UUID after verifying workspace ownership.
func (h *Handler) GetWorkspace(
	ctx context.Context,
	req *connect.Request[flowsk_workspace_pb.GetWorkspaceRequest],
) (*connect.Response[flowsk_workspace_pb.GetWorkspaceResponse], error) {
	const op = "workspacehandler.GetWorkspace"
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
	if _, err := connectrpc.ParseOptionalUUID(op, "actor_id", req.Msg.ActorId); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	ws, err := h.svc.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_workspace_pb.GetWorkspaceResponse{
		Workspace: ws.ToProto(),
	}), nil
}

// ListWorkspaces lists accessible workspaces for the authenticated caller with cursor pagination.
func (h *Handler) ListWorkspaces(
	ctx context.Context,
	req *connect.Request[flowsk_workspace_pb.ListWorkspacesRequest],
) (*connect.Response[flowsk_workspace_pb.ListWorkspacesResponse], error) {
	const op = "workspacehandler.ListWorkspaces"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}

	limit, offset, err := connectrpc.ParsePagination(op, req.Msg.GetPageSize(), maxWorkspacePageSize, req.Msg.GetPageToken())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.FromContext(ctx); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	items, err := h.svc.ListWorkspaces(ctx)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	page, nextToken := connectrpc.PaginateSlice(items, limit, offset)
	out := make([]*flowsk_workspace_pb.Workspace, 0, len(page))
	for i := range page {
		out = append(out, page[i].ToProto())
	}
	return connect.NewResponse(&flowsk_workspace_pb.ListWorkspacesResponse{
		Workspaces:    out,
		NextPageToken: nextToken,
	}), nil
}

// UpdateWorkspace updates mutable workspace metadata after verifying ownership.
func (h *Handler) UpdateWorkspace(
	ctx context.Context,
	req *connect.Request[flowsk_workspace_pb.UpdateWorkspaceRequest],
) (*connect.Response[flowsk_workspace_pb.UpdateWorkspaceResponse], error) {
	const op = "workspacehandler.UpdateWorkspace"
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
	if req.Msg.Name != nil && strings.TrimSpace(*req.Msg.Name) == "" {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "invalid workspace name", errs.FieldViolation{
			Field:       "name",
			Description: "name must not be empty when provided",
		}))
	}
	if req.Msg.Description != nil {
		if err := connectrpc.RejectHostFilesystemPath(op, "description", *req.Msg.Description); err != nil {
			return nil, connectrpc.MapDomainError(err)
		}
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	ws, err := h.svc.UpdateWorkspace(ctx, workspace.UpdateWorkspaceInput{
		WorkspaceID: workspaceID,
		Name:        req.Msg.Name,
		Description: req.Msg.Description,
	})
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_workspace_pb.UpdateWorkspaceResponse{
		Workspace: ws.ToProto(),
	}), nil
}

// DeleteWorkspace soft-deletes a workspace after verifying tenant ownership.
func (h *Handler) DeleteWorkspace(
	ctx context.Context,
	req *connect.Request[flowsk_workspace_pb.DeleteWorkspaceRequest],
) (*connect.Response[flowsk_workspace_pb.DeleteWorkspaceResponse], error) {
	const op = "workspacehandler.DeleteWorkspace"
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

	if err := h.svc.ArchiveWorkspace(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_workspace_pb.DeleteWorkspaceResponse{
		DeletedWorkspaceId: workspaceID.String(),
		DeletedAt:          timestamppb.New(time.Now().UTC()),
	}), nil
}

// RegisterActor enrolls or updates a human or autonomous agent actor in a workspace.
func (h *Handler) RegisterActor(
	ctx context.Context,
	req *connect.Request[flowsk_workspace_pb.RegisterActorRequest],
) (*connect.Response[flowsk_workspace_pb.RegisterActorResponse], error) {
	const op = "workspacehandler.RegisterActor"
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
	initiatingActorID, err := connectrpc.ParseOptionalUUID(op, "actor_id", req.Msg.ActorId)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	kind, err := parseActorKind(op, req.Msg.GetKind(), false)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	handle := strings.TrimSpace(req.Msg.GetHandle())
	displayName := strings.TrimSpace(req.Msg.GetDisplayName())
	if handle == "" || displayName == "" {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "handle and display_name are required",
			errs.FieldViolation{Field: "handle", Description: "handle must not be empty"},
			errs.FieldViolation{Field: "display_name", Description: "display_name must not be empty"},
		))
	}

	// Support public bootstrap enrollment of the initial workspace actor when allowed
	// by PublicProcedures; otherwise enforce strict workspace ownership.
	if _, authErr := authctx.FromContext(ctx); authErr != nil {
		bootstrapActor := uuid.New()
		if initiatingActorID != nil {
			bootstrapActor = *initiatingActorID
		}
		ctx = authctx.WithIdentity(ctx, authctx.Identity{
			WorkspaceID: workspaceID,
			ActorID:     bootstrapActor,
			Kind:        kind,
			Handle:      handle,
		})
	} else if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	role := parseMemberRole(req.Msg.GetRoleTitle(), initiatingActorID == nil)
	member, err := h.svc.UpsertMember(ctx, workspace.UpsertMemberInput{
		WorkspaceID: workspaceID,
		Kind:        kind,
		Handle:      handle,
		DisplayName: displayName,
		Role:        role,
	})
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	protoActor := member.ToProto()
	if protoActor != nil && req.Msg.GetClientMetadata() != "" {
		protoActor.ClientMetadata = req.Msg.GetClientMetadata()
	}
	return connect.NewResponse(&flowsk_workspace_pb.RegisterActorResponse{
		Actor: protoActor,
	}), nil
}

// GetActor retrieves a single workspace actor by UUID.
func (h *Handler) GetActor(
	ctx context.Context,
	req *connect.Request[flowsk_workspace_pb.GetActorRequest],
) (*connect.Response[flowsk_workspace_pb.GetActorResponse], error) {
	const op = "workspacehandler.GetActor"
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
	targetActorID, err := connectrpc.ParseRequiredUUID(op, "target_actor_id", req.Msg.GetTargetActorId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseOptionalUUID(op, "actor_id", req.Msg.ActorId); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	members, err := h.svc.ListMembers(ctx, workspaceID)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	for i := range members {
		if members[i].ActorID == targetActorID {
			return connect.NewResponse(&flowsk_workspace_pb.GetActorResponse{
				Actor: members[i].ToProto(),
			}), nil
		}
	}
	return nil, connectrpc.MapDomainError(errs.New(errs.ErrNotFound, op, "actor not found in workspace"))
}

// ListActors lists registered human and agent actors in a workspace with optional kind filtering.
func (h *Handler) ListActors(
	ctx context.Context,
	req *connect.Request[flowsk_workspace_pb.ListActorsRequest],
) (*connect.Response[flowsk_workspace_pb.ListActorsResponse], error) {
	const op = "workspacehandler.ListActors"
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
	if _, err := connectrpc.ParseOptionalUUID(op, "actor_id", req.Msg.ActorId); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	kindFilter, err := parseActorKind(op, req.Msg.GetKindFilter(), true)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	limit, offset, err := connectrpc.ParsePagination(op, req.Msg.GetPageSize(), maxWorkspacePageSize, req.Msg.GetPageToken())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	members, err := h.svc.ListMembers(ctx, workspaceID)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	filtered := make([]workspace.WorkspaceMember, 0, len(members))
	for _, m := range members {
		if kindFilter != "" && m.Kind != kindFilter {
			continue
		}
		filtered = append(filtered, m)
	}

	page, nextToken := connectrpc.PaginateSlice(filtered, limit, offset)
	out := make([]*flowsk_workspace_pb.Actor, 0, len(page))
	for i := range page {
		out = append(out, page[i].ToProto())
	}
	return connect.NewResponse(&flowsk_workspace_pb.ListActorsResponse{
		Actors:        out,
		NextPageToken: nextToken,
	}), nil
}

func parseActorKind(op string, k flowsk_workspace_pb.ActorKind, allowUnspecified bool) (authctx.ActorKind, error) {
	switch k {
	case flowsk_workspace_pb.ActorKind_ACTOR_KIND_UNSPECIFIED:
		if allowUnspecified {
			return "", nil
		}
		return "", errs.Validation(op, "actor kind is required", errs.FieldViolation{
			Field:       "kind",
			Description: "kind must be ACTOR_KIND_HUMAN or ACTOR_KIND_AGENT",
		})
	case flowsk_workspace_pb.ActorKind_ACTOR_KIND_HUMAN:
		return authctx.ActorKindHuman, nil
	case flowsk_workspace_pb.ActorKind_ACTOR_KIND_AGENT:
		return authctx.ActorKindAgent, nil
	default:
		return "", errs.Validation(op, "invalid actor kind", errs.FieldViolation{
			Field:       "kind",
			Description: "unsupported ActorKind enum value",
		})
	}
}

func parseMemberRole(roleTitle string, isBootstrap bool) workspace.MemberRole {
	switch strings.ToLower(strings.TrimSpace(roleTitle)) {
	case string(workspace.MemberRoleOwner):
		return workspace.MemberRoleOwner
	case string(workspace.MemberRoleMaintainer):
		return workspace.MemberRoleMaintainer
	case string(workspace.MemberRoleReader):
		return workspace.MemberRoleReader
	case string(workspace.MemberRoleContributor):
		return workspace.MemberRoleContributor
	default:
		if isBootstrap {
			return workspace.MemberRoleOwner
		}
		return workspace.MemberRoleContributor
	}
}
