// Package cli implements the Cobra + Viper command-line tree powering the
// unified flowsk binary for both human developers and autonomous AI agents.
//
// Architectural Invariants:
//  1. Segregated Dual-Mode Client Contracts: Every command resolves fine-grained
//     client interfaces in its constructor so cmd/flowsk/main.go can wire either
//     Local Embedded SQLite services or Remote ConnectRPC clients transparently
//     without importing database drivers into internal/cli.
//  2. First-Class Terminal UX & Token-Frugal Output: Interactive terminals
//     receive aligned tables, Kanban columns, and goal progress bars (--format human,
//     auto-selected on TTYs), while autonomous AI agents receive single-line
//     symbolic encodings (--format compact, tsv, jsonl) bounded by --max-tokens.
//  3. Strict Client-Only Local Path Isolation: Only client-side CLI commands
//     (specifically project link, project unlink, project where, and implicit
//     working-directory project resolution) interact with LocalPathRegistry
//     (~/.flowsk/local_paths.json). Flowsk NEVER writes metadata files
//     inside a user's Git repository and NEVER transmits host filesystem paths
//     to any server.
package cli

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/mindmain/flowsk/internal/transport/mcp"
)

// ErrNotImplemented is the canonical sentinel error returned when an optional
// or unconfigured CLI dependency port has not been wired.
var ErrNotImplemented = errors.New("not implemented: interface skeleton only")

// ServeConfig captures validated daemon parameters for `flowsk serve`.
type ServeConfig struct {
	// ListenAddr is the TCP listen address (for example, "127.0.0.1:8080").
	ListenAddr string `json:"listen_addr"`

	// DBDriver selects the persistence engine ("sqlite" or "postgres").
	DBDriver string `json:"db_driver"`

	// DBDSN holds the SQLite file path or PostgreSQL connection URI.
	DBDSN string `json:"db_dsn"`

	// VolumeDir specifies the shared directory used for advisory locks and snapshots.
	VolumeDir string `json:"volume_dir"`

	// AutoMigrate controls whether pending Goose migrations execute on startup.
	AutoMigrate bool `json:"auto_migrate"`

	// EnableUI mounts the embedded Angular Web UI SPA and `/api/ui-config` bootstrap route.
	EnableUI bool `json:"enable_ui,omitempty"`

	// UIWorkspace seeds the initial default workspace slug/UUID for the Web UI bootstrap endpoint.
	UIWorkspace string `json:"ui_workspace,omitempty"`

	// UIProject seeds the initial focused project key/slug/UUID for the Web UI bootstrap endpoint.
	UIProject string `json:"ui_project,omitempty"`

	// UIActor seeds the default operator handle for the Web UI bootstrap endpoint.
	UIActor string `json:"ui_actor,omitempty"`

	// OnListen is an optional callback invoked with the bound TCP `host:port` once listening starts.
	OnListen func(boundAddr string) `json:"-"`
}

// LocalProjectBinding represents a strictly client-local mapping stored in
// ~/.flowsk/local_paths.json associating a host directory on one
// developer's workstation (e.g., Mario's ~/Desktop/app vs Paolo's
// ~/Downloads/app) with a remote logical (WorkspaceID, ProjectID) pair.
type LocalProjectBinding struct {
	// LocalDir is the cleaned absolute directory path on THIS client machine only.
	LocalDir string `json:"local_dir"`

	// WorkspaceID identifies the bound workspace slug or UUID.
	WorkspaceID string `json:"workspace_id"`

	// ProjectID identifies the bound logical project key, slug, or UUID.
	ProjectID string `json:"project_id"`

	// ProjectKey holds an optional uppercase task-key prefix (for example, "CORE").
	ProjectKey string `json:"project_key,omitempty"`

	// GitCommonDir caches the shared local `.git` common directory when LocalDir
	// belongs to a Git checkout or worktree on this machine.
	GitCommonDir string `json:"git_common_dir,omitempty"`

	// GitRelPath caches LocalDir relative to its enclosing Git worktree root
	// (empty for root checkouts; e.g., "apps/web" for sub-projects).
	GitRelPath string `json:"git_rel_path,omitempty"`

	// LinkedAt records the UTC timestamp when the local folder binding was saved.
	LinkedAt time.Time `json:"linked_at"`
}

// UnlinkResultView summarizes local directory bindings removed on the client.
type UnlinkResultView struct {
	// WorkspaceID identifies the workspace filter when --all was used.
	WorkspaceID string `json:"workspace_id,omitempty"`

	// ProjectID identifies the project filter when --all was used.
	ProjectID string `json:"project_id,omitempty"`

	// RemovedDirs lists client-local directories removed from local_paths.json.
	RemovedDirs []string `json:"removed_dirs"`

	// Count is the number of local bindings removed.
	Count int `json:"count"`
}

// CreateWorkspaceInput carries parameters for `flowsk workspace create`.
type CreateWorkspaceInput struct {
	// Slug is the unique URL/CLI-safe workspace identifier.
	Slug string `json:"slug"`

	// Name is the human-readable workspace display name.
	Name string `json:"name"`

	// Description summarizes the purpose of the workspace.
	Description string `json:"description,omitempty"`

	// Actor identifies the human or agent principal creating the workspace.
	Actor string `json:"actor,omitempty"`
}

// ListWorkspacesInput carries pagination parameters for `flowsk workspace list`.
type ListWorkspacesInput struct {
	// Limit bounds the maximum number of workspaces returned.
	Limit int `json:"limit"`

	// Cursor holds an opaque pagination cursor.
	Cursor string `json:"cursor,omitempty"`
}

// WorkspaceView represents a tenant workspace rendered by the CLI.
type WorkspaceView struct {
	// ID is the immutable workspace UUID or canonical identifier.
	ID string `json:"id"`

	// Slug is the unique workspace slug.
	Slug string `json:"slug"`

	// Name is the human-readable display title.
	Name string `json:"name"`

	// Description summarizes the workspace scope.
	Description string `json:"description,omitempty"`

	// DefaultLeaseTTLSeconds configures the default task claim lease duration in seconds.
	DefaultLeaseTTLSeconds int64 `json:"default_lease_ttl_seconds,omitempty"`

	// CreatedAt is the UTC creation timestamp.
	CreatedAt time.Time `json:"created_at"`

	// UpdatedAt is the UTC last-modified timestamp.
	UpdatedAt time.Time `json:"updated_at"`
}

// WorkspaceListResult wraps paginated workspaces for `flowsk workspace list`.
type WorkspaceListResult struct {
	// Items contains the returned workspace projections.
	Items []WorkspaceView `json:"items"`

	// NextCursor holds the continuation token when more rows remain.
	NextCursor string `json:"next_cursor,omitempty"`
}

// WorkspaceUseView reports a persisted default workspace update in config.yaml.
type WorkspaceUseView struct {
	// Workspace is the newly persisted default workspace slug or UUID.
	Workspace string `json:"workspace"`

	// ConfigFile is the user-scoped config.yaml file path updated on disk.
	ConfigFile string `json:"config_file"`
}

// RegisterActorInput carries parameters for `flowsk workspace actor register`.
type RegisterActorInput struct {
	// Workspace identifies the target tenant workspace UUID or slug.
	Workspace string `json:"workspace"`

	// Handle is the workspace-unique actor handle (e.g., "mario", "claude-arch").
	Handle string `json:"handle"`

	// DisplayName is the optional human-friendly actor label.
	DisplayName string `json:"display_name,omitempty"`

	// Kind classifies the actor ("human", "agent", or "ci").
	Kind string `json:"kind"`

	// Model records the underlying LLM identifier when Kind is "agent".
	Model string `json:"model,omitempty"`

	// Role records an operational role or RBAC hint.
	Role string `json:"role,omitempty"`
}

// ListActorsInput carries filtering and pagination options for `flowsk workspace actor list`.
type ListActorsInput struct {
	// Workspace identifies the target workspace UUID or slug.
	Workspace string `json:"workspace"`

	// Kind optionally filters actors by category ("human", "agent", "ci").
	Kind string `json:"kind,omitempty"`

	// Limit caps the maximum number of actors returned.
	Limit int `json:"limit"`

	// Cursor holds an opaque pagination token.
	Cursor string `json:"cursor,omitempty"`
}

// ActorView represents a registered human, AI agent, or CI actor within a workspace.
type ActorView struct {
	// ID is the immutable actor UUID or identifier.
	ID string `json:"id"`

	// WorkspaceID identifies the owning workspace.
	WorkspaceID string `json:"workspace_id"`

	// Handle is the unique actor handle.
	Handle string `json:"handle"`

	// DisplayName is the display name of the actor.
	DisplayName string `json:"display_name,omitempty"`

	// Kind classifies the actor ("human", "agent", "ci").
	Kind string `json:"kind"`

	// Model identifies the backing LLM model for agent actors.
	Model string `json:"model,omitempty"`

	// Role describes the actor's role in the workspace.
	Role string `json:"role,omitempty"`

	// CreatedAt records when the actor was registered in UTC.
	CreatedAt time.Time `json:"created_at"`
}

// ActorListResult wraps paginated actors for `flowsk workspace actor list`.
type ActorListResult struct {
	// Items holds the matching workspace actors.
	Items []ActorView `json:"items"`

	// NextCursor holds an optional continuation cursor.
	NextCursor string `json:"next_cursor,omitempty"`
}

// CreateProjectInput carries parameters for `flowsk project create`.
// Notice that host filesystem paths are strictly excluded.
type CreateProjectInput struct {
	// Workspace identifies the owning workspace UUID or slug.
	Workspace string `json:"workspace"`

	// Key is the short logical project key prefix (for example, "CORE").
	Key string `json:"key"`

	// Name is the human-readable project display title.
	Name string `json:"name"`

	// Description provides concise architectural context for humans and agents.
	Description string `json:"description,omitempty"`

	// DefaultWorkflow seeds canonical Linear-style workflow states when true.
	DefaultWorkflow bool `json:"default_workflow"`

	// Actor identifies the principal creating the project.
	Actor string `json:"actor,omitempty"`
}

// ListProjectsInput carries filter parameters for `flowsk project list`.
type ListProjectsInput struct {
	// Workspace identifies the owning workspace UUID or slug.
	Workspace string `json:"workspace"`

	// IncludeArchived includes soft-archived projects when true.
	IncludeArchived bool `json:"include_archived"`

	// Limit bounds returned project rows.
	Limit int `json:"limit"`

	// Cursor holds an opaque pagination cursor.
	Cursor string `json:"cursor,omitempty"`
}

// UpdateProjectInput carries partial updates for `flowsk project update`.
type UpdateProjectInput struct {
	// Workspace identifies the owning workspace UUID or slug.
	Workspace string `json:"workspace"`

	// Project identifies the target project UUID, slug, or key.
	Project string `json:"project"`

	// Name updates the project display title when non-nil.
	Name *string `json:"name,omitempty"`

	// Description updates the project summary when non-nil.
	Description *string `json:"description,omitempty"`

	// Actor identifies the principal updating the project.
	Actor string `json:"actor,omitempty"`
}

// ArchiveProjectInput carries parameters for `flowsk project archive`.
type ArchiveProjectInput struct {
	// Workspace identifies the owning workspace UUID or slug.
	Workspace string `json:"workspace"`

	// Project identifies the target project UUID, slug, or key.
	Project string `json:"project"`

	// Reason records an optional audit explanation for archiving the project.
	Reason string `json:"reason,omitempty"`

	// Actor identifies the principal archiving the project.
	Actor string `json:"actor,omitempty"`
}

