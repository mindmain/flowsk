// File remote.go implements the remote Buf Connect (`ConnectRPC`) client bridge
// activated transparently when `--server-url` / `FLOWSK_SERVER_URL` is set so
// multiple workstations, CI nodes, and remote agent servers collaborate over
// the network against a shared `flowsk serve` instance.
package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	flowsk_audit_pb "github.com/mindmain/flowsk/gen/go/flowsk/audit/v1"
	flowsk_language_pb "github.com/mindmain/flowsk/gen/go/flowsk/language/v1"
	flowsk_objective_pb "github.com/mindmain/flowsk/gen/go/flowsk/objective/v1"
	flowsk_project_pb "github.com/mindmain/flowsk/gen/go/flowsk/project/v1"
	flowsk_task_pb "github.com/mindmain/flowsk/gen/go/flowsk/task/v1"
	flowsk_workspace_pb "github.com/mindmain/flowsk/gen/go/flowsk/workspace/v1"
	"github.com/mindmain/flowsk/internal/adapter/client"
	"github.com/mindmain/flowsk/internal/cli"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/task"
	"github.com/mindmain/flowsk/internal/domain/tx"
	"github.com/mindmain/flowsk/internal/language"
	"github.com/mindmain/flowsk/internal/transport/connectrpc"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type remoteSession struct {
	prov        *client.Provider
	ctx         context.Context
	workspaceID uuid.UUID
	actorID     uuid.UUID
	handle      string
}

func (c *dualModeClient) openRemoteSession(ctx context.Context, opts cli.GlobalOptions, wsOverride, actorOverride string) (*remoteSession, error) {
	wsRef := firstNonEmpty(wsOverride, opts.Workspace, "default")
	handle := sanitizeActorHandle(firstNonEmpty(actorOverride, opts.Actor))
	kind := classifyActorKind("", handle)

	// Initial workspace & actor UUID guesses (deterministic for "default" so
	// every remote peer resolves the exact same default workspace UUID).
	wsID, parseErr := uuid.Parse(wsRef)
	if parseErr != nil || wsID == uuid.Nil {
		wsID = deterministicUUID("workspace", strings.ToLower(wsRef))
	}
	actorID := deterministicUUID("actor", wsID.String(), handle)

	prov, err := client.NewValidatedProvider(client.Config{
		BaseURL:     opts.ServerURL,
		AuthToken:   opts.AuthToken,
		WorkspaceID: wsRef,
		ActorID:     actorID.String(),
		ActorKind:   string(kind),
		ActorHandle: handle,
		Timeout:     30 * time.Second,
	})
	if err != nil {
		return nil, err
	}

	// Query server workspaces (server resolver resolves slug/default automatically)
	// so non-default custom workspace slugs resolve to their exact server UUID.
	if parseErr != nil && !strings.EqualFold(wsRef, "default") {
		if listResp, lErr := prov.WorkspaceClient().ListWorkspaces(ctx, connect.NewRequest(&flowsk_workspace_pb.ListWorkspacesRequest{PageSize: 200})); lErr == nil {
			for _, w := range listResp.Msg.GetWorkspaces() {
				if strings.EqualFold(w.GetSlug(), wsRef) {
					if parsed, pErr := uuid.Parse(w.GetId()); pErr == nil {
						wsID = parsed
						actorID = deterministicUUID("actor", wsID.String(), handle)
					}
					break
				}
			}
		}
	}

	// Ensure actor registration on the remote workspace to obtain canonical server Actor UUID.
	pbKind := flowsk_workspace_pb.ActorKind_ACTOR_KIND_HUMAN
	if kind == authctx.ActorKindAgent {
		pbKind = flowsk_workspace_pb.ActorKind_ACTOR_KIND_AGENT
	}
	authCtx := authctx.WithIdentity(ctx, authctx.Identity{
		WorkspaceID: wsID,
		ActorID:     actorID,
		Kind:        kind,
		Handle:      handle,
	})
	if regResp, rErr := prov.WorkspaceClient().RegisterActor(authCtx, connect.NewRequest(&flowsk_workspace_pb.RegisterActorRequest{
		WorkspaceId: wsID.String(),
		Kind:        pbKind,
		Handle:      handle,
		DisplayName: handle,
	})); rErr == nil && regResp.Msg.GetActor() != nil {
		if parsedActor, aErr := uuid.Parse(regResp.Msg.GetActor().GetId()); aErr == nil {
			actorID = parsedActor
		}
		if parsedWS, wErr := uuid.Parse(regResp.Msg.GetActor().GetWorkspaceId()); wErr == nil {
			wsID = parsedWS
		}
		authCtx = authctx.WithIdentity(ctx, authctx.Identity{
			WorkspaceID: wsID,
			ActorID:     actorID,
			Kind:        kind,
			Handle:      handle,
		})
	}

	return &remoteSession{
		prov:        prov,
		ctx:         authCtx,
		workspaceID: wsID,
		actorID:     actorID,
		handle:      handle,
	}, nil
}

func (s *remoteSession) resolveOrRegisterRemoteActor(actorRef string) (string, error) {
	trimmed := strings.TrimSpace(actorRef)
	if trimmed == "" {
		return "", nil
	}
	if _, err := uuid.Parse(trimmed); err == nil {
		return trimmed, nil
	}
	handle := sanitizeActorHandle(trimmed)
	pbKind := flowsk_workspace_pb.ActorKind_ACTOR_KIND_HUMAN
	if classifyActorKind("", handle) == authctx.ActorKindAgent {
		pbKind = flowsk_workspace_pb.ActorKind_ACTOR_KIND_AGENT
	}
	actorStr := s.actorID.String()
	resp, err := s.prov.WorkspaceClient().RegisterActor(s.ctx, connect.NewRequest(&flowsk_workspace_pb.RegisterActorRequest{
		WorkspaceId: s.workspaceID.String(),
		ActorId:     &actorStr,
		Kind:        pbKind,
		Handle:      handle,
		DisplayName: handle,
	}))
	if err != nil {
		return "", err
	}
	if resp.Msg.GetActor() != nil && strings.TrimSpace(resp.Msg.GetActor().GetId()) != "" {
		return resp.Msg.GetActor().GetId(), nil
	}
	return deterministicUUID("actor", s.workspaceID.String(), handle).String(), nil
}

func (s *remoteSession) resolveRemoteProject(projectRef string) (*flowsk_project_pb.Project, error) {
	ref := strings.TrimSpace(projectRef)
	if ref == "" {
		return nil, errs.Validation("app.resolveRemoteProject", "project is required")
	}
	if _, err := uuid.Parse(ref); err == nil {
		resp, gErr := s.prov.ProjectClient().GetProject(s.ctx, connect.NewRequest(&flowsk_project_pb.GetProjectRequest{
			WorkspaceId: s.workspaceID.String(),
			ActorId:     s.actorID.String(),
			ProjectId:   ref,
		}))
		if gErr == nil {
			return resp.Msg.GetProject(), nil
		}
	}
	if resp, sErr := s.prov.ProjectClient().GetProjectBySlug(s.ctx, connect.NewRequest(&flowsk_project_pb.GetProjectBySlugRequest{
		WorkspaceId: s.workspaceID.String(),
		ActorId:     s.actorID.String(),
		Slug:        strings.ToLower(ref),
	})); sErr == nil {
		return resp.Msg.GetProject(), nil
	}
	listResp, lErr := s.prov.ProjectClient().ListProjects(s.ctx, connect.NewRequest(&flowsk_project_pb.ListProjectsRequest{
		WorkspaceId:     s.workspaceID.String(),
		ActorId:         s.actorID.String(),
		IncludeArchived: true,
		PageSize:        200,
	}))
	if lErr != nil {
		return nil, lErr
	}
	for _, p := range listResp.Msg.GetProjects() {
		if strings.EqualFold(p.GetKeyPrefix(), ref) || strings.EqualFold(p.GetSlug(), ref) {
			return p, nil
		}
	}
	return nil, errs.New(errs.ErrNotFound, "app.resolveRemoteProject", fmt.Sprintf("remote project %q not found", ref))
}

func newRemoteTaskRequest[T any](msg *T, suppressSignal bool) *connect.Request[T] {
	req := connect.NewRequest(msg)
	if suppressSignal {
		req.Header().Set(connectrpc.HeaderSuppressSignal, "1")
	}
	return req
}

func (s *remoteSession) resolveRemoteTask(taskRef string) (*flowsk_task_pb.Task, []*flowsk_task_pb.TaskRelation, error) {
	return s.resolveRemoteTaskWithOpts("", taskRef, false)
}

func (s *remoteSession) resolveRemoteTaskQuiet(taskRef string) (*flowsk_task_pb.Task, []*flowsk_task_pb.TaskRelation, error) {
	return s.resolveRemoteTaskWithOpts("", taskRef, true)
}

func (s *remoteSession) resolveRemoteTaskInProject(projectRef, taskRef string) (*flowsk_task_pb.Task, []*flowsk_task_pb.TaskRelation, error) {
	return s.resolveRemoteTaskWithOpts(projectRef, taskRef, false)
}

func (s *remoteSession) resolveRemoteTaskInProjectQuiet(projectRef, taskRef string) (*flowsk_task_pb.Task, []*flowsk_task_pb.TaskRelation, error) {
	return s.resolveRemoteTaskWithOpts(projectRef, taskRef, true)
}

func (s *remoteSession) resolveRemoteTaskWithOpts(projectRef, taskRef string, suppressSignal bool) (*flowsk_task_pb.Task, []*flowsk_task_pb.TaskRelation, error) {
	ref := strings.TrimSpace(taskRef)
	if _, err := uuid.Parse(ref); err == nil {
		resp, gErr := s.prov.TaskClient().GetTask(s.ctx, newRemoteTaskRequest(&flowsk_task_pb.GetTaskRequest{
			WorkspaceId: s.workspaceID.String(),
			ActorId:     s.actorID.String(),
			TaskId:      ref,
		}, suppressSignal))
		if gErr != nil {
			return nil, nil, gErr
		}
		return resp.Msg.GetTask(), resp.Msg.GetRelations(), nil
	}
	if resp, err := s.prov.TaskClient().GetTaskByIdentifier(s.ctx, newRemoteTaskRequest(&flowsk_task_pb.GetTaskByIdentifierRequest{
		WorkspaceId: s.workspaceID.String(),
		ActorId:     s.actorID.String(),
		Identifier:  strings.ToUpper(ref),
	}, suppressSignal)); err == nil {
		return resp.Msg.GetTask(), resp.Msg.GetRelations(), nil
	} else if strings.TrimSpace(projectRef) != "" {
		if p, pErr := s.resolveRemoteProject(projectRef); pErr == nil {
			if extResp, extErr := s.prov.TaskClient().GetTaskByExternalId(s.ctx, newRemoteTaskRequest(&flowsk_task_pb.GetTaskByExternalIdRequest{
				WorkspaceId: s.workspaceID.String(),
				ActorId:     s.actorID.String(),
				ProjectId:   p.GetId(),
				ExternalId:  ref,
			}, suppressSignal)); extErr == nil {
				return extResp.Msg.GetTask(), extResp.Msg.GetRelations(), nil
			}
		}
		return nil, nil, err
	} else {
		return nil, nil, err
	}
}

func (c *dualModeClient) remoteCreateWorkspace(ctx context.Context, opts cli.GlobalOptions, in cli.CreateWorkspaceInput) (cli.WorkspaceView, error) {
	sess, err := c.openRemoteSession(ctx, opts, "default", firstNonEmpty(in.Actor, opts.Actor))
	if err != nil {
		return cli.WorkspaceView{}, err
	}
	resp, err := sess.prov.WorkspaceClient().CreateWorkspace(sess.ctx, connect.NewRequest(&flowsk_workspace_pb.CreateWorkspaceRequest{
		Slug:        strings.ToLower(strings.TrimSpace(in.Slug)),
		Name:        firstNonEmpty(in.Name, in.Slug),
		Description: in.Description,
	}))
	if err != nil {
		return cli.WorkspaceView{}, err
	}
	return protoWorkspaceToCLI(resp.Msg.GetWorkspace()), nil
}

func (c *dualModeClient) remoteListWorkspaces(ctx context.Context, opts cli.GlobalOptions, in cli.ListWorkspacesInput) (cli.WorkspaceListResult, error) {
	sess, err := c.openRemoteSession(ctx, opts, opts.Workspace, opts.Actor)
	if err != nil {
		return cli.WorkspaceListResult{}, err
	}
	resp, err := sess.prov.WorkspaceClient().ListWorkspaces(sess.ctx, connect.NewRequest(&flowsk_workspace_pb.ListWorkspacesRequest{
		PageSize:  int32(min(max(1, in.Limit), 200)),
		PageToken: in.Cursor,
	}))
	if err != nil {
		return cli.WorkspaceListResult{}, err
	}
	var items []cli.WorkspaceView
	for _, ws := range resp.Msg.GetWorkspaces() {
		items = append(items, protoWorkspaceToCLI(ws))
	}
	return cli.WorkspaceListResult{Items: items, NextCursor: resp.Msg.GetNextPageToken()}, nil
}

