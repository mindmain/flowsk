// File helpers.go provides shared client-side validation, XDG config/path
// resolution, atomic 0600 file persistence, stdin/file script loading, and
// local working-directory project resolution helpers for Flowsk CLI commands.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/viper"
)

func defaultWorkspace(ws string) string {
	trimmed := strings.TrimSpace(ws)
	if trimmed == "" {
		return "default"
	}
	return trimmed
}

func normalizeLocalDir(rawDir string) (string, error) {
	target := strings.TrimSpace(rawDir)
	if target == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve working directory: %w", err)
		}
		target = cwd
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf("resolve absolute path %q: %w", target, err)
	}
	if resolved, evalErr := filepath.EvalSymlinks(abs); evalErr == nil {
		return filepath.Clean(resolved), nil
	}
	return filepath.Clean(abs), nil
}

// resolveActiveProject resolves the effective (workspace, project) identifiers
// from an explicit positional argument, --project / FLOWSK_PROJECT, or the
// client-only ~/.flowsk/local_paths.json binding for the current working
// directory. Host filesystem paths are never returned to callers.
func resolveActiveProject(ctx context.Context, opts *GlobalOptions, localPaths LocalPathRegistry, explicitProject string) (string, string, error) {
	ws := defaultWorkspace(opts.Workspace)
	if exp := strings.TrimSpace(explicitProject); exp != "" {
		return ws, exp, nil
	}
	if p := strings.TrimSpace(opts.Project); p != "" {
		return ws, p, nil
	}
	if localPaths == nil {
		return "", "", errors.New("project is required: pass --project, set FLOWSK_PROJECT, or bind this directory via 'flowsk project link'")
	}
	cwd, err := normalizeLocalDir("")
	if err != nil {
		return "", "", err
	}
	binding, err := localPaths.ResolveByDir(ctx, cwd)
	if err != nil {
		return "", "", fmt.Errorf("resolve project from local directory binding: %w (pass --project or run 'flowsk project link')", err)
	}
	projRef := strings.TrimSpace(binding.ProjectID)
	if projRef == "" {
		projRef = strings.TrimSpace(binding.ProjectKey)
	}
	if projRef == "" {
		return "", "", errors.New("local path binding has empty project identifier")
	}
	if bws := strings.TrimSpace(binding.WorkspaceID); bws != "" && (strings.TrimSpace(opts.Workspace) == "" || opts.Workspace == "default") {
		ws = bws
	}
	return ws, projRef, nil
}

// resolveOptionalProject attempts to resolve an ambient project from --project
// or the client-local working directory binding, returning an empty project
// string instead of an error when the current folder is unbound.
func resolveOptionalProject(ctx context.Context, opts *GlobalOptions, localPaths LocalPathRegistry) (string, string) {
	ws := defaultWorkspace(opts.Workspace)
	if p := strings.TrimSpace(opts.Project); p != "" {
		return ws, p
	}
	if localPaths == nil {
		return ws, ""
	}
	cwd, err := normalizeLocalDir("")
	if err != nil {
		return ws, ""
	}
	binding, err := localPaths.ResolveByDir(ctx, cwd)
	if err != nil {
		return ws, ""
	}
	if bws := strings.TrimSpace(binding.WorkspaceID); bws != "" && (strings.TrimSpace(opts.Workspace) == "" || opts.Workspace == "default") {
		ws = bws
	}
	projRef := strings.TrimSpace(binding.ProjectID)
	if projRef == "" {
		projRef = strings.TrimSpace(binding.ProjectKey)
	}
	return ws, projRef
}

