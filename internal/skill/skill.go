// Package skill embeds the canonical Flowsk `SKILL.md` inside the Go binary,
// computes deterministic SHA-256 integrity hashes, and manages installation and
// health inspection across supported AI coding agent skill directories.
package skill

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// SkillName is the canonical directory and YAML frontmatter identifier of the Flowsk skill.
	SkillName = "flowsk"

	// ProviderClaude identifies Anthropic Claude Code (`~/.claude/skills`).
	ProviderClaude = "claude"

	// ProviderCodex identifies OpenAI Codex CLI/App (`~/.codex/skills`).
	ProviderCodex = "codex"

	// ProviderAntigravity identifies Google Antigravity (`~/.gemini/config/skills`).
	ProviderAntigravity = "antigravity"

	// ProviderGemini identifies Google Gemini CLI (`~/.gemini/skills`).
	ProviderGemini = "gemini"

	// ProviderAgents identifies the cross-agent standard directory (`~/.agents/skills`).
	ProviderAgents = "agents"

	// ProviderCursor identifies Cursor global skills (`~/.cursor/skills`).
	ProviderCursor = "cursor"
)

//go:embed embed/flowsk/SKILL.md
var rawEmbeddedSkill []byte

// normalizeSkillBytes normalizes CRLF line endings to LF so SHA-256 hashes
// remain identical across macOS, Linux, and Windows checkouts.
func normalizeSkillBytes(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
}

// EmbeddedContent returns a defensive copy of the canonical LF-normalized `SKILL.md` bytes.
func EmbeddedContent() []byte {
	norm := normalizeSkillBytes(rawEmbeddedSkill)
	out := make([]byte, len(norm))
	copy(out, norm)
	return out
}

// EmbeddedMarkdown returns the embedded `SKILL.md` document as a UTF-8 string.
func EmbeddedMarkdown() string {
	return string(EmbeddedContent())
}

// ComputeHash returns the lowercase 64-character hex SHA-256 digest of LF-normalized skill bytes.
func ComputeHash(content []byte) string {
	sum := sha256.Sum256(normalizeSkillBytes(content))
	return hex.EncodeToString(sum[:])
}

// EmbeddedHash returns the lowercase hex SHA-256 digest of the binary's embedded `SKILL.md`.
func EmbeddedHash() string {
	return ComputeHash(rawEmbeddedSkill)
}

// ShortHash truncates a hex SHA-256 digest to 12 characters for compact terminal badges.
func ShortHash(hash string) string {
	trimmed := strings.TrimSpace(hash)
	if len(trimmed) <= 12 {
		return trimmed
	}
	return trimmed[:12]
}

// ProviderTarget describes one AI coding agent's global skill installation target
// and optional MCP configuration file location on the user's workstation.
type ProviderTarget struct {
	// ID is the stable CLI flag slug (e.g. "claude", "codex", "antigravity", "gemini", "agents", "cursor").
	ID string `json:"id"`

	// Name is the human-readable provider label displayed in interactive TUI menus and `flowsk doctor`.
	Name string `json:"name"`

	// Description explains where this provider loads global skills from.
	Description string `json:"description"`

	// HomeMarkerDir is the provider's root user directory used to detect whether the tool is present on disk.
	HomeMarkerDir string `json:"home_marker_dir"`

	// LegacyHomeMarkerDir is an optional pre-migration root user directory also checked during provider detection.
	LegacyHomeMarkerDir string `json:"legacy_home_marker_dir,omitempty"`

	// SkillsDir is the provider's global skills parent folder (`<SkillsDir>/flowsk/SKILL.md`).
	SkillsDir string `json:"skills_dir"`

	// SkillFilePath is the exact destination path of `<SkillsDir>/flowsk/SKILL.md`.
	SkillFilePath string `json:"skill_file_path"`

	// LegacySkillFilePath is an optional pre-migration `SKILL.md` path cleaned up during install and uninstall.
	LegacySkillFilePath string `json:"legacy_skill_file_path,omitempty"`

	// MCPConfigPath is the primary user-level MCP configuration file for this provider, if applicable.
	MCPConfigPath string `json:"mcp_config_path,omitempty"`

	// LegacyMCPConfigPath is an optional pre-migration MCP configuration file cleaned up on uninstall when distinct.
	LegacyMCPConfigPath string `json:"legacy_mcp_config_path,omitempty"`

	// MCPFormat identifies the configuration file syntax ("json" or "toml") when MCPConfigPath is non-empty.
	MCPFormat string `json:"mcp_format,omitempty"`
}

