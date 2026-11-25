-- PostgreSQL queries for top-level workspaces.
-- All read and update operations strictly enforce soft-delete filtering (deleted_at IS NULL).

-- CreateWorkspace inserts a new active workspace and returns the persisted row.
-- name: CreateWorkspace :one
INSERT INTO workspaces (
    id,
    slug,
    name,
    description
) VALUES (
    sqlc.arg('id'),
    sqlc.arg('slug'),
    sqlc.arg('name'),
    sqlc.arg('description')
)
RETURNING *;

-- GetWorkspaceByID retrieves a single active workspace by its UUID.
-- name: GetWorkspaceByID :one
SELECT *
FROM workspaces
WHERE id = sqlc.arg('id')
  AND deleted_at IS NULL;

-- GetWorkspaceBySlug retrieves a single active workspace by its unique URL/CLI slug.
-- name: GetWorkspaceBySlug :one
SELECT *
FROM workspaces
WHERE slug = sqlc.arg('slug')
  AND deleted_at IS NULL;

-- ListWorkspaces returns all non-deleted workspaces ordered deterministically by creation time.
-- name: ListWorkspaces :many
SELECT *
FROM workspaces
WHERE deleted_at IS NULL
ORDER BY created_at ASC, id ASC;

-- UpdateWorkspace performs an atomic partial update on an active workspace and returns the updated row.
-- name: UpdateWorkspace :one
UPDATE workspaces
SET
    slug = COALESCE(sqlc.narg('slug'), slug),
    name = COALESCE(sqlc.narg('name'), name),
    description = COALESCE(sqlc.narg('description'), description),
    updated_at = NOW()
WHERE id = sqlc.arg('id')
  AND deleted_at IS NULL
RETURNING *;

-- SoftDeleteWorkspace marks an active workspace as soft-deleted and returns the affected row count.
-- name: SoftDeleteWorkspace :execrows
UPDATE workspaces
SET
    deleted_at = NOW(),
    updated_at = NOW()
WHERE id = sqlc.arg('id')
  AND deleted_at IS NULL;
