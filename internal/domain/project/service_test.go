// File service_test.go verifies project.Service multi-tenant ownership, slug and
// key-prefix validation, strict rejection of host filesystem paths, defensive tag
// copies, and atomic audit fail-fast rollback semantics.
package project_test

import (
	"context"
	"errors"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/project"
	"github.com/mindmain/flowsk/internal/domain/tx"
)

type memTxHandle struct{}

func (memTxHandle) TxDialect() string { return "memory" }

type memAuditRecorder struct {
	entries *[]tx.AuditEntry
	failErr error
}

func (r *memAuditRecorder) Record(_ context.Context, entry tx.AuditEntry) error {
	if r.failErr != nil {
		return r.failErr
	}
	*r.entries = append(*r.entries, entry.Clone())
	return nil
}

type memScope struct {
	recorder *memAuditRecorder
	projects map[uuid.UUID]project.Project
	bindings []project.ProjectBinding
}

func (s *memScope) Audit() tx.AuditRecorder      { return s.recorder }
func (s *memScope) IsAuditEnabled() bool         { return true }
func (s *memScope) TxHandle() tx.DialectTxHandle { return memTxHandle{} }

type memProjectEnv struct {
	mu       sync.Mutex
	projects map[uuid.UUID]project.Project
	bindings []project.ProjectBinding
	audits   []tx.AuditEntry
	auditFn  func() error
}

func newMemProjectEnv() *memProjectEnv {
	return &memProjectEnv{
		projects: make(map[uuid.UUID]project.Project),
	}
}

func (e *memProjectEnv) WithTransaction(ctx context.Context, fn tx.TxFunc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	staged := make(map[uuid.UUID]project.Project, len(e.projects))
	for id, p := range e.projects {
		staged[id] = p.Clone()
	}
	stagedBindings := append([]project.ProjectBinding(nil), e.bindings...)
	var stagedAudits []tx.AuditEntry
	var failErr error
	if e.auditFn != nil {
		failErr = e.auditFn()
	}
	scope := &memScope{
		recorder: &memAuditRecorder{entries: &stagedAudits, failErr: failErr},
		projects: staged,
		bindings: stagedBindings,
	}
	if err := fn(ctx, scope); err != nil {
		return err
	}
	e.projects = staged
	e.bindings = scope.bindings
	e.audits = append(e.audits, stagedAudits...)
	return nil
}

func (e *memProjectEnv) WithNoAuditTransaction(ctx context.Context, fn tx.TxFunc) error {
	return e.WithTransaction(ctx, fn)
}

type memProjectRepo struct {
	env   *memProjectEnv
	scope *memScope
}

func newMemProjectRepo(env *memProjectEnv) *memProjectRepo {
	return &memProjectRepo{env: env}
}

func (r *memProjectRepo) WithScope(scope tx.Scope) project.Repository {
	ms, _ := scope.(*memScope)
	return &memProjectRepo{env: r.env, scope: ms}
}

func (r *memProjectRepo) readMap(fn func(m map[uuid.UUID]project.Project) error) error {
	if r.scope != nil {
		return fn(r.scope.projects)
	}
	r.env.mu.Lock()
	defer r.env.mu.Unlock()
	return fn(r.env.projects)
}

func (r *memProjectRepo) writeMap(op string, fn func(m map[uuid.UUID]project.Project) error) error {
	if r.scope == nil {
		return errs.New(errs.ErrPreconditionFailed, op, "mutation outside transaction scope")
	}
	return fn(r.scope.projects)
}

func (r *memProjectRepo) Create(_ context.Context, p project.Project) error {
	return r.writeMap("project.Repo.Create", func(m map[uuid.UUID]project.Project) error {
		for _, existing := range m {
			if existing.DeletedAt != nil || existing.WorkspaceID != p.WorkspaceID {
				continue
			}
			if existing.Slug == p.Slug || existing.KeyPrefix == p.KeyPrefix {
				return errs.New(errs.ErrConflict, "project.Repo.Create", "duplicate slug or key_prefix")
			}
		}
		m[p.ID] = p.Clone()
		return nil
	})
}

func (r *memProjectRepo) GetByID(_ context.Context, workspaceID, projectID uuid.UUID) (project.Project, error) {
	var out project.Project
	err := r.readMap(func(m map[uuid.UUID]project.Project) error {
		p, ok := m[projectID]
		if !ok || p.DeletedAt != nil || p.WorkspaceID != workspaceID {
			return errs.New(errs.ErrNotFound, "project.Repo.GetByID", "project not found")
		}
		out = p.Clone()
		return nil
	})
	return out, err
}

