// File render.go implements the built-in multi-format CLI output encoder
// supporting human terminal tables and goal progress bars (`human`), ultra-low-token
// single-line symbolic records (`compact`), tab-separated streams (`tsv`),
// newline-delimited JSON (`jsonl`), indented JSON (`json`), and GitHub-Flavored
// Markdown (`md`/`markdown`), bounded by optional `--max-tokens` budgets.
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"text/tabwriter"
	"unicode/utf8"
)

// ResolveOutputFormat resolves FormatAuto into FormatHuman for interactive TTYs
// (and in-memory non-file writers) or FormatCompact for non-TTY OS pipes, and
// normalizes FormatMarkdownFull ("markdown") to FormatMarkdown ("md").
func ResolveOutputFormat(w io.Writer, format OutputFormat) OutputFormat {
	switch format {
	case "", FormatAuto:
		if f, ok := w.(*os.File); ok {
			if info, err := f.Stat(); err == nil {
				if (info.Mode() & os.ModeCharDevice) != 0 {
					return FormatHuman
				}
				return FormatCompact
			}
		}
		return FormatHuman
	case FormatMarkdownFull:
		return FormatMarkdown
	default:
		return format
	}
}

func renderPayload(w io.Writer, formatter OutputFormatter, opts *GlobalOptions, payload any) error {
	format := FormatAuto
	maxTokens := 0
	if opts != nil {
		format = opts.Format
		maxTokens = opts.MaxTokens
	}
	resolved := ResolveOutputFormat(w, format)
	if formatter != nil {
		return formatter.Format(w, resolved, maxTokens, payload)
	}
	return Render(w, resolved, maxTokens, payload)
}

// Render encodes payload to w in the requested OutputFormat, applying a
// deterministic line-level token ceiling (~4 UTF-8 bytes per token) when
// maxTokens > 0.
func Render(w io.Writer, format OutputFormat, maxTokens int, payload any) error {
	resolved := ResolveOutputFormat(w, format)

	var buf bytes.Buffer
	var err error
	switch resolved {
	case FormatJSON:
		err = renderJSON(&buf, payload)
	case FormatJSONL:
		err = renderJSONL(&buf, payload)
	case FormatTSV:
		err = renderTSV(&buf, payload)
	case FormatCompact:
		err = renderCompact(&buf, payload)
	case FormatMarkdown:
		err = renderMarkdown(&buf, payload)
	case FormatHuman:
		err = renderHuman(&buf, payload)
	default:
		return fmt.Errorf("unsupported output format %q", format)
	}
	if err != nil {
		return err
	}

	out := enforceTokenBudget(buf.String(), maxTokens, resolved)
	_, err = io.WriteString(w, out)
	return err
}

func enforceTokenBudget(text string, maxTokens int, format OutputFormat) string {
	if maxTokens <= 0 || format == FormatJSON {
		return text
	}
	maxBytes := maxTokens * 4
	if len(text) <= maxBytes {
		return text
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	var kept []string
	used := 0
	for _, line := range lines {
		need := len(line) + 1
		if len(kept) > 0 && used+need > maxBytes {
			break
		}
		if len(kept) == 0 && need > maxBytes && maxBytes > 16 {
			kept = append(kept, truncateUTF8RunesByBytes(line, maxBytes-3)+"...")
			break
		}
		kept = append(kept, line)
		used += need
	}
	omitted := len(lines) - len(kept)
	if omitted > 0 {
		kept = append(kept, fmt.Sprintf("... (+%d more lines omitted; --max-tokens=%d)", omitted, maxTokens))
	}
	return strings.Join(kept, "\n") + "\n"
}

func truncateUTF8RunesByBytes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		rLen := utf8.RuneLen(r)
		if rLen < 0 || b.Len()+rLen > maxBytes {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

func renderJSON(w io.Writer, payload any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}

func renderJSONL(w io.Writer, payload any) error {
	items, ok := extractSliceItems(payload)
	if !ok {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "%s\n", data)
		return err
	}
	for _, item := range items {
		data, err := json.Marshal(item)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "%s\n", data); err != nil {
			return err
		}
	}
	return nil
}

func extractSliceItems(payload any) ([]any, bool) {
	switch v := payload.(type) {
	case WorkspaceListResult:
		return toAnySlice(v.Items), true
	case ActorListResult:
		return toAnySlice(v.Items), true
	case ProjectListResult:
		return toAnySlice(v.Items), true
	case ObjectiveListResult:
		return toAnySlice(v.Items), true
	case TaskListResult:
		return toAnySlice(v.Items), true
	case CommentListResult:
		return toAnySlice(v.Items), true
	case AuditEventListResult:
		return toAnySlice(v.Items), true
	case WorkspaceOverviewView:
		return toAnySlice(flattenProjectSummaries(v.Projects)), true
	case InstallSkillView:
		return toAnySlice(v.Items), true
	case DoctorReportView:
		return toAnySlice(v.Checks), true
	case SearchView:
		if v.Scope == "sdk" {
			return toAnySlice(v.SDKMethods), true
		}
		if v.Scope == "goals" {
			return toAnySlice(v.Goals), true
		}
		return toAnySlice(v.Tasks), true
	}
	rv := reflect.ValueOf(payload)
	if rv.IsValid() && rv.Kind() == reflect.Slice {
		out := make([]any, rv.Len())
		for i := range rv.Len() {
			out[i] = rv.Index(i).Interface()
		}
		return out, true
	}
	return nil, false
}

func toAnySlice[T any](in []T) []any {
	out := make([]any, len(in))
	for i := range in {
		out[i] = in[i]
	}
	return out
}

func progressPct(completed, total int) int {
	if total <= 0 {
		return 0
	}
	pct := (completed * 100) / total
	if pct > 100 {
		return 100
	}
	return pct
}

func progressBar(completed, total, width int) string {
	pct := progressPct(completed, total)
	filled := (pct * width) / 100
	if filled > width {
		filled = width
	}
	return fmt.Sprintf("[%s%s] %d/%d (%d%%)",
		strings.Repeat("#", filled),
		strings.Repeat("-", width-filled),
		completed, total, pct,
	)
}

func formatPriorityBadge(p int) string {
	return fmt.Sprintf("P%d", p)
}

