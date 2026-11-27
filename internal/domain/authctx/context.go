// Package authctx provides transport-agnostic authentication identity types and
// context propagation helpers for enforcing mandatory workspace/tenant ownership
// across all Flowsk domain services, DSL executors, and persistence adapters.
//
// Architectural & Security Invariants:
//   - Zero Transport Leakage: This package NEVER imports ConnectRPC, HTTP, CLI,
//     or MCP packages. Transport interceptors/middlewares authenticate requests
//     at the edge and inject an immutable Identity via WithIdentity.
//   - Mandatory Ownership Verification: Every domain operation (read, create,
//     update, transition, claim, delete, or batch script execution) MUST extract
//     the caller's Identity via FromContext and verify tenant scope via
//     RequireWorkspaceOwnership before executing any repository call.
//   - Cross-Tenant Isolation: Attempts to read or mutate resources belonging to a
//     foreign WorkspaceID MUST be rejected immediately with errs.ErrPermissionDenied.
package authctx

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/domain/errs"
)

// ActorKind distinguishes human operators from autonomous AI agent harnesses
// for lease policies, audit attribution, and rate/concurrency governance.
type ActorKind string

const (
	// ActorKindHuman represents an interactive human developer or operator
	// invoking Flowsk via the CLI, web UI, or authenticated RPC client.
	ActorKindHuman ActorKind = "human"

	// ActorKindAgent represents an autonomous coding agent, orchestrator, or
	// MCP harness claiming leases and executing task workflows.
	ActorKindAgent ActorKind = "agent"
)

// Valid reports whether k is a recognized, non-empty ActorKind value.
func (k ActorKind) Valid() bool {
	switch k {
	case ActorKindHuman, ActorKindAgent:
		return true
	default:
		return false
	}
}

// Identity encapsulates the verified security principal and tenant workspace boundary
// bound to an in-flight request context.
//
// Immutability Invariant:
//   - Identity values are passed by value and contain only immutable scalar fields,
//     preventing race conditions or post-authentication privilege mutation across
//     concurrent goroutines.
type Identity struct {
	// WorkspaceID is the mandatory tenant partition UUID that scopes all domain
	// queries, mutations, and audit records executed on behalf of this principal.
	WorkspaceID uuid.UUID

	// ActorID is the unique principal UUID of the human user or autonomous agent.
	ActorID uuid.UUID

	// Kind classifies whether the principal is a human user or an AI agent.
	Kind ActorKind

	// Handle is the stable, human-readable identifier of the actor within the
	// workspace (for example, "mario" or "claude-worker-01"), recorded in logs
	// and lease diagnostics.
	Handle string
}

// Validate checks that the Identity carries non-nil WorkspaceID and ActorID values,
// a valid ActorKind, and a non-empty Handle.
func (id Identity) Validate() error {
	const op = "authctx.Identity.Validate"
	var violations []errs.FieldViolation
	if id.WorkspaceID == uuid.Nil {
		violations = append(violations, errs.FieldViolation{
			Field:       "workspace_id",
			Description: "workspace_id must be a non-nil UUID",
		})
	}
	if id.ActorID == uuid.Nil {
		violations = append(violations, errs.FieldViolation{
			Field:       "actor_id",
			Description: "actor_id must be a non-nil UUID",
		})
	}
	if !id.Kind.Valid() {
		violations = append(violations, errs.FieldViolation{
			Field:       "kind",
			Description: fmt.Sprintf("actor kind %q must be %q or %q", id.Kind, ActorKindHuman, ActorKindAgent),
		})
	}
	if id.Handle == "" {
		violations = append(violations, errs.FieldViolation{
			Field:       "handle",
			Description: "actor handle must not be empty",
		})
	}
	if len(violations) > 0 {
		return errs.Validation(op, "invalid caller identity", violations...)
	}
	return nil
}

// contextKey is an unexported empty struct preventing context key collisions
// with external or transport packages.
type contextKey struct{}

// suppressTaskSignalKey is an unexported empty struct marking contexts that
// must not emit or advance task `last_signal_at` timestamps (for example,
// browser Web UI requests and internal list/relation hydration lookups).
type suppressTaskSignalKey struct{}

// WithSuppressTaskSignal derives a child context instructing task domain services
// not to initialize or update `Task.LastSignalAt` during reads or mutations.
// Browser Web UI interceptors and internal hydration helpers invoke this helper
// so `last_signal_at` strictly reflects CLI, MCP, and autonomous agent activity.
func WithSuppressTaskSignal(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, suppressTaskSignalKey{}, true)
}

// ShouldEmitTaskSignal reports whether domain task operations executed under ctx
// should record an activity signal on `Task.LastSignalAt`.
// Returns true by default for non-nil contexts unless suppressed via WithSuppressTaskSignal.
func ShouldEmitTaskSignal(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	suppressed, ok := ctx.Value(suppressTaskSignalKey{}).(bool)
	return !ok || !suppressed
}

// WithIdentity derives a child context carrying the verified caller Identity.
// Transport authentication interceptors, MCP session handlers, and local CLI
// bootstraps invoke this helper prior to calling any domain service.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

// FromContext extracts and validates the authenticated Identity bound to ctx.
//
// Error Contract:
//   - Returns errs.ErrUnauthenticated if no Identity is present on ctx or if ctx is nil.
//   - Returns errs.ErrUnauthenticated wrapping validation details if the stored Identity
//     is malformed (for example, uuid.Nil WorkspaceID or ActorID).
func FromContext(ctx context.Context) (Identity, error) {
	const op = "authctx.FromContext"
	if ctx == nil {
		return Identity{}, errs.New(errs.ErrUnauthenticated, op, "missing request context")
	}
	raw := ctx.Value(contextKey{})
	if raw == nil {
		return Identity{}, errs.New(errs.ErrUnauthenticated, op, "no authenticated identity in context")
	}
	id, ok := raw.(Identity)
	if !ok {
		return Identity{}, errs.New(errs.ErrUnauthenticated, op, "unexpected identity type in context")
	}
	if err := id.Validate(); err != nil {
		return Identity{}, errs.Wrap(errs.ErrUnauthenticated, op, "invalid authenticated identity", err)
	}
	return id, nil
}

// RequireWorkspaceOwnership verifies that ctx carries a valid authenticated Identity
// AND that its authorized WorkspaceID strictly matches targetWorkspaceID.
//
// Ownership Invariant:
//   - Every domain service method accepting an explicit workspace ID or loading a
//     workspace-scoped aggregate MUST call RequireWorkspaceOwnership before returning
//     data or mutating persistence state.
//
// Error Contract:
//   - Returns errs.ErrUnauthenticated if ctx lacks a valid Identity.
//   - Returns errs.ErrValidation if targetWorkspaceID is uuid.Nil.
//   - Returns errs.ErrPermissionDenied if id.WorkspaceID != targetWorkspaceID.
func RequireWorkspaceOwnership(ctx context.Context, targetWorkspaceID uuid.UUID) (Identity, error) {
	const op = "authctx.RequireWorkspaceOwnership"
	id, err := FromContext(ctx)
	if err != nil {
		return Identity{}, err
	}
	if targetWorkspaceID == uuid.Nil {
		return Identity{}, errs.Validation(op, "target workspace_id is required", errs.FieldViolation{
			Field:       "workspace_id",
			Description: "target workspace_id must be a non-nil UUID",
		})
	}
	if id.WorkspaceID != targetWorkspaceID {
		return Identity{}, errs.New(
			errs.ErrPermissionDenied,
			op,
			"caller identity is not authorized for the target workspace",
		)
	}
	return id, nil
}
