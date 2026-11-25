// File root_test.go verifies root command bootstrap, DefaultDependencies
// sentinel behavior for unwired client ports, self-contained client commands,
// and persistent flag validation.
package cli_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mindmain/flowsk/internal/cli"
)

// TestNewRootCommandDefaultDependenciesSentinelNotImplemented verifies that
// DefaultDependencies returns ErrNotImplemented for unwired client ports.
func TestNewRootCommandDefaultDependenciesSentinelNotImplemented(t *testing.T) {
	t.Setenv("FLOWSK_HOME", filepath.Join(t.TempDir(), ".flowsk"))

	cases := [][]string{
		{"init", "--yes"},
		{"board"},
		{"status", "--format", "human"},
		{"serve", "--listen", "127.0.0.1:18080", "--db-driver", "sqlite"},
		{"mcp", "serve", "--transport", "stdio"},
		{"workspace", "create", "acme", "--name", "Acme"},
		{"workspace", "list"},
		{"workspace", "get"},
		{"workspace", "actor", "register", "claude-dev", "--kind", "agent"},
		{"workspace", "actor", "list"},
		{"project", "create", "CORE", "--name", "Core"},
		{"project", "list"},
		{"project", "get", "CORE"},
		{"project", "update", "CORE", "--name", "Updated"},
		{"project", "archive", "CORE"},
		{"project", "delete", "CORE"},
		{"project", "bind", "CORE", "WEB", "--subpath", "apps/web"},
		{"project", "unbind", "CORE", "WEB"},
		{"project", "bindings"},
		{"project", "link", "/tmp/mario-desktop-repo", "--workspace", "acme", "--project", "CORE"},
		{"project", "unlink", "--all", "--project", "CORE"},
		{"project", "where"},
		{"project", "state", "list"},
		{"project", "state", "create", "QA"},
		{"project", "label", "list"},
		{"project", "label", "create", "storage"},
		{"objective", "create", "Milestone 1", "--kind", "milestone"},
		{"goal", "list", "--kind", "goal"},
		{"objective", "get", "CORE-O1"},
		{"objective", "update", "CORE-O1", "--title", "New"},
		{"objective", "close", "CORE-O1"},
		{"task", "create", "Task 1"},
		{"task", "list"},
		{"task", "get", "CORE-1"},
		{"task", "update", "CORE-1", "--title", "Updated"},
		{"task", "move", "CORE-1", "Done"},
		{"t", "claim", "CORE-1"},
		{"task", "release", "CORE-1"},
		{"task", "block", "CORE-1", "--reason", "waiting"},
		{"task", "unblock", "CORE-1"},
		{"task", "comment", "CORE-1", "-m", "note"},
		{"task", "comments", "CORE-1"},
		{"task", "next", "--claim"},
		{"issue", "digest", "--max-tokens", "300"},
		{"code", "-e", "return flowsk.tasks.ready();", "--dry-run"},
		{"exec", "-e", "return flowsk.goals.list();"},
		{"search", "state:done"},
		{"audit", "list"},
		{"audit", "get", "evt-1"},
		{"db", "migrate", "up"},
		{"db", "migrate", "down"},
		{"db", "migrate", "status"},
		{"db", "ping"},
	}

	for _, args := range cases {
		root := cli.NewRootCommand(cli.DefaultDependencies())
		var stdout, stderr bytes.Buffer
		root.SetOut(&stdout)
		root.SetErr(&stderr)
		root.SetArgs(args)

		err := root.ExecuteContext(context.Background())
		if !errors.Is(err, cli.ErrNotImplemented) {
			t.Fatalf("args %v: got err=%v, want errors.Is(_, ErrNotImplemented)", args, err)
		}
	}
}

// TestSelfContainedCommandsWorkWithDefaultDependencies verifies local config
// and static MCP guide commands without a backend connection.
func TestSelfContainedCommandsWorkWithDefaultDependencies(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("FLOWSK_HOME", "")

	cases := []struct {
		args []string
		want string
	}{
		{args: []string{"config", "paths", "--format", "compact"}, want: "local_paths.json"},
		{args: []string{"config", "init", "--format", "human"}, want: "config.yaml"},
		{args: []string{"workspace", "use", "acme", "--format", "compact"}, want: "acme"},
		{args: []string{"config", "show", "--format", "human"}, want: "acme"},
		{args: []string{"mcp", "tools", "--schema", "--format", "json"}, want: "flowsk_execute"},
		{args: []string{"mcp", "guide", "--client", "antigravity", "--format", "human"}, want: "flowsk mcp serve"},
	}

	for _, tc := range cases {
		root := cli.NewRootCommand(cli.DefaultDependencies())
		var stdout, stderr bytes.Buffer
		root.SetOut(&stdout)
		root.SetErr(&stderr)
		root.SetArgs(tc.args)

		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("args %v failed: %v (stderr=%s)", tc.args, err, stderr.String())
		}
		if !strings.Contains(stdout.String(), tc.want) {
			t.Fatalf("args %v: stdout=%q, expected substring %q", tc.args, stdout.String(), tc.want)
		}
	}
}

// TestInvalidFormatAndMaxTokensRejectedBeforeRunE verifies persistent flag
// validation prior to subcommand execution.
func TestInvalidFormatAndMaxTokensRejectedBeforeRunE(t *testing.T) {
	t.Setenv("FLOWSK_HOME", filepath.Join(t.TempDir(), ".flowsk"))

	for _, args := range [][]string{
		{"--format", "xml", "project", "list"},
		{"--max-tokens", "-5", "project", "list"},
	} {
		root := cli.NewRootCommand(cli.DefaultDependencies())
		root.SetArgs(args)

		err := root.ExecuteContext(context.Background())
		if err == nil || errors.Is(err, cli.ErrNotImplemented) {
			t.Fatalf("args %v: expected flag validation error, got %v", args, err)
		}
	}
}

// TestCanceledContextPropagated verifies immediate abort on pre-canceled contexts.
func TestCanceledContextPropagated(t *testing.T) {
	t.Setenv("FLOWSK_HOME", filepath.Join(t.TempDir(), ".flowsk"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	root := cli.NewRootCommand(cli.DefaultDependencies())
	root.SetArgs([]string{"config", "paths"})
	if err := root.ExecuteContext(ctx); err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
