// Package formatter defines token-budget-aware and human-readable terminal
// serialization contracts for rendering Flowsk workspaces, projects, strategic
// goals/objectives, hierarchical tasks, project boards, and audit logs across
// CLI stdout and MCP responses.
//
// Dual Audience Design Rationale (Human Terminal UX + AI Agent Token Frugality):
//   - Interactive Terminal Operators (`FormatHuman`): Developers inspecting goals,
//     milestones, Kanban boards, blockers, and task hierarchies directly in a TTY
//     receive clean aligned columns, objective progress bars (`3/8 [███░░░░░] 37%`),
//     status badges, and dependency trees that remain readable in any terminal.
//   - Autonomous Coding Agents (`FormatCompact`, `FormatTSV`, `FormatJSONL`):
//     Agents polling queues dozens of times per session avoid verbose JSON noise
//     via single-line symbolic records (for example,
//     `FLW-104 [P0|ready] Add ClaimLease fencing #golang #tx`) strictly bounded by
//     `TokenBudgetOptions` (`MaxTokens`, `TruncateBodyChars`) with deterministic
//     `+N more omitted` continuation trailers.
package formatter

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mindmain/flowsk/internal/domain/audit"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/objective"
	"github.com/mindmain/flowsk/internal/domain/project"
	"github.com/mindmain/flowsk/internal/domain/task"
	"github.com/mindmain/flowsk/internal/domain/workspace"
)

const bytesPerEstimatedToken = 4

// Format enumerates the terminal and wire text encodings supported by OutputFormatter.
type Format string

const (
	// FormatHuman renders clean aligned terminal tables, goal progress bars,
	// Kanban workflow sections, and readable status indicators for interactive CLI users.
	FormatHuman Format = "human"

	// FormatCompact renders ultra-dense, single-line-per-entity symbolic text engineered
	// for minimum LLM BPE token consumption during agent harness polling.
	FormatCompact Format = "compact"

	// FormatTSV renders single-header tab-separated columns ideal for Unix pipelines
	// (`awk`, `fzf`) and low-token tabular LLM ingestion.
	FormatTSV Format = "tsv"

	// FormatJSONL renders newline-delimited JSON objects omitting empty/zero optional fields.
	FormatJSONL Format = "jsonl"

	// FormatJSON renders standard structured JSON documents for programmatic tooling.
	FormatJSON Format = "json"

	// FormatMarkdown renders human- and LLM-readable GitHub-Flavored Markdown tables/sections.
	FormatMarkdown Format = "markdown"

	// FormatMarkdownShort is the short CLI alias ("md") equivalent to FormatMarkdown.
	FormatMarkdownShort Format = "md"
)

// Valid reports whether f is a supported output Format constant.
func (f Format) Valid() bool {
	switch f {
	case FormatHuman, FormatCompact, FormatTSV, FormatJSONL, FormatJSON, FormatMarkdown, FormatMarkdownShort:
		return true
	default:
		return false
	}
}

// TokenBudgetOptions constrains rendered payload size and terminal presentation
// so CLI commands serve clear terminal views to humans while never flooding an
// LLM agent's context window.
type TokenBudgetOptions struct {
	// Format selects the output encoding (defaults to FormatHuman on interactive TTYs
	// or FormatCompact for non-TTY/agent invocations when empty).
	Format Format

	// MaxTokens sets an approximate upper bound on emitted LLM tokens (estimated at
	// ~4 UTF-8 bytes per token when > 0). Excess rows are elided with a deterministic
	// summary trailer indicating how many items remain.
	MaxTokens int

	// TruncateBodyChars caps free-form Markdown descriptions and comment bodies to
	// at most N runes when > 0.
	TruncateBodyChars int

	// TerminalWidth optionally hints the active TTY column width (for example, 120)
	// when rendering FormatHuman tables and progress bars (0 uses safe 100-col layout).
	TerminalWidth int

	// OmitUUIDs suppresses 36-char UUIDs when a compact human/LLM key (`FLW-104`) or
	// slug is already present on the entity.
	OmitUUIDs bool
}

