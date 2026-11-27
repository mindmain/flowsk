// Package task models the core execution unit of Flowsk: hierarchical Tasks,
// dependency/hierarchy TaskRelations, structured agent/human TaskComments,
// time-bounded exclusive ClaimLeases, and token-frugal read projections
// (`CompactTaskSummary` and `ProjectDigest`) engineered for autonomous AI agents.
//
// Architectural, Concurrency & Ownership Invariants:
//   - Zero Transport Dependencies: Pure native Go domain types only; never imports
//     ConnectRPC, Protobuf, HTTP, CLI, or MCP packages.
//   - Mandatory Workspace Isolation: Every Task, TaskRelation, TaskComment, and
//     ClaimLease carries an explicit WorkspaceID verified against authctx.Identity.
//   - Optimistic + Lease Concurrency: Mutating or claiming a task checks both
//     active ClaimLease exclusivity (returning errs.ErrLeaseHeld if held by another
//     principal with ExpiresAt > now) and optional Version optimistic locking.
//   - Defensive Copies: All slice and map fields expose dedicated copy helpers and
//     Clone() methods so callers cannot mutate shared internal state.
package task

import (
	"time"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/domain/authctx"
)

// Status enumerates canonical workflow states of a Task.
type Status string

const (
	// StatusBacklog indicates the task is captured but not yet refined or scheduled for execution.
	StatusBacklog Status = "backlog"

	// StatusReady indicates the task has clear acceptance criteria and is eligible for claiming once unblocked.
	StatusReady Status = "ready"

	// StatusInProgress indicates an actor is actively executing work on the task (typically under a ClaimLease).
	StatusInProgress Status = "in_progress"

	// StatusInReview indicates implementation is complete and awaiting verification or human/agent review.
	StatusInReview Status = "in_review"

	// StatusBlocked indicates external or technical impediments prevent progress.
	StatusBlocked Status = "blocked"

	// StatusDone indicates all acceptance criteria are verified and complete.
	StatusDone Status = "done"

	// StatusCancelled indicates the task was intentionally abandoned or superseded.
	StatusCancelled Status = "cancelled"

	// StatusArchived indicates the task is archived and hidden from active boards,
	// eligible for permanent workspace archive purging (`PurgeArchivedTasks`).
	StatusArchived Status = "archived"
)

// Valid reports whether s is a canonical Task Status value.
func (s Status) Valid() bool {
	switch s {
	case StatusBacklog, StatusReady, StatusInProgress, StatusInReview, StatusBlocked, StatusDone, StatusCancelled, StatusArchived:
		return true
	default:
		return false
	}
}

// IsTerminal reports whether Status represents a closed terminal state (done, cancelled, or archived).
func (s Status) IsTerminal() bool {
	return s == StatusDone || s == StatusCancelled || s == StatusArchived
}

// SignalGlowWindow is the live activity duration (10 minutes) during which a task
// in an active execution/review column illuminates its pulsing green signal glow.
const SignalGlowWindow = 10 * time.Minute

// AcceptsSignal reports whether a task in Status s updates `LastSignalAt` upon
// agent/CLI/MCP creation, point-reads, updates, leases, comments, docs, or transitions.
// Signals are recorded across `backlog`, `ready`, `in_progress`, and `in_review`,
// and stop once a task enters `blocked`, `done`, `cancelled`, or `archived`.
func (s Status) AcceptsSignal() bool {
	switch s {
	case StatusBacklog, StatusReady, StatusInProgress, StatusInReview:
		return true
	default:
		return false
	}
}

// IlluminatesSignalGlow reports whether Status s is eligible to display the live
// pulsing green agent-activity glow (`ready`, `in_progress`, or `in_review`).
// Tasks in `backlog`, `blocked`, `done`, `cancelled`, or `archived` never glow.
func (s Status) IlluminatesSignalGlow() bool {
	switch s {
	case StatusReady, StatusInProgress, StatusInReview:
		return true
	default:
		return false
	}
}

// Priority defines deterministic urgency tiers for ordering agent work queues.
type Priority int32

