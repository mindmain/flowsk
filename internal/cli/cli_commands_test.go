// File cli_commands_test.go exercises every Flowsk CLI command group end-to-end
// against an in-memory mock implementation of cli.Dependencies, verifying human,
// compact, JSON, TSV, JSONL, and Markdown output encodings, client-only local
// path isolation, flag validation, and error handling.
package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mindmain/flowsk/internal/cli"
	"github.com/mindmain/flowsk/internal/transport/mcp"
)

// inMemoryEnv implements cli.Dependencies completely in memory so every CLI
// command group can be tested deterministically without external databases.
type inMemoryEnv struct {
	mu sync.Mutex

	workspaces map[string]cli.WorkspaceView
	actors     map[string][]cli.ActorView
	projects   map[string]cli.ProjectView
	states     map[string][]cli.WorkflowStateView
	labels     map[string][]cli.LabelView
	bindings   map[string]cli.LocalProjectBinding
	objectives map[string]cli.ObjectiveView
	tasks      map[string]*cli.TaskView
	taskOrder  []string
	audits     []cli.AuditEventView
	projDocs   map[string]string
	taskDocs   map[string]string
	orphanCnt  int

	lastServeCfg      cli.ServeConfig
	lastMCPCfg        mcp.Config
	lastExecScriptIn  cli.ExecuteScriptInput
	lastMigrateUpIn   cli.MigrateUpInput
	lastMigrateDownIn cli.MigrateDownInput
	lastPingDriver       string
	lastPingDSN          string
	pingErr              error
	allMigrationsApplied bool

	customFormatter cli.OutputFormatter
	projectPathLeak bool
	now             time.Time
	seqByPrefix     map[string]int
}

func newInMemoryEnv() *inMemoryEnv {
	fixedNow := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	env := &inMemoryEnv{
		workspaces:  make(map[string]cli.WorkspaceView),
		actors:      make(map[string][]cli.ActorView),
		projects:    make(map[string]cli.ProjectView),
		states:      make(map[string][]cli.WorkflowStateView),
		labels:      make(map[string][]cli.LabelView),
		bindings:    make(map[string]cli.LocalProjectBinding),
		objectives:  make(map[string]cli.ObjectiveView),
		tasks:       make(map[string]*cli.TaskView),
		projDocs:    make(map[string]string),
		taskDocs:    make(map[string]string),
		now:         fixedNow,
		seqByPrefix: make(map[string]int),
	}
	env.workspaces["default"] = cli.WorkspaceView{
		ID:                     "ws-default",
		Slug:                   "default",
		Name:                   "Default Workspace",
		DefaultLeaseTTLSeconds: 2700,
		CreatedAt:              fixedNow,
		UpdatedAt:              fixedNow,
	}
	return env
}

func (e *inMemoryEnv) ServerRunner() cli.ServerRunner           { return e }
func (e *inMemoryEnv) MCPRunner() cli.MCPRunner                 { return e }
func (e *inMemoryEnv) WorkspaceClient() cli.WorkspaceClient     { return e }
func (e *inMemoryEnv) ProjectClient() cli.ProjectClient         { return e }
func (e *inMemoryEnv) LocalPathRegistry() cli.LocalPathRegistry { return e }
func (e *inMemoryEnv) ObjectiveClient() cli.ObjectiveClient     { return e }
func (e *inMemoryEnv) TaskClient() cli.TaskClient               { return e }
func (e *inMemoryEnv) LanguageClient() cli.LanguageClient       { return e }
func (e *inMemoryEnv) AuditClient() cli.AuditClient             { return e }
func (e *inMemoryEnv) DatabaseAdmin() cli.DatabaseAdmin         { return e }
func (e *inMemoryEnv) OutputFormatter() cli.OutputFormatter     { return e.customFormatter }

func (e *inMemoryEnv) nextID(prefix string) string {
	e.seqByPrefix[prefix]++
	return fmt.Sprintf("%s-%d", prefix, e.seqByPrefix[prefix])
}

func (e *inMemoryEnv) recordAudit(ws, proj, actor, action, entityType, entityID, summary string) {
	e.audits = append(e.audits, cli.AuditEventView{
		ID:          e.nextID("evt"),
		WorkspaceID: ws,
		ProjectID:   proj,
		Actor:       actor,
		Action:      action,
		EntityType:  entityType,
		EntityID:    entityID,
		Summary:     summary,
		OccurredAt:  e.now,
	})
}

func (e *inMemoryEnv) checkNoHostPath(val string) {
	if strings.Contains(val, string(filepath.Separator)) {
		e.projectPathLeak = true
	}
}

// RunServer implements cli.ServerRunner.
func (e *inMemoryEnv) RunServer(_ context.Context, cfg cli.ServeConfig) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastServeCfg = cfg
	return nil
}

type mockMCPServer struct {
	cfg mcp.Config
}

func (m mockMCPServer) Serve(ctx context.Context) error {
	return ctx.Err()
}

func (m mockMCPServer) RegisterTools() []mcp.ToolSpec         { return mcp.CanonicalTools() }
func (m mockMCPServer) RegisterResources() []mcp.ResourceSpec { return mcp.CanonicalResources() }

// NewMCPServer implements cli.MCPRunner.
func (e *inMemoryEnv) NewMCPServer(cfg mcp.Config) mcp.Server {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastMCPCfg = cfg
	return mockMCPServer{cfg: cfg}
}

// WorkspaceClient implementation.
func (e *inMemoryEnv) CreateWorkspace(_ context.Context, in cli.CreateWorkspaceInput) (cli.WorkspaceView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ws := cli.WorkspaceView{
		ID:                     e.nextID("ws"),
		Slug:                   in.Slug,
		Name:                   in.Name,
		Description:            in.Description,
		DefaultLeaseTTLSeconds: 2700,
		CreatedAt:              e.now,
		UpdatedAt:              e.now,
	}
	e.workspaces[in.Slug] = ws
	e.recordAudit(in.Slug, "", in.Actor, "workspace.created", "workspace", ws.Slug, ws.Name)
	return ws, nil
}

func (e *inMemoryEnv) ListWorkspaces(_ context.Context, _ cli.ListWorkspacesInput) (cli.WorkspaceListResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var items []cli.WorkspaceView
	for _, ws := range e.workspaces {
		items = append(items, ws)
	}
	slices.SortFunc(items, func(a, b cli.WorkspaceView) int {
		return strings.Compare(a.Slug, b.Slug)
	})
	return cli.WorkspaceListResult{Items: items}, nil
}

func (e *inMemoryEnv) GetWorkspace(_ context.Context, ref string) (cli.WorkspaceView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ws, ok := e.workspaces[ref]
	if !ok {
		return cli.WorkspaceView{}, fmt.Errorf("workspace %q not found", ref)
	}
	return ws, nil
}

func (e *inMemoryEnv) RegisterActor(_ context.Context, in cli.RegisterActorInput) (cli.ActorView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	actor := cli.ActorView{
		ID:          e.nextID("act"),
		WorkspaceID: in.Workspace,
		Handle:      in.Handle,
		DisplayName: in.DisplayName,
		Kind:        in.Kind,
		Model:       in.Model,
		Role:        in.Role,
		CreatedAt:   e.now,
	}
	e.actors[in.Workspace] = append(e.actors[in.Workspace], actor)
	e.recordAudit(in.Workspace, "", in.Handle, "actor.registered", "actor", actor.Handle, actor.Kind)
	return actor, nil
}

func (e *inMemoryEnv) ListActors(_ context.Context, in cli.ListActorsInput) (cli.ActorListResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var items []cli.ActorView
	for _, a := range e.actors[in.Workspace] {
		if in.Kind != "" && a.Kind != in.Kind {
			continue
		}
		items = append(items, a)
	}
	return cli.ActorListResult{Items: items}, nil
}

// ProjectClient implementation.
func (e *inMemoryEnv) projMapKey(ws, proj string) string {
	return ws + ":" + strings.ToUpper(proj)
}

func (e *inMemoryEnv) CreateProject(_ context.Context, in cli.CreateProjectInput) (cli.ProjectView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.checkNoHostPath(in.Key)
	key := strings.ToUpper(in.Key)
	pv := cli.ProjectView{
		ID:          e.nextID("proj"),
		WorkspaceID: in.Workspace,
		Slug:        strings.ToLower(key),
		Key:         key,
		Name:        in.Name,
		Description: in.Description,
		Status:      "active",
		CreatedAt:   e.now,
		UpdatedAt:   e.now,
	}
	mk := e.projMapKey(in.Workspace, key)
	e.projects[mk] = pv
	if in.DefaultWorkflow {
		e.states[mk] = []cli.WorkflowStateView{
			{ID: "st-1", ProjectID: pv.ID, Name: "Backlog", Category: "backlog", Position: 10},
			{ID: "st-2", ProjectID: pv.ID, Name: "Todo", Category: "todo", Position: 20},
			{ID: "st-3", ProjectID: pv.ID, Name: "In Progress", Category: "in_progress", Position: 30},
			{ID: "st-4", ProjectID: pv.ID, Name: "In Review", Category: "review", Position: 40},
			{ID: "st-5", ProjectID: pv.ID, Name: "Blocked", Category: "blocked", Position: 50},
			{ID: "st-6", ProjectID: pv.ID, Name: "Done", Category: "done", Position: 60},
		}
	}
	e.recordAudit(in.Workspace, key, in.Actor, "project.created", "project", key, pv.Name)
	return pv, nil
}

func (e *inMemoryEnv) ListProjects(_ context.Context, in cli.ListProjectsInput) (cli.ProjectListResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var items []cli.ProjectView
	for _, p := range e.projects {
		if p.WorkspaceID != in.Workspace {
			continue
		}
		if !in.IncludeArchived && p.Status == "archived" {
			continue
		}
		items = append(items, p)
	}
	slices.SortFunc(items, func(a, b cli.ProjectView) int {
		return strings.Compare(a.Key, b.Key)
	})
	return cli.ProjectListResult{Items: items}, nil
}

