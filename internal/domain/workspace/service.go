// File service.go implements the pure domain application service for provisioning
// multi-tenant Workspaces and managing human/agent actor memberships with atomic
// audit logging.
package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/tx"
)

const (
	// DefaultLeaseTTLSeconds is the default agent claim lease duration (15 minutes)
	// applied when CreateWorkspaceInput.DefaultLeaseTTLSeconds is <= 0.
	DefaultLeaseTTLSeconds int64 = 900

	// MaxLeaseTTLSeconds bounds workspace default lease durations to 24 hours.
	MaxLeaseTTLSeconds int64 = 86400

	maxWorkspaceNameRunes        = 120
	maxWorkspaceDescriptionRunes = 2000
	maxMemberDisplayNameRunes    = 120
)

var (
	workspaceSlugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	memberHandlePattern  = regexp.MustCompile(`^[a-zA-Z0-9_]+([.-][a-zA-Z0-9_]+)*$`)
)

// Service defines the pure domain application operations for managing Workspaces
// and workspace actor memberships.
type Service interface {
	// CreateWorkspace provisions a new Workspace and registers the authenticated
	// caller as its initial Owner within a single atomic audited transaction.
	CreateWorkspace(ctx context.Context, in CreateWorkspaceInput) (Workspace, error)

	// GetWorkspace retrieves the Workspace identified by workspaceID after verifying
	// caller workspace ownership via authctx.RequireWorkspaceOwnership.
	GetWorkspace(ctx context.Context, workspaceID uuid.UUID) (Workspace, error)

	// GetWorkspaceBySlug resolves a Workspace by slug and verifies that the caller
	// is authorized to access the resolved workspace.
	GetWorkspaceBySlug(ctx context.Context, slug string) (Workspace, error)

	// ListWorkspaces lists all active Workspaces accessible to the authenticated actor.
	ListWorkspaces(ctx context.Context) ([]Workspace, error)

	// UpdateWorkspace applies metadata updates to the caller's authorized Workspace
	// inside an audited transaction.
	UpdateWorkspace(ctx context.Context, in UpdateWorkspaceInput) (Workspace, error)

	// ArchiveWorkspace soft-deletes the Workspace inside an audited transaction after
	// verifying Owner permissions and tenant match.
	ArchiveWorkspace(ctx context.Context, workspaceID uuid.UUID) error

	// UpsertMember registers or updates a human or agent actor inside the caller's
	// authorized Workspace within an audited transaction.
	UpsertMember(ctx context.Context, in UpsertMemberInput) (WorkspaceMember, error)

	// ListMembers returns defensive copies of all registered actors in workspaceID
	// after verifying caller workspace ownership.
	ListMembers(ctx context.Context, workspaceID uuid.UUID) ([]WorkspaceMember, error)
}

// ServiceSkeleton implements Service using pure Go domain validation, mandatory
// tenant ownership enforcement, and atomic transaction audit logging.
// The exported type name ServiceSkeleton is retained for cross-package wiring compatibility.
type ServiceSkeleton struct {
	repo  Repository
	txMgr tx.Manager
}

// Compile-time assertion that *ServiceSkeleton satisfies Service.
var _ Service = (*ServiceSkeleton)(nil)

// NewService constructs a workspace ServiceSkeleton wired with its resolved domain
// Repository and transaction Manager. Connection provider wrappers MUST NOT be passed here.
func NewService(repo Repository, txMgr tx.Manager) *ServiceSkeleton {
	return &ServiceSkeleton{
		repo:  repo,
		txMgr: txMgr,
	}
}

