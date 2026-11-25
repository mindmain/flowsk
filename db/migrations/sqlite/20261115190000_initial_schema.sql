-- +goose Up
-- Initial Flowsk persistence schema for SQLite (primary fast single-node and shared-volume engine).
--
-- Architectural invariants:
-- 1. Primary keys use UUID strings stored as TEXT.
-- 2. All timestamps are stored as ISO-8601 UTC strings with millisecond precision:
--    strftime('%Y-%m-%dT%H:%M:%fZ', 'now').
-- 3. Multi-tenant isolation: every domain and operational table (below workspaces) carries workspace_id.
-- 4. Soft-delete safety: all mutable entity tables carry created_at, updated_at, and deleted_at,
--    paired with partial indexes filtering WHERE deleted_at IS NULL.
-- 5. Zero server-side host path leakage: server tables NEVER store local user filesystem paths.

-- =============================================================================
-- 1. WORKSPACES
-- Top-level multi-tenant isolation boundary containing actors, projects, and tasks.
-- =============================================================================
CREATE TABLE IF NOT EXISTS workspaces (
    -- Globally unique workspace UUID string.
    id TEXT PRIMARY KEY,
    -- URL-safe and CLI-friendly unique workspace identifier (active-unique via partial index).
    slug TEXT NOT NULL CHECK (length(trim(slug)) > 0),
    -- Human-readable workspace title.
    name TEXT NOT NULL CHECK (length(trim(name)) > 0),
    -- Optional workspace summary or operating Charter.
    description TEXT NOT NULL DEFAULT '',
    -- Creation timestamp in ISO-8601 UTC format.
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    -- Last modification timestamp in ISO-8601 UTC format.
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    -- Soft-delete timestamp in ISO-8601 UTC format; NULL means active.
    deleted_at TEXT DEFAULT NULL
);

-- Enforce slug uniqueness across non-deleted workspaces while permitting slug reuse after soft-deletion.
CREATE UNIQUE INDEX IF NOT EXISTS idx_workspaces_slug_active
    ON workspaces (slug)
    WHERE deleted_at IS NULL;

-- Accelerate ordered listing of active workspaces.
CREATE INDEX IF NOT EXISTS idx_workspaces_created_active
    ON workspaces (created_at ASC, id ASC)
    WHERE deleted_at IS NULL;

-- =============================================================================
-- 2. ACTORS
-- Principals (human operators or autonomous AI agents) operating inside a workspace.
-- =============================================================================
CREATE TABLE IF NOT EXISTS actors (
    -- Globally unique actor UUID string.
    id TEXT PRIMARY KEY,
    -- Owning workspace foreign key; cascades physical workspace removal.
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    -- Workspace-scoped @handle identifier (e.g. "alice", "coder-agent-1").
    handle TEXT NOT NULL CHECK (length(trim(handle)) > 0),
    -- Human-readable display name shown in CLI, TUI, and audit logs.
    display_name TEXT NOT NULL CHECK (length(trim(display_name)) > 0),
    -- Discriminator distinguishing human operators from autonomous AI agents.
    kind TEXT NOT NULL CHECK (kind IN ('human', 'agent')),
    -- Optional execution harness hint for agent actors (e.g. "claude-code", "antigravity", "codex").
    harness_hint TEXT NOT NULL DEFAULT '',
    -- JSON-encoded object or array describing agent skills, tools, or permissions.
    capabilities_json TEXT NOT NULL DEFAULT '{}',
    -- Creation timestamp in ISO-8601 UTC format.
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    -- Last modification timestamp in ISO-8601 UTC format.
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    -- Soft-delete timestamp in ISO-8601 UTC format; NULL means active.
    deleted_at TEXT DEFAULT NULL
);

-- Guarantee unique actor handles per workspace among active actors.
CREATE UNIQUE INDEX IF NOT EXISTS idx_actors_workspace_handle_active
    ON actors (workspace_id, handle)
    WHERE deleted_at IS NULL;

-- Accelerate filtering active workspace actors by kind ('human' vs 'agent').
CREATE INDEX IF NOT EXISTS idx_actors_workspace_kind_active
    ON actors (workspace_id, kind, handle ASC)
    WHERE deleted_at IS NULL;

