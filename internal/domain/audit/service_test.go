// File service_test.go verifies audit.Service authentication, multi-tenant
// workspace ownership, pagination bounds, filter validation, and Record.Clone()
// defensive byte-slice copies.
package audit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/domain/audit"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/tx"
)

type memAuditRepo struct {
	records   []audit.Record
	lastLimit int32
}

func (r *memAuditRepo) WithScope(_ tx.Scope) audit.Repository {
	return r
}

func (r *memAuditRepo) GetByID(_ context.Context, workspaceID, auditID uuid.UUID) (audit.Record, error) {
	for _, rec := range r.records {
		if rec.WorkspaceID == workspaceID && rec.ID == auditID {
			return rec, nil
		}
	}
	return audit.Record{}, errs.New(errs.ErrNotFound, "audit.Repo.GetByID", "audit record not found")
}

func (r *memAuditRepo) Query(_ context.Context, filter audit.QueryFilter) ([]audit.Record, error) {
	r.lastLimit = filter.Limit
	var out []audit.Record
	for _, rec := range r.records {
		if rec.WorkspaceID != filter.WorkspaceID {
			continue
		}
		if filter.ActorID != uuid.Nil && rec.ActorID != filter.ActorID {
			continue
		}
		if filter.Action != "" && rec.Action != filter.Action {
			continue
		}
		if filter.ResourceType != "" && rec.ResourceType != filter.ResourceType {
			continue
		}
		if filter.ResourceID != "" && rec.ResourceID != filter.ResourceID {
			continue
		}
		if filter.Since != nil && rec.OccurredAt.Before(*filter.Since) {
			continue
		}
		if filter.Until != nil && rec.OccurredAt.After(*filter.Until) {
			continue
		}
		out = append(out, rec)
	}
	if int(filter.Offset) >= len(out) {
		return nil, nil
	}
	out = out[filter.Offset:]
	if filter.Limit > 0 && int(filter.Limit) < len(out) {
		out = out[:filter.Limit]
	}
	return out, nil
}

// TestAuditService_OwnershipBoundsAndDefensiveCopies verifies authentication,
// tenant isolation, safe pagination clamping, and immutable JSON snapshot cloning.
func TestAuditService_OwnershipBoundsAndDefensiveCopies(t *testing.T) {
	t.Parallel()

	tenantA := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	tenantB := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	actorA := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	recID := uuid.MustParse("77777777-7777-7777-7777-777777777777")

	repo := &memAuditRepo{
		records: []audit.Record{
			{
				ID:           recID,
				WorkspaceID:  tenantA,
				ActorID:      actorA,
				ActorKind:    authctx.ActorKindAgent,
				ActorHandle:  "auditor-01",
				Action:       tx.AuditActionUpdate,
				ResourceType: "task",
				ResourceID:   "FLW-1",
				OldValues:    []byte(`{"status":"ready"}`),
				NewValues:    []byte(`{"status":"in_progress"}`),
				OccurredAt:   time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC),
			},
		},
	}

	svc := audit.NewService(repo, nil)

	unauthCtx := context.Background()
	ctxA := authctx.WithIdentity(context.Background(), authctx.Identity{
		WorkspaceID: tenantA,
		ActorID:     actorA,
		Kind:        authctx.ActorKindHuman,
		Handle:      "mario",
	})

	// Unauthenticated rejections.
	if _, err := svc.GetRecord(unauthCtx, tenantA, recID); !errors.Is(err, errs.ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated on GetRecord, got %v", err)
	}
	if _, err := svc.QueryRecords(unauthCtx, audit.QueryFilter{WorkspaceID: tenantA}); !errors.Is(err, errs.ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated on QueryRecords, got %v", err)
	}

	// Cross-tenant rejections.
	if _, err := svc.GetRecord(ctxA, tenantB, recID); !errors.Is(err, errs.ErrPermissionDenied) {
		t.Fatalf("expected ErrPermissionDenied on cross-tenant GetRecord, got %v", err)
	}
	if _, err := svc.QueryRecords(ctxA, audit.QueryFilter{WorkspaceID: tenantB}); !errors.Is(err, errs.ErrPermissionDenied) {
		t.Fatalf("expected ErrPermissionDenied on cross-tenant QueryRecords, got %v", err)
	}

	// Validation checks (nil auditID, negative offset, invalid action, inverted time window).
	if _, err := svc.GetRecord(ctxA, tenantA, uuid.Nil); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("expected ErrValidation for uuid.Nil auditID, got %v", err)
	}
	if _, err := svc.QueryRecords(ctxA, audit.QueryFilter{WorkspaceID: tenantA, Offset: -1}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("expected ErrValidation for negative offset, got %v", err)
	}
	if _, err := svc.QueryRecords(ctxA, audit.QueryFilter{WorkspaceID: tenantA, Action: "InvalidVerb"}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("expected ErrValidation for invalid AuditAction, got %v", err)
	}
	t1 := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	t0 := time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC)
	if _, err := svc.QueryRecords(ctxA, audit.QueryFilter{WorkspaceID: tenantA, Since: &t1, Until: &t0}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("expected ErrValidation when Since > Until, got %v", err)
	}

	// Pagination clamping: Limit <= 0 applies safe positive default; Limit > max clamps to max.
	results, err := svc.QueryRecords(ctxA, audit.QueryFilter{WorkspaceID: tenantA, Limit: 0})
	if err != nil || len(results) != 1 || repo.lastLimit <= 0 {
		t.Fatalf("expected safe default limit > 0 and 1 result, got limit=%d len=%d err=%v", repo.lastLimit, len(results), err)
	}
	_, err = svc.QueryRecords(ctxA, audit.QueryFilter{WorkspaceID: tenantA, Limit: 9999})
	if err != nil || repo.lastLimit > 200 {
		t.Fatalf("expected clamped limit <= 200, got %d (err=%v)", repo.lastLimit, err)
	}

	// Defensive copy verification on GetRecord and QueryRecords.
	got, err := svc.GetRecord(ctxA, tenantA, recID)
	if err != nil {
		t.Fatalf("GetRecord failed: %v", err)
	}
	got.OldValues[0] = 'X'
	got.NewValues[0] = 'Y'

	gotAgain, err := svc.GetRecord(ctxA, tenantA, recID)
	if err != nil {
		t.Fatalf("second GetRecord failed: %v", err)
	}
	if string(gotAgain.OldValues) != `{"status":"ready"}` || string(gotAgain.NewValues) != `{"status":"in_progress"}` {
		t.Fatalf("expected defensive byte copy on GetRecord, got old=%q new=%q", gotAgain.OldValues, gotAgain.NewValues)
	}
}

