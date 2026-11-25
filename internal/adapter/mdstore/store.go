// Package mdstore implements the host-local Markdown document store rooted inside
// the global Flowsk home directory ($FLOWSK_HOME, defaulting to ~/.flowsk).
//
// Directory Layout & Security Invariants:
//   - Sibling Collections: Project and Task Markdown documents live at the same
//     level under `<rootDir>/projects/<workspace_id>/<project_id>.md` and
//     `<rootDir>/tasks/<workspace_id>/<task_id>.md`.
//   - Multi-Tenant Isolation: Every read, write, delete, and orphan scan is
//     strictly namespaced by `workspace_id` so tenants sharing a Flowsk home
//     never read, overwrite, or prune one another's Markdown resources.
//   - Zero Path Traversal & Zero Host Path Exposure: Callers supply strictly
//     validated `uuid.UUID` identifiers (or confined relative orphan keys during
//     pruning); absolute host filesystem paths are never returned across domain
//     or network transport boundaries.
package mdstore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/domain/errs"
)

const (
	// ProjectsSubdir is the top-level subdirectory under $FLOWSK_HOME storing project Markdown documents.
	ProjectsSubdir = "projects"

	// TasksSubdir is the top-level subdirectory under $FLOWSK_HOME storing task Markdown documents.
	TasksSubdir = "tasks"

	markdownExt = ".md"
)

// WorkspaceScan describes the on-disk Markdown files discovered inside one
// collection (`tasks` or `projects`) for a specific workspace.
type WorkspaceScan struct {
	// ValidIDs lists canonical entity UUIDs corresponding to `<uuid>.md` files in the workspace folder.
	ValidIDs []uuid.UUID

	// MalformedRelKeys lists logical relative filenames (within the collection) of `.md` files
	// whose stems are not valid UUIDs and are therefore unindexed orphans.
	MalformedRelKeys []string
}

// FileStore persists optional extended Markdown documents for Tasks and Projects
// inside `<rootDir>/tasks` and `<rootDir>/projects`.
type FileStore struct {
	rootDir     string
	projectsDir string
	tasksDir    string
}

// NewFileStore initializes a FileStore rooted at rootDir (typically `~/.flowsk`),
// ensuring both sibling `projects/` and `tasks/` directories exist with 0700 permissions.
func NewFileStore(rootDir string) (*FileStore, error) {
	const op = "mdstore.NewFileStore"
	trimmed := strings.TrimSpace(rootDir)
	if trimmed == "" {
		return nil, errs.Validation(op, "markdown store root directory must not be empty", errs.FieldViolation{
			Field:       "root_dir",
			Description: "root_dir must be a non-empty directory path",
		})
	}
	absRoot, err := filepath.Abs(filepath.Clean(trimmed))
	if err != nil {
		return nil, fmt.Errorf("%s: resolve root dir: %w", op, err)
	}

	projectsDir := filepath.Join(absRoot, ProjectsSubdir)
	tasksDir := filepath.Join(absRoot, TasksSubdir)
	for _, dir := range []string{absRoot, projectsDir, tasksDir} {
		if mkErr := os.MkdirAll(dir, 0o700); mkErr != nil {
			return nil, fmt.Errorf("%s: create directory: %w", op, mkErr)
		}
	}

	return &FileStore{
		rootDir:     absRoot,
		projectsDir: projectsDir,
		tasksDir:    tasksDir,
	}, nil
}

// RootDir returns the cleaned root directory managed by this store (for local diagnostics only).
func (s *FileStore) RootDir() string {
	if s == nil {
		return ""
	}
	return s.rootDir
}

// ReadTaskMarkdown reads the Markdown document for (workspaceID, taskID).
// Returns errs.ErrNotFound if no `.md` resource exists on disk for the task.
func (s *FileStore) ReadTaskMarkdown(ctx context.Context, workspaceID, taskID uuid.UUID) (string, error) {
	return s.readEntityMarkdown(ctx, "mdstore.ReadTaskMarkdown", s.tasksDir, workspaceID, taskID)
}