// ProjectView represents a logical Flowsk project with zero host path fields.
type ProjectView struct {
	// ID is the immutable project UUID or identifier.
	ID string `json:"id"`

	// WorkspaceID identifies the owning workspace.
	WorkspaceID string `json:"workspace_id"`

	// Slug is the workspace-unique project slug.
	Slug string `json:"slug,omitempty"`

	// Key is the uppercase task key prefix (for example, "CORE").
	Key string `json:"key"`

	// Name is the human-readable project title.
	Name string `json:"name"`

	// Description summarizes the project's purpose.
	Description string `json:"description,omitempty"`

	// Status reflects the lifecycle status ("active", "paused", "archived").
	Status string `json:"status"`

	// HasMarkdownDoc reports whether an extended Markdown document exists for this project.
	HasMarkdownDoc bool `json:"has_markdown_doc,omitempty"`

	// CreatedAt is the UTC creation timestamp.
	CreatedAt time.Time `json:"created_at"`

	// UpdatedAt is the UTC last-modified timestamp.
	UpdatedAt time.Time `json:"updated_at"`
}

// ProjectListResult wraps paginated logical projects for `flowsk project list`.
type ProjectListResult struct {
	// Items contains the returned logical projects.
	Items []ProjectView `json:"items"`

	// NextCursor holds an optional pagination cursor.
	NextCursor string `json:"next_cursor,omitempty"`
}

// CreateStateInput carries parameters for `flowsk project state create`.
type CreateStateInput struct {
	// Workspace identifies the owning workspace UUID or slug.
	Workspace string `json:"workspace"`

	// Project identifies the owning project UUID, slug, or key.
	Project string `json:"project"`

	// Name is the display label of the workflow state (e.g., "QA Verification").
	Name string `json:"name"`

	// Category maps the state to a canonical lifecycle bucket.
	Category string `json:"category"`

	// Position controls column sort ordering within the project pipeline.
	Position int `json:"position"`

	// Color holds an optional hex color badge (e.g., "#0EA5E9").
	Color string `json:"color,omitempty"`

	// Actor identifies the principal creating the workflow state.
	Actor string `json:"actor,omitempty"`
}

// WorkflowStateView represents a configured workflow column inside a project.
type WorkflowStateView struct {
	// ID is the workflow state identifier.
	ID string `json:"id"`

	// ProjectID identifies the owning project.
	ProjectID string `json:"project_id"`

	// Name is the state display name.
	Name string `json:"name"`

	// Category is the canonical workflow category (backlog, todo, in_progress, review, blocked, done, canceled).
	Category string `json:"category"`

	// Position determines column ordering on boards.
	Position int `json:"position"`

	// Color holds an optional hex color code.
	Color string `json:"color,omitempty"`
}

// CreateLabelInput carries parameters for `flowsk project label create`.
type CreateLabelInput struct {
	// Workspace identifies the owning workspace UUID or slug.
	Workspace string `json:"workspace"`

	// Project identifies the owning project UUID, slug, or key.
	Project string `json:"project"`

	// Name is the label token (for example, "storage" or "security").
	Name string `json:"name"`

	// Color holds an optional hex badge color.
	Color string `json:"color,omitempty"`

	// Description explains when humans and agents should apply this label.
	Description string `json:"description,omitempty"`

	// Actor identifies the principal creating the label.
	Actor string `json:"actor,omitempty"`
}

// LabelView represents a project classification label.
type LabelView struct {
	// ID is the label identifier.
	ID string `json:"id"`

	// ProjectID identifies the owning project.
	ProjectID string `json:"project_id"`

	// Name is the unique label name within the project.
	Name string `json:"name"`

	// Color holds an optional hex color string.
	Color string `json:"color,omitempty"`

	// Description summarizes label usage.
	Description string `json:"description,omitempty"`
}

// CreateObjectiveInput carries parameters for `flowsk objective create` (`goal create`).
type CreateObjectiveInput struct {
	// Workspace identifies the owning workspace UUID or slug.
	Workspace string `json:"workspace"`

	// Project identifies the owning project UUID, slug, or key.
	Project string `json:"project"`

	// Title is the concise goal, milestone, or cycle headline.
	Title string `json:"title"`

	// Kind classifies the objective ("goal", "milestone", or "cycle").
	Kind string `json:"kind"`

	// Summary describes measurable outcome criteria.
	Summary string `json:"summary,omitempty"`

	// Priority ranks objective urgency (0..4).
	Priority int `json:"priority"`

	// TargetDate holds an optional YYYY-MM-DD deadline string.
	TargetDate string `json:"target_date,omitempty"`

	// Owner identifies the responsible actor handle.
	Owner string `json:"owner,omitempty"`

	// Actor identifies the principal creating the objective.
	Actor string `json:"actor,omitempty"`
}

// ListObjectivesInput carries filter parameters for `flowsk objective list`.
type ListObjectivesInput struct {
	// Workspace identifies the owning workspace UUID or slug.
	Workspace string `json:"workspace"`

	// Project identifies the owning project UUID, slug, or key.
	Project string `json:"project"`

	// Kind filters by "goal", "milestone", or "cycle" when non-empty.
	Kind string `json:"kind,omitempty"`

	// Status filters by lifecycle status when non-empty.
	Status string `json:"status,omitempty"`

	// Owner filters by responsible actor handle when non-empty.
	Owner string `json:"owner,omitempty"`

	// Limit bounds the maximum objectives returned.
	Limit int `json:"limit"`

	// Cursor holds an opaque pagination cursor.
	Cursor string `json:"cursor,omitempty"`
}

// UpdateObjectiveInput carries partial updates for `flowsk objective update`.
type UpdateObjectiveInput struct {
	// Workspace identifies the owning workspace UUID or slug.
	Workspace string `json:"workspace"`

	// Project optionally scopes lookup when Objective is a project-relative key.
	Project string `json:"project,omitempty"`

	// Objective identifies the target objective UUID, key, or slug.
	Objective string `json:"objective"`

	// Title updates the objective title when non-nil.
	Title *string `json:"title,omitempty"`

	// Summary updates the outcome summary when non-nil.
	Summary *string `json:"summary,omitempty"`

	// Status updates the lifecycle status when non-nil.
	Status *string `json:"status,omitempty"`

	// Priority updates the priority rank (0..4) when non-nil.
	Priority *int `json:"priority,omitempty"`

	// TargetDate updates the YYYY-MM-DD deadline when non-nil.
	TargetDate *string `json:"target_date,omitempty"`

	// Owner updates the responsible actor handle when non-nil.
	Owner *string `json:"owner,omitempty"`

	// Actor identifies the principal performing the update.
	Actor string `json:"actor,omitempty"`
}

// CloseObjectiveInput carries parameters for `flowsk objective close`.
type CloseObjectiveInput struct {
	// Workspace identifies the owning workspace UUID or slug.
	Workspace string `json:"workspace"`

	// Project optionally scopes lookup when Objective is a project-relative key.
	Project string `json:"project,omitempty"`

	// Objective identifies the target objective UUID, key, or slug.
	Objective string `json:"objective"`

	// Resolution specifies the closure outcome ("completed", "canceled", "superseded").
	Resolution string `json:"resolution"`

	// Note records an optional closure summary in the audit trail.
	Note string `json:"note,omitempty"`

	// Actor identifies the principal closing the objective.
	Actor string `json:"actor,omitempty"`
}

// ObjectiveView represents a strategic goal, milestone, or cycle along with live
// task completion counts so terminal views render accurate progress bars.
type ObjectiveView struct {
	// ID is the immutable objective UUID or identifier.
	ID string `json:"id"`

	// WorkspaceID identifies the owning workspace.
	WorkspaceID string `json:"workspace_id"`

	// ProjectID identifies the owning project.
	ProjectID string `json:"project_id"`

	// Key is the short human/LLM reference slug or key (e.g., "CORE-O1" or "q4-storage").
	Key string `json:"key"`

	// Kind classifies the objective ("goal", "milestone", "cycle").
	Kind string `json:"kind"`

	// Title is the concise objective statement.
	Title string `json:"title"`

	// Summary explains the outcome criteria.
	Summary string `json:"summary,omitempty"`

	// Status tracks current lifecycle state (planned, active, at_risk, completed, achieved, canceled, dropped).
	Status string `json:"status"`

	// Priority indicates relative urgency (0..4).
	Priority int `json:"priority"`

	// TargetDate holds an optional YYYY-MM-DD target completion date.
	TargetDate string `json:"target_date,omitempty"`

	// Owner identifies the responsible actor handle.
	Owner string `json:"owner,omitempty"`

	// Resolution records closure resolution when closed.
	Resolution string `json:"resolution,omitempty"`

	// CompletedTasks is the count of linked tasks in a completed state.
	CompletedTasks int `json:"completed_tasks"`

	// TotalTasks is the total count of non-deleted tasks linked to this objective.
	TotalTasks int `json:"total_tasks"`

	// BlockedTasks is the count of linked tasks currently blocked by unfinished dependencies.
	BlockedTasks int `json:"blocked_tasks,omitempty"`

	// CreatedAt is the UTC creation timestamp.
	CreatedAt time.Time `json:"created_at"`

	// UpdatedAt is the UTC last-modified timestamp.
	UpdatedAt time.Time `json:"updated_at"`
}

// ObjectiveListResult wraps paginated objectives for `flowsk objective list`.
type ObjectiveListResult struct {
	// Items holds the returned objectives with live progress counters.
	Items []ObjectiveView `json:"items"`

	// NextCursor holds an optional pagination cursor.
	NextCursor string `json:"next_cursor,omitempty"`
}

// CreateTaskInput carries parameters for `flowsk task create`.
type CreateTaskInput struct {
	// Workspace identifies the owning workspace UUID or slug.
	Workspace string `json:"workspace"`

	// Project identifies the owning project UUID, slug, or key.
	Project string `json:"project"`

	// Title is the concise imperative task title.
	Title string `json:"title"`

	// Kind optionally classifies the task ("epic", "feature", "bug", "chore", "research").
	Kind string `json:"kind,omitempty"`

	// ExternalID optionally attaches a project-scoped external tracker ID (e.g., "CU-869abc").
	ExternalID string `json:"external_id,omitempty"`

	// Description holds Markdown technical specification details.
	Description string `json:"description,omitempty"`

	// Objective links the task to a parent objective UUID, slug, or key.
	Objective string `json:"objective,omitempty"`

	// Parent links the task under a parent epic/task UUID or key.
	Parent string `json:"parent,omitempty"`

	// State specifies the initial workflow state name or category.
	State string `json:"state,omitempty"`

	// Priority sets urgency rank (0..4).
	Priority int `json:"priority"`

	// Assignee pre-assigns an actor handle.
	Assignee string `json:"assignee,omitempty"`

	// Labels attaches classification labels.
	Labels []string `json:"labels,omitempty"`

	// Estimate records optional complexity points.
	Estimate int `json:"estimate,omitempty"`

	// DependsOn lists upstream prerequisite task keys or UUIDs.
	DependsOn []string `json:"depends_on,omitempty"`

	// Acceptance defines deterministic verification criteria.
	Acceptance string `json:"acceptance,omitempty"`

	// Actor identifies the principal creating the task.
	Actor string `json:"actor,omitempty"`
}

// ListTasksInput carries filter parameters for `flowsk task list`.
type ListTasksInput struct {
	// Workspace identifies the owning workspace UUID or slug.
	Workspace string `json:"workspace"`

	// Project identifies the owning project UUID, slug, or key.
	Project string `json:"project"`

	// Objective filters tasks belonging to an objective UUID, slug, or key.
	Objective string `json:"objective,omitempty"`

	// Parent filters direct subtasks of a parent task UUID or key.
	Parent string `json:"parent,omitempty"`

	// ExternalID filters tasks by exact project-scoped external tracker ID.
	ExternalID string `json:"external_id,omitempty"`

	// State filters by workflow state name or canonical category.
	State string `json:"state,omitempty"`

	// Priority filters by exact priority rank when >= 0 (-1 means unfiltered).
	Priority int `json:"priority"`

	// Assignee filters by assigned or claiming actor handle.
	Assignee string `json:"assignee,omitempty"`

	// Labels requires all specified labels.
	Labels []string `json:"labels,omitempty"`

	// Unclaimed restricts results to tasks without an active actor lease.
	Unclaimed bool `json:"unclaimed,omitempty"`

	// Blocked restricts results to tasks with unresolved blockers.
	Blocked bool `json:"blocked,omitempty"`

	// IncludeArchived includes archived tasks when no explicit state filter is set.
	IncludeArchived bool `json:"include_archived,omitempty"`

	// Query applies a GitHub/Linear-style structured & free-text search expression
	// (e.g., `state:done #storage "sqlite wal"`).
	Query string `json:"query,omitempty"`

	// Limit caps maximum tasks returned.
	Limit int `json:"limit"`

	// Cursor holds an opaque pagination cursor.
	Cursor string `json:"cursor,omitempty"`
}

