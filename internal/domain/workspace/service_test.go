// File service_test.go verifies workspace.Service authentication, multi-tenant
// ownership isolation, RBAC owner checks, defensive copies, and atomic audit
// fail-fast transaction rollbacks.
package workspace_test

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/tx"
	"github.com/mindmain/flowsk/internal/domain/workspace"
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
	auditEnabled bool
	recorder     *memAuditRecorder
	state        *memWorkspaceState
}

func (s *memScope) Audit() tx.AuditRecorder      { return s.recorder }
func (s *memScope) IsAuditEnabled() bool         { return s.auditEnabled }
func (s *memScope) TxHandle() tx.DialectTxHandle { return memTxHandle{} }

type memWorkspaceState struct {
	workspaces map[uuid.UUID]workspace.Workspace
	members    map[uuid.UUID]map[uuid.UUID]workspace.WorkspaceMember
}

func newMemWorkspaceState() *memWorkspaceState {
	return &memWorkspaceState{
		workspaces: make(map[uuid.UUID]workspace.Workspace),
		members:    make(map[uuid.UUID]map[uuid.UUID]workspace.WorkspaceMember),
	}
}

func (s *memWorkspaceState) clone() *memWorkspaceState {
	cp := newMemWorkspaceState()
	for id, ws := range s.workspaces {
		cp.workspaces[id] = ws.Clone()
	}
	for wsID, mMap := range s.members {
		cp.members[wsID] = make(map[uuid.UUID]workspace.WorkspaceMember, len(mMap))
		for actorID, m := range mMap {
			cp.members[wsID][actorID] = m
		}
	}
	return cp
}

type memWorkspaceEnv struct {
	mu      sync.Mutex
	state   *memWorkspaceState
	audits  []tx.AuditEntry
	auditFn func() error
}

func newMemWorkspaceEnv() *memWorkspaceEnv {
	return &memWorkspaceEnv{
		state: newMemWorkspaceState(),
	}
}

func (e *memWorkspaceEnv) WithTransaction(ctx context.Context, fn tx.TxFunc) error {
	return e.runTx(ctx, true, fn)
}

func (e *memWorkspaceEnv) WithNoAuditTransaction(ctx context.Context, fn tx.TxFunc) error {
	return e.runTx(ctx, false, fn)
}

func (e *memWorkspaceEnv) runTx(ctx context.Context, auditEnabled bool, fn tx.TxFunc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	staged := e.state.clone()
	var stagedAudits []tx.AuditEntry
	var failErr error
	if e.auditFn != nil {
		failErr = e.auditFn()
	}
	scope := &memScope{
		auditEnabled: auditEnabled,
		recorder:     &memAuditRecorder{entries: &stagedAudits, failErr: failErr},
		state:        staged,
	}
	if err := fn(ctx, scope); err != nil {
		return err
	}
	e.state = staged
	e.audits = append(e.audits, stagedAudits...)
	return nil
}

type memWorkspaceRepo struct {
	env   *memWorkspaceEnv
	scope *memScope
}

func newMemWorkspaceRepo(env *memWorkspaceEnv) *memWorkspaceRepo {
	return &memWorkspaceRepo{env: env}
}

func (r *memWorkspaceRepo) WithScope(scope tx.Scope) workspace.Repository {
	ms, _ := scope.(*memScope)
	return &memWorkspaceRepo{env: r.env, scope: ms}
}

func (r *memWorkspaceRepo) readState(fn func(st *memWorkspaceState) error) error {
	if r.scope != nil {
		return fn(r.scope.state)
	}
	r.env.mu.Lock()
	defer r.env.mu.Unlock()
	return fn(r.env.state)
}

func (r *memWorkspaceRepo) writeState(op string, fn func(st *memWorkspaceState) error) error {
	if r.scope == nil {
		return errs.New(errs.ErrPreconditionFailed, op, "mutation outside transaction scope")
	}
	return fn(r.scope.state)
}

func (r *memWorkspaceRepo) Create(_ context.Context, ws workspace.Workspace) error {
	return r.writeState("workspace.Repo.Create", func(st *memWorkspaceState) error {
		if _, exists := st.workspaces[ws.ID]; exists {
			return errs.New(errs.ErrConflict, "workspace.Repo.Create", "duplicate workspace ID")
		}
		for _, existing := range st.workspaces {
			if existing.DeletedAt == nil && existing.Slug == ws.Slug {
				return errs.New(errs.ErrConflict, "workspace.Repo.Create", "duplicate workspace slug")
			}
		}
		st.workspaces[ws.ID] = ws.Clone()
		return nil
	})
}

