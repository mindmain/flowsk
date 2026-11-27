// File bundle.go composes full Goal and Project Markdown blueprints (`flowsk goal bundle`
// and `flowsk project bundle`) through the abstract ProjectClient, ObjectiveClient,
// and TaskClient ports so both local SQLite and remote ConnectRPC executions
// produce identical supervisor- and developer-ready specification documents.
package cli

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

type cliBundleOptions struct {
	IncludeDone       bool
	IncludeProjectDoc bool
	IncludeComments   bool
}

func extractCLITaskKeyNumber(key string) (string, int) {
	idx := strings.LastIndexByte(key, '-')
	if idx <= 0 || idx+1 >= len(key) {
		return strings.ToUpper(key), 0
	}
	n, err := strconv.Atoi(key[idx+1:])
	if err != nil {
		return strings.ToUpper(key), 0
	}
	return strings.ToUpper(key[:idx]), n
}

func cliPrioritySortRank(p int) int {
	if p >= 1 && p <= 4 {
		return p
	}
	return 5
}

func sortCLITaskSummaries(items []TaskSummaryView) {
	slices.SortFunc(items, func(a, b TaskSummaryView) int {
		pa, na := extractCLITaskKeyNumber(a.Key)
		pb, nb := extractCLITaskKeyNumber(b.Key)
		if pa == pb && na > 0 && nb > 0 {
			return cmp.Compare(na, nb)
		}
		if c := cmp.Compare(cliPrioritySortRank(a.Priority), cliPrioritySortRank(b.Priority)); c != 0 {
			return c
		}
		return cmp.Compare(a.Key, b.Key)
	})
}

func isCLITerminalTask(t TaskSummaryView) bool {
	cat := strings.ToLower(strings.TrimSpace(t.Category))
	st := strings.ToLower(strings.TrimSpace(t.State))
	return cat == "done" || cat == "canceled" || cat == "cancelled" || cat == "archived" ||
		st == "done" || st == "canceled" || st == "cancelled" || st == "archived"
}

func formatCLIBundlePriority(p int) string {
	switch p {
	case 1:
		return "P1 (urgent)"
	case 2:
		return "P2 (high)"
	case 3:
		return "P3 (medium)"
	case 4:
		return "P4 (low)"
	default:
		return "P0 (none)"
	}
}

func escapeCLITableCell(s string) string {
	cleaned := strings.ReplaceAll(strings.TrimSpace(s), "|", "\\|")
	return strings.ReplaceAll(cleaned, "\n", " ")
}

func hydrateCLIBundledTasks(
	ctx context.Context,
	ws, proj string,
	taskClient TaskClient,
	summaries []TaskSummaryView,
	includeComments bool,
) ([]BundledTaskView, error) {
	sorted := append([]TaskSummaryView(nil), summaries...)
	sortCLITaskSummaries(sorted)

	out := make([]BundledTaskView, 0, len(sorted))
	for _, sum := range sorted {
		tv, err := taskClient.GetTask(ctx, GetTaskInput{
			Workspace:       ws,
			Project:         proj,
			Task:            sum.Key,
			IncludeComments: includeComments,
			IncludeDeps:     true,
		})
		if err != nil {
			return nil, err
		}
		var md string
		if tv.HasMarkdownDoc {
			if docView, mErr := taskClient.GetTaskMarkdown(ctx, ws, tv.Key); mErr == nil && docView.HasMarkdownDoc {
				md = docView.Markdown
			}
		}
		out = append(out, BundledTaskView{
			Task:     tv,
			Markdown: md,
		})
	}
	return out, nil
}