// SDKMethodView summarizes a JavaScript method on the Code Mode `flowsk` SDK.
type SDKMethodView struct {
	// Name is the qualified JS method name (e.g., "flowsk.tasks.claim").
	Name string `json:"name"`

	// Category groups related SDK methods ("tasks", "goals", "projects", "search").
	Category string `json:"category"`

	// Signature gives the concise TypeScript signature.
	Signature string `json:"signature"`

	// Summary describes the method behavior.
	Summary string `json:"summary"`

	// Example gives a runnable one-line JS snippet.
	Example string `json:"example"`
}

// SearchView aggregates cross-entity `flowsk search` results for tasks, goals,
// and Code Mode JS SDK discovery.
type SearchView struct {
	// Query echoes the effective search query expression.
	Query string `json:"query"`

	// Scope identifies the searched domain ("all", "tasks", "goals", "sdk").
	Scope string `json:"scope"`

	// Tasks contains matching task summaries.
	Tasks []TaskSummaryView `json:"tasks,omitempty"`

	// Goals contains matching strategic goals/objectives.
	Goals []ObjectiveView `json:"goals,omitempty"`

	// SDKMethods contains matching Code Mode JS SDK methods when Scope == "sdk".
	SDKMethods []SDKMethodView `json:"sdk_methods,omitempty"`
}

// GetTaskInput carries lookup options for `flowsk task get`.
type GetTaskInput struct {
	// Workspace identifies the owning workspace UUID or slug.
	Workspace string `json:"workspace"`

	// Project optionally scopes lookup when Task is a project-scoped reference.
	Project string `json:"project,omitempty"`

	// Task identifies the target task UUID or key (e.g., "CORE-42").
	Task string `json:"task"`

	// IncludeComments includes recent structured handoff/progress comments.
	IncludeComments bool `json:"include_comments"`

	// IncludeSubtasks includes child subtask summaries.
	IncludeSubtasks bool `json:"include_subtasks"`

	// IncludeDeps includes upstream/downstream dependency keys.
	IncludeDeps bool `json:"include_deps"`
}

// UpdateTaskInput carries partial modifications for `flowsk task update`.
type UpdateTaskInput struct {
	// Workspace identifies the owning workspace UUID or slug.
	Workspace string `json:"workspace"`

	// Project optionally scopes task lookup.
	Project string `json:"project,omitempty"`

	// Task identifies the target task UUID or key.
	Task string `json:"task"`

	// Title updates the task title when non-nil.
	Title *string `json:"title,omitempty"`

	// ExternalID updates (or clears when empty string) the project-scoped external tracker ID when non-nil.
	ExternalID *string `json:"external_id,omitempty"`

	// Description updates the technical specification when non-nil.
	Description *string `json:"description,omitempty"`

	// Objective updates the linked objective UUID/key when non-nil.
	Objective *string `json:"objective,omitempty"`

	// Parent updates the parent task UUID/key when non-nil.
	Parent *string `json:"parent,omitempty"`

	// Priority updates urgency rank (0..4) when non-nil.
	Priority *int `json:"priority,omitempty"`

	// Assignee updates the assigned actor handle when non-nil.
	Assignee *string `json:"assignee,omitempty"`

	// AddLabels lists labels to attach.
	AddLabels []string `json:"add_labels,omitempty"`

	// RemoveLabels lists labels to detach.
	RemoveLabels []string `json:"remove_labels,omitempty"`

	// Estimate updates complexity points when non-nil.
	Estimate *int `json:"estimate,omitempty"`

	// Acceptance updates verification criteria when non-nil.
	Acceptance *string `json:"acceptance,omitempty"`

	// Actor identifies the principal updating the task.
	Actor string `json:"actor,omitempty"`
}

// MoveTaskInput carries state transition parameters for `flowsk task move`.
type MoveTaskInput struct {
	// Workspace identifies the owning workspace UUID or slug.
	Workspace string `json:"workspace"`

	// Project optionally scopes task lookup.
	Project string `json:"project,omitempty"`

	// Task identifies the target task UUID or key.
	Task string `json:"task"`

	// State is the target workflow state name or category.
	State string `json:"state"`

	// Note records an optional transition note in the audit/comment log.
	Note string `json:"note,omitempty"`

	// Actor identifies the principal moving the task.
	Actor string `json:"actor,omitempty"`
}

// ClaimTaskInput carries lease acquisition parameters for `flowsk task claim`.
type ClaimTaskInput struct {
	// Workspace identifies the owning workspace UUID or slug.
	Workspace string `json:"workspace"`

	// Project optionally scopes task lookup.
	Project string `json:"project,omitempty"`

	// Task identifies the target task UUID or key.
	Task string `json:"task"`

	// Actor identifies the claiming human or agent handle.
	Actor string `json:"actor"`

	// LeaseTTL specifies the exclusive lease duration before expiry.
	LeaseTTL time.Duration `json:"lease_ttl"`

	// Note records an optional execution intent note.
	Note string `json:"note,omitempty"`
}

// ReleaseTaskInput carries lease release parameters for `flowsk task release`.
type ReleaseTaskInput struct {
	// Workspace identifies the owning workspace UUID or slug.
	Workspace string `json:"workspace"`

	// Project optionally scopes task lookup.
	Project string `json:"project,omitempty"`

	// Task identifies the target task UUID or key.
	Task string `json:"task"`

	// Actor identifies the actor releasing the lease.
	Actor string `json:"actor,omitempty"`

	// TargetState optionally transitions the task upon release (e.g., "Review" or "Done").
	TargetState string `json:"target_state,omitempty"`

	// Note records an optional handoff summary for the next worker.
	Note string `json:"note,omitempty"`
}

// BlockTaskInput carries blocker edge and impediment details for `flowsk task block`.
type BlockTaskInput struct {
	// Workspace identifies the owning workspace UUID or slug.
	Workspace string `json:"workspace"`

	// Project optionally scopes task lookup.
	Project string `json:"project,omitempty"`

	// Task identifies the blocked task UUID or key.
	Task string `json:"task"`

	// BlockedBy lists upstream task keys or UUIDs that must finish first.
	BlockedBy []string `json:"blocked_by,omitempty"`

	// Reason explains the blocking dependency or external impediment.
	Reason string `json:"reason,omitempty"`

	// Actor identifies the principal recording the blocker.
	Actor string `json:"actor,omitempty"`
}

// UnblockTaskInput carries blocker removal parameters for `flowsk task unblock`.
type UnblockTaskInput struct {
	// Workspace identifies the owning workspace UUID or slug.
	Workspace string `json:"workspace"`

	// Project optionally scopes task lookup.
	Project string `json:"project,omitempty"`

	// Task identifies the task UUID or key to unblock.
	Task string `json:"task"`

	// ClearedBy lists specific upstream blocker keys to detach (empty clears impediment state).
	ClearedBy []string `json:"cleared_by,omitempty"`

	// Note records an optional unblock resolution note.
	Note string `json:"note,omitempty"`

	// Actor identifies the principal clearing the blocker.
	Actor string `json:"actor,omitempty"`
}

// AddCommentInput carries structured timeline comment fields for `flowsk task comment`.
type AddCommentInput struct {
	// Workspace identifies the owning workspace UUID or slug.
	Workspace string `json:"workspace"`

	// Project optionally scopes task lookup.
	Project string `json:"project,omitempty"`

	// Task identifies the target task UUID or key.
	Task string `json:"task"`

	// Actor identifies the authoring human or agent handle.
	Actor string `json:"actor,omitempty"`

	// Kind classifies the comment (note, handoff, decision, verification, progress, blocker).
	Kind string `json:"kind"`

	// Body contains the Markdown comment content.
	Body string `json:"body"`
}

// ListCommentsInput carries pagination fields for `flowsk task comments`.
type ListCommentsInput struct {
	// Workspace identifies the owning workspace UUID or slug.
	Workspace string `json:"workspace"`

	// Project optionally scopes task lookup.
	Project string `json:"project,omitempty"`

	// Task identifies the target task UUID or key.
	Task string `json:"task"`

	// Limit caps returned comments.
	Limit int `json:"limit"`

	// Cursor holds an opaque pagination cursor.
	Cursor string `json:"cursor,omitempty"`
}

// NextTaskInput carries selection and atomic claim options for `flowsk task next`.
type NextTaskInput struct {
	// Workspace identifies the owning workspace UUID or slug.
	Workspace string `json:"workspace"`

	// Project identifies the active logical project UUID, slug, or key.
	Project string `json:"project"`

	// Objective optionally restricts selection to a single objective UUID/key.
	Objective string `json:"objective,omitempty"`

	// Labels requires all listed capability labels.
	Labels []string `json:"labels,omitempty"`

	// Claim atomically acquires a lease on the selected task when true.
	Claim bool `json:"claim"`

	// Actor identifies the claiming actor handle when Claim is true.
	Actor string `json:"actor,omitempty"`

	// LeaseTTL sets the lease duration when Claim is true.
	LeaseTTL time.Duration `json:"lease_ttl,omitempty"`
}

// TaskDigestInput configures `flowsk task digest`.
type TaskDigestInput struct {
	// Workspace identifies the owning workspace UUID or slug.
	Workspace string `json:"workspace"`

	// Project identifies the logical project UUID, slug, or key.
	Project string `json:"project"`

	// Objective optionally scopes the digest to one objective.
	Objective string `json:"objective,omitempty"`

	// IncludeBlocked includes blocked tasks in the digest when true.
	IncludeBlocked bool `json:"include_blocked"`

	// Limit bounds summarized tasks across queues.
	Limit int `json:"limit"`

	// MaxTokens provides an optional token budget hint.
	MaxTokens int `json:"max_tokens,omitempty"`
}

// ClaimLeaseView describes an active exclusive actor lease on a task.
type ClaimLeaseView struct {
	// Holder is the actor handle holding the lease.
	Holder string `json:"holder"`

	// LeaseToken is the opaque fencing token for renewals/releases.
	LeaseToken string `json:"lease_token,omitempty"`

	// AcquiredAt is the UTC timestamp when the lease was acquired.
	AcquiredAt time.Time `json:"acquired_at"`

	// ExpiresAt is the UTC expiration timestamp of the lease.
	ExpiresAt time.Time `json:"expires_at"`
}

// CommentView represents a structured comment on a task timeline.
type CommentView struct {
	// ID is the comment UUID or identifier.
	ID string `json:"id"`

	// TaskID identifies the parent task UUID.
	TaskID string `json:"task_id,omitempty"`

	// TaskKey is the compact parent task key (e.g., "CORE-42").
	TaskKey string `json:"task_key,omitempty"`

	// Author is the handle of the human or agent author.
	Author string `json:"author"`

	// Kind classifies the comment (note, handoff, decision, verification, progress, blocker).
	Kind string `json:"kind"`

	// Body holds the Markdown comment text.
	Body string `json:"body"`

	// CreatedAt is the UTC creation timestamp.
	CreatedAt time.Time `json:"created_at"`
}

