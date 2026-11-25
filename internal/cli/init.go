// File init.go implements `flowsk init [key]` for initializing a Flowsk project
// or sub-project in the current directory, verifying direct `.git` presence (or
// prompting confirmation), grouping sibling Git worktrees under the same
// project, and wiring bidirectional parent/sub-project DAG bindings.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mindmain/flowsk/internal/adapter/localpath"
	"github.com/spf13/cobra"
)

// newInitCommand constructs `flowsk init [key]`.
func newInitCommand(opts *GlobalOptions, deps projectCommandDeps) *cobra.Command {
	var (
		client     ProjectClient
		localPaths LocalPathRegistry
		formatter  OutputFormatter
	)
	if deps != nil {
		client = deps.ProjectClient()
		localPaths = deps.LocalPathRegistry()
		formatter = deps.OutputFormatter()
	}

	var (
		targetDir   string
		name        string
		description string
		assumeYes   bool
	)

	cmd := &cobra.Command{
		Use:   "init [key]",
		Short: "Initialize or bind a Flowsk project/sub-project in the current folder (auto-detecting .git, worktrees, and sub-projects)",
		Long: `Initializes a Flowsk project (or sub-project) for the current directory without
writing any files inside your repository:

  1. Direct .git Check: If the target folder directly contains a '.git' directory
     (main checkout) or '.git' file (linked Git worktree), Flowsk claims the path
     immediately. Otherwise it asks for confirmation (or pass -y/--yes).
  2. Git Worktree Grouping: If another Git worktree of the exact same repository
     is already registered in Flowsk, 'flowsk init' automatically binds this
     worktree directory to that same project.
  3. Bidirectional Sub-Project Auto-Binding:
       - Top-Down: Initializing inside a subfolder of an already-bound parent
         project automatically registers a parent -> sub-project binding.
       - Bottom-Up: Initializing a macro project in a parent folder automatically
         adopts any existing sub-projects already initialized in child folders.`,
		Example: `  # Initialize inside a Git repo or worktree (auto-derives project key)
  flowsk init

  # Initialize a sub-project inside apps/web (prompts if no .git in apps/web, or -y)
  flowsk init WEB --name "Web Frontend" --yes`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("init context canceled: %w", err)
			}
			if client == nil || localPaths == nil {
				return ErrNotImplemented
			}

			absDir, err := normalizeLocalDir(targetDir)
			if err != nil {
				return err
			}
			dirStat, err := os.Stat(absDir)
			if err != nil {
				return fmt.Errorf("inspect target directory %q: %w", absDir, err)
			}
			if !dirStat.IsDir() {
				return fmt.Errorf("target path %q is not a directory", absDir)
			}

			hasDirectGit := localpath.HasDirectDotGitEntry(absDir)
			if !hasDirectGit && !assumeYes {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "No .git found directly in %q. Initialize a Flowsk project/sub-project here anyway? [y/N]: ", absDir)
				ok, promptErr := readYesConfirmation(cmd.InOrStdin())
				if promptErr != nil {
					return promptErr
				}
				if !ok {
					return errors.New("initialization canceled (no direct .git in folder; pass --yes to confirm non-interactively)")
				}
			}

			ws := defaultWorkspace(opts.Workspace)
			actor := strings.TrimSpace(opts.Actor)
			gitInfo := localpath.InspectDirectoryGit(ctx, absDir)
			curGitRel := ""
			if gitInfo.GitTopLevel != "" {
				if rel, rErr := filepath.Rel(gitInfo.GitTopLevel, absDir); rErr == nil && rel != "." && !strings.HasPrefix(rel, "..") {
					curGitRel = filepath.ToSlash(rel)
				}
			}

			existingLocal, _ := localPaths.ListByProject(ctx, ws, "")
			existingProjsRes, err := client.ListProjects(ctx, ListProjectsInput{
				Workspace:       ws,
				IncludeArchived: false,
				Limit:           200,
			})
			if err != nil {
				return err
			}
			projByKey := make(map[string]ProjectView, len(existingProjsRes.Items))
			for _, p := range existingProjsRes.Items {
				projByKey[strings.ToUpper(p.Key)] = p
			}

			explicitKey := ""
			if len(args) == 1 {
				rawKey := strings.TrimSpace(args[0])
				if rawKey == "" || strings.Contains(rawKey, "/") || strings.Contains(rawKey, string(filepath.Separator)) {
					return fmt.Errorf("invalid project key %q: must be a logical key, never a filesystem path", args[0])
				}
				explicitKey = deriveProjectKey(rawKey)
			}

			var (
				proj           ProjectView
				reusedExisting bool
				worktreeOf     string
			)

			// 1. Check if this exact directory is already bound, or if a sibling Git
			//    worktree of the same root repository is already bound.
			if explicitKey == "" {
				for _, b := range existingLocal {
					if b.LocalDir == absDir {
						if existing, ok := projByKey[strings.ToUpper(b.ProjectID)]; ok {
							proj = existing
							reusedExisting = true
							break
						}
					}
				}
				if proj.ID == "" && hasDirectGit && gitInfo.GitCommonDir != "" && curGitRel == "" {
					for _, b := range existingLocal {
						if b.GitCommonDir == gitInfo.GitCommonDir && b.GitRelPath == "" {
							if existing, ok := projByKey[strings.ToUpper(b.ProjectID)]; ok {
								proj = existing
								reusedExisting = true
								worktreeOf = existing.Key
								break
							}
						}
					}
				}
			} else if existing, ok := projByKey[explicitKey]; ok {
				proj = existing
				reusedExisting = true
			}

			// 2. Otherwise create a new logical project with default workflow states.
			if proj.ID == "" {
				baseKey := explicitKey
				if baseKey == "" {
					baseKey = deriveProjectKey(filepath.Base(absDir))
				}
				chosenKey := allocateAvailableProjectKey(baseKey, projByKey, explicitKey != "")
				dispName := strings.TrimSpace(name)
				if dispName == "" {
					dispName = filepath.Base(absDir)
					if dispName == "" || dispName == "." || dispName == string(filepath.Separator) {
						dispName = chosenKey
					}
				}
				created, cErr := client.CreateProject(ctx, CreateProjectInput{
					Workspace:       ws,
					Key:             chosenKey,
					Name:            dispName,
					Description:     strings.TrimSpace(description),
					DefaultWorkflow: true,
					Actor:           actor,
				})
				if cErr != nil {
					return cErr
				}
				proj = created
			}

			// 3. Persist the client-local directory binding in ~/.flowsk/local_paths.json.
			if _, err := localPaths.Link(ctx, absDir, ws, proj.Key); err != nil {
				return err
			}

			allLocal, _ := localPaths.ListByProject(ctx, ws, "")

			// 4. Top-Down Auto-Binding: if absDir lives beneath an ancestor project
			//    (either in the same directory tree or a sibling Git worktree), bind
			//    (parent -> proj) with the clean relative subpath!
			var parentKey, scopeSubpath string
			if ancKey, relSub := findNearestAncestorBinding(absDir, gitInfo.GitCommonDir, curGitRel, proj.Key, allLocal, projByKey); ancKey != "" {
				if _, bErr := client.BindSubproject(ctx, BindSubprojectInput{
					Workspace:     ws,
					ParentProject: ancKey,
					ChildProject:  proj.Key,
					ScopeSubpath:  relSub,
					Actor:         actor,
				}); bErr == nil {
					parentKey = ancKey
					scopeSubpath = relSub
				}
			}

			// 5. Bottom-Up Auto-Adoption: if child subfolders beneath absDir already
			//    contain standalone/existing Flowsk projects, bind (proj -> child)
			//    automatically so the macro project adopts them without merging tasks!
			adopted := findDirectDescendantBindings(absDir, gitInfo.GitCommonDir, curGitRel, proj.Key, allLocal, projByKey)
			var adoptedKeys []string
			for _, child := range adopted {
				if _, bErr := client.BindSubproject(ctx, BindSubprojectInput{
					Workspace:     ws,
					ParentProject: proj.Key,
					ChildProject:  child.key,
					ScopeSubpath:  child.relSubpath,
					Actor:         actor,
				}); bErr == nil {
					adoptedKeys = append(adoptedKeys, child.key)
				}
			}

			worktrees := collectProjectWorktrees(ctx, proj.Key, scopeSubpath, nil, allLocal)
			return renderPayload(cmd.OutOrStdout(), formatter, opts, InitResultView{
				Project:            proj,
				LocalDir:           absDir,
				HasDirectDotGit:    hasDirectGit,
				ReusedExisting:     reusedExisting,
				WorktreeOf:         worktreeOf,
				ParentKey:          parentKey,
				ScopeSubpath:       scopeSubpath,
				AdoptedSubprojects: adoptedKeys,
				Worktrees:          worktrees,
			})
		},
	}

	cmd.Flags().StringVar(&targetDir, "dir", "", "Target directory to initialize (defaults to current working directory)")
	cmd.Flags().StringVar(&name, "name", "", "Human-readable project display name (defaults to folder name)")
	cmd.Flags().StringVar(&description, "description", "", "Optional project summary")
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "Confirm initialization automatically when no direct .git entry exists in the folder")
	return cmd
}

