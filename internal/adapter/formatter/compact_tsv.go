// File compact_tsv.go implements ultra-low-token symbolic encoding (`FormatCompact`)
// and deterministic tab-separated tabular output (`FormatTSV`) for AI coding agents
// and Unix CLI pipelines.
package formatter

import (
	"fmt"
	"io"
	"strings"

	"github.com/mindmain/flowsk/internal/domain/audit"
	"github.com/mindmain/flowsk/internal/domain/project"
	"github.com/mindmain/flowsk/internal/domain/task"
	"github.com/mindmain/flowsk/internal/domain/workspace"
)

func cleanOneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	return strings.TrimSpace(s)
}

func formatWorkspacesCompact(w io.Writer, items []workspace.Workspace, opts TokenBudgetOptions) error {
	rows := make([]string, 0, len(items))
	for _, ws := range items {
		rows = append(rows, fmt.Sprintf("ws %s %q ttl=%ds", ws.Slug, cleanOneLine(ws.Name), ws.DefaultLeaseTTLSeconds))
	}
	return writeBudgetedItems(w, "", rows, "ws none\n", FormatCompact, opts.MaxTokens)
}

func formatWorkspacesTSV(w io.Writer, items []workspace.Workspace, opts TokenBudgetOptions) error {
	header := "slug\tname\tlease_ttl_s\tlabels\n"
	rows := make([]string, 0, len(items))
	for _, ws := range items {
		rows = append(rows, fmt.Sprintf(
			"%s\t%s\t%d\t%s",
			cleanOneLine(ws.Slug),
			cleanOneLine(ws.Name),
			ws.DefaultLeaseTTLSeconds,
			cleanOneLine(formatLabelsInline(ws.Labels)),
		))
	}
	return writeBudgetedItems(w, header, rows, "", FormatTSV, opts.MaxTokens)
}

func formatProjectsCompact(w io.Writer, items []project.Project, opts TokenBudgetOptions) error {
	rows := make([]string, 0, len(items))
	for _, p := range items {
		line := fmt.Sprintf("proj %s %s [%s] %q", p.KeyPrefix, p.Slug, p.Status, cleanOneLine(p.Name))
		if len(p.Tags) > 0 {
			line += " " + formatTagsInline(p.Tags)
		}
		rows = append(rows, line)
	}
	return writeBudgetedItems(w, "", rows, "proj none\n", FormatCompact, opts.MaxTokens)
}

func formatProjectsTSV(w io.Writer, items []project.Project, opts TokenBudgetOptions) error {
	header := "key_prefix\tslug\tstatus\tname\tdefault_branch\ttags\n"
	rows := make([]string, 0, len(items))
	for _, p := range items {
		rows = append(rows, fmt.Sprintf(
			"%s\t%s\t%s\t%s\t%s\t%s",
			cleanOneLine(p.KeyPrefix),
			cleanOneLine(p.Slug),
			p.Status,
			cleanOneLine(p.Name),
			cleanOneLine(fallbackDash(p.DefaultBranch)),
			cleanOneLine(strings.Join(p.Tags, ",")),
		))
	}
	return writeBudgetedItems(w, header, rows, "", FormatTSV, opts.MaxTokens)
}

func formatObjectivesCompact(w io.Writer, items []ObjectiveProgress, opts TokenBudgetOptions) error {
	rows := make([]string, 0, len(items))
	for _, it := range items {
		o := it.Objective
		pct := int64(0)
		if it.TotalTasks > 0 {
			pct = (int64(it.CompletedTasks) * 100) / int64(it.TotalTasks)
		}
		line := fmt.Sprintf("obj %s [P%d|%s] %d/%d (%d%%) %q", o.Slug, o.PriorityRank, o.Status, it.CompletedTasks, it.TotalTasks, pct, cleanOneLine(o.Title))
		if it.BlockedTasks > 0 {
			line += fmt.Sprintf(" blk=%d", it.BlockedTasks)
		}
		rows = append(rows, line)
	}
	return writeBudgetedItems(w, "", rows, "obj none\n", FormatCompact, opts.MaxTokens)
}

