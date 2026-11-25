// Package project models logical software or engineering projects inside a Flowsk Workspace.
//
// CRITICAL ARCHITECTURAL INVARIANT — ZERO LOCAL FILESYSTEM PATHS:
//   - A Project aggregate MUST NEVER store, accept, persist, or return host-local
//     filesystem paths (such as "/Users/mario/Desktop/app" or "/Users/paolo/Downloads/app").
//   - Different developers and agent containers check out the same logical project
//     at completely arbitrary local directories on their own workstations or sandboxes
//     (for example, Mario keeps the repo on his Desktop while Paolo keeps it in Downloads).
//   - Mapping a developer's local working directory to a server-side (WorkspaceSlug, ProjectID)
//     is handled EXCLUSIVELY on the client machine by `internal/adapter/localpath`, stored
//     in the user's OS config directory (`~/.config/flowsk/local_paths.json`), without ever
//     polluting the git repository or transmitting host paths to the Flowsk server.
package project

import (
	"time"

	"github.com/google/uuid"
	flowsk_project_pb "github.com/mindmain/flowsk/gen/go/flowsk/project/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Status represents the lifecycle state of a Project within a Workspace.
type Status string

const (
	// StatusActive indicates that objectives and tasks can be actively created and worked on.
	StatusActive Status = "active"

	// StatusPaused indicates that automated agent polling should skip unclaimed work in this project.
	StatusPaused Status = "paused"

	// StatusArchived indicates that the project is read-only for historical reference.
	StatusArchived Status = "archived"
)

// Valid reports whether s is a supported project Status value.
func (s Status) Valid() bool {
	switch s {
	case StatusActive, StatusPaused, StatusArchived:
		return true
	default:
		return false
	}
}

// Project represents a logical product, repository, or engineering stream scoped
// to a single Workspace.
//
// Location-Independence Invariant:
//   - Contains canonical identifiers (ID, WorkspaceID, Slug, KeyPrefix) and optional
//     canonical remote VCS metadata (RemoteRepoURL, DefaultBranch), NEVER local disk paths.
type Project struct {
	// ID is the immutable primary UUID of the project.
	ID uuid.UUID

	// WorkspaceID is the mandatory owning tenant workspace UUID.
	WorkspaceID uuid.UUID

	// Slug is the workspace-unique URL/CLI identifier (for example, "billing-api").
	Slug string

	// KeyPrefix is the short uppercase prefix used to mint human-friendly task keys
	// (for example, "BILL" -> "BILL-42"). Unique within WorkspaceID.
	KeyPrefix string

	// Name is the human-readable project title.
	Name string

	// Description provides concise architectural context for humans and AI agents
	// operating within this project.
	Description string

	// RemoteRepoURL is an optional canonical VCS origin identifier (such as
	// "https://github.com/org/repo"), NEVER a local `file://` or host filesystem path.
	RemoteRepoURL string

	// DefaultBranch is the optional primary VCS branch name (for example, "main").
	DefaultBranch string

	// Status reflects whether the project is active, paused, or archived.
	Status Status

	// Tags holds optional categorization labels. Callers MUST use TagsCopy() or Clone()
	// to preserve slice immutability across package boundaries.
	Tags []string

	// TaskSeq is the monotonic per-project sequence counter used when allocating human-readable task keys.
	TaskSeq int64

	// HasMarkdownDoc reports whether an optional extended Markdown document (`<project_id>.md`)
	// is currently indexed for this project in Flowsk's global storage folder.
	HasMarkdownDoc bool

	// CreatedBy is the ActorID that created the project.
	CreatedBy uuid.UUID

	// CreatedAt is the UTC creation timestamp.
	CreatedAt time.Time

	// UpdatedAt is the UTC timestamp of the last modification.
	UpdatedAt time.Time

	// DeletedAt is non-nil when the project has been soft-deleted.
	DeletedAt *time.Time
}

// TagsCopy returns a defensive copy of the project's Tags slice.
func (p Project) TagsCopy() []string {
	if len(p.Tags) == 0 {
		return nil
	}
	out := make([]string, len(p.Tags))
	copy(out, p.Tags)
	return out
}

// Clone returns a deep copy of Project, duplicating slices and optional pointers.
func (p Project) Clone() Project {
	out := p
	out.Tags = p.TagsCopy()
	if p.DeletedAt != nil {
		deleted := *p.DeletedAt
		out.DeletedAt = &deleted
	}
	return out
}

// CreateProjectInput carries caller-provided parameters for creating a Project
// inside the caller's authorized Workspace.
type CreateProjectInput struct {
	// WorkspaceID is the mandatory target workspace UUID; verified against caller identity.
	WorkspaceID uuid.UUID

	// Slug is the workspace-unique lowercase slug.
	Slug string

	// KeyPrefix is the uppercase task key prefix (for example, "FLW").
	KeyPrefix string

	// Name is the display name of the project.
	Name string

	// Description provides high-level guidance for agents and developers.
	Description string

	// RemoteRepoURL is an optional canonical remote VCS URL (must reject local filesystem paths).
	RemoteRepoURL string

	// DefaultBranch is an optional default VCS branch name.
	DefaultBranch string

	// Tags lists optional classification tags (defensively copied by service).
	Tags []string
}

// UpdateProjectInput carries mutable attributes for updating an existing Project.
type UpdateProjectInput struct {
	// WorkspaceID is the mandatory owning workspace UUID.
	WorkspaceID uuid.UUID

	// ProjectID identifies the project to mutate.
	ProjectID uuid.UUID

	// Name updates the display title when non-nil.
	Name *string

	// Description updates the architectural summary when non-nil.
	Description *string

	// RemoteRepoURL updates the canonical remote VCS URL when non-nil (must reject local paths).
	RemoteRepoURL *string

	// DefaultBranch updates the primary VCS branch when non-nil.
	DefaultBranch *string

	// Status transitions project status (active, paused, archived) when non-nil.
	Status *Status

	// Tags replaces the project tag list when non-nil (defensively copied).
	Tags []string
}

// ListProjectsFilter constrains workspace-scoped project listing queries.
type ListProjectsFilter struct {
	// WorkspaceID is the mandatory tenant scope.
	WorkspaceID uuid.UUID

	// Status filters projects by lifecycle state when non-nil.
	Status *Status

	// Tag filters projects containing a specific tag when non-empty.
	Tag string

	// Limit bounds the maximum number of rows returned (service applies safe cap if <= 0).
	Limit int32

	// Offset specifies pagination offset (>= 0).
	Offset int32
}

// ProjectBinding represents a non-destructive directed parent->child link in
// the workspace multi-binding project DAG. Removing a binding or deleting a
// parent project never deletes the child project or its tasks.
type ProjectBinding struct {
	// ID is the immutable UUID of the binding edge.
	ID uuid.UUID

	// WorkspaceID is the mandatory owning workspace UUID.
	WorkspaceID uuid.UUID

	// ParentProjectID identifies the macro/umbrella parent project.
	ParentProjectID uuid.UUID

	// ChildProjectID identifies the nested sub-project.
	ChildProjectID uuid.UUID

	// ScopeSubpath is an optional normalized relative subpath inside the parent
	// repository (e.g., "apps/web" or "services/auth"), NEVER a host path.
	ScopeSubpath string

	// CreatedBy identifies the actor who established the binding.
	CreatedBy uuid.UUID

	// CreatedAt is the UTC creation timestamp.
	CreatedAt time.Time

	// UpdatedAt is the UTC last-modified timestamp.
	UpdatedAt time.Time
}

// ToProto converts ProjectBinding into its wire-safe Protobuf representation.
func (b ProjectBinding) ToProto() *flowsk_project_pb.ProjectBinding {
	createdBy := ""
	if b.CreatedBy != uuid.Nil {
		createdBy = b.CreatedBy.String()
	}
	return &flowsk_project_pb.ProjectBinding{
		Id:               b.ID.String(),
		WorkspaceId:      b.WorkspaceID.String(),
		ParentProjectId:  b.ParentProjectID.String(),
		ChildProjectId:   b.ChildProjectID.String(),
		ScopeSubpath:     b.ScopeSubpath,
		CreatedByActorId: createdBy,
		CreatedAt:        timestamppb.New(b.CreatedAt.UTC()),
		UpdatedAt:        timestamppb.New(b.UpdatedAt.UTC()),
	}
}

// BindSubprojectInput carries parameters for linking a child sub-project under
// a macro/parent project inside the multi-binding project DAG.
type BindSubprojectInput struct {
	// WorkspaceID is the mandatory owning workspace UUID.
	WorkspaceID uuid.UUID

	// ParentProjectID identifies the macro/parent project.
	ParentProjectID uuid.UUID

	// ChildProjectID identifies the child sub-project.
	ChildProjectID uuid.UUID

	// ScopeSubpath is an optional relative module subpath (e.g., "apps/web");
	// absolute or traversal paths are rejected.
	ScopeSubpath string
}

// UnbindSubprojectInput carries parameters for detaching a child sub-project
// from a parent project without deleting either project.
type UnbindSubprojectInput struct {
	// WorkspaceID is the mandatory owning workspace UUID.
	WorkspaceID uuid.UUID

	// ParentProjectID identifies the macro/parent project.
	ParentProjectID uuid.UUID

	// ChildProjectID identifies the child sub-project.
	ChildProjectID uuid.UUID
}

// ListBindingsFilter constrains workspace-scoped project binding queries.
type ListBindingsFilter struct {
	// WorkspaceID is the mandatory tenant scope.
	WorkspaceID uuid.UUID

	// ParentProjectID filters bindings to children of this parent when non-nil.
	ParentProjectID *uuid.UUID

	// ChildProjectID filters bindings to parents of this child when non-nil.
	ChildProjectID *uuid.UUID
}
