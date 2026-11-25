// Package config defines strongly typed runtime configuration models and Viper-backed
// loading contracts for both the Flowsk server/daemon (`ServerConfig`) and the
// developer/agent workstation CLI (`ClientConfig`).
//
// Architectural Separation Invariants:
//   - Server vs. Client Separation: `ServerConfig` governs ConnectRPC HTTP listeners,
//     dual-dialect database engines (`sqlite` vs. `postgres`), shared Docker volume
//     coordination (`/data/flowsk`), and MCP server transports. `ClientConfig` governs
//     server endpoint connectivity, actor credentials, default token-compact output
//     formatting, and the strictly client-local path registry file
//     (`~/.config/flowsk/local_paths.json`).
//   - Secret Hygiene: Credential fields (`Postgres.DSN`, `ClientConfig.AuthToken`)
//     MUST never be emitted in diagnostic `String()` or startup log dumps.
package config

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/viper"

	"github.com/mindmain/flowsk/internal/adapter/db"
	"github.com/mindmain/flowsk/internal/adapter/db/postgres"
	"github.com/mindmain/flowsk/internal/adapter/db/sqlite"
	"github.com/mindmain/flowsk/internal/adapter/formatter"
	"github.com/mindmain/flowsk/internal/domain/errs"
)

// DefaultEnvPrefix is the canonical environment variable prefix (`FLOWSK_*`)
// used when `LoadOptions.EnvPrefix` is empty.
const DefaultEnvPrefix = "FLOWSK"

// RedactedMask is the placeholder string substituted for sensitive credentials
// in diagnostic output and Redacted configuration copies.
const RedactedMask = "REDACTED"

var kvPasswordPattern = regexp.MustCompile(`(?i)(password\s*=\s*)('[^']*'|"[^"]*"|\S+)`)

// HTTPServerConfig configures the ConnectRPC / HTTP/2 listener for `flowsk serve`.
type HTTPServerConfig struct {
	// ListenAddr is the TCP `host:port` bind address (for example, `127.0.0.1:8080` or `0.0.0.0:8080`).
	ListenAddr string

	// ReadHeaderTimeout bounds HTTP header read duration to mitigate Slowloris attacks.
	ReadHeaderTimeout time.Duration

	// ReadTimeout bounds total request body read time.
	ReadTimeout time.Duration

	// WriteTimeout bounds unary RPC response write duration.
	WriteTimeout time.Duration

	// ShutdownGracePeriod bounds graceful drain duration upon SIGINT/SIGTERM.
	ShutdownGracePeriod time.Duration
}

// DatabaseConfig selects the active SQL dialect (`sqlite` or `postgres`) and carries
// dialect-specific pool and durability parameters.
type DatabaseConfig struct {
	// Dialect selects `db.DialectSQLite` (default for local/shared-volume setups)
	// or `db.DialectPostgres` (for networked multi-replica server deployments).
	Dialect db.Dialect

	// AutoMigrate runs pending Goose SQL migrations automatically on server startup when true.
	AutoMigrate bool

	// SQLite holds `modernc.org/sqlite` WAL and file configuration when Dialect == DialectSQLite.
	SQLite sqlite.Config

	// Postgres holds `jackc/pgx/v5` pool configuration when Dialect == DialectPostgres.
	Postgres postgres.Config
}

// VolumeConfig configures the shared local or Docker volume directory layout and
// periodic online snapshot retention.
type VolumeConfig struct {
	// Enabled activates shared volume directory management and advisory file locking.
	Enabled bool

	// RootDir is the mounted volume path (for example, `/data/flowsk`).
	RootDir string

	// SnapshotRetentionCount specifies how many recent point-in-time snapshots to retain.
	SnapshotRetentionCount int

	// AutoSnapshotInterval schedules periodic background snapshots when > 0.
	AutoSnapshotInterval time.Duration
}

// MCPTransportMode identifies how the embedded Model Context Protocol server exposes tools.
type MCPTransportMode string

const (
	// MCPTransportStdio runs MCP over standard input/output streams for local agent harnesses.
	MCPTransportStdio MCPTransportMode = "stdio"

	// MCPTransportStreamableHTTP exposes MCP over HTTP alongside or standalone from ConnectRPC.
	MCPTransportStreamableHTTP MCPTransportMode = "http"
)