-- =============================================================================
-- 3. PROJECTS
-- Logical product, repository, or initiative containers within a workspace.
--
-- SECURITY & PORTABILITY INVARIANT:
-- Server tables MUST NEVER store local user filesystem paths (such as working
-- directories, git worktree paths, or home directories). Local directory
-- mappings live strictly in client-local configuration on each operator/agent
-- machine and are never persisted in server database tables.
-- =============================================================================
CREATE TABLE IF NOT EXISTS projects (
    -- Globally unique project UUID string.
    id TEXT PRIMARY KEY,
    -- Owning workspace foreign key; cascades physical workspace removal.
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    -- Short uppercase prefix used to mint sequential human-readable task IDs (e.g. "FLW" -> "FLW-42").
    key_prefix TEXT NOT NULL CHECK (length(trim(key_prefix)) > 0),
    -- Workspace-scoped URL/CLI slug for addressing the project.
    slug TEXT NOT NULL CHECK (length(trim(slug)) > 0),
    -- Human-readable project name (NEVER a local filesystem path).
    name TEXT NOT NULL CHECK (length(trim(name)) > 0),
    -- Rich Markdown description of project scope and architecture (NEVER local host paths).
    description TEXT NOT NULL DEFAULT '',
    -- Lifecycle status of the project (e.g. 'planned', 'active', 'paused', 'completed', 'archived', 'canceled').
    status TEXT NOT NULL DEFAULT 'active' CHECK (length(trim(status)) > 0),
    -- Monotonically increasing per-project sequence counter for task identifiers.
    issue_counter INTEGER NOT NULL DEFAULT 0 CHECK (issue_counter >= 0),
    -- Optional actor who created the project; restricted from physical deletion to preserve provenance.
    created_by_actor_id TEXT DEFAULT NULL REFERENCES actors(id) ON DELETE RESTRICT,
    -- Creation timestamp in ISO-8601 UTC format.
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    -- Last modification timestamp in ISO-8601 UTC format.
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    -- Soft-delete timestamp in ISO-8601 UTC format; NULL means active.
    deleted_at TEXT DEFAULT NULL
);

-- Enforce unique project slugs within a workspace for active projects.
CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_workspace_slug_active
    ON projects (workspace_id, slug)
    WHERE deleted_at IS NULL;

-- Enforce unique task key prefixes (e.g. "FLW") within a workspace for active projects.
CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_workspace_key_prefix_active
    ON projects (workspace_id, key_prefix)
    WHERE deleted_at IS NULL;

-- Accelerate listing active projects by workspace and lifecycle status.
CREATE INDEX IF NOT EXISTS idx_projects_workspace_status_active
    ON projects (workspace_id, status, created_at ASC)
    WHERE deleted_at IS NULL;

-- =============================================================================
-- 4. WORKFLOW STATES
-- Customizable per-project Kanban/Linear state columns mapped to canonical categories.
-- =============================================================================
CREATE TABLE IF NOT EXISTS workflow_states (
    -- Globally unique workflow state UUID string.
    id TEXT PRIMARY KEY,
    -- Owning workspace foreign key; cascades physical workspace removal.
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    -- Owning project foreign key; cascades physical project removal.
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- Human-readable state column name (e.g. "Triage", "Todo", "In Progress", "Done").
    name TEXT NOT NULL CHECK (length(trim(name)) > 0),
    -- Canonical state category driving agent claimability and completion semantics.
    category TEXT NOT NULL CHECK (category IN ('triage', 'backlog', 'unstarted', 'started', 'completed', 'canceled')),
    -- Deterministic sort order within the project's workflow board.
    position INTEGER NOT NULL DEFAULT 0,
    -- Hex color code for CLI/UI badges (e.g. "#3B82F6").
    color TEXT NOT NULL DEFAULT '#6B7280',
    -- Boolean flag (0 or 1) marking whether new tasks default into this state.
    is_default INTEGER NOT NULL DEFAULT 0 CHECK (is_default IN (0, 1)),
    -- Creation timestamp in ISO-8601 UTC format.
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    -- Last modification timestamp in ISO-8601 UTC format.
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    -- Soft-delete timestamp in ISO-8601 UTC format; NULL means active.
    deleted_at TEXT DEFAULT NULL
);

