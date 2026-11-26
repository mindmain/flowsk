// File mcp.go implements non-destructive, symlink-preserving Model Context Protocol
// (`flowsk mcp serve --transport stdio`) installation and uninstallation writers across
// supported JSON (`mcpServers.flowsk`, including `~/.agents/mcp.json`) and TOML
// (`[mcp_servers.flowsk]`) AI client configuration files.
package skill

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const (
	// MCPFormatJSON identifies JSON configuration files storing servers under `mcpServers`.
	MCPFormatJSON = "json"

	// MCPFormatTOML identifies TOML configuration files storing servers under `[mcp_servers.<name>]`.
	MCPFormatTOML = "toml"
)

// DefaultMCPArgs returns a fresh copy of the canonical stdio arguments passed to `flowsk`.
func DefaultMCPArgs() []string {
	return []string{"mcp", "serve", "--transport", "stdio"}
}

// ResolveMCPCommand resolves the executable command string written into MCP client
// configurations. When customCommand is empty, it prefers `exec.LookPath("flowsk")`
// so macOS/Linux desktop GUI clients with minimal PATH environments still locate
// `~/go/bin/flowsk`, falling back to `os.Executable()` (when named `flowsk`) or `"flowsk"`.
func ResolveMCPCommand(customCommand string) string {
	if trimmed := strings.TrimSpace(customCommand); trimmed != "" {
		return trimmed
	}
	if resolved, err := exec.LookPath("flowsk"); err == nil && strings.TrimSpace(resolved) != "" {
		return filepath.Clean(resolved)
	}
	if exe, err := os.Executable(); err == nil && strings.TrimSpace(exe) != "" {
		base := strings.TrimSuffix(filepath.Base(exe), ".exe")
		if base == "flowsk" {
			return filepath.Clean(exe)
		}
	}
	return "flowsk"
}

// MCPInstallResult summarizes an atomic MCP server registration write for one ProviderTarget.
type MCPInstallResult struct {
	// Target is the destination AI client provider metadata.
	Target ProviderTarget `json:"target"`

	// ConfigPath is the configured MCP file path (symlink path preserved for display).
	ConfigPath string `json:"config_path"`

	// ResolvedPath is the real underlying file path written on disk after symlink resolution.
	ResolvedPath string `json:"resolved_path"`

	// Command is the executable command configured for `flowsk`.
	Command string `json:"command"`

	// Args lists the CLI arguments configured for `flowsk`.
	Args []string `json:"args"`

	// Action is "installed" (newly added), "updated" (command/args refreshed), or "unchanged".
	Action string `json:"action"`
}

// MCPUninstallResult summarizes removing the `flowsk` MCP server from one ProviderTarget.
type MCPUninstallResult struct {
	// Target is the inspected AI client provider metadata.
	Target ProviderTarget `json:"target"`

	// ConfigPath is the configured MCP file path.
	ConfigPath string `json:"config_path"`

	// ResolvedPath is the real underlying file path after symlink resolution.
	ResolvedPath string `json:"resolved_path"`

	// Action is "removed" when `flowsk` was present and removed, or "not_found".
	Action string `json:"action"`
}

// ResolveMCPProvidersByIDs resolves MCP-capable provider slugs (`agents` / `universal`,
// `claude`, `codex`, `antigravity`, `gemini`, `cursor`, or `all`) against SupportedProviders.
func ResolveMCPProvidersByIDs(homeDir string, ids []string) ([]ProviderTarget, error) {
	catalog, err := SupportedProviders(homeDir)
	if err != nil {
		return nil, err
	}

	byID := make(map[string]ProviderTarget, len(catalog))
	var allMCP []ProviderTarget
	for _, p := range catalog {
		if p.SupportsMCP() {
			byID[p.ID] = p
			allMCP = append(allMCP, p)
		}
	}

	var selected []ProviderTarget
	seen := make(map[string]bool, len(allMCP))
	addUnique := func(t ProviderTarget) {
		if !seen[t.ID] {
			seen[t.ID] = true
			selected = append(selected, t)
		}
	}

	for _, raw := range ids {
		for _, part := range strings.Split(raw, ",") {
			token := strings.ToLower(strings.TrimSpace(part))
			if token == "" {
				continue
			}
			switch token {
			case "all":
				return allMCP, nil
			case "universal":
				token = ProviderAgents
			}
			target, ok := byID[token]
			if !ok {
				return nil, fmt.Errorf("unknown MCP provider %q (supported: agents, claude, codex, antigravity, gemini, cursor, all)", token)
			}
			addUnique(target)
		}
	}
	return selected, nil
}