const (
	// PriorityCritical (P0) indicates urgent production or critical-path blocker work.
	PriorityCritical Priority = 0

	// PriorityHigh (P1) indicates high-priority milestone deliverables.
	PriorityHigh Priority = 1

	// PriorityMedium (P2) is the standard default execution priority.
	PriorityMedium Priority = 2

	// PriorityLow (P3) indicates opportunistic improvements or tech-debt cleanup.
	PriorityLow Priority = 3
)

// Kind classifies the structural intent of a Task node in the work graph.
type Kind string

const (
	// KindEpic represents a multi-task parent initiative decomposed into child tasks.
	KindEpic Kind = "epic"

	// KindFeature represents a user- or API-visible capability addition.
	KindFeature Kind = "feature"

	// KindBug represents a defect fix or regression repair.
	KindBug Kind = "bug"

	// KindChore represents refactoring, tooling, CI, or operational maintenance.
	KindChore Kind = "chore"

	// KindResearch represents bounded investigation or architectural spike work.
	KindResearch Kind = "research"
)

// Task represents an actionable unit of engineering work scoped to a Workspace and Project.
type Task struct {
	// ID is the immutable primary UUID of the task.
	ID uuid.UUID

	// WorkspaceID is the mandatory owning tenant workspace UUID.
	WorkspaceID uuid.UUID

	// ProjectID is the owning Project UUID within WorkspaceID.
	ProjectID uuid.UUID

	// ObjectiveID optionally links this task to a strategic Objective within the same Project.
	ObjectiveID *uuid.UUID

	// ParentTaskID optionally links this task as a hierarchical child of an epic or parent task.
	ParentTaskID *uuid.UUID

	// Key is the human- and LLM-compact project-scoped identifier (for example, "FLW-104").
	// Unique within WorkspaceID.
	Key string

	// ExternalID is an optional caller-supplied external tracker identifier
	// (for example, "CU-869abc", "LIN-42", or "GH-108"), independent of ID and Key.
	// When non-empty, ExternalID is unique per (ProjectID, ExternalID) across active tasks.
	ExternalID string

	// Kind categorizes the task (epic, feature, bug, chore, research).
	Kind Kind

	// Title is a concise imperative summary of the work item.
	Title string

	// Description holds structured Markdown context, constraints, and implementation notes.
	Description string

	// AcceptanceCriteria enumerates verifiable Definition-of-Done checkpoints.
	// Callers MUST use AcceptanceCriteriaCopy() or Clone() across package boundaries.
	AcceptanceCriteria []string

	// Status is the current workflow lifecycle state.
	Status Status

	// Priority determines queue ordering (0 = Critical .. 3 = Low).
	Priority Priority

	// AssigneeActorID optionally identifies the primary assigned human or agent principal.
	AssigneeActorID *uuid.UUID

	// Tags holds capability or domain routing tags (for example, "golang", "db", "frontend").
	Tags []string

	// Labels holds structured key/value metadata annotations.
	Labels map[string]string

	// ActiveLease contains the current non-expired exclusive ClaimLease if held, or nil.
	ActiveLease *ClaimLease

	// UnresolvedBlockerCount reflects the number of non-terminal upstream tasks blocking this task.
	UnresolvedBlockerCount int32

	// HasMarkdownDoc reports whether an optional extended Markdown document (`<task_id>.md`)
	// is currently indexed for this task in Flowsk's global storage folder.
	HasMarkdownDoc bool

	// Version is the monotonically increasing optimistic concurrency counter incremented on every write.
	Version int64

	// CreatedBy is the ActorID of the creator.
	CreatedBy uuid.UUID

	// CreatedAt is the UTC creation timestamp.
	CreatedAt time.Time

	// UpdatedAt is the UTC timestamp of the last state or field mutation.
	UpdatedAt time.Time

	// CompletedAt is non-nil when Status transitioned into a terminal state.
	CompletedAt *time.Time

	// LastSignalAt is the optional UTC timestamp of the most recent agent/CLI/MCP
	// signal recorded while the task was in an active signal-accepting state (nil by default).
	LastSignalAt *time.Time

	// DeletedAt is non-nil if the task has been soft-deleted.
	DeletedAt *time.Time
}