-- Prevent duplicate workflow state names inside the same active project.
CREATE UNIQUE INDEX IF NOT EXISTS idx_workflow_states_project_name_active
    ON workflow_states (workspace_id, project_id, name)
    WHERE deleted_at IS NULL;

-- Accelerate board column ordering per project.
CREATE INDEX IF NOT EXISTS idx_workflow_states_project_position_active
    ON workflow_states (workspace_id, project_id, position ASC, created_at ASC)
    WHERE deleted_at IS NULL;

-- =============================================================================
-- 5. LABELS
-- Categorization tags scoped either globally to a workspace (project_id IS NULL)
-- or locally to a single project (project_id IS NOT NULL).
-- =============================================================================
CREATE TABLE IF NOT EXISTS labels (
    -- Globally unique label UUID string.
    id TEXT PRIMARY KEY,
    -- Owning workspace foreign key; cascades physical workspace removal.
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    -- Optional project scope; NULL indicates a workspace-global label shared by all projects.
    project_id TEXT DEFAULT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- Label display name (e.g. "security", "tech-debt", "frontend").
    name TEXT NOT NULL CHECK (length(trim(name)) > 0),
    -- Hex color code for rendering label chips.
    color TEXT NOT NULL DEFAULT '#6B7280',
    -- Optional guidance describing when agents/humans should apply this label.
    description TEXT NOT NULL DEFAULT '',
    -- Creation timestamp in ISO-8601 UTC format.
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    -- Last modification timestamp in ISO-8601 UTC format.
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    -- Soft-delete timestamp in ISO-8601 UTC format; NULL means active.
    deleted_at TEXT DEFAULT NULL
);

-- Guarantee unique label names among active workspace-global labels (project_id IS NULL).
CREATE UNIQUE INDEX IF NOT EXISTS idx_labels_workspace_global_name_active
    ON labels (workspace_id, name)
    WHERE project_id IS NULL AND deleted_at IS NULL;

-- Guarantee unique label names among active project-scoped labels (project_id IS NOT NULL).
CREATE UNIQUE INDEX IF NOT EXISTS idx_labels_project_name_active
    ON labels (workspace_id, project_id, name)
    WHERE project_id IS NOT NULL AND deleted_at IS NULL;

-- =============================================================================
-- 6. OBJECTIVES
-- Hierarchical milestones, timeboxed cycles, or strategic goals within a project.
-- =============================================================================
CREATE TABLE IF NOT EXISTS objectives (
    -- Globally unique objective UUID string.
    id TEXT PRIMARY KEY,
    -- Owning workspace foreign key; cascades physical workspace removal.
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    -- Owning project foreign key; cascades physical project removal.
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- Optional parent objective enabling nested goal -> milestone trees.
    parent_objective_id TEXT DEFAULT NULL REFERENCES objectives(id) ON DELETE RESTRICT,
    -- Concise objective title.
    title TEXT NOT NULL CHECK (length(trim(title)) > 0),
    -- Detailed Markdown specification and success metrics.
    description TEXT NOT NULL DEFAULT '',
    -- Structural classification of the objective.
    kind TEXT NOT NULL CHECK (kind IN ('milestone', 'cycle', 'goal')),
    -- Operational lifecycle status (e.g. 'planned', 'active', 'completed', 'canceled').
    status TEXT NOT NULL DEFAULT 'planned' CHECK (length(trim(status)) > 0),
    -- Optional scheduled start timestamp in ISO-8601 UTC format.
    starts_at TEXT DEFAULT NULL,
    -- Optional target completion timestamp in ISO-8601 UTC format.
    target_date TEXT DEFAULT NULL,
    -- Optional actor who authored the objective.
    created_by_actor_id TEXT DEFAULT NULL REFERENCES actors(id) ON DELETE RESTRICT,
    -- Creation timestamp in ISO-8601 UTC format.
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    -- Last modification timestamp in ISO-8601 UTC format.
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    -- Soft-delete timestamp in ISO-8601 UTC format; NULL means active.
    deleted_at TEXT DEFAULT NULL,
    -- Prevent trivial self-parenting cycles.
    CHECK (parent_objective_id IS NULL OR parent_objective_id <> id)
);

