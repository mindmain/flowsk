// File task.go implements the `flowsk task` (aliases: `issue`, `t`) CLI command
// group for hierarchical tasks, dependency blockers, agent leases, and digests.
package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type taskCommandDeps interface {
	TaskDependency
	LocalPathDependency
	FormatterDependency
}

type taskHandler struct {
	opts       *GlobalOptions
	client     TaskClient
	localPaths LocalPathRegistry
	formatter  OutputFormatter
}

// newTaskCommand constructs `flowsk task` (aliases: `issue`, `t`) and all 13
// subcommands (`create`, `list`, `get`, `update`, `move`, `claim`, `release`,
// `block`, `unblock`, `comment`, `comments`, `next`, `digest`).
func newTaskCommand(opts *GlobalOptions, deps taskCommandDeps) *cobra.Command {
	var (
		client     TaskClient
		localPaths LocalPathRegistry
		formatter  OutputFormatter
	)
	if deps != nil {
		client = deps.TaskClient()
		localPaths = deps.LocalPathRegistry()
		formatter = deps.OutputFormatter()
	}
	h := &taskHandler{
		opts:       opts,
		client:     client,
		localPaths: localPaths,
		formatter:  formatter,
	}

	taskCmd := &cobra.Command{
		Use:     "task",
		Aliases: []string{"issue", "t"},
		Short:   "Manage hierarchical tasks, atomic agent claims, blockers, comments, next-queue, and digests",
		Long: `Manages Linear-style tasks and subtasks optimized for multi-agent software engineering.

Tip for AI Agents:
  - Poll work in minimal tokens with 'flowsk task next' or 'flowsk task digest'.
  - When creating or updating 3+ tasks/dependencies at once, prefer 'flowsk lang exec'
    to execute the entire batch atomically in a single round-trip.`,
	}

	taskCmd.AddCommand(
		h.newCreateSubcommand(),
		h.newListSubcommand(),
		h.newGetSubcommand(),
		h.newUpdateSubcommand(),
		h.newMoveSubcommand(),
		h.newClaimSubcommand(),
		h.newReleaseSubcommand(),
		h.newBlockSubcommand(),
		h.newUnblockSubcommand(),
		h.newCommentSubcommand(),
		h.newCommentsSubcommand(),
		h.newDocSubcommand(),
		h.newArchiveSubcommand(),
		h.newPurgeArchivedSubcommand(),
		h.newNextSubcommand(),
		h.newDigestSubcommand(),
	)

	return taskCmd
}