// WriteTaskMarkdown atomically writes markdown to `<rootDir>/tasks/<workspaceID>/<taskID>.md`
// with 0600 permissions.
func (s *FileStore) WriteTaskMarkdown(ctx context.Context, workspaceID, taskID uuid.UUID, markdown string) error {
	return s.writeEntityMarkdown(ctx, "mdstore.WriteTaskMarkdown", s.tasksDir, workspaceID, taskID, markdown)
}

// DeleteTaskMarkdown deletes `<rootDir>/tasks/<workspaceID>/<taskID>.md` if present
// (idempotent when already absent).
func (s *FileStore) DeleteTaskMarkdown(ctx context.Context, workspaceID, taskID uuid.UUID) error {
	return s.deleteEntityMarkdown(ctx, "mdstore.DeleteTaskMarkdown", s.tasksDir, workspaceID, taskID)
}

// HasTaskMarkdown reports whether `<rootDir>/tasks/<workspaceID>/<taskID>.md` exists on disk.
func (s *FileStore) HasTaskMarkdown(ctx context.Context, workspaceID, taskID uuid.UUID) (bool, error) {
	return s.hasEntityMarkdown(ctx, "mdstore.HasTaskMarkdown", s.tasksDir, workspaceID, taskID)
}

// ScanTaskMarkdown inspects `<rootDir>/tasks/<workspaceID>` (plus any stray top-level
// `.md` files dropped directly under `<rootDir>/tasks`) and returns discovered UUIDs
// and malformed relative filenames.
func (s *FileStore) ScanTaskMarkdown(ctx context.Context, workspaceID uuid.UUID) (WorkspaceScan, error) {
	return s.scanWorkspaceCollection(ctx, "mdstore.ScanTaskMarkdown", s.tasksDir, workspaceID)
}

// RemoveMalformedTaskMarkdown removes a malformed relative `.md` file discovered by ScanTaskMarkdown,
// strictly confined within `<rootDir>/tasks`.
func (s *FileStore) RemoveMalformedTaskMarkdown(ctx context.Context, relKey string) error {
	return s.removeConfinedRelativeFile(ctx, "mdstore.RemoveMalformedTaskMarkdown", s.tasksDir, relKey)
}

// ReadProjectMarkdown reads the Markdown document for (workspaceID, projectID).
// Returns errs.ErrNotFound if no `.md` resource exists on disk for the project.
func (s *FileStore) ReadProjectMarkdown(ctx context.Context, workspaceID, projectID uuid.UUID) (string, error) {
	return s.readEntityMarkdown(ctx, "mdstore.ReadProjectMarkdown", s.projectsDir, workspaceID, projectID)
}

// WriteProjectMarkdown atomically writes markdown to `<rootDir>/projects/<workspaceID>/<projectID>.md`
// with 0600 permissions.
func (s *FileStore) WriteProjectMarkdown(ctx context.Context, workspaceID, projectID uuid.UUID, markdown string) error {
	return s.writeEntityMarkdown(ctx, "mdstore.WriteProjectMarkdown", s.projectsDir, workspaceID, projectID, markdown)
}

// DeleteProjectMarkdown deletes `<rootDir>/projects/<workspaceID>/<projectID>.md` if present.
func (s *FileStore) DeleteProjectMarkdown(ctx context.Context, workspaceID, projectID uuid.UUID) error {
	return s.deleteEntityMarkdown(ctx, "mdstore.DeleteProjectMarkdown", s.projectsDir, workspaceID, projectID)
}

// HasProjectMarkdown reports whether `<rootDir>/projects/<workspaceID>/<projectID>.md` exists on disk.
func (s *FileStore) HasProjectMarkdown(ctx context.Context, workspaceID, projectID uuid.UUID) (bool, error) {
	return s.hasEntityMarkdown(ctx, "mdstore.HasProjectMarkdown", s.projectsDir, workspaceID, projectID)
}

// ScanProjectMarkdown inspects `<rootDir>/projects/<workspaceID>` (plus stray top-level
// `.md` files directly under `<rootDir>/projects`) and returns discovered UUIDs and
// malformed relative filenames.
func (s *FileStore) ScanProjectMarkdown(ctx context.Context, workspaceID uuid.UUID) (WorkspaceScan, error) {
	return s.scanWorkspaceCollection(ctx, "mdstore.ScanProjectMarkdown", s.projectsDir, workspaceID)
}

