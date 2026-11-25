// File service_test.go verifies objective.Service authentication, cross-tenant
// isolation, slug/title validation, StatusActive defaults, defensive cloning of
// SuccessCriteria, and atomic audit rollback semantics.
package objective_test

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
	"github.com/mindmain/flowsk/internal/domain/objective"
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
	recorder   *memAuditRecorder
	objectives map[uuid.UUID]objective.Objective
}

func (s *memScope) Audit() tx.AuditRecorder      { return s.recorder }
func (s *memScope) IsAuditEnabled() bool         { return true }
func (s *memScope) TxHandle() tx.DialectTxHandle { return memTxHandle{} }

type memObjectiveEnv struct {
	mu         sync.Mutex
	objectives map[uuid.UUID]objective.Objective
	audits     []tx.AuditEntry
	auditFn    func() error
}

func newMemObjectiveEnv() *memObjectiveEnv {
	return &memObjectiveEnv{
		objectives: make(map[uuid.UUID]objective.Objective),
	}
}

func (e *memObjectiveEnv) WithTransaction(ctx context.Context, fn tx.TxFunc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	staged := make(map[uuid.UUID]objective.Objective, len(e.objectives))
	for id, o := range e.objectives {
		staged[id] = o.Clone()
	}
	var stagedAudits []tx.AuditEntry
	var failErr error
	if e.auditFn != nil {
		failErr = e.auditFn()
	}
	scope := &memScope{
		recorder:   &memAuditRecorder{entries: &stagedAudits, failErr: failErr},
		objectives: staged,
	}
	if err := fn(ctx, scope); err != nil {
		return err
	}
	e.objectives = staged
	e.audits = append(e.audits, stagedAudits...)
	return nil
}

func (e *memObjectiveEnv) WithNoAuditTransaction(ctx context.Context, fn tx.TxFunc) error {
	return e.WithTransaction(ctx, fn)
}

type memObjectiveRepo struct {
	env   *memObjectiveEnv
	scope *memScope
}

func newMemObjectiveRepo(env *memObjectiveEnv) *memObjectiveRepo {
	return &memObjectiveRepo{env: env}
}

func (r *memObjectiveRepo) WithScope(scope tx.Scope) objective.Repository {
	ms, _ := scope.(*memScope)
	return &memObjectiveRepo{env: r.env, scope: ms}
}

func (r *memObjectiveRepo) readMap(fn func(m map[uuid.UUID]objective.Objective) error) error {
	if r.scope != nil {
		return fn(r.scope.objectives)
	}
	r.env.mu.Lock()
	defer r.env.mu.Unlock()
	return fn(r.env.objectives)
}

func (r *memObjectiveRepo) writeMap(op string, fn func(m map[uuid.UUID]objective.Objective) error) error {
	if r.scope == nil {
		return errs.New(errs.ErrPreconditionFailed, op, "mutation outside transaction scope")
	}
	return fn(r.scope.objectives)
}

func (r *memObjectiveRepo) Create(_ context.Context, o objective.Objective) error {
	return r.writeMap("objective.Repo.Create", func(m map[uuid.UUID]objective.Objective) error {
		for _, existing := range m {
			if existing.DeletedAt == nil && existing.WorkspaceID == o.WorkspaceID && existing.ProjectID == o.ProjectID && existing.Slug == o.Slug {
				return errs.New(errs.ErrConflict, "objective.Repo.Create", "duplicate objective slug")
			}
		}
		m[o.ID] = o.Clone()
		return nil
	})
}

func (r *memObjectiveRepo) GetByID(_ context.Context, workspaceID, objectiveID uuid.UUID) (objective.Objective, error) {
	var out objective.Objective
	err := r.readMap(func(m map[uuid.UUID]objective.Objective) error {
		o, ok := m[objectiveID]
		if !ok || o.DeletedAt != nil || o.WorkspaceID != workspaceID {
			return errs.New(errs.ErrNotFound, "objective.Repo.GetByID", "objective not found")
		}
		out = o.Clone()
		return nil
	})
	return out, err
}

func (r *memObjectiveRepo) GetBySlug(_ context.Context, workspaceID, projectID uuid.UUID, slug string) (objective.Objective, error) {
	var out objective.Objective
	err := r.readMap(func(m map[uuid.UUID]objective.Objective) error {
		for _, o := range m {
			if o.DeletedAt == nil && o.WorkspaceID == workspaceID && o.ProjectID == projectID && o.Slug == slug {
				out = o.Clone()
				return nil
			}
		}
		return errs.New(errs.ErrNotFound, "objective.Repo.GetBySlug", "objective not found")
	})
	return out, err
}

