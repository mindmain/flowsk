// Package codemode implements Cloudflare-style Code Mode (`flowsk_execute`)
// using the pure-Go `dop251/goja` ECMAScript 6 virtual machine (`CGO_ENABLED=0`).
//
// Security & Transactional Invariants:
//   - Hermetic Sandbox: Every script runs in a fresh `goja.Runtime` with zero
//     filesystem, network, environment variable, or OS process globals.
//   - Preemptive Cancellation & Timeout: A background watchdog interrupts the
//     VM (`vm.Interrupt`) immediately upon `ctx.Done()` or `Timeout` expiry so
//     runaway `while (true) {}` loops cannot stall the server or CLI.
//   - Single-Transaction ACID Execution: All `flowsk.*` SDK reads and mutations
//     execute inside one outer `tx.Manager.WithTransaction` scope via a
//     re-entrant `scopedTxManager`. Any runtime JavaScript error or domain
//     violation rolls back every earlier write in the script atomically, and
//     `DryRun: true` simulates the full script before rolling back cleanly.
package codemode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/dop251/goja"
	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/objective"
	"github.com/mindmain/flowsk/internal/domain/project"
	"github.com/mindmain/flowsk/internal/domain/task"
	"github.com/mindmain/flowsk/internal/domain/tx"
	"github.com/mindmain/flowsk/internal/language"
)

const (
	// DefaultTimeout bounds individual JavaScript sandbox executions when
	// ExecuteInput.Timeout is <= 0.
	DefaultTimeout = 5 * time.Second

	maxConsoleLines = 64
)

var (
	errDryRunRollback = errors.New("codemode dry_run intentional rollback")
	slugNonAlnum      = regexp.MustCompile(`[^a-z0-9]+`)
)

// MarkdownStore abstracts tenant-isolated persistence of optional Task and Project
// Markdown documents (`~/.flowsk/{tasks,projects}/<workspace_id>/<id>.md`) without
// importing filesystem packages. `*mdstore.FileStore` satisfies this interface directly.
type MarkdownStore interface {
	task.MarkdownStore
	project.MarkdownStore
}

// Dependencies groups the persistence repositories and transaction manager
// required to construct a Code Mode JavaScript Engine.
type Dependencies struct {
	// TxManager coordinates the single outer ACID transaction wrapping script execution.
	TxManager tx.Manager

	// ProjectRepo provides project reads and writes inside the active transaction scope.
	ProjectRepo project.Repository

	// ObjectiveRepo provides objective/goal reads and writes inside the active transaction scope.
	ObjectiveRepo objective.Repository

	// TaskRepo provides task, relation, lease, and comment persistence inside the active transaction scope.
	TaskRepo task.Repository

	// KeyAllocator optionally allocates monotonic per-project task keys (`PROJ-N`) inside the active transaction scope.
	KeyAllocator task.KeyAllocator

	// MarkdownStore optionally persists extended `.md` documents for tasks and projects on commit.
	MarkdownStore MarkdownStore
}

// ExecuteInput carries parameters for evaluating a JavaScript Code Mode script.
type ExecuteInput struct {
	// WorkspaceID is the mandatory tenant workspace UUID verified against authctx.Identity.
	WorkspaceID uuid.UUID

	// DefaultProjectID optionally specifies the active logical project UUID when JS calls omit `project`.
	DefaultProjectID *uuid.UUID

	// Script is the ECMAScript 6 source code evaluated inside an IIFE `(function() { ... })()`
	// so top-level `return ...` statements work naturally as in Cloudflare Code Mode.
	Script string

	// DryRun executes the full script inside a real transaction and captures its return value,
	// then rolls back all database writes cleanly.
	DryRun bool

	// Timeout bounds wall-clock script execution duration (defaults to DefaultTimeout when <= 0).
	Timeout time.Duration
}

// ExecuteOutput captures the return value, console output, mutation count, and
// token-compact summary produced by a Code Mode JavaScript execution.
type ExecuteOutput struct {
	// Committed reports whether mutations were durably committed (`false` on DryRun).
	Committed bool

	// DryRun reports whether the run executed in rolled-back simulation mode.
	DryRun bool

	// Mutations counts the domain write operations performed by the script.
	Mutations int

	// Value holds the JSON-compatible Go representation returned by the script.
	Value any

	// Console holds lines emitted via `console.log` / `console.warn` / `console.error`.
	Console []string

	// Summary is a compact human- and LLM-ready textual rendering of Console + Value.
	Summary string
}

// Engine evaluates sandboxed JavaScript scripts against Flowsk domain services
// inside a single ACID transaction.
type Engine struct {
	deps Dependencies
}

// NewEngine constructs a thread-safe Code Mode JavaScript Engine.
func NewEngine(deps Dependencies) *Engine {
	return &Engine{deps: deps}
}

// ValidateSyntax compiles script as ECMAScript 6 inside the Code Mode IIFE
// wrapper without executing any statements or touching persistence, returning a
// structured domain validation error if JavaScript parsing fails.
func ValidateSyntax(script string) error {
	const op = "codemode.ValidateSyntax"
	trimmed := strings.TrimSpace(script)
	if trimmed == "" {
		return errs.Validation(op, "JavaScript code must not be empty", errs.FieldViolation{
			Field:       "script",
			Description: "script must not be empty",
		})
	}
	wrapped := "(function(){\n" + script + "\n})()"
	if _, err := goja.Compile("codemode.js", wrapped, false); err != nil {
		return errs.Validation(op, "JavaScript syntax error", errs.FieldViolation{
			Field:       "script",
			Description: err.Error(),
		})
	}
	return nil
}

