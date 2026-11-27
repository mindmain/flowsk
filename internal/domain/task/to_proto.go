// File to_proto.go provides outbound-only ToProto serialization helpers mapping
// pure domain Task, TaskRelation, TaskComment, and ProjectDigest models to Protobuf.
package task

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	flowsk_project_pb "github.com/mindmain/flowsk/gen/go/flowsk/project/v1"
	flowsk_task_pb "github.com/mindmain/flowsk/gen/go/flowsk/task/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ToProto maps a domain Task Kind onto its wire-level flowsk.task.v1.TaskKind enum.
func (k Kind) ToProto() flowsk_task_pb.TaskKind {
	switch k {
	case KindEpic:
		return flowsk_task_pb.TaskKind_TASK_KIND_EPIC
	case KindFeature:
		return flowsk_task_pb.TaskKind_TASK_KIND_FEATURE
	case KindBug:
		return flowsk_task_pb.TaskKind_TASK_KIND_BUG
	case KindChore:
		return flowsk_task_pb.TaskKind_TASK_KIND_TASK
	case KindResearch:
		return flowsk_task_pb.TaskKind_TASK_KIND_ISSUE
	default:
		return flowsk_task_pb.TaskKind_TASK_KIND_UNSPECIFIED
	}
}

// ToProto maps a domain Task Priority (0=Critical .. 3=Low) onto its wire-level
// flowsk.task.v1.TaskPriority enum.
func (p Priority) ToProto() flowsk_task_pb.TaskPriority {
	switch p {
	case PriorityCritical:
		return flowsk_task_pb.TaskPriority_TASK_PRIORITY_URGENT
	case PriorityHigh:
		return flowsk_task_pb.TaskPriority_TASK_PRIORITY_HIGH
	case PriorityMedium:
		return flowsk_task_pb.TaskPriority_TASK_PRIORITY_MEDIUM
	case PriorityLow:
		return flowsk_task_pb.TaskPriority_TASK_PRIORITY_LOW
	default:
		return flowsk_task_pb.TaskPriority_TASK_PRIORITY_UNSPECIFIED
	}
}

// ToProtoStateCategory maps a domain Task Status onto its canonical Linear-style
// flowsk.project.v1.WorkflowStateCategory enum for board and digest rollups.
func (s Status) ToProtoStateCategory() flowsk_project_pb.WorkflowStateCategory {
	switch s {
	case StatusBacklog:
		return flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_BACKLOG
	case StatusReady:
		return flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_UNSTARTED
	case StatusInProgress, StatusInReview, StatusBlocked:
		return flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_STARTED
	case StatusDone:
		return flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_COMPLETED
	case StatusCancelled:
		return flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_CANCELED
	case StatusArchived:
		return flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_ARCHIVED
	default:
		return flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_UNSPECIFIED
	}
}

// ToProto maps a domain RelationType onto its wire-level flowsk.task.v1.TaskRelationType enum.
func (r RelationType) ToProto() flowsk_task_pb.TaskRelationType {
	switch r {
	case RelationBlocks:
		return flowsk_task_pb.TaskRelationType_TASK_RELATION_TYPE_BLOCKS
	case RelationDuplicates:
		return flowsk_task_pb.TaskRelationType_TASK_RELATION_TYPE_DUPLICATES
	case RelationRelatesTo, RelationSubtaskOf:
		return flowsk_task_pb.TaskRelationType_TASK_RELATION_TYPE_RELATES_TO
	default:
		return flowsk_task_pb.TaskRelationType_TASK_RELATION_TYPE_UNSPECIFIED
	}
}

// ToProto maps a domain CommentKind onto its wire-level flowsk.task.v1.TaskCommentKind enum.
func (k CommentKind) ToProto() flowsk_task_pb.TaskCommentKind {
	switch k {
	case CommentKindBlocker:
		return flowsk_task_pb.TaskCommentKind_TASK_COMMENT_KIND_BLOCKER_REPORT
	case CommentKindHandoff:
		return flowsk_task_pb.TaskCommentKind_TASK_COMMENT_KIND_HANDOFF
	case CommentKindDecision:
		return flowsk_task_pb.TaskCommentKind_TASK_COMMENT_KIND_RESOLUTION
	case CommentKindProgress, CommentKindNote:
		return flowsk_task_pb.TaskCommentKind_TASK_COMMENT_KIND_COMMENT
	default:
		return flowsk_task_pb.TaskCommentKind_TASK_COMMENT_KIND_UNSPECIFIED
	}
}

