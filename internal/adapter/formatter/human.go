// File human.go implements crystal-clear interactive terminal rendering (`FormatHuman`)
// across all 10 OutputFormatter methods, featuring aligned columns, box headers,
// priority/status/blocker/lease badges, and Unicode completion progress bars.
package formatter

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/mindmain/flowsk/internal/domain/audit"
	"github.com/mindmain/flowsk/internal/domain/project"
	"github.com/mindmain/flowsk/internal/domain/task"
	"github.com/mindmain/flowsk/internal/domain/workspace"
)

func effectiveTerminalWidth(opts TokenBudgetOptions) int {
	if opts.TerminalWidth >= 60 {
		return opts.TerminalWidth
	}
	return 100
}

func padRight(s string, width int) string {
	runeLen := len([]rune(s))
	if runeLen >= width {
		return s
	}
	return s + strings.Repeat(" ", width-runeLen)
}

func formatLabelsInline(m map[string]string) string {
	if len(m) == 0 {
		return "-"
	}
	keys := slices.Sorted(maps.Keys(m))
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, m[k]))
	}
	return strings.Join(parts, ",")
}

func formatTagsInline(tags []string) string {
	if len(tags) == 0 {
		return "-"
	}
	prefixed := make([]string, 0, len(tags))
	for _, t := range tags {
		prefixed = append(prefixed, "#"+t)
	}
	return strings.Join(prefixed, " ")
}

func boxBanner(title, subtitle string, width int) string {
	inner := width - 4
	if inner < 40 {
		inner = 40
	}
	var sb strings.Builder
	sb.WriteString("╭─" + strings.Repeat("─", inner) + "─╮\n")
	sb.WriteString("│ " + padRight(truncateRunes(title, inner), inner) + " │\n")
	if strings.TrimSpace(subtitle) != "" {
		sb.WriteString("│ " + padRight(truncateRunes(subtitle, inner), inner) + " │\n")
	}
	sb.WriteString("╰─" + strings.Repeat("─", inner) + "─╯\n")
	return sb.String()
}

func formatWorkspacesHuman(w io.Writer, items []workspace.Workspace, opts TokenBudgetOptions) error {
	header := fmt.Sprintf(
		"WORKSPACES (%d)\n%s  %s  %s  %s\n%s\n",
		len(items),
		padRight("SLUG", 20),
		padRight("NAME", 28),
		padRight("LEASE TTL", 11),
		"LABELS",
		strings.Repeat("─", 78),
	)
	rows := make([]string, 0, len(items))
	for _, ws := range items {
		row := fmt.Sprintf(
			"%s  %s  %s  %s",
			padRight(ws.Slug, 20),
			padRight(truncateRunes(ws.Name, 28), 28),
			padRight(fmt.Sprintf("%ds", ws.DefaultLeaseTTLSeconds), 11),
			formatLabelsInline(ws.Labels),
		)
		if !opts.OmitUUIDs {
			row += fmt.Sprintf("  (%s)", ws.ID)
		}
		rows = append(rows, row)
	}
	return writeBudgetedItems(w, header, rows, "(no workspaces)\n", FormatHuman, opts.MaxTokens)
}

func formatProjectHuman(w io.Writer, item project.Project, opts TokenBudgetOptions) error {
	width := effectiveTerminalWidth(opts)
	status := "[" + strings.ToUpper(string(item.Status)) + "]"
	var sb strings.Builder
	sb.WriteString(boxBanner(
		fmt.Sprintf("PROJECT  [%s]  %s  %s", item.KeyPrefix, item.Name, status),
		fmt.Sprintf("Slug: %s   Branch: %s   Tags: %s", item.Slug, fallbackDash(item.DefaultBranch), formatTagsInline(item.Tags)),
		width,
	))
	if !opts.OmitUUIDs {
		_, _ = fmt.Fprintf(&sb, "  ID:          %s\n", item.ID)
	}
	if item.RemoteRepoURL != "" {
		_, _ = fmt.Fprintf(&sb, "  Remote URL:  %s\n", item.RemoteRepoURL)
	}
	if item.HasMarkdownDoc {
		_, _ = fmt.Fprintf(&sb, "  Markdown:    [MD] attached\n")
	}
	if item.Description != "" {
		_, _ = fmt.Fprintf(&sb, "  Description: %s\n", truncateRunes(item.Description, opts.TruncateBodyChars))
	}
	return writeBudgetedDocument(w, sb.String(), FormatHuman, opts.MaxTokens)
}

