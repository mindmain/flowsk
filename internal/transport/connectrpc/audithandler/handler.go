// Package audithandler implements the Buf ConnectRPC `AuditServiceHandler`
// transport adapter for querying immutable, workspace-scoped audit trails.
package audithandler

import (
	"context"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	flowsk_audit_pb "github.com/mindmain/flowsk/gen/go/flowsk/audit/v1"
	"github.com/mindmain/flowsk/gen/go/flowsk/audit/v1/flowsk_audit_pbconnect"
	"github.com/mindmain/flowsk/internal/adapter/db"
	"github.com/mindmain/flowsk/internal/domain/audit"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/tx"
	"github.com/mindmain/flowsk/internal/transport/connectrpc"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const maxAuditPageSize int32 = 200

// DataDependencies defines the segregated persistence provider contracts consumed
// by `New` to extract concrete audit repository and transaction dependencies.
type DataDependencies interface {
	db.TxManagerProvider
	db.AuditRepoProvider
}

// ServicesDependencies defines the segregated application service provider contract
// consumed by `New` to extract the audit domain service.
type ServicesDependencies interface {
	// AuditService returns the resolved audit domain service.
	AuditService() audit.Service
}

// Handler implements `flowsk_audit_pbconnect.AuditServiceHandler` with pre-resolved
// domain dependencies and zero stored provider wrappers.
type Handler struct {
	txMgr tx.Manager
	repo  audit.Repository
	svc   audit.Service
}

// Compile-time assertion ensuring *Handler satisfies AuditServiceHandler.
var _ flowsk_audit_pbconnect.AuditServiceHandler = (*Handler)(nil)

// New resolves `DataDependencies` and `ServicesDependencies` immediately at startup
// and returns an immutable `AuditService` ConnectRPC handler.
func New(dataDeps DataDependencies, svcDeps ServicesDependencies) *Handler {
	var (
		txMgr tx.Manager
		repo  audit.Repository
		svc   audit.Service
	)
	if dataDeps != nil {
		txMgr = dataDeps.TxManager()
		repo = dataDeps.AuditRepository()
	}
	if svcDeps != nil {
		svc = svcDeps.AuditService()
	}
	if svc == nil {
		svc = audit.NewService(repo, txMgr)
	}
	return &Handler{
		txMgr: txMgr,
		repo:  repo,
		svc:   svc,
	}
}

// Service returns the resolved `audit.Service` bound to this handler.
func (h *Handler) Service() audit.Service {
	if h == nil {
		return nil
	}
	return h.svc
}

// Repository returns the resolved `audit.Repository` bound to this handler.
func (h *Handler) Repository() audit.Repository {
	if h == nil {
		return nil
	}
	return h.repo
}

// TxManager returns the resolved `tx.Manager` bound to this handler.
func (h *Handler) TxManager() tx.Manager {
	if h == nil {
		return nil
	}
	return h.txMgr
}

// ListAuditEvents queries immutable workspace audit trail records ordered newest-first.
func (h *Handler) ListAuditEvents(
	ctx context.Context,
	req *connect.Request[flowsk_audit_pb.ListAuditEventsRequest],
) (*connect.Response[flowsk_audit_pb.ListAuditEventsResponse], error) {
	const op = "audithandler.ListAuditEvents"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}

	workspaceID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseOptionalUUID(op, "project_id", req.Msg.ProjectId); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	targetActorIDPtr, err := connectrpc.ParseOptionalUUID(op, "target_actor_id", req.Msg.TargetActorId)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	entityIDPtr, err := connectrpc.ParseOptionalUUID(op, "entity_id", req.Msg.EntityId)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	action, err := parseAuditAction(op, req.Msg.GetActionFilter())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	limit, offset, err := connectrpc.ParsePagination(op, req.Msg.GetPageSize(), maxAuditPageSize, req.Msg.GetPageToken())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	since := timestampToTimePtr(req.Msg.GetSince())
	until := timestampToTimePtr(req.Msg.GetUntil())
	if since != nil && until != nil && until.Before(*since) {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "invalid audit time range", errs.FieldViolation{
			Field:       "until",
			Description: "until must not precede since",
		}))
	}

	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	var targetActorID uuid.UUID
	if targetActorIDPtr != nil {
		targetActorID = *targetActorIDPtr
	}
	var resourceID string
	if entityIDPtr != nil {
		resourceID = entityIDPtr.String()
	}

	records, err := h.svc.QueryRecords(ctx, audit.QueryFilter{
		WorkspaceID:  workspaceID,
		ActorID:      targetActorID,
		Action:       action,
		ResourceType: strings.TrimSpace(req.Msg.GetEntityTypeFilter()),
		ResourceID:   resourceID,
		Since:        since,
		Until:        until,
		Limit:        limit,
		Offset:       offset,
	})
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	out := make([]*flowsk_audit_pb.AuditEvent, 0, len(records))
	for i := range records {
		out = append(out, records[i].ToProto())
	}
	return connect.NewResponse(&flowsk_audit_pb.ListAuditEventsResponse{
		Events: out,
	}), nil
}

// GetAuditEvent retrieves a single immutable audit trail record by UUID.
func (h *Handler) GetAuditEvent(
	ctx context.Context,
	req *connect.Request[flowsk_audit_pb.GetAuditEventRequest],
) (*connect.Response[flowsk_audit_pb.GetAuditEventResponse], error) {
	const op = "audithandler.GetAuditEvent"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}

	workspaceID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	auditEventID, err := connectrpc.ParseRequiredUUID(op, "audit_event_id", req.Msg.GetAuditEventId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	rec, err := h.svc.GetRecord(ctx, workspaceID, auditEventID)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_audit_pb.GetAuditEventResponse{
		Event: rec.ToProto(),
	}), nil
}

func parseAuditAction(op string, a flowsk_audit_pb.AuditAction) (tx.AuditAction, error) {
	switch a {
	case flowsk_audit_pb.AuditAction_AUDIT_ACTION_UNSPECIFIED:
		return "", nil
	case flowsk_audit_pb.AuditAction_AUDIT_ACTION_CREATE:
		return tx.AuditActionCreate, nil
	case flowsk_audit_pb.AuditAction_AUDIT_ACTION_UPDATE:
		return tx.AuditActionUpdate, nil
	case flowsk_audit_pb.AuditAction_AUDIT_ACTION_DELETE:
		return tx.AuditActionDelete, nil
	case flowsk_audit_pb.AuditAction_AUDIT_ACTION_TRANSITION:
		return tx.AuditActionTransition, nil
	case flowsk_audit_pb.AuditAction_AUDIT_ACTION_CLAIM:
		return tx.AuditActionClaim, nil
	case flowsk_audit_pb.AuditAction_AUDIT_ACTION_RELEASE:
		return tx.AuditActionRelease, nil
	case flowsk_audit_pb.AuditAction_AUDIT_ACTION_BATCH_EXEC:
		return tx.AuditActionBatchExec, nil
	default:
		return "", errs.Validation(op, "invalid action_filter", errs.FieldViolation{
			Field:       "action_filter",
			Description: "unsupported AuditAction enum value",
		})
	}
}

func timestampToTimePtr(ts *timestamppb.Timestamp) *time.Time {
	if ts == nil || !ts.IsValid() {
		return nil
	}
	t := ts.AsTime().UTC()
	return &t
}