func formatObjectivesTSV(w io.Writer, items []ObjectiveProgress, opts TokenBudgetOptions) error {
	header := "rank\tslug\tstatus\tcompleted\ttotal\tblocked\ttitle\n"
	rows := make([]string, 0, len(items))
	for _, it := range items {
		o := it.Objective
		rows = append(rows, fmt.Sprintf(
			"%d\t%s\t%s\t%d\t%d\t%d\t%s",
			o.PriorityRank,
			cleanOneLine(o.Slug),
			o.Status,
			it.CompletedTasks,
			it.TotalTasks,
			it.BlockedTasks,
			cleanOneLine(o.Title),
		))
	}
	return writeBudgetedItems(w, header, rows, "", FormatTSV, opts.MaxTokens)
}

func compactSummaryLine(t task.CompactTaskSummary) string {
	var sb strings.Builder
	_, _ = fmt.Fprintf(&sb, "%s [P%d|%s] %s", t.Key, t.Priority, t.Status, cleanOneLine(t.Title))
	if t.HolderHandle != "" {
		sb.WriteString(" @" + t.HolderHandle)
	}
	if t.UnresolvedBlockerCount > 0 {
		_, _ = fmt.Fprintf(&sb, " blk=%d", t.UnresolvedBlockerCount)
	}
	if t.ParentKey != "" {
		sb.WriteString(" ^" + t.ParentKey)
	}
	if t.ObjectiveSlug != "" {
		sb.WriteString(" ~" + t.ObjectiveSlug)
	}
	for _, tag := range t.Tags {
		sb.WriteString(" #" + tag)
	}
	return sb.String()
}

func formatTaskCompact(w io.Writer, item task.Task, opts TokenBudgetOptions) error {
	holder := ""
	if item.ActiveLease != nil {
		holder = item.ActiveLease.HolderHandle
	}
	line := compactSummaryLine(task.CompactTaskSummary{
		ID:                     item.ID,
		Key:                    item.Key,
		Kind:                   item.Kind,
		Title:                  item.Title,
		Status:                 item.Status,
		Priority:               item.Priority,
		HolderHandle:           holder,
		UnresolvedBlockerCount: item.UnresolvedBlockerCount,
		Tags:                   item.Tags,
		UpdatedAt:              item.UpdatedAt,
	})
	var lines []string
	lines = append(lines, line)
	if item.Description != "" {
		lines = append(lines, "desc: "+cleanOneLine(truncateRunes(item.Description, opts.TruncateBodyChars)))
	}
	return writeBudgetedItems(w, "", lines, "", FormatCompact, opts.MaxTokens)
}

func formatTaskTSV(w io.Writer, item task.Task, opts TokenBudgetOptions) error {
	holder := "-"
	if item.ActiveLease != nil && item.ActiveLease.HolderHandle != "" {
		holder = item.ActiveLease.HolderHandle
	}
	header := "key\tkind\tpriority\tstatus\tholder\tblockers\ttitle\ttags\n"
	row := fmt.Sprintf(
		"%s\t%s\tP%d\t%s\t%s\t%d\t%s\t%s",
		cleanOneLine(item.Key),
		item.Kind,
		item.Priority,
		item.Status,
		cleanOneLine(holder),
		item.UnresolvedBlockerCount,
		cleanOneLine(item.Title),
		cleanOneLine(strings.Join(item.Tags, ",")),
	)
	return writeBudgetedItems(w, header, []string{row}, "", FormatTSV, opts.MaxTokens)
}

func formatTaskQueueCompact(w io.Writer, items []task.CompactTaskSummary, opts TokenBudgetOptions) error {
	rows := make([]string, 0, len(items))
	for _, t := range items {
		rows = append(rows, compactSummaryLine(t))
	}
	return writeBudgetedItems(w, "", rows, "queue empty\n", FormatCompact, opts.MaxTokens)
}