// ToProto serializes a pure domain Task aggregate into its wire-level
// flowsk.task.v1.Task Protobuf message.
//
// Architectural Invariant (`ocinsh-connectrpc-go-service`):
//   - Safely returns nil when invoked on a nil *Task receiver.
//   - Only outbound `ToProto()` mappers exist on domain aggregates; generic
//     `FromProto()` constructors are strictly prohibited.
func (t *Task) ToProto() *flowsk_task_pb.Task {
	if t == nil {
		return nil
	}

	var claimedBy *string
	var claimExpiresAt *timestamppb.Timestamp
	if t.ActiveLease != nil {
		claimedBy = uuidValueToStringPtr(t.ActiveLease.HolderActorID)
		claimExpiresAt = timeToProto(t.ActiveLease.ExpiresAt)
	}

	var workflowStateID string
	if t.ProjectID != uuid.Nil {
		slug := strings.TrimSpace(string(t.Status))
		if slug == "" {
			slug = string(StatusReady)
		}
		workflowStateID = uuid.NewSHA1(
			uuid.NameSpaceOID,
			[]byte("flowsk:workflow-state:"+t.ProjectID.String()+":"+slug),
		).String()
	}

	var labelIDs []string
	if len(t.Labels) > 0 {
		keys := make([]string, 0, len(t.Labels))
		for k := range t.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if parsed, err := uuid.Parse(strings.TrimSpace(k)); err == nil && parsed != uuid.Nil {
				labelIDs = append(labelIDs, parsed.String())
			}
		}
	}

	return &flowsk_task_pb.Task{
		Id:                 uuidToString(t.ID),
		WorkspaceId:        uuidToString(t.WorkspaceID),
		ProjectId:          uuidToString(t.ProjectID),
		Identifier:         t.Key,
		SequenceNumber:     parseSequenceFromKey(t.Key),
		Kind:               t.Kind.ToProto(),
		Priority:           t.Priority.ToProto(),
		WorkflowStateId:    workflowStateID,
		StateCategory:      t.Status.ToProtoStateCategory(),
		Title:              t.Title,
		Description:        t.Description,
		AcceptanceCriteria: strings.Join(t.AcceptanceCriteria, "\n"),
		ObjectiveId:        uuidPtrToStringPtr(t.ObjectiveID),
		ParentTaskId:       uuidPtrToStringPtr(t.ParentTaskID),
		AssigneeActorId:    uuidPtrToStringPtr(t.AssigneeActorID),
		ClaimedByActorId:   claimedBy,
		ClaimExpiresAt:     claimExpiresAt,
		LabelIds:           labelIDs,
		Tags:               t.TagsCopy(),
		CompletedAt:        timePtrToProto(t.CompletedAt),
		CreatedByActorId:   uuidToString(t.CreatedBy),
		CreatedAt:          timeToProto(t.CreatedAt),
		UpdatedAt:          timeToProto(t.UpdatedAt),
		HasMarkdownDoc:     t.HasMarkdownDoc,
		Status:             string(t.Status),
		ExternalId:         t.ExternalID,
		LastSignalAt:       timePtrToProto(t.LastSignalAt),
	}
}

// ToProto serializes a directed domain TaskRelation edge into its wire-level
// flowsk.task.v1.TaskRelation Protobuf message.
// Returns nil safely when invoked on a nil *TaskRelation receiver.
func (r *TaskRelation) ToProto() *flowsk_task_pb.TaskRelation {
	if r == nil {
		return nil
	}
	return &flowsk_task_pb.TaskRelation{
		Id:               uuidToString(r.ID),
		WorkspaceId:      uuidToString(r.WorkspaceID),
		SourceTaskId:     uuidToString(r.SourceTaskID),
		TargetTaskId:     uuidToString(r.TargetTaskID),
		RelationType:     r.Type.ToProto(),
		CreatedByActorId: uuidToString(r.CreatedBy),
		CreatedAt:        timeToProto(r.CreatedAt),
	}
}

// ToProto serializes a domain TaskComment into its wire-level
// flowsk.task.v1.TaskComment Protobuf message.
// Returns nil safely when invoked on a nil *TaskComment receiver.
func (c *TaskComment) ToProto() *flowsk_task_pb.TaskComment {
	if c == nil {
		return nil
	}
	createdAt := timeToProto(c.CreatedAt)
	return &flowsk_task_pb.TaskComment{
		Id:            uuidToString(c.ID),
		WorkspaceId:   uuidToString(c.WorkspaceID),
		TaskId:        uuidToString(c.TaskID),
		AuthorActorId: uuidToString(c.AuthorActorID),
		Kind:          c.Kind.ToProto(),
		Body:          c.Body,
		CreatedAt:     createdAt,
		UpdatedAt:     createdAt,
	}
}

