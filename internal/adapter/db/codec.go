// File codec.go provides shared, lossless domain-to-SQL metadata envelopes,
// workflow state mappings, error translators, and deterministic sorting/pagination
// helpers shared identically across the SQLite and PostgreSQL database adapters.
package db

import (
	"cmp"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/domain/audit"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/objective"
	"github.com/mindmain/flowsk/internal/domain/project"
	"github.com/mindmain/flowsk/internal/domain/task"
	"github.com/mindmain/flowsk/internal/domain/tx"
	"github.com/mindmain/flowsk/internal/domain/workspace"
)

const (
	// SQLiteTimestampLayout is the fixed-width ISO-8601 UTC millisecond format
	// matching SQLite's `strftime('%Y-%m-%dT%H:%M:%fZ', 'now')` and `julianday()`.
	SQLiteTimestampLayout = "2006-01-02T15:04:05.000Z"

	// envelopePrefix marks a text column carrying a lossless JSON sidecar envelope.
	envelopePrefix = "<!--flowsk:v1-->\n"

	// RelationSidecarKeyPrefix namespaces TaskRelation metadata inside dsl_execution_batches.
	RelationSidecarKeyPrefix = "__flowsk_rel_meta:"

	// StubActorHandlePrefix marks auto-provisioned foreign-key placeholder actors.
	StubActorHandlePrefix = "__flowsk_stub_"
)

// WorkflowStateSeed defines one canonical default workflow column seeded for every new Project.
type WorkflowStateSeed struct {
	// Status is the corresponding domain task.Status value.
	Status task.Status

	// Name is the human-readable Kanban column label.
	Name string

	// Category is the SQLite/PostgreSQL CHECK-constrained state category.
	Category string

	// Position orders columns deterministically from left to right.
	Position int32

	// Color is the hex badge color.
	Color string

	// IsDefault marks the initial state used when a task has no explicit status.
	IsDefault bool
}

// DefaultWorkflowStateSeeds returns the 8 standard project workflow states mapping
// every canonical `task.Status` value cleanly onto SQL `workflow_states`.
func DefaultWorkflowStateSeeds() []WorkflowStateSeed {
	return []WorkflowStateSeed{
		{Status: task.StatusBacklog, Name: "Backlog", Category: "backlog", Position: 10, Color: "#6B7280", IsDefault: false},
		{Status: task.StatusReady, Name: "Todo", Category: "unstarted", Position: 20, Color: "#3B82F6", IsDefault: true},
		{Status: task.StatusInProgress, Name: "In Progress", Category: "started", Position: 30, Color: "#F59E0B", IsDefault: false},
		{Status: task.StatusInReview, Name: "In Review", Category: "started", Position: 40, Color: "#8B5CF6", IsDefault: false},
		{Status: task.StatusBlocked, Name: "Blocked", Category: "started", Position: 50, Color: "#EF4444", IsDefault: false},
		{Status: task.StatusDone, Name: "Done", Category: "completed", Position: 60, Color: "#10B981", IsDefault: false},
		{Status: task.StatusCancelled, Name: "Canceled", Category: "canceled", Position: 70, Color: "#9CA3AF", IsDefault: false},
		{Status: task.StatusArchived, Name: "Archived", Category: "canceled", Position: 80, Color: "#64748B", IsDefault: false},
	}
}

// WorkflowStateNameForStatus maps a canonical `task.Status` to its seeded workflow state name.
func WorkflowStateNameForStatus(st task.Status) string {
	switch st {
	case task.StatusBacklog:
		return "Backlog"
	case task.StatusReady, "":
		return "Todo"
	case task.StatusInProgress:
		return "In Progress"
	case task.StatusInReview:
		return "In Review"
	case task.StatusBlocked:
		return "Blocked"
	case task.StatusDone:
		return "Done"
	case task.StatusCancelled:
		return "Canceled"
	case task.StatusArchived:
		return "Archived"
	default:
		return "Todo"
	}
}

// TaskStatusFromWorkflowState maps a persisted `(name, category)` pair back to `task.Status`.
func TaskStatusFromWorkflowState(name, category string) task.Status {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "backlog":
		return task.StatusBacklog
	case "todo", "ready":
		return task.StatusReady
	case "in progress", "in_progress":
		return task.StatusInProgress
	case "in review", "in_review":
		return task.StatusInReview
	case "blocked":
		return task.StatusBlocked
	case "done", "completed":
		return task.StatusDone
	case "canceled", "cancelled":
		return task.StatusCancelled
	case "archived":
		return task.StatusArchived
	}
	switch category {
	case "backlog", "triage":
		return task.StatusBacklog
	case "unstarted":
		return task.StatusReady
	case "started":
		return task.StatusInProgress
	case "completed":
		return task.StatusDone
	case "canceled":
		return task.StatusCancelled
	default:
		return task.StatusReady
	}
}

// SQLWorkflowStateCategory maps a domain workflow state category onto SQL's CHECK-valid
// `workflow_states.category` values (`'triage'`, `'backlog'`, `'unstarted'`, `'started'`,
// `'completed'`, `'canceled'`), mapping `"archived"` and `"cancelled"` onto `"canceled"`.
func SQLWorkflowStateCategory(category string) string {
	switch strings.ToLower(strings.TrimSpace(category)) {
	case "archived", "cancelled":
		return "canceled"
	case "":
		return "unstarted"
	default:
		return strings.ToLower(strings.TrimSpace(category))
	}
}

