// Package skill embeds Flowsk's two specialized operational AI skills
// (`flowsk-plan` for architecture/planning and `flowsk-dev` for validation/implementation),
// computes deterministic SHA-256 integrity digests, and manages installation,
// legacy `flowsk` skill migration cleanup, and MCP inspection across AI providers.
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
	"slices"
	"strings"
)

const (
	// SkillName is the canonical Flowsk MCP server key (`mcpServers.flowsk` /
	// `[mcp_servers.flowsk]`) and the legacy single-skill folder name cleaned up
	// automatically when installing the dual `flowsk-plan` + `flowsk-dev` skills.
	SkillName = "flowsk"

	// SkillPlanName is the canonical directory and YAML frontmatter identifier
	// of the Flowsk planning & supervisor skill (`<SkillsDir>/flowsk-plan/SKILL.md`).
	SkillPlanName = "flowsk-plan"

	// SkillDevName is the canonical directory and YAML frontmatter identifier
	// of the Flowsk validation & implementation skill (`<SkillsDir>/flowsk-dev/SKILL.md`).
	SkillDevName = "flowsk-dev"

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

//go:embed embed/flowsk-plan/SKILL.md
var rawEmbeddedPlanSkill []byte

//go:embed embed/flowsk-dev/SKILL.md
var rawEmbeddedDevSkill []byte

// normalizeSkillBytes normalizes CRLF line endings to LF so SHA-256 hashes
// remain identical across macOS, Linux, and Windows checkouts.
func normalizeSkillBytes(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
}

func cloneNormalizedBytes(b []byte) []byte {
	norm := normalizeSkillBytes(b)
	out := make([]byte, len(norm))
	copy(out, norm)
	return out
}

// EmbeddedPlanContent returns a defensive copy of the canonical LF-normalized `flowsk-plan/SKILL.md` bytes.
func EmbeddedPlanContent() []byte {
	return cloneNormalizedBytes(rawEmbeddedPlanSkill)
}

// EmbeddedPlanMarkdown returns the embedded `flowsk-plan/SKILL.md` document as a UTF-8 string.
func EmbeddedPlanMarkdown() string {
	return string(EmbeddedPlanContent())
}

// EmbeddedPlanHash returns the lowercase hex SHA-256 digest of the embedded `flowsk-plan/SKILL.md`.
func EmbeddedPlanHash() string {
	return ComputeHash(rawEmbeddedPlanSkill)
}

// EmbeddedDevContent returns a defensive copy of the canonical LF-normalized `flowsk-dev/SKILL.md` bytes.
func EmbeddedDevContent() []byte {
	return cloneNormalizedBytes(rawEmbeddedDevSkill)
}

// EmbeddedDevMarkdown returns the embedded `flowsk-dev/SKILL.md` document as a UTF-8 string.
func EmbeddedDevMarkdown() string {
	return string(EmbeddedDevContent())
}

// EmbeddedDevHash returns the lowercase hex SHA-256 digest of the embedded `flowsk-dev/SKILL.md`.
func EmbeddedDevHash() string {
	return ComputeHash(rawEmbeddedDevSkill)
}

// EmbeddedContent returns the combined LF-normalized bytes of both `flowsk-plan`
// and `flowsk-dev` skill documents separated by a deterministic delimiter.
func EmbeddedContent() []byte {
	var buf bytes.Buffer
	buf.Write(normalizeSkillBytes(rawEmbeddedPlanSkill))
	buf.WriteString("\n---\n")
	buf.Write(normalizeSkillBytes(rawEmbeddedDevSkill))
	return buf.Bytes()
}

// EmbeddedMarkdown returns the combined `flowsk-plan` and `flowsk-dev` skill documents as a UTF-8 string.
func EmbeddedMarkdown() string {
	return string(EmbeddedContent())
}

// ComputeHash returns the lowercase 64-character hex SHA-256 digest of LF-normalized skill bytes.
func ComputeHash(content []byte) string {
	sum := sha256.Sum256(normalizeSkillBytes(content))
	return hex.EncodeToString(sum[:])
}

// ComputeBundleHash computes the canonical lowercase 64-character hex SHA-256
// digest over the ordered (`flowsk-plan`, `flowsk-dev`) skill pair.
func ComputeBundleHash(planContent, devContent []byte) string {
	var buf bytes.Buffer
	buf.WriteString("flowsk-plan:")
	buf.Write(normalizeSkillBytes(planContent))
	buf.WriteString("\n---\nflowsk-dev:")
	buf.Write(normalizeSkillBytes(devContent))
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:])
}