// HasActiveSignalGlow reports whether the task should display the live pulsing green
// activity glow at reference timestamp `now` (status in `ready`/`in_progress`/`in_review`
// and `LastSignalAt` non-nil within `SignalGlowWindow`).
func (t Task) HasActiveSignalGlow(now time.Time) bool {
	if !t.Status.IlluminatesSignalGlow() || t.LastSignalAt == nil || t.LastSignalAt.IsZero() {
		return false
	}
	return now.UTC().Sub(t.LastSignalAt.UTC()) <= SignalGlowWindow
}

// AcceptanceCriteriaCopy returns a defensive copy of AcceptanceCriteria.
func (t Task) AcceptanceCriteriaCopy() []string {
	if len(t.AcceptanceCriteria) == 0 {
		return nil
	}
	out := make([]string, len(t.AcceptanceCriteria))
	copy(out, t.AcceptanceCriteria)
	return out
}

// TagsCopy returns a defensive copy of Tags.
func (t Task) TagsCopy() []string {
	if len(t.Tags) == 0 {
		return nil
	}
	out := make([]string, len(t.Tags))
	copy(out, t.Tags)
	return out
}

// LabelsCopy returns a defensive shallow copy of Labels.
func (t Task) LabelsCopy() map[string]string {
	if len(t.Labels) == 0 {
		return nil
	}
	out := make(map[string]string, len(t.Labels))
	for k, v := range t.Labels {
		out[k] = v
	}
	return out
}

// Clone returns a deep copy of Task, duplicating all slices, maps, and optional pointers.
func (t Task) Clone() Task {
	out := t
	out.AcceptanceCriteria = t.AcceptanceCriteriaCopy()
	out.Tags = t.TagsCopy()
	out.Labels = t.LabelsCopy()
	if t.ObjectiveID != nil {
		id := *t.ObjectiveID
		out.ObjectiveID = &id
	}
	if t.ParentTaskID != nil {
		id := *t.ParentTaskID
		out.ParentTaskID = &id
	}
	if t.AssigneeActorID != nil {
		id := *t.AssigneeActorID
		out.AssigneeActorID = &id
	}
	if t.ActiveLease != nil {
		lease := t.ActiveLease.Clone()
		out.ActiveLease = &lease
	}
	if t.CompletedAt != nil {
		completed := *t.CompletedAt
		out.CompletedAt = &completed
	}
	if t.LastSignalAt != nil {
		sig := *t.LastSignalAt
		out.LastSignalAt = &sig
	}
	if t.DeletedAt != nil {
		deleted := *t.DeletedAt
		out.DeletedAt = &deleted
	}
	return out
}

// RelationType classifies directed edges between two Tasks in the same Workspace.
type RelationType string

const (
	// RelationBlocks means SourceTaskID must reach StatusDone before TargetTaskID can be started/completed.
	RelationBlocks RelationType = "blocks"

	// RelationSubtaskOf links SourceTaskID as a child decomposition step of TargetTaskID.
	RelationSubtaskOf RelationType = "subtask_of"

	// RelationRelatesTo links two contextually related tasks without hard execution blocking.
	RelationRelatesTo RelationType = "relates_to"

	// RelationDuplicates marks SourceTaskID as a duplicate of canonical TargetTaskID.
	RelationDuplicates RelationType = "duplicates"
)

// Valid reports whether r is a supported RelationType.
func (r RelationType) Valid() bool {
	switch r {
	case RelationBlocks, RelationSubtaskOf, RelationRelatesTo, RelationDuplicates:
		return true
	default:
		return false
	}
}

// TaskRelation represents a directed typed edge from SourceTaskID to TargetTaskID
// within a single Workspace.
type TaskRelation struct {
	// ID is the unique UUID of the relation edge.
	ID uuid.UUID

	// WorkspaceID is the mandatory owning tenant workspace UUID.
	WorkspaceID uuid.UUID

	// SourceTaskID is the origin task of the directed relation.
	SourceTaskID uuid.UUID

	// TargetTaskID is the destination task of the directed relation.
	TargetTaskID uuid.UUID

	// Type defines the semantic meaning of the directed edge.
	Type RelationType

	// Note holds an optional concise explanation of why the relation exists.
	Note string

	// CreatedBy identifies the actor who established the edge.
	CreatedBy uuid.UUID

	// CreatedAt is the UTC creation timestamp.
	CreatedAt time.Time
}

