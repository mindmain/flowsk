// File root.go assembles the top-level `flowsk` Cobra command tree, wires Viper
// persistent flags, and mounts all local/remote subcommands (`init`, `install`,
// `doctor`, `update`, `board`/`status`, `ui`/`web`, `prune`, `search`, `code`,
// `serve`, `mcp`, `workspace`, `project`, `objective`/`goal`, `task`, `audit`,
// `db`, and `config`).
package cli

import (
	"fmt"

	"github.com/mindmain/flowsk/internal/version"
	"github.com/spf13/cobra"
)

// NewRootCommand wires an isolated Viper configuration store, binds global
// persistent flags, resolves segregated subcommand dependencies immediately at
// construction time, and returns the unified `flowsk` Cobra command tree.
func NewRootCommand(deps Dependencies) *cobra.Command {
	if deps == nil {
		deps = DefaultDependencies()
	}

	v := NewViper()
	opts := &GlobalOptions{}

	rootCmd := &cobra.Command{
		Use:     "flowsk",
		Version: version.NormalizedTag(),
		Short:   "Local-first & network-capable project, goal, and multi-agent task engine (CLI, Web UI, ConnectRPC, MCP)",
		Long: `Flowsk is a Linear-inspired project, strategic goal/objective, and hierarchical
task engine built as a single static binary for developers and autonomous AI agents
(Claude, Gemini, ChatGPT, Codex, Antigravity).

Core Capabilities:
  - Local-First + Optional Network Sharing: Works out-of-the-box against a local
    SQLite WAL database (~/.flowsk/flowsk.db) with zero server setup, serves an
    embedded Angular Web UI ('flowsk ui' / 'flowsk web'), and seamlessly connects
    multiple machines over the network when pointed at 'flowsk serve' via
    --server-url / FLOWSK_SERVER_URL.
  - First-Class Terminal Visibility & Cloudflare Code Mode: Renders aligned human
    tables, goal progress bars, and Kanban boards in interactive terminals
    (--format human) while emitting ultra-compact output (--format compact,
    --max-tokens) and running sandboxed ES6 JavaScript ('flowsk code' / MCP
    'flowsk_execute') inside one atomic ACID transaction for AI agents.
  - Zero Repository & Server Path Pollution: Flowsk NEVER creates metadata files
    inside your Git repository and NEVER transmits host filesystem paths to any
    server. Per-machine directory bindings live exclusively inside
    ~/.flowsk/local_paths.json.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if err := cmd.Context().Err(); err != nil {
				return fmt.Errorf("context canceled: %w", err)
			}
			allowMissing := isConfigBootstrapCommand(cmd)
			if err := loadGlobalOptionsInternal(v, opts, allowMissing); err != nil {
				return fmt.Errorf("load configuration: %w", err)
			}
			cmd.SetContext(WithGlobalOptions(cmd.Context(), *opts))
			return nil
		},
	}

	if err := BindGlobalFlags(rootCmd, v, opts); err != nil {
		panic(fmt.Sprintf("flowsk cli flag initialization failed: %v", err))
	}

	rootCmd.AddCommand(
		newInitCommand(opts, deps),
		newInstallCommand(opts, deps),
		newDoctorCommand(v, opts, deps),
		newUpdateCommand(opts, deps),
		newBoardCommand(opts, deps),
		newUICommand(v, opts, deps),
		newPruneCommand(opts, deps),
		newSearchCommand(opts, deps),
		newCodeCommand(opts, deps),
		newServeCommand(v, opts, deps),
		newMCPCommand(v, opts, deps),
		newWorkspaceCommand(opts, deps),
		newProjectCommand(opts, deps),
		newObjectiveCommand(opts, deps),
		newTaskCommand(opts, deps),
		newAuditCommand(opts, deps),
		newDBCommand(v, opts, deps),
		newConfigCommand(v, opts, deps),
	)

	return rootCmd
}

func isConfigBootstrapCommand(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	parent := cmd.Parent()
	if parent == nil {
		return false
	}
	if parent.Parent() == nil {
		switch cmd.Name() {
		case "install", "doctor", "update":
			return true
		}
	}
	if parent.Name() == "config" && (cmd.Name() == "init" || cmd.Name() == "paths") {
		return true
	}
	if parent.Name() == "workspace" && cmd.Name() == "use" {
		return true
	}
	return false
}