func (c *dualModeClient) remoteGetWorkspace(ctx context.Context, opts cli.GlobalOptions, workspaceRef string) (cli.WorkspaceView, error) {
	sess, err := c.openRemoteSession(ctx, opts, workspaceRef, opts.Actor)
	if err != nil {
		return cli.WorkspaceView{}, err
	}
	actorStr := sess.actorID.String()
	resp, err := sess.prov.WorkspaceClient().GetWorkspace(sess.ctx, connect.NewRequest(&flowsk_workspace_pb.GetWorkspaceRequest{
		WorkspaceId: sess.workspaceID.String(),
		ActorId:     &actorStr,
	}))
	if err != nil {
		return cli.WorkspaceView{}, err
	}
	return protoWorkspaceToCLI(resp.Msg.GetWorkspace()), nil
}

func (c *dualModeClient) remoteRegisterActor(ctx context.Context, opts cli.GlobalOptions, in cli.RegisterActorInput) (cli.ActorView, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, opts.Actor)
	if err != nil {
		return cli.ActorView{}, err
	}
	pbKind := flowsk_workspace_pb.ActorKind_ACTOR_KIND_HUMAN
	if classifyActorKind(in.Kind, in.Handle) == authctx.ActorKindAgent {
		pbKind = flowsk_workspace_pb.ActorKind_ACTOR_KIND_AGENT
	}
	actorStr := sess.actorID.String()
	resp, err := sess.prov.WorkspaceClient().RegisterActor(sess.ctx, connect.NewRequest(&flowsk_workspace_pb.RegisterActorRequest{
		WorkspaceId: sess.workspaceID.String(),
		ActorId:     &actorStr,
		Kind:        pbKind,
		Handle:      sanitizeActorHandle(in.Handle),
		DisplayName: firstNonEmpty(in.DisplayName, in.Handle),
		RoleTitle:   in.Role,
	}))
	if err != nil {
		return cli.ActorView{}, err
	}
	a := resp.Msg.GetActor()
	return protoActorToCLI(a), nil
}

func (c *dualModeClient) remoteListActors(ctx context.Context, opts cli.GlobalOptions, in cli.ListActorsInput) (cli.ActorListResult, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, opts.Actor)
	if err != nil {
		return cli.ActorListResult{}, err
	}
	actorStr := sess.actorID.String()
	resp, err := sess.prov.WorkspaceClient().ListActors(sess.ctx, connect.NewRequest(&flowsk_workspace_pb.ListActorsRequest{
		WorkspaceId: sess.workspaceID.String(),
		ActorId:     &actorStr,
		PageSize:    int32(min(max(1, in.Limit), 200)),
		PageToken:   in.Cursor,
	}))
	if err != nil {
		return cli.ActorListResult{}, err
	}
	var out []cli.ActorView
	for _, a := range resp.Msg.GetActors() {
		out = append(out, protoActorToCLI(a))
	}
	return cli.ActorListResult{Items: out, NextCursor: resp.Msg.GetNextPageToken()}, nil
}

func (c *dualModeClient) remoteCreateProject(ctx context.Context, opts cli.GlobalOptions, in cli.CreateProjectInput) (cli.ProjectView, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, firstNonEmpty(in.Actor, opts.Actor))
	if err != nil {
		return cli.ProjectView{}, err
	}
	keyPrefix := normalizeKeyPrefix(in.Key)
	resp, err := sess.prov.ProjectClient().CreateProject(sess.ctx, connect.NewRequest(&flowsk_project_pb.CreateProjectRequest{
		WorkspaceId:               sess.workspaceID.String(),
		ActorId:                   sess.actorID.String(),
		KeyPrefix:                 keyPrefix,
		Slug:                      slugifyIdentifier(in.Key),
		Name:                      firstNonEmpty(in.Name, keyPrefix),
		Description:               in.Description,
		Status:                    flowsk_project_pb.ProjectStatus_PROJECT_STATUS_ACTIVE,
		SeedDefaultWorkflowStates: in.DefaultWorkflow,
	}))
	if err != nil {
		return cli.ProjectView{}, err
	}
	return protoProjectToCLI(resp.Msg.GetProject()), nil
}

func (c *dualModeClient) remoteListProjects(ctx context.Context, opts cli.GlobalOptions, in cli.ListProjectsInput) (cli.ProjectListResult, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, opts.Actor)
	if err != nil {
		return cli.ProjectListResult{}, err
	}
	resp, err := sess.prov.ProjectClient().ListProjects(sess.ctx, connect.NewRequest(&flowsk_project_pb.ListProjectsRequest{
		WorkspaceId:     sess.workspaceID.String(),
		ActorId:         sess.actorID.String(),
		IncludeArchived: in.IncludeArchived,
		PageSize:        int32(min(max(1, in.Limit), 200)),
		PageToken:       in.Cursor,
	}))
	if err != nil {
		return cli.ProjectListResult{}, err
	}
	var out []cli.ProjectView
	for _, p := range resp.Msg.GetProjects() {
		out = append(out, protoProjectToCLI(p))
	}
	return cli.ProjectListResult{Items: out, NextCursor: resp.Msg.GetNextPageToken()}, nil
}

func (c *dualModeClient) remoteGetProject(ctx context.Context, opts cli.GlobalOptions, wsRef, projRef string) (cli.ProjectView, error) {
	sess, err := c.openRemoteSession(ctx, opts, wsRef, opts.Actor)
	if err != nil {
		return cli.ProjectView{}, err
	}
	p, err := sess.resolveRemoteProject(projRef)
	if err != nil {
		return cli.ProjectView{}, err
	}
	return protoProjectToCLI(p), nil
}

func (c *dualModeClient) remoteUpdateProject(ctx context.Context, opts cli.GlobalOptions, in cli.UpdateProjectInput) (cli.ProjectView, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, firstNonEmpty(in.Actor, opts.Actor))
	if err != nil {
		return cli.ProjectView{}, err
	}
	p, err := sess.resolveRemoteProject(in.Project)
	if err != nil {
		return cli.ProjectView{}, err
	}
	resp, err := sess.prov.ProjectClient().UpdateProject(sess.ctx, connect.NewRequest(&flowsk_project_pb.UpdateProjectRequest{
		WorkspaceId: sess.workspaceID.String(),
		ActorId:     sess.actorID.String(),
		ProjectId:   p.GetId(),
		Name:        in.Name,
		Description: in.Description,
	}))
	if err != nil {
		return cli.ProjectView{}, err
	}
	return protoProjectToCLI(resp.Msg.GetProject()), nil
}

func (c *dualModeClient) remoteArchiveProject(ctx context.Context, opts cli.GlobalOptions, in cli.ArchiveProjectInput) (cli.ProjectView, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, firstNonEmpty(in.Actor, opts.Actor))
	if err != nil {
		return cli.ProjectView{}, err
	}
	p, err := sess.resolveRemoteProject(in.Project)
	if err != nil {
		return cli.ProjectView{}, err
	}
	resp, err := sess.prov.ProjectClient().ArchiveProject(sess.ctx, connect.NewRequest(&flowsk_project_pb.ArchiveProjectRequest{
		WorkspaceId: sess.workspaceID.String(),
		ActorId:     sess.actorID.String(),
		ProjectId:   p.GetId(),
	}))
	if err != nil {
		return cli.ProjectView{}, err
	}
	return protoProjectToCLI(resp.Msg.GetProject()), nil
}

func (c *dualModeClient) remoteDeleteProject(ctx context.Context, opts cli.GlobalOptions, in cli.DeleteProjectInput) (cli.DeleteProjectResultView, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, firstNonEmpty(in.Actor, opts.Actor))
	if err != nil {
		return cli.DeleteProjectResultView{}, err
	}
	p, err := sess.resolveRemoteProject(in.Project)
	if err != nil {
		return cli.DeleteProjectResultView{}, err
	}
	resp, err := sess.prov.ProjectClient().DeleteProject(sess.ctx, connect.NewRequest(&flowsk_project_pb.DeleteProjectRequest{
		WorkspaceId: sess.workspaceID.String(),
		ActorId:     sess.actorID.String(),
		ProjectId:   p.GetId(),
	}))
	if err != nil {
		return cli.DeleteProjectResultView{}, err
	}
	return cli.DeleteProjectResultView{
		WorkspaceID:      sess.workspaceID.String(),
		DeletedProjectID: resp.Msg.GetDeletedProjectId(),
		DeletedKey:       p.GetKeyPrefix(),
	}, nil
}

func (c *dualModeClient) remoteBindSubproject(ctx context.Context, opts cli.GlobalOptions, in cli.BindSubprojectInput) (cli.ProjectBindingView, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, firstNonEmpty(in.Actor, opts.Actor))
	if err != nil {
		return cli.ProjectBindingView{}, err
	}
	parent, err := sess.resolveRemoteProject(in.ParentProject)
	if err != nil {
		return cli.ProjectBindingView{}, err
	}
	child, err := sess.resolveRemoteProject(in.ChildProject)
	if err != nil {
		return cli.ProjectBindingView{}, err
	}
	resp, err := sess.prov.ProjectClient().BindSubproject(sess.ctx, connect.NewRequest(&flowsk_project_pb.BindSubprojectRequest{
		WorkspaceId:     sess.workspaceID.String(),
		ActorId:         sess.actorID.String(),
		ParentProjectId: parent.GetId(),
		ChildProjectId:  child.GetId(),
		ScopeSubpath:    in.ScopeSubpath,
	}))
	if err != nil {
		return cli.ProjectBindingView{}, err
	}
	b := resp.Msg.GetBinding()
	return cli.ProjectBindingView{
		ID:              b.GetId(),
		WorkspaceID:     b.GetWorkspaceId(),
		ParentProjectID: b.GetParentProjectId(),
		ParentKey:       parent.GetKeyPrefix(),
		ChildProjectID:  b.GetChildProjectId(),
		ChildKey:        child.GetKeyPrefix(),
		ScopeSubpath:    b.GetScopeSubpath(),
		CreatedAt:       b.GetCreatedAt().AsTime(),
	}, nil
}

func (c *dualModeClient) remoteUnbindSubproject(ctx context.Context, opts cli.GlobalOptions, in cli.UnbindSubprojectInput) (cli.UnbindSubprojectResultView, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, firstNonEmpty(in.Actor, opts.Actor))
	if err != nil {
		return cli.UnbindSubprojectResultView{}, err
	}
	parent, err := sess.resolveRemoteProject(in.ParentProject)
	if err != nil {
		return cli.UnbindSubprojectResultView{}, err
	}
	child, err := sess.resolveRemoteProject(in.ChildProject)
	if err != nil {
		return cli.UnbindSubprojectResultView{}, err
	}
	resp, err := sess.prov.ProjectClient().UnbindSubproject(sess.ctx, connect.NewRequest(&flowsk_project_pb.UnbindSubprojectRequest{
		WorkspaceId:     sess.workspaceID.String(),
		ActorId:         sess.actorID.String(),
		ParentProjectId: parent.GetId(),
		ChildProjectId:  child.GetId(),
	}))
	if err != nil {
		return cli.UnbindSubprojectResultView{}, err
	}
	return cli.UnbindSubprojectResultView{
		ParentProject: parent.GetKeyPrefix(),
		ChildProject:  child.GetKeyPrefix(),
		Removed:       resp.Msg.GetUnbound(),
	}, nil
}

func (c *dualModeClient) remoteListProjectBindings(ctx context.Context, opts cli.GlobalOptions, in cli.ListProjectBindingsInput) ([]cli.ProjectBindingView, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, opts.Actor)
	if err != nil {
		return nil, err
	}
	req := &flowsk_project_pb.ListProjectBindingsRequest{
		WorkspaceId: sess.workspaceID.String(),
		ActorId:     sess.actorID.String(),
	}
	if strings.TrimSpace(in.ParentProject) != "" {
		p, pErr := sess.resolveRemoteProject(in.ParentProject)
		if pErr != nil {
			return nil, pErr
		}
		pid := p.GetId()
		req.ParentProjectId = &pid
	}
	if strings.TrimSpace(in.ChildProject) != "" {
		ch, cErr := sess.resolveRemoteProject(in.ChildProject)
		if cErr != nil {
			return nil, cErr
		}
		cid := ch.GetId()
		req.ChildProjectId = &cid
	}
	resp, err := sess.prov.ProjectClient().ListProjectBindings(sess.ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	projListResp, _ := sess.prov.ProjectClient().ListProjects(sess.ctx, connect.NewRequest(&flowsk_project_pb.ListProjectsRequest{
		WorkspaceId:     sess.workspaceID.String(),
		ActorId:         sess.actorID.String(),
		IncludeArchived: true,
		PageSize:        200,
	}))
	keyByID := make(map[string]string)
	if projListResp != nil {
		for _, p := range projListResp.Msg.GetProjects() {
			keyByID[p.GetId()] = p.GetKeyPrefix()
		}
	}
	var out []cli.ProjectBindingView
	for _, b := range resp.Msg.GetBindings() {
		out = append(out, cli.ProjectBindingView{
			ID:              b.GetId(),
			WorkspaceID:     b.GetWorkspaceId(),
			ParentProjectID: b.GetParentProjectId(),
			ParentKey:       keyByID[b.GetParentProjectId()],
			ChildProjectID:  b.GetChildProjectId(),
			ChildKey:        keyByID[b.GetChildProjectId()],
			ScopeSubpath:    b.GetScopeSubpath(),
			CreatedAt:       b.GetCreatedAt().AsTime(),
		})
	}
	return out, nil
}