// InstallMCPToProvider non-destructively registers or updates the `flowsk` stdio MCP
// server inside `target.MCPConfigPath`, preserving all unrelated keys, comments (in TOML),
// file permissions, and symbolic links.
func InstallMCPToProvider(target ProviderTarget, customCommand string) (MCPInstallResult, error) {
	if !target.SupportsMCP() {
		return MCPInstallResult{}, fmt.Errorf("provider %q does not have a managed MCP config file", target.ID)
	}

	cmd := ResolveMCPCommand(customCommand)
	args := DefaultMCPArgs()

	realPath, _, err := resolveWritableTargetPath(target.MCPConfigPath)
	if err != nil {
		return MCPInstallResult{}, fmt.Errorf("resolve MCP config path %s: %w", target.MCPConfigPath, err)
	}

	var action string
	switch target.MCPFormat {
	case MCPFormatJSON:
		action, err = upsertJSONMCPServer(realPath, cmd, args)
	case MCPFormatTOML:
		action, err = upsertTOMLMCPServer(realPath, cmd, args)
	default:
		return MCPInstallResult{}, fmt.Errorf("unsupported MCP format %q for provider %q", target.MCPFormat, target.ID)
	}
	if err != nil {
		return MCPInstallResult{}, err
	}

	return MCPInstallResult{
		Target:       target,
		ConfigPath:   target.MCPConfigPath,
		ResolvedPath: realPath,
		Command:      cmd,
		Args:         args,
		Action:       action,
	}, nil
}

// UninstallMCPFromProvider non-destructively removes the `flowsk` MCP server entry
// from `target.MCPConfigPath`, preserving all other MCP servers, unrelated settings,
// comments, permissions, and symbolic links. When a non-symlink config file becomes
// completely empty (e.g. a standalone `mcp.json` created solely for `flowsk`), the
// empty file is removed cleanly.
func UninstallMCPFromProvider(target ProviderTarget) (MCPUninstallResult, error) {
	if !target.SupportsMCP() {
		return MCPUninstallResult{}, fmt.Errorf("provider %q does not have a managed MCP config file", target.ID)
	}

	realPath, isSymlink, err := resolveWritableTargetPath(target.MCPConfigPath)
	if err != nil {
		return MCPUninstallResult{}, fmt.Errorf("resolve MCP config path %s: %w", target.MCPConfigPath, err)
	}

	var action string
	switch target.MCPFormat {
	case MCPFormatJSON:
		action, err = removeJSONMCPServer(realPath, isSymlink)
	case MCPFormatTOML:
		action, err = removeTOMLMCPServer(realPath, isSymlink)
	default:
		return MCPUninstallResult{}, fmt.Errorf("unsupported MCP format %q for provider %q", target.MCPFormat, target.ID)
	}
	if err != nil {
		return MCPUninstallResult{}, err
	}

	return MCPUninstallResult{
		Target:       target,
		ConfigPath:   target.MCPConfigPath,
		ResolvedPath: realPath,
		Action:       action,
	}, nil
}

type jsonMCPServerSpec struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env,omitempty"`
}

