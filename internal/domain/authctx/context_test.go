// File context_test.go verifies tenant ownership guards and identity resolution
// in package authctx.
package authctx_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
)

// TestRequireWorkspaceOwnership_EnforcesTenantIsolation verifies that
// unauthenticated contexts, nil workspace IDs, and cross-workspace access
// attempts fail securely with their expected sentinel errors.
func TestRequireWorkspaceOwnership_EnforcesTenantIsolation(t *testing.T) {
	t.Parallel()

	tenantA := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	tenantB := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	actorA := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")

	validIdentity := authctx.Identity{
		WorkspaceID: tenantA,
		ActorID:     actorA,
		Kind:        authctx.ActorKindAgent,
		Handle:      "agent-dev-01",
	}

	t.Run("rejects unauthenticated context", func(t *testing.T) {
		t.Parallel()
		_, err := authctx.RequireWorkspaceOwnership(context.Background(), tenantA)
		if !errors.Is(err, errs.ErrUnauthenticated) {
			t.Fatalf("expected ErrUnauthenticated, got %v", err)
		}
	})

	t.Run("rejects malformed identity in context", func(t *testing.T) {
		t.Parallel()
		ctx := authctx.WithIdentity(context.Background(), authctx.Identity{
			WorkspaceID: tenantA,
			ActorID:     uuid.Nil,
			Kind:        authctx.ActorKindHuman,
			Handle:      "mario",
		})
		_, err := authctx.RequireWorkspaceOwnership(ctx, tenantA)
		if !errors.Is(err, errs.ErrUnauthenticated) {
			t.Fatalf("expected ErrUnauthenticated for malformed identity, got %v", err)
		}
	})

	t.Run("rejects nil target workspace UUID", func(t *testing.T) {
		t.Parallel()
		ctx := authctx.WithIdentity(context.Background(), validIdentity)
		_, err := authctx.RequireWorkspaceOwnership(ctx, uuid.Nil)
		if !errors.Is(err, errs.ErrValidation) {
			t.Fatalf("expected ErrValidation for uuid.Nil target workspace, got %v", err)
		}
	})

	t.Run("rejects cross-tenant foreign workspace access", func(t *testing.T) {
		t.Parallel()
		ctx := authctx.WithIdentity(context.Background(), validIdentity)
		_, err := authctx.RequireWorkspaceOwnership(ctx, tenantB)
		if !errors.Is(err, errs.ErrPermissionDenied) {
			t.Fatalf("expected ErrPermissionDenied for foreign workspace, got %v", err)
		}
	})

	t.Run("allows matching workspace ownership", func(t *testing.T) {
		t.Parallel()
		ctx := authctx.WithIdentity(context.Background(), validIdentity)
		got, err := authctx.RequireWorkspaceOwnership(ctx, tenantA)
		if err != nil {
			t.Fatalf("unexpected error for matching workspace: %v", err)
		}
		if got.WorkspaceID != tenantA || got.ActorID != actorA || got.Kind != authctx.ActorKindAgent {
			t.Fatalf("unexpected resolved identity: %+v", got)
		}
	})
}
