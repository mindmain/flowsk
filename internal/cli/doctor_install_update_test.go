// File doctor_install_update_test.go tests the 3-stage Lip Gloss + Bubble Tea
// `flowsk install` and `flowsk uninstall` TUI models and CLI execution,
// `flowsk doctor` diagnostics (skill hash verification, MCP checks, orphan prune
// warnings, DB health), and `flowsk update` GitHub release checks.
package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mindmain/flowsk/internal/cli"
	"github.com/mindmain/flowsk/internal/skill"
	"github.com/mindmain/flowsk/internal/version"
)

func TestInstallAndUninstallTUIModelsInteractiveFlow(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".gemini", "antigravity"), 0o755); err != nil {
		t.Fatal(err)
	}

	statuses, err := skill.InspectAll(home)
	if err != nil {
		t.Fatalf("InspectAll: %v", err)
	}

	model := cli.NewInstallTUIModel(statuses, nil)

	// 1. Default Skill selection MUST be ONLY Universal Agents (~/.agents), NOT other detected providers!
	initialSkillTargets := model.SelectedTargets()
	if len(initialSkillTargets) != 1 || initialSkillTargets[0].ID != skill.ProviderAgents {
		t.Fatalf("expected ONLY Universal (%q) Skill pre-selected by default, got %+v", skill.ProviderAgents, initialSkillTargets)
	}

	// 2. Default MCP selection MUST ALSO be ONLY Universal Agents (~/.agents/mcp.json), NOT auto-selecting codex/antigravity!
	initialMCPTargets := model.SelectedMCPTargets()
	if len(initialMCPTargets) != 1 || initialMCPTargets[0].ID != skill.ProviderAgents {
		t.Fatalf("expected ONLY Universal (%q -> ~/.agents/mcp.json) MCP pre-selected by default, got %+v", skill.ProviderAgents, initialMCPTargets)
	}

	stage1View := model.View()
	if !strings.Contains(stage1View, "Step 1/3") || !strings.Contains(stage1View, "Universal Agents (~/.agents)") {
		t.Fatalf("unexpected Stage 1 view:\n%s", stage1View)
	}

	// Advance to Stage 2 (MCP), verify `~/.agents/mcp.json` is listed, toggle Codex ON as well (row 2), then advance to Stage 3.
	_, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	stage2View := model.View()
	if !strings.Contains(stage2View, "Step 2/3") || !strings.Contains(stage2View, ".agents/mcp.json") {
		t.Fatalf("expected Stage 2 MCP selector view with .agents/mcp.json, got:\n%s", stage2View)
	}

	_, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})                        // row 1 = claude
	_, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})                        // row 2 = codex
	_, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}}) // toggle codex ON
	_, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})

	stage3View := model.View()
	if !strings.Contains(stage3View, "Step 3/3") ||
		!strings.Contains(stage3View, "1. Global Skill") ||
		!strings.Contains(stage3View, "2. Flowsk MCP Server") ||
		!strings.Contains(stage3View, "Confirm & Install") {
		t.Fatalf("expected Stage 3 confirmation view showing both Skill and MCP, got:\n%s", stage3View)
	}

	_, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil || !model.Confirmed() || model.Canceled() {
		t.Fatalf("expected model Confirmed()=true and quit cmd after Enter")
	}
	if len(model.SelectedMCPTargets()) != 2 {
		t.Fatalf("expected 2 selected MCP targets (agents + codex), got %+v", model.SelectedMCPTargets())
	}

	// Install those choices on disk, then test UninstallTUIModel pre-selects ONLY what is actually installed!
	for _, st := range model.SelectedTargets() {
		if _, err := skill.InstallToProvider(st); err != nil {
			t.Fatal(err)
		}
	}
	for _, mt := range model.SelectedMCPTargets() {
		if _, err := skill.InstallMCPToProvider(mt, "flowsk"); err != nil {
			t.Fatal(err)
		}
	}

	postStatuses, err := skill.InspectAll(home)
	if err != nil {
		t.Fatal(err)
	}
	unModel := cli.NewUninstallTUIModel(postStatuses, nil, nil)
	if len(unModel.SelectedTargets()) != 1 || unModel.SelectedTargets()[0].ID != skill.ProviderAgents {
		t.Fatalf("expected UninstallTUIModel to pre-select 1 installed skill (agents), got %+v", unModel.SelectedTargets())
	}
	if len(unModel.SelectedMCPTargets()) != 2 {
		t.Fatalf("expected UninstallTUIModel to pre-select 2 configured MCPs (agents + codex), got %+v", unModel.SelectedMCPTargets())
	}
	_, _ = unModel.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_, _ = unModel.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !strings.Contains(unModel.View(), "Confirm & Uninstall") {
		t.Fatalf("expected Confirm & Uninstall view, got:\n%s", unModel.View())
	}
	_, unCmd := unModel.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if unCmd == nil || !unModel.Confirmed() {
		t.Fatal("expected UninstallTUIModel Confirmed()=true")
	}
}