func composeCLIGoalBundle(
	ctx context.Context,
	ws, proj, objRef string,
	opts cliBundleOptions,
	projClient ProjectClient,
	objClient ObjectiveClient,
	taskClient TaskClient,
) (BlueprintBundleView, error) {
	obj, err := objClient.GetObjective(ctx, ws, proj, objRef)
	if err != nil {
		return BlueprintBundleView{}, err
	}

	var pv ProjectView
	var projMD string
	if projClient != nil {
		if gotProj, pErr := projClient.GetProject(ctx, ws, proj); pErr == nil {
			pv = gotProj
			if opts.IncludeProjectDoc && gotProj.HasMarkdownDoc {
				if doc, dErr := projClient.GetProjectMarkdown(ctx, ws, proj); dErr == nil && doc.HasMarkdownDoc {
					projMD = doc.Markdown
				}
			}
		}
	}
	if pv.Key == "" {
		pv.Key = proj
		pv.Name = proj
	}

	var tasks []BundledTaskView
	if taskClient != nil {
		listRes, lErr := taskClient.ListTasks(ctx, ListTasksInput{
			Workspace: ws,
			Project:   proj,
			Objective: obj.Key,
			Priority:  -1,
			Limit:     500,
		})
		if lErr != nil {
			return BlueprintBundleView{}, lErr
		}
		var filtered []TaskSummaryView
		for _, item := range listRes.Items {
			if !opts.IncludeDone && isCLITerminalTask(item) {
				continue
			}
			filtered = append(filtered, item)
		}
		hydrated, hErr := hydrateCLIBundledTasks(ctx, ws, proj, taskClient, filtered, opts.IncludeComments)
		if hErr != nil {
			return BlueprintBundleView{}, hErr
		}
		tasks = hydrated
	}
	if tasks == nil {
		tasks = []BundledTaskView{}
	}

	md := renderCLIGoalBundleMarkdown(pv, projMD, obj, tasks, opts)
	objCopy := obj
	return BlueprintBundleView{
		Kind:            "goal",
		Project:         pv,
		ProjectMarkdown: projMD,
		Goal:            &objCopy,
		Tasks:           tasks,
		Markdown:        md,
	}, nil
}

func composeCLIProjectBundle(
	ctx context.Context,
	ws, proj string,
	opts cliBundleOptions,
	projClient ProjectClient,
	objClient ObjectiveClient,
	taskClient TaskClient,
) (BlueprintBundleView, error) {
	pv, err := projClient.GetProject(ctx, ws, proj)
	if err != nil {
		return BlueprintBundleView{}, err
	}
	var projMD string
	if opts.IncludeProjectDoc && pv.HasMarkdownDoc {
		if doc, dErr := projClient.GetProjectMarkdown(ctx, ws, proj); dErr == nil && doc.HasMarkdownDoc {
			projMD = doc.Markdown
		}
	}

	var goals []ObjectiveView
	if objClient != nil {
		objRes, oErr := objClient.ListObjectives(ctx, ListObjectivesInput{
			Workspace: ws,
			Project:   proj,
			Limit:     500,
		})
		if oErr != nil {
			return BlueprintBundleView{}, oErr
		}
		for _, g := range objRes.Items {
			st := strings.ToLower(strings.TrimSpace(g.Status))
			if !opts.IncludeDone && (st == "achieved" || st == "completed" || st == "dropped" || st == "canceled") {
				continue
			}
			goals = append(goals, g)
		}
	}
	if goals == nil {
		goals = []ObjectiveView{}
	}

	var tasks []BundledTaskView
	if taskClient != nil {
		listRes, lErr := taskClient.ListTasks(ctx, ListTasksInput{
			Workspace: ws,
			Project:   proj,
			Priority:  -1,
			Limit:     500,
		})
		if lErr != nil {
			return BlueprintBundleView{}, lErr
		}
		var filtered []TaskSummaryView
		for _, item := range listRes.Items {
			if !opts.IncludeDone && isCLITerminalTask(item) {
				continue
			}
			filtered = append(filtered, item)
		}
		hydrated, hErr := hydrateCLIBundledTasks(ctx, ws, proj, taskClient, filtered, opts.IncludeComments)
		if hErr != nil {
			return BlueprintBundleView{}, hErr
		}
		tasks = hydrated
	}
	if tasks == nil {
		tasks = []BundledTaskView{}
	}

	md := renderCLIProjectBundleMarkdown(pv, projMD, goals, tasks, opts)
	return BlueprintBundleView{
		Kind:            "project",
		Project:         pv,
		ProjectMarkdown: projMD,
		Goals:           goals,
		Tasks:           tasks,
		Markdown:        md,
	}, nil
}

func writeCLIBundleTaskTable(b *strings.Builder, items []BundledTaskView) {
	if len(items) == 0 {
		b.WriteString("_No tasks match the selected bundle filter._\n\n")
		return
	}
	b.WriteString("| Key | Priority | State | Holder | Parent | Depends On | Spec | Title |\n")
	b.WriteString("| :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- |\n")
	for _, bt := range items {
		t := bt.Task
		holder := "—"
		if t.Lease != nil && strings.TrimSpace(t.Lease.Holder) != "" {
			holder = "@" + t.Lease.Holder
		} else if strings.TrimSpace(t.Assignee) != "" {
			holder = "->" + t.Assignee
		}
		parent := "—"
		if t.ParentKey != "" {
			parent = "`" + t.ParentKey + "`"
		}
		deps := "—"
		if len(t.BlockedBy) > 0 {
			deps = "`" + strings.Join(t.BlockedBy, "`, `") + "`"
		}
		specBadge := "—"
		if strings.TrimSpace(bt.Markdown) != "" {
			specBadge = "`.md`"
		}
		stateStr := t.State
		if t.Blocked && !strings.EqualFold(t.State, "Blocked") {
			stateStr += " (blocked)"
		}
		fmt.Fprintf(b, "| `%s` | `P%d` | `%s` | %s | %s | %s | %s | %s |\n",
			t.Key,
			t.Priority,
			escapeCLITableCell(stateStr),
			escapeCLITableCell(holder),
			parent,
			deps,
			specBadge,
			escapeCLITableCell(t.Title),
		)
	}
	b.WriteString("\n")
}