// Valid reports whether m is a supported MCPTransportMode value.
func (m MCPTransportMode) Valid() bool {
	return m == MCPTransportStdio || m == MCPTransportStreamableHTTP
}

// MCPConfig configures the Model Context Protocol surface exposing Flowsk
// search, digests, and the Cloudflare Code Mode JavaScript executor.
type MCPConfig struct {
	// Enabled activates the MCP server surface.
	Enabled bool

	// Transport selects `stdio` or `http`.
	Transport MCPTransportMode

	// HTTPPath sets the HTTP mount path when Transport == MCPTransportStreamableHTTP (for example, `/mcp`).
	HTTPPath string

	// DefaultMaxOutputTokens bounds tool response token budgets to protect agent context windows.
	DefaultMaxOutputTokens int

	// MaxBatchStatements bounds the maximum number of batch operations accepted per call.
	MaxBatchStatements int
}

// ServerConfig aggregates all validated runtime settings for running a Flowsk
// server or embedded local daemon.
type ServerConfig struct {
	// Environment identifies deployment tier (`development`, `staging`, `production`).
	Environment string

	// LogLevel sets structured logger verbosity (`debug`, `info`, `warn`, `error`).
	LogLevel string

	// HTTP configures the ConnectRPC network listener.
	HTTP HTTPServerConfig

	// Database configures SQLite or PostgreSQL persistence.
	Database DatabaseConfig

	// Volume configures shared local/Docker volume coordination and snapshots.
	Volume VolumeConfig

	// MCP configures Model Context Protocol tool exposure for AI agents.
	MCP MCPConfig
}

// RedactDSN masks embedded passwords inside PostgreSQL URI or keyword/value DSN strings.
func RedactDSN(rawDSN string) string {
	trimmed := strings.TrimSpace(rawDSN)
	if trimmed == "" {
		return ""
	}

	if strings.HasPrefix(strings.ToLower(trimmed), "postgres://") ||
		strings.HasPrefix(strings.ToLower(trimmed), "postgresql://") {
		parsed, err := url.Parse(trimmed)
		if err != nil {
			return RedactedMask
		}
		if parsed.User != nil {
			username := parsed.User.Username()
			if _, hasPassword := parsed.User.Password(); hasPassword {
				parsed.User = url.UserPassword(username, RedactedMask)
			}
		}
		if parsed.RawQuery != "" {
			q := parsed.Query()
			for k := range q {
				if strings.EqualFold(k, "password") {
					q.Set(k, RedactedMask)
				}
			}
			parsed.RawQuery = q.Encode()
		}
		return parsed.String()
	}

	if kvPasswordPattern.MatchString(trimmed) {
		return kvPasswordPattern.ReplaceAllString(trimmed, "${1}"+RedactedMask)
	}
	if strings.Contains(trimmed, "=") {
		return trimmed
	}
	return RedactedMask
}

// Redacted returns a copy of ServerConfig with database credentials masked for safe logging.
func (c ServerConfig) Redacted() ServerConfig {
	out := c
	out.Database.Postgres.DSN = RedactDSN(c.Database.Postgres.DSN)
	return out
}

// String renders a diagnostic representation of ServerConfig with all secrets masked.
func (c ServerConfig) String() string {
	r := c.Redacted()
	return fmt.Sprintf(
		"ServerConfig{Environment:%q, LogLevel:%q, ListenAddr:%q, Dialect:%q, SQLitePath:%q, PostgresDSN:%q, VolumeEnabled:%t, MCPTransport:%q}",
		r.Environment,
		r.LogLevel,
		r.HTTP.ListenAddr,
		r.Database.Dialect,
		r.Database.SQLite.FilePath,
		r.Database.Postgres.DSN,
		r.Volume.Enabled,
		r.MCP.Transport,
	)
}

