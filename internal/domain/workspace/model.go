// Package workspace models the root multi-tenant governance boundary in Flowsk.
//
// Architectural & Ownership Invariants:
//   - Zero Transport Dependencies: This package contains pure native Go domain
//     models, persistence contracts, and service interfaces with zero imports of
//     ConnectRPC, Protobuf, HTTP, CLI, or MCP packages.
//   - Root Tenant Partition: Every project, objective, task, claim lease, comment,
//     and audit entry in Flowsk belongs to exactly one Workspace identified by its
//     immutable UUID. Cross-workspace data access is strictly forbidden.
package workspace

import (
	"time"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/domain/authctx"
)

// Workspace represents a tenant isolation boundary shared by human collaborators
// and autonomous coding agents.
type Workspace struct {
	// ID is the immutable primary UUID of the workspace.
	ID uuid.UUID

	// Slug is the URL- and CLI-safe unique handle within a Flowsk server instance
	// (for example, "acme-platform"). Lowercase ASCII alphanumeric with hyphens.
	Slug string

	// Name is the human-readable display title of the workspace.
	Name string

	// Description summarizes the team, organization, or initiative managed in this workspace.
	Description string

	// DefaultLeaseTTLSeconds configures the default duration in seconds granted to
	// agent ClaimLeases when a claim request does not specify an explicit TTL.
	DefaultLeaseTTLSeconds int64

	// Labels holds optional workspace-wide key/value metadata tags. Callers MUST
	// use LabelsCopy() or Clone() when crossing package boundaries.
	Labels map[string]string

	// CreatedAt is the UTC timestamp when the workspace was provisioned.
	CreatedAt time.Time

	// UpdatedAt is the UTC timestamp of the most recent workspace metadata update.
	UpdatedAt time.Time

	// DeletedAt is non-nil if the workspace has been soft-deleted.
	DeletedAt *time.Time
}

// LabelsCopy returns a defensive shallow copy of the Labels map so external
// callers cannot mutate internal map state.
func (w Workspace) LabelsCopy() map[string]string {
	if len(w.Labels) == 0 {
		return nil
	}
	out := make(map[string]string, len(w.Labels))
	for k, v := range w.Labels {
		out[k] = v
	}
	return out
}

// Clone returns a deep copy of Workspace, duplicating mutable maps and optional
// pointer timestamps.
func (w Workspace) Clone() Workspace {
	out := w
	out.Labels = w.LabelsCopy()
	if w.DeletedAt != nil {
		deleted := *w.DeletedAt
		out.DeletedAt = &deleted
	}
	return out
}

// MemberRole enumerates the RBAC permission level assigned to an actor inside a workspace.
type MemberRole string

const (
	// MemberRoleOwner grants full administrative lifecycle control over workspace settings and members.
	MemberRoleOwner MemberRole = "owner"

	// MemberRoleMaintainer grants authority to create/archive projects, objectives, and revoke stale leases.
	MemberRoleMaintainer MemberRole = "maintainer"

	// MemberRoleContributor grants authority for human developers and agents to create, claim, and update tasks.
	MemberRoleContributor MemberRole = "contributor"

	// MemberRoleReader grants read-only visibility into workspace projects, digests, and tasks.
	MemberRoleReader MemberRole = "reader"
)

// WorkspaceMember associates a human user or autonomous agent actor with a Workspace.
type WorkspaceMember struct {
	// WorkspaceID is the owning workspace UUID.
	WorkspaceID uuid.UUID

	// ActorID is the unique UUID of the human user or autonomous agent.
	ActorID uuid.UUID

	// Kind distinguishes human operators from autonomous AI agents.
	Kind authctx.ActorKind

	// Handle is the workspace-unique actor handle (for example, "paolo" or "swe-agent-1").
	Handle string

	// DisplayName is the human-friendly name shown in task activity timelines.
	DisplayName string

	// Role defines the authorization level of this actor inside the workspace.
	Role MemberRole

	// CreatedAt records when membership was granted in UTC.
	CreatedAt time.Time
}

// CreateWorkspaceInput carries validated parameters for provisioning a new Workspace.
type CreateWorkspaceInput struct {
	// Slug is the unique lowercase identifier for the workspace.
	Slug string

	// Name is the display title of the workspace.
	Name string

	// Description is optional context describing the workspace purpose.
	Description string

	// DefaultLeaseTTLSeconds sets the default task claim lease TTL (if <= 0, service applies system default).
	DefaultLeaseTTLSeconds int64

	// Labels holds optional metadata key/value pairs (defensively copied by service).
	Labels map[string]string
}

// UpdateWorkspaceInput carries partial or full metadata updates for an existing Workspace.
type UpdateWorkspaceInput struct {
	// WorkspaceID identifies the target workspace; must match caller's authorized workspace.
	WorkspaceID uuid.UUID

	// Name updates the display title when non-nil.
	Name *string

	// Description updates the workspace summary when non-nil.
	Description *string

	// DefaultLeaseTTLSeconds updates the default claim lease duration when non-nil.
	DefaultLeaseTTLSeconds *int64

	// Labels replaces workspace labels when non-nil.
	Labels map[string]string
}

// UpsertMemberInput registers or updates an actor's membership within a Workspace.
type UpsertMemberInput struct {
	// WorkspaceID is the target tenant boundary.
	WorkspaceID uuid.UUID

	// ActorID is the target principal UUID (if uuid.Nil on creation, generated by service).
	ActorID uuid.UUID

	// Kind specifies whether the member is human or agent.
	Kind authctx.ActorKind

	// Handle is the unique handle within the workspace.
	Handle string

	// DisplayName is the human-readable actor label.
	DisplayName string

	// Role is the assigned RBAC role.
	Role MemberRole
}
