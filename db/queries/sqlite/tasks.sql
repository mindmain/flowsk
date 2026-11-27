-- SQLite queries for tasks, optimistic agent leases, task labels, directed task relations, and task comments.
-- Entity tables (tasks, task_comments) strictly enforce soft-delete safety (deleted_at IS NULL).
-- Operational link tables (task_labels, task_relations) use physical deletion while respecting workspace_id scope.

-- CreateTask inserts a new task work item within a project and returns the persisted row.
-- name: CreateTask :one
INSERT INTO tasks (
    id,
    workspace_id,
    project_id,
    objective_id,
    parent_task_id,
    state_id,
    sequence_number,
    identifier,
    external_id,
    kind,
    priority,
    title,
    description,
    acceptance_criteria,
    estimate_points,
    reporter_actor_id,
    assignee_actor_id,
    claimed_by_actor_id,
    claim_token,
    claim_expires_at,
    completed_at,
    last_signal_at
) VALUES (
    sqlc.arg('id'),
    sqlc.arg('workspace_id'),
    sqlc.arg('project_id'),
    sqlc.narg('objective_id'),
    sqlc.narg('parent_task_id'),
    sqlc.arg('state_id'),
    sqlc.arg('sequence_number'),
    sqlc.arg('identifier'),
    sqlc.narg('external_id'),
    sqlc.arg('kind'),
    sqlc.arg('priority'),
    sqlc.arg('title'),
    sqlc.arg('description'),
    sqlc.arg('acceptance_criteria'),
    sqlc.narg('estimate_points'),
    sqlc.narg('reporter_actor_id'),
    sqlc.narg('assignee_actor_id'),
    sqlc.narg('claimed_by_actor_id'),
    sqlc.narg('claim_token'),
    sqlc.narg('claim_expires_at'),
    sqlc.narg('completed_at'),
    sqlc.narg('last_signal_at')
)
RETURNING *;

-- GetTaskByExternalID retrieves a single active task within an active project by its external tracker ID (e.g. 'CU-869abc').
-- name: GetTaskByExternalID :one
SELECT t.*
FROM tasks t
WHERE t.workspace_id = sqlc.arg('workspace_id')
  AND t.project_id = sqlc.arg('project_id')
  AND t.external_id = sqlc.arg('external_id')
  AND t.deleted_at IS NULL
  AND EXISTS (
      SELECT 1
      FROM projects p
      WHERE p.id = t.project_id
        AND p.workspace_id = t.workspace_id
        AND p.deleted_at IS NULL
  );

-- GetTaskByID retrieves a single active task within an active project by its UUID string.
-- name: GetTaskByID :one
SELECT t.*
FROM tasks t
WHERE t.workspace_id = sqlc.arg('workspace_id')
  AND t.id = sqlc.arg('id')
  AND t.deleted_at IS NULL
  AND EXISTS (
      SELECT 1
      FROM projects p
      WHERE p.id = t.project_id
        AND p.workspace_id = t.workspace_id
        AND p.deleted_at IS NULL
  );

-- GetTaskByIdentifier retrieves a single active task within an active project by its human key (e.g. 'FLW-42').
-- name: GetTaskByIdentifier :one
SELECT t.*
FROM tasks t
WHERE t.workspace_id = sqlc.arg('workspace_id')
  AND t.identifier = sqlc.arg('identifier')
  AND t.deleted_at IS NULL
  AND EXISTS (
      SELECT 1
      FROM projects p
      WHERE p.id = t.project_id
        AND p.workspace_id = t.workspace_id
        AND p.deleted_at IS NULL
  );

-- ListTasksByProject lists active tasks in a project with optional filters for state, objective,
-- parent task, assignee, kind, and priority, sorting urgent items (priority 1..4) ahead of unprioritized (0).
-- name: ListTasksByProject :many
SELECT *
FROM tasks
WHERE workspace_id = sqlc.arg('workspace_id')
  AND project_id = sqlc.arg('project_id')
  AND (state_id = sqlc.narg('state_id') OR sqlc.narg('state_id') IS NULL)
  AND (objective_id = sqlc.narg('objective_id') OR sqlc.narg('objective_id') IS NULL)
  AND (parent_task_id = sqlc.narg('parent_task_id') OR sqlc.narg('parent_task_id') IS NULL)
  AND (assignee_actor_id = sqlc.narg('assignee_actor_id') OR sqlc.narg('assignee_actor_id') IS NULL)
  AND (kind = sqlc.narg('kind') OR sqlc.narg('kind') IS NULL)
  AND (priority = sqlc.narg('priority') OR sqlc.narg('priority') IS NULL)
  AND deleted_at IS NULL