// SupportsMCP reports whether this provider exposes a global client MCP configuration file
// managed by `flowsk install`.
func (p ProviderTarget) SupportsMCP() bool {
	return strings.TrimSpace(p.MCPConfigPath) != "" && strings.TrimSpace(p.MCPFormat) != ""
}

// ProviderStatus reports the live installation, SHA-256 integrity, and MCP registration
// state of the Flowsk skill for one AI provider.
type ProviderStatus struct {
	// Target holds the resolved provider metadata and paths.
	Target ProviderTarget `json:"target"`

	// Detected is true when the provider's HomeMarkerDir or SkillsDir exists on the machine.
	Detected bool `json:"detected"`

	// Installed is true when `<SkillsDir>/flowsk/SKILL.md` exists on disk.
	Installed bool `json:"installed"`

	// InstalledHash is the hex SHA-256 of the installed `SKILL.md` (empty when Installed is false).
	InstalledHash string `json:"installed_hash,omitempty"`

	// ExpectedHash is the hex SHA-256 of the current binary's embedded `SKILL.md`.
	ExpectedHash string `json:"expected_hash"`

	// UpToDate is true when Installed is true and InstalledHash == ExpectedHash.
	UpToDate bool `json:"up_to_date"`

	// MCPConfigured reports whether the provider's primary MCP config file exists and mentions "flowsk".
	MCPConfigured bool `json:"mcp_configured"`
}

// InstallResult summarizes an atomic skill write to one ProviderTarget.
type InstallResult struct {
	// Target is the destination provider target.
	Target ProviderTarget `json:"target"`

	// Action is "installed" (new file), "updated" (hash changed), or "unchanged" (hash already matched).
	Action string `json:"action"`

	// SHA256 is the verified SHA-256 digest written on disk.
	SHA256 string `json:"sha256"`

	// PreviousSHA256 holds the prior file's SHA-256 when Action == "updated".
	PreviousSHA256 string `json:"previous_sha256,omitempty"`
}

// UninstallResult summarizes removing `SKILL.md` from one ProviderTarget.
type UninstallResult struct {
	// Target is the inspected provider target.
	Target ProviderTarget `json:"target"`

	// Action is "removed" when `<SkillsDir>/flowsk/SKILL.md` existed and was deleted, or "not_found".
	Action string `json:"action"`
}

// SupportedProviders returns the ordered catalog of supported AI coding agent
// global skill locations rooted under homeDir (resolving `os.UserHomeDir()` when homeDir is empty).
func SupportedProviders(homeDir string) ([]ProviderTarget, error) {
	home := strings.TrimSpace(homeDir)
	if home == "" {
		resolved, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve user home directory: %w", err)
		}
		home = resolved
	}
	home = filepath.Clean(home)

	mk := func(id, name, desc, markerRel, skillsRel, mcpRel, mcpFmt string) ProviderTarget {
		skillsDir := filepath.Join(home, skillsRel)
		mcpPath := ""
		if mcpRel != "" {
			mcpPath = filepath.Join(home, mcpRel)
		}
		return ProviderTarget{
			ID:            id,
			Name:          name,
			Description:   desc,
			HomeMarkerDir: filepath.Join(home, markerRel),
			SkillsDir:     skillsDir,
			SkillFilePath: filepath.Join(skillsDir, SkillName, "SKILL.md"),
			MCPConfigPath: mcpPath,
			MCPFormat:     mcpFmt,
		}
	}

	antigravityTarget := mk(
		ProviderAntigravity,
		"Google Antigravity",
		"~/.gemini/config/skills/flowsk/SKILL.md",
		filepath.Join(".gemini", "config"),
		filepath.Join(".gemini", "config", "skills"),
		filepath.Join(".gemini", "config", "mcp_config.json"),
		MCPFormatJSON,
	)
	antigravityTarget.LegacyHomeMarkerDir = filepath.Join(home, ".gemini", "antigravity")
	antigravityTarget.LegacySkillFilePath = filepath.Join(home, ".gemini", "antigravity", "skills", SkillName, "SKILL.md")
	antigravityTarget.LegacyMCPConfigPath = filepath.Join(home, ".gemini", "antigravity", "mcp_config.json")

	return []ProviderTarget{
		mk(ProviderAgents, "Universal Agents (~/.agents)", "~/.agents/skills/flowsk/SKILL.md", ".agents", filepath.Join(".agents", "skills"), filepath.Join(".agents", "mcp.json"), MCPFormatJSON),
		mk(ProviderClaude, "Claude Code", "~/.claude/skills/flowsk/SKILL.md", ".claude", filepath.Join(".claude", "skills"), ".claude.json", MCPFormatJSON),
		mk(ProviderCodex, "OpenAI Codex", "~/.codex/skills/flowsk/SKILL.md", ".codex", filepath.Join(".codex", "skills"), filepath.Join(".codex", "config.toml"), MCPFormatTOML),
		antigravityTarget,
		mk(ProviderGemini, "Gemini CLI", "~/.gemini/skills/flowsk/SKILL.md", filepath.Join(".gemini", "skills"), filepath.Join(".gemini", "skills"), filepath.Join(".gemini", "settings.json"), MCPFormatJSON),
		mk(ProviderCursor, "Cursor", "~/.cursor/skills/flowsk/SKILL.md", ".cursor", filepath.Join(".cursor", "skills"), filepath.Join(".cursor", "mcp.json"), MCPFormatJSON),
	}, nil
}

