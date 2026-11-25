// File local.go implements the in-process local SQLite/PostgreSQL engine and
// wires all domain services, the Cloudflare Code Mode JavaScript sandbox, and
// automatic first-run workspace/actor bootstrap for standalone CLI & server use.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/adapter/codemode"
	"github.com/mindmain/flowsk/internal/adapter/db"
	"github.com/mindmain/flowsk/internal/adapter/db/postgres"
	"github.com/mindmain/flowsk/internal/adapter/db/sqlite"
	"github.com/mindmain/flowsk/internal/adapter/mdstore"
	"github.com/mindmain/flowsk/internal/cli"
	"github.com/mindmain/flowsk/internal/domain/audit"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/objective"
	"github.com/mindmain/flowsk/internal/domain/project"
	"github.com/mindmain/flowsk/internal/domain/task"
	"github.com/mindmain/flowsk/internal/domain/tx"
	"github.com/mindmain/flowsk/internal/domain/workspace"
	"github.com/mindmain/flowsk/internal/language"
	"github.com/mindmain/flowsk/internal/transport/connectrpc/languagehandler"
)

// localEngine bundles an initialized DatabaseAdapter together with all resolved
// pure domain services and the Cloudflare Code Mode JavaScript engine.
type localEngine struct {
	adapter       db.DatabaseAdapter
	workspaceRepo workspace.Repository
	projectRepo   project.Repository
	objectiveRepo objective.Repository
	taskRepo      task.Repository
	auditRepo     audit.Repository

	workspaceSvc workspace.Service
	projectSvc   project.Service
	objectiveSvc objective.Service
	taskSvc      task.Service
	auditSvc     audit.Service
	codeMode     *codemode.Engine
	guideProv    language.GuideProvider
	mdStore      *mdstore.FileStore

	customMu     sync.RWMutex
	customStates map[uuid.UUID][]cli.WorkflowStateView
	customLabels map[uuid.UUID][]cli.LabelView
}

type projectDocBridge struct {
	repo project.Repository
}

func (b *projectDocBridge) ListProjectDocs(ctx context.Context, workspaceID uuid.UUID) ([]task.ProjectDocEntry, error) {
	items, err := b.repo.List(ctx, project.ListProjectsFilter{
		WorkspaceID: workspaceID,
		Limit:       1000,
	})
	if err != nil {
		return nil, err
	}
	out := make([]task.ProjectDocEntry, 0, len(items))
	for _, p := range items {
		out = append(out, task.ProjectDocEntry{
			ID:             p.ID,
			KeyPrefix:      p.KeyPrefix,
			HasMarkdownDoc: p.HasMarkdownDoc,
		})
	}
	return out, nil
}

func (b *projectDocBridge) ClearProjectMarkdownFlag(ctx context.Context, scope tx.Scope, workspaceID, projectID uuid.UUID) error {
	_, err := b.repo.WithScope(scope).SetHasMarkdownDoc(ctx, workspaceID, projectID, false)
	return err
}

func newSQLiteEngine(ctx context.Context, filePath string, autoMigrate bool) (*localEngine, error) {
	mdRoot := ""
	if filePath != ":memory:" && !strings.HasPrefix(filePath, "file::memory:") {
		mdRoot = filepath.Dir(filePath)
		if err := os.MkdirAll(mdRoot, 0o750); err != nil {
			return nil, fmt.Errorf("create flowsk data directory: %w", err)
		}
	}
	adapter, err := sqlite.NewAdapter(sqlite.Config{
		FilePath:         filePath,
		EnableWAL:        true,
		ForeignKeys:      true,
		BusyTimeout:      5 * time.Second,
		Synchronous:      sqlite.SynchronousNormal,
		MaxReadOpenConns: 8,
	})
	if err != nil {
		return nil, err
	}
	return buildEngineFromAdapter(ctx, adapter, autoMigrate, mdRoot)
}

func newPostgresEngine(ctx context.Context, dsn string, autoMigrate bool) (*localEngine, error) {
	adapter, err := postgres.NewAdapter(postgres.Config{
		DSN: dsn,
	})
	if err != nil {
		return nil, err
	}
	mdRoot := strings.TrimSpace(os.Getenv("FLOWSK_VOLUME_DIR"))
	if mdRoot == "" {
		mdRoot, _ = cli.DefaultDataDir()
	}
	return buildEngineFromAdapter(ctx, adapter, autoMigrate, mdRoot)
}

func buildEngineFromAdapter(ctx context.Context, adapter db.DatabaseAdapter, autoMigrate bool, mdRoot string) (*localEngine, error) {
	if autoMigrate {
		if _, err := adapter.Migrations().Up(ctx); err != nil {
			_ = adapter.Close()
			return nil, fmt.Errorf("apply embedded database migrations: %w", err)
		}
	}

	txMgr := adapter.TxManager()
	wsRepo := adapter.WorkspaceRepository()
	projRepo := adapter.ProjectRepository()
	objRepo := adapter.ObjectiveRepository()
	taskRepo := adapter.TaskRepository()
	auditRepo := adapter.AuditRepository()

	keyAlloc := task.KeyAllocatorFunc(func(allocCtx context.Context, scope tx.Scope, wsID, projID uuid.UUID) (string, error) {
		prefix, seq, seqErr := projRepo.WithScope(scope).NextTaskSequence(allocCtx, wsID, projID)
		if seqErr != nil {
			return "", seqErr
		}
		return fmt.Sprintf("%s-%d", prefix, seq), nil
	})

	var mdFileStore *mdstore.FileStore
	if strings.TrimSpace(mdRoot) != "" {
		if fsStore, fsErr := mdstore.NewFileStore(mdRoot); fsErr == nil {
			mdFileStore = fsStore
		}
	}

	projSvc := project.NewService(projRepo, txMgr)
	taskSvc := task.NewServiceWithKeyAllocator(taskRepo, txMgr, keyAlloc)
	if mdFileStore != nil {
		projSvc = projSvc.WithMarkdownStore(mdFileStore)
		taskSvc = taskSvc.WithMarkdownResources(mdFileStore, &projectDocBridge{repo: projRepo})
	}

	eng := &localEngine{
		adapter:       adapter,
		workspaceRepo: wsRepo,
		projectRepo:   projRepo,
		objectiveRepo: objRepo,
		taskRepo:      taskRepo,
		auditRepo:     auditRepo,
		workspaceSvc:  workspace.NewService(wsRepo, txMgr),
		projectSvc:    projSvc,
		objectiveSvc:  objective.NewService(objRepo, txMgr),
		taskSvc:       taskSvc,
		auditSvc:      audit.NewService(auditRepo, txMgr),
		codeMode: codemode.NewEngine(codemode.Dependencies{
			TxManager:     txMgr,
			ProjectRepo:   projRepo,
			ObjectiveRepo: objRepo,
			TaskRepo:      taskRepo,
			KeyAllocator:  keyAlloc,
			MarkdownStore: mdFileStore,
		}),
		guideProv:    language.NewGuideProvider(),
		mdStore:      mdFileStore,
		customStates: make(map[uuid.UUID][]cli.WorkflowStateView),
		customLabels: make(map[uuid.UUID][]cli.LabelView),
	}

	// Ensure default workspace exists out-of-the-box so first-time users never
	// need manual workspace creation commands before creating projects/tasks.
	if autoMigrate {
		_, _, _ = eng.ensureWorkspaceAndActor(ctx, "default", "", "")
	}

	return eng, nil
}

// WorkspaceService satisfies connectrpc handler ServicesDependencies.
func (e *localEngine) WorkspaceService() workspace.Service { return e.workspaceSvc }

// ProjectService satisfies connectrpc handler ServicesDependencies.
func (e *localEngine) ProjectService() project.Service { return e.projectSvc }

// ObjectiveService satisfies connectrpc handler ServicesDependencies.
func (e *localEngine) ObjectiveService() objective.Service { return e.objectiveSvc }

// TaskService satisfies connectrpc handler ServicesDependencies.
func (e *localEngine) TaskService() task.Service { return e.taskSvc }

// AuditService satisfies connectrpc handler ServicesDependencies.
func (e *localEngine) AuditService() audit.Service { return e.auditSvc }

// CodeExecutor satisfies languagehandler ServicesDependencies.
func (e *localEngine) CodeExecutor() languagehandler.CodeExecutor { return e.codeMode }

// GuideProvider satisfies languagehandler ServicesDependencies.
func (e *localEngine) GuideProvider() language.GuideProvider { return e.guideProv }

// ensureWorkspaceAndActor resolves a workspace by UUID or slug (auto-creating
// "default" when absent) and ensures the caller's actor handle is enrolled in
// that workspace, returning an authenticated context ready for domain calls.
func (e *localEngine) ensureWorkspaceAndActor(
	ctx context.Context,
	workspaceRef, rawHandle, rawKind string,
) (context.Context, workspace.Workspace, authctx.Identity) {
	wsRef := strings.TrimSpace(workspaceRef)
	if wsRef == "" {
		wsRef = "default"
	}

	handle := sanitizeActorHandle(rawHandle)
	kind := classifyActorKind(rawKind, handle)

	var ws workspace.Workspace
	var err error
	if parsedWS, parseErr := uuid.Parse(wsRef); parseErr == nil && parsedWS != uuid.Nil {
		ws, err = e.workspaceRepo.GetByID(ctx, parsedWS)
	} else {
		ws, err = e.workspaceRepo.GetBySlug(ctx, strings.ToLower(wsRef))
	}

	if err != nil && errors.Is(err, errs.ErrNotFound) && strings.EqualFold(wsRef, "default") {
		defaultWSID := deterministicUUID("workspace", "default")
		bootActorID := deterministicUUID("actor", defaultWSID.String(), handle)
		_ = e.adapter.TxManager().WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
			wsCandidate := workspace.Workspace{
				ID:                     defaultWSID,
				Slug:                   "default",
				Name:                   "Default Workspace",
				Description:            "Auto-provisioned local Flowsk workspace",
				DefaultLeaseTTLSeconds: 1800,
			}
			if createErr := e.workspaceRepo.WithScope(scope).Create(txCtx, wsCandidate); createErr != nil && !errors.Is(createErr, errs.ErrConflict) {
				return createErr
			}
			return e.workspaceRepo.WithScope(scope).UpsertMember(txCtx, workspace.WorkspaceMember{
				WorkspaceID: defaultWSID,
				ActorID:     bootActorID,
				Kind:        kind,
				Handle:      handle,
				DisplayName: handle,
				Role:        workspace.MemberRoleOwner,
				CreatedAt:   time.Now().UTC(),
			})
		})
		ws, _ = e.workspaceRepo.GetBySlug(ctx, "default")
	}

	if ws.ID == uuid.Nil {
		return ctx, workspace.Workspace{}, authctx.Identity{}
	}

	actorID := deterministicUUID("actor", ws.ID.String(), handle)
	if members, listErr := e.workspaceRepo.ListMembers(ctx, ws.ID); listErr == nil {
		found := false
		for _, m := range members {
			if strings.EqualFold(m.Handle, handle) {
				actorID = m.ActorID
				if m.Kind.Valid() && rawKind == "" {
					kind = m.Kind
				}
				found = true
				break
			}
		}
		if !found {
			_ = e.adapter.TxManager().WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
				return e.workspaceRepo.WithScope(scope).UpsertMember(txCtx, workspace.WorkspaceMember{
					WorkspaceID: ws.ID,
					ActorID:     actorID,
					Kind:        kind,
					Handle:      handle,
					DisplayName: handle,
					Role:        workspace.MemberRoleOwner,
					CreatedAt:   time.Now().UTC(),
				})
			})
		}
	}

	id := authctx.Identity{
		WorkspaceID: ws.ID,
		ActorID:     actorID,
		Kind:        kind,
		Handle:      handle,
	}
	return authctx.WithIdentity(ctx, id), ws, id
}

