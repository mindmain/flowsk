// Package localpath_test verifies client-local directory-to-project bindings,
// longest-prefix ancestor resolution, macOS symlink normalization, atomic 0600
// JSON persistence, race-safe concurrency, and strict zero-footprint invariants
// on target project directories.
package localpath_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mindmain/flowsk/internal/adapter/localpath"
	"github.com/mindmain/flowsk/internal/domain/errs"
)

// TestFileRegistry_LinkResolveLocateListAndUnlink verifies the full lifecycle
// of local bindings, including longest-prefix nested directory matching and
// symlink normalization (such as macOS /var -> /private/var).
func TestFileRegistry_LinkResolveLocateListAndUnlink(t *testing.T) {
	t.Parallel()

	if _, err := localpath.NewFileRegistry(""); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("expected ErrValidation for empty registry path, got %v", err)
	}

	configDir := t.TempDir()
	registryPath := filepath.Join(configDir, "flowsk", "local_paths.json")
	reg, err := localpath.NewFileRegistry(registryPath)
	if err != nil {
		t.Fatalf("NewFileRegistry failed: %v", err)
	}

	ctx := context.Background()
	rootRepo := filepath.Join(t.TempDir(), "monorepo")
	nestedHandlerDir := filepath.Join(rootRepo, "internal", "http", "handlers")
	subServiceDir := filepath.Join(rootRepo, "services", "billing")
	subServiceDeepDir := filepath.Join(subServiceDir, "cmd", "worker")
	siblingRepo := rootRepo + "-other"

	for _, dir := range []string{nestedHandlerDir, subServiceDeepDir, siblingRepo} {
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			t.Fatalf("mkdir %s: %v", dir, mkErr)
		}
	}

	// Create an explicit symlink pointing to rootRepo to verify EvalSymlinks normalization.
	symlinkRoot := filepath.Join(t.TempDir(), "symlink-to-monorepo")
	if symErr := os.Symlink(rootRepo, symlinkRoot); symErr != nil {
		t.Fatalf("symlink: %v", symErr)
	}

	monoID := uuid.New()
	billingID := uuid.New()

	monoBinding, err := reg.LinkProject(ctx, localpath.LinkProjectInput{
		WorkspaceSlug: "acme",
		ProjectID:     monoID,
		ProjectSlug:   "platform-mono",
		LocalPath:     symlinkRoot, // Linked via symlink!
	})
	if err != nil {
		t.Fatalf("LinkProject(platform-mono) failed: %v", err)
	}

	canonicalRoot, err := filepath.EvalSymlinks(rootRepo)
	if err != nil {
		t.Fatalf("EvalSymlinks(rootRepo): %v", err)
	}
	if monoBinding.LocalAbsPath != canonicalRoot {
		t.Fatalf("expected symlink-normalized path %q, got %q", canonicalRoot, monoBinding.LocalAbsPath)
	}

	_, err = reg.LinkProject(ctx, localpath.LinkProjectInput{
		WorkspaceSlug: "acme",
		ProjectID:     billingID,
		ProjectSlug:   "billing-svc",
		LocalPath:     subServiceDir,
	})
	if err != nil {
		t.Fatalf("LinkProject(billing-svc) failed: %v", err)
	}

	// Resolving from deep nested subdir under monorepo resolves platform-mono.
	resolvedMono, err := reg.ResolveByWorkingDir(ctx, nestedHandlerDir)
	if err != nil {
		t.Fatalf("ResolveByWorkingDir(nestedHandlerDir) failed: %v", err)
	}
	if resolvedMono.ProjectID != monoID || resolvedMono.ProjectSlug != "platform-mono" {
		t.Fatalf("expected platform-mono (%s), got %+v", monoID, resolvedMono)
	}

	// Resolving via the symlinked nested path also resolves identically.
	resolvedViaSymlink, err := reg.ResolveByWorkingDir(ctx, filepath.Join(symlinkRoot, "internal", "http", "handlers"))
	if err != nil {
		t.Fatalf("ResolveByWorkingDir(symlink nested) failed: %v", err)
	}
	if resolvedViaSymlink.ProjectID != monoID {
		t.Fatalf("expected symlink nested resolution to match %s, got %s", monoID, resolvedViaSymlink.ProjectID)
	}

	// Longest-prefix ancestor match: nested sub-service directory must resolve billing-svc, not platform-mono.
	resolvedSub, err := reg.ResolveByWorkingDir(ctx, subServiceDeepDir)
	if err != nil {
		t.Fatalf("ResolveByWorkingDir(subServiceDeepDir) failed: %v", err)
	}
	if resolvedSub.ProjectID != billingID || resolvedSub.ProjectSlug != "billing-svc" {
		t.Fatalf("expected longest-prefix match billing-svc (%s), got %+v", billingID, resolvedSub)
	}

	// Boundary check: /path/monorepo-other MUST NOT match /path/monorepo.
	if _, err := reg.ResolveByWorkingDir(ctx, siblingRepo); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for sibling directory %q, got %v", siblingRepo, err)
	}

	// LocateProject resolves local path by (workspaceSlug, projectSlug).
	located, err := reg.LocateProject(ctx, "acme", "billing-svc")
	if err != nil {
		t.Fatalf("LocateProject failed: %v", err)
	}
	if located.ProjectID != billingID {
		t.Fatalf("expected located ProjectID %s, got %s", billingID, located.ProjectID)
	}
	if _, err := reg.LocateProject(ctx, "acme", "nonexistent"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for unknown project, got %v", err)
	}

	// ListBindings returns deterministically ordered bindings (acme/billing-svc before acme/platform-mono).
	list, err := reg.ListBindings(ctx)
	if err != nil {
		t.Fatalf("ListBindings failed: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 bindings, got %d", len(list))
	}
	if list[0].ProjectSlug != "billing-svc" || list[1].ProjectSlug != "platform-mono" {
		t.Fatalf("expected deterministic alphabetical sort by WorkspaceSlug+ProjectSlug, got %+v", list)
	}

	// Unlink sub-service and verify subsequent resolution falls back to ancestor monorepo.
	if err := reg.UnlinkProject(ctx, subServiceDir); err != nil {
		t.Fatalf("UnlinkProject(subServiceDir) failed: %v", err)
	}
	fallbackAfterUnlink, err := reg.ResolveByWorkingDir(ctx, subServiceDeepDir)
	if err != nil {
		t.Fatalf("ResolveByWorkingDir after unlink failed: %v", err)
	}
	if fallbackAfterUnlink.ProjectID != monoID {
		t.Fatalf("expected ancestor fallback to platform-mono after unlink, got %+v", fallbackAfterUnlink)
	}

	// Unlinking an already unlinked directory returns ErrNotFound.
	if err := reg.UnlinkProject(ctx, subServiceDir); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("expected ErrNotFound unlinking unbound path, got %v", err)
	}
}

