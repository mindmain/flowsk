// File handlers_test.go tests all six ConnectRPC bounded-context handlers
// end-to-end over HTTP using generated Connect clients and fake domain services.
package connectrpc_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	flowsk_audit_pb "github.com/mindmain/flowsk/gen/go/flowsk/audit/v1"
	"github.com/mindmain/flowsk/gen/go/flowsk/audit/v1/flowsk_audit_pbconnect"
	flowsk_language_pb "github.com/mindmain/flowsk/gen/go/flowsk/language/v1"
	"github.com/mindmain/flowsk/gen/go/flowsk/language/v1/flowsk_language_pbconnect"
	flowsk_objective_pb "github.com/mindmain/flowsk/gen/go/flowsk/objective/v1"
	"github.com/mindmain/flowsk/gen/go/flowsk/objective/v1/flowsk_objective_pbconnect"
	flowsk_project_pb "github.com/mindmain/flowsk/gen/go/flowsk/project/v1"
	"github.com/mindmain/flowsk/gen/go/flowsk/project/v1/flowsk_project_pbconnect"
	flowsk_task_pb "github.com/mindmain/flowsk/gen/go/flowsk/task/v1"
	"github.com/mindmain/flowsk/gen/go/flowsk/task/v1/flowsk_task_pbconnect"
	flowsk_workspace_pb "github.com/mindmain/flowsk/gen/go/flowsk/workspace/v1"
	"github.com/mindmain/flowsk/gen/go/flowsk/workspace/v1/flowsk_workspace_pbconnect"
	"github.com/mindmain/flowsk/internal/adapter/codemode"
	"github.com/mindmain/flowsk/internal/domain/audit"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/objective"
	"github.com/mindmain/flowsk/internal/domain/project"
	"github.com/mindmain/flowsk/internal/domain/task"
	"github.com/mindmain/flowsk/internal/domain/tx"
	"github.com/mindmain/flowsk/internal/domain/workspace"
	"github.com/mindmain/flowsk/internal/language"
	"github.com/mindmain/flowsk/internal/transport/connectrpc"
	"github.com/mindmain/flowsk/internal/transport/connectrpc/audithandler"
	"github.com/mindmain/flowsk/internal/transport/connectrpc/languagehandler"
	"github.com/mindmain/flowsk/internal/transport/connectrpc/objectivehandler"
	"github.com/mindmain/flowsk/internal/transport/connectrpc/projecthandler"
	"github.com/mindmain/flowsk/internal/transport/connectrpc/taskhandler"
	"github.com/mindmain/flowsk/internal/transport/connectrpc/workspacehandler"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeServiceContainer struct {
	wsSvc   workspace.Service
	projSvc project.Service
	objSvc  objective.Service
	taskSvc task.Service
	exec    languagehandler.CodeExecutor
	guide   language.GuideProvider
	audSvc  audit.Service
}

func (f *fakeServiceContainer) WorkspaceService() workspace.Service          { return f.wsSvc }
func (f *fakeServiceContainer) ProjectService() project.Service              { return f.projSvc }
func (f *fakeServiceContainer) ObjectiveService() objective.Service          { return f.objSvc }
func (f *fakeServiceContainer) TaskService() task.Service                    { return f.taskSvc }
func (f *fakeServiceContainer) CodeExecutor() languagehandler.CodeExecutor   { return f.exec }
func (f *fakeServiceContainer) GuideProvider() language.GuideProvider        { return f.guide }
func (f *fakeServiceContainer) AuditService() audit.Service                  { return f.audSvc }

// fakeWorkspaceService is a thread-safe in-memory double of workspace.Service.
type fakeWorkspaceService struct {
	mu         sync.RWMutex
	workspaces map[uuid.UUID]workspace.Workspace
	members    map[uuid.UUID][]workspace.WorkspaceMember
}

func newFakeWorkspaceService() *fakeWorkspaceService {
	return &fakeWorkspaceService{
		workspaces: make(map[uuid.UUID]workspace.Workspace),
		members:    make(map[uuid.UUID][]workspace.WorkspaceMember),
	}
}