func (e *localEngine) resolveProject(ctx context.Context, workspaceID uuid.UUID, projectRef string) (project.Project, error) {
	ref := strings.TrimSpace(projectRef)
	if ref == "" {
		return project.Project{}, errs.Validation("app.resolveProject", "project identifier is required")
	}
	if id, err := uuid.Parse(ref); err == nil && id != uuid.Nil {
		return e.projectSvc.GetProject(ctx, workspaceID, id)
	}
	if p, err := e.projectSvc.GetProjectBySlug(ctx, workspaceID, strings.ToLower(ref)); err == nil {
		return p, nil
	}
	projects, err := e.projectSvc.ListProjects(ctx, project.ListProjectsFilter{
		WorkspaceID: workspaceID,
		Limit:       200,
	})
	if err != nil {
		return project.Project{}, err
	}
	for _, p := range projects {
		if strings.EqualFold(p.KeyPrefix, ref) || strings.EqualFold(p.Slug, ref) {
			return p, nil
		}
	}
	return project.Project{}, errs.New(errs.ErrNotFound, "app.resolveProject", fmt.Sprintf("project %q not found in workspace", ref))
}

func (e *localEngine) resolveObjective(ctx context.Context, workspaceID, projectID uuid.UUID, objRef string) (objective.Objective, error) {
	ref := strings.TrimSpace(objRef)
	if ref == "" {
		return objective.Objective{}, errs.Validation("app.resolveObjective", "objective identifier is required")
	}
	if id, err := uuid.Parse(ref); err == nil && id != uuid.Nil {
		return e.objectiveSvc.GetObjective(ctx, workspaceID, id)
	}
	if projectID != uuid.Nil {
		if o, err := e.objectiveSvc.GetObjectiveBySlug(ctx, workspaceID, projectID, strings.ToLower(ref)); err == nil {
			return o, nil
		}
	}
	items, err := e.objectiveSvc.ListObjectives(ctx, objective.ListObjectivesFilter{
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
		Limit:       200,
	})
	if err != nil {
		return objective.Objective{}, err
	}
	for _, o := range items {
		if strings.EqualFold(o.Slug, ref) || strings.EqualFold(o.Title, ref) {
			return o, nil
		}
	}
	return objective.Objective{}, errs.New(errs.ErrNotFound, "app.resolveObjective", fmt.Sprintf("objective %q not found", ref))
}

func (e *localEngine) resolveTask(ctx context.Context, workspaceID uuid.UUID, taskRef string) (task.Task, error) {
	return e.resolveTaskInProject(ctx, workspaceID, uuid.Nil, taskRef)
}

func (e *localEngine) resolveTaskInProject(ctx context.Context, workspaceID, projectID uuid.UUID, taskRef string) (task.Task, error) {
	ref := strings.TrimSpace(taskRef)
	if ref == "" {
		return task.Task{}, errs.Validation("app.resolveTask", "task identifier is required")
	}
	if id, err := uuid.Parse(ref); err == nil && id != uuid.Nil {
		return e.taskSvc.GetTask(ctx, workspaceID, id)
	}
	if t, err := e.taskSvc.GetTaskByKey(ctx, workspaceID, strings.ToUpper(ref)); err == nil {
		return t, nil
	}
	if projectID != uuid.Nil {
		if t, extErr := e.taskSvc.GetTaskByExternalID(ctx, workspaceID, projectID, ref); extErr == nil {
			return t, nil
		}
	}
	return e.taskSvc.GetTaskByKey(ctx, workspaceID, strings.ToUpper(ref))
}

func slugifyIdentifier(raw string) string {
	lower := strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	prevDash := false
	for _, r := range lower {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if b.Len() > 0 && !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
		if b.Len() >= 60 {
			break
		}
	}
	res := strings.Trim(b.String(), "-")
	if len(res) < 2 {
		return "item-" + uuid.NewString()[:6]
	}
	return res
}

func normalizeKeyPrefix(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(raw)) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
		if b.Len() >= 10 {
			break
		}
	}
	res := b.String()
	if len(res) > 0 && res[0] >= '0' && res[0] <= '9' {
		res = "P" + res
	}
	if len(res) < 2 {
		return "PRJ"
	}
	return res
}

func mapCLIPriorityToDomain(cliPrio int) task.Priority {
	switch cliPrio {
	case 1:
		return task.PriorityCritical
	case 2:
		return task.PriorityHigh
	case 3, 0:
		return task.PriorityMedium
	default:
		return task.PriorityLow
	}
}

func mapDomainPriorityToCLI(p task.Priority) int {
	switch p {
	case task.PriorityCritical:
		return 1
	case task.PriorityHigh:
		return 2
	case task.PriorityMedium:
		return 3
	default:
		return 4
	}
}

func parseCLIWorkflowStatus(raw string) task.Status {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "backlog", "triage":
		return task.StatusBacklog
	case "ready", "todo", "unstarted", "":
		return task.StatusReady
	case "in_progress", "in progress", "started", "doing":
		return task.StatusInProgress
	case "in_review", "in review", "review", "qa":
		return task.StatusInReview
	case "blocked":
		return task.StatusBlocked
	case "done", "completed", "complete":
		return task.StatusDone
	case "canceled", "cancelled":
		return task.StatusCancelled
	case "archived", "archive":
		return task.StatusArchived
	default:
		return task.StatusReady
	}
}

func displayWorkflowStateName(st task.Status) string {
	switch st {
	case task.StatusBacklog:
		return "Backlog"
	case task.StatusReady:
		return "Todo"
	case task.StatusInProgress:
		return "In Progress"
	case task.StatusInReview:
		return "In Review"
	case task.StatusBlocked:
		return "Blocked"
	case task.StatusDone:
		return "Done"
	case task.StatusCancelled:
		return "Canceled"
	case task.StatusArchived:
		return "Archived"
	default:
		return string(st)
	}
}

func canonicalCategoryForStatus(st task.Status) string {
	switch st {
	case task.StatusBacklog:
		return "backlog"
	case task.StatusReady:
		return "todo"
	case task.StatusInProgress:
		return "in_progress"
	case task.StatusInReview:
		return "review"
	case task.StatusBlocked:
		return "blocked"
	case task.StatusDone:
		return "done"
	case task.StatusCancelled:
		return "canceled"
	case task.StatusArchived:
		return "archived"
	default:
		return "todo"
	}
}

// =============================================================================
// Dual-Mode Dispatch & Local Implementations (`cli.*Client`)
// =============================================================================

func (c *dualModeClient) CreateWorkspace(ctx context.Context, in cli.CreateWorkspaceInput) (cli.WorkspaceView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteCreateWorkspace(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.WorkspaceView{}, err
	}
	handle := sanitizeActorHandle(firstNonEmpty(in.Actor, opts.Actor))
	bootID := deterministicUUID("actor", "create-ws", handle)
	authCtx := authctx.WithIdentity(ctx, authctx.Identity{
		WorkspaceID: uuid.New(),
		ActorID:     bootID,
		Kind:        classifyActorKind("", handle),
		Handle:      handle,
	})
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = in.Slug
	}
	ws, err := eng.workspaceSvc.CreateWorkspace(authCtx, workspace.CreateWorkspaceInput{
		Slug:        strings.ToLower(strings.TrimSpace(in.Slug)),
		Name:        name,
		Description: in.Description,
	})
	if err != nil {
		return cli.WorkspaceView{}, err
	}
	return toWorkspaceView(ws), nil
}

func (c *dualModeClient) ListWorkspaces(ctx context.Context, in cli.ListWorkspacesInput) (cli.WorkspaceListResult, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteListWorkspaces(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.WorkspaceListResult{}, err
	}
	authCtx, _, _ := eng.ensureWorkspaceAndActor(ctx, opts.Workspace, opts.Actor, "")
	items, err := eng.workspaceSvc.ListWorkspaces(authCtx)
	if err != nil {
		return cli.WorkspaceListResult{}, err
	}
	views := make([]cli.WorkspaceView, 0, len(items))
	for _, ws := range items {
		views = append(views, toWorkspaceView(ws))
	}
	return cli.WorkspaceListResult{Items: views}, nil
}

func (c *dualModeClient) GetWorkspace(ctx context.Context, workspaceRef string) (cli.WorkspaceView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteGetWorkspace(ctx, opts, workspaceRef)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.WorkspaceView{}, err
	}
	ref := firstNonEmpty(workspaceRef, opts.Workspace, "default")
	_, ws, _ := eng.ensureWorkspaceAndActor(ctx, ref, opts.Actor, "")
	if ws.ID == uuid.Nil {
		return cli.WorkspaceView{}, errs.New(errs.ErrNotFound, "app.GetWorkspace", fmt.Sprintf("workspace %q not found", ref))
	}
	return toWorkspaceView(ws), nil
}

func (c *dualModeClient) RegisterActor(ctx context.Context, in cli.RegisterActorInput) (cli.ActorView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteRegisterActor(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.ActorView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), opts.Actor, "")
	if ws.ID == uuid.Nil {
		return cli.ActorView{}, errs.New(errs.ErrNotFound, "app.RegisterActor", "workspace not found")
	}
	handle := sanitizeActorHandle(in.Handle)
	kind := classifyActorKind(in.Kind, handle)
	display := firstNonEmpty(in.DisplayName, handle)
	role := workspace.MemberRoleContributor
	switch strings.ToLower(strings.TrimSpace(in.Role)) {
	case "owner", "lead":
		role = workspace.MemberRoleOwner
	case "maintainer":
		role = workspace.MemberRoleMaintainer
	case "reader":
		role = workspace.MemberRoleReader
	}
	member, err := eng.workspaceSvc.UpsertMember(authCtx, workspace.UpsertMemberInput{
		WorkspaceID: ws.ID,
		ActorID:     deterministicUUID("actor", ws.ID.String(), handle),
		Kind:        kind,
		Handle:      handle,
		DisplayName: display,
		Role:        role,
	})
	if err != nil {
		return cli.ActorView{}, err
	}
	return cli.ActorView{
		ID:          member.ActorID.String(),
		WorkspaceID: ws.ID.String(),
		Handle:      member.Handle,
		DisplayName: member.DisplayName,
		Kind:        string(member.Kind),
		Model:       in.Model,
		Role:        string(member.Role),
		CreatedAt:   member.CreatedAt,
	}, nil
}

