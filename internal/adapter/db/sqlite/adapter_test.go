// Package sqlite_test verifies the pure-Go SQLite persistence adapter, automatic
// parent directory creation, Goose migration lifecycle, ACID transaction manager
// with atomic audit fail-fast semantics, all 5 domain repository contracts,
// lossless round-tripping, and multi-goroutine race-free lease coordination.
package sqlite_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/adapter/db"
	"github.com/mindmain/flowsk/internal/adapter/db/sqlite"
	"github.com/mindmain/flowsk/internal/adapter/db/sqlite/sqlcgen"
	"github.com/mindmain/flowsk/internal/domain/audit"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/objective"
	"github.com/mindmain/flowsk/internal/domain/project"
	"github.com/mindmain/flowsk/internal/domain/task"
	"github.com/mindmain/flowsk/internal/domain/tx"
	"github.com/mindmain/flowsk/internal/domain/workspace"
)

// openMigratedSQLiteAdapter opens a fresh WAL-backed SQLite database in a nested
// temporary directory, runs Goose migrations up to latest, and registers cleanup.
func openMigratedSQLiteAdapter(t *testing.T) *sqlite.Adapter {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "nested", "subdir", "flowsk.db")
	adapter, err := sqlite.NewAdapter(sqlite.Config{
		FilePath:               dbPath,
		EnableWAL:              true,
		ForeignKeys:            true,
		BusyTimeout:            10 * time.Second,
		Synchronous:            sqlite.SynchronousNormal,
		WALAutoCheckpointPages: 1000,
		MaxReadOpenConns:       4,
	})
	if err != nil {
		t.Fatalf("sqlite.NewAdapter: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := adapter.Close(); closeErr != nil {
			t.Errorf("adapter.Close: %v", closeErr)
		}
	})

	if _, statErr := os.Stat(filepath.Dir(dbPath)); statErr != nil {
		t.Fatalf("expected parent directory %q to be auto-created: %v", filepath.Dir(dbPath), statErr)
	}

	ctx := context.Background()
	if err := adapter.Ping(ctx); err != nil {
		t.Fatalf("adapter.Ping: %v", err)
	}
	if _, err := adapter.Migrations().Up(ctx); err != nil {
		t.Fatalf("adapter.Migrations().Up: %v", err)
	}
	return adapter
}

// TestAdapterLifecycleAndMigrations verifies configuration validation, nested parent
// directory creation, Ping, Goose Up -> Status -> Down -> Up reversibility, and Close.
func TestAdapterLifecycleAndMigrations(t *testing.T) {
	t.Parallel()

	if _, err := sqlite.NewAdapter(sqlite.Config{}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("NewAdapter(empty) error = %v, want errs.ErrValidation", err)
	}

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "deep", "tree", "flowsk.db")
	adapter, err := sqlite.NewAdapter(sqlite.Config{
		FilePath:    dbPath,
		EnableWAL:   true,
		ForeignKeys: true,
		BusyTimeout: 5 * time.Second,
		Synchronous: sqlite.SynchronousNormal,
	})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	defer func() { _ = adapter.Close() }()

	if adapter.Dialect() != db.DialectSQLite {
		t.Fatalf("Dialect() = %q, want %q", adapter.Dialect(), db.DialectSQLite)
	}
	if err := adapter.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	verUp, err := adapter.Migrations().Up(ctx)
	if err != nil {
		t.Fatalf("Migrations().Up (first): %v", err)
	}
	if verUp <= 0 {
		t.Fatalf("expected positive migration version after Up, got %d", verUp)
	}

	statuses, err := adapter.Migrations().Status(ctx)
	if err != nil {
		t.Fatalf("Migrations().Status: %v", err)
	}
	if len(statuses) == 0 || !statuses[len(statuses)-1].IsApplied || statuses[len(statuses)-1].Version != verUp {
		t.Fatalf("unexpected migration statuses after Up: %+v", statuses)
	}

	for range len(statuses) {
		if _, err := adapter.Migrations().Down(ctx); err != nil {
			t.Fatalf("Migrations().Down: %v", err)
		}
	}

	statusesAfterDown, err := adapter.Migrations().Status(ctx)
	if err != nil {
		t.Fatalf("Migrations().Status after Down: %v", err)
	}
	if len(statusesAfterDown) == 0 || statusesAfterDown[0].IsApplied || statusesAfterDown[len(statusesAfterDown)-1].IsApplied {
		t.Fatalf("expected unapplied status after rolling back all migrations: %+v", statusesAfterDown)
	}

	verReUp, err := adapter.Migrations().Up(ctx)
	if err != nil {
		t.Fatalf("Migrations().Up (re-apply): %v", err)
	}
	if verReUp != verUp {
		t.Fatalf("re-applied version = %d, want %d", verReUp, verUp)
	}
}

// TestTransactionsAndAuditFailFast verifies ACID commit, rollback on error, panic
// recovery + rollback, WithNoAuditTransaction rules, and atomic audit fail-fast.
func TestTransactionsAndAuditFailFast(t *testing.T) {
	t.Parallel()

	adapter := openMigratedSQLiteAdapter(t)
	ctx := context.Background()

	wsRepo := adapter.WorkspaceRepository()
	auditRepo := adapter.AuditRepository()
	txMgr := adapter.TxManager()

	actorID := uuid.New()

	// 1. Callback returning an error after writing business + audit rows rolls back BOTH.
	abortedWS := workspace.Workspace{
		ID:          uuid.New(),
		Slug:        "aborted-ws",
		Name:        "Aborted Workspace",
		Description: "Should never commit",
	}
	abortedAuditID := uuid.New()
	sentinelErr := errors.New("simulated domain failure")

	err := txMgr.WithTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		if !scope.IsAuditEnabled() {
			t.Fatal("expected IsAuditEnabled() == true inside WithTransaction")
		}
		if createErr := wsRepo.WithScope(scope).Create(txCtx, abortedWS); createErr != nil {
			return createErr
		}
		if recErr := scope.Audit().Record(txCtx, tx.AuditEntry{
			ID:           abortedAuditID,
			WorkspaceID:  abortedWS.ID,
			ActorID:      actorID,
			Action:       tx.AuditActionCreate,
			ResourceType: "workspace",
			ResourceID:   abortedWS.ID.String(),
			NewValues:    []byte(`{"slug":"aborted-ws"}`),
		}); recErr != nil {
			return recErr
		}
		return sentinelErr
	})
	if !errors.Is(err, sentinelErr) {
		t.Fatalf("WithTransaction error = %v, want %v", err, sentinelErr)
	}
	if _, getErr := wsRepo.GetByID(ctx, abortedWS.ID); !errors.Is(getErr, errs.ErrNotFound) {
		t.Fatalf("expected aborted workspace to roll back, got err=%v", getErr)
	}
	if _, getErr := auditRepo.GetByID(ctx, abortedWS.ID, abortedAuditID); !errors.Is(getErr, errs.ErrNotFound) {
		t.Fatalf("expected aborted audit record to roll back, got err=%v", getErr)
	}

	// 2. Audit fail-fast: if scope.Audit().Record fails validation, the preceding business write rolls back.
	failFastWS := workspace.Workspace{
		ID:   uuid.New(),
		Slug: "failfast-ws",
		Name: "FailFast Workspace",
	}
	err = txMgr.WithTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		if createErr := wsRepo.WithScope(scope).Create(txCtx, failFastWS); createErr != nil {
			return createErr
		}
		// Invalid AuditEntry (missing ResourceType) must fail validation and abort the tx.
		return scope.Audit().Record(txCtx, tx.AuditEntry{
			WorkspaceID: failFastWS.ID,
			ActorID:     actorID,
			Action:      tx.AuditActionCreate,
			ResourceID:  failFastWS.ID.String(),
		})
	})
	if !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("expected errs.ErrValidation on invalid audit entry, got %v", err)
	}
	if _, getErr := wsRepo.GetByID(ctx, failFastWS.ID); !errors.Is(getErr, errs.ErrNotFound) {
		t.Fatalf("expected fail-fast workspace to roll back, got err=%v", getErr)
	}

	// 3. Panic inside transaction rolls back mutations and re-panics cleanly.
	panicWS := workspace.Workspace{
		ID:   uuid.New(),
		Slug: "panic-ws",
		Name: "Panic Workspace",
	}
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic to propagate after rollback")
			}
		}()
		_ = txMgr.WithTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
			if createErr := wsRepo.WithScope(scope).Create(txCtx, panicWS); createErr != nil {
				return createErr
			}
			panic("unexpected invariant crash")
		})
	}()
	if _, getErr := wsRepo.GetByID(ctx, panicWS.ID); !errors.Is(getErr, errs.ErrNotFound) {
		t.Fatalf("expected panicked workspace to roll back, got err=%v", getErr)
	}

	// 4. WithNoAuditTransaction disables IsAuditEnabled and commits maintenance writes cleanly.
	maintWS := workspace.Workspace{
		ID:   uuid.New(),
		Slug: "maint-ws",
		Name: "Maintenance Workspace",
	}
	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		if scope.IsAuditEnabled() {
			t.Fatal("expected IsAuditEnabled() == false inside WithNoAuditTransaction")
		}
		return wsRepo.WithScope(scope).Create(txCtx, maintWS)
	}); err != nil {
		t.Fatalf("WithNoAuditTransaction: %v", err)
	}
	if got, getErr := wsRepo.GetByID(ctx, maintWS.ID); getErr != nil || got.Slug != "maint-ws" {
		t.Fatalf("expected maintenance workspace to persist, got %+v, err=%v", got, getErr)
	}
}

