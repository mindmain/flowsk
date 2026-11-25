-- Migration 20261125150000_archived_and_markdown_docs adds boolean indicators
-- tracking optional Markdown resource documents stored in Flowsk's global home
-- directory (`~/.flowsk/projects/<workspace_id>/<project_id>.md` and
-- `~/.flowsk/tasks/<workspace_id>/<task_id>.md`) for orphan detection and pruning.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE projects
    ADD COLUMN has_markdown_doc BOOLEAN NOT NULL DEFAULT FALSE;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE tasks
    ADD COLUMN has_markdown_doc BOOLEAN NOT NULL DEFAULT FALSE;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE tasks DROP COLUMN has_markdown_doc;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE projects DROP COLUMN has_markdown_doc;
-- +goose StatementEnd