func (c *dualModeClient) ListActors(ctx context.Context, in cli.ListActorsInput) (cli.ActorListResult, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteListActors(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.ActorListResult{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), opts.Actor, "")
	if ws.ID == uuid.Nil {
		return cli.ActorListResult{}, errs.New(errs.ErrNotFound, "app.ListActors", "workspace not found")
	}
	members, err := eng.workspaceSvc.ListMembers(authCtx, ws.ID)
	if err != nil {
		return cli.ActorListResult{}, err
	}
	kindFilter := strings.ToLower(strings.TrimSpace(in.Kind))
	var out []cli.ActorView
	for _, m := range members {
		if kindFilter != "" && string(m.Kind) != kindFilter {
			continue
		}
		out = append(out, cli.ActorView{
			ID:          m.ActorID.String(),
			WorkspaceID: ws.ID.String(),
			Handle:      m.Handle,
			DisplayName: m.DisplayName,
			Kind:        string(m.Kind),
			Role:        string(m.Role),
			CreatedAt:   m.CreatedAt,
		})
	}
	return cli.ActorListResult{Items: out}, nil
}

func (c *dualModeClient) CreateProject(ctx context.Context, in cli.CreateProjectInput) (cli.ProjectView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteCreateProject(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.ProjectView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), firstNonEmpty(in.Actor, opts.Actor), "")
	if ws.ID == uuid.Nil {
		return cli.ProjectView{}, errs.New(errs.ErrNotFound, "app.CreateProject", "workspace not found")
	}
	keyPrefix := normalizeKeyPrefix(in.Key)
	slug := slugifyIdentifier(in.Key)
	name := firstNonEmpty(in.Name, keyPrefix)
	p, err := eng.projectSvc.CreateProject(authCtx, project.CreateProjectInput{
		WorkspaceID: ws.ID,
		Slug:        slug,
		KeyPrefix:   keyPrefix,
		Name:        name,
		Description: in.Description,
	})
	if err != nil {
		return cli.ProjectView{}, err
	}
	return toProjectView(p), nil
}

func (c *dualModeClient) ListProjects(ctx context.Context, in cli.ListProjectsInput) (cli.ProjectListResult, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteListProjects(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.ProjectListResult{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), opts.Actor, "")
	if ws.ID == uuid.Nil {
		return cli.ProjectListResult{}, errs.New(errs.ErrNotFound, "app.ListProjects", "workspace not found")
	}
	items, err := eng.projectSvc.ListProjects(authCtx, project.ListProjectsFilter{
		WorkspaceID: ws.ID,
		Limit:       int32(max(1, in.Limit)),
	})
	if err != nil {
		return cli.ProjectListResult{}, err
	}
	var out []cli.ProjectView
	for _, p := range items {
		if !in.IncludeArchived && p.Status == project.StatusArchived {
			continue
		}
		out = append(out, toProjectView(p))
	}
	return cli.ProjectListResult{Items: out}, nil
}

func (c *dualModeClient) GetProject(ctx context.Context, workspaceRef, projectRef string) (cli.ProjectView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteGetProject(ctx, opts, workspaceRef, projectRef)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.ProjectView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(workspaceRef, opts.Workspace), opts.Actor, "")
	p, err := eng.resolveProject(authCtx, ws.ID, projectRef)
	if err != nil {
		return cli.ProjectView{}, err
	}
	return toProjectView(p), nil
}

func (c *dualModeClient) UpdateProject(ctx context.Context, in cli.UpdateProjectInput) (cli.ProjectView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteUpdateProject(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.ProjectView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), firstNonEmpty(in.Actor, opts.Actor), "")
	target, err := eng.resolveProject(authCtx, ws.ID, in.Project)
	if err != nil {
		return cli.ProjectView{}, err
	}
	updated, err := eng.projectSvc.UpdateProject(authCtx, project.UpdateProjectInput{
		WorkspaceID: ws.ID,
		ProjectID:   target.ID,
		Name:        in.Name,
		Description: in.Description,
	})
	if err != nil {
		return cli.ProjectView{}, err
	}
	return toProjectView(updated), nil
}

func (c *dualModeClient) ArchiveProject(ctx context.Context, in cli.ArchiveProjectInput) (cli.ProjectView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteArchiveProject(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.ProjectView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), firstNonEmpty(in.Actor, opts.Actor), "")
	target, err := eng.resolveProject(authCtx, ws.ID, in.Project)
	if err != nil {
		return cli.ProjectView{}, err
	}
	st := project.StatusArchived
	updated, err := eng.projectSvc.UpdateProject(authCtx, project.UpdateProjectInput{
		WorkspaceID: ws.ID,
		ProjectID:   target.ID,
		Status:      &st,
	})
	if err != nil {
		return cli.ProjectView{}, err
	}
	return toProjectView(updated), nil
}

func (c *dualModeClient) DeleteProject(ctx context.Context, in cli.DeleteProjectInput) (cli.DeleteProjectResultView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteDeleteProject(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.DeleteProjectResultView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), firstNonEmpty(in.Actor, opts.Actor), "")
	target, err := eng.resolveProject(authCtx, ws.ID, in.Project)
	if err != nil {
		return cli.DeleteProjectResultView{}, err
	}
	if err := eng.projectSvc.DeleteProject(authCtx, ws.ID, target.ID); err != nil {
		return cli.DeleteProjectResultView{}, err
	}
	return cli.DeleteProjectResultView{
		WorkspaceID:      ws.ID.String(),
		DeletedProjectID: target.ID.String(),
		DeletedKey:       target.KeyPrefix,
	}, nil
}

func (c *dualModeClient) BindSubproject(ctx context.Context, in cli.BindSubprojectInput) (cli.ProjectBindingView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteBindSubproject(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.ProjectBindingView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), firstNonEmpty(in.Actor, opts.Actor), "")
	parent, err := eng.resolveProject(authCtx, ws.ID, in.ParentProject)
	if err != nil {
		return cli.ProjectBindingView{}, err
	}
	child, err := eng.resolveProject(authCtx, ws.ID, in.ChildProject)
	if err != nil {
		return cli.ProjectBindingView{}, err
	}
	bound, err := eng.projectSvc.BindSubproject(authCtx, project.BindSubprojectInput{
		WorkspaceID:     ws.ID,
		ParentProjectID: parent.ID,
		ChildProjectID:  child.ID,
		ScopeSubpath:    in.ScopeSubpath,
	})
	if err != nil {
		return cli.ProjectBindingView{}, err
	}
	return cli.ProjectBindingView{
		ID:              bound.ID.String(),
		WorkspaceID:     bound.WorkspaceID.String(),
		ParentProjectID: bound.ParentProjectID.String(),
		ParentKey:       parent.KeyPrefix,
		ChildProjectID:  bound.ChildProjectID.String(),
		ChildKey:        child.KeyPrefix,
		ScopeSubpath:    bound.ScopeSubpath,
		CreatedAt:       bound.CreatedAt,
	}, nil
}

func (c *dualModeClient) UnbindSubproject(ctx context.Context, in cli.UnbindSubprojectInput) (cli.UnbindSubprojectResultView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteUnbindSubproject(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.UnbindSubprojectResultView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), firstNonEmpty(in.Actor, opts.Actor), "")
	parent, err := eng.resolveProject(authCtx, ws.ID, in.ParentProject)
	if err != nil {
		return cli.UnbindSubprojectResultView{}, err
	}
	child, err := eng.resolveProject(authCtx, ws.ID, in.ChildProject)
	if err != nil {
		return cli.UnbindSubprojectResultView{}, err
	}
	removed, err := eng.projectSvc.UnbindSubproject(authCtx, project.UnbindSubprojectInput{
		WorkspaceID:     ws.ID,
		ParentProjectID: parent.ID,
		ChildProjectID:  child.ID,
	})
	if err != nil {
		return cli.UnbindSubprojectResultView{}, err
	}
	return cli.UnbindSubprojectResultView{
		ParentProject: parent.KeyPrefix,
		ChildProject:  child.KeyPrefix,
		Removed:       removed,
	}, nil
}

func (c *dualModeClient) ListProjectBindings(ctx context.Context, in cli.ListProjectBindingsInput) ([]cli.ProjectBindingView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteListProjectBindings(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return nil, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), opts.Actor, "")
	if ws.ID == uuid.Nil {
		return nil, nil
	}
	filter := project.ListBindingsFilter{WorkspaceID: ws.ID}
	if strings.TrimSpace(in.ParentProject) != "" {
		p, pErr := eng.resolveProject(authCtx, ws.ID, in.ParentProject)
		if pErr != nil {
			return nil, pErr
		}
		filter.ParentProjectID = &p.ID
	}
	if strings.TrimSpace(in.ChildProject) != "" {
		ch, cErr := eng.resolveProject(authCtx, ws.ID, in.ChildProject)
		if cErr != nil {
			return nil, cErr
		}
		filter.ChildProjectID = &ch.ID
	}
	bindings, err := eng.projectSvc.ListBindings(authCtx, filter)
	if err != nil {
		return nil, err
	}
	allProjects, _ := eng.projectSvc.ListProjects(authCtx, project.ListProjectsFilter{
		WorkspaceID: ws.ID,
		Limit:       200,
	})
	keyByID := make(map[uuid.UUID]string, len(allProjects))
	for _, p := range allProjects {
		keyByID[p.ID] = p.KeyPrefix
	}
	out := make([]cli.ProjectBindingView, 0, len(bindings))
	for _, b := range bindings {
		out = append(out, cli.ProjectBindingView{
			ID:              b.ID.String(),
			WorkspaceID:     b.WorkspaceID.String(),
			ParentProjectID: b.ParentProjectID.String(),
			ParentKey:       keyByID[b.ParentProjectID],
			ChildProjectID:  b.ChildProjectID.String(),
			ChildKey:        keyByID[b.ChildProjectID],
			ScopeSubpath:    b.ScopeSubpath,
			CreatedAt:       b.CreatedAt,
		})
	}
	return out, nil
}

func (c *dualModeClient) ListStates(ctx context.Context, workspaceRef, projectRef string) ([]cli.WorkflowStateView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteListStates(ctx, opts, workspaceRef, projectRef)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return nil, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(workspaceRef, opts.Workspace), opts.Actor, "")
	proj, err := eng.resolveProject(authCtx, ws.ID, projectRef)
	if err != nil {
		return nil, err
	}
	base := defaultCLIWorkflowStates(proj.ID.String())
	eng.customMu.RLock()
	extra := append([]cli.WorkflowStateView(nil), eng.customStates[proj.ID]...)
	eng.customMu.RUnlock()
	out := append(base, extra...)
	sort.Slice(out, func(i, j int) bool { return out[i].Position < out[j].Position })
	return out, nil
}

func (c *dualModeClient) CreateState(ctx context.Context, in cli.CreateStateInput) (cli.WorkflowStateView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteCreateState(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.WorkflowStateView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), firstNonEmpty(in.Actor, opts.Actor), "")
	proj, err := eng.resolveProject(authCtx, ws.ID, in.Project)
	if err != nil {
		return cli.WorkflowStateView{}, err
	}
	st := cli.WorkflowStateView{
		ID:        uuid.NewString(),
		ProjectID: proj.ID.String(),
		Name:      in.Name,
		Category:  in.Category,
		Position:  in.Position,
		Color:     in.Color,
	}
	eng.customMu.Lock()
	eng.customStates[proj.ID] = append(eng.customStates[proj.ID], st)
	eng.customMu.Unlock()
	return st, nil
}