// TestFileRegistry_ZeroTargetRepoFootprintAndAtomicPermissions explicitly asserts
// that LinkProject, ResolveByWorkingDir, and UnlinkProject create ZERO files or
// hidden directories inside the target project directory, while writing
// local_paths.json with 0600 permissions inside the config directory.
func TestFileRegistry_ZeroTargetRepoFootprintAndAtomicPermissions(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	registryPath := filepath.Join(configDir, "local_paths.json")

	targetRepo := t.TempDir()
	subDir := filepath.Join(targetRepo, "pkg", "domain")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("mkdir subDir: %v", err)
	}
	readmePath := filepath.Join(targetRepo, "README.md")
	if err := os.WriteFile(readmePath, []byte("# Clean Repo\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}

	snapshotRepoEntries := func() []string {
		t.Helper()
		var rels []string
		err := filepath.WalkDir(targetRepo, func(p string, _ os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, relErr := filepath.Rel(targetRepo, p)
			if relErr != nil {
				return relErr
			}
			rels = append(rels, rel)
			return nil
		})
		if err != nil {
			t.Fatalf("WalkDir targetRepo: %v", err)
		}
		return rels
	}

	before := snapshotRepoEntries()

	reg, err := localpath.NewFileRegistry(registryPath)
	if err != nil {
		t.Fatalf("NewFileRegistry: %v", err)
	}

	ctx := context.Background()
	if _, err := reg.LinkProject(ctx, localpath.LinkProjectInput{
		WorkspaceSlug: "clean-ws",
		ProjectID:     uuid.New(),
		ProjectSlug:   "clean-repo",
		LocalPath:     targetRepo,
	}); err != nil {
		t.Fatalf("LinkProject: %v", err)
	}

	// Verify registry file exists under configDir with strict 0600 permissions and no leftover temp files.
	info, err := os.Stat(registryPath)
	if err != nil {
		t.Fatalf("stat registryPath: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("expected 0600 permissions on local_paths.json, got %04o", perm)
	}

	if _, err := reg.ResolveByWorkingDir(ctx, subDir); err != nil {
		t.Fatalf("ResolveByWorkingDir: %v", err)
	}
	if err := reg.UnlinkProject(ctx, targetRepo); err != nil {
		t.Fatalf("UnlinkProject: %v", err)
	}

	after := snapshotRepoEntries()
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("target repository footprint mutated! before=%v after=%v", before, after)
	}

	cfgEntries, err := os.ReadDir(configDir)
	if err != nil {
		t.Fatalf("ReadDir configDir: %v", err)
	}
	for _, entry := range cfgEntries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("unexpected leftover temporary file in config dir: %s", entry.Name())
		}
	}
}