func formatProjectsHuman(w io.Writer, items []project.Project, opts TokenBudgetOptions) error {
	header := fmt.Sprintf(
		"PROJECTS (%d)\n%s  %s  %s  %s  %s  %s\n%s\n",
		len(items),
		padRight("KEY", 8),
		padRight("SLUG", 20),
		padRight("STATUS", 10),
		padRight("NAME", 28),
		padRight("BRANCH", 10),
		"TAGS",
		strings.Repeat("─", 92),
	)
	rows := make([]string, 0, len(items))
	for _, p := range items {
		rows = append(rows, fmt.Sprintf(
			"%s  %s  %s  %s  %s  %s",
			padRight(p.KeyPrefix, 8),
			padRight(p.Slug, 20),
			padRight("["+strings.ToUpper(string(p.Status))+"]", 10),
			padRight(truncateRunes(p.Name, 28), 28),
			padRight(fallbackDash(p.DefaultBranch), 10),
			formatTagsInline(p.Tags),
		))
	}
	return writeBudgetedItems(w, header, rows, "(no projects)\n", FormatHuman, opts.MaxTokens)
}

func formatObjectiveHuman(w io.Writer, item ObjectiveProgress, opts TokenBudgetOptions) error {
	width := effectiveTerminalWidth(opts)
	obj := item.Objective
	badge := "[" + strings.ToUpper(string(obj.Status)) + "]"
	targetStr := "-"
	if obj.TargetDate != nil {
		targetStr = obj.TargetDate.UTC().Format("2006-01-02")
	}

	var sb strings.Builder
	sb.WriteString(boxBanner(
		fmt.Sprintf("OBJECTIVE  #%d  %s  %s", obj.PriorityRank, obj.Title, badge),
		fmt.Sprintf("Slug: %s   Progress: %s   Target: %s", obj.Slug, renderProgressBar(item.CompletedTasks, item.TotalTasks, 8), targetStr),
		width,
	))
	if item.BlockedTasks > 0 {
		_, _ = fmt.Fprintf(&sb, "  Blockers: [BLOCKED] %d linked task(s) blocked\n", item.BlockedTasks)
	}
	if obj.Summary != "" {
		_, _ = fmt.Fprintf(&sb, "  Summary:  %s\n", truncateRunes(obj.Summary, opts.TruncateBodyChars))
	}
	if len(obj.SuccessCriteria) > 0 {
		sb.WriteString("  Success Criteria:\n")
		for _, c := range obj.SuccessCriteria {
			_, _ = fmt.Fprintf(&sb, "    • %s\n", truncateRunes(c, opts.TruncateBodyChars))
		}
	}
	return writeBudgetedDocument(w, sb.String(), FormatHuman, opts.MaxTokens)
}

