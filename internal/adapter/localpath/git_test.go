// File git_test.go verifies Git `.git` detection, multi-worktree discovery,
// subpath commit lookup, and automatic sibling worktree + sub-project path
// resolution in FileRegistry.
package localpath_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/adapter/localpath"
)

func runGitTestCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Tester",
		"GIT_AUTHOR_EMAIL=tester@example.com",
		"GIT_COMMITTER_NAME=Tester",
		"GIT_COMMITTER_EMAIL=tester@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\nout: %s", args, err, string(out))
	}
}

// TestGitWorktreeGroupingAndSubprojectInheritance verifies that linking a main
// `.git` repo and a nested sub-project automatically resolves both the main
// project and the nested sub-project inside newly created Git worktrees, and
// reports distinct per-worktree and per-subpath commits.
func TestGitWorktreeGroupingAndSubprojectInheritance(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	mainRepo := filepath.Join(tmp, "repo-main")
	webDir := filepath.Join(mainRepo, "apps", "web")
	if err := os.MkdirAll(webDir, 0o755); err != nil {
		t.Fatal(err)
	}

	runGitTestCmd(t, mainRepo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(mainRepo, "README.md"), []byte("# Root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTestCmd(t, mainRepo, "add", "README.md")
	runGitTestCmd(t, mainRepo, "commit", "-m", "root initial commit")

	if err := os.WriteFile(filepath.Join(webDir, "index.ts"), []byte("export const v = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTestCmd(t, mainRepo, "add", "apps/web/index.ts")
	runGitTestCmd(t, mainRepo, "commit", "-m", "feat(web): initial web app")

	// Create a second Git worktree on branch feat/ui.
	wtRepo := filepath.Join(tmp, "repo-wt-ui")
	runGitTestCmd(t, mainRepo, "worktree", "add", "-b", "feat/ui", wtRepo)
	if err := os.WriteFile(filepath.Join(wtRepo, "README.md"), []byte("# Root + UI branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTestCmd(t, wtRepo, "add", "README.md")
	runGitTestCmd(t, wtRepo, "commit", "-m", "docs: update readme in ui worktree")

	ctx := context.Background()
	mainGit := localpath.InspectDirectoryGit(ctx, mainRepo)
	wtGit := localpath.InspectDirectoryGit(ctx, wtRepo)
	subGit := localpath.InspectDirectoryGit(ctx, webDir)

	if !mainGit.HasDirectDotGit || !wtGit.HasDirectDotGit {
		t.Fatalf("expected both main repo and linked worktree to report HasDirectDotGit=true: main=%+v wt=%+v", mainGit, wtGit)
	}
	if subGit.HasDirectDotGit {
		t.Fatal("expected nested apps/web without its own .git to report HasDirectDotGit=false")
	}
	if mainGit.GitCommonDir == "" || mainGit.GitCommonDir != wtGit.GitCommonDir {
		t.Fatalf("expected identical GitCommonDir across worktrees: %q vs %q", mainGit.GitCommonDir, wtGit.GitCommonDir)
	}

	wts := localpath.ListGitWorktrees(ctx, mainRepo)
	if len(wts) != 2 {
		t.Fatalf("expected 2 discovered Git worktrees, got %+v", wts)
	}

	webCommit := localpath.InspectSubpathCommit(ctx, wtRepo, "apps/web", "feat/ui")
	if webCommit.Subject != "feat(web): initial web app" {
		t.Fatalf("expected subpath commit for apps/web, got %+v", webCommit)
	}

	// Link ONLY mainRepo (MACRO) and mainRepo/apps/web (WEB), then verify that
	// resolving inside the UNLINKED worktree wtRepo and wtRepo/apps/web/src
	// automatically resolves MACRO and WEB respectively!
	reg, err := localpath.NewFileRegistry(filepath.Join(tmp, "local_paths.json"))
	if err != nil {
		t.Fatal(err)
	}
	macroID := uuid.New()
	webID := uuid.New()

	if _, err := reg.LinkProject(ctx, localpath.LinkProjectInput{
		WorkspaceSlug: "default",
		ProjectID:     macroID,
		ProjectSlug:   "macro",
		LocalPath:     mainRepo,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.LinkProject(ctx, localpath.LinkProjectInput{
		WorkspaceSlug: "default",
		ProjectID:     webID,
		ProjectSlug:   "web",
		LocalPath:     webDir,
	}); err != nil {
		t.Fatal(err)
	}

	resWT, err := reg.ResolveByWorkingDir(ctx, wtRepo)
	if err != nil || resWT.ProjectID != macroID {
		t.Fatalf("expected unlinked sibling worktree root to resolve macro (%s), got %+v err=%v", macroID, resWT, err)
	}

	wtWebDeep := filepath.Join(wtRepo, "apps", "web", "components")
	if err := os.MkdirAll(wtWebDeep, 0o755); err != nil {
		t.Fatal(err)
	}
	resWTWeb, err := reg.ResolveByWorkingDir(ctx, wtWebDeep)
	if err != nil || resWTWeb.ProjectID != webID {
		t.Fatalf("expected sibling worktree apps/web/components to auto-inherit subproject web (%s), got %+v err=%v", webID, resWTWeb, err)
	}
}
