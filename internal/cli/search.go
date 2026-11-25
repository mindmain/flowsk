// File search.go implements the top-level `flowsk search` (aliases: `find`, `s`)
// and `flowsk code` (alias: `exec`) commands providing Cloudflare Code Mode's
// two ultra-ergonomic primitives (`search` + JavaScript `execute`) directly in
// the CLI for AI agents and human developers.
package cli

import (
	"fmt"
	"strings"

	"github.com/mindmain/flowsk/internal/language"
	"github.com/spf13/cobra"
)

type searchCommandDeps interface {
	ObjectiveDependency
	TaskDependency
	LocalPathDependency
	FormatterDependency
}

// newSearchCommand constructs `flowsk search [query...]` (aliases: `find`, `s`)
// supporting GitHub/Linear-style filters (`state:done`, `state:in_progress`,
// `is:open`, `is:blocked`, `#tag`, `@actor`, free text) plus `--type sdk` to
// inspect `flowsk.*` Code Mode JavaScript methods.
func newSearchCommand(opts *GlobalOptions, deps searchCommandDeps) *cobra.Command {
	var (
		objectives ObjectiveClient
		tasks      TaskClient
		localPaths LocalPathRegistry
		formatter  OutputFormatter
	)
	if deps != nil {
		objectives = deps.ObjectiveClient()
		tasks = deps.TaskClient()
		localPaths = deps.LocalPathRegistry()
		formatter = deps.OutputFormatter()
	}

	var (
		scopeFlag  string
		stateFlag  string
		onlyDone   bool
		onlyInProg bool
		onlyTodo   bool
		onlyBlock  bool
		labelFlags []string
		actorFlag  string
		objFlag    string
		limit      int
	)

	cmd := &cobra.Command{
		Use:     "search [query...]",
		Aliases: []string{"find", "s"},
		Short:   "Search tasks, goals, or Code Mode JS SDK methods with simple AI-friendly filters",
		Long: `Searches tasks, strategic goals, or Flowsk Code Mode JavaScript SDK methods using
natural GitHub/Linear-style filters and free-text keywords:
  - State:    state:done, state:in_progress, state:todo, state:blocked, is:open, is:done, is:unclaimed
  - Metadata: #tag / label:rpc, @actor / actor:claude, obj:v1, prio:high
  - SDK Docs: type:sdk claim  (or --type sdk)`,
		Example: `  # Show only completed tasks (ultra-compact for AI when piped)
  flowsk search --done

  # Search in-progress tasks mentioning "sqlite" or carrying #storage
  flowsk search "state:in_progress #storage sqlite"

  # Discover JavaScript SDK methods for claiming tasks in Code Mode
  flowsk search --type sdk claim`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("search context canceled: %w", err)
			}
			if limit <= 0 {
				return fmt.Errorf("--limit must be > 0, got %d", limit)
			}

			rawQuery := strings.TrimSpace(strings.Join(args, " "))
			parsed := language.ParseSearchQuery(rawQuery)

			scope := strings.ToLower(strings.TrimSpace(scopeFlag))
			if scope == "" {
				scope = string(parsed.Scope)
			}
			switch scope {
			case "sdk", "api", "code":
				docs := language.SearchCodeModeSDK(rawQuery)
				sdkOut := make([]SDKMethodView, 0, len(docs))
				for _, d := range docs {
					sdkOut = append(sdkOut, SDKMethodView{
						Name:      d.Name,
						Category:  d.Category,
						Signature: d.Signature,
						Summary:   d.Summary,
						Example:   d.Example,
					})
				}
				return renderPayload(cmd.OutOrStdout(), formatter, opts, SearchView{
					Query:      rawQuery,
					Scope:      "sdk",
					SDKMethods: sdkOut,
				})
			case "all", "tasks", "task", "goals", "goal", "objectives":
			default:
				return fmt.Errorf("invalid --type %q: must be all, tasks, goals, or sdk", scopeFlag)
			}

			if tasks == nil || objectives == nil {
				return ErrNotImplemented
			}

			ws, proj, err := resolveActiveProject(ctx, opts, localPaths, "")
			if err != nil {
				return err
			}

			effState := strings.TrimSpace(stateFlag)
			switch {
			case onlyDone && effState == "":
				effState = "done"
			case onlyInProg && effState == "":
				effState = "in_progress"
			case onlyTodo && effState == "":
				effState = "todo"
			}

			normScope := "all"
			switch scope {
			case "tasks", "task":
				normScope = "tasks"
			case "goals", "goal", "objectives":
				normScope = "goals"
			}

			out := SearchView{
				Query: rawQuery,
				Scope: normScope,
			}

			if normScope == "all" || normScope == "tasks" {
				tRes, tErr := tasks.ListTasks(ctx, ListTasksInput{
					Workspace: ws,
					Project:   proj,
					Objective: strings.TrimSpace(objFlag),
					State:     effState,
					Assignee:  strings.TrimSpace(actorFlag),
					Labels:    cleanStringSlice(labelFlags),
					Blocked:   onlyBlock,
					Query:     rawQuery,
					Priority:  -1,
					Limit:     limit,
				})
				if tErr != nil {
					return tErr
				}
				out.Tasks = tRes.Items
			}

			if (normScope == "all" && effState == "" && !onlyBlock && len(labelFlags) == 0 && len(parsed.Statuses) == 0 && len(parsed.Tags) == 0) || normScope == "goals" {
				oRes, oErr := objectives.ListObjectives(ctx, ListObjectivesInput{
					Workspace: ws,
					Project:   proj,
					Limit:     limit,
				})
				if oErr != nil {
					return oErr
				}
				for _, g := range oRes.Items {
					if parsed.MatchesText(g.Key, g.Title, g.Summary, g.Status, g.Owner) {
						out.Goals = append(out.Goals, g)
					}
				}
			}

			return renderPayload(cmd.OutOrStdout(), formatter, opts, out)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&scopeFlag, "type", "", "Search domain: all|tasks|goals|sdk")
	flags.StringVar(&stateFlag, "state", "", "Filter tasks by state (done, in_progress, todo, review, blocked, backlog)")
	flags.BoolVar(&onlyDone, "done", false, "Show only completed (Done) tasks")
	flags.BoolVar(&onlyInProg, "in-progress", false, "Show only In Progress tasks")
	flags.BoolVar(&onlyTodo, "todo", false, "Show only Todo/Ready tasks")
	flags.BoolVar(&onlyBlock, "blocked", false, "Show only blocked tasks")
	flags.StringSliceVar(&labelFlags, "label", nil, "Require specified task label(s)")
	flags.StringVar(&actorFlag, "actor", "", "Filter by claiming or assigned actor handle")
	flags.StringVar(&objFlag, "objective", "", "Filter tasks belonging to a goal/objective slug")
	flags.IntVar(&limit, "limit", 50, "Maximum items to return")
	return cmd
}