// ObjectiveProgress pairs an Objective (goal, milestone, or cycle) with live task
// completion counts so terminal views can display completion bars and percentages.
type ObjectiveProgress struct {
	// Objective holds the core goal/milestone domain entity.
	Objective objective.Objective

	// TotalTasks is the total number of non-deleted tasks linked to Objective.
	TotalTasks int32

	// CompletedTasks is the count of linked tasks in a completed workflow state.
	CompletedTasks int32

	// BlockedTasks is the count of linked tasks currently blocked by unfinished dependencies.
	BlockedTasks int32
}

// BoardView aggregates a complete terminal dashboard snapshot of a Project,
// combining project metadata, active goals/objectives with progress, workflow
// columns, tasks, active actor leases, and blockers.
type BoardView struct {
	// Project holds the logical project header shown at the top of the terminal board.
	Project project.Project

	// Objectives lists active and planned goals/milestones with task rollup counters.
	Objectives []ObjectiveProgress

	// Digest holds task category totals, ready queue items, active leases, and blockers.
	Digest task.ProjectDigest
}

// OutputFormatter serializes Flowsk domain projections (workspaces, projects,
// goals/objectives, tasks, terminal boards, and audit history) into an
// `io.Writer` while strictly respecting TokenBudgetOptions.
type OutputFormatter interface {
	// FormatWorkspaces renders a list of Workspaces in terminal-human or low-token form.
	FormatWorkspaces(ctx context.Context, w io.Writer, items []workspace.Workspace, opts TokenBudgetOptions) error

	// FormatProject renders a single logical Project summary.
	FormatProject(ctx context.Context, w io.Writer, item project.Project, opts TokenBudgetOptions) error

	// FormatProjects renders a list of logical Projects within a workspace.
	FormatProjects(ctx context.Context, w io.Writer, items []project.Project, opts TokenBudgetOptions) error

	// FormatObjective renders a single Objective (goal/milestone/cycle) and its task progress rollup.
	FormatObjective(ctx context.Context, w io.Writer, item ObjectiveProgress, opts TokenBudgetOptions) error

	// FormatObjectives renders a list of Objectives with completion counters and target dates.
	FormatObjectives(ctx context.Context, w io.Writer, items []ObjectiveProgress, opts TokenBudgetOptions) error

	// FormatTask renders a single full Task aggregate according to opts.
	FormatTask(ctx context.Context, w io.Writer, item task.Task, opts TokenBudgetOptions) error

	// FormatTaskQueue renders a slice of CompactTaskSummary items within the token budget.
	FormatTaskQueue(ctx context.Context, w io.Writer, items []task.CompactTaskSummary, opts TokenBudgetOptions) error

	// FormatProjectDigest renders a situational-awareness ProjectDigest within the token budget.
	FormatProjectDigest(ctx context.Context, w io.Writer, digest task.ProjectDigest, opts TokenBudgetOptions) error

	// FormatBoard renders the unified terminal dashboard (project + goals + tasks + blockers).
	FormatBoard(ctx context.Context, w io.Writer, board BoardView, opts TokenBudgetOptions) error

	// FormatAuditRecords renders immutable workspace audit trail entries.
	FormatAuditRecords(ctx context.Context, w io.Writer, records []audit.Record, opts TokenBudgetOptions) error
}

// Formatter is the stateless, thread-safe implementation of OutputFormatter.
type Formatter struct{}

// FormatterSkeleton is retained as a type alias of Formatter for 100% backward
// compatibility with callers referencing the initial type name.
type FormatterSkeleton = Formatter

// Compile-time check ensuring *Formatter satisfies OutputFormatter.
var _ OutputFormatter = (*Formatter)(nil)

// NewOutputFormatter constructs a stateless, concurrency-safe OutputFormatter.
func NewOutputFormatter() *FormatterSkeleton {
	return &Formatter{}
}