// Execute runs in.Script inside an isolated `goja` VM wrapped in a single ACID
// database transaction (rolling back automatically on error or when in.DryRun is true).
func (e *Engine) Execute(ctx context.Context, in ExecuteInput) (ExecuteOutput, error) {
	const op = "codemode.Engine.Execute"
	if err := ctx.Err(); err != nil {
		return ExecuteOutput{}, fmt.Errorf("%s: %w", op, err)
	}
	if strings.TrimSpace(in.Script) == "" {
		return ExecuteOutput{}, errs.Validation(op, "JavaScript code must not be empty")
	}
	ident, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID)
	if err != nil {
		return ExecuteOutput{}, err
	}

	timeout := in.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stagedMD := newStagedMarkdownStore(e.deps.MarkdownStore)
	var out ExecuteOutput
	txErr := e.deps.TxManager.WithTransaction(execCtx, func(txCtx context.Context, scope tx.Scope) error {
		reentrantTx := &scopedTxManager{scope: scope}
		scopedProjRepo := e.deps.ProjectRepo.WithScope(scope)
		scopedObjRepo := e.deps.ObjectiveRepo.WithScope(scope)
		scopedTaskRepo := e.deps.TaskRepo.WithScope(scope)

		projSvc := project.NewService(scopedProjRepo, reentrantTx).WithMarkdownStore(stagedMD)
		objSvc := objective.NewService(scopedObjRepo, reentrantTx)
		keyAlloc := e.deps.KeyAllocator
		if keyAlloc == nil {
			keyAlloc = task.KeyAllocatorFunc(func(allocCtx context.Context, innerScope tx.Scope, wsID, projID uuid.UUID) (string, error) {
				prefix, seq, seqErr := e.deps.ProjectRepo.WithScope(innerScope).NextTaskSequence(allocCtx, wsID, projID)
				if seqErr != nil {
					return "", seqErr
				}
				return fmt.Sprintf("%s-%d", prefix, seq), nil
			})
		}
		taskSvc := task.NewServiceWithKeyAllocator(scopedTaskRepo, reentrantTx, keyAlloc).WithMarkdownResources(stagedMD, nil)

		rt := &scriptSession{
			ctx:         txCtx,
			ident:       ident,
			wsID:        in.WorkspaceID,
			defaultProj: in.DefaultProjectID,
			projSvc:     projSvc,
			objSvc:      objSvc,
			taskSvc:     taskSvc,
		}

		vm := goja.New()
		stopDone := make(chan struct{})
		defer close(stopDone)
		go func() {
			select {
			case <-execCtx.Done():
				vm.Interrupt(execCtx.Err())
			case <-stopDone:
			}
		}()

		if err := rt.bindGlobals(vm); err != nil {
			return err
		}

		wrapped := "(function(){\n" + in.Script + "\n})()"
		val, runErr := vm.RunString(wrapped)
		if runErr != nil {
			var intr *goja.InterruptedError
			if errors.As(runErr, &intr) {
				return errs.Wrap(errs.ErrValidation, op, "JavaScript execution timed out or was canceled", execCtx.Err())
			}
			var jsExc *goja.Exception
			if errors.As(runErr, &jsExc) {
				if rt.lastDomainErr != nil {
					return rt.lastDomainErr
				}
				return errs.Validation(op, fmt.Sprintf("JavaScript runtime error: %s", jsExc.Value().String()))
			}
			return errs.Validation(op, fmt.Sprintf("JavaScript error: %v", runErr))
		}

		exported := exportJSValue(val)
		if rt.mutations > 0 && !in.DryRun {
			meta, _ := json.Marshal(map[string]any{
				"engine":    "codemode_js",
				"mutations": rt.mutations,
			})
			if recErr := scope.Audit().Record(txCtx, tx.AuditEntry{
				WorkspaceID:  in.WorkspaceID,
				ActorID:      ident.ActorID,
				Action:       tx.AuditActionBatchExec,
				ResourceType: "codemode_script",
				ResourceID:   in.WorkspaceID.String(),
				NewValues:    meta,
			}); recErr != nil {
				return recErr
			}
		}

		out = ExecuteOutput{
			Committed: !in.DryRun,
			DryRun:    in.DryRun,
			Mutations: rt.mutations,
			Value:     exported,
			Console:   append([]string(nil), rt.console...),
			Summary:   formatExecutionSummary(exported, rt.console, rt.mutations, in.DryRun),
		}
		if in.DryRun {
			return errDryRunRollback
		}
		return nil
	})

	if txErr != nil && !errors.Is(txErr, errDryRunRollback) {
		return ExecuteOutput{}, txErr
	}
	if !in.DryRun {
		if flushErr := stagedMD.flush(execCtx, in.WorkspaceID); flushErr != nil {
			return ExecuteOutput{}, flushErr
		}
	}
	return out, nil
}

// stagedMarkdownStore buffers task and project `.md` writes and deletions in memory
// while the Code Mode SQL transaction executes, flushing to the underlying store only
// after commit so DryRun simulations and mid-script rollbacks never leave orphan files.
type stagedMarkdownStore struct {
	base        MarkdownStore
	taskWrites  map[uuid.UUID]string
	taskDeleted map[uuid.UUID]bool
	projWrites  map[uuid.UUID]string
	projDeleted map[uuid.UUID]bool
}

func newStagedMarkdownStore(base MarkdownStore) *stagedMarkdownStore {
	return &stagedMarkdownStore{
		base:        base,
		taskWrites:  make(map[uuid.UUID]string),
		taskDeleted: make(map[uuid.UUID]bool),
		projWrites:  make(map[uuid.UUID]string),
		projDeleted: make(map[uuid.UUID]bool),
	}
}

func (m *stagedMarkdownStore) ReadTaskMarkdown(ctx context.Context, workspaceID, taskID uuid.UUID) (string, error) {
	if m.taskDeleted[taskID] {
		return "", errs.New(errs.ErrNotFound, "codemode.ReadTaskMarkdown", "markdown document does not exist")
	}
	if md, ok := m.taskWrites[taskID]; ok {
		return md, nil
	}
	if m.base != nil {
		return m.base.ReadTaskMarkdown(ctx, workspaceID, taskID)
	}
	return "", errs.New(errs.ErrNotFound, "codemode.ReadTaskMarkdown", "markdown document does not exist")
}

func (m *stagedMarkdownStore) WriteTaskMarkdown(_ context.Context, _, taskID uuid.UUID, markdown string) error {
	delete(m.taskDeleted, taskID)
	m.taskWrites[taskID] = markdown
	return nil
}

func (m *stagedMarkdownStore) DeleteTaskMarkdown(_ context.Context, _, taskID uuid.UUID) error {
	delete(m.taskWrites, taskID)
	m.taskDeleted[taskID] = true
	return nil
}

func (m *stagedMarkdownStore) HasTaskMarkdown(ctx context.Context, workspaceID, taskID uuid.UUID) (bool, error) {
	if m.taskDeleted[taskID] {
		return false, nil
	}
	if _, ok := m.taskWrites[taskID]; ok {
		return true, nil
	}
	if m.base != nil {
		return m.base.HasTaskMarkdown(ctx, workspaceID, taskID)
	}
	return false, nil
}

func (m *stagedMarkdownStore) ScanTaskFiles(ctx context.Context, workspaceID uuid.UUID) ([]uuid.UUID, []string, error) {
	if m.base != nil {
		return m.base.ScanTaskFiles(ctx, workspaceID)
	}
	return nil, nil, nil
}

func (m *stagedMarkdownStore) RemoveMalformedTaskMarkdown(ctx context.Context, relKey string) error {
	if m.base != nil {
		return m.base.RemoveMalformedTaskMarkdown(ctx, relKey)
	}
	return nil
}

func (m *stagedMarkdownStore) ReadProjectMarkdown(ctx context.Context, workspaceID, projectID uuid.UUID) (string, error) {
	if m.projDeleted[projectID] {
		return "", errs.New(errs.ErrNotFound, "codemode.ReadProjectMarkdown", "markdown document does not exist")
	}
	if md, ok := m.projWrites[projectID]; ok {
		return md, nil
	}
	if m.base != nil {
		return m.base.ReadProjectMarkdown(ctx, workspaceID, projectID)
	}
	return "", errs.New(errs.ErrNotFound, "codemode.ReadProjectMarkdown", "markdown document does not exist")
}

func (m *stagedMarkdownStore) WriteProjectMarkdown(_ context.Context, _, projectID uuid.UUID, markdown string) error {
	delete(m.projDeleted, projectID)
	m.projWrites[projectID] = markdown
	return nil
}

func (m *stagedMarkdownStore) DeleteProjectMarkdown(_ context.Context, _, projectID uuid.UUID) error {
	delete(m.projWrites, projectID)
	m.projDeleted[projectID] = true
	return nil
}

func (m *stagedMarkdownStore) HasProjectMarkdown(ctx context.Context, workspaceID, projectID uuid.UUID) (bool, error) {
	if m.projDeleted[projectID] {
		return false, nil
	}
	if _, ok := m.projWrites[projectID]; ok {
		return true, nil
	}
	if m.base != nil {
		return m.base.HasProjectMarkdown(ctx, workspaceID, projectID)
	}
	return false, nil
}

func (m *stagedMarkdownStore) ScanProjectFiles(ctx context.Context, workspaceID uuid.UUID) ([]uuid.UUID, []string, error) {
	if m.base != nil {
		return m.base.ScanProjectFiles(ctx, workspaceID)
	}
	return nil, nil, nil
}

func (m *stagedMarkdownStore) RemoveMalformedProjectMarkdown(ctx context.Context, relKey string) error {
	if m.base != nil {
		return m.base.RemoveMalformedProjectMarkdown(ctx, relKey)
	}
	return nil
}

