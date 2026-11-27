// File to_proto.go provides outbound-only ToProto serialization helpers mapping
// pure domain audit Records onto flowsk.audit.v1 Protobuf wire messages.
package audit

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	flowsk_audit_pb "github.com/mindmain/flowsk/gen/go/flowsk/audit/v1"
	"github.com/mindmain/flowsk/internal/domain/tx"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ActionToProto maps a canonical domain tx.AuditAction constant onto its
// wire-level flowsk.audit.v1.AuditAction Protobuf enum value.
func ActionToProto(action tx.AuditAction) flowsk_audit_pb.AuditAction {
	switch action {
	case tx.AuditActionCreate:
		return flowsk_audit_pb.AuditAction_AUDIT_ACTION_CREATE
	case tx.AuditActionUpdate:
		return flowsk_audit_pb.AuditAction_AUDIT_ACTION_UPDATE
	case tx.AuditActionDelete:
		return flowsk_audit_pb.AuditAction_AUDIT_ACTION_DELETE
	case tx.AuditActionTransition:
		return flowsk_audit_pb.AuditAction_AUDIT_ACTION_TRANSITION
	case tx.AuditActionClaim:
		return flowsk_audit_pb.AuditAction_AUDIT_ACTION_CLAIM
	case tx.AuditActionRelease:
		return flowsk_audit_pb.AuditAction_AUDIT_ACTION_RELEASE
	case tx.AuditActionBatchExec:
		return flowsk_audit_pb.AuditAction_AUDIT_ACTION_BATCH_EXEC
	default:
		return flowsk_audit_pb.AuditAction_AUDIT_ACTION_UNSPECIFIED
	}
}

// auditDiffEnvelope formats OldValues and NewValues JSON snapshots into a single
// deterministic JSON payload string for wire transmission.
type auditDiffEnvelope struct {
	OldValues json.RawMessage `json:"old_values,omitempty"`
	NewValues json.RawMessage `json:"new_values,omitempty"`
}

// ToProto serializes an immutable domain audit Record into its wire-level
// flowsk.audit.v1.AuditEvent Protobuf representation.
//
// Architectural Invariant (`ocinsh-connectrpc-go-service`):
//   - Safely returns nil when invoked on a nil *Record receiver.
//   - Copies JSON snapshot bytes defensively and never defines `FromProto()`
//     constructors on domain models.
func (r *Record) ToProto() *flowsk_audit_pb.AuditEvent {
	if r == nil {
		return nil
	}

	var entityID string
	var entityIdentifier string
	trimmedResID := strings.TrimSpace(r.ResourceID)
	if parsed, err := uuid.Parse(trimmedResID); err == nil && parsed != uuid.Nil {
		entityID = parsed.String()
	} else {
		entityIdentifier = r.ResourceID
		leading := strings.TrimSpace(strings.Split(trimmedResID, ":")[0])
		if parsedLead, leadErr := uuid.Parse(leading); leadErr == nil && parsedLead != uuid.Nil {
			entityID = parsedLead.String()
		} else if r.WorkspaceID != uuid.Nil {
			entityID = r.WorkspaceID.String()
		} else {
			entityID = uuid.Nil.String()
		}
	}

	actorID := uuid.Nil.String()
	if r.ActorID != uuid.Nil {
		actorID = r.ActorID.String()
	}

	summary := strings.TrimSpace(fmt.Sprintf("%s %s %s", r.Action, r.ResourceType, r.ResourceID))

	return &flowsk_audit_pb.AuditEvent{
		Id:               uuidOrNilString(r.ID),
		WorkspaceId:      uuidOrNilString(r.WorkspaceID),
		ActorId:          actorID,
		Action:           ActionToProto(r.Action),
		EntityType:       r.ResourceType,
		EntityId:         entityID,
		EntityIdentifier: entityIdentifier,
		Summary:          summary,
		PayloadJson:      encodeAuditPayloadJSON(r.OldValues, r.NewValues),
		OccurredAt:       timeToProto(r.OccurredAt),
	}
}

func encodeAuditPayloadJSON(oldVals, newVals []byte) string {
	if len(oldVals) == 0 && len(newVals) == 0 {
		return ""
	}
	var env auditDiffEnvelope
	if len(oldVals) > 0 && json.Valid(oldVals) {
		copied := make([]byte, len(oldVals))
		copy(copied, oldVals)
		env.OldValues = copied
	}
	if len(newVals) > 0 && json.Valid(newVals) {
		copied := make([]byte, len(newVals))
		copy(copied, newVals)
		env.NewValues = copied
	}
	if len(env.OldValues) == 0 && len(env.NewValues) == 0 {
		return ""
	}
	encoded, err := json.Marshal(env)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func uuidOrNilString(id uuid.UUID) string {
	if id == uuid.Nil {
		return uuid.Nil.String()
	}
	return id.String()
}

func timeToProto(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t.UTC())
}