func (e *inMemoryEnv) GetProject(_ context.Context, ws, proj string) (cli.ProjectView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.checkNoHostPath(proj)
	pv, ok := e.projects[e.projMapKey(ws, proj)]
	if !ok {
		return cli.ProjectView{}, fmt.Errorf("project %q not found in workspace %q", proj, ws)
	}
	return pv, nil
}

func (e *inMemoryEnv) UpdateProject(_ context.Context, in cli.UpdateProjectInput) (cli.ProjectView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.checkNoHostPath(in.Project)
	mk := e.projMapKey(in.Workspace, in.Project)
	pv, ok := e.projects[mk]
	if !ok {
		return cli.ProjectView{}, fmt.Errorf("project %q not found", in.Project)
	}
	if in.Name != nil {
		pv.Name = *in.Name
	}
	if in.Description != nil {
		pv.Description = *in.Description
	}
	pv.UpdatedAt = e.now
	e.projects[mk] = pv
	return pv, nil
}

func (e *inMemoryEnv) ArchiveProject(_ context.Context, in cli.ArchiveProjectInput) (cli.ProjectView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.checkNoHostPath(in.Project)
	mk := e.projMapKey(in.Workspace, in.Project)
	pv, ok := e.projects[mk]
	if !ok {
		return cli.ProjectView{}, fmt.Errorf("project %q not found", in.Project)
	}
	pv.Status = "archived"
	e.projects[mk] = pv
	e.recordAudit(in.Workspace, pv.Key, in.Actor, "project.archived", "project", pv.Key, in.Reason)
	return pv, nil
}

func (e *inMemoryEnv) DeleteProject(_ context.Context, in cli.DeleteProjectInput) (cli.DeleteProjectResultView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.checkNoHostPath(in.Project)
	mk := e.projMapKey(in.Workspace, in.Project)
	pv, ok := e.projects[mk]
	if !ok {
		return cli.DeleteProjectResultView{}, fmt.Errorf("project %q not found", in.Project)
	}
	delete(e.projects, mk)
	return cli.DeleteProjectResultView{
		WorkspaceID:      in.Workspace,
		DeletedProjectID: pv.ID,
		DeletedKey:       pv.Key,
	}, nil
}

func (e *inMemoryEnv) BindSubproject(_ context.Context, in cli.BindSubprojectInput) (cli.ProjectBindingView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.checkNoHostPath(in.ParentProject)
	e.checkNoHostPath(in.ChildProject)
	p, okP := e.projects[e.projMapKey(in.Workspace, in.ParentProject)]
	if !okP {
		return cli.ProjectBindingView{}, fmt.Errorf("parent project %q not found", in.ParentProject)
	}
	ch, okC := e.projects[e.projMapKey(in.Workspace, in.ChildProject)]
	if !okC {
		return cli.ProjectBindingView{}, fmt.Errorf("child project %q not found", in.ChildProject)
	}
	return cli.ProjectBindingView{
		ID:              e.nextID("pb"),
		WorkspaceID:     in.Workspace,
		ParentProjectID: p.ID,
		ParentKey:       p.Key,
		ChildProjectID:  ch.ID,
		ChildKey:        ch.Key,
		ScopeSubpath:    in.ScopeSubpath,
		CreatedAt:       e.now,
	}, nil
}

func (e *inMemoryEnv) UnbindSubproject(_ context.Context, in cli.UnbindSubprojectInput) (cli.UnbindSubprojectResultView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return cli.UnbindSubprojectResultView{
		ParentProject: strings.ToUpper(in.ParentProject),
		ChildProject:  strings.ToUpper(in.ChildProject),
		Removed:       true,
	}, nil
}

func (e *inMemoryEnv) ListProjectBindings(_ context.Context, _ cli.ListProjectBindingsInput) ([]cli.ProjectBindingView, error) {
	return nil, nil
}

func (e *inMemoryEnv) ListStates(_ context.Context, ws, proj string) ([]cli.WorkflowStateView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.checkNoHostPath(proj)
	return slices.Clone(e.states[e.projMapKey(ws, proj)]), nil
}

func (e *inMemoryEnv) CreateState(_ context.Context, in cli.CreateStateInput) (cli.WorkflowStateView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.checkNoHostPath(in.Project)
	st := cli.WorkflowStateView{
		ID:        e.nextID("st"),
		ProjectID: in.Project,
		Name:      in.Name,
		Category:  in.Category,
		Position:  in.Position,
		Color:     in.Color,
	}
	mk := e.projMapKey(in.Workspace, in.Project)
	e.states[mk] = append(e.states[mk], st)
	return st, nil
}

func (e *inMemoryEnv) ListLabels(_ context.Context, ws, proj string) ([]cli.LabelView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.checkNoHostPath(proj)
	return slices.Clone(e.labels[e.projMapKey(ws, proj)]), nil
}

func (e *inMemoryEnv) CreateLabel(_ context.Context, in cli.CreateLabelInput) (cli.LabelView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.checkNoHostPath(in.Project)
	lb := cli.LabelView{
		ID:          e.nextID("lbl"),
		ProjectID:   in.Project,
		Name:        in.Name,
		Color:       in.Color,
		Description: in.Description,
	}
	mk := e.projMapKey(in.Workspace, in.Project)
	e.labels[mk] = append(e.labels[mk], lb)
	return lb, nil
}

func (e *inMemoryEnv) GetProjectMarkdown(_ context.Context, ws, proj string) (cli.MarkdownDocView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	mk := e.projMapKey(ws, proj)
	pv, ok := e.projects[mk]
	if !ok {
		return cli.MarkdownDocView{}, fmt.Errorf("project %q not found", proj)
	}
	md, exists := e.projDocs[mk]
	return cli.MarkdownDocView{
		EntityKind:     "project",
		EntityKey:      pv.Key,
		EntityID:       pv.ID,
		WorkspaceID:    ws,
		HasMarkdownDoc: exists,
		Markdown:       md,
		Action:         "get",
	}, nil
}

func (e *inMemoryEnv) PutProjectMarkdown(_ context.Context, ws, proj, _, md string) (cli.MarkdownDocView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	mk := e.projMapKey(ws, proj)
	pv, ok := e.projects[mk]
	if !ok {
		return cli.MarkdownDocView{}, fmt.Errorf("project %q not found", proj)
	}
	e.projDocs[mk] = md
	pv.HasMarkdownDoc = strings.TrimSpace(md) != ""
	e.projects[mk] = pv
	return cli.MarkdownDocView{
		EntityKind:     "project",
		EntityKey:      pv.Key,
		EntityID:       pv.ID,
		WorkspaceID:    ws,
		HasMarkdownDoc: pv.HasMarkdownDoc,
		Markdown:       md,
		Action:         "put",
	}, nil
}

func (e *inMemoryEnv) DeleteProjectMarkdown(_ context.Context, ws, proj, _ string) (cli.MarkdownDocView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	mk := e.projMapKey(ws, proj)
	pv, ok := e.projects[mk]
	if !ok {
		return cli.MarkdownDocView{}, fmt.Errorf("project %q not found", proj)
	}
	delete(e.projDocs, mk)
	pv.HasMarkdownDoc = false
	e.projects[mk] = pv
	return cli.MarkdownDocView{
		EntityKind:     "project",
		EntityKey:      pv.Key,
		EntityID:       pv.ID,
		WorkspaceID:    ws,
		HasMarkdownDoc: false,
		Action:         "delete",
	}, nil
}

// LocalPathRegistry implementation (100% client-local).
func (e *inMemoryEnv) Link(_ context.Context, localDir, workspaceID, projectID string) (cli.LocalProjectBinding, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	b := cli.LocalProjectBinding{
		LocalDir:    localDir,
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
		ProjectKey:  strings.ToUpper(projectID),
		LinkedAt:    e.now,
	}
	e.bindings[localDir] = b
	return b, nil
}

func (e *inMemoryEnv) Unlink(_ context.Context, localDir string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.bindings[localDir]; !ok {
		return fmt.Errorf("no local binding for %q", localDir)
	}
	delete(e.bindings, localDir)
	return nil
}

func (e *inMemoryEnv) ResolveByDir(_ context.Context, localDir string) (cli.LocalProjectBinding, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	bestLen := -1
	var best cli.LocalProjectBinding
	for dir, b := range e.bindings {
		if localDir == dir || strings.HasPrefix(localDir, dir+string(filepath.Separator)) {
			if len(dir) > bestLen {
				bestLen = len(dir)
				best = b
			}
		}
	}
	if bestLen < 0 {
		return cli.LocalProjectBinding{}, fmt.Errorf("no local project bound to %q", localDir)
	}
	return best, nil
}

func (e *inMemoryEnv) ListByProject(_ context.Context, workspaceID, projectID string) ([]cli.LocalProjectBinding, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []cli.LocalProjectBinding
	for _, b := range e.bindings {
		if workspaceID != "" && b.WorkspaceID != workspaceID {
			continue
		}
		if projectID != "" && !strings.EqualFold(b.ProjectID, projectID) && !strings.EqualFold(b.ProjectKey, projectID) {
			continue
		}
		out = append(out, b)
	}
	slices.SortFunc(out, func(a, b cli.LocalProjectBinding) int {
		return strings.Compare(a.LocalDir, b.LocalDir)
	})
	return out, nil
}

// ObjectiveClient implementation.
func (e *inMemoryEnv) CreateObjective(_ context.Context, in cli.CreateObjectiveInput) (cli.ObjectiveView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.checkNoHostPath(in.Project)
	key := strings.TrimSpace(in.Slug)
	if key == "" {
		key = fmt.Sprintf("%s-O%d", strings.ToUpper(in.Project), len(e.objectives)+1)
	}
	obj := cli.ObjectiveView{
		ID:              e.nextID("obj"),
		WorkspaceID:     in.Workspace,
		ProjectID:       strings.ToUpper(in.Project),
		Key:             key,
		Kind:            in.Kind,
		Title:           in.Title,
		Summary:         in.Summary,
		SuccessCriteria: append([]string(nil), in.SuccessCriteria...),
		Status:          "active",
		Priority:        in.Priority,
		TargetDate:      in.TargetDate,
		Owner:           in.Owner,
		CreatedAt:       e.now,
		UpdatedAt:       e.now,
	}
	e.objectives[key] = obj
	return obj, nil
}