func formatTaskQueueTSV(w io.Writer, items []task.CompactTaskSummary, opts TokenBudgetOptions) error {
	header := "key\tpriority\tstatus\tholder\tblockers\ttitle\ttags\n"
	rows := make([]string, 0, len(items))
	for _, t := range items {
		rows = append(rows, fmt.Sprintf(
			"%s\tP%d\t%s\t%s\t%d\t%s\t%s",
			cleanOneLine(t.Key),
			t.Priority,
			t.Status,
			cleanOneLine(fallbackDash(t.HolderHandle)),
			t.UnresolvedBlockerCount,
			cleanOneLine(t.Title),
			cleanOneLine(strings.Join(t.Tags, ",")),
		))
	}
	return writeBudgetedItems(w, header, rows, "", FormatTSV, opts.MaxTokens)
}

func formatProjectDigestCompact(w io.Writer, digest task.ProjectDigest, opts TokenBudgetOptions) error {
	header := fmt.Sprintf(
		"digest %s [%s] ready=%d wip=%d blk=%d done=%d\n",
		digest.ProjectSlug,
		digest.ProjectKeyPrefix,
		digest.CountsByStatus[task.StatusReady],
		digest.CountsByStatus[task.StatusInProgress],
		digest.CountsByStatus[task.StatusBlocked],
		digest.CountsByStatus[task.StatusDone],
	)
	var rows []string
	for _, o := range digest.ActiveObjectives {
		rows = append(rows, fmt.Sprintf("obj %s [P%d] %d/%d %q", o.Slug, o.PriorityRank, o.DoneTaskCount, o.OpenTaskCount+o.DoneTaskCount, cleanOneLine(o.Title)))
	}
	for _, t := range digest.ActiveLeases {
		rows = append(rows, "lease "+compactSummaryLine(t))
	}
	for _, t := range digest.ReadyQueue {
		rows = append(rows, "ready "+compactSummaryLine(t))
	}
	for _, t := range digest.BlockedTasks {
		rows = append(rows, "blocked "+compactSummaryLine(t))
	}
	return writeBudgetedItems(w, header, rows, "", FormatCompact, opts.MaxTokens)
}

func formatProjectDigestTSV(w io.Writer, digest task.ProjectDigest, opts TokenBudgetOptions) error {
	header := "section\tkey_or_slug\tpriority\tstatus_or_progress\tholder\ttitle\n"
	var rows []string
	rows = append(rows, fmt.Sprintf(
		"summary\t%s\t-\tready=%d,wip=%d,blk=%d,done=%d\t-\t%s",
		cleanOneLine(digest.ProjectSlug),
		digest.CountsByStatus[task.StatusReady],
		digest.CountsByStatus[task.StatusInProgress],
		digest.CountsByStatus[task.StatusBlocked],
		digest.CountsByStatus[task.StatusDone],
		cleanOneLine(digest.ProjectKeyPrefix),
	))
	for _, o := range digest.ActiveObjectives {
		rows = append(rows, fmt.Sprintf("objective\t%s\tP%d\t%d/%d\t-\t%s", cleanOneLine(o.Slug), o.PriorityRank, o.DoneTaskCount, o.OpenTaskCount+o.DoneTaskCount, cleanOneLine(o.Title)))
	}
	for _, t := range digest.ActiveLeases {
		rows = append(rows, fmt.Sprintf("lease\t%s\tP%d\t%s\t%s\t%s", t.Key, t.Priority, t.Status, fallbackDash(t.HolderHandle), cleanOneLine(t.Title)))
	}
	for _, t := range digest.ReadyQueue {
		rows = append(rows, fmt.Sprintf("ready\t%s\tP%d\t%s\t-\t%s", t.Key, t.Priority, t.Status, cleanOneLine(t.Title)))
	}
	for _, t := range digest.BlockedTasks {
		rows = append(rows, fmt.Sprintf("blocked\t%s\tP%d\t%s\t-\t%s", t.Key, t.Priority, t.Status, cleanOneLine(t.Title)))
	}
	return writeBudgetedItems(w, header, rows, "", FormatTSV, opts.MaxTokens)
}