func (r *memWorkspaceRepo) GetByID(_ context.Context, workspaceID uuid.UUID) (workspace.Workspace, error) {
	var out workspace.Workspace
	err := r.readState(func(st *memWorkspaceState) error {
		ws, ok := st.workspaces[workspaceID]
		if !ok || ws.DeletedAt != nil {
			return errs.New(errs.ErrNotFound, "workspace.Repo.GetByID", "workspace not found")
		}
		out = ws.Clone()
		return nil
	})
	return out, err
}

func (r *memWorkspaceRepo) GetBySlug(_ context.Context, slug string) (workspace.Workspace, error) {
	var out workspace.Workspace
	err := r.readState(func(st *memWorkspaceState) error {
		for _, ws := range st.workspaces {
			if ws.DeletedAt == nil && ws.Slug == slug {
				out = ws.Clone()
				return nil
			}
		}
		return errs.New(errs.ErrNotFound, "workspace.Repo.GetBySlug", "workspace not found")
	})
	return out, err
}

func (r *memWorkspaceRepo) ListForActor(_ context.Context, actorID uuid.UUID) ([]workspace.Workspace, error) {
	var out []workspace.Workspace
	err := r.readState(func(st *memWorkspaceState) error {
		for wsID, ws := range st.workspaces {
			if ws.DeletedAt != nil {
				continue
			}
			if mMap, ok := st.members[wsID]; ok {
				if _, memberExists := mMap[actorID]; memberExists {
					out = append(out, ws.Clone())
				}
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
		return nil
	})
	return out, err
}

func (r *memWorkspaceRepo) Update(_ context.Context, ws workspace.Workspace) error {
	return r.writeState("workspace.Repo.Update", func(st *memWorkspaceState) error {
		cur, ok := st.workspaces[ws.ID]
		if !ok || cur.DeletedAt != nil {
			return errs.New(errs.ErrNotFound, "workspace.Repo.Update", "workspace not found")
		}
		st.workspaces[ws.ID] = ws.Clone()
		return nil
	})
}

func (r *memWorkspaceRepo) SoftDelete(_ context.Context, workspaceID uuid.UUID) error {
	return r.writeState("workspace.Repo.SoftDelete", func(st *memWorkspaceState) error {
		cur, ok := st.workspaces[workspaceID]
		if !ok || cur.DeletedAt != nil {
			return errs.New(errs.ErrNotFound, "workspace.Repo.SoftDelete", "workspace not found")
		}
		now := time.Now().UTC()
		cur.DeletedAt = &now
		st.workspaces[workspaceID] = cur
		return nil
	})
}

func (r *memWorkspaceRepo) UpsertMember(_ context.Context, m workspace.WorkspaceMember) error {
	return r.writeState("workspace.Repo.UpsertMember", func(st *memWorkspaceState) error {
		if st.members[m.WorkspaceID] == nil {
			st.members[m.WorkspaceID] = make(map[uuid.UUID]workspace.WorkspaceMember)
		}
		for actorID, existing := range st.members[m.WorkspaceID] {
			if existing.Handle == m.Handle && actorID != m.ActorID {
				return errs.New(errs.ErrConflict, "workspace.Repo.UpsertMember", "handle already in use")
			}
		}
		st.members[m.WorkspaceID][m.ActorID] = m
		return nil
	})
}

func (r *memWorkspaceRepo) ListMembers(_ context.Context, workspaceID uuid.UUID) ([]workspace.WorkspaceMember, error) {
	var out []workspace.WorkspaceMember
	err := r.readState(func(st *memWorkspaceState) error {
		for _, m := range st.members[workspaceID] {
			out = append(out, m)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Handle < out[j].Handle })
		return nil
	})
	return out, err
}

// TestWorkspaceService_AuthAndTenantIsolation verifies that unauthenticated and
// cross-tenant requests are deterministically rejected across all workspace operations.
func TestWorkspaceService_AuthAndTenantIsolation(t *testing.T) {
	t.Parallel()

	env := newMemWorkspaceEnv()
	repo := newMemWorkspaceRepo(env)
	svc := workspace.NewService(repo, env)

	tenantA := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	tenantB := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	actorA := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")

	// Seed tenantB directly for slug lookup isolation testing.
	env.state.workspaces[tenantB] = workspace.Workspace{
		ID:        tenantB,
		Slug:      "foreign-ws",
		Name:      "Foreign Workspace",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}

	unauthCtx := context.Background()
	ctxA := authctx.WithIdentity(context.Background(), authctx.Identity{
		WorkspaceID: tenantA,
		ActorID:     actorA,
		Kind:        authctx.ActorKindHuman,
		Handle:      "mario",
	})

	newName := "Updated"

	unauthCases := []struct {
		name string
		call func() error
	}{
		{
			name: "CreateWorkspace",
			call: func() error {
				_, err := svc.CreateWorkspace(unauthCtx, workspace.CreateWorkspaceInput{Slug: "acme-core", Name: "Acme"})
				return err
			},
		},
		{
			name: "GetWorkspace",
			call: func() error {
				_, err := svc.GetWorkspace(unauthCtx, tenantA)
				return err
			},
		},
		{
			name: "GetWorkspaceBySlug",
			call: func() error {
				_, err := svc.GetWorkspaceBySlug(unauthCtx, "foreign-ws")
				return err
			},
		},
		{
			name: "ListWorkspaces",
			call: func() error {
				_, err := svc.ListWorkspaces(unauthCtx)
				return err
			},
		},
		{
			name: "UpdateWorkspace",
			call: func() error {
				_, err := svc.UpdateWorkspace(unauthCtx, workspace.UpdateWorkspaceInput{WorkspaceID: tenantA, Name: &newName})
				return err
			},
		},
		{
			name: "ArchiveWorkspace",
			call: func() error { return svc.ArchiveWorkspace(unauthCtx, tenantA) },
		},
		{
			name: "UpsertMember",
			call: func() error {
				_, err := svc.UpsertMember(unauthCtx, workspace.UpsertMemberInput{
					WorkspaceID: tenantA,
					Kind:        authctx.ActorKindAgent,
					Handle:      "agent-01",
					Role:        workspace.MemberRoleContributor,
				})
				return err
			},
		},
		{
			name: "ListMembers",
			call: func() error {
				_, err := svc.ListMembers(unauthCtx, tenantA)
				return err
			},
		},
	}

	for _, tc := range unauthCases {
		t.Run("unauthenticated/"+tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, errs.ErrUnauthenticated) {
				t.Fatalf("expected ErrUnauthenticated, got %v", err)
			}
		})
	}

	crossTenantCases := []struct {
		name string
		call func() error
	}{
		{
			name: "GetWorkspace",
			call: func() error {
				_, err := svc.GetWorkspace(ctxA, tenantB)
				return err
			},
		},
		{
			name: "GetWorkspaceBySlug",
			call: func() error {
				_, err := svc.GetWorkspaceBySlug(ctxA, "foreign-ws")
				return err
			},
		},
		{
			name: "UpdateWorkspace",
			call: func() error {
				_, err := svc.UpdateWorkspace(ctxA, workspace.UpdateWorkspaceInput{WorkspaceID: tenantB, Name: &newName})
				return err
			},
		},
		{
			name: "ArchiveWorkspace",
			call: func() error { return svc.ArchiveWorkspace(ctxA, tenantB) },
		},
		{
			name: "UpsertMember",
			call: func() error {
				_, err := svc.UpsertMember(ctxA, workspace.UpsertMemberInput{
					WorkspaceID: tenantB,
					Kind:        authctx.ActorKindAgent,
					Handle:      "agent-01",
					Role:        workspace.MemberRoleContributor,
				})
				return err
			},
		},
		{
			name: "ListMembers",
			call: func() error {
				_, err := svc.ListMembers(ctxA, tenantB)
				return err
			},
		},
	}

	for _, tc := range crossTenantCases {
		t.Run("cross_tenant/"+tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, errs.ErrPermissionDenied) {
				t.Fatalf("expected ErrPermissionDenied, got %v", err)
			}
		})
	}
}