func (e *inMemoryEnv) refreshObjectiveCountsLocked(obj cli.ObjectiveView) cli.ObjectiveView {
	var total, completed, blocked int
	for _, t := range e.tasks {
		if !strings.EqualFold(t.ObjectiveKey, obj.Key) {
			continue
		}
		total++
		if strings.EqualFold(t.State, "Done") || t.Category == "done" {
			completed++
		}
		if t.Blocked {
			blocked++
		}
	}
	obj.TotalTasks = total
	obj.CompletedTasks = completed
	obj.BlockedTasks = blocked
	return obj
}

func (e *inMemoryEnv) ListObjectives(_ context.Context, in cli.ListObjectivesInput) (cli.ObjectiveListResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.checkNoHostPath(in.Project)
	var items []cli.ObjectiveView
	for _, obj := range e.objectives {
		if obj.WorkspaceID != in.Workspace || !strings.EqualFold(obj.ProjectID, in.Project) {
			continue
		}
		if in.Kind != "" && obj.Kind != in.Kind {
			continue
		}
		if in.Status != "" && obj.Status != in.Status {
			continue
		}
		if in.Owner != "" && obj.Owner != in.Owner {
			continue
		}
		items = append(items, e.refreshObjectiveCountsLocked(obj))
	}
	slices.SortFunc(items, func(a, b cli.ObjectiveView) int {
		return strings.Compare(a.Key, b.Key)
	})
	return cli.ObjectiveListResult{Items: items}, nil
}

func (e *inMemoryEnv) GetObjective(_ context.Context, _, proj, ref string) (cli.ObjectiveView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.checkNoHostPath(proj)
	obj, ok := e.objectives[ref]
	if !ok {
		return cli.ObjectiveView{}, fmt.Errorf("objective %q not found", ref)
	}
	return e.refreshObjectiveCountsLocked(obj), nil
}

func (e *inMemoryEnv) UpdateObjective(_ context.Context, in cli.UpdateObjectiveInput) (cli.ObjectiveView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	obj, ok := e.objectives[in.Objective]
	if !ok {
		return cli.ObjectiveView{}, fmt.Errorf("objective %q not found", in.Objective)
	}
	if in.Title != nil {
		obj.Title = *in.Title
	}
	if in.Summary != nil {
		obj.Summary = *in.Summary
	}
	if in.Status != nil {
		obj.Status = *in.Status
	}
	if in.Priority != nil {
		obj.Priority = *in.Priority
	}
	if in.TargetDate != nil {
		obj.TargetDate = *in.TargetDate
	}
	if in.Owner != nil {
		obj.Owner = *in.Owner
	}
	e.objectives[in.Objective] = obj
	return e.refreshObjectiveCountsLocked(obj), nil
}

func (e *inMemoryEnv) CloseObjective(_ context.Context, in cli.CloseObjectiveInput) (cli.ObjectiveView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	obj, ok := e.objectives[in.Objective]
	if !ok {
		return cli.ObjectiveView{}, fmt.Errorf("objective %q not found", in.Objective)
	}
	obj.Status = in.Resolution
	obj.Resolution = in.Resolution
	e.objectives[in.Objective] = obj
	return e.refreshObjectiveCountsLocked(obj), nil
}

// TaskClient implementation.
func toSummary(tv *cli.TaskView) cli.TaskSummaryView {
	holder := ""
	if tv.Lease != nil {
		holder = tv.Lease.Holder
	}
	return cli.TaskSummaryView{
		ID:           tv.ID,
		Key:          tv.Key,
		ExternalID:   tv.ExternalID,
		Title:        tv.Title,
		State:        tv.State,
		Category:     tv.Category,
		Priority:     tv.Priority,
		ObjectiveKey: tv.ObjectiveKey,
		ParentKey:    tv.ParentKey,
		Assignee:     tv.Assignee,
		Holder:       holder,
		Labels:       slices.Clone(tv.Labels),
		Blocked:      tv.Blocked,
		BlockedBy:    slices.Clone(tv.BlockedBy),
		UpdatedAt:    tv.UpdatedAt,
	}
}

func inferCategory(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "backlog":
		return "backlog"
	case "todo", "ready":
		return "todo"
	case "in progress", "in_progress":
		return "in_progress"
	case "in review", "review", "in_review":
		return "review"
	case "blocked":
		return "blocked"
	case "done":
		return "done"
	case "canceled", "cancelled":
		return "canceled"
	case "archived", "archive":
		return "archived"
	default:
		return "todo"
	}
}

func (e *inMemoryEnv) CreateTask(_ context.Context, in cli.CreateTaskInput) (cli.TaskView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.checkNoHostPath(in.Project)
	projKey := strings.ToUpper(in.Project)
	if in.ExternalID != "" {
		for _, existing := range e.tasks {
			if existing.WorkspaceID == in.Workspace && strings.EqualFold(existing.ProjectID, projKey) && existing.ExternalID == in.ExternalID {
				return cli.TaskView{}, fmt.Errorf("external_id %q already exists in project %s", in.ExternalID, projKey)
			}
		}
	}
	key := fmt.Sprintf("%s-%d", projKey, len(e.taskOrder)+1)
	state := in.State
	if state == "" {
		state = "Todo"
	}
	tv := &cli.TaskView{
		ID:           e.nextID("tsk"),
		WorkspaceID:  in.Workspace,
		ProjectID:    projKey,
		Key:          key,
		ExternalID:   in.ExternalID,
		Title:        in.Title,
		Description:  in.Description,
		State:        state,
		Category:     inferCategory(state),
		Priority:     in.Priority,
		ObjectiveKey: in.Objective,
		ParentKey:    in.Parent,
		Assignee:     in.Assignee,
		Labels:       slices.Clone(in.Labels),
		Estimate:     in.Estimate,
		Acceptance:   in.Acceptance,
		Blocked:      len(in.DependsOn) > 0,
		BlockedBy:    slices.Clone(in.DependsOn),
		CreatedAt:    e.now,
		UpdatedAt:    e.now,
	}
	e.tasks[key] = tv
	e.taskOrder = append(e.taskOrder, key)
	e.recordAudit(in.Workspace, projKey, in.Actor, "task.created", "task", key, tv.Title)
	return *tv, nil
}

func (e *inMemoryEnv) ListTasks(_ context.Context, in cli.ListTasksInput) (cli.TaskListResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.checkNoHostPath(in.Project)
	var items []cli.TaskSummaryView
	for _, key := range e.taskOrder {
		tv := e.tasks[key]
		if tv.WorkspaceID != in.Workspace || !strings.EqualFold(tv.ProjectID, in.Project) {
			continue
		}
		if in.Objective != "" && !strings.EqualFold(tv.ObjectiveKey, in.Objective) {
			continue
		}
		if in.Parent != "" && !strings.EqualFold(tv.ParentKey, in.Parent) {
			continue
		}
		if in.ExternalID != "" && tv.ExternalID != in.ExternalID {
			continue
		}
		if in.State != "" && !strings.EqualFold(tv.State, in.State) && !strings.EqualFold(tv.Category, in.State) {
			continue
		}
		if in.State == "" && !in.IncludeArchived && tv.Category == "archived" {
			continue
		}
		if in.Priority >= 0 && tv.Priority != in.Priority {
			continue
		}
		if in.Assignee != "" && tv.Assignee != in.Assignee && (tv.Lease == nil || tv.Lease.Holder != in.Assignee) {
			continue
		}
		if in.Unclaimed && tv.Lease != nil {
			continue
		}
		if in.Blocked && !tv.Blocked {
			continue
		}
		if len(in.Labels) > 0 {
			hasAll := true
			for _, req := range in.Labels {
				if !slices.Contains(tv.Labels, req) {
					hasAll = false
					break
				}
			}
			if !hasAll {
				continue
			}
		}
		items = append(items, toSummary(tv))
		if in.Limit > 0 && len(items) >= in.Limit {
			break
		}
	}
	return cli.TaskListResult{Items: items}, nil
}

func (e *inMemoryEnv) GetTask(_ context.Context, in cli.GetTaskInput) (cli.TaskView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.checkNoHostPath(in.Project)
	tv, ok := e.tasks[in.Task]
	if !ok {
		return cli.TaskView{}, fmt.Errorf("task %q not found", in.Task)
	}
	out := *tv
	if in.IncludeSubtasks {
		for _, k := range e.taskOrder {
			child := e.tasks[k]
			if strings.EqualFold(child.ParentKey, tv.Key) {
				out.Subtasks = append(out.Subtasks, toSummary(child))
			}
		}
	}
	if !in.IncludeComments {
		out.Comments = nil
	}
	if !in.IncludeDeps {
		out.BlockedBy = nil
		out.Blocks = nil
	}
	return out, nil
}

