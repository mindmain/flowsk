// File doctor_install_update_test.go tests the Lip Gloss + Bubble Tea
// `flowsk install` TUI model and CLI execution, `flowsk doctor` diagnostics
// (skill hash verification, orphan prune warnings, DB health), and `flowsk update`
// GitHub release checks (unpublished 404, up-to-date, and newer release available).
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

func TestInstallTUIModelInteractiveFlow(t *testing.T) {
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
	initialTargets := model.SelectedTargets()
	if len(initialTargets) != 2 {
		t.Fatalf("expected 2 detected providers pre-selected (codex + antigravity), got %d", len(initialTargets))
	}

	// Render Stage 1 (multi-select) and verify Lip Gloss header + badges appear.
	stage1View := model.View()
	if !strings.Contains(stage1View, "FLOWSK SKILL INSTALLER") || !strings.Contains(stage1View, "OpenAI Codex") {
		t.Fatalf("unexpected Stage 1 view:\n%s", stage1View)
	}

	// Toggle first item (Claude Code at cursor 0) ON with space, then press enter to move to confirmation stage.
	_, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	_, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})

	stage2View := model.View()
	if !strings.Contains(stage2View, "Confirm & Install") || !strings.Contains(stage2View, "3 provider(s)") {
		t.Fatalf("expected Stage 2 confirmation view for 3 providers, got:\n%s", stage2View)
	}

	// Press 'enter' on default focused button ("Confirm & Install").
	_, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil || !model.Confirmed() || model.Canceled() {
		t.Fatalf("expected model Confirmed()=true and quit cmd after Enter")
	}
	if len(model.SelectedTargets()) != 3 {
		t.Fatalf("expected 3 selected targets, got %d", len(model.SelectedTargets()))
	}
}

func TestInstallAndDoctorCommandsEndToEnd(t *testing.T) {
	flowskHome := filepath.Join(t.TempDir(), ".flowsk")
	userHome := t.TempDir()
	t.Setenv("FLOWSK_HOME", flowskHome)
	t.Setenv("HOME", userHome)

	// Simulate Codex and Antigravity installed on this workstation, plus Codex MCP config.
	if err := os.MkdirAll(filepath.Join(userHome, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userHome, ".codex", "config.toml"), []byte("[mcp_servers.flowsk]\ncommand = \"flowsk\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(userHome, ".gemini", "antigravity"), 0o755); err != nil {
		t.Fatal(err)
	}

	env := newInMemoryEnv()
	env.allMigrationsApplied = true
	env.orphanCnt = 2 // Trigger orphan prune warning in doctor!

	// Create local SQLite file placeholder so doctor exercises DB Ping + MigrateStatus + InspectOrphans.
	if err := os.MkdirAll(flowskHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(flowskHome, "flowsk.db"), []byte("sqlite-placeholder"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. Before `flowsk install`, `flowsk doctor` warns that detected providers lack SKILL.md and orphans exist.
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
		if rep.WarnCount < 2 {
			t.Fatalf("expected warnings for missing skills and orphan resources, got %+v", rep)
		}
	}

	// 2. Run `flowsk install --providers codex,antigravity --yes`.
	{
		root := cli.NewRootCommand(env)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs([]string{"install", "--home", userHome, "--providers", "codex,antigravity", "--yes", "--format", "json"})
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("flowsk install failed: %v", err)
		}
		var inst cli.InstallSkillView
		if err := json.Unmarshal(out.Bytes(), &inst); err != nil {
			t.Fatalf("unmarshal InstallSkillView: %v", err)
		}
		if inst.InstalledCount != 2 || inst.EmbeddedSHA256 != skill.EmbeddedHash() {
			t.Fatalf("expected 2 installed skills matching embedded SHA-256, got %+v", inst)
		}
	}

	// 3. Clear orphans and verify `flowsk doctor` passes with 0 warnings in --strict mode!
	env.orphanCnt = 0
	{
		root := cli.NewRootCommand(env)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs([]string{"doctor", "--home", userHome, "--strict", "--format", "human"})
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("flowsk doctor --strict expected 0 warnings after install & prune, got err=%v\nout=%s", err, out.String())
		}
		if !strings.Contains(out.String(), "FLOWSK DOCTOR") || !strings.Contains(out.String(), "up-to-date") {
			t.Fatalf("unexpected human doctor output:\n%s", out.String())
		}
	}

	// 4. Corrupt Antigravity SKILL.md on disk and verify `flowsk doctor --strict` catches the hash mismatch!
	corruptPath := filepath.Join(userHome, ".gemini", "antigravity", "skills", "flowsk", "SKILL.md")
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

	runUpdate := func() cli.UpdateCheckView {
		t.Helper()
		root := cli.NewRootCommand(cli.DefaultDependencies())
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs([]string{"update", "--api-base", srv.URL, "--format", "json"})
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("flowsk update (%s) failed: %v", mode, err)
		}
		var v cli.UpdateCheckView
		if err := json.Unmarshal(out.Bytes(), &v); err != nil {
			t.Fatalf("unmarshal UpdateCheckView: %v", err)
		}
		return v
	}

	mode = "unpublished"
	unpub := runUpdate()
	if unpub.Status != "unpublished" || unpub.UpdateAvailable {
		t.Fatalf("expected unpublished status on 404, got %+v", unpub)
	}

	mode = "up_to_date"
	current := runUpdate()
	if current.Status != "up_to_date" || current.UpdateAvailable {
		t.Fatalf("expected up_to_date status, got %+v", current)
	}

	mode = "newer"
	newer := runUpdate()
	if newer.Status != "update_available" || !newer.UpdateAvailable || newer.LatestVersion != "v9.9.0" || !strings.Contains(newer.UpgradeCommand, "@v9.9.0") {
		t.Fatalf("expected update_available v9.9.0, got %+v", newer)
	}
}
