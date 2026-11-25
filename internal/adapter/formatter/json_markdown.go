// File json_markdown.go implements structured JSON (`FormatJSON`), streaming
// newline-delimited JSON (`FormatJSONL`), and GitHub-Flavored Markdown
// (`FormatMarkdown` / `FormatMarkdownShort`) across all 10 OutputFormatter methods.
package formatter

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/mindmain/flowsk/internal/domain/audit"
	"github.com/mindmain/flowsk/internal/domain/project"
	"github.com/mindmain/flowsk/internal/domain/task"
	"github.com/mindmain/flowsk/internal/domain/workspace"
)

func escapeMD(s string) string {
	s = cleanOneLine(s)
	return strings.ReplaceAll(s, "|", "\\|")
}

func workspaceToJSONMap(ws workspace.Workspace, opts TokenBudgetOptions) map[string]any {
	m := map[string]any{
		"slug":                      ws.Slug,
		"name":                      ws.Name,
		"default_lease_ttl_seconds": ws.DefaultLeaseTTLSeconds,
	}
	if !opts.OmitUUIDs {
		m["id"] = ws.ID.String()
	}
	if ws.Description != "" {
		m["description"] = truncateRunes(ws.Description, opts.TruncateBodyChars)
	}
	if len(ws.Labels) > 0 {
		m["labels"] = ws.LabelsCopy()
	}
	return m
}

func projectToJSONMap(p project.Project, opts TokenBudgetOptions) map[string]any {
	m := map[string]any{
		"slug":       p.Slug,
		"key_prefix": p.KeyPrefix,
		"name":       p.Name,
		"status":     p.Status,
	}
	if !opts.OmitUUIDs {
		m["id"] = p.ID.String()
		m["workspace_id"] = p.WorkspaceID.String()
	}
	if p.Description != "" {
		m["description"] = truncateRunes(p.Description, opts.TruncateBodyChars)
	}
	if p.RemoteRepoURL != "" {
		m["remote_repo_url"] = p.RemoteRepoURL
	}
	if p.DefaultBranch != "" {
		m["default_branch"] = p.DefaultBranch
	}
	if len(p.Tags) > 0 {
		m["tags"] = p.TagsCopy()
	}
	return m
}

func objectiveProgressToJSONMap(it ObjectiveProgress, opts TokenBudgetOptions) map[string]any {
	o := it.Objective
	m := map[string]any{
		"slug":            o.Slug,
		"title":           o.Title,
		"status":          o.Status,
		"priority_rank":   o.PriorityRank,
		"completed_tasks": it.CompletedTasks,
		"total_tasks":     it.TotalTasks,
		"blocked_tasks":   it.BlockedTasks,
	}
	if !opts.OmitUUIDs {
		m["id"] = o.ID.String()
		m["project_id"] = o.ProjectID.String()
	}
	if o.Summary != "" {
		m["summary"] = truncateRunes(o.Summary, opts.TruncateBodyChars)
	}
	if len(o.SuccessCriteria) > 0 {
		m["success_criteria"] = o.SuccessCriteriaCopy()
	}
	if o.TargetDate != nil {
		m["target_date"] = o.TargetDate.UTC().Format("2006-01-02")
	}
	return m
}