// CommentKind categorizes structured timeline notes left by agents and humans so
// downstream agents can filter for handoffs, decisions, or blockers with minimal tokens.
type CommentKind string

const (
	// CommentKindProgress records incremental execution milestones or verification outputs.
	CommentKindProgress CommentKind = "progress"

	// CommentKindBlocker documents an impediment preventing task completion.
	CommentKindBlocker CommentKind = "blocker"

	// CommentKindDecision records an architectural or product trade-off settled during work.
	CommentKindDecision CommentKind = "decision"

	// CommentKindHandoff provides structured state transfer for the next agent or reviewer.
	CommentKindHandoff CommentKind = "handoff"

	// CommentKindNote is a general-purpose human or agent remark.
	CommentKindNote CommentKind = "note"
)

// TaskComment represents an immutable or append-focused structured log entry on a Task.
type TaskComment struct {
	// ID is the unique UUID of the comment.
	ID uuid.UUID

	// WorkspaceID is the mandatory owning tenant workspace UUID.
	WorkspaceID uuid.UUID

	// TaskID identifies the parent Task within WorkspaceID.
	TaskID uuid.UUID

	// AuthorActorID is the principal UUID of the human or agent author.
	AuthorActorID uuid.UUID

	// AuthorKind indicates whether the comment was authored by a human or an agent.
	AuthorKind authctx.ActorKind

	// AuthorHandle is the compact handle of the author (for example, "agent-backend-2").
	AuthorHandle string

	// Kind classifies the semantic intent of the comment (handoff, decision, blocker, etc.).
	Kind CommentKind

	// Body contains concise Markdown text.
	Body string

	// CreatedAt is the UTC timestamp when the comment was recorded.
	CreatedAt time.Time
}

// ClaimLease represents an exclusive, time-bounded execution lock acquired by an
// agent or human actor on a Task to prevent duplicate concurrent work by parallel agents.
//
// Lease Semantics:
//   - Only one active lease may exist per Task at any instant.
//   - If an existing row has ExpiresAt <= now, a new claimant may atomically preempt
//     the expired lease inside a single ACID transaction (recording a Claim audit event).
//   - If ExpiresAt > now and HolderActorID != caller.ActorID, Claim attempts MUST fail
//     with errs.ErrLeaseHeld unless explicit maintainer force-override is invoked.
type ClaimLease struct {
	// TaskID is the leased Task UUID.
	TaskID uuid.UUID

	// WorkspaceID is the mandatory owning tenant workspace UUID.
	WorkspaceID uuid.UUID

	// LeaseToken is an opaque fencing token generated upon acquisition, passed on
	// heartbeat renewals and voluntary releases to prevent ABA races.
	LeaseToken string

	// HolderActorID is the principal UUID currently holding exclusive execution rights.
	HolderActorID uuid.UUID

	// HolderKind distinguishes human vs. autonomous agent lease holders.
	HolderKind authctx.ActorKind

	// HolderHandle is the compact display handle of the lease holder.
	HolderHandle string

	// AgentSessionID is an optional harness/conversation run identifier supplied by an agent.
	AgentSessionID string

	// AcquiredAt is the UTC timestamp when the current lease began.
	AcquiredAt time.Time

	// HeartbeatAt is the UTC timestamp of the latest renewal heartbeat.
	HeartbeatAt time.Time

	// ExpiresAt is the UTC deadline after which the lease automatically becomes eligible
	// for preemption by other workers if not renewed.
	ExpiresAt time.Time
}

// IsExpiredAt reports whether the lease has expired relative to the supplied UTC reference time.
func (l ClaimLease) IsExpiredAt(now time.Time) bool {
	return !now.Before(l.ExpiresAt)
}

// Clone returns a value copy of ClaimLease.
func (l ClaimLease) Clone() ClaimLease {
	return l
}

