// Package localpath provides the strictly client-local registry mapping a developer's
// or agent sandbox's local checkout directory to a logical Flowsk `(WorkspaceSlug, ProjectID)`.
//
// CRITICAL PRIVACY, PORTABILITY & REPOSITORY CLEANLINESS INVARIANTS:
//  1. Client-Machine Exclusive Execution: This adapter executes EXCLUSIVELY inside
//     client-side binaries (`flowsk` CLI commands and local client-side MCP stdio bridges).
//     It is NEVER imported, mounted, or invoked by the Flowsk server or domain layer.
//  2. Zero Git Repository Pollution: Bindings are stored in a single user-level config
//     registry file inside the OS user configuration directory (for example,
//     `~/.config/flowsk/local_paths.json` on Linux/macOS or `%AppData%\flowsk\local_paths.json`
//     on Windows). Flowsk NEVER creates `.flowsk/` dotfiles, `.gitignore` entries, or
//     metadata files inside the target project's git working tree.
//  3. Zero Server Transmission of Host Paths: `LocalAbsPath` NEVER leaves the local
//     machine over ConnectRPC or HTTP. Once `ResolveByWorkingDir` resolves `/Users/.../repo`
//     (or any subdirectory inside it) to `(WorkspaceSlug, ProjectID, ProjectSlug)`, only
//     the canonical `WorkspaceID`/`ProjectID`/`Slug` identifiers are sent to the Flowsk
//     server. Consequently, Mario can keep `billing-api` in `/Users/mario/Desktop/billing-api`,
//     Paolo can keep the exact same project in `/Users/paolo/Downloads/work/billing`, and
//     a containerized agent can mount it at `/workspace/billing-api` with zero conflicts!
package localpath

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/domain/errs"
)

// registryLocks coordinates concurrent goroutine read-modify-write access even
// when separate FileRegistry instances point to the same canonical registry file.
var registryLocks sync.Map

// LocalBinding associates a normalized, symlink-resolved local directory path on
// the current client machine with a remote/logical Flowsk Project.
type LocalBinding struct {
	// WorkspaceSlug is the canonical slug of the workspace owning the bound project.
	WorkspaceSlug string `json:"workspace_slug"`

	// ProjectID is the immutable server-side UUID of the bound Flowsk Project.
	ProjectID uuid.UUID `json:"project_id"`

	// ProjectSlug is the human-readable project slug within WorkspaceSlug.
	ProjectSlug string `json:"project_slug"`

	// LocalAbsPath is the cleaned, symlink-evaluated absolute directory path on THIS
	// client machine only (for example, "/Users/mario/Desktop/billing-api").
	// MUST NEVER be sent over RPC requests or persisted in server databases.
	LocalAbsPath string `json:"local_abs_path"`

	// GitCommonDir caches the canonical shared `.git` directory path on this client
	// machine when LocalAbsPath resides inside a Git repository or Git worktree,
	// enabling automatic sibling worktree grouping and relative sub-project inheritance.
	GitCommonDir string `json:"git_common_dir,omitempty"`

	// GitRelPath caches LocalAbsPath relative to its Git worktree top-level
	// ("" or "." for root repos; e.g. "apps/web" for sub-projects inside a repo).
	GitRelPath string `json:"git_rel_path,omitempty"`

	// UpdatedAt records when this local binding was created or last refreshed in UTC.
	UpdatedAt time.Time `json:"updated_at"`
}

// LinkProjectInput carries parameters for binding a local checkout directory to a
// logical Flowsk Project inside the client's local registry file.
type LinkProjectInput struct {
	// WorkspaceSlug identifies the owning workspace slug.
	WorkspaceSlug string

	// ProjectID is the canonical UUID of the project on the Flowsk server.
	ProjectID uuid.UUID

	// ProjectSlug is the canonical slug of the project.
	ProjectSlug string

	// LocalPath is the target local directory (relative or absolute; implementation
	// resolves `filepath.Abs` and `filepath.EvalSymlinks` before persisting).
	LocalPath string
}