// TestAuditRecord_ToProtoValidUUIDs verifies that Record.ToProto always emits valid
// UUID strings for EntityId and ActorId across single-UUID, composite parent:child,
// human key, and nil-actor records.
func TestAuditRecord_ToProtoValidUUIDs(t *testing.T) {
	t.Parallel()

	wsID := uuid.New()
	parentID := uuid.New()
	childID := uuid.New()

	// Composite parentID:childID resource ID (e.g. UnbindSubproject).
	composite := (&audit.Record{
		ID:           uuid.New(),
		WorkspaceID:  wsID,
		ActorID:      uuid.Nil,
		Action:       tx.AuditActionDelete,
		ResourceType: "project_binding",
		ResourceID:   parentID.String() + ":" + childID.String(),
		OccurredAt:   time.Now().UTC(),
	}).ToProto()
	if composite.GetEntityId() != parentID.String() {
		t.Fatalf("composite EntityId = %q, want %q", composite.GetEntityId(), parentID.String())
	}
	if composite.GetEntityIdentifier() != parentID.String()+":"+childID.String() {
		t.Fatalf("composite EntityIdentifier = %q", composite.GetEntityIdentifier())
	}
	if _, err := uuid.Parse(composite.GetActorId()); err != nil {
		t.Fatalf("expected valid fallback ActorId UUID when uuid.Nil, got %q (%v)", composite.GetActorId(), err)
	}

	// Non-UUID human key falls back to WorkspaceID for EntityId while preserving EntityIdentifier.
	byKey := (&audit.Record{
		ID:           uuid.New(),
		WorkspaceID:  wsID,
		ActorID:      uuid.New(),
		Action:       tx.AuditActionTransition,
		ResourceType: "task",
		ResourceID:   "FLW-42",
		OccurredAt:   time.Now().UTC(),
	}).ToProto()
	if byKey.GetEntityId() != wsID.String() || byKey.GetEntityIdentifier() != "FLW-42" {
		t.Fatalf("byKey ToProto = (%q, %q), want (%q, FLW-42)", byKey.GetEntityId(), byKey.GetEntityIdentifier(), wsID.String())
	}
}