func (m *stagedMarkdownStore) flush(ctx context.Context, workspaceID uuid.UUID) error {
	if m == nil || m.base == nil {
		return nil
	}
	for id := range m.taskDeleted {
		if err := m.base.DeleteTaskMarkdown(ctx, workspaceID, id); err != nil {
			return err
		}
	}
	for id, md := range m.taskWrites {
		if err := m.base.WriteTaskMarkdown(ctx, workspaceID, id, md); err != nil {
			return err
		}
	}
	for id := range m.projDeleted {
		if err := m.base.DeleteProjectMarkdown(ctx, workspaceID, id); err != nil {
			return err
		}
	}
	for id, md := range m.projWrites {
		if err := m.base.WriteProjectMarkdown(ctx, workspaceID, id, md); err != nil {
			return err
		}
	}
	return nil
}

// scopedTxManager executes nested domain service WithTransaction calls inline
// on the already-active outer tx.Scope so an entire JS script commits or
// rolls back as one atomic unit without deadlocking single-writer semaphores.
type scopedTxManager struct {
	scope tx.Scope
}

func (s *scopedTxManager) WithTransaction(ctx context.Context, fn tx.TxFunc) error {
	return fn(ctx, s.scope)
}

func (s *scopedTxManager) WithNoAuditTransaction(ctx context.Context, fn tx.TxFunc) error {
	return fn(ctx, s.scope)
}

type scriptSession struct {
	ctx           context.Context
	ident         authctx.Identity
	wsID          uuid.UUID
	defaultProj   *uuid.UUID
	projSvc       project.Service
	objSvc        objective.Service
	taskSvc       task.Service
	mutations     int
	console       []string
	lastDomainErr error
}

func (s *scriptSession) throwErr(vm *goja.Runtime, err error) {
	s.lastDomainErr = err
	panic(vm.NewGoError(err))
}

