// File skill_test.go verifies embedded SKILL.md integrity, SHA-256 hash
// determinism, provider detection, atomic skill installation, non-destructive
// JSON/TOML MCP registration (including symlink preservation), and semver comparison.
package skill_test

import (
	"encoding/json"
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

func TestSupportedProvidersOrderUniversalFirst(t *testing.T) {
	providers, err := skill.SupportedProviders(t.TempDir())
	if err != nil {
		t.Fatalf("SupportedProviders: %v", err)
	}
	if len(providers) == 0 || providers[0].ID != skill.ProviderAgents {
		t.Fatalf("expected Universal Agents (%q) at index 0, got %+v", skill.ProviderAgents, providers)
	}
	if providers[0].SupportsMCP() {
		t.Fatal("expected Universal Agents (~/.agents) to have SupportsMCP() == false")
	}
}

func TestInstallAndInspectProviderLifecycle(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte("[mcp_servers.flowsk]\ncommand = \"flowsk\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	targets, err := skill.ResolveProvidersByIDs(home, []string{"codex"})
	if err != nil || len(targets) != 1 {
		t.Fatalf("ResolveProvidersByIDs failed: %v (%d)", err, len(targets))
	}
	codexTarget := targets[0]

	before := skill.InspectProvider(codexTarget)
	if !before.Detected || before.Installed || before.UpToDate || !before.MCPConfigured {
		t.Fatalf("unexpected pre-install status: %+v", before)
	}

	first, err := skill.InstallToProvider(codexTarget)
	if err != nil {
		t.Fatalf("InstallToProvider (first): %v", err)
	}
	if first.Action != "installed" || first.SHA256 != skill.EmbeddedHash() {
		t.Fatalf("unexpected first install result: %+v", first)
	}

	after := skill.InspectProvider(codexTarget)
	if !after.Installed || !after.UpToDate || after.InstalledHash != skill.EmbeddedHash() {
		t.Fatalf("expected up-to-date skill after install, got %+v", after)
	}

	// Idempotent second install should report "unchanged".
	second, err := skill.InstallToProvider(codexTarget)
	if err != nil {
		t.Fatalf("InstallToProvider (second): %v", err)
	}
	if second.Action != "unchanged" {
		t.Fatalf("expected unchanged on second install, got %+v", second)
	}

	// Tamper with installed SKILL.md to simulate an outdated skill version.
	if err := os.WriteFile(codexTarget.SkillFilePath, []byte("# outdated skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tampered := skill.InspectProvider(codexTarget)
	if !tampered.Installed || tampered.UpToDate {
		t.Fatalf("expected outdated detection after tampering, got %+v", tampered)
	}

	updated, err := skill.InstallToProvider(codexTarget)
	if err != nil {
		t.Fatalf("InstallToProvider (update): %v", err)
	}
	if updated.Action != "updated" || updated.PreviousSHA256 == "" || updated.SHA256 != skill.EmbeddedHash() {
		t.Fatalf("expected updated action with previous hash, got %+v", updated)
	}
}

func TestInstallMCPToProviderJSONSymlinkAndTOMLPreservation(t *testing.T) {
	home := t.TempDir()

	// 1. Antigravity JSON config via symbolic link -> ~/.gemini/config/mcp_config.json
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

	// 2. Codex TOML config with comments, other servers, and nested env table.
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	codexTOMLPath := filepath.Join(codexDir, "config.toml")
	initialTOML := "# User Codex settings\nmodel = \"gpt-5\"\n\n[mcp_servers.crwai]\ncommand = \"crwai\"\n\n[mcp_servers.crwai.env]\nMODE = \"prod\"\n"
	if err := os.WriteFile(codexTOMLPath, []byte(initialTOML), 0o600); err != nil {
		t.Fatal(err)
	}

	// Resolve "universal" MCP targets -> must pick both antigravity and codex!
	mcpTargets, err := skill.ResolveMCPProvidersByIDs(home, []string{skill.MCPUniversalID})
	if err != nil {
		t.Fatalf("ResolveMCPProvidersByIDs(universal): %v", err)
	}
	if len(mcpTargets) != 2 {
		t.Fatalf("expected 2 detected MCP targets (codex + antigravity), got %d: %+v", len(mcpTargets), mcpTargets)
	}

	for _, target := range mcpTargets {
		res, err := skill.InstallMCPToProvider(target, "/opt/bin/flowsk")
		if err != nil {
			t.Fatalf("InstallMCPToProvider(%s): %v", target.ID, err)
		}
		if res.Action != "installed" {
			t.Fatalf("expected action=installed for %s, got %q", target.ID, res.Action)
		}

		// Second call with identical command must be idempotent ("unchanged").
		again, err := skill.InstallMCPToProvider(target, "/opt/bin/flowsk")
		if err != nil {
			t.Fatalf("InstallMCPToProvider(%s, second): %v", target.ID, err)
		}
		if again.Action != "unchanged" {
			t.Fatalf("expected action=unchanged on second run for %s, got %q", target.ID, again.Action)
		}

		// Third call with updated binary path must report "updated".
		upd, err := skill.InstallMCPToProvider(target, "/usr/local/bin/flowsk")
		if err != nil {
			t.Fatalf("InstallMCPToProvider(%s, update): %v", target.ID, err)
		}
		if upd.Action != "updated" {
			t.Fatalf("expected action=updated on path change for %s, got %q", target.ID, upd.Action)
		}
	}

	// Verify Antigravity symlink was NOT replaced by a regular file and unrelated keys survived!
	linfo, err := os.Lstat(symlinkJSON)
	if err != nil || (linfo.Mode()&os.ModeSymlink) == 0 {
		t.Fatalf("expected %s to remain a symlink, got mode=%v err=%v", symlinkJSON, linfo.Mode(), err)
	}
	mergedJSONBytes, err := os.ReadFile(realSharedJSON)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(mergedJSONBytes, &parsed); err != nil {
		t.Fatalf("merged JSON invalid: %v\n%s", err, string(mergedJSONBytes))
	}
	if parsed["theme"] != "dark" {
		t.Fatalf("expected top-level theme='dark' preserved, got %v", parsed["theme"])
	}
	servers, _ := parsed["mcpServers"].(map[string]any)
	if _, ok := servers["StitchMCP"]; !ok {
		t.Fatalf("expected existing StitchMCP preserved, got %+v", servers)
	}
	flowskEntry, _ := servers["flowsk"].(map[string]any)
	if flowskEntry["command"] != "/usr/local/bin/flowsk" {
		t.Fatalf("expected updated flowsk command, got %+v", flowskEntry)
	}

	// Verify Codex TOML preserved comments, other sections, and registered [mcp_servers.flowsk].
	mergedTOMLBytes, err := os.ReadFile(codexTOMLPath)
	if err != nil {
		t.Fatal(err)
	}
	mergedTOML := string(mergedTOMLBytes)
	for _, want := range []string{
		"# User Codex settings",
		`[mcp_servers.crwai]`,
		`[mcp_servers.crwai.env]`,
		`[mcp_servers.flowsk]`,
		`command = "/usr/local/bin/flowsk"`,
		`args = ["mcp", "serve", "--transport", "stdio"]`,
	} {
		if !strings.Contains(mergedTOML, want) {
			t.Fatalf("merged TOML missing %q:\n%s", want, mergedTOML)
		}
	}
	if strings.Count(mergedTOML, "[mcp_servers.flowsk]") != 1 {
		t.Fatalf("expected exactly 1 [mcp_servers.flowsk] header, got:\n%s", mergedTOML)
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