func (c *dualModeClient) ListLabels(ctx context.Context, workspaceRef, projectRef string) ([]cli.LabelView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteListLabels(ctx, opts, workspaceRef, projectRef)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return nil, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(workspaceRef, opts.Workspace), opts.Actor, "")
	proj, err := eng.resolveProject(authCtx, ws.ID, projectRef)
	if err != nil {
		return nil, err
	}
	eng.customMu.RLock()
	out := append([]cli.LabelView(nil), eng.customLabels[proj.ID]...)
	eng.customMu.RUnlock()
	return out, nil
}

func (c *dualModeClient) CreateLabel(ctx context.Context, in cli.CreateLabelInput) (cli.LabelView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteCreateLabel(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.LabelView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), firstNonEmpty(in.Actor, opts.Actor), "")
	proj, err := eng.resolveProject(authCtx, ws.ID, in.Project)
	if err != nil {
		return cli.LabelView{}, err
	}
	lbl := cli.LabelView{
		ID:          uuid.NewString(),
		ProjectID:   proj.ID.String(),
		Name:        in.Name,
		Color:       in.Color,
		Description: in.Description,
	}
	eng.customMu.Lock()
	eng.customLabels[proj.ID] = append(eng.customLabels[proj.ID], lbl)
	eng.customMu.Unlock()
	return lbl, nil
}

func (c *dualModeClient) CreateObjective(ctx context.Context, in cli.CreateObjectiveInput) (cli.ObjectiveView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteCreateObjective(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.ObjectiveView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), firstNonEmpty(in.Actor, opts.Actor), "")
	proj, err := eng.resolveProject(authCtx, ws.ID, in.Project)
	if err != nil {
		return cli.ObjectiveView{}, err
	}
	var targetDate *time.Time
	if strings.TrimSpace(in.TargetDate) != "" {
		parsed, parseErr := time.Parse("2006-01-02", strings.TrimSpace(in.TargetDate))
		if parseErr != nil {
			return cli.ObjectiveView{}, errs.Validation("app.CreateObjective", fmt.Sprintf("invalid --target-date %q: expected YYYY-MM-DD", in.TargetDate))
		}
		utc := parsed.UTC()
		targetDate = &utc
	}
	var criteria []string
	if strings.TrimSpace(in.Summary) != "" {
		criteria = []string{strings.TrimSpace(in.Summary)}
	}
	obj, err := eng.objectiveSvc.CreateObjective(authCtx, objective.CreateObjectiveInput{
		WorkspaceID:     ws.ID,
		ProjectID:       proj.ID,
		Slug:            slugifyIdentifier(in.Title),
		Title:           in.Title,
		Summary:         in.Summary,
		SuccessCriteria: criteria,
		Status:          objective.StatusActive,
		PriorityRank:    int32(max(1, in.Priority)),
		TargetDate:      targetDate,
	})
	if err != nil {
		return cli.ObjectiveView{}, err
	}
	view := toObjectiveView(obj, firstNonEmpty(in.Kind, "goal"), in.Owner)
	eng.populateObjectiveTaskCounts(authCtx, ws.ID, proj.ID, &view, obj.ID)
	return view, nil
}

func (c *dualModeClient) ListObjectives(ctx context.Context, in cli.ListObjectivesInput) (cli.ObjectiveListResult, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteListObjectives(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.ObjectiveListResult{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), opts.Actor, "")
	var projID uuid.UUID
	if strings.TrimSpace(in.Project) != "" {
		proj, projErr := eng.resolveProject(authCtx, ws.ID, in.Project)
		if projErr != nil {
			return cli.ObjectiveListResult{}, projErr
		}
		projID = proj.ID
	}
	items, err := eng.objectiveSvc.ListObjectives(authCtx, objective.ListObjectivesFilter{
		WorkspaceID: ws.ID,
		ProjectID:   projID,
		Limit:       int32(max(1, in.Limit)),
	})
	if err != nil {
		return cli.ObjectiveListResult{}, err
	}
	var out []cli.ObjectiveView
	statusFilter := strings.ToLower(strings.TrimSpace(in.Status))
	for _, item := range items {
		v := toObjectiveView(item, "goal", "")
		if statusFilter != "" && !strings.EqualFold(v.Status, statusFilter) {
			continue
		}
		eng.populateObjectiveTaskCounts(authCtx, ws.ID, item.ProjectID, &v, item.ID)
		out = append(out, v)
	}
	return cli.ObjectiveListResult{Items: out}, nil
}

func (c *dualModeClient) GetObjective(ctx context.Context, workspaceRef, projectRef, objectiveRef string) (cli.ObjectiveView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteGetObjective(ctx, opts, workspaceRef, projectRef, objectiveRef)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.ObjectiveView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(workspaceRef, opts.Workspace), opts.Actor, "")
	var projID uuid.UUID
	if strings.TrimSpace(projectRef) != "" {
		if p, pErr := eng.resolveProject(authCtx, ws.ID, projectRef); pErr == nil {
			projID = p.ID
		}
	}
	obj, err := eng.resolveObjective(authCtx, ws.ID, projID, objectiveRef)
	if err != nil {
		return cli.ObjectiveView{}, err
	}
	v := toObjectiveView(obj, "goal", "")
	eng.populateObjectiveTaskCounts(authCtx, ws.ID, obj.ProjectID, &v, obj.ID)
	return v, nil
}

func (c *dualModeClient) UpdateObjective(ctx context.Context, in cli.UpdateObjectiveInput) (cli.ObjectiveView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteUpdateObjective(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.ObjectiveView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), firstNonEmpty(in.Actor, opts.Actor), "")
	var projID uuid.UUID
	if strings.TrimSpace(in.Project) != "" {
		if p, pErr := eng.resolveProject(authCtx, ws.ID, in.Project); pErr == nil {
			projID = p.ID
		}
	}
	target, err := eng.resolveObjective(authCtx, ws.ID, projID, in.Objective)
	if err != nil {
		return cli.ObjectiveView{}, err
	}
	var stPtr *objective.Status
	if in.Status != nil {
		st := mapCLIObjectiveStatus(*in.Status)
		stPtr = &st
	}
	var prioPtr *int32
	if in.Priority != nil {
		p := int32(*in.Priority)
		prioPtr = &p
	}
	var targetDate *time.Time
	if in.TargetDate != nil && strings.TrimSpace(*in.TargetDate) != "" {
		parsed, parseErr := time.Parse("2006-01-02", strings.TrimSpace(*in.TargetDate))
		if parseErr != nil {
			return cli.ObjectiveView{}, errs.Validation("app.UpdateObjective", "invalid --target-date: expected YYYY-MM-DD")
		}
		utc := parsed.UTC()
		targetDate = &utc
	}
	updated, err := eng.objectiveSvc.UpdateObjective(authCtx, objective.UpdateObjectiveInput{
		WorkspaceID:  ws.ID,
		ObjectiveID:  target.ID,
		Title:        in.Title,
		Summary:      in.Summary,
		Status:       stPtr,
		PriorityRank: prioPtr,
		TargetDate:   targetDate,
	})
	if err != nil {
		return cli.ObjectiveView{}, err
	}
	v := toObjectiveView(updated, "goal", "")
	eng.populateObjectiveTaskCounts(authCtx, ws.ID, updated.ProjectID, &v, updated.ID)
	return v, nil
}

func (c *dualModeClient) CloseObjective(ctx context.Context, in cli.CloseObjectiveInput) (cli.ObjectiveView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteCloseObjective(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.ObjectiveView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), firstNonEmpty(in.Actor, opts.Actor), "")
	var projID uuid.UUID
	if strings.TrimSpace(in.Project) != "" {
		if p, pErr := eng.resolveProject(authCtx, ws.ID, in.Project); pErr == nil {
			projID = p.ID
		}
	}
	target, err := eng.resolveObjective(authCtx, ws.ID, projID, in.Objective)
	if err != nil {
		return cli.ObjectiveView{}, err
	}
	st := objective.StatusAchieved
	if strings.EqualFold(in.Resolution, "canceled") || strings.EqualFold(in.Resolution, "superseded") || strings.EqualFold(in.Resolution, "dropped") {
		st = objective.StatusDropped
	}
	updated, err := eng.objectiveSvc.UpdateObjective(authCtx, objective.UpdateObjectiveInput{
		WorkspaceID: ws.ID,
		ObjectiveID: target.ID,
		Status:      &st,
	})
	if err != nil {
		return cli.ObjectiveView{}, err
	}
	v := toObjectiveView(updated, "goal", "")
	v.Resolution = in.Resolution
	eng.populateObjectiveTaskCounts(authCtx, ws.ID, updated.ProjectID, &v, updated.ID)
	return v, nil
}

func (c *dualModeClient) CreateTask(ctx context.Context, in cli.CreateTaskInput) (cli.TaskView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteCreateTask(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.TaskView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), firstNonEmpty(in.Actor, opts.Actor), "")
	proj, err := eng.resolveProject(authCtx, ws.ID, in.Project)
	if err != nil {
		return cli.TaskView{}, err
	}

	var objID *uuid.UUID
	if strings.TrimSpace(in.Objective) != "" {
		obj, objErr := eng.resolveObjective(authCtx, ws.ID, proj.ID, in.Objective)
		if objErr != nil {
			return cli.TaskView{}, objErr
		}
		idCopy := obj.ID
		objID = &idCopy
	}

	var parentID *uuid.UUID
	if strings.TrimSpace(in.Parent) != "" {
		pt, ptErr := eng.resolveTask(authCtx, ws.ID, in.Parent)
		if ptErr != nil {
			return cli.TaskView{}, ptErr
		}
		idCopy := pt.ID
		parentID = &idCopy
	}

	var assigneeID *uuid.UUID
	if strings.TrimSpace(in.Assignee) != "" {
		_, _, assigneeIdent := eng.ensureWorkspaceAndActor(ctx, ws.ID.String(), in.Assignee, "")
		if assigneeIdent.ActorID != uuid.Nil {
			idCopy := assigneeIdent.ActorID
			assigneeID = &idCopy
		}
	}

	var criteria []string
	if strings.TrimSpace(in.Acceptance) != "" {
		criteria = strings.Split(in.Acceptance, "\n")
	}

	created, err := eng.taskSvc.CreateTask(authCtx, task.CreateTaskInput{
		WorkspaceID:        ws.ID,
		ProjectID:          proj.ID,
		ObjectiveID:        objID,
		ParentTaskID:       parentID,
		ExternalID:         strings.TrimSpace(in.ExternalID),
		Kind:               task.KindFeature,
		Title:              in.Title,
		Description:        in.Description,
		AcceptanceCriteria: criteria,
		Status:             parseCLIWorkflowStatus(in.State),
		Priority:           mapCLIPriorityToDomain(in.Priority),
		AssigneeActorID:    assigneeID,
		Tags:               in.Labels,
	})
	if err != nil {
		return cli.TaskView{}, err
	}

	for _, depRef := range in.DependsOn {
		if strings.TrimSpace(depRef) == "" {
			continue
		}
		blockerTask, depErr := eng.resolveTask(authCtx, ws.ID, depRef)
		if depErr != nil {
			return cli.TaskView{}, depErr
		}
		if _, relErr := eng.taskSvc.AddRelation(authCtx, task.AddRelationInput{
			WorkspaceID:  ws.ID,
			SourceTaskID: blockerTask.ID,
			TargetTaskID: created.ID,
			Type:         task.RelationBlocks,
		}); relErr != nil {
			return cli.TaskView{}, relErr
		}
	}

	reloaded, err := eng.taskSvc.GetTask(authCtx, ws.ID, created.ID)
	if err != nil {
		reloaded = created
	}
	return eng.buildTaskView(authCtx, ws.ID, reloaded, true, true, true), nil
}