// TestFileRegistry_ConcurrentAccessIsRaceSafe stresses concurrent LinkProject,
// ResolveByWorkingDir, LocateProject, ListBindings, and UnlinkProject operations
// across parallel goroutines under the Go race detector.
func TestFileRegistry_ConcurrentAccessIsRaceSafe(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	registryPath := filepath.Join(configDir, "local_paths.json")
	reg, err := localpath.NewFileRegistry(registryPath)
	if err != nil {
		t.Fatalf("NewFileRegistry: %v", err)
	}

	const workers = 16
	dirs := make([]string, workers)
	ids := make([]uuid.UUID, workers)
	for i := range workers {
		dirs[i] = filepath.Join(t.TempDir(), fmt.Sprintf("repo-%02d", i))
		if mkErr := os.MkdirAll(filepath.Join(dirs[i], "src"), 0o755); mkErr != nil {
			t.Fatalf("mkdir: %v", mkErr)
		}
		ids[i] = uuid.New()
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	errCh := make(chan error, workers*4)

	for i := range workers {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			slug := fmt.Sprintf("proj-%02d", idx)
			if _, linkErr := reg.LinkProject(ctx, localpath.LinkProjectInput{
				WorkspaceSlug: "concurrent-ws",
				ProjectID:     ids[idx],
				ProjectSlug:   slug,
				LocalPath:     dirs[idx],
			}); linkErr != nil {
				errCh <- fmt.Errorf("worker %d LinkProject: %w", idx, linkErr)
				return
			}
			if _, resErr := reg.ResolveByWorkingDir(ctx, filepath.Join(dirs[idx], "src")); resErr != nil {
				errCh <- fmt.Errorf("worker %d ResolveByWorkingDir: %w", idx, resErr)
				return
			}
			if _, locErr := reg.LocateProject(ctx, "concurrent-ws", slug); locErr != nil {
				errCh <- fmt.Errorf("worker %d LocateProject: %w", idx, locErr)
				return
			}
			if _, listErr := reg.ListBindings(ctx); listErr != nil {
				errCh <- fmt.Errorf("worker %d ListBindings: %w", idx, listErr)
				return
			}
		}(i)
	}

	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Fatal(e)
	}

	all, err := reg.ListBindings(ctx)
	if err != nil {
		t.Fatalf("final ListBindings failed: %v", err)
	}
	if len(all) != workers {
		t.Fatalf("expected all %d concurrent bindings persisted without lost updates, got %d", workers, len(all))
	}
}

// TestFileRegistry_ExactMatchSkipsGitSubprocesses verifies that ResolveByWorkingDir
// returns immediately on an exact LocalAbsPath match without spawning git subprocesses.
func TestFileRegistry_ExactMatchSkipsGitSubprocesses(t *testing.T) {
	configDir := t.TempDir()
	registryPath := filepath.Join(configDir, "local_paths.json")
	reg, err := localpath.NewFileRegistry(registryPath)
	if err != nil {
		t.Fatalf("NewFileRegistry: %v", err)
	}

	targetDir := t.TempDir()
	projID := uuid.New()
	ctx := context.Background()
	if _, err := reg.LinkProject(ctx, localpath.LinkProjectInput{
		WorkspaceSlug: "fast-ws",
		ProjectID:     projID,
		ProjectSlug:   "fast-proj",
		LocalPath:     targetDir,
	}); err != nil {
		t.Fatalf("LinkProject: %v", err)
	}

	// Point PATH to a directory containing a failing `git` shim; if ResolveByWorkingDir
	// spawned `git`, we would touch the sentinel file.
	shimDir := t.TempDir()
	sentinel := filepath.Join(shimDir, "git-invoked")
	gitShim := filepath.Join(shimDir, "git")
	shimScript := "#!/bin/sh\necho invoked > " + sentinel + "\nexit 1\n"
	if err := os.WriteFile(gitShim, []byte(shimScript), 0o755); err != nil {
		t.Fatalf("write git shim: %v", err)
	}
	t.Setenv("PATH", shimDir)

	got, err := reg.ResolveByWorkingDir(ctx, targetDir)
	if err != nil || got.ProjectID != projID {
		t.Fatalf("ResolveByWorkingDir exact match = %+v, err=%v", got, err)
	}
	if _, statErr := os.Stat(sentinel); !os.IsNotExist(statErr) {
		t.Fatal("expected exact LocalAbsPath match to skip spawning git subprocesses")
	}
}
