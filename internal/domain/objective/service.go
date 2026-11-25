// File service.go implements the pure domain application service for managing
// strategic project Objectives with strict workspace ownership and atomic audits.
package objective

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
	defaultListObjectivesLimit int32 = 50
	maxListObjectivesLimit     int32 = 200
	maxObjectiveTitleRunes           = 200
	maxObjectiveSummaryRunes         = 8000
)

var objectiveSlugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Service defines pure domain operations for managing strategic Objectives within Projects.
type Service interface {
	// CreateObjective validates and persists a new Objective inside an atomic audited transaction.
	CreateObjective(ctx context.Context, in CreateObjectiveInput) (Objective, error)

	// GetObjective retrieves an Objective by ID within workspaceID after verifying tenant ownership.
	GetObjective(ctx context.Context, workspaceID, objectiveID uuid.UUID) (Objective, error)

	// GetObjectiveBySlug resolves an Objective by project-scoped slug after verifying tenant ownership.
	GetObjectiveBySlug(ctx context.Context, workspaceID, projectID uuid.UUID, slug string) (Objective, error)

	// ListObjectives returns defensive copies of Objectives matching filter within workspaceID.
	ListObjectives(ctx context.Context, filter ListObjectivesFilter) ([]Objective, error)

	// UpdateObjective modifies an existing Objective inside an atomic audited transaction.
	UpdateObjective(ctx context.Context, in UpdateObjectiveInput) (Objective, error)

	// DeleteObjective soft-deletes an Objective inside an atomic audited transaction.
	DeleteObjective(ctx context.Context, workspaceID, objectiveID uuid.UUID) error
}

// ServiceSkeleton implements Service with multi-tenant workspace isolation,
// defensive slice/pointer cloning, and atomic transaction audit logging.
// The exported type name ServiceSkeleton is retained for cross-package wiring compatibility.
type ServiceSkeleton struct {
	repo  Repository
	txMgr tx.Manager
}

// Compile-time check ensuring *ServiceSkeleton satisfies Service.
var _ Service = (*ServiceSkeleton)(nil)

// NewService constructs an objective ServiceSkeleton with resolved Repository and
// tx.Manager dependencies.
func NewService(repo Repository, txMgr tx.Manager) *ServiceSkeleton {
	return &ServiceSkeleton{
		repo:  repo,
		txMgr: txMgr,
	}
}

