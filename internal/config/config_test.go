// Package config_test verifies Viper-backed ServerConfig and ClientConfig loading,
// precedence rules (defaults -> YAML file -> FLOWSK_* environment variables),
// local-embedded vs. remote-server dual-mode defaults, validation errors, and
// credential redaction hygiene.
package config_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mindmain/flowsk/internal/adapter/db"
	"github.com/mindmain/flowsk/internal/adapter/formatter"
	"github.com/mindmain/flowsk/internal/config"
	"github.com/mindmain/flowsk/internal/domain/errs"
)

// TestLoadClientConfig_DefaultLocalEmbeddedMode verifies that ClientConfig defaults
// to zero-daemon Local Embedded SQLite mode with ServerURL empty and user-scoped
// SQLite and local_paths.json paths populated.
func TestLoadClientConfig_DefaultLocalEmbeddedMode(t *testing.T) {
	tmpHome := t.TempDir()

	t.Setenv("HOME", tmpHome)
	t.Setenv("FLOWSK_HOME", "")
	t.Setenv("FLOWSK_SERVER_URL", "")
	t.Setenv("FLOWSK_DB_PATH", "")
	t.Setenv("FLOWSK_LOCAL_SQLITE_PATH", "")

	loader := config.NewLoader()
	cfg, err := loader.LoadClientConfig(context.Background(), config.LoadOptions{})
	if err != nil {
		t.Fatalf("LoadClientConfig returned unexpected error: %v", err)
	}

	if cfg.ServerURL != "" {
		t.Fatalf("expected empty ServerURL in default local embedded mode, got %q", cfg.ServerURL)
	}
	wantDB := filepath.Join(tmpHome, ".flowsk", "flowsk.db")
	if cfg.LocalSQLitePath != wantDB {
		t.Fatalf("expected LocalSQLitePath %q, got %q", wantDB, cfg.LocalSQLitePath)
	}
	wantReg := filepath.Join(tmpHome, ".flowsk", "local_paths.json")
	if cfg.LocalPathRegistryFile != wantReg {
		t.Fatalf("expected LocalPathRegistryFile %q, got %q", wantReg, cfg.LocalPathRegistryFile)
	}
	if cfg.DefaultWorkspaceSlug != "default" {
		t.Fatalf("expected DefaultWorkspaceSlug %q, got %q", "default", cfg.DefaultWorkspaceSlug)
	}
	if !cfg.DefaultFormat.Valid() {
		t.Fatalf("expected valid DefaultFormat, got %q", cfg.DefaultFormat)
	}
	if cfg.RequestTimeout <= 0 {
		t.Fatalf("expected positive RequestTimeout, got %v", cfg.RequestTimeout)
	}
}

