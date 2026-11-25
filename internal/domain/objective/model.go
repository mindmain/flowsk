// Package objective models strategic milestones and high-level outcome goals
// within a Flowsk Project.
//
// Architectural & Ownership Invariants:
//   - Zero Transport Dependencies: Pure native Go domain types and contracts only.
//   - Mandatory Workspace Partitioning: Every Objective belongs strictly to a
//     (WorkspaceID, ProjectID) pair. Cross-workspace access is rejected with
//     errs.ErrPermissionDenied.
//   - Hierarchical Context Efficiency: Objectives provide concise strategic framing
//     (SuccessCriteria, Summary) so autonomous agents can align task execution
//     without loading entire historical backlogs into their context window.
package objective

import (
	"time"

	"github.com/google/uuid"
)

// Kind classifies the planning horizon and semantics of an Objective.
type Kind string

const (
	// KindGoal represents a high-level outcome-oriented strategic goal or OKR.
	KindGoal Kind = "goal"

	// KindMilestone represents a release or deliverable checkpoint grouping tasks.
	KindMilestone Kind = "milestone"

	// KindCycle represents a timeboxed iteration or sprint window.
	KindCycle Kind = "cycle"
)

// Valid reports whether k is a valid Objective Kind constant.
func (k Kind) Valid() bool {
	switch k {
	case KindGoal, KindMilestone, KindCycle:
		return true
	default:
		return false
	}
}

// Status represents the lifecycle progress of an Objective.
type Status string

const (
	// StatusDraft indicates the objective is being scoped and is not yet active.
	StatusDraft Status = "draft"

	// StatusActive indicates tasks aligned with this objective are actively prioritized.
	StatusActive Status = "active"

	// StatusAtRisk indicates the objective is active but flagged at risk due to blockers or schedule slip.
	StatusAtRisk Status = "at_risk"

	// StatusAchieved indicates all measurable success criteria have been satisfied.
	StatusAchieved Status = "achieved"

	// StatusDropped indicates the objective was superseded or cancelled.
	StatusDropped Status = "dropped"
)

// Valid reports whether s is a valid Objective Status constant.
func (s Status) Valid() bool {
	switch s {
	case StatusDraft, StatusActive, StatusAtRisk, StatusAchieved, StatusDropped:
		return true
	default:
		return false
	}
}

// Objective represents a milestone or strategic outcome grouping related Tasks
// inside a single Project.
type Objective struct {
	// ID is the immutable primary UUID of the objective.
	ID uuid.UUID

	// WorkspaceID is the mandatory owning tenant workspace UUID.
	WorkspaceID uuid.UUID

	// ProjectID is the owning Project UUID within WorkspaceID.
	ProjectID uuid.UUID

	// Kind classifies whether the objective is a goal, milestone, or cycle (defaults to KindGoal).
	Kind Kind

	// Slug is a project-unique short identifier (for example, "q4-zero-downtime-migrations").
	Slug string

	// Title is a concise one-line goal statement.
	Title string

	// Summary explains the strategic rationale and architectural constraints.
	Summary string

	// SuccessCriteria lists explicit, verifiable acceptance outcomes. Callers MUST
	// use SuccessCriteriaCopy() or Clone() when crossing package boundaries.
	SuccessCriteria []string

	// Status tracks the lifecycle state of the objective.
	Status Status

	// PriorityRank orders competing objectives within a project (lower integer = higher priority).
	PriorityRank int32

	// TargetDate is an optional UTC target completion timestamp.
	TargetDate *time.Time

	// CreatedBy identifies the human or agent ActorID that authored the objective.
	CreatedBy uuid.UUID

	// CreatedAt is the UTC creation timestamp.
	CreatedAt time.Time

	// UpdatedAt is the UTC timestamp of the latest modification.
	UpdatedAt time.Time

	// DeletedAt is non-nil if the objective has been soft-deleted.
	DeletedAt *time.Time
}

// SuccessCriteriaCopy returns a defensive copy of SuccessCriteria so callers
// cannot mutate internal slice state.
func (o Objective) SuccessCriteriaCopy() []string {
	if len(o.SuccessCriteria) == 0 {
		return nil
	}
	out := make([]string, len(o.SuccessCriteria))
	copy(out, o.SuccessCriteria)
	return out
}

// Clone returns a deep copy of Objective, duplicating slices and optional time pointers.
func (o Objective) Clone() Objective {
	out := o
	out.SuccessCriteria = o.SuccessCriteriaCopy()
	if o.TargetDate != nil {
		target := *o.TargetDate
		out.TargetDate = &target
	}
	if o.DeletedAt != nil {
		deleted := *o.DeletedAt
		out.DeletedAt = &deleted
	}
	return out
}

// CreateObjectiveInput carries command parameters for authoring a new Objective.
type CreateObjectiveInput struct {
	// WorkspaceID is the mandatory tenant scope verified against caller identity.
	WorkspaceID uuid.UUID

	// ProjectID identifies the owning project inside WorkspaceID.
	ProjectID uuid.UUID

	// Kind classifies whether the objective is a goal, milestone, or cycle (defaults to KindGoal if empty).
	Kind Kind

	// Slug is the project-unique handle for CLI and Code Mode JavaScript SDK references.
	Slug string

	// Title is the concise goal headline.
	Title string

	// Summary provides strategic context for agents and humans.
	Summary string

	// SuccessCriteria enumerates verifiable completion criteria (defensively copied).
	SuccessCriteria []string

	// Status sets the initial lifecycle state (defaults to StatusActive if empty).
	Status Status

	// PriorityRank sets relative milestone priority within the project.
	PriorityRank int32

	// TargetDate is an optional UTC milestone deadline.
	TargetDate *time.Time
}

// UpdateObjectiveInput carries partial updates to an existing Objective.
type UpdateObjectiveInput struct {
	// WorkspaceID is the mandatory tenant scope.
	WorkspaceID uuid.UUID

	// ObjectiveID identifies the objective to update.
	ObjectiveID uuid.UUID

	// Kind updates the objective classification when non-nil.
	Kind *Kind

	// Title updates the headline when non-nil.
	Title *string

	// Summary updates the rationale when non-nil.
	Summary *string

	// SuccessCriteria replaces completion criteria when non-nil (defensively copied).
	SuccessCriteria []string

	// Status transitions lifecycle state when non-nil.
	Status *Status

	// PriorityRank updates relative ranking when non-nil.
	PriorityRank *int32

	// TargetDate updates the optional target date when non-nil.
	TargetDate *time.Time
}

// ListObjectivesFilter constrains workspace- and project-scoped objective queries.
type ListObjectivesFilter struct {
	// WorkspaceID is the mandatory tenant scope.
	WorkspaceID uuid.UUID

	// ProjectID optionally restricts results to a single project when non-nil/non-zero.
	ProjectID uuid.UUID

	// Kind optionally filters objectives by classification.
	Kind *Kind

	// Status optionally filters objectives by lifecycle state.
	Status *Status

	// Limit caps returned row count.
	Limit int32

	// Offset specifies pagination offset.
	Offset int32
}
