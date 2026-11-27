// File to_proto.go provides outbound-only ToProto serialization helpers mapping
// pure domain Objective entities onto flowsk.objective.v1 Protobuf messages.
package objective

import (
	"strings"
	"time"

	"github.com/google/uuid"
	flowsk_objective_pb "github.com/mindmain/flowsk/gen/go/flowsk/objective/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ToProto translates a pure domain Objective Kind into its wire-level
// flowsk.objective.v1.ObjectiveKind enum value.
func (k Kind) ToProto() flowsk_objective_pb.ObjectiveKind {
	switch k {
	case KindMilestone:
		return flowsk_objective_pb.ObjectiveKind_OBJECTIVE_KIND_MILESTONE
	case KindCycle:
		return flowsk_objective_pb.ObjectiveKind_OBJECTIVE_KIND_CYCLE
	case KindGoal, "":
		return flowsk_objective_pb.ObjectiveKind_OBJECTIVE_KIND_GOAL
	default:
		return flowsk_objective_pb.ObjectiveKind_OBJECTIVE_KIND_GOAL
	}
}

// ToProto translates a pure domain Objective Status into its wire-level
// flowsk.objective.v1.ObjectiveStatus enum value.
func (s Status) ToProto() flowsk_objective_pb.ObjectiveStatus {
	switch s {
	case StatusDraft:
		return flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_PLANNED
	case StatusActive:
		return flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_ACTIVE
	case StatusAtRisk:
		return flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_AT_RISK
	case StatusAchieved:
		return flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_ACHIEVED
	case StatusDropped:
		return flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_DROPPED
	default:
		return flowsk_objective_pb.ObjectiveStatus_OBJECTIVE_STATUS_UNSPECIFIED
	}
}

// ToProto serializes a pure domain Objective aggregate into its wire-level
// flowsk.objective.v1.Objective Protobuf message.
//
// Architectural Invariant (`ocinsh-connectrpc-go-service`):
//   - Safely returns nil when invoked on a nil *Objective receiver.
//   - Domain models define outbound `ToProto()` mappers only; inbound Protobuf
//     decoding is performed explicitly inside transport handlers.
func (o *Objective) ToProto() *flowsk_objective_pb.Objective {
	if o == nil {
		return nil
	}

	var closedAt *timestamppb.Timestamp
	if o.Status == StatusAchieved || o.Status == StatusDropped {
		closedAt = timeToProto(o.UpdatedAt)
	}

	return &flowsk_objective_pb.Objective{
		Id:               uuidToString(o.ID),
		WorkspaceId:      uuidToString(o.WorkspaceID),
		ProjectId:        uuidToString(o.ProjectID),
		Kind:             o.Kind.ToProto(),
		Status:           o.Status.ToProto(),
		Title:            o.Title,
		Description:      o.Summary,
		SuccessCriteria:  strings.Join(o.SuccessCriteria, "\n"),
		TargetDate:       timePtrToProto(o.TargetDate),
		ClosedAt:         closedAt,
		CreatedByActorId: uuidToString(o.CreatedBy),
		CreatedAt:        timeToProto(o.CreatedAt),
		UpdatedAt:        timeToProto(o.UpdatedAt),
		Slug:             o.Slug,
		PriorityRank:     o.PriorityRank,
	}
}

func uuidToString(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}

func timeToProto(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t.UTC())
}

func timePtrToProto(t *time.Time) *timestamppb.Timestamp {
	if t == nil || t.IsZero() {
		return nil
	}
	return timestamppb.New(t.UTC())
}
