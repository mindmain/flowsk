// File git.go provides strictly client-side Git repository, `.git` presence,
// multi-worktree discovery (`git worktree list --porcelain`), and subpath HEAD
// commit inspection (`git log -1`) without ever transmitting local filesystem
// paths to any server.
package localpath

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const gitCmdTimeout = 2 * time.Second

// GitCommitInfo summarizes the latest Git commit affecting a directory or subpath.
type GitCommitInfo struct {
	// Branch is the checked-out branch name (or "HEAD" when detached).
	Branch string `json:"branch,omitempty"`

	// ShortSHA is the abbreviated commit hash (e.g., "3108510").
	ShortSHA string `json:"short_sha,omitempty"`

	// Subject is the single-line commit summary.
	Subject string `json:"subject,omitempty"`

	// RelativeTime is a human-friendly age string (e.g., "2 hours ago").
	RelativeTime string `json:"relative_time,omitempty"`
}

// GitWorktreeInfo describes one local Git worktree belonging to a repository.
type GitWorktreeInfo struct {
	// Path is the symlink-normalized absolute directory of the worktree on this machine.
	Path string `json:"path"`

	// Branch is the checked-out short branch name (or "detached").
	Branch string `json:"branch"`

	// HeadSHA is the abbreviated commit hash checked out in this worktree.
	HeadSHA string `json:"head_sha"`

	// Commit holds the latest commit subject and relative timestamp on this worktree.
	Commit GitCommitInfo `json:"commit"`
}

// DirectoryGitInfo captures client-side Git metadata for a candidate directory.
type DirectoryGitInfo struct {
	// HasDirectDotGit is true when `<dir>/.git` exists directly as a directory
	// (primary Git checkout) or a file (linked Git worktree).
	HasDirectDotGit bool

	// InsideGitRepo is true when `<dir>` is at or beneath a Git working tree.
	InsideGitRepo bool

	// GitTopLevel is the symlink-normalized root of the enclosing Git working tree.
	GitTopLevel string

	// GitCommonDir is the symlink-normalized shared `.git` directory shared by all
	// worktrees of the same Git repository.
	GitCommonDir string

	// Head holds the latest commit on the enclosing worktree.
	Head GitCommitInfo
}

// HasDirectDotGitEntry reports whether dir itself directly contains a `.git`
// directory or `.git` worktree pointer file.
func HasDirectDotGitEntry(dir string) bool {
	trimmed := strings.TrimSpace(dir)
	if trimmed == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(trimmed, ".git"))
	return err == nil
}

// InspectDirectoryGit inspects dir locally using `git` (gracefully returning
// zero-valued fields if `git` is unavailable or dir is outside any repository).
func InspectDirectoryGit(ctx context.Context, dir string) DirectoryGitInfo {
	normDir, violation := normalizeLocalDir(dir)
	if violation != nil || normDir == "" {
		return DirectoryGitInfo{}
	}
	info := DirectoryGitInfo{
		HasDirectDotGit: HasDirectDotGitEntry(normDir),
	}
	if _, statErr := os.Stat(normDir); statErr != nil {
		return info
	}

	out, ok := runGit(ctx, normDir, "rev-parse", "--show-toplevel", "--git-common-dir", "--abbrev-ref", "HEAD")
	if !ok {
		return info
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return info
	}

	topRaw := strings.TrimSpace(lines[0])
	commonRaw := strings.TrimSpace(lines[1])
	if !filepath.IsAbs(commonRaw) {
		commonRaw = filepath.Join(normDir, commonRaw)
	}
	if symTop, err := filepath.EvalSymlinks(topRaw); err == nil {
		topRaw = symTop
	}
	if symCommon, err := filepath.EvalSymlinks(commonRaw); err == nil {
		commonRaw = symCommon
	}

	info.InsideGitRepo = true
	info.GitTopLevel = filepath.Clean(topRaw)
	info.GitCommonDir = filepath.Clean(commonRaw)
	if len(lines) >= 3 {
		info.Head.Branch = strings.TrimSpace(lines[2])
	}
	info.Head = InspectSubpathCommit(ctx, info.GitTopLevel, "", info.Head.Branch)
	return info
}

