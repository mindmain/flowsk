-- SQLite queries for projects, per-project workflow states, and workspace/project labels.
-- All queries enforce workspace_id tenant isolation and soft-delete safety (deleted_at IS NULL).
-- Reminder: projects NEVER store local user filesystem paths on the server.

-- CreateProject inserts a new project into a workspace and returns the persisted row.
-- name: CreateProject :one
INSERT INTO projects (
    id,
    workspace_id,
    key_prefix,
    slug,
    name,
    description,
    status,
    issue_counter,
    created_by_actor_id
) VALUES (
    sqlc.arg('id'),
    sqlc.arg('workspace_id'),
    sqlc.arg('key_prefix'),
    sqlc.arg('slug'),
    sqlc.arg('name'),
    sqlc.arg('description'),
    sqlc.arg('status'),
    sqlc.arg('issue_counter'),
    sqlc.narg('created_by_actor_id')
)
RETURNING *;

-- GetProjectByID retrieves an active project within a workspace by its UUID string.
-- name: GetProjectByID :one
SELECT *
FROM projects
WHERE workspace_id = sqlc.arg('workspace_id')
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL;

-- GetProjectBySlug retrieves an active project within a workspace by its URL/CLI slug.
-- name: GetProjectBySlug :one
SELECT *
FROM projects
WHERE workspace_id = sqlc.arg('workspace_id')
  AND slug = sqlc.arg('slug')
  AND deleted_at IS NULL;

-- GetProjectByKeyPrefix retrieves an active project within a workspace by its short key prefix (e.g. 'FLW').
-- name: GetProjectByKeyPrefix :one
SELECT *
FROM projects
WHERE workspace_id = sqlc.arg('workspace_id')
  AND key_prefix = sqlc.arg('key_prefix')
  AND deleted_at IS NULL;

-- ListProjectsByWorkspace lists active projects in a workspace with optional lifecycle status filtering.
-- name: ListProjectsByWorkspace :many
SELECT *
FROM projects
WHERE workspace_id = sqlc.arg('workspace_id')
  AND (status = sqlc.narg('status') OR sqlc.narg('status') IS NULL)
  AND deleted_at IS NULL
ORDER BY created_at ASC, id ASC;

-- IncrementProjectIssueCounter atomically increments a project's monotonic task sequence counter
-- and returns the updated project row so callers can mint sequential identifiers (e.g. 'FLW-42').
-- name: IncrementProjectIssueCounter :one
UPDATE projects
SET
    issue_counter = issue_counter + 1,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE workspace_id = sqlc.arg('workspace_id')
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL
RETURNING *;

-- UpdateProject performs an atomic partial update on an active project within a workspace.
-- name: UpdateProject :one
UPDATE projects
SET
    slug = COALESCE(sqlc.narg('slug'), slug),
    name = COALESCE(sqlc.narg('name'), name),
    description = COALESCE(sqlc.narg('description'), description),
    status = COALESCE(sqlc.narg('status'), status),
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE workspace_id = sqlc.arg('workspace_id')
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL
RETURNING *;

-- SoftDeleteProject marks an active project within a workspace as soft-deleted.
-- name: SoftDeleteProject :execrows
UPDATE projects
SET
    deleted_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE workspace_id = sqlc.arg('workspace_id')
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL;

-- CreateWorkflowState inserts a workflow column definition for a project and returns the row.
-- name: CreateWorkflowState :one
INSERT INTO workflow_states (
    id,
    workspace_id,
    project_id,
    name,
    category,
    position,
    color,
    is_default
) VALUES (
    sqlc.arg('id'),
    sqlc.arg('workspace_id'),
    sqlc.arg('project_id'),
    sqlc.arg('name'),
    sqlc.arg('category'),
    sqlc.arg('position'),
    sqlc.arg('color'),
    sqlc.arg('is_default')
)
RETURNING *;

-- GetWorkflowStateByID retrieves an active workflow state scoped to its workspace.
-- name: GetWorkflowStateByID :one
SELECT *
FROM workflow_states
WHERE workspace_id = sqlc.arg('workspace_id')
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL;

-- ListWorkflowStatesByProject returns all active workflow states for a project ordered by board position.
-- name: ListWorkflowStatesByProject :many
SELECT *
FROM workflow_states
WHERE workspace_id = sqlc.arg('workspace_id')
  AND project_id = sqlc.arg('project_id')
  AND deleted_at IS NULL
ORDER BY position ASC, created_at ASC, id ASC;

-- UpdateWorkflowState performs an atomic partial update on an active project workflow state.
-- name: UpdateWorkflowState :one
UPDATE workflow_states
SET
    name = COALESCE(sqlc.narg('name'), name),
    category = COALESCE(sqlc.narg('category'), category),
    position = COALESCE(sqlc.narg('position'), position),
    color = COALESCE(sqlc.narg('color'), color),
    is_default = COALESCE(sqlc.narg('is_default'), is_default),
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE workspace_id = sqlc.arg('workspace_id')
  AND project_id = sqlc.arg('project_id')
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL
RETURNING *;

