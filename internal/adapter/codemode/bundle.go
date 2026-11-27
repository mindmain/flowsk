// File bundle.go composes full supervisor/executor Markdown blueprints for
// strategic goals (`flowsk.goals.bundle` / `flowsk.goals.getDoc`) and logical
// projects (`flowsk.projects.bundle`) inside the Code Mode JavaScript sandbox,
// aggregating project charters, goal success criteria, DAG dependency edges,
// timeline comments, and per-task `.md` specifications in a single ACID call.
package codemode

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dop251/goja"
	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/domain/objective"
	"github.com/mindmain/flowsk/internal/domain/project"
	"github.com/mindmain/flowsk/internal/domain/task"
)

type bundleOptions struct {
	projectRef        string
	includeDone       bool
	includeProjectDoc bool
	includeComments   bool
}

func defaultBundleOptions() bundleOptions {
	return bundleOptions{
		includeDone:       true,
		includeProjectDoc: true,
		includeComments:   true,
	}
}

func applyBundleOptionsMap(opts *bundleOptions, m map[string]any) {
	if p := strField(m, "project"); p != "" {
		opts.projectRef = p
	}
	if b, ok := m["includeDone"].(bool); ok {
		opts.includeDone = b
	} else if b, ok := m["includeCompleted"].(bool); ok {
		opts.includeDone = b
	}
	if b, ok := m["includeProjectDoc"].(bool); ok {
		opts.includeProjectDoc = b
	}
	if b, ok := m["includeComments"].(bool); ok {
		opts.includeComments = b
	}
}

func parseGoalBundleArgs(vm *goja.Runtime, call goja.FunctionCall, methodName string) (string, bundleOptions) {
	ref := strings.TrimSpace(requireJSStringArg(vm, call, 0, methodName, "slugOrId"))
	if ref == "" {
		panic(vm.NewTypeError(fmt.Sprintf("%s: argument 'slugOrId' must not be empty", methodName)))
	}
	opts := defaultBundleOptions()
	if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
		if m, ok := exportJSValue(call.Arguments[1]).(map[string]any); ok {
			applyBundleOptionsMap(&opts, m)
		} else {
			panic(vm.NewTypeError(fmt.Sprintf("%s: expected options object as second argument", methodName)))
		}
	}
	return ref, opts
}

func parseProjectBundleArgs(vm *goja.Runtime, call goja.FunctionCall) bundleOptions {
	opts := defaultBundleOptions()
	if len(call.Arguments) == 0 || goja.IsUndefined(call.Arguments[0]) || goja.IsNull(call.Arguments[0]) {
		return opts
	}
	switch v := exportJSValue(call.Arguments[0]).(type) {
	case string:
		opts.projectRef = strings.TrimSpace(v)
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
			if m, ok := exportJSValue(call.Arguments[1]).(map[string]any); ok {
				applyBundleOptionsMap(&opts, m)
			} else {
				panic(vm.NewTypeError("flowsk.projects.bundle: expected options object as second argument"))
			}
		}
	case map[string]any:
		applyBundleOptionsMap(&opts, v)
	default:
		panic(vm.NewTypeError("flowsk.projects.bundle: expected optional project slug/key string or options object"))
	}
	return opts
}

type hydratedBundleTask struct {
	summary   task.CompactTaskSummary
	full      task.Task
	markdown  string
	dependsOn []string
	blocks    []string
	comments  []task.TaskComment
	jsMap     map[string]any
}