func (r *memProjectRepo) GetBySlug(_ context.Context, workspaceID uuid.UUID, slug string) (project.Project, error) {
	var out project.Project
	err := r.readMap(func(m map[uuid.UUID]project.Project) error {
		for _, p := range m {
			if p.DeletedAt == nil && p.WorkspaceID == workspaceID && p.Slug == slug {
				out = p.Clone()
				return nil
			}
		}
		return errs.New(errs.ErrNotFound, "project.Repo.GetBySlug", "project not found")
	})
	return out, err
}

func (r *memProjectRepo) List(_ context.Context, filter project.ListProjectsFilter) ([]project.Project, error) {
	var out []project.Project
	err := r.readMap(func(m map[uuid.UUID]project.Project) error {
		for _, p := range m {
			if p.DeletedAt != nil || p.WorkspaceID != filter.WorkspaceID {
				continue
			}
			if filter.Status != nil && p.Status != *filter.Status {
				continue
			}
			if filter.Tag != "" && !slices.Contains(p.Tags, filter.Tag) {
				continue
			}
			out = append(out, p.Clone())
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
		if int(filter.Offset) >= len(out) {
			out = nil
			return nil
		}
		out = out[filter.Offset:]
		if filter.Limit > 0 && int(filter.Limit) < len(out) {
			out = out[:filter.Limit]
		}
		return nil
	})
	return out, err
}

func (r *memProjectRepo) Update(_ context.Context, p project.Project) error {
	return r.writeMap("project.Repo.Update", func(m map[uuid.UUID]project.Project) error {
		cur, ok := m[p.ID]
		if !ok || cur.DeletedAt != nil || cur.WorkspaceID != p.WorkspaceID {
			return errs.New(errs.ErrNotFound, "project.Repo.Update", "project not found")
		}
		m[p.ID] = p.Clone()
		return nil
	})
}

func (r *memProjectRepo) NextTaskSequence(_ context.Context, workspaceID, projectID uuid.UUID) (string, int64, error) {
	var prefix string
	var seq int64
	err := r.writeMap("project.Repo.NextTaskSequence", func(m map[uuid.UUID]project.Project) error {
		cur, ok := m[projectID]
		if !ok || cur.DeletedAt != nil || cur.WorkspaceID != workspaceID {
			return errs.New(errs.ErrNotFound, "project.Repo.NextTaskSequence", "project not found")
		}
		cur.TaskSeq++
		m[projectID] = cur
		prefix = cur.KeyPrefix
		seq = cur.TaskSeq
		return nil
	})
	return prefix, seq, err
}

func (r *memProjectRepo) SetHasMarkdownDoc(_ context.Context, workspaceID, projectID uuid.UUID, hasDoc bool) (project.Project, error) {
	var updated project.Project
	err := r.writeMap("project.Repo.SetHasMarkdownDoc", func(m map[uuid.UUID]project.Project) error {
		cur, ok := m[projectID]
		if !ok || cur.DeletedAt != nil || cur.WorkspaceID != workspaceID {
			return errs.New(errs.ErrNotFound, "project.Repo.SetHasMarkdownDoc", "project not found")
		}
		cur.HasMarkdownDoc = hasDoc
		cur.UpdatedAt = time.Now().UTC()
		m[projectID] = cur
		updated = cur.Clone()
		return nil
	})
	return updated, err
}

func (r *memProjectRepo) SoftDelete(_ context.Context, workspaceID, projectID uuid.UUID) error {
	return r.writeMap("project.Repo.SoftDelete", func(m map[uuid.UUID]project.Project) error {
		cur, ok := m[projectID]
		if !ok || cur.DeletedAt != nil || cur.WorkspaceID != workspaceID {
			return errs.New(errs.ErrNotFound, "project.Repo.SoftDelete", "project not found")
		}
		now := time.Now().UTC()
		cur.DeletedAt = &now
		m[projectID] = cur
		return nil
	})
}

func (r *memProjectRepo) UpsertBinding(_ context.Context, b project.ProjectBinding) (project.ProjectBinding, error) {
	if r.scope == nil {
		return project.ProjectBinding{}, errs.New(errs.ErrPreconditionFailed, "UpsertBinding", "unscoped")
	}
	for i, existing := range r.scope.bindings {
		if existing.WorkspaceID == b.WorkspaceID &&
			existing.ParentProjectID == b.ParentProjectID &&
			existing.ChildProjectID == b.ChildProjectID {
			r.scope.bindings[i].ScopeSubpath = b.ScopeSubpath
			r.scope.bindings[i].UpdatedAt = b.UpdatedAt
			return r.scope.bindings[i], nil
		}
	}
	r.scope.bindings = append(r.scope.bindings, b)
	return b, nil
}

func (r *memProjectRepo) DeleteBinding(_ context.Context, workspaceID, parentProjectID, childProjectID uuid.UUID) (bool, error) {
	if r.scope == nil {
		return false, errs.New(errs.ErrPreconditionFailed, "DeleteBinding", "unscoped")
	}
	for i, existing := range r.scope.bindings {
		if existing.WorkspaceID == workspaceID &&
			existing.ParentProjectID == parentProjectID &&
			existing.ChildProjectID == childProjectID {
			r.scope.bindings = append(r.scope.bindings[:i], r.scope.bindings[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

func (r *memProjectRepo) DeleteBindingsForProject(_ context.Context, workspaceID, projectID uuid.UUID) error {
	if r.scope == nil {
		return errs.New(errs.ErrPreconditionFailed, "DeleteBindingsForProject", "unscoped")
	}
	var kept []project.ProjectBinding
	for _, b := range r.scope.bindings {
		if b.WorkspaceID == workspaceID && (b.ParentProjectID == projectID || b.ChildProjectID == projectID) {
			continue
		}
		kept = append(kept, b)
	}
	r.scope.bindings = kept
	return nil
}

func (r *memProjectRepo) ListBindings(_ context.Context, filter project.ListBindingsFilter) ([]project.ProjectBinding, error) {
	var src []project.ProjectBinding
	var projects map[uuid.UUID]project.Project
	if r.scope != nil {
		src = r.scope.bindings
		projects = r.scope.projects
	} else {
		r.env.mu.Lock()
		src = append([]project.ProjectBinding(nil), r.env.bindings...)
		projects = r.env.projects
		r.env.mu.Unlock()
	}
	var out []project.ProjectBinding
	for _, b := range src {
		if b.WorkspaceID != filter.WorkspaceID {
			continue
		}
		pParent, okP := projects[b.ParentProjectID]
		pChild, okC := projects[b.ChildProjectID]
		if !okP || !okC || pParent.DeletedAt != nil || pChild.DeletedAt != nil {
			continue
		}
		if filter.ParentProjectID != nil && b.ParentProjectID != *filter.ParentProjectID {
			continue
		}
		if filter.ChildProjectID != nil && b.ChildProjectID != *filter.ChildProjectID {
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

// TestProjectService_AuthAndTenantIsolation verifies authentication and workspace
// ownership checks across all read and write methods.
func TestProjectService_AuthAndTenantIsolation(t *testing.T) {
	t.Parallel()

	env := newMemProjectEnv()
	svc := project.NewService(newMemProjectRepo(env), env)

	tenantA := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	tenantB := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	projID := uuid.MustParse("99999999-9999-9999-9999-999999999999")
	actorA := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")

	unauthCtx := context.Background()
	ctxA := authctx.WithIdentity(context.Background(), authctx.Identity{
		WorkspaceID: tenantA,
		ActorID:     actorA,
		Kind:        authctx.ActorKindHuman,
		Handle:      "mario",
	})

	newName := "Updated"

	for _, tc := range []struct {
		name string
		fn   func(ctx context.Context, wsID uuid.UUID) error
	}{
		{
			name: "CreateProject",
			fn: func(ctx context.Context, wsID uuid.UUID) error {
				_, err := svc.CreateProject(ctx, project.CreateProjectInput{
					WorkspaceID: wsID,
					Slug:        "billing-api",
					KeyPrefix:   "BILL",
					Name:        "Billing API",
				})
				return err
			},
		},
		{
			name: "GetProject",
			fn: func(ctx context.Context, wsID uuid.UUID) error {
				_, err := svc.GetProject(ctx, wsID, projID)
				return err
			},
		},
		{
			name: "GetProjectBySlug",
			fn: func(ctx context.Context, wsID uuid.UUID) error {
				_, err := svc.GetProjectBySlug(ctx, wsID, "billing-api")
				return err
			},
		},
		{
			name: "ListProjects",
			fn: func(ctx context.Context, wsID uuid.UUID) error {
				_, err := svc.ListProjects(ctx, project.ListProjectsFilter{WorkspaceID: wsID})
				return err
			},
		},
		{
			name: "UpdateProject",
			fn: func(ctx context.Context, wsID uuid.UUID) error {
				_, err := svc.UpdateProject(ctx, project.UpdateProjectInput{
					WorkspaceID: wsID,
					ProjectID:   projID,
					Name:        &newName,
				})
				return err
			},
		},
		{
			name: "DeleteProject",
			fn:   func(ctx context.Context, wsID uuid.UUID) error { return svc.DeleteProject(ctx, wsID, projID) },
		},
	} {
		t.Run("unauthenticated/"+tc.name, func(t *testing.T) {
			if err := tc.fn(unauthCtx, tenantA); !errors.Is(err, errs.ErrUnauthenticated) {
				t.Fatalf("expected ErrUnauthenticated, got %v", err)
			}
		})
		t.Run("cross_tenant/"+tc.name, func(t *testing.T) {
			if err := tc.fn(ctxA, tenantB); !errors.Is(err, errs.ErrPermissionDenied) {
				t.Fatalf("expected ErrPermissionDenied, got %v", err)
			}
		})
	}
}

// TestProjectService_HostPathRejectionAndValidation verifies table-driven slug,
// key-prefix, and strict zero-host-filesystem-path validation on Name and RemoteRepoURL.
func TestProjectService_HostPathRejectionAndValidation(t *testing.T) {
	t.Parallel()

	env := newMemProjectEnv()
	svc := project.NewService(newMemProjectRepo(env), env)

	wsID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	actorID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	ctx := authctx.WithIdentity(context.Background(), authctx.Identity{
		WorkspaceID: wsID,
		ActorID:     actorID,
		Kind:        authctx.ActorKindAgent,
		Handle:      "agent-01",
	})

	invalidCases := []struct {
		name string
		in   project.CreateProjectInput
	}{
		{
			name: "invalid uppercase slug",
			in:   project.CreateProjectInput{WorkspaceID: wsID, Slug: "Billing_API", KeyPrefix: "BILL", Name: "Billing"},
		},
		{
			name: "lowercase key_prefix",
			in:   project.CreateProjectInput{WorkspaceID: wsID, Slug: "billing-api", KeyPrefix: "bill", Name: "Billing"},
		},
		{
			name: "single char key_prefix",
			in:   project.CreateProjectInput{WorkspaceID: wsID, Slug: "billing-api", KeyPrefix: "B", Name: "Billing"},
		},
		{
			name: "unix Users path in RemoteRepoURL",
			in:   project.CreateProjectInput{WorkspaceID: wsID, Slug: "billing-api", KeyPrefix: "BILL", Name: "Billing", RemoteRepoURL: "/Users/mario/Desktop/app"},
		},
		{
			name: "tilde path in RemoteRepoURL",
			in:   project.CreateProjectInput{WorkspaceID: wsID, Slug: "billing-api", KeyPrefix: "BILL", Name: "Billing", RemoteRepoURL: "~/github.com/org/repo"},
		},
		{
			name: "windows path in RemoteRepoURL",
			in:   project.CreateProjectInput{WorkspaceID: wsID, Slug: "billing-api", KeyPrefix: "BILL", Name: "Billing", RemoteRepoURL: `C:\Users\paolo\Downloads\repo`},
		},
		{
			name: "file scheme in RemoteRepoURL",
			in:   project.CreateProjectInput{WorkspaceID: wsID, Slug: "billing-api", KeyPrefix: "BILL", Name: "Billing", RemoteRepoURL: "file:///Users/mario/repo"},
		},
		{
			name: "unix path in Name",
			in:   project.CreateProjectInput{WorkspaceID: wsID, Slug: "billing-api", KeyPrefix: "BILL", Name: "/Users/mario/Desktop/billing"},
		},
		{
			name: "tilde path in Name",
			in:   project.CreateProjectInput{WorkspaceID: wsID, Slug: "billing-api", KeyPrefix: "BILL", Name: "~/Desktop/billing"},
		},
		{
			name: "windows path in Name",
			in:   project.CreateProjectInput{WorkspaceID: wsID, Slug: "billing-api", KeyPrefix: "BILL", Name: `C:\work\billing`},
		},
		{
			name: "file scheme in Name",
			in:   project.CreateProjectInput{WorkspaceID: wsID, Slug: "billing-api", KeyPrefix: "BILL", Name: "file:///tmp/billing"},
		},
	}

	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.CreateProject(ctx, tc.in); !errors.Is(err, errs.ErrValidation) {
				t.Fatalf("expected ErrValidation for %s, got %v", tc.name, err)
			}
		})
	}
}

// TestProjectService_CRUDDefensiveCopyAndAuditFailFast verifies end-to-end project
// lifecycle operations, defensive tag slice cloning, and transaction rollback on audit failure.
func TestProjectService_CRUDDefensiveCopyAndAuditFailFast(t *testing.T) {
	t.Parallel()

	env := newMemProjectEnv()
	svc := project.NewService(newMemProjectRepo(env), env)

	wsID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	actorID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	ctx := authctx.WithIdentity(context.Background(), authctx.Identity{
		WorkspaceID: wsID,
		ActorID:     actorID,
		Kind:        authctx.ActorKindHuman,
		Handle:      "paolo",
	})

	callerTags := []string{"golang", "backend"}
	created, err := svc.CreateProject(ctx, project.CreateProjectInput{
		WorkspaceID:   wsID,
		Slug:          "billing-api",
		KeyPrefix:     "BILL",
		Name:          "Billing Service",
		Description:   "Core invoicing engine",
		RemoteRepoURL: "https://github.com/acme/billing-api",
		DefaultBranch: "main",
		Tags:          callerTags,
	})
	if err != nil {
		t.Fatalf("CreateProject failed: %v", err)
	}
	callerTags[0] = "mutated"
	if created.Tags[0] != "golang" {
		t.Fatalf("expected defensive copy on CreateProject input Tags, got %+v", created.Tags)
	}
	if created.Status != project.StatusActive || created.CreatedBy != actorID {
		t.Fatalf("unexpected created project metadata: %+v", created)
	}

	fetched, err := svc.GetProject(ctx, wsID, created.ID)
	if err != nil {
		t.Fatalf("GetProject failed: %v", err)
	}
	fetched.Tags[0] = "corrupted"
	bySlug, err := svc.GetProjectBySlug(ctx, wsID, "billing-api")
	if err != nil || bySlug.Tags[0] != "golang" {
		t.Fatalf("expected defensive copy on GetProject output Tags, got %+v (err=%v)", bySlug, err)
	}

	// Reject host filesystem paths on UpdateProject too.
	badRepo := "file:///Users/paolo/billing-api"
	if _, err := svc.UpdateProject(ctx, project.UpdateProjectInput{
		WorkspaceID:   wsID,
		ProjectID:     created.ID,
		RemoteRepoURL: &badRepo,
	}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("expected ErrValidation on UpdateProject host path, got %v", err)
	}

	// Verify atomic rollback when scope.Audit().Record fails.
	auditErr := errors.New("audit storage unavailable")
	env.auditFn = func() error { return auditErr }
	paused := project.StatusPaused
	if _, err := svc.UpdateProject(ctx, project.UpdateProjectInput{
		WorkspaceID: wsID,
		ProjectID:   created.ID,
		Status:      &paused,
	}); !errors.Is(err, auditErr) {
		t.Fatalf("expected auditErr, got %v", err)
	}
	env.auditFn = nil

	afterRollback, err := svc.GetProject(ctx, wsID, created.ID)
	if err != nil || afterRollback.Status != project.StatusActive {
		t.Fatalf("expected rollback to preserve StatusActive, got %+v (err=%v)", afterRollback, err)
	}

	// Valid update, list, and delete.
	sshURL := "git@github.com:acme/billing-api.git"
	updated, err := svc.UpdateProject(ctx, project.UpdateProjectInput{
		WorkspaceID:   wsID,
		ProjectID:     created.ID,
		RemoteRepoURL: &sshURL,
		Status:        &paused,
		Tags:          []string{"payments"},
	})
	if err != nil || updated.Status != project.StatusPaused || updated.RemoteRepoURL != sshURL {
		t.Fatalf("UpdateProject failed: %+v, err=%v", updated, err)
	}

	listed, err := svc.ListProjects(ctx, project.ListProjectsFilter{
		WorkspaceID: wsID,
		Status:      &paused,
		Tag:         "payments",
	})
	if err != nil || len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("ListProjects failed: %+v, err=%v", listed, err)
	}

	if err := svc.DeleteProject(ctx, wsID, created.ID); err != nil {
		t.Fatalf("DeleteProject failed: %v", err)
	}
	if _, err := svc.GetProject(ctx, wsID, created.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("expected deleted project to return ErrNotFound, got %v", err)
	}
}

// TestProjectService_MultiBindingDAGCycleAndSafeParentDeletion verifies multi-parent
// bindings, relative subpath validation, anti-cycle DAG checks, and that deleting
// a macro parent project removes ONLY bindings while preserving child projects.
func TestProjectService_MultiBindingDAGCycleAndSafeParentDeletion(t *testing.T) {
	t.Parallel()

	env := newMemProjectEnv()
	svc := project.NewService(newMemProjectRepo(env), env)
	wsID := uuid.New()
	ctx := authctx.WithIdentity(context.Background(), authctx.Identity{
		WorkspaceID: wsID,
		ActorID:     uuid.New(),
		Kind:        authctx.ActorKindHuman,
		Handle:      "mario",
	})

	macroA, _ := svc.CreateProject(ctx, project.CreateProjectInput{WorkspaceID: wsID, Slug: "macro-a", KeyPrefix: "MACA", Name: "Macro A"})
	macroB, _ := svc.CreateProject(ctx, project.CreateProjectInput{WorkspaceID: wsID, Slug: "macro-b", KeyPrefix: "MACB", Name: "Macro B"})
	subWeb, _ := svc.CreateProject(ctx, project.CreateProjectInput{WorkspaceID: wsID, Slug: "sub-web", KeyPrefix: "WEB", Name: "Web App"})

	// Reject host filesystem paths or parent traversal in ScopeSubpath.
	if _, err := svc.BindSubproject(ctx, project.BindSubprojectInput{
		WorkspaceID:     wsID,
		ParentProjectID: macroA.ID,
		ChildProjectID:  subWeb.ID,
		ScopeSubpath:    "/Users/mario/repo/apps/web",
	}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("expected ErrValidation for absolute ScopeSubpath, got %v", err)
	}

	// Multi-binding: subWeb can belong to both macroA and macroB cleanly!
	b1, err := svc.BindSubproject(ctx, project.BindSubprojectInput{
		WorkspaceID:     wsID,
		ParentProjectID: macroA.ID,
		ChildProjectID:  subWeb.ID,
		ScopeSubpath:    "apps/web",
	})
	if err != nil || b1.ScopeSubpath != "apps/web" {
		t.Fatalf("BindSubproject macroA->subWeb failed: %+v err=%v", b1, err)
	}
	if _, err := svc.BindSubproject(ctx, project.BindSubprojectInput{
		WorkspaceID:     wsID,
		ParentProjectID: macroB.ID,
		ChildProjectID:  subWeb.ID,
		ScopeSubpath:    "packages/web",
	}); err != nil {
		t.Fatalf("BindSubproject macroB->subWeb failed: %v", err)
	}

	// Cycle guard: subWeb -> macroA must fail with ErrConflict!
	if _, err := svc.BindSubproject(ctx, project.BindSubprojectInput{
		WorkspaceID:     wsID,
		ParentProjectID: subWeb.ID,
		ChildProjectID:  macroA.ID,
	}); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("expected ErrConflict for cyclic subWeb->macroA binding, got %v", err)
	}

	// Safe non-destructive parent deletion: deleting macroA removes macroA->subWeb
	// binding while subWeb remains alive (and still bound under macroB)!
	if err := svc.DeleteProject(ctx, wsID, macroA.ID); err != nil {
		t.Fatalf("DeleteProject(macroA) failed: %v", err)
	}
	stillAlive, err := svc.GetProject(ctx, wsID, subWeb.ID)
	if err != nil || stillAlive.ID != subWeb.ID {
		t.Fatalf("expected subWeb to survive parent deletion intact, got err=%v", err)
	}
	remainingBindings, err := svc.ListBindings(ctx, project.ListBindingsFilter{WorkspaceID: wsID})
	if err != nil || len(remainingBindings) != 1 || remainingBindings[0].ParentProjectID != macroB.ID {
		t.Fatalf("expected only macroB->subWeb binding to remain, got %+v err=%v", remainingBindings, err)
	}
}