-- Accelerate listing active project objectives by kind and status.
CREATE INDEX IF NOT EXISTS idx_objectives_project_active
    ON objectives (workspace_id, project_id, kind, status)
    WHERE deleted_at IS NULL;

-- Accelerate hierarchical child lookups under a parent objective.
CREATE INDEX IF NOT EXISTS idx_objectives_parent_active
    ON objectives (workspace_id, parent_objective_id)
    WHERE parent_objective_id IS NOT NULL AND deleted_at IS NULL;

-- =============================================================================
-- 7. TASKS
-- Primary units of work (tasks, bugs, features, issues, epics) with built-in
-- time-bounded optimistic lease coordination for autonomous AI agents.
-- =============================================================================
CREATE TABLE IF NOT EXISTS tasks (
    -- Globally unique task UUID string.
    id TEXT PRIMARY KEY,
    -- Owning workspace foreign key; cascades physical workspace removal.
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    -- Owning project foreign key; restricted from accidental physical project deletion when tasks exist.
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    -- Optional milestone/cycle/goal alignment foreign key.
    objective_id TEXT DEFAULT NULL REFERENCES objectives(id) ON DELETE RESTRICT,
    -- Optional parent task/epic foreign key for subtask decomposition.
    parent_task_id TEXT DEFAULT NULL REFERENCES tasks(id) ON DELETE RESTRICT,
    -- Current workflow state column foreign key.
    state_id TEXT NOT NULL REFERENCES workflow_states(id) ON DELETE RESTRICT,
    -- Per-project 1-based monotonic sequence integer (e.g. 42).
    sequence_number INTEGER NOT NULL CHECK (sequence_number > 0),
    -- Canonical human-friendly key combining project prefix and sequence (e.g. "FLW-42").
    identifier TEXT NOT NULL CHECK (length(trim(identifier)) > 0),
    -- Classification of work item semantics.
    kind TEXT NOT NULL DEFAULT 'task' CHECK (kind IN ('task', 'bug', 'feature', 'issue', 'epic')),
    -- Priority level from 0 to 4 (0 = unprioritized, 1 = urgent, 2 = high, 3 = medium, 4 = low).
    priority INTEGER NOT NULL DEFAULT 0 CHECK (priority >= 0 AND priority <= 4),
    -- Concise actionable summary of the work item.
    title TEXT NOT NULL CHECK (length(trim(title)) > 0),
    -- Detailed Markdown context, reproduction steps, or implementation notes.
    description TEXT NOT NULL DEFAULT '',
    -- Explicit verification checklist required before marking the task completed.
    acceptance_criteria TEXT NOT NULL DEFAULT '',
    -- Optional complexity/effort point estimate (>= 0).
    estimate_points INTEGER DEFAULT NULL CHECK (estimate_points IS NULL OR estimate_points >= 0),
    -- Optional actor who reported or created the task.
    reporter_actor_id TEXT DEFAULT NULL REFERENCES actors(id) ON DELETE RESTRICT,
    -- Optional actor designated as owner/assignee of the task.
    assignee_actor_id TEXT DEFAULT NULL REFERENCES actors(id) ON DELETE RESTRICT,
    -- Optional actor currently holding an active execution lease on the task.
    claimed_by_actor_id TEXT DEFAULT NULL REFERENCES actors(id) ON DELETE RESTRICT,
    -- Opaque optimistic concurrency token proving ownership of the active claim lease.
    claim_token TEXT DEFAULT NULL,
    -- ISO-8601 UTC timestamp when the current agent execution lease expires.
    claim_expires_at TEXT DEFAULT NULL,
    -- ISO-8601 UTC timestamp recorded when the task transitions into a completed state.
    completed_at TEXT DEFAULT NULL,
    -- Creation timestamp in ISO-8601 UTC format.
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    -- Last modification timestamp in ISO-8601 UTC format.
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    -- Soft-delete timestamp in ISO-8601 UTC format; NULL means active.
    deleted_at TEXT DEFAULT NULL,
    -- Prevent a task from parenting itself.
    CHECK (parent_task_id IS NULL OR parent_task_id <> id)
);

-- Guarantee globally unique human-readable identifiers (e.g. "FLW-42") per workspace.
CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_workspace_identifier_active
    ON tasks (workspace_id, identifier)
    WHERE deleted_at IS NULL;