// ClientConfig aggregates all validated workstation/agent CLI settings, supporting
// both zero-setup Local Embedded SQLite mode (when ServerURL is empty) and Remote
// ConnectRPC Network mode (when ServerURL points to a shared `flowsk serve` node),
// along with the strictly client-local path registry file location and terminal
// vs. token-frugal output defaults.
type ClientConfig struct {
	// LocalSQLitePath is the local SQLite WAL database path (for example,
	// `~/.local/share/flowsk/flowsk.db`) opened directly in-process when ServerURL
	// is empty so standalone local CLI/MCP usage requires zero background daemons.
	LocalSQLitePath string

	// ServerURL is the optional ConnectRPC base endpoint of a shared Flowsk server
	// over localhost, LAN, VPN, or internet (for example, `http://192.168.1.50:8080`).
	// When empty, the CLI operates directly in Local Embedded SQLite mode.
	ServerURL string

	// DefaultWorkspaceSlug sets the fallback workspace slug when not inferred from a local path binding.
	DefaultWorkspaceSlug string

	// ActorHandle identifies the local human user or autonomous agent handle.
	ActorHandle string

	// AuthToken carries the bearer authentication credential resolved from env/keychain/config.
	AuthToken string

	// LocalPathRegistryFile is the client-local OS user config path (for example,
	// `~/.config/flowsk/local_paths.json`) used by `internal/adapter/localpath` to map
	// local checkout directories to server Project IDs without touching git repositories
	// or sending local filesystem paths to the server.
	LocalPathRegistryFile string

	// DefaultFormat sets the default CLI/agent output encoding (`human`, `compact`, `tsv`, `jsonl`, `json`, `markdown`).
	DefaultFormat formatter.Format

	// DefaultMaxTokens bounds rendered CLI output token budgets when > 0.
	DefaultMaxTokens int

	// RequestTimeout bounds individual CLI -> Server RPC roundtrips when ServerURL is set.
	RequestTimeout time.Duration
}

// IsRemoteMode reports whether ClientConfig points to a remote ConnectRPC server
// rather than running in Local Embedded SQLite mode.
func (c ClientConfig) IsRemoteMode() bool {
	return strings.TrimSpace(c.ServerURL) != ""
}

// Redacted returns a copy of ClientConfig with AuthToken masked for safe diagnostics.
func (c ClientConfig) Redacted() ClientConfig {
	out := c
	if strings.TrimSpace(out.AuthToken) != "" {
		out.AuthToken = RedactedMask
	}
	return out
}

// String renders a diagnostic representation of ClientConfig with AuthToken masked.
func (c ClientConfig) String() string {
	r := c.Redacted()
	return fmt.Sprintf(
		"ClientConfig{LocalSQLitePath:%q, ServerURL:%q, DefaultWorkspaceSlug:%q, ActorHandle:%q, AuthToken:%q, LocalPathRegistryFile:%q, DefaultFormat:%q, DefaultMaxTokens:%d, RequestTimeout:%s}",
		r.LocalSQLitePath,
		r.ServerURL,
		r.DefaultWorkspaceSlug,
		r.ActorHandle,
		r.AuthToken,
		r.LocalPathRegistryFile,
		r.DefaultFormat,
		r.DefaultMaxTokens,
		r.RequestTimeout,
	)
}

// LoadOptions customizes Viper configuration discovery across explicit config file
// overrides, environment variable prefixes (`FLOWSK_*`), and CLI flags.
type LoadOptions struct {
	// ConfigFilePath overrides automatic config file search when non-empty.
	ConfigFilePath string

	// EnvPrefix overrides the default `FLOWSK` environment variable prefix when non-empty.
	EnvPrefix string
}

// Loader resolves, merges (defaults -> config file -> env vars -> flags), and
// validates `ServerConfig` and `ClientConfig` structs via `github.com/spf13/viper`.
type Loader interface {
	// LoadServerConfig loads and validates server/daemon configuration.
	LoadServerConfig(ctx context.Context, opts LoadOptions) (ServerConfig, error)

	// LoadClientConfig loads and validates workstation/agent CLI configuration,
	// resolving `os.UserConfigDir()` for `LocalPathRegistryFile` when unset.
	LoadClientConfig(ctx context.Context, opts LoadOptions) (ClientConfig, error)
}

// ViperLoader implements Loader using isolated per-call Viper instances so
// concurrent callers never contend on package-level global state.
type ViperLoader struct{}

// ViperLoaderSkeleton is retained as a type alias of ViperLoader for 100%
// backward compatibility with existing callers referencing the initial type name.
type ViperLoaderSkeleton = ViperLoader

// Compile-time check ensuring *ViperLoader satisfies Loader.
var _ Loader = (*ViperLoader)(nil)

