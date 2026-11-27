// File project.go implements the `flowsk project` CLI subcommand group for
// logical projects, workflow states, labels, and strictly client-local path bindings.
package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

type projectCommandDeps interface {
	ProjectDependency
	LocalPathDependency
	FormatterDependency
}

type projectHandler struct {
	opts       *GlobalOptions
	client     ProjectClient
	localPaths LocalPathRegistry
	formatter  OutputFormatter
}

// newProjectCommand constructs `flowsk project` (`create`, `list`, `get`,
// `update`, `archive`, `link`, `unlink`, `where`, `state list|create`,
// `label list|create`).
func newProjectCommand(opts *GlobalOptions, deps projectCommandDeps) *cobra.Command {
	var (
		client     ProjectClient
		localPaths LocalPathRegistry
		formatter  OutputFormatter
	)
	if deps != nil {
		client = deps.ProjectClient()
		localPaths = deps.LocalPathRegistry()
		formatter = deps.OutputFormatter()
	}
	h := &projectHandler{
		opts:       opts,
		client:     client,
		localPaths: localPaths,
		formatter:  formatter,
	}

	projCmd := &cobra.Command{
		Use:     "project",
		Aliases: []string{"proj"},
		Short:   "Manage logical projects, custom workflow states, labels, and client-only folder bindings",
		Long: `Manages logical projects, Linear-style workflow state pipelines, labels, and
strictly client-local directory bindings.

Zero Repo & Server Path Footprint:
  Host directory associations created with 'flowsk project link' are stored
  100% locally in ~/.flowsk/local_paths.json on your machine (so Mario's
  ~/Desktop/app and Paolo's ~/Downloads/app map independently to the same server
  project without polluting the Git repo or sending host paths to the server).`,
	}

	projCmd.AddCommand(
		h.newCreateSubcommand(),
		h.newListSubcommand(),
		h.newGetSubcommand(),
		h.newUpdateSubcommand(),
		h.newArchiveSubcommand(),
		h.newDeleteSubcommand(),
		h.newBindSubcommand(),
		h.newUnbindSubcommand(),
		h.newBindingsSubcommand(),
		h.newLinkSubcommand(),
		h.newUnlinkSubcommand(),
		h.newWhereSubcommand(),
		h.newDocSubcommand(),
		h.newStateSubcommand(),
		h.newLabelSubcommand(),
	)

	return projCmd
}

func (h *projectHandler) newCreateSubcommand() *cobra.Command {
	var (
		name            string
		description     string
		defaultWorkflow bool
	)

	cmd := &cobra.Command{
		Use:     "create <key>",
		Short:   "Create a logical project (e.g. CORE, API, WEB) inside the active workspace",
		Example: `  flowsk project create CORE --name "Core Platform" --default-workflow`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("project create context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			key := strings.TrimSpace(args[0])
			if key == "" || strings.Contains(key, string(filepath.Separator)) || strings.Contains(key, "/") {
				return fmt.Errorf("invalid project key %q: must be a logical identifier, never a filesystem path", args[0])
			}
			dispName := strings.TrimSpace(name)
			if dispName == "" {
				dispName = key
			}
			pv, err := h.client.CreateProject(ctx, CreateProjectInput{
				Workspace:       defaultWorkspace(h.opts.Workspace),
				Key:             key,
				Name:            dispName,
				Description:     strings.TrimSpace(description),
				DefaultWorkflow: defaultWorkflow,
				Actor:           strings.TrimSpace(h.opts.Actor),
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, pv)
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Human-readable project name")
	cmd.Flags().StringVar(&description, "description", "", "Short project description")
	cmd.Flags().BoolVar(&defaultWorkflow, "default-workflow", true, "Seed default Linear-style workflow states (Backlog, Todo, In Progress, Review, Blocked, Done, Canceled)")
	return cmd
}

func (h *projectHandler) newListSubcommand() *cobra.Command {
	var (
		includeArchived bool
		limit           int
		cursor          string
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List logical projects in the active workspace",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("project list context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			if limit <= 0 {
				return fmt.Errorf("--limit must be > 0, got %d", limit)
			}
			res, err := h.client.ListProjects(ctx, ListProjectsInput{
				Workspace:       defaultWorkspace(h.opts.Workspace),
				IncludeArchived: includeArchived,
				Limit:           limit,
				Cursor:          strings.TrimSpace(cursor),
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, res)
		},
	}

	cmd.Flags().BoolVar(&includeArchived, "include-archived", false, "Include archived projects")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum projects to return")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Opaque pagination cursor")
	return cmd
}

func (h *projectHandler) newGetSubcommand() *cobra.Command {
	return &cobra.Command{
		Use:   "get [project-id-or-key]",
		Short: "Inspect a logical project (defaults to --project or local cwd binding)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("project get context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			explicit := ""
			if len(args) == 1 {
				explicit = args[0]
			}
			ws, proj, err := resolveActiveProject(ctx, h.opts, h.localPaths, explicit)
			if err != nil {
				return err
			}
			pv, err := h.client.GetProject(ctx, ws, proj)
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, pv)
		},
	}
}