func compactTaskLine(t TaskSummaryView) string {
	var extras []string
	if t.ExternalID != "" {
		extras = append(extras, "ext="+t.ExternalID)
	}
	if t.ObjectiveKey != "" {
		extras = append(extras, "obj="+t.ObjectiveKey)
	}
	if t.ParentKey != "" {
		extras = append(extras, "parent="+t.ParentKey)
	}
	if t.Holder != "" {
		extras = append(extras, "@"+t.Holder)
	} else if t.Assignee != "" {
		extras = append(extras, "->"+t.Assignee)
	}
	if t.Blocked {
		if len(t.BlockedBy) > 0 {
			extras = append(extras, "blocked_by="+strings.Join(t.BlockedBy, ","))
		} else {
			extras = append(extras, "BLOCKED")
		}
	}
	for _, lb := range t.Labels {
		extras = append(extras, "#"+lb)
	}
	suffix := ""
	if len(extras) > 0 {
		suffix = " " + strings.Join(extras, " ")
	}
	return fmt.Sprintf("%s [%s|%s] %s%s", t.Key, formatPriorityBadge(t.Priority), t.State, t.Title, suffix)
}

func compactObjectiveLine(o ObjectiveView) string {
	pct := progressPct(o.CompletedTasks, o.TotalTasks)
	var tail []string
	if o.TargetDate != "" {
		tail = append(tail, "due="+o.TargetDate)
	}
	if o.Owner != "" {
		tail = append(tail, "@"+o.Owner)
	}
	suffix := ""
	if len(tail) > 0 {
		suffix = " " + strings.Join(tail, " ")
	}
	return fmt.Sprintf("%s [%s|%s|%s] %d/%d (%d%%) %s%s",
		o.Key, o.Kind, o.Status, formatPriorityBadge(o.Priority),
		o.CompletedTasks, o.TotalTasks, pct, o.Title, suffix)
}