// ResolveProvidersByIDs resolves a list of provider slugs (or "all") against SupportedProviders.
func ResolveProvidersByIDs(homeDir string, ids []string) ([]ProviderTarget, error) {
	catalog, err := SupportedProviders(homeDir)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]ProviderTarget, len(catalog))
	for _, p := range catalog {
		byID[p.ID] = p
	}

	var selected []ProviderTarget
	seen := make(map[string]bool, len(catalog))
	for _, raw := range ids {
		for _, part := range strings.Split(raw, ",") {
			token := strings.ToLower(strings.TrimSpace(part))
			if token == "" {
				continue
			}
			if token == "all" {
				return catalog, nil
			}
			target, ok := byID[token]
			if !ok {
				return nil, fmt.Errorf("unknown provider %q (supported: claude, codex, antigravity, gemini, agents, cursor, all)", token)
			}
			if !seen[target.ID] {
				seen[target.ID] = true
				selected = append(selected, target)
			}
		}
	}
	return selected, nil
}

// InspectProvider checks whether target is detected on disk, whether `SKILL.md`
// is installed and matches EmbeddedHash(), and whether its MCP config references `flowsk`.
func InspectProvider(target ProviderTarget) ProviderStatus {
	expected := EmbeddedHash()
	st := ProviderStatus{
		Target:       target,
		ExpectedHash: expected,
	}

	if dirExists(target.HomeMarkerDir) ||
		dirExists(target.LegacyHomeMarkerDir) ||
		dirExists(target.SkillsDir) ||
		(target.LegacySkillFilePath != "" && fileExists(target.LegacySkillFilePath)) ||
		(target.MCPConfigPath != "" && fileExists(target.MCPConfigPath)) {
		st.Detected = true
	}

	if data, err := os.ReadFile(target.SkillFilePath); err == nil {
		st.Detected = true
		st.Installed = true
		st.InstalledHash = ComputeHash(data)
		st.UpToDate = st.InstalledHash == expected
	}

	if target.MCPConfigPath != "" {
		st.MCPConfigured = fileMentionsFlowskMCP(target.MCPConfigPath)
	}
	return st
}

// InspectAll inspects all SupportedProviders under homeDir.
func InspectAll(homeDir string) ([]ProviderStatus, error) {
	catalog, err := SupportedProviders(homeDir)
	if err != nil {
		return nil, err
	}
	out := make([]ProviderStatus, 0, len(catalog))
	for _, target := range catalog {
		out = append(out, InspectProvider(target))
	}
	return out, nil
}