// CreateObjective validates and persists a new Objective inside an atomic audited transaction.
func (s *ServiceSkeleton) CreateObjective(ctx context.Context, in CreateObjectiveInput) (Objective, error) {
	const op = "objective.Service.CreateObjective"
	if err := checkContext(ctx, op); err != nil {
		return Objective{}, err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID)
	if err != nil {
		return Objective{}, err
	}
	if err := s.ensureWriteReady(op); err != nil {
		return Objective{}, err
	}

	slug := strings.TrimSpace(in.Slug)
	title := strings.TrimSpace(in.Title)
	summary := strings.TrimSpace(in.Summary)
	kind := in.Kind
	if kind == "" {
		kind = KindGoal
	}
	status := in.Status
	if status == "" {
		status = StatusActive
	}

	var violations []errs.FieldViolation
	if in.ProjectID == uuid.Nil {
		violations = append(violations, errs.FieldViolation{
			Field:       "project_id",
			Description: "project_id must be a non-nil UUID",
		})
	}
	if !kind.Valid() {
		violations = append(violations, errs.FieldViolation{
			Field:       "kind",
			Description: "kind must be goal, milestone, or cycle",
		})
	}
	if len(slug) < 2 || len(slug) > 64 || !objectiveSlugPattern.MatchString(slug) {
		violations = append(violations, errs.FieldViolation{
			Field:       "slug",
			Description: "slug must be 2-64 lowercase ASCII alphanumeric characters separated by single hyphens",
		})
	}
	if title == "" || utf8.RuneCountInString(title) > maxObjectiveTitleRunes {
		violations = append(violations, errs.FieldViolation{
			Field:       "title",
			Description: "title must be between 1 and 200 characters",
		})
	}
	if utf8.RuneCountInString(summary) > maxObjectiveSummaryRunes {
		violations = append(violations, errs.FieldViolation{
			Field:       "summary",
			Description: "summary must not exceed 8000 characters",
		})
	}
	if !status.Valid() {
		violations = append(violations, errs.FieldViolation{
			Field:       "status",
			Description: "status must be draft, active, at_risk, achieved, or dropped",
		})
	}
	criteria, critViolation := sanitizeCriteria(in.SuccessCriteria)
	if critViolation != nil {
		violations = append(violations, *critViolation)
	}
	if len(violations) > 0 {
		return Objective{}, errs.Validation(op, "invalid objective creation parameters", violations...)
	}

	now := time.Now().UTC()
	var targetDate *time.Time
	if in.TargetDate != nil {
		td := in.TargetDate.UTC()
		targetDate = &td
	}

	obj := Objective{
		ID:              newUUID(),
		WorkspaceID:     in.WorkspaceID,
		ProjectID:       in.ProjectID,
		Kind:            kind,
		Slug:            slug,
		Title:           title,
		Summary:         summary,
		SuccessCriteria: criteria,
		Status:          status,
		PriorityRank:    in.PriorityRank,
		TargetDate:      targetDate,
		CreatedBy:       caller.ActorID,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	if err := s.txMgr.WithTransaction(ctx, func(ctx context.Context, scope tx.Scope) error {
		scopedRepo := s.repo.WithScope(scope)
		if err := scopedRepo.Create(ctx, obj.Clone()); err != nil {
			return err
		}
		newSnapshot, err := marshalSnapshot(op, obj)
		if err != nil {
			return err
		}
		return scope.Audit().Record(ctx, tx.AuditEntry{
			ID:           newUUID(),
			WorkspaceID:  in.WorkspaceID,
			ActorID:      caller.ActorID,
			Action:       tx.AuditActionCreate,
			ResourceType: "objective",
			ResourceID:   obj.ID.String(),
			NewValues:    newSnapshot,
			OccurredAt:   now,
		})
	}); err != nil {
		return Objective{}, err
	}

	return obj.Clone(), nil
}

// GetObjective retrieves an Objective by ID within workspaceID after verifying tenant ownership.
func (s *ServiceSkeleton) GetObjective(ctx context.Context, workspaceID, objectiveID uuid.UUID) (Objective, error) {
	const op = "objective.Service.GetObjective"
	if err := checkContext(ctx, op); err != nil {
		return Objective{}, err
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return Objective{}, err
	}
	if objectiveID == uuid.Nil {
		return Objective{}, errs.Validation(op, "objective_id is required", errs.FieldViolation{
			Field:       "objective_id",
			Description: "objective_id must be a non-nil UUID",
		})
	}
	if err := s.ensureReadReady(op); err != nil {
		return Objective{}, err
	}

	obj, err := s.repo.GetByID(ctx, workspaceID, objectiveID)
	if err != nil {
		return Objective{}, err
	}
	return obj.Clone(), nil
}

// GetObjectiveBySlug resolves an Objective by project-scoped slug after verifying tenant ownership.
func (s *ServiceSkeleton) GetObjectiveBySlug(ctx context.Context, workspaceID, projectID uuid.UUID, slug string) (Objective, error) {
	const op = "objective.Service.GetObjectiveBySlug"
	if err := checkContext(ctx, op); err != nil {
		return Objective{}, err
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return Objective{}, err
	}
	var violations []errs.FieldViolation
	if projectID == uuid.Nil {
		violations = append(violations, errs.FieldViolation{
			Field:       "project_id",
			Description: "project_id must be a non-nil UUID",
		})
	}
	normalized := strings.TrimSpace(slug)
	if normalized == "" {
		violations = append(violations, errs.FieldViolation{
			Field:       "slug",
			Description: "slug must not be empty",
		})
	}
	if len(violations) > 0 {
		return Objective{}, errs.Validation(op, "invalid objective slug lookup parameters", violations...)
	}
	if err := s.ensureReadReady(op); err != nil {
		return Objective{}, err
	}

	obj, err := s.repo.GetBySlug(ctx, workspaceID, projectID, normalized)
	if err != nil {
		return Objective{}, err
	}
	return obj.Clone(), nil
}

// ListObjectives returns defensive copies of Objectives matching filter within workspaceID.
func (s *ServiceSkeleton) ListObjectives(ctx context.Context, filter ListObjectivesFilter) ([]Objective, error) {
	const op = "objective.Service.ListObjectives"
	if err := checkContext(ctx, op); err != nil {
		return nil, err
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, filter.WorkspaceID); err != nil {
		return nil, err
	}
	if err := s.ensureReadReady(op); err != nil {
		return nil, err
	}

	var violations []errs.FieldViolation
	if filter.Kind != nil && !filter.Kind.Valid() {
		violations = append(violations, errs.FieldViolation{
			Field:       "kind",
			Description: "kind filter must be goal, milestone, or cycle",
		})
	}
	if filter.Status != nil && !filter.Status.Valid() {
		violations = append(violations, errs.FieldViolation{
			Field:       "status",
			Description: "status filter must be draft, active, at_risk, achieved, or dropped",
		})
	}
	if filter.Offset < 0 {
		violations = append(violations, errs.FieldViolation{
			Field:       "offset",
			Description: "offset must be >= 0",
		})
	}
	if len(violations) > 0 {
		return nil, errs.Validation(op, "invalid objective list filter", violations...)
	}

	normalized := filter
	if normalized.Limit <= 0 {
		normalized.Limit = defaultListObjectivesLimit
	} else if normalized.Limit > maxListObjectivesLimit {
		normalized.Limit = maxListObjectivesLimit
	}
	if filter.Kind != nil {
		kd := *filter.Kind
		normalized.Kind = &kd
	}
	if filter.Status != nil {
		st := *filter.Status
		normalized.Status = &st
	}

	items, err := s.repo.List(ctx, normalized)
	if err != nil {
		return nil, err
	}
	out := make([]Objective, len(items))
	for i := range items {
		out[i] = items[i].Clone()
	}
	return out, nil
}

// UpdateObjective modifies an existing Objective inside an atomic audited transaction.
func (s *ServiceSkeleton) UpdateObjective(ctx context.Context, in UpdateObjectiveInput) (Objective, error) {
	const op = "objective.Service.UpdateObjective"
	if err := checkContext(ctx, op); err != nil {
		return Objective{}, err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID)
	if err != nil {
		return Objective{}, err
	}
	if err := s.ensureWriteReady(op); err != nil {
		return Objective{}, err
	}

	var violations []errs.FieldViolation
	if in.ObjectiveID == uuid.Nil {
		violations = append(violations, errs.FieldViolation{
			Field:       "objective_id",
			Description: "objective_id must be a non-nil UUID",
		})
	}
	if in.Title != nil {
		trimmed := strings.TrimSpace(*in.Title)
		if trimmed == "" || utf8.RuneCountInString(trimmed) > maxObjectiveTitleRunes {
			violations = append(violations, errs.FieldViolation{
				Field:       "title",
				Description: "title must be between 1 and 200 characters",
			})
		}
	}
	if in.Summary != nil && utf8.RuneCountInString(strings.TrimSpace(*in.Summary)) > maxObjectiveSummaryRunes {
		violations = append(violations, errs.FieldViolation{
			Field:       "summary",
			Description: "summary must not exceed 8000 characters",
		})
	}
	if in.Kind != nil && !in.Kind.Valid() {
		violations = append(violations, errs.FieldViolation{
			Field:       "kind",
			Description: "kind must be goal, milestone, or cycle",
		})
	}
	if in.Status != nil && !in.Status.Valid() {
		violations = append(violations, errs.FieldViolation{
			Field:       "status",
			Description: "status must be draft, active, at_risk, achieved, or dropped",
		})
	}
	var nextCriteria []string
	if in.SuccessCriteria != nil {
		var critViolation *errs.FieldViolation
		nextCriteria, critViolation = sanitizeCriteria(in.SuccessCriteria)
		if critViolation != nil {
			violations = append(violations, *critViolation)
		}
	}
	if len(violations) > 0 {
		return Objective{}, errs.Validation(op, "invalid objective update parameters", violations...)
	}

	var updated Objective
	if err := s.txMgr.WithTransaction(ctx, func(ctx context.Context, scope tx.Scope) error {
		scopedRepo := s.repo.WithScope(scope)
		current, err := scopedRepo.GetByID(ctx, in.WorkspaceID, in.ObjectiveID)
		if err != nil {
			return err
		}
		oldSnapshot, err := marshalSnapshot(op, current)
		if err != nil {
			return err
		}

		now := time.Now().UTC()
		next := current.Clone()
		action := tx.AuditActionUpdate

		if in.Kind != nil {
			next.Kind = *in.Kind
		}
		if in.Title != nil {
			next.Title = strings.TrimSpace(*in.Title)
		}
		if in.Summary != nil {
			next.Summary = strings.TrimSpace(*in.Summary)
		}
		if in.SuccessCriteria != nil {
			next.SuccessCriteria = nextCriteria
		}
		if in.Status != nil {
			if next.Status != *in.Status {
				action = tx.AuditActionTransition
			}
			next.Status = *in.Status
		}
		if in.PriorityRank != nil {
			next.PriorityRank = *in.PriorityRank
		}
		if in.TargetDate != nil {
			if in.TargetDate.IsZero() {
				next.TargetDate = nil
			} else {
				td := in.TargetDate.UTC()
				next.TargetDate = &td
			}
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
			Action:       action,
			ResourceType: "objective",
			ResourceID:   in.ObjectiveID.String(),
			OldValues:    oldSnapshot,
			NewValues:    newSnapshot,
			OccurredAt:   now,
		}); err != nil {
			return err
		}
		updated = next
		return nil
	}); err != nil {
		return Objective{}, err
	}

	return updated.Clone(), nil
}