func renderCompact(w io.Writer, payload any) error {
	switch v := payload.(type) {
	case WorkspaceView:
		_, err := fmt.Fprintf(w, "ws %s name=%q ttl=%ds\n", v.Slug, v.Name, v.DefaultLeaseTTLSeconds)
		return err
	case WorkspaceListResult:
		for _, ws := range v.Items {
			_, _ = fmt.Fprintf(w, "%s\t%s\n", ws.Slug, ws.Name)
		}
		return nil
	case WorkspaceUseView:
		_, err := fmt.Fprintf(w, "workspace=%s config=%s\n", v.Workspace, v.ConfigFile)
		return err
	case ActorView:
		_, err := fmt.Fprintf(w, "actor %s [%s] role=%s model=%s\n", v.Handle, v.Kind, v.Role, v.Model)
		return err
	case ActorListResult:
		for _, a := range v.Items {
			_, _ = fmt.Fprintf(w, "%s [%s] role=%s model=%s\n", a.Handle, a.Kind, a.Role, a.Model)
		}
		return nil
	case ProjectView:
		_, err := fmt.Fprintf(w, "proj %s [%s] %s\n", v.Key, v.Status, v.Name)
		return err
	case ProjectListResult:
		for _, p := range v.Items {
			_, _ = fmt.Fprintf(w, "%s [%s] %s\n", p.Key, p.Status, p.Name)
		}
		return nil
	case DeleteProjectResultView:
		_, err := fmt.Fprintf(w, "deleted_project=%s id=%s\n", v.DeletedKey, v.DeletedProjectID)
		return err
	case ProjectBindingView:
		_, err := fmt.Fprintf(w, "bind %s -> %s subpath=%s\n", v.ParentKey, v.ChildKey, v.ScopeSubpath)
		return err
	case []ProjectBindingView:
		for _, b := range v {
			_, _ = fmt.Fprintf(w, "%s -> %s subpath=%s\n", b.ParentKey, b.ChildKey, b.ScopeSubpath)
		}
		return nil
	case UnbindSubprojectResultView:
		_, err := fmt.Fprintf(w, "unbind %s -> %s removed=%t\n", v.ParentProject, v.ChildProject, v.Removed)
		return err
	case InitResultView:
		return renderCompactInitResult(w, v)
	case WorkspaceOverviewView:
		return renderCompactWorkspaceOverview(w, v)
	case LocalProjectBinding:
		_, err := fmt.Fprintf(w, "%s -> %s/%s\n", v.LocalDir, v.WorkspaceID, v.ProjectID)
		return err
	case []LocalProjectBinding:
		for _, b := range v {
			_, _ = fmt.Fprintf(w, "%s -> %s/%s\n", b.LocalDir, b.WorkspaceID, b.ProjectID)
		}
		return nil
	case UnlinkResultView:
		_, err := fmt.Fprintf(w, "unlinked=%d dirs=%s\n", v.Count, strings.Join(v.RemovedDirs, ","))
		return err
	case WorkflowStateView:
		_, err := fmt.Fprintf(w, "state %s [%s] pos=%d\n", v.Name, v.Category, v.Position)
		return err
	case []WorkflowStateView:
		for _, st := range v {
			_, _ = fmt.Fprintf(w, "%d:%s[%s]\n", st.Position, st.Name, st.Category)
		}
		return nil
	case LabelView:
		_, err := fmt.Fprintf(w, "label #%s %s\n", v.Name, v.Color)
		return err
	case []LabelView:
		for _, lb := range v {
			_, _ = fmt.Fprintf(w, "#%s\t%s\t%s\n", lb.Name, lb.Color, lb.Description)
		}
		return nil
	case ObjectiveView:
		_, err := fmt.Fprintln(w, compactObjectiveLine(v))
		return err
	case ObjectiveListResult:
		for _, o := range v.Items {
			_, _ = fmt.Fprintln(w, compactObjectiveLine(o))
		}
		return nil
	case TaskView:
		return renderCompactTaskDetail(w, v)
	case TaskListResult:
		for _, t := range v.Items {
			_, _ = fmt.Fprintln(w, compactTaskLine(t))
		}
		return nil
	case NextTaskResult:
		if v.Task == nil {
			_, err := fmt.Fprintln(w, "next=none")
			return err
		}
		return renderCompactTaskDetail(w, *v.Task)
	case CommentView:
		_, err := fmt.Fprintf(w, "comment %s [%s] @%s: %s\n", v.ID, v.Kind, v.Author, singleLine(v.Body))
		return err
	case CommentListResult:
		for _, c := range v.Items {
			_, _ = fmt.Fprintf(w, "%s [%s] @%s: %s\n", c.ID, c.Kind, c.Author, singleLine(c.Body))
		}
		return nil
	case ProjectDigestView:
		return renderCompactDigest(w, v)
	case BoardView:
		return renderCompactBoard(w, v)
	case SearchView:
		return renderCompactSearch(w, v)
	case ExecuteScriptResult:
		return renderCompactExecResult(w, v)
	case LanguageGuideView:
		if v.WrittenToPath != "" {
			_, err := fmt.Fprintf(w, "guide section=%s written=%s\n", v.Section, v.WrittenToPath)
			return err
		}
		_, err := io.WriteString(w, ensureTrailingNewline(v.Markdown))
		return err
	case AuditEventView:
		_, err := fmt.Fprintf(w, "%s %s %s:%s @%s %s\n", v.ID, v.Action, v.EntityType, v.EntityID, v.Actor, v.Summary)
		return err
	case AuditEventListResult:
		for _, ev := range v.Items {
			_, _ = fmt.Fprintf(w, "%s %s %s:%s @%s %s\n", ev.ID, ev.Action, ev.EntityType, ev.EntityID, ev.Actor, ev.Summary)
		}
		return nil
	case MigrationResult:
		_, err := fmt.Fprintf(w, "migrate %s driver=%s version=%d count=%d\n", v.Direction, v.Driver, v.CurrentVersion, v.AppliedCount)
		return err
	case []MigrationStatusEntry:
		for _, m := range v {
			_, _ = fmt.Fprintf(w, "%d\tapplied=%t\t%s\n", m.Version, m.Applied, m.Name)
		}
		return nil
	case DBPingView:
		_, err := fmt.Fprintf(w, "db=%s status=%s latency_ms=%d\n", v.Driver, v.Status, v.LatencyMS)
		return err
	case ConfigPathsView:
		_, err := fmt.Fprintf(w, "config_dir=%s config_file=%s local_paths_file=%s local_db_file=%s\n",
			v.ConfigDir, v.ConfigFile, v.LocalPathsFile, v.LocalDBFile)
		return err
	case ConfigShowView:
		_, err := fmt.Fprintf(w, "mode=%s workspace=%s project=%s actor=%s format=%s auth_token=%s db=%s server=%s\n",
			v.Mode, v.Workspace, v.Project, v.Actor, v.Format, v.AuthToken, v.DBPath, v.ServerURL)
		return err
	case ConfigInitView:
		_, err := fmt.Fprintf(w, "initialized config=%s local_paths=%s overwritten=%t\n",
			v.ConfigFile, v.LocalPathsFile, v.Overwritten)
		return err
	case MCPToolsView:
		for _, t := range v.Tools {
			_, _ = fmt.Fprintf(w, "tool %s readonly=%t idempotent=%t\n", t.Name, t.ReadOnly, t.Idempotent)
		}
		for _, r := range v.Resources {
			_, _ = fmt.Fprintf(w, "resource %s (%s)\n", r.URI, r.MIMEType)
		}
		return nil
	case MCPGuideView:
		for _, s := range v.Snippets {
			_, _ = fmt.Fprintf(w, "[%s] (%s)\n%s\n", s.Client, s.ConfigPathHint, strings.TrimSpace(s.ConfigSnippet))
		}
		return nil
	case MarkdownDocView:
		if v.Action == "get" {
			if !v.HasMarkdownDoc {
				_, err := fmt.Fprintf(w, "doc %s:%s exists=false\n", v.EntityKind, v.EntityKey)
				return err
			}
			_, err := io.WriteString(w, ensureTrailingNewline(v.Markdown))
			return err
		}
		_, err := fmt.Fprintf(w, "doc %s:%s action=%s exists=%t\n", v.EntityKind, v.EntityKey, v.Action, v.HasMarkdownDoc)
		return err
	case PurgeArchivedView:
		_, err := fmt.Fprintf(w, "purged_archived tasks=%d md_files=%d keys=%s\n",
			v.DeletedTaskCount, v.DeletedMarkdownCount, strings.Join(v.DeletedTaskKeys, ","))
		return err
	case OrphanReportView:
		_, err := fmt.Fprintf(w, "orphans total=%d orphan_task_files=%d missing_task_files=%d orphan_project_files=%d missing_project_files=%d\n",
			v.TotalIssues, v.OrphanTaskFilesCount, v.MissingTaskFilesCount, v.OrphanProjectFilesCount, v.MissingProjectFilesCount)
		return err
	case PruneResultView:
		_, err := fmt.Fprintf(w, "prune dry_run=%t total=%d removed_task_md=%d cleared_task_refs=%d removed_proj_md=%d cleared_proj_refs=%d\n",
			v.DryRun, v.TotalCleaned, v.RemovedTaskFilesCount, v.ClearedTaskRefsCount, v.RemovedProjectFilesCount, v.ClearedProjectRefsCount)
		return err
	case InstallSkillView:
		_, _ = fmt.Fprintf(w, "install skill_sha256=%s skill_installed=%d skill_updated=%d skill_unchanged=%d mcp_installed=%d mcp_updated=%d mcp_unchanged=%d\n",
			v.EmbeddedSHA256, v.InstalledCount, v.UpdatedCount, v.UnchangedCount, v.MCPInstalledCount, v.MCPUpdatedCount, v.MCPUnchangedCount)
		for _, it := range v.Items {
			_, _ = fmt.Fprintf(w, "  skill %s action=%s sha256=%s path=%s\n",
				it.ProviderID, it.Action, it.SHA256[:min(len(it.SHA256), 12)], it.SkillPath)
		}
		for _, m := range v.MCPItems {
			_, _ = fmt.Fprintf(w, "  mcp   %s action=%s cmd=%s path=%s\n",
				m.ProviderID, m.Action, m.Command, m.ConfigPath)
		}
		return nil
	case UninstallSkillView:
		_, _ = fmt.Fprintf(w, "uninstall skill_removed=%d skill_absent=%d mcp_removed=%d mcp_absent=%d\n",
			v.RemovedSkillCount, v.NotFoundSkillCount, v.RemovedMCPCount, v.NotFoundMCPCount)
		for _, it := range v.SkillItems {
			_, _ = fmt.Fprintf(w, "  skill %s action=%s path=%s\n", it.ProviderID, it.Action, it.SkillPath)
		}
		for _, m := range v.MCPItems {
			_, _ = fmt.Fprintf(w, "  mcp   %s action=%s path=%s\n", m.ProviderID, m.Action, m.ConfigPath)
		}
		return nil
	case DoctorReportView:
		_, _ = fmt.Fprintf(w, "doctor version=%s mode=%s ok=%d warn=%d fail=%d skill_sha256=%s\n",
			v.Version, v.Mode, v.OKCount, v.WarnCount, v.FailCount, v.EmbeddedSkillSHA256[:min(len(v.EmbeddedSkillSHA256), 12)])
		for _, c := range v.Checks {
			_, _ = fmt.Fprintf(w, "  [%s] %s: %s\n", c.Status, c.Name, c.Summary)
		}
		return nil
	case UpdateCheckView:
		_, err := fmt.Fprintf(w, "update repo=%s status=%s current=%s latest=%s update_available=%t\n",
			v.Repo, v.Status, v.CurrentVersion, v.LatestVersion, v.UpdateAvailable)
		return err
	default:
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "%s\n", data)
		return err
	}
}