// LocalPathRegistry manages client-side directory-to-project bindings stored in
// the user's OS configuration directory (`~/.config/flowsk/local_paths.json`)
// without ever writing inside the target project directory or leaking host paths
// to the server.
type LocalPathRegistry interface {
	// LinkProject records or updates the local binding between in.LocalPath and
	// `(in.WorkspaceSlug, in.ProjectID, in.ProjectSlug)` in `~/.config/flowsk/local_paths.json`.
	LinkProject(ctx context.Context, in LinkProjectInput) (LocalBinding, error)

	// UnlinkProject removes the binding associated with localPath from the user's local registry file.
	UnlinkProject(ctx context.Context, localPath string) error

	// ResolveByWorkingDir resolves workingDir (or its nearest bound ancestor directory
	// via longest-path-prefix match) to its registered LocalBinding.
	// Returns errs.ErrNotFound if neither workingDir nor any parent directory is bound.
	ResolveByWorkingDir(ctx context.Context, workingDir string) (LocalBinding, error)

	// LocateProject looks up the local checkout directory bound on THIS machine for
	// `(workspaceSlug, projectSlug)` or `projectID` — enabling `flowsk project cd`
	// or local agent workspace discovery without server involvement.
	// Returns errs.ErrNotFound if the project is not linked on this machine.
	LocateProject(ctx context.Context, workspaceSlug, projectSlug string) (LocalBinding, error)

	// ListBindings returns defensive copies of all LocalBindings configured on this
	// client machine, ordered deterministically by WorkspaceSlug, then ProjectSlug.
	ListBindings(ctx context.Context) ([]LocalBinding, error)
}

type persistedRegistry struct {
	Version  int            `json:"version"`
	Bindings []LocalBinding `json:"bindings"`
}

// FileRegistry implements LocalPathRegistry backed by an atomic `0600` JSON file
// in the user's OS configuration directory.
type FileRegistry struct {
	registryFilePath string
	mu               *sync.RWMutex
}

// FileRegistrySkeleton is retained as a type alias of FileRegistry for 100%
// backward compatibility with callers referencing the initial type name.
type FileRegistrySkeleton = FileRegistry

// Compile-time check ensuring *FileRegistry satisfies LocalPathRegistry.
var _ LocalPathRegistry = (*FileRegistry)(nil)

// NewFileRegistry constructs a thread-safe FileRegistry persisting bindings at
// registryFilePath in the user's OS configuration directory.
func NewFileRegistry(registryFilePath string) (*FileRegistrySkeleton, error) {
	trimmed := strings.TrimSpace(registryFilePath)
	if trimmed == "" {
		return nil, errs.Validation("localpath.NewFileRegistry", "registry file path is required", errs.FieldViolation{
			Field:       "registry_file_path",
			Description: "client local_paths.json file path must not be empty",
		})
	}
	absRegPath, err := filepath.Abs(trimmed)
	if err != nil {
		return nil, errs.Wrap(errs.ErrValidation, "localpath.NewFileRegistry", "invalid registry file path", err)
	}
	absRegPath = filepath.Clean(absRegPath)

	lockVal, _ := registryLocks.LoadOrStore(absRegPath, &sync.RWMutex{})
	return &FileRegistry{
		registryFilePath: absRegPath,
		mu:               lockVal.(*sync.RWMutex),
	}, nil
}

// RegistryFilePath returns the user-config file path storing client-local bindings.
func (r *FileRegistry) RegistryFilePath() string {
	return r.registryFilePath
}