// EmbeddedHash returns the lowercase hex SHA-256 bundle digest of the binary's
// embedded `flowsk-plan` and `flowsk-dev` skills.
func EmbeddedHash() string {
	return ComputeBundleHash(rawEmbeddedPlanSkill, rawEmbeddedDevSkill)
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
// (`flowsk-plan` + `flowsk-dev`) and optional MCP configuration file location.
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

	// SkillsDir is the provider's global skills parent folder (`<SkillsDir>/flowsk-{plan,dev}/SKILL.md`).
	SkillsDir string `json:"skills_dir"`

	// SkillPlanFilePath is the exact destination path of `<SkillsDir>/flowsk-plan/SKILL.md`.
	SkillPlanFilePath string `json:"skill_plan_file_path"`

	// SkillDevFilePath is the exact destination path of `<SkillsDir>/flowsk-dev/SKILL.md`.
	SkillDevFilePath string `json:"skill_dev_file_path"`

	// SkillFilePath is the display/summary path pattern (`<SkillsDir>/flowsk-{plan,dev}/SKILL.md`).
	SkillFilePath string `json:"skill_file_path"`

	// LegacySkillFilePaths lists pre-migration single-skill (`<SkillsDir>/flowsk/SKILL.md`)
	// and legacy provider `SKILL.md` paths cleaned up during install and uninstall.
	LegacySkillFilePaths []string `json:"legacy_skill_file_paths,omitempty"`

	// MCPConfigPath is the primary user-level MCP configuration file for this provider, if applicable.
	MCPConfigPath string `json:"mcp_config_path,omitempty"`

	// LegacyMCPConfigPath is an optional pre-migration MCP configuration file cleaned up on uninstall when distinct.
	LegacyMCPConfigPath string `json:"legacy_mcp_config_path,omitempty"`

	// SecondaryMCPConfigPaths lists additional isolated client profile MCP config files
	// discovered on disk (such as T3 Code's `~/.t3/userdata/providers/antigravity/*/config/mcp_config.json`)
	// that are synchronized alongside MCPConfigPath during install/uninstall and checked for drift in `flowsk doctor`.
	SecondaryMCPConfigPaths []string `json:"secondary_mcp_config_paths,omitempty"`

	// MCPFormat identifies the configuration file syntax ("json" or "toml") when MCPConfigPath is non-empty.
	MCPFormat string `json:"mcp_format,omitempty"`
}

// SupportsMCP reports whether this provider exposes a global client MCP configuration file
// managed by `flowsk install`.
func (p ProviderTarget) SupportsMCP() bool {
	return strings.TrimSpace(p.MCPConfigPath) != "" && strings.TrimSpace(p.MCPFormat) != ""
}