// TestWorkspaceService_LifecycleAndAuditRollback verifies workspace creation with
// initial Owner membership, validation, defensive copies, RBAC archive checks,
// and atomic rollback when audit recording fails.
func TestWorkspaceService_LifecycleAndAuditRollback(t *testing.T) {
	t.Parallel()

	env := newMemWorkspaceEnv()
	repo := newMemWorkspaceRepo(env)
	svc := workspace.NewService(repo, env)

	bootstrapWS := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	ownerActor := uuid.MustParse("aaaaaaaa-1111-1111-1111-111111111111")
	bootCtx := authctx.WithIdentity(context.Background(), authctx.Identity{
		WorkspaceID: bootstrapWS,
		ActorID:     ownerActor,
		Kind:        authctx.ActorKindHuman,
		Handle:      "alice",
	})

	invalidInputs := []workspace.CreateWorkspaceInput{
		{Slug: "Invalid_Slug!", Name: "Acme"},
		{Slug: "a", Name: "Too short slug"},
		{Slug: "acme-platform", Name: "   "},
	}
	for _, bad := range invalidInputs {
		if _, err := svc.CreateWorkspace(bootCtx, bad); !errors.Is(err, errs.ErrValidation) {
			t.Fatalf("expected ErrValidation for %+v, got %v", bad, err)
		}
	}

	labels := map[string]string{"env": "prod"}
	created, err := svc.CreateWorkspace(bootCtx, workspace.CreateWorkspaceInput{
		Slug:        "acme-platform",
		Name:        "Acme Platform",
		Description: "Core engineering tenant",
		Labels:      labels,
	})
	if err != nil {
		t.Fatalf("CreateWorkspace failed: %v", err)
	}
	labels["env"] = "mutated"
	if created.Labels["env"] != "prod" {
		t.Fatalf("expected defensive copy of Labels, got %+v", created.Labels)
	}
	if created.DefaultLeaseTTLSeconds <= 0 {
		t.Fatalf("expected positive default lease TTL, got %d", created.DefaultLeaseTTLSeconds)
	}

	// Authenticate directly inside the newly provisioned workspace.
	wsCtx := authctx.WithIdentity(context.Background(), authctx.Identity{
		WorkspaceID: created.ID,
		ActorID:     ownerActor,
		Kind:        authctx.ActorKindHuman,
		Handle:      "alice",
	})

	members, err := svc.ListMembers(wsCtx, created.ID)
	if err != nil {
		t.Fatalf("ListMembers failed: %v", err)
	}
	if len(members) != 1 || members[0].ActorID != ownerActor || members[0].Role != workspace.MemberRoleOwner {
		t.Fatalf("expected caller registered as MemberRoleOwner, got %+v", members)
	}

	listed, err := svc.ListWorkspaces(wsCtx)
	if err != nil || len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("expected 1 accessible workspace, got %+v (err=%v)", listed, err)
	}

	bySlug, err := svc.GetWorkspaceBySlug(wsCtx, "acme-platform")
	if err != nil || bySlug.ID != created.ID {
		t.Fatalf("GetWorkspaceBySlug failed: %+v, err=%v", bySlug, err)
	}

	// Register an agent contributor and verify non-owners cannot archive workspace.
	agentMember, err := svc.UpsertMember(wsCtx, workspace.UpsertMemberInput{
		WorkspaceID: created.ID,
		Kind:        authctx.ActorKindAgent,
		Handle:      "coder-agent-1",
		DisplayName: "Coder Agent",
		Role:        workspace.MemberRoleContributor,
	})
	if err != nil {
		t.Fatalf("UpsertMember failed: %v", err)
	}
	if agentMember.ActorID == uuid.Nil {
		t.Fatalf("expected generated ActorID on UpsertMember")
	}

	agentCtx := authctx.WithIdentity(context.Background(), authctx.Identity{
		WorkspaceID: created.ID,
		ActorID:     agentMember.ActorID,
		Kind:        authctx.ActorKindAgent,
		Handle:      agentMember.Handle,
	})
	if err := svc.ArchiveWorkspace(agentCtx, created.ID); !errors.Is(err, errs.ErrPermissionDenied) {
		t.Fatalf("expected non-owner ArchiveWorkspace to fail with ErrPermissionDenied, got %v", err)
	}

	// Inject audit failure and verify UpdateWorkspace rolls back completely.
	auditBoom := errors.New("simulated audit write failure")
	env.auditFn = func() error { return auditBoom }
	updatedTitle := "Should Rollback"
	if _, err := svc.UpdateWorkspace(wsCtx, workspace.UpdateWorkspaceInput{
		WorkspaceID: created.ID,
		Name:        &updatedTitle,
	}); !errors.Is(err, auditBoom) {
		t.Fatalf("expected audit failure %v, got %v", auditBoom, err)
	}
	env.auditFn = nil

	reloaded, err := svc.GetWorkspace(wsCtx, created.ID)
	if err != nil {
		t.Fatalf("GetWorkspace failed: %v", err)
	}
	if reloaded.Name != "Acme Platform" {
		t.Fatalf("expected rolled-back workspace Name 'Acme Platform', got %q", reloaded.Name)
	}

	// Valid update & owner archive succeed and record audits.
	finalName := "Acme Platform v2"
	updated, err := svc.UpdateWorkspace(wsCtx, workspace.UpdateWorkspaceInput{
		WorkspaceID: created.ID,
		Name:        &finalName,
	})
	if err != nil || updated.Name != finalName {
		t.Fatalf("UpdateWorkspace failed: %+v, err=%v", updated, err)
	}

	if err := svc.ArchiveWorkspace(wsCtx, created.ID); err != nil {
		t.Fatalf("ArchiveWorkspace by owner failed: %v", err)
	}
	if _, err := svc.GetWorkspace(wsCtx, created.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("expected archived workspace to return ErrNotFound, got %v", err)
	}
}