// NewLoader constructs a Viper-backed configuration Loader.
func NewLoader() *ViperLoaderSkeleton {
	return &ViperLoader{}
}

// LoadServerConfig loads, merges (defaults -> YAML -> FLOWSK_* env), and validates ServerConfig.
func (l *ViperLoader) LoadServerConfig(ctx context.Context, opts LoadOptions) (ServerConfig, error) {
	const op = "config.Loader.LoadServerConfig"
	if err := ctx.Err(); err != nil {
		return ServerConfig{}, fmt.Errorf("%s: %w", op, err)
	}

	v, prefix, err := newConfiguredViper(opts)
	if err != nil {
		return ServerConfig{}, errs.Wrap(errs.ErrValidation, op, "failed to read server configuration file", err)
	}

	defaultSQLiteFile, err := resolveDefaultSQLitePath()
	if err != nil {
		return ServerConfig{}, errs.Wrap(errs.ErrValidation, op, "failed to resolve default local sqlite path", err)
	}

	cfg := ServerConfig{
		Environment: stringFromViper(v, prefix, "development", "environment"),
		LogLevel:    strings.ToLower(stringFromViper(v, prefix, "info", "log_level")),
		HTTP: HTTPServerConfig{
			ListenAddr:          stringFromViper(v, prefix, "127.0.0.1:8080", "http.listen_addr", "listen_addr"),
			ReadHeaderTimeout:   durationFromViper(v, prefix, 5*time.Second, "http.read_header_timeout"),
			ReadTimeout:         durationFromViper(v, prefix, 30*time.Second, "http.read_timeout"),
			WriteTimeout:        durationFromViper(v, prefix, 30*time.Second, "http.write_timeout"),
			ShutdownGracePeriod: durationFromViper(v, prefix, 15*time.Second, "http.shutdown_grace_period"),
		},
		Database: DatabaseConfig{
			Dialect:     db.Dialect(strings.ToLower(stringFromViper(v, prefix, string(db.DialectSQLite), "database.dialect", "db_dialect"))),
			AutoMigrate: boolFromViper(v, prefix, true, "database.auto_migrate", "auto_migrate"),
			SQLite: sqlite.Config{
				FilePath:               stringFromViper(v, prefix, defaultSQLiteFile, "database.sqlite.file_path", "sqlite_path", "db_path"),
				EnableWAL:              boolFromViper(v, prefix, true, "database.sqlite.enable_wal"),
				ForeignKeys:            boolFromViper(v, prefix, true, "database.sqlite.foreign_keys"),
				BusyTimeout:            durationFromViper(v, prefix, 5*time.Second, "database.sqlite.busy_timeout"),
				Synchronous:            sqlite.SynchronousMode(strings.ToUpper(stringFromViper(v, prefix, string(sqlite.SynchronousNormal), "database.sqlite.synchronous"))),
				WALAutoCheckpointPages: intFromViper(v, prefix, 1000, "database.sqlite.wal_auto_checkpoint_pages"),
				MaxReadOpenConns:       intFromViper(v, prefix, 4, "database.sqlite.max_read_open_conns"),
			},
			Postgres: postgres.Config{
				DSN:               stringFromViper(v, prefix, "", "database.postgres.dsn", "postgres_dsn"),
				MaxConns:          int32(intFromViper(v, prefix, 16, "database.postgres.max_conns")),
				MinConns:          int32(intFromViper(v, prefix, 2, "database.postgres.min_conns")),
				MaxConnLifetime:   durationFromViper(v, prefix, 30*time.Minute, "database.postgres.max_conn_lifetime"),
				MaxConnIdleTime:   durationFromViper(v, prefix, 5*time.Minute, "database.postgres.max_conn_idle_time"),
				HealthCheckPeriod: durationFromViper(v, prefix, time.Minute, "database.postgres.health_check_period"),
				ConnectTimeout:    durationFromViper(v, prefix, 5*time.Second, "database.postgres.connect_timeout"),
				StatementTimeout:  durationFromViper(v, prefix, 30*time.Second, "database.postgres.statement_timeout"),
			},
		},
		Volume: VolumeConfig{
			Enabled:                boolFromViper(v, prefix, false, "volume.enabled"),
			RootDir:                stringFromViper(v, prefix, "/data/flowsk", "volume.root_dir"),
			SnapshotRetentionCount: intFromViper(v, prefix, 5, "volume.snapshot_retention_count"),
			AutoSnapshotInterval:   durationFromViper(v, prefix, 0, "volume.auto_snapshot_interval"),
		},
		MCP: MCPConfig{
			Enabled:                boolFromViper(v, prefix, true, "mcp.enabled"),
			Transport:              MCPTransportMode(strings.ToLower(stringFromViper(v, prefix, string(MCPTransportStdio), "mcp.transport"))),
			HTTPPath:               stringFromViper(v, prefix, "/mcp", "mcp.http_path"),
			DefaultMaxOutputTokens: intFromViper(v, prefix, 2000, "mcp.default_max_output_tokens"),
			MaxBatchStatements:     intFromViper(v, prefix, 64, "mcp.max_batch_statements"),
		},
	}

	if err := validateServerConfig(op, cfg); err != nil {
		return ServerConfig{}, err
	}
	return cfg, nil
}

