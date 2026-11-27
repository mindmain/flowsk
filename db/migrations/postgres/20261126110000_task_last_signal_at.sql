-- Migration 20261126110000_task_last_signal_at adds an optional nullable
-- last_signal_at timestamp column (defaulting to NULL) on tasks to record the
-- most recent agent/CLI/MCP activity signal (creation, point-read, update, lease,
-- comment, doc change, or state transition up to in_review) powering live UI indicators.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE tasks
    ADD COLUMN last_signal_at TIMESTAMPTZ DEFAULT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE tasks
    DROP COLUMN last_signal_at;
-- +goose StatementEnd
