// File flowsk17_cli_test.go verifies environment variable compatibility in
// `serve` and `db`, non-destructive `workspace use` YAML persistence, UTF-8
// rune-safe truncation and token budgets, and `update` upgrade hints.
package cli_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mindmain/flowsk/internal/cli"
)

func TestServeAndDBEnvVarFallbacks(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLOWSK_HOME", filepath.Join(tmp, ".flowsk"))

	// 1. FLOWSK_HTTP_ADDR + FLOWSK_SQLITE_PATH fallback on `serve` when --listen / --db-dsn / FLOWSK_LISTEN / FLOWSK_DB_DSN unset.
	t.Setenv("FLOWSK_LISTEN", "")
	t.Setenv("FLOWSK_DB_DSN", "")
	t.Setenv("FLOWSK_HTTP_ADDR", "0.0.0.0:19191")
	t.Setenv("FLOWSK_SQLITE_PATH", "/var/lib/flowsk/custom.sqlite")

	env := newInMemoryEnv()
	mustExecCLI(t, env, "", "serve")
	if env.lastServeCfg.ListenAddr != "0.0.0.0:19191" {
		t.Fatalf("expected FLOWSK_HTTP_ADDR fallback 0.0.0.0:19191, got %q", env.lastServeCfg.ListenAddr)
	}
	if env.lastServeCfg.DBDSN != "/var/lib/flowsk/custom.sqlite" {
		t.Fatalf("expected FLOWSK_SQLITE_PATH fallback /var/lib/flowsk/custom.sqlite, got %q", env.lastServeCfg.DBDSN)
	}

	// 2. FLOWSK_POSTGRES_DSN fallback on `serve --db-driver postgres` and `db ping --db-driver postgres`.
	t.Setenv("FLOWSK_POSTGRES_DSN", "postgres://flowsk:pw@pg.internal:5432/flowsk?sslmode=disable")
	mustExecCLI(t, env, "", "serve", "--db-driver", "postgres")
	if env.lastServeCfg.DBDSN != "postgres://flowsk:pw@pg.internal:5432/flowsk?sslmode=disable" {
		t.Fatalf("expected FLOWSK_POSTGRES_DSN fallback on serve, got %q", env.lastServeCfg.DBDSN)
	}

	mustExecCLI(t, env, "", "db", "--db-driver", "postgres", "ping")
	if env.lastPingDriver != "postgres" || env.lastPingDSN != "postgres://flowsk:pw@pg.internal:5432/flowsk?sslmode=disable" {
		t.Fatalf("expected FLOWSK_POSTGRES_DSN fallback on db ping, got driver=%q dsn=%q", env.lastPingDriver, env.lastPingDSN)
	}

	// 3. FLOWSK_SQLITE_PATH fallback on `db ping` (default sqlite driver).
	mustExecCLI(t, env, "", "db", "ping")
	if env.lastPingDriver != "sqlite" || env.lastPingDSN != "/var/lib/flowsk/custom.sqlite" {
		t.Fatalf("expected FLOWSK_SQLITE_PATH fallback on db ping, got driver=%q dsn=%q", env.lastPingDriver, env.lastPingDSN)
	}
}

func TestPersistWorkspacePreservesAuthTokenProjectAndExtraKeys(t *testing.T) {
	tmpHome := t.TempDir()
	flowskDir := filepath.Join(tmpHome, ".flowsk")
	if err := os.MkdirAll(flowskDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(flowskDir, "config.yaml")
	seedYAML := strings.Join([]string{
		`workspace: "default"`,
		`db-path: "/custom/flowsk.db"`,
		`server-url: "https://flowsk.internal"`,
		`auth-token: "tok_live_preserve_me"`,
		`project: "CORE"`,
		`actor: "alice"`,
		`format: "compact"`,
		`max-tokens: 500`,
		`custom-feature: "enabled"`,
		`custom-nested:`,
		`  retries: 3`,
		``,
	}, "\n")
	if err := os.WriteFile(cfgPath, []byte(seedYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", tmpHome)
	t.Setenv("FLOWSK_HOME", flowskDir)

	env := newInMemoryEnv()
	mustExecCLI(t, env, "", "workspace", "use", "platform-prod")

	afterRaw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	after := string(afterRaw)
	for _, want := range []string{
		`workspace: "platform-prod"`,
		`auth-token: "tok_live_preserve_me"`,
		`project: "CORE"`,
		`actor: "alice"`,
		`custom-feature: "enabled"`,
		`retries: 3`,
	} {
		if !strings.Contains(after, want) {
			t.Fatalf("expected %q preserved after workspace use, got:\n%s", want, after)
		}
	}

	// Verify `config show --redact=false` reads back all preserved keys cleanly via Viper!
	shown := mustExecCLI(t, env, "", "--format", "json", "config", "show", "--redact=false")
	for _, want := range []string{`"platform-prod"`, `"tok_live_preserve_me"`, `"CORE"`} {
		if !strings.Contains(shown, want) {
			t.Fatalf("expected %s in config show after workspace use, got:\n%s", want, shown)
		}
	}
}

func TestUTF8SafeTruncationAndUpdateHints(t *testing.T) {
	// 1. Multi-byte UTF-8 commit subject and single-line token budget truncation must NEVER split UTF-8 runes.
	overview := cli.WorkspaceOverviewView{
		Workspace:        "default",
		TotalProjects:    1,
		OrphanIssuesCount: 1,
		Projects: []cli.ProjectStatusSummaryView{
			{
				Project: cli.ProjectView{Key: "UTF", Name: "Unicode Project"},
				Worktrees: []cli.WorktreeCommitView{
					{
						Path:     "/repo/utf",
						Branch:   "main",
						ShortSHA: "abc1234",
						Subject:  strings.Repeat("🚀日本語—テスト", 12),
					},
				},
			},
		},
	}

	var humanBuf bytes.Buffer
	if err := cli.Render(&humanBuf, cli.FormatHuman, 0, overview); err != nil {
		t.Fatalf("Render human overview: %v", err)
	}
	if !utf8.ValidString(humanBuf.String()) {
		t.Fatalf("Render human produced invalid UTF-8:\n%q", humanBuf.String())
	}

	longTask := cli.TaskListResult{
		Items: []cli.TaskSummaryView{
			{
				Key:      "UTF-1",
				Priority: 1,
				State:    "Todo",
				Title:    strings.Repeat("🔥マルチバイト文字テスト", 20),
			},
		},
	}
	for budget := 5; budget <= 15; budget++ {
		var compactBuf bytes.Buffer
		if err := cli.Render(&compactBuf, cli.FormatCompact, budget, longTask); err != nil {
			t.Fatalf("Render compact budget=%d: %v", budget, err)
		}
		if !utf8.ValidString(compactBuf.String()) {
			t.Fatalf("enforceTokenBudget(maxTokens=%d) split multi-byte UTF-8 rune: %q", budget, compactBuf.String())
		}
	}

	// 2. Verify `flowsk update` human output mentions go install, task install, and binary download.
	t.Setenv("FLOWSK_HOME", filepath.Join(t.TempDir(), ".flowsk"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"v99.0.0","html_url":"https://github.com/mindmain/flowsk/releases/tag/v99.0.0"}`))
	}))
	defer srv.Close()

	root := cli.NewRootCommand(newInMemoryEnv())
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"update", "--api-base", srv.URL, "--format", "human"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("flowsk update --format human failed: %v", err)
	}
	rendered := out.String()
	for _, want := range []string{"go install", "task install", "Download binary"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("expected %q in human update output, got:\n%s", want, rendered)
		}
	}
}