// LoadClientConfig loads, merges (defaults -> YAML -> FLOWSK_* env), and validates ClientConfig.
func (l *ViperLoader) LoadClientConfig(ctx context.Context, opts LoadOptions) (ClientConfig, error) {
	const op = "config.Loader.LoadClientConfig"
	if err := ctx.Err(); err != nil {
		return ClientConfig{}, fmt.Errorf("%s: %w", op, err)
	}

	v, prefix, err := newConfiguredViper(opts)
	if err != nil {
		return ClientConfig{}, errs.Wrap(errs.ErrValidation, op, "failed to read client configuration file", err)
	}

	defaultSQLiteFile, err := resolveDefaultSQLitePath()
	if err != nil {
		return ClientConfig{}, errs.Wrap(errs.ErrValidation, op, "failed to resolve default local sqlite path", err)
	}
	defaultRegistryFile, err := resolveDefaultRegistryPath()
	if err != nil {
		return ClientConfig{}, errs.Wrap(errs.ErrValidation, op, "failed to resolve default local_paths.json path", err)
	}

	rawFormat := strings.ToLower(stringFromViper(v, prefix, string(formatter.FormatCompact), "default_format", "format"))
	if rawFormat == "" || rawFormat == "auto" {
		rawFormat = string(formatter.FormatCompact)
	}

	rawServerURL := stringFromViper(v, prefix, "", "server_url", "client.server_url")
	normalizedServerURL, urlViolation := normalizeOptionalServerURL(rawServerURL)

	cfg := ClientConfig{
		LocalSQLitePath:       stringFromViper(v, prefix, defaultSQLiteFile, "local_sqlite_path", "db_path", "client.local_sqlite_path"),
		ServerURL:             normalizedServerURL,
		DefaultWorkspaceSlug:  stringFromViper(v, prefix, "default", "default_workspace_slug", "workspace"),
		ActorHandle:           stringFromViper(v, prefix, "", "actor_handle", "actor"),
		AuthToken:             stringFromViper(v, prefix, "", "auth_token"),
		LocalPathRegistryFile: stringFromViper(v, prefix, defaultRegistryFile, "local_path_registry_file"),
		DefaultFormat:         formatter.Format(rawFormat),
		DefaultMaxTokens:      intFromViper(v, prefix, 0, "default_max_tokens", "max_tokens"),
		RequestTimeout:        durationFromViper(v, prefix, 30*time.Second, "request_timeout"),
	}

	var violations []errs.FieldViolation
	if urlViolation != nil {
		violations = append(violations, *urlViolation)
	}
	if !cfg.IsRemoteMode() && cfg.LocalSQLitePath == "" {
		violations = append(violations, errs.FieldViolation{
			Field:       "local_sqlite_path",
			Description: "local_sqlite_path is required when running in local embedded mode",
		})
	}
	if cfg.DefaultWorkspaceSlug == "" {
		violations = append(violations, errs.FieldViolation{
			Field:       "default_workspace_slug",
			Description: "default_workspace_slug must not be empty",
		})
	}
	if cfg.LocalPathRegistryFile == "" {
		violations = append(violations, errs.FieldViolation{
			Field:       "local_path_registry_file",
			Description: "local_path_registry_file must not be empty",
		})
	}
	if !cfg.DefaultFormat.Valid() {
		violations = append(violations, errs.FieldViolation{
			Field:       "default_format",
			Description: fmt.Sprintf("unsupported output format %q", cfg.DefaultFormat),
		})
	}
	if cfg.DefaultMaxTokens < 0 {
		violations = append(violations, errs.FieldViolation{
			Field:       "default_max_tokens",
			Description: "default_max_tokens must be >= 0",
		})
	}
	if cfg.RequestTimeout <= 0 {
		violations = append(violations, errs.FieldViolation{
			Field:       "request_timeout",
			Description: "request_timeout must be > 0",
		})
	}
	if len(violations) > 0 {
		return ClientConfig{}, errs.Validation(op, "invalid client configuration", violations...)
	}
	return cfg, nil
}