func upsertJSONMCPServer(realPath, cmd string, args []string) (string, error) {
	var raw []byte
	if existing, err := os.ReadFile(realPath); err == nil {
		raw = existing
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read %s: %w", realPath, err)
	}

	root := make(map[string]json.RawMessage)
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &root); err != nil {
			return "", fmt.Errorf("refusing to modify malformed JSON in %s: %w", realPath, err)
		}
	}

	servers := make(map[string]json.RawMessage)
	if existingServersRaw, ok := root["mcpServers"]; ok && len(bytes.TrimSpace(existingServersRaw)) > 0 && string(bytes.TrimSpace(existingServersRaw)) != "null" {
		if err := json.Unmarshal(existingServersRaw, &servers); err != nil {
			return "", fmt.Errorf("refusing to modify non-object mcpServers in %s: %w", realPath, err)
		}
	}

	desired := jsonMCPServerSpec{
		Command: cmd,
		Args:    args,
	}

	action := "installed"
	if prevRaw, exists := servers[SkillName]; exists && len(bytes.TrimSpace(prevRaw)) > 0 {
		var prevMap map[string]json.RawMessage
		if err := json.Unmarshal(prevRaw, &prevMap); err == nil {
			var prevSpec jsonMCPServerSpec
			if err := json.Unmarshal(prevRaw, &prevSpec); err == nil {
				if len(prevSpec.Env) > 0 {
					desired.Env = prevSpec.Env
				}
				if prevSpec.Command == desired.Command && slices.Equal(prevSpec.Args, desired.Args) {
					return "unchanged", nil
				}
			}
			cmdRaw, _ := json.Marshal(desired.Command)
			argsRaw, _ := json.Marshal(desired.Args)
			prevMap["command"] = cmdRaw
			prevMap["args"] = argsRaw
			mergedEntry, err := json.Marshal(prevMap)
			if err != nil {
				return "", fmt.Errorf("marshal mcpServers.flowsk for %s: %w", realPath, err)
			}
			servers[SkillName] = mergedEntry
			action = "updated"
		} else {
			entryRaw, err := json.Marshal(desired)
			if err != nil {
				return "", fmt.Errorf("marshal mcpServers.flowsk for %s: %w", realPath, err)
			}
			servers[SkillName] = entryRaw
			action = "updated"
		}
	} else {
		entryRaw, err := json.Marshal(desired)
		if err != nil {
			return "", fmt.Errorf("marshal mcpServers.flowsk for %s: %w", realPath, err)
		}
		servers[SkillName] = entryRaw
	}

	serversBytes, err := json.Marshal(servers)
	if err != nil {
		return "", fmt.Errorf("marshal mcpServers map for %s: %w", realPath, err)
	}
	root["mcpServers"] = serversBytes

	formatted, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return "", fmt.Errorf("format JSON for %s: %w", realPath, err)
	}
	formatted = append(formatted, '\n')

	if err := atomicWriteConfigFile(realPath, formatted, 0o600); err != nil {
		return "", err
	}
	return action, nil
}

func removeJSONMCPServer(realPath string, isSymlink bool) (string, error) {
	raw, err := os.ReadFile(realPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "not_found", nil
		}
		return "", fmt.Errorf("read %s: %w", realPath, err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return "not_found", nil
	}

	root := make(map[string]json.RawMessage)
	if err := json.Unmarshal(raw, &root); err != nil {
		return "", fmt.Errorf("refusing to modify malformed JSON in %s: %w", realPath, err)
	}

	existingServersRaw, ok := root["mcpServers"]
	if !ok || len(bytes.TrimSpace(existingServersRaw)) == 0 || string(bytes.TrimSpace(existingServersRaw)) == "null" {
		return "not_found", nil
	}

	servers := make(map[string]json.RawMessage)
	if err := json.Unmarshal(existingServersRaw, &servers); err != nil {
		return "", fmt.Errorf("refusing to modify non-object mcpServers in %s: %w", realPath, err)
	}

	if _, exists := servers[SkillName]; !exists {
		return "not_found", nil
	}
	delete(servers, SkillName)

	// If no other MCP servers and no other top-level keys remain in a standalone non-symlink file, remove it cleanly.
	if len(servers) == 0 && len(root) == 1 && !isSymlink {
		if err := os.Remove(realPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("remove empty MCP config %s: %w", realPath, err)
		}
		return "removed", nil
	}

	serversBytes, err := json.Marshal(servers)
	if err != nil {
		return "", fmt.Errorf("marshal mcpServers map for %s: %w", realPath, err)
	}
	root["mcpServers"] = serversBytes

	formatted, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return "", fmt.Errorf("format JSON for %s: %w", realPath, err)
	}
	formatted = append(formatted, '\n')

	if err := atomicWriteConfigFile(realPath, formatted, 0o600); err != nil {
		return "", err
	}
	return "removed", nil
}