// CompactTaskSummary is an ultra-dense, token-frugal projection of a Task designed
// specifically for CLI polling, MCP tool responses, and multi-agent work-queue digests
// where full Markdown descriptions would waste LLM context window tokens.
type CompactTaskSummary struct {
	// ID is the task UUID.
	ID uuid.UUID

	// Key is the short human/LLM reference key (for example, "FLW-104").
	Key string

	// ExternalID is the optional external tracker identifier scoped to the project (empty if unlinked).
	ExternalID string

	// Kind classifies the task type.
	Kind Kind

	// Title is the single-line task summary.
	Title string

	// Status is the current workflow state.
	Status Status

	// Priority is the numeric urgency rank (0..3).
	Priority Priority

	// ObjectiveID is the optional UUID of the linked Objective (nil if unlinked).
	ObjectiveID *uuid.UUID

	// ObjectiveSlug is the optional short slug of the linked Objective (empty if unlinked).
	ObjectiveSlug string

	// ParentKey is the optional short key of the parent epic/task (empty if top-level).
	ParentKey string

	// HolderActorID is the optional principal UUID of the active non-expired lease holder (nil if unclaimed).
	HolderActorID *uuid.UUID

	// HolderHandle is the actor handle of the active non-expired lease holder (empty if unclaimed).
	HolderHandle string

	// LeaseExpiresAt is non-nil when an active lease is currently held.
	LeaseExpiresAt *time.Time

	// UnresolvedBlockerCount is > 0 if upstream blocking tasks are not yet Done.
	UnresolvedBlockerCount int32

	// HasMarkdownDoc reports whether an optional extended Markdown document (`<task_id>.md`) is stored for this task.
	HasMarkdownDoc bool

	// Tags lists compact routing tags.
	Tags []string

	// LastSignalAt is the optional UTC timestamp of the most recent agent/CLI/MCP signal (nil if unset).
	LastSignalAt *time.Time

	// UpdatedAt is the UTC timestamp of the most recent change.
	UpdatedAt time.Time
}

// Clone returns a deep copy of CompactTaskSummary, duplicating Tags and optional pointers.
func (c CompactTaskSummary) Clone() CompactTaskSummary {
	out := c
	if len(c.Tags) > 0 {
		out.Tags = make([]string, len(c.Tags))
		copy(out.Tags, c.Tags)
	}
	if c.ObjectiveID != nil {
		oid := *c.ObjectiveID
		out.ObjectiveID = &oid
	}
	if c.HolderActorID != nil {
		hid := *c.HolderActorID
		out.HolderActorID = &hid
	}
	if c.LeaseExpiresAt != nil {
		exp := *c.LeaseExpiresAt
		out.LeaseExpiresAt = &exp
	}
	if c.LastSignalAt != nil {
		sig := *c.LastSignalAt
		out.LastSignalAt = &sig
	}
	return out
}

// ObjectiveDigestItem summarizes an active Objective inside a ProjectDigest.
type ObjectiveDigestItem struct {
	// ID is the Objective UUID.
	ID uuid.UUID

	// Slug is the project-scoped objective slug.
	Slug string

	// Title is the concise objective headline.
	Title string

	// PriorityRank indicates relative priority among active objectives.
	PriorityRank int32

	// OpenTaskCount is the number of non-terminal tasks linked to this objective.
	OpenTaskCount int32

	// DoneTaskCount is the number of completed tasks linked to this objective.
	DoneTaskCount int32
}