func formatObjectivesHuman(w io.Writer, items []ObjectiveProgress, opts TokenBudgetOptions) error {
	header := fmt.Sprintf(
		"STRATEGIC OBJECTIVES (%d)\n%s  %s  %s  %s  %s  %s  %s\n%s\n",
		len(items),
		padRight("RANK", 5),
		padRight("SLUG", 22),
		padRight("STATUS", 10),
		padRight("PROGRESS", 22),
		padRight("BLOCKED", 10),
		padRight("TARGET", 10),
		"TITLE",
		strings.Repeat("─", 100),
	)
	rows := make([]string, 0, len(items))
	for _, it := range items {
		o := it.Objective
		blk := "-"
		if it.BlockedTasks > 0 {
			blk = fmt.Sprintf("[BLOCKED:%d]", it.BlockedTasks)
		}
		target := "-"
		if o.TargetDate != nil {
			target = o.TargetDate.UTC().Format("2006-01-02")
		}
		rows = append(rows, fmt.Sprintf(
			"%s  %s  %s  %s  %s  %s  %s",
			padRight(fmt.Sprintf("#%d", o.PriorityRank), 5),
			padRight(o.Slug, 22),
			padRight("["+strings.ToUpper(string(o.Status))+"]", 10),
			padRight(renderProgressBar(it.CompletedTasks, it.TotalTasks, 8), 22),
			padRight(blk, 10),
			padRight(target, 10),
			o.Title,
		))
	}
	return writeBudgetedItems(w, header, rows, "(no objectives)\n", FormatHuman, opts.MaxTokens)
}

func formatTaskHuman(w io.Writer, item task.Task, opts TokenBudgetOptions) error {
	width := effectiveTerminalWidth(opts)
	pBadge := priorityBadge(item.Priority)
	sBadge := statusBadge(item.Status, item.UnresolvedBlockerCount)
	if item.Status == task.StatusBlocked || item.UnresolvedBlockerCount > 0 {
		sBadge = "[BLOCKED]"
	}

	leaseInfo := "unclaimed"
	if item.ActiveLease != nil && item.ActiveLease.HolderHandle != "" {
		leaseInfo = fmt.Sprintf("@%s (%s, expires %s)",
			item.ActiveLease.HolderHandle,
			item.ActiveLease.HolderKind,
			item.ActiveLease.ExpiresAt.UTC().Format("15:04:05Z"),
		)
	}

	var sb strings.Builder
	sb.WriteString(boxBanner(
		fmt.Sprintf("%s  %s  %s  %s", item.Key, pBadge, sBadge, item.Title),
		fmt.Sprintf("Kind: %s   Lease: %s   Tags: %s", item.Kind, leaseInfo, formatTagsInline(item.Tags)),
		width,
	))
	if !opts.OmitUUIDs {
		_, _ = fmt.Fprintf(&sb, "  ID:          %s (v%d)\n", item.ID, item.Version)
	}
	if item.ExternalID != "" {
		_, _ = fmt.Fprintf(&sb, "  External ID: %s\n", item.ExternalID)
	}
	if item.HasMarkdownDoc {
		_, _ = fmt.Fprintf(&sb, "  Markdown:    [MD] attached\n")
	}
	if item.UnresolvedBlockerCount > 0 {
		_, _ = fmt.Fprintf(&sb, "  Blockers:    [BLOCKED] %d unresolved upstream blocker(s)\n", item.UnresolvedBlockerCount)
	}
	if len(item.Labels) > 0 {
		_, _ = fmt.Fprintf(&sb, "  Labels:      %s\n", formatLabelsInline(item.Labels))
	}
	if item.Description != "" {
		_, _ = fmt.Fprintf(&sb, "  Details:     %s\n", truncateRunes(item.Description, opts.TruncateBodyChars))
	}
	if len(item.AcceptanceCriteria) > 0 {
		sb.WriteString("  Acceptance Criteria:\n")
		for _, ac := range item.AcceptanceCriteria {
			_, _ = fmt.Fprintf(&sb, "    • %s\n", truncateRunes(ac, opts.TruncateBodyChars))
		}
	}
	return writeBudgetedDocument(w, sb.String(), FormatHuman, opts.MaxTokens)
}

