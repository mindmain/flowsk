// File objective.go implements the `flowsk objective` (aliases: `obj`, `goal`,
// `goals`) CLI command group for creating, listing, inspecting, updating, and
// closing strategic goals, milestones, and timeboxed cycles in the terminal.
package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type objectiveCommandDeps interface {
	ObjectiveDependency
	LocalPathDependency
	FormatterDependency
}

type objectiveHandler struct {
	opts       *GlobalOptions
	client     ObjectiveClient
	localPaths LocalPathRegistry
	formatter  OutputFormatter
}

// newObjectiveCommand constructs `flowsk objective` (aliases: `obj`, `goal`,
// `goals`) and its `create`, `list`, `get`, `update`, and `close` subcommands
// with immediate constructor dependency resolution.
func newObjectiveCommand(opts *GlobalOptions, deps objectiveCommandDeps) *cobra.Command {
	var (
		client     ObjectiveClient
		localPaths LocalPathRegistry
		formatter  OutputFormatter
	)
	if deps != nil {
		client = deps.ObjectiveClient()
		localPaths = deps.LocalPathRegistry()
		formatter = deps.OutputFormatter()
	}
	h := &objectiveHandler{
		opts:       opts,
		client:     client,
		localPaths: localPaths,
		formatter:  formatter,
	}

	objCmd := &cobra.Command{
		Use:     "objective",
		Aliases: []string{"obj", "goal", "goals"},
		Short:   "Manage strategic goals, project objectives, milestones, and cycles grouping tasks",
		Long: `Manages high-level goals, milestones, and cycles within a logical project and
displays live task completion progress bars in the terminal so developers and
AI agents can align daily tasks with measurable project outcomes.`,
	}

	objCmd.AddCommand(
		h.newCreateSubcommand(),
		h.newListSubcommand(),
		h.newGetSubcommand(),
		h.newUpdateSubcommand(),
		h.newCloseSubcommand(),
	)

	return objCmd
}

