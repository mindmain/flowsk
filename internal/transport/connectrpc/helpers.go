// File helpers.go provides transport-edge UUID validation, cursor pagination,
// zero-host-path guards, and deterministic canonical workflow state mappings
// shared across Flowsk ConnectRPC bounded-context handlers.
package connectrpc

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	flowsk_project_pb "github.com/mindmain/flowsk/gen/go/flowsk/project/v1"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/task"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var hexColorRegex = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

// ParseRequiredUUID parses raw as a canonical non-nil UUID, returning an
// errs.ErrValidation DomainError when missing or malformed.
func ParseRequiredUUID(op, field, raw string) (uuid.UUID, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return uuid.Nil, errs.Validation(op, "invalid request field", errs.FieldViolation{
			Field:       field,
			Description: field + " is required and must be a valid UUID",
		})
	}
	parsed, err := uuid.Parse(trimmed)
	if err != nil || parsed == uuid.Nil {
		return uuid.Nil, errs.Validation(op, "invalid request field", errs.FieldViolation{
			Field:       field,
			Description: field + " must be a valid non-nil UUID",
		})
	}
	return parsed, nil
}

// ParseOptionalUUID parses an optional pointer string into a non-nil *uuid.UUID,
// returning (nil, nil) when raw is nil.
func ParseOptionalUUID(op, field string, raw *string) (*uuid.UUID, error) {
	if raw == nil {
		return nil, nil
	}
	parsed, err := ParseRequiredUUID(op, field, *raw)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

// ParseUUIDSlice validates and parses a slice of UUID strings.
func ParseUUIDSlice(op, field string, raws []string) ([]uuid.UUID, error) {
	if len(raws) == 0 {
		return nil, nil
	}
	out := make([]uuid.UUID, 0, len(raws))
	for _, raw := range raws {
		id, err := ParseRequiredUUID(op, field, raw)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

// ValidateHexColor verifies that colorHex is either empty or a valid '#RRGGBB' hex color.
func ValidateHexColor(op, field, colorHex string) error {
	if colorHex == "" {
		return nil
	}
	if !hexColorRegex.MatchString(colorHex) {
		return errs.Validation(op, "invalid color_hex format", errs.FieldViolation{
			Field:       field,
			Description: "must be empty or a 6-digit hex color prefixed with '#'",
		})
	}
	return nil
}

// LooksLikeHostPath reports whether s resembles a host-local filesystem path or file:// URI.
func LooksLikeHostPath(s string) bool {
	v := strings.TrimSpace(s)
	if v == "" {
		return false
	}
	if strings.HasPrefix(v, "/") ||
		strings.HasPrefix(v, "~/") ||
		strings.HasPrefix(v, "./") ||
		strings.HasPrefix(v, "../") ||
		strings.HasPrefix(strings.ToLower(v), "file://") ||
		strings.Contains(v, `\`) {
		return true
	}
	if len(v) >= 3 && ((v[0] >= 'A' && v[0] <= 'Z') || (v[0] >= 'a' && v[0] <= 'z')) && v[1] == ':' && (v[2] == '/' || v[2] == '\\') {
		return true
	}
	return false
}

// RejectHostFilesystemPath returns an errs.ErrValidation error if val resembles a local filesystem path.
func RejectHostFilesystemPath(op, field, val string) error {
	if LooksLikeHostPath(val) {
		return errs.Validation(op, "host filesystem paths are forbidden on server transports", errs.FieldViolation{
			Field:       field,
			Description: "must not contain local host filesystem paths",
		})
	}
	return nil
}

// ParsePagination validates pageSize (0..maxPageSize) and decodes an integer offset pageToken.
func ParsePagination(op string, pageSize, maxPageSize int32, pageToken string) (limit, offset int32, err error) {
	if pageSize < 0 || pageSize > maxPageSize {
		return 0, 0, errs.Validation(op, "invalid pagination parameters", errs.FieldViolation{
			Field:       "page_size",
			Description: "page_size must be between 0 and " + strconv.Itoa(int(maxPageSize)),
		})
	}
	trimmed := strings.TrimSpace(pageToken)
	if trimmed == "" {
		return pageSize, 0, nil
	}
	parsedOffset, parseErr := strconv.ParseInt(trimmed, 10, 32)
	if parseErr != nil || parsedOffset < 0 {
		return 0, 0, errs.Validation(op, "invalid pagination parameters", errs.FieldViolation{
			Field:       "page_token",
			Description: "page_token is malformed",
		})
	}
	return pageSize, int32(parsedOffset), nil
}

// PaginateSlice slices items according to offset and limit and returns the next page token (if any).
func PaginateSlice[T any](items []T, limit, offset int32) ([]T, string) {
	total := len(items)
	start := int(offset)
	if start >= total {
		return nil, ""
	}
	if limit <= 0 {
		out := make([]T, total-start)
		copy(out, items[start:])
		return out, ""
	}
	end := start + int(limit)
	if end >= total {
		out := make([]T, total-start)
		copy(out, items[start:])
		return out, ""
	}
	out := make([]T, end-start)
	copy(out, items[start:end])
	return out, strconv.Itoa(end)
}

// canonicalStateSpec describes one built-in Linear-style project workflow column.
type canonicalStateSpec struct {
	slug        string
	name        string
	description string
	category    flowsk_project_pb.WorkflowStateCategory
	colorHex    string
	position    int32
	isDefault   bool
	taskStatus  task.Status
}

var defaultWorkflowStateSpecs = []canonicalStateSpec{
	{
		slug:        "triage",
		name:        "Triage",
		description: "Incoming items awaiting initial triage.",
		category:    flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_TRIAGE,
		colorHex:    "#94A3B8",
		position:    0,
		isDefault:   false,
		taskStatus:  task.StatusBacklog,
	},
	{
		slug:        "backlog",
		name:        "Backlog",
		description: "Accepted work queued for future milestones or cycles.",
		category:    flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_BACKLOG,
		colorHex:    "#64748B",
		position:    1,
		isDefault:   false,
		taskStatus:  task.StatusBacklog,
	},
	{
		slug:        "todo",
		name:        "Todo",
		description: "Refined, unstarted work ready to be claimed.",
		category:    flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_UNSTARTED,
		colorHex:    "#3B82F6",
		position:    2,
		isDefault:   true,
		taskStatus:  task.StatusReady,
	},
	{
		slug:        "in_progress",
		name:        "In Progress",
		description: "Actively under implementation by a human or agent.",
		category:    flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_STARTED,
		colorHex:    "#F59E0B",
		position:    3,
		isDefault:   false,
		taskStatus:  task.StatusInProgress,
	},
	{
		slug:        "in_review",
		name:        "In Review",
		description: "Implementation complete and awaiting verification or review.",
		category:    flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_STARTED,
		colorHex:    "#8B5CF6",
		position:    4,
		isDefault:   false,
		taskStatus:  task.StatusInReview,
	},
	{
		slug:        "blocked",
		name:        "Blocked",
		description: "Execution paused awaiting dependency or blocker resolution.",
		category:    flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_STARTED,
		colorHex:    "#DC2626",
		position:    5,
		isDefault:   false,
		taskStatus:  task.StatusBlocked,
	},
	{
		slug:        "done",
		name:        "Done",
		description: "Verified complete against acceptance criteria.",
		category:    flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_COMPLETED,
		colorHex:    "#10B981",
		position:    6,
		isDefault:   false,
		taskStatus:  task.StatusDone,
	},
	{
		slug:        "canceled",
		name:        "Canceled",
		description: "Closed without implementation or superseded.",
		category:    flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_CANCELED,
		colorHex:    "#EF4444",
		position:    7,
		isDefault:   false,
		taskStatus:  task.StatusCancelled,
	},
	{
		slug:        "archived",
		name:        "Archived",
		description: "Archived out of active boards and eligible for permanent archive purge.",
		category:    flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_ARCHIVED,
		colorHex:    "#64748B",
		position:    8,
		isDefault:   false,
		taskStatus:  task.StatusArchived,
	},
}

var (
	customStateMu         sync.RWMutex
	customStatesByProject = make(map[uuid.UUID]map[uuid.UUID]flowsk_project_pb.WorkflowStateCategory)
)

// RegisterCustomWorkflowState records the category of a custom workflow state UUID
// under the wildcard project bucket for backward-compatible callers.
func RegisterCustomWorkflowState(stateID uuid.UUID, category flowsk_project_pb.WorkflowStateCategory) {
	RegisterProjectWorkflowState(uuid.Nil, stateID, category)
}

// RegisterProjectWorkflowState records the category of a custom workflow state UUID
// scoped to `projectID` so task state transitions resolve custom columns safely
// without leaking state IDs across projects.
func RegisterProjectWorkflowState(projectID, stateID uuid.UUID, category flowsk_project_pb.WorkflowStateCategory) {
	if stateID == uuid.Nil {
		return
	}
	customStateMu.Lock()
	perProj := customStatesByProject[projectID]
	if perProj == nil {
		perProj = make(map[uuid.UUID]flowsk_project_pb.WorkflowStateCategory)
		customStatesByProject[projectID] = perProj
	}
	perProj[stateID] = category
	customStateMu.Unlock()
}

// UnregisterProjectWorkflowStates removes any cached custom workflow state mappings
// belonging to `projectID` when a project is deleted.
func UnregisterProjectWorkflowStates(projectID uuid.UUID) {
	if projectID == uuid.Nil {
		return
	}
	customStateMu.Lock()
	delete(customStatesByProject, projectID)
	customStateMu.Unlock()
}

// CanonicalWorkflowStateID computes the deterministic UUIDv5 for a canonical workflow
// state column belonging to projectID.
func CanonicalWorkflowStateID(projectID uuid.UUID, slug string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("flowsk:workflow-state:"+projectID.String()+":"+slug))
}

// DefaultWorkflowStates returns defensive copies of the seven canonical Linear-style
// workflow states for (workspaceID, projectID), ordered by position ascending.
func DefaultWorkflowStates(workspaceID, projectID uuid.UUID, createdAt time.Time) []*flowsk_project_pb.WorkflowState {
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	ts := timestamppb.New(createdAt.UTC())
	out := make([]*flowsk_project_pb.WorkflowState, 0, len(defaultWorkflowStateSpecs))
	for _, spec := range defaultWorkflowStateSpecs {
		out = append(out, &flowsk_project_pb.WorkflowState{
			Id:          CanonicalWorkflowStateID(projectID, spec.slug).String(),
			WorkspaceId: workspaceID.String(),
			ProjectId:   projectID.String(),
			Name:        spec.name,
			Description: spec.description,
			Category:    spec.category,
			ColorHex:    spec.colorHex,
			Position:    spec.position,
			IsDefault:   spec.isDefault,
			CreatedAt:   ts,
			UpdatedAt:   ts,
		})
	}
	return out
}

// WorkflowStateIDForStatus returns the deterministic canonical workflow state UUID
// corresponding to a domain task.Status inside projectID.
func WorkflowStateIDForStatus(projectID uuid.UUID, st task.Status) uuid.UUID {
	switch st {
	case task.StatusBacklog:
		return CanonicalWorkflowStateID(projectID, "backlog")
	case task.StatusReady:
		return CanonicalWorkflowStateID(projectID, "todo")
	case task.StatusInProgress:
		return CanonicalWorkflowStateID(projectID, "in_progress")
	case task.StatusBlocked:
		return CanonicalWorkflowStateID(projectID, "blocked")
	case task.StatusInReview:
		return CanonicalWorkflowStateID(projectID, "in_review")
	case task.StatusDone:
		return CanonicalWorkflowStateID(projectID, "done")
	case task.StatusCancelled:
		return CanonicalWorkflowStateID(projectID, "canceled")
	case task.StatusArchived:
		return CanonicalWorkflowStateID(projectID, "archived")
	default:
		return CanonicalWorkflowStateID(projectID, "todo")
	}
}

// ResolveWorkflowStateStatus resolves a workflow state UUID back into its canonical
// domain task.Status for projectID (checking project-scoped defaults, global defaults,
// and project-scoped custom workflow states).
func ResolveWorkflowStateStatus(projectID, stateID uuid.UUID) (task.Status, bool) {
	for _, spec := range defaultWorkflowStateSpecs {
		if (projectID != uuid.Nil && CanonicalWorkflowStateID(projectID, spec.slug) == stateID) ||
			CanonicalWorkflowStateID(uuid.Nil, spec.slug) == stateID {
			return spec.taskStatus, true
		}
	}

	customStateMu.RLock()
	var (
		cat flowsk_project_pb.WorkflowStateCategory
		ok  bool
	)
	if projectID != uuid.Nil {
		if perProj := customStatesByProject[projectID]; perProj != nil {
			cat, ok = perProj[stateID]
		}
	}
	if !ok {
		if global := customStatesByProject[uuid.Nil]; global != nil {
			cat, ok = global[stateID]
		}
	}
	customStateMu.RUnlock()
	if !ok {
		return "", false
	}
	return StatusFromWorkflowCategory(cat)
}

// StatusFromWorkflowCategory maps a canonical WorkflowStateCategory to its primary domain task.Status.
func StatusFromWorkflowCategory(cat flowsk_project_pb.WorkflowStateCategory) (task.Status, bool) {
	switch cat {
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_TRIAGE,
		flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_BACKLOG:
		return task.StatusBacklog, true
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_UNSTARTED:
		return task.StatusReady, true
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_STARTED:
		return task.StatusInProgress, true
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_COMPLETED:
		return task.StatusDone, true
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_CANCELED:
		return task.StatusCancelled, true
	case flowsk_project_pb.WorkflowStateCategory_WORKFLOW_STATE_CATEGORY_ARCHIVED:
		return task.StatusArchived, true
	default:
		return "", false
	}
}