// RemoveMalformedProjectMarkdown removes a malformed relative `.md` file discovered by ScanProjectMarkdown,
// strictly confined within `<rootDir>/projects`.
func (s *FileStore) RemoveMalformedProjectMarkdown(ctx context.Context, relKey string) error {
	return s.removeConfinedRelativeFile(ctx, "mdstore.RemoveMalformedProjectMarkdown", s.projectsDir, relKey)
}

// ScanTaskFiles exposes ScanTaskMarkdown results using primitive slices so pure
// domain ports satisfy structural typing without importing adapter types.
func (s *FileStore) ScanTaskFiles(ctx context.Context, workspaceID uuid.UUID) ([]uuid.UUID, []string, error) {
	scan, err := s.ScanTaskMarkdown(ctx, workspaceID)
	if err != nil {
		return nil, nil, err
	}
	return scan.ValidIDs, scan.MalformedRelKeys, nil
}

// ScanProjectFiles exposes ScanProjectMarkdown results using primitive slices so
// pure domain ports satisfy structural typing without importing adapter types.
func (s *FileStore) ScanProjectFiles(ctx context.Context, workspaceID uuid.UUID) ([]uuid.UUID, []string, error) {
	scan, err := s.ScanProjectMarkdown(ctx, workspaceID)
	if err != nil {
		return nil, nil, err
	}
	return scan.ValidIDs, scan.MalformedRelKeys, nil
}

func (s *FileStore) entityFilePath(op, baseDir string, workspaceID, entityID uuid.UUID) (string, error) {
	if s == nil || baseDir == "" {
		return "", errs.New(errs.ErrPreconditionFailed, op, "markdown file store is not initialized")
	}
	if workspaceID == uuid.Nil {
		return "", errs.Validation(op, "workspace_id is required", errs.FieldViolation{
			Field:       "workspace_id",
			Description: "workspace_id must be a non-nil UUID",
		})
	}
	if entityID == uuid.Nil {
		return "", errs.Validation(op, "entity_id is required", errs.FieldViolation{
			Field:       "entity_id",
			Description: "entity_id must be a non-nil UUID",
		})
	}
	target := filepath.Join(baseDir, workspaceID.String(), entityID.String()+markdownExt)
	if !isStrictDescendant(baseDir, target) {
		return "", errs.Validation(op, "resolved markdown path escaped collection root")
	}
	return target, nil
}

func (s *FileStore) readEntityMarkdown(ctx context.Context, op, baseDir string, workspaceID, entityID uuid.UUID) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	target, err := s.entityFilePath(op, baseDir, workspaceID, entityID)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(target) // #nosec G304 -- strictly constructed from validated non-nil UUIDs inside baseDir
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", errs.New(errs.ErrNotFound, op, "markdown document does not exist")
		}
		return "", fmt.Errorf("%s: read markdown resource: %w", op, err)
	}
	return string(raw), nil
}

func (s *FileStore) writeEntityMarkdown(ctx context.Context, op, baseDir string, workspaceID, entityID uuid.UUID, markdown string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	target, err := s.entityFilePath(op, baseDir, workspaceID, entityID)
	if err != nil {
		return err
	}
	wsDir := filepath.Dir(target)
	if err := os.MkdirAll(wsDir, 0o700); err != nil {
		return fmt.Errorf("%s: ensure workspace markdown dir: %w", op, err)
	}

	tmp, err := os.CreateTemp(wsDir, ".flowsk-md-*")
	if err != nil {
		return fmt.Errorf("%s: create temp markdown file: %w", op, err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("%s: chmod temp markdown file: %w", op, err)
	}
	if _, err := tmp.WriteString(markdown); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("%s: write temp markdown file: %w", op, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("%s: sync temp markdown file: %w", op, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("%s: close temp markdown file: %w", op, err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("%s: rename temp markdown file: %w", op, err)
	}
	cleanup = false
	return nil
}

func (s *FileStore) deleteEntityMarkdown(ctx context.Context, op, baseDir string, workspaceID, entityID uuid.UUID) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	target, err := s.entityFilePath(op, baseDir, workspaceID, entityID)
	if err != nil {
		return err
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: remove markdown resource: %w", op, err)
	}
	return nil
}

func (s *FileStore) hasEntityMarkdown(ctx context.Context, op, baseDir string, workspaceID, entityID uuid.UUID) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("%s: %w", op, err)
	}
	target, err := s.entityFilePath(op, baseDir, workspaceID, entityID)
	if err != nil {
		return false, err
	}
	info, err := os.Stat(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("%s: stat markdown resource: %w", op, err)
	}
	return !info.IsDir(), nil
}

