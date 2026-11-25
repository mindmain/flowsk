// File prune.go implements `flowsk prune`, scanning and reconciling orphan
// Markdown `.md` files and broken database `has_markdown_doc` references inside
// the active workspace's `~/.flowsk/{projects,tasks}` storage.
package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

type pruneCommandDeps interface {
	TaskDependency
	FormatterDependency
}

type pruneHandler struct {
	opts      *GlobalOptions
	tasks     TaskClient
	formatter OutputFormatter
}

// newPruneCommand constructs `flowsk prune` with immediate constructor
// dependency resolution.
func newPruneCommand(opts *GlobalOptions, deps pruneCommandDeps) *cobra.Command {
	var (
		tasks     TaskClient
		formatter OutputFormatter
	)
	if deps != nil {
		tasks = deps.TaskClient()
		formatter = deps.OutputFormatter()
	}

	h := &pruneHandler{
		opts:      opts,
		tasks:     tasks,
		formatter: formatter,
	}

	var (
		dryRun    bool
		checkOnly bool
	)

	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Clean up orphan task/project Markdown (.md) files and broken database doc flags in ~/.flowsk",
		Long: `Inspects and reconciles Markdown (.md) resources stored under ~/.flowsk/projects/<ws>/
and ~/.flowsk/tasks/<ws/> against active database records in the current workspace:
  1. Removes orphan .md files on disk whose task or project no longer exists in the DB.
  2. Clears stale has_markdown_doc=true flags on active tasks/projects whose .md file is missing.`,
		Example: `  # Preview orphan .md files and broken references without mutating state
  flowsk prune --dry-run

  # Reconcile all orphan .md files and broken DB references
  flowsk prune`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("prune context canceled: %w", err)
			}
			if h.tasks == nil {
				return ErrNotImplemented
			}
			ws := defaultWorkspace(h.opts.Workspace)
			if checkOnly {
				rep, err := h.tasks.InspectOrphans(ctx, ws)
				if err != nil {
					return err
				}
				return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, rep)
			}
			res, err := h.tasks.PruneOrphans(ctx, ws, strings.TrimSpace(h.opts.Actor), dryRun)
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, res)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report orphan .md files and broken DB flags without deleting or modifying anything")
	cmd.Flags().BoolVar(&checkOnly, "check", false, "Inspect orphan status only without pruning")
	return cmd
}