// LinkProject validates, symlink-normalizes, and atomically persists a local
// directory binding without touching any file inside in.LocalPath.
func (r *FileRegistry) LinkProject(ctx context.Context, in LinkProjectInput) (LocalBinding, error) {
	const op = "localpath.LocalPathRegistry.LinkProject"
	if err := ctx.Err(); err != nil {
		return LocalBinding{}, fmt.Errorf("%s: %w", op, err)
	}

	wsSlug := strings.TrimSpace(in.WorkspaceSlug)
	projSlug := strings.TrimSpace(in.ProjectSlug)
	var violations []errs.FieldViolation
	if wsSlug == "" {
		violations = append(violations, errs.FieldViolation{
			Field:       "workspace_slug",
			Description: "workspace_slug must not be empty",
		})
	}
	if in.ProjectID == uuid.Nil {
		violations = append(violations, errs.FieldViolation{
			Field:       "project_id",
			Description: "project_id must be a non-nil UUID",
		})
	}
	if projSlug == "" {
		violations = append(violations, errs.FieldViolation{
			Field:       "project_slug",
			Description: "project_slug must not be empty",
		})
	}
	normPath, pathViolation := normalizeLocalDir(in.LocalPath)
	if pathViolation != nil {
		violations = append(violations, *pathViolation)
	}
	if len(violations) > 0 {
		return LocalBinding{}, errs.Validation(op, "invalid project link parameters", violations...)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	bindings, err := r.readBindingsLocked(op)
	if err != nil {
		return LocalBinding{}, err
	}

	gitMeta := InspectDirectoryGit(ctx, normPath)
	relInRepo := ""
	if gitMeta.GitTopLevel != "" {
		if rel, rErr := filepath.Rel(gitMeta.GitTopLevel, normPath); rErr == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			relInRepo = filepath.ToSlash(rel)
		}
	}

	updated := LocalBinding{
		WorkspaceSlug: wsSlug,
		ProjectID:     in.ProjectID,
		ProjectSlug:   projSlug,
		LocalAbsPath:  normPath,
		GitCommonDir:  gitMeta.GitCommonDir,
		GitRelPath:    relInRepo,
		UpdatedAt:     time.Now().UTC().Truncate(time.Millisecond),
	}

	filtered := make([]LocalBinding, 0, len(bindings)+1)
	for _, b := range bindings {
		// Replace existing entry only when the exact normalized local path is re-linked,
		// allowing multiple local worktrees/checkouts to bind to the same logical project.
		if b.LocalAbsPath == normPath {
			continue
		}
		filtered = append(filtered, b)
	}
	filtered = append(filtered, updated)
	sortBindings(filtered)

	if err := r.writeBindingsAtomicallyLocked(op, filtered); err != nil {
		return LocalBinding{}, err
	}
	return updated, nil
}

// UnlinkProject removes the binding associated with localPath from the client-local
// JSON registry without touching the target project directory.
func (r *FileRegistry) UnlinkProject(ctx context.Context, localPath string) error {
	const op = "localpath.LocalPathRegistry.UnlinkProject"
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	normPath, pathViolation := normalizeLocalDir(localPath)
	if pathViolation != nil {
		return errs.Validation(op, "invalid local path to unlink", *pathViolation)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	bindings, err := r.readBindingsLocked(op)
	if err != nil {
		return err
	}

	kept := make([]LocalBinding, 0, len(bindings))
	removed := false
	for _, b := range bindings {
		if b.LocalAbsPath == normPath {
			removed = true
			continue
		}
		kept = append(kept, b)
	}
	if !removed {
		return errs.New(errs.ErrNotFound, op, "no project binding registered for local directory")
	}

	sortBindings(kept)
	return r.writeBindingsAtomicallyLocked(op, kept)
}

// ResolveByWorkingDir resolves workingDir (or its nearest bound ancestor directory
// via longest-prefix boundary-aware path matching) to its registered LocalBinding.
func (r *FileRegistry) ResolveByWorkingDir(ctx context.Context, workingDir string) (LocalBinding, error) {
	const op = "localpath.LocalPathRegistry.ResolveByWorkingDir"
	if err := ctx.Err(); err != nil {
		return LocalBinding{}, fmt.Errorf("%s: %w", op, err)
	}

	normDir, pathViolation := normalizeLocalDir(workingDir)
	if pathViolation != nil {
		return LocalBinding{}, errs.Validation(op, "invalid working directory", *pathViolation)
	}

	r.mu.RLock()
	bindings, err := r.readBindingsLocked(op)
	r.mu.RUnlock()
	if err != nil {
		return LocalBinding{}, err
	}

	var (
		bestMatch LocalBinding
		found     bool
	)
	for _, b := range bindings {
		if !isSameOrSubpath(b.LocalAbsPath, normDir) {
			continue
		}
		if !found || len(b.LocalAbsPath) > len(bestMatch.LocalAbsPath) {
			bestMatch = b
			found = true
		}
	}

	// Also check sibling Git worktree inheritance: if normDir resides inside a Git
	// worktree sharing GitCommonDir with bound repo/sub-projects, inherit the most
	// specific relative subpath binding (so both unlinked sibling worktrees AND
	// sub-projects inside sibling worktrees resolve automatically!).
	gitMeta := InspectDirectoryGit(ctx, normDir)
	if gitMeta.GitCommonDir != "" && gitMeta.GitTopLevel != "" {
		relTarget := ""
		if rel, rErr := filepath.Rel(gitMeta.GitTopLevel, normDir); rErr == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			relTarget = filepath.ToSlash(rel)
		}
		curBestRelLen := -1
		if found {
			if foundGit := InspectDirectoryGit(ctx, bestMatch.LocalAbsPath); foundGit.GitCommonDir == gitMeta.GitCommonDir && foundGit.GitTopLevel != "" {
				if fRel, rErr := filepath.Rel(foundGit.GitTopLevel, bestMatch.LocalAbsPath); rErr == nil && fRel != "." {
					curBestRelLen = len(filepath.ToSlash(fRel))
				} else {
					curBestRelLen = 0
				}
			}
		}
		for _, b := range bindings {
			bCommon := b.GitCommonDir
			bRel := b.GitRelPath
			if bCommon == "" {
				bg := InspectDirectoryGit(ctx, b.LocalAbsPath)
				bCommon = bg.GitCommonDir
				if bg.GitTopLevel != "" {
					if r, rErr := filepath.Rel(bg.GitTopLevel, b.LocalAbsPath); rErr == nil && r != "." && !strings.HasPrefix(r, "..") {
						bRel = filepath.ToSlash(r)
					}
				}
			}
			if bCommon != gitMeta.GitCommonDir {
				continue
			}
			if !isSameOrSlashSubpath(bRel, relTarget) {
				continue
			}
			if len(bRel) > curBestRelLen {
				curBestRelLen = len(bRel)
				derivedPath := gitMeta.GitTopLevel
				if bRel != "" {
					derivedPath = filepath.Join(gitMeta.GitTopLevel, filepath.FromSlash(bRel))
				}
				bestMatch = LocalBinding{
					WorkspaceSlug: b.WorkspaceSlug,
					ProjectID:     b.ProjectID,
					ProjectSlug:   b.ProjectSlug,
					LocalAbsPath:  derivedPath,
					GitCommonDir:  bCommon,
					GitRelPath:    bRel,
					UpdatedAt:     b.UpdatedAt,
				}
				found = true
			}
		}
	}

	if !found {
		return LocalBinding{}, errs.New(errs.ErrNotFound, op, "no project bound to working directory or its ancestors")
	}
	return bestMatch, nil
}