func readYesConfirmation(r io.Reader) (bool, error) {
	if r == nil {
		return false, nil
	}
	br := bufio.NewReader(r)
	line, err := br.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read confirmation input: %w", err)
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
}

func deriveProjectKey(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(raw)) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
		if b.Len() >= 10 {
			break
		}
	}
	res := b.String()
	if len(res) > 0 && res[0] >= '0' && res[0] <= '9' {
		res = "P" + res
		if len(res) > 10 {
			res = res[:10]
		}
	}
	if len(res) == 1 {
		res += "X"
	}
	if len(res) < 2 {
		return "PRJ"
	}
	return res
}

func allocateAvailableProjectKey(baseKey string, existing map[string]ProjectView, exact bool) string {
	if exact {
		return baseKey
	}
	if _, taken := existing[baseKey]; !taken {
		return baseKey
	}
	stem := baseKey
	if len(stem) > 8 {
		stem = stem[:8]
	}
	for i := 2; i <= 99; i++ {
		cand := fmt.Sprintf("%s%d", stem, i)
		if _, taken := existing[cand]; !taken {
			return cand
		}
	}
	return baseKey
}

type childCandidate struct {
	key        string
	dir        string
	relSubpath string
}

func findNearestAncestorBinding(
	targetDir, gitCommonDir, curGitRel, selfKey string,
	allLocal []LocalProjectBinding,
	activeProjs map[string]ProjectView,
) (string, string) {
	bestLen := -1
	bestKey := ""
	bestRel := ""

	for _, b := range allLocal {
		bKey := strings.ToUpper(firstNonEmptyCLI(b.ProjectKey, b.ProjectID))
		if strings.EqualFold(bKey, selfKey) {
			continue
		}
		if _, ok := activeProjs[bKey]; !ok {
			continue
		}
		if isStrictSubpathDir(b.LocalDir, targetDir) {
			if len(b.LocalDir) > bestLen {
				rel, err := filepath.Rel(b.LocalDir, targetDir)
				if err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
					bestLen = len(b.LocalDir)
					bestKey = bKey
					bestRel = filepath.ToSlash(rel)
				}
			}
			continue
		}
		if gitCommonDir != "" && b.GitCommonDir == gitCommonDir && isStrictSubpathSlash(b.GitRelPath, curGitRel) {
			score := len(b.GitRelPath)
			if score > bestLen {
				rel := curGitRel
				if b.GitRelPath != "" {
					rel = strings.TrimPrefix(curGitRel, b.GitRelPath+"/")
				}
				bestLen = score
				bestKey = bKey
				bestRel = rel
			}
		}
	}
	return bestKey, bestRel
}