func resolveClientConfigPaths(opts *GlobalOptions) (string, string, string, string, error) {
	var (
		cfgFile string
		err     error
	)
	if opts != nil && strings.TrimSpace(opts.ConfigFile) != "" {
		cfgFile = filepath.Clean(strings.TrimSpace(opts.ConfigFile))
	} else {
		cfgFile, err = DefaultConfigFile()
		if err != nil {
			return "", "", "", "", err
		}
	}
	cfgDir := filepath.Dir(cfgFile)
	localPathsFile := filepath.Join(cfgDir, "local_paths.json")

	var dbFile string
	if opts != nil && strings.TrimSpace(opts.DBPath) != "" {
		dbFile = filepath.Clean(strings.TrimSpace(opts.DBPath))
	} else {
		dbFile, err = DefaultLocalDBFile()
		if err != nil {
			return "", "", "", "", err
		}
	}
	return cfgDir, cfgFile, localPathsFile, dbFile, nil
}

// writeSecureFile ensures parent directories exist with 0700 permissions and
// atomically writes content to targetPath with 0600 permissions.
func writeSecureFile(targetPath string, content []byte) error {
	cleanPath := filepath.Clean(targetPath)
	dir := filepath.Dir(cleanPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create directory %q: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("chmod directory %q: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".flowsk-tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, cleanPath); err != nil {
		return fmt.Errorf("rename temp file to %q: %w", cleanPath, err)
	}
	cleanup = false
	if err := os.Chmod(cleanPath, 0o600); err != nil {
		return fmt.Errorf("chmod %q: %w", cleanPath, err)
	}
	return nil
}

func persistWorkspaceInConfig(opts *GlobalOptions, workspaceSlug string) (string, error) {
	_, cfgFile, _, dbFile, err := resolveClientConfigPaths(opts)
	if err != nil {
		return "", err
	}

	cfgViper := viper.New()
	cfgViper.SetConfigType("yaml")
	if existing, readErr := os.ReadFile(filepath.Clean(cfgFile)); readErr == nil { // #nosec G304 -- user-scoped config path resolved via DefaultConfigFile or --config
		if err := cfgViper.ReadConfig(strings.NewReader(string(existing))); err != nil {
			return "", fmt.Errorf("parse existing config %q: %w", cfgFile, err)
		}
	} else if !os.IsNotExist(readErr) {
		return "", fmt.Errorf("read config %q: %w", cfgFile, readErr)
	}

	var b strings.Builder
	b.WriteString("# Flowsk user-level client configuration (never stored inside Git repositories)\n")
	_, _ = fmt.Fprintf(&b, "workspace: %q\n", workspaceSlug)

	dbVal := strings.TrimSpace(cfgViper.GetString("db-path"))
	if dbVal == "" {
		dbVal = dbFile
	}
	_, _ = fmt.Fprintf(&b, "db-path: %q\n", dbVal)

	if srv := strings.TrimSpace(cfgViper.GetString("server-url")); srv != "" {
		_, _ = fmt.Fprintf(&b, "server-url: %q\n", srv)
	}
	if authTok := strings.TrimSpace(cfgViper.GetString("auth-token")); authTok != "" {
		_, _ = fmt.Fprintf(&b, "auth-token: %q\n", authTok)
	}
	if proj := strings.TrimSpace(cfgViper.GetString("project")); proj != "" {
		_, _ = fmt.Fprintf(&b, "project: %q\n", proj)
	}
	if actor := strings.TrimSpace(cfgViper.GetString("actor")); actor != "" {
		_, _ = fmt.Fprintf(&b, "actor: %q\n", actor)
	}
	if fmtVal := strings.TrimSpace(cfgViper.GetString("format")); fmtVal != "" {
		_, _ = fmt.Fprintf(&b, "format: %q\n", fmtVal)
	}
	if mt := cfgViper.GetInt("max-tokens"); mt > 0 {
		_, _ = fmt.Fprintf(&b, "max-tokens: %d\n", mt)
	}

	handled := map[string]bool{
		"workspace":  true,
		"db-path":    true,
		"server-url": true,
		"auth-token": true,
		"project":    true,
		"actor":      true,
		"format":     true,
		"max-tokens": true,
	}
	allSettings := cfgViper.AllSettings()
	extraKeys := make([]string, 0, len(allSettings))
	for k := range allSettings {
		if !handled[strings.ToLower(k)] {
			extraKeys = append(extraKeys, k)
		}
	}
	slices.Sort(extraKeys)
	for _, k := range extraKeys {
		writeYAMLKeyValue(&b, k, allSettings[k], 0)
	}

	if err := writeSecureFile(cfgFile, []byte(b.String())); err != nil {
		return "", err
	}
	return cfgFile, nil
}

func writeYAMLKeyValue(b *strings.Builder, key string, val any, indent int) {
	pad := strings.Repeat("  ", indent)
	switch v := val.(type) {
	case nil:
		return
	case string:
		_, _ = fmt.Fprintf(b, "%s%s: %q\n", pad, key, v)
	case bool:
		_, _ = fmt.Fprintf(b, "%s%s: %t\n", pad, key, v)
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		_, _ = fmt.Fprintf(b, "%s%s: %v\n", pad, key, v)
	case []any:
		if len(v) == 0 {
			_, _ = fmt.Fprintf(b, "%s%s: []\n", pad, key)
			return
		}
		_, _ = fmt.Fprintf(b, "%s%s:\n", pad, key)
		for _, elem := range v {
			switch ev := elem.(type) {
			case string:
				_, _ = fmt.Fprintf(b, "%s  - %q\n", pad, ev)
			default:
				_, _ = fmt.Fprintf(b, "%s  - %v\n", pad, ev)
			}
		}
	case map[string]any:
		if len(v) == 0 {
			_, _ = fmt.Fprintf(b, "%s%s: {}\n", pad, key)
			return
		}
		_, _ = fmt.Fprintf(b, "%s%s:\n", pad, key)
		subKeys := make([]string, 0, len(v))
		for sk := range v {
			subKeys = append(subKeys, sk)
		}
		slices.Sort(subKeys)
		for _, sk := range subKeys {
			writeYAMLKeyValue(b, sk, v[sk], indent+1)
		}
	default:
		_, _ = fmt.Fprintf(b, "%s%s: %q\n", pad, key, fmt.Sprint(v))
	}
}

func readExclusiveTextInput(stdin io.Reader, inlineText, filePath string, fromStdin bool, kindLabel string) (string, error) {
	sources := 0
	if inlineText != "" {
		sources++
	}
	if strings.TrimSpace(filePath) != "" {
		sources++
	}
	if fromStdin {
		sources++
	}
	if sources == 0 {
		return "", fmt.Errorf("%s input required: provide exactly one input flag", kindLabel)
	}
	if sources > 1 {
		return "", fmt.Errorf("ambiguous %s input: specify only one input source", kindLabel)
	}

	var raw string
	switch {
	case inlineText != "":
		raw = inlineText
	case strings.TrimSpace(filePath) != "":
		cleanFile := filepath.Clean(strings.TrimSpace(filePath))
		data, err := os.ReadFile(cleanFile) // #nosec G304 -- explicit client-local -f/--file CLI flag; path is never sent to server
		if err != nil {
			return "", fmt.Errorf("read %s file %q: %w", kindLabel, cleanFile, err)
		}
		raw = string(data)
	case fromStdin:
		if stdin == nil {
			return "", fmt.Errorf("stdin reader unavailable for %s", kindLabel)
		}
		data, err := io.ReadAll(stdin)
		if err != nil {
			return "", fmt.Errorf("read %s from stdin: %w", kindLabel, err)
		}
		raw = string(data)
	}

	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("%s content must not be empty", kindLabel)
	}
	return raw, nil
}

func validateDBDriver(driver string) (string, error) {
	norm := strings.ToLower(strings.TrimSpace(driver))
	switch norm {
	case "sqlite", "postgres":
		return norm, nil
	default:
		return "", fmt.Errorf("invalid --db-driver %q: must be sqlite or postgres", driver)
	}
}