func (c *dualModeClient) remoteListStates(ctx context.Context, opts cli.GlobalOptions, wsRef, projRef string) ([]cli.WorkflowStateView, error) {
	sess, err := c.openRemoteSession(ctx, opts, wsRef, opts.Actor)
	if err != nil {
		return nil, err
	}
	p, err := sess.resolveRemoteProject(projRef)
	if err != nil {
		return nil, err
	}
	resp, err := sess.prov.ProjectClient().ListWorkflowStates(sess.ctx, connect.NewRequest(&flowsk_project_pb.ListWorkflowStatesRequest{
		WorkspaceId: sess.workspaceID.String(),
		ActorId:     sess.actorID.String(),
		ProjectId:   p.GetId(),
	}))
	if err != nil {
		return nil, err
	}
	var out []cli.WorkflowStateView
	for _, st := range resp.Msg.GetWorkflowStates() {
		out = append(out, protoStateToCLI(st))
	}
	return out, nil
}

func (c *dualModeClient) remoteCreateState(ctx context.Context, opts cli.GlobalOptions, in cli.CreateStateInput) (cli.WorkflowStateView, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, firstNonEmpty(in.Actor, opts.Actor))
	if err != nil {
		return cli.WorkflowStateView{}, err
	}
	p, err := sess.resolveRemoteProject(in.Project)
	if err != nil {
		return cli.WorkflowStateView{}, err
	}
	resp, err := sess.prov.ProjectClient().CreateWorkflowState(sess.ctx, connect.NewRequest(&flowsk_project_pb.CreateWorkflowStateRequest{
		WorkspaceId: sess.workspaceID.String(),
		ActorId:     sess.actorID.String(),
		ProjectId:   p.GetId(),
		Name:        in.Name,
		Category:    parseProtoStateCategory(in.Category),
		Position:    int32(in.Position),
		ColorHex:    in.Color,
	}))
	if err != nil {
		return cli.WorkflowStateView{}, err
	}
	return protoStateToCLI(resp.Msg.GetWorkflowState()), nil
}

func (c *dualModeClient) remoteListLabels(ctx context.Context, opts cli.GlobalOptions, wsRef, projRef string) ([]cli.LabelView, error) {
	sess, err := c.openRemoteSession(ctx, opts, wsRef, opts.Actor)
	if err != nil {
		return nil, err
	}
	p, err := sess.resolveRemoteProject(projRef)
	if err != nil {
		return nil, err
	}
	resp, err := sess.prov.ProjectClient().ListLabels(sess.ctx, connect.NewRequest(&flowsk_project_pb.ListLabelsRequest{
		WorkspaceId: sess.workspaceID.String(),
		ActorId:     sess.actorID.String(),
		ProjectId:   p.GetId(),
	}))
	if err != nil {
		return nil, err
	}
	var out []cli.LabelView
	for _, l := range resp.Msg.GetLabels() {
		out = append(out, cli.LabelView{
			ID:          l.GetId(),
			ProjectID:   l.GetProjectId(),
			Name:        l.GetName(),
			Color:       l.GetColorHex(),
			Description: l.GetDescription(),
		})
	}
	return out, nil
}

func (c *dualModeClient) remoteCreateLabel(ctx context.Context, opts cli.GlobalOptions, in cli.CreateLabelInput) (cli.LabelView, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, firstNonEmpty(in.Actor, opts.Actor))
	if err != nil {
		return cli.LabelView{}, err
	}
	p, err := sess.resolveRemoteProject(in.Project)
	if err != nil {
		return cli.LabelView{}, err
	}
	resp, err := sess.prov.ProjectClient().CreateLabel(sess.ctx, connect.NewRequest(&flowsk_project_pb.CreateLabelRequest{
		WorkspaceId: sess.workspaceID.String(),
		ActorId:     sess.actorID.String(),
		ProjectId:   p.GetId(),
		Name:        in.Name,
		Description: in.Description,
		ColorHex:    in.Color,
	}))
	if err != nil {
		return cli.LabelView{}, err
	}
	l := resp.Msg.GetLabel()
	return cli.LabelView{
		ID:          l.GetId(),
		ProjectID:   l.GetProjectId(),
		Name:        l.GetName(),
		Color:       l.GetColorHex(),
		Description: l.GetDescription(),
	}, nil
}

func (c *dualModeClient) remoteCreateObjective(ctx context.Context, opts cli.GlobalOptions, in cli.CreateObjectiveInput) (cli.ObjectiveView, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, firstNonEmpty(in.Actor, opts.Actor))
	if err != nil {
		return cli.ObjectiveView{}, err
	}
	p, err := sess.resolveRemoteProject(in.Project)
	if err != nil {
		return cli.ObjectiveView{}, err
	}
	pbKind := flowsk_objective_pb.ObjectiveKind_OBJECTIVE_KIND_GOAL
	switch strings.ToLower(strings.TrimSpace(in.Kind)) {
	case "milestone":
		pbKind = flowsk_objective_pb.ObjectiveKind_OBJECTIVE_KIND_MILESTONE
	case "cycle":
		pbKind = flowsk_objective_pb.ObjectiveKind_OBJECTIVE_KIND_CYCLE
	}
	var td *timestamppb.Timestamp
	if strings.TrimSpace(in.TargetDate) != "" {
		if parsed, pErr := time.Parse("2006-01-02", strings.TrimSpace(in.TargetDate)); pErr == nil {
			td = timestamppb.New(parsed.UTC())
		}
	}
	prio := int32(max(0, in.Priority))
	resp, err := sess.prov.ObjectiveClient().CreateObjective(sess.ctx, connect.NewRequest(&flowsk_objective_pb.CreateObjectiveRequest{
		WorkspaceId:     sess.workspaceID.String(),
		ActorId:         sess.actorID.String(),
		ProjectId:       p.GetId(),
		Kind:            pbKind,
		Status:          flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_ACTIVE,
		Title:           in.Title,
		Slug:            strings.TrimSpace(in.Slug),
		Description:     in.Summary,
		SuccessCriteria: strings.Join(in.SuccessCriteria, "\n"),
		PriorityRank:    &prio,
		TargetDate:      td,
	}))
	if err != nil {
		return cli.ObjectiveView{}, err
	}
	return protoObjectiveToCLI(resp.Msg.GetObjective()), nil
}

func (c *dualModeClient) remoteListObjectives(ctx context.Context, opts cli.GlobalOptions, in cli.ListObjectivesInput) (cli.ObjectiveListResult, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, opts.Actor)
	if err != nil {
		return cli.ObjectiveListResult{}, err
	}
	var projIDPtr *string
	if strings.TrimSpace(in.Project) != "" {
		p, pErr := sess.resolveRemoteProject(in.Project)
		if pErr != nil {
			return cli.ObjectiveListResult{}, pErr
		}
		pid := p.GetId()
		projIDPtr = &pid
	}
	kindFilter := strings.ToLower(strings.TrimSpace(in.Kind))
	statusFilter := strings.ToLower(strings.TrimSpace(in.Status))
	resp, err := sess.prov.ObjectiveClient().ListObjectives(sess.ctx, connect.NewRequest(&flowsk_objective_pb.ListObjectivesRequest{
		WorkspaceId:  sess.workspaceID.String(),
		ActorId:      sess.actorID.String(),
		ProjectId:    projIDPtr,
		KindFilter:   mapCLIObjectiveKindToProto(kindFilter),
		StatusFilter: mapCLIObjectiveStatusToProto(statusFilter),
		PageSize:     int32(min(max(1, in.Limit), 200)),
		PageToken:    in.Cursor,
	}))
	if err != nil {
		return cli.ObjectiveListResult{}, err
	}
	var out []cli.ObjectiveView
	for _, o := range resp.Msg.GetObjectives() {
		ov := protoObjectiveToCLI(o)
		if kindFilter != "" && !strings.EqualFold(ov.Kind, kindFilter) {
			continue
		}
		if statusFilter != "" && !matchesRemoteObjectiveStatus(ov.Status, statusFilter) {
			continue
		}
		out = append(out, ov)
	}
	return cli.ObjectiveListResult{Items: out, NextCursor: resp.Msg.GetNextPageToken()}, nil
}

func (c *dualModeClient) remoteGetObjective(ctx context.Context, opts cli.GlobalOptions, wsRef, projRef, objRef string) (cli.ObjectiveView, error) {
	list, err := c.remoteListObjectives(ctx, opts, cli.ListObjectivesInput{Workspace: wsRef, Project: projRef, Limit: 200})
	if err != nil {
		return cli.ObjectiveView{}, err
	}
	for _, it := range list.Items {
		if strings.EqualFold(it.ID, objRef) || strings.EqualFold(it.Key, objRef) || strings.EqualFold(it.Title, objRef) {
			return it, nil
		}
	}
	return cli.ObjectiveView{}, errs.New(errs.ErrNotFound, "app.remoteGetObjective", fmt.Sprintf("objective %q not found", objRef))
}

func (c *dualModeClient) remoteUpdateObjective(ctx context.Context, opts cli.GlobalOptions, in cli.UpdateObjectiveInput) (cli.ObjectiveView, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, firstNonEmpty(in.Actor, opts.Actor))
	if err != nil {
		return cli.ObjectiveView{}, err
	}
	cur, err := c.remoteGetObjective(ctx, opts, in.Workspace, in.Project, in.Objective)
	if err != nil {
		return cli.ObjectiveView{}, err
	}
	req := &flowsk_objective_pb.UpdateObjectiveRequest{
		WorkspaceId: sess.workspaceID.String(),
		ActorId:     sess.actorID.String(),
		ObjectiveId: cur.ID,
		Title:       in.Title,
		Description: in.Summary,
	}
	if in.SuccessCriteria != nil {
		joined := strings.Join(*in.SuccessCriteria, "\n")
		req.SuccessCriteria = &joined
	}
	if in.Priority != nil {
		pr := int32(max(0, *in.Priority))
		req.PriorityRank = &pr
	}
	if in.Status != nil && strings.TrimSpace(*in.Status) != "" {
		req.Status = mapCLIObjectiveStatusToProto(*in.Status)
	}
	if in.TargetDate != nil {
		trimmedTD := strings.TrimSpace(*in.TargetDate)
		if trimmedTD == "" {
			req.ClearTargetDate = true
		} else {
			parsed, pErr := time.Parse("2006-01-02", trimmedTD)
			if pErr != nil {
				return cli.ObjectiveView{}, errs.Validation("app.remoteUpdateObjective", "invalid --target-date: expected YYYY-MM-DD")
			}
			req.TargetDate = timestamppb.New(parsed.UTC())
		}
	}
	resp, err := sess.prov.ObjectiveClient().UpdateObjective(sess.ctx, connect.NewRequest(req))
	if err != nil {
		return cli.ObjectiveView{}, err
	}
	return protoObjectiveToCLI(resp.Msg.GetObjective()), nil
}

func (c *dualModeClient) remoteCloseObjective(ctx context.Context, opts cli.GlobalOptions, in cli.CloseObjectiveInput) (cli.ObjectiveView, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, firstNonEmpty(in.Actor, opts.Actor))
	if err != nil {
		return cli.ObjectiveView{}, err
	}
	cur, err := c.remoteGetObjective(ctx, opts, in.Workspace, in.Project, in.Objective)
	if err != nil {
		return cli.ObjectiveView{}, err
	}
	term := flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_ACHIEVED
	if strings.EqualFold(in.Resolution, "canceled") || strings.EqualFold(in.Resolution, "superseded") || strings.EqualFold(in.Resolution, "dropped") {
		term = flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_DROPPED
	}
	resp, err := sess.prov.ObjectiveClient().CloseObjective(sess.ctx, connect.NewRequest(&flowsk_objective_pb.CloseObjectiveRequest{
		WorkspaceId:       sess.workspaceID.String(),
		ActorId:           sess.actorID.String(),
		ObjectiveId:       cur.ID,
		TerminalStatus:    term,
		ResolutionSummary: in.Note,
	}))
	if err != nil {
		return cli.ObjectiveView{}, err
	}
	return protoObjectiveToCLI(resp.Msg.GetObjective()), nil
}