ORDER BY
    CASE WHEN priority = 0 THEN 5 ELSE priority END ASC,
    sequence_number DESC,
    id ASC;

-- ListAgentClaimableTasks discovers actionable, unblocked tasks eligible to be claimed by an autonomous agent.
-- Eligibility rules:
-- 1. Active task inside an active project and active workflow state within the workspace.
-- 2. Optional project_id scope and optional actor_id affinity (unassigned OR assigned to actor_id).
-- 3. Workflow state category is 'backlog'/'unstarted', OR 'started' with an expired lease.
-- 4. Task has no active lease (unclaimed or claim_expires_at <= current UTC time via julianday).
-- 5. Task has zero unresolved incoming 'blocks' relations from active non-terminal tasks.
-- name: ListAgentClaimableTasks :many
SELECT t.*
FROM tasks t
JOIN projects p
    ON p.id = t.project_id
   AND p.workspace_id = t.workspace_id
   AND p.deleted_at IS NULL
JOIN workflow_states ws
    ON ws.id = t.state_id
   AND ws.workspace_id = t.workspace_id
   AND ws.deleted_at IS NULL
WHERE t.workspace_id = sqlc.arg('workspace_id')
  AND (t.project_id = sqlc.narg('project_id') OR sqlc.narg('project_id') IS NULL)
  AND (t.assignee_actor_id IS NULL OR t.assignee_actor_id = sqlc.narg('actor_id'))
  AND t.deleted_at IS NULL
  AND (
      ws.category IN ('backlog', 'unstarted')
      OR (
          ws.category = 'started'
          AND (
              t.claimed_by_actor_id IS NULL
              OR t.claim_expires_at IS NULL
              OR julianday(t.claim_expires_at) <= julianday('now')
          )
      )
  )
  AND (
      t.claimed_by_actor_id IS NULL
      OR t.claim_expires_at IS NULL
      OR julianday(t.claim_expires_at) <= julianday('now')
  )
  AND NOT EXISTS (
      SELECT 1
      FROM task_relations tr
      JOIN tasks blocker
          ON blocker.id = tr.source_task_id
         AND blocker.workspace_id = tr.workspace_id
         AND blocker.deleted_at IS NULL
      JOIN workflow_states bws
          ON bws.id = blocker.state_id
         AND bws.workspace_id = blocker.workspace_id
         AND bws.deleted_at IS NULL
      WHERE tr.workspace_id = t.workspace_id
        AND tr.target_task_id = t.id
        AND tr.relation_type = 'blocks'
        AND bws.category NOT IN ('completed', 'canceled')
  )
ORDER BY
    CASE WHEN t.priority = 0 THEN 5 ELSE t.priority END ASC,
    t.sequence_number ASC,
    t.id ASC
LIMIT sqlc.arg('limit');

-- UpdateTask performs an atomic update on mutable metadata fields (including nullable foreign keys) of an active task.
-- name: UpdateTask :one
UPDATE tasks
SET
    objective_id = sqlc.narg('objective_id'),
    parent_task_id = sqlc.narg('parent_task_id'),
    kind = COALESCE(sqlc.narg('kind'), kind),
    priority = COALESCE(sqlc.narg('priority'), priority),
    title = COALESCE(sqlc.narg('title'), title),
    description = COALESCE(sqlc.narg('description'), description),
    acceptance_criteria = COALESCE(sqlc.narg('acceptance_criteria'), acceptance_criteria),
    estimate_points = sqlc.narg('estimate_points'),
    assignee_actor_id = sqlc.narg('assignee_actor_id'),
    last_signal_at = sqlc.narg('last_signal_at'),
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE workspace_id = sqlc.arg('workspace_id')
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL
RETURNING *;

-- TouchTaskLastSignal updates last_signal_at on an active task within a workspace without mutating updated_at.
-- name: TouchTaskLastSignal :one
UPDATE tasks
SET
    last_signal_at = sqlc.narg('last_signal_at')
WHERE workspace_id = sqlc.arg('workspace_id')
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL
RETURNING *;

-- TransitionTaskState atomically moves an active task to a target workflow state and updates completed_at.
-- name: TransitionTaskState :one
UPDATE tasks
SET
    state_id = sqlc.arg('state_id'),
    completed_at = sqlc.narg('completed_at'),
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE workspace_id = sqlc.arg('workspace_id')
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL
RETURNING *;