-- Guarantee unique monotonic sequence numbers per project.
CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_project_sequence_active
    ON tasks (workspace_id, project_id, sequence_number)
    WHERE deleted_at IS NULL;

-- Accelerate board queries filtering by project, state, and priority.
CREATE INDEX IF NOT EXISTS idx_tasks_project_state_priority_active
    ON tasks (workspace_id, project_id, state_id, priority ASC, sequence_number DESC)
    WHERE deleted_at IS NULL;

-- Accelerate agent work-queue polling and lease expiration scans.
CREATE INDEX IF NOT EXISTS idx_tasks_claim_lease_active
    ON tasks (workspace_id, project_id, claim_expires_at)
    WHERE deleted_at IS NULL;

-- Accelerate assignee workload views.
CREATE INDEX IF NOT EXISTS idx_tasks_assignee_active
    ON tasks (workspace_id, assignee_actor_id)
    WHERE assignee_actor_id IS NOT NULL AND deleted_at IS NULL;

-- Accelerate subtask tree traversal.
CREATE INDEX IF NOT EXISTS idx_tasks_parent_active
    ON tasks (workspace_id, parent_task_id)
    WHERE parent_task_id IS NOT NULL AND deleted_at IS NULL;

-- Accelerate objective progress rollups.
CREATE INDEX IF NOT EXISTS idx_tasks_objective_active
    ON tasks (workspace_id, objective_id)
    WHERE objective_id IS NOT NULL AND deleted_at IS NULL;

-- =============================================================================
-- 8. TASK LABELS
-- Operational many-to-many join table linking tasks and labels (physical delete).
-- =============================================================================
CREATE TABLE IF NOT EXISTS task_labels (
    -- Owning workspace foreign key for tenant isolation.
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    -- Target task foreign key; cascades physical task removal.
    task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    -- Applied label foreign key; cascades physical label removal.
    label_id TEXT NOT NULL REFERENCES labels(id) ON DELETE CASCADE,
    -- Attachment timestamp in ISO-8601 UTC format.
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (workspace_id, task_id, label_id)
);

-- Accelerate reverse lookups of all tasks carrying a specific label.
CREATE INDEX IF NOT EXISTS idx_task_labels_label
    ON task_labels (workspace_id, label_id, task_id);

-- =============================================================================
-- 9. TASK RELATIONS
-- Operational directed dependency and cross-reference edges between tasks
-- (uses physical delete when unlinked).
-- Semantics: for relation_type = 'blocks', source_task_id blocks target_task_id.
-- =============================================================================
CREATE TABLE IF NOT EXISTS task_relations (
    -- Globally unique relation edge UUID string.
    id TEXT PRIMARY KEY,
    -- Owning workspace foreign key; cascades physical workspace removal.
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    -- Origin task in the directed relation (e.g. the blocking task).
    source_task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    -- Destination task in the directed relation (e.g. the blocked task).
    target_task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    -- Edge semantics: 'blocks', 'duplicates', or 'relates_to'.
    relation_type TEXT NOT NULL CHECK (relation_type IN ('blocks', 'duplicates', 'relates_to')),
    -- Optional actor who established the relation.
    created_by_actor_id TEXT DEFAULT NULL REFERENCES actors(id) ON DELETE RESTRICT,
    -- Edge creation timestamp in ISO-8601 UTC format.
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    -- Forbid reflexive self-edges.
    CHECK (source_task_id <> target_task_id)
);

-- Prevent duplicate identical relation edges between the same pair of tasks.
CREATE UNIQUE INDEX IF NOT EXISTS idx_task_relations_unique_edge
    ON task_relations (workspace_id, source_task_id, target_task_id, relation_type);

-- Accelerate incoming dependency checks (e.g. finding active blockers targeting a task).
CREATE INDEX IF NOT EXISTS idx_task_relations_target
    ON task_relations (workspace_id, target_task_id, relation_type);