func upsertTOMLMCPServer(realPath, cmd string, args []string) (string, error) {
	var raw []byte
	if existing, err := os.ReadFile(realPath); err == nil {
		raw = existing
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read %s: %w", realPath, err)
	}

	quotedArgs := make([]string, len(args))
	for i, a := range args {
		quotedArgs[i] = strconv.Quote(a)
	}
	canonicalBlock := fmt.Sprintf("[mcp_servers.%s]\ncommand = %s\nargs = [%s]",
		SkillName, strconv.Quote(cmd), strings.Join(quotedArgs, ", "))

	normalized := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if strings.TrimSpace(normalized) == "" {
		if err := atomicWriteConfigFile(realPath, []byte(canonicalBlock+"\n"), 0o600); err != nil {
			return "", err
		}
		return "installed", nil
	}

	lines := strings.Split(normalized, "\n")
	startIdx := -1
	endIdx := len(lines)

	for i, line := range lines {
		hdr, isHdr := parseTOMLTableHeader(line)
		if !isHdr {
			continue
		}
		if startIdx == -1 {
			if isFlowskTOMLHeader(hdr, false) {
				startIdx = i
			}
		} else {
			endIdx = i
			break
		}
	}

	if startIdx == -1 {
		trimmedRight := strings.TrimRight(normalized, "\n")
		updatedContent := trimmedRight + "\n\n" + canonicalBlock + "\n"
		if err := atomicWriteConfigFile(realPath, []byte(updatedContent), 0o600); err != nil {
			return "", err
		}
		return "installed", nil
	}

	bodyEnd := endIdx
	for bodyEnd > startIdx+1 && strings.TrimSpace(lines[bodyEnd-1]) == "" {
		bodyEnd--
	}

	existingBlock := strings.Join(lines[startIdx:bodyEnd], "\n")
	if strings.TrimSpace(existingBlock) == canonicalBlock {
		return "unchanged", nil
	}

	var outLines []string
	outLines = append(outLines, lines[:startIdx]...)
	outLines = append(outLines, strings.Split(canonicalBlock, "\n")...)
	outLines = append(outLines, lines[bodyEnd:]...)

	updatedContent := strings.TrimRight(strings.Join(outLines, "\n"), "\n") + "\n"
	if err := atomicWriteConfigFile(realPath, []byte(updatedContent), 0o600); err != nil {
		return "", err
	}
	return "updated", nil
}

func removeTOMLMCPServer(realPath string, isSymlink bool) (string, error) {
	raw, err := os.ReadFile(realPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "not_found", nil
		}
		return "", fmt.Errorf("read %s: %w", realPath, err)
	}

	normalized := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if strings.TrimSpace(normalized) == "" {
		return "not_found", nil
	}

	lines := strings.Split(normalized, "\n")
	var kept []string
	skipping := false
	removedAny := false

	for _, line := range lines {
		if hdr, isHdr := parseTOMLTableHeader(line); isHdr {
			if isFlowskTOMLHeader(hdr, true) {
				skipping = true
				removedAny = true
				continue
			}
			skipping = false
		}
		if !skipping {
			kept = append(kept, line)
		}
	}

	if !removedAny {
		return "not_found", nil
	}

	// Collapse consecutive blank lines left behind by removed sections.
	var compact []string
	prevBlank := false
	for _, line := range kept {
		blank := strings.TrimSpace(line) == ""
		if blank && prevBlank {
			continue
		}
		compact = append(compact, line)
		prevBlank = blank
	}

	finalStr := strings.TrimSpace(strings.Join(compact, "\n"))
	if finalStr == "" && !isSymlink {
		if err := os.Remove(realPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("remove empty TOML config %s: %w", realPath, err)
		}
		return "removed", nil
	}

	var outBytes []byte
	if finalStr != "" {
		outBytes = []byte(finalStr + "\n")
	}
	if err := atomicWriteConfigFile(realPath, outBytes, 0o600); err != nil {
		return "", err
	}
	return "removed", nil
}

