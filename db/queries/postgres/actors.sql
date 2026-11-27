-- PostgreSQL queries for workspace-scoped human and autonomous agent actors.
-- All queries enforce workspace_id tenant isolation and soft-delete safety (deleted_at IS NULL).

-- CreateActor registers a new human or agent actor inside a workspace and returns the created row.
-- name: CreateActor :one
INSERT INTO actors (
    id,
    workspace_id,
    handle,
    display_name,
    kind,
    harness_hint,
    capabilities_json
) VALUES (
    sqlc.arg('id'),
    sqlc.arg('workspace_id'),
    sqlc.arg('handle'),
    sqlc.arg('display_name'),
    sqlc.arg('kind'),
    sqlc.arg('harness_hint'),
    sqlc.arg('capabilities_json')
)
RETURNING *;

-- GetActorByID retrieves an active actor within a workspace by its UUID.
-- name: GetActorByID :one
SELECT *
FROM actors
WHERE workspace_id = sqlc.arg('workspace_id')
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL;

-- GetActorByHandle retrieves an active actor within a workspace by its unique @handle.
-- name: GetActorByHandle :one
SELECT *
FROM actors
WHERE workspace_id = sqlc.arg('workspace_id')
  AND handle = sqlc.arg('handle')
  AND deleted_at IS NULL;

-- ListActorsByWorkspace lists active actors in a workspace with optional kind filtering ('human' or 'agent').
-- name: ListActorsByWorkspace :many
SELECT *
FROM actors
WHERE workspace_id = sqlc.arg('workspace_id')
  AND (sqlc.narg('kind')::text IS NULL OR kind = sqlc.narg('kind'))
  AND deleted_at IS NULL
ORDER BY handle ASC, id ASC;

-- UpdateActor performs an atomic partial update on an active actor within a workspace.
-- name: UpdateActor :one
UPDATE actors
SET
    handle = COALESCE(sqlc.narg('handle'), handle),
    display_name = COALESCE(sqlc.narg('display_name'), display_name),
    harness_hint = COALESCE(sqlc.narg('harness_hint'), harness_hint),
    capabilities_json = COALESCE(sqlc.narg('capabilities_json'), capabilities_json),
    updated_at = NOW()
WHERE workspace_id = sqlc.arg('workspace_id')
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL
RETURNING *;

-- SoftDeleteActor marks an active actor as soft-deleted within its workspace.
-- name: SoftDeleteActor :execrows
UPDATE actors
SET
    deleted_at = NOW(),
    updated_at = NOW()
WHERE workspace_id = sqlc.arg('workspace_id')
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL;

-- CheckActorIDExists reports whether an actor primary key UUID is already allocated in any workspace.
-- name: CheckActorIDExists :one
SELECT EXISTS (
    SELECT 1
    FROM actors
    WHERE id = sqlc.arg('id')
);