func (c *dualModeClient) remoteCreateTask(ctx context.Context, opts cli.GlobalOptions, in cli.CreateTaskInput) (cli.TaskView, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, firstNonEmpty(in.Actor, opts.Actor))
	if err != nil {
		return cli.TaskView{}, err
	}
	p, err := sess.resolveRemoteProject(in.Project)
	if err != nil {
		return cli.TaskView{}, err
	}
	var objIDPtr *string
	if strings.TrimSpace(in.Objective) != "" {
		o, oErr := c.remoteGetObjective(ctx, opts, in.Workspace, in.Project, in.Objective)
		if oErr != nil {
			return cli.TaskView{}, oErr
		}
		objIDPtr = &o.ID
	}
	var parentIDPtr *string
	if strings.TrimSpace(in.Parent) != "" {
		pt, _, ptErr := sess.resolveRemoteTaskInProjectQuiet(in.Project, in.Parent)
		if ptErr != nil {
			return cli.TaskView{}, ptErr
		}
		pid := pt.GetId()
		parentIDPtr = &pid
	}

	var blockerIDs []string
	seenBlockers := make(map[string]bool, len(in.DependsOn))
	for _, dep := range in.DependsOn {
		if strings.TrimSpace(dep) == "" {
			continue
		}
		up, _, upErr := sess.resolveRemoteTaskInProjectQuiet(in.Project, dep)
		if upErr != nil {
			return cli.TaskView{}, upErr
		}
		if !seenBlockers[up.GetId()] {
			seenBlockers[up.GetId()] = true
			blockerIDs = append(blockerIDs, up.GetId())
		}
	}

	var assigneeIDPtr *string
	if strings.TrimSpace(in.Assignee) != "" {
		aid, aErr := sess.resolveOrRegisterRemoteActor(in.Assignee)
		if aErr != nil {
			return cli.TaskView{}, aErr
		}
		if aid != "" {
			assigneeIDPtr = &aid
		}
	}

	var stateIDPtr *string
	if strings.TrimSpace(in.State) != "" {
		projUUID, _ := uuid.Parse(p.GetId())
		stID := connectrpc.WorkflowStateIDForStatus(projUUID, parseCLIWorkflowStatus(in.State)).String()
		stateIDPtr = &stID
	}
	resp, err := sess.prov.TaskClient().CreateTask(sess.ctx, connect.NewRequest(&flowsk_task_pb.CreateTaskRequest{
		WorkspaceId:        sess.workspaceID.String(),
		ActorId:            sess.actorID.String(),
		ProjectId:          p.GetId(),
		Kind:               mapCLITaskKindToProto(in.Kind),
		Priority:           mapCLIPriorityToProto(in.Priority),
		WorkflowStateId:    stateIDPtr,
		Title:              in.Title,
		Description:        in.Description,
		AcceptanceCriteria: in.Acceptance,
		ObjectiveId:        objIDPtr,
		ParentTaskId:       parentIDPtr,
		AssigneeActorId:    assigneeIDPtr,
		EstimatePoints:     int32(max(0, in.Estimate)),
		ExternalId:         strings.TrimSpace(in.ExternalID),
		Tags:               in.Labels,
	}))
	if err != nil {
		return cli.TaskView{}, err
	}
	created := resp.Msg.GetTask()
	for _, blockerID := range blockerIDs {
		if _, relErr := sess.prov.TaskClient().AddTaskRelation(sess.ctx, connect.NewRequest(&flowsk_task_pb.AddTaskRelationRequest{
			WorkspaceId:  sess.workspaceID.String(),
			ActorId:      sess.actorID.String(),
			SourceTaskId: blockerID,
			TargetTaskId: created.GetId(),
			RelationType: flowsk_task_pb.TaskRelationType_TASK_RELATION_TYPE_BLOCKS,
		})); relErr != nil && connect.CodeOf(relErr) != connect.CodeAlreadyExists {
			return cli.TaskView{}, relErr
		}
	}
	reloaded, rels, rErr := sess.resolveRemoteTaskQuiet(created.GetId())
	if rErr != nil {
		return protoTaskToCLIWithRelations(sess, created, nil), nil
	}
	return protoTaskToCLIWithRelations(sess, reloaded, rels), nil
}

func (c *dualModeClient) remoteListTasks(ctx context.Context, opts cli.GlobalOptions, in cli.ListTasksInput) (cli.TaskListResult, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, opts.Actor)
	if err != nil {
		return cli.TaskListResult{}, err
	}
	var projIDPtr *string
	if strings.TrimSpace(in.Project) != "" {
		p, pErr := sess.resolveRemoteProject(in.Project)
		if pErr != nil {
			return cli.TaskListResult{}, pErr
		}
		pid := p.GetId()
		projIDPtr = &pid
	}
	parsedQ := language.ParseSearchQuery(in.Query)
	extFilter := firstNonEmpty(in.ExternalID, parsedQ.ExternalID)
	var extFilterPtr *string
	if extFilter != "" {
		extFilterPtr = &extFilter
	}
	reqTags := append([]string(nil), in.Labels...)
	reqTags = append(reqTags, parsedQ.Tags...)
	effActor := firstNonEmpty(in.Assignee, parsedQ.Actor)
	var effActorID string
	if effActor != "" {
		effActorID, _ = sess.resolveOrRegisterRemoteActor(effActor)
	}

	resp, err := sess.prov.TaskClient().ListTasks(sess.ctx, connect.NewRequest(&flowsk_task_pb.ListTasksRequest{
		WorkspaceId:      sess.workspaceID.String(),
		ActorId:          sess.actorID.String(),
		ProjectId:        projIDPtr,
		UnclaimedOnly:    in.Unclaimed,
		ExternalIdFilter: extFilterPtr,
		PageSize:         int32(min(max(100, in.Limit*2), 200)),
		PageToken:        in.Cursor,
	}))
	if err != nil {
		return cli.TaskListResult{}, err
	}
	var out []cli.TaskSummaryView
	for _, t := range resp.Msg.GetTasks() {
		sv := protoTaskToSummaryCLI(t)
		if extFilter != "" && sv.ExternalID != extFilter {
			continue
		}
		if !in.IncludeArchived && strings.TrimSpace(in.State) == "" && len(parsedQ.Statuses) == 0 && sv.Category == "archived" {
			continue
		}
		if strings.TrimSpace(in.State) != "" {
			if st, ok := language.NormalizeStatusToken(in.State); ok {
				if canonicalCategoryForStatus(st) != sv.Category && !strings.EqualFold(sv.State, in.State) {
					continue
				}
			}
		} else if len(parsedQ.Statuses) > 0 {
			matchedSt := false
			for _, st := range parsedQ.Statuses {
				if canonicalCategoryForStatus(st) == sv.Category {
					matchedSt = true
					break
				}
			}
			if !matchedSt {
				continue
			}
		}
		if in.Priority >= 0 && sv.Priority != in.Priority {
			continue
		}
		if parsedQ.Priority != nil && sv.Priority != mapDomainPriorityToCLI(*parsedQ.Priority) {
			continue
		}
		if len(reqTags) > 0 && !hasAllRemoteTags(sv.Labels, reqTags) {
			continue
		}
		if effActor != "" {
			matchedActor := strings.EqualFold(sv.Holder, effActor) ||
				(effActorID != "" && (strings.EqualFold(t.GetClaimedByActorId(), effActorID) || strings.EqualFold(t.GetAssigneeActorId(), effActorID)))
			if !matchedActor {
				continue
			}
		}
		if (in.Blocked || (parsedQ.Blocked != nil && *parsedQ.Blocked)) && !sv.Blocked {
			continue
		}
		if !parsedQ.MatchesText(sv.Key, sv.ExternalID, sv.Title, t.GetDescription(), strings.Join(sv.Labels, " "), sv.ObjectiveKey, sv.Holder) {
			continue
		}
		out = append(out, sv)
		if in.Limit > 0 && len(out) >= in.Limit {
			break
		}
	}
	return cli.TaskListResult{Items: out, NextCursor: resp.Msg.GetNextPageToken()}, nil
}

func (c *dualModeClient) remoteGetTask(ctx context.Context, opts cli.GlobalOptions, in cli.GetTaskInput) (cli.TaskView, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, opts.Actor)
	if err != nil {
		return cli.TaskView{}, err
	}
	t, rels, err := sess.resolveRemoteTaskInProject(in.Project, in.Task)
	if err != nil {
		return cli.TaskView{}, err
	}
	return protoTaskToCLIWithRelations(sess, t, rels), nil
}

func (c *dualModeClient) remoteUpdateTask(ctx context.Context, opts cli.GlobalOptions, in cli.UpdateTaskInput) (cli.TaskView, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, firstNonEmpty(in.Actor, opts.Actor))
	if err != nil {
		return cli.TaskView{}, err
	}
	t, _, err := sess.resolveRemoteTaskInProjectQuiet(in.Project, in.Task)
	if err != nil {
		return cli.TaskView{}, err
	}
	req := &flowsk_task_pb.UpdateTaskRequest{
		WorkspaceId:        sess.workspaceID.String(),
		ActorId:            sess.actorID.String(),
		TaskId:             t.GetId(),
		Title:              in.Title,
		Description:        in.Description,
		AcceptanceCriteria: in.Acceptance,
	}
	if in.ExternalID != nil {
		trimmed := strings.TrimSpace(*in.ExternalID)
		if trimmed == "" {
			req.ClearExternalId = true
		} else {
			req.ExternalId = &trimmed
		}
	}
	if in.Priority != nil {
		req.Priority = mapCLIPriorityToProto(*in.Priority)
	}
	if in.Objective != nil {
		trimmed := strings.TrimSpace(*in.Objective)
		if trimmed == "" {
			req.ClearObjective = true
		} else {
			o, oErr := c.remoteGetObjective(ctx, opts, in.Workspace, firstNonEmpty(in.Project, t.GetProjectId()), trimmed)
			if oErr != nil {
				return cli.TaskView{}, oErr
			}
			req.ObjectiveId = &o.ID
		}
	}
	if in.Parent != nil {
		trimmed := strings.TrimSpace(*in.Parent)
		if trimmed == "" {
			req.ClearParentTask = true
		} else {
			pt, _, ptErr := sess.resolveRemoteTaskInProjectQuiet(in.Project, trimmed)
			if ptErr != nil {
				return cli.TaskView{}, ptErr
			}
			pid := pt.GetId()
			req.ParentTaskId = &pid
		}
	}
	if in.Assignee != nil {
		trimmed := strings.TrimSpace(*in.Assignee)
		if trimmed == "" {
			req.ClearAssignee = true
		} else {
			aid, aErr := sess.resolveOrRegisterRemoteActor(trimmed)
			if aErr != nil {
				return cli.TaskView{}, aErr
			}
			req.AssigneeActorId = &aid
		}
	}
	if len(in.AddLabels) > 0 || len(in.RemoveLabels) > 0 {
		tagSet := make(map[string]bool)
		for _, tg := range t.GetTags() {
			tagSet[tg] = true
		}
		for _, tg := range in.AddLabels {
			tagSet[tg] = true
		}
		for _, tg := range in.RemoveLabels {
			delete(tagSet, tg)
		}
		merged := make([]string, 0, len(tagSet))
		for tg := range tagSet {
			merged = append(merged, tg)
		}
		sort.Strings(merged)
		req.Tags = merged
		req.ReplaceTags = true
	}
	if in.Estimate != nil {
		est := int32(*in.Estimate)
		req.EstimatePoints = &est
	}
	resp, err := sess.prov.TaskClient().UpdateTask(sess.ctx, connect.NewRequest(req))
	if err != nil {
		return cli.TaskView{}, err
	}
	reloaded, rels, rErr := sess.resolveRemoteTaskQuiet(resp.Msg.GetTask().GetId())
	if rErr != nil {
		return protoTaskToCLIWithRelations(sess, resp.Msg.GetTask(), nil), nil
	}
	return protoTaskToCLIWithRelations(sess, reloaded, rels), nil
}

func (c *dualModeClient) remoteMoveTask(ctx context.Context, opts cli.GlobalOptions, in cli.MoveTaskInput) (cli.TaskView, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, firstNonEmpty(in.Actor, opts.Actor))
	if err != nil {
		return cli.TaskView{}, err
	}
	t, _, err := sess.resolveRemoteTaskQuiet(in.Task)
	if err != nil {
		return cli.TaskView{}, err
	}
	projUUID, _ := uuid.Parse(t.GetProjectId())
	dest := parseCLIWorkflowStatus(in.State)
	stID := connectrpc.WorkflowStateIDForStatus(projUUID, dest).String()
	resp, err := sess.prov.TaskClient().TransitionTaskState(sess.ctx, connect.NewRequest(&flowsk_task_pb.TransitionTaskStateRequest{
		WorkspaceId:     sess.workspaceID.String(),
		ActorId:         sess.actorID.String(),
		TaskId:          t.GetId(),
		WorkflowStateId: stID,
		ResolutionNote:  in.Note,
		ReleaseClaim:    dest.IsTerminal(),
	}))
	if err != nil {
		return cli.TaskView{}, err
	}
	return protoTaskToCLIWithRelations(sess, resp.Msg.GetTask(), nil), nil
}