func (s *scriptSession) bindGlobals(vm *goja.Runtime) error {
	consoleObj := vm.NewObject()
	logFn := func(call goja.FunctionCall) goja.Value {
		if len(s.console) >= maxConsoleLines {
			return goja.Undefined()
		}
		parts := make([]string, 0, len(call.Arguments))
		for _, arg := range call.Arguments {
			exp := exportJSValue(arg)
			switch v := exp.(type) {
			case string:
				parts = append(parts, v)
			default:
				b, _ := json.Marshal(v)
				parts = append(parts, string(b))
			}
		}
		s.console = append(s.console, strings.Join(parts, " "))
		return goja.Undefined()
	}
	_ = consoleObj.Set("log", logFn)
	_ = consoleObj.Set("info", logFn)
	_ = consoleObj.Set("warn", logFn)
	_ = consoleObj.Set("error", logFn)
	if err := vm.Set("console", consoleObj); err != nil {
		return err
	}

	projectsObj := vm.NewObject()
	_ = projectsObj.Set("list", func(_ goja.FunctionCall) goja.Value {
		items, err := s.projSvc.ListProjects(s.ctx, project.ListProjectsFilter{WorkspaceID: s.wsID, Limit: 100})
		if err != nil {
			s.throwErr(vm, err)
		}
		out := make([]map[string]any, 0, len(items))
		for _, p := range items {
			out = append(out, projectMap(p))
		}
		return vm.ToValue(out)
	})
	_ = projectsObj.Set("get", func(call goja.FunctionCall) goja.Value {
		ref := optArgString(call, 0)
		p, err := s.resolveProject(ref)
		if err != nil {
			s.throwErr(vm, err)
		}
		return vm.ToValue(projectMap(p))
	})
	_ = projectsObj.Set("create", func(call goja.FunctionCall) goja.Value {
		m := argMap(vm, call, 0)
		key := strings.ToUpper(strings.TrimSpace(strField(m, "key", "keyPrefix")))
		name := strings.TrimSpace(strField(m, "name", "title"))
		slug := strings.ToLower(strings.TrimSpace(strField(m, "slug")))
		if slug == "" {
			slug = slugify(firstNonEmpty(key, name))
		}
		if key == "" {
			key = strings.ToUpper(slug[:min(len(slug), 6)])
		}
		created, err := s.projSvc.CreateProject(s.ctx, project.CreateProjectInput{
			WorkspaceID: s.wsID,
			Slug:        slug,
			KeyPrefix:   key,
			Name:        firstNonEmpty(name, key),
			Description: strField(m, "description", "summary"),
		})
		if err != nil {
			s.throwErr(vm, err)
		}
		s.mutations++
		if s.defaultProj == nil {
			idCopy := created.ID
			s.defaultProj = &idCopy
		}
		return vm.ToValue(projectMap(created))
	})
	_ = projectsObj.Set("bind", func(call goja.FunctionCall) goja.Value {
		parent, err := s.resolveProject(optArgString(call, 0))
		if err != nil {
			s.throwErr(vm, err)
		}
		child, err := s.resolveProject(optArgString(call, 1))
		if err != nil {
			s.throwErr(vm, err)
		}
		bound, err := s.projSvc.BindSubproject(s.ctx, project.BindSubprojectInput{
			WorkspaceID:     s.wsID,
			ParentProjectID: parent.ID,
			ChildProjectID:  child.ID,
			ScopeSubpath:    optArgString(call, 2),
		})
		if err != nil {
			s.throwErr(vm, err)
		}
		s.mutations++
		return vm.ToValue(map[string]any{
			"id":            bound.ID.String(),
			"parentKey":     parent.KeyPrefix,
			"childKey":      child.KeyPrefix,
			"scopeSubpath":  bound.ScopeSubpath,
			"parentProject": parent.ID.String(),
			"childProject":  child.ID.String(),
		})
	})
	_ = projectsObj.Set("unbind", func(call goja.FunctionCall) goja.Value {
		parent, err := s.resolveProject(optArgString(call, 0))
		if err != nil {
			s.throwErr(vm, err)
		}
		child, err := s.resolveProject(optArgString(call, 1))
		if err != nil {
			s.throwErr(vm, err)
		}
		removed, err := s.projSvc.UnbindSubproject(s.ctx, project.UnbindSubprojectInput{
			WorkspaceID:     s.wsID,
			ParentProjectID: parent.ID,
			ChildProjectID:  child.ID,
		})
		if err != nil {
			s.throwErr(vm, err)
		}
		if removed {
			s.mutations++
		}
		return vm.ToValue(map[string]any{
			"parentKey": parent.KeyPrefix,
			"childKey":  child.KeyPrefix,
			"removed":   removed,
		})
	})
	_ = projectsObj.Set("bindings", func(_ goja.FunctionCall) goja.Value {
		items, err := s.projSvc.ListBindings(s.ctx, project.ListBindingsFilter{WorkspaceID: s.wsID})
		if err != nil {
			s.throwErr(vm, err)
		}
		projs, _ := s.projSvc.ListProjects(s.ctx, project.ListProjectsFilter{WorkspaceID: s.wsID, Limit: 200})
		keys := make(map[uuid.UUID]string, len(projs))
		for _, p := range projs {
			keys[p.ID] = p.KeyPrefix
		}
		out := make([]map[string]any, 0, len(items))
		for _, b := range items {
			out = append(out, map[string]any{
				"id":           b.ID.String(),
				"parentKey":    keys[b.ParentProjectID],
				"childKey":     keys[b.ChildProjectID],
				"scopeSubpath": b.ScopeSubpath,
			})
		}
		return vm.ToValue(out)
	})

	goalsObj := vm.NewObject()
	_ = goalsObj.Set("list", func(call goja.FunctionCall) goja.Value {
		rawFilter := optArgString(call, 0)
		p, err := s.resolveProject("")
		if err != nil {
			s.throwErr(vm, err)
		}
		objs, err := s.objSvc.ListObjectives(s.ctx, objective.ListObjectivesFilter{
			WorkspaceID: s.wsID,
			ProjectID:   p.ID,
			Limit:       100,
		})
		if err != nil {
			s.throwErr(vm, err)
		}
		tasks, _ := s.listAllProjectTasks(p.ID)
		q := language.ParseSearchQuery(rawFilter)
		out := make([]map[string]any, 0, len(objs))
		for _, o := range objs {
			if !q.MatchesText(o.Slug, o.Title, o.Summary, string(o.Status)) {
				continue
			}
			out = append(out, objectiveMap(o, tasks))
		}
		return vm.ToValue(out)
	})
	_ = goalsObj.Set("create", func(call goja.FunctionCall) goja.Value {
		m := argMap(vm, call, 0)
		p, err := s.resolveProject(strField(m, "project"))
		if err != nil {
			s.throwErr(vm, err)
		}
		title := strings.TrimSpace(strField(m, "title", "name"))
		slug := strings.TrimSpace(strField(m, "slug", "key"))
		if slug == "" {
			slug = slugify(title)
		}
		prio := parseJSPriorityRank(m["priority"])
		var td *time.Time
		if rawTD := strField(m, "targetDate", "target_date", "due"); rawTD != "" {
			if parsed, pErr := time.Parse("2006-01-02", rawTD); pErr == nil {
				utc := parsed.UTC()
				td = &utc
			}
		}
		criteria := strSliceField(m, "successCriteria", "criteria")
		if len(criteria) == 0 {
			criteria = []string{firstNonEmpty(strField(m, "summary", "description"), title)}
		}
		created, err := s.objSvc.CreateObjective(s.ctx, objective.CreateObjectiveInput{
			WorkspaceID:     s.wsID,
			ProjectID:       p.ID,
			Slug:            slug,
			Title:           title,
			Summary:         strField(m, "summary", "description"),
			SuccessCriteria: criteria,
			Status:          objective.StatusActive,
			PriorityRank:    prio,
			TargetDate:      td,
		})
		if err != nil {
			s.throwErr(vm, err)
		}
		s.mutations++
		return vm.ToValue(objectiveMap(created, nil))
	})
	_ = goalsObj.Set("close", func(call goja.FunctionCall) goja.Value {
		ref := optArgString(call, 0)
		p, err := s.resolveProject("")
		if err != nil {
			s.throwErr(vm, err)
		}
		obj, err := s.resolveObjective(p.ID, ref)
		if err != nil {
			s.throwErr(vm, err)
		}
		st := objective.StatusAchieved
		updated, err := s.objSvc.UpdateObjective(s.ctx, objective.UpdateObjectiveInput{
			WorkspaceID: s.wsID,
			ObjectiveID: obj.ID,
			Status:      &st,
		})
		if err != nil {
			s.throwErr(vm, err)
		}
		s.mutations++
		tasks, _ := s.listAllProjectTasks(p.ID)
		return vm.ToValue(objectiveMap(updated, tasks))
	})

	tasksObj := vm.NewObject()
	searchTasksFn := func(call goja.FunctionCall) goja.Value {
		p, err := s.resolveProject("")
		if err != nil {
			s.throwErr(vm, err)
		}
		var q language.SearchQuery
		if len(call.Arguments) > 0 && !goja.IsUndefined(call.Arguments[0]) && !goja.IsNull(call.Arguments[0]) {
			exp := exportJSValue(call.Arguments[0])
			switch v := exp.(type) {
			case string:
				q = language.ParseSearchQuery(v)
			case map[string]any:
				q = mapToSearchQuery(v)
			}
		}
		matched, err := s.searchProjectTasks(p.ID, q)
		if err != nil {
			s.throwErr(vm, err)
		}
		return vm.ToValue(matched)
	}
	_ = tasksObj.Set("list", searchTasksFn)
	_ = tasksObj.Set("search", searchTasksFn)
	_ = tasksObj.Set("ready", func(call goja.FunctionCall) goja.Value {
		p, err := s.resolveProject("")
		if err != nil {
			s.throwErr(vm, err)
		}
		unblocked := true
		items, err := s.taskSvc.PollWorkQueue(s.ctx, task.AgentWorkQueueFilter{
			WorkspaceID:   s.wsID,
			ProjectID:     p.ID,
			Statuses:      []task.Status{task.StatusReady, task.StatusBacklog},
			OnlyUnblocked: true,
			OnlyUnclaimed: true,
			Limit:         50,
		})
		_ = unblocked
		if err != nil {
			s.throwErr(vm, err)
		}
		out := make([]map[string]any, 0, len(items))
		for _, it := range items {
			out = append(out, summaryMap(it))
		}
		return vm.ToValue(out)
	})
	_ = tasksObj.Set("get", func(call goja.FunctionCall) goja.Value {
		t, err := s.resolveTask(optArgString(call, 0))
		if err != nil {
			s.throwErr(vm, err)
		}
		return vm.ToValue(s.fullTaskMap(t))
	})
	_ = tasksObj.Set("getByExternal", func(call goja.FunctionCall) goja.Value {
		extID := optArgString(call, 0)
		projRef := optArgString(call, 1)
		p, err := s.resolveProject(projRef)
		if err != nil {
			s.throwErr(vm, err)
		}
		t, err := s.taskSvc.GetTaskByExternalID(s.ctx, s.wsID, p.ID, extID)
		if err != nil {
			if errors.Is(err, errs.ErrNotFound) {
				return goja.Null()
			}
			s.throwErr(vm, err)
		}
		return vm.ToValue(s.fullTaskMap(t))
	})
	_ = tasksObj.Set("next", func(call goja.FunctionCall) goja.Value {
		p, err := s.resolveProject("")
		if err != nil {
			s.throwErr(vm, err)
		}
		claim := false
		ttl := int64(1800)
		if len(call.Arguments) > 0 && !goja.IsUndefined(call.Arguments[0]) && !goja.IsNull(call.Arguments[0]) {
			if m, ok := exportJSValue(call.Arguments[0]).(map[string]any); ok {
				if b, okB := m["claim"].(bool); okB {
					claim = b
				}
				if v := intField(m, "ttlSeconds", "ttl_seconds", "ttl"); v > 0 {
					ttl = int64(v)
				}
			}
		}
		ready, err := s.taskSvc.PollWorkQueue(s.ctx, task.AgentWorkQueueFilter{
			WorkspaceID:   s.wsID,
			ProjectID:     p.ID,
			Statuses:      []task.Status{task.StatusReady, task.StatusBacklog},
			OnlyUnblocked: true,
			OnlyUnclaimed: true,
			Limit:         5,
		})
		if err != nil {
			s.throwErr(vm, err)
		}
		if len(ready) == 0 {
			return goja.Null()
		}
		bestID := ready[0].ID
		if claim {
			if _, err := s.taskSvc.ClaimTask(s.ctx, task.ClaimTaskInput{
				WorkspaceID: s.wsID,
				TaskID:      bestID,
				TTLSeconds:  ttl,
			}); err != nil {
				s.throwErr(vm, err)
			}
			_, _ = s.taskSvc.TransitionTask(s.ctx, task.TransitionTaskInput{
				WorkspaceID:  s.wsID,
				TaskID:       bestID,
				TargetStatus: task.StatusInProgress,
			})
			s.mutations++
		}
		full, err := s.taskSvc.GetTask(s.ctx, s.wsID, bestID)
		if err != nil {
			s.throwErr(vm, err)
		}
		return vm.ToValue(s.fullTaskMap(full))
	})
	_ = tasksObj.Set("create", func(call goja.FunctionCall) goja.Value {
		m := argMap(vm, call, 0)
		p, err := s.resolveProject(strField(m, "project"))
		if err != nil {
			s.throwErr(vm, err)
		}
		var objID *uuid.UUID
		if objRef := strField(m, "objective", "goal"); objRef != "" {
			o, oErr := s.resolveObjective(p.ID, objRef)
			if oErr != nil {
				s.throwErr(vm, oErr)
			}
			objID = &o.ID
		}
		var parentID *uuid.UUID
		if parRef := strField(m, "parent"); parRef != "" {
			pt, pErr := s.resolveTask(parRef)
			if pErr != nil {
				s.throwErr(vm, pErr)
			}
			parentID = &pt.ID
		}
		st := task.StatusReady
		if rawSt := strField(m, "state", "status"); rawSt != "" {
			if mapped, ok := language.NormalizeStatusToken(rawSt); ok {
				st = mapped
			}
		}
		kind := task.KindFeature
		if rawKind := strings.ToLower(strField(m, "kind")); rawKind != "" {
			switch task.Kind(rawKind) {
			case task.KindEpic, task.KindFeature, task.KindBug, task.KindChore, task.KindResearch:
				kind = task.Kind(rawKind)
			}
		}
		created, err := s.taskSvc.CreateTask(s.ctx, task.CreateTaskInput{
			WorkspaceID:        s.wsID,
			ProjectID:          p.ID,
			ObjectiveID:        objID,
			ParentTaskID:       parentID,
			ExternalID:         strField(m, "externalId", "external_id"),
			Kind:               kind,
			Title:              strField(m, "title", "name"),
			Description:        strField(m, "description", "body"),
			AcceptanceCriteria: strSliceField(m, "acceptance", "acceptanceCriteria"),
			Status:             st,
			Priority:           parseJSTaskPriority(m["priority"]),
			Tags:               strSliceField(m, "tags", "labels"),
		})
		if err != nil {
			s.throwErr(vm, err)
		}
		s.mutations++
		for _, depRef := range strSliceField(m, "dependsOn", "depends_on", "blockedBy", "blocked_by") {
			up, dErr := s.resolveTask(depRef)
			if dErr != nil {
				s.throwErr(vm, dErr)
			}
			if _, rErr := s.taskSvc.AddRelation(s.ctx, task.AddRelationInput{
				WorkspaceID:  s.wsID,
				SourceTaskID: up.ID,
				TargetTaskID: created.ID,
				Type:         task.RelationBlocks,
			}); rErr != nil {
				s.throwErr(vm, rErr)
			}
		}
		reloaded, _ := s.taskSvc.GetTask(s.ctx, s.wsID, created.ID)
		return vm.ToValue(s.fullTaskMap(reloaded))
	})
	_ = tasksObj.Set("upsertExternal", func(call goja.FunctionCall) goja.Value {
		m := argMap(vm, call, 0)
		extID := strField(m, "externalId", "external_id")
		if extID == "" {
			s.throwErr(vm, errs.Validation("flowsk.tasks.upsertExternal", "externalId is required"))
		}
		p, err := s.resolveProject(strField(m, "project"))
		if err != nil {
			s.throwErr(vm, err)
		}
		existing, getErr := s.taskSvc.GetTaskByExternalID(s.ctx, s.wsID, p.ID, extID)
		if getErr != nil && !errors.Is(getErr, errs.ErrNotFound) {
			s.throwErr(vm, getErr)
		}
		if getErr == nil {
			upd := task.UpdateTaskInput{
				WorkspaceID: s.wsID,
				TaskID:      existing.ID,
			}
			if v := strField(m, "title", "name"); v != "" {
				upd.Title = &v
			}
			if _, hasDesc := m["description"]; hasDesc {
				v := strField(m, "description", "body")
				upd.Description = &v
			}
			if rawP, hasP := m["priority"]; hasP {
				pr := parseJSTaskPriority(rawP)
				upd.Priority = &pr
			}
			if tags := strSliceField(m, "tags", "labels"); len(tags) > 0 {
				upd.Tags = tags
			}
			if acc := strSliceField(m, "acceptance", "acceptanceCriteria"); len(acc) > 0 {
				upd.AcceptanceCriteria = acc
			}
			updated, uErr := s.taskSvc.UpdateTask(s.ctx, upd)
			if uErr != nil {
				s.throwErr(vm, uErr)
			}
			if rawSt := strField(m, "state", "status"); rawSt != "" {
				if mapped, ok := language.NormalizeStatusToken(rawSt); ok && mapped != updated.Status {
					updated, uErr = s.taskSvc.TransitionTask(s.ctx, task.TransitionTaskInput{
						WorkspaceID:        s.wsID,
						TaskID:             existing.ID,
						TargetStatus:       mapped,
						ReleaseActiveLease: mapped.IsTerminal(),
					})
					if uErr != nil {
						s.throwErr(vm, uErr)
					}
				}
			}
			s.mutations++
			res := s.fullTaskMap(updated)
			res["upserted"] = "updated"
			return vm.ToValue(res)
		}

		var objID *uuid.UUID
		if objRef := strField(m, "objective", "goal"); objRef != "" {
			o, oErr := s.resolveObjective(p.ID, objRef)
			if oErr != nil {
				s.throwErr(vm, oErr)
			}
			objID = &o.ID
		}
		var parentID *uuid.UUID
		if parRef := strField(m, "parent"); parRef != "" {
			pt, pErr := s.resolveTask(parRef)
			if pErr != nil {
				s.throwErr(vm, pErr)
			}
			parentID = &pt.ID
		}
		st := task.StatusReady
		if rawSt := strField(m, "state", "status"); rawSt != "" {
			if mapped, ok := language.NormalizeStatusToken(rawSt); ok {
				st = mapped
			}
		}
		created, cErr := s.taskSvc.CreateTask(s.ctx, task.CreateTaskInput{
			WorkspaceID:        s.wsID,
			ProjectID:          p.ID,
			ObjectiveID:        objID,
			ParentTaskID:       parentID,
			ExternalID:         extID,
			Kind:               task.KindFeature,
			Title:              strField(m, "title", "name"),
			Description:        strField(m, "description", "body"),
			AcceptanceCriteria: strSliceField(m, "acceptance", "acceptanceCriteria"),
			Status:             st,
			Priority:           parseJSTaskPriority(m["priority"]),
			Tags:               strSliceField(m, "tags", "labels"),
		})
		if cErr != nil {
			s.throwErr(vm, cErr)
		}
		s.mutations++
		for _, depRef := range strSliceField(m, "dependsOn", "depends_on", "blockedBy", "blocked_by") {
			up, dErr := s.resolveTask(depRef)
			if dErr != nil {
				s.throwErr(vm, dErr)
			}
			if _, rErr := s.taskSvc.AddRelation(s.ctx, task.AddRelationInput{
				WorkspaceID:  s.wsID,
				SourceTaskID: up.ID,
				TargetTaskID: created.ID,
				Type:         task.RelationBlocks,
			}); rErr != nil {
				s.throwErr(vm, rErr)
			}
		}
		reloaded, _ := s.taskSvc.GetTask(s.ctx, s.wsID, created.ID)
		res := s.fullTaskMap(reloaded)
		res["upserted"] = "created"
		return vm.ToValue(res)
	})
	_ = tasksObj.Set("update", func(call goja.FunctionCall) goja.Value {
		t, err := s.resolveTask(optArgString(call, 0))
		if err != nil {
			s.throwErr(vm, err)
		}
		m := argMap(vm, call, 1)
		upd := task.UpdateTaskInput{
			WorkspaceID: s.wsID,
			TaskID:      t.ID,
		}
		if v := strField(m, "title"); v != "" {
			upd.Title = &v
		}
		if _, ok := m["description"]; ok {
			v := strField(m, "description")
			upd.Description = &v
		}
		if rawExt, hasExt := m["externalId"]; hasExt {
			v := strings.TrimSpace(fmt.Sprint(rawExt))
			if rawExt == nil {
				v = ""
			}
			upd.ExternalID = &v
		} else if rawExt, hasExt2 := m["external_id"]; hasExt2 {
			v := strings.TrimSpace(fmt.Sprint(rawExt))
			if rawExt == nil {
				v = ""
			}
			upd.ExternalID = &v
		}
		if rawP, ok := m["priority"]; ok {
			p := parseJSTaskPriority(rawP)
			upd.Priority = &p
		}
		if tags := strSliceField(m, "tags", "labels"); len(tags) > 0 {
			upd.Tags = tags
		}
		if acc := strSliceField(m, "acceptance", "acceptanceCriteria"); len(acc) > 0 {
			upd.AcceptanceCriteria = acc
		}
		updated, err := s.taskSvc.UpdateTask(s.ctx, upd)
		if err != nil {
			s.throwErr(vm, err)
		}
		s.mutations++
		return vm.ToValue(s.fullTaskMap(updated))
	})
	_ = tasksObj.Set("move", func(call goja.FunctionCall) goja.Value {
		t, err := s.resolveTask(optArgString(call, 0))
		if err != nil {
			s.throwErr(vm, err)
		}
		targetRaw := optArgString(call, 1)
		note := optArgString(call, 2)
		st, ok := language.NormalizeStatusToken(targetRaw)
		if !ok {
			s.throwErr(vm, errs.Validation("flowsk.tasks.move", fmt.Sprintf("unknown state %q", targetRaw)))
		}
		updated, err := s.taskSvc.TransitionTask(s.ctx, task.TransitionTaskInput{
			WorkspaceID:        s.wsID,
			TaskID:             t.ID,
			TargetStatus:       st,
			ResolutionComment:  note,
			ReleaseActiveLease: st.IsTerminal(),
		})
		if err != nil {
			s.throwErr(vm, err)
		}
		s.mutations++
		return vm.ToValue(s.fullTaskMap(updated))
	})
	_ = tasksObj.Set("done", func(call goja.FunctionCall) goja.Value {
		t, err := s.resolveTask(optArgString(call, 0))
		if err != nil {
			s.throwErr(vm, err)
		}
		note := optArgString(call, 1)
		updated, err := s.taskSvc.TransitionTask(s.ctx, task.TransitionTaskInput{
			WorkspaceID:        s.wsID,
			TaskID:             t.ID,
			TargetStatus:       task.StatusDone,
			ResolutionComment:  note,
			ReleaseActiveLease: true,
		})
		if err != nil {
			s.throwErr(vm, err)
		}
		s.mutations++
		return vm.ToValue(s.fullTaskMap(updated))
	})
	_ = tasksObj.Set("claim", func(call goja.FunctionCall) goja.Value {
		t, err := s.resolveTask(optArgString(call, 0))
		if err != nil {
			s.throwErr(vm, err)
		}
		ttl := int64(1800)
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) {
			if n := call.Arguments[1].ToInteger(); n > 0 {
				ttl = n
			}
		}
		if _, err := s.taskSvc.ClaimTask(s.ctx, task.ClaimTaskInput{
			WorkspaceID: s.wsID,
			TaskID:      t.ID,
			TTLSeconds:  ttl,
		}); err != nil {
			s.throwErr(vm, err)
		}
		if t.Status == task.StatusBacklog || t.Status == task.StatusReady {
			_, _ = s.taskSvc.TransitionTask(s.ctx, task.TransitionTaskInput{
				WorkspaceID:  s.wsID,
				TaskID:       t.ID,
				TargetStatus: task.StatusInProgress,
			})
		}
		s.mutations++
		reloaded, _ := s.taskSvc.GetTask(s.ctx, s.wsID, t.ID)
		return vm.ToValue(s.fullTaskMap(reloaded))
	})
	_ = tasksObj.Set("release", func(call goja.FunctionCall) goja.Value {
		t, err := s.resolveTask(optArgString(call, 0))
		if err != nil {
			s.throwErr(vm, err)
		}
		var stateRaw, note string
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
			if m, ok := exportJSValue(call.Arguments[1]).(map[string]any); ok {
				stateRaw = strField(m, "state", "status")
				note = strField(m, "note", "comment")
			}
		}
		if err := s.taskSvc.ReleaseTask(s.ctx, task.ReleaseTaskInput{
			WorkspaceID:    s.wsID,
			TaskID:         t.ID,
			HandoffComment: note,
		}); err != nil {
			s.throwErr(vm, err)
		}
		if st, ok := language.NormalizeStatusToken(stateRaw); ok {
			_, err = s.taskSvc.TransitionTask(s.ctx, task.TransitionTaskInput{
				WorkspaceID:  s.wsID,
				TaskID:       t.ID,
				TargetStatus: st,
			})
			if err != nil {
				s.throwErr(vm, err)
			}
		}
		s.mutations++
		reloaded, _ := s.taskSvc.GetTask(s.ctx, s.wsID, t.ID)
		return vm.ToValue(s.fullTaskMap(reloaded))
	})
	_ = tasksObj.Set("block", func(call goja.FunctionCall) goja.Value {
		t, err := s.resolveTask(optArgString(call, 0))
		if err != nil {
			s.throwErr(vm, err)
		}
		m := argMap(vm, call, 1)
		for _, upRef := range strSliceField(m, "by", "blockedBy", "dependsOn") {
			up, uErr := s.resolveTask(upRef)
			if uErr != nil {
				s.throwErr(vm, uErr)
			}
			if _, rErr := s.taskSvc.AddRelation(s.ctx, task.AddRelationInput{
				WorkspaceID:  s.wsID,
				SourceTaskID: up.ID,
				TargetTaskID: t.ID,
				Type:         task.RelationBlocks,
			}); rErr != nil {
				s.throwErr(vm, rErr)
			}
		}
		reason := strField(m, "reason", "note")
		if len(strSliceField(m, "by", "blockedBy", "dependsOn")) == 0 {
			_, err = s.taskSvc.TransitionTask(s.ctx, task.TransitionTaskInput{
				WorkspaceID:       s.wsID,
				TaskID:            t.ID,
				TargetStatus:      task.StatusBlocked,
				ResolutionComment: reason,
			})
			if err != nil {
				s.throwErr(vm, err)
			}
		} else if reason != "" {
			_, _ = s.taskSvc.AddComment(s.ctx, task.AddCommentInput{
				WorkspaceID: s.wsID,
				TaskID:      t.ID,
				Kind:        task.CommentKindBlocker,
				Body:        reason,
			})
		}
		s.mutations++
		reloaded, _ := s.taskSvc.GetTask(s.ctx, s.wsID, t.ID)
		return vm.ToValue(s.fullTaskMap(reloaded))
	})
	_ = tasksObj.Set("unblock", func(call goja.FunctionCall) goja.Value {
		t, err := s.resolveTask(optArgString(call, 0))
		if err != nil {
			s.throwErr(vm, err)
		}
		rels, _ := s.taskSvc.ListRelations(s.ctx, s.wsID, t.ID)
		for _, r := range rels {
			if r.TargetTaskID == t.ID && r.Type == task.RelationBlocks {
				_ = s.taskSvc.RemoveRelation(s.ctx, s.wsID, r.ID)
			}
		}
		reloaded, _ := s.taskSvc.GetTask(s.ctx, s.wsID, t.ID)
		if reloaded.Status == task.StatusBlocked && reloaded.UnresolvedBlockerCount == 0 {
			reloaded, _ = s.taskSvc.TransitionTask(s.ctx, task.TransitionTaskInput{
				WorkspaceID:  s.wsID,
				TaskID:       t.ID,
				TargetStatus: task.StatusReady,
			})
		}
		s.mutations++
		return vm.ToValue(s.fullTaskMap(reloaded))
	})
	_ = tasksObj.Set("comment", func(call goja.FunctionCall) goja.Value {
		t, err := s.resolveTask(optArgString(call, 0))
		if err != nil {
			s.throwErr(vm, err)
		}
		body := optArgString(call, 1)
		kindRaw := strings.ToLower(optArgString(call, 2))
		ck := task.CommentKindNote
		switch kindRaw {
		case "progress", "verification":
			ck = task.CommentKindProgress
		case "decision":
			ck = task.CommentKindDecision
		case "handoff":
			ck = task.CommentKindHandoff
		case "blocker":
			ck = task.CommentKindBlocker
		}
		cm, err := s.taskSvc.AddComment(s.ctx, task.AddCommentInput{
			WorkspaceID: s.wsID,
			TaskID:      t.ID,
			Kind:        ck,
			Body:        body,
		})
		if err != nil {
			s.throwErr(vm, err)
		}
		s.mutations++
		return vm.ToValue(map[string]any{
			"id":     cm.ID.String(),
			"task":   t.Key,
			"kind":   string(cm.Kind),
			"author": firstNonEmpty(cm.AuthorHandle, s.ident.Handle),
			"body":   cm.Body,
		})
	})

	root := vm.NewObject()
	_ = root.Set("projects", projectsObj)
	_ = root.Set("goals", goalsObj)
	_ = root.Set("objectives", goalsObj)
	_ = root.Set("tasks", tasksObj)
	_ = root.Set("search", func(call goja.FunctionCall) goja.Value {
		raw := optArgString(call, 0)
		q := language.ParseSearchQuery(raw)
		if q.Scope == language.SearchScopeSDK {
			return vm.ToValue(language.SearchCodeModeSDK(raw))
		}
		p, err := s.resolveProject("")
		if err != nil {
			s.throwErr(vm, err)
		}
		res := map[string]any{}
		if q.Scope == language.SearchScopeAll || q.Scope == language.SearchScopeTasks {
			tasks, tErr := s.searchProjectTasks(p.ID, q)
			if tErr != nil {
				s.throwErr(vm, tErr)
			}
			res["tasks"] = tasks
		}
		if q.Scope == language.SearchScopeAll || q.Scope == language.SearchScopeGoals {
			objs, _ := s.objSvc.ListObjectives(s.ctx, objective.ListObjectivesFilter{
				WorkspaceID: s.wsID,
				ProjectID:   p.ID,
				Limit:       100,
			})
			allTasks, _ := s.listAllProjectTasks(p.ID)
			var matchedGoals []map[string]any
			for _, o := range objs {
				if q.MatchesText(o.Slug, o.Title, o.Summary, string(o.Status)) {
					matchedGoals = append(matchedGoals, objectiveMap(o, allTasks))
				}
			}
			res["goals"] = matchedGoals
		}
		return vm.ToValue(res)
	})
	_ = root.Set("board", func(_ goja.FunctionCall) goja.Value {
		p, err := s.resolveProject("")
		if err != nil {
			s.throwErr(vm, err)
		}
		objs, _ := s.objSvc.ListObjectives(s.ctx, objective.ListObjectivesFilter{WorkspaceID: s.wsID, ProjectID: p.ID, Limit: 100})
		tasks, _ := s.listAllProjectTasks(p.ID)
		goals := make([]map[string]any, 0, len(objs))
		for _, o := range objs {
			goals = append(goals, objectiveMap(o, tasks))
		}
		rows := make([]map[string]any, 0, len(tasks))
		for _, t := range tasks {
			rows = append(rows, summaryMap(t))
		}
		return vm.ToValue(map[string]any{
			"project": projectMap(p),
			"goals":   goals,
			"tasks":   rows,
		})
	})

	if err := vm.Set("flowsk", root); err != nil {
		return err
	}
	return vm.Set("api", root)
}

