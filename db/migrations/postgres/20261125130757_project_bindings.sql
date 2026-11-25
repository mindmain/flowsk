-- +goose Up
-- Multi-binding directed acyclic graph (DAG) edges linking macro/parent projects
-- to child sub-projects within the same workspace.
--
-- Architectural & Safety Invariants:
-- 1. Unbinding or deleting a parent project removes ONLY the row in project_bindings;
--    child projects in `projects` (and all their goals/tasks) are NEVER deleted.
-- 2. scope_subpath stores ONLY an optional relative module identifier (e.g. 'apps/web'
--    or 'services/auth'), NEVER an absolute host filesystem path ('/Users/...').

CREATE TABLE IF NOT EXISTS project_bindings (
    -- Globally unique binding edge UUID.
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Owning workspace foreign key; cascades physical workspace removal.
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    -- Macro / parent project UUID.
    parent_project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- Nested / child sub-project UUID.
    child_project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- Optional relative subpath inside the parent repository (e.g. 'apps/web'); never a host path.
    scope_subpath TEXT NOT NULL DEFAULT '',
    -- Optional actor who established the binding.
    created_by_actor_id UUID DEFAULT NULL REFERENCES actors(id) ON DELETE SET NULL,
    -- Creation timestamp in UTC-normalized TIMESTAMPTZ.
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Last modification timestamp in UTC-normalized TIMESTAMPTZ.
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Forbid reflexive self-bindings.
    CHECK (parent_project_id <> child_project_id)
);

-- Prevent duplicate parent->child bindings for the same pair of projects.
CREATE UNIQUE INDEX IF NOT EXISTS idx_project_bindings_unique_edge
    ON project_bindings (workspace_id, parent_project_id, child_project_id);

-- Accelerate listing all sub-projects bound under a parent project.
CREATE INDEX IF NOT EXISTS idx_project_bindings_parent
    ON project_bindings (workspace_id, parent_project_id);

-- Accelerate reverse lookups of all macro projects referencing a sub-project.
CREATE INDEX IF NOT EXISTS idx_project_bindings_child
    ON project_bindings (workspace_id, child_project_id);

-- +goose Down
DROP INDEX IF EXISTS idx_project_bindings_child;
DROP INDEX IF EXISTS idx_project_bindings_parent;
DROP INDEX IF EXISTS idx_project_bindings_unique_edge;
DROP TABLE IF EXISTS project_bindings;