// InstallToProvider atomically writes the embedded `SKILL.md` to `target.SkillFilePath`
// and verifies the persisted SHA-256 digest.
func InstallToProvider(target ProviderTarget) (InstallResult, error) {
	if strings.TrimSpace(target.SkillFilePath) == "" {
		return InstallResult{}, errors.New("provider skill_file_path must not be empty")
	}

	expected := EmbeddedHash()
	payload := EmbeddedContent()

	var prevHash string
	if existing, err := os.ReadFile(target.SkillFilePath); err == nil {
		prevHash = ComputeHash(existing)
		if prevHash == expected {
			removeLegacySkillFile(target.LegacySkillFilePath, target.SkillFilePath)
			return InstallResult{
				Target: target,
				Action: "unchanged",
				SHA256: expected,
			}, nil
		}
	}

	destDir := filepath.Dir(target.SkillFilePath)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return InstallResult{}, fmt.Errorf("create skill directory %s: %w", destDir, err)
	}

	tmpFile, err := os.CreateTemp(destDir, ".flowsk-skill-*.tmp")
	if err != nil {
		return InstallResult{}, fmt.Errorf("create temporary skill file in %s: %w", destDir, err)
	}
	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmpFile.Write(payload); err != nil {
		_ = tmpFile.Close()
		return InstallResult{}, fmt.Errorf("write skill content: %w", err)
	}
	if err := tmpFile.Chmod(0o644); err != nil {
		_ = tmpFile.Close()
		return InstallResult{}, fmt.Errorf("chmod temporary skill file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return InstallResult{}, fmt.Errorf("close temporary skill file: %w", err)
	}

	if err := os.Rename(tmpPath, target.SkillFilePath); err != nil {
		return InstallResult{}, fmt.Errorf("atomically replace %s: %w", target.SkillFilePath, err)
	}
	cleanup = false

	written, err := os.ReadFile(target.SkillFilePath)
	if err != nil {
		return InstallResult{}, fmt.Errorf("verify installed skill %s: %w", target.SkillFilePath, err)
	}
	gotHash := ComputeHash(written)
	if gotHash != expected {
		return InstallResult{}, fmt.Errorf("installed skill hash mismatch: got %s, expected %s", gotHash, expected)
	}

	removeLegacySkillFile(target.LegacySkillFilePath, target.SkillFilePath)

	action := "installed"
	if prevHash != "" {
		action = "updated"
	}
	return InstallResult{
		Target:         target,
		Action:         action,
		SHA256:         gotHash,
		PreviousSHA256: prevHash,
	}, nil
}

// UninstallFromProvider removes `<SkillsDir>/flowsk/SKILL.md` (plus any pre-migration
// `LegacySkillFilePath`) if present and cleans up empty `flowsk` skill folders without
// touching sibling skills.
func UninstallFromProvider(target ProviderTarget) (UninstallResult, error) {
	if strings.TrimSpace(target.SkillFilePath) == "" {
		return UninstallResult{}, errors.New("provider skill_file_path must not be empty")
	}

	removedCanonical := false
	if err := os.Remove(target.SkillFilePath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return UninstallResult{}, fmt.Errorf("remove skill %s: %w", target.SkillFilePath, err)
		}
	} else {
		removedCanonical = true
		// Best-effort removal of `<SkillsDir>/flowsk` only if now empty.
		_ = os.Remove(filepath.Dir(target.SkillFilePath))
	}

	removedLegacy := removeLegacySkillFile(target.LegacySkillFilePath, target.SkillFilePath)

	if !removedCanonical && !removedLegacy {
		return UninstallResult{
			Target: target,
			Action: "not_found",
		}, nil
	}

	return UninstallResult{
		Target: target,
		Action: "removed",
	}, nil
}

// removeLegacySkillFile best-effort deletes a pre-migration SKILL.md path and its
// parent `flowsk` directory when empty, never touching canonicalPath or sibling skills.
func removeLegacySkillFile(legacyPath, canonicalPath string) bool {
	trimmed := strings.TrimSpace(legacyPath)
	if trimmed == "" || filepath.Clean(trimmed) == filepath.Clean(canonicalPath) {
		return false
	}
	if err := os.Remove(trimmed); err != nil {
		return false
	}
	_ = os.Remove(filepath.Dir(trimmed))
	return true
}

func dirExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func fileExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

const maxMCPConfigInspectBytes = 32 * 1024 * 1024

// fileMentionsFlowskMCP performs a bounded (32 MiB) read-only check verifying
// whether path registers the `flowsk` MCP server under JSON `mcpServers.flowsk`
// or TOML `[mcp_servers.flowsk]` without exposing any secret values or triggering
// false positives on unrelated workspace/project history entries.
func fileMentionsFlowskMCP(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() > maxMCPConfigInspectBytes {
		return false
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		var root struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		}
		if err := json.Unmarshal(raw, &root); err != nil {
			return false
		}
		entry, ok := root.MCPServers[SkillName]
		trimmed := bytes.TrimSpace(entry)
		return ok && len(trimmed) > 0 && string(trimmed) != "null"
	case ".toml":
		normalized := strings.ReplaceAll(string(raw), "\r\n", "\n")
		for _, h := range scanTOMLTableHeaders(strings.Split(normalized, "\n")) {
			if isFlowskTOMLHeader(h.Header, false) {
				return true
			}
		}
		return false
	default:
		lower := strings.ToLower(string(raw))
		return strings.Contains(lower, `"flowsk"`) ||
			strings.Contains(lower, `mcp_servers.flowsk`) ||
			strings.Contains(lower, `flowsk mcp serve`)
	}
}