func taskToJSONMap(t task.Task, opts TokenBudgetOptions) map[string]any {
	m := map[string]any{
		"key":                      t.Key,
		"kind":                     t.Kind,
		"title":                    t.Title,
		"status":                   t.Status,
		"priority":                 t.Priority,
		"unresolved_blocker_count": t.UnresolvedBlockerCount,
		"version":                  t.Version,
	}
	if !opts.OmitUUIDs {
		m["id"] = t.ID.String()
		m["workspace_id"] = t.WorkspaceID.String()
		m["project_id"] = t.ProjectID.String()
	}
	if t.Description != "" {
		m["description"] = truncateRunes(t.Description, opts.TruncateBodyChars)
	}
	if len(t.AcceptanceCriteria) > 0 {
		m["acceptance_criteria"] = t.AcceptanceCriteriaCopy()
	}
	if len(t.Tags) > 0 {
		m["tags"] = t.TagsCopy()
	}
	if len(t.Labels) > 0 {
		m["labels"] = t.LabelsCopy()
	}
	if t.ActiveLease != nil {
		m["holder_handle"] = t.ActiveLease.HolderHandle
		m["lease_expires_at"] = t.ActiveLease.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	return m
}

func compactTaskToJSONMap(t task.CompactTaskSummary, opts TokenBudgetOptions) map[string]any {
	m := map[string]any{
		"key":      t.Key,
		"kind":     t.Kind,
		"title":    t.Title,
		"status":   t.Status,
		"priority": t.Priority,
	}
	if !opts.OmitUUIDs {
		m["id"] = t.ID.String()
	}
	if t.HolderHandle != "" {
		m["holder_handle"] = t.HolderHandle
	}
	if t.UnresolvedBlockerCount > 0 {
		m["unresolved_blocker_count"] = t.UnresolvedBlockerCount
	}
	if t.ObjectiveSlug != "" {
		m["objective_slug"] = t.ObjectiveSlug
	}
	if t.ParentKey != "" {
		m["parent_key"] = t.ParentKey
	}
	if len(t.Tags) > 0 {
		m["tags"] = append([]string(nil), t.Tags...)
	}
	return m
}

func digestToJSONMap(d task.ProjectDigest, opts TokenBudgetOptions) map[string]any {
	counts := make(map[string]int32, len(d.CountsByStatus))
	for k, v := range d.CountsByStatus {
		counts[string(k)] = v
	}
	ready := make([]map[string]any, 0, len(d.ReadyQueue))
	for _, t := range d.ReadyQueue {
		ready = append(ready, compactTaskToJSONMap(t, opts))
	}
	leases := make([]map[string]any, 0, len(d.ActiveLeases))
	for _, t := range d.ActiveLeases {
		leases = append(leases, compactTaskToJSONMap(t, opts))
	}
	blocked := make([]map[string]any, 0, len(d.BlockedTasks))
	for _, t := range d.BlockedTasks {
		blocked = append(blocked, compactTaskToJSONMap(t, opts))
	}
	objs := make([]map[string]any, 0, len(d.ActiveObjectives))
	for _, o := range d.ActiveObjectives {
		om := map[string]any{
			"slug":            o.Slug,
			"title":           o.Title,
			"priority_rank":   o.PriorityRank,
			"open_task_count": o.OpenTaskCount,
			"done_task_count": o.DoneTaskCount,
		}
		if !opts.OmitUUIDs {
			om["id"] = o.ID.String()
		}
		objs = append(objs, om)
	}
	m := map[string]any{
		"project_slug":       d.ProjectSlug,
		"project_key_prefix": d.ProjectKeyPrefix,
		"counts_by_status":   counts,
		"active_objectives":  objs,
		"ready_queue":        ready,
		"active_leases":      leases,
		"blocked_tasks":      blocked,
	}
	if !opts.OmitUUIDs {
		m["workspace_id"] = d.WorkspaceID.String()
		m["project_id"] = d.ProjectID.String()
	}
	return m
}

func boardToJSONMap(b BoardView, opts TokenBudgetOptions) map[string]any {
	objs := make([]map[string]any, 0, len(b.Objectives))
	for _, o := range b.Objectives {
		objs = append(objs, objectiveProgressToJSONMap(o, opts))
	}
	return map[string]any{
		"project":    projectToJSONMap(b.Project, opts),
		"objectives": objs,
		"digest":     digestToJSONMap(b.Digest, opts),
	}
}

func auditToJSONMap(r audit.Record, opts TokenBudgetOptions) map[string]any {
	m := map[string]any{
		"action":        r.Action,
		"actor_handle":  r.ActorHandle,
		"actor_kind":    r.ActorKind,
		"resource_type": r.ResourceType,
		"resource_id":   r.ResourceID,
		"occurred_at":   r.OccurredAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if !opts.OmitUUIDs {
		m["id"] = r.ID.String()
		m["workspace_id"] = r.WorkspaceID.String()
		m["actor_id"] = r.ActorID.String()
	}
	return m
}

func formatJSONLRecords(w io.Writer, records []map[string]any, opts TokenBudgetOptions) error {
	if len(records) == 0 {
		_, err := io.WriteString(w, "{}\n")
		return err
	}
	lines := make([]string, 0, len(records))
	for _, rec := range records {
		raw, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		lines = append(lines, string(raw))
	}
	return writeBudgetedItems(w, "", lines, "{}\n", FormatJSONL, opts.MaxTokens)
}

func formatSingleJSON(w io.Writer, obj map[string]any, opts TokenBudgetOptions) error {
	raw, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if opts.MaxTokens > 0 && len(raw) > opts.MaxTokens*bytesPerEstimatedToken {
		compactRaw, err := json.Marshal(map[string]any{
			"truncated": true,
			"summary":   "+1 more omitted",
		})
		if err != nil {
			return err
		}
		_, err = io.WriteString(w, string(compactRaw)+"\n")
		return err
	}
	_, err = w.Write(raw)
	return err
}

func formatSliceJSON(w io.Writer, records []map[string]any, opts TokenBudgetOptions) error {
	raw, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if opts.MaxTokens <= 0 || len(raw) <= opts.MaxTokens*bytesPerEstimatedToken {
		_, err = w.Write(raw)
		return err
	}

	maxBytes := opts.MaxTokens * bytesPerEstimatedToken
	kept := make([]map[string]any, 0, len(records))
	for i, rec := range records {
		candidate := append(kept, rec)
		omitted := len(records) - (i + 1)
		env := map[string]any{
			"items":   candidate,
			"omitted": omitted,
			"summary": fmt.Sprintf("+%d more omitted", omitted),
		}
		encoded, encErr := json.MarshalIndent(env, "", "  ")
		if encErr != nil {
			return encErr
		}
		if len(encoded)+1 > maxBytes {
			break
		}
		kept = candidate
	}
	omitted := len(records) - len(kept)
	finalEnv := map[string]any{
		"items":   kept,
		"omitted": omitted,
		"summary": fmt.Sprintf("+%d more omitted", omitted),
	}
	out, err := json.MarshalIndent(finalEnv, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(out, '\n'))
	return err
}

func formatWorkspacesJSONL(w io.Writer, items []workspace.Workspace, opts TokenBudgetOptions) error {
	recs := make([]map[string]any, 0, len(items))
	for _, ws := range items {
		recs = append(recs, workspaceToJSONMap(ws, opts))
	}
	return formatJSONLRecords(w, recs, opts)
}

func formatWorkspacesJSON(w io.Writer, items []workspace.Workspace, opts TokenBudgetOptions) error {
	recs := make([]map[string]any, 0, len(items))
	for _, ws := range items {
		recs = append(recs, workspaceToJSONMap(ws, opts))
	}
	return formatSliceJSON(w, recs, opts)
}

func formatProjectsJSONL(w io.Writer, items []project.Project, opts TokenBudgetOptions) error {
	recs := make([]map[string]any, 0, len(items))
	for _, p := range items {
		recs = append(recs, projectToJSONMap(p, opts))
	}
	return formatJSONLRecords(w, recs, opts)
}

func formatProjectsJSON(w io.Writer, items []project.Project, opts TokenBudgetOptions) error {
	recs := make([]map[string]any, 0, len(items))
	for _, p := range items {
		recs = append(recs, projectToJSONMap(p, opts))
	}
	return formatSliceJSON(w, recs, opts)
}

func formatObjectivesJSONL(w io.Writer, items []ObjectiveProgress, opts TokenBudgetOptions) error {
	recs := make([]map[string]any, 0, len(items))
	for _, it := range items {
		recs = append(recs, objectiveProgressToJSONMap(it, opts))
	}
	return formatJSONLRecords(w, recs, opts)
}

func formatObjectivesJSON(w io.Writer, items []ObjectiveProgress, opts TokenBudgetOptions) error {
	recs := make([]map[string]any, 0, len(items))
	for _, it := range items {
		recs = append(recs, objectiveProgressToJSONMap(it, opts))
	}
	return formatSliceJSON(w, recs, opts)
}

func formatTaskQueueJSONL(w io.Writer, items []task.CompactTaskSummary, opts TokenBudgetOptions) error {
	recs := make([]map[string]any, 0, len(items))
	for _, t := range items {
		recs = append(recs, compactTaskToJSONMap(t, opts))
	}
	return formatJSONLRecords(w, recs, opts)
}

func formatTaskQueueJSON(w io.Writer, items []task.CompactTaskSummary, opts TokenBudgetOptions) error {
	recs := make([]map[string]any, 0, len(items))
	for _, t := range items {
		recs = append(recs, compactTaskToJSONMap(t, opts))
	}
	return formatSliceJSON(w, recs, opts)
}

func formatProjectDigestJSONL(w io.Writer, d task.ProjectDigest, opts TokenBudgetOptions) error {
	return formatJSONLRecords(w, []map[string]any{digestToJSONMap(d, opts)}, opts)
}

func formatBoardJSONL(w io.Writer, b BoardView, opts TokenBudgetOptions) error {
	return formatJSONLRecords(w, []map[string]any{boardToJSONMap(b, opts)}, opts)
}

func formatAuditRecordsJSONL(w io.Writer, records []audit.Record, opts TokenBudgetOptions) error {
	recs := make([]map[string]any, 0, len(records))
	for _, r := range records {
		recs = append(recs, auditToJSONMap(r, opts))
	}
	return formatJSONLRecords(w, recs, opts)
}

func formatAuditRecordsJSON(w io.Writer, records []audit.Record, opts TokenBudgetOptions) error {
	recs := make([]map[string]any, 0, len(records))
	for _, r := range records {
		recs = append(recs, auditToJSONMap(r, opts))
	}
	return formatSliceJSON(w, recs, opts)
}

// --- GitHub-Flavored Markdown Renderers ---

func formatWorkspacesMarkdown(w io.Writer, items []workspace.Workspace, opts TokenBudgetOptions) error {
	header := "## Workspaces\n\n| Slug | Name | Lease TTL | Labels |\n| --- | --- | --- | --- |\n"
	rows := make([]string, 0, len(items))
	for _, ws := range items {
		rows = append(rows, fmt.Sprintf("| `%s` | %s | %ds | %s |", escapeMD(ws.Slug), escapeMD(ws.Name), ws.DefaultLeaseTTLSeconds, escapeMD(formatLabelsInline(ws.Labels))))
	}
	return writeBudgetedItems(w, header, rows, "_No workspaces._\n", FormatMarkdown, opts.MaxTokens)
}

func formatProjectMarkdown(w io.Writer, p project.Project, opts TokenBudgetOptions) error {
	var sb strings.Builder
	_, _ = fmt.Fprintf(&sb, "## Project: %s (`%s`)\n\n", escapeMD(p.Name), escapeMD(p.KeyPrefix))
	_, _ = fmt.Fprintf(&sb, "- **Slug**: `%s`\n- **Status**: `%s`\n- **Branch**: `%s`\n", escapeMD(p.Slug), p.Status, escapeMD(fallbackDash(p.DefaultBranch)))
	if p.Description != "" {
		_, _ = fmt.Fprintf(&sb, "\n%s\n", truncateRunes(p.Description, opts.TruncateBodyChars))
	}
	return writeBudgetedDocument(w, sb.String(), FormatMarkdown, opts.MaxTokens)
}

func formatProjectsMarkdown(w io.Writer, items []project.Project, opts TokenBudgetOptions) error {
	header := "## Projects\n\n| Key | Slug | Status | Name | Tags |\n| --- | --- | --- | --- | --- |\n"
	rows := make([]string, 0, len(items))
	for _, p := range items {
		rows = append(rows, fmt.Sprintf("| `%s` | `%s` | `%s` | %s | %s |", escapeMD(p.KeyPrefix), escapeMD(p.Slug), p.Status, escapeMD(p.Name), escapeMD(formatTagsInline(p.Tags))))
	}
	return writeBudgetedItems(w, header, rows, "_No projects._\n", FormatMarkdown, opts.MaxTokens)
}

func formatObjectiveMarkdown(w io.Writer, it ObjectiveProgress, opts TokenBudgetOptions) error {
	o := it.Objective
	var sb strings.Builder
	_, _ = fmt.Fprintf(&sb, "## Objective: %s (`%s`)\n\n", escapeMD(o.Title), escapeMD(o.Slug))
	_, _ = fmt.Fprintf(&sb, "- **Status**: `%s`\n- **Progress**: `%s`\n", o.Status, renderProgressBar(it.CompletedTasks, it.TotalTasks, 8))
	if o.Summary != "" {
		_, _ = fmt.Fprintf(&sb, "\n%s\n", truncateRunes(o.Summary, opts.TruncateBodyChars))
	}
	return writeBudgetedDocument(w, sb.String(), FormatMarkdown, opts.MaxTokens)
}

func formatObjectivesMarkdown(w io.Writer, items []ObjectiveProgress, opts TokenBudgetOptions) error {
	header := "## Objectives\n\n| Rank | Slug | Status | Progress | Title |\n| --- | --- | --- | --- | --- |\n"
	rows := make([]string, 0, len(items))
	for _, it := range items {
		o := it.Objective
		rows = append(rows, fmt.Sprintf("| #%d | `%s` | `%s` | `%s` | %s |", o.PriorityRank, escapeMD(o.Slug), o.Status, renderProgressBar(it.CompletedTasks, it.TotalTasks, 8), escapeMD(o.Title)))
	}
	return writeBudgetedItems(w, header, rows, "_No objectives._\n", FormatMarkdown, opts.MaxTokens)
}

func formatTaskMarkdown(w io.Writer, item task.Task, opts TokenBudgetOptions) error {
	var sb strings.Builder
	_, _ = fmt.Fprintf(&sb, "## %s: %s\n\n", escapeMD(item.Key), escapeMD(item.Title))
	_, _ = fmt.Fprintf(&sb, "- **Priority**: `%s`\n- **Status**: `%s`\n- **Kind**: `%s`\n", priorityBadge(item.Priority), item.Status, item.Kind)
	if item.ActiveLease != nil && item.ActiveLease.HolderHandle != "" {
		_, _ = fmt.Fprintf(&sb, "- **Lease**: `@%s`\n", escapeMD(item.ActiveLease.HolderHandle))
	}
	if item.Description != "" {
		_, _ = fmt.Fprintf(&sb, "\n%s\n", truncateRunes(item.Description, opts.TruncateBodyChars))
	}
	return writeBudgetedDocument(w, sb.String(), FormatMarkdown, opts.MaxTokens)
}

func formatTaskQueueMarkdown(w io.Writer, items []task.CompactTaskSummary, opts TokenBudgetOptions) error {
	header := "## Task Queue\n\n| Key | Priority | Status | Lease | Title | Tags |\n| --- | --- | --- | --- | --- | --- |\n"
	rows := make([]string, 0, len(items))
	for _, t := range items {
		lease := "-"
		if t.HolderHandle != "" {
			lease = "@" + t.HolderHandle
		}
		rows = append(rows, fmt.Sprintf("| `%s` | `%s` | `%s` | %s | %s | %s |", escapeMD(t.Key), priorityBadge(t.Priority), t.Status, escapeMD(lease), escapeMD(t.Title), escapeMD(formatTagsInline(t.Tags))))
	}
	return writeBudgetedItems(w, header, rows, "_Queue is empty._\n", FormatMarkdown, opts.MaxTokens)
}

func formatProjectDigestMarkdown(w io.Writer, d task.ProjectDigest, opts TokenBudgetOptions) error {
	var sb strings.Builder
	_, _ = fmt.Fprintf(&sb, "## Project Digest: `%s` (`%s`)\n\n", escapeMD(d.ProjectSlug), escapeMD(d.ProjectKeyPrefix))
	_, _ = fmt.Fprintf(&sb, "- Ready: **%d** | In Progress: **%d** | Blocked: **%d** | Done: **%d**\n",
		d.CountsByStatus[task.StatusReady],
		d.CountsByStatus[task.StatusInProgress],
		d.CountsByStatus[task.StatusBlocked],
		d.CountsByStatus[task.StatusDone],
	)
	for _, t := range d.ReadyQueue {
		_, _ = fmt.Fprintf(&sb, "- `%s` %s %s\n", escapeMD(t.Key), priorityBadge(t.Priority), escapeMD(t.Title))
	}
	return writeBudgetedDocument(w, sb.String(), FormatMarkdown, opts.MaxTokens)
}

func formatBoardMarkdown(w io.Writer, b BoardView, opts TokenBudgetOptions) error {
	var sb strings.Builder
	_, _ = fmt.Fprintf(&sb, "# Board: %s (`%s`)\n\n", escapeMD(b.Project.Name), escapeMD(b.Project.KeyPrefix))
	sb.WriteString("## Objectives\n\n")
	for _, op := range b.Objectives {
		_, _ = fmt.Fprintf(&sb, "- **%s** (`%s`): `%s`\n", escapeMD(op.Objective.Title), escapeMD(op.Objective.Slug), renderProgressBar(op.CompletedTasks, op.TotalTasks, 8))
	}
	sb.WriteString("\n## Tasks\n\n")
	for _, t := range b.Digest.ActiveLeases {
		_, _ = fmt.Fprintf(&sb, "- `%s` [IN PROGRESS] @%s — %s\n", escapeMD(t.Key), escapeMD(t.HolderHandle), escapeMD(t.Title))
	}
	for _, t := range b.Digest.ReadyQueue {
		_, _ = fmt.Fprintf(&sb, "- `%s` %s — %s\n", escapeMD(t.Key), priorityBadge(t.Priority), escapeMD(t.Title))
	}
	for _, t := range b.Digest.BlockedTasks {
		_, _ = fmt.Fprintf(&sb, "- `%s` [BLOCKED] — %s\n", escapeMD(t.Key), escapeMD(t.Title))
	}
	return writeBudgetedDocument(w, sb.String(), FormatMarkdown, opts.MaxTokens)
}

func formatAuditRecordsMarkdown(w io.Writer, records []audit.Record, opts TokenBudgetOptions) error {
	header := "## Audit Trail\n\n| Occurred At | Action | Actor | Resource |\n| --- | --- | --- | --- |\n"
	rows := make([]string, 0, len(records))
	for _, r := range records {
		rows = append(rows, fmt.Sprintf("| `%s` | `%s` | `@%s` | `%s:%s` |", r.OccurredAt.UTC().Format("2006-01-02T15:04:05Z"), r.Action, escapeMD(r.ActorHandle), escapeMD(r.ResourceType), escapeMD(r.ResourceID)))
	}
	return writeBudgetedItems(w, header, rows, "_No audit records._\n", FormatMarkdown, opts.MaxTokens)
}