-- =============================================================================
-- 10. TASK COMMENTS
-- Structured discussion entries, agent blocker reports, handoff notes, and
-- resolution summaries attached to tasks (soft-deletable entity table).
-- =============================================================================
CREATE TABLE IF NOT EXISTS task_comments (
    -- Globally unique comment UUID string.
    id TEXT PRIMARY KEY,
    -- Owning workspace foreign key; cascades physical workspace removal.
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    -- Parent task foreign key; cascades physical task removal.
    task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    -- Authoring human or agent actor foreign key.
    author_actor_id TEXT NOT NULL REFERENCES actors(id) ON DELETE RESTRICT,
    -- Semantic message category enabling structured agent handoffs and blocker escalations.
    kind TEXT NOT NULL DEFAULT 'comment' CHECK (kind IN ('comment', 'blocker_report', 'handoff', 'resolution')),
    -- Markdown comment content.
    body TEXT NOT NULL CHECK (length(trim(body)) > 0),
    -- Structured JSON payload (e.g. commit SHAs, test artifacts, blocker diagnostics).
    metadata_json TEXT NOT NULL DEFAULT '{}',
    -- Creation timestamp in ISO-8601 UTC format.
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    -- Last modification timestamp in ISO-8601 UTC format.
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    -- Soft-delete timestamp in ISO-8601 UTC format; NULL means active.
    deleted_at TEXT DEFAULT NULL
);

-- Accelerate chronological timeline loading of active comments on a task.
CREATE INDEX IF NOT EXISTS idx_task_comments_task_created_active
    ON task_comments (workspace_id, task_id, created_at ASC, id ASC)
    WHERE deleted_at IS NULL;