func findDirectDescendantBindings(
	targetDir, gitCommonDir, curGitRel, selfKey string,
	allLocal []LocalProjectBinding,
	activeProjs map[string]ProjectView,
) []childCandidate {
	var raw []childCandidate
	seenKey := make(map[string]bool)

	for _, b := range allLocal {
		bKey := strings.ToUpper(firstNonEmptyCLI(b.ProjectKey, b.ProjectID))
		if strings.EqualFold(bKey, selfKey) || seenKey[bKey] {
			continue
		}
		if _, ok := activeProjs[bKey]; !ok {
			continue
		}
		if isStrictSubpathDir(targetDir, b.LocalDir) {
			rel, err := filepath.Rel(targetDir, b.LocalDir)
			if err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
				raw = append(raw, childCandidate{
					key:        bKey,
					dir:        b.LocalDir,
					relSubpath: filepath.ToSlash(rel),
				})
				seenKey[bKey] = true
			}
			continue
		}
		if gitCommonDir != "" && b.GitCommonDir == gitCommonDir && isStrictSubpathSlash(curGitRel, b.GitRelPath) {
			rel := b.GitRelPath
			if curGitRel != "" {
				rel = strings.TrimPrefix(b.GitRelPath, curGitRel+"/")
			}
			raw = append(raw, childCandidate{
				key:        bKey,
				dir:        b.LocalDir,
				relSubpath: rel,
			})
			seenKey[bKey] = true
		}
	}

	// Keep only direct children (exclude c2 if another candidate c1 is a strict ancestor of c2).
	var direct []childCandidate
	for i, cand := range raw {
		nested := false
		for j, other := range raw {
			if i == j {
				continue
			}
			if isStrictSubpathSlash(other.relSubpath, cand.relSubpath) {
				nested = true
				break
			}
		}
		if !nested {
			direct = append(direct, cand)
		}
	}
	slices.SortFunc(direct, func(a, b childCandidate) int {
		return strings.Compare(a.key, b.key)
	})
	return direct
}