// DeleteObjective soft-deletes an Objective inside an atomic audited transaction.
func (s *ServiceSkeleton) DeleteObjective(ctx context.Context, workspaceID, objectiveID uuid.UUID) error {
	const op = "objective.Service.DeleteObjective"
	if err := checkContext(ctx, op); err != nil {
		return err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID)
	if err != nil {
		return err
	}
	if objectiveID == uuid.Nil {
		return errs.Validation(op, "objective_id is required", errs.FieldViolation{
			Field:       "objective_id",
			Description: "objective_id must be a non-nil UUID",
		})
	}
	if err := s.ensureWriteReady(op); err != nil {
		return err
	}

	return s.txMgr.WithTransaction(ctx, func(ctx context.Context, scope tx.Scope) error {
		scopedRepo := s.repo.WithScope(scope)
		current, err := scopedRepo.GetByID(ctx, workspaceID, objectiveID)
		if err != nil {
			return err
		}
		oldSnapshot, err := marshalSnapshot(op, current)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if err := scopedRepo.SoftDelete(ctx, workspaceID, objectiveID); err != nil {
			return err
		}
		deleted := current.Clone()
		deleted.UpdatedAt = now
		deleted.DeletedAt = &now
		newSnapshot, err := marshalSnapshot(op, deleted)
		if err != nil {
			return err
		}
		return scope.Audit().Record(ctx, tx.AuditEntry{
			ID:           newUUID(),
			WorkspaceID:  workspaceID,
			ActorID:      caller.ActorID,
			Action:       tx.AuditActionDelete,
			ResourceType: "objective",
			ResourceID:   objectiveID.String(),
			OldValues:    oldSnapshot,
			NewValues:    newSnapshot,
			OccurredAt:   now,
		})
	})
}

func (s *ServiceSkeleton) ensureReadReady(op string) error {
	if s == nil || s.repo == nil {
		return errs.New(errs.ErrPreconditionFailed, op, "objective repository is not initialized")
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

func sanitizeCriteria(items []string) ([]string, *errs.FieldViolation) {
	if len(items) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(items))
	for _, raw := range items {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			return nil, &errs.FieldViolation{
				Field:       "success_criteria",
				Description: "success_criteria entries must not be empty",
			}
		}
		out = append(out, trimmed)
	}
	return out, nil
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