func renderCompactTaskDetail(w io.Writer, v TaskView) error {
	holder := ""
	if v.Lease != nil {
		holder = v.Lease.Holder
	}
	sum := TaskSummaryView{
		ID:           v.ID,
		Key:          v.Key,
		ExternalID:   v.ExternalID,
		Title:        v.Title,
		State:        v.State,
		Category:     v.Category,
		Priority:     v.Priority,
		ObjectiveKey: v.ObjectiveKey,
		ParentKey:    v.ParentKey,
		Assignee:     v.Assignee,
		Holder:       holder,
		Labels:       v.Labels,
		Blocked:      v.Blocked,
		BlockedBy:    v.BlockedBy,
	}
	_, _ = fmt.Fprintln(w, compactTaskLine(sum))
	if v.Acceptance != "" {
		_, _ = fmt.Fprintf(w, "  accept: %s\n", singleLine(v.Acceptance))
	}
	for _, st := range v.Subtasks {
		_, _ = fmt.Fprintf(w, "  sub: %s\n", compactTaskLine(st))
	}
	return nil
}

func renderCompactDigest(w io.Writer, v ProjectDigestView) error {
	var counts []string
	keys := make([]string, 0, len(v.CountsByState))
	for k := range v.CountsByState {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		counts = append(counts, fmt.Sprintf("%s=%d", k, v.CountsByState[k]))
	}
	_, _ = fmt.Fprintf(w, "DIGEST %s [%s]\n", v.ProjectKey, strings.Join(counts, " "))
	for _, o := range v.Objectives {
		_, _ = fmt.Fprintf(w, "OBJ %s\n", compactObjectiveLine(o))
	}
	for _, l := range v.ActiveLeases {
		_, _ = fmt.Fprintf(w, "LEASE %s\n", compactTaskLine(l))
	}
	for _, r := range v.ReadyQueue {
		_, _ = fmt.Fprintf(w, "READY %s\n", compactTaskLine(r))
	}
	for _, b := range v.BlockedTasks {
		_, _ = fmt.Fprintf(w, "BLOCKED %s\n", compactTaskLine(b))
	}
	return nil
}

func renderCompactBoard(w io.Writer, v BoardView) error {
	sc := v.StateCounts
	extra := ""
	if sc.Archived > 0 {
		extra += fmt.Sprintf(" archived=%d", sc.Archived)
	}
	if v.OrphanIssuesCount > 0 {
		extra += fmt.Sprintf(" orphans=%d", v.OrphanIssuesCount)
	}
	if sc.Total > 0 || sc.Archived > 0 {
		_, _ = fmt.Fprintf(w, "BOARD %s (%s) total=%d backlog=%d planned=%d in_progress=%d review=%d blocked=%d done=%d canceled=%d claims=%d%s\n",
			v.Project.Key, v.Project.Name,
			sc.Total, sc.Backlog, sc.Planned, sc.InProgress, sc.InReview, sc.Blocked, sc.Done, sc.Canceled, sc.ActiveClaims, extra)
	} else {
		_, _ = fmt.Fprintf(w, "BOARD %s (%s) tasks=%d claims=%d blocked=%d%s\n",
			v.Project.Key, v.Project.Name, v.TotalTasks, v.ActiveClaims, v.BlockedCount, extra)
	}
	for _, wt := range v.Worktrees {
		_, _ = fmt.Fprintf(w, "WORKTREE %s branch=%s sha=%s\n", wt.Path, wt.Branch, wt.ShortSHA)
	}
	for _, sub := range v.Subprojects {
		rc := sub.Counts
		_, _ = fmt.Fprintf(w, "SUBPROJ %s subpath=%s total=%d backlog=%d planned=%d in_progress=%d review=%d blocked=%d done=%d\n",
			sub.Project.Key, sub.ScopeSubpath, rc.Total, rc.Backlog, rc.Planned, rc.InProgress, rc.InReview, rc.Blocked, rc.Done)
	}
	for _, o := range v.Objectives {
		_, _ = fmt.Fprintf(w, "GOAL %s\n", compactObjectiveLine(o))
	}
	for _, col := range v.Columns {
		if col.TotalCount == 0 {
			continue
		}
		_, _ = fmt.Fprintf(w, "[%s:%d]\n", col.State, col.TotalCount)
		for _, t := range col.Tasks {
			_, _ = fmt.Fprintf(w, "  %s\n", compactTaskLine(t))
		}
		if col.OmittedCount > 0 {
			_, _ = fmt.Fprintf(w, "  ... +%d more\n", col.OmittedCount)
		}
	}
	return nil
}

func renderCompactSearch(w io.Writer, v SearchView) error {
	if v.Scope == "sdk" {
		for _, m := range v.SDKMethods {
			_, _ = fmt.Fprintf(w, "%s — %s (e.g. %s)\n", m.Signature, m.Summary, m.Example)
		}
		return nil
	}
	for _, g := range v.Goals {
		_, _ = fmt.Fprintf(w, "GOAL %s\n", compactObjectiveLine(g))
	}
	for _, t := range v.Tasks {
		_, _ = fmt.Fprintln(w, compactTaskLine(t))
	}
	if len(v.Goals) == 0 && len(v.Tasks) == 0 {
		_, _ = fmt.Fprintf(w, "search(%q) matches=0\n", v.Query)
	}
	return nil
}

func renderCompactExecResult(w io.Writer, v ExecuteScriptResult) error {
	if v.FormattedSummary != "" {
		_, err := io.WriteString(w, ensureTrailingNewline(v.FormattedSummary))
		return err
	}
	mode := "commit"
	if v.DryRun {
		mode = "dry_run"
	}
	_, err := fmt.Fprintf(w, "code mode=%s mutations=%d\n", mode, v.AppliedCount)
	return err
}