// ProjectDigest provides a single-roundtrip, token-budgeted situational awareness
// snapshot of a Project so an autonomous coding agent can orient itself, inspect
// active milestones, see in-flight peer leases, and pick the next ready task in
// minimal tokens.
type ProjectDigest struct {
	// WorkspaceID is the owning tenant workspace UUID.
	WorkspaceID uuid.UUID

	// ProjectID is the target Project UUID.
	ProjectID uuid.UUID

	// ProjectSlug is the canonical project slug.
	ProjectSlug string

	// ProjectKeyPrefix is the task key prefix (for example, "FLW").
	ProjectKeyPrefix string

	// GeneratedAt is the UTC snapshot timestamp.
	GeneratedAt time.Time

	// CountsByStatus maps each Task Status to its current active task count in the project.
	CountsByStatus map[Status]int32

	// ActiveObjectives lists active project objectives ordered by PriorityRank.
	ActiveObjectives []ObjectiveDigestItem

	// ReadyQueue lists the top-priority unblocked, unclaimed tasks ready for immediate claiming.
	ReadyQueue []CompactTaskSummary

	// ActiveLeases lists tasks currently claimed under non-expired leases by humans or peer agents.
	ActiveLeases []CompactTaskSummary

	// BlockedTasks lists high-priority tasks currently awaiting upstream blocker completion.
	BlockedTasks []CompactTaskSummary
}

// Clone returns a deep copy of ProjectDigest, duplicating maps and slices.
func (d ProjectDigest) Clone() ProjectDigest {
	out := d
	if len(d.CountsByStatus) > 0 {
		out.CountsByStatus = make(map[Status]int32, len(d.CountsByStatus))
		for k, v := range d.CountsByStatus {
			out.CountsByStatus[k] = v
		}
	}
	if len(d.ActiveObjectives) > 0 {
		out.ActiveObjectives = make([]ObjectiveDigestItem, len(d.ActiveObjectives))
		copy(out.ActiveObjectives, d.ActiveObjectives)
	}
	if len(d.ReadyQueue) > 0 {
		out.ReadyQueue = make([]CompactTaskSummary, len(d.ReadyQueue))
		for i, item := range d.ReadyQueue {
			out.ReadyQueue[i] = item.Clone()
		}
	}
	if len(d.ActiveLeases) > 0 {
		out.ActiveLeases = make([]CompactTaskSummary, len(d.ActiveLeases))
		for i, item := range d.ActiveLeases {
			out.ActiveLeases[i] = item.Clone()
		}
	}
	if len(d.BlockedTasks) > 0 {
		out.BlockedTasks = make([]CompactTaskSummary, len(d.BlockedTasks))
		for i, item := range d.BlockedTasks {
			out.BlockedTasks[i] = item.Clone()
		}
	}
	return out
}

// AgentWorkQueueFilter parameterizes deterministic polling queries used by autonomous
// agents seeking actionable work items in a Workspace/Project.
type AgentWorkQueueFilter struct {
	// WorkspaceID is the mandatory tenant scope verified against caller identity.
	WorkspaceID uuid.UUID

	// ProjectID restricts polling to a specific Project when non-zero.
	ProjectID uuid.UUID

	// ObjectiveID restricts polling to tasks aligned with a specific Objective when non-nil.
	ObjectiveID *uuid.UUID

	// ParentTaskID restricts polling to subtasks of a specific epic/parent task when non-nil.
	ParentTaskID *uuid.UUID

	// ExternalID restricts polling to tasks matching this exact ExternalID when non-empty.
	ExternalID string

	// Statuses restricts tasks to specific lifecycle states (defaults to [StatusReady] for claim queues).
	Statuses []Status

	// RequiredTags restricts results to tasks carrying all specified capability tags.
	RequiredTags []string

	// OnlyUnblocked excludes tasks with UnresolvedBlockerCount > 0 when true.
	OnlyUnblocked bool

	// OnlyUnclaimed excludes tasks with an active non-expired ClaimLease when true.
	OnlyUnclaimed bool

	// MaxPriority optionally caps maximum Priority integer (for example, <= PriorityHigh).
	MaxPriority *Priority

	// Limit bounds returned summaries to preserve LLM token budgets (default e.g. 20, max e.g. 100).
	Limit int32
}

