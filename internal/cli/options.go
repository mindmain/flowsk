// File options.go defines global CLI flags, Viper environment bindings, XDG
// user-level configuration/data path helpers, dual-mode execution selection
// (local embedded SQLite vs. remote ConnectRPC network server), and terminal
// vs. agent output format options.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// OutputFormat enumerates the terminal and wire output encodings supported
// across all Flowsk CLI commands.
type OutputFormat string

const (
	// FormatAuto automatically selects FormatHuman when writing to an interactive
	// terminal (TTY) and FormatCompact when piped or invoked by AI agent harnesses.
	FormatAuto OutputFormat = "auto"

	// FormatHuman renders aligned terminal tables, goal/objective progress bars,
	// workflow state columns, and blocker indicators tailored for human operators.
	FormatHuman OutputFormat = "human"

	// FormatCompact is the low-token single-line/symbolic output encoding
	// engineered so AI coding agents consume minimal context window tokens.
	FormatCompact OutputFormat = "compact"

	// FormatTSV emits deterministic tab-separated records ideal for Unix pipes
	// and token-dense list scans.
	FormatTSV OutputFormat = "tsv"

	// FormatJSONL emits newline-delimited JSON objects for streaming parsers.
	FormatJSONL OutputFormat = "jsonl"

	// FormatJSON emits pretty or structured JSON documents.
	FormatJSON OutputFormat = "json"

	// FormatMarkdown emits human- and LLM-readable GitHub-Flavored Markdown.
	FormatMarkdown OutputFormat = "md"

	// FormatMarkdownFull is the canonical alias ("markdown") for FormatMarkdown.
	FormatMarkdownFull OutputFormat = "markdown"
)

// Valid reports whether f is one of the supported Flowsk CLI output encodings.
func (f OutputFormat) Valid() bool {
	switch f {
	case FormatAuto, FormatHuman, FormatCompact, FormatTSV, FormatJSONL, FormatJSON, FormatMarkdown, FormatMarkdownFull:
		return true
	default:
		return false
	}
}

const (
	envPrefix = "FLOWSK"
)

// GlobalOptions holds root persistent CLI options bound to Viper (FLOWSK_*
// environment variables + ~/.config/flowsk/config.yaml + explicit flags).
//
// Dual-Mode Topology (Local-First Embedded + Optional Network Server):
//   - Local Embedded Mode (Default when ServerURL is empty): Commands persist
//     and read workspaces, projects, goals/objectives, tasks, and audit logs
//     directly in the local SQLite WAL file (DBPath, defaulting to
//     ~/.local/share/flowsk/flowsk.db) with zero background daemon required.
//   - Remote Network Mode (When --server-url or FLOWSK_SERVER_URL is set):
//     Commands connect over HTTP/1.1 or HTTP/2 Buf Connect (`ConnectRPC`) to a
//     shared `flowsk serve` instance so multiple developer machines, CI runners,
//     or remote agent servers collaborate on the same Flowsk state over LAN/VPN.
//
// Zero Target-Repository Footprint Invariant:
// Flowsk NEVER searches, reads, or writes configuration files inside the user's
// working directory or target Git repository. Unless --config (or FLOWSK_CONFIG)
// explicitly points elsewhere, configuration is loaded strictly from the
// user-scoped ~/.config/flowsk/config.yaml file.
type GlobalOptions struct {
	// ConfigFile overrides the default ~/.config/flowsk/config.yaml path.
	ConfigFile string

	// DBPath is the local SQLite database file path used in Local Embedded Mode
	// when ServerURL is empty (FLOWSK_DB_PATH, default ~/.local/share/flowsk/flowsk.db).
	DBPath string

	// ServerURL is the optional base HTTP/ConnectRPC URL of a remote or shared
	// Flowsk server (FLOWSK_SERVER_URL, e.g., "http://192.168.1.50:8080"). When
	// empty (default), the CLI runs directly against the local SQLite DBPath.
	ServerURL string

	// AuthToken carries an optional bearer token sent when ServerURL is set
	// (FLOWSK_AUTH_TOKEN). Never logged or printed unredacted.
	AuthToken string

	// Workspace identifies the active multi-tenant workspace UUID or slug
	// (FLOWSK_WORKSPACE). Defaults to "default" in standalone local mode.
	Workspace string

	// Project identifies the active logical project UUID or key (FLOWSK_PROJECT).
	// When omitted on the client, CLI commands may consult the strictly local
	// ~/.config/flowsk/local_paths.json registry for the current working folder,
	// transmitting ONLY the resolved Project ID/Key when talking to a server.
	Project string

	// Actor identifies the human developer or autonomous agent handle performing
	// the operation (FLOWSK_ACTOR, e.g., "mario", "claude-architect", "gemini-dev-1").
	Actor string

	// Format controls the output encoding: auto|human|compact|tsv|jsonl|json|md|markdown
	// (FLOWSK_FORMAT, default "auto": human in interactive TTY, compact in non-TTY/agents).
	Format OutputFormat

	// MaxTokens sets an optional upper token budget on rendered CLI responses so
	// autonomous agent shells never overflow context windows (FLOWSK_MAX_TOKENS).
	MaxTokens int
}

