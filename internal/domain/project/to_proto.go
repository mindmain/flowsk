// File to_proto.go provides outbound-only ToProto serialization helpers mapping
// pure domain Project aggregates onto flowsk.project.v1 Protobuf messages.
package project

import (
	"strings"
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

// WorkflowStateCategoryToProto maps a canonical workflow state category string
// onto its wire-level flowsk.project.v1.WorkflowStateCategory enum value.
func WorkflowStateCategoryToProto(category string) flowsk_project_pb.WorkflowStateCategory {
	switch strings.ToLower(strings.TrimSpace(category)) {
	case "triage":
		return flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_TRIAGE
	case "backlog":
		return flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_BACKLOG
	case "unstarted":
		return flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_UNSTARTED
	case "started":
		return flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_STARTED
	case "completed":
		return flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_COMPLETED
	case "canceled", "cancelled":
		return flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_CANCELED
	case "archived":
		return flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_ARCHIVED
	default:
		return flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_UNSPECIFIED
	}
}

// ToProto serializes a domain WorkflowState into its wire-level
// flowsk.project.v1.WorkflowState Protobuf representation.
func (s *WorkflowState) ToProto() *flowsk_project_pb.WorkflowState {
	if s == nil {
		return nil
	}
	return &flowsk_project_pb.WorkflowState{
		Id:          uuidToString(s.ID),
		WorkspaceId: uuidToString(s.WorkspaceID),
		ProjectId:   uuidToString(s.ProjectID),
		Name:        s.Name,
		Description: s.Description,
		Category:    WorkflowStateCategoryToProto(s.Category),
		ColorHex:    s.ColorHex,
		Position:    s.Position,
		IsDefault:   s.IsDefault,
		CreatedAt:   timeToProto(s.CreatedAt),
		UpdatedAt:   timeToProto(s.UpdatedAt),
	}
}

// ToProto serializes a domain ProjectLabel into its wire-level
// flowsk.project.v1.Label Protobuf representation.
func (l *ProjectLabel) ToProto() *flowsk_project_pb.Label {
	if l == nil {
		return nil
	}
	return &flowsk_project_pb.Label{
		Id:          uuidToString(l.ID),
		WorkspaceId: uuidToString(l.WorkspaceID),
		ProjectId:   uuidToString(l.ProjectID),
		Name:        l.Name,
		Description: l.Description,
		ColorHex:    l.ColorHex,
		CreatedAt:   timeToProto(l.CreatedAt),
		UpdatedAt:   timeToProto(l.UpdatedAt),
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