func (c *dualModeClient) ListTasks(ctx context.Context, in cli.ListTasksInput) (cli.TaskListResult, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteListTasks(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.TaskListResult{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), opts.Actor, "")
	var projID uuid.UUID
	if strings.TrimSpace(in.Project) != "" {
		proj, projErr := eng.resolveProject(authCtx, ws.ID, in.Project)
		if projErr != nil {
			return cli.TaskListResult{}, projErr
		}
		projID = proj.ID
	}

	var objID *uuid.UUID
	if strings.TrimSpace(in.Objective) != "" {
		obj, objErr := eng.resolveObjective(authCtx, ws.ID, projID, in.Objective)
		if objErr != nil {
			return cli.TaskListResult{}, objErr
		}
		idCopy := obj.ID
		objID = &idCopy
	}

	var parentID *uuid.UUID
	if strings.TrimSpace(in.Parent) != "" {
		pt, ptErr := eng.resolveTask(authCtx, ws.ID, in.Parent)
		if ptErr != nil {
			return cli.TaskListResult{}, ptErr
		}
		idCopy := pt.ID
		parentID = &idCopy
	}

	parsedQ := language.ParseSearchQuery(in.Query)

	var statuses []task.Status
	if strings.TrimSpace(in.State) != "" {
		statuses = []task.Status{parseCLIWorkflowStatus(in.State)}
	} else if len(parsedQ.Statuses) > 0 {
		statuses = parsedQ.Statuses
	} else {
		statuses = []task.Status{
			task.StatusBacklog,
			task.StatusReady,
			task.StatusInProgress,
			task.StatusInReview,
			task.StatusBlocked,
			task.StatusDone,
			task.StatusCancelled,
		}
		if in.IncludeArchived {
			statuses = append(statuses, task.StatusArchived)
		}
	}

	reqTags := append([]string(nil), in.Labels...)
	reqTags = append(reqTags, parsedQ.Tags...)
	unclaimedOnly := in.Unclaimed || (parsedQ.Claimed != nil && !*parsedQ.Claimed)
	extFilter := firstNonEmpty(in.ExternalID, parsedQ.ExternalID)

	rows, err := eng.taskSvc.PollWorkQueue(authCtx, task.AgentWorkQueueFilter{
		WorkspaceID:   ws.ID,
		ProjectID:     projID,
		ObjectiveID:   objID,
		ParentTaskID:  parentID,
		ExternalID:    extFilter,
		Statuses:      statuses,
		RequiredTags:  reqTags,
		OnlyUnclaimed: unclaimedOnly,
		Limit:         int32(max(1, min(in.Limit, 100))),
	})
	if err != nil {
		return cli.TaskListResult{}, err
	}

	var out []cli.TaskSummaryView
	for _, r := range rows {
		if extFilter != "" && r.ExternalID != extFilter {
			continue
		}
		isBlocked := r.UnresolvedBlockerCount > 0 || r.Status == task.StatusBlocked
		if in.Blocked && !isBlocked {
			continue
		}
		if parsedQ.Blocked != nil && isBlocked != *parsedQ.Blocked {
			continue
		}
		if parsedQ.Claimed != nil && *parsedQ.Claimed && strings.TrimSpace(r.HolderHandle) == "" {
			continue
		}
		if in.Priority >= 0 && mapDomainPriorityToCLI(r.Priority) != in.Priority {
			continue
		}
		if parsedQ.Priority != nil && r.Priority != *parsedQ.Priority {
			continue
		}
		effActor := firstNonEmpty(in.Assignee, parsedQ.Actor)
		if effActor != "" && !strings.EqualFold(r.HolderHandle, effActor) {
			continue
		}
		if parsedQ.Objective != "" && !strings.EqualFold(r.ObjectiveSlug, parsedQ.Objective) {
			continue
		}
		if parsedQ.Parent != "" && !strings.EqualFold(r.ParentKey, parsedQ.Parent) {
			continue
		}
		if len(parsedQ.Terms) > 0 {
			full, gErr := eng.taskSvc.GetTask(authCtx, ws.ID, r.ID)
			var commentText string
			if cms, cErr := eng.taskSvc.ListComments(authCtx, ws.ID, r.ID, 20); cErr == nil {
				var cb strings.Builder
				for _, cm := range cms {
					cb.WriteString(cm.Body)
					cb.WriteByte('\n')
				}
				commentText = cb.String()
			}
			if gErr == nil {
				if !parsedQ.MatchesText(r.Key, r.ExternalID, r.Title, full.Description, strings.Join(full.AcceptanceCriteriaCopy(), " "), strings.Join(r.Tags, " "), commentText) {
					continue
				}
			} else if !parsedQ.MatchesText(r.Key, r.ExternalID, r.Title, strings.Join(r.Tags, " ")) {
				continue
			}
		}
		out = append(out, toTaskSummaryView(r))
	}
	return cli.TaskListResult{Items: out}, nil
}

func (c *dualModeClient) GetTask(ctx context.Context, in cli.GetTaskInput) (cli.TaskView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteGetTask(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.TaskView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), opts.Actor, "")
	var projID uuid.UUID
	if strings.TrimSpace(in.Project) != "" {
		if p, pErr := eng.resolveProject(authCtx, ws.ID, in.Project); pErr == nil {
			projID = p.ID
		}
	}
	t, err := eng.resolveTaskInProject(authCtx, ws.ID, projID, in.Task)
	if err != nil {
		return cli.TaskView{}, err
	}
	return eng.buildTaskView(authCtx, ws.ID, t, in.IncludeDeps, in.IncludeSubtasks, in.IncludeComments), nil
}

func (c *dualModeClient) UpdateTask(ctx context.Context, in cli.UpdateTaskInput) (cli.TaskView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteUpdateTask(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.TaskView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), firstNonEmpty(in.Actor, opts.Actor), "")
	var projID uuid.UUID
	if strings.TrimSpace(in.Project) != "" {
		if p, pErr := eng.resolveProject(authCtx, ws.ID, in.Project); pErr == nil {
			projID = p.ID
		}
	}
	target, err := eng.resolveTaskInProject(authCtx, ws.ID, projID, in.Task)
	if err != nil {
		return cli.TaskView{}, err
	}

	upd := task.UpdateTaskInput{
		WorkspaceID: ws.ID,
		TaskID:      target.ID,
		ExternalID:  in.ExternalID,
		Title:       in.Title,
		Description: in.Description,
	}
	if in.Priority != nil {
		p := mapCLIPriorityToDomain(*in.Priority)
		upd.Priority = &p
	}
	if in.Objective != nil && strings.TrimSpace(*in.Objective) != "" {
		obj, oErr := eng.resolveObjective(authCtx, ws.ID, target.ProjectID, *in.Objective)
		if oErr != nil {
			return cli.TaskView{}, oErr
		}
		idCopy := obj.ID
		upd.ObjectiveID = &idCopy
	}
	if in.Parent != nil && strings.TrimSpace(*in.Parent) != "" {
		pt, ptErr := eng.resolveTask(authCtx, ws.ID, *in.Parent)
		if ptErr != nil {
			return cli.TaskView{}, ptErr
		}
		idCopy := pt.ID
		upd.ParentTaskID = &idCopy
	}
	if in.Acceptance != nil {
		upd.AcceptanceCriteria = strings.Split(*in.Acceptance, "\n")
	}
	if len(in.AddLabels) > 0 || len(in.RemoveLabels) > 0 {
		tagSet := make(map[string]bool)
		for _, tg := range target.Tags {
			tagSet[tg] = true
		}
		for _, tg := range in.AddLabels {
			tagSet[tg] = true
		}
		for _, tg := range in.RemoveLabels {
			delete(tagSet, tg)
		}
		var merged []string
		for tg := range tagSet {
			merged = append(merged, tg)
		}
		sort.Strings(merged)
		upd.Tags = merged
	}

	updated, err := eng.taskSvc.UpdateTask(authCtx, upd)
	if err != nil {
		return cli.TaskView{}, err
	}
	return eng.buildTaskView(authCtx, ws.ID, updated, true, true, true), nil
}

func (c *dualModeClient) MoveTask(ctx context.Context, in cli.MoveTaskInput) (cli.TaskView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteMoveTask(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.TaskView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), firstNonEmpty(in.Actor, opts.Actor), "")
	target, err := eng.resolveTask(authCtx, ws.ID, in.Task)
	if err != nil {
		return cli.TaskView{}, err
	}
	dest := parseCLIWorkflowStatus(in.State)
	moved, err := eng.taskSvc.TransitionTask(authCtx, task.TransitionTaskInput{
		WorkspaceID:        ws.ID,
		TaskID:             target.ID,
		TargetStatus:       dest,
		ResolutionComment:  in.Note,
		ReleaseActiveLease: dest.IsTerminal(),
	})
	if err != nil {
		return cli.TaskView{}, err
	}
	return eng.buildTaskView(authCtx, ws.ID, moved, true, true, true), nil
}

func (c *dualModeClient) ClaimTask(ctx context.Context, in cli.ClaimTaskInput) (cli.TaskView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteClaimTask(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.TaskView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), firstNonEmpty(in.Actor, opts.Actor), "")
	target, err := eng.resolveTask(authCtx, ws.ID, in.Task)
	if err != nil {
		return cli.TaskView{}, err
	}
	ttl := int64(in.LeaseTTL.Seconds())
	if ttl <= 0 {
		ttl = 1800
	}
	if _, err := eng.taskSvc.ClaimTask(authCtx, task.ClaimTaskInput{
		WorkspaceID:              ws.ID,
		TaskID:                   target.ID,
		TTLSeconds:               ttl,
		AutoTransitionInProgress: true,
	}); err != nil {
		return cli.TaskView{}, err
	}
	if strings.TrimSpace(in.Note) != "" {
		_, _ = eng.taskSvc.AddComment(authCtx, task.AddCommentInput{
			WorkspaceID: ws.ID,
			TaskID:      target.ID,
			Kind:        task.CommentKindProgress,
			Body:        in.Note,
		})
	}
	reloaded, err := eng.taskSvc.GetTask(authCtx, ws.ID, target.ID)
	if err != nil {
		return cli.TaskView{}, err
	}
	return eng.buildTaskView(authCtx, ws.ID, reloaded, true, true, true), nil
}