func (r *memObjectiveRepo) List(_ context.Context, filter objective.ListObjectivesFilter) ([]objective.Objective, error) {
	var out []objective.Objective
	err := r.readMap(func(m map[uuid.UUID]objective.Objective) error {
		for _, o := range m {
			if o.DeletedAt != nil || o.WorkspaceID != filter.WorkspaceID {
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
		sort.Slice(out, func(i, j int) bool {
			if out[i].PriorityRank != out[j].PriorityRank {
				return out[i].PriorityRank < out[j].PriorityRank
			}
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		})
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

func (r *memObjectiveRepo) Update(_ context.Context, o objective.Objective) error {
	return r.writeMap("objective.Repo.Update", func(m map[uuid.UUID]objective.Objective) error {
		cur, ok := m[o.ID]
		if !ok || cur.DeletedAt != nil || cur.WorkspaceID != o.WorkspaceID {
			return errs.New(errs.ErrNotFound, "objective.Repo.Update", "objective not found")
		}
		m[o.ID] = o.Clone()
		return nil
	})
}

func (r *memObjectiveRepo) SoftDelete(_ context.Context, workspaceID, objectiveID uuid.UUID) error {
	return r.writeMap("objective.Repo.SoftDelete", func(m map[uuid.UUID]objective.Objective) error {
		cur, ok := m[objectiveID]
		if !ok || cur.DeletedAt != nil || cur.WorkspaceID != workspaceID {
			return errs.New(errs.ErrNotFound, "objective.Repo.SoftDelete", "objective not found")
		}
		now := time.Now().UTC()
		cur.DeletedAt = &now
		m[objectiveID] = cur
		return nil
	})
}

// TestObjectiveService_AuthAndTenantIsolation verifies authentication and
// cross-tenant isolation across every read and mutation method.
func TestObjectiveService_AuthAndTenantIsolation(t *testing.T) {
	t.Parallel()

	env := newMemObjectiveEnv()
	svc := objective.NewService(newMemObjectiveRepo(env), env)

	tenantA := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	tenantB := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	projID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	objID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	actorA := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")

	unauthCtx := context.Background()
	ctxA := authctx.WithIdentity(context.Background(), authctx.Identity{
		WorkspaceID: tenantA,
		ActorID:     actorA,
		Kind:        authctx.ActorKindAgent,
		Handle:      "planner-01",
	})

	newTitle := "Updated Goal"

	for _, tc := range []struct {
		name string
		fn   func(ctx context.Context, wsID uuid.UUID) error
	}{
		{
			name: "CreateObjective",
			fn: func(ctx context.Context, wsID uuid.UUID) error {
				_, err := svc.CreateObjective(ctx, objective.CreateObjectiveInput{
					WorkspaceID: wsID,
					ProjectID:   projID,
					Slug:        "q4-ship",
					Title:       "Ship Q4 Release",
				})
				return err
			},
		},
		{
			name: "GetObjective",
			fn: func(ctx context.Context, wsID uuid.UUID) error {
				_, err := svc.GetObjective(ctx, wsID, objID)
				return err
			},
		},
		{
			name: "GetObjectiveBySlug",
			fn: func(ctx context.Context, wsID uuid.UUID) error {
				_, err := svc.GetObjectiveBySlug(ctx, wsID, projID, "q4-ship")
				return err
			},
		},
		{
			name: "ListObjectives",
			fn: func(ctx context.Context, wsID uuid.UUID) error {
				_, err := svc.ListObjectives(ctx, objective.ListObjectivesFilter{WorkspaceID: wsID})
				return err
			},
		},
		{
			name: "UpdateObjective",
			fn: func(ctx context.Context, wsID uuid.UUID) error {
				_, err := svc.UpdateObjective(ctx, objective.UpdateObjectiveInput{
					WorkspaceID: wsID,
					ObjectiveID: objID,
					Title:       &newTitle,
				})
				return err
			},
		},
		{
			name: "DeleteObjective",
			fn:   func(ctx context.Context, wsID uuid.UUID) error { return svc.DeleteObjective(ctx, wsID, objID) },
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

// TestObjectiveService_LifecycleValidationAndAuditFailFast tests validation,
// StatusActive default assignment, SuccessCriteria defensive copies, and atomic
// rollback when audit recording fails.
func TestObjectiveService_LifecycleValidationAndAuditFailFast(t *testing.T) {
	t.Parallel()

	env := newMemObjectiveEnv()
	svc := objective.NewService(newMemObjectiveRepo(env), env)

	wsID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	projID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	actorID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	ctx := authctx.WithIdentity(context.Background(), authctx.Identity{
		WorkspaceID: wsID,
		ActorID:     actorID,
		Kind:        authctx.ActorKindHuman,
		Handle:      "mario",
	})

	for _, bad := range []objective.CreateObjectiveInput{
		{WorkspaceID: wsID, ProjectID: uuid.Nil, Slug: "q4-goal", Title: "Missing Project"},
		{WorkspaceID: wsID, ProjectID: projID, Slug: "BAD SLUG", Title: "Bad Slug"},
		{WorkspaceID: wsID, ProjectID: projID, Slug: "q4-goal", Title: "   "},
		{WorkspaceID: wsID, ProjectID: projID, Slug: "q4-goal", Title: "Invalid Status", Status: "unknown"},
	} {
		if _, err := svc.CreateObjective(ctx, bad); !errors.Is(err, errs.ErrValidation) {
			t.Fatalf("expected ErrValidation for %+v, got %v", bad, err)
		}
	}

	criteria := []string{"100% test coverage", "zero downtime"}
	target := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	created, err := svc.CreateObjective(ctx, objective.CreateObjectiveInput{
		WorkspaceID:     wsID,
		ProjectID:       projID,
		Slug:            "q4-zero-downtime",
		Title:           "Zero-Downtime Schema Migrations",
		Summary:         "Seamless SQLite and PostgreSQL upgrades",
		SuccessCriteria: criteria,
		PriorityRank:    1,
		TargetDate:      &target,
	})
	if err != nil {
		t.Fatalf("CreateObjective failed: %v", err)
	}
	criteria[0] = "mutated"
	if created.Status != objective.StatusActive {
		t.Fatalf("expected default StatusActive, got %q", created.Status)
	}
	if created.SuccessCriteria[0] != "100% test coverage" {
		t.Fatalf("expected defensive copy of SuccessCriteria, got %+v", created.SuccessCriteria)
	}

	fetched, err := svc.GetObjectiveBySlug(ctx, wsID, projID, "q4-zero-downtime")
	if err != nil || fetched.ID != created.ID {
		t.Fatalf("GetObjectiveBySlug failed: %+v, err=%v", fetched, err)
	}
	fetched.SuccessCriteria[0] = "corrupted"
	reloaded, err := svc.GetObjective(ctx, wsID, created.ID)
	if err != nil || reloaded.SuccessCriteria[0] != "100% test coverage" {
		t.Fatalf("expected defensive copy on GetObjective output, got %+v", reloaded.SuccessCriteria)
	}

	// Verify atomic audit fail-fast rollback on UpdateObjective.
	auditBoom := errors.New("audit failure")
	env.auditFn = func() error { return auditBoom }
	achieved := objective.StatusAchieved
	if _, err := svc.UpdateObjective(ctx, objective.UpdateObjectiveInput{
		WorkspaceID: wsID,
		ObjectiveID: created.ID,
		Status:      &achieved,
	}); !errors.Is(err, auditBoom) {
		t.Fatalf("expected auditBoom, got %v", err)
	}
	env.auditFn = nil

	stillActive, err := svc.GetObjective(ctx, wsID, created.ID)
	if err != nil || stillActive.Status != objective.StatusActive {
		t.Fatalf("expected rollback to retain StatusActive, got %+v (err=%v)", stillActive, err)
	}

	updated, err := svc.UpdateObjective(ctx, objective.UpdateObjectiveInput{
		WorkspaceID:     wsID,
		ObjectiveID:     created.ID,
		Status:          &achieved,
		SuccessCriteria: []string{"verified in staging"},
	})
	if err != nil || updated.Status != objective.StatusAchieved || len(updated.SuccessCriteria) != 1 {
		t.Fatalf("UpdateObjective failed: %+v, err=%v", updated, err)
	}

	listed, err := svc.ListObjectives(ctx, objective.ListObjectivesFilter{
		WorkspaceID: wsID,
		ProjectID:   projID,
		Status:      &achieved,
	})
	if err != nil || len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("ListObjectives failed: %+v, err=%v", listed, err)
	}

	if err := svc.DeleteObjective(ctx, wsID, created.ID); err != nil {
		t.Fatalf("DeleteObjective failed: %v", err)
	}
	if _, err := svc.GetObjective(ctx, wsID, created.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("expected deleted objective to return ErrNotFound, got %v", err)
	}
}