// FormatWorkspaces renders a slice of Workspaces across any supported Format.
func (f *Formatter) FormatWorkspaces(ctx context.Context, w io.Writer, items []workspace.Workspace, opts TokenBudgetOptions) error {
	const op = "formatter.OutputFormatter.FormatWorkspaces"
	resolved, err := validateCall(ctx, w, op, opts)
	if err != nil {
		return err
	}
	switch resolved {
	case FormatHuman:
		return formatWorkspacesHuman(w, items, opts)
	case FormatCompact:
		return formatWorkspacesCompact(w, items, opts)
	case FormatTSV:
		return formatWorkspacesTSV(w, items, opts)
	case FormatJSONL:
		return formatWorkspacesJSONL(w, items, opts)
	case FormatJSON:
		return formatWorkspacesJSON(w, items, opts)
	case FormatMarkdown, FormatMarkdownShort:
		return formatWorkspacesMarkdown(w, items, opts)
	default:
		return errs.Validation(op, "unsupported format")
	}
}

// FormatProject renders a single logical Project summary across any supported Format.
func (f *Formatter) FormatProject(ctx context.Context, w io.Writer, item project.Project, opts TokenBudgetOptions) error {
	const op = "formatter.OutputFormatter.FormatProject"
	resolved, err := validateCall(ctx, w, op, opts)
	if err != nil {
		return err
	}
	switch resolved {
	case FormatHuman:
		return formatProjectHuman(w, item, opts)
	case FormatCompact:
		return formatProjectsCompact(w, []project.Project{item}, opts)
	case FormatTSV:
		return formatProjectsTSV(w, []project.Project{item}, opts)
	case FormatJSONL:
		return formatProjectsJSONL(w, []project.Project{item}, opts)
	case FormatJSON:
		return formatSingleJSON(w, projectToJSONMap(item, opts), opts)
	case FormatMarkdown, FormatMarkdownShort:
		return formatProjectMarkdown(w, item, opts)
	default:
		return errs.Validation(op, "unsupported format")
	}
}

// FormatProjects renders a slice of Projects across any supported Format.
func (f *Formatter) FormatProjects(ctx context.Context, w io.Writer, items []project.Project, opts TokenBudgetOptions) error {
	const op = "formatter.OutputFormatter.FormatProjects"
	resolved, err := validateCall(ctx, w, op, opts)
	if err != nil {
		return err
	}
	switch resolved {
	case FormatHuman:
		return formatProjectsHuman(w, items, opts)
	case FormatCompact:
		return formatProjectsCompact(w, items, opts)
	case FormatTSV:
		return formatProjectsTSV(w, items, opts)
	case FormatJSONL:
		return formatProjectsJSONL(w, items, opts)
	case FormatJSON:
		return formatProjectsJSON(w, items, opts)
	case FormatMarkdown, FormatMarkdownShort:
		return formatProjectsMarkdown(w, items, opts)
	default:
		return errs.Validation(op, "unsupported format")
	}
}

// FormatObjective renders a single ObjectiveProgress across any supported Format.
func (f *Formatter) FormatObjective(ctx context.Context, w io.Writer, item ObjectiveProgress, opts TokenBudgetOptions) error {
	const op = "formatter.OutputFormatter.FormatObjective"
	resolved, err := validateCall(ctx, w, op, opts)
	if err != nil {
		return err
	}
	switch resolved {
	case FormatHuman:
		return formatObjectiveHuman(w, item, opts)
	case FormatCompact:
		return formatObjectivesCompact(w, []ObjectiveProgress{item}, opts)
	case FormatTSV:
		return formatObjectivesTSV(w, []ObjectiveProgress{item}, opts)
	case FormatJSONL:
		return formatObjectivesJSONL(w, []ObjectiveProgress{item}, opts)
	case FormatJSON:
		return formatSingleJSON(w, objectiveProgressToJSONMap(item, opts), opts)
	case FormatMarkdown, FormatMarkdownShort:
		return formatObjectiveMarkdown(w, item, opts)
	default:
		return errs.Validation(op, "unsupported format")
	}
}