func renderTSV(w io.Writer, payload any) error {
	switch v := payload.(type) {
	case WorkspaceListResult:
		for _, ws := range v.Items {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", ws.ID, ws.Slug, ws.Name)
		}
		return nil
	case ActorListResult:
		for _, a := range v.Items {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", a.Handle, a.Kind, a.Role, a.Model)
		}
		return nil
	case ProjectListResult:
		for _, p := range v.Items {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", p.Key, p.Status, p.Name)
		}
		return nil
	case []ProjectBindingView:
		for _, b := range v {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", b.ParentKey, b.ChildKey, b.ScopeSubpath)
		}
		return nil
	case WorkspaceOverviewView:
		for _, row := range flattenProjectSummaries(v.Projects) {
			rc := row.Counts
			_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n",
				row.Project.Key, row.ScopeSubpath, rc.Total, rc.Backlog, rc.Planned, rc.InProgress, rc.InReview, rc.Blocked, rc.Done)
		}
		return nil
	case []WorkflowStateView:
		for _, st := range v {
			_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", st.Position, st.Name, st.Category, st.Color)
		}
		return nil
	case []LabelView:
		for _, lb := range v {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", lb.Name, lb.Color, lb.Description)
		}
		return nil
	case ObjectiveListResult:
		for _, o := range v.Items {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d/%d\t%s\n", o.Key, o.Kind, o.Status, o.CompletedTasks, o.TotalTasks, o.Title)
		}
		return nil
	case TaskListResult:
		for _, t := range v.Items {
			_, _ = fmt.Fprintf(w, "%s\tP%d\t%s\t%s\t%s\n", t.Key, t.Priority, t.State, t.Holder, t.Title)
		}
		return nil
	case []MigrationStatusEntry:
		for _, m := range v {
			_, _ = fmt.Fprintf(w, "%d\t%t\t%s\n", m.Version, m.Applied, m.Name)
		}
		return nil
	case AuditEventListResult:
		for _, ev := range v.Items {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", ev.ID, ev.Action, ev.EntityType, ev.EntityID, ev.Actor)
		}
		return nil
	default:
		return renderCompact(w, payload)
	}
}

func renderMarkdown(w io.Writer, payload any) error {
	switch v := payload.(type) {
	case WorkspaceOverviewView:
		return renderMarkdownWorkspaceOverview(w, v)
	case BoardView:
		_, _ = fmt.Fprintf(w, "# Board: %s (%s)\n\n", v.Project.Name, v.Project.Key)
		sc := v.StateCounts
		if sc.Total > 0 {
			_, _ = fmt.Fprintf(w, "- **Total**: %d | **Backlog**: %d | **Planned**: %d | **In Progress**: %d | **Review**: %d | **Blocked**: %d | **Done**: %d | **Active Claims**: %d\n\n",
				sc.Total, sc.Backlog, sc.Planned, sc.InProgress, sc.InReview, sc.Blocked, sc.Done, sc.ActiveClaims)
		} else {
			_, _ = fmt.Fprintf(w, "- **Total Tasks**: %d | **Active Claims**: %d | **Blocked**: %d\n\n", v.TotalTasks, v.ActiveClaims, v.BlockedCount)
		}
		if len(v.Objectives) > 0 {
			_, _ = fmt.Fprintln(w, "## Strategic Goals & Milestones")
			for _, o := range v.Objectives {
				_, _ = fmt.Fprintf(w, "- **%s** (%s, %s): %s — `%s`\n", o.Key, o.Kind, o.Status, o.Title, progressBar(o.CompletedTasks, o.TotalTasks, 10))
			}
			_, _ = fmt.Fprintln(w)
		}
		for _, col := range v.Columns {
			if col.TotalCount == 0 {
				continue
			}
			_, _ = fmt.Fprintf(w, "## %s (%d)\n", col.State, col.TotalCount)
			for _, t := range col.Tasks {
				_, _ = fmt.Fprintf(w, "- %s\n", compactTaskLine(t))
			}
			if col.OmittedCount > 0 {
				_, _ = fmt.Fprintf(w, "- _+%d more omitted_\n", col.OmittedCount)
			}
			_, _ = fmt.Fprintln(w)
		}
		return nil
	case LanguageGuideView:
		if v.WrittenToPath != "" {
			_, err := fmt.Fprintf(w, "Wrote Flowsk Code Mode (%s) guide to `%s`.\n", v.Section, v.WrittenToPath)
			return err
		}
		_, err := io.WriteString(w, ensureTrailingNewline(v.Markdown))
		return err
	default:
		return renderHuman(w, payload)
	}
}