// TestRepositoriesLosslessRoundTripIsolationAndSoftDelete exercises all 5 domain
// repositories end-to-end: lossless round-tripping, tenant isolation, soft-delete
// slug/key reuse, workflow state seeding, sequence allocation, relations, comments,
// leases, compact queues, project digests, and audit queries.
func TestRepositoriesLosslessRoundTripIsolationAndSoftDelete(t *testing.T) {
	t.Parallel()

	adapter := openMigratedSQLiteAdapter(t)
	ctx := context.Background()

	wsRepo := adapter.WorkspaceRepository()
	projRepo := adapter.ProjectRepository()
	objRepo := adapter.ObjectiveRepository()
	taskRepo := adapter.TaskRepository()
	auditRepo := adapter.AuditRepository()
	txMgr := adapter.TxManager()

	// --- 1. Workspace & Member round-trip + multi-workspace membership + soft-delete slug reuse ---
	wsA := workspace.Workspace{
		ID:                     uuid.New(),
		Slug:                   "acme-eng",
		Name:                   "Acme Engineering",
		Description:            "Primary platform workspace",
		DefaultLeaseTTLSeconds: 900,
		Labels:                 map[string]string{"tier": "enterprise", "region": "eu"},
	}
	wsB := workspace.Workspace{
		ID:          uuid.New(),
		Slug:        "other-tenant",
		Name:        "Other Tenant",
		Description: "Isolated second tenant",
	}
	sharedHumanID := uuid.New()
	agentActorID := uuid.New()

	if err := txMgr.WithTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		wTx := wsRepo.WithScope(scope)
		if err := wTx.Create(txCtx, wsA); err != nil {
			return err
		}
		if err := wTx.Create(txCtx, wsB); err != nil {
			return err
		}
		if err := wTx.UpsertMember(txCtx, workspace.WorkspaceMember{
			WorkspaceID: wsA.ID,
			ActorID:     sharedHumanID,
			Kind:        authctx.ActorKindHuman,
			Handle:      "alice",
			DisplayName: "Alice Maintainer",
			Role:        workspace.MemberRoleOwner,
		}); err != nil {
			return err
		}
		if err := wTx.UpsertMember(txCtx, workspace.WorkspaceMember{
			WorkspaceID: wsA.ID,
			ActorID:     agentActorID,
			Kind:        authctx.ActorKindAgent,
			Handle:      "swe-agent-01",
			DisplayName: "Autonomous Agent 01",
			Role:        workspace.MemberRoleContributor,
		}); err != nil {
			return err
		}
		// Same human ActorID joining wsB must succeed without PK collision.
		if err := wTx.UpsertMember(txCtx, workspace.WorkspaceMember{
			WorkspaceID: wsB.ID,
			ActorID:     sharedHumanID,
			Kind:        authctx.ActorKindHuman,
			Handle:      "alice",
			DisplayName: "Alice in Tenant B",
			Role:        workspace.MemberRoleReader,
		}); err != nil {
			return err
		}
		return scope.Audit().Record(txCtx, tx.AuditEntry{
			WorkspaceID:  wsA.ID,
			ActorID:      sharedHumanID,
			Action:       tx.AuditActionCreate,
			ResourceType: "workspace",
			ResourceID:   wsA.ID.String(),
			NewValues:    []byte(`{"slug":"acme-eng"}`),
		})
	}); err != nil {
		t.Fatalf("seed workspaces & members: %v", err)
	}

	gotWSA, err := wsRepo.GetBySlug(ctx, "acme-eng")
	if err != nil {
		t.Fatalf("wsRepo.GetBySlug: %v", err)
	}
	if gotWSA.Name != wsA.Name || gotWSA.Description != wsA.Description ||
		gotWSA.DefaultLeaseTTLSeconds != 900 || gotWSA.Labels["tier"] != "enterprise" || gotWSA.Labels["region"] != "eu" {
		t.Fatalf("Workspace round-trip mismatch: got %+v", gotWSA)
	}

	// Handle collision with a different ActorID in the same workspace must return ErrConflict.
	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		return wsRepo.WithScope(scope).UpsertMember(txCtx, workspace.WorkspaceMember{
			WorkspaceID: wsA.ID,
			ActorID:     uuid.New(),
			Kind:        authctx.ActorKindHuman,
			Handle:      "alice",
			DisplayName: "Impersonator",
			Role:        workspace.MemberRoleReader,
		})
	}); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("expected errs.ErrConflict on duplicate member handle, got %v", err)
	}

	actorWorkspaces, err := wsRepo.ListForActor(ctx, sharedHumanID)
	if err != nil {
		t.Fatalf("ListForActor: %v", err)
	}
	if len(actorWorkspaces) != 2 || actorWorkspaces[0].Slug != "acme-eng" || actorWorkspaces[1].Slug != "other-tenant" {
		t.Fatalf("ListForActor mismatch: %+v", actorWorkspaces)
	}

	membersA, err := wsRepo.ListMembers(ctx, wsA.ID)
	if err != nil {
		t.Fatalf("ListMembers(wsA): %v", err)
	}
	if len(membersA) != 2 || membersA[0].Handle != "alice" || membersA[0].Role != workspace.MemberRoleOwner ||
		membersA[1].Handle != "swe-agent-01" || membersA[1].Kind != authctx.ActorKindAgent {
		t.Fatalf("ListMembers(wsA) mismatch: %+v", membersA)
	}

	// Soft-delete and slug reuse on Workspace.
	tempWS := workspace.Workspace{ID: uuid.New(), Slug: "ephemeral-ws", Name: "Ephemeral"}
	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		if err := wsRepo.WithScope(scope).Create(txCtx, tempWS); err != nil {
			return err
		}
		return wsRepo.WithScope(scope).SoftDelete(txCtx, tempWS.ID)
	}); err != nil {
		t.Fatalf("create + soft-delete ephemeral workspace: %v", err)
	}
	if _, err := wsRepo.GetBySlug(ctx, "ephemeral-ws"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for soft-deleted workspace, got %v", err)
	}
	reusedWS := workspace.Workspace{ID: uuid.New(), Slug: "ephemeral-ws", Name: "Reborn"}
	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		return wsRepo.WithScope(scope).Create(txCtx, reusedWS)
	}); err != nil {
		t.Fatalf("expected slug reuse after workspace soft-delete to succeed: %v", err)
	}

	// --- 2. Project round-trip, default workflow states, NextTaskSequence & soft-delete slug reuse ---
	unregisteredCreator := uuid.New() // Verifies FK safety even when creator wasn't pre-inserted via UpsertMember!
	projA := project.Project{
		ID:            uuid.New(),
		WorkspaceID:   wsA.ID,
		Slug:          "flowsk-core",
		KeyPrefix:     "FLW",
		Name:          "Flowsk Core",
		Description:   "Core persistence & language engine",
		RemoteRepoURL: "https://github.com/mindmain/flowsk",
		DefaultBranch: "main",
		Status:        project.StatusActive,
		Tags:          []string{"golang", "sqlite"},
		CreatedBy:     unregisteredCreator,
	}
	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		return projRepo.WithScope(scope).Create(txCtx, projA)
	}); err != nil {
		t.Fatalf("projRepo.Create: %v", err)
	}

	gotProj, err := projRepo.GetBySlug(ctx, wsA.ID, "flowsk-core")
	if err != nil {
		t.Fatalf("projRepo.GetBySlug: %v", err)
	}
	if gotProj.RemoteRepoURL != projA.RemoteRepoURL || gotProj.DefaultBranch != "main" ||
		len(gotProj.Tags) != 2 || gotProj.Tags[0] != "golang" || gotProj.CreatedBy != unregisteredCreator {
		t.Fatalf("Project round-trip mismatch: %+v", gotProj)
	}

	// Tenant isolation check: wsB cannot read, update, or soft-delete wsA's project.
	if _, err := projRepo.GetByID(ctx, wsB.ID, projA.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant GetByID = %v, want errs.ErrNotFound", err)
	}
	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		return projRepo.WithScope(scope).SoftDelete(txCtx, wsB.ID, projA.ID)
	}); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant SoftDelete = %v, want errs.ErrNotFound", err)
	}

	// Stub creator must not pollute WorkspaceRepository.ListMembers!
	membersAfterStub, err := wsRepo.ListMembers(ctx, wsA.ID)
	if err != nil || len(membersAfterStub) != 2 {
		t.Fatalf("expected stub FK actor to stay hidden from ListMembers, got %+v (err=%v)", membersAfterStub, err)
	}

	// NextTaskSequence increments monotonically (FLW-1, FLW-2).
	var pfx1, pfx2 string
	var seq1, seq2 int64
	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		var seqErr error
		pfx1, seq1, seqErr = projRepo.WithScope(scope).NextTaskSequence(txCtx, wsA.ID, projA.ID)
		if seqErr != nil {
			return seqErr
		}
		pfx2, seq2, seqErr = projRepo.WithScope(scope).NextTaskSequence(txCtx, wsA.ID, projA.ID)
		return seqErr
	}); err != nil {
		t.Fatalf("NextTaskSequence: %v", err)
	}
	if pfx1 != "FLW" || seq1 != 1 || pfx2 != "FLW" || seq2 != 2 {
		t.Fatalf("NextTaskSequence = (%s-%d, %s-%d), want (FLW-1, FLW-2)", pfx1, seq1, pfx2, seq2)
	}

	// Soft-delete project & reuse slug + key_prefix.
	tempProj := project.Project{
		ID:          uuid.New(),
		WorkspaceID: wsA.ID,
		Slug:        "temp-proj",
		KeyPrefix:   "TMP",
		Name:        "Temporary",
		Status:      project.StatusActive,
	}
	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		pTx := projRepo.WithScope(scope)
		if err := pTx.Create(txCtx, tempProj); err != nil {
			return err
		}
		if err := pTx.SoftDelete(txCtx, wsA.ID, tempProj.ID); err != nil {
			return err
		}
		tempProj.ID = uuid.New()
		return pTx.Create(txCtx, tempProj)
	}); err != nil {
		t.Fatalf("project soft-delete & slug/prefix reuse failed: %v", err)
	}

	// --- 3. Objective round-trip, priority ordering & soft-delete slug reuse ---
	targetDeadline := time.Now().UTC().Add(72 * time.Hour).Truncate(time.Millisecond)
	objA := objective.Objective{
		ID:              uuid.New(),
		WorkspaceID:     wsA.ID,
		ProjectID:       projA.ID,
		Slug:            "m1-storage-engine",
		Title:           "Deliver Dual-Engine Storage",
		Summary:         "Zero-SQLITE_BUSY WAL persistence and Postgres parity",
		SuccessCriteria: []string{"100% repo tests green", "Zero race warnings"},
		Status:          objective.StatusActive,
		PriorityRank:    1,
		TargetDate:      &targetDeadline,
		CreatedBy:       sharedHumanID,
	}
	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		return objRepo.WithScope(scope).Create(txCtx, objA)
	}); err != nil {
		t.Fatalf("objRepo.Create: %v", err)
	}

	// Duplicate slug in same project must return ErrConflict.
	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		dup := objA.Clone()
		dup.ID = uuid.New()
		return objRepo.WithScope(scope).Create(txCtx, dup)
	}); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("duplicate objective slug error = %v, want errs.ErrConflict", err)
	}

	gotObj, err := objRepo.GetBySlug(ctx, wsA.ID, projA.ID, "m1-storage-engine")
	if err != nil {
		t.Fatalf("objRepo.GetBySlug: %v", err)
	}
	if gotObj.Summary != objA.Summary || len(gotObj.SuccessCriteria) != 2 ||
		gotObj.PriorityRank != 1 || gotObj.TargetDate == nil || !gotObj.TargetDate.Equal(targetDeadline) {
		t.Fatalf("Objective round-trip mismatch: %+v", gotObj)
	}

	// Cross-tenant isolation on Objective.
	if _, err := objRepo.GetByID(ctx, wsB.ID, objA.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant objRepo.GetByID = %v, want errs.ErrNotFound", err)
	}

	// Soft-delete objective & reuse slug.
	tempObj := objective.Objective{
		ID:          uuid.New(),
		WorkspaceID: wsA.ID,
		ProjectID:   projA.ID,
		Slug:        "retire-me",
		Title:       "Temporary Goal",
		Status:      objective.StatusDraft,
	}
	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		oTx := objRepo.WithScope(scope)
		if err := oTx.Create(txCtx, tempObj); err != nil {
			return err
		}
		if err := oTx.SoftDelete(txCtx, wsA.ID, tempObj.ID); err != nil {
			return err
		}
		tempObj.ID = uuid.New()
		return oTx.Create(txCtx, tempObj)
	}); err != nil {
		t.Fatalf("objective soft-delete & slug reuse failed: %v", err)
	}

	// --- 4. Task round-trip (KindChore, auto-Key minting, Version optimistic lock, Relations, Comments, Leases, Digest) ---
	epicTask := task.Task{
		ID:                 uuid.New(),
		WorkspaceID:        wsA.ID,
		ProjectID:          projA.ID,
		ObjectiveID:        &objA.ID,
		Key:                "", // Must auto-allocate FLW-3 from project!
		Kind:               task.KindEpic,
		Title:              "Storage Epic",
		Description:        "Parent epic for storage",
		AcceptanceCriteria: []string{"SQLite green", "Postgres green"},
		Status:             task.StatusReady,
		Priority:           task.PriorityCritical,
		Tags:               []string{"db", "storage"},
		Labels:             map[string]string{"component": "adapter"},
		CreatedBy:          sharedHumanID,
	}
	choreBlocker := task.Task{
		ID:                 uuid.New(),
		WorkspaceID:        wsA.ID,
		ProjectID:          projA.ID,
		ObjectiveID:        &objA.ID,
		ParentTaskID:       &epicTask.ID,
		Key:                "", // Must auto-allocate FLW-4!
		Kind:               task.KindChore,
		Title:              "Wire SQLite WAL Pragmas",
		Description:        "Configure busy_timeout and foreign_keys",
		AcceptanceCriteria: []string{"WAL enabled"},
		Status:             task.StatusReady,
		Priority:           task.PriorityHigh,
		AssigneeActorID:    &agentActorID,
		Tags:               []string{"sqlite", "wal"},
		Labels:             map[string]string{"owner": "agent"},
		CreatedBy:          sharedHumanID,
	}
	blockedResearch := task.Task{
		ID:           uuid.New(),
		WorkspaceID:  wsA.ID,
		ProjectID:    projA.ID,
		ObjectiveID:  &objA.ID,
		ParentTaskID: &epicTask.ID,
		Key:          "FLW-100",
		Kind:         task.KindResearch,
		Title:        "Benchmark Multi-Agent Throughput",
		Description:  "Depends on WAL pragmas",
		Status:       task.StatusReady,
		Priority:     task.PriorityMedium,
		Tags:         []string{"bench"},
		CreatedBy:    sharedHumanID,
	}

	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		tTx := taskRepo.WithScope(scope)
		if err := tTx.Create(txCtx, epicTask); err != nil {
			return err
		}
		if err := tTx.Create(txCtx, choreBlocker); err != nil {
			return err
		}
		if err := tTx.Create(txCtx, blockedResearch); err != nil {
			return err
		}
		// Add 'blocks' relation (choreBlocker blocks blockedResearch) and 'subtask_of' relation with Note!
		if err := tTx.CreateRelation(txCtx, task.TaskRelation{
			ID:           uuid.New(),
			WorkspaceID:  wsA.ID,
			SourceTaskID: choreBlocker.ID,
			TargetTaskID: blockedResearch.ID,
			Type:         task.RelationBlocks,
			Note:         "Pragmas must land before running throughput benchmarks",
			CreatedBy:    sharedHumanID,
		}); err != nil {
			return err
		}
		return tTx.CreateRelation(txCtx, task.TaskRelation{
			ID:           uuid.New(),
			WorkspaceID:  wsA.ID,
			SourceTaskID: blockedResearch.ID,
			TargetTaskID: epicTask.ID,
			Type:         task.RelationSubtaskOf,
			Note:         "Child spike under storage epic",
			CreatedBy:    sharedHumanID,
		})
	}); err != nil {
		t.Fatalf("seed tasks & relations: %v", err)
	}

	gotChore, err := taskRepo.GetByID(ctx, wsA.ID, choreBlocker.ID)
	if err != nil {
		t.Fatalf("taskRepo.GetByID(choreBlocker): %v", err)
	}
	if gotChore.Key != "FLW-4" || gotChore.Kind != task.KindChore || gotChore.Priority != task.PriorityHigh ||
		gotChore.Status != task.StatusReady || gotChore.Version != 1 ||
		len(gotChore.AcceptanceCriteria) != 1 || gotChore.Labels["owner"] != "agent" ||
		len(gotChore.Tags) != 2 {
		t.Fatalf("Task KindChore round-trip mismatch: %+v", gotChore)
	}

	gotBlocked, err := taskRepo.GetByKey(ctx, wsA.ID, "FLW-100")
	if err != nil {
		t.Fatalf("taskRepo.GetByKey(FLW-100): %v", err)
	}
	if gotBlocked.Kind != task.KindResearch || gotBlocked.UnresolvedBlockerCount != 1 {
		t.Fatalf("expected FLW-100 KindResearch with UnresolvedBlockerCount=1, got %+v", gotBlocked)
	}

	// Verify TaskRelation lossless round-tripping (including RelationSubtaskOf and Note!).
	rels, err := taskRepo.ListRelationsForTask(ctx, wsA.ID, blockedResearch.ID)
	if err != nil {
		t.Fatalf("ListRelationsForTask: %v", err)
	}
	if len(rels) != 2 {
		t.Fatalf("len(ListRelationsForTask) = %d, want 2", len(rels))
	}
	var sawBlocks, sawSubtask bool
	for _, r := range rels {
		if r.Type == task.RelationBlocks && r.Note == "Pragmas must land before running throughput benchmarks" {
			sawBlocks = true
		}
		if r.Type == task.RelationSubtaskOf && r.Note == "Child spike under storage epic" {
			sawSubtask = true
		}
	}
	if !sawBlocks || !sawSubtask {
		t.Fatalf("TaskRelation lossless round-trip failed: %+v", rels)
	}

	// Verify TaskComment lossless round-tripping across all CommentKinds.
	for _, kind := range []task.CommentKind{
		task.CommentKindProgress,
		task.CommentKindBlocker,
		task.CommentKindDecision,
		task.CommentKindHandoff,
		task.CommentKindNote,
	} {
		if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
			return taskRepo.WithScope(scope).CreateComment(txCtx, task.TaskComment{
				ID:            uuid.New(),
				WorkspaceID:   wsA.ID,
				TaskID:        choreBlocker.ID,
				AuthorActorID: agentActorID,
				AuthorKind:    authctx.ActorKindAgent,
				AuthorHandle:  "swe-agent-01",
				Kind:          kind,
				Body:          "Entry for " + string(kind),
			})
		}); err != nil {
			t.Fatalf("CreateComment(%s): %v", kind, err)
		}
	}
	comments, err := taskRepo.ListComments(ctx, wsA.ID, choreBlocker.ID, 10)
	if err != nil || len(comments) != 5 {
		t.Fatalf("ListComments = %+v (err=%v), want 5", comments, err)
	}
	if comments[2].Kind != task.CommentKindDecision || comments[2].AuthorHandle != "swe-agent-01" ||
		comments[2].AuthorKind != authctx.ActorKindAgent {
		t.Fatalf("TaskComment round-trip mismatch: %+v", comments[2])
	}

	// Verify Lease acquisition, ActiveLease population, Digest, and Optimistic Version Locking.
	now := time.Now().UTC().Truncate(time.Millisecond)
	lease := task.ClaimLease{
		TaskID:         choreBlocker.ID,
		WorkspaceID:    wsA.ID,
		LeaseToken:     "tok-001",
		HolderActorID:  agentActorID,
		HolderKind:     authctx.ActorKindAgent,
		HolderHandle:   "swe-agent-01",
		AgentSessionID: "sess-xyz",
		AcquiredAt:     now,
		HeartbeatAt:    now,
		ExpiresAt:      now.Add(10 * time.Minute),
	}
	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		return taskRepo.WithScope(scope).UpsertLease(txCtx, lease)
	}); err != nil {
		t.Fatalf("UpsertLease: %v", err)
	}

	gotLease, err := taskRepo.GetLease(ctx, wsA.ID, choreBlocker.ID)
	if err != nil {
		t.Fatalf("GetLease: %v", err)
	}
	if gotLease.LeaseToken != "tok-001" || gotLease.AgentSessionID != "sess-xyz" ||
		gotLease.HolderActorID != agentActorID || gotLease.HolderHandle != "swe-agent-01" {
		t.Fatalf("ClaimLease round-trip mismatch: %+v", gotLease)
	}

	// Cross-tenant isolation on Task & Lease.
	if _, err := taskRepo.GetByID(ctx, wsB.ID, choreBlocker.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant taskRepo.GetByID = %v, want errs.ErrNotFound", err)
	}
	if _, err := taskRepo.GetLease(ctx, wsB.ID, choreBlocker.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant taskRepo.GetLease = %v, want errs.ErrNotFound", err)
	}

	// Compact queue & ProjectDigest accuracy before completing blocker.
	queue, err := taskRepo.QueryCompactQueue(ctx, task.AgentWorkQueueFilter{
		WorkspaceID:   wsA.ID,
		ProjectID:     projA.ID,
		Statuses:      []task.Status{task.StatusReady},
		OnlyUnblocked: true,
		OnlyUnclaimed: true,
		Limit:         10,
	}, now)
	if err != nil {
		t.Fatalf("QueryCompactQueue: %v", err)
	}
	// Only epicTask (FLW-3) is Ready + Unblocked + Unclaimed!
	if len(queue) != 1 || queue[0].ID != epicTask.ID || queue[0].ObjectiveSlug != "m1-storage-engine" {
		t.Fatalf("QueryCompactQueue unexpected result: %+v", queue)
	}

	digest, err := taskRepo.BuildProjectDigest(ctx, wsA.ID, projA.ID, task.DigestOptions{
		MaxReadyTasks:   10,
		MaxBlockedTasks: 10,
	}, now)
	if err != nil {
		t.Fatalf("BuildProjectDigest: %v", err)
	}
	if digest.ProjectSlug != "flowsk-core" || len(digest.ActiveObjectives) != 1 ||
		digest.ActiveObjectives[0].OpenTaskCount != 3 ||
		len(digest.ReadyQueue) != 1 || len(digest.ActiveLeases) != 1 || len(digest.BlockedTasks) != 1 {
		t.Fatalf("ProjectDigest mismatch: %+v", digest)
	}

	// Optimistic locking: wrong ExpectedVersion returns ErrConflict; matching ExpectedVersion increments Version -> 2 and transitions to Done!
	staleVer := int64(99)
	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		return taskRepo.WithScope(scope).Update(txCtx, gotChore, &staleVer)
	}); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("expected ErrConflict on stale ExpectedVersion, got %v", err)
	}

	expectedV1 := int64(1)
	completedAt := time.Now().UTC().Truncate(time.Millisecond)
	gotChore.Status = task.StatusDone
	gotChore.CompletedAt = &completedAt
	var auditRecID uuid.UUID
	if err := txMgr.WithTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		tTx := taskRepo.WithScope(scope)
		if err := tTx.Update(txCtx, gotChore, &expectedV1); err != nil {
			return err
		}
		if err := tTx.DeleteLease(txCtx, wsA.ID, gotChore.ID); err != nil {
			return err
		}
		auditRecID = uuid.New()
		return scope.Audit().Record(txCtx, tx.AuditEntry{
			ID:           auditRecID,
			WorkspaceID:  wsA.ID,
			ActorID:      agentActorID,
			Action:       tx.AuditActionTransition,
			ResourceType: "task",
			ResourceID:   gotChore.Key,
			OldValues:    []byte(`{"status":"ready"}`),
			NewValues:    []byte(`{"status":"done"}`),
		})
	}); err != nil {
		t.Fatalf("Update(Done) + DeleteLease + Audit: %v", err)
	}

	// Now choreBlocker is Version=2, Status=Done, and blockedResearch has UnresolvedBlockerCount == 0!
	afterDone, err := taskRepo.GetByID(ctx, wsA.ID, gotChore.ID)
	if err != nil {
		t.Fatalf("GetByID after Done: %v", err)
	}
	if afterDone.Status != task.StatusDone || afterDone.Version != 2 || afterDone.ActiveLease != nil {
		t.Fatalf("expected Done v2 with nil ActiveLease, got %+v", afterDone)
	}
	unblockedNow, err := taskRepo.GetByID(ctx, wsA.ID, blockedResearch.ID)
	if err != nil || unblockedNow.UnresolvedBlockerCount != 0 {
		t.Fatalf("expected FLW-100 UnresolvedBlockerCount=0 after blocker completed, got %+v (err=%v)", unblockedNow, err)
	}

	// Task soft-delete & Key reuse.
	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		tTx := taskRepo.WithScope(scope)
		if err := tTx.SoftDelete(txCtx, wsA.ID, blockedResearch.ID); err != nil {
			return err
		}
		reborn := blockedResearch.Clone()
		reborn.ID = uuid.New()
		reborn.ParentTaskID = nil
		return tTx.Create(txCtx, reborn)
	}); err != nil {
		t.Fatalf("task soft-delete & key FLW-100 reuse failed: %v", err)
	}

	// --- 5. AuditRepository GetByID, Query filtering & tenant isolation ---
	gotAudit, err := auditRepo.GetByID(ctx, wsA.ID, auditRecID)
	if err != nil {
		t.Fatalf("auditRepo.GetByID: %v", err)
	}
	if gotAudit.Action != tx.AuditActionTransition || gotAudit.ResourceType != "task" ||
		gotAudit.ResourceID != gotChore.Key || gotAudit.ActorID != agentActorID ||
		gotAudit.ActorHandle != "swe-agent-01" || gotAudit.ActorKind != authctx.ActorKindAgent ||
		!bytes.Equal(gotAudit.OldValues, []byte(`{"status":"ready"}`)) ||
		!bytes.Equal(gotAudit.NewValues, []byte(`{"status":"done"}`)) {
		t.Fatalf("Audit Record round-trip mismatch: %+v", gotAudit)
	}
	if _, err := auditRepo.GetByID(ctx, wsB.ID, auditRecID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant auditRepo.GetByID = %v, want errs.ErrNotFound", err)
	}

	queriedAudits, err := auditRepo.Query(ctx, audit.QueryFilter{
		WorkspaceID:  wsA.ID,
		ActorID:      agentActorID,
		Action:       tx.AuditActionTransition,
		ResourceType: "task",
		Limit:        10,
	})
	if err != nil || len(queriedAudits) != 1 || queriedAudits[0].ID != auditRecID {
		t.Fatalf("auditRepo.Query mismatch: %+v (err=%v)", queriedAudits, err)
	}

	// Verify DeleteExpiredLeases reaps expired leases cleanly without touching unexpired leases.
	expiredLease := task.ClaimLease{
		TaskID:        epicTask.ID,
		WorkspaceID:   wsA.ID,
		LeaseToken:    "expired-tok",
		HolderActorID: agentActorID,
		HolderKind:    authctx.ActorKindAgent,
		HolderHandle:  "swe-agent-01",
		AcquiredAt:    now.Add(-20 * time.Minute),
		HeartbeatAt:   now.Add(-20 * time.Minute),
		ExpiresAt:     now.Add(-5 * time.Minute),
	}
	var reapedCount int64
	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		tTx := taskRepo.WithScope(scope)
		if err := tTx.UpsertLease(txCtx, expiredLease); err != nil {
			return err
		}
		var reapErr error
		reapedCount, reapErr = tTx.DeleteExpiredLeases(txCtx, wsA.ID, now)
		return reapErr
	}); err != nil {
		t.Fatalf("DeleteExpiredLeases: %v", err)
	}
	if reapedCount != 1 {
		t.Fatalf("reapedCount = %d, want 1", reapedCount)
	}
	if _, err := taskRepo.GetLease(ctx, wsA.ID, epicTask.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("expected reaped lease to be deleted, got %v", err)
	}
}