-- ClaimTaskLease atomically acquires or renews an exclusive execution lease on an active task
-- using optimistic concurrency: succeeds only if unclaimed, already held by actor_id, or expired.
-- Optionally transitions state_id (e.g. into a 'started' workflow state) and defaults assignee_actor_id.
-- name: ClaimTaskLease :one
UPDATE tasks
SET
    claimed_by_actor_id = sqlc.arg('actor_id'),
    claim_token = sqlc.arg('claim_token'),
    claim_expires_at = sqlc.arg('claim_expires_at'),
    assignee_actor_id = COALESCE(assignee_actor_id, sqlc.arg('actor_id')),
    state_id = COALESCE(sqlc.narg('state_id'), state_id),
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE workspace_id = sqlc.arg('workspace_id')
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL
  AND (
      claimed_by_actor_id IS NULL
      OR claimed_by_actor_id = sqlc.arg('actor_id')
      OR claim_expires_at IS NULL
      OR julianday(claim_expires_at) <= julianday('now')
  )
RETURNING *;

-- ReleaseTaskLease clears the active execution lease on a task when matching optional actor_id/claim_token guards.
-- name: ReleaseTaskLease :execrows
UPDATE tasks
SET
    claimed_by_actor_id = NULL,
    claim_token = NULL,
    claim_expires_at = NULL,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE workspace_id = sqlc.arg('workspace_id')
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL
  AND (claimed_by_actor_id = sqlc.narg('actor_id') OR sqlc.narg('actor_id') IS NULL)
  AND (claim_token = sqlc.narg('claim_token') OR sqlc.narg('claim_token') IS NULL);

-- SoftDeleteTask marks an active task as soft-deleted and releases any held execution lease.
-- name: SoftDeleteTask :execrows
UPDATE tasks
SET
    deleted_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
    claimed_by_actor_id = NULL,
    claim_token = NULL,
    claim_expires_at = NULL
WHERE workspace_id = sqlc.arg('workspace_id')
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL;

-- AttachTaskLabel idempotently links a label to a task within a workspace.
-- name: AttachTaskLabel :exec
INSERT INTO task_labels (
    workspace_id,
    task_id,
    label_id
) VALUES (
    sqlc.arg('workspace_id'),
    sqlc.arg('task_id'),
    sqlc.arg('label_id')
)
ON CONFLICT (workspace_id, task_id, label_id) DO NOTHING;

-- DetachTaskLabel physically removes a label link from a task within a workspace.
-- name: DetachTaskLabel :execrows
DELETE FROM task_labels
WHERE workspace_id = sqlc.arg('workspace_id')
  AND task_id = sqlc.arg('task_id')
  AND label_id = sqlc.arg('label_id');

-- ListTaskLabels returns all active labels currently attached to a task within a workspace.
-- name: ListTaskLabels :many
SELECT l.*
FROM labels l
JOIN task_labels tl
    ON tl.label_id = l.id
   AND tl.workspace_id = l.workspace_id
WHERE tl.workspace_id = sqlc.arg('workspace_id')
  AND tl.task_id = sqlc.arg('task_id')
  AND l.deleted_at IS NULL
ORDER BY l.name ASC, l.id ASC;

-- CreateTaskRelation inserts a directed relation edge ('blocks', 'duplicates', 'relates_to') between two tasks.
-- name: CreateTaskRelation :one
INSERT INTO task_relations (
    id,
    workspace_id,
    source_task_id,
    target_task_id,
    relation_type,
    created_by_actor_id
) VALUES (
    sqlc.arg('id'),
    sqlc.arg('workspace_id'),
    sqlc.arg('source_task_id'),
    sqlc.arg('target_task_id'),
    sqlc.arg('relation_type'),
    sqlc.narg('created_by_actor_id')
)
RETURNING *;

-- DeleteTaskRelation physically deletes a task relation edge within a workspace.
-- name: DeleteTaskRelation :execrows
DELETE FROM task_relations
WHERE workspace_id = sqlc.arg('workspace_id')
  AND id = sqlc.arg('id');

-- ListTaskRelations returns all outgoing and incoming relation edges for a task where both endpoints remain active.
-- name: ListTaskRelations :many
SELECT tr.*
FROM task_relations tr
JOIN tasks src
    ON src.id = tr.source_task_id
   AND src.workspace_id = tr.workspace_id
   AND src.deleted_at IS NULL
JOIN tasks tgt
    ON tgt.id = tr.target_task_id
   AND tgt.workspace_id = tr.workspace_id
   AND tgt.deleted_at IS NULL
