// File service.go implements the pure domain read-side query service for
// inspecting workspace-scoped immutable audit history.
package audit

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/tx"
)

const (
	defaultQueryLimit int32 = 50
	maxQueryLimit     int32 = 200
)

// Service defines read-only governance and timeline inspection operations over
// immutable Workspace audit logs.
type Service interface {
	// GetRecord retrieves a single audit entry by ID within workspaceID after
	// verifying caller workspace ownership.
	GetRecord(ctx context.Context, workspaceID, auditID uuid.UUID) (Record, error)

	// QueryRecords lists audit entries matching filter within filter.WorkspaceID
	// after verifying caller workspace ownership and applying safe pagination bounds.
	QueryRecords(ctx context.Context, filter QueryFilter) ([]Record, error)
}

// ServiceSkeleton implements audit.Service with mandatory multi-tenant workspace
// ownership checks, safe pagination clamping, and defensive deep copies.
// The exported type name ServiceSkeleton is retained for cross-package wiring compatibility.
type ServiceSkeleton struct {
	repo  Repository
	txMgr tx.Manager
}

// Compile-time check ensuring *ServiceSkeleton satisfies Service.
var _ Service = (*ServiceSkeleton)(nil)

// NewService constructs an audit ServiceSkeleton with resolved Repository and
// optional tx.Manager dependencies.
func NewService(repo Repository, txMgr tx.Manager) *ServiceSkeleton {
	return &ServiceSkeleton{
		repo:  repo,
		txMgr: txMgr,
	}
}

// TxManager returns the optional transaction manager bound to this audit service.
func (s *ServiceSkeleton) TxManager() tx.Manager {
	if s == nil {
		return nil
	}
	return s.txMgr
}

// GetRecord retrieves a single audit entry by ID within workspaceID after
// verifying caller workspace ownership, returning a defensive Clone().
func (s *ServiceSkeleton) GetRecord(ctx context.Context, workspaceID, auditID uuid.UUID) (Record, error) {
	const op = "audit.Service.GetRecord"
	if err := checkContext(ctx, op); err != nil {
		return Record{}, err
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return Record{}, err
	}
	if auditID == uuid.Nil {
		return Record{}, errs.Validation(op, "audit_id is required", errs.FieldViolation{
			Field:       "audit_id",
			Description: "audit_id must be a non-nil UUID",
		})
	}
	if s == nil || s.repo == nil {
		return Record{}, errs.New(errs.ErrPreconditionFailed, op, "audit repository is not initialized")
	}

	rec, err := s.repo.GetByID(ctx, workspaceID, auditID)
	if err != nil {
		return Record{}, err
	}
	return rec.Clone(), nil
}

// QueryRecords lists audit entries matching filter within filter.WorkspaceID
// after verifying caller workspace ownership, validating filter bounds, and
// returning defensive deep copies via Record.Clone().
func (s *ServiceSkeleton) QueryRecords(ctx context.Context, filter QueryFilter) ([]Record, error) {
	const op = "audit.Service.QueryRecords"
	if err := checkContext(ctx, op); err != nil {
		return nil, err
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, filter.WorkspaceID); err != nil {
		return nil, err
	}
	if s == nil || s.repo == nil {
		return nil, errs.New(errs.ErrPreconditionFailed, op, "audit repository is not initialized")
	}

	var violations []errs.FieldViolation
	if filter.Action != "" && !filter.Action.Valid() {
		violations = append(violations, errs.FieldViolation{
			Field:       "action",
			Description: "action must be a canonical AuditAction constant",
		})
	}
	if filter.Offset < 0 {
		violations = append(violations, errs.FieldViolation{
			Field:       "offset",
			Description: "offset must be >= 0",
		})
	}
	if filter.Since != nil && filter.Until != nil && filter.Since.After(*filter.Until) {
		violations = append(violations, errs.FieldViolation{
			Field:       "since",
			Description: "since timestamp must not be after until timestamp",
		})
	}
	if len(violations) > 0 {
		return nil, errs.Validation(op, "invalid audit query filter", violations...)
	}

	normalized := QueryFilter{
		WorkspaceID:  filter.WorkspaceID,
		ActorID:      filter.ActorID,
		Action:       filter.Action,
		ResourceType: strings.TrimSpace(filter.ResourceType),
		ResourceID:   strings.TrimSpace(filter.ResourceID),
		Limit:        filter.Limit,
		Offset:       filter.Offset,
	}
	if normalized.Limit <= 0 {
		normalized.Limit = defaultQueryLimit
	} else if normalized.Limit > maxQueryLimit {
		normalized.Limit = maxQueryLimit
	}
	if filter.Since != nil {
		sTime := filter.Since.UTC()
		normalized.Since = &sTime
	}
	if filter.Until != nil {
		uTime := filter.Until.UTC()
		normalized.Until = &uTime
	}

	records, err := s.repo.Query(ctx, normalized)
	if err != nil {
		return nil, err
	}
	out := make([]Record, len(records))
	for i := range records {
		out[i] = records[i].Clone()
	}
	return out, nil
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
