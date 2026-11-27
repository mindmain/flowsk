// File mcp.go implements the `flowsk mcp` CLI subcommand group (`serve`, `tools`,
// `guide`) exposing Flowsk over stdio or Streamable HTTP Model Context Protocol.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/mindmain/flowsk/internal/transport/mcp"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type mcpCommandDeps interface {
	MCPDependency
	FormatterDependency
}

type mcpHandler struct {
	opts      *GlobalOptions
	runner    MCPRunner
	formatter OutputFormatter
}

// newMCPCommand constructs `flowsk mcp` and its `serve`, `tools`, and `guide`
// subcommands with immediate constructor dependency resolution.
func newMCPCommand(v *viper.Viper, opts *GlobalOptions, deps mcpCommandDeps) *cobra.Command {
	var (
		runner    MCPRunner
		formatter OutputFormatter
	)
	if deps != nil {
		runner = deps.MCPRunner()
		formatter = deps.OutputFormatter()
	}
	h := &mcpHandler{
		opts:      opts,
		runner:    runner,
		formatter: formatter,
	}

	mcpCmd := &cobra.Command{
		Use:   "mcp",
		Short: "Model Context Protocol (MCP) server, tool descriptors, and agent setup guides",
		Long: `Exposes Flowsk's token-frugal Model Context Protocol (MCP) interface over stdio
or Streamable HTTP for Claude, Gemini, ChatGPT, Codex, and Antigravity agents.

Instead of dozens of verbose CRUD tools, Flowsk implements Cloudflare Code Mode
advertising four compact tools and one on-demand specification resource:
  - flowsk_search          GitHub-style search across tasks, goals, or JS SDK methods
  - flowsk_execute         Sandboxed ES6 JavaScript (Code Mode) in one ACID transaction
  - flowsk_language_guide  On-demand TypeScript .d.ts SDK & search reference
  - flowsk_digest          Ultra-low-token project & ready-task digest
  - flowsk://language/guide Read-only Markdown SDK specification resource`,
	}

	mcpCmd.AddCommand(
		h.newServeSubcommand(v),
		h.newToolsSubcommand(),
		h.newGuideSubcommand(),
	)

	return mcpCmd
}

func (h *mcpHandler) newServeSubcommand(v *viper.Viper) *cobra.Command {
	var (
		transport string
		httpAddr  string
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the Flowsk MCP server over stdio or Streamable HTTP",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cmd.Context().Err(); err != nil {
				return fmt.Errorf("mcp serve context canceled: %w", err)
			}
			rawTransport := strings.TrimSpace(v.GetString("mcp-transport"))
			if rawTransport == "" {
				rawTransport = transport
			}
			kind := mcp.TransportKind(strings.ToLower(rawTransport))
			if !kind.Valid() {
				return fmt.Errorf("invalid --transport %q: must be stdio or http", rawTransport)
			}
			if h.runner == nil {
				return ErrNotImplemented
			}
			resolvedAddr := strings.TrimSpace(v.GetString("mcp-http-addr"))
			if resolvedAddr == "" {
				resolvedAddr = httpAddr
			}
			mcpCfg := mcp.Config{
				Transport:          kind,
				HTTPAddr:           resolvedAddr,
				Stdin:              cmd.InOrStdin(),
				Stdout:             cmd.OutOrStdout(),
				Stderr:             cmd.ErrOrStderr(),
				DefaultWorkspaceID: defaultWorkspace(h.opts.Workspace),
				DefaultProjectID:   strings.TrimSpace(h.opts.Project),
				DefaultActorID:     strings.TrimSpace(h.opts.Actor),
				MaxTokensDefault:   h.opts.MaxTokens,
			}
			setMCPConfigAuthToken(&mcpCfg, firstNonEmptyCLI(h.opts.AuthToken, os.Getenv("FLOWSK_AUTH_TOKEN")))
			srv := h.runner.NewMCPServer(mcpCfg)
			err := srv.Serve(cmd.Context())
			if errors.Is(err, mcp.ErrNotImplemented) {
				return ErrNotImplemented
			}
			return err
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&transport, "transport", string(mcp.TransportStdio), "MCP wire transport: stdio|http (env FLOWSK_MCP_TRANSPORT)")
	flags.StringVar(&httpAddr, "http-addr", "127.0.0.1:8081", "Listen address when --transport=http (env FLOWSK_MCP_HTTP_ADDR)")
	_ = v.BindPFlag("mcp-transport", flags.Lookup("transport"))
	_ = v.BindPFlag("mcp-http-addr", flags.Lookup("http-addr"))

	return cmd
}

