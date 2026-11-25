// File to_proto.go provides outbound-only ToProto serialization helpers mapping
// pure domain Project aggregates onto flowsk.project.v1 Protobuf messages.
package project

import (
	"time"

	"github.com/google/uuid"
	flowsk_project_pb "github.com/mindmain/flowsk/gen/go/flowsk/project/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ToProto maps a pure domain Project Status onto its wire-level
// flowsk.project.v1.ProjectStatus enum representation.
func (s Status) ToProto() flowsk_project_pb.ProjectStatus {
	switch s {
	case StatusActive:
		return flowsk_project_pb.ProjectStatus_PROJECT_STATUS_ACTIVE
	case StatusPaused:
		return flowsk_project_pb.ProjectStatus_PROJECT_STATUS_PAUSED
	case StatusArchived:
		return flowsk_project_pb.ProjectStatus_PROJECT_STATUS_ARCHIVED
	default:
		return flowsk_project_pb.ProjectStatus_PROJECT_STATUS_UNSPECIFIED
	}
}

// ToProto serializes a pure domain Project aggregate into its wire-level
// flowsk.project.v1.Project Protobuf message.
//
// Architectural & Zero-Path Invariants:
//   - Safely returns nil when invoked on a nil *Project receiver.
//   - Never emits host-local filesystem paths; project directory bindings exist
//     exclusively on client machines (`~/.config/flowsk/local_paths.json`).
//   - Only outbound `ToProto()` serialization is defined on domain entities
//     (`FromProto()` constructors are forbidden per `ocinsh-connectrpc-go-service`).
func (p *Project) ToProto() *flowsk_project_pb.Project {
	if p == nil {
		return nil
	}
	return &flowsk_project_pb.Project{
		Id:               uuidToString(p.ID),
		WorkspaceId:      uuidToString(p.WorkspaceID),
		KeyPrefix:        p.KeyPrefix,
		Slug:             p.Slug,
		Name:             p.Name,
		Description:      p.Description,
		Status:           p.Status.ToProto(),
		IssueCounter:     p.TaskSeq,
		CreatedByActorId: uuidToString(p.CreatedBy),
		CreatedAt:        timeToProto(p.CreatedAt),
		UpdatedAt:        timeToProto(p.UpdatedAt),
		HasMarkdownDoc:   p.HasMarkdownDoc,
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