// DomainWorkflowStateCategory maps a persisted SQL `(name, category)` pair onto a domain
// workflow state category, restoring `"archived"` when the state is named `"Archived"`.
func DomainWorkflowStateCategory(name, sqlCategory string) string {
	if strings.EqualFold(strings.TrimSpace(name), "archived") && sqlCategory == "canceled" {
		return "archived"
	}
	return sqlCategory
}

// FormatSQLiteTime formats t in UTC millisecond ISO-8601 format for SQLite storage.
func FormatSQLiteTime(t time.Time) string {
	return t.UTC().Format(SQLiteTimestampLayout)
}

// ParseSQLiteTime parses an ISO-8601 UTC timestamp stored in SQLite TEXT columns.
func ParseSQLiteTime(raw string) time.Time {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		SQLiteTimestampLayout,
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05",
	} {
		if parsed, err := time.Parse(layout, trimmed); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

// textEnvelope wraps plain text alongside typed structured metadata inside a TEXT column.
type textEnvelope[M any] struct {
	Text string `json:"text"`
	Meta M      `json:"meta"`
}

func encodeEnvelope[M any](plain string, meta M, isZeroMeta bool) string {
	if isZeroMeta && !strings.HasPrefix(plain, envelopePrefix) {
		return plain
	}
	raw, err := json.Marshal(textEnvelope[M]{Text: plain, Meta: meta})
	if err != nil {
		return plain
	}
	return envelopePrefix + string(raw)
}

func decodeEnvelope[M any](stored string) (string, M, bool) {
	var zero M
	if !strings.HasPrefix(stored, envelopePrefix) {
		return stored, zero, false
	}
	payload := strings.TrimPrefix(stored, envelopePrefix)
	var env textEnvelope[M]
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		return stored, zero, false
	}
	return env.Text, env.Meta, true
}

// WorkspaceMeta stores domain Workspace attributes not represented as dedicated SQL columns.
type WorkspaceMeta struct {
	// DefaultLeaseTTLSeconds preserves Workspace.DefaultLeaseTTLSeconds.
	DefaultLeaseTTLSeconds int64 `json:"default_lease_ttl_seconds,omitempty"`

	// Labels preserves Workspace.Labels.
	Labels map[string]string `json:"labels,omitempty"`
}

// EncodeWorkspaceDescription packs Description, DefaultLeaseTTLSeconds, and Labels losslessly.
func EncodeWorkspaceDescription(ws workspace.Workspace) string {
	meta := WorkspaceMeta{
		DefaultLeaseTTLSeconds: ws.DefaultLeaseTTLSeconds,
		Labels:                 ws.LabelsCopy(),
	}
	isZero := meta.DefaultLeaseTTLSeconds == 0 && len(meta.Labels) == 0
	return encodeEnvelope(ws.Description, meta, isZero)
}

// DecodeWorkspaceDescription unpacks Description, DefaultLeaseTTLSeconds, and Labels.
func DecodeWorkspaceDescription(raw string) (description string, ttl int64, labels map[string]string) {
	text, meta, ok := decodeEnvelope[WorkspaceMeta](raw)
	if !ok {
		return raw, 0, nil
	}
	var copied map[string]string
	if len(meta.Labels) > 0 {
		copied = make(map[string]string, len(meta.Labels))
		for k, v := range meta.Labels {
			copied[k] = v
		}
	}
	return text, meta.DefaultLeaseTTLSeconds, copied
}

// ActorCapabilitiesMeta stores RBAC membership role, cross-workspace principal UUID,
// and stub actor status inside `actors.capabilities_json`.
type ActorCapabilitiesMeta struct {
	// PrincipalActorID stores the logical domain ActorID when `actors.id` had to be
	// deterministically namespaced to allow membership in multiple workspaces.
	PrincipalActorID string `json:"principal_actor_id,omitempty"`

	// Role stores the workspace.MemberRole for registered members.
	Role workspace.MemberRole `json:"role,omitempty"`

	// MemberKind preserves the latest ActorKind when a stub actor is promoted to a full member.
	MemberKind authctx.ActorKind `json:"member_kind,omitempty"`

	// RawDisplayName preserves an empty DisplayName when a non-empty placeholder satisfied SQL CHECK.
	RawDisplayName string `json:"raw_display_name,omitempty"`

	// HasRawDisplayName indicates RawDisplayName overrides the SQL display_name column.
	HasRawDisplayName bool `json:"has_raw_display_name,omitempty"`

	// IsStub marks FK-safety placeholder rows that have not been registered via UpsertMember.
	IsStub bool `json:"is_stub,omitempty"`
}

// ScopedActorRowID derives a deterministic UUIDv5 row primary key for `(workspaceID, principalActorID)`
// when `principalActorID` is already occupied as a primary key in another workspace.
func ScopedActorRowID(workspaceID, principalActorID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(workspaceID, principalActorID[:])
}