// ProviderStatus reports the live installation, SHA-256 integrity, and MCP registration
// state of the `flowsk-plan` + `flowsk-dev` skill pair for one AI provider.
type ProviderStatus struct {
	// Target holds the resolved provider metadata and paths.
	Target ProviderTarget `json:"target"`

	// Detected is true when the provider's HomeMarkerDir, SkillsDir, or secondary profile directory exists on the machine.
	Detected bool `json:"detected"`

	// Installed is true when `flowsk-plan`, `flowsk-dev`, or a legacy `flowsk` skill exists on disk.
	Installed bool `json:"installed"`

	// InstalledHash is the hex SHA-256 bundle digest of the installed skills (empty when Installed is false).
	InstalledHash string `json:"installed_hash,omitempty"`

	// ExpectedHash is the hex SHA-256 bundle digest of the current binary's embedded `flowsk-plan` + `flowsk-dev` skills.
	ExpectedHash string `json:"expected_hash"`

	// UpToDate is true when both `flowsk-plan` and `flowsk-dev` match the embedded hashes and no obsolete legacy `flowsk` skill remains.
	UpToDate bool `json:"up_to_date"`

	// MCPConfigured reports whether the provider's primary MCP config file exists and mentions "flowsk".
	MCPConfigured bool `json:"mcp_configured"`

	// SyncedMCPPaths lists existing secondary profile MCP config files that register "flowsk".
	SyncedMCPPaths []string `json:"synced_mcp_paths,omitempty"`

	// UnsyncedMCPPaths lists existing secondary profile MCP config files (e.g. T3 Code ACP profiles)
	// where "flowsk" is missing or empty.
	UnsyncedMCPPaths []string `json:"unsynced_mcp_paths,omitempty"`
}

// InstallResult summarizes an atomic `flowsk-plan` + `flowsk-dev` skill installation to one ProviderTarget.
type InstallResult struct {
	// Target is the destination provider target.
	Target ProviderTarget `json:"target"`

	// Action is "installed" (new skill pair), "updated" (hash changed or legacy migrated), or "unchanged" (both hashes matched).
	Action string `json:"action"`

	// SHA256 is the verified bundle SHA-256 digest on disk.
	SHA256 string `json:"sha256"`

	// PreviousSHA256 holds the prior bundle/file SHA-256 when Action == "updated".
	PreviousSHA256 string `json:"previous_sha256,omitempty"`
}

