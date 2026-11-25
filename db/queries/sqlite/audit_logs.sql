-- SQLite queries for the immutable append-only workspace audit log.
-- Audit log records are never updated or deleted; all reads scope strictly by workspace_id.

-- InsertAuditLog appends an immutable audit event capturing old/new JSON snapshots of a domain mutation.
-- name: InsertAuditLog :one
INSERT INTO audit_logs (
    id,
    workspace_id,
    actor_id,
    action,
    resource_type,
    resource_id,
    old_values_json,
    new_values_json
) VALUES (
    sqlc.arg('id'),
    sqlc.arg('workspace_id'),
    sqlc.narg('actor_id'),
    sqlc.arg('action'),
    sqlc.arg('resource_type'),
    sqlc.arg('resource_id'),
    sqlc.narg('old_values_json'),
    sqlc.narg('new_values_json')
)
RETURNING *;

-- GetAuditLogByID retrieves a single immutable audit event within a workspace.
-- name: GetAuditLogByID :one
SELECT *
FROM audit_logs
WHERE workspace_id = sqlc.arg('workspace_id')
  AND id = sqlc.arg('id');

-- ListAuditLogsByWorkspace returns paginated workspace audit events ordered newest-first.
-- name: ListAuditLogsByWorkspace :many
SELECT *
FROM audit_logs
WHERE workspace_id = sqlc.arg('workspace_id')
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- ListAuditLogsByResource returns paginated audit history for a specific domain entity within a workspace.
-- name: ListAuditLogsByResource :many
SELECT *
FROM audit_logs
WHERE workspace_id = sqlc.arg('workspace_id')
  AND resource_type = sqlc.arg('resource_type')
  AND resource_id = sqlc.arg('resource_id')
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');