// EncodeActorCapabilities serializes ActorCapabilitiesMeta as UTF-8 JSON bytes.
func EncodeActorCapabilities(meta ActorCapabilitiesMeta) []byte {
	raw, err := json.Marshal(meta)
	if err != nil {
		return []byte("{}")
	}
	return raw
}

// DecodeActorCapabilities parses ActorCapabilitiesMeta from JSON text or JSONB bytes.
func DecodeActorCapabilities(raw []byte) ActorCapabilitiesMeta {
	var meta ActorCapabilitiesMeta
	if len(raw) == 0 {
		return meta
	}
	_ = json.Unmarshal(raw, &meta)
	return meta
}

// ResolvePrincipalActorID returns the logical domain ActorID for a persisted Actor row.
func ResolvePrincipalActorID(rowID uuid.UUID, meta ActorCapabilitiesMeta) uuid.UUID {
	if meta.PrincipalActorID != "" {
		if parsed, err := uuid.Parse(meta.PrincipalActorID); err == nil && parsed != uuid.Nil {
			return parsed
		}
	}
	return rowID
}

// ProjectMeta stores domain Project attributes inside `projects.description`.
type ProjectMeta struct {
	// RemoteRepoURL preserves Project.RemoteRepoURL.
	RemoteRepoURL string `json:"remote_repo_url,omitempty"`

	// DefaultBranch preserves Project.DefaultBranch.
	DefaultBranch string `json:"default_branch,omitempty"`

	// Tags preserves Project.Tags.
	Tags []string `json:"tags,omitempty"`
}

// EncodeProjectDescription packs Project Description, RemoteRepoURL, DefaultBranch, and Tags.
func EncodeProjectDescription(p project.Project) string {
	meta := ProjectMeta{
		RemoteRepoURL: p.RemoteRepoURL,
		DefaultBranch: p.DefaultBranch,
		Tags:          p.TagsCopy(),
	}
	isZero := meta.RemoteRepoURL == "" && meta.DefaultBranch == "" && len(meta.Tags) == 0
	return encodeEnvelope(p.Description, meta, isZero)
}

// DecodeProjectDescription unpacks Project Description and ProjectMeta.
func DecodeProjectDescription(raw string) (string, ProjectMeta) {
	text, meta, ok := decodeEnvelope[ProjectMeta](raw)
	if !ok {
		return raw, ProjectMeta{}
	}
	return text, meta
}

// ObjectiveMeta stores domain Objective attributes inside `objectives.description`.
type ObjectiveMeta struct {
	// Slug preserves Objective.Slug (unique per active project).
	Slug string `json:"slug,omitempty"`

	// SuccessCriteria preserves Objective.SuccessCriteria.
	SuccessCriteria []string `json:"success_criteria,omitempty"`

	// PriorityRank preserves Objective.PriorityRank.
	PriorityRank int32 `json:"priority_rank,omitempty"`

	// HasTargetDate distinguishes an intentionally cleared nil TargetDate on Update.
	HasTargetDate bool `json:"has_target_date"`
}

// EncodeObjectiveDescription packs Objective Summary, Slug, SuccessCriteria, and PriorityRank.
func EncodeObjectiveDescription(o objective.Objective) string {
	meta := ObjectiveMeta{
		Slug:            o.Slug,
		SuccessCriteria: o.SuccessCriteriaCopy(),
		PriorityRank:    o.PriorityRank,
		HasTargetDate:   o.TargetDate != nil,
	}
	return encodeEnvelope(o.Summary, meta, false)
}

// DecodeObjectiveDescription unpacks Objective Summary and ObjectiveMeta.
func DecodeObjectiveDescription(raw string, hasSQLTargetDate bool) (string, ObjectiveMeta) {
	text, meta, ok := decodeEnvelope[ObjectiveMeta](raw)
	if !ok {
		return raw, ObjectiveMeta{HasTargetDate: hasSQLTargetDate}
	}
	return text, meta
}

// TaskMeta stores lossless domain Task fields inside `tasks.description`.
type TaskMeta struct {
	// DomainKind preserves exact task.Kind (including 'chore' and 'research').
	DomainKind task.Kind `json:"domain_kind,omitempty"`

	// DomainPriority preserves exact task.Priority (0..3).
	DomainPriority task.Priority `json:"domain_priority"`

	// Tags preserves Task.Tags.
	Tags []string `json:"tags,omitempty"`

	// Labels preserves Task.Labels.
	Labels map[string]string `json:"labels,omitempty"`

	// Version tracks the optimistic concurrency version counter.
	Version int64 `json:"version"`

	// CreatedBy preserves the logical creator ActorID.
	CreatedBy string `json:"created_by,omitempty"`

	// HasObjectiveID tracks whether ObjectiveID is currently non-nil.
	HasObjectiveID bool `json:"has_objective_id"`

	// HasParentTaskID tracks whether ParentTaskID is currently non-nil.
	HasParentTaskID bool `json:"has_parent_task_id"`

	// HasAssigneeActorID tracks whether AssigneeActorID is currently non-nil.
	HasAssigneeActorID bool `json:"has_assignee_actor_id"`
}