// UninstallResult summarizes removing `flowsk-plan` and `flowsk-dev` (plus legacy `flowsk`) from one ProviderTarget.
type UninstallResult struct {
	// Target is the inspected provider target.
	Target ProviderTarget `json:"target"`

	// Action is "removed" when at least one Flowsk skill file existed and was deleted, or "not_found".
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
		planPath := filepath.Join(skillsDir, SkillPlanName, "SKILL.md")
		devPath := filepath.Join(skillsDir, SkillDevName, "SKILL.md")
		return ProviderTarget{
			ID:                id,
			Name:              name,
			Description:       desc,
			HomeMarkerDir:     filepath.Join(home, markerRel),
			SkillsDir:         skillsDir,
			SkillPlanFilePath: planPath,
			SkillDevFilePath:  devPath,
			SkillFilePath:     filepath.Join(skillsDir, "flowsk-{plan,dev}", "SKILL.md"),
			LegacySkillFilePaths: []string{
				filepath.Join(skillsDir, SkillName, "SKILL.md"),
			},
			MCPConfigPath: mcpPath,
			MCPFormat:     mcpFmt,
		}
	}

	antigravityTarget := mk(
		ProviderAntigravity,
		"Google Antigravity",
		"~/.gemini/config/skills/flowsk-{plan,dev}/SKILL.md",
		filepath.Join(".gemini", "config"),
		filepath.Join(".gemini", "config", "skills"),
		filepath.Join(".gemini", "config", "mcp_config.json"),
		MCPFormatJSON,
	)
	antigravityTarget.LegacyHomeMarkerDir = filepath.Join(home, ".gemini", "antigravity")
	antigravityTarget.LegacySkillFilePaths = append(antigravityTarget.LegacySkillFilePaths,
		filepath.Join(home, ".gemini", "antigravity", "skills", SkillName, "SKILL.md"),
		filepath.Join(home, ".gemini", "antigravity", "skills", SkillPlanName, "SKILL.md"),
		filepath.Join(home, ".gemini", "antigravity", "skills", SkillDevName, "SKILL.md"),
	)
	antigravityTarget.LegacyMCPConfigPath = filepath.Join(home, ".gemini", "antigravity", "mcp_config.json")
	antigravityTarget.SecondaryMCPConfigPaths = discoverAntigravityT3MCPConfigPaths(home)

	return []ProviderTarget{
		mk(ProviderAgents, "Universal Agents (~/.agents)", "~/.agents/skills/flowsk-{plan,dev}/SKILL.md", ".agents", filepath.Join(".agents", "skills"), filepath.Join(".agents", "mcp.json"), MCPFormatJSON),
		mk(ProviderClaude, "Claude Code", "~/.claude/skills/flowsk-{plan,dev}/SKILL.md", ".claude", filepath.Join(".claude", "skills"), ".claude.json", MCPFormatJSON),
		mk(ProviderCodex, "OpenAI Codex", "~/.codex/skills/flowsk-{plan,dev}/SKILL.md", ".codex", filepath.Join(".codex", "skills"), filepath.Join(".codex", "config.toml"), MCPFormatTOML),
		antigravityTarget,
		mk(ProviderGemini, "Gemini CLI", "~/.gemini/skills/flowsk-{plan,dev}/SKILL.md", filepath.Join(".gemini", "skills"), filepath.Join(".gemini", "skills"), filepath.Join(".gemini", "settings.json"), MCPFormatJSON),
		mk(ProviderCursor, "Cursor", "~/.cursor/skills/flowsk-{plan,dev}/SKILL.md", ".cursor", filepath.Join(".cursor", "skills"), filepath.Join(".cursor", "mcp.json"), MCPFormatJSON),
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

func resolveTargetSkillPairPaths(target ProviderTarget) (string, string, error) {
	planPath := strings.TrimSpace(target.SkillPlanFilePath)
	devPath := strings.TrimSpace(target.SkillDevFilePath)
	if planPath == "" && strings.TrimSpace(target.SkillsDir) != "" {
		planPath = filepath.Join(target.SkillsDir, SkillPlanName, "SKILL.md")
	}
	if devPath == "" && strings.TrimSpace(target.SkillsDir) != "" {
		devPath = filepath.Join(target.SkillsDir, SkillDevName, "SKILL.md")
	}
	if planPath == "" || devPath == "" {
		return "", "", errors.New("provider skill_plan_file_path and skill_dev_file_path must not be empty")
	}
	return planPath, devPath, nil
}

// InspectProvider checks whether target is detected on disk, whether both
// `flowsk-plan/SKILL.md` and `flowsk-dev/SKILL.md` are installed and match
// EmbeddedHash(), and whether its MCP config references `flowsk`.
func InspectProvider(target ProviderTarget) ProviderStatus {
	expected := EmbeddedHash()
	st := ProviderStatus{
		Target:       target,
		ExpectedHash: expected,
	}

	planPath, devPath, _ := resolveTargetSkillPairPaths(target)

	legacyInSkillsDir := ""
	if strings.TrimSpace(target.SkillsDir) != "" {
		legacyInSkillsDir = filepath.Join(target.SkillsDir, SkillName, "SKILL.md")
	}

	hasAnyLegacy := false
	for _, lp := range target.LegacySkillFilePaths {
		if lp != "" && fileExists(lp) {
			hasAnyLegacy = true
			break
		}
	}

	if dirExists(target.HomeMarkerDir) ||
		dirExists(target.LegacyHomeMarkerDir) ||
		dirExists(target.SkillsDir) ||
		hasAnyLegacy ||
		(target.MCPConfigPath != "" && fileExists(target.MCPConfigPath)) ||
		len(target.SecondaryMCPConfigPaths) > 0 {
		st.Detected = true
	}

	planData, planErr := os.ReadFile(planPath)
	devData, devErr := os.ReadFile(devPath)
	legacySameDirExists := legacyInSkillsDir != "" && fileExists(legacyInSkillsDir)

	switch {
	case planErr == nil && devErr == nil:
		st.Detected = true
		st.Installed = true
		st.InstalledHash = ComputeBundleHash(planData, devData)
		st.UpToDate = st.InstalledHash == expected && !legacySameDirExists
		if legacySameDirExists && st.InstalledHash == expected {
			// Surface a distinct hash marker when an obsolete single `flowsk/SKILL.md` still coexists.
			st.InstalledHash = ComputeHash(append(planData, byte('!')))
		}
	case planErr == nil:
		st.Detected = true
		st.Installed = true
		st.InstalledHash = ComputeHash(planData)
		st.UpToDate = false
	case devErr == nil:
		st.Detected = true
		st.Installed = true
		st.InstalledHash = ComputeHash(devData)
		st.UpToDate = false
	case legacySameDirExists:
		// Pre-split `<SkillsDir>/flowsk/SKILL.md` inside the canonical skills directory counts as an outdated install so `flowsk install` / `doctor` upgrades it cleanly.
		legacyBytes, _ := os.ReadFile(legacyInSkillsDir)
		st.Detected = true
		st.Installed = true
		st.InstalledHash = ComputeHash(legacyBytes)
		st.UpToDate = false
	}

	if target.MCPConfigPath != "" {
		st.MCPConfigured = fileMentionsFlowskMCP(target.MCPConfigPath)
	}
	for _, secPath := range target.SecondaryMCPConfigPaths {
		if strings.TrimSpace(secPath) == "" {
			continue
		}
		if fileMentionsFlowskMCP(secPath) {
			st.SyncedMCPPaths = append(st.SyncedMCPPaths, secPath)
		} else {
			st.UnsyncedMCPPaths = append(st.UnsyncedMCPPaths, secPath)
		}
	}
	return st
}

// discoverAntigravityT3MCPConfigPaths discovers isolated T3 Code Antigravity ACP profile
// MCP configuration files (`~/.t3/userdata/providers/antigravity/*/config/mcp_config.json`).
func discoverAntigravityT3MCPConfigPaths(home string) []string {
	pattern := filepath.Join(home, ".t3", "userdata", "providers", "antigravity", "*", "config")
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return nil
	}
	slices.Sort(matches)
	out := make([]string, 0, len(matches))
	for _, cfgDir := range matches {
		if dirExists(cfgDir) {
			out = append(out, filepath.Join(cfgDir, "mcp_config.json"))
		}
	}
	return out
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

func writeVerifiedSkillFile(destPath string, payload []byte, expectedHash string) error {
	destDir := filepath.Dir(destPath)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("create skill directory %s: %w", destDir, err)
	}

	tmpFile, err := os.CreateTemp(destDir, ".flowsk-skill-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary skill file in %s: %w", destDir, err)
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
		return fmt.Errorf("write skill content: %w", err)
	}
	if err := tmpFile.Chmod(0o644); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("chmod temporary skill file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temporary skill file: %w", err)
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("atomically replace %s: %w", destPath, err)
	}
	cleanup = false

	written, err := os.ReadFile(destPath)
	if err != nil {
		return fmt.Errorf("verify installed skill %s: %w", destPath, err)
	}
	if gotHash := ComputeHash(written); gotHash != expectedHash {
		return fmt.Errorf("installed skill hash mismatch at %s: got %s, expected %s", destPath, gotHash, expectedHash)
	}
	return nil
}