// FormatObjectives renders a slice of ObjectiveProgress entries across any supported Format.
func (f *Formatter) FormatObjectives(ctx context.Context, w io.Writer, items []ObjectiveProgress, opts TokenBudgetOptions) error {
	const op = "formatter.OutputFormatter.FormatObjectives"
	resolved, err := validateCall(ctx, w, op, opts)
	if err != nil {
		return err
	}
	switch resolved {
	case FormatHuman:
		return formatObjectivesHuman(w, items, opts)
	case FormatCompact:
		return formatObjectivesCompact(w, items, opts)
	case FormatTSV:
		return formatObjectivesTSV(w, items, opts)
	case FormatJSONL:
		return formatObjectivesJSONL(w, items, opts)
	case FormatJSON:
		return formatObjectivesJSON(w, items, opts)
	case FormatMarkdown, FormatMarkdownShort:
		return formatObjectivesMarkdown(w, items, opts)
	default:
		return errs.Validation(op, "unsupported format")
	}
}

// FormatTask renders a full Task aggregate across any supported Format.
func (f *Formatter) FormatTask(ctx context.Context, w io.Writer, item task.Task, opts TokenBudgetOptions) error {
	const op = "formatter.OutputFormatter.FormatTask"
	resolved, err := validateCall(ctx, w, op, opts)
	if err != nil {
		return err
	}
	switch resolved {
	case FormatHuman:
		return formatTaskHuman(w, item, opts)
	case FormatCompact:
		return formatTaskCompact(w, item, opts)
	case FormatTSV:
		return formatTaskTSV(w, item, opts)
	case FormatJSONL:
		return formatJSONLRecords(w, []map[string]any{taskToJSONMap(item, opts)}, opts)
	case FormatJSON:
		return formatSingleJSON(w, taskToJSONMap(item, opts), opts)
	case FormatMarkdown, FormatMarkdownShort:
		return formatTaskMarkdown(w, item, opts)
	default:
		return errs.Validation(op, "unsupported format")
	}
}

// FormatTaskQueue renders CompactTaskSummary items across any supported Format.
func (f *Formatter) FormatTaskQueue(ctx context.Context, w io.Writer, items []task.CompactTaskSummary, opts TokenBudgetOptions) error {
	const op = "formatter.OutputFormatter.FormatTaskQueue"
	resolved, err := validateCall(ctx, w, op, opts)
	if err != nil {
		return err
	}
	switch resolved {
	case FormatHuman:
		return formatTaskQueueHuman(w, items, opts)
	case FormatCompact:
		return formatTaskQueueCompact(w, items, opts)
	case FormatTSV:
		return formatTaskQueueTSV(w, items, opts)
	case FormatJSONL:
		return formatTaskQueueJSONL(w, items, opts)
	case FormatJSON:
		return formatTaskQueueJSON(w, items, opts)
	case FormatMarkdown, FormatMarkdownShort:
		return formatTaskQueueMarkdown(w, items, opts)
	default:
		return errs.Validation(op, "unsupported format")
	}
}

// FormatProjectDigest renders a situational-awareness ProjectDigest across any supported Format.
func (f *Formatter) FormatProjectDigest(ctx context.Context, w io.Writer, digest task.ProjectDigest, opts TokenBudgetOptions) error {
	const op = "formatter.OutputFormatter.FormatProjectDigest"
	resolved, err := validateCall(ctx, w, op, opts)
	if err != nil {
		return err
	}
	switch resolved {
	case FormatHuman:
		return formatProjectDigestHuman(w, digest, opts)
	case FormatCompact:
		return formatProjectDigestCompact(w, digest, opts)
	case FormatTSV:
		return formatProjectDigestTSV(w, digest, opts)
	case FormatJSONL:
		return formatProjectDigestJSONL(w, digest, opts)
	case FormatJSON:
		return formatSingleJSON(w, digestToJSONMap(digest, opts), opts)
	case FormatMarkdown, FormatMarkdownShort:
		return formatProjectDigestMarkdown(w, digest, opts)
	default:
		return errs.Validation(op, "unsupported format")
	}
}