// CreateTaskInput carries parameters for creating a new Task inside an authorized Workspace.
type CreateTaskInput struct {
	// WorkspaceID is the mandatory tenant scope.
	WorkspaceID uuid.UUID

	// ProjectID identifies the target Project inside WorkspaceID.
	ProjectID uuid.UUID

	// ObjectiveID optionally links the task to an Objective in the same Project.
	ObjectiveID *uuid.UUID

	// ParentTaskID optionally attaches the task under an epic or parent task.
	ParentTaskID *uuid.UUID

	// ExternalID optionally links an external tracker ID (unique per active task in ProjectID).
	ExternalID string

	// Kind specifies epic, feature, bug, chore, or research.
	Kind Kind

	// Title is the concise imperative task title.
	Title string

	// Description provides Markdown specification details.
	Description string

	// AcceptanceCriteria lists verifiable completion conditions (defensively copied).
	AcceptanceCriteria []string

	// Status sets initial status (defaults to StatusReady or StatusBacklog per service rules).
	Status Status

	// Priority sets initial urgency rank.
	Priority Priority

	// AssigneeActorID optionally pre-assigns an actor.
	AssigneeActorID *uuid.UUID

	// Tags attaches capability/routing tags (defensively copied).
	Tags []string

	// Labels attaches key/value metadata (defensively copied).
	Labels map[string]string
}

// UpdateTaskInput carries partial modifications to an existing Task.
type UpdateTaskInput struct {
	// WorkspaceID is the mandatory tenant scope.
	WorkspaceID uuid.UUID

	// TaskID identifies the target task.
	TaskID uuid.UUID

	// ExpectedVersion optionally enforces optimistic concurrency when non-nil.
	ExpectedVersion *int64

	// ObjectiveID updates objective linkage when non-nil.
	ObjectiveID *uuid.UUID

	// ParentTaskID updates parent hierarchy linkage when non-nil.
	ParentTaskID *uuid.UUID

	// ExternalID updates (or when empty string clears) the project-scoped external tracker ID when non-nil.
	ExternalID *string

	// Kind updates task classification when non-nil.
	Kind *Kind

	// Title updates the summary title when non-nil.
	Title *string

	// Description updates the Markdown body when non-nil.
	Description *string

	// AcceptanceCriteria replaces acceptance criteria when non-nil.
	AcceptanceCriteria []string

	// Priority updates task urgency when non-nil.
	Priority *Priority

	// AssigneeActorID updates primary assignee when non-nil.
	AssigneeActorID *uuid.UUID

	// Tags replaces routing tags when non-nil.
	Tags []string

	// Labels replaces metadata labels when non-nil.
	Labels map[string]string
}

// TransitionTaskInput requests a lifecycle state transition on a Task.
type TransitionTaskInput struct {
	// WorkspaceID is the mandatory tenant scope.
	WorkspaceID uuid.UUID

	// TaskID identifies the target task.
	TaskID uuid.UUID

	// TargetStatus is the desired destination Status.
	TargetStatus Status

	// ResolutionComment optionally appends a structured handoff/progress TaskComment atomically.
	ResolutionComment string

	// ReleaseActiveLease instructs the service to atomically release the caller's
	// ClaimLease upon transitioning to InReview, Blocked, Done, or Cancelled.
	ReleaseActiveLease bool

	// ExpectedVersion optionally enforces optimistic concurrency when non-nil.
	ExpectedVersion *int64
}

// ClaimTaskInput requests acquisition or heartbeat renewal of an exclusive ClaimLease.
type ClaimTaskInput struct {
	// WorkspaceID is the mandatory tenant scope.
	WorkspaceID uuid.UUID

	// TaskID identifies the task to claim or renew.
	TaskID uuid.UUID

	// TTLSeconds specifies requested lease duration in seconds (uses Workspace default if <= 0).
	TTLSeconds int64

	// AgentSessionID is an optional harness run identifier recorded on the lease.
	AgentSessionID string

	// LeaseToken is supplied when renewing an existing lease held by the caller.
	LeaseToken string

	// AutoTransitionInProgress transitions StatusReady -> StatusInProgress atomically when true.
	AutoTransitionInProgress bool
}

// ReleaseTaskInput releases an active ClaimLease on a Task.
type ReleaseTaskInput struct {
	// WorkspaceID is the mandatory tenant scope.
	WorkspaceID uuid.UUID

	// TaskID identifies the leased task.
	TaskID uuid.UUID

	// LeaseToken verifies the caller's active lease fencing token (ignored only when Force is true by Maintainer/Owner).
	LeaseToken string

	// Force allows a Workspace Maintainer or Owner to administratively revoke a stale peer lease.
	Force bool

	// HandoffComment optionally records a CommentKindHandoff comment atomically on release.
	HandoffComment string
}