// InstallToProvider atomically writes both `flowsk-plan/SKILL.md` and `flowsk-dev/SKILL.md`
// to `target`, verifies their persisted SHA-256 digests, and removes any legacy
// single `flowsk/SKILL.md` files cleanly.
func InstallToProvider(target ProviderTarget) (InstallResult, error) {
	planPath, devPath, err := resolveTargetSkillPairPaths(target)
	if err != nil {
		return InstallResult{}, err
	}

	expectedBundle := EmbeddedHash()
	planPayload := EmbeddedPlanContent()
	devPayload := EmbeddedDevContent()
	expectedPlanHash := EmbeddedPlanHash()
	expectedDevHash := EmbeddedDevHash()

	priorStatus := InspectProvider(target)
	prevHash := priorStatus.InstalledHash

	if priorStatus.Installed && priorStatus.UpToDate {
		removeLegacySkillFiles(target.LegacySkillFilePaths, planPath, devPath)
		return InstallResult{
			Target: target,
			Action: "unchanged",
			SHA256: expectedBundle,
		}, nil
	}

	if err := writeVerifiedSkillFile(planPath, planPayload, expectedPlanHash); err != nil {
		return InstallResult{}, err
	}
	if err := writeVerifiedSkillFile(devPath, devPayload, expectedDevHash); err != nil {
		return InstallResult{}, err
	}

	removedLegacy := removeLegacySkillFiles(target.LegacySkillFilePaths, planPath, devPath)

	planWritten, err := os.ReadFile(planPath)
	if err != nil {
		return InstallResult{}, fmt.Errorf("verify installed plan skill %s: %w", planPath, err)
	}
	devWritten, err := os.ReadFile(devPath)
	if err != nil {
		return InstallResult{}, fmt.Errorf("verify installed dev skill %s: %w", devPath, err)
	}
	gotBundle := ComputeBundleHash(planWritten, devWritten)
	if gotBundle != expectedBundle {
		return InstallResult{}, fmt.Errorf("installed skill bundle hash mismatch: got %s, expected %s", gotBundle, expectedBundle)
	}

	action := "installed"
	if priorStatus.Installed || (removedLegacy && prevHash != "") {
		action = "updated"
	}
	return InstallResult{
		Target:         target,
		Action:         action,
		SHA256:         gotBundle,
		PreviousSHA256: prevHash,
	}, nil
}