// LocateProject looks up the local checkout directory bound on THIS machine for
// `(workspaceSlug, projectSlug)` (also matching stringified ProjectID).
func (r *FileRegistry) LocateProject(ctx context.Context, workspaceSlug, projectSlug string) (LocalBinding, error) {
	const op = "localpath.LocalPathRegistry.LocateProject"
	if err := ctx.Err(); err != nil {
		return LocalBinding{}, fmt.Errorf("%s: %w", op, err)
	}

	ws := strings.TrimSpace(workspaceSlug)
	proj := strings.TrimSpace(projectSlug)
	var violations []errs.FieldViolation
	if ws == "" {
		violations = append(violations, errs.FieldViolation{
			Field:       "workspace_slug",
			Description: "workspace_slug must not be empty",
		})
	}
	if proj == "" {
		violations = append(violations, errs.FieldViolation{
			Field:       "project_slug",
			Description: "project_slug must not be empty",
		})
	}
	if len(violations) > 0 {
		return LocalBinding{}, errs.Validation(op, "invalid project lookup parameters", violations...)
	}

	r.mu.RLock()
	bindings, err := r.readBindingsLocked(op)
	r.mu.RUnlock()
	if err != nil {
		return LocalBinding{}, err
	}

	for _, b := range bindings {
		if b.WorkspaceSlug == ws && (b.ProjectSlug == proj || b.ProjectID.String() == proj) {
			return b, nil
		}
	}
	return LocalBinding{}, errs.New(errs.ErrNotFound, op, "project is not linked to a local directory on this machine")
}

// ListBindings returns a deterministically ordered defensive copy of all bindings.
func (r *FileRegistry) ListBindings(ctx context.Context) ([]LocalBinding, error) {
	const op = "localpath.LocalPathRegistry.ListBindings"
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	r.mu.RLock()
	bindings, err := r.readBindingsLocked(op)
	r.mu.RUnlock()
	if err != nil {
		return nil, err
	}

	sortBindings(bindings)
	out := make([]LocalBinding, len(bindings))
	copy(out, bindings)
	return out, nil
}

func (r *FileRegistry) readBindingsLocked(op string) ([]LocalBinding, error) {
	raw, err := os.ReadFile(r.registryFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: read registry file: %w", op, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil
	}

	var doc persistedRegistry
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, errs.Wrap(errs.ErrValidation, op, "corrupted local_paths.json registry file", err)
	}
	return doc.Bindings, nil
}