func (h *mcpHandler) newToolsSubcommand() *cobra.Command {
	var includeSchema bool

	cmd := &cobra.Command{
		Use:   "tools",
		Short: "Inspect canonical Flowsk MCP tool descriptors and input schemas",
		Long:  "Lists the registered Flowsk MCP tools (flowsk_search, flowsk_execute, flowsk_language_guide, flowsk_digest) and optionally dumps their JSON Schemas.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cmd.Context().Err(); err != nil {
				return fmt.Errorf("mcp tools context canceled: %w", err)
			}
			specs := mcp.CanonicalTools()
			items := make([]MCPToolItemView, 0, len(specs))
			for _, sp := range specs {
				item := MCPToolItemView{
					Name:        sp.Name,
					Title:       sp.Title,
					Description: sp.Description,
					ReadOnly:    sp.Annotations.ReadOnlyHint,
					Idempotent:  sp.Annotations.IdempotentHint,
				}
				if includeSchema && len(sp.InputSchema) > 0 {
					var parsed any
					if err := json.Unmarshal(sp.InputSchema, &parsed); err == nil {
						item.InputSchema = parsed
					} else {
						item.InputSchema = string(sp.InputSchema)
					}
				}
				items = append(items, item)
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, MCPToolsView{
				Tools:         items,
				Resources:     mcp.CanonicalResources(),
				IncludeSchema: includeSchema,
			})
		},
	}

	cmd.Flags().BoolVar(&includeSchema, "schema", false, "Include full JSON Schema definitions for each MCP tool")
	return cmd
}

func (h *mcpHandler) newGuideSubcommand() *cobra.Command {
	var clientTarget string

	cmd := &cobra.Command{
		Use:   "guide",
		Short: "Print MCP configuration snippets and agent onboarding instructions",
		Long:  "Renders copy-paste MCP server configuration blocks and minimal system-prompt snippets for Claude, Gemini, Antigravity, Codex, and ChatGPT harnesses.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cmd.Context().Err(); err != nil {
				return fmt.Errorf("mcp guide context canceled: %w", err)
			}
			target := strings.ToLower(strings.TrimSpace(clientTarget))
			if target == "" {
				target = "all"
			}
			allSnippets := buildMCPClientSnippets(defaultWorkspace(h.opts.Workspace))
			var selected []MCPClientSnippetView
			for _, s := range allSnippets {
				if target == "all" || s.Client == target {
					selected = append(selected, s)
				}
			}
			if len(selected) == 0 {
				return fmt.Errorf("invalid --client %q: must be one of all, claude, gemini, antigravity, codex, chatgpt", clientTarget)
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, MCPGuideView{
				TargetClient: target,
				Snippets:     selected,
			})
		},
	}

	cmd.Flags().StringVar(&clientTarget, "client", "all", "Target agent client: all|claude|gemini|antigravity|codex|chatgpt")
	return cmd
}

func setMCPConfigAuthToken(cfg *mcp.Config, token string) {
	if cfg == nil {
		return
	}
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return
	}
	rv := reflect.ValueOf(cfg).Elem()
	if f := rv.FieldByName("AuthToken"); f.IsValid() && f.CanSet() && f.Kind() == reflect.String {
		f.SetString(trimmed)
	}
}

func buildMCPClientSnippets(workspace string) []MCPClientSnippetView {
	promptHint := "Poll project state with flowsk_digest, filter tasks/goals or discover SDK methods with flowsk_search, and execute multi-step ES6 JavaScript against flowsk.* atomically via flowsk_execute."
	jsonBlock := func(actor string) string {
		return fmt.Sprintf(`# Command: flowsk mcp serve --transport stdio --workspace %s --actor %s
{
  "mcpServers": {
    "flowsk": {
      "command": "flowsk",
      "args": ["mcp", "serve", "--transport", "stdio", "--workspace", %q, "--actor", %q]
    }
  }
}`, workspace, actor, workspace, actor)
	}
	return []MCPClientSnippetView{
		{
			Client:         "claude",
			ConfigPathHint: "~/.claude.json or .mcp.json",
			ConfigSnippet:  jsonBlock("claude-agent"),
			SystemPrompt:   promptHint,
		},
		{
			Client:         "gemini",
			ConfigPathHint: "~/.gemini/settings.json",
			ConfigSnippet:  jsonBlock("gemini-agent"),
			SystemPrompt:   promptHint,
		},
		{
			Client:         "antigravity",
			ConfigPathHint: "~/.gemini/config/mcp_config.json",
			ConfigSnippet:  jsonBlock("antigravity-agent"),
			SystemPrompt:   promptHint,
		},
		{
			Client:         "codex",
			ConfigPathHint: "~/.codex/config.toml",
			ConfigSnippet: fmt.Sprintf(`[mcp_servers.flowsk]
command = "flowsk"
args = ["mcp", "serve", "--transport", "stdio", "--workspace", %q, "--actor", "codex-agent"]`, workspace),
			SystemPrompt: promptHint,
		},
		{
			Client:         "chatgpt",
			ConfigPathHint: "Streamable HTTP MCP endpoint",
			ConfigSnippet:  fmt.Sprintf("flowsk mcp serve --transport http --http-addr 127.0.0.1:8081 --workspace %s", workspace),
			SystemPrompt:   promptHint,
		},
	}
}