func (c *dualModeClient) ReleaseTask(ctx context.Context, in cli.ReleaseTaskInput) (cli.TaskView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteReleaseTask(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.TaskView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), firstNonEmpty(in.Actor, opts.Actor), "")
	target, err := eng.resolveTask(authCtx, ws.ID, in.Task)
	if err != nil {
		return cli.TaskView{}, err
	}
	token := ""
	if target.ActiveLease != nil {
		token = target.ActiveLease.LeaseToken
	}
	if strings.TrimSpace(in.TargetState) != "" {
		dest := parseCLIWorkflowStatus(in.TargetState)
		moved, moveErr := eng.taskSvc.TransitionTask(authCtx, task.TransitionTaskInput{
			WorkspaceID:        ws.ID,
			TaskID:             target.ID,
			TargetStatus:       dest,
			ResolutionComment:  in.Note,
			ReleaseActiveLease: true,
		})
		if moveErr != nil {
			return cli.TaskView{}, moveErr
		}
		return eng.buildTaskView(authCtx, ws.ID, moved, true, true, true), nil
	}
	if err := eng.taskSvc.ReleaseTask(authCtx, task.ReleaseTaskInput{
		WorkspaceID:    ws.ID,
		TaskID:         target.ID,
		LeaseToken:     token,
		Force:          token == "",
		HandoffComment: in.Note,
	}); err != nil {
		return cli.TaskView{}, err
	}
	reloaded, err := eng.taskSvc.GetTask(authCtx, ws.ID, target.ID)
	if err != nil {
		return cli.TaskView{}, err
	}
	return eng.buildTaskView(authCtx, ws.ID, reloaded, true, true, true), nil
}

func (c *dualModeClient) BlockTask(ctx context.Context, in cli.BlockTaskInput) (cli.TaskView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteBlockTask(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.TaskView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), firstNonEmpty(in.Actor, opts.Actor), "")
	target, err := eng.resolveTask(authCtx, ws.ID, in.Task)
	if err != nil {
		return cli.TaskView{}, err
	}
	for _, upRef := range in.BlockedBy {
		if strings.TrimSpace(upRef) == "" {
			continue
		}
		upstream, upErr := eng.resolveTask(authCtx, ws.ID, upRef)
		if upErr != nil {
			return cli.TaskView{}, upErr
		}
		if _, relErr := eng.taskSvc.AddRelation(authCtx, task.AddRelationInput{
			WorkspaceID:  ws.ID,
			SourceTaskID: upstream.ID,
			TargetTaskID: target.ID,
			Type:         task.RelationBlocks,
			Note:         in.Reason,
		}); relErr != nil && !errors.Is(relErr, errs.ErrConflict) {
			return cli.TaskView{}, relErr
		}
	}
	if len(in.BlockedBy) == 0 {
		_, err = eng.taskSvc.TransitionTask(authCtx, task.TransitionTaskInput{
			WorkspaceID:       ws.ID,
			TaskID:            target.ID,
			TargetStatus:      task.StatusBlocked,
			ResolutionComment: in.Reason,
		})
		if err != nil {
			return cli.TaskView{}, err
		}
	} else if strings.TrimSpace(in.Reason) != "" {
		_, _ = eng.taskSvc.AddComment(authCtx, task.AddCommentInput{
			WorkspaceID: ws.ID,
			TaskID:      target.ID,
			Kind:        task.CommentKindBlocker,
			Body:        in.Reason,
		})
	}
	reloaded, err := eng.taskSvc.GetTask(authCtx, ws.ID, target.ID)
	if err != nil {
		return cli.TaskView{}, err
	}
	return eng.buildTaskView(authCtx, ws.ID, reloaded, true, true, true), nil
}

func (c *dualModeClient) UnblockTask(ctx context.Context, in cli.UnblockTaskInput) (cli.TaskView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteUnblockTask(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.TaskView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), firstNonEmpty(in.Actor, opts.Actor), "")
	target, err := eng.resolveTask(authCtx, ws.ID, in.Task)
	if err != nil {
		return cli.TaskView{}, err
	}
	rels, _ := eng.taskSvc.ListRelations(authCtx, ws.ID, target.ID)
	clearFilter := make(map[uuid.UUID]bool)
	for _, ref := range in.ClearedBy {
		if up, upErr := eng.resolveTask(authCtx, ws.ID, ref); upErr == nil {
			clearFilter[up.ID] = true
		}
	}
	for _, rel := range rels {
		if rel.TargetTaskID == target.ID && rel.Type == task.RelationBlocks {
			if len(clearFilter) == 0 || clearFilter[rel.SourceTaskID] {
				_ = eng.taskSvc.RemoveRelation(authCtx, ws.ID, rel.ID)
			}
		}
	}
	reloaded, err := eng.taskSvc.GetTask(authCtx, ws.ID, target.ID)
	if err != nil {
		return cli.TaskView{}, err
	}
	if reloaded.Status == task.StatusBlocked && reloaded.UnresolvedBlockerCount == 0 {
		reloaded, _ = eng.taskSvc.TransitionTask(authCtx, task.TransitionTaskInput{
			WorkspaceID:       ws.ID,
			TaskID:            target.ID,
			TargetStatus:      task.StatusReady,
			ResolutionComment: in.Note,
		})
	}
	return eng.buildTaskView(authCtx, ws.ID, reloaded, true, true, true), nil
}

func (c *dualModeClient) AddComment(ctx context.Context, in cli.AddCommentInput) (cli.CommentView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteAddComment(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.CommentView{}, err
	}
	authCtx, ws, ident := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), firstNonEmpty(in.Actor, opts.Actor), "")
	target, err := eng.resolveTask(authCtx, ws.ID, in.Task)
	if err != nil {
		return cli.CommentView{}, err
	}
	ck := mapCLICommentKind(in.Kind)
	cm, err := eng.taskSvc.AddComment(authCtx, task.AddCommentInput{
		WorkspaceID: ws.ID,
		TaskID:      target.ID,
		Kind:        ck,
		Body:        in.Body,
	})
	if err != nil {
		return cli.CommentView{}, err
	}
	return cli.CommentView{
		ID:        cm.ID.String(),
		TaskID:    target.ID.String(),
		TaskKey:   target.Key,
		Author:    firstNonEmpty(cm.AuthorHandle, ident.Handle),
		Kind:      string(cm.Kind),
		Body:      cm.Body,
		CreatedAt: cm.CreatedAt,
	}, nil
}

func (c *dualModeClient) ListComments(ctx context.Context, in cli.ListCommentsInput) (cli.CommentListResult, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteListComments(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.CommentListResult{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), opts.Actor, "")
	target, err := eng.resolveTask(authCtx, ws.ID, in.Task)
	if err != nil {
		return cli.CommentListResult{}, err
	}
	items, err := eng.taskSvc.ListComments(authCtx, ws.ID, target.ID, int32(max(1, in.Limit)))
	if err != nil {
		return cli.CommentListResult{}, err
	}
	out := make([]cli.CommentView, 0, len(items))
	for _, cm := range items {
		out = append(out, cli.CommentView{
			ID:        cm.ID.String(),
			TaskID:    target.ID.String(),
			TaskKey:   target.Key,
			Author:    firstNonEmpty(cm.AuthorHandle, cm.AuthorActorID.String()),
			Kind:      string(cm.Kind),
			Body:      cm.Body,
			CreatedAt: cm.CreatedAt,
		})
	}
	return cli.CommentListResult{Items: out}, nil
}

func (c *dualModeClient) NextTask(ctx context.Context, in cli.NextTaskInput) (cli.NextTaskResult, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteNextTask(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.NextTaskResult{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), firstNonEmpty(in.Actor, opts.Actor), "")
	proj, err := eng.resolveProject(authCtx, ws.ID, in.Project)
	if err != nil {
		return cli.NextTaskResult{}, err
	}
	var objID *uuid.UUID
	if strings.TrimSpace(in.Objective) != "" {
		obj, objErr := eng.resolveObjective(authCtx, ws.ID, proj.ID, in.Objective)
		if objErr != nil {
			return cli.NextTaskResult{}, objErr
		}
		idCopy := obj.ID
		objID = &idCopy
	}
	queue, err := eng.taskSvc.PollWorkQueue(authCtx, task.AgentWorkQueueFilter{
		WorkspaceID:   ws.ID,
		ProjectID:     proj.ID,
		ObjectiveID:   objID,
		Statuses:      []task.Status{task.StatusReady, task.StatusBacklog},
		RequiredTags:  in.Labels,
		OnlyUnblocked: true,
		OnlyUnclaimed: true,
		Limit:         5,
	})
	if err != nil {
		return cli.NextTaskResult{}, err
	}
	if len(queue) == 0 {
		return cli.NextTaskResult{Task: nil, Claimed: false}, nil
	}
	bestID := queue[0].ID
	claimed := false
	if in.Claim {
		ttl := int64(in.LeaseTTL.Seconds())
		if ttl <= 0 {
			ttl = 1800
		}
		if _, claimErr := eng.taskSvc.ClaimTask(authCtx, task.ClaimTaskInput{
			WorkspaceID:              ws.ID,
			TaskID:                   bestID,
			TTLSeconds:               ttl,
			AutoTransitionInProgress: true,
		}); claimErr != nil {
			return cli.NextTaskResult{}, claimErr
		}
		claimed = true
	}
	full, err := eng.taskSvc.GetTask(authCtx, ws.ID, bestID)
	if err != nil {
		return cli.NextTaskResult{}, err
	}
	view := eng.buildTaskView(authCtx, ws.ID, full, true, true, true)
	return cli.NextTaskResult{Task: &view, Claimed: claimed}, nil
}

func (c *dualModeClient) Digest(ctx context.Context, in cli.TaskDigestInput) (cli.ProjectDigestView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteDigest(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.ProjectDigestView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), opts.Actor, "")
	proj, err := eng.resolveProject(authCtx, ws.ID, in.Project)
	if err != nil {
		return cli.ProjectDigestView{}, err
	}
	lim := int32(max(5, in.Limit))
	dig, err := eng.taskSvc.GetProjectDigest(authCtx, ws.ID, proj.ID, task.DigestOptions{
		MaxReadyTasks:   lim,
		MaxBlockedTasks: lim,
	})
	if err != nil {
		return cli.ProjectDigestView{}, err
	}
	counts := make(map[string]int, len(dig.CountsByStatus))
	for st, cnt := range dig.CountsByStatus {
		counts[string(st)] = int(cnt)
	}
	var objViews []cli.ObjectiveView
	for _, o := range dig.ActiveObjectives {
		objViews = append(objViews, cli.ObjectiveView{
			ID:             o.ID.String(),
			WorkspaceID:    ws.ID.String(),
			ProjectID:      proj.ID.String(),
			Key:            o.Slug,
			Kind:           "goal",
			Title:          o.Title,
			Status:         "active",
			Priority:       int(o.PriorityRank),
			CompletedTasks: int(o.DoneTaskCount),
			TotalTasks:     int(o.OpenTaskCount + o.DoneTaskCount),
		})
	}
	var leases, ready, blocked []cli.TaskSummaryView
	for _, it := range dig.ActiveLeases {
		leases = append(leases, toTaskSummaryView(it))
	}
	for _, it := range dig.ReadyQueue {
		ready = append(ready, toTaskSummaryView(it))
	}
	if in.IncludeBlocked {
		for _, it := range dig.BlockedTasks {
			blocked = append(blocked, toTaskSummaryView(it))
		}
	}
	return cli.ProjectDigestView{
		WorkspaceID:  ws.ID.String(),
		ProjectID:    proj.ID.String(),
		ProjectKey:   proj.KeyPrefix,
		ProjectName:  proj.Name,
		GeneratedAt:  dig.GeneratedAt,
		CountsByState: counts,
		Objectives:   objViews,
		ActiveLeases: leases,
		ReadyQueue:   ready,
		BlockedTasks: blocked,
	}, nil
}