func formatTaskQueueHuman(w io.Writer, items []task.CompactTaskSummary, opts TokenBudgetOptions) error {
	header := fmt.Sprintf(
		"TASK QUEUE (%d)\n%s  %s  %s  %s  %s  %s\n%s\n",
		len(items),
		padRight("KEY", 10),
		padRight("PRIORITY", 9),
		padRight("STATUS", 14),
		padRight("LEASE", 14),
		padRight("TITLE", 38),
		"TAGS",
		strings.Repeat("─", 98),
	)
	rows := make([]string, 0, len(items))
	for _, t := range items {
		lease := "-"
		if t.HolderHandle != "" {
			lease = "@" + t.HolderHandle
		}
		var extras []string
		if t.ExternalID != "" {
			extras = append(extras, "ext="+t.ExternalID)
		}
		if t.HasMarkdownDoc {
			extras = append(extras, "[MD]")
		}
		if len(t.Tags) > 0 {
			extras = append(extras, formatTagsInline(t.Tags))
		}
		metaCol := "-"
		if len(extras) > 0 {
			metaCol = strings.Join(extras, " ")
		}
		rows = append(rows, fmt.Sprintf(
			"%s  %s  %s  %s  %s  %s",
			padRight(t.Key, 10),
			padRight(priorityBadge(t.Priority), 9),
			padRight(statusBadge(t.Status, t.UnresolvedBlockerCount), 14),
			padRight(lease, 14),
			padRight(t.Title, 38),
			metaCol,
		))
	}
	return writeBudgetedItems(w, header, rows, "(no tasks in queue)\n", FormatHuman, opts.MaxTokens)
}

func formatProjectDigestHuman(w io.Writer, digest task.ProjectDigest, opts TokenBudgetOptions) error {
	width := effectiveTerminalWidth(opts)
	countsSummary := fmt.Sprintf(
		"Ready: %d  |  In Progress: %d  |  In Review: %d  |  Blocked: %d  |  Done: %d",
		digest.CountsByStatus[task.StatusReady],
		digest.CountsByStatus[task.StatusInProgress],
		digest.CountsByStatus[task.StatusInReview],
		digest.CountsByStatus[task.StatusBlocked],
		digest.CountsByStatus[task.StatusDone],
	)

	var sb strings.Builder
	sb.WriteString(boxBanner(
		fmt.Sprintf("PROJECT DIGEST  [%s]  %s", digest.ProjectKeyPrefix, digest.ProjectSlug),
		countsSummary,
		width,
	))

	if len(digest.ActiveObjectives) > 0 {
		sb.WriteString("\n── ACTIVE OBJECTIVES ──────────────────────────────────────────────────────────\n")
		for _, o := range digest.ActiveObjectives {
			total := o.OpenTaskCount + o.DoneTaskCount
			_, _ = fmt.Fprintf(
				&sb,
				"  #%d  %s  %s  %s\n",
				o.PriorityRank,
				padRight(o.Slug, 22),
				padRight(renderProgressBar(o.DoneTaskCount, total, 8), 22),
				o.Title,
			)
		}
	}
	writeHumanDigestSection(&sb, "ACTIVE LEASES", digest.ActiveLeases)
	writeHumanDigestSection(&sb, "READY QUEUE", digest.ReadyQueue)
	writeHumanDigestSection(&sb, "BLOCKED TASKS", digest.BlockedTasks)

	return writeBudgetedDocument(w, sb.String(), FormatHuman, opts.MaxTokens)
}