// InspectSubpathCommit queries the latest Git commit touching relSubpath inside
// repoDir (or the worktree HEAD commit when relSubpath is empty).
func InspectSubpathCommit(ctx context.Context, repoDir, relSubpath, fallbackBranch string) GitCommitInfo {
	res := GitCommitInfo{Branch: fallbackBranch}
	if strings.TrimSpace(repoDir) == "" {
		return res
	}
	if res.Branch == "" {
		if br, ok := runGit(ctx, repoDir, "rev-parse", "--abbrev-ref", "HEAD"); ok {
			res.Branch = strings.TrimSpace(br)
		}
	}
	args := []string{"log", "-1", "--format=%h\x1f%s\x1f%cr"}
	if cleanRel := strings.Trim(strings.TrimSpace(relSubpath), "/"); cleanRel != "" && cleanRel != "." {
		args = append(args, "--", filepath.FromSlash(cleanRel))
	}
	logOut, ok := runGit(ctx, repoDir, args...)
	if !ok || strings.TrimSpace(logOut) == "" {
		return res
	}
	parts := strings.SplitN(strings.TrimSpace(logOut), "\x1f", 3)
	if len(parts) >= 1 {
		res.ShortSHA = strings.TrimSpace(parts[0])
	}
	if len(parts) >= 2 {
		res.Subject = strings.TrimSpace(parts[1])
	}
	if len(parts) >= 3 {
		res.RelativeTime = strings.TrimSpace(parts[2])
	}
	return res
}

// ListGitWorktrees returns all local Git worktrees sharing the same repository
// as repoDir, each enriched with its checked-out branch and latest commit.
func ListGitWorktrees(ctx context.Context, repoDir string) []GitWorktreeInfo {
	if strings.TrimSpace(repoDir) == "" {
		return nil
	}
	raw, ok := runGit(ctx, repoDir, "worktree", "list", "--porcelain")
	if !ok || strings.TrimSpace(raw) == "" {
		return nil
	}

	var out []GitWorktreeInfo
	var cur GitWorktreeInfo
	flush := func() {
		if cur.Path == "" {
			return
		}
		if sym, err := filepath.EvalSymlinks(cur.Path); err == nil {
			cur.Path = filepath.Clean(sym)
		} else {
			cur.Path = filepath.Clean(cur.Path)
		}
		if cur.Branch == "" {
			cur.Branch = "detached"
		}
		cur.Commit = InspectSubpathCommit(ctx, cur.Path, "", cur.Branch)
		if cur.HeadSHA == "" {
			cur.HeadSHA = cur.Commit.ShortSHA
		} else if len(cur.HeadSHA) > 7 {
			cur.HeadSHA = cur.HeadSHA[:7]
		}
		out = append(out, cur)
		cur = GitWorktreeInfo{}
	}

	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			flush()
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "worktree "):
			flush()
			cur.Path = strings.TrimSpace(strings.TrimPrefix(trimmed, "worktree "))
		case strings.HasPrefix(trimmed, "HEAD "):
			cur.HeadSHA = strings.TrimSpace(strings.TrimPrefix(trimmed, "HEAD "))
		case strings.HasPrefix(trimmed, "branch "):
			ref := strings.TrimSpace(strings.TrimPrefix(trimmed, "branch "))
			cur.Branch = strings.TrimPrefix(ref, "refs/heads/")
		case trimmed == "detached":
			cur.Branch = "detached"
		}
	}
	flush()
	return out
}

func runGit(ctx context.Context, dir string, args ...string) (string, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	cctx, cancel := context.WithTimeout(ctx, gitCmdTimeout)
	defer cancel()

	fullArgs := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(cctx, "git", fullArgs...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", false
	}
	return stdout.String(), true
}