func TestInstallDoctorAndUninstallCommandsEndToEnd(t *testing.T) {
	flowskHome := filepath.Join(t.TempDir(), ".flowsk")
	userHome := t.TempDir()
	t.Setenv("FLOWSK_HOME", flowskHome)
	t.Setenv("HOME", userHome)

	// Simulate Codex and Antigravity present on this workstation (must NOT be auto-modified by default install!).
	if err := os.MkdirAll(filepath.Join(userHome, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(userHome, ".gemini", "antigravity"), 0o755); err != nil {
		t.Fatal(err)
	}

	env := newInMemoryEnv()
	env.allMigrationsApplied = true
	env.orphanCnt = 2 // Trigger orphan prune warning in doctor!

	if err := os.MkdirAll(flowskHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(flowskHome, "flowsk.db"), []byte("sqlite-placeholder"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. Before `flowsk install`, `flowsk doctor` warns about missing skills, missing MCP, and orphans.
	{
		root := cli.NewRootCommand(env)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs([]string{"doctor", "--home", userHome, "--format", "json"})
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("flowsk doctor (pre-install) failed: %v", err)
		}
		var rep cli.DoctorReportView
		if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
			t.Fatalf("unmarshal DoctorReportView: %v\nraw=%s", err, out.String())
		}
		if rep.WarnCount < 3 {
			t.Fatalf("expected warnings for missing skills, missing MCP, and orphan resources, got %+v", rep)
		}
	}

	// 2. Run `flowsk install --yes` with ZERO provider flags -> installs ONLY into Universal (~/.agents):
	//    Skill -> ~/.agents/skills/flowsk/SKILL.md
	//    MCP   -> ~/.agents/mcp.json
	{
		root := cli.NewRootCommand(env)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs([]string{"install", "--home", userHome, "--mcp-command", "flowsk", "--yes", "--format", "json"})
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("flowsk install --yes failed: %v", err)
		}
		var inst cli.InstallSkillView
		if err := json.Unmarshal(out.Bytes(), &inst); err != nil {
			t.Fatalf("unmarshal InstallSkillView: %v", err)
		}
		if inst.InstalledCount != 1 || len(inst.Items) != 1 || inst.Items[0].ProviderID != skill.ProviderAgents {
			t.Fatalf("expected default Skill install into 'agents' only, got %+v", inst)
		}
		if inst.MCPInstalledCount != 1 || len(inst.MCPItems) != 1 || inst.MCPItems[0].ProviderID != skill.ProviderAgents {
			t.Fatalf("expected default MCP install into 'agents' (~/.agents/mcp.json) only, got %+v", inst)
		}
		if _, err := os.Stat(filepath.Join(userHome, ".codex", "config.toml")); !os.IsNotExist(err) {
			t.Fatal("expected ~/.codex/config.toml NOT to be created when only Universal default was selected")
		}
	}

	// 3. Clear orphans and verify `flowsk doctor --strict` passes with 0 warnings!
	env.orphanCnt = 0
	{
		root := cli.NewRootCommand(env)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs([]string{"doctor", "--home", userHome, "--strict", "--format", "human"})
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("flowsk doctor --strict expected 0 warnings after install & prune, got err=%v\nout=%s", err, out.String())
		}
		if !strings.Contains(out.String(), "FLOWSK DOCTOR") || !strings.Contains(out.String(), "[mcp ✓]") {
			t.Fatalf("expected [mcp ✓] badge on Universal Agents in doctor output:\n%s", out.String())
		}
	}

	// 4. Run `flowsk uninstall --yes` -> removes both ~/.agents/skills/flowsk/SKILL.md and ~/.agents/mcp.json!
	{
		root := cli.NewRootCommand(env)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs([]string{"uninstall", "--home", userHome, "--yes", "--format", "json"})
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("flowsk uninstall --yes failed: %v", err)
		}
		var uninst cli.UninstallSkillView
		if err := json.Unmarshal(out.Bytes(), &uninst); err != nil {
			t.Fatalf("unmarshal UninstallSkillView: %v", err)
		}
		if uninst.RemovedSkillCount != 1 || uninst.RemovedMCPCount != 1 {
			t.Fatalf("expected 1 removed skill and 1 removed MCP, got %+v", uninst)
		}
		if _, err := os.Stat(filepath.Join(userHome, ".agents", "skills", "flowsk", "SKILL.md")); !os.IsNotExist(err) {
			t.Fatal("expected SKILL.md to be removed after uninstall")
		}
		if _, err := os.Stat(filepath.Join(userHome, ".agents", "mcp.json")); !os.IsNotExist(err) {
			t.Fatal("expected ~/.agents/mcp.json to be removed after uninstall")
		}
	}

	// 5. Simulate T3 Code isolated Antigravity profile with 0-byte mcp_config.json while ~/.gemini/config has flowsk:
	//    `flowsk doctor --strict` must warn about the unsynced T3 profile until `flowsk install -p antigravity --yes` syncs it!
	t3CfgDir := filepath.Join(userHome, ".t3", "userdata", "providers", "antigravity", "ac0a3dfd", "config")
	if err := os.MkdirAll(t3CfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t3MCPFile := filepath.Join(t3CfgDir, "mcp_config.json")
	if err := os.WriteFile(t3MCPFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	agCfgDir := filepath.Join(userHome, ".gemini", "config")
	if err := os.MkdirAll(filepath.Join(agCfgDir, "skills", "flowsk"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agCfgDir, "skills", "flowsk", "SKILL.md"), skill.EmbeddedContent(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agCfgDir, "mcp_config.json"), []byte(`{"mcpServers":{"flowsk":{"command":"/usr/local/bin/flowsk","args":["mcp","serve","--transport","stdio"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	{
		root := cli.NewRootCommand(env)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs([]string{"doctor", "--home", userHome, "--strict", "--format", "human"})
		if err := root.ExecuteContext(context.Background()); err == nil {
			t.Fatalf("expected flowsk doctor --strict to warn on unsynced T3 Antigravity mcp_config.json, got nil\nout=%s", out.String())
		}
		if !strings.Contains(out.String(), "unsynced") {
			t.Fatalf("expected [mcp ! 1 unsynced] badge in doctor output, got:\n%s", out.String())
		}
	}
	{
		root := cli.NewRootCommand(env)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs([]string{"install", "--home", userHome, "--providers", "antigravity", "--mcp-command", "/usr/local/bin/flowsk", "--yes", "--format", "json"})
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("flowsk install -p antigravity --yes failed: %v", err)
		}
	}
	{
		root := cli.NewRootCommand(env)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs([]string{"doctor", "--home", userHome, "--strict", "--format", "human"})
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("expected flowsk doctor --strict to pass after syncing T3 Antigravity profile, got %v\nout=%s", err, out.String())
		}
	}
}

func TestUpdateCommandGitHubStates(t *testing.T) {
	t.Setenv("FLOWSK_HOME", filepath.Join(t.TempDir(), ".flowsk"))

	var mode string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "unpublished":
			http.NotFound(w, r)
		case "up_to_date":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"tag_name":"` + version.NormalizedTag() + `","html_url":"https://github.com/mindmain/flowsk/releases/tag/` + version.NormalizedTag() + `"}`))
		case "newer":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"tag_name":"v9.9.0","html_url":"https://github.com/mindmain/flowsk/releases/tag/v9.9.0","published_at":"2026-12-01T00:00:00Z"}`))
		}
	}))
	defer srv.Close()

	env := newInMemoryEnv()

	for _, tc := range []struct {
		mode       string
		wantStatus string
		wantUpdate bool
	}{
		{mode: "unpublished", wantStatus: "unpublished", wantUpdate: false},
		{mode: "up_to_date", wantStatus: "up_to_date", wantUpdate: false},
		{mode: "newer", wantStatus: "update_available", wantUpdate: true},
	} {
		mode = tc.mode
		root := cli.NewRootCommand(env)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs([]string{"update", "--api-base", srv.URL, "--format", "json"})
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("flowsk update (%s) failed: %v", tc.mode, err)
		}
		var view cli.UpdateCheckView
		if err := json.Unmarshal(out.Bytes(), &view); err != nil {
			t.Fatalf("unmarshal UpdateCheckView (%s): %v", tc.mode, err)
		}
		if view.Status != tc.wantStatus || view.UpdateAvailable != tc.wantUpdate {
			t.Fatalf("mode=%s: got status=%q update=%t, want status=%q update=%t",
				tc.mode, view.Status, view.UpdateAvailable, tc.wantStatus, tc.wantUpdate)
		}
	}
}