// parseTOMLTableHeader extracts the raw header expression inside `[...]` or `[[...]]`
// when line starts a TOML table, ignoring `#` comments outside double/single quotes.
func parseTOMLTableHeader(line string) (string, bool) {
	s := strings.TrimSpace(stripTOMLComment(line))
	if len(s) < 3 || s[0] != '[' || s[len(s)-1] != ']' {
		return "", false
	}
	if strings.HasPrefix(s, "[[") && strings.HasSuffix(s, "]]") && len(s) >= 5 {
		return strings.TrimSpace(s[2 : len(s)-2]), true
	}
	return strings.TrimSpace(s[1 : len(s)-1]), true
}

func isFlowskTOMLHeader(hdr string, includeSubtables bool) bool {
	parts := strings.Split(hdr, ".")
	if len(parts) < 2 {
		return false
	}
	if !includeSubtables && len(parts) != 2 {
		return false
	}
	clean := func(p string) string {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, `"'`)
		return p
	}
	return clean(parts[0]) == "mcp_servers" && clean(parts[1]) == SkillName
}

func stripTOMLComment(line string) string {
	var inDouble, inSingle, escaped bool
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' && inDouble {
			escaped = true
			continue
		}
		if ch == '"' && !inSingle {
			inDouble = !inDouble
			continue
		}
		if ch == '\'' && !inDouble {
			inSingle = !inSingle
			continue
		}
		if ch == '#' && !inDouble && !inSingle {
			return line[:i]
		}
	}
	return line
}

// resolveWritableTargetPath resolves symbolic links on path (if present) so atomic
// rename writes update the symlink target file instead of replacing the symlink itself.
func resolveWritableTargetPath(path string) (realPath string, isSymlink bool, err error) {
	cleaned := filepath.Clean(strings.TrimSpace(path))
	if cleaned == "" || cleaned == "." {
		return "", false, errors.New("empty config file path")
	}

	linfo, err := os.Lstat(cleaned)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cleaned, false, nil
		}
		return "", false, err
	}

	if linfo.Mode()&os.ModeSymlink != 0 {
		resolved, evalErr := filepath.EvalSymlinks(cleaned)
		if evalErr == nil {
			return resolved, true, nil
		}
		linkDest, readErr := os.Readlink(cleaned)
		if readErr != nil {
			return "", true, evalErr
		}
		if !filepath.IsAbs(linkDest) {
			linkDest = filepath.Join(filepath.Dir(cleaned), linkDest)
		}
		return filepath.Clean(linkDest), true, nil
	}

	return cleaned, false, nil
}

func atomicWriteConfigFile(realPath string, content []byte, defaultPerm os.FileMode) error {
	perm := defaultPerm
	if info, err := os.Stat(realPath); err == nil {
		if info.IsDir() {
			return fmt.Errorf("config path %s is a directory", realPath)
		}
		perm = info.Mode().Perm()
	}

	dir := filepath.Dir(realPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config directory %s: %w", dir, err)
	}

	tmpFile, err := os.CreateTemp(dir, ".flowsk-mcp-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary config file in %s: %w", dir, err)
	}
	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmpFile.Write(content); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("write temporary config file: %w", err)
	}
	if err := tmpFile.Chmod(perm); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("chmod temporary config file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temporary config file: %w", err)
	}

	if err := os.Rename(tmpPath, realPath); err != nil {
		return fmt.Errorf("atomically replace %s: %w", realPath, err)
	}
	cleanup = false
	return nil
}