func formatBoardHuman(w io.Writer, board BoardView, opts TokenBudgetOptions) error {
	width := effectiveTerminalWidth(opts)
	p := board.Project
	d := board.Digest
	pStatus := "[" + strings.ToUpper(string(p.Status)) + "]"
	countsSummary := fmt.Sprintf(
		"Ready: %d  |  In Progress: %d  |  Blocked: %d  |  Done: %d   Branch: %s",
		d.CountsByStatus[task.StatusReady],
		d.CountsByStatus[task.StatusInProgress],
		d.CountsByStatus[task.StatusBlocked],
		d.CountsByStatus[task.StatusDone],
		fallbackDash(p.DefaultBranch),
	)

	var sb strings.Builder
	sb.WriteString(boxBanner(
		fmt.Sprintf("BOARD  [%s]  %s  (%s)  %s", p.KeyPrefix, p.Name, p.Slug, pStatus),
		countsSummary,
		width,
	))

	sb.WriteString("\n── STRATEGIC GOALS & OBJECTIVES ───────────────────────────────────────────────\n")
	if len(board.Objectives) == 0 {
		sb.WriteString("  (no active objectives)\n")
	} else {
		for _, op := range board.Objectives {
			o := op.Objective
			blkSuffix := ""
			if op.BlockedTasks > 0 {
				blkSuffix = fmt.Sprintf("  [BLOCKED] (%d blocked)", op.BlockedTasks)
			}
			_, _ = fmt.Fprintf(
				&sb,
				"  #%d  %s  [%s]  %s  %s%s\n",
				o.PriorityRank,
				padRight(o.Slug, 20),
				strings.ToUpper(string(o.Status)),
				padRight(renderProgressBar(op.CompletedTasks, op.TotalTasks, 8), 22),
				o.Title,
				blkSuffix,
			)
		}
	}

	writeHumanDigestSection(&sb, "IN PROGRESS & ACTIVE LEASES", d.ActiveLeases)
	writeHumanDigestSection(&sb, "READY TO CLAIM", d.ReadyQueue)
	writeHumanDigestSection(&sb, "BLOCKED", d.BlockedTasks)

	return writeBudgetedDocument(w, sb.String(), FormatHuman, opts.MaxTokens)
}

func writeHumanDigestSection(sb *strings.Builder, title string, items []task.CompactTaskSummary) {
	_, _ = fmt.Fprintf(sb, "\n── %s (%d) %s\n", title, len(items), strings.Repeat("─", max(4, 72-len(title))))
	if len(items) == 0 {
		sb.WriteString("  (none)\n")
		return
	}
	for _, t := range items {
		sBadge := statusBadge(t.Status, t.UnresolvedBlockerCount)
		if t.Status == task.StatusBlocked || t.UnresolvedBlockerCount > 0 {
			sBadge = "[BLOCKED]"
		}
		leaseBadge := ""
		if t.HolderHandle != "" {
			leaseBadge = "  @" + t.HolderHandle
		}
		extBadge := ""
		if t.ExternalID != "" {
			extBadge = "  ext=" + t.ExternalID
		}
		mdBadge := ""
		if t.HasMarkdownDoc {
			mdBadge = "  [MD]"
		}
		tags := ""
		if len(t.Tags) > 0 {
			tags = "  " + formatTagsInline(t.Tags)
		}
		_, _ = fmt.Fprintf(
			sb,
			"  %s  %s  %s  %s%s%s%s%s\n",
			padRight(t.Key, 9),
			padRight(priorityBadge(t.Priority), 8),
			padRight(sBadge, 13),
			t.Title,
			leaseBadge,
			extBadge,
			mdBadge,
			tags,
		)
	}
}

func formatAuditRecordsHuman(w io.Writer, records []audit.Record, opts TokenBudgetOptions) error {
	header := fmt.Sprintf(
		"AUDIT TRAIL (%d)\n%s  %s  %s  %s\n%s\n",
		len(records),
		padRight("OCCURRED AT (UTC)", 20),
		padRight("ACTION", 12),
		padRight("ACTOR", 20),
		"RESOURCE",
		strings.Repeat("─", 80),
	)
	rows := make([]string, 0, len(records))
	for _, r := range records {
		rows = append(rows, fmt.Sprintf(
			"%s  %s  %s  %s:%s",
			padRight(r.OccurredAt.UTC().Format("2006-01-02 15:04:05"), 20),
			padRight("["+strings.ToUpper(string(r.Action))+"]", 12),
			padRight(fmt.Sprintf("@%s (%s)", r.ActorHandle, r.ActorKind), 20),
			r.ResourceType,
			r.ResourceID,
		))
	}
	return writeBudgetedItems(w, header, rows, "(no audit records)\n", FormatHuman, opts.MaxTokens)
}

func fallbackDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