// CommentListResult wraps paginated task comments.
type CommentListResult struct {
	// Items holds the returned task comments.
	Items []CommentView `json:"items"`

	// NextCursor holds an optional continuation cursor.
	NextCursor string `json:"next_cursor,omitempty"`
}

// TaskSummaryView is a token-frugal single-line projection of a task used in
// lists, Kanban board columns, and project digests.
type TaskSummaryView struct {
	// ID is the task UUID or identifier.
	ID string `json:"id"`

	// Key is the compact project-scoped task key (e.g., "CORE-42").
	Key string `json:"key"`

	// ExternalID is the optional project-scoped external tracker ID (e.g., "CU-869abc").
	ExternalID string `json:"external_id,omitempty"`

	// Title is the concise task summary.
	Title string `json:"title"`

	// State is the display workflow state name (e.g., "In Progress", "Ready").
	State string `json:"state"`

	// Category is the canonical state category (backlog, todo, ready, in_progress, review, blocked, done, canceled).
	Category string `json:"category,omitempty"`

	// Priority is the numeric urgency rank (0..4).
	Priority int `json:"priority"`

	// ObjectiveKey is the linked objective slug or key, if any.
	ObjectiveKey string `json:"objective_key,omitempty"`

	// ParentKey is the parent task key when this item is a subtask.
	ParentKey string `json:"parent_key,omitempty"`

	// Assignee is the assigned actor handle, if any.
	Assignee string `json:"assignee,omitempty"`

	// Holder is the active lease holder handle, if claimed.
	Holder string `json:"holder,omitempty"`

	// Labels lists attached label names.
	Labels []string `json:"labels,omitempty"`

	// Blocked reports whether the task is currently blocked.
	Blocked bool `json:"blocked,omitempty"`

	// BlockedBy lists unresolved upstream blocker task keys.
	BlockedBy []string `json:"blocked_by,omitempty"`

	// HasMarkdownDoc reports whether an extended Markdown document exists for this task.
	HasMarkdownDoc bool `json:"has_markdown_doc,omitempty"`

	// LastSignalAt is the optional UTC timestamp of the most recent agent/CLI/MCP signal.
	LastSignalAt *time.Time `json:"last_signal_at,omitempty"`

	// UpdatedAt is the UTC timestamp of the last modification.
	UpdatedAt time.Time `json:"updated_at"`
}

// TaskView represents a full task aggregate including optional subtasks,
// dependency edges, lease details, and comments.
type TaskView struct {
	// ID is the immutable task UUID or identifier.
	ID string `json:"id"`

	// WorkspaceID identifies the owning workspace.
	WorkspaceID string `json:"workspace_id"`

	// ProjectID identifies the owning project.
	ProjectID string `json:"project_id"`

	// Key is the compact task key (e.g., "CORE-42").
	Key string `json:"key"`

	// ExternalID is the optional project-scoped external tracker ID (e.g., "CU-869abc").
	ExternalID string `json:"external_id,omitempty"`

	// Title is the imperative task headline.
	Title string `json:"title"`

	// Description holds Markdown technical details.
	Description string `json:"description,omitempty"`

	// State is the current workflow state name.
	State string `json:"state"`

	// Category is the canonical workflow category.
	Category string `json:"category,omitempty"`

	// Priority is the numeric urgency rank (0..4).
	Priority int `json:"priority"`

	// ObjectiveKey identifies the linked objective slug or key, if any.
	ObjectiveKey string `json:"objective_key,omitempty"`

	// ParentKey identifies the parent task key, if any.
	ParentKey string `json:"parent_key,omitempty"`

	// Assignee identifies the assigned actor handle, if any.
	Assignee string `json:"assignee,omitempty"`

	// Labels lists attached classification labels.
	Labels []string `json:"labels,omitempty"`

	// Estimate holds complexity points when > 0.
	Estimate int `json:"estimate,omitempty"`

	// Acceptance holds deterministic verification criteria.
	Acceptance string `json:"acceptance,omitempty"`

	// Blocked reports whether unresolved blockers or impediments exist.
	Blocked bool `json:"blocked,omitempty"`

	// BlockedBy lists upstream blocking task keys.
	BlockedBy []string `json:"blocked_by,omitempty"`

	// Blocks lists downstream task keys blocked by this task.
	Blocks []string `json:"blocks,omitempty"`

	// BlockerReason holds an explicit impediment explanation when blocked.
	BlockerReason string `json:"blocker_reason,omitempty"`

	// Lease holds the active exclusive actor claim, or nil if unclaimed.
	Lease *ClaimLeaseView `json:"lease,omitempty"`

	// Subtasks holds child subtask summaries when requested.
	Subtasks []TaskSummaryView `json:"subtasks,omitempty"`

	// Comments holds recent structured timeline comments when requested.
	Comments []CommentView `json:"comments,omitempty"`

	// HasMarkdownDoc reports whether an extended Markdown document exists for this task.
	HasMarkdownDoc bool `json:"has_markdown_doc,omitempty"`

	// LastSignalAt is the optional UTC timestamp of the most recent agent/CLI/MCP signal.
	LastSignalAt *time.Time `json:"last_signal_at,omitempty"`

	// CreatedAt is the UTC creation timestamp.
	CreatedAt time.Time `json:"created_at"`

	// UpdatedAt is the UTC last-modified timestamp.
	UpdatedAt time.Time `json:"updated_at"`
}

// TaskListResult wraps paginated task summaries for `flowsk task list`.
type TaskListResult struct {
	// Items holds the matching task summaries.
	Items []TaskSummaryView `json:"items"`

	// NextCursor holds an optional continuation cursor.
	NextCursor string `json:"next_cursor,omitempty"`
}

// NextTaskResult wraps the candidate returned by `flowsk task next`.
type NextTaskResult struct {
	// Task is the highest-priority unblocked ready task, or nil when the queue is empty.
	Task *TaskView `json:"task"`

	// Claimed reports whether an exclusive lease was atomically acquired.
	Claimed bool `json:"claimed"`
}

// ProjectDigestView provides an ultra-compact situational snapshot of a project
// for `flowsk task digest`.
type ProjectDigestView struct {
	// WorkspaceID identifies the owning workspace.
	WorkspaceID string `json:"workspace_id"`

	// ProjectID identifies the project UUID or slug.
	ProjectID string `json:"project_id"`

	// ProjectKey is the short uppercase project prefix (e.g., "CORE").
	ProjectKey string `json:"project_key"`

	// ProjectName is the human-readable project name.
	ProjectName string `json:"project_name,omitempty"`

	// GeneratedAt is the UTC snapshot timestamp.
	GeneratedAt time.Time `json:"generated_at"`

	// CountsByState maps workflow state names/categories to task counts.
	CountsByState map[string]int `json:"counts_by_state,omitempty"`

	// Objectives summarizes active goals and milestones with completion counts.
	Objectives []ObjectiveView `json:"objectives,omitempty"`

	// ActiveLeases lists tasks currently claimed by human or AI agent workers.
	ActiveLeases []TaskSummaryView `json:"active_leases,omitempty"`

	// ReadyQueue lists top unblocked, unclaimed tasks ready for work.
	ReadyQueue []TaskSummaryView `json:"ready_queue,omitempty"`

	// BlockedTasks lists tasks awaiting upstream blockers.
	BlockedTasks []TaskSummaryView `json:"blocked_tasks,omitempty"`
}

// BoardColumnView represents one workflow state column on `flowsk board`.
type BoardColumnView struct {
	// State is the column header name.
	State string `json:"state"`

	// Category is the canonical workflow category of the column.
	Category string `json:"category,omitempty"`

	// Tasks holds the visible tasks in this column (capped by --limit-per-state).
	Tasks []TaskSummaryView `json:"tasks"`

	// TotalCount is the total number of matching tasks in this workflow column.
	TotalCount int `json:"total_count"`

	// OmittedCount is the count of additional tasks truncated by --limit-per-state.
	OmittedCount int `json:"omitted_count,omitempty"`
}

// WorktreeCommitView summarizes one local Git checkout/worktree (or sub-project
// subpath) and its latest Git commit inspected strictly on the client machine.
type WorktreeCommitView struct {
	// Path is the normalized local directory path of the worktree or sub-project.
	Path string `json:"path"`

	// RelSubpath is the relative path inside the Git worktree when scoped to a sub-project.
	RelSubpath string `json:"rel_subpath,omitempty"`

	// Branch is the checked-out Git branch name (or "detached").
	Branch string `json:"branch,omitempty"`

	// ShortSHA is the abbreviated commit hash.
	ShortSHA string `json:"short_sha,omitempty"`

	// Subject is the single-line commit summary.
	Subject string `json:"subject,omitempty"`

	// RelativeTime is the human-readable age of the commit (e.g., "2 hours ago").
	RelativeTime string `json:"relative_time,omitempty"`
}

// ProjectStateCountsView partitions all non-deleted tasks of a project across
// Flowsk's canonical lifecycle buckets.
type ProjectStateCountsView struct {
	// Total is the total count of non-deleted tasks in the project.
	Total int `json:"total"`

	// Backlog counts tasks in triage/backlog states.
	Backlog int `json:"backlog"`

	// Planned counts unblocked tasks in todo/ready states.
	Planned int `json:"planned"`

	// InProgress counts active tasks in progress.
	InProgress int `json:"in_progress"`

	// InReview counts tasks awaiting review or QA.
	InReview int `json:"in_review"`

	// Blocked counts tasks in a blocked state or waiting on unresolved blockers.
	Blocked int `json:"blocked"`

	// Done counts completed tasks.
	Done int `json:"done"`

	// Canceled counts canceled tasks.
	Canceled int `json:"canceled"`

	// Archived counts archived tasks.
	Archived int `json:"archived,omitempty"`

	// ActiveClaims counts tasks currently holding a live actor lease.
	ActiveClaims int `json:"active_claims"`
}

// ProjectStatusSummaryView summarizes one project's task state counts, goal
// completion, local Git worktrees/commits, and nested child sub-projects.
type ProjectStatusSummaryView struct {
	// Project holds the logical project metadata.
	Project ProjectView `json:"project"`

	// ScopeSubpath holds the relative subpath under its parent project when bound as a sub-project.
	ScopeSubpath string `json:"scope_subpath,omitempty"`

	// Depth indicates nesting depth in the multi-project hierarchy (0 = root project).
	Depth int `json:"depth"`

	// GoalsDone counts completed/achieved objectives in this project.
	GoalsDone int `json:"goals_done"`

	// GoalsTotal counts total objectives in this project.
	GoalsTotal int `json:"goals_total"`

	// Counts holds the separate per-state task breakdown for THIS project only.
	Counts ProjectStateCountsView `json:"counts"`

	// Worktrees lists local Git worktrees (or subpath commits) bound on this machine.
	Worktrees []WorktreeCommitView `json:"worktrees,omitempty"`

	// Children lists direct sub-projects bound beneath this project in project_bindings.
	Children []ProjectStatusSummaryView `json:"children,omitempty"`
}

// WorkspaceOverviewView renders the global multi-project `flowsk status` (`--all`
// or outside any bound directory) across all active projects and sub-projects.
type WorkspaceOverviewView struct {
	// Workspace is the active workspace slug or UUID.
	Workspace string `json:"workspace"`

	// TotalProjects counts distinct active projects in the workspace.
	TotalProjects int `json:"total_projects"`

	// TotalSubprojects counts distinct active projects bound under at least one parent.
	TotalSubprojects int `json:"total_subprojects"`

	// Counts aggregates task counts across every distinct active project in the workspace.
	Counts ProjectStateCountsView `json:"counts"`

	// Projects holds root/macro projects with nested child sub-projects underneath.
	Projects []ProjectStatusSummaryView `json:"projects"`

	// OrphanIssuesCount counts detected orphan `.md` files or broken DB Markdown references.
	OrphanIssuesCount int `json:"orphan_issues_count,omitempty"`
}