func formatBoardCompact(w io.Writer, board BoardView, opts TokenBudgetOptions) error {
	header := fmt.Sprintf(
		"board %s [%s|%s] ready=%d wip=%d blk=%d done=%d\n",
		board.Project.Slug,
		board.Project.KeyPrefix,
		board.Project.Status,
		board.Digest.CountsByStatus[task.StatusReady],
		board.Digest.CountsByStatus[task.StatusInProgress],
		board.Digest.CountsByStatus[task.StatusBlocked],
		board.Digest.CountsByStatus[task.StatusDone],
	)
	var rows []string
	for _, op := range board.Objectives {
		rows = append(rows, fmt.Sprintf("goal %s %d/%d %q", op.Objective.Slug, op.CompletedTasks, op.TotalTasks, cleanOneLine(op.Objective.Title)))
	}
	for _, t := range board.Digest.ActiveLeases {
		rows = append(rows, "lease "+compactSummaryLine(t))
	}
	for _, t := range board.Digest.ReadyQueue {
		rows = append(rows, "ready "+compactSummaryLine(t))
	}
	for _, t := range board.Digest.BlockedTasks {
		rows = append(rows, "blocked "+compactSummaryLine(t))
	}
	return writeBudgetedItems(w, header, rows, "", FormatCompact, opts.MaxTokens)
}

func formatBoardTSV(w io.Writer, board BoardView, opts TokenBudgetOptions) error {
	header := "section\tid\tstate\tdetail\n"
	var rows []string
	rows = append(rows, fmt.Sprintf("project\t%s\t%s\t%s", cleanOneLine(board.Project.Slug), board.Project.Status, cleanOneLine(board.Project.Name)))
	for _, op := range board.Objectives {
		rows = append(rows, fmt.Sprintf("objective\t%s\t%d/%d\t%s", cleanOneLine(op.Objective.Slug), op.CompletedTasks, op.TotalTasks, cleanOneLine(op.Objective.Title)))
	}
	for _, t := range board.Digest.ActiveLeases {
		rows = append(rows, fmt.Sprintf("lease\t%s\t%s\t%s", t.Key, t.Status, cleanOneLine(t.Title)))
	}
	for _, t := range board.Digest.ReadyQueue {
		rows = append(rows, fmt.Sprintf("ready\t%s\t%s\t%s", t.Key, t.Status, cleanOneLine(t.Title)))
	}
	for _, t := range board.Digest.BlockedTasks {
		rows = append(rows, fmt.Sprintf("blocked\t%s\t%s\t%s", t.Key, t.Status, cleanOneLine(t.Title)))
	}
	return writeBudgetedItems(w, header, rows, "", FormatTSV, opts.MaxTokens)
}

func formatAuditRecordsCompact(w io.Writer, records []audit.Record, opts TokenBudgetOptions) error {
	rows := make([]string, 0, len(records))
	for _, r := range records {
		rows = append(rows, fmt.Sprintf(
			"%s %s %s:%s @%s",
			r.OccurredAt.UTC().Format("2006-01-02T15:04:05Z"),
			r.Action,
			r.ResourceType,
			r.ResourceID,
			r.ActorHandle,
		))
	}
	return writeBudgetedItems(w, "", rows, "audit none\n", FormatCompact, opts.MaxTokens)
}

func formatAuditRecordsTSV(w io.Writer, records []audit.Record, opts TokenBudgetOptions) error {
	header := "occurred_at\taction\tactor_handle\tactor_kind\tresource_type\tresource_id\n"
	rows := make([]string, 0, len(records))
	for _, r := range records {
		rows = append(rows, fmt.Sprintf(
			"%s\t%s\t%s\t%s\t%s\t%s",
			r.OccurredAt.UTC().Format("2006-01-02T15:04:05Z"),
			r.Action,
			cleanOneLine(r.ActorHandle),
			r.ActorKind,
			cleanOneLine(r.ResourceType),
			cleanOneLine(r.ResourceID),
		))
	}
	return writeBudgetedItems(w, header, rows, "", FormatTSV, opts.MaxTokens)
}