type codeCommandDeps interface {
	LanguageDependency
	LocalPathDependency
	FormatterDependency
}

// newCodeCommand constructs `flowsk code` (alias: `exec`) for evaluating
// Cloudflare Code Mode ES6 JavaScript scripts directly from the terminal.
func newCodeCommand(opts *GlobalOptions, deps codeCommandDeps) *cobra.Command {
	var (
		client     LanguageClient
		localPaths LocalPathRegistry
		formatter  OutputFormatter
	)
	if deps != nil {
		client = deps.LanguageClient()
		localPaths = deps.LocalPathRegistry()
		formatter = deps.OutputFormatter()
	}

	var (
		evalCode string
		codeFile string
		stdin    bool
		dryRun   bool
	)

	cmd := &cobra.Command{
		Use:     "code",
		Aliases: []string{"exec"},
		Short:   "Execute sandboxed ES6 JavaScript against the Flowsk SDK in one ACID transaction (Cloudflare Code Mode)",
		Long: `Evaluates standard ES6 JavaScript inside Flowsk's pure-Go hermetic sandbox (goja)
against the global 'flowsk' SDK. All reads and writes run inside ONE atomic ACID
database transaction (rolling back automatically on error or --dry-run).`,
		Example: `  # Query completed tasks and return custom JSON from JavaScript
  flowsk code -e 'return flowsk.tasks.search("state:done")'

  # Create a goal, two linked tasks, and claim the first in one atomic JS call
  flowsk code -e '
    const g = flowsk.goals.create({ title: "Q4 Search", priority: "high" });
    const t1 = flowsk.tasks.create({ title: "Add goja VM", objective: g.key, priority: "urgent" });
    const t2 = flowsk.tasks.create({ title: "Wire CLI search", objective: g.key, dependsOn: [t1.key] });
    return flowsk.tasks.claim(t1.key, 1800);
  '`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("code context canceled: %w", err)
			}
			if client == nil {
				return ErrNotImplemented
			}
			script, err := readExclusiveTextInput(cmd.InOrStdin(), evalCode, codeFile, stdin, "JavaScript code")
			if err != nil {
				return err
			}
			ws, proj := resolveOptionalProject(ctx, opts, localPaths)
			res, err := client.ExecuteScript(ctx, ExecuteScriptInput{
				Workspace: ws,
				Project:   proj,
				Actor:     strings.TrimSpace(opts.Actor),
				Script:    script,
				DryRun:    dryRun,
				Density:   "standard",
				MaxTokens: opts.MaxTokens,
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), formatter, opts, res)
		},
	}

	flags := cmd.Flags()
	flags.StringVarP(&evalCode, "eval", "e", "", "Inline ES6 JavaScript snippet to execute against `flowsk.*`")
	flags.StringVarP(&codeFile, "file", "f", "", "Local path to a .js script file (read locally; path never sent to server)")
	flags.BoolVar(&stdin, "stdin", false, "Read JavaScript code from standard input")
	flags.BoolVar(&dryRun, "dry-run", false, "Simulate execution in a rolled-back transaction without persisting changes")
	return cmd
}
