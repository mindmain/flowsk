// File doctor_install_update_test.go tests the 3-stage Lip Gloss + Bubble Tea
// `flowsk install` TUI model (Skill -> MCP -> Confirm) and CLI execution,
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

func TestInstallTUIModelInteractiveThreeStageFlow(t *testing.T) {
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

	// 1. Default Skill selection MUST be ONLY Universal Agents (~/.agents), even when other providers exist!
	initialSkillTargets := model.SelectedTargets()
	if len(initialSkillTargets) != 1 || initialSkillTargets[0].ID != skill.ProviderAgents {
		t.Fatalf("expected ONLY Universal (%q) Skill pre-selected by default, got %+v", skill.ProviderAgents, initialSkillTargets)
	}

	// Default MCP selection MUST be Universal (resolving to the 2 detected MCP clients: codex + antigravity)!
	if !model.UniversalMCPSelected() {
		t.Fatal("expected UniversalMCPSelected() == true by default")
	}
	initialMCPTargets := model.SelectedMCPTargets()
	if len(initialMCPTargets) != 2 {
		t.Fatalf("expected Universal MCP to resolve 2 detected clients (codex + antigravity), got %+v", initialMCPTargets)
	}

	// Render Stage 1 (Skill) and verify Lip Gloss header + Universal default appear.
	stage1View := model.View()
	if !strings.Contains(stage1View, "Step 1/3") || !strings.Contains(stage1View, "Universal Agents (~/.agents)") {
		t.Fatalf("unexpected Stage 1 view:\n%s", stage1View)
	}

	// Move cursor down once (to Claude Code) and toggle it ON with space, then press Enter to advance to Stage 2 (MCP).
	_, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	_, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	_, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})

	stage2View := model.View()
	if !strings.Contains(stage2View, "Step 2/3") || !strings.Contains(stage2View, "Universal (All Detected Clients)") {
		t.Fatalf("expected Stage 2 MCP selector view, got:\n%s", stage2View)
	}

	// Press Enter on Stage 2 keeping Universal MCP selected -> advances to Stage 3 (Confirm) showing BOTH Skill and MCP!
	_, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	stage3View := model.View()
	if !strings.Contains(stage3View, "Step 3/3") ||
		!strings.Contains(stage3View, "1. Global Skill") ||
		!strings.Contains(stage3View, "2. Flowsk MCP Server") ||
		!strings.Contains(stage3View, "Confirm & Install") {
		t.Fatalf("expected Stage 3 confirmation view showing both Skill and MCP, got:\n%s", stage3View)
	}

	// Test going back to Stage 2 (MCP) with 'b', switching from Universal to a single MCP client (Claude Code at row 1), then confirming!
	_, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	_, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})                        // row 1 = Claude Code
	_, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}}) // switches off Universal, checks Claude Code only
	if model.UniversalMCPSelected() || len(model.SelectedMCPTargets()) != 1 || model.SelectedMCPTargets()[0].ID != skill.ProviderClaude {
		t.Fatalf("expected explicit single MCP client selection (claude), got universal=%t targets=%+v",
			model.UniversalMCPSelected(), model.SelectedMCPTargets())
	}

	_, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter}) // back to Stage 3
	_, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil || !model.Confirmed() || model.Canceled() {
		t.Fatalf("expected model Confirmed()=true and quit cmd after Enter")
	}
	if len(model.SelectedTargets()) != 2 {
		t.Fatalf("expected 2 selected Skill targets (agents + claude), got %d", len(model.SelectedTargets()))
	}
}

func TestInstallAndDoctorCommandsEndToEnd(t *testing.T) {
	flowskHome := filepath.Join(t.TempDir(), ".flowsk")
	userHome := t.TempDir()
	t.Setenv("FLOWSK_HOME", flowskHome)
	t.Setenv("HOME", userHome)

	// Simulate Codex and Antigravity present on this workstation (with NO MCP configured yet!).
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

	// 2. Run `flowsk install --yes` with ZERO provider flags -> uses Universal defaults:
	//    Skill -> Universal Agents (~/.agents/skills/flowsk/SKILL.md)
	//    MCP   -> Universal (both detected clients: ~/.codex/config.toml & ~/.gemini/antigravity/mcp_config.json)!
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
		if inst.MCPInstalledCount != 2 || len(inst.MCPItems) != 2 {
			t.Fatalf("expected default Universal MCP install into 2 detected clients (codex + antigravity), got %+v", inst)
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
			t.Fatalf("expected [mcp ✓] badge in human doctor output:\n%s", out.String())
		}
	}

	// 4. Corrupt Universal SKILL.md on disk and verify `flowsk doctor --strict` catches the hash mismatch!
	corruptPath := filepath.Join(userHome, ".agents", "skills", "flowsk", "SKILL.md")
	if err := os.WriteFile(corruptPath, []byte("outdated skill body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	{
		root := cli.NewRootCommand(env)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs([]string{"doctor", "--home", userHome, "--strict", "--format", "human"})
		if err := root.ExecuteContext(context.Background()); err == nil {
			t.Fatalf("expected flowsk doctor --strict to fail on outdated SKILL.md hash, out:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "outdated") {
			t.Fatalf("expected 'outdated' badge in doctor output, got:\n%s", out.String())
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
