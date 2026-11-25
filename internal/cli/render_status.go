// File render_status.go renders multi-project `flowsk status` overviews,
// `flowsk init` results, project DAG bindings, and Git worktree/subpath commit
// tables across human, compact, TSV, and Markdown formats.
package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

func formatWorktreeCommitShort(wt WorktreeCommitView) string {
	if wt.ShortSHA == "" {
		if wt.Branch != "" {
			return fmt.Sprintf("[%s] (no commits)", wt.Branch)
		}
		return "(not a git repository)"
	}
	age := ""
	if wt.RelativeTime != "" {
		age = " (" + wt.RelativeTime + ")"
	}
	subj := truncateText(singleLine(wt.Subject), 48)
	if wt.Branch != "" {
		return fmt.Sprintf("[%s] %s %s%s", wt.Branch, wt.ShortSHA, subj, age)
	}
	return fmt.Sprintf("%s %s%s", wt.ShortSHA, subj, age)
}

func truncateText(s string, maxLen int) string {
	if maxLen <= 4 || len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

func flattenProjectSummaries(roots []ProjectStatusSummaryView) []ProjectStatusSummaryView {
	var flat []ProjectStatusSummaryView
	var walk func(items []ProjectStatusSummaryView)
	walk = func(items []ProjectStatusSummaryView) {
		for _, item := range items {
			flat = append(flat, item)
			if len(item.Children) > 0 {
				walk(item.Children)
			}
		}
	}
	walk(roots)
	return flat
}

func renderHumanWorkspaceOverview(w io.Writer, v WorkspaceOverviewView) error {
	_, _ = fmt.Fprintf(w, "=== FLOWSK STATUS — Workspace: %s (%d projects, %d sub-projects) ===\n",
		v.Workspace, v.TotalProjects, v.TotalSubprojects)
	c := v.Counts
	archSuffix := ""
	if c.Archived > 0 {
		archSuffix = fmt.Sprintf(" | Archived: %d", c.Archived)
	}
	_, _ = fmt.Fprintf(w, "Totals: %d tasks | Backlog: %d | Planned: %d | Progress: %d | Review: %d | Blocked: %d | Done: %d | Canceled: %d%s\n",
		c.Total, c.Backlog, c.Planned, c.InProgress, c.InReview, c.Blocked, c.Done, c.Canceled, archSuffix)
	if v.OrphanIssuesCount > 0 {
		_, _ = fmt.Fprintf(w, "\033[33m⚠ Warning: %d orphan or missing Markdown resource(s) detected in ~/.flowsk — run 'flowsk prune' to clean up.\033[0m\n", v.OrphanIssuesCount)
	}

	if len(v.Projects) == 0 {
		_, err := fmt.Fprintln(w, "\nNo active projects in this workspace yet. Run 'flowsk init' inside a repository to start.")
		return err
	}

	_, _ = fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PROJECT\tNAME / SUBPATH\tTOTAL\tBACKLOG\tPLANNED\tPROGRESS\tREVIEW\tBLOCKED\tDONE\tGOALS\tLATEST GIT COMMIT")
	rows := flattenProjectSummaries(v.Projects)
	for _, row := range rows {
		label := row.Project.Key
		if row.Depth > 0 {
			label = strings.Repeat("  ", row.Depth-1) + "└─ " + row.Project.Key
		}
		nameCol := row.Project.Name
		if row.ScopeSubpath != "" {
			nameCol = fmt.Sprintf("%s (%s)", row.Project.Name, row.ScopeSubpath)
		}
		goalsStr := "-"
		if row.GoalsTotal > 0 {
			goalsStr = fmt.Sprintf("%d/%d", row.GoalsDone, row.GoalsTotal)
		}
		commitStr := "-"
		if len(row.Worktrees) == 1 {
			commitStr = formatWorktreeCommitShort(row.Worktrees[0])
		} else if len(row.Worktrees) > 1 {
			commitStr = fmt.Sprintf("%d worktrees (%s)", len(row.Worktrees), formatWorktreeCommitShort(row.Worktrees[0]))
		}
		rc := row.Counts
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%s\n",
			label, nameCol,
			rc.Total, rc.Backlog, rc.Planned, rc.InProgress, rc.InReview, rc.Blocked, rc.Done,
			goalsStr, commitStr)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	// Render grouped worktree details whenever any project has multiple worktrees
	// (or bound local checkouts) so every worktree folder and its latest commit are visible!
	hasWorktrees := false
	for _, row := range rows {
		if len(row.Worktrees) > 0 {
			hasWorktrees = true
			break
		}
	}
	if hasWorktrees {
		_, _ = fmt.Fprintln(w, "\n--- Local Checkouts & Git Worktrees ---")
		wtw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(wtw, "PROJECT\tBRANCH\tCOMMIT\tPATH")
		for _, row := range rows {
			for _, wt := range row.Worktrees {
				commitDesc := "-"
				if wt.ShortSHA != "" {
					commitDesc = fmt.Sprintf("%s %s", wt.ShortSHA, truncateText(singleLine(wt.Subject), 44))
					if wt.RelativeTime != "" {
						commitDesc += " (" + wt.RelativeTime + ")"
					}
				}
				br := wt.Branch
				if br == "" {
					br = "-"
				}
				_, _ = fmt.Fprintf(wtw, "%s\t%s\t%s\t%s\n", row.Project.Key, br, commitDesc, wt.Path)
			}
		}
		_ = wtw.Flush()
	}
	return nil
}

func renderCompactWorkspaceOverview(w io.Writer, v WorkspaceOverviewView) error {
	c := v.Counts
	extra := ""
	if c.Archived > 0 {
		extra += fmt.Sprintf(" archived=%d", c.Archived)
	}
	if v.OrphanIssuesCount > 0 {
		extra += fmt.Sprintf(" orphans=%d", v.OrphanIssuesCount)
	}
	_, _ = fmt.Fprintf(w, "STATUS workspace=%s projects=%d subprojects=%d total=%d backlog=%d planned=%d in_progress=%d review=%d blocked=%d done=%d canceled=%d%s\n",
		v.Workspace, v.TotalProjects, v.TotalSubprojects,
		c.Total, c.Backlog, c.Planned, c.InProgress, c.InReview, c.Blocked, c.Done, c.Canceled, extra)
	for _, row := range flattenProjectSummaries(v.Projects) {
		indent := strings.Repeat("  ", row.Depth)
		sub := ""
		if row.ScopeSubpath != "" {
			sub = " subpath=" + row.ScopeSubpath
		}
		gitPart := ""
		if len(row.Worktrees) > 0 && row.Worktrees[0].ShortSHA != "" {
			gitPart = fmt.Sprintf(" git=%s@%s(wt=%d)", row.Worktrees[0].Branch, row.Worktrees[0].ShortSHA, len(row.Worktrees))
		}
		rc := row.Counts
		_, _ = fmt.Fprintf(w, "%sPROJ %s (%s)%s total=%d backlog=%d planned=%d in_progress=%d review=%d blocked=%d done=%d canceled=%d goals=%d/%d%s\n",
			indent, row.Project.Key, row.Project.Name, sub,
			rc.Total, rc.Backlog, rc.Planned, rc.InProgress, rc.InReview, rc.Blocked, rc.Done, rc.Canceled,
			row.GoalsDone, row.GoalsTotal, gitPart)
	}
	return nil
}

func renderMarkdownWorkspaceOverview(w io.Writer, v WorkspaceOverviewView) error {
	c := v.Counts
	_, _ = fmt.Fprintf(w, "# Flowsk Status — `%s`\n\n", v.Workspace)
	_, _ = fmt.Fprintf(w, "- **Projects**: %d (%d sub-projects) | **Total Tasks**: %d (Backlog: %d, Planned: %d, In Progress: %d, Review: %d, Blocked: %d, Done: %d)\n\n",
		v.TotalProjects, v.TotalSubprojects, c.Total, c.Backlog, c.Planned, c.InProgress, c.InReview, c.Blocked, c.Done)
	if len(v.Projects) == 0 {
		return nil
	}
	_, _ = fmt.Fprintln(w, "| Project | Name / Subpath | Total | Backlog | Planned | Progress | Review | Blocked | Done | Goals | Latest Commit |")
	_, _ = fmt.Fprintln(w, "| :--- | :--- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | :---: | :--- |")
	for _, row := range flattenProjectSummaries(v.Projects) {
		label := row.Project.Key
		if row.Depth > 0 {
			label = "↳ " + row.Project.Key
		}
		name := row.Project.Name
		if row.ScopeSubpath != "" {
			name = fmt.Sprintf("%s (`%s`)", row.Project.Name, row.ScopeSubpath)
		}
		commit := "-"
		if len(row.Worktrees) > 0 {
			commit = formatWorktreeCommitShort(row.Worktrees[0])
		}
		rc := row.Counts
		_, _ = fmt.Fprintf(w, "| **%s** | %s | %d | %d | %d | %d | %d | %d | %d | %d/%d | %s |\n",
			label, name, rc.Total, rc.Backlog, rc.Planned, rc.InProgress, rc.InReview, rc.Blocked, rc.Done,
			row.GoalsDone, row.GoalsTotal, commit)
	}
	return nil
}

func renderHumanInitResult(w io.Writer, v InitResultView) error {
	action := "Initialized"
	if v.ReusedExisting {
		action = "Bound existing"
	}
	_, _ = fmt.Fprintf(w, "%s Flowsk project %s (%s)\n", action, v.Project.Key, v.Project.Name)
	_, _ = fmt.Fprintf(w, "  Local Dir:  %s\n", v.LocalDir)
	if v.WorktreeOf != "" {
		_, _ = fmt.Fprintf(w, "  Worktree:   grouped with existing repository project %s (%d total worktrees)\n", v.WorktreeOf, len(v.Worktrees))
	}
	if v.ParentKey != "" {
		_, _ = fmt.Fprintf(w, "  Parent:     %s (subpath: %s)\n", v.ParentKey, v.ScopeSubpath)
	}
	if len(v.AdoptedSubprojects) > 0 {
		_, _ = fmt.Fprintf(w, "  Adopted:    %s (sub-projects bound under %s)\n", strings.Join(v.AdoptedSubprojects, ", "), v.Project.Key)
	}
	for _, wt := range v.Worktrees {
		if wt.ShortSHA != "" {
			_, _ = fmt.Fprintf(w, "  Git HEAD:   %s — %s\n", formatWorktreeCommitShort(wt), wt.Path)
		}
	}
	return nil
}

func renderCompactInitResult(w io.Writer, v InitResultView) error {
	var extras []string
	if v.ReusedExisting {
		extras = append(extras, "reused=true")
	}
	if v.WorktreeOf != "" {
		extras = append(extras, "worktree_of="+v.WorktreeOf)
	}
	if v.ParentKey != "" {
		extras = append(extras, fmt.Sprintf("parent=%s(%s)", v.ParentKey, v.ScopeSubpath))
	}
	if len(v.AdoptedSubprojects) > 0 {
		extras = append(extras, "adopted="+strings.Join(v.AdoptedSubprojects, ","))
	}
	tail := ""
	if len(extras) > 0 {
		tail = " " + strings.Join(extras, " ")
	}
	_, err := fmt.Fprintf(w, "init %s dir=%s%s\n", v.Project.Key, v.LocalDir, tail)
	return err
}

func renderHumanProjectBindings(w io.Writer, items []ProjectBindingView) error {
	if len(items) == 0 {
		_, err := fmt.Fprintln(w, "No sub-project bindings configured in this workspace.")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PARENT\tCHILD\tSUBPATH")
	for _, b := range items {
		sub := b.ScopeSubpath
		if sub == "" {
			sub = "-"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n",
			firstNonEmptyCLI(b.ParentKey, b.ParentProjectID),
			firstNonEmptyCLI(b.ChildKey, b.ChildProjectID),
			sub)
	}
	return tw.Flush()
}