func writeCLIBundledTaskSection(b *strings.Builder, bt BundledTaskView) {
	t := bt.Task
	stateLabel := t.State
	if t.Blocked && !strings.EqualFold(t.State, "Blocked") {
		stateLabel += " · BLOCKED"
	}
	fmt.Fprintf(b, "### `%s` — %s [`P%d` · `%s`]\n\n", t.Key, strings.TrimSpace(t.Title), t.Priority, stateLabel)

	var meta []string
	meta = append(meta, fmt.Sprintf("**Priority**: `%s`", formatCLIBundlePriority(t.Priority)))
	if t.ExternalID != "" {
		meta = append(meta, fmt.Sprintf("**External ID**: `%s`", t.ExternalID))
	}
	if t.ObjectiveKey != "" {
		meta = append(meta, fmt.Sprintf("**Goal**: `%s`", t.ObjectiveKey))
	}
	if t.ParentKey != "" {
		meta = append(meta, fmt.Sprintf("**Parent**: `%s`", t.ParentKey))
	}
	if t.Lease != nil && t.Lease.Holder != "" {
		meta = append(meta, fmt.Sprintf("**Leased By**: `@%s`", t.Lease.Holder))
	}
	if len(t.BlockedBy) > 0 {
		meta = append(meta, fmt.Sprintf("**Depends On**: `%s`", strings.Join(t.BlockedBy, "`, `")))
	}
	if len(t.Blocks) > 0 {
		meta = append(meta, fmt.Sprintf("**Blocks**: `%s`", strings.Join(t.Blocks, "`, `")))
	}
	if len(t.Labels) > 0 {
		meta = append(meta, fmt.Sprintf("**Labels**: `#%s`", strings.Join(t.Labels, "`, `#")))
	}
	fmt.Fprintf(b, "- %s\n", strings.Join(meta, " | "))

	if desc := strings.TrimSpace(t.Description); desc != "" {
		fmt.Fprintf(b, "\n%s\n", desc)
	}
	if acc := strings.TrimSpace(t.Acceptance); acc != "" {
		b.WriteString("\n**Acceptance Criteria:**\n")
		check := "[ ]"
		if strings.EqualFold(t.State, "Done") || strings.EqualFold(t.Category, "done") {
			check = "[x]"
		}
		for _, line := range strings.Split(acc, "\n") {
			if trimmed := strings.TrimSpace(line); trimmed != "" {
				fmt.Fprintf(b, "- %s %s\n", check, trimmed)
			}
		}
	}
	if md := strings.TrimSpace(bt.Markdown); md != "" {
		fmt.Fprintf(b, "\n#### Task Specification (`%s.md`)\n\n%s\n", t.Key, md)
	}
	if len(t.Comments) > 0 {
		b.WriteString("\n#### Timeline & Handoff Notes\n")
		for _, cm := range t.Comments {
			author := cm.Author
			if author == "" {
				author = "actor"
			}
			fmt.Fprintf(b, "- **[%s · @%s]** %s\n", cm.Kind, author, strings.TrimSpace(cm.Body))
		}
	}
	b.WriteString("\n---\n\n")
}

