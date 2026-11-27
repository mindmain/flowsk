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
	nonUpperAlnum     = regexp.MustCompile(`[^A-Z0-9]+`)
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
			taskRepo:    scopedTaskRepo,
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
				if unwrapped := jsExc.Unwrap(); unwrapped != nil {
					return unwrapped
				}
				if excVal := jsExc.Value(); excVal != nil && !goja.IsUndefined(excVal) && !goja.IsNull(excVal) {
					if goErr, ok := excVal.Export().(error); ok && goErr != nil {
						return goErr
					}
				}
				return errs.Validation(op, fmt.Sprintf("JavaScript runtime error: %s", jsExc.Value().String()))
			}
			return errs.Validation(op, fmt.Sprintf("JavaScript error: %v", runErr))
		}

		exported := exportJSValue(val)
		if exported != nil {
			if _, marshalErr := json.Marshal(exported); marshalErr != nil {
				return errs.Validation(op, fmt.Sprintf("JavaScript return value is not JSON-serializable: %v", marshalErr), errs.FieldViolation{
					Field:       "return_value",
					Description: marshalErr.Error(),
				})
			}
		}
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
		flushCtx, flushCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer flushCancel()
		if flushErr := stagedMD.flush(flushCtx, in.WorkspaceID); flushErr != nil {
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
	taskRepo      task.Repository
	mutations     int
	console       []string
	lastDomainErr error
}