func (e *inMemoryEnv) UpdateTask(_ context.Context, in cli.UpdateTaskInput) (cli.TaskView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	tv, ok := e.tasks[in.Task]
	if !ok {
		return cli.TaskView{}, fmt.Errorf("task %q not found", in.Task)
	}
	if in.Title != nil {
		tv.Title = *in.Title
	}
	if in.ExternalID != nil {
		if *in.ExternalID != "" && *in.ExternalID != tv.ExternalID {
			for _, existing := range e.tasks {
				if existing.Key != tv.Key && existing.WorkspaceID == tv.WorkspaceID && strings.EqualFold(existing.ProjectID, tv.ProjectID) && existing.ExternalID == *in.ExternalID {
					return cli.TaskView{}, fmt.Errorf("external_id %q already exists in project %s", *in.ExternalID, tv.ProjectID)
				}
			}
		}
		tv.ExternalID = *in.ExternalID
	}
	if in.Description != nil {
		tv.Description = *in.Description
	}
	if in.Objective != nil {
		tv.ObjectiveKey = *in.Objective
	}
	if in.Parent != nil {
		tv.ParentKey = *in.Parent
	}
	if in.Priority != nil {
		tv.Priority = *in.Priority
	}
	if in.Assignee != nil {
		tv.Assignee = *in.Assignee
	}
	for _, add := range in.AddLabels {
		if !slices.Contains(tv.Labels, add) {
			tv.Labels = append(tv.Labels, add)
		}
	}
	for _, rm := range in.RemoveLabels {
		tv.Labels = slices.DeleteFunc(tv.Labels, func(s string) bool { return s == rm })
	}
	if in.Estimate != nil {
		tv.Estimate = *in.Estimate
	}
	if in.Acceptance != nil {
		tv.Acceptance = *in.Acceptance
	}
	return *tv, nil
}

func (e *inMemoryEnv) MoveTask(_ context.Context, in cli.MoveTaskInput) (cli.TaskView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	tv, ok := e.tasks[in.Task]
	if !ok {
		return cli.TaskView{}, fmt.Errorf("task %q not found", in.Task)
	}
	tv.State = in.State
	tv.Category = inferCategory(in.State)
	e.recordAudit(in.Workspace, tv.ProjectID, in.Actor, "task.moved", "task", tv.Key, in.State)
	return *tv, nil
}

func (e *inMemoryEnv) ClaimTask(_ context.Context, in cli.ClaimTaskInput) (cli.TaskView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	tv, ok := e.tasks[in.Task]
	if !ok {
		return cli.TaskView{}, fmt.Errorf("task %q not found", in.Task)
	}
	if tv.Lease != nil && tv.Lease.Holder != in.Actor {
		return cli.TaskView{}, fmt.Errorf("task %s already claimed by %s", tv.Key, tv.Lease.Holder)
	}
	tv.Lease = &cli.ClaimLeaseView{
		Holder:     in.Actor,
		LeaseToken: "tok-" + tv.Key,
		AcquiredAt: e.now,
		ExpiresAt:  e.now.Add(in.LeaseTTL),
	}
	tv.State = "In Progress"
	tv.Category = "in_progress"
	e.recordAudit(in.Workspace, tv.ProjectID, in.Actor, "task.claimed", "task", tv.Key, in.Actor)
	return *tv, nil
}

func (e *inMemoryEnv) ReleaseTask(_ context.Context, in cli.ReleaseTaskInput) (cli.TaskView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	tv, ok := e.tasks[in.Task]
	if !ok {
		return cli.TaskView{}, fmt.Errorf("task %q not found", in.Task)
	}
	tv.Lease = nil
	if in.TargetState != "" {
		tv.State = in.TargetState
		tv.Category = inferCategory(in.TargetState)
	} else {
		tv.State = "Todo"
		tv.Category = "todo"
	}
	e.recordAudit(in.Workspace, tv.ProjectID, in.Actor, "task.released", "task", tv.Key, in.Note)
	return *tv, nil
}

func (e *inMemoryEnv) BlockTask(_ context.Context, in cli.BlockTaskInput) (cli.TaskView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	tv, ok := e.tasks[in.Task]
	if !ok {
		return cli.TaskView{}, fmt.Errorf("task %q not found", in.Task)
	}
	tv.Blocked = true
	tv.BlockedBy = slices.Clone(in.BlockedBy)
	tv.BlockerReason = in.Reason
	tv.State = "Blocked"
	tv.Category = "blocked"
	return *tv, nil
}

func (e *inMemoryEnv) UnblockTask(_ context.Context, in cli.UnblockTaskInput) (cli.TaskView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	tv, ok := e.tasks[in.Task]
	if !ok {
		return cli.TaskView{}, fmt.Errorf("task %q not found", in.Task)
	}
	if len(in.ClearedBy) == 0 {
		tv.BlockedBy = nil
	} else {
		for _, c := range in.ClearedBy {
			tv.BlockedBy = slices.DeleteFunc(tv.BlockedBy, func(s string) bool { return s == c })
		}
	}
	tv.Blocked = len(tv.BlockedBy) > 0
	if !tv.Blocked && tv.Category == "blocked" {
		tv.State = "Todo"
		tv.Category = "todo"
	}
	tv.BlockerReason = ""
	return *tv, nil
}

func (e *inMemoryEnv) AddComment(_ context.Context, in cli.AddCommentInput) (cli.CommentView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	tv, ok := e.tasks[in.Task]
	if !ok {
		return cli.CommentView{}, fmt.Errorf("task %q not found", in.Task)
	}
	author := in.Actor
	if author == "" {
		author = "anonymous"
	}
	cv := cli.CommentView{
		ID:        e.nextID("cmt"),
		TaskID:    tv.ID,
		TaskKey:   tv.Key,
		Author:    author,
		Kind:      in.Kind,
		Body:      in.Body,
		CreatedAt: e.now,
	}
	tv.Comments = append(tv.Comments, cv)
	return cv, nil
}

func (e *inMemoryEnv) ListComments(_ context.Context, in cli.ListCommentsInput) (cli.CommentListResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	tv, ok := e.tasks[in.Task]
	if !ok {
		return cli.CommentListResult{}, fmt.Errorf("task %q not found", in.Task)
	}
	return cli.CommentListResult{Items: slices.Clone(tv.Comments)}, nil
}

