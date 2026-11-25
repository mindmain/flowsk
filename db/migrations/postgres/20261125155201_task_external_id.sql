-- Migration 20261125155201_task_external_id adds an optional project-scoped
-- external_id column on tasks so autonomous agents and integrations can link
-- external tracker items (e.g., ClickUp, Linear, Jira, GitHub Issues) without
-- colliding with Flowsk internal UUIDs or sequential keys, enforcing uniqueness
-- per (project_id, external_id) across active non-deleted tasks.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE tasks
    ADD COLUMN external_id TEXT;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_project_external_id_unique
    ON tasks (project_id, external_id)
    WHERE external_id IS NOT NULL AND deleted_at IS NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_tasks_project_external_id_unique;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE tasks
    DROP COLUMN external_id;
-- +goose StatementEnd