// IsRemoteMode reports whether the CLI is configured to connect over the network
// to a remote/shared `flowsk serve` ConnectRPC server rather than opening the
// local embedded SQLite database directly.
func (o GlobalOptions) IsRemoteMode() bool {
	return strings.TrimSpace(o.ServerURL) != ""
}

// DefaultFlowskHomeDir resolves the self-contained Flowsk home directory
// ($FLOWSK_HOME when set, otherwise ~/.flowsk) storing config.yaml,
// local_paths.json, flowsk.db, and snapshots in one durable folder that
// survives even if the flowsk binary is uninstalled or upgraded.
func DefaultFlowskHomeDir() (string, error) {
	if fh := strings.TrimSpace(os.Getenv("FLOWSK_HOME")); fh != "" {
		return filepath.Clean(fh), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory: %w", err)
	}
	return filepath.Join(home, ".flowsk"), nil
}

// DefaultConfigDir resolves the user-level Flowsk configuration directory
// ($FLOWSK_HOME when set, otherwise ~/.flowsk). Target repositories are never
// inspected or modified.
func DefaultConfigDir() (string, error) {
	return DefaultFlowskHomeDir()
}

// DefaultDataDir resolves the user-level Flowsk local persistence directory
// ($FLOWSK_HOME when set, otherwise ~/.flowsk) used for zero-setup standalone
// SQLite storage.
func DefaultDataDir() (string, error) {
	return DefaultFlowskHomeDir()
}

// DefaultLocalDBFile returns the canonical local SQLite database file path
// (~/.flowsk/flowsk.db) used when running in Local Embedded Mode.
func DefaultLocalDBFile() (string, error) {
	dir, err := DefaultDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "flowsk.db"), nil
}

// DefaultConfigFile returns the canonical user-level client configuration file
// path (~/.flowsk/config.yaml).
func DefaultConfigFile() (string, error) {
	dir, err := DefaultConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yaml"), nil
}

// DefaultLocalPathsFile returns the canonical host-only directory-to-project
// registry path (~/.flowsk/local_paths.json). Entries in this file are
// strictly private to each user's workstation and are never synced to the server.
func DefaultLocalPathsFile() (string, error) {
	dir, err := DefaultConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "local_paths.json"), nil
}

// NewViper constructs an isolated Viper instance configured with the FLOWSK_
// environment prefix and hyphen/dot underscore normalization, avoiding any
// package-level global Viper state.
func NewViper() *viper.Viper {
	v := viper.New()
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_", ".", "_"))
	v.AutomaticEnv()
	return v
}

// BindGlobalFlags attaches persistent global flags to rootCmd and binds each
// flag to the provided Viper instance. Single-letter persistent shorthands that
// would collide with subcommand flags (such as -f on `lang exec`) are
// intentionally avoided.
func BindGlobalFlags(rootCmd *cobra.Command, v *viper.Viper, opts *GlobalOptions) error {
	pflags := rootCmd.PersistentFlags()

	defaultDB, _ := DefaultLocalDBFile()

	pflags.StringVar(&opts.ConfigFile, "config", "", "Path to client config file (default ~/.config/flowsk/config.yaml; never reads target repo)")
	pflags.String("db-path", defaultDB, "Local SQLite database path used when --server-url is empty (env FLOWSK_DB_PATH)")
	pflags.String("server-url", "", "Optional remote Flowsk ConnectRPC server URL; empty uses local SQLite directly (env FLOWSK_SERVER_URL)")
	pflags.String("auth-token", "", "Optional bearer token for remote ConnectRPC servers (env FLOWSK_AUTH_TOKEN)")
	pflags.String("workspace", "default", "Tenant workspace UUID or slug (env FLOWSK_WORKSPACE)")
	pflags.String("project", "", "Logical project UUID or key; falls back to local ~/.config/flowsk/local_paths.json cwd binding (env FLOWSK_PROJECT)")
	pflags.String("actor", "", "Human or AI agent actor handle recorded on claims and audit logs (env FLOWSK_ACTOR)")
	pflags.String("format", string(FormatAuto), "Output format: auto|human|compact|tsv|jsonl|json|md (auto = human on TTY, compact for agents; env FLOWSK_FORMAT)")
	pflags.Int("max-tokens", 0, "Optional maximum token budget for rendered output (0 = unlimited; env FLOWSK_MAX_TOKENS)")

	v.SetDefault("db-path", defaultDB)
	v.SetDefault("server-url", "")
	v.SetDefault("workspace", "default")
	v.SetDefault("format", string(FormatAuto))
	v.SetDefault("max-tokens", 0)

	keys := []string{
		"config",
		"db-path",
		"server-url",
		"auth-token",
		"workspace",
		"project",
		"actor",
		"format",
		"max-tokens",
	}
	for _, key := range keys {
		if err := v.BindPFlag(key, pflags.Lookup(key)); err != nil {
			return fmt.Errorf("bind global flag %q: %w", key, err)
		}
	}
	return nil
}