func (c *dualModeClient) ExecuteScript(ctx context.Context, in cli.ExecuteScriptInput) (cli.ExecuteScriptResult, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteExecuteScript(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.ExecuteScriptResult{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), firstNonEmpty(in.Actor, opts.Actor), "")
	var ambientProj *uuid.UUID
	if strings.TrimSpace(in.Project) != "" {
		if p, pErr := eng.resolveProject(authCtx, ws.ID, in.Project); pErr == nil {
			idCopy := p.ID
			ambientProj = &idCopy
		}
	}
	jsRes, jsErr := eng.codeMode.Execute(authCtx, codemode.ExecuteInput{
		WorkspaceID:      ws.ID,
		DefaultProjectID: ambientProj,
		Script:           in.Script,
		DryRun:           in.DryRun,
	})
	if jsErr != nil {
		return cli.ExecuteScriptResult{}, jsErr
	}
	return cli.ExecuteScriptResult{
		DryRun:           in.DryRun,
		Density:          in.Density,
		AppliedCount:     jsRes.Mutations,
		FormattedSummary: jsRes.Summary,
	}, nil
}

func (c *dualModeClient) GetGuide(ctx context.Context, section string) (cli.LanguageGuideView, error) {
	prov := language.NewGuideProvider()
	spec, err := prov.MarkdownSpec(ctx)
	if err != nil {
		return cli.LanguageGuideView{}, err
	}
	return cli.LanguageGuideView{
		Section:  firstNonEmpty(section, "all"),
		Version:  "2.0.0",
		Markdown: spec,
	}, nil
}

func (c *dualModeClient) ListAuditEvents(ctx context.Context, in cli.ListAuditEventsInput) (cli.AuditEventListResult, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteListAuditEvents(ctx, opts, in)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.AuditEventListResult{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(in.Workspace, opts.Workspace), opts.Actor, "")
	records, err := eng.auditSvc.QueryRecords(authCtx, audit.QueryFilter{
		WorkspaceID:  ws.ID,
		ResourceType: strings.TrimSpace(in.EntityType),
		ResourceID:   strings.TrimSpace(in.EntityID),
		Since:        in.Since,
		Limit:        int32(max(1, in.Limit)),
	})
	if err != nil {
		return cli.AuditEventListResult{}, err
	}
	out := make([]cli.AuditEventView, 0, len(records))
	for _, r := range records {
		out = append(out, toAuditEventView(r))
	}
	return cli.AuditEventListResult{Items: out}, nil
}

func (c *dualModeClient) GetAuditEvent(ctx context.Context, workspaceRef, eventID string) (cli.AuditEventView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteGetAuditEvent(ctx, opts, workspaceRef, eventID)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.AuditEventView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(workspaceRef, opts.Workspace), opts.Actor, "")
	aid, err := uuid.Parse(strings.TrimSpace(eventID))
	if err != nil {
		return cli.AuditEventView{}, errs.Validation("app.GetAuditEvent", "audit event ID must be a valid UUID")
	}
	rec, err := eng.auditSvc.GetRecord(authCtx, ws.ID, aid)
	if err != nil {
		return cli.AuditEventView{}, err
	}
	return toAuditEventView(rec), nil
}

func (c *dualModeClient) GetProjectMarkdown(ctx context.Context, workspaceRef, projectRef string) (cli.MarkdownDocView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteGetProjectMarkdown(ctx, opts, workspaceRef, projectRef)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(workspaceRef, opts.Workspace), opts.Actor, "")
	p, err := eng.resolveProject(authCtx, ws.ID, projectRef)
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	md, exists, err := eng.projectSvc.GetProjectMarkdown(authCtx, ws.ID, p.ID)
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	return cli.MarkdownDocView{
		EntityKind:     "project",
		EntityKey:      p.KeyPrefix,
		EntityID:       p.ID.String(),
		WorkspaceID:    ws.ID.String(),
		HasMarkdownDoc: exists,
		Markdown:       md,
		Action:         "get",
	}, nil
}

func (c *dualModeClient) PutProjectMarkdown(ctx context.Context, workspaceRef, projectRef, actor, markdown string) (cli.MarkdownDocView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remotePutProjectMarkdown(ctx, opts, workspaceRef, projectRef, actor, markdown)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(workspaceRef, opts.Workspace), firstNonEmpty(actor, opts.Actor), "")
	p, err := eng.resolveProject(authCtx, ws.ID, projectRef)
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	updated, err := eng.projectSvc.PutProjectMarkdown(authCtx, ws.ID, p.ID, markdown)
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	return cli.MarkdownDocView{
		EntityKind:     "project",
		EntityKey:      updated.KeyPrefix,
		EntityID:       updated.ID.String(),
		WorkspaceID:    ws.ID.String(),
		HasMarkdownDoc: updated.HasMarkdownDoc,
		Markdown:       markdown,
		Action:         "put",
	}, nil
}

func (c *dualModeClient) DeleteProjectMarkdown(ctx context.Context, workspaceRef, projectRef, actor string) (cli.MarkdownDocView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteDeleteProjectMarkdown(ctx, opts, workspaceRef, projectRef, actor)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(workspaceRef, opts.Workspace), firstNonEmpty(actor, opts.Actor), "")
	p, err := eng.resolveProject(authCtx, ws.ID, projectRef)
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	updated, err := eng.projectSvc.DeleteProjectMarkdown(authCtx, ws.ID, p.ID)
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	return cli.MarkdownDocView{
		EntityKind:     "project",
		EntityKey:      updated.KeyPrefix,
		EntityID:       updated.ID.String(),
		WorkspaceID:    ws.ID.String(),
		HasMarkdownDoc: false,
		Action:         "delete",
	}, nil
}

func (c *dualModeClient) GetTaskMarkdown(ctx context.Context, workspaceRef, taskRef string) (cli.MarkdownDocView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteGetTaskMarkdown(ctx, opts, workspaceRef, taskRef)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(workspaceRef, opts.Workspace), opts.Actor, "")
	t, err := eng.resolveTask(authCtx, ws.ID, taskRef)
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	md, exists, err := eng.taskSvc.GetTaskMarkdown(authCtx, ws.ID, t.ID)
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	return cli.MarkdownDocView{
		EntityKind:     "task",
		EntityKey:      t.Key,
		EntityID:       t.ID.String(),
		WorkspaceID:    ws.ID.String(),
		HasMarkdownDoc: exists,
		Markdown:       md,
		Action:         "get",
	}, nil
}

func (c *dualModeClient) PutTaskMarkdown(ctx context.Context, workspaceRef, taskRef, actor, markdown string) (cli.MarkdownDocView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remotePutTaskMarkdown(ctx, opts, workspaceRef, taskRef, actor, markdown)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(workspaceRef, opts.Workspace), firstNonEmpty(actor, opts.Actor), "")
	t, err := eng.resolveTask(authCtx, ws.ID, taskRef)
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	updated, err := eng.taskSvc.PutTaskMarkdown(authCtx, ws.ID, t.ID, markdown)
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	return cli.MarkdownDocView{
		EntityKind:     "task",
		EntityKey:      updated.Key,
		EntityID:       updated.ID.String(),
		WorkspaceID:    ws.ID.String(),
		HasMarkdownDoc: updated.HasMarkdownDoc,
		Markdown:       markdown,
		Action:         "put",
	}, nil
}

func (c *dualModeClient) DeleteTaskMarkdown(ctx context.Context, workspaceRef, taskRef, actor string) (cli.MarkdownDocView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteDeleteTaskMarkdown(ctx, opts, workspaceRef, taskRef, actor)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(workspaceRef, opts.Workspace), firstNonEmpty(actor, opts.Actor), "")
	t, err := eng.resolveTask(authCtx, ws.ID, taskRef)
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	updated, err := eng.taskSvc.DeleteTaskMarkdown(authCtx, ws.ID, t.ID)
	if err != nil {
		return cli.MarkdownDocView{}, err
	}
	return cli.MarkdownDocView{
		EntityKind:     "task",
		EntityKey:      updated.Key,
		EntityID:       updated.ID.String(),
		WorkspaceID:    ws.ID.String(),
		HasMarkdownDoc: false,
		Action:         "delete",
	}, nil
}

func (c *dualModeClient) PurgeArchivedTasks(ctx context.Context, workspaceRef, actor string) (cli.PurgeArchivedView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remotePurgeArchivedTasks(ctx, opts, workspaceRef, actor)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.PurgeArchivedView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(workspaceRef, opts.Workspace), firstNonEmpty(actor, opts.Actor), "")
	res, err := eng.taskSvc.PurgeArchivedTasks(authCtx, ws.ID)
	if err != nil {
		return cli.PurgeArchivedView{}, err
	}
	return cli.PurgeArchivedView{
		WorkspaceID:          ws.ID.String(),
		DeletedTaskCount:     int(res.DeletedTaskCount),
		DeletedMarkdownCount: int(res.DeletedMarkdownCount),
		DeletedTaskKeys:      append([]string(nil), res.DeletedTaskKeys...),
	}, nil
}

func (c *dualModeClient) InspectOrphans(ctx context.Context, workspaceRef string) (cli.OrphanReportView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remoteInspectOrphans(ctx, opts, workspaceRef)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.OrphanReportView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(workspaceRef, opts.Workspace), opts.Actor, "")
	if ws.ID == uuid.Nil {
		return cli.OrphanReportView{}, nil
	}
	rep, err := eng.taskSvc.InspectOrphans(authCtx, ws.ID)
	if err != nil {
		return cli.OrphanReportView{}, err
	}
	return cli.OrphanReportView{
		WorkspaceID:              ws.Slug,
		OrphanTaskFilesCount:     int(rep.OrphanTaskFilesCount),
		MissingTaskFilesCount:    int(rep.MissingTaskFilesCount),
		OrphanProjectFilesCount:  int(rep.OrphanProjectFilesCount),
		MissingProjectFilesCount: int(rep.MissingProjectFilesCount),
		TotalIssues:              int(rep.TotalIssues),
		IssueSummaries:           append([]string(nil), rep.IssueSummaries...),
	}, nil
}

func (c *dualModeClient) PruneOrphans(ctx context.Context, workspaceRef, actor string, dryRun bool) (cli.PruneResultView, error) {
	opts := c.rt.optionsFromCtx(ctx)
	if opts.IsRemoteMode() {
		return c.remotePruneOrphans(ctx, opts, workspaceRef, actor, dryRun)
	}
	eng, err := c.rt.openOrGetLocalEngine(ctx, opts.DBPath)
	if err != nil {
		return cli.PruneResultView{}, err
	}
	authCtx, ws, _ := eng.ensureWorkspaceAndActor(ctx, firstNonEmpty(workspaceRef, opts.Workspace), firstNonEmpty(actor, opts.Actor), "")
	res, err := eng.taskSvc.PruneOrphans(authCtx, ws.ID, dryRun)
	if err != nil {
		return cli.PruneResultView{}, err
	}
	return cli.PruneResultView{
		WorkspaceID:              ws.Slug,
		DryRun:                   res.DryRun,
		RemovedTaskFilesCount:    int(res.RemovedTaskFilesCount),
		ClearedTaskRefsCount:     int(res.ClearedTaskRefsCount),
		RemovedProjectFilesCount: int(res.RemovedProjectFilesCount),
		ClearedProjectRefsCount:  int(res.ClearedProjectRefsCount),
		TotalCleaned:             int(res.TotalCleaned),
		CleanedSummaries:         append([]string(nil), res.CleanedSummaries...),
	}, nil
}