func isStrictSubpathDir(ancestor, target string) bool {
	if ancestor == "" || target == "" || ancestor == target {
		return false
	}
	sep := string(filepath.Separator)
	if ancestor == sep {
		return strings.HasPrefix(target, sep) && target != sep
	}
	return strings.HasPrefix(target, ancestor+sep)
}

func isStrictSubpathSlash(ancestorRel, targetRel string) bool {
	if ancestorRel == targetRel || targetRel == "" {
		return false
	}
	if ancestorRel == "" {
		return true
	}
	return strings.HasPrefix(targetRel, ancestorRel+"/")
}

func firstNonEmptyCLI(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}

// collectProjectWorktrees gathers all client-local Git worktrees (or plain
// folders) associated with projKey, computing per-worktree HEAD commits or
// per-subpath commits when projKey is scoped to a relative subfolder.
func collectProjectWorktrees(
	ctx context.Context,
	projKey, scopeSubpath string,
	parentKeys []string,
	allLocal []LocalProjectBinding,
) []WorktreeCommitView {
	seenPaths := make(map[string]bool)
	var out []WorktreeCommitView

	addEntry := func(entry WorktreeCommitView) {
		if entry.Path == "" || seenPaths[entry.Path] {
			return
		}
		seenPaths[entry.Path] = true
		out = append(out, entry)
	}

	for _, b := range allLocal {
		bKey := firstNonEmptyCLI(b.ProjectKey, b.ProjectID)
		if !strings.EqualFold(bKey, projKey) && !strings.EqualFold(b.ProjectID, projKey) {
			continue
		}
		rel := firstNonEmptyCLI(b.GitRelPath, scopeSubpath)
		wts := localpath.ListGitWorktrees(ctx, b.LocalDir)
		if len(wts) == 0 {
			gitMeta := localpath.InspectDirectoryGit(ctx, b.LocalDir)
			addEntry(WorktreeCommitView{
				Path:         b.LocalDir,
				RelSubpath:   rel,
				Branch:       gitMeta.Head.Branch,
				ShortSHA:     gitMeta.Head.ShortSHA,
				Subject:      gitMeta.Head.Subject,
				RelativeTime: gitMeta.Head.RelativeTime,
			})
			continue
		}
		for _, wt := range wts {
			if rel == "" {
				addEntry(WorktreeCommitView{
					Path:         wt.Path,
					Branch:       wt.Branch,
					ShortSHA:     wt.Commit.ShortSHA,
					Subject:      wt.Commit.Subject,
					RelativeTime: wt.Commit.RelativeTime,
				})
				continue
			}
			subDir := filepath.Join(wt.Path, filepath.FromSlash(rel))
			if st, err := os.Stat(subDir); err != nil || !st.IsDir() {
				continue
			}
			subCommit := localpath.InspectSubpathCommit(ctx, wt.Path, rel, wt.Branch)
			if subCommit.ShortSHA == "" {
				subCommit = wt.Commit
			}
			addEntry(WorktreeCommitView{
				Path:         subDir,
				RelSubpath:   rel,
				Branch:       firstNonEmptyCLI(subCommit.Branch, wt.Branch),
				ShortSHA:     subCommit.ShortSHA,
				Subject:      subCommit.Subject,
				RelativeTime: subCommit.RelativeTime,
			})
		}
	}

	// If no direct local path binding exists for a sub-project with scopeSubpath,
	// derive subpath commit status from any locally-bound parent macro project worktree!
	if len(out) == 0 && scopeSubpath != "" && len(parentKeys) > 0 {
		for _, pk := range parentKeys {
			for _, b := range allLocal {
				bKey := firstNonEmptyCLI(b.ProjectKey, b.ProjectID)
				if !strings.EqualFold(bKey, pk) {
					continue
				}
				for _, wt := range localpath.ListGitWorktrees(ctx, b.LocalDir) {
					subDir := filepath.Join(wt.Path, filepath.FromSlash(scopeSubpath))
					if st, err := os.Stat(subDir); err != nil || !st.IsDir() {
						continue
					}
					subCommit := localpath.InspectSubpathCommit(ctx, wt.Path, scopeSubpath, wt.Branch)
					if subCommit.ShortSHA == "" {
						subCommit = wt.Commit
					}
					addEntry(WorktreeCommitView{
						Path:         subDir,
						RelSubpath:   scopeSubpath,
						Branch:       firstNonEmptyCLI(subCommit.Branch, wt.Branch),
						ShortSHA:     subCommit.ShortSHA,
						Subject:      subCommit.Subject,
						RelativeTime: subCommit.RelativeTime,
					})
				}
			}
		}
	}

	return out
}