// LoadGlobalOptions loads user-level YAML configuration (strictly from
// --config / FLOWSK_CONFIG or ~/.config/flowsk/config.yaml, never from the
// target Git repository), merges FLOWSK_* environment variables and CLI flags,
// and validates effective option invariants.
func LoadGlobalOptions(v *viper.Viper, opts *GlobalOptions) error {
	return loadGlobalOptionsInternal(v, opts, false)
}

func loadGlobalOptionsInternal(v *viper.Viper, opts *GlobalOptions, allowMissingExplicit bool) error {
	explicitCfg := strings.TrimSpace(opts.ConfigFile)
	if explicitCfg == "" {
		explicitCfg = strings.TrimSpace(v.GetString("config"))
	}

	if explicitCfg != "" {
		v.SetConfigFile(explicitCfg)
		if err := v.ReadInConfig(); err != nil {
			if !allowMissingExplicit || !os.IsNotExist(err) {
				return fmt.Errorf("read config file %q: %w", explicitCfg, err)
			}
		}
		opts.ConfigFile = explicitCfg
	} else if defaultDir, err := DefaultConfigDir(); err == nil {
		v.AddConfigPath(defaultDir)
		v.SetConfigName("config")
		v.SetConfigType("yaml")
		if readErr := v.ReadInConfig(); readErr != nil {
			var notFound viper.ConfigFileNotFoundError
			if !errors.As(readErr, &notFound) && !os.IsNotExist(readErr) {
				return fmt.Errorf("read default config in %q: %w", defaultDir, readErr)
			}
		}
	}

	opts.DBPath = strings.TrimSpace(v.GetString("db-path"))
	opts.ServerURL = strings.TrimSpace(v.GetString("server-url"))
	opts.AuthToken = strings.TrimSpace(v.GetString("auth-token"))
	opts.Workspace = strings.TrimSpace(v.GetString("workspace"))
	opts.Project = strings.TrimSpace(v.GetString("project"))
	opts.Actor = strings.TrimSpace(v.GetString("actor"))
	opts.Format = OutputFormat(strings.ToLower(strings.TrimSpace(v.GetString("format"))))
	opts.MaxTokens = v.GetInt("max-tokens")

	if !opts.Format.Valid() {
		return fmt.Errorf("invalid --format %q: must be one of auto, human, compact, tsv, jsonl, json, md, markdown", opts.Format)
	}
	if opts.MaxTokens < 0 {
		return fmt.Errorf("invalid --max-tokens %d: must be >= 0", opts.MaxTokens)
	}
	return nil
}

type globalOptionsContextKey struct{}

// WithGlobalOptions attaches a value copy of parsed GlobalOptions to ctx so
// wired runtime Dependencies can inspect per-invocation flags (--db-path,
// --server-url, --auth-token, --workspace, --project, --actor, --config)
// without package-level mutable state.
func WithGlobalOptions(ctx context.Context, opts GlobalOptions) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, globalOptionsContextKey{}, opts)
}

// GlobalOptionsFromContext extracts the per-invocation GlobalOptions attached
// by PersistentPreRunE, returning (GlobalOptions{}, false) when absent.
func GlobalOptionsFromContext(ctx context.Context) (GlobalOptions, bool) {
	if ctx == nil {
		return GlobalOptions{}, false
	}
	opts, ok := ctx.Value(globalOptionsContextKey{}).(GlobalOptions)
	return opts, ok
}