func (s *scriptSession) hostFn(fn func(goja.FunctionCall) goja.Value) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		s.lastDomainErr = nil
		return fn(call)
	}
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
	_ = projectsObj.Set("list", s.hostFn(func(_ goja.FunctionCall) goja.Value {
		items, err := s.projSvc.ListProjects(s.ctx, project.ListProjectsFilter{WorkspaceID: s.wsID, Limit: 500})
		if err != nil {
			s.throwErr(vm, err)
		}
		out := make([]map[string]any, 0, len(items))
		for _, p := range items {
			out = append(out, projectMap(p))
		}
		return vm.ToValue(out)
	}))
	_ = projectsObj.Set("get", s.hostFn(func(call goja.FunctionCall) goja.Value {
		ref := optArgString(call, 0)
		p, err := s.resolveProject(ref)
		if err != nil {
			s.throwErr(vm, err)
		}
		return vm.ToValue(projectMap(p))
	}))
	_ = projectsObj.Set("create", s.hostFn(func(call goja.FunctionCall) goja.Value {
		m := argMap(vm, call, 0)
		key := strings.ToUpper(strings.TrimSpace(strField(m, "key", "keyPrefix")))
		name := strings.TrimSpace(strField(m, "name", "title"))
		rawSlug := strings.ToLower(strings.TrimSpace(strField(m, "slug")))
		slug := rawSlug
		if slug == "" {
			slug = slugify(firstNonEmpty(key, name))
		}
		if key == "" {
			key = deriveProjectKeyPrefix(firstNonEmpty(rawSlug, name))
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
	}))
	_ = projectsObj.Set("bind", s.hostFn(func(call goja.FunctionCall) goja.Value {
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
	}))
	_ = projectsObj.Set("unbind", s.hostFn(func(call goja.FunctionCall) goja.Value {
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
	}))
	_ = projectsObj.Set("bindings", s.hostFn(func(_ goja.FunctionCall) goja.Value {
		items, err := s.projSvc.ListBindings(s.ctx, project.ListBindingsFilter{WorkspaceID: s.wsID})
		if err != nil {
			s.throwErr(vm, err)
		}
		projs, err := s.projSvc.ListProjects(s.ctx, project.ListProjectsFilter{WorkspaceID: s.wsID, Limit: 500})
		if err != nil {
			s.throwErr(vm, err)
		}
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
	}))
	_ = projectsObj.Set("getDoc", s.hostFn(func(call goja.FunctionCall) goja.Value {
		p, err := s.resolveProject(optArgString(call, 0))
		if err != nil {
			s.throwErr(vm, err)
		}
		md, hasDoc, err := s.projSvc.GetProjectMarkdown(s.ctx, s.wsID, p.ID)
		if err != nil {
			s.throwErr(vm, err)
		}
		return vm.ToValue(map[string]any{
			"id":             p.ID.String(),
			"key":            p.KeyPrefix,
			"slug":           p.Slug,
			"markdown":       md,
			"hasMarkdownDoc": hasDoc,
		})
	}))
	_ = projectsObj.Set("setDoc", s.hostFn(func(call goja.FunctionCall) goja.Value {
		var projRef, md string
		if len(call.Arguments) == 1 {
			md = rawArgString(call, 0)
		} else {
			projRef = optArgString(call, 0)
			md = rawArgString(call, 1)
		}
		p, err := s.resolveProject(projRef)
		if err != nil {
			s.throwErr(vm, err)
		}
		var updated project.Project
		if strings.TrimSpace(md) == "" {
			updated, err = s.projSvc.DeleteProjectMarkdown(s.ctx, s.wsID, p.ID)
		} else {
			updated, err = s.projSvc.PutProjectMarkdown(s.ctx, s.wsID, p.ID, md)
		}
		if err != nil {
			s.throwErr(vm, err)
		}
		s.mutations++
		return vm.ToValue(projectMap(updated))
	}))
	_ = projectsObj.Set("deleteDoc", s.hostFn(func(call goja.FunctionCall) goja.Value {
		p, err := s.resolveProject(optArgString(call, 0))
		if err != nil {
			s.throwErr(vm, err)
		}
		updated, err := s.projSvc.DeleteProjectMarkdown(s.ctx, s.wsID, p.ID)
		if err != nil {
			s.throwErr(vm, err)
		}
		s.mutations++
		return vm.ToValue(projectMap(updated))
	}))

	goalsObj := vm.NewObject()
	_ = goalsObj.Set("list", s.hostFn(func(call goja.FunctionCall) goja.Value {
		rawFilter := optArgString(call, 0)
		p, err := s.resolveProject("")
		if err != nil {
			s.throwErr(vm, err)
		}
		objs, err := s.objSvc.ListObjectives(s.ctx, objective.ListObjectivesFilter{
			WorkspaceID: s.wsID,
			ProjectID:   p.ID,
			Limit:       500,
		})
		if err != nil {
			s.throwErr(vm, err)
		}
		tasks, err := s.listAllProjectTasks(p.ID)
		if err != nil {
			s.throwErr(vm, err)
		}
		q := language.ParseSearchQuery(rawFilter)
		out := make([]map[string]any, 0, len(objs))
		for _, o := range objs {
			if !q.MatchesText(o.Slug, o.Title, o.Summary, string(o.Status)) {
				continue
			}
			out = append(out, objectiveMap(o, tasks))
		}
		return vm.ToValue(out)
	}))
	_ = goalsObj.Set("create", s.hostFn(func(call goja.FunctionCall) goja.Value {
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
		td, tdHas, tdErr := parseOptionalTargetDate("flowsk.goals.create", m, "targetDate", "target_date", "due")
		if tdErr != nil {
			s.throwErr(vm, tdErr)
		}
		if tdHas && td != nil && td.IsZero() {
			td = nil
		}
		criteria, _ := optStrSliceField(m, "successCriteria", "criteria")
		kind := parseJSObjectiveKind(strField(m, "kind"))
		status := parseJSObjectiveStatus(strField(m, "status", "state"), objective.StatusActive)
		created, err := s.objSvc.CreateObjective(s.ctx, objective.CreateObjectiveInput{
			WorkspaceID:     s.wsID,
			ProjectID:       p.ID,
			Kind:            kind,
			Slug:            slug,
			Title:           title,
			Summary:         strField(m, "summary", "description"),
			SuccessCriteria: criteria,
			Status:          status,
			PriorityRank:    prio,
			TargetDate:      td,
		})
		if err != nil {
			s.throwErr(vm, err)
		}
		s.mutations++
		return vm.ToValue(objectiveMap(created, nil))
	}))
	_ = goalsObj.Set("update", s.hostFn(func(call goja.FunctionCall) goja.Value {
		ref := optArgString(call, 0)
		m := argMap(vm, call, 1)
		p, err := s.resolveProject(strField(m, "project"))
		if err != nil {
			s.throwErr(vm, err)
		}
		obj, err := s.resolveObjective(p.ID, ref)
		if err != nil {
			s.throwErr(vm, err)
		}
		upd := objective.UpdateObjectiveInput{
			WorkspaceID: s.wsID,
			ObjectiveID: obj.ID,
		}
		if v := strField(m, "title", "name"); v != "" {
			upd.Title = &v
		}
		if _, hasSum := m["summary"]; hasSum {
			v := strField(m, "summary")
			upd.Summary = &v
		} else if _, hasDesc := m["description"]; hasDesc {
			v := strField(m, "description")
			upd.Summary = &v
		}
		if rawKind := strField(m, "kind"); rawKind != "" {
			k := parseJSObjectiveKind(rawKind)
			upd.Kind = &k
		}
		if rawStatus := strField(m, "status", "state"); rawStatus != "" {
			st := parseJSObjectiveStatus(rawStatus, obj.Status)
			upd.Status = &st
		}
		if _, hasPrio := m["priority"]; hasPrio {
			pr := parseJSPriorityRank(m["priority"])
			upd.PriorityRank = &pr
		}
		td, tdHas, tdErr := parseOptionalTargetDate("flowsk.goals.update", m, "targetDate", "target_date", "due")
		if tdErr != nil {
			s.throwErr(vm, tdErr)
		}
		if tdHas {
			upd.TargetDate = td
		}
		if criteria, hasCriteria := optStrSliceField(m, "successCriteria", "criteria"); hasCriteria {
			upd.SuccessCriteria = criteria
		}
		updated, err := s.objSvc.UpdateObjective(s.ctx, upd)
		if err != nil {
			s.throwErr(vm, err)
		}
		s.mutations++
		tasks, err := s.listAllProjectTasks(p.ID)
		if err != nil {
			s.throwErr(vm, err)
		}
		return vm.ToValue(objectiveMap(updated, tasks))
	}))
	_ = goalsObj.Set("close", s.hostFn(func(call goja.FunctionCall) goja.Value {
		ref := optArgString(call, 0)
		var note, projRef string
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
			switch v := exportJSValue(call.Arguments[1]).(type) {
			case string:
				note = strings.TrimSpace(v)
			case map[string]any:
				note = strField(v, "note", "reason", "summary")
				projRef = strField(v, "project")
			}
		}
		p, err := s.resolveProject(projRef)
		if err != nil {
			s.throwErr(vm, err)
		}
		obj, err := s.resolveObjective(p.ID, ref)
		if err != nil {
			s.throwErr(vm, err)
		}
		st := objective.StatusAchieved
		upd := objective.UpdateObjectiveInput{
			WorkspaceID: s.wsID,
			ObjectiveID: obj.ID,
			Status:      &st,
		}
		if note != "" {
			merged := note
			if existing := strings.TrimSpace(obj.Summary); existing != "" {
				merged = existing + "\n\n" + note
			}
			upd.Summary = &merged
		}
		updated, err := s.objSvc.UpdateObjective(s.ctx, upd)
		if err != nil {
			s.throwErr(vm, err)
		}
		s.mutations++
		tasks, err := s.listAllProjectTasks(p.ID)
		if err != nil {
			s.throwErr(vm, err)
		}
		return vm.ToValue(objectiveMap(updated, tasks))
	}))

	tasksObj := vm.NewObject()
	searchTasksFn := s.hostFn(func(call goja.FunctionCall) goja.Value {
		var (
			q                language.SearchQuery
			projRef          string
			limit            int
			includeCompleted = true
			hasIncCompleted  bool
		)
		if len(call.Arguments) > 0 && !goja.IsUndefined(call.Arguments[0]) && !goja.IsNull(call.Arguments[0]) {
			exp := exportJSValue(call.Arguments[0])
			switch v := exp.(type) {
			case string:
				q = language.ParseSearchQuery(v)
			case map[string]any:
				q = mapToSearchQuery(v)
				projRef = strField(v, "project")
				limit = intField(v, "limit", "max")
				if inc, ok := v["includeCompleted"].(bool); ok {
					includeCompleted = inc
					hasIncCompleted = true
				}
			}
		}
		p, err := s.resolveProject(projRef)
		if err != nil {
			s.throwErr(vm, err)
		}
		if hasIncCompleted && !includeCompleted && len(q.Statuses) == 0 {
			q.Statuses = []task.Status{
				task.StatusBacklog, task.StatusReady, task.StatusInProgress,
				task.StatusInReview, task.StatusBlocked,
			}
		}
		matched, err := s.searchProjectTasks(p.ID, q)
		if err != nil {
			s.throwErr(vm, err)
		}
		if limit > 0 && len(matched) > limit {
			matched = matched[:limit]
		}
		return vm.ToValue(matched)
	})
	_ = tasksObj.Set("list", searchTasksFn)
	_ = tasksObj.Set("search", searchTasksFn)
	_ = tasksObj.Set("ready", s.hostFn(func(call goja.FunctionCall) goja.Value {
		limit := int32(50)
		var projRef, objRef string
		if len(call.Arguments) > 0 && !goja.IsUndefined(call.Arguments[0]) && !goja.IsNull(call.Arguments[0]) {
			if m, ok := exportJSValue(call.Arguments[0]).(map[string]any); ok {
				if n := intField(m, "limit", "max"); n > 0 {
					limit = int32(n)
				}
				projRef = strField(m, "project")
				objRef = strField(m, "objective", "goal")
			} else if n := call.Arguments[0].ToInteger(); n > 0 {
				limit = int32(n)
			}
		}
		p, err := s.resolveProject(projRef)
		if err != nil {
			s.throwErr(vm, err)
		}
		filter := task.AgentWorkQueueFilter{
			WorkspaceID:   s.wsID,
			ProjectID:     p.ID,
			Statuses:      []task.Status{task.StatusReady, task.StatusBacklog},
			OnlyUnblocked: true,
			OnlyUnclaimed: true,
			Limit:         limit,
		}
		if objRef != "" {
			o, oErr := s.resolveObjective(p.ID, objRef)
			if oErr != nil {
				s.throwErr(vm, oErr)
			}
			filter.ObjectiveID = &o.ID
		}
		items, err := s.taskSvc.PollWorkQueue(s.ctx, filter)
		if err != nil {
			s.throwErr(vm, err)
		}
		if int32(len(items)) > limit {
			items = items[:limit]
		}
		out := make([]map[string]any, 0, len(items))
		for _, it := range items {
			out = append(out, summaryMap(it))
		}
		return vm.ToValue(out)
	}))
	_ = tasksObj.Set("get", s.hostFn(func(call goja.FunctionCall) goja.Value {
		t, err := s.resolveTask(optArgString(call, 0))
		if err != nil {
			s.throwErr(vm, err)
		}
		return vm.ToValue(s.fullTaskMap(t))
	}))
	_ = tasksObj.Set("getByExternal", s.hostFn(func(call goja.FunctionCall) goja.Value {
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
	}))
	_ = tasksObj.Set("next", s.hostFn(func(call goja.FunctionCall) goja.Value {
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
		fetchCtx := s.ctx
		if claim {
			if _, err := s.taskSvc.ClaimTask(s.ctx, task.ClaimTaskInput{
				WorkspaceID: s.wsID,
				TaskID:      bestID,
				TTLSeconds:  ttl,
			}); err != nil {
				s.throwErr(vm, err)
			}
			if _, err := s.taskSvc.TransitionTask(s.ctx, task.TransitionTaskInput{
				WorkspaceID:  s.wsID,
				TaskID:       bestID,
				TargetStatus: task.StatusInProgress,
			}); err != nil {
				s.throwErr(vm, err)
			}
			s.mutations++
			fetchCtx = authctx.WithSuppressTaskSignal(s.ctx)
		}
		full, err := s.taskSvc.GetTask(fetchCtx, s.wsID, bestID)
		if err != nil {
			s.throwErr(vm, err)
		}
		return vm.ToValue(s.fullTaskMap(full))
	}))
	_ = tasksObj.Set("create", s.hostFn(func(call goja.FunctionCall) goja.Value {
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
			pt, pErr := s.resolveTaskQuiet(parRef)
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
		if parsedKind, ok := parseJSTaskKind(strField(m, "kind")); ok {
			kind = parsedKind
		}
		assigneeID := s.resolveAssigneeInput(strField(m, "assignee", "assigneeActorId"))
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
			AssigneeActorID:    assigneeID,
			Tags:               strSliceField(m, "tags", "labels"),
		})
		if err != nil {
			s.throwErr(vm, err)
		}
		s.mutations++
		if mdRaw := rawMapString(m, "markdown", "markdownDoc", "doc"); strings.TrimSpace(mdRaw) != "" {
			if _, mdErr := s.taskSvc.PutTaskMarkdown(s.ctx, s.wsID, created.ID, mdRaw); mdErr != nil {
				s.throwErr(vm, mdErr)
			}
		}
		for _, depRef := range strSliceField(m, "dependsOn", "depends_on", "blockedBy", "blocked_by") {
			up, dErr := s.resolveTaskQuiet(depRef)
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
		reloaded, rErr := s.taskSvc.GetTask(authctx.WithSuppressTaskSignal(s.ctx), s.wsID, created.ID)
		if rErr != nil {
			s.throwErr(vm, rErr)
		}
		return vm.ToValue(s.fullTaskMap(reloaded))
	}))
	_ = tasksObj.Set("upsertExternal", s.hostFn(func(call goja.FunctionCall) goja.Value {
		m := argMap(vm, call, 0)
		extID := strField(m, "externalId", "external_id")
		if extID == "" {
			s.throwErr(vm, errs.Validation("flowsk.tasks.upsertExternal", "externalId is required"))
		}
		p, err := s.resolveProject(strField(m, "project"))
		if err != nil {
			s.throwErr(vm, err)
		}
		quietCtx := authctx.WithSuppressTaskSignal(s.ctx)
		existing, getErr := s.taskSvc.GetTaskByExternalID(quietCtx, s.wsID, p.ID, extID)
		if getErr != nil && !errors.Is(getErr, errs.ErrNotFound) {
			s.throwErr(vm, getErr)
		}
		if getErr == nil {
			upd := task.UpdateTaskInput{
				WorkspaceID: s.wsID,
				TaskID:      existing.ID,
			}
			if parsedKind, ok := parseJSTaskKind(strField(m, "kind")); ok {
				upd.Kind = &parsedKind
			}
			if v := strField(m, "title", "name"); v != "" {
				upd.Title = &v
			}
			if desc, hasDesc := hasRawMapString(m, "description", "body"); hasDesc {
				trimmed := strings.TrimSpace(desc)
				upd.Description = &trimmed
			}
			if rawP, hasP := m["priority"]; hasP {
				pr := parseJSTaskPriority(rawP)
				upd.Priority = &pr
			}
			if tags, hasTags := optStrSliceField(m, "tags", "labels"); hasTags {
				upd.Tags = tags
			}
			if acc, hasAcc := optStrSliceField(m, "acceptance", "acceptanceCriteria"); hasAcc {
				upd.AcceptanceCriteria = acc
			}
			if objRef, hasObj := hasRawMapString(m, "objective", "goal"); hasObj {
				if strings.TrimSpace(objRef) == "" {
					nilUUID := uuid.Nil
					upd.ObjectiveID = &nilUUID
				} else {
					o, oErr := s.resolveObjective(p.ID, objRef)
					if oErr != nil {
						s.throwErr(vm, oErr)
					}
					upd.ObjectiveID = &o.ID
				}
			}
			if parRef, hasPar := hasRawMapString(m, "parent"); hasPar {
				if strings.TrimSpace(parRef) == "" {
					nilUUID := uuid.Nil
					upd.ParentTaskID = &nilUUID
				} else {
					pt, pErr := s.resolveTaskQuiet(parRef)
					if pErr != nil {
						s.throwErr(vm, pErr)
					}
					upd.ParentTaskID = &pt.ID
				}
			}
			updated, uErr := s.taskSvc.UpdateTask(s.ctx, upd)
			if uErr != nil {
				s.throwErr(vm, uErr)
			}
			if deps, hasDeps := optStrSliceField(m, "dependsOn", "depends_on", "blockedBy", "blocked_by"); hasDeps {
				if syncErr := s.syncTaskBlockers(existing.ID, deps); syncErr != nil {
					s.throwErr(vm, syncErr)
				}
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
			if mdRaw, hasMD := hasRawMapString(m, "markdown", "markdownDoc", "doc"); hasMD {
				if strings.TrimSpace(mdRaw) == "" {
					updated, uErr = s.taskSvc.DeleteTaskMarkdown(s.ctx, s.wsID, existing.ID)
				} else {
					updated, uErr = s.taskSvc.PutTaskMarkdown(s.ctx, s.wsID, existing.ID, mdRaw)
				}
				if uErr != nil {
					s.throwErr(vm, uErr)
				}
			}
			reloaded, rErr := s.taskSvc.GetTask(quietCtx, s.wsID, existing.ID)
			if rErr != nil {
				s.throwErr(vm, rErr)
			}
			s.mutations++
			res := s.fullTaskMap(reloaded)
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
			pt, pErr := s.resolveTaskQuiet(parRef)
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
		if parsedKind, ok := parseJSTaskKind(strField(m, "kind")); ok {
			kind = parsedKind
		}
		created, cErr := s.taskSvc.CreateTask(s.ctx, task.CreateTaskInput{
			WorkspaceID:        s.wsID,
			ProjectID:          p.ID,
			ObjectiveID:        objID,
			ParentTaskID:       parentID,
			ExternalID:         extID,
			Kind:               kind,
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
		if mdRaw := rawMapString(m, "markdown", "markdownDoc", "doc"); strings.TrimSpace(mdRaw) != "" {
			if _, mdErr := s.taskSvc.PutTaskMarkdown(s.ctx, s.wsID, created.ID, mdRaw); mdErr != nil {
				s.throwErr(vm, mdErr)
			}
		}
		for _, depRef := range strSliceField(m, "dependsOn", "depends_on", "blockedBy", "blocked_by") {
			up, dErr := s.resolveTaskQuiet(depRef)
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
		reloaded, rErr := s.taskSvc.GetTask(quietCtx, s.wsID, created.ID)
		if rErr != nil {
			s.throwErr(vm, rErr)
		}
		res := s.fullTaskMap(reloaded)
		res["upserted"] = "created"
		return vm.ToValue(res)
	}))
	_ = tasksObj.Set("update", s.hostFn(func(call goja.FunctionCall) goja.Value {
		t, err := s.resolveTaskQuiet(optArgString(call, 0))
		if err != nil {
			s.throwErr(vm, err)
		}
		m := argMap(vm, call, 1)
		upd := task.UpdateTaskInput{
			WorkspaceID: s.wsID,
			TaskID:      t.ID,
		}
		if parsedKind, ok := parseJSTaskKind(strField(m, "kind")); ok {
			upd.Kind = &parsedKind
		}
		if v := strField(m, "title", "name"); v != "" {
			upd.Title = &v
		}
		if desc, hasDesc := hasRawMapString(m, "description", "body"); hasDesc {
			trimmed := strings.TrimSpace(desc)
			upd.Description = &trimmed
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
		if tags, hasTags := optStrSliceField(m, "tags", "labels"); hasTags {
			upd.Tags = tags
		}
		if acc, hasAcc := optStrSliceField(m, "acceptance", "acceptanceCriteria"); hasAcc {
			upd.AcceptanceCriteria = acc
		}
		if objRef, hasObj := hasRawMapString(m, "objective", "goal"); hasObj {
			if strings.TrimSpace(objRef) == "" {
				nilUUID := uuid.Nil
				upd.ObjectiveID = &nilUUID
			} else {
				o, oErr := s.resolveObjective(t.ProjectID, objRef)
				if oErr != nil {
					s.throwErr(vm, oErr)
				}
				upd.ObjectiveID = &o.ID
			}
		}
		if parRef, hasPar := hasRawMapString(m, "parent"); hasPar {
			if strings.TrimSpace(parRef) == "" {
				nilUUID := uuid.Nil
				upd.ParentTaskID = &nilUUID
			} else {
				pt, pErr := s.resolveTaskQuiet(parRef)
				if pErr != nil {
					s.throwErr(vm, pErr)
				}
				upd.ParentTaskID = &pt.ID
			}
		}
		if assRef, hasAss := hasRawMapString(m, "assignee", "assigneeActorId"); hasAss {
			if strings.TrimSpace(assRef) == "" {
				nilUUID := uuid.Nil
				upd.AssigneeActorID = &nilUUID
			} else if aid := s.resolveAssigneeInput(assRef); aid != nil {
				upd.AssigneeActorID = aid
			}
		}
		updated, err := s.taskSvc.UpdateTask(s.ctx, upd)
		if err != nil {
			s.throwErr(vm, err)
		}
		if mdRaw, hasMD := hasRawMapString(m, "markdown", "markdownDoc", "doc"); hasMD {
			if strings.TrimSpace(mdRaw) == "" {
				updated, err = s.taskSvc.DeleteTaskMarkdown(s.ctx, s.wsID, t.ID)
			} else {
				updated, err = s.taskSvc.PutTaskMarkdown(s.ctx, s.wsID, t.ID, mdRaw)
			}
			if err != nil {
				s.throwErr(vm, err)
			}
		}
		s.mutations++
		return vm.ToValue(s.fullTaskMap(updated))
	}))
	_ = tasksObj.Set("getDoc", s.hostFn(func(call goja.FunctionCall) goja.Value {
		t, err := s.resolveTaskQuiet(optArgString(call, 0))
		if err != nil {
			s.throwErr(vm, err)
		}
		md, hasDoc, err := s.taskSvc.GetTaskMarkdown(s.ctx, s.wsID, t.ID)
		if err != nil {
			s.throwErr(vm, err)
		}
		return vm.ToValue(map[string]any{
			"id":             t.ID.String(),
			"key":            t.Key,
			"markdown":       md,
			"hasMarkdownDoc": hasDoc,
		})
	}))
	_ = tasksObj.Set("setDoc", s.hostFn(func(call goja.FunctionCall) goja.Value {
		t, err := s.resolveTaskQuiet(optArgString(call, 0))
		if err != nil {
			s.throwErr(vm, err)
		}
		md := rawArgString(call, 1)
		var updated task.Task
		if strings.TrimSpace(md) == "" {
			updated, err = s.taskSvc.DeleteTaskMarkdown(s.ctx, s.wsID, t.ID)
		} else {
			updated, err = s.taskSvc.PutTaskMarkdown(s.ctx, s.wsID, t.ID, md)
		}
		if err != nil {
			s.throwErr(vm, err)
		}
		s.mutations++
		return vm.ToValue(s.fullTaskMap(updated))
	}))
	_ = tasksObj.Set("deleteDoc", s.hostFn(func(call goja.FunctionCall) goja.Value {
		t, err := s.resolveTaskQuiet(optArgString(call, 0))
		if err != nil {
			s.throwErr(vm, err)
		}
		updated, err := s.taskSvc.DeleteTaskMarkdown(s.ctx, s.wsID, t.ID)
		if err != nil {
			s.throwErr(vm, err)
		}
		s.mutations++
		return vm.ToValue(s.fullTaskMap(updated))
	}))
	_ = tasksObj.Set("move", s.hostFn(func(call goja.FunctionCall) goja.Value {
		t, err := s.resolveTaskQuiet(optArgString(call, 0))
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
	}))
	_ = tasksObj.Set("done", s.hostFn(func(call goja.FunctionCall) goja.Value {
		t, err := s.resolveTaskQuiet(optArgString(call, 0))
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
	}))
	_ = tasksObj.Set("claim", s.hostFn(func(call goja.FunctionCall) goja.Value {
		t, err := s.resolveTaskQuiet(optArgString(call, 0))
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
			if _, err := s.taskSvc.TransitionTask(s.ctx, task.TransitionTaskInput{
				WorkspaceID:  s.wsID,
				TaskID:       t.ID,
				TargetStatus: task.StatusInProgress,
			}); err != nil {
				s.throwErr(vm, err)
			}
		}
		s.mutations++
		reloaded, err := s.taskSvc.GetTask(authctx.WithSuppressTaskSignal(s.ctx), s.wsID, t.ID)
		if err != nil {
			s.throwErr(vm, err)
		}
		return vm.ToValue(s.fullTaskMap(reloaded))
	}))
	_ = tasksObj.Set("release", s.hostFn(func(call goja.FunctionCall) goja.Value {
		t, err := s.resolveTaskQuiet(optArgString(call, 0))
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
		reloaded, err := s.taskSvc.GetTask(authctx.WithSuppressTaskSignal(s.ctx), s.wsID, t.ID)
		if err != nil {
			s.throwErr(vm, err)
		}
		return vm.ToValue(s.fullTaskMap(reloaded))
	}))
	_ = tasksObj.Set("block", s.hostFn(func(call goja.FunctionCall) goja.Value {
		t, err := s.resolveTaskQuiet(optArgString(call, 0))
		if err != nil {
			s.throwErr(vm, err)
		}
		var (
			reason string
			byRefs []string
		)
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
			switch v := exportJSValue(call.Arguments[1]).(type) {
			case map[string]any:
				reason = strField(v, "reason", "note")
				byRefs = strSliceField(v, "by", "blockedBy", "dependsOn")
			case string:
				reason = strings.TrimSpace(v)
				if len(call.Arguments) > 2 && !goja.IsUndefined(call.Arguments[2]) && !goja.IsNull(call.Arguments[2]) {
					byRefs = valueToStringSlice(exportJSValue(call.Arguments[2]))
				}
			default:
				panic(vm.NewTypeError("expected blocker reason string or options object"))
			}
		}
		for _, upRef := range byRefs {
			up, uErr := s.resolveTaskQuiet(upRef)
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
		if len(byRefs) == 0 {
			if _, err := s.taskSvc.TransitionTask(s.ctx, task.TransitionTaskInput{
				WorkspaceID:       s.wsID,
				TaskID:            t.ID,
				TargetStatus:      task.StatusBlocked,
				ResolutionComment: reason,
			}); err != nil {
				s.throwErr(vm, err)
			}
		} else {
			if t.Status != task.StatusBlocked && !t.Status.IsTerminal() {
				if _, err := s.taskSvc.TransitionTask(s.ctx, task.TransitionTaskInput{
					WorkspaceID:  s.wsID,
					TaskID:       t.ID,
					TargetStatus: task.StatusBlocked,
				}); err != nil {
					s.throwErr(vm, err)
				}
			}
			if reason != "" {
				if _, err := s.taskSvc.AddComment(authctx.WithSuppressTaskSignal(s.ctx), task.AddCommentInput{
					WorkspaceID: s.wsID,
					TaskID:      t.ID,
					Kind:        task.CommentKindBlocker,
					Body:        reason,
				}); err != nil {
					s.throwErr(vm, err)
				}
			}
		}
		s.mutations++
		reloaded, err := s.taskSvc.GetTask(authctx.WithSuppressTaskSignal(s.ctx), s.wsID, t.ID)
		if err != nil {
			s.throwErr(vm, err)
		}
		return vm.ToValue(s.fullTaskMap(reloaded))
	}))
	_ = tasksObj.Set("unblock", s.hostFn(func(call goja.FunctionCall) goja.Value {
		t, err := s.resolveTaskQuiet(optArgString(call, 0))
		if err != nil {
			s.throwErr(vm, err)
		}
		var (
			filterSourceIDs map[uuid.UUID]bool
			hasClearedArg   bool
		)
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
			hasClearedArg = true
			var clearedRefs []string
			if m, ok := exportJSValue(call.Arguments[1]).(map[string]any); ok {
				clearedRefs = strSliceField(m, "clearedBy", "by")
			} else {
				clearedRefs = valueToStringSlice(exportJSValue(call.Arguments[1]))
			}
			filterSourceIDs = make(map[uuid.UUID]bool, len(clearedRefs))
			for _, ref := range clearedRefs {
				up, uErr := s.resolveTaskQuiet(ref)
				if uErr != nil {
					s.throwErr(vm, uErr)
				}
				filterSourceIDs[up.ID] = true
			}
		}
		rels, err := s.taskSvc.ListRelations(s.ctx, s.wsID, t.ID)
		if err != nil {
			s.throwErr(vm, err)
		}
		for _, r := range rels {
			if r.TargetTaskID == t.ID && r.Type == task.RelationBlocks {
				if hasClearedArg && !filterSourceIDs[r.SourceTaskID] {
					continue
				}
				if rmErr := s.taskSvc.RemoveRelation(s.ctx, s.wsID, r.ID); rmErr != nil {
					s.throwErr(vm, rmErr)
				}
			}
		}
		reloaded, err := s.taskSvc.GetTask(authctx.WithSuppressTaskSignal(s.ctx), s.wsID, t.ID)
		if err != nil {
			s.throwErr(vm, err)
		}
		if reloaded.Status == task.StatusBlocked && reloaded.UnresolvedBlockerCount == 0 {
			reloaded, err = s.taskSvc.TransitionTask(s.ctx, task.TransitionTaskInput{
				WorkspaceID:  s.wsID,
				TaskID:       t.ID,
				TargetStatus: task.StatusReady,
			})
			if err != nil {
				s.throwErr(vm, err)
			}
		}
		s.mutations++
		return vm.ToValue(s.fullTaskMap(reloaded))
	}))
	_ = tasksObj.Set("comment", s.hostFn(func(call goja.FunctionCall) goja.Value {
		t, err := s.resolveTaskQuiet(optArgString(call, 0))
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
	}))

	root := vm.NewObject()
	_ = root.Set("projects", projectsObj)
	_ = root.Set("goals", goalsObj)
	_ = root.Set("objectives", goalsObj)
	_ = root.Set("tasks", tasksObj)
	_ = root.Set("search", s.hostFn(func(call goja.FunctionCall) goja.Value {
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
			objs, oErr := s.objSvc.ListObjectives(s.ctx, objective.ListObjectivesFilter{
				WorkspaceID: s.wsID,
				ProjectID:   p.ID,
				Limit:       500,
			})
			if oErr != nil {
				s.throwErr(vm, oErr)
			}
			allTasks, tErr := s.listAllProjectTasks(p.ID)
			if tErr != nil {
				s.throwErr(vm, tErr)
			}
			var matchedGoals []map[string]any
			for _, o := range objs {
				if q.MatchesText(o.Slug, o.Title, o.Summary, string(o.Status)) {
					matchedGoals = append(matchedGoals, objectiveMap(o, allTasks))
				}
			}
			res["goals"] = matchedGoals
		}
		return vm.ToValue(res)
	}))
	_ = root.Set("board", s.hostFn(func(call goja.FunctionCall) goja.Value {
		var (
			projRef          string
			objRef           string
			includeCompleted bool
		)
		if len(call.Arguments) > 0 && !goja.IsUndefined(call.Arguments[0]) && !goja.IsNull(call.Arguments[0]) {
			if m, ok := exportJSValue(call.Arguments[0]).(map[string]any); ok {
				projRef = strField(m, "project")
				objRef = strField(m, "objective", "goal")
				if inc, okInc := m["includeCompleted"].(bool); okInc {
					includeCompleted = inc
				}
			}
		}
		p, err := s.resolveProject(projRef)
		if err != nil {
			s.throwErr(vm, err)
		}
		var targetObj *objective.Objective
		if objRef != "" {
			resolvedObj, oErr := s.resolveObjective(p.ID, objRef)
			if oErr != nil {
				s.throwErr(vm, oErr)
			}
			targetObj = &resolvedObj
		}
		objs, err := s.objSvc.ListObjectives(s.ctx, objective.ListObjectivesFilter{WorkspaceID: s.wsID, ProjectID: p.ID, Limit: 500})
		if err != nil {
			s.throwErr(vm, err)
		}
		allTasks, err := s.listAllProjectTasks(p.ID)
		if err != nil {
			s.throwErr(vm, err)
		}
		goals := make([]map[string]any, 0, len(objs))
		for _, o := range objs {
			if targetObj != nil && o.ID != targetObj.ID {
				continue
			}
			if targetObj == nil && !includeCompleted && (o.Status == objective.StatusAchieved || o.Status == objective.StatusDropped) {
				continue
			}
			goals = append(goals, objectiveMap(o, allTasks))
		}
		rows := make([]map[string]any, 0, len(allTasks))
		for _, t := range allTasks {
			if targetObj != nil && !strings.EqualFold(t.ObjectiveSlug, targetObj.Slug) {
				continue
			}
			if !includeCompleted && t.Status.IsTerminal() {
				continue
			}
			rows = append(rows, summaryMap(t))
		}
		return vm.ToValue(map[string]any{
			"project": projectMap(p),
			"goals":   goals,
			"tasks":   rows,
		})
	}))

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
	items, err := s.objSvc.ListObjectives(s.ctx, objective.ListObjectivesFilter{WorkspaceID: s.wsID, ProjectID: projID, Limit: 500})
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
	return s.resolveTaskWithContext(s.ctx, ref)
}

func (s *scriptSession) resolveTaskQuiet(ref string) (task.Task, error) {
	return s.resolveTaskWithContext(authctx.WithSuppressTaskSignal(s.ctx), ref)
}

func (s *scriptSession) resolveTaskWithContext(ctx context.Context, ref string) (task.Task, error) {
	trimmed := strings.TrimSpace(ref)
	if tid, err := uuid.Parse(trimmed); err == nil {
		return s.taskSvc.GetTask(ctx, s.wsID, tid)
	}
	if t, err := s.taskSvc.GetTaskByKey(ctx, s.wsID, strings.ToUpper(trimmed)); err == nil {
		return t, nil
	}
	if p, pErr := s.resolveProject(""); pErr == nil && p.ID != uuid.Nil {
		if t, extErr := s.taskSvc.GetTaskByExternalID(ctx, s.wsID, p.ID, trimmed); extErr == nil {
			return t, nil
		}
	}
	return s.taskSvc.GetTaskByKey(ctx, s.wsID, strings.ToUpper(trimmed))
}

func (s *scriptSession) resolveAssigneeInput(raw string) *uuid.UUID {
	trimmed := strings.TrimPrefix(strings.TrimSpace(raw), "@")
	if trimmed == "" {
		return nil
	}
	if aid, err := uuid.Parse(trimmed); err == nil && aid != uuid.Nil {
		return &aid
	}
	if strings.EqualFold(trimmed, "me") || (s.ident.Handle != "" && strings.EqualFold(trimmed, s.ident.Handle)) {
		if s.ident.ActorID != uuid.Nil {
			idCopy := s.ident.ActorID
			return &idCopy
		}
	}
	return nil
}

func (s *scriptSession) syncTaskBlockers(taskID uuid.UUID, depRefs []string) error {
	wantSourceIDs := make([]uuid.UUID, 0, len(depRefs))
	wantSet := make(map[uuid.UUID]bool, len(depRefs))
	for _, depRef := range depRefs {
		up, err := s.resolveTaskQuiet(depRef)
		if err != nil {
			return err
		}
		if !wantSet[up.ID] {
			wantSet[up.ID] = true
			wantSourceIDs = append(wantSourceIDs, up.ID)
		}
	}
	rels, err := s.taskSvc.ListRelations(s.ctx, s.wsID, taskID)
	if err != nil {
		return err
	}
	existingSources := make(map[uuid.UUID]uuid.UUID)
	for _, r := range rels {
		if r.TargetTaskID == taskID && r.Type == task.RelationBlocks {
			existingSources[r.SourceTaskID] = r.ID
			if !wantSet[r.SourceTaskID] {
				if rmErr := s.taskSvc.RemoveRelation(s.ctx, s.wsID, r.ID); rmErr != nil {
					return rmErr
				}
			}
		}
	}
	for _, upID := range wantSourceIDs {
		if _, exists := existingSources[upID]; !exists {
			if _, addErr := s.taskSvc.AddRelation(s.ctx, task.AddRelationInput{
				WorkspaceID:  s.wsID,
				SourceTaskID: upID,
				TargetTaskID: taskID,
				Type:         task.RelationBlocks,
			}); addErr != nil {
				return addErr
			}
		}
	}
	return nil
}

func defaultNonArchivedStatuses() []task.Status {
	return []task.Status{
		task.StatusBacklog, task.StatusReady, task.StatusInProgress,
		task.StatusInReview, task.StatusBlocked, task.StatusDone, task.StatusCancelled,
	}
}

func (s *scriptSession) listAllProjectTasks(projID uuid.UUID) ([]task.CompactTaskSummary, error) {
	return s.taskSvc.PollWorkQueue(s.ctx, task.AgentWorkQueueFilter{
		WorkspaceID: s.wsID,
		ProjectID:   projID,
		Statuses:    defaultNonArchivedStatuses(),
		Limit:       1000,
	})
}

func (s *scriptSession) lookupFullTask(id uuid.UUID, prefetched map[uuid.UUID]task.Task) (task.Task, bool) {
	if prefetched != nil {
		if t, ok := prefetched[id]; ok {
			return t, true
		}
	}
	full, err := s.taskSvc.GetTask(authctx.WithSuppressTaskSignal(s.ctx), s.wsID, id)
	if err != nil {
		return task.Task{}, false
	}
	return full, true
}

func (s *scriptSession) matchesTaskActor(it task.CompactTaskSummary, actorQuery string, prefetched map[uuid.UUID]task.Task) bool {
	want := strings.TrimPrefix(strings.TrimSpace(actorQuery), "@")
	if want == "" {
		return true
	}
	if strings.EqualFold(it.HolderHandle, want) {
		return true
	}
	full, ok := s.lookupFullTask(it.ID, prefetched)
	if !ok {
		return false
	}
	if full.ActiveLease != nil {
		if strings.EqualFold(full.ActiveLease.HolderHandle, want) ||
			strings.EqualFold(full.ActiveLease.HolderActorID.String(), want) ||
			(strings.EqualFold(want, "me") && full.ActiveLease.HolderActorID == s.ident.ActorID) {
			return true
		}
	}
	if full.AssigneeActorID != nil && *full.AssigneeActorID != uuid.Nil {
		assigneeID := *full.AssigneeActorID
		if strings.EqualFold(assigneeID.String(), want) {
			return true
		}
		if (strings.EqualFold(want, "me") || (s.ident.Handle != "" && strings.EqualFold(s.ident.Handle, want))) && assigneeID == s.ident.ActorID {
			return true
		}
		if cms, cErr := s.taskSvc.ListComments(s.ctx, s.wsID, it.ID, 50); cErr == nil {
			for _, cm := range cms {
				if cm.AuthorActorID == assigneeID && strings.EqualFold(cm.AuthorHandle, want) {
					return true
				}
			}
		}
	}
	return false
}

func (s *scriptSession) searchProjectTasks(projID uuid.UUID, q language.SearchQuery) ([]map[string]any, error) {
	statuses := defaultNonArchivedStatuses()
	if len(q.Statuses) > 0 {
		statuses = append([]task.Status(nil), q.Statuses...)
	}
	filter := task.AgentWorkQueueFilter{
		WorkspaceID: s.wsID,
		ProjectID:   projID,
		Statuses:    statuses,
		ExternalID:  q.ExternalID,
		Limit:       1000,
	}
	if len(q.Tags) > 0 {
		filter.RequiredTags = append([]string(nil), q.Tags...)
	}
	if q.Claimed != nil && !*q.Claimed {
		filter.OnlyUnclaimed = true
	}
	if q.Objective != "" {
		o, oErr := s.resolveObjective(projID, q.Objective)
		if oErr != nil {
			if errors.Is(oErr, errs.ErrNotFound) {
				return []map[string]any{}, nil
			}
			return nil, oErr
		}
		filter.ObjectiveID = &o.ID
	}

	all, err := s.taskSvc.PollWorkQueue(s.ctx, filter)
	if err != nil {
		return nil, err
	}
	var prefetched map[uuid.UUID]task.Task
	if len(all) > 1 && (q.Actor != "" || len(q.Terms) > 0) && s.taskRepo != nil {
		if wsTasks, lErr := s.taskRepo.ListByWorkspace(s.ctx, s.wsID); lErr == nil {
			prefetched = make(map[uuid.UUID]task.Task, len(wsTasks))
			for _, wt := range wsTasks {
				prefetched[wt.ID] = wt
			}
		}
	}
	out := make([]map[string]any, 0, len(all))
	for _, it := range all {
		if len(q.Statuses) > 0 && !slices.Contains(q.Statuses, it.Status) {
			continue
		}
		if q.Priority != nil && it.Priority != *q.Priority {
			continue
		}
		if q.Parent != "" && !strings.EqualFold(it.ParentKey, q.Parent) {
			continue
		}
		if q.ExternalID != "" && it.ExternalID != q.ExternalID {
			continue
		}
		if q.Actor != "" && !s.matchesTaskActor(it, q.Actor, prefetched) {
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
		if q.HasMarkdownDoc != nil && it.HasMarkdownDoc != *q.HasMarkdownDoc {
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
			full, hasFull := s.lookupFullTask(it.ID, prefetched)
			var commentText string
			if cms, cErr := s.taskSvc.ListComments(s.ctx, s.wsID, it.ID, 20); cErr == nil {
				var cb strings.Builder
				for _, cm := range cms {
					cb.WriteString(cm.Body)
					cb.WriteByte('\n')
				}
				commentText = cb.String()
			}
			if hasFull {
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
	parentKey := ""
	if t.ParentTaskID != nil {
		if pt, err := s.taskSvc.GetTask(authctx.WithSuppressTaskSignal(s.ctx), s.wsID, *t.ParentTaskID); err == nil {
			parentKey = pt.Key
		}
	}
	holder := ""
	if t.ActiveLease != nil {
		holder = firstNonEmpty(t.ActiveLease.HolderHandle, t.ActiveLease.HolderActorID.String())
	}
	assignee := ""
	if t.AssigneeActorID != nil && *t.AssigneeActorID != uuid.Nil {
		assignee = t.AssigneeActorID.String()
	}
	tags := t.TagsCopy()
	if tags == nil {
		tags = []string{}
	}
	acc := t.AcceptanceCriteriaCopy()
	if acc == nil {
		acc = []string{}
	}
	var lastSignalAt any
	if t.LastSignalAt != nil && !t.LastSignalAt.IsZero() {
		lastSignalAt = t.LastSignalAt.UTC().Format(time.RFC3339Nano)
	}
	return map[string]any{
		"id":             t.ID.String(),
		"key":            t.Key,
		"kind":           string(t.Kind),
		"externalId":     t.ExternalID,
		"title":          t.Title,
		"description":    t.Description,
		"status":         string(t.Status),
		"state":          string(t.Status),
		"priority":       int(t.Priority),
		"tags":           tags,
		"objective":      objKey,
		"parent":         parentKey,
		"assignee":       assignee,
		"holder":         holder,
		"blocked":        t.UnresolvedBlockerCount > 0 || t.Status == task.StatusBlocked,
		"hasMarkdownDoc": t.HasMarkdownDoc,
		"lastSignalAt":   lastSignalAt,
		"acceptance":     acc,
	}
}

func projectMap(p project.Project) map[string]any {
	return map[string]any{
		"id":             p.ID.String(),
		"slug":           p.Slug,
		"key":            p.KeyPrefix,
		"name":           p.Name,
		"description":    p.Description,
		"status":         string(p.Status),
		"hasMarkdownDoc": p.HasMarkdownDoc,
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
	kind := string(o.Kind)
	if kind == "" {
		kind = string(objective.KindGoal)
	}
	criteria := o.SuccessCriteriaCopy()
	if criteria == nil {
		criteria = []string{}
	}
	var targetDate any
	if o.TargetDate != nil && !o.TargetDate.IsZero() {
		targetDate = o.TargetDate.UTC().Format("2006-01-02")
	}
	return map[string]any{
		"id":              o.ID.String(),
		"key":             o.Slug,
		"slug":            o.Slug,
		"kind":            kind,
		"title":           o.Title,
		"summary":         o.Summary,
		"status":          string(o.Status),
		"priority":        int(o.PriorityRank),
		"targetDate":      targetDate,
		"successCriteria": criteria,
		"totalTasks":      total,
		"completedTasks":  done,
	}
}

func summaryMap(s task.CompactTaskSummary) map[string]any {
	tags := make([]string, len(s.Tags))
	copy(tags, s.Tags)
	var lastSignalAt any
	if s.LastSignalAt != nil && !s.LastSignalAt.IsZero() {
		lastSignalAt = s.LastSignalAt.UTC().Format(time.RFC3339Nano)
	}
	return map[string]any{
		"id":             s.ID.String(),
		"key":            s.Key,
		"kind":           string(s.Kind),
		"externalId":     s.ExternalID,
		"title":          s.Title,
		"status":         string(s.Status),
		"state":          string(s.Status),
		"priority":       int(s.Priority),
		"objective":      s.ObjectiveSlug,
		"parent":         s.ParentKey,
		"holder":         s.HolderHandle,
		"tags":           tags,
		"blocked":        s.UnresolvedBlockerCount > 0 || s.Status == task.StatusBlocked,
		"hasMarkdownDoc": s.HasMarkdownDoc,
		"lastSignalAt":   lastSignalAt,
	}
}

func mapToSearchQuery(m map[string]any) language.SearchQuery {
	q := language.ParseSearchQuery(strField(m, "q", "query"))
	if stRaw := strField(m, "state", "status"); stRaw != "" {
		for _, part := range strings.Split(stRaw, ",") {
			if st, ok := language.NormalizeStatusToken(part); ok && !slices.Contains(q.Statuses, st) {
				q.Statuses = append(q.Statuses, st)
			}
		}
	}
	if tag := strField(m, "tag", "label"); tag != "" {
		q.Tags = append(q.Tags, strings.ToLower(tag))
	}
	if tags, hasTags := optStrSliceField(m, "tags", "labels"); hasTags {
		for _, t := range tags {
			q.Tags = append(q.Tags, strings.ToLower(t))
		}
	}
	if obj := strField(m, "objective", "goal"); obj != "" {
		q.Objective = obj
	}
	if par := strField(m, "parent"); par != "" {
		q.Parent = par
	}
	if ext := strField(m, "externalId", "external_id", "ext"); ext != "" {
		q.ExternalID = ext
	}
	if act := strField(m, "actor", "assignee", "holder"); act != "" {
		q.Actor = strings.TrimPrefix(act, "@")
	}
	if b, ok := m["blocked"].(bool); ok {
		q.Blocked = &b
	}
	if c, ok := m["claimed"].(bool); ok {
		q.Claimed = &c
	}
	if md, ok := m["hasMarkdownDoc"].(bool); ok {
		q.HasMarkdownDoc = &md
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

func rawArgString(call goja.FunctionCall, idx int) string {
	if idx >= len(call.Arguments) || goja.IsUndefined(call.Arguments[idx]) || goja.IsNull(call.Arguments[idx]) {
		return ""
	}
	return call.Arguments[idx].String()
}

func rawMapString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if raw, ok := m[k]; ok && raw != nil {
			if s, okS := raw.(string); okS {
				return s
			}
		}
	}
	return ""
}

func hasRawMapString(m map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if raw, ok := m[k]; ok {
			if raw == nil {
				return "", true
			}
			if s, okS := raw.(string); okS {
				return s, true
			}
		}
	}
	return "", false
}

func parseJSObjectiveKind(raw string) objective.Kind {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "milestone":
		return objective.KindMilestone
	case "cycle", "sprint":
		return objective.KindCycle
	default:
		return objective.KindGoal
	}
}

func parseJSObjectiveStatus(raw string, fallback objective.Status) objective.Status {
	switch strings.ToLower(strings.ReplaceAll(strings.TrimSpace(raw), "-", "_")) {
	case "draft", "planned":
		return objective.StatusDraft
	case "active", "in_progress":
		return objective.StatusActive
	case "at_risk", "risk":
		return objective.StatusAtRisk
	case "achieved", "done", "completed":
		return objective.StatusAchieved
	case "dropped", "cancelled", "canceled":
		return objective.StatusDropped
	default:
		return fallback
	}
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
	vals, _ := optStrSliceField(m, keys...)
	return vals
}

func optStrSliceField(m map[string]any, keys ...string) ([]string, bool) {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		if raw == nil {
			return []string{}, true
		}
		return valueToStringSlice(raw), true
	}
	return nil, false
}

func valueToStringSlice(raw any) []string {
	switch v := raw.(type) {
	case string:
		if strings.TrimSpace(v) != "" {
			return []string{strings.TrimSpace(v)}
		}
		return []string{}
	case []string:
		out := make([]string, 0, len(v))
		for _, el := range v {
			if s := strings.TrimSpace(el); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(v))
		for _, el := range v {
			if el == nil {
				continue
			}
			if s := strings.TrimSpace(fmt.Sprint(el)); s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return []string{}
	}
}

func parseJSTaskKind(raw string) (task.Kind, bool) {
	k := task.Kind(strings.ToLower(strings.TrimSpace(raw)))
	switch k {
	case task.KindEpic, task.KindFeature, task.KindBug, task.KindChore, task.KindResearch:
		return k, true
	default:
		return "", false
	}
}

func parseOptionalTargetDate(op string, m map[string]any, keys ...string) (*time.Time, bool, error) {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		if raw == nil {
			zero := time.Time{}
			return &zero, true, nil
		}
		switch v := raw.(type) {
		case time.Time:
			utc := v.UTC()
			return &utc, true, nil
		case string:
			trimmed := strings.TrimSpace(v)
			if trimmed == "" {
				zero := time.Time{}
				return &zero, true, nil
			}
			if parsed, err := time.Parse("2006-01-02", trimmed); err == nil {
				utc := parsed.UTC()
				return &utc, true, nil
			}
			if parsed, err := time.Parse(time.RFC3339Nano, trimmed); err == nil {
				utc := parsed.UTC()
				return &utc, true, nil
			}
			return nil, true, errs.Validation(op, fmt.Sprintf("invalid targetDate %q: expected YYYY-MM-DD or RFC3339 timestamp", trimmed), errs.FieldViolation{
				Field:       "targetDate",
				Description: "must be a valid YYYY-MM-DD date or RFC3339 timestamp",
			})
		default:
			return nil, true, errs.Validation(op, "invalid targetDate type: expected string or null", errs.FieldViolation{
				Field:       "targetDate",
				Description: "must be a YYYY-MM-DD or RFC3339 string, or null/empty to clear",
			})
		}
	}
	return nil, false, nil
}

func deriveProjectKeyPrefix(raw string) string {
	cleaned := nonUpperAlnum.ReplaceAllString(strings.ToUpper(strings.TrimSpace(raw)), "")
	if cleaned != "" && cleaned[0] >= '0' && cleaned[0] <= '9' {
		cleaned = "P" + cleaned
	}
	if len(cleaned) < 2 {
		return "PRJ"
	}
	if len(cleaned) > 10 {
		cleaned = cleaned[:10]
	}
	return cleaned
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