// TestLoadClientConfig_YAMLAndEnvPrecedenceAndRemoteSwitch verifies that YAML
// settings override defaults and FLOWSK_* environment variables cleanly switch
// ClientConfig into remote ConnectRPC mode.
func TestLoadClientConfig_YAMLAndEnvPrecedenceAndRemoteSwitch(t *testing.T) {
	tmpDir := t.TempDir()
	yamlPath := filepath.Join(tmpDir, "config.yaml")
	yamlContent := `
local_sqlite_path: /tmp/from-yaml/flowsk.db
server_url: ""
default_workspace_slug: acme-yaml
actor_handle: mario-yaml
auth_token: yaml-secret-token
local_path_registry_file: /tmp/from-yaml/local_paths.json
default_format: human
default_max_tokens: 1200
request_timeout: 12s
`
	if err := os.WriteFile(yamlPath, []byte(yamlContent), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	// Environment variables must override YAML values and switch cleanly to remote mode.
	t.Setenv("TESTFLOWSK_SERVER_URL", "https://flowsk.internal.example.com:8443/")
	t.Setenv("TESTFLOWSK_ACTOR_HANDLE", "agent-override")
	t.Setenv("TESTFLOWSK_AUTH_TOKEN", "env-super-secret-token")
	t.Setenv("TESTFLOWSK_DEFAULT_FORMAT", "compact")
	t.Setenv("TESTFLOWSK_DEFAULT_MAX_TOKENS", "600")

	loader := config.NewLoader()
	cfg, err := loader.LoadClientConfig(context.Background(), config.LoadOptions{
		ConfigFilePath: yamlPath,
		EnvPrefix:      "TESTFLOWSK",
	})
	if err != nil {
		t.Fatalf("LoadClientConfig returned unexpected error: %v", err)
	}

	if cfg.ServerURL != "https://flowsk.internal.example.com:8443" {
		t.Fatalf("expected normalized remote ServerURL, got %q", cfg.ServerURL)
	}
	if cfg.LocalSQLitePath != "/tmp/from-yaml/flowsk.db" {
		t.Fatalf("expected YAML LocalSQLitePath preserved, got %q", cfg.LocalSQLitePath)
	}
	if cfg.DefaultWorkspaceSlug != "acme-yaml" {
		t.Fatalf("expected DefaultWorkspaceSlug %q, got %q", "acme-yaml", cfg.DefaultWorkspaceSlug)
	}
	if cfg.ActorHandle != "agent-override" {
		t.Fatalf("expected env ActorHandle override, got %q", cfg.ActorHandle)
	}
	if cfg.AuthToken != "env-super-secret-token" {
		t.Fatalf("expected env AuthToken override, got %q", cfg.AuthToken)
	}
	if cfg.DefaultFormat != formatter.FormatCompact {
		t.Fatalf("expected DefaultFormat %q, got %q", formatter.FormatCompact, cfg.DefaultFormat)
	}
	if cfg.DefaultMaxTokens != 600 {
		t.Fatalf("expected DefaultMaxTokens 600, got %d", cfg.DefaultMaxTokens)
	}
	if cfg.RequestTimeout != 12*time.Second {
		t.Fatalf("expected RequestTimeout 12s, got %v", cfg.RequestTimeout)
	}
}

// TestLoadServerConfig_DefaultsYAMLAndEnv verifies ServerConfig loading across
// defaults, YAML configuration files, and FLOWSK_* environment variables.
func TestLoadServerConfig_DefaultsYAMLAndEnv(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmpDir, "data"))

	loader := config.NewLoader()
	defCfg, err := loader.LoadServerConfig(context.Background(), config.LoadOptions{
		EnvPrefix: "TESTSRVDEF",
	})
	if err != nil {
		t.Fatalf("LoadServerConfig defaults failed: %v", err)
	}
	if defCfg.Database.Dialect != db.DialectSQLite {
		t.Fatalf("expected default dialect sqlite, got %q", defCfg.Database.Dialect)
	}
	if defCfg.HTTP.ListenAddr == "" {
		t.Fatal("expected non-empty default HTTP.ListenAddr")
	}
	if !defCfg.Database.SQLite.EnableWAL || !defCfg.Database.SQLite.ForeignKeys {
		t.Fatal("expected SQLite WAL and ForeignKeys enabled by default")
	}

	yamlPath := filepath.Join(tmpDir, "server.yaml")
	yamlContent := `
environment: staging
log_level: debug
http:
  listen_addr: 0.0.0.0:9090
  read_header_timeout: 3s
  read_timeout: 20s
  write_timeout: 25s
  shutdown_grace_period: 10s
database:
  dialect: postgres
  auto_migrate: false
  postgres:
    dsn: postgres://flowsk_user:yaml_pass@db.internal:5432/flowsk?sslmode=require
    max_conns: 32
    min_conns: 4
volume:
  enabled: true
  root_dir: /data/flowsk
  snapshot_retention_count: 10
  auto_snapshot_interval: 15m
mcp:
  enabled: true
  transport: http
  http_path: /mcp/v1
  default_max_output_tokens: 1500
  max_batch_statements: 50
`
	if err := os.WriteFile(yamlPath, []byte(yamlContent), 0o600); err != nil {
		t.Fatalf("write server.yaml: %v", err)
	}

	t.Setenv("TESTSRV_ENVIRONMENT", "production")
	t.Setenv("TESTSRV_DATABASE_POSTGRES_DSN", "postgres://flowsk_user:env_pass_123@pg.prod:5432/flowsk")
	t.Setenv("TESTSRV_HTTP_LISTEN_ADDR", "127.0.0.1:18080")

	srvCfg, err := loader.LoadServerConfig(context.Background(), config.LoadOptions{
		ConfigFilePath: yamlPath,
		EnvPrefix:      "TESTSRV",
	})
	if err != nil {
		t.Fatalf("LoadServerConfig YAML+env failed: %v", err)
	}

	if srvCfg.Environment != "production" {
		t.Fatalf("expected Environment production, got %q", srvCfg.Environment)
	}
	if srvCfg.HTTP.ListenAddr != "127.0.0.1:18080" {
		t.Fatalf("expected HTTP.ListenAddr 127.0.0.1:18080, got %q", srvCfg.HTTP.ListenAddr)
	}
	if srvCfg.Database.Dialect != db.DialectPostgres {
		t.Fatalf("expected Dialect postgres, got %q", srvCfg.Database.Dialect)
	}
	if srvCfg.Database.AutoMigrate {
		t.Fatal("expected AutoMigrate false from YAML")
	}
	if srvCfg.Database.Postgres.MaxConns != 32 {
		t.Fatalf("expected MaxConns 32, got %d", srvCfg.Database.Postgres.MaxConns)
	}
	if srvCfg.MCP.Transport != config.MCPTransportStreamableHTTP {
		t.Fatalf("expected MCP transport http, got %q", srvCfg.MCP.Transport)
	}
	if srvCfg.Volume.SnapshotRetentionCount != 10 {
		t.Fatalf("expected SnapshotRetentionCount 10, got %d", srvCfg.Volume.SnapshotRetentionCount)
	}
}