func (s *FileStore) scanWorkspaceCollection(ctx context.Context, op, baseDir string, workspaceID uuid.UUID) (WorkspaceScan, error) {
	if err := ctx.Err(); err != nil {
		return WorkspaceScan{}, fmt.Errorf("%s: %w", op, err)
	}
	if s == nil || baseDir == "" {
		return WorkspaceScan{}, errs.New(errs.ErrPreconditionFailed, op, "markdown file store is not initialized")
	}
	if workspaceID == uuid.Nil {
		return WorkspaceScan{}, errs.Validation(op, "workspace_id is required", errs.FieldViolation{
			Field:       "workspace_id",
			Description: "workspace_id must be a non-nil UUID",
		})
	}

	var out WorkspaceScan

	// 1. Check stray top-level `.md` files placed directly under `<rootDir>/{tasks,projects}/`
	// (outside any workspace UUID directory).
	if topEntries, topErr := os.ReadDir(baseDir); topErr == nil {
		for _, entry := range topEntries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if strings.HasSuffix(strings.ToLower(name), markdownExt) && !strings.HasPrefix(name, ".flowsk-") {
				out.MalformedRelKeys = append(out.MalformedRelKeys, name)
			}
		}
	}

	// 2. Scan `<rootDir>/{tasks,projects}/<workspaceID>/*.md`.
	wsSubdir := workspaceID.String()
	wsDir := filepath.Join(baseDir, wsSubdir)
	entries, err := os.ReadDir(wsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			slices.Sort(out.MalformedRelKeys)
			return out, nil
		}
		return WorkspaceScan{}, fmt.Errorf("%s: read workspace markdown dir: %w", op, err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".flowsk-") || !strings.HasSuffix(strings.ToLower(name), markdownExt) {
			continue
		}
		stem := name[:len(name)-len(markdownExt)]
		parsed, parseErr := uuid.Parse(stem)
		if parseErr != nil || parsed == uuid.Nil {
			out.MalformedRelKeys = append(out.MalformedRelKeys, wsSubdir+"/"+name)
			continue
		}
		out.ValidIDs = append(out.ValidIDs, parsed)
	}

	slices.SortFunc(out.ValidIDs, func(a, b uuid.UUID) int {
		return strings.Compare(a.String(), b.String())
	})
	slices.Sort(out.MalformedRelKeys)
	return out, nil
}

func (s *FileStore) removeConfinedRelativeFile(ctx context.Context, op, baseDir, relKey string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if s == nil || baseDir == "" {
		return errs.New(errs.ErrPreconditionFailed, op, "markdown file store is not initialized")
	}
	trimmed := strings.TrimSpace(relKey)
	if trimmed == "" || !strings.HasSuffix(strings.ToLower(trimmed), markdownExt) {
		return errs.Validation(op, "relative markdown key must end with .md")
	}
	if filepath.IsAbs(trimmed) || strings.Contains(trimmed, `\`) || strings.Contains(trimmed, "..") {
		return errs.Validation(op, "relative markdown key must not contain path traversal segments")
	}
	target := filepath.Join(baseDir, filepath.FromSlash(trimmed))
	if !isStrictDescendant(baseDir, target) {
		return errs.Validation(op, "relative markdown key escaped collection directory")
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: remove orphan markdown file: %w", op, err)
	}
	return nil
}

func isStrictDescendant(baseDir, candidate string) bool {
	rel, err := filepath.Rel(filepath.Clean(baseDir), filepath.Clean(candidate))
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