func extractTaskKeyNumber(key string) (string, int) {
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

func sortBundleSummaries(items []task.CompactTaskSummary) {
	slices.SortFunc(items, func(a, b task.CompactTaskSummary) int {
		pa, na := extractTaskKeyNumber(a.Key)
		pb, nb := extractTaskKeyNumber(b.Key)
		if pa == pb && na > 0 && nb > 0 {
			return cmp.Compare(na, nb)
		}
		if c := cmp.Compare(a.Priority.SortOrder(), b.Priority.SortOrder()); c != 0 {
			return c
		}
		return cmp.Compare(a.Key, b.Key)
	})
}

func computeBundleStats(items []task.CompactTaskSummary) map[string]any {
	var total, done, inProg, inRev, ready, backlog, blocked int
	for _, it := range items {
		total++
		switch it.Status {
		case task.StatusDone:
			done++
		case task.StatusInProgress:
			inProg++
		case task.StatusInReview:
			inRev++
		case task.StatusReady:
			ready++
		case task.StatusBacklog:
			backlog++
		case task.StatusBlocked:
			blocked++
		}
		if it.Status != task.StatusBlocked && it.UnresolvedBlockerCount > 0 {
			blocked++
		}
	}
	return map[string]any{
		"total":      total,
		"done":       done,
		"inProgress": inProg,
		"inReview":   inRev,
		"ready":      ready,
		"backlog":    backlog,
		"blocked":    blocked,
	}
}

func (s *scriptSession) hydrateBundleTasks(
	allProjectTasks []task.CompactTaskSummary,
	selected []task.CompactTaskSummary,
	includeComments bool,
) ([]hydratedBundleTask, error) {
	sorted := append([]task.CompactTaskSummary(nil), selected...)
	sortBundleSummaries(sorted)

	keyByID := make(map[uuid.UUID]string, len(allProjectTasks))
	for _, it := range allProjectTasks {
		keyByID[it.ID] = it.Key
	}

	var prefetched map[uuid.UUID]task.Task
	if len(sorted) > 0 && s.taskRepo != nil {
		if wsTasks, err := s.taskRepo.ListByWorkspace(s.ctx, s.wsID); err == nil {
			prefetched = make(map[uuid.UUID]task.Task, len(wsTasks))
			for _, wt := range wsTasks {
				prefetched[wt.ID] = wt
				if _, ok := keyByID[wt.ID]; !ok {
					keyByID[wt.ID] = wt.Key
				}
			}
		}
	}

	out := make([]hydratedBundleTask, 0, len(sorted))
	for _, it := range sorted {
		full, ok := s.lookupFullTask(it.ID, prefetched)
		if !ok {
			continue
		}
		tm := s.fullTaskMap(full)
		md, _ := tm["markdown"].(string)

		var dependsOn, blocks []string
		if rels, rErr := s.taskSvc.ListRelations(s.ctx, s.wsID, it.ID); rErr == nil {
			for _, r := range rels {
				if r.Type != task.RelationBlocks {
					continue
				}
				if r.TargetTaskID == it.ID {
					if k := keyByID[r.SourceTaskID]; k != "" && !slices.Contains(dependsOn, k) {
						dependsOn = append(dependsOn, k)
					}
				} else if r.SourceTaskID == it.ID {
					if k := keyByID[r.TargetTaskID]; k != "" && !slices.Contains(blocks, k) {
						blocks = append(blocks, k)
					}
				}
			}
		}
		slices.Sort(dependsOn)
		slices.Sort(blocks)
		if dependsOn == nil {
			dependsOn = []string{}
		}
		if blocks == nil {
			blocks = []string{}
		}
		tm["dependsOn"] = dependsOn
		tm["blocks"] = blocks

		var comments []task.TaskComment
		commentMaps := []map[string]any{}
		if includeComments {
			if cms, cErr := s.taskSvc.ListComments(s.ctx, s.wsID, it.ID, 30); cErr == nil {
				comments = cms
				commentMaps = make([]map[string]any, 0, len(cms))
				for _, cm := range cms {
					commentMaps = append(commentMaps, map[string]any{
						"id":        cm.ID.String(),
						"kind":      string(cm.Kind),
						"author":    cm.AuthorHandle,
						"body":      cm.Body,
						"createdAt": cm.CreatedAt.UTC().Format(time.RFC3339),
					})
				}
			}
		}
		tm["comments"] = commentMaps

		out = append(out, hydratedBundleTask{
			summary:   it,
			full:      full,
			markdown:  md,
			dependsOn: dependsOn,
			blocks:    blocks,
			comments:  comments,
			jsMap:     tm,
		})
	}
	return out, nil
}

func (s *scriptSession) buildGoalBundle(ref string, opts bundleOptions) (map[string]any, error) {
	p, err := s.resolveProject(opts.projectRef)
	if err != nil {
		return nil, err
	}
	obj, err := s.resolveObjective(p.ID, ref)
	if err != nil {
		return nil, err
	}
	allTasks, err := s.listAllProjectTasks(p.ID)
	if err != nil {
		return nil, err
	}

	var allGoalTasks, filteredGoalTasks []task.CompactTaskSummary
	for _, it := range allTasks {
		if !strings.EqualFold(it.ObjectiveSlug, obj.Slug) {
			continue
		}
		allGoalTasks = append(allGoalTasks, it)
		if !opts.includeDone && it.Status.IsTerminal() {
			continue
		}
		filteredGoalTasks = append(filteredGoalTasks, it)
	}

	hydrated, err := s.hydrateBundleTasks(allTasks, filteredGoalTasks, opts.includeComments)
	if err != nil {
		return nil, err
	}

	projMD := ""
	if opts.includeProjectDoc && p.HasMarkdownDoc {
		if md, hasDoc, mdErr := s.projSvc.GetProjectMarkdown(s.ctx, s.wsID, p.ID); mdErr == nil && hasDoc {
			projMD = md
		}
	}

	projView := projectMap(p)
	projView["markdown"] = projMD
	goalView := objectiveMap(obj, allTasks)
	stats := computeBundleStats(allGoalTasks)

	jsTasks := make([]map[string]any, 0, len(hydrated))
	for _, ht := range hydrated {
		jsTasks = append(jsTasks, ht.jsMap)
	}

	md := composeGoalBundleMarkdown(p, projMD, obj, stats, hydrated, opts)
	return map[string]any{
		"project":  projView,
		"goal":     goalView,
		"stats":    stats,
		"tasks":    jsTasks,
		"markdown": md,
	}, nil
}

func (s *scriptSession) buildProjectBundle(opts bundleOptions) (map[string]any, error) {
	p, err := s.resolveProject(opts.projectRef)
	if err != nil {
		return nil, err
	}
	objs, err := s.objSvc.ListObjectives(s.ctx, objective.ListObjectivesFilter{
		WorkspaceID: s.wsID,
		ProjectID:   p.ID,
		Limit:       500,
	})
	if err != nil {
		return nil, err
	}
	allTasks, err := s.listAllProjectTasks(p.ID)
	if err != nil {
		return nil, err
	}

	var filteredTasks []task.CompactTaskSummary
	for _, it := range allTasks {
		if !opts.includeDone && it.Status.IsTerminal() {
			continue
		}
		filteredTasks = append(filteredTasks, it)
	}

	hydrated, err := s.hydrateBundleTasks(allTasks, filteredTasks, opts.includeComments)
	if err != nil {
		return nil, err
	}

	projMD := ""
	if opts.includeProjectDoc && p.HasMarkdownDoc {
		if md, hasDoc, mdErr := s.projSvc.GetProjectMarkdown(s.ctx, s.wsID, p.ID); mdErr == nil && hasDoc {
			projMD = md
		}
	}

	projView := projectMap(p)
	projView["markdown"] = projMD

	goalViews := make([]map[string]any, 0, len(objs))
	for _, o := range objs {
		if !opts.includeDone && (o.Status == objective.StatusAchieved || o.Status == objective.StatusDropped) {
			continue
		}
		goalViews = append(goalViews, objectiveMap(o, allTasks))
	}

	jsTasks := make([]map[string]any, 0, len(hydrated))
	for _, ht := range hydrated {
		jsTasks = append(jsTasks, ht.jsMap)
	}

	stats := computeBundleStats(allTasks)
	md := composeProjectBundleMarkdown(p, projMD, objs, stats, hydrated, opts)
	return map[string]any{
		"project":  projView,
		"goals":    goalViews,
		"stats":    stats,
		"tasks":    jsTasks,
		"markdown": md,
	}, nil
}

func formatBundlePriority(p int) string {
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

func escapeMDTableCell(s string) string {
	cleaned := strings.ReplaceAll(strings.TrimSpace(s), "|", "\\|")
	cleaned = strings.ReplaceAll(cleaned, "\n", " ")
	return cleaned
}

func writeTaskExecutionTable(b *strings.Builder, items []hydratedBundleTask) {
	if len(items) == 0 {
		b.WriteString("_No tasks match the selected bundle filter._\n\n")
		return
	}
	b.WriteString("| Key | Priority | State | Holder | Parent | Depends On | Spec | Title |\n")
	b.WriteString("| :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- |\n")
	for _, ht := range items {
		holder := "—"
		if strings.TrimSpace(ht.summary.HolderHandle) != "" {
			holder = "@" + ht.summary.HolderHandle
		}
		parent := "—"
		if ht.summary.ParentKey != "" {
			parent = "`" + ht.summary.ParentKey + "`"
		}
		deps := "—"
		if len(ht.dependsOn) > 0 {
			deps = "`" + strings.Join(ht.dependsOn, "`, `") + "`"
		}
		specBadge := "—"
		if strings.TrimSpace(ht.markdown) != "" {
			specBadge = "`.md`"
		}
		stateStr := string(ht.full.Status)
		if ht.full.UnresolvedBlockerCount > 0 && ht.full.Status != task.StatusBlocked {
			stateStr += " (blocked)"
		}
		fmt.Fprintf(b, "| `%s` | `P%d` | `%s` | %s | %s | %s | %s | %s |\n",
			ht.full.Key,
			int(ht.full.Priority),
			stateStr,
			escapeMDTableCell(holder),
			parent,
			deps,
			specBadge,
			escapeMDTableCell(ht.full.Title),
		)
	}
	b.WriteString("\n")
}

func writeHydratedTaskSection(b *strings.Builder, ht hydratedBundleTask) {
	t := ht.full
	stateLabel := string(t.Status)
	if t.UnresolvedBlockerCount > 0 && t.Status != task.StatusBlocked {
		stateLabel += " · BLOCKED"
	}
	fmt.Fprintf(b, "### `%s` — %s [`P%d` · `%s`]\n\n", t.Key, strings.TrimSpace(t.Title), int(t.Priority), stateLabel)

	var metaParts []string
	metaParts = append(metaParts, fmt.Sprintf("**Kind**: `%s`", t.Kind))
	metaParts = append(metaParts, fmt.Sprintf("**Priority**: `%s`", formatBundlePriority(int(t.Priority))))
	if t.ExternalID != "" {
		metaParts = append(metaParts, fmt.Sprintf("**External ID**: `%s`", t.ExternalID))
	}
	if ht.summary.ObjectiveSlug != "" {
		metaParts = append(metaParts, fmt.Sprintf("**Goal**: `%s`", ht.summary.ObjectiveSlug))
	}
	if ht.summary.ParentKey != "" {
		metaParts = append(metaParts, fmt.Sprintf("**Parent**: `%s`", ht.summary.ParentKey))
	}
	if ht.summary.HolderHandle != "" {
		metaParts = append(metaParts, fmt.Sprintf("**Leased By**: `@%s`", ht.summary.HolderHandle))
	}
	if len(ht.dependsOn) > 0 {
		metaParts = append(metaParts, fmt.Sprintf("**Depends On**: `%s`", strings.Join(ht.dependsOn, "`, `")))
	}
	if len(ht.blocks) > 0 {
		metaParts = append(metaParts, fmt.Sprintf("**Blocks**: `%s`", strings.Join(ht.blocks, "`, `")))
	}
	if tags := t.TagsCopy(); len(tags) > 0 {
		metaParts = append(metaParts, fmt.Sprintf("**Tags**: `#%s`", strings.Join(tags, "`, `#")))
	}
	fmt.Fprintf(b, "- %s\n", strings.Join(metaParts, " | "))

	if desc := strings.TrimSpace(t.Description); desc != "" {
		fmt.Fprintf(b, "\n%s\n", desc)
	}

	if acc := t.AcceptanceCriteriaCopy(); len(acc) > 0 {
		b.WriteString("\n**Acceptance Criteria:**\n")
		check := "[ ]"
		if t.Status == task.StatusDone {
			check = "[x]"
		}
		for _, item := range acc {
			if trimmed := strings.TrimSpace(item); trimmed != "" {
				fmt.Fprintf(b, "- %s %s\n", check, trimmed)
			}
		}
	}

	if md := strings.TrimSpace(ht.markdown); md != "" {
		fmt.Fprintf(b, "\n#### Task Specification (`%s.md`)\n\n%s\n", t.Key, md)
	}

	if len(ht.comments) > 0 {
		b.WriteString("\n#### Timeline & Handoff Notes\n")
		for _, cm := range ht.comments {
			author := cm.AuthorHandle
			if author == "" {
				author = "actor"
			}
			fmt.Fprintf(b, "- **[%s · @%s]** %s\n", cm.Kind, author, strings.TrimSpace(cm.Body))
		}
	}
	b.WriteString("\n---\n\n")
}

func composeGoalBundleMarkdown(
	p project.Project,
	projMD string,
	obj objective.Objective,
	stats map[string]any,
	tasks []hydratedBundleTask,
	opts bundleOptions,
) string {
	var b strings.Builder
	kind := string(obj.Kind)
	if kind == "" {
		kind = string(objective.KindGoal)
	}
	fmt.Fprintf(&b, "# Goal Blueprint: %s (`%s`)\n\n", strings.TrimSpace(obj.Title), obj.Slug)
	fmt.Fprintf(&b, "- **Project**: `%s` — %s (`%s`)\n", p.KeyPrefix, p.Name, p.Slug)
	fmt.Fprintf(&b, "- **Kind / Status / Priority**: `%s` | `%s` | `%s`\n", kind, obj.Status, formatBundlePriority(int(obj.PriorityRank)))
	fmt.Fprintf(&b, "- **Progress**: `%v/%v` done (`%v` in progress, `%v` in review, `%v` ready, `%v` backlog, `%v` blocked)\n",
		stats["done"], stats["total"], stats["inProgress"], stats["inReview"], stats["ready"], stats["backlog"], stats["blocked"])
	if obj.TargetDate != nil && !obj.TargetDate.IsZero() {
		fmt.Fprintf(&b, "- **Target Date**: `%s`\n", obj.TargetDate.UTC().Format("2006-01-02"))
	}
	b.WriteString("\n")

	if sum := strings.TrimSpace(obj.Summary); sum != "" {
		fmt.Fprintf(&b, "## Goal Summary\n\n%s\n\n", sum)
	}

	if criteria := obj.SuccessCriteriaCopy(); len(criteria) > 0 {
		b.WriteString("## Success Criteria\n\n")
		box := "[ ]"
		if obj.Status == objective.StatusAchieved {
			box = "[x]"
		}
		for _, c := range criteria {
			if trimmed := strings.TrimSpace(c); trimmed != "" {
				fmt.Fprintf(&b, "- %s %s\n", box, trimmed)
			}
		}
		b.WriteString("\n")
	}

	if opts.includeProjectDoc && strings.TrimSpace(projMD) != "" {
		fmt.Fprintf(&b, "## Project Charter (`%s.md`)\n\n%s\n\n", p.KeyPrefix, strings.TrimSpace(projMD))
	}

	fmt.Fprintf(&b, "## Task Execution Map (%d shown)\n\n", len(tasks))
	writeTaskExecutionTable(&b, tasks)

	if len(tasks) > 0 {
		b.WriteString("## Detailed Task Specifications\n\n")
		for _, ht := range tasks {
			writeHydratedTaskSection(&b, ht)
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func composeProjectBundleMarkdown(
	p project.Project,
	projMD string,
	objs []objective.Objective,
	stats map[string]any,
	tasks []hydratedBundleTask,
	opts bundleOptions,
) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Project Blueprint: %s — %s (`%s`)\n\n", p.KeyPrefix, p.Name, p.Slug)
	fmt.Fprintf(&b, "- **Status**: `%s`\n", p.Status)
	fmt.Fprintf(&b, "- **Tasks Overview**: `%v/%v` done (`%v` in progress, `%v` in review, `%v` ready, `%v` backlog, `%v` blocked)\n",
		stats["done"], stats["total"], stats["inProgress"], stats["inReview"], stats["ready"], stats["backlog"], stats["blocked"])
	if desc := strings.TrimSpace(p.Description); desc != "" {
		fmt.Fprintf(&b, "- **Description**: %s\n", desc)
	}
	b.WriteString("\n")

	if opts.includeProjectDoc && strings.TrimSpace(projMD) != "" {
		fmt.Fprintf(&b, "## Project Charter (`%s.md`)\n\n%s\n\n", p.KeyPrefix, strings.TrimSpace(projMD))
	}

	tasksByGoal := make(map[string][]hydratedBundleTask)
	var unscoped []hydratedBundleTask
	for _, ht := range tasks {
		slug := strings.ToLower(strings.TrimSpace(ht.summary.ObjectiveSlug))
		if slug == "" {
			unscoped = append(unscoped, ht)
		} else {
			tasksByGoal[slug] = append(tasksByGoal[slug], ht)
		}
	}

	if len(objs) > 0 {
		b.WriteString("## Strategic Goals & Milestones\n\n")
		for _, o := range objs {
			if !opts.includeDone && (o.Status == objective.StatusAchieved || o.Status == objective.StatusDropped) {
				continue
			}
			kind := string(o.Kind)
			if kind == "" {
				kind = string(objective.KindGoal)
			}
			fmt.Fprintf(&b, "### Goal: %s (`%s`) [`%s` · `%s` · `%s`]\n\n",
				strings.TrimSpace(o.Title), o.Slug, kind, o.Status, formatBundlePriority(int(o.PriorityRank)))
			if sum := strings.TrimSpace(o.Summary); sum != "" {
				fmt.Fprintf(&b, "%s\n\n", sum)
			}
			if criteria := o.SuccessCriteriaCopy(); len(criteria) > 0 {
				b.WriteString("**Success Criteria:**\n")
				box := "[ ]"
				if o.Status == objective.StatusAchieved {
					box = "[x]"
				}
				for _, c := range criteria {
					if trimmed := strings.TrimSpace(c); trimmed != "" {
						fmt.Fprintf(&b, "- %s %s\n", box, trimmed)
					}
				}
				b.WriteString("\n")
			}
			goalTasks := tasksByGoal[strings.ToLower(o.Slug)]
			writeTaskExecutionTable(&b, goalTasks)
			for _, ht := range goalTasks {
				writeHydratedTaskSection(&b, ht)
			}
		}
	}

	if len(unscoped) > 0 {
		b.WriteString("## Unscoped Project Tasks\n\n")
		writeTaskExecutionTable(&b, unscoped)
		for _, ht := range unscoped {
			writeHydratedTaskSection(&b, ht)
		}
	}

	return strings.TrimRight(b.String(), "\n") + "\n"
}