func renderHuman(w io.Writer, payload any) error {
	switch v := payload.(type) {
	case WorkspaceView:
		_, err := fmt.Fprintf(w, "Workspace:   %s (%s)\nID:          %s\nLease TTL:   %ds\nDescription: %s\n",
			v.Slug, v.Name, v.ID, v.DefaultLeaseTTLSeconds, v.Description)
		return err
	case WorkspaceListResult:
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "SLUG\tNAME\tDESCRIPTION")
		for _, ws := range v.Items {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", ws.Slug, ws.Name, ws.Description)
		}
		return tw.Flush()
	case WorkspaceUseView:
		_, err := fmt.Fprintf(w, "Active workspace set to %q in %s\n", v.Workspace, v.ConfigFile)
		return err
	case ActorView:
		_, err := fmt.Fprintf(w, "Actor:   %s (%s)\nKind:    %s\nRole:    %s\nModel:   %s\n",
			v.Handle, v.DisplayName, v.Kind, v.Role, v.Model)
		return err
	case ActorListResult:
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "HANDLE\tKIND\tROLE\tMODEL\tDISPLAY NAME")
		for _, a := range v.Items {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", a.Handle, a.Kind, a.Role, a.Model, a.DisplayName)
		}
		return tw.Flush()
	case ProjectView:
		_, err := fmt.Fprintf(w, "Project:     %s (%s)\nStatus:      %s\nWorkspace:   %s\nDescription: %s\n",
			v.Key, v.Name, v.Status, v.WorkspaceID, v.Description)
		return err
	case ProjectListResult:
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "KEY\tSTATUS\tNAME\tDESCRIPTION")
		for _, p := range v.Items {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.Key, p.Status, p.Name, p.Description)
		}
		return tw.Flush()
	case DeleteProjectResultView:
		_, err := fmt.Fprintf(w, "Deleted project %s (%s); any bound sub-projects were detached non-destructively.\n",
			v.DeletedKey, v.DeletedProjectID)
		return err
	case ProjectBindingView:
		_, err := fmt.Fprintf(w, "Bound sub-project %s under %s (subpath: %s)\n",
			v.ChildKey, v.ParentKey, firstNonEmptyCLI(v.ScopeSubpath, "."))
		return err
	case []ProjectBindingView:
		return renderHumanProjectBindings(w, v)
	case UnbindSubprojectResultView:
		_, err := fmt.Fprintf(w, "Unbound sub-project %s from %s (removed=%t)\n",
			v.ChildProject, v.ParentProject, v.Removed)
		return err
	case InitResultView:
		return renderHumanInitResult(w, v)
	case WorkspaceOverviewView:
		return renderHumanWorkspaceOverview(w, v)
	case LocalProjectBinding:
		_, err := fmt.Fprintf(w, "Local Dir:   %s\nWorkspace:   %s\nProject:     %s\n",
			v.LocalDir, v.WorkspaceID, v.ProjectID)
		return err
	case []LocalProjectBinding:
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "LOCAL DIRECTORY\tWORKSPACE\tPROJECT")
		for _, b := range v {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", b.LocalDir, b.WorkspaceID, b.ProjectID)
		}
		return tw.Flush()
	case UnlinkResultView:
		_, err := fmt.Fprintf(w, "Removed %d local directory binding(s): %s\n", v.Count, strings.Join(v.RemovedDirs, ", "))
		return err
	case WorkflowStateView:
		_, err := fmt.Fprintf(w, "Workflow State: %s [%s] (position %d, color %s)\n", v.Name, v.Category, v.Position, v.Color)
		return err
	case []WorkflowStateView:
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "POS\tNAME\tCATEGORY\tCOLOR")
		for _, st := range v {
			_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", st.Position, st.Name, st.Category, st.Color)
		}
		return tw.Flush()
	case LabelView:
		_, err := fmt.Fprintf(w, "Label: #%s (%s) — %s\n", v.Name, v.Color, v.Description)
		return err
	case []LabelView:
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "LABEL\tCOLOR\tDESCRIPTION")
		for _, lb := range v {
			_, _ = fmt.Fprintf(tw, "#%s\t%s\t%s\n", lb.Name, lb.Color, lb.Description)
		}
		return tw.Flush()
	case ObjectiveView:
		_, err := fmt.Fprintf(w, "Objective: %s — %s\nKind:      %s | Status: %s | Priority: P%d\nProgress:  %s\nOwner:     %s | Target: %s\nSummary:   %s\n",
			v.Key, v.Title, v.Kind, v.Status, v.Priority,
			progressBar(v.CompletedTasks, v.TotalTasks, 10),
			v.Owner, v.TargetDate, v.Summary)
		return err
	case ObjectiveListResult:
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "KEY\tKIND\tSTATUS\tPRIO\tPROGRESS\tDUE\tOWNER\tTITLE")
		for _, o := range v.Items {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\tP%d\t%s\t%s\t%s\t%s\n",
				o.Key, o.Kind, o.Status, o.Priority,
				progressBar(o.CompletedTasks, o.TotalTasks, 8),
				o.TargetDate, o.Owner, o.Title)
		}
		return tw.Flush()
	case TaskView:
		return renderHumanTaskDetail(w, v)
	case TaskListResult:
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "KEY\tPRIO\tSTATE\tCLAIM/ASSIGNEE\tOBJ\tTITLE")
		for _, t := range v.Items {
			actor := t.Assignee
			if t.Holder != "" {
				actor = "@" + t.Holder + " (lease)"
			}
			title := t.Title
			if t.Blocked {
				title = "[BLOCKED] " + title
			}
			_, _ = fmt.Fprintf(tw, "%s\tP%d\t%s\t%s\t%s\t%s\n",
				t.Key, t.Priority, t.State, actor, t.ObjectiveKey, title)
		}
		return tw.Flush()
	case NextTaskResult:
		if v.Task == nil {
			_, err := fmt.Fprintln(w, "No unblocked ready tasks available in the active project.")
			return err
		}
		if v.Claimed {
			_, _ = fmt.Fprintln(w, "Claimed next ready task:")
		}
		return renderHumanTaskDetail(w, *v.Task)
	case CommentView:
		_, err := fmt.Fprintf(w, "[%s] %s (@%s): %s\n", v.Kind, v.ID, v.Author, v.Body)
		return err
	case CommentListResult:
		for _, c := range v.Items {
			_, _ = fmt.Fprintf(w, "- [%s] @%s (%s): %s\n", c.Kind, c.Author, c.ID, strings.TrimSpace(c.Body))
		}
		return nil
	case ProjectDigestView:
		return renderHumanDigest(w, v)
	case BoardView:
		return renderHumanBoard(w, v)
	case SearchView:
		return renderHumanSearch(w, v)
	default:
		return renderHumanAdminAndSystem(w, payload)
	}
}

func renderHumanSearch(w io.Writer, v SearchView) error {
	if v.Scope == "sdk" {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "METHOD\tSIGNATURE\tSUMMARY")
		for _, m := range v.SDKMethods {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", m.Name, m.Signature, m.Summary)
		}
		return tw.Flush()
	}
	if len(v.Goals) == 0 && len(v.Tasks) == 0 {
		_, err := fmt.Fprintf(w, "No matches found for query %q.\n", v.Query)
		return err
	}
	if len(v.Goals) > 0 {
		_, _ = fmt.Fprintf(w, "Goals (%d):\n", len(v.Goals))
		if err := renderHuman(w, ObjectiveListResult{Items: v.Goals}); err != nil {
			return err
		}
		if len(v.Tasks) > 0 {
			_, _ = fmt.Fprintln(w)
		}
	}
	if len(v.Tasks) > 0 {
		_, _ = fmt.Fprintf(w, "Tasks (%d):\n", len(v.Tasks))
		return renderHuman(w, TaskListResult{Items: v.Tasks})
	}
	return nil
}

func renderHumanTaskDetail(w io.Writer, v TaskView) error {
	_, _ = fmt.Fprintf(w, "Task:        %s — %s\nState:       %s | Priority: P%d\n", v.Key, v.Title, v.State, v.Priority)
	if v.ExternalID != "" {
		_, _ = fmt.Fprintf(w, "External ID: %s\n", v.ExternalID)
	}
	if v.ObjectiveKey != "" || v.ParentKey != "" {
		_, _ = fmt.Fprintf(w, "Hierarchy:   objective=%s parent=%s\n", v.ObjectiveKey, v.ParentKey)
	}
	if v.Lease != nil {
		_, _ = fmt.Fprintf(w, "Active Lease: @%s (expires %s)\n", v.Lease.Holder, v.Lease.ExpiresAt.Format("15:04:05Z"))
	} else if v.Assignee != "" {
		_, _ = fmt.Fprintf(w, "Assignee:    %s\n", v.Assignee)
	}
	if len(v.Labels) > 0 {
		_, _ = fmt.Fprintf(w, "Labels:      %s\n", strings.Join(v.Labels, ", "))
	}
	if v.Blocked {
		_, _ = fmt.Fprintf(w, "Blocked By:  %s (%s)\n", strings.Join(v.BlockedBy, ", "), v.BlockerReason)
	}
	if v.Acceptance != "" {
		_, _ = fmt.Fprintf(w, "Acceptance:  %s\n", v.Acceptance)
	}
	if v.Description != "" {
		_, _ = fmt.Fprintf(w, "Description: %s\n", v.Description)
	}
	if len(v.Subtasks) > 0 {
		_, _ = fmt.Fprintln(w, "Subtasks:")
		for _, st := range v.Subtasks {
			_, _ = fmt.Fprintf(w, "  - %s\n", compactTaskLine(st))
		}
	}
	if len(v.Comments) > 0 {
		_, _ = fmt.Fprintln(w, "Comments:")
		for _, c := range v.Comments {
			_, _ = fmt.Fprintf(w, "  - [%s] @%s: %s\n", c.Kind, c.Author, strings.TrimSpace(c.Body))
		}
	}
	return nil
}