// EncodeTaskDescription packs Task Description and lossless TaskMeta.
func EncodeTaskDescription(t task.Task, version int64) string {
	createdBy := ""
	if t.CreatedBy != uuid.Nil {
		createdBy = t.CreatedBy.String()
	}
	meta := TaskMeta{
		DomainKind:         t.Kind,
		DomainPriority:     t.Priority,
		Tags:               t.TagsCopy(),
		Labels:             t.LabelsCopy(),
		Version:            version,
		CreatedBy:          createdBy,
		HasObjectiveID:     t.ObjectiveID != nil && *t.ObjectiveID != uuid.Nil,
		HasParentTaskID:    t.ParentTaskID != nil && *t.ParentTaskID != uuid.Nil,
		HasAssigneeActorID: t.AssigneeActorID != nil && *t.AssigneeActorID != uuid.Nil,
	}
	return encodeEnvelope(t.Description, meta, false)
}

// DecodeTaskDescription unpacks Task Description and TaskMeta (falling back cleanly for raw rows).
func DecodeTaskDescription(raw, sqlKind string, sqlPriority int32, hasObj, hasParent, hasAssignee bool) (string, TaskMeta) {
	text, meta, ok := decodeEnvelope[TaskMeta](raw)
	if ok {
		if meta.Version <= 0 {
			meta.Version = 1
		}
		return text, meta
	}
	domainPrio := task.PriorityMedium
	if sqlPriority >= 1 && sqlPriority <= 4 {
		domainPrio = task.Priority(sqlPriority - 1)
	}
	domainKind := task.Kind(sqlKind)
	if sqlKind == "task" || sqlKind == "issue" {
		domainKind = task.KindChore
	}
	return raw, TaskMeta{
		DomainKind:         domainKind,
		DomainPriority:     domainPrio,
		Version:            1,
		HasObjectiveID:     hasObj,
		HasParentTaskID:    hasParent,
		HasAssigneeActorID: hasAssignee,
	}
}

// EncodeAcceptanceCriteria serializes Task.AcceptanceCriteria slice into `tasks.acceptance_criteria`.
func EncodeAcceptanceCriteria(items []string) string {
	if len(items) == 0 {
		return ""
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return strings.Join(items, "\n")
	}
	return envelopePrefix + string(raw)
}

// DecodeAcceptanceCriteria deserializes `tasks.acceptance_criteria` back into a string slice.
func DecodeAcceptanceCriteria(raw string) []string {
	if raw == "" {
		return nil
	}
	if strings.HasPrefix(raw, envelopePrefix) {
		var items []string
		if err := json.Unmarshal([]byte(strings.TrimPrefix(raw, envelopePrefix)), &items); err == nil {
			if len(items) == 0 {
				return nil
			}
			return items
		}
	}
	return []string{raw}
}

// SQLTaskKind maps domain `task.Kind` onto SQLite/PostgreSQL CHECK-valid `tasks.kind` values
// (`'task'`, `'bug'`, `'feature'`, `'issue'`, `'epic'`).
func SQLTaskKind(k task.Kind) string {
	switch k {
	case task.KindEpic:
		return "epic"
	case task.KindFeature:
		return "feature"
	case task.KindBug:
		return "bug"
	case task.KindChore, task.KindResearch:
		return "task"
	default:
		return "task"
	}
}

// SQLTaskPriority maps domain `task.Priority` (0=Critical..3=Low) onto SQL `tasks.priority` (1..4).
func SQLTaskPriority(p task.Priority) int32 {
	mapped := int32(p) + 1
	if mapped < 1 {
		return 1
	}
	if mapped > 4 {
		return 4
	}
	return mapped
}

// ParseTrailingSequence extracts the positive numeric suffix from a task key (e.g. "FLW-104" -> 104).
func ParseTrailingSequence(key string) (int64, bool) {
	idx := strings.LastIndexByte(key, '-')
	if idx < 0 || idx+1 >= len(key) {
		return 0, false
	}
	seq, err := strconv.ParseInt(key[idx+1:], 10, 64)
	if err != nil || seq <= 0 {
		return 0, false
	}
	return seq, true
}

// ClaimTokenMeta packs full ClaimLease metadata inside `tasks.claim_token`.
type ClaimTokenMeta struct {
	// LeaseToken is the caller-visible fencing token.
	LeaseToken string `json:"lease_token"`

	// HolderActorID is the logical principal UUID holding the lease.
	HolderActorID string `json:"holder_actor_id"`

	// HolderKind is human or agent.
	HolderKind authctx.ActorKind `json:"holder_kind"`

	// HolderHandle is the actor handle for digest views.
	HolderHandle string `json:"holder_handle"`

	// AgentSessionID is the optional agent session/run identifier.
	AgentSessionID string `json:"agent_session_id,omitempty"`

	// AcquiredAt is the UTC lease acquisition timestamp.
	AcquiredAt time.Time `json:"acquired_at"`

	// HeartbeatAt is the UTC lease heartbeat timestamp.
	HeartbeatAt time.Time `json:"heartbeat_at"`
}