func (s *scriptSession) resolveProject(ref string) (project.Project, error) {
	trimmed := strings.TrimSpace(ref)
	if trimmed != "" {
		if pid, err := uuid.Parse(trimmed); err == nil {
			return s.projSvc.GetProject(s.ctx, s.wsID, pid)
		}
		if p, err := s.projSvc.GetProjectBySlug(s.ctx, s.wsID, strings.ToLower(trimmed)); err == nil {
			return p, nil
		}
	}
	if trimmed == "" && s.defaultProj != nil && *s.defaultProj != uuid.Nil {
		return s.projSvc.GetProject(s.ctx, s.wsID, *s.defaultProj)
	}
	items, err := s.projSvc.ListProjects(s.ctx, project.ListProjectsFilter{WorkspaceID: s.wsID, Limit: 100})
	if err != nil {
		return project.Project{}, err
	}
	if trimmed != "" {
		for _, it := range items {
			if strings.EqualFold(it.KeyPrefix, trimmed) || strings.EqualFold(it.Slug, trimmed) {
				return it, nil
			}
		}
		return project.Project{}, errs.New(errs.ErrNotFound, "codemode.resolveProject", fmt.Sprintf("project %q not found", trimmed))
	}
	if len(items) == 1 {
		return items[0], nil
	}
	return project.Project{}, errs.Validation("codemode.resolveProject", "project is required when workspace has zero or multiple projects")
}