func validateServerConfig(op string, cfg ServerConfig) error {
	var violations []errs.FieldViolation

	if strings.TrimSpace(cfg.Environment) == "" {
		violations = append(violations, errs.FieldViolation{
			Field:       "environment",
			Description: "environment must not be empty",
		})
	}
	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		violations = append(violations, errs.FieldViolation{
			Field:       "log_level",
			Description: fmt.Sprintf("log_level %q must be debug, info, warn, or error", cfg.LogLevel),
		})
	}
	if strings.TrimSpace(cfg.HTTP.ListenAddr) == "" {
		violations = append(violations, errs.FieldViolation{
			Field:       "http.listen_addr",
			Description: "http.listen_addr must not be empty",
		})
	}
	if !cfg.Database.Dialect.Valid() {
		violations = append(violations, errs.FieldViolation{
			Field:       "database.dialect",
			Description: fmt.Sprintf("database.dialect %q must be sqlite or postgres", cfg.Database.Dialect),
		})
	} else {
		switch cfg.Database.Dialect {
		case db.DialectSQLite:
			if strings.TrimSpace(cfg.Database.SQLite.FilePath) == "" {
				violations = append(violations, errs.FieldViolation{
					Field:       "database.sqlite.file_path",
					Description: "database.sqlite.file_path must not be empty when dialect is sqlite",
				})
			}
			if cfg.Database.SQLite.Synchronous != sqlite.SynchronousNormal &&
				cfg.Database.SQLite.Synchronous != sqlite.SynchronousFull {
				violations = append(violations, errs.FieldViolation{
					Field:       "database.sqlite.synchronous",
					Description: "database.sqlite.synchronous must be NORMAL or FULL",
				})
			}
		case db.DialectPostgres:
			if strings.TrimSpace(cfg.Database.Postgres.DSN) == "" {
				violations = append(violations, errs.FieldViolation{
					Field:       "database.postgres.dsn",
					Description: "database.postgres.dsn must not be empty when dialect is postgres",
				})
			}
			if cfg.Database.Postgres.MaxConns < cfg.Database.Postgres.MinConns {
				violations = append(violations, errs.FieldViolation{
					Field:       "database.postgres.max_conns",
					Description: "database.postgres.max_conns must be >= min_conns",
				})
			}
		}
	}
	if cfg.Volume.Enabled {
		if strings.TrimSpace(cfg.Volume.RootDir) == "" {
			violations = append(violations, errs.FieldViolation{
				Field:       "volume.root_dir",
				Description: "volume.root_dir must not be empty when volume coordination is enabled",
			})
		}
		if cfg.Volume.SnapshotRetentionCount < 0 {
			violations = append(violations, errs.FieldViolation{
				Field:       "volume.snapshot_retention_count",
				Description: "volume.snapshot_retention_count must be >= 0",
			})
		}
	}
	if !cfg.MCP.Transport.Valid() {
		violations = append(violations, errs.FieldViolation{
			Field:       "mcp.transport",
			Description: fmt.Sprintf("mcp.transport %q must be stdio or http", cfg.MCP.Transport),
		})
	}
	if cfg.MCP.DefaultMaxOutputTokens < 0 {
		violations = append(violations, errs.FieldViolation{
			Field:       "mcp.default_max_output_tokens",
			Description: "mcp.default_max_output_tokens must be >= 0",
		})
	}
	if cfg.MCP.MaxBatchStatements <= 0 {
		violations = append(violations, errs.FieldViolation{
			Field:       "mcp.max_batch_statements",
			Description: "mcp.max_batch_statements must be > 0",
		})
	}

	if len(violations) > 0 {
		return errs.Validation(op, "invalid server configuration", violations...)
	}
	return nil
}