// FormatBoard renders a unified terminal dashboard BoardView across any supported Format.
func (f *Formatter) FormatBoard(ctx context.Context, w io.Writer, board BoardView, opts TokenBudgetOptions) error {
	const op = "formatter.OutputFormatter.FormatBoard"
	resolved, err := validateCall(ctx, w, op, opts)
	if err != nil {
		return err
	}
	switch resolved {
	case FormatHuman:
		return formatBoardHuman(w, board, opts)
	case FormatCompact:
		return formatBoardCompact(w, board, opts)
	case FormatTSV:
		return formatBoardTSV(w, board, opts)
	case FormatJSONL:
		return formatBoardJSONL(w, board, opts)
	case FormatJSON:
		return formatSingleJSON(w, boardToJSONMap(board, opts), opts)
	case FormatMarkdown, FormatMarkdownShort:
		return formatBoardMarkdown(w, board, opts)
	default:
		return errs.Validation(op, "unsupported format")
	}
}

// FormatAuditRecords renders workspace audit trail entries across any supported Format.
func (f *Formatter) FormatAuditRecords(ctx context.Context, w io.Writer, records []audit.Record, opts TokenBudgetOptions) error {
	const op = "formatter.OutputFormatter.FormatAuditRecords"
	resolved, err := validateCall(ctx, w, op, opts)
	if err != nil {
		return err
	}
	switch resolved {
	case FormatHuman:
		return formatAuditRecordsHuman(w, records, opts)
	case FormatCompact:
		return formatAuditRecordsCompact(w, records, opts)
	case FormatTSV:
		return formatAuditRecordsTSV(w, records, opts)
	case FormatJSONL:
		return formatAuditRecordsJSONL(w, records, opts)
	case FormatJSON:
		return formatAuditRecordsJSON(w, records, opts)
	case FormatMarkdown, FormatMarkdownShort:
		return formatAuditRecordsMarkdown(w, records, opts)
	default:
		return errs.Validation(op, "unsupported format")
	}
}

func validateCall(ctx context.Context, w io.Writer, op string, opts TokenBudgetOptions) (Format, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	if w == nil {
		return "", errs.Validation(op, "output writer is required", errs.FieldViolation{
			Field:       "writer",
			Description: "io.Writer must not be nil",
		})
	}
	if opts.MaxTokens < 0 {
		return "", errs.Validation(op, "invalid token budget", errs.FieldViolation{
			Field:       "max_tokens",
			Description: "max_tokens must be >= 0",
		})
	}
	resolved := opts.Format
	if resolved == "" {
		if isCharacterDevice(w) {
			resolved = FormatHuman
		} else {
			resolved = FormatCompact
		}
	}
	if !resolved.Valid() {
		return "", errs.Validation(op, "unsupported output format", errs.FieldViolation{
			Field:       "format",
			Description: fmt.Sprintf("format %q must be human, compact, tsv, jsonl, json, markdown, or md", resolved),
		})
	}
	return resolved, nil
}