-- =============================================================================
-- 11. DSL EXECUTION BATCHES
-- Immutable idempotency and execution ledger for batch Flowsk DSL scripts.
-- =============================================================================
CREATE TABLE IF NOT EXISTS dsl_execution_batches (
    -- Globally unique batch execution UUID string.
    id TEXT PRIMARY KEY,
    -- Owning workspace foreign key; cascades physical workspace removal.
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    -- Optional actor who submitted the DSL batch.
    actor_id TEXT DEFAULT NULL REFERENCES actors(id) ON DELETE RESTRICT,
    -- Client-supplied idempotency key preventing duplicate batch re-execution.
    idempotency_key TEXT NOT NULL CHECK (length(trim(idempotency_key)) > 0),
    -- Execution mode (e.g. 'validate', 'dry_run', 'apply').
    mode TEXT NOT NULL CHECK (length(trim(mode)) > 0),
    -- Hex-encoded cryptographic hash (SHA-256) of the normalized DSL script source.
    script_hash TEXT NOT NULL CHECK (length(trim(script_hash)) > 0),
    -- Number of parsed statements executed in the batch.
    statement_count INTEGER NOT NULL DEFAULT 0 CHECK (statement_count >= 0),
    -- Terminal batch status (e.g. 'succeeded', 'failed', 'dry_run').
    status TEXT NOT NULL CHECK (length(trim(status)) > 0),
    -- JSON-encoded execution summary including created/updated entity references and diagnostics.
    summary_json TEXT NOT NULL DEFAULT '{}',
    -- Execution record creation timestamp in ISO-8601 UTC format.
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- Enforce strict per-workspace idempotency key uniqueness.
CREATE UNIQUE INDEX IF NOT EXISTS idx_dsl_execution_batches_idempotency
    ON dsl_execution_batches (workspace_id, idempotency_key);

-- Accelerate recent DSL batch history inspection per workspace.
CREATE INDEX IF NOT EXISTS idx_dsl_execution_batches_workspace_created
    ON dsl_execution_batches (workspace_id, created_at DESC, id DESC);

-- =============================================================================
-- 12. AUDIT LOGS
-- Immutable append-only audit trail capturing every state-mutating operation.
-- =============================================================================
CREATE TABLE IF NOT EXISTS audit_logs (
    -- Globally unique audit event UUID string.
    id TEXT PRIMARY KEY,
    -- Owning workspace foreign key; cascades physical workspace removal.
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    -- Optional actor who performed the mutation (NULL for system bootstrap events).
    actor_id TEXT DEFAULT NULL REFERENCES actors(id) ON DELETE RESTRICT,
    -- Canonical verb describing the mutation (e.g. 'task.created', 'task.claimed', 'project.updated').
    action TEXT NOT NULL CHECK (length(trim(action)) > 0),
    -- Domain entity kind mutated (e.g. 'workspace', 'actor', 'project', 'task', 'objective').
    resource_type TEXT NOT NULL CHECK (length(trim(resource_type)) > 0),
    -- Target entity identifier (UUID string or composite join identifier).
    resource_id TEXT NOT NULL CHECK (length(trim(resource_id)) > 0),
    -- Nullable JSON snapshot of resource fields prior to mutation (NULL on creation).
    old_values_json TEXT DEFAULT NULL,
    -- Nullable JSON snapshot of resource fields after mutation (NULL on physical deletion).
    new_values_json TEXT DEFAULT NULL,
    -- Immutable event timestamp in ISO-8601 UTC format.
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- Accelerate reverse-chronological workspace audit log feeds.
CREATE INDEX IF NOT EXISTS idx_audit_logs_workspace_created
    ON audit_logs (workspace_id, created_at DESC, id DESC);

-- Accelerate entity-specific audit history queries.
CREATE INDEX IF NOT EXISTS idx_audit_logs_workspace_resource
    ON audit_logs (workspace_id, resource_type, resource_id, created_at DESC, id DESC);

-- Accelerate actor activity feeds.
CREATE INDEX IF NOT EXISTS idx_audit_logs_workspace_actor
    ON audit_logs (workspace_id, actor_id, created_at DESC)
    WHERE actor_id IS NOT NULL;

-- +goose Down
-- Tear down all indexes and tables in strict reverse foreign-key dependency order.
DROP INDEX IF EXISTS idx_audit_logs_workspace_actor;
DROP INDEX IF EXISTS idx_audit_logs_workspace_resource;
DROP INDEX IF EXISTS idx_audit_logs_workspace_created;
DROP TABLE IF EXISTS audit_logs;

DROP INDEX IF EXISTS idx_dsl_execution_batches_workspace_created;
DROP INDEX IF EXISTS idx_dsl_execution_batches_idempotency;
DROP TABLE IF EXISTS dsl_execution_batches;

DROP INDEX IF EXISTS idx_task_comments_task_created_active;
DROP TABLE IF EXISTS task_comments;

DROP INDEX IF EXISTS idx_task_relations_target;
DROP INDEX IF EXISTS idx_task_relations_unique_edge;
DROP TABLE IF EXISTS task_relations;

DROP INDEX IF EXISTS idx_task_labels_label;
DROP TABLE IF EXISTS task_labels;

DROP INDEX IF EXISTS idx_tasks_objective_active;
DROP INDEX IF EXISTS idx_tasks_parent_active;
DROP INDEX IF EXISTS idx_tasks_assignee_active;
DROP INDEX IF EXISTS idx_tasks_claim_lease_active;
DROP INDEX IF EXISTS idx_tasks_project_state_priority_active;
DROP INDEX IF EXISTS idx_tasks_project_sequence_active;
DROP INDEX IF EXISTS idx_tasks_workspace_identifier_active;
DROP TABLE IF EXISTS tasks;

DROP INDEX IF EXISTS idx_objectives_parent_active;
DROP INDEX IF EXISTS idx_objectives_project_active;
DROP TABLE IF EXISTS objectives;

DROP INDEX IF EXISTS idx_labels_project_name_active;
DROP INDEX IF EXISTS idx_labels_workspace_global_name_active;
DROP TABLE IF EXISTS labels;

DROP INDEX IF EXISTS idx_workflow_states_project_position_active;
DROP INDEX IF EXISTS idx_workflow_states_project_name_active;
DROP TABLE IF EXISTS workflow_states;

DROP INDEX IF EXISTS idx_projects_workspace_status_active;
DROP INDEX IF EXISTS idx_projects_workspace_key_prefix_active;
DROP INDEX IF EXISTS idx_projects_workspace_slug_active;
DROP TABLE IF EXISTS projects;

DROP INDEX IF EXISTS idx_actors_workspace_kind_active;
DROP INDEX IF EXISTS idx_actors_workspace_handle_active;
DROP TABLE IF EXISTS actors;

DROP INDEX IF EXISTS idx_workspaces_created_active;
DROP INDEX IF EXISTS idx_workspaces_slug_active;
DROP TABLE IF EXISTS workspaces;