// EncodeClaimToken serializes ClaimLease metadata into `tasks.claim_token`.
func EncodeClaimToken(lease task.ClaimLease) string {
	meta := ClaimTokenMeta{
		LeaseToken:     lease.LeaseToken,
		HolderActorID:  lease.HolderActorID.String(),
		HolderKind:     lease.HolderKind,
		HolderHandle:   lease.HolderHandle,
		AgentSessionID: lease.AgentSessionID,
		AcquiredAt:     lease.AcquiredAt.UTC(),
		HeartbeatAt:    lease.HeartbeatAt.UTC(),
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return lease.LeaseToken
	}
	return envelopePrefix + string(raw)
}

// DecodeClaimToken reconstructs a ClaimLease from `tasks` lease columns.
func DecodeClaimToken(
	workspaceID, taskID, fallbackActorID uuid.UUID,
	fallbackKind authctx.ActorKind,
	fallbackHandle string,
	rawToken string,
	expiresAt, fallbackTime time.Time,
) task.ClaimLease {
	lease := task.ClaimLease{
		TaskID:        taskID,
		WorkspaceID:   workspaceID,
		LeaseToken:    rawToken,
		HolderActorID: fallbackActorID,
		HolderKind:    fallbackKind,
		HolderHandle:  fallbackHandle,
		AcquiredAt:    fallbackTime.UTC(),
		HeartbeatAt:   fallbackTime.UTC(),
		ExpiresAt:     expiresAt.UTC(),
	}
	if strings.HasPrefix(rawToken, envelopePrefix) {
		var meta ClaimTokenMeta
		if err := json.Unmarshal([]byte(strings.TrimPrefix(rawToken, envelopePrefix)), &meta); err == nil {
			lease.LeaseToken = meta.LeaseToken
			if parsed, pErr := uuid.Parse(meta.HolderActorID); pErr == nil && parsed != uuid.Nil {
				lease.HolderActorID = parsed
			}
			if meta.HolderKind != "" {
				lease.HolderKind = meta.HolderKind
			}
			if meta.HolderHandle != "" {
				lease.HolderHandle = meta.HolderHandle
			}
			lease.AgentSessionID = meta.AgentSessionID
			if !meta.AcquiredAt.IsZero() {
				lease.AcquiredAt = meta.AcquiredAt.UTC()
			}
			if !meta.HeartbeatAt.IsZero() {
				lease.HeartbeatAt = meta.HeartbeatAt.UTC()
			}
		}
	}
	return lease
}

// RelationSidecarMeta stores domain `TaskRelation.Type` (`subtask_of`) and `Note`
// inside the workspace batch ledger keyed by `RelationSidecarKeyPrefix + relID`.
type RelationSidecarMeta struct {
	// DomainType preserves the exact domain task.RelationType.
	DomainType task.RelationType `json:"domain_type"`

	// Note preserves the human/agent explanation on the edge.
	Note string `json:"note,omitempty"`
}

// RelationMetaIdempotencyKey builds the deterministic workspace key for a relation's sidecar metadata.
func RelationMetaIdempotencyKey(relationID uuid.UUID) string {
	return RelationSidecarKeyPrefix + relationID.String()
}

// SQLRelationType validates a domain RelationType and maps it onto SQL's CHECK-valid
// `relation_type` (`'blocks'`, `'duplicates'`, `'relates_to'`).
func SQLRelationType(op string, rt task.RelationType) (string, error) {
	switch rt {
	case task.RelationBlocks:
		return "blocks", nil
	case task.RelationDuplicates:
		return "duplicates", nil
	case task.RelationRelatesTo, task.RelationSubtaskOf:
		return "relates_to", nil
	default:
		return "", errs.Validation(op, "invalid task relation type", errs.FieldViolation{
			Field:       "type",
			Description: "relation type must be blocks, subtask_of, relates_to, or duplicates",
		})
	}
}

// CommentMeta preserves exact domain TaskComment fields inside `task_comments.metadata_json`.
type CommentMeta struct {
	// DomainKind preserves task.CommentKind (progress, blocker, decision, handoff, note).
	DomainKind task.CommentKind `json:"domain_kind"`

	// AuthorActorID preserves the logical author UUID.
	AuthorActorID string `json:"author_actor_id,omitempty"`

	// AuthorKind preserves human vs. agent authorship.
	AuthorKind authctx.ActorKind `json:"author_kind,omitempty"`

	// AuthorHandle preserves the author's compact handle.
	AuthorHandle string `json:"author_handle,omitempty"`
}

// SQLCommentKind maps `task.CommentKind` onto SQL's CHECK-valid `task_comments.kind`
// (`'comment'`, `'blocker_report'`, `'handoff'`, `'resolution'`).
func SQLCommentKind(k task.CommentKind) string {
	switch k {
	case task.CommentKindBlocker:
		return "blocker_report"
	case task.CommentKindHandoff:
		return "handoff"
	case task.CommentKindDecision:
		return "resolution"
	case task.CommentKindProgress, task.CommentKindNote:
		return "comment"
	default:
		return "comment"
	}
}

// EncodeCommentMeta serializes CommentMeta into UTF-8 JSON bytes.
func EncodeCommentMeta(c task.TaskComment) []byte {
	raw, err := json.Marshal(CommentMeta{
		DomainKind:    c.Kind,
		AuthorActorID: c.AuthorActorID.String(),
		AuthorKind:    c.AuthorKind,
		AuthorHandle:  c.AuthorHandle,
	})
	if err != nil {
		return []byte("{}")
	}
	return raw
}