func (s *scriptSession) resolveObjective(projID uuid.UUID, ref string) (objective.Objective, error) {
	trimmed := strings.TrimSpace(ref)
	if oid, err := uuid.Parse(trimmed); err == nil {
		return s.objSvc.GetObjective(s.ctx, s.wsID, oid)
	}
	if o, err := s.objSvc.GetObjectiveBySlug(s.ctx, s.wsID, projID, strings.ToLower(trimmed)); err == nil {
		return o, nil
	}
	items, err := s.objSvc.ListObjectives(s.ctx, objective.ListObjectivesFilter{WorkspaceID: s.wsID, ProjectID: projID, Limit: 100})
	if err != nil {
		return objective.Objective{}, err
	}
	for _, it := range items {
		if strings.EqualFold(it.Slug, trimmed) || strings.EqualFold(it.Title, trimmed) {
			return it, nil
		}
	}
	return objective.Objective{}, errs.New(errs.ErrNotFound, "codemode.resolveObjective", fmt.Sprintf("goal/objective %q not found", trimmed))
}

func (s *scriptSession) resolveTask(ref string) (task.Task, error) {
	trimmed := strings.TrimSpace(ref)
	if tid, err := uuid.Parse(trimmed); err == nil {
		return s.taskSvc.GetTask(s.ctx, s.wsID, tid)
	}
	if t, err := s.taskSvc.GetTaskByKey(s.ctx, s.wsID, strings.ToUpper(trimmed)); err == nil {
		return t, nil
	}
	if p, pErr := s.resolveProject(""); pErr == nil && p.ID != uuid.Nil {
		if t, extErr := s.taskSvc.GetTaskByExternalID(s.ctx, s.wsID, p.ID, trimmed); extErr == nil {
			return t, nil
		}
	}
	return s.taskSvc.GetTaskByKey(s.ctx, s.wsID, strings.ToUpper(trimmed))
}

