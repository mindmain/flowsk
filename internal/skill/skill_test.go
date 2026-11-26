// File skill_test.go verifies embedded SKILL.md integrity, SHA-256 hash
// determinism, Universal (~/.agents) provider defaults, atomic skill install/uninstall,
// non-destructive JSON/TOML MCP install/uninstall (including symlink preservation),
// and semver comparison.
package skill_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mindmain/flowsk/internal/skill"
	"github.com/mindmain/flowsk/internal/version"
)

func TestEmbeddedSkillIntegrityAndContent(t *testing.T) {
	md := skill.EmbeddedMarkdown()
	if len(md) < 200 {
		t.Fatalf("embedded SKILL.md unexpectedly short: %d bytes", len(md))
	}
	for _, required := range []string{
		"name: flowsk",
		"description:",
		"flowsk_digest",
		"flowsk_search",
		"flowsk_execute",
		"externalId",
		"upsertExternal",
		"flowsk mcp serve --transport stdio",
	} {
		if !strings.Contains(md, required) {
			t.Fatalf("embedded SKILL.md missing required operational token %q", required)
		}
	}

	hash := skill.EmbeddedHash()
	if len(hash) != 64 {
		t.Fatalf("expected 64-char SHA-256 hex digest, got %q", hash)
	}
	if got := skill.ShortHash(hash); len(got) != 12 {
		t.Fatalf("expected 12-char short hash, got %q", got)
	}
}

func TestSupportedProvidersOrderUniversalFirstWithMCP(t *testing.T) {
	home := t.TempDir()
	providers, err := skill.SupportedProviders(home)
	if err != nil {
		t.Fatalf("SupportedProviders: %v", err)
	}
	if len(providers) == 0 || providers[0].ID != skill.ProviderAgents {
		t.Fatalf("expected Universal Agents (%q) at index 0, got %+v", skill.ProviderAgents, providers)
	}
	if !providers[0].SupportsMCP() || providers[0].MCPConfigPath != filepath.Join(home, ".agents", "mcp.json") {
		t.Fatalf("expected Universal Agents (~/.agents) to manage ~/.agents/mcp.json, got %+v", providers[0])
	}
}

