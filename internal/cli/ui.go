// File ui.go implements `flowsk ui` (alias: `flowsk web`), starting the embedded
// Angular + Tailwind + SignalStore Web UI alongside ConnectRPC endpoints and
// optionally opening the operator's default browser.
package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type uiCommandDeps interface {
	ServerDependency
	LocalPathDependency
}

type uiHandler struct {
	v          *viper.Viper
	opts       *GlobalOptions
	runner     ServerRunner
	localPaths LocalPathRegistry
}

// newUICommand constructs `flowsk ui` (alias: `web`) with immediate constructor
// dependency resolution.
func newUICommand(v *viper.Viper, opts *GlobalOptions, deps uiCommandDeps) *cobra.Command {
	var (
		runner     ServerRunner
		localPaths LocalPathRegistry
	)
	if deps != nil {
		runner = deps.ServerRunner()
		localPaths = deps.LocalPathRegistry()
	}

	h := &uiHandler{
		v:          v,
		opts:       opts,
		runner:     runner,
		localPaths: localPaths,
	}

	var (
		listenAddr  string
		dbDriver    string
		dbDSN       string
		volumeDir   string
		autoMigrate bool
		noOpen      bool
	)

	cmd := &cobra.Command{
		Use:     "ui",
		Aliases: []string{"web"},
		Short:   "Launch the embedded Flowsk Web UI panel (Kanban, Grid, Projects, Goals, Archive, Orphan Prune)",
		Long: `Starts the embedded minimalist Flowsk Web UI (Angular + Tailwind CSS + SignalStore)
and serves all ConnectRPC APIs from a single static Go binary.

Capabilities:
  - Drag-and-drop Kanban board & compact Grid view across Backlog, Todo, In Progress,
    In Review, Blocked, Done, Canceled, and Archived states.
  - Project & Objective/Goal management, instant GitHub/Linear search, and per-task /
    per-project Markdown documents stored inside ~/.flowsk/{projects,tasks}/.
  - One-click Archive Purge (permanent DB + .md deletion) and Orphan Resource Prune.
  - Automatically focuses the project bound to your current directory (if any) via
    ~/.flowsk/local_paths.json without exposing host paths to the server.`,
		Example: `  # Launch the Flowsk Web UI on http://127.0.0.1:8080 and open the browser
  flowsk ui

  # Alias 'flowsk web' on a custom port without opening a browser tab
  flowsk web --listen 127.0.0.1:9090 --no-open`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("ui context canceled: %w", err)
			}
			if h.runner == nil {
				return ErrNotImplemented
			}
			driver, err := validateDBDriver(dbDriver)
			if err != nil {
				return err
			}
			addr := strings.TrimSpace(listenAddr)
			if addr == "" {
				return errors.New("--listen must not be empty")
			}
			dsn := strings.TrimSpace(dbDSN)
			if !cmd.Flags().Changed("db-dsn") && driver == "sqlite" && strings.TrimSpace(h.opts.DBPath) != "" {
				dsn = strings.TrimSpace(h.opts.DBPath)
			}
			if dsn == "" && !h.opts.IsRemoteMode() {
				return errors.New("--db-dsn must not be empty")
			}

			ws, proj := resolveOptionalProject(ctx, h.opts, h.localPaths)
			out := cmd.OutOrStdout()

			effective := ServeConfig{
				ListenAddr:  addr,
				DBDriver:    driver,
				DBDSN:       dsn,
				VolumeDir:   strings.TrimSpace(volumeDir),
				AutoMigrate: autoMigrate,
				EnableUI:    true,
				UIWorkspace: ws,
				UIProject:   proj,
				UIActor:     strings.TrimSpace(h.opts.Actor),
				OnListen: func(boundAddr string) {
					uiURL := formatBrowserURL(boundAddr)
					_, _ = fmt.Fprintf(out, "Flowsk Web UI listening on %s (workspace=%s", uiURL, defaultWorkspace(ws))
					if proj != "" {
						_, _ = fmt.Fprintf(out, ", project=%s", proj)
					}
					_, _ = fmt.Fprintln(out, ") — press Ctrl+C to stop.")
					if !noOpen {
						openSystemBrowser(ctx, uiURL)
					}
				},
			}
			return h.runner.RunServer(ctx, effective)
		},
	}

	defaultDSN, err := DefaultLocalDBFile()
	if err != nil || strings.TrimSpace(defaultDSN) == "" {
		defaultDSN = "/data/flowsk/flowsk.db"
	}
	defaultVol, err := DefaultDataDir()
	if err != nil || strings.TrimSpace(defaultVol) == "" {
		defaultVol = "/data/flowsk"
	}

	flags := cmd.Flags()
	flags.StringVar(&listenAddr, "listen", "127.0.0.1:8080", "TCP address for the Flowsk Web UI & ConnectRPC server")
	flags.StringVar(&dbDriver, "db-driver", "sqlite", "Persistence engine: sqlite|postgres")
	flags.StringVar(&dbDSN, "db-dsn", defaultDSN, "SQLite file path or PostgreSQL connection URI")
	flags.StringVar(&volumeDir, "volume-dir", defaultVol, "Shared volume directory for advisory locks and snapshots")
	flags.BoolVar(&autoMigrate, "auto-migrate", true, "Run pending Goose migrations automatically on startup")
	flags.BoolVar(&noOpen, "no-open", false, "Do not automatically open the Web UI URL in the default browser")

	return cmd
}

func formatBrowserURL(boundAddr string) string {
	host, port, err := net.SplitHostPort(strings.TrimSpace(boundAddr))
	if err != nil {
		return "http://" + strings.TrimSpace(boundAddr)
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func openSystemBrowser(ctx context.Context, targetURL string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", targetURL)
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", targetURL)
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", targetURL)
	}
	_ = cmd.Start()
}
