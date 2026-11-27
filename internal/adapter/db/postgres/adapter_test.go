// Package postgres_test verifies offline lazy adapter construction, SQLC scope
// resolution guards, and optional live PostgreSQL integration parity when
// FLOWSK_TEST_POSTGRES_DSN is provided in the test environment.
package postgres_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/adapter/db"
	"github.com/mindmain/flowsk/internal/adapter/db/postgres"
	"github.com/mindmain/flowsk/internal/adapter/db/sqlite"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/objective"
	"github.com/mindmain/flowsk/internal/domain/project"
	"github.com/mindmain/flowsk/internal/domain/task"
	"github.com/mindmain/flowsk/internal/domain/tx"
	"github.com/mindmain/flowsk/internal/domain/workspace"
)

// fakeScope is a lightweight tx.Scope test double for verifying cross-dialect
// rejection in postgres.ResolveScopeQuerier.
type fakeScope struct {
	handle tx.DialectTxHandle
}

func (f fakeScope) Audit() tx.AuditRecorder       { return nil }
func (f fakeScope) IsAuditEnabled() bool          { return false }
func (f fakeScope) TxHandle() tx.DialectTxHandle { return f.handle }

// TestNewAdapterOfflineAndScopeBridge verifies that NewAdapter validates DSN syntax
// lazily without requiring a live PostgreSQL server at construction time, wires
// non-nil domain repositories, and rejects incompatible transaction handles.
func TestNewAdapterOfflineAndScopeBridge(t *testing.T) {
	t.Parallel()

	if _, err := postgres.NewAdapter(postgres.Config{}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("NewAdapter(empty) = %v, want errs.ErrValidation", err)
	}

	adapter, err := postgres.NewAdapter(postgres.Config{
		DSN:             "postgres://flowsk:flowsk@127.0.0.1:5432/flowsk?sslmode=disable",
		MaxConns:        8,
		MinConns:        0,
		MaxConnLifetime: 30 * time.Minute,
		ConnectTimeout:  2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewAdapter(valid offline DSN): %v", err)
	}
	defer func() { _ = adapter.Close() }()

	if adapter.Dialect() != db.DialectPostgres {
		t.Fatalf("Dialect() = %q, want %q", adapter.Dialect(), db.DialectPostgres)
	}
	if adapter.TxManager() == nil ||
		adapter.WorkspaceRepository() == nil ||
		adapter.ProjectRepository() == nil ||
		adapter.ObjectiveRepository() == nil ||
		adapter.TaskRepository() == nil ||
		adapter.AuditRepository() == nil ||
		adapter.Migrations() == nil {
		t.Fatal("expected all DatabaseAdapter providers on postgres.Adapter to return non-nil implementations")
	}

	// Cross-dialect SQLite handle passed into Postgres ResolveScopeQuerier must fail with ErrPreconditionFailed.
	if _, err := postgres.ResolveScopeQuerier(nil, fakeScope{handle: sqlite.TxHandle{}}); !errors.Is(err, errs.ErrPreconditionFailed) {
		t.Fatalf("ResolveScopeQuerier(sqlite handle) = %v, want errs.ErrPreconditionFailed", err)
	}
	if _, err := postgres.ResolveScopeQuerier(nil, nil); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("ResolveScopeQuerier(nil) = %v, want errs.ErrValidation", err)
	}
}

