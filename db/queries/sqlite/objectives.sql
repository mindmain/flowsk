-- SQLite queries for project objectives (milestones, cycles, and strategic goals).
-- All queries enforce workspace_id tenant isolation and soft-delete safety (deleted_at IS NULL).

-- CreateObjective inserts a new objective within a project and returns the persisted row.
-- name: CreateObjective :one
INSERT INTO objectives (
    id,
    workspace_id,
    project_id,
    parent_objective_id,
    title,
    description,
    kind,
    status,
    starts_at,
    target_date,
    created_by_actor_id
) VALUES (
    sqlc.arg('id'),
    sqlc.arg('workspace_id'),
    sqlc.arg('project_id'),
    sqlc.narg('parent_objective_id'),
    sqlc.arg('title'),
    sqlc.arg('description'),
    sqlc.arg('kind'),
    sqlc.arg('status'),
    sqlc.narg('starts_at'),
    sqlc.narg('target_date'),
    sqlc.narg('created_by_actor_id')
)
RETURNING *;

-- GetObjectiveByID retrieves an active objective within a workspace by its UUID string.
-- name: GetObjectiveByID :one
SELECT *
FROM objectives
WHERE workspace_id = sqlc.arg('workspace_id')
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL;

-- ListObjectivesByProject lists active objectives for a project with optional kind and status filters,
-- sorting objectives with explicit target dates ahead of undated objectives.
-- name: ListObjectivesByProject :many
SELECT *
FROM objectives
WHERE workspace_id = sqlc.arg('workspace_id')
  AND project_id = sqlc.arg('project_id')
  AND (kind = sqlc.narg('kind') OR sqlc.narg('kind') IS NULL)
  AND (status = sqlc.narg('status') OR sqlc.narg('status') IS NULL)
  AND deleted_at IS NULL
ORDER BY
    CASE WHEN target_date IS NULL THEN 1 ELSE 0 END ASC,
    target_date ASC,
    created_at ASC,
    id ASC;

-- UpdateObjective performs an atomic partial update on an active objective within a workspace.
-- name: UpdateObjective :one
UPDATE objectives
SET
    parent_objective_id = COALESCE(sqlc.narg('parent_objective_id'), parent_objective_id),
    title = COALESCE(sqlc.narg('title'), title),
    description = COALESCE(sqlc.narg('description'), description),
    kind = COALESCE(sqlc.narg('kind'), kind),
    status = COALESCE(sqlc.narg('status'), status),
    starts_at = COALESCE(sqlc.narg('starts_at'), starts_at),
    target_date = COALESCE(sqlc.narg('target_date'), target_date),
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE workspace_id = sqlc.arg('workspace_id')
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL
RETURNING *;

-- SoftDeleteObjective marks an active objective within a workspace as soft-deleted.
-- name: SoftDeleteObjective :execrows
UPDATE objectives
SET
    deleted_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE workspace_id = sqlc.arg('workspace_id')
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL;