func (s *scriptSession) listAllProjectTasks(projID uuid.UUID) ([]task.CompactTaskSummary, error) {
	return s.taskSvc.PollWorkQueue(s.ctx, task.AgentWorkQueueFilter{
		WorkspaceID: s.wsID,
		ProjectID:   projID,
		Statuses: []task.Status{
			task.StatusBacklog, task.StatusReady, task.StatusInProgress,
			task.StatusInReview, task.StatusBlocked, task.StatusDone, task.StatusCancelled,
		},
		Limit: 100,
	})
}

func (s *scriptSession) searchProjectTasks(projID uuid.UUID, q language.SearchQuery) ([]map[string]any, error) {
	all, err := s.listAllProjectTasks(projID)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, it := range all {
		if len(q.Statuses) > 0 && !slices.Contains(q.Statuses, it.Status) {
			continue
		}
		if q.Priority != nil && it.Priority != *q.Priority {
			continue
		}
		if q.Objective != "" && !strings.EqualFold(it.ObjectiveSlug, q.Objective) {
			continue
		}
		if q.Parent != "" && !strings.EqualFold(it.ParentKey, q.Parent) {
			continue
		}
		if q.ExternalID != "" && it.ExternalID != q.ExternalID {
			continue
		}
		if q.Actor != "" && !strings.EqualFold(it.HolderHandle, q.Actor) {
			continue
		}
		isBlocked := it.UnresolvedBlockerCount > 0 || it.Status == task.StatusBlocked
		if q.Blocked != nil && isBlocked != *q.Blocked {
			continue
		}
		isClaimed := strings.TrimSpace(it.HolderHandle) != ""
		if q.Claimed != nil && isClaimed != *q.Claimed {
			continue
		}
		if len(q.Tags) > 0 {
			okTags := true
			for _, want := range q.Tags {
				found := false
				for _, got := range it.Tags {
					if strings.EqualFold(got, want) {
						found = true
						break
					}
				}
				if !found {
					okTags = false
					break
				}
			}
			if !okTags {
				continue
			}
		}
		if len(q.Terms) > 0 {
			full, gErr := s.taskSvc.GetTask(s.ctx, s.wsID, it.ID)
			var commentText string
			if cms, cErr := s.taskSvc.ListComments(s.ctx, s.wsID, it.ID, 20); cErr == nil {
				var cb strings.Builder
				for _, cm := range cms {
					cb.WriteString(cm.Body)
					cb.WriteByte('\n')
				}
				commentText = cb.String()
			}
			if gErr == nil {
				if !q.MatchesText(it.Key, it.ExternalID, it.Title, full.Description, strings.Join(full.AcceptanceCriteriaCopy(), " "), strings.Join(it.Tags, " "), commentText) {
					continue
				}
			} else if !q.MatchesText(it.Key, it.ExternalID, it.Title, strings.Join(it.Tags, " ")) {
				continue
			}
		}
		out = append(out, summaryMap(it))
	}
	return out, nil
}