// UninstallFromProvider removes `<SkillsDir>/flowsk-plan/SKILL.md` and
// `<SkillsDir>/flowsk-dev/SKILL.md` (plus any pre-migration `LegacySkillFilePaths`)
// and cleans up empty Flowsk skill directories without touching sibling skills.
func UninstallFromProvider(target ProviderTarget) (UninstallResult, error) {
	planPath, devPath, err := resolveTargetSkillPairPaths(target)
	if err != nil {
		return UninstallResult{}, err
	}

	removedCanonical := false
	for _, p := range []string{planPath, devPath} {
		if rmErr := os.Remove(p); rmErr != nil {
			if !errors.Is(rmErr, os.ErrNotExist) {
				return UninstallResult{}, fmt.Errorf("remove skill %s: %w", p, rmErr)
			}
		} else {
			removedCanonical = true
			_ = os.Remove(filepath.Dir(p))
		}
	}

	legacyPaths := append([]string(nil), target.LegacySkillFilePaths...)
	if strings.TrimSpace(target.SkillsDir) != "" {
		legacyPaths = append(legacyPaths, filepath.Join(target.SkillsDir, SkillName, "SKILL.md"))
	}
	removedLegacy := removeLegacySkillFiles(legacyPaths, planPath, devPath)

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

// removeLegacySkillFiles best-effort deletes obsolete SKILL.md paths and their
// parent directories when empty, never touching canonicalPaths or sibling skills.
func removeLegacySkillFiles(legacyPaths []string, canonicalPaths ...string) bool {
	protected := make(map[string]bool, len(canonicalPaths))
	for _, cp := range canonicalPaths {
		if trimmed := strings.TrimSpace(cp); trimmed != "" {
			protected[filepath.Clean(trimmed)] = true
		}
	}
	removedAny := false
	for _, raw := range legacyPaths {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		cleaned := filepath.Clean(trimmed)
		if protected[cleaned] {
			continue
		}
		if err := os.Remove(cleaned); err == nil {
			removedAny = true
			_ = os.Remove(filepath.Dir(cleaned))
		}
	}
	return removedAny
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
