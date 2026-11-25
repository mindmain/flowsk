// File config.go implements the `flowsk config` CLI subcommands (`init`, `show`,
// `paths`) managing user-scoped `~/.flowsk/` client settings and memory paths.
package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// newConfigCommand constructs `flowsk config` (`init`, `show`, `paths`) for
// managing strictly user-level client configuration under ~/.config/flowsk/.
func newConfigCommand(v *viper.Viper, opts *GlobalOptions, deps FormatterDependency) *cobra.Command {
	var formatter OutputFormatter
	if deps != nil {
		formatter = deps.OutputFormatter()
	}

	cfgCmd := &cobra.Command{
		Use:   "config",
		Short: "Initialize and inspect user-level ~/.flowsk configuration, database, and local path store",
		Long: `Manages self-contained Flowsk settings and storage in ~/.flowsk/config.yaml,
~/.flowsk/local_paths.json, and ~/.flowsk/flowsk.db (or $FLOWSK_HOME).

Zero Repository Footprint & Durable Home Memory:
  Flowsk never creates or reads config files inside target Git repositories, and
  all tasks, goals, and configuration remain preserved in ~/.flowsk even if the
  flowsk binary is uninstalled.`,
	}

	var force bool
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Create default ~/.flowsk/config.yaml and local_paths.json on this machine",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cmd.Context().Err(); err != nil {
				return fmt.Errorf("config init context canceled: %w", err)
			}
			_, cfgFile, lpFile, dbFile, err := resolveClientConfigPaths(opts)
			if err != nil {
				return err
			}

			overwritten := false
			if _, statErr := os.Stat(cfgFile); statErr == nil {
				if !force {
					return fmt.Errorf("config file %q already exists (use --force to overwrite)", cfgFile)
				}
				overwritten = true
			} else if !os.IsNotExist(statErr) {
				return fmt.Errorf("inspect config file %q: %w", cfgFile, statErr)
			}

			defaultYAML := fmt.Sprintf(`# Flowsk user-level client configuration
# Target Git repositories are never modified by Flowsk.
workspace: %q
db-path: %q
server-url: ""
actor: %q
format: "auto"
max-tokens: 0
`, defaultWorkspace(opts.Workspace), dbFile, strings.TrimSpace(opts.Actor))

			if err := writeSecureFile(cfgFile, []byte(defaultYAML)); err != nil {
				return err
			}
			if _, statErr := os.Stat(lpFile); os.IsNotExist(statErr) {
				if err := writeSecureFile(lpFile, []byte("{\n  \"bindings\": []\n}\n")); err != nil {
					return err
				}
			}

			return renderPayload(cmd.OutOrStdout(), formatter, opts, ConfigInitView{
				ConfigFile:     cfgFile,
				LocalPathsFile: lpFile,
				Overwritten:    overwritten,
			})
		},
	}
	initCmd.Flags().BoolVar(&force, "force", false, "Overwrite existing ~/.config/flowsk/config.yaml if present")

	var redact bool
	showCmd := &cobra.Command{
		Use:   "show",
		Short: "Display effective merged configuration (flags + FLOWSK_* env + ~/.config/flowsk/config.yaml)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cmd.Context().Err(); err != nil {
				return fmt.Errorf("config show context canceled: %w", err)
			}
			_, cfgFile, lpFile, dbFile, err := resolveClientConfigPaths(opts)
			if err != nil {
				return err
			}
			if used := strings.TrimSpace(v.ConfigFileUsed()); used != "" {
				cfgFile = used
			}
			mode := "local"
			if opts.IsRemoteMode() {
				mode = "remote"
			}
			token := opts.AuthToken
			if token != "" && redact {
				token = "***REDACTED***"
			}
			return renderPayload(cmd.OutOrStdout(), formatter, opts, ConfigShowView{
				Mode:           mode,
				ConfigFile:     cfgFile,
				LocalPathsFile: lpFile,
				DBPath:         dbFile,
				ServerURL:      opts.ServerURL,
				AuthToken:      token,
				Workspace:      defaultWorkspace(opts.Workspace),
				Project:        opts.Project,
				Actor:          opts.Actor,
				Format:         string(opts.Format),
				MaxTokens:      opts.MaxTokens,
			})
		},
	}
	showCmd.Flags().BoolVar(&redact, "redact", true, "Redact credentials and tokens in displayed configuration")

	pathsCmd := &cobra.Command{
		Use:   "paths",
		Short: "Print resolved user-level paths for config.yaml and local_paths.json",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cmd.Context().Err(); err != nil {
				return fmt.Errorf("config paths context canceled: %w", err)
			}
			cfgDir, cfgFile, lpFile, dbFile, err := resolveClientConfigPaths(opts)
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), formatter, opts, ConfigPathsView{
				ConfigDir:      cfgDir,
				ConfigFile:     cfgFile,
				LocalPathsFile: lpFile,
				LocalDBFile:    dbFile,
			})
		},
	}

	cfgCmd.AddCommand(initCmd, showCmd, pathsCmd)
	return cfgCmd
}