// TestConfigValidationAndContextCancellation verifies validation errors for
// invalid formats, invalid dialects, missing Postgres DSN, and cancelled contexts.
func TestConfigValidationAndContextCancellation(t *testing.T) {
	loader := config.NewLoader()

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := loader.LoadServerConfig(canceledCtx, config.LoadOptions{}); err == nil {
		t.Fatal("expected error for canceled context in LoadServerConfig")
	}
	if _, err := loader.LoadClientConfig(canceledCtx, config.LoadOptions{}); err == nil {
		t.Fatal("expected error for canceled context in LoadClientConfig")
	}

	t.Setenv("TESTBAD_DATABASE_DIALECT", "postgres")
	t.Setenv("TESTBAD_DATABASE_POSTGRES_DSN", "")
	if _, err := loader.LoadServerConfig(context.Background(), config.LoadOptions{EnvPrefix: "TESTBAD"}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("expected ErrValidation when postgres dialect lacks DSN, got %v", err)
	}

	t.Setenv("TESTBADCLI_SERVER_URL", "file:///Users/mario/repo")
	if _, err := loader.LoadClientConfig(context.Background(), config.LoadOptions{EnvPrefix: "TESTBADCLI"}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("expected ErrValidation for invalid file:// ServerURL, got %v", err)
	}
}

// TestSecretHygiene_RedactsDSNAndAuthToken verifies that Redacted() and fmt.Sprint
// never leak PostgreSQL DSN passwords or client bearer authentication tokens.
func TestSecretHygiene_RedactsDSNAndAuthToken(t *testing.T) {
	t.Parallel()

	srv := config.ServerConfig{
		Environment: "production",
		LogLevel:    "info",
	}
	srv.Database.Dialect = db.DialectPostgres
	srv.Database.Postgres.DSN = "postgres://admin:UltraSecretPass!99@db.example.com:5432/flowsk?sslmode=require"

	redactedSrv := srv.Redacted()
	if strings.Contains(redactedSrv.Database.Postgres.DSN, "UltraSecretPass!99") {
		t.Fatalf("expected Postgres URI password redacted, got %q", redactedSrv.Database.Postgres.DSN)
	}
	if !strings.Contains(redactedSrv.Database.Postgres.DSN, config.RedactedMask) {
		t.Fatalf("expected RedactedMask in redacted DSN, got %q", redactedSrv.Database.Postgres.DSN)
	}
	if strings.Contains(fmt.Sprint(srv), "UltraSecretPass!99") {
		t.Fatalf("expected ServerConfig String() to redact DSN password, got %q", fmt.Sprint(srv))
	}

	// Keyword-value DSN format must also have password redacted.
	srvKV := srv
	srvKV.Database.Postgres.DSN = "host=db.example.com user=admin password=KeyValueSecret42 dbname=flowsk sslmode=require"
	if strings.Contains(srvKV.Redacted().Database.Postgres.DSN, "KeyValueSecret42") {
		t.Fatalf("expected keyword/value Postgres DSN password redacted, got %q", srvKV.Redacted().Database.Postgres.DSN)
	}

	cli := config.ClientConfig{
		ServerURL: "https://flowsk.example.com",
		AuthToken: "flw_live_secret_bearer_token_xyz",
	}
	redactedCLI := cli.Redacted()
	if strings.Contains(redactedCLI.AuthToken, "flw_live_secret_bearer_token_xyz") {
		t.Fatalf("expected AuthToken redacted, got %q", redactedCLI.AuthToken)
	}
	if redactedCLI.AuthToken != config.RedactedMask {
		t.Fatalf("expected AuthToken == %q, got %q", config.RedactedMask, redactedCLI.AuthToken)
	}
	if strings.Contains(fmt.Sprint(cli), "flw_live_secret_bearer_token_xyz") {
		t.Fatalf("expected ClientConfig String() to redact AuthToken, got %q", fmt.Sprint(cli))
	}
}