func renderHumanDigest(w io.Writer, v ProjectDigestView) error {
	_, _ = fmt.Fprintf(w, "Project Digest: %s (%s)\n", v.ProjectKey, v.ProjectName)
	if len(v.Objectives) > 0 {
		_, _ = fmt.Fprintln(w, "\nGoals & Milestones:")
		for _, o := range v.Objectives {
			_, _ = fmt.Fprintf(w, "  %s  %-10s  %s  %s\n",
				o.Key, o.Kind, progressBar(o.CompletedTasks, o.TotalTasks, 8), o.Title)
		}
	}
	if len(v.ActiveLeases) > 0 {
		_, _ = fmt.Fprintln(w, "\nActive Agent/Human Leases:")
		for _, t := range v.ActiveLeases {
			_, _ = fmt.Fprintf(w, "  %s\n", compactTaskLine(t))
		}
	}
	if len(v.ReadyQueue) > 0 {
		_, _ = fmt.Fprintln(w, "\nReady Queue:")
		for _, t := range v.ReadyQueue {
			_, _ = fmt.Fprintf(w, "  %s\n", compactTaskLine(t))
		}
	}
	if len(v.BlockedTasks) > 0 {
		_, _ = fmt.Fprintln(w, "\nBlocked Tasks:")
		for _, t := range v.BlockedTasks {
			_, _ = fmt.Fprintf(w, "  %s\n", compactTaskLine(t))
		}
	}
	return nil
}

func renderHumanBoard(w io.Writer, v BoardView) error {
	_, _ = fmt.Fprintf(w, "=== PROJECT BOARD: %s — %s [%s] ===\n", v.Project.Key, v.Project.Name, v.Project.Status)
	if len(v.ParentKeys) > 0 {
		_, _ = fmt.Fprintf(w, "Parent Project: %s (subpath: %s)\n",
			strings.Join(v.ParentKeys, ", "), firstNonEmptyCLI(v.ScopeSubpath, "."))
	}
	sc := v.StateCounts
	archSuffix := ""
	if sc.Archived > 0 {
		archSuffix = fmt.Sprintf(" | Archived: %d", sc.Archived)
	}
	if sc.Total > 0 || sc.Archived > 0 {
		_, _ = fmt.Fprintf(w, "Tasks: %d | Backlog: %d | Planned: %d | Progress: %d | Review: %d | Blocked: %d | Done: %d | Active Leases: %d%s\n",
			sc.Total, sc.Backlog, sc.Planned, sc.InProgress, sc.InReview, sc.Blocked, sc.Done, sc.ActiveClaims, archSuffix)
	} else {
		_, _ = fmt.Fprintf(w, "Tasks: %d | Active Leases: %d | Blocked: %d\n", v.TotalTasks, v.ActiveClaims, v.BlockedCount)
	}
	if v.OrphanIssuesCount > 0 {
		_, _ = fmt.Fprintln(w, tuiWarnStyle.Render(fmt.Sprintf("⚠ Warning: %d orphan or missing Markdown resource(s) detected in ~/.flowsk — run 'flowsk prune' to clean up.", v.OrphanIssuesCount)))
	}

	if len(v.Worktrees) > 0 {
		_, _ = fmt.Fprintln(w, "\n--- Local Checkouts & Git Worktrees ---")
		wtw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(wtw, "BRANCH\tLATEST COMMIT\tPATH")
		for _, wt := range v.Worktrees {
			br := wt.Branch
			if br == "" {
				br = "-"
			}
			commitCol := "-"
			if wt.ShortSHA != "" {
				commitCol = fmt.Sprintf("%s %s", wt.ShortSHA, truncateText(singleLine(wt.Subject), 48))
				if wt.RelativeTime != "" {
					commitCol += " (" + wt.RelativeTime + ")"
				}
			}
			_, _ = fmt.Fprintf(wtw, "%s\t%s\t%s\n", br, commitCol, wt.Path)
		}
		_ = wtw.Flush()
	}

	if len(v.Subprojects) > 0 {
		_, _ = fmt.Fprintln(w, "\n--- Sub-Projects (Separate Task Scope) ---")
		stw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(stw, "SUBPROJECT\tSUBPATH\tTOTAL\tBACKLOG\tPLANNED\tPROGRESS\tREVIEW\tBLOCKED\tDONE\tLATEST SUBPATH COMMIT")
		for _, sub := range v.Subprojects {
			rc := sub.Counts
			commitStr := "-"
			if len(sub.Worktrees) > 0 {
				commitStr = formatWorktreeCommitShort(sub.Worktrees[0])
			}
			_, _ = fmt.Fprintf(stw, "%s (%s)\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\n",
				sub.Project.Key, sub.Project.Name, firstNonEmptyCLI(sub.ScopeSubpath, "."),
				rc.Total, rc.Backlog, rc.Planned, rc.InProgress, rc.InReview, rc.Blocked, rc.Done, commitStr)
		}
		_ = stw.Flush()
	}

	if len(v.Objectives) > 0 {
		_, _ = fmt.Fprintln(w, "\n--- Strategic Goals, Milestones & Cycles ---")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "KEY\tKIND\tSTATUS\tPROGRESS\tTITLE")
		for _, o := range v.Objectives {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				o.Key, o.Kind, o.Status, progressBar(o.CompletedTasks, o.TotalTasks, 10), o.Title)
		}
		_ = tw.Flush()
	}

	_, _ = fmt.Fprintln(w, "\n--- Workflow Columns ---")
	for _, col := range v.Columns {
		if col.TotalCount == 0 {
			continue
		}
		_, _ = fmt.Fprintf(w, "\n[%s] (%d)\n", col.State, col.TotalCount)
		for _, t := range col.Tasks {
			_, _ = fmt.Fprintf(w, "  • %s\n", compactTaskLine(t))
		}
		if col.OmittedCount > 0 {
			_, _ = fmt.Fprintf(w, "  ... +%d more in %s\n", col.OmittedCount, col.State)
		}
	}
	return nil
}