func (e *inMemoryEnv) NextTask(_ context.Context, in cli.NextTaskInput) (cli.NextTaskResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.checkNoHostPath(in.Project)
	var best *cli.TaskView
	for _, k := range e.taskOrder {
		tv := e.tasks[k]
		if tv.WorkspaceID != in.Workspace || !strings.EqualFold(tv.ProjectID, in.Project) {
			continue
		}
		if tv.Blocked || tv.Lease != nil || tv.Category == "done" || tv.Category == "canceled" {
			continue
		}
		if in.Objective != "" && !strings.EqualFold(tv.ObjectiveKey, in.Objective) {
			continue
		}
		if len(in.Labels) > 0 {
			ok := true
			for _, req := range in.Labels {
				if !slices.Contains(tv.Labels, req) {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
		}
		if best == nil || cliPrioritySortRank(tv.Priority) < cliPrioritySortRank(best.Priority) {
			best = tv
		}
	}
	if best == nil {
		return cli.NextTaskResult{}, nil
	}
	claimed := false
	if in.Claim {
		best.Lease = &cli.ClaimLeaseView{
			Holder:     in.Actor,
			LeaseToken: "tok-" + best.Key,
			AcquiredAt: e.now,
			ExpiresAt:  e.now.Add(in.LeaseTTL),
		}
		best.State = "In Progress"
		best.Category = "in_progress"
		claimed = true
	}
	copied := *best
	return cli.NextTaskResult{Task: &copied, Claimed: claimed}, nil
}

func cliPrioritySortRank(p int) int {
	if p >= 1 && p <= 4 {
		return p
	}
	return 5
}

func (e *inMemoryEnv) Digest(_ context.Context, in cli.TaskDigestInput) (cli.ProjectDigestView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.checkNoHostPath(in.Project)
	projKey := strings.ToUpper(in.Project)
	counts := make(map[string]int)
	var ready, leases, blocked []cli.TaskSummaryView
	for _, k := range e.taskOrder {
		tv := e.tasks[k]
		if tv.WorkspaceID != in.Workspace || !strings.EqualFold(tv.ProjectID, projKey) {
			continue
		}
		if in.Objective != "" && !strings.EqualFold(tv.ObjectiveKey, in.Objective) {
			continue
		}
		counts[tv.Category]++
		sum := toSummary(tv)
		switch {
		case tv.Lease != nil:
			leases = append(leases, sum)
		case tv.Blocked:
			if in.IncludeBlocked {
				blocked = append(blocked, sum)
			}
		case tv.Category != "done" && tv.Category != "canceled":
			ready = append(ready, sum)
		}
	}
	var objs []cli.ObjectiveView
	for _, obj := range e.objectives {
		if obj.WorkspaceID == in.Workspace && strings.EqualFold(obj.ProjectID, projKey) {
			objs = append(objs, e.refreshObjectiveCountsLocked(obj))
		}
	}
	return cli.ProjectDigestView{
		WorkspaceID:   in.Workspace,
		ProjectID:     projKey,
		ProjectKey:    projKey,
		ProjectName:   projKey,
		GeneratedAt:   e.now,
		CountsByState: counts,
		Objectives:    objs,
		ActiveLeases:  leases,
		ReadyQueue:    ready,
		BlockedTasks:  blocked,
	}, nil
}

func (e *inMemoryEnv) GetTaskMarkdown(_ context.Context, ws, taskRef string) (cli.MarkdownDocView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	tv, ok := e.tasks[taskRef]
	if !ok {
		return cli.MarkdownDocView{}, fmt.Errorf("task %q not found", taskRef)
	}
	md, exists := e.taskDocs[taskRef]
	return cli.MarkdownDocView{
		EntityKind:     "task",
		EntityKey:      tv.Key,
		EntityID:       tv.ID,
		WorkspaceID:    ws,
		HasMarkdownDoc: exists,
		Markdown:       md,
		Action:         "get",
	}, nil
}

func (e *inMemoryEnv) PutTaskMarkdown(_ context.Context, ws, taskRef, _, md string) (cli.MarkdownDocView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	tv, ok := e.tasks[taskRef]
	if !ok {
		return cli.MarkdownDocView{}, fmt.Errorf("task %q not found", taskRef)
	}
	e.taskDocs[taskRef] = md
	tv.HasMarkdownDoc = strings.TrimSpace(md) != ""
	return cli.MarkdownDocView{
		EntityKind:     "task",
		EntityKey:      tv.Key,
		EntityID:       tv.ID,
		WorkspaceID:    ws,
		HasMarkdownDoc: tv.HasMarkdownDoc,
		Markdown:       md,
		Action:         "put",
	}, nil
}

func (e *inMemoryEnv) DeleteTaskMarkdown(_ context.Context, ws, taskRef, _ string) (cli.MarkdownDocView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	tv, ok := e.tasks[taskRef]
	if !ok {
		return cli.MarkdownDocView{}, fmt.Errorf("task %q not found", taskRef)
	}
	delete(e.taskDocs, taskRef)
	tv.HasMarkdownDoc = false
	return cli.MarkdownDocView{
		EntityKind:     "task",
		EntityKey:      tv.Key,
		EntityID:       tv.ID,
		WorkspaceID:    ws,
		HasMarkdownDoc: false,
		Action:         "delete",
	}, nil
}

func (e *inMemoryEnv) PurgeArchivedTasks(_ context.Context, ws, _ string) (cli.PurgeArchivedView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var res cli.PurgeArchivedView
	res.WorkspaceID = ws
	var remaining []string
	for _, k := range e.taskOrder {
		tv := e.tasks[k]
		if tv.WorkspaceID == ws && tv.Category == "archived" {
			res.DeletedTaskCount++
			res.DeletedTaskKeys = append(res.DeletedTaskKeys, k)
			if _, ok := e.taskDocs[k]; ok {
				delete(e.taskDocs, k)
				res.DeletedMarkdownCount++
			}
			delete(e.tasks, k)
			continue
		}
		remaining = append(remaining, k)
	}
	e.taskOrder = remaining
	return res, nil
}

func (e *inMemoryEnv) InspectOrphans(_ context.Context, ws string) (cli.OrphanReportView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return cli.OrphanReportView{
		WorkspaceID:          ws,
		OrphanTaskFilesCount: e.orphanCnt,
		TotalIssues:          e.orphanCnt,
	}, nil
}

func (e *inMemoryEnv) PruneOrphans(_ context.Context, ws, _ string, dryRun bool) (cli.PruneResultView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cleaned := e.orphanCnt
	if !dryRun {
		e.orphanCnt = 0
	}
	return cli.PruneResultView{
		WorkspaceID:           ws,
		DryRun:                dryRun,
		RemovedTaskFilesCount: cleaned,
		TotalCleaned:          cleaned,
	}, nil
}

// LanguageClient implementation.
func (e *inMemoryEnv) ExecuteScript(_ context.Context, in cli.ExecuteScriptInput) (cli.ExecuteScriptResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastExecScriptIn = in
	return cli.ExecuteScriptResult{
		DryRun:           in.DryRun,
		Density:          in.Density,
		AppliedCount:     2,
		FormattedSummary: `{"goal":"CORE-O1","task":"CORE-10"}`,
	}, nil
}

func (e *inMemoryEnv) GetGuide(_ context.Context, section string) (cli.LanguageGuideView, error) {
	return cli.LanguageGuideView{
		Section:  section,
		Version:  "2.0.0",
		Markdown: "# Flowsk Code Mode Guide (" + section + ")\n\nconst t1 = flowsk.tasks.create({ title: \"Demo\" });\n",
	}, nil
}

// AuditClient implementation.
func (e *inMemoryEnv) ListAuditEvents(_ context.Context, in cli.ListAuditEventsInput) (cli.AuditEventListResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var items []cli.AuditEventView
	for _, ev := range e.audits {
		if ev.WorkspaceID != in.Workspace {
			continue
		}
		if in.Project != "" && !strings.EqualFold(ev.ProjectID, in.Project) {
			continue
		}
		if in.Actor != "" && ev.Actor != in.Actor {
			continue
		}
		if in.EntityType != "" && ev.EntityType != in.EntityType {
			continue
		}
		if in.EntityID != "" && ev.EntityID != in.EntityID {
			continue
		}
		if in.Action != "" && ev.Action != in.Action {
			continue
		}
		items = append(items, ev)
	}
	return cli.AuditEventListResult{Items: items}, nil
}

func (e *inMemoryEnv) GetAuditEvent(_ context.Context, ws, id string) (cli.AuditEventView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ev := range e.audits {
		if ev.WorkspaceID == ws && ev.ID == id {
			return ev, nil
		}
	}
	return cli.AuditEventView{}, fmt.Errorf("audit event %q not found", id)
}

// DatabaseAdmin implementation.
func (e *inMemoryEnv) MigrateUp(_ context.Context, in cli.MigrateUpInput) (cli.MigrationResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastMigrateUpIn = in
	return cli.MigrationResult{
		Driver:         in.Driver,
		Direction:      "up",
		CurrentVersion: 2026112501,
		AppliedCount:   3,
	}, nil
}

func (e *inMemoryEnv) MigrateDown(_ context.Context, in cli.MigrateDownInput) (cli.MigrationResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastMigrateDownIn = in
	return cli.MigrationResult{
		Driver:         in.Driver,
		Direction:      "down",
		CurrentVersion: 2026112500,
		AppliedCount:   in.Steps,
	}, nil
}

func (e *inMemoryEnv) MigrateStatus(_ context.Context, _, _ string) ([]cli.MigrationStatusEntry, error) {
	appliedAt := e.now
	var secondAppliedAt *time.Time
	if e.allMigrationsApplied {
		secondAppliedAt = &appliedAt
	}
	return []cli.MigrationStatusEntry{
		{Version: 2026112500, Name: "001_init_schema.sql", Applied: true, AppliedAt: &appliedAt},
		{Version: 2026112501, Name: "002_indexes.sql", Applied: e.allMigrationsApplied, AppliedAt: secondAppliedAt},
	}, nil
}

func (e *inMemoryEnv) PingDB(_ context.Context, driver, dsn string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastPingDriver = driver
	e.lastPingDSN = dsn
	return e.pingErr
}

func execCLI(t *testing.T, deps cli.Dependencies, stdin string, args ...string) (string, string, error) {
	t.Helper()
	root := cli.NewRootCommand(deps)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	if stdin != "" {
		root.SetIn(strings.NewReader(stdin))
	}
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), err
}

func mustExecCLI(t *testing.T, deps cli.Dependencies, stdin string, args ...string) string {
	t.Helper()
	out, stderr, err := execCLI(t, deps, stdin, args...)
	if err != nil {
		t.Fatalf("cli %v failed: %v\nstderr: %s\nstdout: %s", args, err, stderr, out)
	}
	return out
}

// TestWorkspaceProjectObjectiveTaskBoardAndAuditLifecycle exercises the full
// CLI command tree across workspaces, projects, goals, tasks, boards, and audits.
func TestWorkspaceProjectObjectiveTaskBoardAndAuditLifecycle(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("FLOWSK_HOME", filepath.Join(tmp, ".flowsk"))

	env := newInMemoryEnv()

	// 1. Workspace lifecycle + actor registration.
	out := mustExecCLI(t, env, "", "--format", "human", "workspace", "create", "acme", "--name", "Acme Corp", "--description", "Main tenant")
	if !strings.Contains(out, "acme") || !strings.Contains(out, "Acme Corp") {
		t.Fatalf("unexpected workspace create human output: %s", out)
	}

	out = mustExecCLI(t, env, "", "--format", "compact", "ws", "list")
	if !strings.Contains(out, "acme") || !strings.Contains(out, "default") {
		t.Fatalf("unexpected workspace list compact output: %s", out)
	}

	out = mustExecCLI(t, env, "", "--format", "json", "workspace", "get", "acme")
	var wsView cli.WorkspaceView
	if err := json.Unmarshal([]byte(out), &wsView); err != nil || wsView.Slug != "acme" {
		t.Fatalf("workspace get json unmarshal failed: err=%v out=%s", err, out)
	}

	mustExecCLI(t, env, "", "workspace", "use", "acme")

	out = mustExecCLI(t, env, "", "--format", "human", "workspace", "actor", "register", "claude-arch", "--kind", "agent", "--model", "claude-sonnet", "--role", "architect", "--display-name", "Claude Architect")
	if !strings.Contains(out, "claude-arch") {
		t.Fatalf("expected actor handle in output: %s", out)
	}

	out = mustExecCLI(t, env, "", "--format", "compact", "workspace", "actor", "list", "--kind", "agent")
	if !strings.Contains(out, "claude-arch") {
		t.Fatalf("expected claude-arch in actor list: %s", out)
	}

	if _, _, err := execCLI(t, env, "", "workspace", "actor", "register", "bad", "--kind", "robot"); err == nil {
		t.Fatal("expected invalid actor --kind to fail")
	}

	// 2. Project lifecycle, custom states, labels, and zero-repo-footprint local path linking.
	out = mustExecCLI(t, env, "", "--actor", "claude-arch", "--format", "human", "project", "create", "CORE", "--name", "Core Engine", "--description", "Storage & RPC")
	if !strings.Contains(out, "CORE") || !strings.Contains(out, "Core Engine") {
		t.Fatalf("unexpected project create output: %s", out)
	}

	out = mustExecCLI(t, env, "", "--format", "compact", "project", "list")
	if !strings.Contains(out, "CORE") {
		t.Fatalf("expected CORE in project list: %s", out)
	}

	out = mustExecCLI(t, env, "", "--format", "human", "project", "update", "CORE", "--description", "Updated storage engine")
	if !strings.Contains(out, "Updated storage engine") {
		t.Fatalf("expected updated description: %s", out)
	}

	if _, _, err := execCLI(t, env, "", "project", "update", "CORE"); err == nil {
		t.Fatal("expected project update with zero changed flags to fail")
	}

	// Link Mario's Desktop folder & Paolo's Downloads folder to the same logical project CORE.
	marioRepo := filepath.Join(tmp, "mario-desktop", "flowsk-app")
	marioSubdir := filepath.Join(marioRepo, "internal", "service")
	paoloRepo := filepath.Join(tmp, "paolo-downloads", "flowsk-app")
	if err := os.MkdirAll(marioSubdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paoloRepo, 0o755); err != nil {
		t.Fatal(err)
	}

	out = mustExecCLI(t, env, "", "--project", "CORE", "--format", "human", "project", "link", marioRepo)
	if !strings.Contains(out, "CORE") {
		t.Fatalf("unexpected project link output: %s", out)
	}
	mustExecCLI(t, env, "", "--project", "CORE", "--format", "compact", "project", "link", paoloRepo)

	// Verify ZERO files were created inside Mario's target repository.
	entries, err := os.ReadDir(marioRepo)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "internal" {
		t.Fatalf("expected zero Flowsk metadata files inside target repo, got %v", entries)
	}

	// Verify `project where` by directory and by --project.
	out = mustExecCLI(t, env, "", "--format", "compact", "project", "where", marioSubdir)
	if !strings.Contains(out, "CORE") {
		t.Fatalf("expected project where on subdirectory to resolve CORE, got: %s", out)
	}
	out = mustExecCLI(t, env, "", "--project", "CORE", "--format", "human", "project", "where")
	if !strings.Contains(out, "mario-desktop") || !strings.Contains(out, "paolo-downloads") {
		t.Fatalf("expected both local bindings in project where --project CORE, got: %s", out)
	}

	// Chdir into Mario's subdirectory so subsequent commands resolve project CORE automatically WITHOUT --project!
	t.Chdir(marioSubdir)

	out = mustExecCLI(t, env, "", "--format", "compact", "project", "get")
	if !strings.Contains(out, "CORE") {
		t.Fatalf("expected cwd resolution for project get, got: %s", out)
	}

	mustExecCLI(t, env, "", "--format", "human", "project", "state", "create", "QA Gate", "--category", "review", "--position", "45", "--color", "#0EA5E9")
	out = mustExecCLI(t, env, "", "--format", "compact", "project", "state", "list")
	if !strings.Contains(out, "QA Gate") {
		t.Fatalf("expected QA Gate in state list: %s", out)
	}

	mustExecCLI(t, env, "", "--format", "human", "project", "label", "create", "storage", "--color", "#22C55E", "--description", "SQLite & Postgres")
	out = mustExecCLI(t, env, "", "--format", "tsv", "project", "label", "list")
	if !strings.Contains(out, "storage") {
		t.Fatalf("expected storage in label list: %s", out)
	}

	// 3. Objective / Goal lifecycle + live task completion progress bars.
	out = mustExecCLI(t, env, "", "--format", "human", "goal", "create", "Ship Dual-Mode Engine", "--kind", "goal", "--priority", "1", "--target-date", "2026-12-15", "--owner", "claude-arch", "--summary", "Local SQLite + ConnectRPC")
	if !strings.Contains(out, "CORE-O1") || !strings.Contains(out, "Ship Dual-Mode Engine") {
		t.Fatalf("unexpected goal create output: %s", out)
	}

	if _, _, err := execCLI(t, env, "", "goal", "create", "Bad Date", "--target-date", "15-12-2026"); err == nil {
		t.Fatal("expected invalid YYYY-MM-DD target-date to fail")
	}

	// 4. Task lifecycle: create, subtasks, dependencies, claim, comment, move, block, unblock, next, digest.
	out = mustExecCLI(t, env, "", "--actor", "claude-arch", "--format", "compact", "task", "create", "Build SQLite WAL store", "--objective", "CORE-O1", "--priority", "2", "--label", "storage", "--acceptance", "go test -race green")
	if !strings.Contains(out, "CORE-1") {
		t.Fatalf("expected CORE-1 in task create compact output: %s", out)
	}

	out = mustExecCLI(t, env, "", "--actor", "claude-arch", "--format", "compact", "task", "create", "Wire ConnectRPC server", "--objective", "CORE-O1", "--parent", "CORE-1", "--priority", "3", "--depends-on", "CORE-1")
	if !strings.Contains(out, "CORE-2") {
		t.Fatalf("expected CORE-2 in task create output: %s", out)
	}

	mustExecCLI(t, env, "", "--actor", "claude-arch", "--format", "compact", "task", "create", "Write CLI renderers", "--objective", "CORE-O1", "--priority", "1", "--label", "cli")

	// `task next` without --claim selects highest-priority unblocked task (CORE-3, P1 urgent).
	out = mustExecCLI(t, env, "", "--format", "compact", "task", "next")
	if !strings.Contains(out, "CORE-3") {
		t.Fatalf("expected CORE-3 from task next, got: %s", out)
	}

	// `task next --claim` without --actor must fail validation.
	if _, _, err := execCLI(t, env, "", "task", "next", "--claim"); err == nil {
		t.Fatal("expected task next --claim without --actor to fail")
	}

	// `task next --claim --actor gemini-dev` atomically claims CORE-3.
	out = mustExecCLI(t, env, "", "--actor", "gemini-dev", "--format", "human", "task", "next", "--claim", "--lease-ttl", "30m")
	if !strings.Contains(out, "CORE-3") || !strings.Contains(out, "gemini-dev") {
		t.Fatalf("expected claimed CORE-3 in human output: %s", out)
	}

	// Foreign actor attempting to claim CORE-3 must receive lease error.
	if _, _, err := execCLI(t, env, "", "--actor", "other-agent", "task", "claim", "CORE-3"); err == nil {
		t.Fatal("expected foreign claim on active lease to fail")
	}

	// Claim CORE-1, post comments (-m and --stdin), complete CORE-1, unblock CORE-2.
	mustExecCLI(t, env, "", "--actor", "claude-arch", "--format", "compact", "task", "claim", "CORE-1")
	mustExecCLI(t, env, "", "--actor", "claude-arch", "--format", "compact", "task", "comment", "CORE-1", "-m", "WAL pragmas verified", "--kind", "verification")
	mustExecCLI(t, env, "Handoff details from stdin\n", "--actor", "claude-arch", "--format", "human", "task", "comment", "CORE-1", "--stdin", "--kind", "handoff")

	out = mustExecCLI(t, env, "", "--format", "human", "task", "comments", "CORE-1")
	if !strings.Contains(out, "WAL pragmas verified") || !strings.Contains(out, "Handoff details from stdin") {
		t.Fatalf("expected both comments in task comments output: %s", out)
	}

	mustExecCLI(t, env, "", "--actor", "claude-arch", "--format", "compact", "task", "release", "CORE-1", "--state", "Done", "--note", "Completed")
	mustExecCLI(t, env, "", "--actor", "claude-arch", "--format", "compact", "task", "unblock", "CORE-2", "--note", "CORE-1 finished")
	mustExecCLI(t, env, "", "--actor", "claude-arch", "--format", "compact", "task", "update", "CORE-2", "--title", "Wire ConnectRPC HTTP/2 server", "--priority", "1", "--add-label", "rpc")
	mustExecCLI(t, env, "", "--actor", "claude-arch", "--format", "compact", "task", "block", "CORE-2", "--by", "CORE-3", "--reason", "Waiting for CLI DTOs")
	mustExecCLI(t, env, "", "--actor", "gemini-dev", "--format", "compact", "task", "move", "CORE-3", "In Review", "--note", "PR ready")

	out = mustExecCLI(t, env, "", "--format", "human", "task", "get", "CORE-1")
	if !strings.Contains(out, "CORE-1") || !strings.Contains(out, "CORE-2") {
		t.Fatalf("expected task get to display CORE-1 and subtask CORE-2: %s", out)
	}

	out = mustExecCLI(t, env, "", "--format", "jsonl", "task", "list")
	if !strings.Contains(out, `"key":"CORE-1"`) {
		t.Fatalf("expected JSONL task output: %s", out)
	}

	// Verify goal list now displays 1/3 (33%) live task completion progress!
	out = mustExecCLI(t, env, "", "--format", "human", "goal", "list")
	if !strings.Contains(out, "1/3") || !strings.Contains(out, "33%") {
		t.Fatalf("expected 1/3 (33%%) objective progress in human goal list, got:\n%s", out)
	}
	out = mustExecCLI(t, env, "", "--format", "compact", "objective", "get", "CORE-O1")
	if !strings.Contains(out, "1/3") {
		t.Fatalf("expected 1/3 progress in compact objective get, got: %s", out)
	}

	// 5. Digest & Board (`flowsk board` / `flowsk status`) in human, compact, md, json, and max-tokens!
	out = mustExecCLI(t, env, "", "--format", "human", "task", "digest")
	if !strings.Contains(out, "CORE") || !strings.Contains(out, "CORE-O1") {
		t.Fatalf("unexpected human digest output:\n%s", out)
	}
	out = mustExecCLI(t, env, "", "--format", "compact", "--max-tokens", "25", "issue", "digest")
	if len(out) == 0 {
		t.Fatal("expected non-empty token-budgeted compact digest")
	}

	out = mustExecCLI(t, env, "", "--format", "human", "board", "--include-completed", "--limit-per-state", "1")
	if !strings.Contains(out, "Core Engine") || !strings.Contains(out, "Ship Dual-Mode Engine") || !strings.Contains(out, "1/3") {
		t.Fatalf("expected project, goal, and progress bar in board human view, got:\n%s", out)
	}

	out = mustExecCLI(t, env, "", "--format", "compact", "status")
	if !strings.Contains(out, "CORE") {
		t.Fatalf("expected compact status output to mention CORE, got: %s", out)
	}

	out = mustExecCLI(t, env, "", "--format", "md", "board")
	if !strings.Contains(out, "#") || !strings.Contains(out, "CORE") {
		t.Fatalf("expected Markdown board output, got: %s", out)
	}

	// Close objective & archive project + inspect audit trail.
	mustExecCLI(t, env, "", "--format", "compact", "objective", "update", "CORE-O1", "--status", "active", "--priority", "0")
	out = mustExecCLI(t, env, "", "--format", "human", "objective", "close", "CORE-O1", "--resolution", "completed", "--note", "All milestones shipped")
	if !strings.Contains(out, "completed") {
		t.Fatalf("expected completed objective status, got: %s", out)
	}

	out = mustExecCLI(t, env, "", "--format", "human", "audit", "list", "--since", "24h")
	if !strings.Contains(out, "task.created") || !strings.Contains(out, "evt-1") {
		t.Fatalf("expected audit events in output:\n%s", out)
	}

	out = mustExecCLI(t, env, "", "--format", "json", "audit", "get", "evt-1")
	if !strings.Contains(out, `"id": "evt-1"`) && !strings.Contains(out, `"id":"evt-1"`) {
		t.Fatalf("expected evt-1 in audit get json: %s", out)
	}

	if _, _, err := execCLI(t, env, "", "audit", "list", "--since", "not-a-timestamp"); err == nil {
		t.Fatal("expected invalid --since to return validation error")
	}

	// Unlink Paolo's and Mario's folders (--all) and verify unbound cwd requires --project.
	out = mustExecCLI(t, env, "", "--project", "CORE", "--format", "compact", "project", "unlink", "--all")
	if !strings.Contains(out, "2") {
		t.Fatalf("expected 2 unlinked directories, got: %s", out)
	}
	if _, _, err := execCLI(t, env, "", "task", "list"); err == nil {
		t.Fatal("expected task list without --project in unlinked folder to fail")
	}

	mustExecCLI(t, env, "", "--format", "compact", "project", "archive", "CORE", "--reason", "v1 sunset")

	if env.projectPathLeak {
		t.Fatal("SECURITY VIOLATION: local filesystem path leaked into remote client port")
	}
}

// TestCodeModeDBConfigMCPAndServeCommands verifies Cloudflare Code Mode JS
// execution (`flowsk code` / `exec`), DB administration, config redaction,
// MCP tools, and serve flags.
func TestCodeModeDBConfigMCPAndServeCommands(t *testing.T) {
	tmp := t.TempDir()
	flowskHome := filepath.Join(tmp, ".flowsk")
	t.Setenv("HOME", tmp)
	t.Setenv("FLOWSK_HOME", "")

	env := newInMemoryEnv()

	// 1. Cloudflare Code Mode (`flowsk code` / `exec`): -e, -f (verify path privacy!), --stdin, --dry-run, and SDK search.
	out := mustExecCLI(t, env, "", "--project", "CORE", "--format", "compact", "code", "-e", `const g = flowsk.goals.create({ title: "G1" }); return g;`, "--dry-run")
	if !strings.Contains(out, "CORE-O1") || !env.lastExecScriptIn.DryRun {
		t.Fatalf("unexpected code -e output: %s", out)
	}

	jsFile := filepath.Join(tmp, "secret-local-folder", "plan.js")
	if err := os.MkdirAll(filepath.Dir(jsFile), 0o755); err != nil {
		t.Fatal(err)
	}
	scriptBody := "const t1 = flowsk.tasks.create({ title: \"From local file\" }); return t1;\n"
	if err := os.WriteFile(jsFile, []byte(scriptBody), 0o600); err != nil {
		t.Fatal(err)
	}
	mustExecCLI(t, env, "", "--format", "human", "code", "-f", jsFile)
	if env.lastExecScriptIn.Script != scriptBody {
		t.Fatalf("expected exact file body %q, got %q", scriptBody, env.lastExecScriptIn.Script)
	}
	if strings.Contains(env.lastExecScriptIn.Script, "secret-local-folder") || strings.Contains(env.lastExecScriptIn.Project, "secret-local-folder") {
		t.Fatal("local js file path leaked into ExecuteScriptInput")
	}

	mustExecCLI(t, env, "return flowsk.tasks.ready();\n", "--format", "json", "exec", "--stdin")
	if !strings.Contains(env.lastExecScriptIn.Script, "flowsk.tasks.ready") {
		t.Fatalf("expected stdin script, got %q", env.lastExecScriptIn.Script)
	}

	if _, _, err := execCLI(t, env, "", "code", "-e", "return 1;", "--stdin"); err == nil {
		t.Fatal("expected error when both -e and --stdin are set")
	}
	if _, _, err := execCLI(t, env, "", "code"); err == nil {
		t.Fatal("expected error when no script source is provided")
	}
	if _, _, err := execCLI(t, env, "", "lang", "exec", "-e", "return 1;"); err == nil {
		t.Fatal("expected removed legacy 'lang' command to be rejected")
	}

	out = mustExecCLI(t, env, "", "--format", "human", "search", "--type", "sdk", "claim")
	if !strings.Contains(out, "flowsk.tasks.claim") {
		t.Fatalf("expected SDK method in search output: %s", out)
	}

	// 2. Database admin (`db migrate up|down|status`, `db ping`).
	out = mustExecCLI(t, env, "", "--format", "human", "db", "--db-driver", "sqlite", "--db-dsn", "/tmp/test.db", "migrate", "up", "--target-version", "2026112501")
	if !strings.Contains(out, "2026112501") || env.lastMigrateUpIn.TargetVersion != 2026112501 {
		t.Fatalf("unexpected db migrate up output: %s", out)
	}

	out = mustExecCLI(t, env, "", "--format", "compact", "db", "migrate", "down", "--steps", "2")
	if !strings.Contains(out, "down") || env.lastMigrateDownIn.Steps != 2 {
		t.Fatalf("unexpected db migrate down output: %s", out)
	}

	out = mustExecCLI(t, env, "", "--format", "tsv", "db", "migrate", "status")
	if !strings.Contains(out, "001_init_schema.sql") {
		t.Fatalf("unexpected db migrate status output: %s", out)
	}

	out = mustExecCLI(t, env, "", "--format", "human", "db", "--db-driver", "postgres", "--db-dsn", "postgres://u:p@localhost:5432/flowsk", "ping", "--timeout", "2s")
	if !strings.Contains(out, "ok") || env.lastPingDriver != "postgres" {
		t.Fatalf("unexpected db ping output: %s", out)
	}

	if _, _, err := execCLI(t, env, "", "db", "--db-driver", "mysql", "ping"); err == nil {
		t.Fatal("expected unsupported --db-driver error")
	}

	// 3. Config (`init`, `show --redact`, `paths`) with strict 0700/0600 permissions inside ~/.flowsk.
	mustExecCLI(t, env, "", "config", "init")
	cfgFile := filepath.Join(flowskHome, "config.yaml")
	lpFile := filepath.Join(flowskHome, "local_paths.json")

	dirInfo, err := os.Stat(filepath.Dir(cfgFile))
	if err != nil || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("expected 0700 dir permissions, err=%v mode=%v", err, dirInfo.Mode().Perm())
	}
	fileInfo, err := os.Stat(cfgFile)
	if err != nil || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 config.yaml permissions, err=%v mode=%v", err, fileInfo.Mode().Perm())
	}
	lpInfo, err := os.Stat(lpFile)
	if err != nil || lpInfo.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 local_paths.json permissions, err=%v mode=%v", err, lpInfo.Mode().Perm())
	}

	if _, _, err := execCLI(t, env, "", "config", "init"); err == nil {
		t.Fatal("expected second config init without --force to fail")
	}
	mustExecCLI(t, env, "", "config", "init", "--force")

	out = mustExecCLI(t, env, "", "--server-url", "https://flowsk.example.com", "--auth-token", "super-secret-jwt", "--format", "human", "config", "show", "--redact")
	if strings.Contains(out, "super-secret-jwt") || !strings.Contains(out, "REDACTED") {
		t.Fatalf("expected auth-token to be redacted in config show, got:\n%s", out)
	}
	out = mustExecCLI(t, env, "", "--auth-token", "super-secret-jwt", "--format", "json", "config", "show", "--redact=false")
	if !strings.Contains(out, "super-secret-jwt") {
		t.Fatalf("expected unredacted token when --redact=false, got:\n%s", out)
	}

	// 4. MCP (`tools`, `guide`, `serve`) & `serve` daemon command.
	out = mustExecCLI(t, env, "", "--format", "human", "mcp", "tools")
	if !strings.Contains(out, "flowsk_execute") || !strings.Contains(out, "flowsk_digest") {
		t.Fatalf("expected canonical MCP tools in output: %s", out)
	}

	out = mustExecCLI(t, env, "", "--format", "compact", "mcp", "guide", "--client", "claude")
	if !strings.Contains(out, "claude") {
		t.Fatalf("expected claude MCP guide snippet: %s", out)
	}
	if _, _, err := execCLI(t, env, "", "mcp", "guide", "--client", "unknown-ide"); err == nil {
		t.Fatal("expected unknown --client error on mcp guide")
	}

	mustExecCLI(t, env, "", "--workspace", "acme", "--actor", "claude-dev", "mcp", "serve", "--transport", "http", "--http-addr", "127.0.0.1:18081")
	if env.lastMCPCfg.Transport != mcp.TransportHTTP || env.lastMCPCfg.HTTPAddr != "127.0.0.1:18081" || env.lastMCPCfg.DefaultWorkspaceID != "acme" {
		t.Fatalf("unexpected MCP serve config: %+v", env.lastMCPCfg)
	}
	if _, _, err := execCLI(t, env, "", "mcp", "serve", "--transport", "websocket"); err == nil {
		t.Fatal("expected invalid --transport error on mcp serve")
	}

	mustExecCLI(t, env, "", "serve", "--listen", "0.0.0.0:19090", "--db-driver", "postgres", "--db-dsn", "postgres://localhost/flowsk", "--auto-migrate=false")
	if env.lastServeCfg.ListenAddr != "0.0.0.0:19090" || env.lastServeCfg.DBDriver != "postgres" || env.lastServeCfg.AutoMigrate {
		t.Fatalf("unexpected ServeConfig: %+v", env.lastServeCfg)
	}
	if _, _, err := execCLI(t, env, "", "serve", "--db-driver", "oracle"); err == nil {
		t.Fatal("expected invalid --db-driver error on serve")
	}

	// 5. Web UI (`flowsk ui` / `flowsk web`), Markdown docs, Archive Purge, and Orphan Prune.
	mustExecCLI(t, env, "", "ui", "--no-open")
	if !env.lastServeCfg.EnableUI || env.lastServeCfg.ListenAddr != cli.DefaultUIListenAddr || cli.DefaultUIListenAddr != "127.0.0.1:1616" {
		t.Fatalf("expected default flowsk ui ListenAddr=127.0.0.1:1616 with EnableUI=true, got: %+v", env.lastServeCfg)
	}
	mustExecCLI(t, env, "", "ui", "--listen", "127.0.0.1:18888", "--no-open")
	if !env.lastServeCfg.EnableUI || env.lastServeCfg.ListenAddr != "127.0.0.1:18888" {
		t.Fatalf("expected EnableUI=true on flowsk ui, got: %+v", env.lastServeCfg)
	}
	mustExecCLI(t, env, "", "web", "--listen", "127.0.0.1:18889", "--no-open")
	if !env.lastServeCfg.EnableUI || env.lastServeCfg.ListenAddr != "127.0.0.1:18889" {
		t.Fatalf("expected EnableUI=true on flowsk web alias, got: %+v", env.lastServeCfg)
	}

	mustExecCLI(t, env, "", "project", "create", "WEBUI", "--name", "Web Panel")
	mustExecCLI(t, env, "", "--project", "WEBUI", "project", "doc", "--set", "# Project Spec")
	projDocOut := mustExecCLI(t, env, "", "--project", "WEBUI", "--format", "human", "project", "doc")
	if !strings.Contains(projDocOut, "# Project Spec") {
		t.Fatalf("unexpected project doc output: %q", projDocOut)
	}
	mustExecCLI(t, env, "", "--project", "WEBUI", "project", "doc", "--clear")

	mustExecCLI(t, env, "", "--project", "WEBUI", "task", "create", "Ship Kanban", "--priority", "1")
	mustExecCLI(t, env, "", "--project", "WEBUI", "task", "doc", "WEBUI-1", "--set", "# Task Notes")
	taskDocOut := mustExecCLI(t, env, "", "--project", "WEBUI", "--format", "human", "task", "doc", "WEBUI-1")
	if !strings.Contains(taskDocOut, "# Task Notes") {
		t.Fatalf("unexpected task doc output: %q", taskDocOut)
	}
	mustExecCLI(t, env, "", "--project", "WEBUI", "task", "archive", "WEBUI-1", "--note", "Done & archived")
	archList := mustExecCLI(t, env, "", "--project", "WEBUI", "--format", "compact", "task", "list", "--archived")
	if !strings.Contains(archList, "WEBUI-1") {
		t.Fatalf("expected archived task in --archived list, got: %q", archList)
	}
	purgeOut := mustExecCLI(t, env, "", "--format", "human", "task", "purge-archived")
	if !strings.Contains(purgeOut, "WEBUI-1") {
		t.Fatalf("expected WEBUI-1 in purge-archived output, got: %q", purgeOut)
	}

	env.orphanCnt = 2
	statusWarn := mustExecCLI(t, env, "", "--format", "human", "status", "--all")
	if !strings.Contains(statusWarn, "Warning: 2 orphan") || strings.Contains(statusWarn, "\033[33m") {
		t.Fatalf("expected Lip Gloss orphan warning in flowsk status without raw ANSI in non-TTY, got:\n%s", statusWarn)
	}
	pruneDry := mustExecCLI(t, env, "", "--format", "compact", "prune", "--dry-run")
	if !strings.Contains(pruneDry, "dry_run=true") || !strings.Contains(pruneDry, "total=2") {
		t.Fatalf("unexpected prune --dry-run output: %q", pruneDry)
	}
	pruneReal := mustExecCLI(t, env, "", "--format", "compact", "prune")
	if !strings.Contains(pruneReal, "dry_run=false") || !strings.Contains(pruneReal, "total=2") {
		t.Fatalf("unexpected prune output: %q", pruneReal)
	}
	statusClean := mustExecCLI(t, env, "", "--format", "human", "status", "--all")
	if strings.Contains(statusClean, "orphan") {
		t.Fatalf("expected zero orphan warning after prune, got:\n%s", statusClean)
	}

	// 6. Agent CLI ergonomics: `code -c`, positional `code [script]`, `goal create --slug --success-criteria`, `task show`, `task done`, and unknown subcommand rejection.
	codeShortOut := mustExecCLI(t, env, "", "code", "-c", "return 42;")
	if !strings.Contains(codeShortOut, "COMMITTED") || env.lastExecScriptIn.Script != "return 42;" {
		t.Fatalf("expected `flowsk code -c` to execute inline script 'return 42;', got script=%q out=%q", env.lastExecScriptIn.Script, codeShortOut)
	}
	mustExecCLI(t, env, "", "code", "return 99;")
	if env.lastExecScriptIn.Script != "return 99;" {
		t.Fatalf("expected positional `flowsk code [script]` to execute 'return 99;', got script=%q", env.lastExecScriptIn.Script)
	}
	if _, _, ambErr := execCLI(t, env, "", "code", "-e", "return 1;", "-c", "return 2;"); ambErr == nil {
		t.Fatal("expected error when both -e and -c are provided")
	}
	goalErgOut := mustExecCLI(t, env, "", "--project", "CORE", "--format", "json", "goal", "create", "Ergonomic Goal", "--slug", "ergo-goal", "--priority", "1", "--success-criteria", "Pass gate A", "--success-criteria", "Pass gate B")
	if !strings.Contains(goalErgOut, "Ergonomic Goal") {
		t.Fatalf("unexpected goal create output: %s", goalErgOut)
	}
	createdTaskJSON := mustExecCLI(t, env, "", "--project", "WEBUI", "--format", "json", "task", "create", "Close via done alias", "--priority", "1")
	var createdTask cli.TaskView
	if err := json.Unmarshal([]byte(createdTaskJSON), &createdTask); err != nil || createdTask.Key == "" {
		t.Fatalf("failed to parse created task JSON %q: %v", createdTaskJSON, err)
	}
	showOut := mustExecCLI(t, env, "", "--project", "WEBUI", "--format", "compact", "task", "show", createdTask.Key)
	if !strings.Contains(showOut, createdTask.Key) {
		t.Fatalf("expected `flowsk task show` alias to return %s, got: %q", createdTask.Key, showOut)
	}
	doneOut := mustExecCLI(t, env, "", "--project", "WEBUI", "--format", "compact", "task", "done", createdTask.Key, "--note", "Shipped")
	if !strings.Contains(strings.ToLower(doneOut), "done") || !strings.Contains(doneOut, "P1") {
		t.Fatalf("expected `flowsk task done` to transition %s to done with P1, got: %q", createdTask.Key, doneOut)
	}

	// 7. Composed Goal & Project Markdown blueprints (`flowsk goal bundle` / `flowsk project bundle`).
	mustExecCLI(t, env, "", "--project", "WEBUI", "project", "doc", "--set", "# WebUI Charter\n\nStrict OnPush architecture.")
	mustExecCLI(t, env, "", "--project", "WEBUI", "goal", "create", "Bundle Goal", "--slug", "bundle-goal", "--priority", "1", "--success-criteria", "Pass gate A")
	bundledCreateJSON := mustExecCLI(t, env, "", "--project", "WEBUI", "--format", "json", "task", "create", "Bundle Task Spec", "--objective", "bundle-goal", "--priority", "1")
	var bundledTask cli.TaskView
	if err := json.Unmarshal([]byte(bundledCreateJSON), &bundledTask); err != nil || bundledTask.Key == "" {
		t.Fatalf("failed to parse bundled task JSON %q: %v", bundledCreateJSON, err)
	}
	mustExecCLI(t, env, "", "--project", "WEBUI", "task", "doc", bundledTask.Key, "--set", "## Target Files\n- internal/cli/bundle.go")

	goalBundleOut := mustExecCLI(t, env, "", "--project", "WEBUI", "--format", "human", "goal", "bundle", "bundle-goal")
	for _, want := range []string{"# Goal Blueprint: Bundle Goal", "WebUI Charter", "Pass gate A", "internal/cli/bundle.go"} {
		if !strings.Contains(goalBundleOut, want) {
			t.Fatalf("expected %q in `flowsk goal bundle` output, got:\n%s", want, goalBundleOut)
		}
	}
	projBundleOut := mustExecCLI(t, env, "", "--project", "WEBUI", "--format", "human", "project", "bundle")
	if !strings.Contains(projBundleOut, "# Project Blueprint: WEBUI") || !strings.Contains(projBundleOut, "internal/cli/bundle.go") {
		t.Fatalf("unexpected `flowsk project bundle` output:\n%s", projBundleOut)
	}
	for _, unknownCmd := range [][]string{
		{"bogus-root"},
		{"task", "nonexistent-subcmd"},
		{"goal", "nonexistent-subcmd"},
		{"project", "nonexistent-subcmd"},
	} {
		if _, _, uErr := execCLI(t, env, "", unknownCmd...); uErr == nil {
			t.Fatalf("expected non-zero error for unknown command %v, got nil", unknownCmd)
		}
	}
}

type recordingFormatter struct {
	calls int
}

func (r *recordingFormatter) Format(w io.Writer, format cli.OutputFormat, maxTokens int, payload any) error {
	r.calls++
	_, err := fmt.Fprintf(w, "CUSTOM_FORMAT[%s|%d]:%T\n", format, maxTokens, payload)
	return err
}

// TestCustomOutputFormatterInjection verifies pluggable OutputFormatter wiring.
func TestCustomOutputFormatterInjection(t *testing.T) {
	t.Setenv("FLOWSK_HOME", filepath.Join(t.TempDir(), ".flowsk"))
	env := newInMemoryEnv()
	rf := &recordingFormatter{}
	env.customFormatter = rf

	out := mustExecCLI(t, env, "", "--format", "compact", "--max-tokens", "120", "workspace", "list")
	if rf.calls != 1 || !strings.Contains(out, "CUSTOM_FORMAT[compact|120]") {
		t.Fatalf("expected injected OutputFormatter to be invoked once, calls=%d out=%q", rf.calls, out)
	}
}
