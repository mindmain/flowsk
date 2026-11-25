// File skill_test.go verifies embedded SKILL.md integrity, SHA-256 hash
// determinism, provider detection, atomic installation, and outdated hash detection.
package skill_test

import (
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