// ToProto serializes a token-frugal CompactTaskSummary into its wire-level
// flowsk.task.v1.CompactTaskSummary Protobuf representation.
// Returns nil safely when invoked on a nil *CompactTaskSummary receiver.
func (c *CompactTaskSummary) ToProto() *flowsk_task_pb.CompactTaskSummary {
	if c == nil {
		return nil
	}
	return &flowsk_task_pb.CompactTaskSummary{
		Id:               uuidToString(c.ID),
		Identifier:       c.Key,
		Kind:             c.Kind.ToProto(),
		Priority:         c.Priority.ToProto(),
		StateCategory:    c.Status.ToProtoStateCategory(),
		StateName:        string(c.Status),
		Title:            c.Title,
		IsBlocked:        c.UnresolvedBlockerCount > 0,
		ClaimedByActorId: uuidPtrToStringPtr(c.HolderActorID),
		ClaimExpiresAt:   timePtrToProto(c.LeaseExpiresAt),
		ObjectiveId:      uuidPtrToStringPtr(c.ObjectiveID),
		HasMarkdownDoc:   c.HasMarkdownDoc,
		ExternalId:       c.ExternalID,
		LastSignalAt:     timePtrToProto(c.LastSignalAt),
	}
}

// ToProto serializes a domain ProjectDigest snapshot into its wire-level
// flowsk.task.v1.ProjectDigest Protobuf message with deterministic category ordering.
// Returns nil safely when invoked on a nil *ProjectDigest receiver.
func (d *ProjectDigest) ToProto() *flowsk_task_pb.ProjectDigest {
	if d == nil {
		return nil
	}

	categoryTotals := make(map[flowsk_project_pb.WorkflowStateCategory]int32, len(d.CountsByStatus))
	var totalTasks int32
	for st, count := range d.CountsByStatus {
		totalTasks += count
		categoryTotals[st.ToProtoStateCategory()] += count
	}

	categories := make([]int, 0, len(categoryTotals))
	for cat := range categoryTotals {
		categories = append(categories, int(cat))
	}
	sort.Ints(categories)

	countsByCategory := make([]*flowsk_task_pb.StateCategoryCount, 0, len(categories))
	for _, rawCat := range categories {
		cat := flowsk_project_pb.WorkflowStateCategory(rawCat)
		countsByCategory = append(countsByCategory, &flowsk_task_pb.StateCategoryCount{
			Category: cat,
			Count:    categoryTotals[cat],
		})
	}

	activeBlockers := make([]*flowsk_task_pb.CompactTaskSummary, 0, len(d.BlockedTasks))
	for i := range d.BlockedTasks {
		activeBlockers = append(activeBlockers, d.BlockedTasks[i].ToProto())
	}

	readyQueue := make([]*flowsk_task_pb.CompactTaskSummary, 0, len(d.ReadyQueue))
	for i := range d.ReadyQueue {
		readyQueue = append(readyQueue, d.ReadyQueue[i].ToProto())
	}

	return &flowsk_task_pb.ProjectDigest{
		WorkspaceId:                uuidToString(d.WorkspaceID),
		ProjectId:                  uuidToString(d.ProjectID),
		ProjectKeyPrefix:           d.ProjectKeyPrefix,
		TotalTasks:                 totalTasks,
		CountsByStateCategory:      countsByCategory,
		ActiveBlockers:             activeBlockers,
		UnclaimedHighPriorityTasks: readyQueue,
		ActiveAgentClaims:          clampIntToInt32(len(d.ActiveLeases)),
		GeneratedAt:                timeToProto(d.GeneratedAt),
	}
}

func clampIntToInt32(n int) int32 {
	if n <= 0 {
		return 0
	}
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(n)
}

func parseSequenceFromKey(key string) int64 {
	idx := strings.LastIndexByte(key, '-')
	if idx < 0 || idx+1 >= len(key) {
		return 0
	}
	seq, err := strconv.ParseInt(key[idx+1:], 10, 64)
	if err != nil || seq < 0 {
		return 0
	}
	return seq
}

func uuidToString(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}

func uuidPtrToStringPtr(id *uuid.UUID) *string {
	if id == nil || *id == uuid.Nil {
		return nil
	}
	s := id.String()
	return &s
}

func uuidValueToStringPtr(id uuid.UUID) *string {
	if id == uuid.Nil {
		return nil
	}
	s := id.String()
	return &s
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