// BoardView represents the unified project dashboard rendered by `flowsk board`
// (`flowsk status`, `flowsk dash`).
type BoardView struct {
	// Project holds the active project metadata header.
	Project ProjectView `json:"project"`

	// StateCounts holds the full per-state task breakdown across the entire project.
	StateCounts ProjectStateCountsView `json:"state_counts"`

	// ScopeSubpath holds the relative subpath when this project is bound under a parent.
	ScopeSubpath string `json:"scope_subpath,omitempty"`

	// ParentKeys lists parent macro project keys when this project is a sub-project.
	ParentKeys []string `json:"parent_keys,omitempty"`

	// Worktrees lists local Git worktrees and latest commits for this project on this machine.
	Worktrees []WorktreeCommitView `json:"worktrees,omitempty"`

	// Subprojects lists child sub-projects bound under this project, treated separately.
	Subprojects []ProjectStatusSummaryView `json:"subprojects,omitempty"`

	// Objectives holds goals, milestones, and cycles with live completion progress.
	Objectives []ObjectiveView `json:"objectives"`

	// Columns holds tasks grouped by workflow state pipeline order.
	Columns []BoardColumnView `json:"columns"`

	// ActiveClaims counts tasks with an active actor lease.
	ActiveClaims int `json:"active_claims"`

	// BlockedCount counts tasks currently blocked.
	BlockedCount int `json:"blocked_count"`

	// TotalTasks counts all matching tasks across board columns.
	TotalTasks int `json:"total_tasks"`

	// OrphanIssuesCount counts detected orphan `.md` files or broken DB Markdown references.
	OrphanIssuesCount int `json:"orphan_issues_count,omitempty"`
}

// ExecuteScriptInput carries client-read ES6 JavaScript parameters for
// `flowsk code` (`exec`) and MCP `flowsk_execute`. Local script file paths are
// never included on the wire.
type ExecuteScriptInput struct {
	// Workspace identifies the target workspace UUID or slug.
	Workspace string `json:"workspace"`

	// Project optionally provides an ambient default project UUID, slug, or key.
	Project string `json:"project,omitempty"`

	// Actor identifies the executing human or agent handle.
	Actor string `json:"actor,omitempty"`

	// Script contains the raw UTF-8 ES6 JavaScript source code targeting `flowsk.*`.
	Script string `json:"script"`

	// DryRun simulates execution in a rolled-back transaction without committing mutations.
	DryRun bool `json:"dry_run"`

	// IdempotencyKey provides an optional deduplication token for safe retries.
	IdempotencyKey string `json:"idempotency_key,omitempty"`

	// Density controls response verbosity ("minimal", "standard", "verbose").
	Density string `json:"density"`

	// MaxTokens caps serialized execution summary size when > 0.
	MaxTokens int `json:"max_tokens,omitempty"`
}

// ExecuteScriptResult captures the outcome of `flowsk code` (`flowsk_execute`).
type ExecuteScriptResult struct {
	// DryRun reports whether the script ran in rollback simulation mode.
	DryRun bool `json:"dry_run"`

	// Density echoes the requested output verbosity tier.
	Density string `json:"density"`

	// AppliedCount is the number of domain mutations performed during execution.
	AppliedCount int `json:"applied_count"`

	// FormattedSummary holds the rendered console output and serialized JS return value.
	FormattedSummary string `json:"formatted_summary,omitempty"`
}

// LanguageGuideView wraps the Code Mode TypeScript SDK Markdown specification.
type LanguageGuideView struct {
	// Section echoes the requested documentation slice.
	Section string `json:"section"`

	// Version identifies the specification revision.
	Version string `json:"version,omitempty"`

	// Markdown holds the specification content.
	Markdown string `json:"markdown"`

	// WrittenToPath is populated when written to a local file.
	WrittenToPath string `json:"written_to_path,omitempty"`
}

// ListAuditEventsInput carries filter parameters for `flowsk audit list`.
type ListAuditEventsInput struct {
	// Workspace identifies the mandatory tenant workspace UUID or slug.
	Workspace string `json:"workspace"`

	// Project optionally filters events scoped to a project UUID or key.
	Project string `json:"project,omitempty"`

	// Actor optionally filters events triggered by an actor handle.
	Actor string `json:"actor,omitempty"`

	// EntityType optionally filters by domain aggregate kind (task, objective, project, etc.).
	EntityType string `json:"entity_type,omitempty"`

	// EntityID optionally filters by target entity UUID or key.
	EntityID string `json:"entity_id,omitempty"`

	// Action optionally filters by mutation verb (e.g., "task.claimed").
	Action string `json:"action,omitempty"`

	// Since restricts events to OccurredAt >= *Since when non-nil.
	Since *time.Time `json:"since,omitempty"`

	// Limit bounds the maximum events returned.
	Limit int `json:"limit"`

	// Cursor holds an opaque pagination cursor.
	Cursor string `json:"cursor,omitempty"`
}

// AuditEventView represents an immutable workspace mutation audit entry.
type AuditEventView struct {
	// ID is the unique audit event UUID or identifier.
	ID string `json:"id"`

	// WorkspaceID identifies the owning workspace.
	WorkspaceID string `json:"workspace_id"`

	// ProjectID optionally identifies the affected project.
	ProjectID string `json:"project_id,omitempty"`

	// Actor is the handle or ID of the actor who performed the mutation.
	Actor string `json:"actor"`

	// ActorKind classifies the actor ("human", "agent", "ci").
	ActorKind string `json:"actor_kind,omitempty"`

	// Action names the recorded mutation verb.
	Action string `json:"action"`

	// EntityType identifies the mutated resource category.
	EntityType string `json:"entity_type"`

	// EntityID identifies the mutated entity UUID or key.
	EntityID string `json:"entity_id"`

	// Summary provides a compact one-line description of the change.
	Summary string `json:"summary,omitempty"`

	// OldValues holds an optional JSON snapshot prior to mutation.
	OldValues string `json:"old_values,omitempty"`

	// NewValues holds an optional JSON snapshot after mutation.
	NewValues string `json:"new_values,omitempty"`

	// OccurredAt is the UTC transaction timestamp.
	OccurredAt time.Time `json:"occurred_at"`
}

// AuditEventListResult wraps paginated audit entries for `flowsk audit list`.
type AuditEventListResult struct {
	// Items holds the returned audit events.
	Items []AuditEventView `json:"items"`

	// NextCursor holds an optional continuation cursor.
	NextCursor string `json:"next_cursor,omitempty"`
}

// MigrateUpInput carries forward schema migration options for `flowsk db migrate up`.
type MigrateUpInput struct {
	// Driver selects "sqlite" or "postgres".
	Driver string `json:"driver"`

	// DSN is the SQLite file path or PostgreSQL connection URI.
	DSN string `json:"dsn"`

	// TargetVersion optionally caps forward migration to a specific version (0 = latest).
	TargetVersion int64 `json:"target_version,omitempty"`
}

// MigrateDownInput carries rollback schema migration options for `flowsk db migrate down`.
type MigrateDownInput struct {
	// Driver selects "sqlite" or "postgres".
	Driver string `json:"driver"`

	// DSN is the SQLite file path or PostgreSQL connection URI.
	DSN string `json:"dsn"`

	// Steps specifies how many applied migrations to roll back.
	Steps int `json:"steps"`
}

// MigrationResult summarizes applied or rolled-back Goose migrations.
type MigrationResult struct {
	// Driver echoes "sqlite" or "postgres".
	Driver string `json:"driver"`

	// Direction indicates "up" or "down".
	Direction string `json:"direction"`

	// CurrentVersion is the active schema version after migration.
	CurrentVersion int64 `json:"current_version"`

	// AppliedCount is the number of migration files executed.
	AppliedCount int `json:"applied_count"`
}

// MigrationStatusEntry describes a single Goose schema migration file's state.
type MigrationStatusEntry struct {
	// Version is the numeric migration version.
	Version int64 `json:"version"`

	// Name is the descriptive migration filename or title.
	Name string `json:"name"`

	// Applied reports whether the migration has been applied to the target DB.
	Applied bool `json:"applied"`

	// AppliedAt records the UTC timestamp when applied, if available.
	AppliedAt *time.Time `json:"applied_at,omitempty"`
}

// DBPingView reports connectivity status for `flowsk db ping`.
type DBPingView struct {
	// Status is "ok" when the database responded within timeout.
	Status string `json:"status"`

	// Driver echoes "sqlite" or "postgres".
	Driver string `json:"driver"`

	// LatencyMS records round-trip ping duration in milliseconds.
	LatencyMS int64 `json:"latency_ms"`
}

// ConfigPathsView displays resolved user-scoped Flowsk client file locations.
type ConfigPathsView struct {
	// ConfigDir is the user-level configuration directory (~/.flowsk).
	ConfigDir string `json:"config_dir"`

	// ConfigFile is the resolved YAML configuration path (~/.flowsk/config.yaml).
	ConfigFile string `json:"config_file"`

	// LocalPathsFile is the client-only folder binding registry (~/.flowsk/local_paths.json).
	LocalPathsFile string `json:"local_paths_file"`

	// LocalDBFile is the local embedded SQLite WAL database path (~/.flowsk/flowsk.db).
	LocalDBFile string `json:"local_db_file"`
}

// ConfigShowView displays merged effective client settings for `flowsk config show`.
type ConfigShowView struct {
	// Mode reports "local" (embedded SQLite) or "remote" (ConnectRPC server).
	Mode string `json:"mode"`

	// ConfigFile is the active config.yaml path.
	ConfigFile string `json:"config_file"`

	// LocalPathsFile is the client-local directory registry path.
	LocalPathsFile string `json:"local_paths_file"`

	// DBPath is the local SQLite database path used in local mode.
	DBPath string `json:"db_path"`

	// ServerURL is the remote ConnectRPC server URL when configured.
	ServerURL string `json:"server_url,omitempty"`

	// AuthToken is either empty, redacted ("***REDACTED***"), or raw when --redact=false.
	AuthToken string `json:"auth_token,omitempty"`

	// Workspace is the active default workspace slug or UUID.
	Workspace string `json:"workspace"`

	// Project is the explicit active project key/UUID, if set.
	Project string `json:"project,omitempty"`

	// Actor is the active human or AI agent handle, if set.
	Actor string `json:"actor,omitempty"`

	// Format is the configured output encoding.
	Format string `json:"format"`

	// MaxTokens is the configured output token ceiling (0 = unlimited).
	MaxTokens int `json:"max_tokens"`
}

// ConfigInitView reports files initialized by `flowsk config init`.
type ConfigInitView struct {
	// ConfigFile is the created or updated config.yaml path.
	ConfigFile string `json:"config_file"`

	// LocalPathsFile is the initialized or preserved local_paths.json path.
	LocalPathsFile string `json:"local_paths_file"`

	// Overwritten reports whether an existing config.yaml was overwritten via --force.
	Overwritten bool `json:"overwritten"`
}

// MCPToolItemView summarizes a canonical Flowsk MCP tool for `flowsk mcp tools`.
type MCPToolItemView struct {
	// Name is the MCP tool identifier (e.g., "flowsk_execute").
	Name string `json:"name"`

	// Title is the human-readable tool title.
	Title string `json:"title"`

	// Description summarizes tool behavior and agent guidance.
	Description string `json:"description"`

	// ReadOnly reports whether the tool is side-effect free.
	ReadOnly bool `json:"read_only"`

	// Idempotent reports whether repeated identical calls are idempotent.
	Idempotent bool `json:"idempotent"`

	// InputSchema holds the JSON Schema when --schema is requested.
	InputSchema any `json:"input_schema,omitempty"`
}

// MCPToolsView aggregates canonical MCP tools and resources for `flowsk mcp tools`.
type MCPToolsView struct {
	// Tools lists registered Flowsk MCP tools.
	Tools []MCPToolItemView `json:"tools"`

	// Resources lists registered read-only Flowsk MCP resources.
	Resources []mcp.ResourceSpec `json:"resources"`

	// IncludeSchema indicates whether full JSON Schemas are included.
	IncludeSchema bool `json:"include_schema"`
}