func (h *projectHandler) newUpdateSubcommand() *cobra.Command {
	var (
		name        string
		description string
	)

	cmd := &cobra.Command{
		Use:   "update [project-id-or-key]",
		Short: "Update logical project metadata",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("project update context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			explicit := ""
			if len(args) == 1 {
				explicit = args[0]
			}
			ws, proj, err := resolveActiveProject(ctx, h.opts, h.localPaths, explicit)
			if err != nil {
				return err
			}
			in := UpdateProjectInput{
				Workspace: ws,
				Project:   proj,
				Actor:     strings.TrimSpace(h.opts.Actor),
			}
			changed := false
			if cmd.Flags().Changed("name") {
				v := strings.TrimSpace(name)
				in.Name = &v
				changed = true
			}
			if cmd.Flags().Changed("description") {
				v := strings.TrimSpace(description)
				in.Description = &v
				changed = true
			}
			if !changed {
				return errors.New("at least one of --name or --description must be provided")
			}
			pv, err := h.client.UpdateProject(ctx, in)
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, pv)
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Updated project display name")
	cmd.Flags().StringVar(&description, "description", "", "Updated project description")
	return cmd
}

func (h *projectHandler) newArchiveSubcommand() *cobra.Command {
	var reason string

	cmd := &cobra.Command{
		Use:   "archive [project-id-or-key]",
		Short: "Soft-archive a logical project while preserving historical tasks and audit logs",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("project archive context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			explicit := ""
			if len(args) == 1 {
				explicit = args[0]
			}
			ws, proj, err := resolveActiveProject(ctx, h.opts, h.localPaths, explicit)
			if err != nil {
				return err
			}
			pv, err := h.client.ArchiveProject(ctx, ArchiveProjectInput{
				Workspace: ws,
				Project:   proj,
				Reason:    strings.TrimSpace(reason),
				Actor:     strings.TrimSpace(h.opts.Actor),
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, pv)
		},
	}

	cmd.Flags().StringVar(&reason, "reason", "", "Optional audit note explaining why the project was archived")
	return cmd
}

func (h *projectHandler) newDeleteSubcommand() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <project-id-or-key>",
		Short: "Soft-delete a logical project (detaching any sub-projects non-destructively so children survive)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("project delete context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			res, err := h.client.DeleteProject(ctx, DeleteProjectInput{
				Workspace: defaultWorkspace(h.opts.Workspace),
				Project:   strings.TrimSpace(args[0]),
				Actor:     strings.TrimSpace(h.opts.Actor),
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, res)
		},
	}
}

func (h *projectHandler) newBindSubcommand() *cobra.Command {
	var subpath string

	cmd := &cobra.Command{
		Use:     "bind <parent-project> <child-project>",
		Short:   "Bind a child sub-project beneath a parent macro project in the workspace DAG",
		Example: `  flowsk project bind CORE WEB --subpath apps/web`,
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("project bind context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			res, err := h.client.BindSubproject(ctx, BindSubprojectInput{
				Workspace:     defaultWorkspace(h.opts.Workspace),
				ParentProject: strings.TrimSpace(args[0]),
				ChildProject:  strings.TrimSpace(args[1]),
				ScopeSubpath:  strings.TrimSpace(subpath),
				Actor:         strings.TrimSpace(h.opts.Actor),
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, res)
		},
	}

	cmd.Flags().StringVar(&subpath, "subpath", "", "Optional relative subfolder inside the parent repository (e.g. apps/web)")
	return cmd
}

func (h *projectHandler) newUnbindSubcommand() *cobra.Command {
	return &cobra.Command{
		Use:   "unbind <parent-project> <child-project>",
		Short: "Detach a sub-project binding without deleting either project or its tasks",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("project unbind context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			res, err := h.client.UnbindSubproject(ctx, UnbindSubprojectInput{
				Workspace:     defaultWorkspace(h.opts.Workspace),
				ParentProject: strings.TrimSpace(args[0]),
				ChildProject:  strings.TrimSpace(args[1]),
				Actor:         strings.TrimSpace(h.opts.Actor),
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, res)
		},
	}
}

func (h *projectHandler) newBindingsSubcommand() *cobra.Command {
	var (
		parent string
		child  string
	)

	cmd := &cobra.Command{
		Use:   "bindings",
		Short: "List parent -> sub-project bindings in the active workspace",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("project bindings context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			res, err := h.client.ListProjectBindings(ctx, ListProjectBindingsInput{
				Workspace:     defaultWorkspace(h.opts.Workspace),
				ParentProject: strings.TrimSpace(parent),
				ChildProject:  strings.TrimSpace(child),
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, res)
		},
	}

	cmd.Flags().StringVar(&parent, "parent", "", "Optional parent project filter")
	cmd.Flags().StringVar(&child, "child", "", "Optional child sub-project filter")
	return cmd
}

