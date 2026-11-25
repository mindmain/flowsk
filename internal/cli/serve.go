// File serve.go implements `flowsk serve`, starting the ConnectRPC HTTP/1.1 &
// HTTP/2 API server so local or remote network peers can share Flowsk state.
package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type serveHandler struct {
	v      *viper.Viper
	opts   *GlobalOptions
	runner ServerRunner
}

// newServeCommand constructs `flowsk serve` by resolving ServerRunner
// immediately in the constructor and binding server runtime flags to Viper.
func newServeCommand(v *viper.Viper, opts *GlobalOptions, deps ServerDependency) *cobra.Command {
	var runner ServerRunner
	if deps != nil {
		runner = deps.ServerRunner()
	}
	h := &serveHandler{
		v:      v,
		opts:   opts,
		runner: runner,
	}

	var cfg ServeConfig

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the Flowsk ConnectRPC HTTP/1.1 & HTTP/2 API server",
		Long: `Starts the unified Flowsk API daemon exposing ConnectRPC (HTTP/1.1, HTTP/2,
and gRPC-compatible) endpoints backed by pure-Go SQLite (WAL + shared volume
coordination) or PostgreSQL.

Zero Host Path Exposure:
  The server stores strictly logical workspace, project, objective, task, and
  audit records. Client repository folder paths are never accepted or persisted.`,
		Example: `  # Start local SQLite server with automatic Goose schema migrations
  flowsk serve --listen 127.0.0.1:8080 --db-driver sqlite --db-dsn /data/flowsk/flowsk.db

  # Start shared office server backed by PostgreSQL
  flowsk serve --listen 0.0.0.0:8080 --db-driver postgres --db-dsn "postgres://flowsk:secret@db:5432/flowsk?sslmode=disable"`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("serve context canceled: %w", err)
			}
			for _, key := range []string{"listen", "db-driver", "db-dsn", "volume-dir", "auto-migrate", "ui"} {
				_ = h.v.BindPFlag(key, cmd.Flags().Lookup(key))
			}
			if h.runner == nil {
				return ErrNotImplemented
			}
			driver, err := validateDBDriver(h.v.GetString("db-driver"))
			if err != nil {
				return err
			}
			listenAddr := strings.TrimSpace(h.v.GetString("listen"))
			if listenAddr == "" {
				return errors.New("--listen must not be empty")
			}
			dsn := strings.TrimSpace(h.v.GetString("db-dsn"))
			if !cmd.Flags().Changed("db-dsn") && driver == "sqlite" && strings.TrimSpace(h.opts.DBPath) != "" && h.v.GetString("db_dsn") == "" {
				dsn = strings.TrimSpace(h.opts.DBPath)
			}
			if dsn == "" {
				return errors.New("--db-dsn must not be empty")
			}
			effective := ServeConfig{
				ListenAddr:  listenAddr,
				DBDriver:    driver,
				DBDSN:       dsn,
				VolumeDir:   strings.TrimSpace(h.v.GetString("volume-dir")),
				AutoMigrate: h.v.GetBool("auto-migrate"),
				EnableUI:    h.v.GetBool("ui"),
				UIWorkspace: strings.TrimSpace(h.opts.Workspace),
				UIProject:   strings.TrimSpace(h.opts.Project),
				UIActor:     strings.TrimSpace(h.opts.Actor),
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
	flags.StringVar(&cfg.ListenAddr, "listen", "127.0.0.1:8080", "TCP address for the ConnectRPC HTTP server (env FLOWSK_LISTEN)")
	flags.StringVar(&cfg.DBDriver, "db-driver", "sqlite", "Persistence engine: sqlite|postgres (env FLOWSK_DB_DRIVER)")
	flags.StringVar(&cfg.DBDSN, "db-dsn", defaultDSN, "SQLite file path or PostgreSQL connection URI (env FLOWSK_DB_DSN)")
	flags.StringVar(&cfg.VolumeDir, "volume-dir", defaultVol, "Shared volume directory for advisory locks and SQLite snapshots (env FLOWSK_VOLUME_DIR)")
	flags.BoolVar(&cfg.AutoMigrate, "auto-migrate", true, "Run pending Goose migrations automatically on startup (env FLOWSK_AUTO_MIGRATE)")
	flags.BoolVar(&cfg.EnableUI, "ui", false, "Also serve the embedded Flowsk Web UI SPA on the same HTTP listener (env FLOWSK_UI)")

	for _, key := range []string{"listen", "db-driver", "db-dsn", "volume-dir", "auto-migrate", "ui"} {
		_ = v.BindPFlag(key, flags.Lookup(key))
	}

	return cmd
}