// CreateWorkspace provisions a new Workspace and registers the authenticated
// caller as MemberRoleOwner inside an atomic audited transaction.
func (s *ServiceSkeleton) CreateWorkspace(ctx context.Context, in CreateWorkspaceInput) (Workspace, error) {
	const op = "workspace.Service.CreateWorkspace"
	if err := checkContext(ctx, op); err != nil {
		return Workspace{}, err
	}
	caller, err := authctx.FromContext(ctx)
	if err != nil {
		return Workspace{}, err
	}
	if err := s.ensureWriteReady(op); err != nil {
		return Workspace{}, err
	}

	slug := strings.TrimSpace(in.Slug)
	name := strings.TrimSpace(in.Name)
	desc := strings.TrimSpace(in.Description)

	var violations []errs.FieldViolation
	if len(slug) < 2 || len(slug) > 64 || !workspaceSlugPattern.MatchString(slug) {
		violations = append(violations, errs.FieldViolation{
			Field:       "slug",
			Description: "slug must be 2-64 lowercase ASCII alphanumeric characters separated by single hyphens",
		})
	}
	if name == "" || utf8.RuneCountInString(name) > maxWorkspaceNameRunes {
		violations = append(violations, errs.FieldViolation{
			Field:       "name",
			Description: "name must be between 1 and 120 characters",
		})
	}
	if utf8.RuneCountInString(desc) > maxWorkspaceDescriptionRunes {
		violations = append(violations, errs.FieldViolation{
			Field:       "description",
			Description: "description must not exceed 2000 characters",
		})
	}
	ttl := in.DefaultLeaseTTLSeconds
	if ttl <= 0 {
		ttl = DefaultLeaseTTLSeconds
	} else if ttl > MaxLeaseTTLSeconds {
		violations = append(violations, errs.FieldViolation{
			Field:       "default_lease_ttl_seconds",
			Description: "default_lease_ttl_seconds must not exceed 86400",
		})
	}
	if err := validateLabels(in.Labels); err != nil {
		violations = append(violations, errs.FieldViolation{
			Field:       "labels",
			Description: err.Error(),
		})
	}
	if len(violations) > 0 {
		return Workspace{}, errs.Validation(op, "invalid workspace creation parameters", violations...)
	}

	now := time.Now().UTC()
	ws := Workspace{
		ID:                     newUUID(),
		Slug:                   slug,
		Name:                   name,
		Description:            desc,
		DefaultLeaseTTLSeconds: ttl,
		Labels:                 cloneLabels(in.Labels),
		CreatedAt:              now,
		UpdatedAt:              now,
	}

	owner := WorkspaceMember{
		WorkspaceID: ws.ID,
		ActorID:     caller.ActorID,
		Kind:        caller.Kind,
		Handle:      caller.Handle,
		DisplayName: caller.Handle,
		Role:        MemberRoleOwner,
		CreatedAt:   now,
	}

	if err := s.txMgr.WithTransaction(ctx, func(ctx context.Context, scope tx.Scope) error {
		scopedRepo := s.repo.WithScope(scope)
		if err := scopedRepo.Create(ctx, ws.Clone()); err != nil {
			return err
		}
		if err := scopedRepo.UpsertMember(ctx, owner); err != nil {
			return err
		}
		newSnapshot, err := marshalSnapshot(op, ws)
		if err != nil {
			return err
		}
		return scope.Audit().Record(ctx, tx.AuditEntry{
			ID:           newUUID(),
			WorkspaceID:  ws.ID,
			ActorID:      caller.ActorID,
			Action:       tx.AuditActionCreate,
			ResourceType: "workspace",
			ResourceID:   ws.ID.String(),
			NewValues:    newSnapshot,
			OccurredAt:   now,
		})
	}); err != nil {
		return Workspace{}, err
	}

	return ws.Clone(), nil
}

// GetWorkspace retrieves the Workspace identified by workspaceID after verifying
// caller workspace ownership.
func (s *ServiceSkeleton) GetWorkspace(ctx context.Context, workspaceID uuid.UUID) (Workspace, error) {
	const op = "workspace.Service.GetWorkspace"
	if err := checkContext(ctx, op); err != nil {
		return Workspace{}, err
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return Workspace{}, err
	}
	if err := s.ensureReadReady(op); err != nil {
		return Workspace{}, err
	}

	ws, err := s.repo.GetByID(ctx, workspaceID)
	if err != nil {
		return Workspace{}, err
	}
	return ws.Clone(), nil
}

// GetWorkspaceBySlug resolves a Workspace by slug and verifies that the caller
// is authorized to access the resolved workspace.
func (s *ServiceSkeleton) GetWorkspaceBySlug(ctx context.Context, slug string) (Workspace, error) {
	const op = "workspace.Service.GetWorkspaceBySlug"
	if err := checkContext(ctx, op); err != nil {
		return Workspace{}, err
	}
	if _, err := authctx.FromContext(ctx); err != nil {
		return Workspace{}, err
	}
	if err := s.ensureReadReady(op); err != nil {
		return Workspace{}, err
	}

	normalized := strings.TrimSpace(slug)
	if normalized == "" {
		return Workspace{}, errs.Validation(op, "workspace slug is required", errs.FieldViolation{
			Field:       "slug",
			Description: "slug must not be empty",
		})
	}

	ws, err := s.repo.GetBySlug(ctx, normalized)
	if err != nil {
		return Workspace{}, err
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, ws.ID); err != nil {
		return Workspace{}, err
	}
	return ws.Clone(), nil
}

