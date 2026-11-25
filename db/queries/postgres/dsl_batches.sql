-- PostgreSQL queries for the immutable DSL batch execution and idempotency ledger.
-- All queries enforce workspace_id tenant isolation.

-- CreateDSLExecutionBatch records an immutable DSL batch execution outcome keyed by workspace_id and idempotency_key.
-- name: CreateDSLExecutionBatch :one
INSERT INTO dsl_execution_batches (
    id,
    workspace_id,
    actor_id,
    idempotency_key,
    mode,
    script_hash,
    statement_count,
    status,
    summary_json
) VALUES (
    sqlc.arg('id'),
    sqlc.arg('workspace_id'),
    sqlc.narg('actor_id'),
    sqlc.arg('idempotency_key'),
    sqlc.arg('mode'),
    sqlc.arg('script_hash'),
    sqlc.arg('statement_count'),
    sqlc.arg('status'),
    sqlc.arg('summary_json')
)
RETURNING *;

-- GetDSLExecutionBatchByIdempotencyKey looks up a previously recorded DSL batch inside a workspace
-- to guarantee safe idempotent replay without re-mutating domain state.
-- name: GetDSLExecutionBatchByIdempotencyKey :one
SELECT *
FROM dsl_execution_batches
WHERE workspace_id = sqlc.arg('workspace_id')
  AND idempotency_key = sqlc.arg('idempotency_key');

-- ListDSLExecutionBatchesByWorkspace returns recent DSL batch executions for a workspace in reverse chronological order.
-- name: ListDSLExecutionBatchesByWorkspace :many
SELECT *
FROM dsl_execution_batches
WHERE workspace_id = sqlc.arg('workspace_id')
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');