-- SoftDeleteWorkflowState marks a project workflow state as soft-deleted.
-- name: SoftDeleteWorkflowState :execrows
UPDATE workflow_states
SET
    deleted_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE workspace_id = sqlc.arg('workspace_id')
  AND project_id = sqlc.arg('project_id')
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL;

-- CreateLabel inserts a workspace-global (project_id IS NULL) or project-scoped label.
-- name: CreateLabel :one
INSERT INTO labels (
    id,
    workspace_id,
    project_id,
    name,
    color,
    description
) VALUES (
    sqlc.arg('id'),
    sqlc.arg('workspace_id'),
    sqlc.narg('project_id'),
    sqlc.arg('name'),
    sqlc.arg('color'),
    sqlc.arg('description')
)
RETURNING *;

-- GetLabelByID retrieves an active label within a workspace by its UUID string.
-- name: GetLabelByID :one
SELECT *
FROM labels
WHERE workspace_id = sqlc.arg('workspace_id')
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL;

-- ListLabelsByProject returns active workspace-global labels (project_id IS NULL) together with
-- active project-specific labels matching the optional project_id argument.
-- name: ListLabelsByProject :many
SELECT *
FROM labels
WHERE workspace_id = sqlc.arg('workspace_id')
  AND (project_id IS NULL OR project_id = sqlc.narg('project_id'))
  AND deleted_at IS NULL
ORDER BY name ASC, id ASC;

-- SoftDeleteLabel marks an active label within a workspace (and optional project scope) as soft-deleted.
-- name: SoftDeleteLabel :execrows
UPDATE labels
SET
    deleted_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE workspace_id = sqlc.arg('workspace_id')
  AND (project_id = sqlc.narg('project_id') OR sqlc.narg('project_id') IS NULL)
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL;

-- UpsertProjectBinding creates or updates a parent->child sub-project binding edge within a workspace.
-- name: UpsertProjectBinding :one
INSERT INTO project_bindings (
    id,
    workspace_id,
    parent_project_id,
    child_project_id,
    scope_subpath,
    created_by_actor_id
) VALUES (
    sqlc.arg('id'),
    sqlc.arg('workspace_id'),
    sqlc.arg('parent_project_id'),
    sqlc.arg('child_project_id'),
    sqlc.arg('scope_subpath'),
    sqlc.narg('created_by_actor_id')
)
ON CONFLICT (workspace_id, parent_project_id, child_project_id) DO UPDATE SET
    scope_subpath = excluded.scope_subpath,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
RETURNING *;

-- DeleteProjectBinding removes a single parent->child sub-project binding without deleting either project.
-- name: DeleteProjectBinding :execrows
DELETE FROM project_bindings
WHERE workspace_id = sqlc.arg('workspace_id')
  AND parent_project_id = sqlc.arg('parent_project_id')
  AND child_project_id = sqlc.arg('child_project_id');

-- DeleteProjectBindingsForProject detaches all parent/child binding edges referencing a soft-deleted project
-- so surviving child sub-projects automatically become independent top-level projects.
-- name: DeleteProjectBindingsForProject :exec
DELETE FROM project_bindings
WHERE workspace_id = sqlc.arg('workspace_id')
  AND (parent_project_id = sqlc.arg('project_id') OR child_project_id = sqlc.arg('project_id'));

-- ListProjectBindingsByWorkspace lists all active parent->child project bindings where both endpoints are non-deleted.
-- name: ListProjectBindingsByWorkspace :many
SELECT pb.*
FROM project_bindings pb
INNER JOIN projects parent_p
    ON parent_p.id = pb.parent_project_id
   AND parent_p.workspace_id = pb.workspace_id
   AND parent_p.deleted_at IS NULL
INNER JOIN projects child_p
    ON child_p.id = pb.child_project_id
   AND child_p.workspace_id = pb.workspace_id
   AND child_p.deleted_at IS NULL
WHERE pb.workspace_id = sqlc.arg('workspace_id')
  AND (pb.parent_project_id = sqlc.narg('parent_project_id') OR sqlc.narg('parent_project_id') IS NULL)
  AND (pb.child_project_id = sqlc.narg('child_project_id') OR sqlc.narg('child_project_id') IS NULL)
ORDER BY pb.scope_subpath ASC, pb.created_at ASC, pb.id ASC;

-- SetProjectMarkdownDoc toggles the has_markdown_doc indicator on an active project within a workspace.
-- name: SetProjectMarkdownDoc :one
UPDATE projects
SET
    has_markdown_doc = sqlc.arg('has_markdown_doc'),
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE workspace_id = sqlc.arg('workspace_id')
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL
RETURNING *;