func (h *taskHandler) newCreateSubcommand() *cobra.Command {
	var (
		description string
		externalID  string
		objective   string
		parent      string
		state       string
		priority    int
		assignee    string
		labels      []string
		estimate    int
		dependsOn   []string
		acceptance  string
	)

	cmd := &cobra.Command{
		Use:     "create <title>",
		Short:   "Create a task or subtask in the active project",
		Example: `  flowsk task create "Implement SQLite WAL snapshotter" --objective CORE-O1 --external-id CU-42 --priority 1 --label storage`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("task create context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			title := strings.TrimSpace(args[0])
			if title == "" {
				return errors.New("task title must not be empty")
			}
			if priority < 0 || priority > 4 {
				return fmt.Errorf("--priority must be between 0 and 4, got %d", priority)
			}
			if estimate < 0 {
				return fmt.Errorf("--estimate must be >= 0, got %d", estimate)
			}
			ws, proj, err := resolveActiveProject(ctx, h.opts, h.localPaths, "")
			if err != nil {
				return err
			}
			tv, err := h.client.CreateTask(ctx, CreateTaskInput{
				Workspace:   ws,
				Project:     proj,
				Title:       title,
				ExternalID:  strings.TrimSpace(externalID),
				Description: strings.TrimSpace(description),
				Objective:   strings.TrimSpace(objective),
				Parent:      strings.TrimSpace(parent),
				State:       strings.TrimSpace(state),
				Priority:    priority,
				Assignee:    strings.TrimSpace(assignee),
				Labels:      cleanStringSlice(labels),
				Estimate:    estimate,
				DependsOn:   cleanStringSlice(dependsOn),
				Acceptance:  strings.TrimSpace(acceptance),
				Actor:       strings.TrimSpace(h.opts.Actor),
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, tv)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&description, "description", "", "Concise technical task specification")
	flags.StringVar(&externalID, "external-id", "", "Optional project-unique external tracker ID (e.g. ClickUp, Linear, Jira, GitHub)")
	flags.StringVar(&objective, "objective", "", "Parent objective UUID or key")
	flags.StringVar(&parent, "parent", "", "Optional parent task UUID or key for hierarchical subtasks")
	flags.StringVar(&state, "state", "", "Initial workflow state name or ID (defaults to project's default state)")
	flags.IntVar(&priority, "priority", 2, "Priority rank: 0=none, 1=urgent, 2=high, 3=medium, 4=low")
	flags.StringVar(&assignee, "assignee", "", "Initial assignee actor handle")
	flags.StringSliceVar(&labels, "label", nil, "Labels to attach (repeatable or comma-separated)")
	flags.IntVar(&estimate, "estimate", 0, "Optional complexity estimate points")
	flags.StringSliceVar(&dependsOn, "depends-on", nil, "Prerequisite task keys that must complete before this task is unblocked")
	flags.StringVar(&acceptance, "acceptance", "", "Deterministic verification / acceptance criteria")
	return cmd
}

func (h *taskHandler) newListSubcommand() *cobra.Command {
	var (
		objective    string
		parent       string
		externalID   string
		state        string
		priority     int
		assignee     string
		labels       []string
		unclaimed    bool
		blocked      bool
		onlyDone     bool
		onlyInProg   bool
		onlyTodo     bool
		onlyArchived bool
		searchQ      string
		limit        int
		cursor       string
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List or search tasks with simple AI & terminal filters (--done, --in-progress, --archived, --external-id, -q)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("task list context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			if limit <= 0 {
				return fmt.Errorf("--limit must be > 0, got %d", limit)
			}
			if cmd.Flags().Changed("priority") && (priority < 0 || priority > 4) {
				return fmt.Errorf("--priority must be between 0 and 4, got %d", priority)
			}
			effState := strings.TrimSpace(state)
			switch {
			case onlyArchived && effState == "":
				effState = "archived"
			case onlyDone && effState == "":
				effState = "done"
			case onlyInProg && effState == "":
				effState = "in_progress"
			case onlyTodo && effState == "":
				effState = "todo"
			}
			ws, proj, err := resolveActiveProject(ctx, h.opts, h.localPaths, "")
			if err != nil {
				return err
			}
			res, err := h.client.ListTasks(ctx, ListTasksInput{
				Workspace:  ws,
				Project:    proj,
				Objective:  strings.TrimSpace(objective),
				Parent:     strings.TrimSpace(parent),
				ExternalID: strings.TrimSpace(externalID),
				State:      effState,
				Priority:   priority,
				Assignee:   strings.TrimSpace(assignee),
				Labels:     cleanStringSlice(labels),
				Unclaimed:  unclaimed,
				Blocked:    blocked,
				Query:      strings.TrimSpace(searchQ),
				Limit:      limit,
				Cursor:     strings.TrimSpace(cursor),
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, res)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&objective, "objective", "", "Filter by objective UUID or key")
	flags.StringVar(&parent, "parent", "", "Filter subtasks of a parent task UUID or key")
	flags.StringVar(&externalID, "external-id", "", "Filter by exact external tracker ID")
	flags.StringVar(&state, "state", "", "Filter by workflow state name or category")
	flags.BoolVar(&onlyDone, "done", false, "Only return completed (Done) tasks")
	flags.BoolVar(&onlyInProg, "in-progress", false, "Only return In Progress tasks")
	flags.BoolVar(&onlyTodo, "todo", false, "Only return Todo/Ready tasks")
	flags.BoolVar(&onlyArchived, "archived", false, "Only return Archived tasks")
	flags.StringVarP(&searchQ, "search", "q", "", "GitHub-style search query (e.g. 'state:done #storage sqlite')")
	flags.IntVar(&priority, "priority", -1, "Filter by exact priority rank (0..4)")
	flags.StringVar(&assignee, "assignee", "", "Filter by assigned/claiming actor handle")
	flags.StringSliceVar(&labels, "label", nil, "Require all specified labels")
	flags.BoolVar(&unclaimed, "unclaimed", false, "Only return tasks without an active actor claim")
	flags.BoolVar(&blocked, "blocked", false, "Only return tasks currently blocked by unfinished dependencies")
	flags.IntVar(&limit, "limit", 50, "Maximum tasks to return")
	flags.StringVar(&cursor, "cursor", "", "Opaque pagination cursor")
	return cmd
}

func (h *taskHandler) newGetSubcommand() *cobra.Command {
	var (
		includeComments bool
		includeSubtasks bool
		includeDeps     bool
	)

	cmd := &cobra.Command{
		Use:   "get <task-id-or-key>",
		Short: "Fetch a task by UUID or key (e.g. CORE-42) in token-budgeted detail",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("task get context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			ref := strings.TrimSpace(args[0])
			if ref == "" {
				return errors.New("task identifier must not be empty")
			}
			ws, proj := resolveOptionalProject(ctx, h.opts, h.localPaths)
			tv, err := h.client.GetTask(ctx, GetTaskInput{
				Workspace:       ws,
				Project:         proj,
				Task:            ref,
				IncludeComments: includeComments,
				IncludeSubtasks: includeSubtasks,
				IncludeDeps:     includeDeps,
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, tv)
		},
	}

	cmd.Flags().BoolVar(&includeComments, "include-comments", true, "Include recent handoff and decision comments")
	cmd.Flags().BoolVar(&includeSubtasks, "include-subtasks", true, "Include child subtask roll-up")
	cmd.Flags().BoolVar(&includeDeps, "include-deps", true, "Include upstream/downstream blocking edges")
	return cmd
}

func (h *taskHandler) newUpdateSubcommand() *cobra.Command {
	var (
		title        string
		externalID   string
		description  string
		objective    string
		parent       string
		priority     int
		assignee     string
		addLabels    []string
		removeLabels []string
		estimate     int
		acceptance   string
	)

	cmd := &cobra.Command{
		Use:   "update <task-id-or-key>",
		Short: "Modify task metadata, external ID, hierarchy links, priority, or labels",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("task update context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			ref := strings.TrimSpace(args[0])
			if ref == "" {
				return errors.New("task identifier must not be empty")
			}
			ws, proj := resolveOptionalProject(ctx, h.opts, h.localPaths)
			in := UpdateTaskInput{
				Workspace:    ws,
				Project:      proj,
				Task:         ref,
				AddLabels:    cleanStringSlice(addLabels),
				RemoveLabels: cleanStringSlice(removeLabels),
				Actor:        strings.TrimSpace(h.opts.Actor),
			}
			changed := len(in.AddLabels) > 0 || len(in.RemoveLabels) > 0
			if cmd.Flags().Changed("title") {
				v := strings.TrimSpace(title)
				in.Title = &v
				changed = true
			}
			if cmd.Flags().Changed("external-id") {
				v := strings.TrimSpace(externalID)
				in.ExternalID = &v
				changed = true
			}
			if cmd.Flags().Changed("description") {
				v := strings.TrimSpace(description)
				in.Description = &v
				changed = true
			}
			if cmd.Flags().Changed("objective") {
				v := strings.TrimSpace(objective)
				in.Objective = &v
				changed = true
			}
			if cmd.Flags().Changed("parent") {
				v := strings.TrimSpace(parent)
				in.Parent = &v
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
			if cmd.Flags().Changed("assignee") {
				v := strings.TrimSpace(assignee)
				in.Assignee = &v
				changed = true
			}
			if cmd.Flags().Changed("estimate") {
				if estimate < 0 {
					return fmt.Errorf("--estimate must be >= 0, got %d", estimate)
				}
				est := estimate
				in.Estimate = &est
				changed = true
			}
			if cmd.Flags().Changed("acceptance") {
				v := strings.TrimSpace(acceptance)
				in.Acceptance = &v
				changed = true
			}
			if !changed {
				return errors.New("at least one task update flag must be specified")
			}
			tv, err := h.client.UpdateTask(ctx, in)
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, tv)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&title, "title", "", "Updated task title")
	flags.StringVar(&externalID, "external-id", "", "Updated project-unique external tracker ID (pass \"\" to clear)")
	flags.StringVar(&description, "description", "", "Updated technical specification")
	flags.StringVar(&objective, "objective", "", "Reassign objective UUID or key")
	flags.StringVar(&parent, "parent", "", "Reassign parent task UUID or key")
	flags.IntVar(&priority, "priority", -1, "Updated priority rank (0..4)")
	flags.StringVar(&assignee, "assignee", "", "Updated assignee actor handle")
	flags.StringSliceVar(&addLabels, "add-label", nil, "Labels to attach")
	flags.StringSliceVar(&removeLabels, "remove-label", nil, "Labels to detach")
	flags.IntVar(&estimate, "estimate", -1, "Updated complexity estimate")
	flags.StringVar(&acceptance, "acceptance", "", "Updated verification / acceptance criteria")
	return cmd
}

func (h *taskHandler) newMoveSubcommand() *cobra.Command {
	var note string

	cmd := &cobra.Command{
		Use:     "move <task-id-or-key> <state>",
		Short:   "Transition a task into another workflow state",
		Example: `  flowsk task move CORE-42 "In Review" --note "All unit + race tests green"`,
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("task move context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			ref := strings.TrimSpace(args[0])
			targetState := strings.TrimSpace(args[1])
			if ref == "" || targetState == "" {
				return errors.New("both task identifier and target state are required")
			}
			ws, proj := resolveOptionalProject(ctx, h.opts, h.localPaths)
			tv, err := h.client.MoveTask(ctx, MoveTaskInput{
				Workspace: ws,
				Project:   proj,
				Task:      ref,
				State:     targetState,
				Note:      strings.TrimSpace(note),
				Actor:     strings.TrimSpace(h.opts.Actor),
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, tv)
		},
	}

	cmd.Flags().StringVar(&note, "note", "", "Optional transition note recorded in audit trail")
	return cmd
}

func (h *taskHandler) newClaimSubcommand() *cobra.Command {
	var (
		leaseTTL time.Duration
		note     string
	)

	cmd := &cobra.Command{
		Use:   "claim <task-id-or-key>",
		Short: "Atomically claim exclusive lease ownership of a task for --actor",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("task claim context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			actor := strings.TrimSpace(h.opts.Actor)
			if actor == "" {
				return errors.New("--actor (or FLOWSK_ACTOR) is required when claiming a task")
			}
			if leaseTTL <= 0 {
				return fmt.Errorf("--lease-ttl must be > 0, got %s", leaseTTL)
			}
			ref := strings.TrimSpace(args[0])
			if ref == "" {
				return errors.New("task identifier must not be empty")
			}
			ws, proj := resolveOptionalProject(ctx, h.opts, h.localPaths)
			tv, err := h.client.ClaimTask(ctx, ClaimTaskInput{
				Workspace: ws,
				Project:   proj,
				Task:      ref,
				Actor:     actor,
				LeaseTTL:  leaseTTL,
				Note:      strings.TrimSpace(note),
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, tv)
		},
	}

	cmd.Flags().DurationVar(&leaseTTL, "lease-ttl", 45*time.Minute, "Exclusive claim lease duration before automatic expiry")
	cmd.Flags().StringVar(&note, "note", "", "Optional execution intent note")
	return cmd
}

func (h *taskHandler) newReleaseSubcommand() *cobra.Command {
	var (
		targetState string
		note        string
	)

	cmd := &cobra.Command{
		Use:   "release <task-id-or-key>",
		Short: "Release an active actor lease and optionally transition workflow state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("task release context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			ref := strings.TrimSpace(args[0])
			if ref == "" {
				return errors.New("task identifier must not be empty")
			}
			ws, proj := resolveOptionalProject(ctx, h.opts, h.localPaths)
			tv, err := h.client.ReleaseTask(ctx, ReleaseTaskInput{
				Workspace:   ws,
				Project:     proj,
				Task:        ref,
				Actor:       strings.TrimSpace(h.opts.Actor),
				TargetState: strings.TrimSpace(targetState),
				Note:        strings.TrimSpace(note),
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, tv)
		},
	}

	cmd.Flags().StringVar(&targetState, "state", "", "Optional target state upon release (e.g. Todo, Review, Done)")
	cmd.Flags().StringVar(&note, "note", "", "Handoff note for the next human or agent")
	return cmd
}

func (h *taskHandler) newBlockSubcommand() *cobra.Command {
	var (
		blockedBy []string
		reason    string
	)

	cmd := &cobra.Command{
		Use:     "block <task-id-or-key>",
		Short:   "Mark a task as blocked by upstream tasks or an external impediment",
		Example: `  flowsk task block CORE-45 --by CORE-41,CORE-42 --reason "Awaiting protobuf contract merge"`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("task block context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			ref := strings.TrimSpace(args[0])
			if ref == "" {
				return errors.New("task identifier must not be empty")
			}
			by := cleanStringSlice(blockedBy)
			trimmedReason := strings.TrimSpace(reason)
			if len(by) == 0 && trimmedReason == "" {
				return errors.New("specify at least --by or --reason when blocking a task")
			}
			ws, proj := resolveOptionalProject(ctx, h.opts, h.localPaths)
			tv, err := h.client.BlockTask(ctx, BlockTaskInput{
				Workspace: ws,
				Project:   proj,
				Task:      ref,
				BlockedBy: by,
				Reason:    trimmedReason,
				Actor:     strings.TrimSpace(h.opts.Actor),
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, tv)
		},
	}

	cmd.Flags().StringSliceVar(&blockedBy, "by", nil, "Blocking upstream task keys or UUIDs")
	cmd.Flags().StringVar(&reason, "reason", "", "Concise blocker explanation")
	return cmd
}

func (h *taskHandler) newUnblockSubcommand() *cobra.Command {
	var (
		clearedBy []string
		note      string
	)

	cmd := &cobra.Command{
		Use:   "unblock <task-id-or-key>",
		Short: "Clear dependency blockers or external impediments on a task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("task unblock context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			ref := strings.TrimSpace(args[0])
			if ref == "" {
				return errors.New("task identifier must not be empty")
			}
			ws, proj := resolveOptionalProject(ctx, h.opts, h.localPaths)
			tv, err := h.client.UnblockTask(ctx, UnblockTaskInput{
				Workspace: ws,
				Project:   proj,
				Task:      ref,
				ClearedBy: cleanStringSlice(clearedBy),
				Note:      strings.TrimSpace(note),
				Actor:     strings.TrimSpace(h.opts.Actor),
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, tv)
		},
	}

	cmd.Flags().StringSliceVar(&clearedBy, "by", nil, "Specific upstream blocker task keys to detach (omit to clear impediment state)")
	cmd.Flags().StringVar(&note, "note", "", "Optional unblock resolution note")
	return cmd
}

func (h *taskHandler) newCommentSubcommand() *cobra.Command {
	var (
		message   string
		fromStdin bool
		kind      string
	)

	cmd := &cobra.Command{
		Use:     "comment <task-id-or-key>",
		Short:   "Append a structured note, handoff summary, decision, or verification log",
		Example: `  flowsk task comment CORE-42 -m "Passed go test -race ./internal/adapter/..." --kind verification`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("task comment context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			ref := strings.TrimSpace(args[0])
			if ref == "" {
				return errors.New("task identifier must not be empty")
			}
			normKind, err := validateCommentKind(kind)
			if err != nil {
				return err
			}
			body, err := readExclusiveTextInput(cmd.InOrStdin(), message, "", fromStdin, "comment")
			if err != nil {
				return err
			}
			ws, proj := resolveOptionalProject(ctx, h.opts, h.localPaths)
			cv, err := h.client.AddComment(ctx, AddCommentInput{
				Workspace: ws,
				Project:   proj,
				Task:      ref,
				Actor:     strings.TrimSpace(h.opts.Actor),
				Kind:      normKind,
				Body:      strings.TrimSpace(body),
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, cv)
		},
	}

	cmd.Flags().StringVarP(&message, "message", "m", "", "Comment Markdown text")
	cmd.Flags().BoolVar(&fromStdin, "stdin", false, "Read comment Markdown body from standard input")
	cmd.Flags().StringVar(&kind, "kind", "note", "Structured comment type: note|handoff|decision|verification|progress|blocker")
	return cmd
}

func (h *taskHandler) newCommentsSubcommand() *cobra.Command {
	var (
		limit  int
		cursor string
	)

	cmd := &cobra.Command{
		Use:   "comments <task-id-or-key>",
		Short: "List comments and agent handoff notes on a task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("task comments context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			if limit <= 0 {
				return fmt.Errorf("--limit must be > 0, got %d", limit)
			}
			ref := strings.TrimSpace(args[0])
			if ref == "" {
				return errors.New("task identifier must not be empty")
			}
			ws, proj := resolveOptionalProject(ctx, h.opts, h.localPaths)
			res, err := h.client.ListComments(ctx, ListCommentsInput{
				Workspace: ws,
				Project:   proj,
				Task:      ref,
				Limit:     limit,
				Cursor:    strings.TrimSpace(cursor),
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, res)
		},
	}

	cmd.Flags().IntVar(&limit, "limit", 25, "Maximum comments to return")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Opaque pagination cursor")
	return cmd
}

func (h *taskHandler) newNextSubcommand() *cobra.Command {
	var (
		objective string
		labels    []string
		claim     bool
		leaseTTL  time.Duration
	)

	cmd := &cobra.Command{
		Use:   "next",
		Short: "Fetch (and optionally claim) the next highest-priority unblocked ready task in minimal tokens",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("task next context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			actor := strings.TrimSpace(h.opts.Actor)
			if claim {
				if actor == "" {
					return errors.New("--actor (or FLOWSK_ACTOR) is required when --claim is set")
				}
				if leaseTTL <= 0 {
					return fmt.Errorf("--lease-ttl must be > 0, got %s", leaseTTL)
				}
			}
			ws, proj, err := resolveActiveProject(ctx, h.opts, h.localPaths, "")
			if err != nil {
				return err
			}
			res, err := h.client.NextTask(ctx, NextTaskInput{
				Workspace: ws,
				Project:   proj,
				Objective: strings.TrimSpace(objective),
				Labels:    cleanStringSlice(labels),
				Claim:     claim,
				Actor:     actor,
				LeaseTTL:  leaseTTL,
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, res)
		},
	}

	cmd.Flags().StringVar(&objective, "objective", "", "Restrict selection to a specific objective UUID or key")
	cmd.Flags().StringSliceVar(&labels, "label", nil, "Restrict selection to tasks carrying all specified labels")
	cmd.Flags().BoolVar(&claim, "claim", false, "Atomically claim the returned task for --actor in the same transaction")
	cmd.Flags().DurationVar(&leaseTTL, "lease-ttl", 45*time.Minute, "Lease duration when --claim is enabled")
	return cmd
}

func (h *taskHandler) newDigestSubcommand() *cobra.Command {
	var (
		objective      string
		includeBlocked bool
		limit          int
	)

	cmd := &cobra.Command{
		Use:     "digest",
		Short:   "Render an ultra-compact project board, active lease, ready-queue, and blocker snapshot",
		Example: `  flowsk task digest --max-tokens 400`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("task digest context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			if limit <= 0 {
				return fmt.Errorf("--limit must be > 0, got %d", limit)
			}
			ws, proj, err := resolveActiveProject(ctx, h.opts, h.localPaths, "")
			if err != nil {
				return err
			}
			digest, err := h.client.Digest(ctx, TaskDigestInput{
				Workspace:      ws,
				Project:        proj,
				Objective:      strings.TrimSpace(objective),
				IncludeBlocked: includeBlocked,
				Limit:          limit,
				MaxTokens:      h.opts.MaxTokens,
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, digest)
		},
	}

	cmd.Flags().StringVar(&objective, "objective", "", "Optional objective UUID or key to scope the digest")
	cmd.Flags().BoolVar(&includeBlocked, "include-blocked", true, "Include active dependency blockers section")
	cmd.Flags().IntVar(&limit, "limit", 25, "Maximum tasks to summarize across ready/in-progress queues")
	return cmd
}

func (h *taskHandler) newDocSubcommand() *cobra.Command {
	var (
		setMarkdown string
		fromStdin   bool
		clearDoc    bool
	)

	cmd := &cobra.Command{
		Use:     "doc <task-id-or-key>",
		Short:   "Read, write, or clear the extended Markdown (.md) document for a task in ~/.flowsk/tasks/",
		Example: `  flowsk task doc CORE-42 --set "# Design Notes\n\nDetailed specification..."`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("task doc context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			ref := strings.TrimSpace(args[0])
			if ref == "" {
				return errors.New("task identifier must not be empty")
			}
			ws, _ := resolveOptionalProject(ctx, h.opts, h.localPaths)
			if clearDoc {
				if cmd.Flags().Changed("set") || fromStdin {
					return errors.New("--clear cannot be combined with --set or --stdin")
				}
				res, dErr := h.client.DeleteTaskMarkdown(ctx, ws, ref, strings.TrimSpace(h.opts.Actor))
				if dErr != nil {
					return dErr
				}
				return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, res)
			}
			if cmd.Flags().Changed("set") || fromStdin {
				body, rErr := readExclusiveTextInput(cmd.InOrStdin(), setMarkdown, "", fromStdin, "markdown")
				if rErr != nil {
					return rErr
				}
				res, pErr := h.client.PutTaskMarkdown(ctx, ws, ref, strings.TrimSpace(h.opts.Actor), body)
				if pErr != nil {
					return pErr
				}
				return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, res)
			}
			res, err := h.client.GetTaskMarkdown(ctx, ws, ref)
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, res)
		},
	}

	cmd.Flags().StringVarP(&setMarkdown, "set", "m", "", "Write Markdown content to the task's .md document")
	cmd.Flags().BoolVar(&fromStdin, "stdin", false, "Read Markdown content from standard input")
	cmd.Flags().BoolVar(&clearDoc, "clear", false, "Delete the task's .md document")
	return cmd
}

func (h *taskHandler) newArchiveSubcommand() *cobra.Command {
	var note string

	cmd := &cobra.Command{
		Use:     "archive <task-id-or-key>",
		Short:   "Move a task into the Archived state (hidden from active boards, eligible for purge-archived)",
		Example: `  flowsk task archive CORE-42 --note "Superseded by v2 architecture"`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("task archive context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			ref := strings.TrimSpace(args[0])
			if ref == "" {
				return errors.New("task identifier must not be empty")
			}
			ws, proj := resolveOptionalProject(ctx, h.opts, h.localPaths)
			tv, err := h.client.MoveTask(ctx, MoveTaskInput{
				Workspace: ws,
				Project:   proj,
				Task:      ref,
				State:     "archived",
				Note:      strings.TrimSpace(note),
				Actor:     strings.TrimSpace(h.opts.Actor),
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, tv)
		},
	}

	cmd.Flags().StringVar(&note, "note", "", "Optional note recorded when archiving the task")
	return cmd
}

func (h *taskHandler) newPurgeArchivedSubcommand() *cobra.Command {
	return &cobra.Command{
		Use:     "purge-archived",
		Aliases: []string{"empty-archive"},
		Short:   "Permanently delete all archived tasks across the workspace from the DB and remove their .md files",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("task purge-archived context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			ws := defaultWorkspace(h.opts.Workspace)
			res, err := h.client.PurgeArchivedTasks(ctx, ws, strings.TrimSpace(h.opts.Actor))
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, res)
		},
	}
}

func cleanStringSlice(in []string) []string {
	var out []string
	for _, s := range in {
		if trimmed := strings.TrimSpace(s); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func validateCommentKind(kind string) (string, error) {
	norm := strings.ToLower(strings.TrimSpace(kind))
	switch norm {
	case "note", "handoff", "decision", "verification", "progress", "blocker":
		return norm, nil
	default:
		return "", fmt.Errorf("invalid comment --kind %q: must be note, handoff, decision, verification, progress, or blocker", kind)
	}
}