// AddRelationInput creates a directed typed edge between two Tasks in the same Workspace.
type AddRelationInput struct {
	// WorkspaceID is the mandatory tenant scope.
	WorkspaceID uuid.UUID

	// SourceTaskID is the origin task UUID.
	SourceTaskID uuid.UUID

	// TargetTaskID is the destination task UUID.
	TargetTaskID uuid.UUID

	// Type specifies blocks, subtask_of, relates_to, or duplicates.
	Type RelationType

	// Note is an optional rationale for the edge.
	Note string
}

// AddCommentInput appends a structured TaskComment to a Task timeline.
type AddCommentInput struct {
	// WorkspaceID is the mandatory tenant scope.
	WorkspaceID uuid.UUID

	// TaskID identifies the target task.
	TaskID uuid.UUID

	// Kind classifies the comment (progress, blocker, decision, handoff, note).
	Kind CommentKind

	// Body is the Markdown comment content.
	Body string
}

// DigestOptions tunes token density when building a ProjectDigest.
type DigestOptions struct {
	// MaxReadyTasks caps the number of ReadyQueue items included (default e.g. 15).
	MaxReadyTasks int32

	// MaxBlockedTasks caps the number of BlockedTasks items included (default e.g. 10).
	MaxBlockedTasks int32
}

// PurgeArchivedResult summarizes a workspace-wide permanent purge of archived tasks.
type PurgeArchivedResult struct {
	// DeletedTaskCount is the number of archived tasks permanently deleted from the database.
	DeletedTaskCount int32

	// DeletedMarkdownCount is the number of associated task `.md` files removed from global storage.
	DeletedMarkdownCount int32

	// DeletedTaskKeys lists the human-friendly keys (e.g., "FLW-12") of all purged tasks.
	DeletedTaskKeys []string
}

// OrphanResourcesReport summarizes unindexed `.md` files and broken DB `has_markdown_doc`
// flags detected across a Workspace.
type OrphanResourcesReport struct {
	// OrphanTaskFilesCount counts `.md` files in `tasks/<workspace_id>/` without an active DB task.
	OrphanTaskFilesCount int32

	// MissingTaskFilesCount counts active DB tasks with `HasMarkdownDoc == true` whose `.md` file is absent.
	MissingTaskFilesCount int32

	// OrphanProjectFilesCount counts `.md` files in `projects/<workspace_id>/` without an active DB project.
	OrphanProjectFilesCount int32

	// MissingProjectFilesCount counts active DB projects with `HasMarkdownDoc == true` whose `.md` file is absent.
	MissingProjectFilesCount int32

	// TotalIssues is the sum of all detected orphan files and missing Markdown references.
	TotalIssues int32

	// IssueSummaries lists concise, path-free diagnostic summaries for each detected issue.
	IssueSummaries []string
}

// PruneResourcesResult reports the outcome of reconciling orphan `.md` files and
// broken DB `has_markdown_doc` flags in a Workspace.
type PruneResourcesResult struct {
	// RemovedTaskFilesCount counts orphan task `.md` files deleted (or matched in DryRun).
	RemovedTaskFilesCount int32

	// ClearedTaskRefsCount counts broken task `has_markdown_doc` flags cleared (or matched in DryRun).
	ClearedTaskRefsCount int32

	// RemovedProjectFilesCount counts orphan project `.md` files deleted (or matched in DryRun).
	RemovedProjectFilesCount int32

	// ClearedProjectRefsCount counts broken project `has_markdown_doc` flags cleared (or matched in DryRun).
	ClearedProjectRefsCount int32

	// TotalCleaned is the total number of reconciled files and DB flags.
	TotalCleaned int32

	// DryRun reports whether the prune ran in preview-only mode without mutating disk or DB.
	DryRun bool

	// CleanedSummaries lists concise, path-free summaries of each reconciled item.
	CleanedSummaries []string
}