func renderHumanAdminAndSystem(w io.Writer, payload any) error {
	switch v := payload.(type) {
	case ExecuteScriptResult:
		mode := "COMMITTED"
		if v.DryRun {
			mode = "DRY-RUN (rolled back)"
		}
		_, _ = fmt.Fprintf(w, "Flowsk Code Mode [%s] — %d mutation(s)\n", mode, v.AppliedCount)
		if strings.TrimSpace(v.FormattedSummary) != "" {
			_, _ = io.WriteString(w, ensureTrailingNewline(v.FormattedSummary))
		}
		return nil
	case LanguageGuideView:
		if v.WrittenToPath != "" {
			_, err := fmt.Fprintf(w, "Exported Flowsk Code Mode (%s) guide to %s\n", v.Section, v.WrittenToPath)
			return err
		}
		_, err := io.WriteString(w, ensureTrailingNewline(v.Markdown))
		return err
	case AuditEventView:
		_, err := fmt.Fprintf(w, "Audit Event: %s\nAction:      %s\nEntity:      %s (%s)\nActor:       %s\nSummary:     %s\n",
			v.ID, v.Action, v.EntityType, v.EntityID, v.Actor, v.Summary)
		return err
	case AuditEventListResult:
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "ID\tACTION\tENTITY\tACTOR\tSUMMARY")
		for _, ev := range v.Items {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s:%s\t%s\t%s\n",
				ev.ID, ev.Action, ev.EntityType, ev.EntityID, ev.Actor, ev.Summary)
		}
		return tw.Flush()
	case MigrationResult:
		_, err := fmt.Fprintf(w, "Database migrate %s (%s): version=%d applied=%d\n",
			v.Direction, v.Driver, v.CurrentVersion, v.AppliedCount)
		return err
	case []MigrationStatusEntry:
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "VERSION\tAPPLIED\tNAME")
		for _, m := range v {
			_, _ = fmt.Fprintf(tw, "%d\t%t\t%s\n", m.Version, m.Applied, m.Name)
		}
		return tw.Flush()
	case DBPingView:
		_, err := fmt.Fprintf(w, "Database ping %s: driver=%s latency=%dms\n", v.Status, v.Driver, v.LatencyMS)
		return err
	case ConfigPathsView:
		_, err := fmt.Fprintf(w, "Config Dir:       %s\nConfig File:      %s\nLocal Paths File: %s\nLocal SQLite DB:  %s\n",
			v.ConfigDir, v.ConfigFile, v.LocalPathsFile, v.LocalDBFile)
		return err
	case ConfigShowView:
		_, err := fmt.Fprintf(w, "Mode:             %s\nConfig File:      %s\nLocal Paths File: %s\nLocal SQLite DB:  %s\nServer URL:       %s\nAuth Token:       %s\nWorkspace:        %s\nProject:          %s\nActor:            %s\nFormat:           %s\nMax Tokens:       %d\n",
			v.Mode, v.ConfigFile, v.LocalPathsFile, v.DBPath, v.ServerURL, v.AuthToken, v.Workspace, v.Project, v.Actor, v.Format, v.MaxTokens)
		return err
	case ConfigInitView:
		_, err := fmt.Fprintf(w, "Initialized Flowsk user configuration:\n  Config File:      %s\n  Local Paths File: %s\n",
			v.ConfigFile, v.LocalPathsFile)
		return err
	case MCPToolsView:
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "TOOL\tREAD-ONLY\tIDEMPOTENT\tTITLE")
		for _, t := range v.Tools {
			_, _ = fmt.Fprintf(tw, "%s\t%t\t%t\t%s\n", t.Name, t.ReadOnly, t.Idempotent, t.Title)
		}
		_ = tw.Flush()
		if v.IncludeSchema {
			for _, t := range v.Tools {
				raw, _ := json.MarshalIndent(t.InputSchema, "  ", "  ")
				_, _ = fmt.Fprintf(w, "\nSchema (%s):\n  %s\n", t.Name, raw)
			}
		}
		return nil
	case MCPGuideView:
		for _, s := range v.Snippets {
			_, _ = fmt.Fprintf(w, "=== MCP Onboarding: %s (%s) ===\n%s\n\nAgent Prompt Hint:\n  %s\n\n",
				s.Client, s.ConfigPathHint, strings.TrimSpace(s.ConfigSnippet), s.SystemPrompt)
		}
		return nil
	case MarkdownDocView:
		switch v.Action {
		case "get":
			if !v.HasMarkdownDoc {
				_, err := fmt.Fprintf(w, "No Markdown document stored for %s %s.\n", v.EntityKind, v.EntityKey)
				return err
			}
			_, err := io.WriteString(w, ensureTrailingNewline(v.Markdown))
			return err
		case "delete":
			_, err := fmt.Fprintf(w, "Deleted Markdown document for %s %s.\n", v.EntityKind, v.EntityKey)
			return err
		default:
			_, err := fmt.Fprintf(w, "Saved Markdown document for %s %s (%d bytes).\n", v.EntityKind, v.EntityKey, len(v.Markdown))
			return err
		}
	case PurgeArchivedView:
		if v.DeletedTaskCount == 0 {
			_, err := fmt.Fprintln(w, "No archived tasks to purge.")
			return err
		}
		_, err := fmt.Fprintf(w, "Permanently purged %d archived task(s) and %d .md resource(s): %s\n",
			v.DeletedTaskCount, v.DeletedMarkdownCount, strings.Join(v.DeletedTaskKeys, ", "))
		return err
	case OrphanReportView:
		if v.TotalIssues == 0 {
			_, err := fmt.Fprintln(w, "No orphan .md files or broken database references found.")
			return err
		}
		_, _ = fmt.Fprintln(w, tuiWarnStyle.Render(fmt.Sprintf("Detected %d resource issue(s) (orphan task .md=%d, missing task .md=%d, orphan project .md=%d, missing project .md=%d):",
			v.TotalIssues, v.OrphanTaskFilesCount, v.MissingTaskFilesCount, v.OrphanProjectFilesCount, v.MissingProjectFilesCount)))
		for _, line := range v.IssueSummaries {
			_, _ = fmt.Fprintf(w, "  - %s\n", line)
		}
		return nil
	case PruneResultView:
		mode := "Pruned"
		if v.DryRun {
			mode = "Dry-run matched"
		}
		if v.TotalCleaned == 0 {
			_, err := fmt.Fprintln(w, "Workspace resources are clean — zero orphan .md files or broken references.")
			return err
		}
		_, _ = fmt.Fprintf(w, "%s %d item(s) (task .md=%d, task refs=%d, project .md=%d, project refs=%d):\n",
			mode, v.TotalCleaned, v.RemovedTaskFilesCount, v.ClearedTaskRefsCount, v.RemovedProjectFilesCount, v.ClearedProjectRefsCount)
		for _, line := range v.CleanedSummaries {
			_, _ = fmt.Fprintf(w, "  - %s\n", line)
		}
		return nil
	case InstallSkillView:
		return renderHumanInstallSkill(w, v)
	case UninstallSkillView:
		return renderHumanUninstallSkill(w, v)
	case DoctorReportView:
		return renderHumanDoctorReport(w, v)
	case UpdateCheckView:
		return renderHumanUpdateCheck(w, v)
	default:
		return renderJSON(w, payload)
	}
}

func singleLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func ensureTrailingNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}