func (c *dualModeClient) remoteClaimTask(ctx context.Context, opts cli.GlobalOptions, in cli.ClaimTaskInput) (cli.TaskView, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, firstNonEmpty(in.Actor, opts.Actor))
	if err != nil {
		return cli.TaskView{}, err
	}
	t, _, err := sess.resolveRemoteTaskQuiet(in.Task)
	if err != nil {
		return cli.TaskView{}, err
	}
	resp, err := sess.prov.TaskClient().ClaimTask(sess.ctx, connect.NewRequest(&flowsk_task_pb.ClaimTaskRequest{
		WorkspaceId:     sess.workspaceID.String(),
		ActorId:         sess.actorID.String(),
		TaskId:          t.GetId(),
		LeaseTtlSeconds: int32(max(60, int(in.LeaseTTL.Seconds()))),
	}))
	if err != nil {
		return cli.TaskView{}, err
	}
	if strings.TrimSpace(in.Note) != "" {
		_, _ = sess.prov.TaskClient().AddTaskComment(sess.ctx, connect.NewRequest(&flowsk_task_pb.AddTaskCommentRequest{
			WorkspaceId: sess.workspaceID.String(),
			ActorId:     sess.actorID.String(),
			TaskId:      t.GetId(),
			Kind:        flowsk_task_pb.TaskCommentKind_TASK_COMMENT_KIND_COMMENT,
			Body:        in.Note,
		}))
	}
	reloaded, rels, rErr := sess.resolveRemoteTaskQuiet(t.GetId())
	if rErr != nil {
		return protoTaskToCLIWithRelations(sess, resp.Msg.GetTask(), nil), nil
	}
	return protoTaskToCLIWithRelations(sess, reloaded, rels), nil
}

func (c *dualModeClient) remoteReleaseTask(ctx context.Context, opts cli.GlobalOptions, in cli.ReleaseTaskInput) (cli.TaskView, error) {
	if strings.TrimSpace(in.TargetState) != "" {
		return c.remoteMoveTask(ctx, opts, cli.MoveTaskInput{
			Workspace: in.Workspace,
			Project:   in.Project,
			Task:      in.Task,
			State:     in.TargetState,
			Note:      in.Note,
			Actor:     in.Actor,
		})
	}
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, firstNonEmpty(in.Actor, opts.Actor))
	if err != nil {
		return cli.TaskView{}, err
	}
	t, _, err := sess.resolveRemoteTaskQuiet(in.Task)
	if err != nil {
		return cli.TaskView{}, err
	}
	forceRelease := t.GetClaimedByActorId() != "" && t.GetClaimedByActorId() != sess.actorID.String()
	resp, err := sess.prov.TaskClient().ReleaseTaskClaim(sess.ctx, connect.NewRequest(&flowsk_task_pb.ReleaseTaskClaimRequest{
		WorkspaceId: sess.workspaceID.String(),
		ActorId:     sess.actorID.String(),
		TaskId:      t.GetId(),
		HandoffNote: in.Note,
		Force:       forceRelease,
	}))
	if err != nil {
		return cli.TaskView{}, err
	}
	return protoTaskToCLIWithRelations(sess, resp.Msg.GetTask(), nil), nil
}

func (c *dualModeClient) remoteBlockTask(ctx context.Context, opts cli.GlobalOptions, in cli.BlockTaskInput) (cli.TaskView, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, firstNonEmpty(in.Actor, opts.Actor))
	if err != nil {
		return cli.TaskView{}, err
	}
	t, _, err := sess.resolveRemoteTaskInProjectQuiet(in.Project, in.Task)
	if err != nil {
		return cli.TaskView{}, err
	}
	var addedBlockers int
	for _, ref := range in.BlockedBy {
		if strings.TrimSpace(ref) == "" {
			continue
		}
		up, _, upErr := sess.resolveRemoteTaskInProjectQuiet(in.Project, ref)
		if upErr != nil {
			return cli.TaskView{}, upErr
		}
		if _, relErr := sess.prov.TaskClient().AddTaskRelation(sess.ctx, connect.NewRequest(&flowsk_task_pb.AddTaskRelationRequest{
			WorkspaceId:  sess.workspaceID.String(),
			ActorId:      sess.actorID.String(),
			SourceTaskId: up.GetId(),
			TargetTaskId: t.GetId(),
			RelationType: flowsk_task_pb.TaskRelationType_TASK_RELATION_TYPE_BLOCKS,
		})); relErr != nil && connect.CodeOf(relErr) != connect.CodeAlreadyExists {
			return cli.TaskView{}, relErr
		}
		addedBlockers++
	}
	if addedBlockers == 0 {
		projUUID, _ := uuid.Parse(t.GetProjectId())
		stID := connectrpc.WorkflowStateIDForStatus(projUUID, task.StatusBlocked).String()
		if _, trErr := sess.prov.TaskClient().TransitionTaskState(sess.ctx, connect.NewRequest(&flowsk_task_pb.TransitionTaskStateRequest{
			WorkspaceId:     sess.workspaceID.String(),
			ActorId:         sess.actorID.String(),
			TaskId:          t.GetId(),
			WorkflowStateId: stID,
			ResolutionNote:  in.Reason,
		})); trErr != nil {
			return cli.TaskView{}, trErr
		}
	} else if strings.TrimSpace(in.Reason) != "" {
		_, _ = sess.prov.TaskClient().AddTaskComment(sess.ctx, newRemoteTaskRequest(&flowsk_task_pb.AddTaskCommentRequest{
			WorkspaceId: sess.workspaceID.String(),
			ActorId:     sess.actorID.String(),
			TaskId:      t.GetId(),
			Kind:        flowsk_task_pb.TaskCommentKind_TASK_COMMENT_KIND_BLOCKER_REPORT,
			Body:        in.Reason,
		}, true))
	}
	reloaded, rels, err := sess.resolveRemoteTaskQuiet(t.GetId())
	if err != nil {
		return protoTaskToCLIWithRelations(sess, t, nil), nil
	}
	return protoTaskToCLIWithRelations(sess, reloaded, rels), nil
}

func (c *dualModeClient) remoteUnblockTask(ctx context.Context, opts cli.GlobalOptions, in cli.UnblockTaskInput) (cli.TaskView, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, firstNonEmpty(in.Actor, opts.Actor))
	if err != nil {
		return cli.TaskView{}, err
	}
	t, rels, err := sess.resolveRemoteTaskInProjectQuiet(in.Project, in.Task)
	if err != nil {
		return cli.TaskView{}, err
	}
	clearFilter := make(map[string]bool, len(in.ClearedBy))
	for _, ref := range in.ClearedBy {
		if strings.TrimSpace(ref) == "" {
			continue
		}
		up, _, upErr := sess.resolveRemoteTaskInProjectQuiet(in.Project, ref)
		if upErr != nil {
			return cli.TaskView{}, upErr
		}
		clearFilter[up.GetId()] = true
	}
	for _, rel := range rels {
		if rel.GetTargetTaskId() == t.GetId() && rel.GetRelationType() == flowsk_task_pb.TaskRelationType_TASK_RELATION_TYPE_BLOCKS {
			if len(clearFilter) == 0 || clearFilter[rel.GetSourceTaskId()] {
				if _, rmErr := sess.prov.TaskClient().RemoveTaskRelation(sess.ctx, connect.NewRequest(&flowsk_task_pb.RemoveTaskRelationRequest{
					WorkspaceId: sess.workspaceID.String(),
					ActorId:     sess.actorID.String(),
					RelationId:  rel.GetId(),
				})); rmErr != nil {
					return cli.TaskView{}, rmErr
				}
			}
		}
	}
	reloaded, remainingRels, err := sess.resolveRemoteTaskQuiet(t.GetId())
	if err != nil {
		return protoTaskToCLIWithRelations(sess, t, nil), nil
	}
	unresolvedBlockers := 0
	for _, rel := range remainingRels {
		if rel.GetTargetTaskId() == t.GetId() && rel.GetRelationType() == flowsk_task_pb.TaskRelationType_TASK_RELATION_TYPE_BLOCKS {
			if up, _, upErr := sess.resolveRemoteTaskQuiet(rel.GetSourceTaskId()); upErr == nil {
				if !isRemoteTaskTerminal(up) {
					unresolvedBlockers++
				}
			} else {
				unresolvedBlockers++
			}
		}
	}
	if unresolvedBlockers == 0 && (strings.EqualFold(reloaded.GetStatus(), string(task.StatusBlocked)) || protoTaskToCLI(reloaded).Category == "blocked") {
		projUUID, _ := uuid.Parse(reloaded.GetProjectId())
		stID := connectrpc.WorkflowStateIDForStatus(projUUID, task.StatusReady).String()
		if trResp, trErr := sess.prov.TaskClient().TransitionTaskState(sess.ctx, connect.NewRequest(&flowsk_task_pb.TransitionTaskStateRequest{
			WorkspaceId:     sess.workspaceID.String(),
			ActorId:         sess.actorID.String(),
			TaskId:          reloaded.GetId(),
			WorkflowStateId: stID,
			ResolutionNote:  in.Note,
		})); trErr == nil && trResp.Msg.GetTask() != nil {
			reloaded = trResp.Msg.GetTask()
		}
	}
	return protoTaskToCLIWithRelations(sess, reloaded, remainingRels), nil
}

func (c *dualModeClient) remoteAddComment(ctx context.Context, opts cli.GlobalOptions, in cli.AddCommentInput) (cli.CommentView, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, firstNonEmpty(in.Actor, opts.Actor))
	if err != nil {
		return cli.CommentView{}, err
	}
	t, _, err := sess.resolveRemoteTaskInProjectQuiet(in.Project, in.Task)
	if err != nil {
		return cli.CommentView{}, err
	}
	pbKind := mapCLICommentKindToProto(in.Kind)
	resp, err := sess.prov.TaskClient().AddTaskComment(sess.ctx, connect.NewRequest(&flowsk_task_pb.AddTaskCommentRequest{
		WorkspaceId: sess.workspaceID.String(),
		ActorId:     sess.actorID.String(),
		TaskId:      t.GetId(),
		Kind:        pbKind,
		Body:        in.Body,
	}))
	if err != nil {
		return cli.CommentView{}, err
	}
	cm := resp.Msg.GetComment()
	return cli.CommentView{
		ID:        cm.GetId(),
		TaskID:    cm.GetTaskId(),
		TaskKey:   t.GetIdentifier(),
		Author:    sess.handle,
		Kind:      protoCommentKindToCLI(cm.GetKind(), in.Kind),
		Body:      cm.GetBody(),
		CreatedAt: cm.GetCreatedAt().AsTime(),
	}, nil
}

func (c *dualModeClient) remoteListComments(ctx context.Context, opts cli.GlobalOptions, in cli.ListCommentsInput) (cli.CommentListResult, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, opts.Actor)
	if err != nil {
		return cli.CommentListResult{}, err
	}
	t, _, err := sess.resolveRemoteTaskInProjectQuiet(in.Project, in.Task)
	if err != nil {
		return cli.CommentListResult{}, err
	}
	resp, err := sess.prov.TaskClient().ListTaskComments(sess.ctx, connect.NewRequest(&flowsk_task_pb.ListTaskCommentsRequest{
		WorkspaceId: sess.workspaceID.String(),
		ActorId:     sess.actorID.String(),
		TaskId:      t.GetId(),
		PageSize:    int32(min(max(1, in.Limit), 200)),
		PageToken:   in.Cursor,
	}))
	if err != nil {
		return cli.CommentListResult{}, err
	}
	var out []cli.CommentView
	for _, cm := range resp.Msg.GetComments() {
		out = append(out, cli.CommentView{
			ID:        cm.GetId(),
			TaskID:    cm.GetTaskId(),
			TaskKey:   t.GetIdentifier(),
			Author:    cm.GetAuthorActorId(),
			Kind:      protoCommentKindToCLI(cm.GetKind(), ""),
			Body:      cm.GetBody(),
			CreatedAt: cm.GetCreatedAt().AsTime(),
		})
	}
	return cli.CommentListResult{Items: out, NextCursor: resp.Msg.GetNextPageToken()}, nil
}