func (h *objectiveHandler) newCreateSubcommand() *cobra.Command {
	var (
		kind       string
		summary    string
		priority   int
		targetDate string
		owner      string
	)

	cmd := &cobra.Command{
		Use:   "create <title>",
		Short: "Create a new strategic goal, milestone, or cycle inside the active project",
		Example: `  flowsk goal create "Zero-Copy SQLite Sync" --kind goal --priority 1 --target-date 2026-12-01 --owner mario
  flowsk objective create "v1.0 Public Beta" --kind milestone --summary "All core CLI and RPC handlers verified"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("objective create context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			title := strings.TrimSpace(args[0])
			if title == "" {
				return errors.New("objective title must not be empty")
			}
			normKind, err := validateObjectiveKind(kind)
			if err != nil {
				return err
			}
			if priority < 0 || priority > 4 {
				return fmt.Errorf("--priority must be between 0 and 4, got %d", priority)
			}
			if err := validateOptionalDate(targetDate); err != nil {
				return err
			}
			ws, proj, err := resolveActiveProject(ctx, h.opts, h.localPaths, "")
			if err != nil {
				return err
			}
			obj, err := h.client.CreateObjective(ctx, CreateObjectiveInput{
				Workspace:  ws,
				Project:    proj,
				Title:      title,
				Kind:       normKind,
				Summary:    strings.TrimSpace(summary),
				Priority:   priority,
				TargetDate: strings.TrimSpace(targetDate),
				Owner:      strings.TrimSpace(owner),
				Actor:      strings.TrimSpace(h.opts.Actor),
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, obj)
		},
	}

	cmd.Flags().StringVar(&kind, "kind", "goal", "Objective classification: goal|milestone|cycle")
	cmd.Flags().StringVar(&summary, "summary", "", "Concise goal or milestone outcome criteria")
	cmd.Flags().IntVar(&priority, "priority", 2, "Priority rank: 0=none, 1=urgent, 2=high, 3=medium, 4=low")
	cmd.Flags().StringVar(&targetDate, "target-date", "", "Optional target date in YYYY-MM-DD format")
	cmd.Flags().StringVar(&owner, "owner", "", "Responsible actor handle")
	return cmd
}

func (h *objectiveHandler) newListSubcommand() *cobra.Command {
	var (
		kind   string
		status string
		owner  string
		limit  int
		cursor string
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List goals, milestones, and cycles with task completion progress",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("objective list context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			if limit <= 0 {
				return fmt.Errorf("--limit must be > 0, got %d", limit)
			}
			normKind := ""
			if strings.TrimSpace(kind) != "" {
				var err error
				normKind, err = validateObjectiveKind(kind)
				if err != nil {
					return err
				}
			}
			ws, proj, err := resolveActiveProject(ctx, h.opts, h.localPaths, "")
			if err != nil {
				return err
			}
			res, err := h.client.ListObjectives(ctx, ListObjectivesInput{
				Workspace: ws,
				Project:   proj,
				Kind:      normKind,
				Status:    strings.ToLower(strings.TrimSpace(status)),
				Owner:     strings.TrimSpace(owner),
				Limit:     limit,
				Cursor:    strings.TrimSpace(cursor),
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, res)
		},
	}

	cmd.Flags().StringVar(&kind, "kind", "", "Filter by classification: goal|milestone|cycle")
	cmd.Flags().StringVar(&status, "status", "", "Filter by status: planned|active|at_risk|achieved|dropped")
	cmd.Flags().StringVar(&owner, "owner", "", "Filter by owner actor handle")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum objectives to return")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Opaque pagination cursor")
	return cmd
}

func (h *objectiveHandler) newGetSubcommand() *cobra.Command {
	return &cobra.Command{
		Use:   "get <objective-id-or-key>",
		Short: "Inspect an objective and its task completion roll-up",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("objective get context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			ref := strings.TrimSpace(args[0])
			if ref == "" {
				return errors.New("objective identifier must not be empty")
			}
			ws, proj := resolveOptionalProject(ctx, h.opts, h.localPaths)
			obj, err := h.client.GetObjective(ctx, ws, proj, ref)
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, obj)
		},
	}
}

func (h *objectiveHandler) newUpdateSubcommand() *cobra.Command {
	var (
		title      string
		summary    string
		status     string
		priority   int
		targetDate string
		owner      string
	)

	cmd := &cobra.Command{
		Use:   "update <objective-id-or-key>",
		Short: "Update objective fields, status, or priority",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("objective update context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			ref := strings.TrimSpace(args[0])
			if ref == "" {
				return errors.New("objective identifier must not be empty")
			}
			ws, proj := resolveOptionalProject(ctx, h.opts, h.localPaths)
			in := UpdateObjectiveInput{
				Workspace: ws,
				Project:   proj,
				Objective: ref,
				Actor:     strings.TrimSpace(h.opts.Actor),
			}
			changed := false
			if cmd.Flags().Changed("title") {
				v := strings.TrimSpace(title)
				in.Title = &v
				changed = true
			}
			if cmd.Flags().Changed("summary") {
				v := strings.TrimSpace(summary)
				in.Summary = &v
				changed = true
			}
			if cmd.Flags().Changed("status") {
				v := strings.ToLower(strings.TrimSpace(status))
				in.Status = &v
				changed = true
			}
			if cmd.Flags().Changed("priority") {
				if priority < 0 || priority > 4 {
					return fmt.Errorf("--priority must be between 0 and 4, got %d", priority)
				}
				p := priority
				in.Priority = &p
				changed = true
			}
			if cmd.Flags().Changed("target-date") {
				if err := validateOptionalDate(targetDate); err != nil {
					return err
				}
				v := strings.TrimSpace(targetDate)
				in.TargetDate = &v
				changed = true
			}
			if cmd.Flags().Changed("owner") {
				v := strings.TrimSpace(owner)
				in.Owner = &v
				changed = true
			}
			if !changed {
				return errors.New("at least one objective field flag must be specified")
			}
			obj, err := h.client.UpdateObjective(ctx, in)
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, obj)
		},
	}

	cmd.Flags().StringVar(&title, "title", "", "Updated objective title")
	cmd.Flags().StringVar(&summary, "summary", "", "Updated objective outcome summary")
	cmd.Flags().StringVar(&status, "status", "", "Updated status: planned|active|at_risk|completed|canceled")
	cmd.Flags().IntVar(&priority, "priority", -1, "Updated priority rank (0..4)")
	cmd.Flags().StringVar(&targetDate, "target-date", "", "Updated YYYY-MM-DD target date")
	cmd.Flags().StringVar(&owner, "owner", "", "Updated owner actor handle")
	return cmd
}

func (h *objectiveHandler) newCloseSubcommand() *cobra.Command {
	var (
		resolution string
		note       string
	)

	cmd := &cobra.Command{
		Use:   "close <objective-id-or-key>",
		Short: "Close an objective as completed, canceled, or superseded",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("objective close context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			ref := strings.TrimSpace(args[0])
			if ref == "" {
				return errors.New("objective identifier must not be empty")
			}
			normRes := strings.ToLower(strings.TrimSpace(resolution))
			switch normRes {
			case "completed", "achieved", "canceled", "cancelled", "superseded", "dropped":
			default:
				return fmt.Errorf("invalid --resolution %q: must be completed, canceled, or superseded", resolution)
			}
			ws, proj := resolveOptionalProject(ctx, h.opts, h.localPaths)
			obj, err := h.client.CloseObjective(ctx, CloseObjectiveInput{
				Workspace:  ws,
				Project:    proj,
				Objective:  ref,
				Resolution: normRes,
				Note:       strings.TrimSpace(note),
				Actor:      strings.TrimSpace(h.opts.Actor),
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, obj)
		},
	}

	cmd.Flags().StringVar(&resolution, "resolution", "completed", "Closure resolution: completed|canceled|superseded")
	cmd.Flags().StringVar(&note, "note", "", "Audit summary recorded on objective closure")
	return cmd
}

func validateObjectiveKind(kind string) (string, error) {
	norm := strings.ToLower(strings.TrimSpace(kind))
	switch norm {
	case "goal", "milestone", "cycle":
		return norm, nil
	default:
		return "", fmt.Errorf("invalid objective --kind %q: must be goal, milestone, or cycle", kind)
	}
}

func validateOptionalDate(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	if _, err := time.Parse(time.DateOnly, trimmed); err != nil {
		return fmt.Errorf("invalid --target-date %q: expected YYYY-MM-DD", raw)
	}
	return nil
}