WHERE tr.workspace_id = sqlc.arg('workspace_id')
  AND (tr.source_task_id = sqlc.arg('task_id') OR tr.target_task_id = sqlc.arg('task_id'))
ORDER BY tr.created_at ASC, tr.id ASC;

-- CreateTaskComment appends a structured comment, blocker report, handoff note, or resolution summary to a task.
-- name: CreateTaskComment :one
INSERT INTO task_comments (
    id,
    workspace_id,
    task_id,
    author_actor_id,
    kind,
    body,
    metadata_json
) VALUES (
    sqlc.arg('id'),
    sqlc.arg('workspace_id'),
    sqlc.arg('task_id'),
    sqlc.arg('author_actor_id'),
    sqlc.arg('kind'),
    sqlc.arg('body'),
    sqlc.arg('metadata_json')
)
RETURNING *;

-- ListTaskComments returns active comments on a task ordered chronologically, with optional kind filtering.
-- name: ListTaskComments :many
SELECT *
FROM task_comments
WHERE workspace_id = sqlc.arg('workspace_id')
  AND task_id = sqlc.arg('task_id')
  AND (kind = sqlc.narg('kind') OR sqlc.narg('kind') IS NULL)
  AND deleted_at IS NULL
ORDER BY created_at ASC, id ASC;

-- SoftDeleteTaskComment marks an active task comment within a workspace as soft-deleted.
-- name: SoftDeleteTaskComment :execrows
UPDATE task_comments
SET
    deleted_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE workspace_id = sqlc.arg('workspace_id')
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL;

-- ListTasksByWorkspace lists all active tasks across every non-deleted project in a workspace.
-- name: ListTasksByWorkspace :many
SELECT t.*
FROM tasks t
JOIN projects p
    ON p.id = t.project_id
   AND p.workspace_id = t.workspace_id
   AND p.deleted_at IS NULL
WHERE t.workspace_id = sqlc.arg('workspace_id')
  AND t.deleted_at IS NULL
ORDER BY
    CASE WHEN t.priority = 0 THEN 5 ELSE t.priority END ASC,
    t.sequence_number DESC,
    t.id ASC;

-- SetTaskMarkdownDoc toggles the has_markdown_doc indicator on an active task within a workspace.
-- name: SetTaskMarkdownDoc :one
UPDATE tasks
SET
    has_markdown_doc = sqlc.arg('has_markdown_doc'),
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE workspace_id = sqlc.arg('workspace_id')
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL
RETURNING *;

-- SetTaskExternalID sets or clears (when NULL) the project-unique external_id on an active task within a workspace.
-- name: SetTaskExternalID :one
UPDATE tasks
SET
    external_id = sqlc.narg('external_id'),
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE workspace_id = sqlc.arg('workspace_id')
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL
RETURNING *;

-- DetachSubtasksFromParent clears parent_task_id on child tasks before physically deleting a parent task.
-- name: DetachSubtasksFromParent :exec
UPDATE tasks
SET
    parent_task_id = NULL,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE workspace_id = sqlc.arg('workspace_id')
  AND parent_task_id = sqlc.arg('parent_task_id');

-- HardDeleteTask permanently deletes a task row (and via ON DELETE CASCADE its labels, relations, and comments)
-- within a workspace when purging archived tasks.
-- name: HardDeleteTask :execrows
DELETE FROM tasks
WHERE workspace_id = sqlc.arg('workspace_id')
  AND id = sqlc.arg('id');

-- SoftDeleteTasksByProject soft-deletes all active tasks belonging to a project and clears their active claim leases.
-- name: SoftDeleteTasksByProject :exec
UPDATE tasks
SET
    deleted_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
    claimed_by_actor_id = NULL,
    claim_token = NULL,
    claim_expires_at = NULL
WHERE workspace_id = sqlc.arg('workspace_id')
  AND project_id = sqlc.arg('project_id')
  AND deleted_at IS NULL;

-- CheckTaskSequenceOrIdentifierConflict reports whether an active task already uses sequence_number in project_id or identifier in workspace_id.
-- name: CheckTaskSequenceOrIdentifierConflict :one
SELECT EXISTS (
    SELECT 1
    FROM tasks
    WHERE workspace_id = sqlc.arg('workspace_id')
      AND deleted_at IS NULL
      AND (
          (project_id = sqlc.arg('project_id') AND sequence_number = sqlc.arg('sequence_number'))
          OR identifier = sqlc.arg('identifier')
      )
);
