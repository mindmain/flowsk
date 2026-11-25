// File workspace.go implements the `flowsk workspace` (alias: `ws`) CLI command
// group for managing multi-tenant workspaces and human/agent actor identities.
package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

type workspaceCommandDeps interface {
	WorkspaceDependency
	FormatterDependency
}

type workspaceHandler struct {
	opts      *GlobalOptions
	client    WorkspaceClient
	formatter OutputFormatter
}

// newWorkspaceCommand constructs `flowsk workspace` (`create`, `list`, `get`,
// `use`, `actor register`, `actor list`) with immediate dependency resolution.
func newWorkspaceCommand(opts *GlobalOptions, deps workspaceCommandDeps) *cobra.Command {
	var (
		client    WorkspaceClient
		formatter OutputFormatter
	)
	if deps != nil {
		client = deps.WorkspaceClient()
		formatter = deps.OutputFormatter()
	}
	h := &workspaceHandler{
		opts:      opts,
		client:    client,
		formatter: formatter,
	}

	wsCmd := &cobra.Command{
		Use:     "workspace",
		Aliases: []string{"ws"},
		Short:   "Manage multi-tenant workspaces and human/AI agent actor identities",
		Long: `Manages isolated multi-tenant Flowsk workspaces and registers human or AI agent
actors whose handles are recorded on task claims, handoff comments, and
immutable audit events.`,
	}

	wsCmd.AddCommand(
		h.newCreateSubcommand(),
		h.newListSubcommand(),
		h.newGetSubcommand(),
		h.newUseSubcommand(),
		h.newActorSubcommand(),
	)

	return wsCmd
}

func (h *workspaceHandler) newCreateSubcommand() *cobra.Command {
	var (
		name        string
		description string
	)

	cmd := &cobra.Command{
		Use:   "create <slug>",
		Short: "Create a new multi-tenant workspace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("workspace create context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			slug := strings.TrimSpace(args[0])
			if slug == "" {
				return errors.New("workspace slug must not be empty")
			}
			dispName := strings.TrimSpace(name)
			if dispName == "" {
				dispName = slug
			}
			ws, err := h.client.CreateWorkspace(ctx, CreateWorkspaceInput{
				Slug:        slug,
				Name:        dispName,
				Description: strings.TrimSpace(description),
				Actor:       strings.TrimSpace(h.opts.Actor),
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, ws)
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Human-readable workspace display name")
	cmd.Flags().StringVar(&description, "description", "", "Short workspace purpose summary")
	return cmd
}

func (h *workspaceHandler) newListSubcommand() *cobra.Command {
	var (
		limit  int
		cursor string
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List accessible workspaces in compact low-token form",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("workspace list context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			if limit <= 0 {
				return fmt.Errorf("--limit must be > 0, got %d", limit)
			}
			res, err := h.client.ListWorkspaces(ctx, ListWorkspacesInput{
				Limit:  limit,
				Cursor: strings.TrimSpace(cursor),
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, res)
		},
	}

	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum workspaces to return")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Opaque pagination cursor")
	return cmd
}

func (h *workspaceHandler) newGetSubcommand() *cobra.Command {
	return &cobra.Command{
		Use:   "get [workspace-id-or-slug]",
		Short: "Inspect a workspace by UUID or slug (defaults to active --workspace)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("workspace get context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			ref := defaultWorkspace(h.opts.Workspace)
			if len(args) == 1 && strings.TrimSpace(args[0]) != "" {
				ref = strings.TrimSpace(args[0])
			}
			ws, err := h.client.GetWorkspace(ctx, ref)
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, ws)
		},
	}
}

func (h *workspaceHandler) newUseSubcommand() *cobra.Command {
	return &cobra.Command{
		Use:   "use <workspace-id-or-slug>",
		Short: "Persist the default workspace in ~/.config/flowsk/config.yaml",
		Long: `Updates the user-level ~/.config/flowsk/config.yaml default workspace setting.
Never writes files inside the current working directory or target Git repository.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cmd.Context().Err(); err != nil {
				return fmt.Errorf("workspace use context canceled: %w", err)
			}
			slug := strings.TrimSpace(args[0])
			if slug == "" {
				return errors.New("workspace identifier must not be empty")
			}
			cfgFile, err := persistWorkspaceInConfig(h.opts, slug)
			if err != nil {
				return err
			}
			h.opts.Workspace = slug
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, WorkspaceUseView{
				Workspace:  slug,
				ConfigFile: cfgFile,
			})
		},
	}
}

func (h *workspaceHandler) newActorSubcommand() *cobra.Command {
	actorCmd := &cobra.Command{
		Use:   "actor",
		Short: "Register and list human, AI agent, and CI actors within a workspace",
	}

	var (
		displayName string
		kind        string
		modelName   string
		role        string
	)
	registerCmd := &cobra.Command{
		Use:   "register <handle>",
		Short: "Register a human developer, AI agent, or CI runner identity",
		Example: `  flowsk workspace actor register mario --kind human --display-name "Mario Rossi" --role lead
  flowsk workspace actor register claude-arch --kind agent --model claude-sonnet --role architect
  flowsk workspace actor register gemini-dev-1 --kind agent --model gemini-pro --role developer`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("workspace actor register context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			handle := strings.TrimSpace(args[0])
			if handle == "" {
				return errors.New("actor handle must not be empty")
			}
			normKind, err := validateActorKind(kind)
			if err != nil {
				return err
			}
			act, err := h.client.RegisterActor(ctx, RegisterActorInput{
				Workspace:   defaultWorkspace(h.opts.Workspace),
				Handle:      handle,
				DisplayName: strings.TrimSpace(displayName),
				Kind:        normKind,
				Model:       strings.TrimSpace(modelName),
				Role:        strings.TrimSpace(role),
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, act)
		},
	}
	registerCmd.Flags().StringVar(&displayName, "display-name", "", "Actor display name")
	registerCmd.Flags().StringVar(&kind, "kind", "agent", "Actor category: human|agent|ci")
	registerCmd.Flags().StringVar(&modelName, "model", "", "Underlying LLM model identifier when --kind=agent")
	registerCmd.Flags().StringVar(&role, "role", "", "Operational role hint (e.g. architect, developer, reviewer)")

	var (
		filterKind string
		limit      int
		cursor     string
	)
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List registered actors in the active workspace",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("workspace actor list context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			if limit <= 0 {
				return fmt.Errorf("--limit must be > 0, got %d", limit)
			}
			normKind := ""
			if strings.TrimSpace(filterKind) != "" {
				var err error
				normKind, err = validateActorKind(filterKind)
				if err != nil {
					return err
				}
			}
			res, err := h.client.ListActors(ctx, ListActorsInput{
				Workspace: defaultWorkspace(h.opts.Workspace),
				Kind:      normKind,
				Limit:     limit,
				Cursor:    strings.TrimSpace(cursor),
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, res)
		},
	}
	listCmd.Flags().StringVar(&filterKind, "kind", "", "Optional actor category filter: human|agent|ci")
	listCmd.Flags().IntVar(&limit, "limit", 50, "Maximum actors to return")
	listCmd.Flags().StringVar(&cursor, "cursor", "", "Opaque pagination cursor")

	actorCmd.AddCommand(registerCmd, listCmd)
	return actorCmd
}

func validateActorKind(kind string) (string, error) {
	norm := strings.ToLower(strings.TrimSpace(kind))
	switch norm {
	case "human", "agent", "ci":
		return norm, nil
	default:
		return "", fmt.Errorf("invalid actor --kind %q: must be human, agent, or ci", kind)
	}
}