func (s *fakeWorkspaceService) CreateWorkspace(ctx context.Context, in workspace.CreateWorkspaceInput) (workspace.Workspace, error) {
	id, err := authctx.FromContext(ctx)
	if err != nil {
		return workspace.Workspace{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	ws := workspace.Workspace{
		ID:          id.WorkspaceID,
		Slug:        in.Slug,
		Name:        in.Name,
		Description: in.Description,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	s.workspaces[ws.ID] = ws
	return ws, nil
}

func (s *fakeWorkspaceService) GetWorkspace(ctx context.Context, workspaceID uuid.UUID) (workspace.Workspace, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return workspace.Workspace{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	ws, ok := s.workspaces[workspaceID]
	if !ok {
		return workspace.Workspace{}, errs.New(errs.ErrNotFound, "fakeWorkspaceService.GetWorkspace", "workspace not found")
	}
	return ws.Clone(), nil
}

func (s *fakeWorkspaceService) GetWorkspaceBySlug(ctx context.Context, slug string) (workspace.Workspace, error) {
	id, err := authctx.FromContext(ctx)
	if err != nil {
		return workspace.Workspace{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, ws := range s.workspaces {
		if ws.Slug == slug && ws.ID == id.WorkspaceID {
			return ws.Clone(), nil
		}
	}
	return workspace.Workspace{}, errs.New(errs.ErrNotFound, "fakeWorkspaceService.GetWorkspaceBySlug", "workspace not found")
}

func (s *fakeWorkspaceService) ListWorkspaces(ctx context.Context) ([]workspace.Workspace, error) {
	id, err := authctx.FromContext(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []workspace.Workspace
	if ws, ok := s.workspaces[id.WorkspaceID]; ok {
		out = append(out, ws.Clone())
	}
	return out, nil
}

func (s *fakeWorkspaceService) UpdateWorkspace(ctx context.Context, in workspace.UpdateWorkspaceInput) (workspace.Workspace, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID); err != nil {
		return workspace.Workspace{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ws, ok := s.workspaces[in.WorkspaceID]
	if !ok {
		return workspace.Workspace{}, errs.New(errs.ErrNotFound, "fakeWorkspaceService.UpdateWorkspace", "workspace not found")
	}
	if in.Name != nil {
		ws.Name = *in.Name
	}
	if in.Description != nil {
		ws.Description = *in.Description
	}
	ws.UpdatedAt = time.Now().UTC()
	s.workspaces[in.WorkspaceID] = ws
	return ws.Clone(), nil
}

func (s *fakeWorkspaceService) ArchiveWorkspace(ctx context.Context, workspaceID uuid.UUID) error {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.workspaces[workspaceID]; !ok {
		return errs.New(errs.ErrNotFound, "fakeWorkspaceService.ArchiveWorkspace", "workspace not found")
	}
	delete(s.workspaces, workspaceID)
	return nil
}

func (s *fakeWorkspaceService) UpsertMember(ctx context.Context, in workspace.UpsertMemberInput) (workspace.WorkspaceMember, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID); err != nil {
		return workspace.WorkspaceMember{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	actorID := in.ActorID
	if actorID == uuid.Nil {
		actorID = uuid.New()
	}
	member := workspace.WorkspaceMember{
		WorkspaceID: in.WorkspaceID,
		ActorID:     actorID,
		Kind:        in.Kind,
		Handle:      in.Handle,
		DisplayName: in.DisplayName,
		Role:        in.Role,
		CreatedAt:   time.Now().UTC(),
	}
	s.members[in.WorkspaceID] = append(s.members[in.WorkspaceID], member)
	return member, nil
}

func (s *fakeWorkspaceService) ListMembers(ctx context.Context, workspaceID uuid.UUID) ([]workspace.WorkspaceMember, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.members[workspaceID]
	out := make([]workspace.WorkspaceMember, len(src))
	copy(out, src)
	return out, nil
}

// fakeProjectService is a thread-safe in-memory double of project.Service.
type fakeProjectService struct {
	mu       sync.RWMutex
	projects map[uuid.UUID]project.Project
	docs     map[uuid.UUID]string
}

func newFakeProjectService() *fakeProjectService {
	return &fakeProjectService{
		projects: make(map[uuid.UUID]project.Project),
		docs:     make(map[uuid.UUID]string),
	}
}

func (s *fakeProjectService) CreateProject(ctx context.Context, in project.CreateProjectInput) (project.Project, error) {
	id, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID)
	if err != nil {
		return project.Project{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	p := project.Project{
		ID:          uuid.New(),
		WorkspaceID: in.WorkspaceID,
		Slug:        in.Slug,
		KeyPrefix:   in.KeyPrefix,
		Name:        in.Name,
		Description: in.Description,
		Status:      project.StatusActive,
		TaskSeq:     0,
		CreatedBy:   id.ActorID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	s.projects[p.ID] = p
	return p.Clone(), nil
}

func (s *fakeProjectService) GetProject(ctx context.Context, workspaceID, projectID uuid.UUID) (project.Project, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return project.Project{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.projects[projectID]
	if !ok || p.WorkspaceID != workspaceID {
		return project.Project{}, errs.New(errs.ErrNotFound, "fakeProjectService.GetProject", "project not found")
	}
	return p.Clone(), nil
}

func (s *fakeProjectService) GetProjectBySlug(ctx context.Context, workspaceID uuid.UUID, slug string) (project.Project, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return project.Project{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.projects {
		if p.WorkspaceID == workspaceID && p.Slug == slug {
			return p.Clone(), nil
		}
	}
	return project.Project{}, errs.New(errs.ErrNotFound, "fakeProjectService.GetProjectBySlug", "project not found")
}

func (s *fakeProjectService) ListProjects(ctx context.Context, filter project.ListProjectsFilter) ([]project.Project, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, filter.WorkspaceID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []project.Project
	for _, p := range s.projects {
		if p.WorkspaceID != filter.WorkspaceID {
			continue
		}
		if filter.Status != nil && p.Status != *filter.Status {
			continue
		}
		out = append(out, p.Clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	if int(filter.Offset) >= len(out) {
		return nil, nil
	}
	out = out[filter.Offset:]
	if filter.Limit > 0 && len(out) > int(filter.Limit) {
		out = out[:filter.Limit]
	}
	return out, nil
}

func (s *fakeProjectService) UpdateProject(ctx context.Context, in project.UpdateProjectInput) (project.Project, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID); err != nil {
		return project.Project{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[in.ProjectID]
	if !ok || p.WorkspaceID != in.WorkspaceID {
		return project.Project{}, errs.New(errs.ErrNotFound, "fakeProjectService.UpdateProject", "project not found")
	}
	if in.Name != nil {
		p.Name = *in.Name
	}
	if in.Description != nil {
		p.Description = *in.Description
	}
	if in.Status != nil {
		p.Status = *in.Status
	}
	p.UpdatedAt = time.Now().UTC()
	s.projects[p.ID] = p
	return p.Clone(), nil
}

func (s *fakeProjectService) DeleteProject(ctx context.Context, workspaceID, projectID uuid.UUID) error {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[projectID]
	if !ok || p.WorkspaceID != workspaceID {
		return errs.New(errs.ErrNotFound, "fakeProjectService.DeleteProject", "project not found")
	}
	delete(s.projects, projectID)
	return nil
}

func (s *fakeProjectService) BindSubproject(ctx context.Context, in project.BindSubprojectInput) (project.ProjectBinding, error) {
	id, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID)
	if err != nil {
		return project.ProjectBinding{}, err
	}
	now := time.Now().UTC()
	return project.ProjectBinding{
		ID:              uuid.New(),
		WorkspaceID:     in.WorkspaceID,
		ParentProjectID: in.ParentProjectID,
		ChildProjectID:  in.ChildProjectID,
		ScopeSubpath:    in.ScopeSubpath,
		CreatedBy:       id.ActorID,
		CreatedAt:       now,
		UpdatedAt:       now,
	}, nil
}

func (s *fakeProjectService) UnbindSubproject(ctx context.Context, in project.UnbindSubprojectInput) (bool, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID); err != nil {
		return false, err
	}
	return true, nil
}

func (s *fakeProjectService) ListBindings(ctx context.Context, filter project.ListBindingsFilter) ([]project.ProjectBinding, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, filter.WorkspaceID); err != nil {
		return nil, err
	}
	return nil, nil
}

func (s *fakeProjectService) GetProjectMarkdown(ctx context.Context, workspaceID, projectID uuid.UUID) (string, bool, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return "", false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.projects[projectID]
	if !ok || p.WorkspaceID != workspaceID {
		return "", false, errs.New(errs.ErrNotFound, "fakeProjectService.GetProjectMarkdown", "project not found")
	}
	md, exists := s.docs[projectID]
	return md, exists, nil
}

func (s *fakeProjectService) PutProjectMarkdown(ctx context.Context, workspaceID, projectID uuid.UUID, markdown string) (project.Project, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return project.Project{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[projectID]
	if !ok || p.WorkspaceID != workspaceID {
		return project.Project{}, errs.New(errs.ErrNotFound, "fakeProjectService.PutProjectMarkdown", "project not found")
	}
	s.docs[projectID] = markdown
	p.HasMarkdownDoc = strings.TrimSpace(markdown) != ""
	p.UpdatedAt = time.Now().UTC()
	s.projects[projectID] = p
	return p.Clone(), nil
}

func (s *fakeProjectService) DeleteProjectMarkdown(ctx context.Context, workspaceID, projectID uuid.UUID) (project.Project, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return project.Project{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[projectID]
	if !ok || p.WorkspaceID != workspaceID {
		return project.Project{}, errs.New(errs.ErrNotFound, "fakeProjectService.DeleteProjectMarkdown", "project not found")
	}
	delete(s.docs, projectID)
	p.HasMarkdownDoc = false
	p.UpdatedAt = time.Now().UTC()
	s.projects[projectID] = p
	return p.Clone(), nil
}

// fakeObjectiveService is a thread-safe in-memory double of objective.Service.
type fakeObjectiveService struct {
	mu         sync.RWMutex
	objectives map[uuid.UUID]objective.Objective
}

func newFakeObjectiveService() *fakeObjectiveService {
	return &fakeObjectiveService{objectives: make(map[uuid.UUID]objective.Objective)}
}

func (s *fakeObjectiveService) CreateObjective(ctx context.Context, in objective.CreateObjectiveInput) (objective.Objective, error) {
	id, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID)
	if err != nil {
		return objective.Objective{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	st := in.Status
	if st == "" {
		st = objective.StatusDraft
	}
	o := objective.Objective{
		ID:              uuid.New(),
		WorkspaceID:     in.WorkspaceID,
		ProjectID:       in.ProjectID,
		Slug:            in.Slug,
		Title:           in.Title,
		Summary:         in.Summary,
		SuccessCriteria: append([]string(nil), in.SuccessCriteria...),
		Status:          st,
		PriorityRank:    in.PriorityRank,
		TargetDate:      in.TargetDate,
		CreatedBy:       id.ActorID,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	s.objectives[o.ID] = o
	return o.Clone(), nil
}

func (s *fakeObjectiveService) GetObjective(ctx context.Context, workspaceID, objectiveID uuid.UUID) (objective.Objective, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return objective.Objective{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.objectives[objectiveID]
	if !ok || o.WorkspaceID != workspaceID {
		return objective.Objective{}, errs.New(errs.ErrNotFound, "fakeObjectiveService.GetObjective", "objective not found")
	}
	return o.Clone(), nil
}

func (s *fakeObjectiveService) GetObjectiveBySlug(ctx context.Context, workspaceID, projectID uuid.UUID, slug string) (objective.Objective, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return objective.Objective{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, o := range s.objectives {
		if o.WorkspaceID == workspaceID && o.ProjectID == projectID && o.Slug == slug {
			return o.Clone(), nil
		}
	}
	return objective.Objective{}, errs.New(errs.ErrNotFound, "fakeObjectiveService.GetObjectiveBySlug", "objective not found")
}

func (s *fakeObjectiveService) ListObjectives(ctx context.Context, filter objective.ListObjectivesFilter) ([]objective.Objective, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, filter.WorkspaceID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []objective.Objective
	for _, o := range s.objectives {
		if o.WorkspaceID != filter.WorkspaceID {
			continue
		}
		if filter.ProjectID != uuid.Nil && o.ProjectID != filter.ProjectID {
			continue
		}
		if filter.Status != nil && o.Status != *filter.Status {
			continue
		}
		out = append(out, o.Clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	if int(filter.Offset) >= len(out) {
		return nil, nil
	}
	out = out[filter.Offset:]
	if filter.Limit > 0 && len(out) > int(filter.Limit) {
		out = out[:filter.Limit]
	}
	return out, nil
}

func (s *fakeObjectiveService) UpdateObjective(ctx context.Context, in objective.UpdateObjectiveInput) (objective.Objective, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID); err != nil {
		return objective.Objective{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objectives[in.ObjectiveID]
	if !ok || o.WorkspaceID != in.WorkspaceID {
		return objective.Objective{}, errs.New(errs.ErrNotFound, "fakeObjectiveService.UpdateObjective", "objective not found")
	}
	if in.Title != nil {
		o.Title = *in.Title
	}
	if in.Summary != nil {
		o.Summary = *in.Summary
	}
	if in.SuccessCriteria != nil {
		o.SuccessCriteria = append([]string(nil), in.SuccessCriteria...)
	}
	if in.Status != nil {
		o.Status = *in.Status
	}
	if in.TargetDate != nil {
		if in.TargetDate.IsZero() {
			o.TargetDate = nil
		} else {
			cp := *in.TargetDate
			o.TargetDate = &cp
		}
	}
	o.UpdatedAt = time.Now().UTC()
	s.objectives[o.ID] = o
	return o.Clone(), nil
}

func (s *fakeObjectiveService) DeleteObjective(ctx context.Context, workspaceID, objectiveID uuid.UUID) error {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objectives[objectiveID]
	if !ok || o.WorkspaceID != workspaceID {
		return errs.New(errs.ErrNotFound, "fakeObjectiveService.DeleteObjective", "objective not found")
	}
	delete(s.objectives, objectiveID)
	return nil
}

// fakeTaskService is a thread-safe in-memory double of task.Service.
type fakeTaskService struct {
	mu              sync.RWMutex
	seq             int64
	tasks           map[uuid.UUID]task.Task
	relations       map[uuid.UUID]task.TaskRelation
	comments        map[uuid.UUID][]task.TaskComment
	docs            map[uuid.UUID]string
	lastReleaseTask task.ReleaseTaskInput
}

func newFakeTaskService() *fakeTaskService {
	return &fakeTaskService{
		tasks:     make(map[uuid.UUID]task.Task),
		relations: make(map[uuid.UUID]task.TaskRelation),
		comments:  make(map[uuid.UUID][]task.TaskComment),
		docs:      make(map[uuid.UUID]string),
	}
}

func (s *fakeTaskService) CreateTask(ctx context.Context, in task.CreateTaskInput) (task.Task, error) {
	id, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID)
	if err != nil {
		return task.Task{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if in.ExternalID != "" {
		for _, ex := range s.tasks {
			if ex.WorkspaceID == in.WorkspaceID && ex.ProjectID == in.ProjectID && ex.ExternalID == in.ExternalID {
				return task.Task{}, errs.New(errs.ErrConflict, "fakeTaskService.CreateTask", "external_id already exists in project")
			}
		}
	}
	s.seq++
	now := time.Now().UTC()
	st := in.Status
	if st == "" {
		st = task.StatusReady
	}
	t := task.Task{
		ID:                 uuid.New(),
		WorkspaceID:        in.WorkspaceID,
		ProjectID:          in.ProjectID,
		ObjectiveID:        in.ObjectiveID,
		ParentTaskID:       in.ParentTaskID,
		Key:                fmt.Sprintf("FLW-%d", s.seq),
		ExternalID:         in.ExternalID,
		Kind:               in.Kind,
		Title:              in.Title,
		Description:        in.Description,
		AcceptanceCriteria: append([]string(nil), in.AcceptanceCriteria...),
		Status:             st,
		Priority:           in.Priority,
		AssigneeActorID:    in.AssigneeActorID,
		Tags:               append([]string(nil), in.Tags...),
		Version:            1,
		CreatedBy:          id.ActorID,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	s.tasks[t.ID] = t
	return t.Clone(), nil
}

func (s *fakeTaskService) GetTask(ctx context.Context, workspaceID, taskID uuid.UUID) (task.Task, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return task.Task{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tasks[taskID]
	if !ok || t.WorkspaceID != workspaceID {
		return task.Task{}, errs.New(errs.ErrNotFound, "fakeTaskService.GetTask", "task not found")
	}
	return t.Clone(), nil
}

func (s *fakeTaskService) GetTaskByKey(ctx context.Context, workspaceID uuid.UUID, key string) (task.Task, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return task.Task{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.tasks {
		if t.WorkspaceID == workspaceID && t.Key == key {
			return t.Clone(), nil
		}
	}
	return task.Task{}, errs.New(errs.ErrNotFound, "fakeTaskService.GetTaskByKey", "task not found")
}

func (s *fakeTaskService) GetTaskByExternalID(ctx context.Context, workspaceID, projectID uuid.UUID, externalID string) (task.Task, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return task.Task{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.tasks {
		if t.WorkspaceID == workspaceID && t.ProjectID == projectID && t.ExternalID != "" && t.ExternalID == externalID {
			return t.Clone(), nil
		}
	}
	return task.Task{}, errs.New(errs.ErrNotFound, "fakeTaskService.GetTaskByExternalID", "task not found")
}

func (s *fakeTaskService) UpdateTask(ctx context.Context, in task.UpdateTaskInput) (task.Task, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID); err != nil {
		return task.Task{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[in.TaskID]
	if !ok || t.WorkspaceID != in.WorkspaceID {
		return task.Task{}, errs.New(errs.ErrNotFound, "fakeTaskService.UpdateTask", "task not found")
	}
	if in.ExternalID != nil {
		if *in.ExternalID != "" && *in.ExternalID != t.ExternalID {
			for _, ex := range s.tasks {
				if ex.ID != t.ID && ex.WorkspaceID == t.WorkspaceID && ex.ProjectID == t.ProjectID && ex.ExternalID == *in.ExternalID {
					return task.Task{}, errs.New(errs.ErrConflict, "fakeTaskService.UpdateTask", "external_id already exists in project")
				}
			}
		}
		t.ExternalID = *in.ExternalID
	}
	if in.Title != nil {
		t.Title = *in.Title
	}
	if in.Description != nil {
		t.Description = *in.Description
	}
	if in.Priority != nil {
		t.Priority = *in.Priority
	}
	if in.Kind != nil {
		t.Kind = *in.Kind
	}
	if in.Tags != nil {
		t.Tags = append([]string(nil), in.Tags...)
	}
	t.UpdatedAt = time.Now().UTC()
	s.tasks[t.ID] = t
	return t.Clone(), nil
}

func (s *fakeTaskService) TransitionTask(ctx context.Context, in task.TransitionTaskInput) (task.Task, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID); err != nil {
		return task.Task{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[in.TaskID]
	if !ok || t.WorkspaceID != in.WorkspaceID {
		return task.Task{}, errs.New(errs.ErrNotFound, "fakeTaskService.TransitionTask", "task not found")
	}
	t.Status = in.TargetStatus
	if in.ReleaseActiveLease {
		t.ActiveLease = nil
	}
	now := time.Now().UTC()
	if t.Status.IsTerminal() {
		t.CompletedAt = &now
	}
	t.UpdatedAt = now
	s.tasks[t.ID] = t
	return t.Clone(), nil
}

func (s *fakeTaskService) DeleteTask(ctx context.Context, workspaceID, taskID uuid.UUID) error {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, taskID)
	return nil
}

func (s *fakeTaskService) ClaimTask(ctx context.Context, in task.ClaimTaskInput) (task.ClaimLease, error) {
	id, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID)
	if err != nil {
		return task.ClaimLease{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[in.TaskID]
	if !ok || t.WorkspaceID != in.WorkspaceID {
		return task.ClaimLease{}, errs.New(errs.ErrNotFound, "fakeTaskService.ClaimTask", "task not found")
	}
	now := time.Now().UTC()
	if t.ActiveLease != nil && !t.ActiveLease.IsExpiredAt(now) && t.ActiveLease.HolderActorID != id.ActorID {
		return task.ClaimLease{}, errs.New(errs.ErrLeaseHeld, "fakeTaskService.ClaimTask", "task lease is already held by another actor")
	}
	ttl := in.TTLSeconds
	if ttl <= 0 {
		ttl = 1800
	}
	lease := task.ClaimLease{
		TaskID:        t.ID,
		WorkspaceID:   t.WorkspaceID,
		LeaseToken:    "tok-" + uuid.NewString(),
		HolderActorID: id.ActorID,
		HolderKind:    id.Kind,
		HolderHandle:  id.Handle,
		AcquiredAt:    now,
		HeartbeatAt:   now,
		ExpiresAt:     now.Add(time.Duration(ttl) * time.Second),
	}
	t.ActiveLease = &lease
	if in.AutoTransitionInProgress && t.Status == task.StatusReady {
		t.Status = task.StatusInProgress
	}
	s.tasks[t.ID] = t
	return lease, nil
}

func (s *fakeTaskService) ReleaseTask(ctx context.Context, in task.ReleaseTaskInput) error {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastReleaseTask = in
	t, ok := s.tasks[in.TaskID]
	if !ok || t.WorkspaceID != in.WorkspaceID {
		return errs.New(errs.ErrNotFound, "fakeTaskService.ReleaseTask", "task not found")
	}
	t.ActiveLease = nil
	s.tasks[t.ID] = t
	return nil
}

func (s *fakeTaskService) PollWorkQueue(ctx context.Context, filter task.AgentWorkQueueFilter) ([]task.CompactTaskSummary, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, filter.WorkspaceID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []task.CompactTaskSummary
	for _, t := range s.tasks {
		if t.WorkspaceID != filter.WorkspaceID {
			continue
		}
		if filter.ProjectID != uuid.Nil && t.ProjectID != filter.ProjectID {
			continue
		}
		if filter.ExternalID != "" && t.ExternalID != filter.ExternalID {
			continue
		}
		if filter.OnlyUnclaimed && t.ActiveLease != nil {
			continue
		}
		if len(filter.Statuses) > 0 {
			okStatus := false
			for _, st := range filter.Statuses {
				if t.Status == st {
					okStatus = true
					break
				}
			}
			if !okStatus {
				continue
			}
		}
		var holder string
		var expires *time.Time
		if t.ActiveLease != nil {
			holder = t.ActiveLease.HolderHandle
			exp := t.ActiveLease.ExpiresAt
			expires = &exp
		}
		out = append(out, task.CompactTaskSummary{
			ID:             t.ID,
			Key:            t.Key,
			ExternalID:     t.ExternalID,
			Kind:           t.Kind,
			Title:          t.Title,
			Status:         t.Status,
			Priority:       t.Priority,
			HolderHandle:   holder,
			LeaseExpiresAt: expires,
			HasMarkdownDoc: t.HasMarkdownDoc,
			UpdatedAt:      t.UpdatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	if filter.Limit > 0 && len(out) > int(filter.Limit) {
		out = out[:filter.Limit]
	}
	return out, nil
}

func (s *fakeTaskService) GetProjectDigest(ctx context.Context, workspaceID, projectID uuid.UUID, _ task.DigestOptions) (task.ProjectDigest, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return task.ProjectDigest{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	counts := make(map[task.Status]int32)
	var ready []task.CompactTaskSummary
	for _, t := range s.tasks {
		if t.WorkspaceID == workspaceID && t.ProjectID == projectID {
			counts[t.Status]++
			if t.Status == task.StatusReady && t.ActiveLease == nil {
				ready = append(ready, task.CompactTaskSummary{
					ID:       t.ID,
					Key:      t.Key,
					Kind:     t.Kind,
					Title:    t.Title,
					Status:   t.Status,
					Priority: t.Priority,
				})
			}
		}
	}
	return task.ProjectDigest{
		WorkspaceID:      workspaceID,
		ProjectID:        projectID,
		ProjectSlug:      "core",
		ProjectKeyPrefix: "FLW",
		GeneratedAt:      time.Now().UTC(),
		CountsByStatus:   counts,
		ReadyQueue:       ready,
	}, nil
}

func (s *fakeTaskService) AddRelation(ctx context.Context, in task.AddRelationInput) (task.TaskRelation, error) {
	id, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID)
	if err != nil {
		return task.TaskRelation{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rel := task.TaskRelation{
		ID:           uuid.New(),
		WorkspaceID:  in.WorkspaceID,
		SourceTaskID: in.SourceTaskID,
		TargetTaskID: in.TargetTaskID,
		Type:         in.Type,
		Note:         in.Note,
		CreatedBy:    id.ActorID,
		CreatedAt:    time.Now().UTC(),
	}
	s.relations[rel.ID] = rel
	return rel, nil
}

func (s *fakeTaskService) RemoveRelation(ctx context.Context, workspaceID, relationID uuid.UUID) error {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.relations, relationID)
	return nil
}

func (s *fakeTaskService) ListRelations(ctx context.Context, workspaceID, taskID uuid.UUID) ([]task.TaskRelation, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []task.TaskRelation
	for _, r := range s.relations {
		if r.WorkspaceID == workspaceID && (r.SourceTaskID == taskID || r.TargetTaskID == taskID) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *fakeTaskService) AddComment(ctx context.Context, in task.AddCommentInput) (task.TaskComment, error) {
	id, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID)
	if err != nil {
		return task.TaskComment{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := task.TaskComment{
		ID:            uuid.New(),
		WorkspaceID:   in.WorkspaceID,
		TaskID:        in.TaskID,
		AuthorActorID: id.ActorID,
		AuthorKind:    id.Kind,
		AuthorHandle:  id.Handle,
		Kind:          in.Kind,
		Body:          in.Body,
		CreatedAt:     time.Now().UTC(),
	}
	s.comments[in.TaskID] = append(s.comments[in.TaskID], c)
	return c, nil
}

func (s *fakeTaskService) ListComments(ctx context.Context, workspaceID, taskID uuid.UUID, _ int32) ([]task.TaskComment, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.comments[taskID]
	out := make([]task.TaskComment, len(src))
	copy(out, src)
	return out, nil
}

func (s *fakeTaskService) GetTaskMarkdown(ctx context.Context, workspaceID, taskID uuid.UUID) (string, bool, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return "", false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tasks[taskID]
	if !ok || t.WorkspaceID != workspaceID {
		return "", false, errs.New(errs.ErrNotFound, "fakeTaskService.GetTaskMarkdown", "task not found")
	}
	md, exists := s.docs[taskID]
	return md, exists, nil
}

func (s *fakeTaskService) PutTaskMarkdown(ctx context.Context, workspaceID, taskID uuid.UUID, markdown string) (task.Task, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return task.Task{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[taskID]
	if !ok || t.WorkspaceID != workspaceID {
		return task.Task{}, errs.New(errs.ErrNotFound, "fakeTaskService.PutTaskMarkdown", "task not found")
	}
	s.docs[taskID] = markdown
	t.HasMarkdownDoc = strings.TrimSpace(markdown) != ""
	t.UpdatedAt = time.Now().UTC()
	s.tasks[taskID] = t
	return t.Clone(), nil
}

func (s *fakeTaskService) DeleteTaskMarkdown(ctx context.Context, workspaceID, taskID uuid.UUID) (task.Task, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return task.Task{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[taskID]
	if !ok || t.WorkspaceID != workspaceID {
		return task.Task{}, errs.New(errs.ErrNotFound, "fakeTaskService.DeleteTaskMarkdown", "task not found")
	}
	delete(s.docs, taskID)
	t.HasMarkdownDoc = false
	t.UpdatedAt = time.Now().UTC()
	s.tasks[taskID] = t
	return t.Clone(), nil
}

func (s *fakeTaskService) PurgeArchivedTasks(ctx context.Context, workspaceID uuid.UUID) (task.PurgeArchivedResult, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return task.PurgeArchivedResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var res task.PurgeArchivedResult
	for id, t := range s.tasks {
		if t.WorkspaceID == workspaceID && t.Status == task.StatusArchived {
			res.DeletedTaskCount++
			res.DeletedTaskKeys = append(res.DeletedTaskKeys, t.Key)
			if _, ok := s.docs[id]; ok {
				delete(s.docs, id)
				res.DeletedMarkdownCount++
			}
			delete(s.tasks, id)
		}
	}
	return res, nil
}

func (s *fakeTaskService) InspectOrphans(ctx context.Context, workspaceID uuid.UUID) (task.OrphanResourcesReport, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return task.OrphanResourcesReport{}, err
	}
	return task.OrphanResourcesReport{}, nil
}

func (s *fakeTaskService) PruneOrphans(ctx context.Context, workspaceID uuid.UUID, dryRun bool) (task.PruneResourcesResult, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return task.PruneResourcesResult{}, err
	}
	return task.PruneResourcesResult{DryRun: dryRun}, nil
}

// fakeCodeExecutor is a deterministic double of languagehandler.CodeExecutor.
type fakeCodeExecutor struct{}

func (e *fakeCodeExecutor) Execute(ctx context.Context, in codemode.ExecuteInput) (codemode.ExecuteOutput, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID); err != nil {
		return codemode.ExecuteOutput{}, err
	}
	if strings.Contains(in.Script, "flowsk.board()") {
		return codemode.ExecuteOutput{
			Committed: false,
			DryRun:    in.DryRun,
			Mutations: 0,
			Summary:   `{"ready":0}`,
		}, nil
	}
	return codemode.ExecuteOutput{
		Committed: !in.DryRun,
		DryRun:    in.DryRun,
		Mutations: 1,
		Summary:   `{"key":"FLW-99"}`,
	}, nil
}

// fakeAuditService is a deterministic double of audit.Service.
type fakeAuditService struct {
	record  audit.Record
	records []audit.Record
}

func (s *fakeAuditService) allRecords() []audit.Record {
	if len(s.records) > 0 {
		return s.records
	}
	if s.record.ID != uuid.Nil {
		return []audit.Record{s.record}
	}
	return nil
}

func (s *fakeAuditService) GetRecord(ctx context.Context, workspaceID, auditID uuid.UUID) (audit.Record, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return audit.Record{}, err
	}
	for _, rec := range s.allRecords() {
		if rec.ID == auditID && rec.WorkspaceID == workspaceID {
			return rec.Clone(), nil
		}
	}
	return audit.Record{}, errs.New(errs.ErrNotFound, "fakeAuditService.GetRecord", "audit record not found")
}

func (s *fakeAuditService) QueryRecords(ctx context.Context, filter audit.QueryFilter) ([]audit.Record, error) {
	if _, err := authctx.RequireWorkspaceOwnership(ctx, filter.WorkspaceID); err != nil {
		return nil, err
	}
	var matched []audit.Record
	for _, rec := range s.allRecords() {
		if rec.WorkspaceID != filter.WorkspaceID {
			continue
		}
		matched = append(matched, rec.Clone())
	}
	if int(filter.Offset) >= len(matched) {
		return nil, nil
	}
	matched = matched[filter.Offset:]
	if filter.Limit > 0 && len(matched) > int(filter.Limit) {
		matched = matched[:filter.Limit]
	}
	return matched, nil
}

func attachAuth[T any](req *connect.Request[T], wsID, actorID uuid.UUID, handle string) *connect.Request[T] {
	req.Header().Set(connectrpc.HeaderWorkspaceID, wsID.String())
	req.Header().Set(connectrpc.HeaderActorID, actorID.String())
	req.Header().Set(connectrpc.HeaderActorKind, "agent")
	req.Header().Set(connectrpc.HeaderActorHandle, handle)
	return req
}

// TestAllConnectRPCHandlersEndToEnd verifies every ConnectRPC service handler,
// proto mapper, and tenant ownership guard end-to-end.
func TestAllConnectRPCHandlersEndToEnd(t *testing.T) {
	t.Parallel()

	wsID := uuid.New()
	foreignWSID := uuid.New()
	actorID := uuid.New()
	peerActorID := uuid.New()
	auditID := uuid.New()

	fakeAudit := &fakeAuditService{
		record: audit.Record{
			ID:           auditID,
			WorkspaceID:  wsID,
			ActorID:      actorID,
			ActorKind:    authctx.ActorKindAgent,
			ActorHandle:  "agent-alpha",
			Action:       tx.AuditActionCreate,
			ResourceType: "task",
			ResourceID:   uuid.NewString(),
			NewValues:    []byte(`{"title":"Ship handlers"}`),
			OccurredAt:   time.Now().UTC(),
		},
	}

	svcs := &fakeServiceContainer{
		wsSvc:   newFakeWorkspaceService(),
		projSvc: newFakeProjectService(),
		objSvc:  newFakeObjectiveService(),
		taskSvc: newFakeTaskService(),
		exec:    &fakeCodeExecutor{},
		guide:   language.NewGuideProvider(),
		audSvc:  fakeAudit,
	}

	handlers := connectrpc.HandlerSet{
		Workspace: workspacehandler.New(nil, svcs),
		Project:   projecthandler.New(nil, svcs),
		Objective: objectivehandler.New(nil, svcs),
		Task:      taskhandler.New(nil, svcs),
		Language:  languagehandler.New(nil, svcs),
		Audit:     audithandler.New(nil, svcs),
	}

	resolver := &connectrpc.HeaderIdentityResolver{
		PublicProcedures: map[string]bool{
			flowsk_language_pbconnect.LanguageServiceGetLanguageGuideProcedure: true,
			flowsk_workspace_pbconnect.WorkspaceServiceCreateWorkspaceProcedure: true,
		},
	}

	mux := connectrpc.NewMux(
		handlers,
		connect.WithInterceptors(
			connectrpc.NewAuthContextInterceptor(resolver),
			connectrpc.NewValidationInterceptor(nil),
		),
	)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	httpClient := srv.Client()
	baseURL := srv.URL

	wsClient := flowsk_workspace_pbconnect.NewWorkspaceServiceClient(httpClient, baseURL)
	projClient := flowsk_project_pbconnect.NewProjectServiceClient(httpClient, baseURL)
	objClient := flowsk_objective_pbconnect.NewObjectiveServiceClient(httpClient, baseURL)
	taskClient := flowsk_task_pbconnect.NewTaskServiceClient(httpClient, baseURL)
	langClient := flowsk_language_pbconnect.NewLanguageServiceClient(httpClient, baseURL)
	audClient := flowsk_audit_pbconnect.NewAuditServiceClient(httpClient, baseURL)

	ctx := context.Background()

	// 1. WorkspaceService: Create, Get, List, Update, RegisterActor, GetActor, ListActors, cross-workspace & invalid UUID checks.
	createWSResp, err := wsClient.CreateWorkspace(ctx, attachAuth(connect.NewRequest(&flowsk_workspace_pb.CreateWorkspaceRequest{
		Slug:        "acme-eng",
		Name:        "Acme Engineering",
		Description: "Core engineering workspace",
	}), wsID, actorID, "agent-alpha"))
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if createWSResp.Msg.GetWorkspace().GetId() != wsID.String() || createWSResp.Msg.GetWorkspace().GetSlug() != "acme-eng" {
		t.Fatalf("unexpected CreateWorkspace response: %+v", createWSResp.Msg.GetWorkspace())
	}

	// Public bootstrap CreateWorkspace without headers also succeeds cleanly.
	if _, err := wsClient.CreateWorkspace(ctx, connect.NewRequest(&flowsk_workspace_pb.CreateWorkspaceRequest{
		Slug: "public-bootstrap",
		Name: "Public Bootstrap",
	})); err != nil {
		t.Fatalf("public bootstrap CreateWorkspace: %v", err)
	}

	// Bad UUID on GetWorkspace -> CodeInvalidArgument.
	_, err = wsClient.GetWorkspace(ctx, attachAuth(connect.NewRequest(&flowsk_workspace_pb.GetWorkspaceRequest{
		WorkspaceId: "not-a-uuid",
	}), wsID, actorID, "agent-alpha"))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad UUID GetWorkspace code = %v, want %v", connect.CodeOf(err), connect.CodeInvalidArgument)
	}

	// Cross-workspace access -> CodePermissionDenied.
	_, err = wsClient.GetWorkspace(ctx, attachAuth(connect.NewRequest(&flowsk_workspace_pb.GetWorkspaceRequest{
		WorkspaceId: foreignWSID.String(),
	}), wsID, actorID, "agent-alpha"))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("cross-workspace GetWorkspace code = %v, want %v", connect.CodeOf(err), connect.CodePermissionDenied)
	}

	// Unauthenticated GetWorkspace -> CodeUnauthenticated.
	_, err = wsClient.GetWorkspace(ctx, connect.NewRequest(&flowsk_workspace_pb.GetWorkspaceRequest{
		WorkspaceId: wsID.String(),
	}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("unauthenticated GetWorkspace code = %v, want %v", connect.CodeOf(err), connect.CodeUnauthenticated)
	}

	getWSResp, err := wsClient.GetWorkspace(ctx, attachAuth(connect.NewRequest(&flowsk_workspace_pb.GetWorkspaceRequest{
		WorkspaceId: wsID.String(),
	}), wsID, actorID, "agent-alpha"))
	if err != nil || getWSResp.Msg.GetWorkspace().GetName() != "Acme Engineering" {
		t.Fatalf("GetWorkspace: err=%v resp=%+v", err, getWSResp)
	}

	updatedName := "Acme Platform"
	updWSResp, err := wsClient.UpdateWorkspace(ctx, attachAuth(connect.NewRequest(&flowsk_workspace_pb.UpdateWorkspaceRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		Name:        &updatedName,
	}), wsID, actorID, "agent-alpha"))
	if err != nil || updWSResp.Msg.GetWorkspace().GetName() != updatedName {
		t.Fatalf("UpdateWorkspace: err=%v resp=%+v", err, updWSResp)
	}

	listWSResp, err := wsClient.ListWorkspaces(ctx, attachAuth(connect.NewRequest(&flowsk_workspace_pb.ListWorkspacesRequest{
		PageSize: 10,
	}), wsID, actorID, "agent-alpha"))
	if err != nil || len(listWSResp.Msg.GetWorkspaces()) != 1 {
		t.Fatalf("ListWorkspaces: err=%v count=%d", err, len(listWSResp.Msg.GetWorkspaces()))
	}

	actorIDStr := actorID.String()
	regActorResp, err := wsClient.RegisterActor(ctx, attachAuth(connect.NewRequest(&flowsk_workspace_pb.RegisterActorRequest{
		WorkspaceId: wsID.String(),
		ActorId:     &actorIDStr,
		Kind:        flowsk_workspace_pb.ActorKind_ACTOR_KIND_AGENT,
		Handle:      "worker-bee",
		DisplayName: "Worker Bee",
		RoleTitle:   "contributor",
	}), wsID, actorID, "agent-alpha"))
	if err != nil || regActorResp.Msg.GetActor().GetHandle() != "worker-bee" {
		t.Fatalf("RegisterActor: err=%v resp=%+v", err, regActorResp)
	}

	registeredActorID := regActorResp.Msg.GetActor().GetId()
	getActorResp, err := wsClient.GetActor(ctx, attachAuth(connect.NewRequest(&flowsk_workspace_pb.GetActorRequest{
		WorkspaceId:   wsID.String(),
		TargetActorId: registeredActorID,
	}), wsID, actorID, "agent-alpha"))
	if err != nil || getActorResp.Msg.GetActor().GetDisplayName() != "Worker Bee" {
		t.Fatalf("GetActor: err=%v resp=%+v", err, getActorResp)
	}

	listActorsResp, err := wsClient.ListActors(ctx, attachAuth(connect.NewRequest(&flowsk_workspace_pb.ListActorsRequest{
		WorkspaceId: wsID.String(),
		KindFilter:  flowsk_workspace_pb.ActorKind_ACTOR_KIND_AGENT,
	}), wsID, actorID, "agent-alpha"))
	if err != nil || len(listActorsResp.Msg.GetActors()) != 1 {
		t.Fatalf("ListActors: err=%v count=%d", err, len(listActorsResp.Msg.GetActors()))
	}

	// 2. ProjectService: Create, Get, GetBySlug, List, Update, WorkflowStates, Labels, Archive, Delete.
	createProjResp, err := projClient.CreateProject(ctx, attachAuth(connect.NewRequest(&flowsk_project_pb.CreateProjectRequest{
		WorkspaceId:               wsID.String(),
		ActorId:                   actorID.String(),
		KeyPrefix:                 "FLW",
		Slug:                      "flowsk-core",
		Name:                      "Flowsk Core",
		Description:               "Core Flowsk engine",
		SeedDefaultWorkflowStates: true,
	}), wsID, actorID, "agent-alpha"))
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	projectID := uuid.MustParse(createProjResp.Msg.GetProject().GetId())
	if len(createProjResp.Msg.GetWorkflowStates()) < 6 {
		t.Fatalf("expected seeded workflow states, got %d", len(createProjResp.Msg.GetWorkflowStates()))
	}

	if _, err := projClient.GetProject(ctx, attachAuth(connect.NewRequest(&flowsk_project_pb.GetProjectRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		ProjectId:   projectID.String(),
	}), wsID, actorID, "agent-alpha")); err != nil {
		t.Fatalf("GetProject: %v", err)
	}

	slugProjResp, err := projClient.GetProjectBySlug(ctx, attachAuth(connect.NewRequest(&flowsk_project_pb.GetProjectBySlugRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		Slug:        "flowsk-core",
	}), wsID, actorID, "agent-alpha"))
	if err != nil || slugProjResp.Msg.GetProject().GetId() != projectID.String() {
		t.Fatalf("GetProjectBySlug: err=%v resp=%+v", err, slugProjResp)
	}

	statesResp, err := projClient.ListWorkflowStates(ctx, attachAuth(connect.NewRequest(&flowsk_project_pb.ListWorkflowStatesRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		ProjectId:   projectID.String(),
	}), wsID, actorID, "agent-alpha"))
	if err != nil || len(statesResp.Msg.GetWorkflowStates()) < 6 {
		t.Fatalf("ListWorkflowStates: err=%v states=%+v", err, statesResp)
	}
	var doneStateID string
	for _, st := range statesResp.Msg.GetWorkflowStates() {
		if st.GetCategory() == flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_COMPLETED {
			doneStateID = st.GetId()
		}
	}
	if doneStateID == "" {
		t.Fatal("expected COMPLETED canonical workflow state in ListWorkflowStates")
	}

	createdStateResp, err := projClient.CreateWorkflowState(ctx, attachAuth(connect.NewRequest(&flowsk_project_pb.CreateWorkflowStateRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		ProjectId:   projectID.String(),
		Name:        "QA Verification",
		Category:    flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_STARTED,
		ColorHex:    "#22C55E",
		Position:    5,
	}), wsID, actorID, "agent-alpha"))
	if err != nil || createdStateResp.Msg.GetWorkflowState().GetName() != "QA Verification" {
		t.Fatalf("CreateWorkflowState: err=%v resp=%+v", err, createdStateResp)
	}

	renamedState := "Security Review"
	updStateResp, err := projClient.UpdateWorkflowState(ctx, attachAuth(connect.NewRequest(&flowsk_project_pb.UpdateWorkflowStateRequest{
		WorkspaceId:     wsID.String(),
		ActorId:         actorID.String(),
		ProjectId:       projectID.String(),
		WorkflowStateId: createdStateResp.Msg.GetWorkflowState().GetId(),
		Name:            &renamedState,
	}), wsID, actorID, "agent-alpha"))
	if err != nil || updStateResp.Msg.GetWorkflowState().GetName() != renamedState {
		t.Fatalf("UpdateWorkflowState: err=%v resp=%+v", err, updStateResp)
	}

	lblResp, err := projClient.CreateLabel(ctx, attachAuth(connect.NewRequest(&flowsk_project_pb.CreateLabelRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		ProjectId:   projectID.String(),
		Name:        "security",
		ColorHex:    "#EF4444",
	}), wsID, actorID, "agent-alpha"))
	if err != nil || lblResp.Msg.GetLabel().GetName() != "security" {
		t.Fatalf("CreateLabel: err=%v resp=%+v", err, lblResp)
	}

	listLblResp, err := projClient.ListLabels(ctx, attachAuth(connect.NewRequest(&flowsk_project_pb.ListLabelsRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		ProjectId:   projectID.String(),
	}), wsID, actorID, "agent-alpha"))
	if err != nil || len(listLblResp.Msg.GetLabels()) != 1 {
		t.Fatalf("ListLabels: err=%v count=%d", err, len(listLblResp.Msg.GetLabels()))
	}

	if _, err := projClient.DeleteLabel(ctx, attachAuth(connect.NewRequest(&flowsk_project_pb.DeleteLabelRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		ProjectId:   projectID.String(),
		LabelId:     lblResp.Msg.GetLabel().GetId(),
	}), wsID, actorID, "agent-alpha")); err != nil {
		t.Fatalf("DeleteLabel: %v", err)
	}

	// 3. ObjectiveService: Create, Get, List, Update, Close, Delete.
	targetDate := timestamppb.New(time.Now().UTC().Add(7 * 24 * time.Hour))
	createObjResp, err := objClient.CreateObjective(ctx, attachAuth(connect.NewRequest(&flowsk_objective_pb.CreateObjectiveRequest{
		WorkspaceId:     wsID.String(),
		ActorId:         actorID.String(),
		ProjectId:       projectID.String(),
		Kind:            flowsk_objective_pb.ObjectiveKind_OBJECTIVE_KIND_MILESTONE,
		Status:          flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_ACTIVE,
		Title:           "Ship Transport v1",
		Description:     "Deliver ConnectRPC and MCP transports",
		SuccessCriteria: "All tests green\nZero lint issues",
		TargetDate:      targetDate,
	}), wsID, actorID, "agent-alpha"))
	if err != nil || createObjResp.Msg.GetObjective().GetTitle() != "Ship Transport v1" {
		t.Fatalf("CreateObjective: err=%v resp=%+v", err, createObjResp)
	}
	objectiveID := uuid.MustParse(createObjResp.Msg.GetObjective().GetId())

	if _, err := objClient.GetObjective(ctx, attachAuth(connect.NewRequest(&flowsk_objective_pb.GetObjectiveRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		ObjectiveId: objectiveID.String(),
	}), wsID, actorID, "agent-alpha")); err != nil {
		t.Fatalf("GetObjective: %v", err)
	}

	projFilterStr := projectID.String()
	listObjResp, err := objClient.ListObjectives(ctx, attachAuth(connect.NewRequest(&flowsk_objective_pb.ListObjectivesRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		ProjectId:   &projFilterStr,
	}), wsID, actorID, "agent-alpha"))
	if err != nil || len(listObjResp.Msg.GetObjectives()) != 1 {
		t.Fatalf("ListObjectives: err=%v count=%d", err, len(listObjResp.Msg.GetObjectives()))
	}

	newObjTitle := "Ship Transport v1.1"
	if _, err := objClient.UpdateObjective(ctx, attachAuth(connect.NewRequest(&flowsk_objective_pb.UpdateObjectiveRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		ObjectiveId: objectiveID.String(),
		Title:       &newObjTitle,
	}), wsID, actorID, "agent-alpha")); err != nil {
		t.Fatalf("UpdateObjective: %v", err)
	}

	closeObjResp, err := objClient.CloseObjective(ctx, attachAuth(connect.NewRequest(&flowsk_objective_pb.CloseObjectiveRequest{
		WorkspaceId:       wsID.String(),
		ActorId:           actorID.String(),
		ObjectiveId:       objectiveID.String(),
		TerminalStatus:    flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_ACHIEVED,
		ResolutionSummary: "Completed ahead of schedule",
	}), wsID, actorID, "agent-alpha"))
	if err != nil || closeObjResp.Msg.GetObjective().GetStatus() != flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_ACHIEVED {
		t.Fatalf("CloseObjective: err=%v resp=%+v", err, closeObjResp)
	}

	// 4. TaskService: Create, Get, GetByIdentifier, List, Update, Claim + ErrLeaseHeld, Release, Relations, Comments, Queue, Digest, Transition, Delete.
	objIDStr := objectiveID.String()
	createTask1Resp, err := taskClient.CreateTask(ctx, attachAuth(connect.NewRequest(&flowsk_task_pb.CreateTaskRequest{
		WorkspaceId:        wsID.String(),
		ActorId:            actorID.String(),
		ProjectId:          projectID.String(),
		Kind:               flowsk_task_pb.TaskKind_TASK_KIND_FEATURE,
		Priority:           flowsk_task_pb.TaskPriority_TASK_PRIORITY_URGENT,
		Title:              "Implement ConnectRPC handlers",
		Description:        "Wire all 6 services",
		AcceptanceCriteria: "100% test pass",
		ObjectiveId:        &objIDStr,
	}), wsID, actorID, "agent-alpha"))
	if err != nil {
		t.Fatalf("CreateTask 1: %v", err)
	}
	task1ID := uuid.MustParse(createTask1Resp.Msg.GetTask().GetId())

	createTask2Resp, err := taskClient.CreateTask(ctx, attachAuth(connect.NewRequest(&flowsk_task_pb.CreateTaskRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		ProjectId:   projectID.String(),
		Kind:        flowsk_task_pb.TaskKind_TASK_KIND_TASK,
		Priority:    flowsk_task_pb.TaskPriority_TASK_PRIORITY_HIGH,
		Title:       "Implement MCP server",
	}), wsID, actorID, "agent-alpha"))
	if err != nil {
		t.Fatalf("CreateTask 2: %v", err)
	}
	task2ID := uuid.MustParse(createTask2Resp.Msg.GetTask().GetId())

	byKeyResp, err := taskClient.GetTaskByIdentifier(ctx, attachAuth(connect.NewRequest(&flowsk_task_pb.GetTaskByIdentifierRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		Identifier:  "FLW-1",
	}), wsID, actorID, "agent-alpha"))
	if err != nil || byKeyResp.Msg.GetTask().GetId() != task1ID.String() {
		t.Fatalf("GetTaskByIdentifier: err=%v resp=%+v", err, byKeyResp)
	}

	listTasksResp, err := taskClient.ListTasks(ctx, attachAuth(connect.NewRequest(&flowsk_task_pb.ListTasksRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		ProjectId:   &projFilterStr,
	}), wsID, actorID, "agent-alpha"))
	if err != nil || len(listTasksResp.Msg.GetTasks()) != 2 {
		t.Fatalf("ListTasks: err=%v count=%d", err, len(listTasksResp.Msg.GetTasks()))
	}

	// AddTaskRelation self-loop -> CodeInvalidArgument.
	_, err = taskClient.AddTaskRelation(ctx, attachAuth(connect.NewRequest(&flowsk_task_pb.AddTaskRelationRequest{
		WorkspaceId:  wsID.String(),
		ActorId:      actorID.String(),
		SourceTaskId: task1ID.String(),
		TargetTaskId: task1ID.String(),
		RelationType: flowsk_task_pb.TaskRelationType_TASK_RELATION_TYPE_BLOCKS,
	}), wsID, actorID, "agent-alpha"))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("self-loop AddTaskRelation code = %v, want %v", connect.CodeOf(err), connect.CodeInvalidArgument)
	}

	relResp, err := taskClient.AddTaskRelation(ctx, attachAuth(connect.NewRequest(&flowsk_task_pb.AddTaskRelationRequest{
		WorkspaceId:  wsID.String(),
		ActorId:      actorID.String(),
		SourceTaskId: task1ID.String(),
		TargetTaskId: task2ID.String(),
		RelationType: flowsk_task_pb.TaskRelationType_TASK_RELATION_TYPE_BLOCKS,
	}), wsID, actorID, "agent-alpha"))
	if err != nil || relResp.Msg.GetRelation().GetRelationType() != flowsk_task_pb.TaskRelationType_TASK_RELATION_TYPE_BLOCKS {
		t.Fatalf("AddTaskRelation: err=%v resp=%+v", err, relResp)
	}

	getTaskResp, err := taskClient.GetTask(ctx, attachAuth(connect.NewRequest(&flowsk_task_pb.GetTaskRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		TaskId:      task1ID.String(),
	}), wsID, actorID, "agent-alpha"))
	if err != nil || len(getTaskResp.Msg.GetRelations()) != 1 {
		t.Fatalf("GetTask with relations: err=%v rels=%d", err, len(getTaskResp.Msg.GetRelations()))
	}

	// ClaimTask by actorID succeeds; competing ClaimTask by peerActorID fails with CodeFailedPrecondition!
	claimResp, err := taskClient.ClaimTask(ctx, attachAuth(connect.NewRequest(&flowsk_task_pb.ClaimTaskRequest{
		WorkspaceId:     wsID.String(),
		ActorId:         actorID.String(),
		TaskId:          task1ID.String(),
		LeaseTtlSeconds: 1800,
	}), wsID, actorID, "agent-alpha"))
	if err != nil || claimResp.Msg.GetTask().GetClaimedByActorId() != actorID.String() {
		t.Fatalf("ClaimTask: err=%v resp=%+v", err, claimResp)
	}

	_, err = taskClient.ClaimTask(ctx, attachAuth(connect.NewRequest(&flowsk_task_pb.ClaimTaskRequest{
		WorkspaceId:     wsID.String(),
		ActorId:         peerActorID.String(),
		TaskId:          task1ID.String(),
		LeaseTtlSeconds: 1800,
	}), wsID, peerActorID, "agent-beta"))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("competing ClaimTask code = %v, want %v (err=%v)", connect.CodeOf(err), connect.CodeFailedPrecondition, err)
	}

	queueResp, err := taskClient.GetAgentWorkQueue(ctx, attachAuth(connect.NewRequest(&flowsk_task_pb.GetAgentWorkQueueRequest{
		WorkspaceId:            wsID.String(),
		ActorId:                actorID.String(),
		ProjectId:              &projFilterStr,
		MaxItems:               10,
		IncludeClaimedByCaller: true,
	}), wsID, actorID, "agent-alpha"))
	if err != nil || len(queueResp.Msg.GetReadyTasks()) != 1 || len(queueResp.Msg.GetClaimedByCaller()) != 1 {
		t.Fatalf("GetAgentWorkQueue: err=%v ready=%d claimed=%d", err, len(queueResp.Msg.GetReadyTasks()), len(queueResp.Msg.GetClaimedByCaller()))
	}

	digestResp, err := taskClient.GetProjectDigest(ctx, attachAuth(connect.NewRequest(&flowsk_task_pb.GetProjectDigestRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		ProjectId:   projectID.String(),
	}), wsID, actorID, "agent-alpha"))
	if err != nil || digestResp.Msg.GetDigest().GetTotalTasks() != 2 {
		t.Fatalf("GetProjectDigest: err=%v digest=%+v", err, digestResp)
	}

	commentResp, err := taskClient.AddTaskComment(ctx, attachAuth(connect.NewRequest(&flowsk_task_pb.AddTaskCommentRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		TaskId:      task1ID.String(),
		Kind:        flowsk_task_pb.TaskCommentKind_TASK_COMMENT_KIND_HANDOFF,
		Body:        "Handlers verified with race detector.",
	}), wsID, actorID, "agent-alpha"))
	if err != nil || commentResp.Msg.GetComment().GetKind() != flowsk_task_pb.TaskCommentKind_TASK_COMMENT_KIND_HANDOFF {
		t.Fatalf("AddTaskComment: err=%v resp=%+v", err, commentResp)
	}

	listCommentsResp, err := taskClient.ListTaskComments(ctx, attachAuth(connect.NewRequest(&flowsk_task_pb.ListTaskCommentsRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		TaskId:      task1ID.String(),
	}), wsID, actorID, "agent-alpha"))
	if err != nil || len(listCommentsResp.Msg.GetComments()) != 1 {
		t.Fatalf("ListTaskComments: err=%v count=%d", err, len(listCommentsResp.Msg.GetComments()))
	}

	transResp, err := taskClient.TransitionTaskState(ctx, attachAuth(connect.NewRequest(&flowsk_task_pb.TransitionTaskStateRequest{
		WorkspaceId:     wsID.String(),
		ActorId:         actorID.String(),
		TaskId:          task1ID.String(),
		WorkflowStateId: doneStateID,
		ResolutionNote:  "Shipped",
		ReleaseClaim:    true,
	}), wsID, actorID, "agent-alpha"))
	if err != nil || transResp.Msg.GetTask().GetStateCategory() != flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_COMPLETED {
		t.Fatalf("TransitionTaskState: err=%v resp=%+v", err, transResp)
	}

	if _, err := taskClient.RemoveTaskRelation(ctx, attachAuth(connect.NewRequest(&flowsk_task_pb.RemoveTaskRelationRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		RelationId:  relResp.Msg.GetRelation().GetId(),
	}), wsID, actorID, "agent-alpha")); err != nil {
		t.Fatalf("RemoveTaskRelation: %v", err)
	}

	if _, err := taskClient.DeleteTask(ctx, attachAuth(connect.NewRequest(&flowsk_task_pb.DeleteTaskRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		TaskId:      task2ID.String(),
	}), wsID, actorID, "agent-alpha")); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}

	// 5. LanguageService: GetLanguageGuide (public & auth), ListLanguageExamples, ValidateScript, ExecuteScript (Code Mode JS).
	guideResp, err := langClient.GetLanguageGuide(ctx, connect.NewRequest(&flowsk_language_pb.GetLanguageGuideRequest{
		Density: flowsk_language_pb.OutputDensity_OUTPUT_DENSITY_STANDARD,
	}))
	if err != nil || !strings.Contains(guideResp.Msg.GetMarkdownSpec(), "Cloudflare Code Mode") {
		t.Fatalf("GetLanguageGuide: err=%v specLen=%d", err, len(guideResp.Msg.GetMarkdownSpec()))
	}

	examplesResp, err := langClient.ListLanguageExamples(ctx, attachAuth(connect.NewRequest(&flowsk_language_pb.ListLanguageExamplesRequest{}), wsID, actorID, "agent-alpha"))
	if err != nil || len(examplesResp.Msg.GetExamples()) == 0 {
		t.Fatalf("ListLanguageExamples: err=%v count=%d", err, len(examplesResp.Msg.GetExamples()))
	}

	valOKResp, err := langClient.ValidateScript(ctx, attachAuth(connect.NewRequest(&flowsk_language_pb.ValidateScriptRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		Script:      `const t = flowsk.tasks.create({ title: "Valid JS" }); return t.key;`,
	}), wsID, actorID, "agent-alpha"))
	if err != nil || !valOKResp.Msg.GetValid() || valOKResp.Msg.GetStatementCount() != 1 {
		t.Fatalf("ValidateScript (valid): err=%v resp=%+v", err, valOKResp)
	}

	valBadResp, err := langClient.ValidateScript(ctx, attachAuth(connect.NewRequest(&flowsk_language_pb.ValidateScriptRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		Script:      "const = ;",
	}), wsID, actorID, "agent-alpha"))
	if err != nil || valBadResp.Msg.GetValid() || len(valBadResp.Msg.GetDiagnostics()) == 0 {
		t.Fatalf("ValidateScript (invalid): err=%v resp=%+v", err, valBadResp)
	}

	execResp, err := langClient.ExecuteScript(ctx, attachAuth(connect.NewRequest(&flowsk_language_pb.ExecuteScriptRequest{
		WorkspaceId:      wsID.String(),
		ActorId:          actorID.String(),
		DefaultProjectId: &projFilterStr,
		Script:           `const t1 = flowsk.tasks.create({ title: "Batch task" }); return t1;`,
		Mode:             flowsk_language_pb.ExecutionMode_EXECUTION_MODE_ATOMIC_COMMIT,
		OutputDensity:    flowsk_language_pb.OutputDensity_OUTPUT_DENSITY_MINIMAL_TOKENS,
	}), wsID, actorID, "agent-alpha"))
	if err != nil || !execResp.Msg.GetCommitted() || !strings.Contains(execResp.Msg.GetCompactOutput(), "FLW-99") {
		t.Fatalf("ExecuteScript: err=%v resp=%+v", err, execResp)
	}

	// 6. AuditService: ListAuditEvents and GetAuditEvent.
	listAudResp, err := audClient.ListAuditEvents(ctx, attachAuth(connect.NewRequest(&flowsk_audit_pb.ListAuditEventsRequest{
		WorkspaceId:  wsID.String(),
		ActorId:      actorID.String(),
		ActionFilter: flowsk_audit_pb.AuditAction_AUDIT_ACTION_CREATE,
		PageSize:     20,
	}), wsID, actorID, "agent-alpha"))
	if err != nil || len(listAudResp.Msg.GetEvents()) != 1 {
		t.Fatalf("ListAuditEvents: err=%v count=%d", err, len(listAudResp.Msg.GetEvents()))
	}

	getAudResp, err := audClient.GetAuditEvent(ctx, attachAuth(connect.NewRequest(&flowsk_audit_pb.GetAuditEventRequest{
		WorkspaceId:  wsID.String(),
		ActorId:      actorID.String(),
		AuditEventId: auditID.String(),
	}), wsID, actorID, "agent-alpha"))
	if err != nil || getAudResp.Msg.GetEvent().GetId() != auditID.String() {
		t.Fatalf("GetAuditEvent: err=%v resp=%+v", err, getAudResp)
	}

	// Archive & Delete cleanup paths.
	archProjResp, err := projClient.ArchiveProject(ctx, attachAuth(connect.NewRequest(&flowsk_project_pb.ArchiveProjectRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		ProjectId:   projectID.String(),
	}), wsID, actorID, "agent-alpha"))
	if err != nil || archProjResp.Msg.GetProject().GetStatus() != flowsk_project_pb.ProjectStatus_PROJECT_STATUS_ARCHIVED {
		t.Fatalf("ArchiveProject: err=%v resp=%+v", err, archProjResp)
	}

	if _, err := objClient.DeleteObjective(ctx, attachAuth(connect.NewRequest(&flowsk_objective_pb.DeleteObjectiveRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		ObjectiveId: objectiveID.String(),
	}), wsID, actorID, "agent-alpha")); err != nil {
		t.Fatalf("DeleteObjective: %v", err)
	}

	if _, err := projClient.DeleteProject(ctx, attachAuth(connect.NewRequest(&flowsk_project_pb.DeleteProjectRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		ProjectId:   projectID.String(),
	}), wsID, actorID, "agent-alpha")); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}

	if _, err := wsClient.DeleteWorkspace(ctx, attachAuth(connect.NewRequest(&flowsk_workspace_pb.DeleteWorkspaceRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
	}), wsID, actorID, "agent-alpha")); err != nil {
		t.Fatalf("DeleteWorkspace: %v", err)
	}
}

func TestHandlersHardening_TagsPaginationFiltersAndBindingsOwnership(t *testing.T) {
	t.Parallel()

	wsID := uuid.New()
	foreignWSID := uuid.New()
	actorID := uuid.New()

	fakeTasks := newFakeTaskService()
	fakeAudit := &fakeAuditService{
		records: []audit.Record{
			{ID: uuid.New(), WorkspaceID: wsID, ActorID: actorID, ActorKind: authctx.ActorKindAgent, ActorHandle: "a1", Action: tx.AuditActionCreate, ResourceType: "task", ResourceID: "1", OccurredAt: time.Now().UTC()},
			{ID: uuid.New(), WorkspaceID: wsID, ActorID: actorID, ActorKind: authctx.ActorKindAgent, ActorHandle: "a1", Action: tx.AuditActionUpdate, ResourceType: "task", ResourceID: "2", OccurredAt: time.Now().UTC()},
			{ID: uuid.New(), WorkspaceID: wsID, ActorID: actorID, ActorKind: authctx.ActorKindAgent, ActorHandle: "a1", Action: tx.AuditActionTransition, ResourceType: "task", ResourceID: "3", OccurredAt: time.Now().UTC()},
		},
	}

	svcs := &fakeServiceContainer{
		wsSvc:   newFakeWorkspaceService(),
		projSvc: newFakeProjectService(),
		objSvc:  newFakeObjectiveService(),
		taskSvc: fakeTasks,
		exec:    &fakeCodeExecutor{},
		guide:   language.NewGuideProvider(),
		audSvc:  fakeAudit,
	}

	mux := connectrpc.NewMux(
		connectrpc.HandlerSet{
			Workspace: workspacehandler.New(nil, svcs),
			Project:   projecthandler.New(nil, svcs),
			Objective: objectivehandler.New(nil, svcs),
			Task:      taskhandler.New(nil, svcs),
			Language:  languagehandler.New(nil, svcs),
			Audit:     audithandler.New(nil, svcs),
		},
		connect.WithInterceptors(connectrpc.NewAuthContextInterceptor(nil)),
	)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	projClient := flowsk_project_pbconnect.NewProjectServiceClient(srv.Client(), srv.URL)
	objClient := flowsk_objective_pbconnect.NewObjectiveServiceClient(srv.Client(), srv.URL)
	taskClient := flowsk_task_pbconnect.NewTaskServiceClient(srv.Client(), srv.URL)
	langClient := flowsk_language_pbconnect.NewLanguageServiceClient(srv.Client(), srv.URL)
	audClient := flowsk_audit_pbconnect.NewAuditServiceClient(srv.Client(), srv.URL)

	ctx := context.Background()

	// 1. Project bindings require workspace ownership and ListProjects filters archived BEFORE pagination.
	_, err := projClient.BindSubproject(ctx, attachAuth(connect.NewRequest(&flowsk_project_pb.BindSubprojectRequest{
		WorkspaceId:     foreignWSID.String(),
		ActorId:         actorID.String(),
		ParentProjectId: uuid.NewString(),
		ChildProjectId:  uuid.NewString(),
	}), wsID, actorID, "agent"))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("foreign BindSubproject code = %v, want PermissionDenied", connect.CodeOf(err))
	}
	_, err = projClient.UnbindSubproject(ctx, attachAuth(connect.NewRequest(&flowsk_project_pb.UnbindSubprojectRequest{
		WorkspaceId:     foreignWSID.String(),
		ActorId:         actorID.String(),
		ParentProjectId: uuid.NewString(),
		ChildProjectId:  uuid.NewString(),
	}), wsID, actorID, "agent"))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("foreign UnbindSubproject code = %v, want PermissionDenied", connect.CodeOf(err))
	}
	_, err = projClient.ListProjectBindings(ctx, attachAuth(connect.NewRequest(&flowsk_project_pb.ListProjectBindingsRequest{
		WorkspaceId: foreignWSID.String(),
		ActorId:     actorID.String(),
	}), wsID, actorID, "agent"))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("foreign ListProjectBindings code = %v, want PermissionDenied", connect.CodeOf(err))
	}

	// Create 2 archived projects sorting lexicographically first ("a-arch-1", "a-arch-2") + 3 active projects.
	for i, spec := range []struct {
		slug     string
		archived bool
	}{
		{"a-arch-1", true},
		{"a-arch-2", true},
		{"b-active-1", false},
		{"b-active-2", false},
		{"b-active-3", false},
	} {
		st := flowsk_project_pb.ProjectStatus_PROJECT_STATUS_ACTIVE
		if spec.archived {
			st = flowsk_project_pb.ProjectStatus_PROJECT_STATUS_ARCHIVED
		}
		if _, err := projClient.CreateProject(ctx, attachAuth(connect.NewRequest(&flowsk_project_pb.CreateProjectRequest{
			WorkspaceId: wsID.String(),
			ActorId:     actorID.String(),
			KeyPrefix:   fmt.Sprintf("P%d", i),
			Slug:        spec.slug,
			Name:        spec.slug,
			Status:      st,
		}), wsID, actorID, "agent")); err != nil {
			t.Fatalf("CreateProject %s: %v", spec.slug, err)
		}
	}

	projPage1, err := projClient.ListProjects(ctx, attachAuth(connect.NewRequest(&flowsk_project_pb.ListProjectsRequest{
		WorkspaceId:     wsID.String(),
		ActorId:         actorID.String(),
		IncludeArchived: false,
		PageSize:        2,
	}), wsID, actorID, "agent"))
	if err != nil {
		t.Fatalf("ListProjects page 1: %v", err)
	}
	if len(projPage1.Msg.GetProjects()) != 2 || projPage1.Msg.GetNextPageToken() != "2" {
		t.Fatalf("ListProjects page 1 got %d items token=%q, want 2 items token=2", len(projPage1.Msg.GetProjects()), projPage1.Msg.GetNextPageToken())
	}
	projPage2, err := projClient.ListProjects(ctx, attachAuth(connect.NewRequest(&flowsk_project_pb.ListProjectsRequest{
		WorkspaceId:     wsID.String(),
		ActorId:         actorID.String(),
		IncludeArchived: false,
		PageSize:        2,
		PageToken:       projPage1.Msg.GetNextPageToken(),
	}), wsID, actorID, "agent"))
	if err != nil || len(projPage2.Msg.GetProjects()) != 1 || projPage2.Msg.GetNextPageToken() != "" {
		t.Fatalf("ListProjects page 2: err=%v count=%d token=%q", err, len(projPage2.Msg.GetProjects()), projPage2.Msg.GetNextPageToken())
	}

	mainProjectID := uuid.MustParse(projPage1.Msg.GetProjects()[0].GetId())

	// 2. Objectives: ClearTargetDate & ListObjectives NextPageToken.
	due := timestamppb.New(time.Now().UTC().Add(48 * time.Hour))
	var firstObjID string
	for i := 0; i < 3; i++ {
		resp, err := objClient.CreateObjective(ctx, attachAuth(connect.NewRequest(&flowsk_objective_pb.CreateObjectiveRequest{
			WorkspaceId: wsID.String(),
			ActorId:     actorID.String(),
			ProjectId:   mainProjectID.String(),
			Kind:        flowsk_objective_pb.ObjectiveKind_OBJECTIVE_KIND_GOAL,
			Title:       fmt.Sprintf("Goal %d", i),
			TargetDate:  due,
		}), wsID, actorID, "agent"))
		if err != nil {
			t.Fatalf("CreateObjective %d: %v", i, err)
		}
		if i == 0 {
			firstObjID = resp.Msg.GetObjective().GetId()
		}
	}

	updObjResp, err := objClient.UpdateObjective(ctx, attachAuth(connect.NewRequest(&flowsk_objective_pb.UpdateObjectiveRequest{
		WorkspaceId:     wsID.String(),
		ActorId:         actorID.String(),
		ObjectiveId:     firstObjID,
		ClearTargetDate: true,
	}), wsID, actorID, "agent"))
	if err != nil || updObjResp.Msg.GetObjective().GetTargetDate() != nil {
		t.Fatalf("ClearTargetDate failed: err=%v targetDate=%v", err, updObjResp.Msg.GetObjective().GetTargetDate())
	}

	objPage1, err := objClient.ListObjectives(ctx, attachAuth(connect.NewRequest(&flowsk_objective_pb.ListObjectivesRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		PageSize:    2,
	}), wsID, actorID, "agent"))
	if err != nil || len(objPage1.Msg.GetObjectives()) != 2 || objPage1.Msg.GetNextPageToken() != "2" {
		t.Fatalf("ListObjectives page 1: err=%v count=%d token=%q", err, len(objPage1.Msg.GetObjectives()), objPage1.Msg.GetNextPageToken())
	}

	// 3. Tasks: Tags + LabelIds separation, workflow_state_id filter, >200 pagination, ReleaseTaskClaim Force.
	labelUUID := uuid.NewString()
	doneStateID := connectrpc.WorkflowStateIDForStatus(mainProjectID, task.StatusDone).String()
	todoStateID := connectrpc.WorkflowStateIDForStatus(mainProjectID, task.StatusReady).String()

	createdTaskResp, err := taskClient.CreateTask(ctx, attachAuth(connect.NewRequest(&flowsk_task_pb.CreateTaskRequest{
		WorkspaceId:     wsID.String(),
		ActorId:         actorID.String(),
		ProjectId:       mainProjectID.String(),
		Title:           "Tagged task",
		WorkflowStateId: &doneStateID,
		Tags:            []string{"security", "backend"},
		LabelIds:        []string{labelUUID},
	}), wsID, actorID, "agent"))
	if err != nil {
		t.Fatalf("CreateTask with tags: %v", err)
	}
	gotTask := createdTaskResp.Msg.GetTask()
	if len(gotTask.GetTags()) != 3 || len(gotTask.GetLabelIds()) != 1 || gotTask.GetLabelIds()[0] != labelUUID {
		t.Fatalf("unexpected tags/label_ids split: tags=%v label_ids=%v", gotTask.GetTags(), gotTask.GetLabelIds())
	}

	updTaskResp, err := taskClient.UpdateTask(ctx, attachAuth(connect.NewRequest(&flowsk_task_pb.UpdateTaskRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		TaskId:      gotTask.GetId(),
		Tags:        []string{"hardened"},
		ReplaceTags: true,
	}), wsID, actorID, "agent"))
	if err != nil || len(updTaskResp.Msg.GetTask().GetTags()) != 1 || len(updTaskResp.Msg.GetTask().GetLabelIds()) != 0 {
		t.Fatalf("UpdateTask ReplaceTags: err=%v task=%+v", err, updTaskResp.Msg.GetTask())
	}

	// Create 205 additional Todo tasks so total tasks > 200.
	for i := 0; i < 205; i++ {
		if _, err := taskClient.CreateTask(ctx, attachAuth(connect.NewRequest(&flowsk_task_pb.CreateTaskRequest{
			WorkspaceId:     wsID.String(),
			ActorId:         actorID.String(),
			ProjectId:       mainProjectID.String(),
			Title:           fmt.Sprintf("Todo %03d", i),
			WorkflowStateId: &todoStateID,
		}), wsID, actorID, "agent")); err != nil {
			t.Fatalf("CreateTask bulk %d: %v", i, err)
		}
	}

	projIDStr := mainProjectID.String()
	doneTasksResp, err := taskClient.ListTasks(ctx, attachAuth(connect.NewRequest(&flowsk_task_pb.ListTasksRequest{
		WorkspaceId:     wsID.String(),
		ActorId:         actorID.String(),
		ProjectId:       &projIDStr,
		WorkflowStateId: &doneStateID,
	}), wsID, actorID, "agent"))
	if err != nil || len(doneTasksResp.Msg.GetTasks()) != 1 || doneTasksResp.Msg.GetTasks()[0].GetId() != gotTask.GetId() {
		t.Fatalf("ListTasks by workflow_state_id(done): err=%v count=%d", err, len(doneTasksResp.Msg.GetTasks()))
	}

	pageBeyond200, err := taskClient.ListTasks(ctx, attachAuth(connect.NewRequest(&flowsk_task_pb.ListTasksRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		ProjectId:   &projIDStr,
		PageSize:    200,
		PageToken:   "200",
	}), wsID, actorID, "agent"))
	if err != nil || len(pageBeyond200.Msg.GetTasks()) != 6 {
		t.Fatalf("ListTasks page beyond 200: err=%v count=%d, want 6", err, len(pageBeyond200.Msg.GetTasks()))
	}

	if _, err := taskClient.ReleaseTaskClaim(ctx, attachAuth(connect.NewRequest(&flowsk_task_pb.ReleaseTaskClaimRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		TaskId:      gotTask.GetId(),
		Force:       true,
	}), wsID, actorID, "agent")); err != nil {
		t.Fatalf("ReleaseTaskClaim(Force=true): %v", err)
	}
	fakeTasks.mu.RLock()
	gotForce := fakeTasks.lastReleaseTask.Force
	fakeTasks.mu.RUnlock()
	if !gotForce {
		t.Fatal("expected ReleaseTaskClaim Force=true to propagate to ReleaseTaskInput.Force")
	}

	// 4. LanguageService: 0-mutation script reports StatementsExecuted == 0.
	readOnlyExec, err := langClient.ExecuteScript(ctx, attachAuth(connect.NewRequest(&flowsk_language_pb.ExecuteScriptRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		Script:      "return flowsk.board();",
	}), wsID, actorID, "agent"))
	if err != nil || readOnlyExec.Msg.GetStatementsExecuted() != 0 {
		t.Fatalf("expected StatementsExecuted=0 for read-only script, got err=%v count=%d", err, readOnlyExec.Msg.GetStatementsExecuted())
	}

	// 5. AuditService: ListAuditEvents populates NextPageToken.
	audPage1, err := audClient.ListAuditEvents(ctx, attachAuth(connect.NewRequest(&flowsk_audit_pb.ListAuditEventsRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		PageSize:    2,
	}), wsID, actorID, "agent"))
	if err != nil || len(audPage1.Msg.GetEvents()) != 2 || audPage1.Msg.GetNextPageToken() != "2" {
		t.Fatalf("ListAuditEvents page 1: err=%v count=%d token=%q", err, len(audPage1.Msg.GetEvents()), audPage1.Msg.GetNextPageToken())
	}
	audPage2, err := audClient.ListAuditEvents(ctx, attachAuth(connect.NewRequest(&flowsk_audit_pb.ListAuditEventsRequest{
		WorkspaceId: wsID.String(),
		ActorId:     actorID.String(),
		PageSize:    2,
		PageToken:   audPage1.Msg.GetNextPageToken(),
	}), wsID, actorID, "agent"))
	if err != nil || len(audPage2.Msg.GetEvents()) != 1 || audPage2.Msg.GetNextPageToken() != "" {
		t.Fatalf("ListAuditEvents page 2: err=%v count=%d token=%q", err, len(audPage2.Msg.GetEvents()), audPage2.Msg.GetNextPageToken())
	}
}