func (c *dualModeClient) remoteNextTask(ctx context.Context, opts cli.GlobalOptions, in cli.NextTaskInput) (cli.NextTaskResult, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, firstNonEmpty(in.Actor, opts.Actor))
	if err != nil {
		return cli.NextTaskResult{}, err
	}
	p, err := sess.resolveRemoteProject(in.Project)
	if err != nil {
		return cli.NextTaskResult{}, err
	}
	pid := p.GetId()
	qResp, err := sess.prov.TaskClient().GetAgentWorkQueue(sess.ctx, connect.NewRequest(&flowsk_task_pb.GetAgentWorkQueueRequest{
		WorkspaceId: sess.workspaceID.String(),
		ActorId:     sess.actorID.String(),
		ProjectId:   &pid,
		MaxItems:    5,
	}))
	if err != nil {
		return cli.NextTaskResult{}, err
	}
	ready := qResp.Msg.GetReadyTasks()
	if len(ready) == 0 {
		return cli.NextTaskResult{}, nil
	}
	bestID := ready[0].GetId()
	if in.Claim {
		cResp, cErr := sess.prov.TaskClient().ClaimTask(sess.ctx, connect.NewRequest(&flowsk_task_pb.ClaimTaskRequest{
			WorkspaceId:     sess.workspaceID.String(),
			ActorId:         sess.actorID.String(),
			TaskId:          bestID,
			LeaseTtlSeconds: int32(max(60, int(in.LeaseTTL.Seconds()))),
		}))
		if cErr != nil {
			return cli.NextTaskResult{}, cErr
		}
		tv := protoTaskToCLI(cResp.Msg.GetTask())
		return cli.NextTaskResult{Task: &tv, Claimed: true}, nil
	}
	t, _, err := sess.resolveRemoteTask(bestID)
	if err != nil {
		return cli.NextTaskResult{}, err
	}
	tv := protoTaskToCLI(t)
	return cli.NextTaskResult{Task: &tv, Claimed: false}, nil
}

func (c *dualModeClient) remoteDigest(ctx context.Context, opts cli.GlobalOptions, in cli.TaskDigestInput) (cli.ProjectDigestView, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, opts.Actor)
	if err != nil {
		return cli.ProjectDigestView{}, err
	}
	p, err := sess.resolveRemoteProject(in.Project)
	if err != nil {
		return cli.ProjectDigestView{}, err
	}
	resp, err := sess.prov.TaskClient().GetProjectDigest(sess.ctx, connect.NewRequest(&flowsk_task_pb.GetProjectDigestRequest{
		WorkspaceId: sess.workspaceID.String(),
		ActorId:     sess.actorID.String(),
		ProjectId:   p.GetId(),
	}))
	if err != nil {
		return cli.ProjectDigestView{}, err
	}
	dig := resp.Msg.GetDigest()
	counts := make(map[string]int)
	for _, sc := range dig.GetCountsByStateCategory() {
		counts[sc.GetCategory().String()] = int(sc.GetCount())
	}
	var ready, blocked []cli.TaskSummaryView
	for _, r := range dig.GetUnclaimedHighPriorityTasks() {
		ready = append(ready, protoCompactSummaryToCLI(r))
	}
	if in.IncludeBlocked {
		for _, b := range dig.GetActiveBlockers() {
			blocked = append(blocked, protoCompactSummaryToCLI(b))
		}
	}
	return cli.ProjectDigestView{
		WorkspaceID:   sess.workspaceID.String(),
		ProjectID:     p.GetId(),
		ProjectKey:    p.GetKeyPrefix(),
		ProjectName:   p.GetName(),
		GeneratedAt:   dig.GetGeneratedAt().AsTime(),
		CountsByState: counts,
		ReadyQueue:    ready,
		BlockedTasks:  blocked,
	}, nil
}

func (c *dualModeClient) remoteExecuteScript(ctx context.Context, opts cli.GlobalOptions, in cli.ExecuteScriptInput) (cli.ExecuteScriptResult, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, firstNonEmpty(in.Actor, opts.Actor))
	if err != nil {
		return cli.ExecuteScriptResult{}, err
	}
	var defProjPtr *string
	if strings.TrimSpace(in.Project) != "" {
		if p, pErr := sess.resolveRemoteProject(in.Project); pErr == nil {
			pid := p.GetId()
			defProjPtr = &pid
		}
	}
	mode := flowsk_language_pb.ExecutionMode_EXECUTION_MODE_ATOMIC_COMMIT
	if in.DryRun {
		mode = flowsk_language_pb.ExecutionMode_EXECUTION_MODE_DRY_RUN
	}
	resp, err := sess.prov.LanguageClient().ExecuteScript(sess.ctx, connect.NewRequest(&flowsk_language_pb.ExecuteScriptRequest{
		WorkspaceId:      sess.workspaceID.String(),
		ActorId:          sess.actorID.String(),
		DefaultProjectId: defProjPtr,
		Script:           in.Script,
		Mode:             mode,
		OutputDensity:    flowsk_language_pb.OutputDensity_OUTPUT_DENSITY_MINIMAL_TOKENS,
	}))
	if err != nil {
		return cli.ExecuteScriptResult{}, err
	}
	return cli.ExecuteScriptResult{
		DryRun:           in.DryRun,
		Density:          in.Density,
		AppliedCount:     int(resp.Msg.GetStatementsExecuted()),
		FormattedSummary: resp.Msg.GetCompactOutput(),
	}, nil
}

func (c *dualModeClient) remoteListAuditEvents(ctx context.Context, opts cli.GlobalOptions, in cli.ListAuditEventsInput) (cli.AuditEventListResult, error) {
	sess, err := c.openRemoteSession(ctx, opts, in.Workspace, opts.Actor)
	if err != nil {
		return cli.AuditEventListResult{}, err
	}
	var sincePB *timestamppb.Timestamp
	if in.Since != nil {
		sincePB = timestamppb.New(in.Since.UTC())
	}
	var entityIDPtr *string
	if eid := strings.TrimSpace(in.EntityID); eid != "" {
		if _, parseErr := uuid.Parse(eid); parseErr == nil {
			entityIDPtr = &eid
		} else if t, _, tErr := sess.resolveRemoteTaskInProjectQuiet(in.Project, eid); tErr == nil && t.GetId() != "" {
			resolvedID := t.GetId()
			entityIDPtr = &resolvedID
		} else {
			entityIDPtr = &eid
		}
	}
	resp, err := sess.prov.AuditClient().ListAuditEvents(sess.ctx, connect.NewRequest(&flowsk_audit_pb.ListAuditEventsRequest{
		WorkspaceId:      sess.workspaceID.String(),
		ActorId:          sess.actorID.String(),
		EntityId:         entityIDPtr,
		EntityTypeFilter: in.EntityType,
		ActionFilter:     mapCLIAuditActionToProto(in.Action),
		Since:            sincePB,
		PageSize:         int32(min(max(1, in.Limit), 200)),
		PageToken:        in.Cursor,
	}))
	if err != nil {
		return cli.AuditEventListResult{}, err
	}
	var out []cli.AuditEventView
	for _, ev := range resp.Msg.GetEvents() {
		out = append(out, protoAuditToCLI(ev))
	}
	return cli.AuditEventListResult{Items: out, NextCursor: resp.Msg.GetNextPageToken()}, nil
}

func (c *dualModeClient) remoteGetAuditEvent(ctx context.Context, opts cli.GlobalOptions, wsRef, eventID string) (cli.AuditEventView, error) {
	sess, err := c.openRemoteSession(ctx, opts, wsRef, opts.Actor)
	if err != nil {
		return cli.AuditEventView{}, err
	}
	resp, err := sess.prov.AuditClient().GetAuditEvent(sess.ctx, connect.NewRequest(&flowsk_audit_pb.GetAuditEventRequest{
		WorkspaceId:  sess.workspaceID.String(),
		ActorId:      sess.actorID.String(),
		AuditEventId: eventID,
	}))
	if err != nil {
		return cli.AuditEventView{}, err
	}
	return protoAuditToCLI(resp.Msg.GetEvent()), nil
}

func (c *dualModeClient) remoteGetProjectMarkdown(ctx context.Context, opts cli.GlobalOptions, wsRef, projRef string) (cli.MarkdownDocView, error) {
	sess, err := c.openRemoteSession(ctx, opts, wsRef, opts.Actor)
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	p, err := sess.resolveRemoteProject(projRef)
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	resp, err := sess.prov.ProjectClient().GetProjectMarkdown(sess.ctx, connect.NewRequest(&flowsk_project_pb.GetProjectMarkdownRequest{
		WorkspaceId: sess.workspaceID.String(),
		ActorId:     sess.actorID.String(),
		ProjectId:   p.GetId(),
	}))
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	return cli.MarkdownDocView{
		EntityKind:     "project",
		EntityKey:      p.GetKeyPrefix(),
		EntityID:       p.GetId(),
		WorkspaceID:    sess.workspaceID.String(),
		HasMarkdownDoc: resp.Msg.GetHasMarkdownDoc(),
		Markdown:       resp.Msg.GetMarkdown(),
		Action:         "get",
	}, nil
}

func (c *dualModeClient) remotePutProjectMarkdown(ctx context.Context, opts cli.GlobalOptions, wsRef, projRef, actor, markdown string) (cli.MarkdownDocView, error) {
	sess, err := c.openRemoteSession(ctx, opts, wsRef, firstNonEmpty(actor, opts.Actor))
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	p, err := sess.resolveRemoteProject(projRef)
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	resp, err := sess.prov.ProjectClient().PutProjectMarkdown(sess.ctx, connect.NewRequest(&flowsk_project_pb.PutProjectMarkdownRequest{
		WorkspaceId: sess.workspaceID.String(),
		ActorId:     sess.actorID.String(),
		ProjectId:   p.GetId(),
		Markdown:    markdown,
	}))
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	upd := resp.Msg.GetProject()
	return cli.MarkdownDocView{
		EntityKind:     "project",
		EntityKey:      p.GetKeyPrefix(),
		EntityID:       p.GetId(),
		WorkspaceID:    sess.workspaceID.String(),
		HasMarkdownDoc: upd != nil && upd.GetHasMarkdownDoc(),
		Markdown:       markdown,
		Action:         "put",
	}, nil
}

func (c *dualModeClient) remoteDeleteProjectMarkdown(ctx context.Context, opts cli.GlobalOptions, wsRef, projRef, actor string) (cli.MarkdownDocView, error) {
	sess, err := c.openRemoteSession(ctx, opts, wsRef, firstNonEmpty(actor, opts.Actor))
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	p, err := sess.resolveRemoteProject(projRef)
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	if _, err := sess.prov.ProjectClient().DeleteProjectMarkdown(sess.ctx, connect.NewRequest(&flowsk_project_pb.DeleteProjectMarkdownRequest{
		WorkspaceId: sess.workspaceID.String(),
		ActorId:     sess.actorID.String(),
		ProjectId:   p.GetId(),
	})); err != nil {
		return cli.MarkdownDocView{}, err
	}
	return cli.MarkdownDocView{
		EntityKind:     "project",
		EntityKey:      p.GetKeyPrefix(),
		EntityID:       p.GetId(),
		WorkspaceID:    sess.workspaceID.String(),
		HasMarkdownDoc: false,
		Action:         "delete",
	}, nil
}

func (c *dualModeClient) remoteGetTaskMarkdown(ctx context.Context, opts cli.GlobalOptions, wsRef, taskRef string) (cli.MarkdownDocView, error) {
	sess, err := c.openRemoteSession(ctx, opts, wsRef, opts.Actor)
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	t, _, err := sess.resolveRemoteTaskQuiet(taskRef)
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	resp, err := sess.prov.TaskClient().GetTaskMarkdown(sess.ctx, connect.NewRequest(&flowsk_task_pb.GetTaskMarkdownRequest{
		WorkspaceId: sess.workspaceID.String(),
		ActorId:     sess.actorID.String(),
		TaskId:      t.GetId(),
	}))
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	return cli.MarkdownDocView{
		EntityKind:     "task",
		EntityKey:      t.GetIdentifier(),
		EntityID:       t.GetId(),
		WorkspaceID:    sess.workspaceID.String(),
		HasMarkdownDoc: resp.Msg.GetHasMarkdownDoc(),
		Markdown:       resp.Msg.GetMarkdown(),
		Action:         "get",
	}, nil
}

func (c *dualModeClient) remotePutTaskMarkdown(ctx context.Context, opts cli.GlobalOptions, wsRef, taskRef, actor, markdown string) (cli.MarkdownDocView, error) {
	sess, err := c.openRemoteSession(ctx, opts, wsRef, firstNonEmpty(actor, opts.Actor))
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	t, _, err := sess.resolveRemoteTaskQuiet(taskRef)
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	resp, err := sess.prov.TaskClient().PutTaskMarkdown(sess.ctx, connect.NewRequest(&flowsk_task_pb.PutTaskMarkdownRequest{
		WorkspaceId: sess.workspaceID.String(),
		ActorId:     sess.actorID.String(),
		TaskId:      t.GetId(),
		Markdown:    markdown,
	}))
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	upd := resp.Msg.GetTask()
	return cli.MarkdownDocView{
		EntityKind:     "task",
		EntityKey:      t.GetIdentifier(),
		EntityID:       t.GetId(),
		WorkspaceID:    sess.workspaceID.String(),
		HasMarkdownDoc: upd != nil && upd.GetHasMarkdownDoc(),
		Markdown:       markdown,
		Action:         "put",
	}, nil
}