// DecodeCommentMeta deserializes CommentMeta from UTF-8 JSON bytes.
func DecodeCommentMeta(raw []byte, sqlKind string) CommentMeta {
	var meta CommentMeta
	if len(raw) > 0 && json.Unmarshal(raw, &meta) == nil && meta.DomainKind != "" {
		return meta
	}
	switch sqlKind {
	case "blocker_report":
		meta.DomainKind = task.CommentKindBlocker
	case "handoff":
		meta.DomainKind = task.CommentKindHandoff
	case "resolution":
		meta.DomainKind = task.CommentKindDecision
	default:
		meta.DomainKind = task.CommentKindNote
	}
	return meta
}

// ValidateAuditEntry enforces `tx.AuditRecorder.Record` input invariants before persistence.
func ValidateAuditEntry(op string, entry tx.AuditEntry) error {
	var violations []errs.FieldViolation
	if entry.WorkspaceID == uuid.Nil {
		violations = append(violations, errs.FieldViolation{
			Field:       "workspace_id",
			Description: "audit entry WorkspaceID must not be uuid.Nil",
		})
	}
	if entry.ActorID == uuid.Nil {
		violations = append(violations, errs.FieldViolation{
			Field:       "actor_id",
			Description: "audit entry ActorID must not be uuid.Nil",
		})
	}
	if !entry.Action.Valid() {
		violations = append(violations, errs.FieldViolation{
			Field:       "action",
			Description: "audit entry Action must be a valid tx.AuditAction",
		})
	}
	if strings.TrimSpace(entry.ResourceType) == "" {
		violations = append(violations, errs.FieldViolation{
			Field:       "resource_type",
			Description: "audit entry ResourceType must not be empty",
		})
	}
	if strings.TrimSpace(entry.ResourceID) == "" {
		violations = append(violations, errs.FieldViolation{
			Field:       "resource_id",
			Description: "audit entry ResourceID must not be empty",
		})
	}
	if len(violations) > 0 {
		return errs.Validation(op, "invalid audit entry", violations...)
	}
	return nil
}

// MapSQLDriverError translates raw SQLite or PostgreSQL driver errors into canonical
// transport-safe `errs.DomainError` sentinels without exposing internal SQL text.
func MapSQLDriverError(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errs.ErrNotFound) ||
		errors.Is(err, errs.ErrConflict) ||
		errors.Is(err, errs.ErrValidation) ||
		errors.Is(err, errs.ErrPreconditionFailed) ||
		errors.Is(err, errs.ErrLeaseHeld) ||
		errors.Is(err, errs.ErrPermissionDenied) ||
		errors.Is(err, errs.ErrUnauthenticated) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		return errs.Wrap(errs.ErrNotFound, op, "requested resource was not found", err)
	}
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "no rows in result set"):
		return errs.Wrap(errs.ErrNotFound, op, "requested resource was not found", err)
	case strings.Contains(lower, "unique constraint") ||
		strings.Contains(lower, "duplicate key value") ||
		strings.Contains(lower, "23505"):
		return errs.Wrap(errs.ErrConflict, op, "resource already exists or conflicts with a unique constraint", err)
	case strings.Contains(lower, "check constraint") ||
		strings.Contains(lower, "23514"):
		return errs.Wrap(errs.ErrValidation, op, "entity attribute failed persistence constraint check", err)
	case strings.Contains(lower, "foreign key constraint") ||
		strings.Contains(lower, "23503"):
		return errs.Wrap(errs.ErrPreconditionFailed, op, "referenced parent or dependent entity constraint failed", err)
	default:
		return err
	}
}

// IsPrimaryKeyConflict reports whether err resulted from a primary-key uniqueness collision on `actors.id`.
func IsPrimaryKeyConflict(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "actors.id") || strings.Contains(lower, "actors_pkey")
}

// SortAndSliceProjects filters by optional tag, sorts deterministically by Slug ASC, and applies Offset/Limit.
func SortAndSliceProjects(items []project.Project, filter project.ListProjectsFilter) []project.Project {
	filtered := make([]project.Project, 0, len(items))
	for _, p := range items {
		if filter.Tag != "" && !slices.Contains(p.Tags, filter.Tag) {
			continue
		}
		filtered = append(filtered, p.Clone())
	}
	slices.SortFunc(filtered, func(a, b project.Project) int {
		if c := cmp.Compare(a.Slug, b.Slug); c != 0 {
			return c
		}
		return cmp.Compare(a.ID.String(), b.ID.String())
	})
	return PaginateSlice(filtered, filter.Offset, filter.Limit)
}