// TestLoadServerConfig_ServeEnvAliases verifies that canonical `flowsk serve`
// environment aliases (`LISTEN`, `HTTP_ADDR`, `DB_DRIVER`, `DB_DSN`, and `FLOWSK_HOME`)
// resolve consistently with `serve.go`.
func TestLoadServerConfig_ServeEnvAliases(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("FLOWSK_HOME", tmpHome)

	loader := config.NewLoader()

	// 1. Canonical LISTEN + DB_DSN for SQLite.
	t.Setenv("TESTALIAS_LISTEN", ":19090")
	t.Setenv("TESTALIAS_HTTP_ADDR", ":19999")
	t.Setenv("TESTALIAS_DB_DRIVER", "sqlite")
	t.Setenv("TESTALIAS_DB_DSN", "/data/flowsk/custom.db")
	t.Setenv("TESTALIAS_SQLITE_PATH", "/data/flowsk/legacy.db")

	sqliteCfg, err := loader.LoadServerConfig(context.Background(), config.LoadOptions{
		EnvPrefix: "TESTALIAS",
	})
	if err != nil {
		t.Fatalf("LoadServerConfig SQLite aliases failed: %v", err)
	}
	if sqliteCfg.HTTP.ListenAddr != ":19090" {
		t.Fatalf("expected LISTEN alias :19090 to win over HTTP_ADDR, got %q", sqliteCfg.HTTP.ListenAddr)
	}
	if sqliteCfg.Database.Dialect != db.DialectSQLite {
		t.Fatalf("expected dialect sqlite, got %q", sqliteCfg.Database.Dialect)
	}
	if sqliteCfg.Database.SQLite.FilePath != "/data/flowsk/custom.db" {
		t.Fatalf("expected DB_DSN /data/flowsk/custom.db, got %q", sqliteCfg.Database.SQLite.FilePath)
	}

	// 2. Fallback HTTP_ADDR + DB_DRIVER=postgres + DB_DSN.
	t.Setenv("TESTALIASPG_HTTP_ADDR", "0.0.0.0:18088")
	t.Setenv("TESTALIASPG_DB_DRIVER", "postgres")
	t.Setenv("TESTALIASPG_DB_DSN", "postgres://flowsk:secret@pg.local:5432/flowsk?sslmode=disable")

	pgCfg, err := loader.LoadServerConfig(context.Background(), config.LoadOptions{
		EnvPrefix: "TESTALIASPG",
	})
	if err != nil {
		t.Fatalf("LoadServerConfig Postgres aliases failed: %v", err)
	}
	if pgCfg.HTTP.ListenAddr != "0.0.0.0:18088" {
		t.Fatalf("expected HTTP_ADDR fallback 0.0.0.0:18088, got %q", pgCfg.HTTP.ListenAddr)
	}
	if pgCfg.Database.Dialect != db.DialectPostgres {
		t.Fatalf("expected dialect postgres from DB_DRIVER, got %q", pgCfg.Database.Dialect)
	}
	if pgCfg.Database.Postgres.DSN != "postgres://flowsk:secret@pg.local:5432/flowsk?sslmode=disable" {
		t.Fatalf("expected Postgres DSN from DB_DSN, got %q", pgCfg.Database.Postgres.DSN)
	}
}