// MCPClientSnippetView provides copy-paste setup instructions for one AI harness.
type MCPClientSnippetView struct {
	// Client identifies the target harness ("claude", "gemini", "antigravity", "codex", "chatgpt").
	Client string `json:"client"`

	// ConfigPathHint indicates where the client reads its MCP config.
	ConfigPathHint string `json:"config_path_hint"`

	// ConfigSnippet contains the copy-paste JSON/TOML/CLI setup block.
	ConfigSnippet string `json:"config_snippet"`

	// SystemPrompt contains a minimal token-frugal agent instruction snippet.
	SystemPrompt string `json:"system_prompt"`
}

// MCPGuideView holds onboarding configurations rendered by `flowsk mcp guide`.
type MCPGuideView struct {
	// TargetClient echoes the requested client filter ("all" or specific harness).
	TargetClient string `json:"target_client"`

	// Snippets holds the matching harness setup blocks.
	Snippets []MCPClientSnippetView `json:"snippets"`
}

// ServerRunner executes the HTTP/ConnectRPC Flowsk API daemon (`flowsk serve`).
type ServerRunner interface {
	// RunServer starts the ConnectRPC server and blocks until ctx is canceled or fatal error.
	RunServer(ctx context.Context, cfg ServeConfig) error
}

// MCPRunner constructs the Flowsk Model Context Protocol server for `flowsk mcp serve`.
type MCPRunner interface {
	// NewMCPServer builds an MCP Server bound to cfg.
	NewMCPServer(cfg mcp.Config) mcp.Server
}

// WorkspaceClient defines the local/remote client port for tenant workspaces and
// registered human/agent actors.
type WorkspaceClient interface {
	// CreateWorkspace provisions a new multi-tenant workspace.
	CreateWorkspace(ctx context.Context, in CreateWorkspaceInput) (WorkspaceView, error)

	// ListWorkspaces lists accessible workspaces.
	ListWorkspaces(ctx context.Context, in ListWorkspacesInput) (WorkspaceListResult, error)

	// GetWorkspace fetches a single workspace by UUID or slug.
	GetWorkspace(ctx context.Context, workspaceRef string) (WorkspaceView, error)

	// RegisterActor registers or updates a human, AI agent, or CI actor in a workspace.
	RegisterActor(ctx context.Context, in RegisterActorInput) (ActorView, error)

	// ListActors lists registered actors within a workspace.
	ListActors(ctx context.Context, in ListActorsInput) (ActorListResult, error)
}

// DeleteProjectInput carries parameters for `flowsk project delete`.
type DeleteProjectInput struct {
	// Workspace identifies the owning workspace UUID or slug.
	Workspace string `json:"workspace"`

	// Project identifies the target project UUID, slug, or key.
	Project string `json:"project"`

	// Actor identifies the principal deleting the project.
	Actor string `json:"actor,omitempty"`
}

// DeleteProjectResultView reports soft-deletion of a project (with non-destructive
// detachment of any sub-project bindings).
type DeleteProjectResultView struct {
	// WorkspaceID identifies the owning workspace.
	WorkspaceID string `json:"workspace_id"`

	// DeletedProjectID is the soft-deleted project UUID.
	DeletedProjectID string `json:"deleted_project_id"`

	// DeletedKey is the uppercase key of the deleted project.
	DeletedKey string `json:"deleted_key,omitempty"`
}

// BindSubprojectInput carries parameters for `flowsk project bind`.
type BindSubprojectInput struct {
	// Workspace identifies the owning workspace UUID or slug.
	Workspace string `json:"workspace"`

	// ParentProject identifies the parent macro project UUID, slug, or key.
	ParentProject string `json:"parent_project"`

	// ChildProject identifies the child sub-project UUID, slug, or key.
	ChildProject string `json:"child_project"`

	// ScopeSubpath holds an optional clean relative subpath (never an absolute host path).
	ScopeSubpath string `json:"scope_subpath,omitempty"`

	// Actor identifies the principal creating the binding.
	Actor string `json:"actor,omitempty"`
}

// UnbindSubprojectInput carries parameters for `flowsk project unbind`.
type UnbindSubprojectInput struct {
	// Workspace identifies the owning workspace UUID or slug.
	Workspace string `json:"workspace"`

	// ParentProject identifies the parent macro project UUID, slug, or key.
	ParentProject string `json:"parent_project"`

	// ChildProject identifies the child sub-project UUID, slug, or key.
	ChildProject string `json:"child_project"`

	// Actor identifies the principal removing the binding.
	Actor string `json:"actor,omitempty"`
}

// UnbindSubprojectResultView reports non-destructive removal of a sub-project binding.
type UnbindSubprojectResultView struct {
	// ParentProject is the parent project key.
	ParentProject string `json:"parent_project"`

	// ChildProject is the child sub-project key.
	ChildProject string `json:"child_project"`

	// Removed reports whether an active binding edge existed and was removed.
	Removed bool `json:"removed"`
}

// ListProjectBindingsInput carries filter parameters for `flowsk project bindings`.
type ListProjectBindingsInput struct {
	// Workspace identifies the owning workspace UUID or slug.
	Workspace string `json:"workspace"`

	// ParentProject optionally filters edges by parent project UUID, slug, or key.
	ParentProject string `json:"parent_project,omitempty"`

	// ChildProject optionally filters edges by child project UUID, slug, or key.
	ChildProject string `json:"child_project,omitempty"`
}

// ProjectBindingView represents a directed parent->child sub-project edge.
type ProjectBindingView struct {
	// ID is the immutable binding UUID.
	ID string `json:"id"`

	// WorkspaceID identifies the owning workspace.
	WorkspaceID string `json:"workspace_id"`

	// ParentProjectID is the parent project UUID.
	ParentProjectID string `json:"parent_project_id"`

	// ParentKey is the parent project key prefix.
	ParentKey string `json:"parent_key,omitempty"`

	// ChildProjectID is the child sub-project UUID.
	ChildProjectID string `json:"child_project_id"`

	// ChildKey is the child sub-project key prefix.
	ChildKey string `json:"child_key,omitempty"`

	// ScopeSubpath holds the optional relative subfolder under the parent repo.
	ScopeSubpath string `json:"scope_subpath,omitempty"`

	// CreatedAt records when the binding was established in UTC.
	CreatedAt time.Time `json:"created_at"`
}

// InitResultView summarizes the outcome of `flowsk init`.
type InitResultView struct {
	// Project is the created or reused logical project.
	Project ProjectView `json:"project"`

	// LocalDir is the client-local folder bound in ~/.flowsk/local_paths.json.
	LocalDir string `json:"local_dir"`

	// HasDirectDotGit reports whether a direct .git entry was found in LocalDir.
	HasDirectDotGit bool `json:"has_direct_dot_git"`

	// ReusedExisting reports whether an existing project (or sibling Git worktree) was reused.
	ReusedExisting bool `json:"reused_existing"`

	// WorktreeOf holds the shared project key when LocalDir was auto-grouped as a Git worktree.
	WorktreeOf string `json:"worktree_of,omitempty"`

	// ParentKey holds the parent macro project key when auto-bound as a sub-project.
	ParentKey string `json:"parent_key,omitempty"`

	// ScopeSubpath holds the relative subpath under ParentKey when auto-bound.
	ScopeSubpath string `json:"scope_subpath,omitempty"`

	// AdoptedSubprojects lists existing child sub-project keys automatically adopted beneath LocalDir.
	AdoptedSubprojects []string `json:"adopted_subprojects,omitempty"`

	// Worktrees lists discovered Git worktrees and commits for this project on this machine.
	Worktrees []WorktreeCommitView `json:"worktrees,omitempty"`
}

// MarkdownDocView represents an extended `.md` document attached to a task or project.
type MarkdownDocView struct {
	// EntityKind identifies "task" or "project".
	EntityKind string `json:"entity_kind"`

	// EntityKey is the compact task identifier (e.g. "CORE-1") or project key (e.g. "CORE").
	EntityKey string `json:"entity_key"`

	// EntityID is the target entity UUID.
	EntityID string `json:"entity_id"`

	// WorkspaceID identifies the owning workspace.
	WorkspaceID string `json:"workspace_id"`

	// HasMarkdownDoc reports whether a Markdown document currently exists for the entity.
	HasMarkdownDoc bool `json:"has_markdown_doc"`

	// Markdown holds the UTF-8 Markdown content when fetched or updated.
	Markdown string `json:"markdown,omitempty"`

	// Action describes the operation performed ("get", "put", "delete").
	Action string `json:"action"`
}

// PurgeArchivedView summarizes permanent deletion of all archived tasks in a workspace.
type PurgeArchivedView struct {
	// WorkspaceID identifies the purged workspace.
	WorkspaceID string `json:"workspace_id"`

	// DeletedTaskCount is the number of archived tasks permanently deleted from the database.
	DeletedTaskCount int `json:"deleted_task_count"`

	// DeletedMarkdownCount is the number of associated task `.md` files removed from global storage.
	DeletedMarkdownCount int `json:"deleted_markdown_count"`

	// DeletedTaskKeys lists the compact task keys permanently removed.
	DeletedTaskKeys []string `json:"deleted_task_keys,omitempty"`
}

// OrphanReportView summarizes detected orphan `.md` files and broken DB Markdown flags.
type OrphanReportView struct {
	// WorkspaceID identifies the inspected workspace.
	WorkspaceID string `json:"workspace_id"`

	// OrphanTaskFilesCount counts `.md` files in `tasks/<ws>/` without an active DB task.
	OrphanTaskFilesCount int `json:"orphan_task_files_count"`

	// MissingTaskFilesCount counts active DB tasks with `has_markdown_doc=true` missing their `.md` file.
	MissingTaskFilesCount int `json:"missing_task_files_count"`

	// OrphanProjectFilesCount counts `.md` files in `projects/<ws>/` without an active DB project.
	OrphanProjectFilesCount int `json:"orphan_project_files_count"`

	// MissingProjectFilesCount counts active DB projects with `has_markdown_doc=true` missing their `.md` file.
	MissingProjectFilesCount int `json:"missing_project_files_count"`

	// TotalIssues is the total number of detected discrepancies across tasks and projects.
	TotalIssues int `json:"total_issues"`

	// IssueSummaries lists concise diagnostic lines (never exposing host filesystem paths).
	IssueSummaries []string `json:"issue_summaries,omitempty"`
}

// PruneResultView summarizes reconciliation performed by `flowsk prune`.
type PruneResultView struct {
	// WorkspaceID identifies the pruned workspace.
	WorkspaceID string `json:"workspace_id"`

	// DryRun reports whether `flowsk prune --dry-run` ran without mutating state.
	DryRun bool `json:"dry_run"`

	// RemovedTaskFilesCount counts orphan task `.md` files removed (or matched in dry-run).
	RemovedTaskFilesCount int `json:"removed_task_files_count"`

	// ClearedTaskRefsCount counts broken task `has_markdown_doc` flags reset.
	ClearedTaskRefsCount int `json:"cleared_task_refs_count"`

	// RemovedProjectFilesCount counts orphan project `.md` files removed (or matched in dry-run).
	RemovedProjectFilesCount int `json:"removed_project_files_count"`

	// ClearedProjectRefsCount counts broken project `has_markdown_doc` flags reset.
	ClearedProjectRefsCount int `json:"cleared_project_refs_count"`

	// TotalCleaned is the total number of reconciled orphan files and broken flags.
	TotalCleaned int `json:"total_cleaned"`

	// CleanedSummaries lists human-readable reconciliation lines.
	CleanedSummaries []string `json:"cleaned_summaries,omitempty"`
}