// TestLivePostgresIntegration exercises full Goose migrations, ACID transactions,
// and all 5 concrete PostgreSQL repositories when FLOWSK_TEST_POSTGRES_DSN is set.
func TestLivePostgresIntegration(t *testing.T) {
	dsn := os.Getenv("FLOWSK_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("FLOWSK_TEST_POSTGRES_DSN is unset; skipping live PostgreSQL integration suite")
	}

	ctx := context.Background()
	adapter, err := postgres.NewAdapter(postgres.Config{
		DSN:            dsn,
		MaxConns:       8,
		ConnectTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("postgres.NewAdapter: %v", err)
	}
	defer func() { _ = adapter.Close() }()

	if err := adapter.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if _, err := adapter.Migrations().Up(ctx); err != nil {
		t.Fatalf("Migrations().Up: %v", err)
	}

	slugSuffix := uuid.NewString()[:8]
	ws := workspace.Workspace{
		ID:                     uuid.New(),
		Slug:                   "pg-ws-" + slugSuffix,
		Name:                   "Postgres Workspace",
		Description:            "Live Postgres verification",
		DefaultLeaseTTLSeconds: 600,
		Labels:                 map[string]string{"engine": "postgres"},
	}
	actorID := uuid.New()
	proj := project.Project{
		ID:          uuid.New(),
		WorkspaceID: ws.ID,
		Slug:        "pg-proj-" + slugSuffix,
		KeyPrefix:   "PG" + slugSuffix[:3],
		Name:        "Postgres Project",
		Status:      project.StatusActive,
		Tags:        []string{"pgx"},
		CreatedBy:   actorID,
	}
	obj := objective.Objective{
		ID:              uuid.New(),
		WorkspaceID:     ws.ID,
		ProjectID:       proj.ID,
		Slug:            "pg-goal",
		Title:           "Postgres Parity",
		SuccessCriteria: []string{"All repos green"},
		Status:          objective.StatusActive,
		PriorityRank:    1,
		CreatedBy:       actorID,
	}
	tsk := task.Task{
		ID:          uuid.New(),
		WorkspaceID: ws.ID,
		ProjectID:   proj.ID,
		ObjectiveID: &obj.ID,
		Kind:        task.KindChore,
		Title:       "Verify pgx sqlc queries",
		Status:      task.StatusReady,
		Priority:    task.PriorityCritical,
		Tags:        []string{"postgres"},
		CreatedBy:   actorID,
	}

	if err := adapter.TxManager().WithTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		if err := adapter.WorkspaceRepository().WithScope(scope).Create(txCtx, ws); err != nil {
			return err
		}
		if err := adapter.WorkspaceRepository().WithScope(scope).UpsertMember(txCtx, workspace.WorkspaceMember{
			WorkspaceID: ws.ID,
			ActorID:     actorID,
			Kind:        authctx.ActorKindHuman,
			Handle:      "pg-operator",
			DisplayName: "PG Operator",
			Role:        workspace.MemberRoleOwner,
		}); err != nil {
			return err
		}
		if err := adapter.ProjectRepository().WithScope(scope).Create(txCtx, proj); err != nil {
			return err
		}
		if err := adapter.ObjectiveRepository().WithScope(scope).Create(txCtx, obj); err != nil {
			return err
		}
		if err := adapter.TaskRepository().WithScope(scope).Create(txCtx, tsk); err != nil {
			return err
		}
		return scope.Audit().Record(txCtx, tx.AuditEntry{
			WorkspaceID:  ws.ID,
			ActorID:      actorID,
			Action:       tx.AuditActionCreate,
			ResourceType: "task",
			ResourceID:   tsk.ID.String(),
			NewValues:    []byte(`{"engine":"postgres"}`),
		})
	}); err != nil {
		t.Fatalf("live postgres transaction failed: %v", err)
	}

	gotTask, err := adapter.TaskRepository().GetByID(ctx, ws.ID, tsk.ID)
	if err != nil || gotTask.Kind != task.KindChore || gotTask.Key == "" {
		t.Fatalf("postgres GetByID task mismatch: %+v (err=%v)", gotTask, err)
	}

	// Verify multi-workspace actor enrollment inside a single pgx.Tx never aborts with SQLSTATE 25P02.
	ws2 := workspace.Workspace{
		ID:   uuid.New(),
		Slug: "pg-ws2-" + slugSuffix,
		Name: "Postgres Second Workspace",
	}
	proj2 := project.Project{
		ID:          uuid.New(),
		WorkspaceID: ws2.ID,
		Slug:        "pg-proj2-" + slugSuffix,
		KeyPrefix:   "P2" + slugSuffix[:3],
		Name:        "Second Project",
		Status:      project.StatusActive,
		CreatedBy:   actorID,
	}
	if err := adapter.TxManager().WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		if err := adapter.WorkspaceRepository().WithScope(scope).Create(txCtx, ws2); err != nil {
			return err
		}
		if err := adapter.ProjectRepository().WithScope(scope).Create(txCtx, proj2); err != nil {
			return err
		}
		return adapter.WorkspaceRepository().WithScope(scope).UpsertMember(txCtx, workspace.WorkspaceMember{
			WorkspaceID: ws2.ID,
			ActorID:     actorID,
			Kind:        authctx.ActorKindHuman,
			Handle:      "pg-operator",
			DisplayName: "PG Operator",
			Role:        workspace.MemberRoleOwner,
		})
	}); err != nil {
		t.Fatalf("multi-workspace actor transaction failed (25P02 regression): %v", err)
	}
}