func (h *projectHandler) newLinkSubcommand() *cobra.Command {
	return &cobra.Command{
		Use:   "link [dir]",
		Short: "Bind a local directory to a logical project in ~/.flowsk/local_paths.json (100% client-local)",
		Long: `Associates [dir] (or the current working directory when omitted) with the logical
project specified via --project (and --workspace).

Strict Local-Only Isolation Rules:
  1. 100% Local Per Machine: The directory mapping is written ONLY to
     ~/.flowsk/local_paths.json on THIS client machine.
  2. Zero Target-Repository Footprint: Flowsk NEVER creates .flowsk, config files,
     or gitignored artifacts inside [dir].
  3. Zero Server Path Leakage: [dir] is NEVER transmitted, logged, or persisted
     on the Flowsk server.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("project link context canceled: %w", err)
			}
			if h.localPaths == nil {
				return ErrNotImplemented
			}
			proj := strings.TrimSpace(h.opts.Project)
			if proj == "" {
				return errors.New("--project is required to link a local directory")
			}
			rawDir := ""
			if len(args) == 1 {
				rawDir = args[0]
			}
			absDir, err := normalizeLocalDir(rawDir)
			if err != nil {
				return err
			}
			binding, err := h.localPaths.Link(ctx, absDir, defaultWorkspace(h.opts.Workspace), proj)
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, binding)
		},
	}
}

func (h *projectHandler) newUnlinkSubcommand() *cobra.Command {
	var allForProject bool

	cmd := &cobra.Command{
		Use:   "unlink [dir]",
		Short: "Remove a client-local directory binding from ~/.flowsk/local_paths.json",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("project unlink context canceled: %w", err)
			}
			if h.localPaths == nil {
				return ErrNotImplemented
			}
			if allForProject {
				proj := strings.TrimSpace(h.opts.Project)
				if proj == "" {
					return errors.New("--project is required when --all is set")
				}
				ws := defaultWorkspace(h.opts.Workspace)
				bindings, err := h.localPaths.ListByProject(ctx, ws, proj)
				if err != nil {
					return err
				}
				removed := make([]string, 0, len(bindings))
				for _, b := range bindings {
					if err := h.localPaths.Unlink(ctx, b.LocalDir); err != nil {
						return err
					}
					removed = append(removed, b.LocalDir)
				}
				return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, UnlinkResultView{
					WorkspaceID: ws,
					ProjectID:   proj,
					RemovedDirs: removed,
					Count:       len(removed),
				})
			}

			rawDir := ""
			if len(args) == 1 {
				rawDir = args[0]
			}
			absDir, err := normalizeLocalDir(rawDir)
			if err != nil {
				return err
			}
			if err := h.localPaths.Unlink(ctx, absDir); err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, UnlinkResultView{
				RemovedDirs: []string{absDir},
				Count:       1,
			})
		},
	}

	cmd.Flags().BoolVar(&allForProject, "all", false, "Remove every local directory binding on this machine mapped to --project")
	return cmd
}

func (h *projectHandler) newWhereSubcommand() *cobra.Command {
	return &cobra.Command{
		Use:   "where [dir]",
		Short: "Show the client-local project binding from ~/.flowsk/local_paths.json",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("project where context canceled: %w", err)
			}
			if h.localPaths == nil {
				return ErrNotImplemented
			}
			if len(args) == 0 && strings.TrimSpace(h.opts.Project) != "" {
				bindings, err := h.localPaths.ListByProject(ctx, defaultWorkspace(h.opts.Workspace), strings.TrimSpace(h.opts.Project))
				if err != nil {
					return err
				}
				return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, bindings)
			}
			rawDir := ""
			if len(args) == 1 {
				rawDir = args[0]
			}
			absDir, err := normalizeLocalDir(rawDir)
			if err != nil {
				return err
			}
			binding, err := h.localPaths.ResolveByDir(ctx, absDir)
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, binding)
		},
	}
}

func (h *projectHandler) newStateSubcommand() *cobra.Command {
	stateCmd := &cobra.Command{
		Use:   "state",
		Short: "List and create custom workflow states for a project",
	}

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List ordered workflow states configured for the active project",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("project state list context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			ws, proj, err := resolveActiveProject(ctx, h.opts, h.localPaths, "")
			if err != nil {
				return err
			}
			states, err := h.client.ListStates(ctx, ws, proj)
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, states)
		},
	}

	var (
		category string
		position int
		color    string
	)
	createCmd := &cobra.Command{
		Use:     "create <name>",
		Short:   "Create a custom workflow state in the project pipeline",
		Example: `  flowsk project state create "QA Verification" --category review --position 40 --color "#0EA5E9"`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("project state create context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			stateName := strings.TrimSpace(args[0])
			if stateName == "" {
				return errors.New("state name must not be empty")
			}
			normCat, err := validateStateCategory(category)
			if err != nil {
				return err
			}
			ws, proj, err := resolveActiveProject(ctx, h.opts, h.localPaths, "")
			if err != nil {
				return err
			}
			st, err := h.client.CreateState(ctx, CreateStateInput{
				Workspace: ws,
				Project:   proj,
				Name:      stateName,
				Category:  normCat,
				Position:  position,
				Color:     strings.TrimSpace(color),
				Actor:     strings.TrimSpace(h.opts.Actor),
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, st)
		},
	}
	createCmd.Flags().StringVar(&category, "category", "in_progress", "Canonical state category: triage|backlog|todo|in_progress|review|blocked|done|canceled")
	createCmd.Flags().IntVar(&position, "position", 0, "Sort order within the project's state pipeline")
	createCmd.Flags().StringVar(&color, "color", "", "Optional hex badge color (e.g. #22C55E)")

	stateCmd.AddCommand(listCmd, createCmd)
	return stateCmd
}

func (h *projectHandler) newLabelSubcommand() *cobra.Command {
	labelCmd := &cobra.Command{
		Use:   "label",
		Short: "List and create classification labels for a project",
	}

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List labels defined in the active project",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("project label list context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			ws, proj, err := resolveActiveProject(ctx, h.opts, h.localPaths, "")
			if err != nil {
				return err
			}
			labels, err := h.client.ListLabels(ctx, ws, proj)
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, labels)
		},
	}

	var (
		color       string
		description string
	)
	createCmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a label in the active project",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("project label create context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			labelName := strings.TrimSpace(args[0])
			if labelName == "" {
				return errors.New("label name must not be empty")
			}
			ws, proj, err := resolveActiveProject(ctx, h.opts, h.localPaths, "")
			if err != nil {
				return err
			}
			lb, err := h.client.CreateLabel(ctx, CreateLabelInput{
				Workspace:   ws,
				Project:     proj,
				Name:        labelName,
				Color:       strings.TrimSpace(color),
				Description: strings.TrimSpace(description),
				Actor:       strings.TrimSpace(h.opts.Actor),
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, lb)
		},
	}
	createCmd.Flags().StringVar(&color, "color", "", "Optional hex color (e.g. #F59E0B)")
	createCmd.Flags().StringVar(&description, "description", "", "Short label usage hint for agents and humans")

	labelCmd.AddCommand(listCmd, createCmd)
	return labelCmd
}

func (h *projectHandler) newDocSubcommand() *cobra.Command {
	var (
		setMarkdown string
		fromStdin   bool
		clearDoc    bool
	)

	cmd := &cobra.Command{
		Use:   "doc [project-id-or-key]",
		Short: "Read, write, or clear the extended Markdown (.md) document for a project in ~/.flowsk/projects/",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("project doc context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			explicit := ""
			if len(args) == 1 {
				explicit = args[0]
			}
			ws, proj, err := resolveActiveProject(ctx, h.opts, h.localPaths, explicit)
			if err != nil {
				return err
			}
			if clearDoc {
				if cmd.Flags().Changed("set") || fromStdin {
					return errors.New("--clear cannot be combined with --set or --stdin")
				}
				res, dErr := h.client.DeleteProjectMarkdown(ctx, ws, proj, strings.TrimSpace(h.opts.Actor))
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
				res, pErr := h.client.PutProjectMarkdown(ctx, ws, proj, strings.TrimSpace(h.opts.Actor), body)
				if pErr != nil {
					return pErr
				}
				return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, res)
			}
			res, err := h.client.GetProjectMarkdown(ctx, ws, proj)
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, res)
		},
	}

	cmd.Flags().StringVarP(&setMarkdown, "set", "m", "", "Write Markdown content to the project's .md document")
	cmd.Flags().BoolVar(&fromStdin, "stdin", false, "Read Markdown content from standard input")
	cmd.Flags().BoolVar(&clearDoc, "clear", false, "Delete the project's .md document")
	return cmd
}

func validateStateCategory(category string) (string, error) {
	norm := strings.ToLower(strings.TrimSpace(category))
	switch norm {
	case "triage", "backlog", "todo", "ready", "in_progress", "review", "in_review", "blocked", "done", "canceled", "cancelled", "archived":
		return norm, nil
	default:
		return "", fmt.Errorf("invalid --category %q: must be triage, backlog, todo, in_progress, review, blocked, done, canceled, or archived", category)
	}
}
