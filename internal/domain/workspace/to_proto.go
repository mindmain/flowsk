// File to_proto.go provides outbound-only ToProto serialization helpers mapping
// pure domain Workspace and WorkspaceMember entities onto flowsk.workspace.v1.
package workspace

import (
	"time"

	"github.com/google/uuid"
	flowsk_workspace_pb "github.com/mindmain/flowsk/gen/go/flowsk/workspace/v1"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ActorKindToProto translates a pure domain authctx.ActorKind into its wire-level
// flowsk.workspace.v1.ActorKind enum value.
//
// Architectural Invariant (`ocinsh-connectrpc-go-service`):
//   - Domain models expose one-way `ToProto()` serialization helpers only.
//   - Generic `FromProto()` constructors on domain aggregates are strictly forbidden;
//     inbound request translation occurs explicitly at transport boundaries.
func ActorKindToProto(kind authctx.ActorKind) flowsk_workspace_pb.ActorKind {
	switch kind {
	case authctx.ActorKindHuman:
		return flowsk_workspace_pb.ActorKind_ACTOR_KIND_HUMAN
	case authctx.ActorKindAgent:
		return flowsk_workspace_pb.ActorKind_ACTOR_KIND_AGENT
	default:
		return flowsk_workspace_pb.ActorKind_ACTOR_KIND_UNSPECIFIED
	}
}

// ToProto serializes a domain Workspace aggregate into its wire-level
// flowsk.workspace.v1.Workspace Protobuf representation.
// Returns nil safely when invoked on a nil *Workspace receiver.
func (w *Workspace) ToProto() *flowsk_workspace_pb.Workspace {
	if w == nil {
		return nil
	}
	return &flowsk_workspace_pb.Workspace{
		Id:          uuidToString(w.ID),
		Slug:        w.Slug,
		Name:        w.Name,
		Description: w.Description,
		CreatedAt:   timeToProto(w.CreatedAt),
		UpdatedAt:   timeToProto(w.UpdatedAt),
	}
}

// ToProto serializes a domain WorkspaceMember into its wire-level
// flowsk.workspace.v1.Actor Protobuf representation.
// Returns nil safely when invoked on a nil *WorkspaceMember receiver.
func (m *WorkspaceMember) ToProto() *flowsk_workspace_pb.Actor {
	if m == nil {
		return nil
	}
	createdAt := timeToProto(m.CreatedAt)
	return &flowsk_workspace_pb.Actor{
		Id:          uuidToString(m.ActorID),
		WorkspaceId: uuidToString(m.WorkspaceID),
		Kind:        ActorKindToProto(m.Kind),
		Handle:      m.Handle,
		DisplayName: m.DisplayName,
		RoleTitle:   string(m.Role),
		IsActive:    true,
		CreatedAt:   createdAt,
		UpdatedAt:   createdAt,
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