func normalizeOptionalServerURL(raw string) (string, *errs.FieldViolation) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", &errs.FieldViolation{
			Field:       "server_url",
			Description: "server_url must be a valid http:// or https:// endpoint with a non-empty host",
		}
	}
	return strings.TrimRight(trimmed, "/"), nil
}

func newConfiguredViper(opts LoadOptions) (*viper.Viper, string, error) {
	prefix := strings.TrimSpace(opts.EnvPrefix)
	if prefix == "" {
		prefix = DefaultEnvPrefix
	}
	prefix = strings.ToUpper(prefix)

	v := viper.New()
	v.SetEnvPrefix(prefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
	v.AutomaticEnv()

	cfgPath := strings.TrimSpace(opts.ConfigFilePath)
	if cfgPath == "" {
		cfgPath = strings.TrimSpace(os.Getenv(prefix + "_CONFIG"))
	}

	if cfgPath != "" {
		v.SetConfigFile(cfgPath)
		if err := v.ReadInConfig(); err != nil {
			return nil, prefix, err
		}
		return v, prefix, nil
	}

	if cfgDir, err := resolveUserConfigDir(); err == nil && cfgDir != "" {
		v.AddConfigPath(cfgDir)
		v.SetConfigName("config")
		v.SetConfigType("yaml")
		if readErr := v.ReadInConfig(); readErr != nil {
			var notFound viper.ConfigFileNotFoundError
			if !errors.As(readErr, &notFound) && !os.IsNotExist(readErr) {
				return nil, prefix, readErr
			}
		}
	}
	return v, prefix, nil
}

func envVarName(prefix, key string) string {
	normalized := strings.ToUpper(strings.NewReplacer(".", "_", "-", "_").Replace(key))
	return prefix + "_" + normalized
}

func stringFromViper(v *viper.Viper, prefix, fallback string, keys ...string) string {
	// 1. Non-empty environment variables take highest precedence across all candidate keys.
	for _, key := range keys {
		if val, ok := os.LookupEnv(envVarName(prefix, key)); ok && strings.TrimSpace(val) != "" {
			return strings.TrimSpace(val)
		}
	}
	// 2. Configuration file values take second precedence.
	for _, key := range keys {
		if v.InConfig(key) {
			cfgVal := strings.TrimSpace(v.GetString(key))
			if cfgVal != "" || fallback == "" {
				return cfgVal
			}
		}
	}
	return fallback
}

func intFromViper(v *viper.Viper, prefix string, fallback int, keys ...string) int {
	for _, key := range keys {
		if val, ok := os.LookupEnv(envVarName(prefix, key)); ok && strings.TrimSpace(val) != "" {
			return v.GetInt(key)
		}
	}
	for _, key := range keys {
		if v.InConfig(key) {
			return v.GetInt(key)
		}
	}
	return fallback
}

func boolFromViper(v *viper.Viper, prefix string, fallback bool, keys ...string) bool {
	for _, key := range keys {
		if val, ok := os.LookupEnv(envVarName(prefix, key)); ok && strings.TrimSpace(val) != "" {
			return v.GetBool(key)
		}
	}
	for _, key := range keys {
		if v.InConfig(key) {
			return v.GetBool(key)
		}
	}
	return fallback
}

func durationFromViper(v *viper.Viper, prefix string, fallback time.Duration, keys ...string) time.Duration {
	for _, key := range keys {
		if val, ok := os.LookupEnv(envVarName(prefix, key)); ok && strings.TrimSpace(val) != "" {
			return v.GetDuration(key)
		}
	}
	for _, key := range keys {
		if v.InConfig(key) {
			return v.GetDuration(key)
		}
	}
	return fallback
}

func resolveFlowskHomeDir() (string, error) {
	if fh := strings.TrimSpace(os.Getenv("FLOWSK_HOME")); fh != "" {
		return filepath.Clean(fh), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".flowsk"), nil
}

func resolveUserConfigDir() (string, error) {
	return resolveFlowskHomeDir()
}

func resolveDefaultRegistryPath() (string, error) {
	dir, err := resolveFlowskHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "local_paths.json"), nil
}

func resolveDefaultSQLitePath() (string, error) {
	dir, err := resolveFlowskHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "flowsk.db"), nil
}