// SkillInstallItemView summarizes a single AI provider `SKILL.md` installation outcome.
type SkillInstallItemView struct {
	// ProviderID is the canonical provider slug ("claude", "codex", "antigravity", etc.).
	ProviderID string `json:"provider_id"`

	// ProviderName is the human-readable AI agent name.
	ProviderName string `json:"provider_name"`

	// SkillPath is the absolute path of the installed `SKILL.md`.
	SkillPath string `json:"skill_path"`

	// Action reports "installed", "updated", or "unchanged".
	Action string `json:"action"`

	// SHA256 is the verified hex SHA-256 digest of the installed `SKILL.md`.
	SHA256 string `json:"sha256"`

	// PreviousSHA256 holds the prior digest when Action == "updated".
	PreviousSHA256 string `json:"previous_sha256,omitempty"`

	// MCPConfigured reports whether the provider's MCP configuration already mentions `flowsk`.
	MCPConfigured bool `json:"mcp_configured"`

	// MCPConfigPath is the provider's primary MCP config file hint, if applicable.
	MCPConfigPath string `json:"mcp_config_path,omitempty"`
}

// MCPInstallItemView records the result of registering or updating the Flowsk MCP
// server inside one AI client configuration file during `flowsk install`.
type MCPInstallItemView struct {
	// ProviderID is the canonical provider slug (claude, codex, antigravity, gemini, cursor).
	ProviderID string `json:"provider_id"`

	// ProviderName is the human-readable AI client name.
	ProviderName string `json:"provider_name"`

	// ConfigPath is the configured MCP file path.
	ConfigPath string `json:"config_path"`

	// Command is the executable configured for `flowsk`.
	Command string `json:"command"`

	// Args lists the CLI arguments configured for `flowsk`.
	Args []string `json:"args"`

	// Action reports "installed", "updated", or "unchanged".
	Action string `json:"action"`
}

// InstallSkillView summarizes the execution of `flowsk install` across both
// global `SKILL.md` directories and client MCP server configuration files.
type InstallSkillView struct {
	// EmbeddedSHA256 is the canonical SHA-256 digest embedded in the running `flowsk` binary.
	EmbeddedSHA256 string `json:"embedded_sha256"`

	// InstalledCount counts newly created `SKILL.md` files.
	InstalledCount int `json:"installed_count"`

	// UpdatedCount counts overwritten `SKILL.md` files whose prior hash differed.
	UpdatedCount int `json:"updated_count"`

	// UnchangedCount counts `SKILL.md` files already matching EmbeddedSHA256.
	UnchangedCount int `json:"unchanged_count"`

	// Items lists per-provider `SKILL.md` installation outcomes.
	Items []SkillInstallItemView `json:"items"`

	// MCPInstalledCount counts newly registered `flowsk` MCP server entries.
	MCPInstalledCount int `json:"mcp_installed_count"`

	// MCPUpdatedCount counts refreshed `flowsk` MCP server entries.
	MCPUpdatedCount int `json:"mcp_updated_count"`

	// MCPUnchangedCount counts `flowsk` MCP server entries already up to date.
	MCPUnchangedCount int `json:"mcp_unchanged_count"`

	// MCPItems lists per-provider MCP configuration outcomes.
	MCPItems []MCPInstallItemView `json:"mcp_items,omitempty"`
}

// SkillUninstallItemView records the result of removing `SKILL.md` from one AI provider.
type SkillUninstallItemView struct {
	// ProviderID is the canonical provider slug.
	ProviderID string `json:"provider_id"`

	// ProviderName is the human-readable AI tool name.
	ProviderName string `json:"provider_name"`

	// SkillPath is the target `SKILL.md` path inspected/removed.
	SkillPath string `json:"skill_path"`

	// Action is "removed" or "not_found".
	Action string `json:"action"`
}

// MCPUninstallItemView records the result of removing the Flowsk MCP server from one AI client config.
type MCPUninstallItemView struct {
	// ProviderID is the canonical provider slug.
	ProviderID string `json:"provider_id"`

	// ProviderName is the human-readable AI client name.
	ProviderName string `json:"provider_name"`

	// ConfigPath is the configured MCP file path inspected/updated.
	ConfigPath string `json:"config_path"`

	// Action is "removed" or "not_found".
	Action string `json:"action"`
}

// UninstallSkillView summarizes the execution of `flowsk uninstall` across both
// global `SKILL.md` directories and client MCP server configuration files.
type UninstallSkillView struct {
	// RemovedSkillCount counts deleted `SKILL.md` files.
	RemovedSkillCount int `json:"removed_skill_count"`

	// NotFoundSkillCount counts selected skill paths that were already absent.
	NotFoundSkillCount int `json:"not_found_skill_count"`

	// SkillItems lists per-provider skill removal outcomes.
	SkillItems []SkillUninstallItemView `json:"skill_items,omitempty"`

	// RemovedMCPCount counts removed `flowsk` MCP server entries.
	RemovedMCPCount int `json:"removed_mcp_count"`

	// NotFoundMCPCount counts selected MCP configs where `flowsk` was already absent.
	NotFoundMCPCount int `json:"not_found_mcp_count"`

	// MCPItems lists per-provider MCP removal outcomes.
	MCPItems []MCPUninstallItemView `json:"mcp_items,omitempty"`
}

// DoctorCheckView represents one diagnostic subsystem check inside `flowsk doctor`.
type DoctorCheckView struct {
	// Name identifies the inspected subsystem ("home", "database", "migrations", "prune", "skills", "mcp").
	Name string `json:"name"`

	// Status is "ok", "warn", "fail", or "skip".
	Status string `json:"status"`

	// Summary is a concise human/agent status explanation.
	Summary string `json:"summary"`

	// Hint provides an actionable remediation command when Status is "warn" or "fail".
	Hint string `json:"hint,omitempty"`

	// Details lists optional diagnostic sub-bullets.
	Details []string `json:"details,omitempty"`
}

// DoctorSkillView reports per-provider `SKILL.md` hash and MCP registration status in `flowsk doctor`.
type DoctorSkillView struct {
	// ProviderID is the canonical provider slug.
	ProviderID string `json:"provider_id"`

	// ProviderName is the human-readable AI tool name.
	ProviderName string `json:"provider_name"`

	// Detected reports whether the provider's home folder exists on this machine.
	Detected bool `json:"detected"`

	// Installed reports whether `<provider>/skills/flowsk/SKILL.md` exists on disk.
	Installed bool `json:"installed"`

	// UpToDate reports whether InstalledSHA256 matches ExpectedSHA256.
	UpToDate bool `json:"up_to_date"`

	// InstalledSHA256 is the hex SHA-256 of the on-disk `SKILL.md`, if installed.
	InstalledSHA256 string `json:"installed_sha256,omitempty"`

	// ExpectedSHA256 is the hex SHA-256 embedded in the running `flowsk` binary.
	ExpectedSHA256 string `json:"expected_sha256"`

	// SkillPath is the target `SKILL.md` path for this provider.
	SkillPath string `json:"skill_path"`

	// MCPConfigured reports whether the provider's MCP config mentions `flowsk`.
	MCPConfigured bool `json:"mcp_configured"`
}

// DoctorReportView aggregates all diagnostic checks produced by `flowsk doctor`.
type DoctorReportView struct {
	// Version is the semantic version of the running `flowsk` binary.
	Version string `json:"version"`

	// Mode indicates "local" or "remote".
	Mode string `json:"mode"`

	// EmbeddedSkillSHA256 is the hex SHA-256 digest of the embedded `SKILL.md`.
	EmbeddedSkillSHA256 string `json:"embedded_skill_sha256"`

	// OKCount counts passing subsystem checks.
	OKCount int `json:"ok_count"`

	// WarnCount counts warning subsystem checks.
	WarnCount int `json:"warn_count"`

	// FailCount counts failing subsystem checks.
	FailCount int `json:"fail_count"`

	// Checks lists individual subsystem diagnostic entries.
	Checks []DoctorCheckView `json:"checks"`

	// Skills lists per-provider skill installation & hash verification statuses.
	Skills []DoctorSkillView `json:"skills"`
}

// UpdateCheckView summarizes GitHub release inspection performed by `flowsk update`.
type UpdateCheckView struct {
	// Repo is the GitHub `owner/repo` slug inspected (default "mindmain/flowsk").
	Repo string `json:"repo"`

	// CurrentVersion is the semantic version of the running `flowsk` binary.
	CurrentVersion string `json:"current_version"`

	// LatestVersion is the latest published GitHub release/tag version, if any.
	LatestVersion string `json:"latest_version,omitempty"`

	// Status is "up_to_date", "update_available", "ahead", or "unpublished".
	Status string `json:"status"`

	// UpdateAvailable is true when LatestVersion is newer than CurrentVersion.
	UpdateAvailable bool `json:"update_available"`

	// ReleaseURL links to the GitHub release page when available.
	ReleaseURL string `json:"release_url,omitempty"`

	// PublishedAt holds the UTC ISO-8601 release timestamp when available.
	PublishedAt string `json:"published_at,omitempty"`

	// UpgradeCommand provides the one-line command to install the latest release when UpdateAvailable is true.
	UpgradeCommand string `json:"upgrade_command,omitempty"`

	// Message provides a human-readable summary of the update check.
	Message string `json:"message"`
}

// ProjectClient defines the local/remote client port for logical projects,
// multi-binding sub-project DAG edges, workflow states, and labels. Host
// filesystem paths are never accepted by this port.
type ProjectClient interface {
	// CreateProject creates a new logical project within a workspace.
	CreateProject(ctx context.Context, in CreateProjectInput) (ProjectView, error)

	// ListProjects lists logical projects in a workspace.
	ListProjects(ctx context.Context, in ListProjectsInput) (ProjectListResult, error)

	// GetProject inspects a logical project by UUID, slug, or key.
	GetProject(ctx context.Context, workspaceRef, projectRef string) (ProjectView, error)

	// UpdateProject updates mutable metadata on a logical project.
	UpdateProject(ctx context.Context, in UpdateProjectInput) (ProjectView, error)

	// ArchiveProject soft-archives a logical project while preserving history.
	ArchiveProject(ctx context.Context, in ArchiveProjectInput) (ProjectView, error)

	// DeleteProject soft-deletes a logical project while detaching (never deleting) sub-projects.
	DeleteProject(ctx context.Context, in DeleteProjectInput) (DeleteProjectResultView, error)

	// BindSubproject creates or updates a parent->child sub-project edge in the workspace DAG.
	BindSubproject(ctx context.Context, in BindSubprojectInput) (ProjectBindingView, error)

	// UnbindSubproject detaches a parent->child sub-project edge without deleting either project.
	UnbindSubproject(ctx context.Context, in UnbindSubprojectInput) (UnbindSubprojectResultView, error)

	// ListProjectBindings lists active parent->child sub-project bindings in a workspace.
	ListProjectBindings(ctx context.Context, in ListProjectBindingsInput) ([]ProjectBindingView, error)

	// ListStates lists ordered workflow states configured for a project.
	ListStates(ctx context.Context, workspaceRef, projectRef string) ([]WorkflowStateView, error)

	// CreateState appends a custom workflow state to a project's pipeline.
	CreateState(ctx context.Context, in CreateStateInput) (WorkflowStateView, error)

	// ListLabels lists classification labels defined in a project.
	ListLabels(ctx context.Context, workspaceRef, projectRef string) ([]LabelView, error)

	// CreateLabel creates a classification label inside a project.
	CreateLabel(ctx context.Context, in CreateLabelInput) (LabelView, error)

	// GetProjectMarkdown retrieves the optional extended Markdown document for a project.
	GetProjectMarkdown(ctx context.Context, workspaceRef, projectRef string) (MarkdownDocView, error)

	// PutProjectMarkdown creates or overwrites the optional Markdown document for a project.
	PutProjectMarkdown(ctx context.Context, workspaceRef, projectRef, actor, markdown string) (MarkdownDocView, error)

	// DeleteProjectMarkdown removes the optional Markdown document for a project.
	DeleteProjectMarkdown(ctx context.Context, workspaceRef, projectRef, actor string) (MarkdownDocView, error)
}