// SortAndSliceObjectives sorts deterministically by PriorityRank ASC, CreatedAt ASC, ID ASC, and paginates.
func SortAndSliceObjectives(items []objective.Objective, offset, limit int32) []objective.Objective {
	cloned := make([]objective.Objective, len(items))
	for i := range items {
		cloned[i] = items[i].Clone()
	}
	slices.SortFunc(cloned, func(a, b objective.Objective) int {
		if c := cmp.Compare(a.PriorityRank, b.PriorityRank); c != 0 {
			return c
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			if a.CreatedAt.Before(b.CreatedAt) {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.ID.String(), b.ID.String())
	})
	return PaginateSlice(cloned, offset, limit)
}

// FilterAndSortCompactQueue applies AgentWorkQueueFilter predicates and deterministic ordering.
func FilterAndSortCompactQueue(
	tasks []task.Task,
	objectiveSlugs map[uuid.UUID]string,
	parentKeys map[uuid.UUID]string,
	leases map[uuid.UUID]task.ClaimLease,
	filter task.AgentWorkQueueFilter,
	now time.Time,
) []task.CompactTaskSummary {
	refNow := now.UTC()
	out := make([]task.CompactTaskSummary, 0, len(tasks))

	for _, t := range tasks {
		if filter.ObjectiveID != nil && (t.ObjectiveID == nil || *t.ObjectiveID != *filter.ObjectiveID) {
			continue
		}
		if filter.ParentTaskID != nil && (t.ParentTaskID == nil || *t.ParentTaskID != *filter.ParentTaskID) {
			continue
		}
		if filter.ExternalID != "" && t.ExternalID != filter.ExternalID {
			continue
		}
		if len(filter.Statuses) > 0 && !slices.Contains(filter.Statuses, t.Status) {
			continue
		}
		if filter.MaxPriority != nil && t.Priority > *filter.MaxPriority {
			continue
		}
		if filter.OnlyUnblocked && t.UnresolvedBlockerCount > 0 {
			continue
		}
		hasAllTags := true
		for _, reqTag := range filter.RequiredTags {
			if !slices.Contains(t.Tags, reqTag) {
				hasAllTags = false
				break
			}
		}
		if !hasAllTags {
			continue
		}

		var holderHandle string
		var holderActorID *uuid.UUID
		var leaseExp *time.Time
		if l, ok := leases[t.ID]; ok && !l.IsExpiredAt(refNow) {
			if filter.OnlyUnclaimed {
				continue
			}
			holderHandle = l.HolderHandle
			if l.HolderActorID != uuid.Nil {
				hid := l.HolderActorID
				holderActorID = &hid
			}
			expCopy := l.ExpiresAt
			leaseExp = &expCopy
		}

		var objID *uuid.UUID
		objSlug := ""
		if t.ObjectiveID != nil && *t.ObjectiveID != uuid.Nil {
			oid := *t.ObjectiveID
			objID = &oid
			objSlug = objectiveSlugs[oid]
		}
		parentKey := ""
		if t.ParentTaskID != nil {
			parentKey = parentKeys[*t.ParentTaskID]
		}

		var lastSignalCopy *time.Time
		if t.LastSignalAt != nil {
			sig := t.LastSignalAt.UTC()
			lastSignalCopy = &sig
		}

		out = append(out, task.CompactTaskSummary{
			ID:                     t.ID,
			Key:                    t.Key,
			ExternalID:             t.ExternalID,
			Kind:                   t.Kind,
			Title:                  t.Title,
			Status:                 t.Status,
			Priority:               t.Priority,
			ObjectiveID:            objID,
			ObjectiveSlug:          objSlug,
			ParentKey:              parentKey,
			HolderActorID:          holderActorID,
			HolderHandle:           holderHandle,
			LeaseExpiresAt:         leaseExp,
			UnresolvedBlockerCount: t.UnresolvedBlockerCount,
			HasMarkdownDoc:         t.HasMarkdownDoc,
			Tags:                   t.TagsCopy(),
			LastSignalAt:           lastSignalCopy,
			UpdatedAt:              t.UpdatedAt,
		})
	}

	// Sort deterministically by Priority ASC, CreatedAt (looked up via task map) ASC, ID ASC.
	createdMap := make(map[uuid.UUID]time.Time, len(tasks))
	for _, t := range tasks {
		createdMap[t.ID] = t.CreatedAt
	}
	slices.SortFunc(out, func(a, b task.CompactTaskSummary) int {
		if c := cmp.Compare(a.Priority, b.Priority); c != 0 {
			return c
		}
		ca, cb := createdMap[a.ID], createdMap[b.ID]
		if !ca.Equal(cb) {
			if ca.Before(cb) {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.ID.String(), b.ID.String())
	})

	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	return PaginateSlice(out, 0, limit)
}

// AssembleProjectDigest computes a complete ProjectDigest from hydrated domain entities.
func AssembleProjectDigest(
	proj project.Project,
	objectives []objective.Objective,
	tasks []task.Task,
	leases map[uuid.UUID]task.ClaimLease,
	opts task.DigestOptions,
	now time.Time,
) task.ProjectDigest {
	refNow := now.UTC()
	counts := make(map[task.Status]int32)
	objSlugs := make(map[uuid.UUID]string, len(objectives))
	parentKeys := make(map[uuid.UUID]string, len(tasks))

	for _, o := range objectives {
		objSlugs[o.ID] = o.Slug
	}
	for _, t := range tasks {
		counts[t.Status]++
		parentKeys[t.ID] = t.Key
	}

	sortedObjs := SortAndSliceObjectives(objectives, 0, 0)
	var activeObjItems []task.ObjectiveDigestItem
	for _, o := range sortedObjs {
		if o.Status != objective.StatusActive {
			continue
		}
		var openCnt, doneCnt int32
		for _, t := range tasks {
			if t.ObjectiveID != nil && *t.ObjectiveID == o.ID {
				if t.Status == task.StatusDone {
					doneCnt++
				} else if !t.Status.IsTerminal() {
					openCnt++
				}
			}
		}
		activeObjItems = append(activeObjItems, task.ObjectiveDigestItem{
			ID:            o.ID,
			Slug:          o.Slug,
			Title:         o.Title,
			PriorityRank:  o.PriorityRank,
			OpenTaskCount: openCnt,
			DoneTaskCount: doneCnt,
		})
	}

	maxReady := opts.MaxReadyTasks
	if maxReady <= 0 {
		maxReady = 15
	}
	maxBlocked := opts.MaxBlockedTasks
	if maxBlocked <= 0 {
		maxBlocked = 10
	}

	readyQueue := FilterAndSortCompactQueue(tasks, objSlugs, parentKeys, leases, task.AgentWorkQueueFilter{
		WorkspaceID:   proj.WorkspaceID,
		ProjectID:     proj.ID,
		Statuses:      []task.Status{task.StatusReady},
		OnlyUnblocked: true,
		OnlyUnclaimed: true,
		Limit:         maxReady,
	}, refNow)

	allSummaries := FilterAndSortCompactQueue(tasks, objSlugs, parentKeys, leases, task.AgentWorkQueueFilter{
		WorkspaceID: proj.WorkspaceID,
		ProjectID:   proj.ID,
		Limit:       int32(max(len(tasks), 1)),
	}, refNow)

	var activeLeaseItems []task.CompactTaskSummary
	var blockedItems []task.CompactTaskSummary
	for _, s := range allSummaries {
		if s.LeaseExpiresAt != nil {
			activeLeaseItems = append(activeLeaseItems, s.Clone())
		}
		if !s.Status.IsTerminal() && (s.UnresolvedBlockerCount > 0 || s.Status == task.StatusBlocked) {
			if int32(len(blockedItems)) < maxBlocked {
				blockedItems = append(blockedItems, s.Clone())
			}
		}
	}

	return task.ProjectDigest{
		WorkspaceID:      proj.WorkspaceID,
		ProjectID:        proj.ID,
		ProjectSlug:      proj.Slug,
		ProjectKeyPrefix: proj.KeyPrefix,
		GeneratedAt:      refNow,
		CountsByStatus:   counts,
		ActiveObjectives: activeObjItems,
		ReadyQueue:       readyQueue,
		ActiveLeases:     activeLeaseItems,
		BlockedTasks:     blockedItems,
	}
}

// AuditQueryNeedsPostFilter reports whether filter carries predicates not evaluated
// directly inside SQL's ListAuditLogsByWorkspace or ListAuditLogsByResource queries.
func AuditQueryNeedsPostFilter(filter audit.QueryFilter) bool {
	hasBothResource := filter.ResourceType != "" && filter.ResourceID != ""
	hasPartialResource := (filter.ResourceType != "" || filter.ResourceID != "") && !hasBothResource
	return filter.ActorID != uuid.Nil ||
		filter.Action != "" ||
		filter.Since != nil ||
		filter.Until != nil ||
		hasPartialResource
}

// FilterSortAndSliceAuditRecords applies QueryFilter predicates and reverse-chronological sorting.
func FilterSortAndSliceAuditRecords(items []audit.Record, filter audit.QueryFilter) []audit.Record {
	matched := make([]audit.Record, 0, len(items))
	for _, r := range items {
		if filter.ActorID != uuid.Nil && r.ActorID != filter.ActorID {
			continue
		}
		if filter.Action != "" && r.Action != filter.Action {
			continue
		}
		if filter.ResourceType != "" && r.ResourceType != filter.ResourceType {
			continue
		}
		if filter.ResourceID != "" && r.ResourceID != filter.ResourceID {
			continue
		}
		if filter.Since != nil && r.OccurredAt.Before(filter.Since.UTC()) {
			continue
		}
		if filter.Until != nil && r.OccurredAt.After(filter.Until.UTC()) {
			continue
		}
		matched = append(matched, r.Clone())
	}
	slices.SortFunc(matched, func(a, b audit.Record) int {
		if !a.OccurredAt.Equal(b.OccurredAt) {
			if a.OccurredAt.After(b.OccurredAt) {
				return -1
			}
			return 1
		}
		return cmp.Compare(b.ID.String(), a.ID.String())
	})
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	return PaginateSlice(matched, filter.Offset, limit)
}

// PaginateSlice returns a bounded subslice respecting non-negative offset and optional positive limit.
func PaginateSlice[T any](items []T, offset, limit int32) []T {
	if len(items) == 0 {
		return nil
	}
	start := int(max(offset, 0))
	if start >= len(items) {
		return nil
	}
	sub := items[start:]
	if limit > 0 && int(limit) < len(sub) {
		sub = sub[:int(limit)]
	}
	out := make([]T, len(sub))
	copy(out, sub)
	return out
}