func (s *scriptSession) fullTaskMap(t task.Task) map[string]any {
	objKey := ""
	if t.ObjectiveID != nil {
		if o, err := s.objSvc.GetObjective(s.ctx, s.wsID, *t.ObjectiveID); err == nil {
			objKey = o.Slug
		}
	}
	holder := ""
	if t.ActiveLease != nil {
		holder = firstNonEmpty(t.ActiveLease.HolderHandle, t.ActiveLease.HolderActorID.String())
	}
	return map[string]any{
		"id":          t.ID.String(),
		"key":         t.Key,
		"externalId":  t.ExternalID,
		"title":       t.Title,
		"description": t.Description,
		"status":      string(t.Status),
		"state":       string(t.Status),
		"priority":    int(t.Priority),
		"tags":        t.TagsCopy(),
		"objective":   objKey,
		"holder":      holder,
		"blocked":     t.UnresolvedBlockerCount > 0 || t.Status == task.StatusBlocked,
		"acceptance":  t.AcceptanceCriteriaCopy(),
	}
}

func projectMap(p project.Project) map[string]any {
	return map[string]any{
		"id":          p.ID.String(),
		"slug":        p.Slug,
		"key":         p.KeyPrefix,
		"name":        p.Name,
		"description": p.Description,
		"status":      string(p.Status),
	}
}

func objectiveMap(o objective.Objective, tasks []task.CompactTaskSummary) map[string]any {
	total, done := 0, 0
	for _, t := range tasks {
		if strings.EqualFold(t.ObjectiveSlug, o.Slug) {
			total++
			if t.Status == task.StatusDone {
				done++
			}
		}
	}
	return map[string]any{
		"id":             o.ID.String(),
		"key":            o.Slug,
		"slug":           o.Slug,
		"title":          o.Title,
		"summary":        o.Summary,
		"status":         string(o.Status),
		"priority":       int(o.PriorityRank),
		"totalTasks":     total,
		"completedTasks": done,
	}
}

func summaryMap(s task.CompactTaskSummary) map[string]any {
	return map[string]any{
		"id":         s.ID.String(),
		"key":        s.Key,
		"externalId": s.ExternalID,
		"title":      s.Title,
		"status":     string(s.Status),
		"state":      string(s.Status),
		"priority":   int(s.Priority),
		"objective":  s.ObjectiveSlug,
		"parent":     s.ParentKey,
		"holder":     s.HolderHandle,
		"tags":       append([]string(nil), s.Tags...),
		"blocked":    s.UnresolvedBlockerCount > 0 || s.Status == task.StatusBlocked,
	}
}

func mapToSearchQuery(m map[string]any) language.SearchQuery {
	q := language.ParseSearchQuery(strField(m, "q", "query"))
	if stRaw := strField(m, "state", "status"); stRaw != "" {
		if st, ok := language.NormalizeStatusToken(stRaw); ok {
			q.Statuses = append(q.Statuses, st)
		}
	}
	if tag := strField(m, "tag", "label"); tag != "" {
		q.Tags = append(q.Tags, strings.ToLower(tag))
	}
	if obj := strField(m, "objective", "goal"); obj != "" {
		q.Objective = obj
	}
	if ext := strField(m, "externalId", "external_id", "ext"); ext != "" {
		q.ExternalID = ext
	}
	if act := strField(m, "actor", "assignee"); act != "" {
		q.Actor = act
	}
	if b, ok := m["blocked"].(bool); ok {
		q.Blocked = &b
	}
	return q
}

func exportJSValue(v goja.Value) any {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil
	}
	return v.Export()
}

func formatExecutionSummary(val any, console []string, mutations int, dryRun bool) string {
	var lines []string
	mode := "commit"
	if dryRun {
		mode = "dry_run"
	}
	lines = append(lines, fmt.Sprintf("codemode(%s) mutations=%d", mode, mutations))
	for _, c := range console {
		lines = append(lines, "log: "+c)
	}
	if val != nil {
		b, err := json.Marshal(val)
		if err == nil {
			lines = append(lines, "result: "+string(b))
		}
	}
	return strings.Join(lines, "\n")
}

func optArgString(call goja.FunctionCall, idx int) string {
	if idx >= len(call.Arguments) || goja.IsUndefined(call.Arguments[idx]) || goja.IsNull(call.Arguments[idx]) {
		return ""
	}
	return strings.TrimSpace(call.Arguments[idx].String())
}

func argMap(vm *goja.Runtime, call goja.FunctionCall, idx int) map[string]any {
	if idx >= len(call.Arguments) || goja.IsUndefined(call.Arguments[idx]) || goja.IsNull(call.Arguments[idx]) {
		panic(vm.NewTypeError("expected options object argument"))
	}
	m, ok := call.Arguments[idx].Export().(map[string]any)
	if !ok {
		panic(vm.NewTypeError("expected plain object argument"))
	}
	return m
}

func strField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if raw, ok := m[k]; ok && raw != nil {
			if s, okS := raw.(string); okS && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func intField(m map[string]any, keys ...string) int {
	for _, k := range keys {
		if raw, ok := m[k]; ok && raw != nil {
			switch n := raw.(type) {
			case int64:
				return int(n)
			case int:
				return n
			case float64:
				return int(n)
			}
		}
	}
	return 0
}

func strSliceField(m map[string]any, keys ...string) []string {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok || raw == nil {
			continue
		}
		switch v := raw.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return []string{strings.TrimSpace(v)}
			}
		case []string:
			return append([]string(nil), v...)
		case []any:
			out := make([]string, 0, len(v))
			for _, el := range v {
				if s := strings.TrimSpace(fmt.Sprint(el)); s != "" {
					out = append(out, s)
				}
			}
			return out
		}
	}
	return nil
}

func parseJSTaskPriority(raw any) task.Priority {
	switch v := raw.(type) {
	case string:
		if p, ok := language.NormalizePriorityToken(v); ok {
			return p
		}
	case int64:
		if v >= 0 && v <= 3 {
			return task.Priority(v)
		}
	case float64:
		if v >= 0 && v <= 3 {
			return task.Priority(int32(v))
		}
	}
	return task.PriorityMedium
}

func parseJSPriorityRank(raw any) int32 {
	switch v := raw.(type) {
	case string:
		if p, ok := language.NormalizePriorityToken(v); ok {
			return int32(p) + 1
		}
	case int64:
		if v >= 0 {
			return int32(v)
		}
	case float64:
		if v >= 0 {
			return int32(v)
		}
	}
	return 2
}

func slugify(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = slugNonAlnum.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) < 2 {
		return "item-" + uuid.NewString()[:8]
	}
	if len(s) > 60 {
		s = strings.Trim(s[:60], "-")
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}