// TestConcurrentMultiAgentStressSQLiteWAL spawns 20 concurrent agent goroutines
// competing to claim a shared pool of tasks and read digests simultaneously under
// -race, verifying zero SQLITE_BUSY errors and zero double-claims.
func TestConcurrentMultiAgentStressSQLiteWAL(t *testing.T) {
	t.Parallel()

	adapter := openMigratedSQLiteAdapter(t)
	ctx := context.Background()

	wsRepo := adapter.WorkspaceRepository()
	projRepo := adapter.ProjectRepository()
	taskRepo := adapter.TaskRepository()
	txMgr := adapter.TxManager()

	ws := workspace.Workspace{ID: uuid.New(), Slug: "swarm-ws", Name: "Swarm Workspace"}
	proj := project.Project{
		ID:          uuid.New(),
		WorkspaceID: ws.ID,
		Slug:        "swarm-proj",
		KeyPrefix:   "SWM",
		Name:        "Swarm Project",
		Status:      project.StatusActive,
	}

	const numTasks = 10
	const numAgents = 20

	taskIDs := make([]uuid.UUID, numTasks)
	agentIDs := make([]uuid.UUID, numAgents)

	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		if err := wsRepo.WithScope(scope).Create(txCtx, ws); err != nil {
			return err
		}
		for i := range numAgents {
			agentIDs[i] = uuid.New()
			if err := wsRepo.WithScope(scope).UpsertMember(txCtx, workspace.WorkspaceMember{
				WorkspaceID: ws.ID,
				ActorID:     agentIDs[i],
				Kind:        authctx.ActorKindAgent,
				Handle:      fmt.Sprintf("worker-%02d", i),
				DisplayName: fmt.Sprintf("Worker %02d", i),
				Role:        workspace.MemberRoleContributor,
			}); err != nil {
				return err
			}
		}
		if err := projRepo.WithScope(scope).Create(txCtx, proj); err != nil {
			return err
		}
		for i := range numTasks {
			taskIDs[i] = uuid.New()
			if err := taskRepo.WithScope(scope).Create(txCtx, task.Task{
				ID:          taskIDs[i],
				WorkspaceID: ws.ID,
				ProjectID:   proj.ID,
				Kind:        task.KindFeature,
				Title:       fmt.Sprintf("Concurrent Work Item %d", i),
				Status:      task.StatusReady,
				Priority:    task.PriorityHigh,
				CreatedBy:   agentIDs[0],
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed concurrent stress fixture: %v", err)
	}

	// Track which agent successfully claimed each task to prove zero double-claims.
	var claimMu sync.Mutex
	claimedWinner := make(map[uuid.UUID]uuid.UUID, numTasks)
	var unexpectedErrors atomic.Int32

	var wg sync.WaitGroup
	startGate := make(chan struct{})

	for a := range numAgents {
		agentID := agentIDs[a]
		handle := fmt.Sprintf("worker-%02d", a)
		wg.Add(1)
		go func(workerIdx int) {
			defer wg.Done()
			<-startGate

			// Interleave concurrent reader pool digest queries while writers run transactions!
			if _, digestErr := taskRepo.BuildProjectDigest(ctx, ws.ID, proj.ID, task.DigestOptions{MaxReadyTasks: 5}, time.Now().UTC()); digestErr != nil {
				t.Errorf("worker %d BuildProjectDigest failed: %v", workerIdx, digestErr)
				unexpectedErrors.Add(1)
				return
			}

			for offset := range numTasks {
				targetTaskID := taskIDs[(workerIdx+offset)%numTasks]
				claimed := false

				txErr := txMgr.WithTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
					tTx := taskRepo.WithScope(scope)
					now := time.Now().UTC()
					current, getErr := tTx.GetByID(txCtx, ws.ID, targetTaskID)
					if getErr != nil {
						return getErr
					}
					if current.ActiveLease != nil && !current.ActiveLease.IsExpiredAt(now) {
						return errs.New(errs.ErrLeaseHeld, "stress.claim", "already claimed")
					}
					lease := task.ClaimLease{
						TaskID:        targetTaskID,
						WorkspaceID:   ws.ID,
						LeaseToken:    uuid.NewString(),
						HolderActorID: agentID,
						HolderKind:    authctx.ActorKindAgent,
						HolderHandle:  handle,
						AcquiredAt:    now,
						HeartbeatAt:   now,
						ExpiresAt:     now.Add(5 * time.Minute),
					}
					if upsertErr := tTx.UpsertLease(txCtx, lease); upsertErr != nil {
						return upsertErr
					}
					return scope.Audit().Record(txCtx, tx.AuditEntry{
						WorkspaceID:  ws.ID,
						ActorID:      agentID,
						Action:       tx.AuditActionClaim,
						ResourceType: "task",
						ResourceID:   targetTaskID.String(),
						NewValues:    []byte(`{"claimed":true}`),
					})
				})

				if txErr == nil {
					claimed = true
				} else if !errors.Is(txErr, errs.ErrLeaseHeld) {
					t.Errorf("worker %d unexpected transaction error: %v", workerIdx, txErr)
					unexpectedErrors.Add(1)
					return
				}

				if claimed {
					claimMu.Lock()
					if prev, dup := claimedWinner[targetTaskID]; dup {
						t.Errorf("DOUBLE CLAIM DETECTED on task %s: %s and %s", targetTaskID, prev, agentID)
						unexpectedErrors.Add(1)
					} else {
						claimedWinner[targetTaskID] = agentID
					}
					claimMu.Unlock()
					break
				}
			}
		}(a)
	}

	close(startGate)
	wg.Wait()

	if unexpectedErrors.Load() != 0 {
		t.Fatalf("stress test recorded %d unexpected errors", unexpectedErrors.Load())
	}
	if len(claimedWinner) != numTasks {
		t.Fatalf("claimed tasks count = %d, want all %d tasks claimed exactly once", len(claimedWinner), numTasks)
	}
}

