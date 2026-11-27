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
// configurations. When customCommand is empty, it checks `os.Executable()` first
// (when its basename is `flowsk` and not a `.test` or `go-build` temporary binary,
// converted via `filepath.Abs`), falling back to `exec.LookPath("flowsk")` (also
// converted via `filepath.Abs`) or `"flowsk"`.
func ResolveMCPCommand(customCommand string) string {
	if trimmed := strings.TrimSpace(customCommand); trimmed != "" {
		return trimmed
	}
	if exe, err := os.Executable(); err == nil && strings.TrimSpace(exe) != "" {
		rawBase := filepath.Base(exe)
		base := strings.TrimSuffix(rawBase, ".exe")
		if base == "flowsk" && !strings.HasSuffix(rawBase, ".test") && !strings.Contains(exe, "go-build") {
			if absExe, absErr := filepath.Abs(exe); absErr == nil {
				return filepath.Clean(absExe)
			}
			return filepath.Clean(exe)
		}
	}
	if resolved, err := exec.LookPath("flowsk"); err == nil && strings.TrimSpace(resolved) != "" {
		if absPath, absErr := filepath.Abs(resolved); absErr == nil {
			return filepath.Clean(absPath)
		}
		return filepath.Clean(resolved)
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

	// Best-effort cleanup of a distinct pre-migration MCP config file when present.
	if legacyCfg := strings.TrimSpace(target.LegacyMCPConfigPath); legacyCfg != "" {
		if legacyReal, legacySym, lErr := resolveWritableTargetPath(legacyCfg); lErr == nil && filepath.Clean(legacyReal) != filepath.Clean(realPath) {
			var legacyAction string
			switch target.MCPFormat {
			case MCPFormatJSON:
				legacyAction, _ = removeJSONMCPServer(legacyReal, legacySym)
			case MCPFormatTOML:
				legacyAction, _ = removeTOMLMCPServer(legacyReal, legacySym)
			}
			if action == "not_found" && legacyAction == "removed" {
				action = "removed"
			}
		}
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
	wantCmdLine := fmt.Sprintf("command = %s", strconv.Quote(cmd))
	wantArgsLine := fmt.Sprintf("args = [%s]", strings.Join(quotedArgs, ", "))
	canonicalBlock := fmt.Sprintf("[mcp_servers.%s]\n%s\n%s", SkillName, wantCmdLine, wantArgsLine)

	normalized := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if strings.TrimSpace(normalized) == "" {
		if err := atomicWriteConfigFile(realPath, []byte(canonicalBlock+"\n"), 0o600); err != nil {
			return "", err
		}
		return "installed", nil
	}

	lines := strings.Split(normalized, "\n")
	headers := scanTOMLTableHeaders(lines)
	startIdx := -1
	endIdx := len(lines)

	for _, h := range headers {
		if startIdx == -1 {
			if isFlowskTOMLHeader(h.Header, false) {
				startIdx = h.LineIdx
			}
		} else {
			endIdx = h.LineIdx
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

	existingSection := lines[startIdx:bodyEnd]
	updatedSection := updateFlowskTOMLSection(existingSection, wantCmdLine, wantArgsLine)
	if slices.Equal(existingSection, updatedSection) {
		return "unchanged", nil
	}

	var outLines []string
	outLines = append(outLines, lines[:startIdx]...)
	outLines = append(outLines, updatedSection...)
	outLines = append(outLines, lines[bodyEnd:]...)

	updatedContent := strings.TrimRight(strings.Join(outLines, "\n"), "\n") + "\n"
	if err := atomicWriteConfigFile(realPath, []byte(updatedContent), 0o600); err != nil {
		return "", err
	}
	return "updated", nil
}

func updateFlowskTOMLSection(sectionLines []string, wantCmdLine, wantArgsLine string) []string {
	if len(sectionLines) == 0 {
		return []string{fmt.Sprintf("[mcp_servers.%s]", SkillName), wantCmdLine, wantArgsLine}
	}
	rebuilt := []string{sectionLines[0]}
	seenCmd := false
	seenArgs := false
	cmdIdx := -1

	i := 1
	for i < len(sectionLines) {
		line := sectionLines[i]
		key, rhs, isKV := splitTOMLTopLevelKeyValue(line)
		if !isKV {
			rebuilt = append(rebuilt, line)
			i++
			continue
		}
		endLine := consumeTOMLValueSpan(sectionLines, i, rhs)
		switch key {
		case "command":
			if !seenCmd {
				cmdIdx = len(rebuilt)
				rebuilt = append(rebuilt, wantCmdLine)
				seenCmd = true
			}
		case "args":
			if !seenArgs {
				rebuilt = append(rebuilt, wantArgsLine)
				seenArgs = true
			}
		default:
			rebuilt = append(rebuilt, sectionLines[i:endLine+1]...)
		}
		i = endLine + 1
	}

	if !seenCmd && !seenArgs {
		rebuilt = slices.Insert(rebuilt, 1, wantCmdLine, wantArgsLine)
	} else if !seenCmd {
		rebuilt = slices.Insert(rebuilt, 1, wantCmdLine)
	} else if !seenArgs {
		insertAt := cmdIdx + 1
		if insertAt < 1 || insertAt > len(rebuilt) {
			insertAt = len(rebuilt)
		}
		rebuilt = slices.Insert(rebuilt, insertAt, wantArgsLine)
	}
	return rebuilt
}

func splitTOMLTopLevelKeyValue(line string) (key, rhs string, ok bool) {
	stripped := stripTOMLComment(line)
	trimmed := strings.TrimSpace(stripped)
	if trimmed == "" || trimmed[0] == '[' {
		return "", "", false
	}
	var inDouble, inSingle, escaped bool
	for i := 0; i < len(trimmed); i++ {
		ch := trimmed[i]
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
		if ch == '=' && !inDouble && !inSingle {
			rawKey := strings.TrimSpace(trimmed[:i])
			rawKey = strings.Trim(rawKey, `"'`)
			if rawKey == "" {
				return "", "", false
			}
			return rawKey, strings.TrimSpace(trimmed[i+1:]), true
		}
	}
	return "", "", false
}

func consumeTOMLValueSpan(lines []string, startIdx int, firstRHS string) int {
	var inMultiDouble, inMultiSingle bool
	bracketDepth := 0
	braceDepth := 0

	scanSegment := func(seg string) {
		j := 0
		for j < len(seg) {
			if inMultiDouble {
				if seg[j] == '\\' {
					j += 2
					continue
				}
				if strings.HasPrefix(seg[j:], `"""`) {
					j += 3
					for j < len(seg) && seg[j] == '"' {
						j++
					}
					inMultiDouble = false
					continue
				}
				j++
				continue
			}
			if inMultiSingle {
				if strings.HasPrefix(seg[j:], `'''`) {
					j += 3
					for j < len(seg) && seg[j] == '\'' {
						j++
					}
					inMultiSingle = false
					continue
				}
				j++
				continue
			}
			ch := seg[j]
			if ch == '#' {
				return
			}
			if strings.HasPrefix(seg[j:], `"""`) {
				inMultiDouble = true
				j += 3
				continue
			}
			if strings.HasPrefix(seg[j:], `'''`) {
				inMultiSingle = true
				j += 3
				continue
			}
			if ch == '"' {
				j++
				for j < len(seg) {
					if seg[j] == '\\' {
						j += 2
						continue
					}
					if seg[j] == '"' {
						j++
						break
					}
					j++
				}
				continue
			}
			if ch == '\'' {
				j++
				for j < len(seg) {
					if seg[j] == '\'' {
						j++
						break
					}
					j++
				}
				continue
			}
			switch ch {
			case '[':
				bracketDepth++
			case ']':
				if bracketDepth > 0 {
					bracketDepth--
				}
			case '{':
				braceDepth++
			case '}':
				if braceDepth > 0 {
					braceDepth--
				}
			}
			j++
		}
	}

	scanSegment(firstRHS)
	cur := startIdx
	for (inMultiDouble || inMultiSingle || bracketDepth > 0 || braceDepth > 0) && cur+1 < len(lines) {
		cur++
		scanSegment(lines[cur])
	}
	return cur
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
	headers := scanTOMLTableHeaders(lines)
	headerByLine := make(map[int]string, len(headers))
	for _, h := range headers {
		headerByLine[h.LineIdx] = h.Header
	}

	var kept []string
	skipping := false
	removedAny := false

	for idx, line := range lines {
		if hdr, isHdr := headerByLine[idx]; isHdr {
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

type tomlHeaderLine struct {
	LineIdx int
	Header  string
}

func scanTOMLTableHeaders(lines []string) []tomlHeaderLine {
	var out []tomlHeaderLine
	var inMultiDouble, inMultiSingle bool
	for idx, line := range lines {
		startedInMulti := inMultiDouble || inMultiSingle
		advanceTOMLMultilineState(line, &inMultiDouble, &inMultiSingle)
		if startedInMulti || inMultiDouble || inMultiSingle {
			continue
		}
		if hdr, ok := parseTOMLTableHeader(line); ok {
			out = append(out, tomlHeaderLine{LineIdx: idx, Header: hdr})
		}
	}
	return out
}

func advanceTOMLMultilineState(line string, inMultiDouble, inMultiSingle *bool) {
	i := 0
	for i < len(line) {
		if *inMultiDouble {
			if line[i] == '\\' {
				i += 2
				continue
			}
			if strings.HasPrefix(line[i:], `"""`) {
				i += 3
				for i < len(line) && line[i] == '"' {
					i++
				}
				*inMultiDouble = false
				continue
			}
			i++
			continue
		}
		if *inMultiSingle {
			if strings.HasPrefix(line[i:], `'''`) {
				i += 3
				for i < len(line) && line[i] == '\'' {
					i++
				}
				*inMultiSingle = false
				continue
			}
			i++
			continue
		}
		ch := line[i]
		if ch == '#' {
			return
		}
		if strings.HasPrefix(line[i:], `"""`) {
			*inMultiDouble = true
			i += 3
			continue
		}
		if strings.HasPrefix(line[i:], `'''`) {
			*inMultiSingle = true
			i += 3
			continue
		}
		if ch == '"' {
			i++
			for i < len(line) {
				if line[i] == '\\' {
					i += 2
					continue
				}
				if line[i] == '"' {
					i++
					break
				}
				i++
			}
			continue
		}
		if ch == '\'' {
			i++
			for i < len(line) {
				if line[i] == '\'' {
					i++
					break
				}
				i++
			}
			continue
		}
		i++
	}
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

// resolveWritableTargetPath resolves symbolic links on path (looping up to 8 hops
// across dangling symlink chains) so atomic rename writes update the final target
// file instead of replacing any symlink along the chain.
func resolveWritableTargetPath(path string) (realPath string, isSymlink bool, err error) {
	cleaned := filepath.Clean(strings.TrimSpace(path))
	if cleaned == "" || cleaned == "." {
		return "", false, errors.New("empty config file path")
	}

	const maxSymlinkHops = 8
	cur := cleaned
	for range maxSymlinkHops {
		linfo, lErr := os.Lstat(cur)
		if lErr != nil {
			if errors.Is(lErr, os.ErrNotExist) {
				return cur, isSymlink, nil
			}
			return "", isSymlink, lErr
		}
		if linfo.Mode()&os.ModeSymlink == 0 {
			return cur, isSymlink, nil
		}
		isSymlink = true
		if resolved, evalErr := filepath.EvalSymlinks(cur); evalErr == nil {
			return filepath.Clean(resolved), true, nil
		} else if !errors.Is(evalErr, os.ErrNotExist) {
			return "", true, evalErr
		}
		linkDest, readErr := os.Readlink(cur)
		if readErr != nil {
			return "", true, readErr
		}
		if !filepath.IsAbs(linkDest) {
			linkDest = filepath.Join(filepath.Dir(cur), linkDest)
		}
		cur = filepath.Clean(linkDest)
	}

	if linfo, lErr := os.Lstat(cur); lErr == nil && linfo.Mode()&os.ModeSymlink != 0 {
		return "", true, fmt.Errorf("too many symbolic link levels resolving %s", cleaned)
	}
	return cur, isSymlink, nil
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