// LocalPathRegistry manages the client-only local path store at
// ~/.flowsk/local_paths.json. Implementations must NEVER touch files
// inside target Git repositories and must NEVER send LocalDir values to any
// remote Flowsk server.
type LocalPathRegistry interface {
	// Link binds localDir on this workstation to (workspaceID, projectID).
	Link(ctx context.Context, localDir, workspaceID, projectID string) (LocalProjectBinding, error)

	// Unlink removes the local binding for localDir on this workstation.
	Unlink(ctx context.Context, localDir string) error

	// ResolveByDir resolves localDir (or its nearest bound ancestor) to a logical project.
	ResolveByDir(ctx context.Context, localDir string) (LocalProjectBinding, error)

	// ListByProject returns all local directories on this workstation bound to (workspaceID, projectID).
	ListByProject(ctx context.Context, workspaceID, projectID string) ([]LocalProjectBinding, error)
}

// ObjectiveClient defines the local/remote client port for strategic goals,
// milestones, and cycles with live task completion counts.
type ObjectiveClient interface {
	// CreateObjective creates a strategic goal, milestone, or cycle in a project.
	CreateObjective(ctx context.Context, in CreateObjectiveInput) (ObjectiveView, error)

	// ListObjectives lists objectives with live task completion roll-ups.
	ListObjectives(ctx context.Context, in ListObjectivesInput) (ObjectiveListResult, error)

	// GetObjective fetches a single objective and its task completion counts.
	GetObjective(ctx context.Context, workspaceRef, projectRef, objectiveRef string) (ObjectiveView, error)

	// UpdateObjective modifies objective fields, status, or priority.
	UpdateObjective(ctx context.Context, in UpdateObjectiveInput) (ObjectiveView, error)

	// CloseObjective closes an objective as completed, canceled, or superseded.
	CloseObjective(ctx context.Context, in CloseObjectiveInput) (ObjectiveView, error)
}

// TaskClient defines the local/remote client port for hierarchical tasks,
// optimistic agent claims/leases, dependency blockers, structured handoff
// comments, next-task selection, and low-token board digests.
type TaskClient interface {
	// CreateTask creates a task or subtask in a project.
	CreateTask(ctx context.Context, in CreateTaskInput) (TaskView, error)

	// ListTasks lists tasks matching compact filter criteria.
	ListTasks(ctx context.Context, in ListTasksInput) (TaskListResult, error)

	// GetTask fetches full task details by UUID or key.
	GetTask(ctx context.Context, in GetTaskInput) (TaskView, error)

	// UpdateTask applies partial updates to a task.
	UpdateTask(ctx context.Context, in UpdateTaskInput) (TaskView, error)

	// MoveTask transitions a task to another workflow state.
	MoveTask(ctx context.Context, in MoveTaskInput) (TaskView, error)

	// ClaimTask acquires an exclusive time-bounded lease on a task for an actor.
	ClaimTask(ctx context.Context, in ClaimTaskInput) (TaskView, error)

	// ReleaseTask releases an active lease and optionally transitions state.
	ReleaseTask(ctx context.Context, in ReleaseTaskInput) (TaskView, error)

	// BlockTask marks a task as blocked by upstream tasks or an impediment.
	BlockTask(ctx context.Context, in BlockTaskInput) (TaskView, error)

	// UnblockTask clears dependency blockers or impediments on a task.
	UnblockTask(ctx context.Context, in UnblockTaskInput) (TaskView, error)

	// AddComment appends a structured comment to a task timeline.
	AddComment(ctx context.Context, in AddCommentInput) (CommentView, error)

	// ListComments lists structured comments on a task.
	ListComments(ctx context.Context, in ListCommentsInput) (CommentListResult, error)

	// NextTask selects (and optionally claims) the highest-priority unblocked ready task.
	NextTask(ctx context.Context, in NextTaskInput) (NextTaskResult, error)

	// Digest returns an ultra-compact project board, lease, ready-queue, and blocker snapshot.
	Digest(ctx context.Context, in TaskDigestInput) (ProjectDigestView, error)

	// GetTaskMarkdown retrieves the optional extended Markdown document for a task.
	GetTaskMarkdown(ctx context.Context, workspaceRef, taskRef string) (MarkdownDocView, error)

	// PutTaskMarkdown creates or overwrites the optional Markdown document for a task.
	PutTaskMarkdown(ctx context.Context, workspaceRef, taskRef, actor, markdown string) (MarkdownDocView, error)

	// DeleteTaskMarkdown removes the optional Markdown document for a task.
	DeleteTaskMarkdown(ctx context.Context, workspaceRef, taskRef, actor string) (MarkdownDocView, error)

	// PurgeArchivedTasks permanently deletes all archived tasks and their `.md` files in a workspace.
	PurgeArchivedTasks(ctx context.Context, workspaceRef, actor string) (PurgeArchivedView, error)

	// InspectOrphans inspects a workspace for orphan `.md` files and broken DB Markdown flags.
	InspectOrphans(ctx context.Context, workspaceRef string) (OrphanReportView, error)

	// PruneOrphans reconciles orphan `.md` files and broken DB Markdown flags in a workspace.
	PruneOrphans(ctx context.Context, workspaceRef, actor string, dryRun bool) (PruneResultView, error)
}

// LanguageClient defines the local/remote port for Cloudflare Code Mode
// JavaScript execution (`flowsk code` / `flowsk_execute`) and on-demand
// TypeScript SDK specification retrieval (`flowsk_language_guide`).
type LanguageClient interface {
	// ExecuteScript atomically evaluates an ES6 JavaScript snippet against `flowsk.*`.
	ExecuteScript(ctx context.Context, in ExecuteScriptInput) (ExecuteScriptResult, error)

	// GetGuide retrieves the Code Mode JavaScript SDK Markdown specification.
	GetGuide(ctx context.Context, section string) (LanguageGuideView, error)
}

// AuditClient defines the local/remote client port for querying immutable,
// workspace-scoped mutation audit events.
type AuditClient interface {
	// ListAuditEvents queries workspace-scoped audit records matching filters.
	ListAuditEvents(ctx context.Context, in ListAuditEventsInput) (AuditEventListResult, error)

	// GetAuditEvent fetches a single audit event by ID within workspaceRef.
	GetAuditEvent(ctx context.Context, workspaceRef, eventID string) (AuditEventView, error)
}

// DatabaseAdmin defines schema migration (Goose) and health ping operations for
// SQLite and PostgreSQL backends (`flowsk db`).
type DatabaseAdmin interface {
	// MigrateUp applies pending forward Goose schema migrations.
	MigrateUp(ctx context.Context, in MigrateUpInput) (MigrationResult, error)

	// MigrateDown rolls back applied Goose schema migrations.
	MigrateDown(ctx context.Context, in MigrateDownInput) (MigrationResult, error)

	// MigrateStatus returns applied and pending Goose migration versions.
	MigrateStatus(ctx context.Context, driver, dsn string) ([]MigrationStatusEntry, error)

	// PingDB verifies database connectivity and readiness.
	PingDB(ctx context.Context, driver, dsn string) error
}

// OutputFormatter encodes structured command outputs into terminal-human or
// token-efficient wire representations (human, compact, tsv, jsonl, json, md)
// honoring --max-tokens caps.
type OutputFormatter interface {
	// Format serializes payload to w using format and optional maxTokens ceiling.
	Format(w io.Writer, format OutputFormat, maxTokens int, payload any) error
}

// Segregated single-method dependency accessors resolved immediately inside
// subcommand constructors per constructor-resolution DI rules.

// ServerDependency provides the daemon runner required by `flowsk serve`.
type ServerDependency interface {
	ServerRunner() ServerRunner
}

// MCPDependency provides the MCP server factory required by `flowsk mcp`.
type MCPDependency interface {
	MCPRunner() MCPRunner
}

// WorkspaceDependency provides the workspace client required by `flowsk workspace`.
type WorkspaceDependency interface {
	WorkspaceClient() WorkspaceClient
}

// ProjectDependency provides the logical project client required by `flowsk project`.
type ProjectDependency interface {
	ProjectClient() ProjectClient
}

// LocalPathDependency provides the client-only host directory registry required
// by `flowsk project link|unlink|where` and working-directory resolution.
type LocalPathDependency interface {
	LocalPathRegistry() LocalPathRegistry
}

// ObjectiveDependency provides the objective client required by `flowsk objective`.
type ObjectiveDependency interface {
	ObjectiveClient() ObjectiveClient
}

// TaskDependency provides the task client required by `flowsk task`.
type TaskDependency interface {
	TaskClient() TaskClient
}

// LanguageDependency provides the Code Mode JavaScript client required by `flowsk code`.
type LanguageDependency interface {
	LanguageClient() LanguageClient
}

// AuditDependency provides the audit client required by `flowsk audit`.
type AuditDependency interface {
	AuditClient() AuditClient
}

// DatabaseDependency provides the migration/ping admin required by `flowsk db`.
type DatabaseDependency interface {
	DatabaseAdmin() DatabaseAdmin
}

// FormatterDependency provides the token-aware output encoder shared across
// client commands.
type FormatterDependency interface {
	OutputFormatter() OutputFormatter
}

// Dependencies aggregates all segregated CLI capability providers injected at
// root bootstrap (`cmd/flowsk/main.go`). Each subcommand constructor extracts
// only its own resolved concrete client interfaces at construction time.
type Dependencies interface {
	ServerDependency
	MCPDependency
	WorkspaceDependency
	ProjectDependency
	LocalPathDependency
	ObjectiveDependency
	TaskDependency
	LanguageDependency
	AuditDependency
	DatabaseDependency
	FormatterDependency
}

// defaultDependencies is the zero-side-effect skeleton dependency provider used
// before concrete adapters are wired.
type defaultDependencies struct{}

// DefaultDependencies returns a safe skeleton Dependencies implementation for
// bootstrapping the full Flowsk CLI command tree without external connections.
func DefaultDependencies() Dependencies {
	return defaultDependencies{}
}

// ServerRunner returns nil until the HTTP/ConnectRPC daemon runner is wired.
func (defaultDependencies) ServerRunner() ServerRunner { return nil }

// MCPRunner returns nil until a concrete MCP server runner is wired.
func (defaultDependencies) MCPRunner() MCPRunner { return nil }

// WorkspaceClient returns nil until concrete local/remote workspace wiring is active.
func (defaultDependencies) WorkspaceClient() WorkspaceClient { return nil }

// ProjectClient returns nil until concrete local/remote project wiring is active.
func (defaultDependencies) ProjectClient() ProjectClient { return nil }

// LocalPathRegistry returns nil until the client-local JSON path registry is wired.
func (defaultDependencies) LocalPathRegistry() LocalPathRegistry { return nil }

// ObjectiveClient returns nil until concrete local/remote objective wiring is active.
func (defaultDependencies) ObjectiveClient() ObjectiveClient { return nil }

// TaskClient returns nil until concrete local/remote task wiring is active.
func (defaultDependencies) TaskClient() TaskClient { return nil }

// LanguageClient returns nil until concrete local/remote DSL wiring is active.
func (defaultDependencies) LanguageClient() LanguageClient { return nil }

// AuditClient returns nil until concrete local/remote audit wiring is active.
func (defaultDependencies) AuditClient() AuditClient { return nil }

// DatabaseAdmin returns nil until concrete Goose database administration is wired.
func (defaultDependencies) DatabaseAdmin() DatabaseAdmin { return nil }

// OutputFormatter returns nil so commands fall back to the built-in CLI renderer
// when no external OutputFormatter adapter is injected.
func (defaultDependencies) OutputFormatter() OutputFormatter { return nil }