// ListWorkspaces lists all active Workspaces accessible to the authenticated actor.
func (s *ServiceSkeleton) ListWorkspaces(ctx context.Context) ([]Workspace, error) {
	const op = "workspace.Service.ListWorkspaces"
	if err := checkContext(ctx, op); err != nil {
		return nil, err
	}
	caller, err := authctx.FromContext(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.ensureReadReady(op); err != nil {
		return nil, err
	}

	items, err := s.repo.ListForActor(ctx, caller.ActorID)
	if err != nil {
		return nil, err
	}
	out := make([]Workspace, len(items))
	for i := range items {
		out[i] = items[i].Clone()
	}
	return out, nil
}

// UpdateWorkspace applies metadata updates to the caller's authorized Workspace
// inside an atomic audited transaction.
func (s *ServiceSkeleton) UpdateWorkspace(ctx context.Context, in UpdateWorkspaceInput) (Workspace, error) {
	const op = "workspace.Service.UpdateWorkspace"
	if err := checkContext(ctx, op); err != nil {
		return Workspace{}, err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID)
	if err != nil {
		return Workspace{}, err
	}
	if err := s.ensureWriteReady(op); err != nil {
		return Workspace{}, err
	}

	var violations []errs.FieldViolation
	if in.Name != nil {
		trimmed := strings.TrimSpace(*in.Name)
		if trimmed == "" || utf8.RuneCountInString(trimmed) > maxWorkspaceNameRunes {
			violations = append(violations, errs.FieldViolation{
				Field:       "name",
				Description: "name must be between 1 and 120 characters",
			})
		}
	}
	if in.Description != nil && utf8.RuneCountInString(strings.TrimSpace(*in.Description)) > maxWorkspaceDescriptionRunes {
		violations = append(violations, errs.FieldViolation{
			Field:       "description",
			Description: "description must not exceed 2000 characters",
		})
	}
	if in.DefaultLeaseTTLSeconds != nil && (*in.DefaultLeaseTTLSeconds <= 0 || *in.DefaultLeaseTTLSeconds > MaxLeaseTTLSeconds) {
		violations = append(violations, errs.FieldViolation{
			Field:       "default_lease_ttl_seconds",
			Description: "default_lease_ttl_seconds must be between 1 and 86400",
		})
	}
	if in.Labels != nil {
		if err := validateLabels(in.Labels); err != nil {
			violations = append(violations, errs.FieldViolation{
				Field:       "labels",
				Description: err.Error(),
			})
		}
	}
	if len(violations) > 0 {
		return Workspace{}, errs.Validation(op, "invalid workspace update parameters", violations...)
	}

	var updated Workspace
	if err := s.txMgr.WithTransaction(ctx, func(ctx context.Context, scope tx.Scope) error {
		scopedRepo := s.repo.WithScope(scope)
		current, err := scopedRepo.GetByID(ctx, in.WorkspaceID)
		if err != nil {
			return err
		}
		oldSnapshot, err := marshalSnapshot(op, current)
		if err != nil {
			return err
		}

		now := time.Now().UTC()
		next := current.Clone()
		if in.Name != nil {
			next.Name = strings.TrimSpace(*in.Name)
		}
		if in.Description != nil {
			next.Description = strings.TrimSpace(*in.Description)
		}
		if in.DefaultLeaseTTLSeconds != nil {
			next.DefaultLeaseTTLSeconds = *in.DefaultLeaseTTLSeconds
		}
		if in.Labels != nil {
			next.Labels = cloneLabels(in.Labels)
		}
		next.UpdatedAt = now

		if err := scopedRepo.Update(ctx, next.Clone()); err != nil {
			return err
		}
		newSnapshot, err := marshalSnapshot(op, next)
		if err != nil {
			return err
		}
		if err := scope.Audit().Record(ctx, tx.AuditEntry{
			ID:           newUUID(),
			WorkspaceID:  in.WorkspaceID,
			ActorID:      caller.ActorID,
			Action:       tx.AuditActionUpdate,
			ResourceType: "workspace",
			ResourceID:   in.WorkspaceID.String(),
			OldValues:    oldSnapshot,
			NewValues:    newSnapshot,
			OccurredAt:   now,
		}); err != nil {
			return err
		}
		updated = next
		return nil
	}); err != nil {
		return Workspace{}, err
	}

	return updated.Clone(), nil
}

// ArchiveWorkspace soft-deletes the Workspace inside an audited transaction after
// verifying Owner permissions and tenant match.
func (s *ServiceSkeleton) ArchiveWorkspace(ctx context.Context, workspaceID uuid.UUID) error {
	const op = "workspace.Service.ArchiveWorkspace"
	if err := checkContext(ctx, op); err != nil {
		return err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID)
	if err != nil {
		return err
	}
	if err := s.ensureWriteReady(op); err != nil {
		return err
	}

	return s.txMgr.WithTransaction(ctx, func(ctx context.Context, scope tx.Scope) error {
		scopedRepo := s.repo.WithScope(scope)
		current, err := scopedRepo.GetByID(ctx, workspaceID)
		if err != nil {
			return err
		}

		members, err := scopedRepo.ListMembers(ctx, workspaceID)
		if err != nil {
			return err
		}
		if len(members) > 0 {
			isOwner := false
			for _, m := range members {
				if m.ActorID == caller.ActorID && m.Role == MemberRoleOwner {
					isOwner = true
					break
				}
			}
			if !isOwner {
				return errs.New(errs.ErrPermissionDenied, op, "only workspace owners may archive a workspace")
			}
		}

		oldSnapshot, err := marshalSnapshot(op, current)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if err := scopedRepo.SoftDelete(ctx, workspaceID); err != nil {
			return err
		}
		archived := current.Clone()
		archived.UpdatedAt = now
		archived.DeletedAt = &now
		newSnapshot, err := marshalSnapshot(op, archived)
		if err != nil {
			return err
		}
		return scope.Audit().Record(ctx, tx.AuditEntry{
			ID:           newUUID(),
			WorkspaceID:  workspaceID,
			ActorID:      caller.ActorID,
			Action:       tx.AuditActionDelete,
			ResourceType: "workspace",
			ResourceID:   workspaceID.String(),
			OldValues:    oldSnapshot,
			NewValues:    newSnapshot,
			OccurredAt:   now,
		})
	})
}

// UpsertMember registers or updates a human or agent actor inside the caller's
// authorized Workspace within an audited transaction.
func (s *ServiceSkeleton) UpsertMember(ctx context.Context, in UpsertMemberInput) (WorkspaceMember, error) {
	const op = "workspace.Service.UpsertMember"
	if err := checkContext(ctx, op); err != nil {
		return WorkspaceMember{}, err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID)
	if err != nil {
		return WorkspaceMember{}, err
	}
	if err := s.ensureWriteReady(op); err != nil {
		return WorkspaceMember{}, err
	}

	handle := strings.TrimSpace(in.Handle)
	displayName := strings.TrimSpace(in.DisplayName)
	if displayName == "" {
		displayName = handle
	}

	var violations []errs.FieldViolation
	if !in.Kind.Valid() {
		violations = append(violations, errs.FieldViolation{
			Field:       "kind",
			Description: "kind must be 'human' or 'agent'",
		})
	}
	if len(handle) < 2 || len(handle) > 64 || !memberHandlePattern.MatchString(handle) {
		violations = append(violations, errs.FieldViolation{
			Field:       "handle",
			Description: "handle must be 2-64 alphanumeric, underscore, dot, or hyphen characters",
		})
	}
	if utf8.RuneCountInString(displayName) > maxMemberDisplayNameRunes {
		violations = append(violations, errs.FieldViolation{
			Field:       "display_name",
			Description: "display_name must not exceed 120 characters",
		})
	}
	if !isValidRole(in.Role) {
		violations = append(violations, errs.FieldViolation{
			Field:       "role",
			Description: "role must be owner, maintainer, contributor, or reader",
		})
	}
	if len(violations) > 0 {
		return WorkspaceMember{}, errs.Validation(op, "invalid workspace member parameters", violations...)
	}

	actorID := in.ActorID
	if actorID == uuid.Nil {
		actorID = newUUID()
	}

	var persisted WorkspaceMember
	if err := s.txMgr.WithTransaction(ctx, func(ctx context.Context, scope tx.Scope) error {
		scopedRepo := s.repo.WithScope(scope)
		if _, err := scopedRepo.GetByID(ctx, in.WorkspaceID); err != nil {
			return err
		}

		existingMembers, err := scopedRepo.ListMembers(ctx, in.WorkspaceID)
		if err != nil {
			return err
		}

		now := time.Now().UTC()
		createdAt := now
		action := tx.AuditActionCreate
		effectiveRole := in.Role
		var oldSnapshot []byte

		for _, existing := range existingMembers {
			if existing.ActorID == actorID || (in.ActorID == uuid.Nil && strings.EqualFold(existing.Handle, handle)) {
				actorID = existing.ActorID
				action = tx.AuditActionUpdate
				if !existing.CreatedAt.IsZero() {
					createdAt = existing.CreatedAt
				}
				if existing.Role == MemberRoleOwner && in.ActorID == uuid.Nil {
					effectiveRole = MemberRoleOwner
				}
				oldSnapshot, err = marshalSnapshot(op, existing)
				if err != nil {
					return err
				}
				break
			}
		}

		member := WorkspaceMember{
			WorkspaceID: in.WorkspaceID,
			ActorID:     actorID,
			Kind:        in.Kind,
			Handle:      handle,
			DisplayName: displayName,
			Role:        effectiveRole,
			CreatedAt:   createdAt,
		}
		if err := scopedRepo.UpsertMember(ctx, member); err != nil {
			return err
		}
		newSnapshot, err := marshalSnapshot(op, member)
		if err != nil {
			return err
		}
		if err := scope.Audit().Record(ctx, tx.AuditEntry{
			ID:           newUUID(),
			WorkspaceID:  in.WorkspaceID,
			ActorID:      caller.ActorID,
			Action:       action,
			ResourceType: "workspace_member",
			ResourceID:   member.ActorID.String(),
			OldValues:    oldSnapshot,
			NewValues:    newSnapshot,
			OccurredAt:   now,
		}); err != nil {
			return err
		}
		persisted = member
		return nil
	}); err != nil {
		return WorkspaceMember{}, err
	}

	return persisted, nil
}

// ListMembers returns defensive copies of all registered actors in workspaceID
// after verifying caller workspace ownership.
func (s *ServiceSkeleton) ListMembers(ctx context.Context, workspaceID uuid.UUID) ([]WorkspaceMember, error) {
	const op = "workspace.Service.ListMembers"
	if err := checkContext(ctx, op); err != nil {
		return nil, err
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, err
	}
	if err := s.ensureReadReady(op); err != nil {
		return nil, err
	}

	members, err := s.repo.ListMembers(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	out := make([]WorkspaceMember, len(members))
	copy(out, members)
	return out, nil
}

func (s *ServiceSkeleton) ensureReadReady(op string) error {
	if s == nil || s.repo == nil {
		return errs.New(errs.ErrPreconditionFailed, op, "workspace repository is not initialized")
	}
	return nil
}

func (s *ServiceSkeleton) ensureWriteReady(op string) error {
	if err := s.ensureReadReady(op); err != nil {
		return err
	}
	if s.txMgr == nil {
		return errs.New(errs.ErrPreconditionFailed, op, "transaction manager is not initialized")
	}
	return nil
}

func checkContext(ctx context.Context, op string) error {
	if ctx == nil {
		return errs.New(errs.ErrUnauthenticated, op, "missing request context")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

func isValidRole(r MemberRole) bool {
	switch r {
	case MemberRoleOwner, MemberRoleMaintainer, MemberRoleContributor, MemberRoleReader:
		return true
	default:
		return false
	}
}

func validateLabels(labels map[string]string) error {
	for k := range labels {
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("label keys must not be empty")
		}
	}
	return nil
}

func cloneLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = v
	}
	return out
}

func marshalSnapshot(op string, v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, errs.Wrap(errs.ErrValidation, op, "failed to encode audit snapshot", err)
	}
	return b, nil
}

func newUUID() uuid.UUID {
	if id, err := uuid.NewV7(); err == nil {
		return id
	}
	return uuid.New()
}
