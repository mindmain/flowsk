// File db.go implements the `flowsk db` CLI subcommand group (`migrate`, `ping`)
// for reversible Goose SQL migrations and connectivity checks on SQLite/PostgreSQL.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type dbCommandDeps interface {
	DatabaseDependency
	FormatterDependency
}

type dbHandler struct {
	v         *viper.Viper
	dbCmd     *cobra.Command
	opts      *GlobalOptions
	admin     DatabaseAdmin
	formatter OutputFormatter
}

// newDBCommand constructs `flowsk db` (`migrate up`, `migrate down`,
// `migrate status`, `ping`) for reversible Goose schema migrations and health
// verification across SQLite and PostgreSQL.
func newDBCommand(v *viper.Viper, opts *GlobalOptions, deps dbCommandDeps) *cobra.Command {
	var (
		admin     DatabaseAdmin
		formatter OutputFormatter
	)
	if deps != nil {
		admin = deps.DatabaseAdmin()
		formatter = deps.OutputFormatter()
	}
	dbCmd := &cobra.Command{
		Use:   "db",
		Short: "Run reversible Goose database migrations and health checks (SQLite / PostgreSQL)",
		Long: `Administers Flowsk persistence backends supporting seamless switching between
local/shared-volume pure-Go SQLite and shared office PostgreSQL.`,
	}

	h := &dbHandler{
		v:         v,
		dbCmd:     dbCmd,
		opts:      opts,
		admin:     admin,
		formatter: formatter,
	}

	defaultDSN, err := DefaultLocalDBFile()
	if err != nil || strings.TrimSpace(defaultDSN) == "" {
		defaultDSN = "/data/flowsk/flowsk.db"
	}
	dbCmd.PersistentFlags().String("db-driver", "sqlite", "Database engine: sqlite|postgres (env FLOWSK_DB_DRIVER)")
	dbCmd.PersistentFlags().String("db-dsn", defaultDSN, "SQLite file path or PostgreSQL URI (env FLOWSK_DB_DSN)")

	dbCmd.AddCommand(
		h.newMigrateSubcommand(),
		h.newPingSubcommand(),
	)

	return dbCmd
}

func (h *dbHandler) resolveDriverAndDSN() (string, string, error) {
	_ = h.v.BindPFlag("db-driver", h.dbCmd.PersistentFlags().Lookup("db-driver"))
	_ = h.v.BindPFlag("db-dsn", h.dbCmd.PersistentFlags().Lookup("db-dsn"))
	driver, err := validateDBDriver(h.v.GetString("db-driver"))
	if err != nil {
		return "", "", err
	}
	dsn := strings.TrimSpace(h.v.GetString("db-dsn"))
	if !h.dbCmd.PersistentFlags().Changed("db-dsn") && strings.TrimSpace(os.Getenv("FLOWSK_DB_DSN")) == "" && !h.v.InConfig("db-dsn") && !h.v.InConfig("db_dsn") {
		switch driver {
		case "postgres":
			if pgDSN := strings.TrimSpace(os.Getenv("FLOWSK_POSTGRES_DSN")); pgDSN != "" {
				dsn = pgDSN
			}
		case "sqlite":
			if sqlitePath := strings.TrimSpace(os.Getenv("FLOWSK_SQLITE_PATH")); sqlitePath != "" {
				dsn = sqlitePath
			} else if strings.TrimSpace(h.opts.DBPath) != "" {
				dsn = strings.TrimSpace(h.opts.DBPath)
			}
		}
	}
	if dsn == "" {
		return "", "", errors.New("--db-dsn must not be empty")
	}
	return driver, dsn, nil
}

func (h *dbHandler) newMigrateSubcommand() *cobra.Command {
	migrateCmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply, roll back, or inspect Goose SQL migrations",
	}

	var targetVersion int64
	upCmd := &cobra.Command{
		Use:   "up",
		Short: "Apply pending forward Goose schema migrations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("db migrate up context canceled: %w", err)
			}
			if h.admin == nil {
				return ErrNotImplemented
			}
			if targetVersion < 0 {
				return fmt.Errorf("--target-version must be >= 0, got %d", targetVersion)
			}
			driver, dsn, err := h.resolveDriverAndDSN()
			if err != nil {
				return err
			}
			res, err := h.admin.MigrateUp(ctx, MigrateUpInput{
				Driver:        driver,
				DSN:           dsn,
				TargetVersion: targetVersion,
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, res)
		},
	}
	upCmd.Flags().Int64Var(&targetVersion, "target-version", 0, "Optional target migration version timestamp (0 = latest)")

	var steps int
	downCmd := &cobra.Command{
		Use:   "down",
		Short: "Roll back applied Goose schema migrations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("db migrate down context canceled: %w", err)
			}
			if h.admin == nil {
				return ErrNotImplemented
			}
			if steps <= 0 {
				return fmt.Errorf("--steps must be >= 1, got %d", steps)
			}
			driver, dsn, err := h.resolveDriverAndDSN()
			if err != nil {
				return err
			}
			res, err := h.admin.MigrateDown(ctx, MigrateDownInput{
				Driver: driver,
				DSN:    dsn,
				Steps:  steps,
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, res)
		},
	}
	downCmd.Flags().IntVar(&steps, "steps", 1, "Number of migrations to roll back")

	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Display applied and pending Goose migration versions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("db migrate status context canceled: %w", err)
			}
			if h.admin == nil {
				return ErrNotImplemented
			}
			driver, dsn, err := h.resolveDriverAndDSN()
			if err != nil {
				return err
			}
			entries, err := h.admin.MigrateStatus(ctx, driver, dsn)
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, entries)
		},
	}

	migrateCmd.AddCommand(upCmd, downCmd, statusCmd)
	return migrateCmd
}

func (h *dbHandler) newPingSubcommand() *cobra.Command {
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "ping",
		Short: "Verify SQLite WAL or PostgreSQL database connectivity and schema readiness",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("db ping context canceled: %w", err)
			}
			if h.admin == nil {
				return ErrNotImplemented
			}
			if timeout <= 0 {
				return fmt.Errorf("--timeout must be > 0, got %s", timeout)
			}
			driver, dsn, err := h.resolveDriverAndDSN()
			if err != nil {
				return err
			}
			pingCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			start := time.Now()
			if err := h.admin.PingDB(pingCtx, driver, dsn); err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, DBPingView{
				Status:    "ok",
				Driver:    driver,
				LatencyMS: time.Since(start).Milliseconds(),
			})
		},
	}

	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Second, "Maximum connectivity probe timeout")
	return cmd
}