func TestInstallInspectAndUninstallSkillLifecycle(t *testing.T) {
	home := t.TempDir()
	// Create a sibling skill in ~/.agents/skills/other to verify uninstall never touches siblings.
	siblingDir := filepath.Join(home, ".agents", "skills", "other-skill")
	if err := os.MkdirAll(siblingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(siblingDir, "SKILL.md"), []byte("# sibling\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	targets, err := skill.ResolveProvidersByIDs(home, []string{"agents"})
	if err != nil || len(targets) != 1 {
		t.Fatalf("ResolveProvidersByIDs failed: %v (%d)", err, len(targets))
	}
	agentsTarget := targets[0]

	first, err := skill.InstallToProvider(agentsTarget)
	if err != nil {
		t.Fatalf("InstallToProvider (first): %v", err)
	}
	if first.Action != "installed" || first.SHA256 != skill.EmbeddedHash() {
		t.Fatalf("unexpected first install result: %+v", first)
	}

	after := skill.InspectProvider(agentsTarget)
	if !after.Installed || !after.UpToDate || after.InstalledHash != skill.EmbeddedHash() {
		t.Fatalf("expected up-to-date skill after install, got %+v", after)
	}

	// Idempotent second install should report "unchanged".
	second, err := skill.InstallToProvider(agentsTarget)
	if err != nil {
		t.Fatalf("InstallToProvider (second): %v", err)
	}
	if second.Action != "unchanged" {
		t.Fatalf("expected unchanged on second install, got %+v", second)
	}

	// Tamper with installed SKILL.md to simulate an outdated skill version.
	if err := os.WriteFile(agentsTarget.SkillFilePath, []byte("# outdated skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tampered := skill.InspectProvider(agentsTarget)
	if !tampered.Installed || tampered.UpToDate {
		t.Fatalf("expected outdated detection after tampering, got %+v", tampered)
	}

	updated, err := skill.InstallToProvider(agentsTarget)
	if err != nil {
		t.Fatalf("InstallToProvider (update): %v", err)
	}
	if updated.Action != "updated" || updated.PreviousSHA256 == "" || updated.SHA256 != skill.EmbeddedHash() {
		t.Fatalf("expected updated action with previous hash, got %+v", updated)
	}

	// Uninstall removes flowsk/SKILL.md and empty flowsk/ folder while keeping sibling-skill intact!
	unres, err := skill.UninstallFromProvider(agentsTarget)
	if err != nil || unres.Action != "removed" {
		t.Fatalf("UninstallFromProvider: res=%+v err=%v", unres, err)
	}
	if _, err := os.Stat(filepath.Dir(agentsTarget.SkillFilePath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected empty flowsk skill directory to be removed, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(siblingDir, "SKILL.md")); err != nil {
		t.Fatalf("expected sibling skill to remain untouched: %v", err)
	}

	// Second uninstall is idempotent ("not_found").
	unres2, err := skill.UninstallFromProvider(agentsTarget)
	if err != nil || unres2.Action != "not_found" {
		t.Fatalf("expected second UninstallFromProvider action=not_found, got %+v err=%v", unres2, err)
	}
}

func TestInstallAndUninstallMCPJSONSymlinkAndTOMLPreservation(t *testing.T) {
	home := t.TempDir()

	// 1. Universal (~/.agents/mcp.json) starts non-existent -> created on install, deleted cleanly on uninstall!
	// 2. Antigravity JSON config via symbolic link -> ~/.gemini/config/mcp_config.json
	sharedDir := filepath.Join(home, ".gemini", "config")
	agDir := filepath.Join(home, ".gemini", "antigravity")
	if err := os.MkdirAll(sharedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(agDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realSharedJSON := filepath.Join(sharedDir, "mcp_config.json")
	initialJSON := `{"theme":"dark","mcpServers":{"StitchMCP":{"command":"stitch","args":["serve"]}}}` + "\n"
	if err := os.WriteFile(realSharedJSON, []byte(initialJSON), 0o640); err != nil {
		t.Fatal(err)
	}
	symlinkJSON := filepath.Join(agDir, "mcp_config.json")
	if err := os.Symlink(realSharedJSON, symlinkJSON); err != nil {
		t.Fatal(err)
	}

	// 3. Codex TOML config with comments, other servers, and nested env table.
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	codexTOMLPath := filepath.Join(codexDir, "config.toml")
	initialTOML := "# User Codex settings\nmodel = \"gpt-5\"\n\n[mcp_servers.crwai]\ncommand = \"crwai\"\n\n[mcp_servers.crwai.env]\nMODE = \"prod\"\n"
	if err := os.WriteFile(codexTOMLPath, []byte(initialTOML), 0o600); err != nil {
		t.Fatal(err)
	}

	// "universal" alias must map strictly to "agents" (~/.agents/mcp.json), NOT all providers!
	uniOnly, err := skill.ResolveMCPProvidersByIDs(home, []string{"universal"})
	if err != nil || len(uniOnly) != 1 || uniOnly[0].ID != skill.ProviderAgents {
		t.Fatalf("expected 'universal' to resolve strictly to 'agents', got %+v err=%v", uniOnly, err)
	}

	mcpTargets, err := skill.ResolveMCPProvidersByIDs(home, []string{"agents", "antigravity", "codex"})
	if err != nil || len(mcpTargets) != 3 {
		t.Fatalf("ResolveMCPProvidersByIDs: got %d (%+v) err=%v", len(mcpTargets), mcpTargets, err)
	}

	for _, target := range mcpTargets {
		res, err := skill.InstallMCPToProvider(target, "/opt/bin/flowsk")
		if err != nil || res.Action != "installed" {
			t.Fatalf("InstallMCPToProvider(%s): res=%+v err=%v", target.ID, res, err)
		}
		again, err := skill.InstallMCPToProvider(target, "/opt/bin/flowsk")
		if err != nil || again.Action != "unchanged" {
			t.Fatalf("InstallMCPToProvider(%s, second): res=%+v err=%v", target.ID, again, err)
		}
		upd, err := skill.InstallMCPToProvider(target, "/usr/local/bin/flowsk")
		if err != nil || upd.Action != "updated" {
			t.Fatalf("InstallMCPToProvider(%s, update): res=%+v err=%v", target.ID, upd, err)
		}
	}

	// Verify Antigravity symlink remained a symlink and unrelated keys survived.
	linfo, err := os.Lstat(symlinkJSON)
	if err != nil || (linfo.Mode()&os.ModeSymlink) == 0 {
		t.Fatalf("expected %s to remain a symlink, got mode=%v err=%v", symlinkJSON, linfo.Mode(), err)
	}

	// Now UNINSTALL MCP from all 3 targets and verify clean non-destructive removal!
	for _, target := range mcpTargets {
		ures, err := skill.UninstallMCPFromProvider(target)
		if err != nil || ures.Action != "removed" {
			t.Fatalf("UninstallMCPFromProvider(%s): res=%+v err=%v", target.ID, ures, err)
		}
		ures2, err := skill.UninstallMCPFromProvider(target)
		if err != nil || ures2.Action != "not_found" {
			t.Fatalf("UninstallMCPFromProvider(%s, second): res=%+v err=%v", target.ID, ures2, err)
		}
	}

	// Standalone ~/.agents/mcp.json had only flowsk -> should be removed completely!
	if _, err := os.Stat(filepath.Join(home, ".agents", "mcp.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected empty standalone ~/.agents/mcp.json to be deleted on uninstall, stat err=%v", err)
	}

	// Antigravity symlink must STILL be a symlink and StitchMCP + theme='dark' must still be intact!
	linfoAfter, err := os.Lstat(symlinkJSON)
	if err != nil || (linfoAfter.Mode()&os.ModeSymlink) == 0 {
		t.Fatalf("expected %s to remain a symlink after uninstall, got mode=%v err=%v", symlinkJSON, linfoAfter.Mode(), err)
	}
	afterJSONBytes, err := os.ReadFile(realSharedJSON)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(afterJSONBytes, &parsed); err != nil {
		t.Fatalf("post-uninstall JSON invalid: %v\n%s", err, string(afterJSONBytes))
	}
	if parsed["theme"] != "dark" {
		t.Fatalf("expected theme='dark' preserved after uninstall, got %v", parsed["theme"])
	}
	servers, _ := parsed["mcpServers"].(map[string]any)
	if _, ok := servers["StitchMCP"]; !ok {
		t.Fatalf("expected StitchMCP preserved after uninstall, got %+v", servers)
	}
	if _, ok := servers["flowsk"]; ok {
		t.Fatalf("expected flowsk removed from mcpServers, got %+v", servers)
	}

	// Codex TOML must retain comments and crwai while having zero flowsk references!
	afterTOMLBytes, err := os.ReadFile(codexTOMLPath)
	if err != nil {
		t.Fatal(err)
	}
	afterTOML := string(afterTOMLBytes)
	if strings.Contains(afterTOML, "flowsk") {
		t.Fatalf("expected flowsk removed from Codex TOML, got:\n%s", afterTOML)
	}
	for _, want := range []string{"# User Codex settings", "[mcp_servers.crwai]", "[mcp_servers.crwai.env]"} {
		if !strings.Contains(afterTOML, want) {
			t.Fatalf("expected %q preserved after TOML uninstall, got:\n%s", want, afterTOML)
		}
	}
}

func TestVersionCompareSemver(t *testing.T) {
	if c, ok := version.CompareSemver("v0.2.0", "0.2.0"); !ok || c != 0 {
		t.Fatalf("expected v0.2.0 == 0.2.0, got c=%d ok=%t", c, ok)
	}
	if c, ok := version.CompareSemver("v0.2.0", "v0.3.0"); !ok || c >= 0 {
		t.Fatalf("expected v0.2.0 < v0.3.0, got c=%d ok=%t", c, ok)
	}
	if c, ok := version.CompareSemver("v1.0.0-rc.1", "v1.0.0"); !ok || c >= 0 {
		t.Fatalf("expected prerelease < stable, got c=%d ok=%t", c, ok)
	}
}