// =============================================================================
// Projection & Rollup Helpers
// =============================================================================

func (e *localEngine) populateObjectiveTaskCounts(ctx context.Context, wsID, projID uuid.UUID, view *cli.ObjectiveView, objID uuid.UUID) {
	rows, err := e.taskSvc.PollWorkQueue(ctx, task.AgentWorkQueueFilter{
		WorkspaceID: wsID,
		ProjectID:   projID,
		ObjectiveID: &objID,
		Statuses: []task.Status{
			task.StatusBacklog,
			task.StatusReady,
			task.StatusInProgress,
			task.StatusInReview,
			task.StatusBlocked,
			task.StatusDone,
		},
		Limit: 100,
	})
	if err != nil {
		return
	}
	total, done, blocked := 0, 0, 0
	for _, r := range rows {
		total++
		if r.Status == task.StatusDone {
			done++
		}
		if r.UnresolvedBlockerCount > 0 || r.Status == task.StatusBlocked {
			blocked++
		}
	}
	view.TotalTasks = total
	view.CompletedTasks = done
	view.BlockedTasks = blocked
}

func (e *localEngine) buildTaskView(
	ctx context.Context,
	wsID uuid.UUID,
	t task.Task,
	includeDeps, includeSubtasks, includeComments bool,
) cli.TaskView {
	v := cli.TaskView{
		ID:          t.ID.String(),
		WorkspaceID: t.WorkspaceID.String(),
		ProjectID:   t.ProjectID.String(),
		Key:         t.Key,
		ExternalID:  t.ExternalID,
		Title:       t.Title,
		Description: t.Description,
		State:       displayWorkflowStateName(t.Status),
		Category:    canonicalCategoryForStatus(t.Status),
		Priority:    mapDomainPriorityToCLI(t.Priority),
		Labels:      t.TagsCopy(),
		Acceptance:     strings.Join(t.AcceptanceCriteriaCopy(), "\n"),
		Blocked:        t.UnresolvedBlockerCount > 0 || t.Status == task.StatusBlocked,
		HasMarkdownDoc: t.HasMarkdownDoc,
		CreatedAt:      t.CreatedAt,
		UpdatedAt:      t.UpdatedAt,
	}
	if t.ObjectiveID != nil {
		if obj, err := e.objectiveSvc.GetObjective(ctx, wsID, *t.ObjectiveID); err == nil {
			v.ObjectiveKey = obj.Slug
		}
	}
	if t.ParentTaskID != nil {
		if pt, err := e.taskSvc.GetTask(ctx, wsID, *t.ParentTaskID); err == nil {
			v.ParentKey = pt.Key
		}
	}
	if t.ActiveLease != nil {
		v.Lease = &cli.ClaimLeaseView{
			Holder:     firstNonEmpty(t.ActiveLease.HolderHandle, t.ActiveLease.HolderActorID.String()),
			LeaseToken: t.ActiveLease.LeaseToken,
			AcquiredAt: t.ActiveLease.AcquiredAt,
			ExpiresAt:  t.ActiveLease.ExpiresAt,
		}
	}
	if includeDeps {
		if rels, err := e.taskSvc.ListRelations(ctx, wsID, t.ID); err == nil {
			for _, rel := range rels {
				if rel.Type != task.RelationBlocks {
					continue
				}
				if rel.TargetTaskID == t.ID {
					if up, upErr := e.taskSvc.GetTask(ctx, wsID, rel.SourceTaskID); upErr == nil && !up.Status.IsTerminal() {
						v.BlockedBy = append(v.BlockedBy, up.Key)
					}
				} else if rel.SourceTaskID == t.ID {
					if down, downErr := e.taskSvc.GetTask(ctx, wsID, rel.TargetTaskID); downErr == nil {
						v.Blocks = append(v.Blocks, down.Key)
					}
				}
			}
		}
	}
	if includeSubtasks {
		parentCopy := t.ID
		if subs, err := e.taskSvc.PollWorkQueue(ctx, task.AgentWorkQueueFilter{
			WorkspaceID:  wsID,
			ProjectID:    t.ProjectID,
			ParentTaskID: &parentCopy,
			Statuses: []task.Status{
				task.StatusBacklog, task.StatusReady, task.StatusInProgress,
				task.StatusInReview, task.StatusBlocked, task.StatusDone,
			},
			Limit: 50,
		}); err == nil {
			for _, s := range subs {
				v.Subtasks = append(v.Subtasks, toTaskSummaryView(s))
			}
		}
	}
	if includeComments {
		if cms, err := e.taskSvc.ListComments(ctx, wsID, t.ID, 25); err == nil {
			for _, cm := range cms {
				v.Comments = append(v.Comments, cli.CommentView{
					ID:        cm.ID.String(),
					TaskID:    t.ID.String(),
					TaskKey:   t.Key,
					Author:    firstNonEmpty(cm.AuthorHandle, cm.AuthorActorID.String()),
					Kind:      string(cm.Kind),
					Body:      cm.Body,
					CreatedAt: cm.CreatedAt,
				})
			}
		}
	}
	return v
}

func defaultCLIWorkflowStates(projID string) []cli.WorkflowStateView {
	return []cli.WorkflowStateView{
		{ID: deterministicUUID("state", projID, "backlog").String(), ProjectID: projID, Name: "Backlog", Category: "backlog", Position: 10},
		{ID: deterministicUUID("state", projID, "todo").String(), ProjectID: projID, Name: "Todo", Category: "todo", Position: 20},
		{ID: deterministicUUID("state", projID, "in_progress").String(), ProjectID: projID, Name: "In Progress", Category: "in_progress", Position: 30},
		{ID: deterministicUUID("state", projID, "in_review").String(), ProjectID: projID, Name: "In Review", Category: "review", Position: 40},
		{ID: deterministicUUID("state", projID, "blocked").String(), ProjectID: projID, Name: "Blocked", Category: "blocked", Position: 50},
		{ID: deterministicUUID("state", projID, "done").String(), ProjectID: projID, Name: "Done", Category: "done", Position: 60},
		{ID: deterministicUUID("state", projID, "canceled").String(), ProjectID: projID, Name: "Canceled", Category: "canceled", Position: 70},
		{ID: deterministicUUID("state", projID, "archived").String(), ProjectID: projID, Name: "Archived", Category: "archived", Position: 80},
	}
}

func toWorkspaceView(ws workspace.Workspace) cli.WorkspaceView {
	return cli.WorkspaceView{
		ID:                     ws.ID.String(),
		Slug:                   ws.Slug,
		Name:                   ws.Name,
		Description:            ws.Description,
		DefaultLeaseTTLSeconds: ws.DefaultLeaseTTLSeconds,
		CreatedAt:              ws.CreatedAt,
		UpdatedAt:              ws.UpdatedAt,
	}
}

func toProjectView(p project.Project) cli.ProjectView {
	return cli.ProjectView{
		ID:             p.ID.String(),
		WorkspaceID:    p.WorkspaceID.String(),
		Slug:           p.Slug,
		Key:            p.KeyPrefix,
		Name:           p.Name,
		Description:    p.Description,
		Status:         string(p.Status),
		HasMarkdownDoc: p.HasMarkdownDoc,
		CreatedAt:      p.CreatedAt,
		UpdatedAt:      p.UpdatedAt,
	}
}

func toObjectiveView(o objective.Objective, kind, owner string) cli.ObjectiveView {
	td := ""
	if o.TargetDate != nil && !o.TargetDate.IsZero() {
		td = o.TargetDate.UTC().Format("2006-01-02")
	}
	return cli.ObjectiveView{
		ID:         o.ID.String(),
		WorkspaceID: o.WorkspaceID.String(),
		ProjectID:  o.ProjectID.String(),
		Key:        o.Slug,
		Kind:       firstNonEmpty(kind, "goal"),
		Title:      o.Title,
		Summary:    o.Summary,
		Status:     string(o.Status),
		Priority:   int(o.PriorityRank),
		TargetDate: td,
		Owner:      owner,
		CreatedAt:  o.CreatedAt,
		UpdatedAt:  o.UpdatedAt,
	}
}

func toTaskSummaryView(s task.CompactTaskSummary) cli.TaskSummaryView {
	return cli.TaskSummaryView{
		ID:           s.ID.String(),
		Key:          s.Key,
		ExternalID:   s.ExternalID,
		Title:        s.Title,
		State:        displayWorkflowStateName(s.Status),
		Category:     canonicalCategoryForStatus(s.Status),
		Priority:     mapDomainPriorityToCLI(s.Priority),
		ObjectiveKey: s.ObjectiveSlug,
		ParentKey:    s.ParentKey,
		Holder:       s.HolderHandle,
		Labels:         append([]string(nil), s.Tags...),
		Blocked:        s.UnresolvedBlockerCount > 0 || s.Status == task.StatusBlocked,
		HasMarkdownDoc: s.HasMarkdownDoc,
		UpdatedAt:      s.UpdatedAt,
	}
}

func toAuditEventView(r audit.Record) cli.AuditEventView {
	oldRaw := string(r.OldValuesCopy())
	newRaw := string(r.NewValuesCopy())
	return cli.AuditEventView{
		ID:          r.ID.String(),
		WorkspaceID: r.WorkspaceID.String(),
		Actor:       firstNonEmpty(r.ActorHandle, r.ActorID.String()),
		ActorKind:   string(r.ActorKind),
		Action:      string(r.Action),
		EntityType:  r.ResourceType,
		EntityID:    r.ResourceID,
		Summary:     summarizeAuditPayload(string(r.Action), r.ResourceType, newRaw, oldRaw),
		OldValues:   oldRaw,
		NewValues:   newRaw,
		OccurredAt:  r.OccurredAt,
	}
}

func summarizeAuditPayload(action, entityType, newRaw, oldRaw string) string {
	for _, raw := range []string{newRaw, oldRaw} {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err == nil {
			key, _ := m["key"].(string)
			slug, _ := m["slug"].(string)
			title, _ := m["title"].(string)
			name, _ := m["name"].(string)
			status, _ := m["status"].(string)
			label := firstNonEmpty(key, slug)
			desc := firstNonEmpty(title, name)
			if label != "" && desc != "" {
				if status != "" {
					return fmt.Sprintf("%s: %s [%s]", label, desc, status)
				}
				return fmt.Sprintf("%s: %s", label, desc)
			}
			if desc != "" {
				return desc
			}
			if label != "" {
				return label
			}
		}
	}
	return fmt.Sprintf("%s %s", action, entityType)
}

func mapCLIObjectiveStatus(raw string) objective.Status {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "planned", "draft":
		return objective.StatusDraft
	case "achieved", "completed", "done":
		return objective.StatusAchieved
	case "dropped", "canceled", "cancelled":
		return objective.StatusDropped
	default:
		return objective.StatusActive
	}
}

func mapCLICommentKind(raw string) task.CommentKind {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "handoff":
		return task.CommentKindHandoff
	case "decision", "resolution":
		return task.CommentKindDecision
	case "blocker", "blocker_report":
		return task.CommentKindBlocker
	case "progress", "verification":
		return task.CommentKindProgress
	default:
		return task.CommentKindNote
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}