func (c *dualModeClient) remoteDeleteTaskMarkdown(ctx context.Context, opts cli.GlobalOptions, wsRef, taskRef, actor string) (cli.MarkdownDocView, error) {
	sess, err := c.openRemoteSession(ctx, opts, wsRef, firstNonEmpty(actor, opts.Actor))
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	t, _, err := sess.resolveRemoteTaskQuiet(taskRef)
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	if _, err := sess.prov.TaskClient().DeleteTaskMarkdown(sess.ctx, connect.NewRequest(&flowsk_task_pb.DeleteTaskMarkdownRequest{
		WorkspaceId: sess.workspaceID.String(),
		ActorId:     sess.actorID.String(),
		TaskId:      t.GetId(),
	})); err != nil {
		return cli.MarkdownDocView{}, err
	}
	return cli.MarkdownDocView{
		EntityKind:     "task",
		EntityKey:      t.GetIdentifier(),
		EntityID:       t.GetId(),
		WorkspaceID:    sess.workspaceID.String(),
		HasMarkdownDoc: false,
		Action:         "delete",
	}, nil
}

func (c *dualModeClient) remotePurgeArchivedTasks(ctx context.Context, opts cli.GlobalOptions, wsRef, actor string) (cli.PurgeArchivedView, error) {
	sess, err := c.openRemoteSession(ctx, opts, wsRef, firstNonEmpty(actor, opts.Actor))
	if err != nil {
		return cli.PurgeArchivedView{}, err
	}
	resp, err := sess.prov.TaskClient().PurgeArchivedTasks(sess.ctx, connect.NewRequest(&flowsk_task_pb.PurgeArchivedTasksRequest{
		WorkspaceId: sess.workspaceID.String(),
		ActorId:     sess.actorID.String(),
	}))
	if err != nil {
		return cli.PurgeArchivedView{}, err
	}
	return cli.PurgeArchivedView{
		WorkspaceID:          sess.workspaceID.String(),
		DeletedTaskCount:     int(resp.Msg.GetDeletedTaskCount()),
		DeletedMarkdownCount: int(resp.Msg.GetDeletedMarkdownCount()),
		DeletedTaskKeys:      append([]string(nil), resp.Msg.GetDeletedTaskKeys()...),
	}, nil
}

func (c *dualModeClient) remoteInspectOrphans(ctx context.Context, opts cli.GlobalOptions, wsRef string) (cli.OrphanReportView, error) {
	sess, err := c.openRemoteSession(ctx, opts, wsRef, opts.Actor)
	if err != nil {
		return cli.OrphanReportView{}, err
	}
	resp, err := sess.prov.TaskClient().InspectOrphanResources(sess.ctx, connect.NewRequest(&flowsk_task_pb.InspectOrphanResourcesRequest{
		WorkspaceId: sess.workspaceID.String(),
		ActorId:     sess.actorID.String(),
	}))
	if err != nil {
		return cli.OrphanReportView{}, err
	}
	return cli.OrphanReportView{
		WorkspaceID:              sess.workspaceID.String(),
		OrphanTaskFilesCount:     int(resp.Msg.GetOrphanTaskFilesCount()),
		MissingTaskFilesCount:    int(resp.Msg.GetMissingTaskFilesCount()),
		OrphanProjectFilesCount:  int(resp.Msg.GetOrphanProjectFilesCount()),
		MissingProjectFilesCount: int(resp.Msg.GetMissingProjectFilesCount()),
		TotalIssues:              int(resp.Msg.GetTotalIssues()),
		IssueSummaries:           append([]string(nil), resp.Msg.GetIssueSummaries()...),
	}, nil
}

func (c *dualModeClient) remotePruneOrphans(ctx context.Context, opts cli.GlobalOptions, wsRef, actor string, dryRun bool) (cli.PruneResultView, error) {
	sess, err := c.openRemoteSession(ctx, opts, wsRef, firstNonEmpty(actor, opts.Actor))
	if err != nil {
		return cli.PruneResultView{}, err
	}
	resp, err := sess.prov.TaskClient().PruneOrphanResources(sess.ctx, connect.NewRequest(&flowsk_task_pb.PruneOrphanResourcesRequest{
		WorkspaceId: sess.workspaceID.String(),
		ActorId:     sess.actorID.String(),
		DryRun:      dryRun,
	}))
	if err != nil {
		return cli.PruneResultView{}, err
	}
	return cli.PruneResultView{
		WorkspaceID:              sess.workspaceID.String(),
		DryRun:                   resp.Msg.GetDryRun(),
		RemovedTaskFilesCount:    int(resp.Msg.GetRemovedTaskFilesCount()),
		ClearedTaskRefsCount:     int(resp.Msg.GetClearedTaskRefsCount()),
		RemovedProjectFilesCount: int(resp.Msg.GetRemovedProjectFilesCount()),
		ClearedProjectRefsCount:  int(resp.Msg.GetClearedProjectRefsCount()),
		TotalCleaned:             int(resp.Msg.GetTotalCleaned()),
		CleanedSummaries:         append([]string(nil), resp.Msg.GetCleanedSummaries()...),
	}, nil
}

// =============================================================================
// Protobuf -> CLI View Converters
// =============================================================================

func protoWorkspaceToCLI(ws *flowsk_workspace_pb.Workspace) cli.WorkspaceView {
	if ws == nil {
		return cli.WorkspaceView{}
	}
	return cli.WorkspaceView{
		ID:          ws.GetId(),
		Slug:        ws.GetSlug(),
		Name:        ws.GetName(),
		Description: ws.GetDescription(),
		CreatedAt:   ws.GetCreatedAt().AsTime(),
		UpdatedAt:   ws.GetUpdatedAt().AsTime(),
	}
}

func protoActorToCLI(a *flowsk_workspace_pb.Actor) cli.ActorView {
	if a == nil {
		return cli.ActorView{}
	}
	kind := "human"
	if a.GetKind() == flowsk_workspace_pb.ActorKind_ACTOR_KIND_AGENT {
		kind = "agent"
	}
	return cli.ActorView{
		ID:          a.GetId(),
		WorkspaceID: a.GetWorkspaceId(),
		Handle:      a.GetHandle(),
		DisplayName: a.GetDisplayName(),
		Kind:        kind,
		Role:        a.GetRoleTitle(),
		CreatedAt:   a.GetCreatedAt().AsTime(),
	}
}

func protoProjectToCLI(p *flowsk_project_pb.Project) cli.ProjectView {
	if p == nil {
		return cli.ProjectView{}
	}
	status := "active"
	switch p.GetStatus() {
	case flowsk_project_pb.ProjectStatus_PROJECT_STATUS_PAUSED:
		status = "paused"
	case flowsk_project_pb.ProjectStatus_PROJECT_STATUS_ARCHIVED:
		status = "archived"
	}
	return cli.ProjectView{
		ID:             p.GetId(),
		WorkspaceID:    p.GetWorkspaceId(),
		Slug:           p.GetSlug(),
		Key:            p.GetKeyPrefix(),
		Name:           p.GetName(),
		Description:    p.GetDescription(),
		Status:         status,
		HasMarkdownDoc: p.GetHasMarkdownDoc(),
		CreatedAt:      p.GetCreatedAt().AsTime(),
		UpdatedAt:      p.GetUpdatedAt().AsTime(),
	}
}

func protoStateToCLI(st *flowsk_project_pb.WorkflowState) cli.WorkflowStateView {
	if st == nil {
		return cli.WorkflowStateView{}
	}
	return cli.WorkflowStateView{
		ID:        st.GetId(),
		ProjectID: st.GetProjectId(),
		Name:      st.GetName(),
		Category:  protoCategoryToCLICategory(st.GetCategory()),
		Position:  int(st.GetPosition()),
		Color:     st.GetColorHex(),
	}
}

func protoObjectiveToCLI(o *flowsk_objective_pb.Objective) cli.ObjectiveView {
	if o == nil {
		return cli.ObjectiveView{}
	}
	kind := "goal"
	switch o.GetKind() {
	case flowsk_objective_pb.ObjectiveKind_OBJECTIVE_KIND_MILESTONE:
		kind = "milestone"
	case flowsk_objective_pb.ObjectiveKind_OBJECTIVE_KIND_CYCLE:
		kind = "cycle"
	}
	status := "active"
	switch o.GetStatus() {
	case flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_PLANNED:
		status = "planned"
	case flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_AT_RISK:
		status = "at_risk"
	case flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_ACHIEVED:
		status = "achieved"
	case flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_DROPPED:
		status = "dropped"
	}
	td := ""
	if o.GetTargetDate() != nil {
		td = o.GetTargetDate().AsTime().UTC().Format("2006-01-02")
	}
	var criteria []string
	if strings.TrimSpace(o.GetSuccessCriteria()) != "" {
		for _, line := range strings.Split(o.GetSuccessCriteria(), "\n") {
			if trimmed := strings.TrimSpace(line); trimmed != "" {
				criteria = append(criteria, trimmed)
			}
		}
	}
	return cli.ObjectiveView{
		ID:              o.GetId(),
		WorkspaceID:     o.GetWorkspaceId(),
		ProjectID:       o.GetProjectId(),
		Key:             firstNonEmpty(o.GetSlug(), slugifyIdentifier(o.GetTitle())),
		Kind:            kind,
		Title:           o.GetTitle(),
		Summary:         o.GetDescription(),
		SuccessCriteria: criteria,
		Status:          status,
		Priority:        int(o.GetPriorityRank()),
		TargetDate:      td,
		CompletedTasks:  int(o.GetCompletedTaskCount()),
		TotalTasks:      int(o.GetTotalTaskCount()),
		CreatedAt:       o.GetCreatedAt().AsTime(),
		UpdatedAt:       o.GetUpdatedAt().AsTime(),
	}
}

func protoTaskToCLI(t *flowsk_task_pb.Task) cli.TaskView {
	if t == nil {
		return cli.TaskView{}
	}
	cat := protoCategoryToCLICategory(t.GetStateCategory())
	state := protoCategoryToDisplayName(t.GetStateCategory())
	if rawSt := strings.TrimSpace(t.GetStatus()); rawSt != "" {
		parsedSt := parseCLIWorkflowStatus(rawSt)
		cat = canonicalCategoryForStatus(parsedSt)
		state = displayWorkflowStateName(parsedSt)
	}
	var lease *cli.ClaimLeaseView
	if t.GetClaimedByActorId() != "" && t.GetClaimExpiresAt() != nil {
		lease = &cli.ClaimLeaseView{
			Holder:    t.GetClaimedByActorId(),
			ExpiresAt: t.GetClaimExpiresAt().AsTime(),
		}
	}
	var lastSignalAt *time.Time
	if t.GetLastSignalAt() != nil {
		ts := t.GetLastSignalAt().AsTime().UTC()
		if !ts.IsZero() {
			lastSignalAt = &ts
		}
	}
	return cli.TaskView{
		ID:             t.GetId(),
		WorkspaceID:    t.GetWorkspaceId(),
		ProjectID:      t.GetProjectId(),
		Key:            t.GetIdentifier(),
		ExternalID:     t.GetExternalId(),
		Title:          t.GetTitle(),
		Description:    t.GetDescription(),
		State:          state,
		Category:       cat,
		Priority:       mapProtoPriorityToCLI(t.GetPriority()),
		Labels:         append([]string(nil), t.GetTags()...),
		Estimate:       int(t.GetEstimatePoints()),
		Acceptance:     t.GetAcceptanceCriteria(),
		Blocked:        cat == "blocked",
		Lease:          lease,
		HasMarkdownDoc: t.GetHasMarkdownDoc(),
		LastSignalAt:   lastSignalAt,
		CreatedAt:      t.GetCreatedAt().AsTime(),
		UpdatedAt:      t.GetUpdatedAt().AsTime(),
	}
}