func isCharacterDevice(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

func truncateRunes(s string, maxChars int) string {
	if maxChars <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxChars {
		return s
	}
	if maxChars <= 3 {
		return string(runes[:maxChars])
	}
	return string(runes[:maxChars-3]) + "..."
}

func renderProgressBar(completed, total int32, barWidth int) string {
	if barWidth <= 0 {
		barWidth = 8
	}
	if completed < 0 {
		completed = 0
	}
	if total <= 0 {
		return fmt.Sprintf("0/0 [%s] 0%%", strings.Repeat("░", barWidth))
	}
	capped := completed
	if capped > total {
		capped = total
	}
	pct := (int64(capped) * 100) / int64(total)
	filled := int((int64(capped) * int64(barWidth)) / int64(total))
	if filled == 0 && capped > 0 {
		filled = 1
	}
	if filled > barWidth {
		filled = barWidth
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)
	return fmt.Sprintf("%d/%d [%s] %d%%", completed, total, bar, pct)
}

func priorityBadge(p task.Priority) string {
	switch p {
	case task.PriorityNone:
		return "[NONE]"
	case task.PriorityUrgent:
		return "[URGENT]"
	case task.PriorityHigh:
		return "[HIGH]"
	case task.PriorityMedium:
		return "[MED]"
	case task.PriorityLow:
		return "[LOW]"
	default:
		return fmt.Sprintf("[P%d]", p)
	}
}

func statusBadge(s task.Status, blockers int32) string {
	if s == task.StatusBlocked || blockers > 0 {
		if blockers > 1 {
			return fmt.Sprintf("[BLOCKED:%d]", blockers)
		}
		return "[BLOCKED]"
	}
	return "[" + strings.ToUpper(strings.ReplaceAll(string(s), "_", " ")) + "]"
}

func omittedTrailerLine(format Format, omitted int) string {
	switch format {
	case FormatJSONL:
		return fmt.Sprintf("{\"omitted\":%d,\"summary\":\"+%d more omitted\"}\n", omitted, omitted)
	case FormatMarkdown, FormatMarkdownShort:
		return fmt.Sprintf("_... +%d more omitted_\n", omitted)
	default:
		return fmt.Sprintf("... +%d more omitted\n", omitted)
	}
}

// writeBudgetedItems writes an optional header plus whole-line item blocks into w,
// guaranteeing that when opts.MaxTokens > 0, no item or line is ever cut mid-line
// and an explicit "+N more omitted" trailer is appended whenever items are elided.
func writeBudgetedItems(w io.Writer, header string, items []string, emptyFallback string, format Format, maxTokens int) error {
	if header != "" && !strings.HasSuffix(header, "\n") {
		header += "\n"
	}
	normalized := make([]string, len(items))
	for i, it := range items {
		if !strings.HasSuffix(it, "\n") {
			it += "\n"
		}
		normalized[i] = it
	}

	if len(normalized) == 0 {
		out := header + emptyFallback
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		_, err := io.WriteString(w, out)
		return err
	}

	if maxTokens <= 0 {
		var sb strings.Builder
		sb.WriteString(header)
		for _, it := range normalized {
			sb.WriteString(it)
		}
		_, err := io.WriteString(w, sb.String())
		return err
	}

	maxBytes := maxTokens * bytesPerEstimatedToken
	totalBytes := len(header)
	for _, it := range normalized {
		totalBytes += len(it)
	}
	if totalBytes <= maxBytes {
		var sb strings.Builder
		sb.WriteString(header)
		for _, it := range normalized {
			sb.WriteString(it)
		}
		_, err := io.WriteString(w, sb.String())
		return err
	}

	var sb strings.Builder
	worstTrailer := omittedTrailerLine(format, len(normalized))
	if header != "" && len(header)+len(worstTrailer) <= maxBytes {
		sb.WriteString(header)
	}

	kept := 0
	for i, it := range normalized {
		remainingAfterThis := len(normalized) - (i + 1)
		nextTrailerLen := 0
		if remainingAfterThis > 0 {
			nextTrailerLen = len(omittedTrailerLine(format, remainingAfterThis))
		}
		if sb.Len()+len(it)+nextTrailerLen > maxBytes {
			break
		}
		sb.WriteString(it)
		kept++
	}

	omitted := len(normalized) - kept
	if omitted > 0 {
		sb.WriteString(omittedTrailerLine(format, omitted))
	}
	_, err := io.WriteString(w, sb.String())
	return err
}

// writeBudgetedDocument writes multi-line rendered text into w, enforcing whole-line
// truncation with an explicit "+N more omitted" trailer when maxTokens > 0.
func writeBudgetedDocument(w io.Writer, doc string, format Format, maxTokens int) error {
	if !strings.HasSuffix(doc, "\n") {
		doc += "\n"
	}
	if maxTokens <= 0 || len(doc) <= maxTokens*bytesPerEstimatedToken {
		_, err := io.WriteString(w, doc)
		return err
	}

	rawLines := strings.Split(strings.TrimSuffix(doc, "\n"), "\n")
	return writeBudgetedItems(w, "", rawLines, "", format, maxTokens)
}