// TestRepositoryStabilizationInvariants verifies FLOWSK-14 persistence invariants:
//   - Clearing nullable Task (ObjectiveID, ParentTaskID, AssigneeActorID) and Objective (TargetDate)
//     clears both domain fields and underlying SQL columns while preserving EstimatePoints;
//   - Cross-project and cross-tenant ObjectiveID validation on TaskRepository.Create and Update;
//   - ProjectRepository.SoftDelete cascading soft-delete to tasks/objectives and KeyPrefix reuse;
//   - TaskRepository.Create O(1) sequence/identifier conflict reallocation and sidecar cleanup;
//   - BoardMetadataRepository & BoardMetadataService custom workflow states ("archived" mapping,
//     single-default invariant) and project labels CRUD;
//   - AuditRepository.Query SQL Offset/Limit pushdown and filtered pagination.
func TestRepositoryStabilizationInvariants(t *testing.T) {
	t.Parallel()

	adapter := openMigratedSQLiteAdapter(t)
	ctx := context.Background()

	wsRepo := adapter.WorkspaceRepository()
	projRepo := adapter.ProjectRepository()
	objRepo := adapter.ObjectiveRepository()
	taskRepo := adapter.TaskRepository()
	auditRepo := adapter.AuditRepository()
	txMgr := adapter.TxManager()
	readQ := adapter.SQLCBridge().ReadQueries()

	wsA := workspace.Workspace{ID: uuid.New(), Slug: "stab-ws-a", Name: "Stabilization A"}
	wsB := workspace.Workspace{ID: uuid.New(), Slug: "stab-ws-b", Name: "Stabilization B"}
	actorID := uuid.New()
	assigneeID := uuid.New()

	proj1 := project.Project{ID: uuid.New(), WorkspaceID: wsA.ID, Slug: "proj-one", KeyPrefix: "ONE", Name: "Project One", Status: project.StatusActive, CreatedBy: actorID}
	proj2 := project.Project{ID: uuid.New(), WorkspaceID: wsA.ID, Slug: "proj-two", KeyPrefix: "TWO", Name: "Project Two", Status: project.StatusActive, CreatedBy: actorID}
	projB := project.Project{ID: uuid.New(), WorkspaceID: wsB.ID, Slug: "proj-b", KeyPrefix: "PRB", Name: "Project B", Status: project.StatusActive, CreatedBy: actorID}

	deadline := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Millisecond)
	obj1 := objective.Objective{
		ID:          uuid.New(),
		WorkspaceID: wsA.ID,
		ProjectID:   proj1.ID,
		Slug:        "obj-one",
		Title:       "Objective One",
		Status:      objective.StatusActive,
		TargetDate:  &deadline,
		CreatedBy:   actorID,
	}
	objB := objective.Objective{
		ID:          uuid.New(),
		WorkspaceID: wsB.ID,
		ProjectID:   projB.ID,
		Slug:        "obj-b",
		Title:       "Objective B",
		Status:      objective.StatusActive,
		CreatedBy:   actorID,
	}

	parentTask := task.Task{
		ID:          uuid.New(),
		WorkspaceID: wsA.ID,
		ProjectID:   proj1.ID,
		Kind:        task.KindEpic,
		Title:       "Parent Epic",
		Status:      task.StatusReady,
		Priority:    task.PriorityHigh,
		CreatedBy:   actorID,
	}
	childTask := task.Task{
		ID:              uuid.New(),
		WorkspaceID:     wsA.ID,
		ProjectID:       proj1.ID,
		ObjectiveID:     &obj1.ID,
		ParentTaskID:    &parentTask.ID,
		AssigneeActorID: &assigneeID,
		Kind:            task.KindFeature,
		Title:           "Child Feature",
		Status:          task.StatusReady,
		Priority:        task.PriorityCritical,
		CreatedBy:       actorID,
	}

	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		if err := wsRepo.WithScope(scope).Create(txCtx, wsA); err != nil {
			return err
		}
		if err := wsRepo.WithScope(scope).Create(txCtx, wsB); err != nil {
			return err
		}
		for _, p := range []project.Project{proj1, proj2, projB} {
			if err := projRepo.WithScope(scope).Create(txCtx, p); err != nil {
				return err
			}
		}
		if err := objRepo.WithScope(scope).Create(txCtx, obj1); err != nil {
			return err
		}
		if err := objRepo.WithScope(scope).Create(txCtx, objB); err != nil {
			return err
		}
		if err := taskRepo.WithScope(scope).Create(txCtx, parentTask); err != nil {
			return err
		}
		return taskRepo.WithScope(scope).Create(txCtx, childTask)
	}); err != nil {
		t.Fatalf("seed stabilization fixtures: %v", err)
	}

	// 1. Cross-project and cross-tenant ObjectiveID validation on Create & Update.
	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		return taskRepo.WithScope(scope).Create(txCtx, task.Task{
			ID:          uuid.New(),
			WorkspaceID: wsA.ID,
			ProjectID:   proj2.ID,
			ObjectiveID: &obj1.ID, // belongs to proj1, not proj2!
			Title:       "Cross-project objective task",
			Status:      task.StatusReady,
		})
	}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("expected ErrValidation for cross-project ObjectiveID on Create, got %v", err)
	}
	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		return taskRepo.WithScope(scope).Create(txCtx, task.Task{
			ID:          uuid.New(),
			WorkspaceID: wsA.ID,
			ProjectID:   proj1.ID,
			ObjectiveID: &objB.ID, // belongs to wsB!
			Title:       "Cross-tenant objective task",
			Status:      task.StatusReady,
		})
	}); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for cross-tenant ObjectiveID on Create, got %v", err)
	}

	proj2Task := task.Task{
		ID:          uuid.New(),
		WorkspaceID: wsA.ID,
		ProjectID:   proj2.ID,
		Title:       "Proj2 Task",
		Status:      task.StatusReady,
	}
	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		return taskRepo.WithScope(scope).Create(txCtx, proj2Task)
	}); err != nil {
		t.Fatalf("Create proj2Task: %v", err)
	}
	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		badUpd := proj2Task.Clone()
		badUpd.ObjectiveID = &obj1.ID
		return taskRepo.WithScope(scope).Update(txCtx, badUpd, nil)
	}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("expected ErrValidation for cross-project ObjectiveID on Update, got %v", err)
	}

	// 2. Nullable field clearing on Task (ObjectiveID, ParentTaskID, AssigneeActorID) & Objective (TargetDate).
	loadedChild, err := taskRepo.GetByID(ctx, wsA.ID, childTask.ID)
	if err != nil {
		t.Fatalf("GetByID(childTask): %v", err)
	}
	if loadedChild.ObjectiveID == nil || loadedChild.ParentTaskID == nil || loadedChild.AssigneeActorID == nil {
		t.Fatalf("expected non-nil initial pointers on childTask: %+v", loadedChild)
	}

	loadedChild.ObjectiveID = nil
	loadedChild.ParentTaskID = nil
	loadedChild.AssigneeActorID = nil

	loadedObj, err := objRepo.GetByID(ctx, wsA.ID, obj1.ID)
	if err != nil || loadedObj.TargetDate == nil {
		t.Fatalf("GetByID(obj1): %+v, err=%v", loadedObj, err)
	}
	loadedObj.TargetDate = nil

	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		if err := taskRepo.WithScope(scope).Update(txCtx, loadedChild, nil); err != nil {
			return err
		}
		return objRepo.WithScope(scope).Update(txCtx, loadedObj)
	}); err != nil {
		t.Fatalf("Update clearing nullable fields: %v", err)
	}

	clearedChild, err := taskRepo.GetByID(ctx, wsA.ID, childTask.ID)
	if err != nil {
		t.Fatalf("GetByID(clearedChild): %v", err)
	}
	if clearedChild.ObjectiveID != nil || clearedChild.ParentTaskID != nil || clearedChild.AssigneeActorID != nil {
		t.Fatalf("expected nil domain pointers after clearing, got %+v", clearedChild)
	}

	// Verify underlying SQL columns in ListTasksByProject and ListAgentClaimableTasks are NULL!
	wsAStr := wsA.ID.String()
	proj1Str := proj1.ID.String()
	obj1Str := obj1.ID.String()
	sqlTasks, err := readQ.ListTasksByProject(ctx, sqliteSQLCListTasksParams(wsAStr, proj1Str, nil))
	if err != nil {
		t.Fatalf("readQ.ListTasksByProject: %v", err)
	}
	var foundChildSQL bool
	for _, r := range sqlTasks {
		if r.ID == childTask.ID.String() {
			foundChildSQL = true
			if r.ObjectiveID != nil || r.ParentTaskID != nil || r.AssigneeActorID != nil {
				t.Fatalf("expected SQL NULL columns on cleared task, got obj=%v parent=%v assignee=%v",
					r.ObjectiveID, r.ParentTaskID, r.AssigneeActorID)
			}
		}
	}
	if !foundChildSQL {
		t.Fatal("childTask missing from ListTasksByProject")
	}
	byOldObj, err := readQ.ListTasksByProject(ctx, sqliteSQLCListTasksParams(wsAStr, proj1Str, &obj1Str))
	if err != nil || len(byOldObj) != 0 {
		t.Fatalf("expected 0 SQL tasks still linked to cleared ObjectiveID, got %d (err=%v)", len(byOldObj), err)
	}
	otherActorStr := uuid.NewString()
	claimable, err := readQ.ListAgentClaimableTasks(ctx, sqliteSQLCClaimableParams(wsAStr, &proj1Str, &otherActorStr, 20))
	if err != nil {
		t.Fatalf("ListAgentClaimableTasks: %v", err)
	}
	var childClaimable bool
	for _, c := range claimable {
		if c.ID == childTask.ID.String() && c.AssigneeActorID == nil {
			childClaimable = true
		}
	}
	if !childClaimable {
		t.Fatalf("expected cleared-assignee childTask to appear in ListAgentClaimableTasks for another actor")
	}

	clearedObj, err := objRepo.GetByID(ctx, wsA.ID, obj1.ID)
	if err != nil || clearedObj.TargetDate != nil {
		t.Fatalf("expected nil TargetDate on clearedObj, got %+v (err=%v)", clearedObj, err)
	}
	rawObj, err := readQ.GetObjectiveByID(ctx, sqliteSQLCGetObjParams(wsAStr, obj1Str))
	if err != nil || rawObj.TargetDate != nil {
		t.Fatalf("expected SQL NULL target_date on objective, got %v (err=%v)", rawObj.TargetDate, err)
	}

	// 3. TaskRepository.Create collision reallocation & relation sidecar cleanup on DeleteRelation + HardDelete.
	seqProj := project.Project{ID: uuid.New(), WorkspaceID: wsA.ID, Slug: "seq-proj", KeyPrefix: "SEQ", Name: "Seq Project", Status: project.StatusActive}
	manualSeq1 := task.Task{ID: uuid.New(), WorkspaceID: wsA.ID, ProjectID: seqProj.ID, Key: "SEQ-1", Title: "Manual SEQ-1", Status: task.StatusReady}
	collidingKey := task.Task{ID: uuid.New(), WorkspaceID: wsA.ID, ProjectID: seqProj.ID, Key: "SEQ-1", Title: "Colliding SEQ-1", Status: task.StatusArchived}
	rel1ID := uuid.New()
	rel2ID := uuid.New()

	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		if err := projRepo.WithScope(scope).Create(txCtx, seqProj); err != nil {
			return err
		}
		tTx := taskRepo.WithScope(scope)
		if err := tTx.Create(txCtx, manualSeq1); err != nil {
			return err
		}
		if err := tTx.Create(txCtx, collidingKey); err != nil {
			return err
		}
		if err := tTx.CreateRelation(txCtx, task.TaskRelation{
			ID:           rel1ID,
			WorkspaceID:  wsA.ID,
			SourceTaskID: manualSeq1.ID,
			TargetTaskID: collidingKey.ID,
			Type:         task.RelationBlocks,
			Note:         "sidecar-1",
			CreatedBy:    actorID,
		}); err != nil {
			return err
		}
		return tTx.CreateRelation(txCtx, task.TaskRelation{
			ID:           rel2ID,
			WorkspaceID:  wsA.ID,
			SourceTaskID: collidingKey.ID,
			TargetTaskID: manualSeq1.ID,
			Type:         task.RelationSubtaskOf,
			Note:         "sidecar-2",
			CreatedBy:    actorID,
		})
	}); err != nil {
		t.Fatalf("seed colliding task & sidecar relations: %v", err)
	}
	reallocated, err := taskRepo.GetByID(ctx, wsA.ID, collidingKey.ID)
	if err != nil || reallocated.Key != "SEQ-2" {
		t.Fatalf("expected colliding SEQ-1 to reallocate to SEQ-2, got %+v (err=%v)", reallocated, err)
	}

	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		if err := taskRepo.WithScope(scope).DeleteRelation(txCtx, wsA.ID, rel1ID); err != nil {
			return err
		}
		return taskRepo.WithScope(scope).HardDelete(txCtx, wsA.ID, collidingKey.ID)
	}); err != nil {
		t.Fatalf("DeleteRelation + HardDelete: %v", err)
	}
	for _, rid := range []uuid.UUID{rel1ID, rel2ID} {
		if _, bErr := readQ.GetDSLExecutionBatchByIdempotencyKey(ctx, sqliteSQLCBatchByKeyParams(wsAStr, db.RelationMetaIdempotencyKey(rid))); bErr == nil {
			t.Fatalf("expected relation sidecar %s to be deleted from dsl_execution_batches", rid)
		}
	}

	// 4. DeleteProject cascade soft-deletes tasks + objectives so KeyPrefix can be reused cleanly!
	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		return taskRepo.WithScope(scope).UpsertLease(txCtx, task.ClaimLease{
			TaskID:        manualSeq1.ID,
			WorkspaceID:   wsA.ID,
			LeaseToken:    "lease-before-proj-del",
			HolderActorID: actorID,
			HolderKind:    authctx.ActorKindHuman,
			HolderHandle:  "alice",
			AcquiredAt:    time.Now().UTC(),
			HeartbeatAt:   time.Now().UTC(),
			ExpiresAt:     time.Now().UTC().Add(10 * time.Minute),
		})
	}); err != nil {
		t.Fatalf("UpsertLease before project delete: %v", err)
	}
	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		return projRepo.WithScope(scope).SoftDelete(txCtx, wsA.ID, seqProj.ID)
	}); err != nil {
		t.Fatalf("SoftDelete(seqProj): %v", err)
	}
	if _, err := taskRepo.GetByID(ctx, wsA.ID, manualSeq1.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("expected task of deleted project to return ErrNotFound, got %v", err)
	}
	reusedSeqProj := project.Project{ID: uuid.New(), WorkspaceID: wsA.ID, Slug: "seq-proj-reborn", KeyPrefix: "SEQ", Name: "Seq Reborn", Status: project.StatusActive}
	rebornSeq1 := task.Task{ID: uuid.New(), WorkspaceID: wsA.ID, ProjectID: reusedSeqProj.ID, Title: "Fresh SEQ-1", Status: task.StatusReady}
	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		if err := projRepo.WithScope(scope).Create(txCtx, reusedSeqProj); err != nil {
			return err
		}
		return taskRepo.WithScope(scope).Create(txCtx, rebornSeq1)
	}); err != nil {
		t.Fatalf("reuse KeyPrefix SEQ and mint SEQ-1 after project SoftDelete failed: %v", err)
	}
	gotRebornSeq1, err := taskRepo.GetByID(ctx, wsA.ID, rebornSeq1.ID)
	if err != nil || gotRebornSeq1.Key != "SEQ-1" {
		t.Fatalf("expected reborn project first task key SEQ-1, got %+v (err=%v)", gotRebornSeq1, err)
	}

	// 5. BoardMetadataService (WorkflowStates & ProjectLabels) end-to-end over SQLite.
	projSvc := project.NewService(projRepo, txMgr)
	authCtx := authctx.WithIdentity(ctx, authctx.Identity{
		WorkspaceID: wsA.ID,
		ActorID:     actorID,
		Kind:        authctx.ActorKindHuman,
		Handle:      "alice",
	})
	states, err := projSvc.ListWorkflowStates(authCtx, wsA.ID, proj1.ID)
	if err != nil || len(states) != 8 {
		t.Fatalf("ListWorkflowStates = %d states (err=%v), want 8", len(states), err)
	}
	var sawArchivedCat bool
	for _, st := range states {
		if st.Name == "Archived" && st.Category == "archived" {
			sawArchivedCat = true
		}
	}
	if !sawArchivedCat {
		t.Fatalf("expected seeded Archived state to expose Category='archived', got %+v", states)
	}

	qaState, err := projSvc.CreateWorkflowState(authCtx, project.CreateWorkflowStateInput{
		WorkspaceID: wsA.ID,
		ProjectID:   proj1.ID,
		Name:        "QA Verification",
		Category:    "started",
		ColorHex:    "#14B8A6",
		Position:    45,
		IsDefault:   true,
	})
	if err != nil || !qaState.IsDefault || qaState.ColorHex != "#14B8A6" {
		t.Fatalf("CreateWorkflowState failed: %+v, err=%v", qaState, err)
	}
	newStateName := "Staging QA"
	updatedQA, err := projSvc.UpdateWorkflowState(authCtx, project.UpdateWorkflowStateInput{
		WorkspaceID: wsA.ID,
		ProjectID:   proj1.ID,
		StateID:     qaState.ID,
		Name:        &newStateName,
	})
	if err != nil || updatedQA.Name != "Staging QA" {
		t.Fatalf("UpdateWorkflowState failed: %+v, err=%v", updatedQA, err)
	}
	statesAfter, err := projSvc.ListWorkflowStates(authCtx, wsA.ID, proj1.ID)
	if err != nil {
		t.Fatalf("ListWorkflowStates after update: %v", err)
	}
	defaultCount := 0
	for _, st := range statesAfter {
		if st.IsDefault {
			defaultCount++
		}
	}
	if defaultCount != 1 {
		t.Fatalf("expected exactly 1 default workflow state in project, got %d", defaultCount)
	}

	lbl, err := projSvc.CreateLabel(authCtx, project.CreateProjectLabelInput{
		WorkspaceID: wsA.ID,
		ProjectID:   proj1.ID,
		Name:        "security",
		ColorHex:    "#EF4444",
		Description: "Security-sensitive change",
	})
	if err != nil || lbl.Name != "security" {
		t.Fatalf("CreateLabel failed: %+v, err=%v", lbl, err)
	}
	labels, err := projSvc.ListLabels(authCtx, wsA.ID, proj1.ID)
	if err != nil || len(labels) != 1 || labels[0].ID != lbl.ID {
		t.Fatalf("ListLabels = %+v, err=%v", labels, err)
	}
	if err := projSvc.DeleteLabel(authCtx, wsA.ID, proj1.ID, lbl.ID); err != nil {
		t.Fatalf("DeleteLabel failed: %v", err)
	}
	labelsAfterDel, err := projSvc.ListLabels(authCtx, wsA.ID, proj1.ID)
	if err != nil || len(labelsAfterDel) != 0 {
		t.Fatalf("expected 0 labels after DeleteLabel, got %+v (err=%v)", labelsAfterDel, err)
	}
	if err := projSvc.DeleteLabel(authCtx, wsA.ID, proj1.ID, lbl.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("expected ErrNotFound deleting already-deleted label, got %v", err)
	}

	// 6. AuditRepository.Query Offset/Limit pagination (both pushdown and post-filtered).
	page1, err := auditRepo.Query(ctx, audit.QueryFilter{WorkspaceID: wsA.ID, Offset: 0, Limit: 2})
	if err != nil || len(page1) != 2 {
		t.Fatalf("auditRepo.Query page1 = %+v (err=%v)", page1, err)
	}
	page2, err := auditRepo.Query(ctx, audit.QueryFilter{WorkspaceID: wsA.ID, Offset: 2, Limit: 2})
	if err != nil || len(page2) != 2 || page1[0].ID == page2[0].ID {
		t.Fatalf("auditRepo.Query page2 = %+v (err=%v)", page2, err)
	}
	filteredCreate, err := auditRepo.Query(ctx, audit.QueryFilter{
		WorkspaceID: wsA.ID,
		Action:      tx.AuditActionCreate,
		Offset:      1,
		Limit:       1,
	})
	if err != nil || len(filteredCreate) != 1 || filteredCreate[0].Action != tx.AuditActionCreate {
		t.Fatalf("auditRepo.Query filteredCreate = %+v (err=%v)", filteredCreate, err)
	}

	// 7. TaskRepository LastSignalAt default NULL and TouchSignal round-trip.
	nullSigTask := task.Task{
		ID:          uuid.New(),
		WorkspaceID: wsA.ID,
		ProjectID:   proj1.ID,
		Kind:        task.KindChore,
		Title:       "Nullable Signal Task",
		Status:      task.StatusReady,
		Priority:    task.PriorityMedium,
		CreatedBy:   actorID,
	}
	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		return adapter.TaskRepository().WithScope(scope).Create(txCtx, nullSigTask)
	}); err != nil {
		t.Fatalf("Create nullSigTask: %v", err)
	}
	gotNullSig, err := adapter.TaskRepository().GetByID(ctx, wsA.ID, nullSigTask.ID)
	if err != nil || gotNullSig.LastSignalAt != nil {
		t.Fatalf("expected default NULL LastSignalAt on SQLite task, got %v (err=%v)", gotNullSig.LastSignalAt, err)
	}
	touchTime := time.Now().UTC().Truncate(time.Millisecond)
	var touchedTask task.Task
	if err := txMgr.WithNoAuditTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		var tErr error
		touchedTask, tErr = adapter.TaskRepository().WithScope(scope).TouchSignal(txCtx, wsA.ID, nullSigTask.ID, touchTime)
		return tErr
	}); err != nil {
		t.Fatalf("TouchSignal: %v", err)
	}
	if touchedTask.LastSignalAt == nil || !touchedTask.LastSignalAt.Equal(touchTime) {
		t.Fatalf("TouchSignal round-trip = %v, want %v", touchedTask.LastSignalAt, touchTime)
	}
	if !touchedTask.UpdatedAt.Equal(gotNullSig.UpdatedAt) {
		t.Fatalf("expected TouchSignal not to mutate UpdatedAt (was %v, got %v)", gotNullSig.UpdatedAt, touchedTask.UpdatedAt)
	}
}

func sqliteSQLCListTasksParams(wsID, projID string, objID *string) sqlcgen.ListTasksByProjectParams {
	return sqlcgen.ListTasksByProjectParams{
		WorkspaceID: wsID,
		ProjectID:   projID,
		ObjectiveID: objID,
	}
}

func sqliteSQLCClaimableParams(wsID string, projID, actorID *string, limit int64) sqlcgen.ListAgentClaimableTasksParams {
	return sqlcgen.ListAgentClaimableTasksParams{
		WorkspaceID: wsID,
		ProjectID:   projID,
		ActorID:     actorID,
		Limit:       limit,
	}
}

func sqliteSQLCGetObjParams(wsID, objID string) sqlcgen.GetObjectiveByIDParams {
	return sqlcgen.GetObjectiveByIDParams{
		WorkspaceID: wsID,
		ID:          objID,
	}
}

func sqliteSQLCBatchByKeyParams(wsID, key string) sqlcgen.GetDSLExecutionBatchByIdempotencyKeyParams {
	return sqlcgen.GetDSLExecutionBatchByIdempotencyKeyParams{
		WorkspaceID:    wsID,
		IdempotencyKey: key,
	}
}