func renderCLIGoalBundleMarkdown(
	pv ProjectView,
	projMD string,
	obj ObjectiveView,
	tasks []BundledTaskView,
	opts cliBundleOptions,
) string {
	var b strings.Builder
	kind := obj.Kind
	if kind == "" {
		kind = "goal"
	}
	fmt.Fprintf(&b, "# Goal Blueprint: %s (`%s`)\n\n", strings.TrimSpace(obj.Title), obj.Key)
	fmt.Fprintf(&b, "- **Project**: `%s` — %s\n", pv.Key, firstNonEmptyCLI(pv.Name, pv.Key))
	fmt.Fprintf(&b, "- **Kind / Status / Priority**: `%s` | `%s` | `%s`\n", kind, obj.Status, formatCLIBundlePriority(obj.Priority))
	fmt.Fprintf(&b, "- **Progress**: `%d/%d` done (`%d` blocked)\n", obj.CompletedTasks, obj.TotalTasks, obj.BlockedTasks)
	if strings.TrimSpace(obj.TargetDate) != "" {
		fmt.Fprintf(&b, "- **Target Date**: `%s`\n", strings.TrimSpace(obj.TargetDate))
	}
	b.WriteString("\n")

	if sum := strings.TrimSpace(obj.Summary); sum != "" {
		fmt.Fprintf(&b, "## Goal Summary\n\n%s\n\n", sum)
	}
	if len(obj.SuccessCriteria) > 0 {
		b.WriteString("## Success Criteria\n\n")
		box := "[ ]"
		if strings.EqualFold(obj.Status, "achieved") || strings.EqualFold(obj.Status, "completed") {
			box = "[x]"
		}
		for _, c := range obj.SuccessCriteria {
			if trimmed := strings.TrimSpace(c); trimmed != "" {
				fmt.Fprintf(&b, "- %s %s\n", box, trimmed)
			}
		}
		b.WriteString("\n")
	}
	if opts.IncludeProjectDoc && strings.TrimSpace(projMD) != "" {
		fmt.Fprintf(&b, "## Project Charter (`%s.md`)\n\n%s\n\n", pv.Key, strings.TrimSpace(projMD))
	}

	fmt.Fprintf(&b, "## Task Execution Map (%d shown)\n\n", len(tasks))
	writeCLIBundleTaskTable(&b, tasks)

	if len(tasks) > 0 {
		b.WriteString("## Detailed Task Specifications\n\n")
		for _, bt := range tasks {
			writeCLIBundledTaskSection(&b, bt)
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func renderCLIProjectBundleMarkdown(
	pv ProjectView,
	projMD string,
	goals []ObjectiveView,
	tasks []BundledTaskView,
	opts cliBundleOptions,
) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Project Blueprint: %s — %s\n\n", pv.Key, firstNonEmptyCLI(pv.Name, pv.Key))
	fmt.Fprintf(&b, "- **Status**: `%s`\n", firstNonEmptyCLI(pv.Status, "active"))
	if desc := strings.TrimSpace(pv.Description); desc != "" {
		fmt.Fprintf(&b, "- **Description**: %s\n", desc)
	}
	b.WriteString("\n")

	if opts.IncludeProjectDoc && strings.TrimSpace(projMD) != "" {
		fmt.Fprintf(&b, "## Project Charter (`%s.md`)\n\n%s\n\n", pv.Key, strings.TrimSpace(projMD))
	}

	tasksByGoal := make(map[string][]BundledTaskView)
	var unscoped []BundledTaskView
	for _, bt := range tasks {
		gk := strings.ToLower(strings.TrimSpace(bt.Task.ObjectiveKey))
		if gk == "" {
			unscoped = append(unscoped, bt)
		} else {
			tasksByGoal[gk] = append(tasksByGoal[gk], bt)
		}
	}

	if len(goals) > 0 {
		b.WriteString("## Strategic Goals & Milestones\n\n")
		for _, g := range goals {
			kind := firstNonEmptyCLI(g.Kind, "goal")
			fmt.Fprintf(&b, "### Goal: %s (`%s`) [`%s` · `%s` · `%s`]\n\n",
				strings.TrimSpace(g.Title), g.Key, kind, g.Status, formatCLIBundlePriority(g.Priority))
			if sum := strings.TrimSpace(g.Summary); sum != "" {
				fmt.Fprintf(&b, "%s\n\n", sum)
			}
			if len(g.SuccessCriteria) > 0 {
				b.WriteString("**Success Criteria:**\n")
				box := "[ ]"
				if strings.EqualFold(g.Status, "achieved") || strings.EqualFold(g.Status, "completed") {
					box = "[x]"
				}
				for _, c := range g.SuccessCriteria {
					if trimmed := strings.TrimSpace(c); trimmed != "" {
						fmt.Fprintf(&b, "- %s %s\n", box, trimmed)
					}
				}
				b.WriteString("\n")
			}
			goalTasks := tasksByGoal[strings.ToLower(g.Key)]
			writeCLIBundleTaskTable(&b, goalTasks)
			for _, bt := range goalTasks {
				writeCLIBundledTaskSection(&b, bt)
			}
		}
	}

	if len(unscoped) > 0 {
		b.WriteString("## Unscoped Project Tasks\n\n")
		writeCLIBundleTaskTable(&b, unscoped)
		for _, bt := range unscoped {
			writeCLIBundledTaskSection(&b, bt)
		}
	}

	return strings.TrimRight(b.String(), "\n") + "\n"
}