func (r *FileRegistry) writeBindingsAtomicallyLocked(op string, bindings []LocalBinding) error {
	parentDir := filepath.Dir(r.registryFilePath)
	if err := os.MkdirAll(parentDir, 0o700); err != nil {
		return fmt.Errorf("%s: create config directory: %w", op, err)
	}

	payload, err := json.MarshalIndent(persistedRegistry{
		Version:  1,
		Bindings: bindings,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("%s: marshal registry json: %w", op, err)
	}
	payload = append(payload, '\n')

	tmpFile, err := os.CreateTemp(parentDir, ".local_paths-*.tmp")
	if err != nil {
		return fmt.Errorf("%s: create temp registry file: %w", op, err)
	}
	tmpPath := tmpFile.Name()
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmpFile.Chmod(0o600); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("%s: chmod temp registry file: %w", op, err)
	}
	if _, err := tmpFile.Write(payload); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("%s: write temp registry file: %w", op, err)
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("%s: sync temp registry file: %w", op, err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("%s: close temp registry file: %w", op, err)
	}
	if err := os.Rename(tmpPath, r.registryFilePath); err != nil {
		return fmt.Errorf("%s: rename temp registry file: %w", op, err)
	}
	cleanupTmp = false

	if err := os.Chmod(r.registryFilePath, 0o600); err != nil {
		return fmt.Errorf("%s: enforce 0600 permissions: %w", op, err)
	}
	return nil
}

// normalizeLocalDir resolves filepath.Abs and filepath.EvalSymlinks (even when leaf
// directories do not yet exist on disk by resolving the deepest existing ancestor)
// without creating or modifying any files inside rawPath.
func normalizeLocalDir(rawPath string) (string, *errs.FieldViolation) {
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		return "", &errs.FieldViolation{
			Field:       "local_path",
			Description: "local directory path must not be empty",
		}
	}

	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return "", &errs.FieldViolation{
			Field:       "local_path",
			Description: fmt.Sprintf("cannot resolve absolute path: %v", err),
		}
	}
	abs = filepath.Clean(abs)

	info, statErr := os.Stat(abs)
	if statErr == nil {
		if !info.IsDir() {
			return "", &errs.FieldViolation{
				Field:       "local_path",
				Description: "local_path must reference a directory, not a file",
			}
		}
		resolved, symErr := filepath.EvalSymlinks(abs)
		if symErr != nil {
			return "", &errs.FieldViolation{
				Field:       "local_path",
				Description: fmt.Sprintf("cannot evaluate symlinks: %v", symErr),
			}
		}
		return filepath.Clean(resolved), nil
	}

	// Walk upward to resolve symlinks on the deepest existing ancestor (e.g. /var -> /private/var on macOS).
	curr := abs
	var tailSegments []string
	for {
		parent := filepath.Dir(curr)
		tailSegments = append(tailSegments, filepath.Base(curr))
		if parent == curr {
			return abs, nil
		}
		curr = parent
		if pInfo, pErr := os.Stat(curr); pErr == nil && pInfo.IsDir() {
			resolvedAncestor, symErr := filepath.EvalSymlinks(curr)
			if symErr != nil {
				return abs, nil
			}
			slices.Reverse(tailSegments)
			parts := append([]string{resolvedAncestor}, tailSegments...)
			return filepath.Clean(filepath.Join(parts...)), nil
		}
	}
}

func isSameOrSubpath(ancestor, target string) bool {
	if ancestor == target {
		return true
	}
	sep := string(filepath.Separator)
	if ancestor == sep {
		return strings.HasPrefix(target, sep)
	}
	return strings.HasPrefix(target, ancestor+sep)
}

func isSameOrSlashSubpath(ancestorRel, targetRel string) bool {
	if ancestorRel == "" {
		return true
	}
	if ancestorRel == targetRel {
		return true
	}
	return strings.HasPrefix(targetRel, ancestorRel+"/")
}

func sortBindings(bindings []LocalBinding) {
	slices.SortFunc(bindings, func(a, b LocalBinding) int {
		if c := cmp.Compare(a.WorkspaceSlug, b.WorkspaceSlug); c != 0 {
			return c
		}
		if c := cmp.Compare(a.ProjectSlug, b.ProjectSlug); c != 0 {
			return c
		}
		return cmp.Compare(a.LocalAbsPath, b.LocalAbsPath)
	})
}
