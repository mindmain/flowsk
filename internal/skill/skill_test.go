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
	// 2. Antigravity JSON config (~/.gemini/config/mcp_config.json) via symbolic link -> ~/dotfiles/mcp_config.json
	sharedDir := filepath.Join(home, "dotfiles")
	agConfigDir := filepath.Join(home, ".gemini", "config")
	if err := os.MkdirAll(sharedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(agConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realSharedJSON := filepath.Join(sharedDir, "mcp_config.json")
	initialJSON := `{"theme":"dark","mcpServers":{"StitchMCP":{"command":"stitch","args":["serve"]}}}` + "\n"
	if err := os.WriteFile(realSharedJSON, []byte(initialJSON), 0o640); err != nil {
		t.Fatal(err)
	}
	symlinkJSON := filepath.Join(agConfigDir, "mcp_config.json")
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
	if c, ok := version.CompareSemver("v1.0.0-alpha.2", "v1.0.0-alpha.10"); !ok || c >= 0 {
		t.Fatalf("expected v1.0.0-alpha.2 < v1.0.0-alpha.10, got c=%d ok=%t", c, ok)
	}
}

func TestTOMLPreservesUserKeysAndIgnoresMultilineFakeHeaders(t *testing.T) {
	home := t.TempDir()
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tomlPath := filepath.Join(codexDir, "config.toml")
	initial := strings.Join([]string{
		`model = "gpt-5"`,
		`instructions = """`,
		`[mcp_servers.flowsk]`,
		`command = "fake-inside-multiline"`,
		`[mcp_servers.fake_after]`,
		`"""`,
		``,
		`[mcp_servers.flowsk]`,
		`# Keep user comments and extra fields intact`,
		`command = "/old/bin/flowsk"`,
		`args = [`,
		`  "mcp",`,
		`  "serve"`,
		`]`,
		`cwd = "/Users/dev/work"`,
		`enabled = true`,
		`startup_timeout_sec = 45`,
		`env = { FLOWSK_WORKSPACE = "eng" }`,
		``,
		`[mcp_servers.other]`,
		`command = "other-mcp"`,
		``,
	}, "\n")
	if err := os.WriteFile(tomlPath, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}

	targets, err := skill.ResolveMCPProvidersByIDs(home, []string{"codex"})
	if err != nil || len(targets) != 1 {
		t.Fatalf("ResolveMCPProvidersByIDs(codex): %v", err)
	}

	upd, err := skill.InstallMCPToProvider(targets[0], "/new/bin/flowsk")
	if err != nil || upd.Action != "updated" {
		t.Fatalf("expected updated action on TOML upsert, got %+v err=%v", upd, err)
	}

	unchanged, err := skill.InstallMCPToProvider(targets[0], "/new/bin/flowsk")
	if err != nil || unchanged.Action != "unchanged" {
		t.Fatalf("expected unchanged on second identical TOML upsert, got %+v err=%v", unchanged, err)
	}

	afterBytes, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	after := string(afterBytes)
	for _, want := range []string{
		`command = "fake-inside-multiline"`,
		`# Keep user comments and extra fields intact`,
		`command = "/new/bin/flowsk"`,
		`args = ["mcp", "serve", "--transport", "stdio"]`,
		`cwd = "/Users/dev/work"`,
		`enabled = true`,
		`startup_timeout_sec = 45`,
		`env = { FLOWSK_WORKSPACE = "eng" }`,
		`[mcp_servers.other]`,
	} {
		if !strings.Contains(after, want) {
			t.Fatalf("expected %q preserved in updated TOML, got:\n%s", want, after)
		}
	}
	if strings.Contains(after, `/old/bin/flowsk`) {
		t.Fatalf("expected /old/bin/flowsk to be replaced, got:\n%s", after)
	}
}

func TestMultiHopDanglingSymlinkChainAndAccurateMCPDetection(t *testing.T) {
	home := t.TempDir()

	// 1. 4-hop dangling symlink chain for ~/.cursor/mcp.json -> link1 -> link2 -> link3 -> real/mcp.json
	cursorDir := filepath.Join(home, ".cursor")
	linksDir := filepath.Join(home, "links")
	realTarget := filepath.Join(home, "dotfiles", "cursor-mcp.json")
	if err := os.MkdirAll(cursorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(linksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	l1 := filepath.Join(linksDir, "hop1.json")
	l2 := filepath.Join(linksDir, "hop2.json")
	l3 := filepath.Join(linksDir, "hop3.json")
	if err := os.Symlink(realTarget, l3); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(l3, l2); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(l2, l1); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(l1, filepath.Join(cursorDir, "mcp.json")); err != nil {
		t.Fatal(err)
	}

	cursorTargets, err := skill.ResolveMCPProvidersByIDs(home, []string{"cursor"})
	if err != nil || len(cursorTargets) != 1 {
		t.Fatalf("ResolveMCPProvidersByIDs(cursor): %v", err)
	}
	res, err := skill.InstallMCPToProvider(cursorTargets[0], "/usr/local/bin/flowsk")
	if err != nil || res.Action != "installed" {
		t.Fatalf("InstallMCPToProvider across 4-hop dangling symlink failed: res=%+v err=%v", res, err)
	}
	if _, statErr := os.Stat(realTarget); statErr != nil {
		t.Fatalf("expected real symlink destination %s to be created: %v", realTarget, statErr)
	}
	if linfo, lErr := os.Lstat(filepath.Join(cursorDir, "mcp.json")); lErr != nil || linfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected cursor/mcp.json to remain a symlink, err=%v", lErr)
	}

	// 2. ~/.claude.json with project history mentioning "flowsk" MUST NOT report MCPConfigured=true.
	claudeJSONPath := filepath.Join(home, ".claude.json")
	falsePosPayload := `{"projects":{"/Users/dev/github.com/mindmain/flowsk":{"lastPrompt":"run flowsk mcp serve"}},"mcpServers":{"other":{"command":"other"}}}`
	if err := os.WriteFile(claudeJSONPath, []byte(falsePosPayload), 0o600); err != nil {
		t.Fatal(err)
	}
	claudeTargets, err := skill.ResolveProvidersByIDs(home, []string{"claude"})
	if err != nil || len(claudeTargets) != 1 {
		t.Fatal(err)
	}
	if st := skill.InspectProvider(claudeTargets[0]); st.MCPConfigured {
		t.Fatalf("expected MCPConfigured=false when flowsk only appears in projects history, got %+v", st)
	}

	// 3. Large (>4 MiB, <32 MiB) ~/.claude.json WITH mcpServers.flowsk MUST report MCPConfigured=true.
	padding := strings.Repeat("x", 5*1024*1024)
	largePayload := `{"historyPadding":"` + padding + `","mcpServers":{"flowsk":{"command":"flowsk","args":["mcp","serve","--transport","stdio"]}}}`
	if err := os.WriteFile(claudeJSONPath, []byte(largePayload), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := skill.InspectProvider(claudeTargets[0]); !st.MCPConfigured {
		t.Fatalf("expected MCPConfigured=true on 5 MiB ~/.claude.json with mcpServers.flowsk, got %+v", st)
	}

	// 4. TOML with [mcp_servers.flowsk] inside multiline string MUST report MCPConfigured=false.
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeTOML := "notes = '''\n[mcp_servers.flowsk]\ncommand = \"flowsk\"\n'''\n"
	if err := os.WriteFile(filepath.Join(codexDir, "config.toml"), []byte(fakeTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	codexTargets, _ := skill.ResolveProvidersByIDs(home, []string{"codex"})
	if st := skill.InspectProvider(codexTargets[0]); st.MCPConfigured {
		t.Fatalf("expected MCPConfigured=false when [mcp_servers.flowsk] is inside ''' multiline string")
	}
}

func TestAntigravityCanonicalConfigPathsAndLegacySkillCleanup(t *testing.T) {
	home := t.TempDir()

	// Seed legacy ~/.gemini/antigravity/skills/flowsk/SKILL.md and a sibling skill.
	legacyFlowskDir := filepath.Join(home, ".gemini", "antigravity", "skills", "flowsk")
	legacySiblingDir := filepath.Join(home, ".gemini", "antigravity", "skills", "other-skill")
	if err := os.MkdirAll(legacyFlowskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(legacySiblingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacySkillPath := filepath.Join(legacyFlowskDir, "SKILL.md")
	if err := os.WriteFile(legacySkillPath, []byte("# legacy antigravity skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacySiblingDir, "SKILL.md"), []byte("# sibling\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	targets, err := skill.ResolveProvidersByIDs(home, []string{"antigravity"})
	if err != nil || len(targets) != 1 {
		t.Fatalf("ResolveProvidersByIDs(antigravity): %v", err)
	}
	ag := targets[0]

	wantSkillPath := filepath.Join(home, ".gemini", "config", "skills", "flowsk", "SKILL.md")
	wantMCPPath := filepath.Join(home, ".gemini", "config", "mcp_config.json")
	if ag.SkillFilePath != wantSkillPath || ag.MCPConfigPath != wantMCPPath {
		t.Fatalf("expected canonical ~/.gemini/config paths (%s, %s), got (%s, %s)",
			wantSkillPath, wantMCPPath, ag.SkillFilePath, ag.MCPConfigPath)
	}

	// Pre-migration ~/.gemini/antigravity must still mark Antigravity as Detected=true,
	// while Installed=false because canonical ~/.gemini/config/skills/flowsk/SKILL.md is missing.
	pre := skill.InspectProvider(ag)
	if !pre.Detected || pre.Installed {
		t.Fatalf("expected Detected=true and Installed=false before canonical install, got %+v", pre)
	}

	// Installing writes ~/.gemini/config/skills/flowsk/SKILL.md AND removes legacy flowsk/SKILL.md!
	res, err := skill.InstallToProvider(ag)
	if err != nil || res.Action != "installed" {
		t.Fatalf("InstallToProvider(antigravity): res=%+v err=%v", res, err)
	}
	if _, err := os.Stat(wantSkillPath); err != nil {
		t.Fatalf("expected canonical skill installed at %s: %v", wantSkillPath, err)
	}
	if _, err := os.Stat(legacySkillPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected legacy skill %s to be cleaned up, stat err=%v", legacySkillPath, err)
	}
	if _, err := os.Stat(filepath.Join(legacySiblingDir, "SKILL.md")); err != nil {
		t.Fatalf("expected legacy sibling skill to remain untouched: %v", err)
	}

	post := skill.InspectProvider(ag)
	if !post.Installed || !post.UpToDate {
		t.Fatalf("expected Installed=true and UpToDate=true after install, got %+v", post)
	}
}

func TestAntigravityT3CodeSecondaryMCPSyncAndDriftDetection(t *testing.T) {
	home := t.TempDir()

	// Simulate T3 Code's isolated Antigravity ACP profile folder with a 0-byte mcp_config.json
	// while skills is symlinked to ~/.gemini/config/skills.
	agSkillsDir := filepath.Join(home, ".gemini", "config", "skills")
	t3ConfigDir := filepath.Join(home, ".t3", "userdata", "providers", "antigravity", "ac0a3dfd6dddb20962ce", "config")
	if err := os.MkdirAll(agSkillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(t3ConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(agSkillsDir, filepath.Join(t3ConfigDir, "skills")); err != nil {
		t.Fatal(err)
	}
	t3MCPPath := filepath.Join(t3ConfigDir, "mcp_config.json")
	if err := os.WriteFile(t3MCPPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// Seed primary ~/.gemini/config/mcp_config.json with flowsk already configured, leaving T3 0-byte.
	primaryMCPPath := filepath.Join(home, ".gemini", "config", "mcp_config.json")
	primaryInitial := `{"mcpServers":{"flowsk":{"command":"/usr/local/bin/flowsk","args":["mcp","serve","--transport","stdio"]}}}` + "\n"
	if err := os.WriteFile(primaryMCPPath, []byte(primaryInitial), 0o600); err != nil {
		t.Fatal(err)
	}

	targets, err := skill.ResolveMCPProvidersByIDs(home, []string{"antigravity"})
	if err != nil || len(targets) != 1 {
		t.Fatalf("ResolveMCPProvidersByIDs(antigravity): %v", err)
	}
	ag := targets[0]
	if len(ag.SecondaryMCPConfigPaths) != 1 || ag.SecondaryMCPConfigPaths[0] != t3MCPPath {
		t.Fatalf("expected T3 secondary MCP path %q discovered, got %+v", t3MCPPath, ag.SecondaryMCPConfigPaths)
	}

	// Before syncing, InspectProvider must report MCPConfigured=true AND UnsyncedMCPPaths=[t3MCPPath].
	driftStatus := skill.InspectProvider(ag)
	if !driftStatus.MCPConfigured || len(driftStatus.UnsyncedMCPPaths) != 1 || driftStatus.UnsyncedMCPPaths[0] != t3MCPPath {
		t.Fatalf("expected drift detected on T3 profile MCP config, got %+v", driftStatus)
	}

	// InstallMCPToProvider synchronizes the 0-byte T3 mcp_config.json and reports Action="updated".
	instRes, err := skill.InstallMCPToProvider(ag, "/usr/local/bin/flowsk")
	if err != nil {
		t.Fatalf("InstallMCPToProvider(antigravity): %v", err)
	}
	if instRes.Action != "updated" || len(instRes.SyncedSecondaryPaths) != 1 || instRes.SyncedSecondaryPaths[0] != t3MCPPath {
		t.Fatalf("expected Action=updated and SyncedSecondaryPaths=[%s], got %+v", t3MCPPath, instRes)
	}

	syncedStatus := skill.InspectProvider(ag)
	if !syncedStatus.MCPConfigured || len(syncedStatus.UnsyncedMCPPaths) != 0 || len(syncedStatus.SyncedMCPPaths) != 1 {
		t.Fatalf("expected 0 unsynced paths after install, got %+v", syncedStatus)
	}

	// UninstallMCPFromProvider removes flowsk from both primary and T3 profile while keeping T3 mcp_config.json valid JSON on disk.
	unRes, err := skill.UninstallMCPFromProvider(ag)
	if err != nil || unRes.Action != "removed" {
		t.Fatalf("UninstallMCPFromProvider(antigravity): res=%+v err=%v", unRes, err)
	}
	afterUninstall := skill.InspectProvider(ag)
	if afterUninstall.MCPConfigured || len(afterUninstall.SyncedMCPPaths) != 0 {
		t.Fatalf("expected flowsk removed from primary and T3 profile configs, got %+v", afterUninstall)
	}
	rawT3After, err := os.ReadFile(t3MCPPath)
	if err != nil {
		t.Fatalf("expected T3 mcp_config.json file preserved on disk: %v", err)
	}
	var parsedT3 map[string]any
	if err := json.Unmarshal(rawT3After, &parsedT3); err != nil {
		t.Fatalf("expected T3 mcp_config.json to remain valid JSON after uninstall, got %q: %v", string(rawT3After), err)
	}
}
