-- PostgreSQL queries for project objectives (milestones, cycles, and strategic goals).
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

-- GetObjectiveByID retrieves an active objective within an active project by its UUID.
-- name: GetObjectiveByID :one
SELECT o.*
FROM objectives o
WHERE o.workspace_id = sqlc.arg('workspace_id')
  AND o.id = sqlc.arg('id')
  AND o.deleted_at IS NULL
  AND EXISTS (
      SELECT 1
      FROM projects p
      WHERE p.id = o.project_id
        AND p.workspace_id = o.workspace_id
        AND p.deleted_at IS NULL
  );

-- ListObjectivesByProject lists active objectives for a project with optional kind and status filters,
-- sorting objectives with explicit target dates ahead of undated objectives.
-- name: ListObjectivesByProject :many
SELECT *
FROM objectives
WHERE workspace_id = sqlc.arg('workspace_id')
  AND project_id = sqlc.arg('project_id')
  AND (sqlc.narg('kind')::text IS NULL OR kind = sqlc.narg('kind'))
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status'))
  AND deleted_at IS NULL
ORDER BY
    CASE WHEN target_date IS NULL THEN 1 ELSE 0 END ASC,
    target_date ASC,
    created_at ASC,
    id ASC;

-- UpdateObjective performs an atomic update on an active objective (including nullable dates and parent_objective_id).
-- name: UpdateObjective :one
UPDATE objectives
SET
    parent_objective_id = sqlc.narg('parent_objective_id'),
    title = COALESCE(sqlc.narg('title'), title),
    description = COALESCE(sqlc.narg('description'), description),
    kind = COALESCE(sqlc.narg('kind'), kind),
    status = COALESCE(sqlc.narg('status'), status),
    starts_at = sqlc.narg('starts_at'),
    target_date = sqlc.narg('target_date'),
    updated_at = NOW()
WHERE workspace_id = sqlc.arg('workspace_id')
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL
RETURNING *;

-- SoftDeleteObjective marks an active objective within a workspace as soft-deleted.
-- name: SoftDeleteObjective :execrows
UPDATE objectives
SET
    deleted_at = NOW(),
    updated_at = NOW()
WHERE workspace_id = sqlc.arg('workspace_id')
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL;

-- SoftDeleteObjectivesByProject soft-deletes all active objectives belonging to a project.
-- name: SoftDeleteObjectivesByProject :exec
UPDATE objectives
SET
    deleted_at = NOW(),
    updated_at = NOW()
WHERE workspace_id = sqlc.arg('workspace_id')
  AND project_id = sqlc.arg('project_id')
  AND deleted_at IS NULL;