func protoTaskToCLIWithRelations(sess *remoteSession, t *flowsk_task_pb.Task, rels []*flowsk_task_pb.TaskRelation) cli.TaskView {
	v := protoTaskToCLI(t)
	if sess == nil || t == nil {
		return v
	}
	if t.GetObjectiveId() != "" {
		projID := t.GetProjectId()
		if oResp, oErr := sess.prov.ObjectiveClient().ListObjectives(sess.ctx, connect.NewRequest(&flowsk_objective_pb.ListObjectivesRequest{
			WorkspaceId: sess.workspaceID.String(),
			ActorId:     sess.actorID.String(),
			ProjectId:   &projID,
			PageSize:    200,
		})); oErr == nil {
			for _, obj := range oResp.Msg.GetObjectives() {
				if obj.GetId() == t.GetObjectiveId() {
					v.ObjectiveKey = slugifyIdentifier(obj.GetTitle())
					break
				}
			}
		}
	}
	if t.GetParentTaskId() != "" {
		if pt, _, ptErr := sess.resolveRemoteTaskQuiet(t.GetParentTaskId()); ptErr == nil && pt != nil {
			v.ParentKey = pt.GetIdentifier()
		}
	}
	if t.GetAssigneeActorId() != "" {
		v.Assignee = t.GetAssigneeActorId()
		actorStr := sess.actorID.String()
		if aResp, aErr := sess.prov.WorkspaceClient().ListActors(sess.ctx, connect.NewRequest(&flowsk_workspace_pb.ListActorsRequest{
			WorkspaceId: sess.workspaceID.String(),
			ActorId:     &actorStr,
			PageSize:    200,
		})); aErr == nil {
			for _, a := range aResp.Msg.GetActors() {
				if a.GetId() == t.GetAssigneeActorId() {
					v.Assignee = firstNonEmpty(a.GetHandle(), a.GetId())
					break
				}
			}
		}
	}
	for _, rel := range rels {
		if rel.GetRelationType() != flowsk_task_pb.TaskRelationType_TASK_RELATION_TYPE_BLOCKS {
			continue
		}
		if rel.GetTargetTaskId() == t.GetId() {
			if up, _, upErr := sess.resolveRemoteTaskQuiet(rel.GetSourceTaskId()); upErr == nil && up != nil {
				if !isRemoteTaskTerminal(up) {
					v.BlockedBy = append(v.BlockedBy, up.GetIdentifier())
					v.Blocked = true
				}
			}
		} else if rel.GetSourceTaskId() == t.GetId() {
			if down, _, downErr := sess.resolveRemoteTaskQuiet(rel.GetTargetTaskId()); downErr == nil && down != nil {
				v.Blocks = append(v.Blocks, down.GetIdentifier())
			}
		}
	}
	return v
}

func isRemoteTaskTerminal(t *flowsk_task_pb.Task) bool {
	if t == nil {
		return false
	}
	if rawSt := strings.TrimSpace(t.GetStatus()); rawSt != "" {
		return parseCLIWorkflowStatus(rawSt).IsTerminal()
	}
	switch t.GetStateCategory() {
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_COMPLETED,
		flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_CANCELED,
		flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_ARCHIVED:
		return true
	default:
		return false
	}
}

func protoTaskToSummaryCLI(t *flowsk_task_pb.Task) cli.TaskSummaryView {
	if t == nil {
		return cli.TaskSummaryView{}
	}
	cat := protoCategoryToCLICategory(t.GetStateCategory())
	state := protoCategoryToDisplayName(t.GetStateCategory())
	if rawSt := strings.TrimSpace(t.GetStatus()); rawSt != "" {
		parsedSt := parseCLIWorkflowStatus(rawSt)
		cat = canonicalCategoryForStatus(parsedSt)
		state = displayWorkflowStateName(parsedSt)
	}
	var lastSignalAt *time.Time
	if t.GetLastSignalAt() != nil {
		ts := t.GetLastSignalAt().AsTime().UTC()
		if !ts.IsZero() {
			lastSignalAt = &ts
		}
	}
	return cli.TaskSummaryView{
		ID:             t.GetId(),
		Key:            t.GetIdentifier(),
		ExternalID:     t.GetExternalId(),
		Title:          t.GetTitle(),
		State:          state,
		Category:       cat,
		Priority:       mapProtoPriorityToCLI(t.GetPriority()),
		Holder:         t.GetClaimedByActorId(),
		Labels:         append([]string(nil), t.GetTags()...),
		Blocked:        cat == "blocked",
		HasMarkdownDoc: t.GetHasMarkdownDoc(),
		LastSignalAt:   lastSignalAt,
		UpdatedAt:      t.GetUpdatedAt().AsTime(),
	}
}

func protoCompactSummaryToCLI(s *flowsk_task_pb.CompactTaskSummary) cli.TaskSummaryView {
	if s == nil {
		return cli.TaskSummaryView{}
	}
	var lastSignalAt *time.Time
	if s.GetLastSignalAt() != nil {
		ts := s.GetLastSignalAt().AsTime().UTC()
		if !ts.IsZero() {
			lastSignalAt = &ts
		}
	}
	return cli.TaskSummaryView{
		ID:             s.GetId(),
		Key:            s.GetIdentifier(),
		ExternalID:     s.GetExternalId(),
		Title:          s.GetTitle(),
		State:          firstNonEmpty(s.GetStateName(), protoCategoryToDisplayName(s.GetStateCategory())),
		Category:       protoCategoryToCLICategory(s.GetStateCategory()),
		Priority:       mapProtoPriorityToCLI(s.GetPriority()),
		Holder:         s.GetClaimedByActorId(),
		Blocked:        s.GetIsBlocked(),
		HasMarkdownDoc: s.GetHasMarkdownDoc(),
		LastSignalAt:   lastSignalAt,
	}
}

func protoAuditToCLI(ev *flowsk_audit_pb.AuditEvent) cli.AuditEventView {
	if ev == nil {
		return cli.AuditEventView{}
	}
	return cli.AuditEventView{
		ID:          ev.GetId(),
		WorkspaceID: ev.GetWorkspaceId(),
		ProjectID:   ev.GetProjectId(),
		Actor:       ev.GetActorId(),
		Action:      ev.GetAction().String(),
		EntityType:  ev.GetEntityType(),
		EntityID:    firstNonEmpty(ev.GetEntityIdentifier(), ev.GetEntityId()),
		Summary:     ev.GetSummary(),
		NewValues:   ev.GetPayloadJson(),
		OccurredAt:  ev.GetOccurredAt().AsTime(),
	}
}

func parseProtoStateCategory(cat string) flowsk_project_pb.WorkflowStateCategory {
	switch strings.ToLower(strings.TrimSpace(cat)) {
	case "triage":
		return flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_TRIAGE
	case "backlog":
		return flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_BACKLOG
	case "todo", "ready", "unstarted":
		return flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_UNSTARTED
	case "in_progress", "review", "blocked", "started":
		return flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_STARTED
	case "done", "completed":
		return flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_COMPLETED
	case "canceled", "cancelled":
		return flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_CANCELED
	case "archived", "archive":
		return flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_ARCHIVED
	default:
		return flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_UNSTARTED
	}
}

func protoCategoryToCLICategory(cat flowsk_project_pb.WorkflowStateCategory) string {
	switch cat {
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_TRIAGE,
		flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_BACKLOG:
		return "backlog"
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_UNSTARTED:
		return "todo"
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_STARTED:
		return "in_progress"
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_COMPLETED:
		return "done"
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_CANCELED:
		return "canceled"
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_ARCHIVED:
		return "archived"
	default:
		return "todo"
	}
}

func protoCategoryToDisplayName(cat flowsk_project_pb.WorkflowStateCategory) string {
	switch cat {
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_TRIAGE:
		return "Triage"
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_BACKLOG:
		return "Backlog"
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_UNSTARTED:
		return "Todo"
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_STARTED:
		return "In Progress"
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_COMPLETED:
		return "Done"
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_CANCELED:
		return "Canceled"
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_ARCHIVED:
		return "Archived"
	default:
		return "Todo"
	}
}

func mapCLIPriorityToProto(p int) flowsk_task_pb.TaskPriority {
	switch p {
	case 0:
		return flowsk_task_pb.TaskPriority_TASK_PRIORITY_NONE
	case 1:
		return flowsk_task_pb.TaskPriority_TASK_PRIORITY_URGENT
	case 2:
		return flowsk_task_pb.TaskPriority_TASK_PRIORITY_HIGH
	case 3:
		return flowsk_task_pb.TaskPriority_TASK_PRIORITY_MEDIUM
	default:
		return flowsk_task_pb.TaskPriority_TASK_PRIORITY_LOW
	}
}

func mapProtoPriorityToCLI(p flowsk_task_pb.TaskPriority) int {
	switch p {
	case flowsk_task_pb.TaskPriority_TASK_PRIORITY_NONE:
		return 0
	case flowsk_task_pb.TaskPriority_TASK_PRIORITY_URGENT:
		return 1
	case flowsk_task_pb.TaskPriority_TASK_PRIORITY_HIGH:
		return 2
	case flowsk_task_pb.TaskPriority_TASK_PRIORITY_MEDIUM,
		flowsk_task_pb.TaskPriority_TASK_PRIORITY_UNSPECIFIED:
		return 3
	default:
		return 4
	}
}

func mapCLIObjectiveKindToProto(raw string) flowsk_objective_pb.ObjectiveKind {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "goal":
		return flowsk_objective_pb.ObjectiveKind_OBJECTIVE_KIND_GOAL
	case "milestone":
		return flowsk_objective_pb.ObjectiveKind_OBJECTIVE_KIND_MILESTONE
	case "cycle":
		return flowsk_objective_pb.ObjectiveKind_OBJECTIVE_KIND_CYCLE
	default:
		return flowsk_objective_pb.ObjectiveKind_OBJECTIVE_KIND_UNSPECIFIED
	}
}

func mapCLIObjectiveStatusToProto(raw string) flowsk_objective_pb.ObjectiveStatus {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "planned", "draft":
		return flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_PLANNED
	case "active":
		return flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_ACTIVE
	case "at_risk", "at-risk":
		return flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_AT_RISK
	case "achieved", "completed", "done":
		return flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_ACHIEVED
	case "dropped", "canceled", "cancelled", "superseded":
		return flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_DROPPED
	default:
		return flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_UNSPECIFIED
	}
}

func matchesRemoteObjectiveStatus(viewStatus, filter string) bool {
	if strings.EqualFold(viewStatus, filter) {
		return true
	}
	return mapCLIObjectiveStatusToProto(viewStatus) != flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_UNSPECIFIED &&
		mapCLIObjectiveStatusToProto(viewStatus) == mapCLIObjectiveStatusToProto(filter)
}

func mapCLITaskKindToProto(raw string) flowsk_task_pb.TaskKind {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "epic":
		return flowsk_task_pb.TaskKind_TASK_KIND_EPIC
	case "bug":
		return flowsk_task_pb.TaskKind_TASK_KIND_BUG
	case "chore", "task":
		return flowsk_task_pb.TaskKind_TASK_KIND_TASK
	case "research", "issue":
		return flowsk_task_pb.TaskKind_TASK_KIND_ISSUE
	default:
		return flowsk_task_pb.TaskKind_TASK_KIND_FEATURE
	}
}

func mapCLICommentKindToProto(raw string) flowsk_task_pb.TaskCommentKind {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "handoff":
		return flowsk_task_pb.TaskCommentKind_TASK_COMMENT_KIND_HANDOFF
	case "blocker", "blocker_report":
		return flowsk_task_pb.TaskCommentKind_TASK_COMMENT_KIND_BLOCKER_REPORT
	case "decision", "resolution":
		return flowsk_task_pb.TaskCommentKind_TASK_COMMENT_KIND_RESOLUTION
	default:
		return flowsk_task_pb.TaskCommentKind_TASK_COMMENT_KIND_COMMENT
	}
}

func protoCommentKindToCLI(pbKind flowsk_task_pb.TaskCommentKind, fallback string) string {
	switch pbKind {
	case flowsk_task_pb.TaskCommentKind_TASK_COMMENT_KIND_HANDOFF:
		return "handoff"
	case flowsk_task_pb.TaskCommentKind_TASK_COMMENT_KIND_BLOCKER_REPORT:
		return "blocker"
	case flowsk_task_pb.TaskCommentKind_TASK_COMMENT_KIND_RESOLUTION:
		if strings.EqualFold(strings.TrimSpace(fallback), "resolution") {
			return "resolution"
		}
		return "decision"
	default:
		if trimmed := strings.ToLower(strings.TrimSpace(fallback)); trimmed != "" {
			return trimmed
		}
		return "note"
	}
}

func mapCLIAuditActionToProto(raw string) flowsk_audit_pb.AuditAction {
	switch mapCLIAuditAction(raw) {
	case tx.AuditActionCreate:
		return flowsk_audit_pb.AuditAction_AUDIT_ACTION_CREATE
	case tx.AuditActionUpdate:
		return flowsk_audit_pb.AuditAction_AUDIT_ACTION_UPDATE
	case tx.AuditActionDelete:
		return flowsk_audit_pb.AuditAction_AUDIT_ACTION_DELETE
	case tx.AuditActionTransition:
		return flowsk_audit_pb.AuditAction_AUDIT_ACTION_TRANSITION
	case tx.AuditActionClaim:
		return flowsk_audit_pb.AuditAction_AUDIT_ACTION_CLAIM
	case tx.AuditActionRelease:
		return flowsk_audit_pb.AuditAction_AUDIT_ACTION_RELEASE
	case tx.AuditActionBatchExec:
		return flowsk_audit_pb.AuditAction_AUDIT_ACTION_BATCH_EXEC
	default:
		return flowsk_audit_pb.AuditAction_AUDIT_ACTION_UNSPECIFIED
	}
}

func hasAllRemoteTags(have, required []string) bool {
	for _, req := range required {
		found := false
		for _, h := range have {
			if strings.EqualFold(strings.TrimSpace(h), strings.TrimSpace(req)) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
