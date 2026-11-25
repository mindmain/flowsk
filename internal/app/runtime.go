// Package app wires the Flowsk application composition root, connecting pure
// domain services, the Cloudflare Code Mode JavaScript engine (`goja`), embedded
// Goose SQLite/PostgreSQL persistence, client-only local path bindings,
// ConnectRPC server/client adapters, and MCP servers behind `cli.Dependencies`.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/adapter/db"
	"github.com/mindmain/flowsk/internal/adapter/db/postgres"
	"github.com/mindmain/flowsk/internal/adapter/db/sqlite"
	"github.com/mindmain/flowsk/internal/adapter/localpath"
	"github.com/mindmain/flowsk/internal/cli"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/transport/mcp"
)

var flowskUUIDNamespace = uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8")

// deterministicUUID derives a stable UUIDv5 within Flowsk's internal namespace.
func deterministicUUID(parts ...string) uuid.UUID {
	return uuid.NewSHA1(flowskUUIDNamespace, []byte("flowsk:"+strings.Join(parts, ":")))
}

// sanitizeActorHandle normalizes an arbitrary username or handle into a valid
// Flowsk actor handle (`^[a-z0-9_]+(-[a-z0-9_]+)*$`, length 2..64).
func sanitizeActorHandle(raw string) string {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "" {
		if u, err := user.Current(); err == nil && strings.TrimSpace(u.Username) != "" {
			trimmed = strings.ToLower(strings.TrimSpace(u.Username))
		}
	}
	var b strings.Builder
	lastHyphen := false
	for _, r := range trimmed {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_':
			b.WriteRune(r)
			lastHyphen = false
		case unicode.IsSpace(r) || r == '-' || r == '.' || r == '/' || r == '\\':
			if b.Len() > 0 && !lastHyphen {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
		if b.Len() >= 60 {
			break
		}
	}
	res := strings.Trim(b.String(), "-")
	if len(res) < 2 {
		return "local-operator"
	}
	return res
}

// classifyActorKind maps CLI kind strings ("human", "agent", "ci") to domain ActorKind.
func classifyActorKind(rawKind, handle string) authctx.ActorKind {
	switch strings.ToLower(strings.TrimSpace(rawKind)) {
	case "agent", "ci":
		return authctx.ActorKindAgent
	case "human":
		return authctx.ActorKindHuman
	default:
		lower := strings.ToLower(handle)
		if strings.Contains(lower, "agent") || strings.Contains(lower, "claude") ||
			strings.Contains(lower, "gemini") || strings.Contains(lower, "codex") ||
			strings.Contains(lower, "bot") {
			return authctx.ActorKindAgent
		}
		return authctx.ActorKindHuman
	}
}

// Runtime manages lazy-initialized embedded SQLite engines, client-local path
// registries, and remote ConnectRPC clients across a CLI or server invocation.
type Runtime struct {
	mu      sync.Mutex
	engines map[string]*localEngine
}

// NewRuntime constructs a ready-to-execute Flowsk application Runtime.
func NewRuntime() *Runtime {
	return &Runtime{
		engines: make(map[string]*localEngine),
	}
}

// Close closes all opened embedded SQLite/PostgreSQL database adapters cleanly.
func (r *Runtime) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var errsOut []error
	for path, eng := range r.engines {
		if err := eng.adapter.Close(); err != nil {
			errsOut = append(errsOut, fmt.Errorf("close engine %q: %w", path, err))
		}
		delete(r.engines, path)
	}
	return errors.Join(errsOut...)
}

// CLIDependencies returns the segregated `cli.Dependencies` backed by this Runtime.
func (r *Runtime) CLIDependencies() cli.Dependencies {
	return &runtimeDeps{rt: r}
}

func (r *Runtime) optionsFromCtx(ctx context.Context) cli.GlobalOptions {
	if opts, ok := cli.GlobalOptionsFromContext(ctx); ok {
		return opts
	}
	defaultDB, _ := cli.DefaultLocalDBFile()
	return cli.GlobalOptions{
		DBPath:    defaultDB,
		Workspace: "default",
		Format:    cli.FormatAuto,
	}
}

func (r *Runtime) openOrGetLocalEngine(ctx context.Context, dbPath string) (*localEngine, error) {
	resolvedPath := strings.TrimSpace(dbPath)
	if resolvedPath == "" {
		var err error
		resolvedPath, err = cli.DefaultLocalDBFile()
		if err != nil {
			return nil, err
		}
	}
	if resolvedPath != ":memory:" && !strings.HasPrefix(resolvedPath, "file::memory:") {
		abs, err := filepath.Abs(resolvedPath)
		if err == nil {
			resolvedPath = filepath.Clean(abs)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.engines[resolvedPath]; ok {
		return existing, nil
	}

	eng, err := newSQLiteEngine(ctx, resolvedPath, true)
	if err != nil {
		return nil, err
	}
	r.engines[resolvedPath] = eng
	return eng, nil
}

type runtimeDeps struct {
	rt *Runtime
}

func (d *runtimeDeps) ServerRunner() cli.ServerRunner         { return &appServerRunner{rt: d.rt} }
func (d *runtimeDeps) MCPRunner() cli.MCPRunner               { return &appMCPRunner{rt: d.rt} }
func (d *runtimeDeps) WorkspaceClient() cli.WorkspaceClient   { return &dualModeClient{rt: d.rt} }
func (d *runtimeDeps) ProjectClient() cli.ProjectClient       { return &dualModeClient{rt: d.rt} }
func (d *runtimeDeps) LocalPathRegistry() cli.LocalPathRegistry { return &localPathBridge{rt: d.rt} }
func (d *runtimeDeps) ObjectiveClient() cli.ObjectiveClient   { return &dualModeClient{rt: d.rt} }
func (d *runtimeDeps) TaskClient() cli.TaskClient             { return &dualModeClient{rt: d.rt} }
func (d *runtimeDeps) LanguageClient() cli.LanguageClient     { return &dualModeClient{rt: d.rt} }
func (d *runtimeDeps) AuditClient() cli.AuditClient           { return &dualModeClient{rt: d.rt} }
func (d *runtimeDeps) DatabaseAdmin() cli.DatabaseAdmin       { return &dbAdminBridge{} }
func (d *runtimeDeps) OutputFormatter() cli.OutputFormatter   { return nil }

// localPathBridge adapts `internal/adapter/localpath.FileRegistry` to `cli.LocalPathRegistry`.
type localPathBridge struct {
	rt *Runtime
}

func (b *localPathBridge) openRegistry(ctx context.Context) (*localpath.FileRegistry, error) {
	opts := b.rt.optionsFromCtx(ctx)
	var regPath string
	if strings.TrimSpace(opts.ConfigFile) != "" {
		regPath = filepath.Join(filepath.Dir(filepath.Clean(opts.ConfigFile)), "local_paths.json")
	} else {
		var err error
		regPath, err = cli.DefaultLocalPathsFile()
		if err != nil {
			return nil, err
		}
	}
	return localpath.NewFileRegistry(regPath)
}

func (b *localPathBridge) Link(ctx context.Context, localDir, workspaceID, projectID string) (cli.LocalProjectBinding, error) {
	reg, err := b.openRegistry(ctx)
	if err != nil {
		return cli.LocalProjectBinding{}, err
	}
	wsSlug := strings.TrimSpace(workspaceID)
	if wsSlug == "" {
		wsSlug = "default"
	}
	projRef := strings.TrimSpace(projectID)
	projUUID, parseErr := uuid.Parse(projRef)
	if parseErr != nil {
		projUUID = deterministicUUID("localpath-proj", wsSlug, strings.ToLower(projRef))
	}
	bound, err := reg.LinkProject(ctx, localpath.LinkProjectInput{
		WorkspaceSlug: wsSlug,
		ProjectID:     projUUID,
		ProjectSlug:   projRef,
		LocalPath:     localDir,
	})
	if err != nil {
		return cli.LocalProjectBinding{}, err
	}
	return cli.LocalProjectBinding{
		LocalDir:     bound.LocalAbsPath,
		WorkspaceID:  bound.WorkspaceSlug,
		ProjectID:    bound.ProjectSlug,
		ProjectKey:   strings.ToUpper(bound.ProjectSlug),
		GitCommonDir: bound.GitCommonDir,
		GitRelPath:   bound.GitRelPath,
		LinkedAt:     bound.UpdatedAt,
	}, nil
}

func (b *localPathBridge) Unlink(ctx context.Context, localDir string) error {
	reg, err := b.openRegistry(ctx)
	if err != nil {
		return err
	}
	return reg.UnlinkProject(ctx, localDir)
}

func (b *localPathBridge) ResolveByDir(ctx context.Context, localDir string) (cli.LocalProjectBinding, error) {
	reg, err := b.openRegistry(ctx)
	if err != nil {
		return cli.LocalProjectBinding{}, err
	}
	bound, err := reg.ResolveByWorkingDir(ctx, localDir)
	if err != nil {
		return cli.LocalProjectBinding{}, err
	}
	return toCLILocalBinding(ctx, bound), nil
}

func (b *localPathBridge) ListByProject(ctx context.Context, workspaceID, projectID string) ([]cli.LocalProjectBinding, error) {
	reg, err := b.openRegistry(ctx)
	if err != nil {
		return nil, err
	}
	all, err := reg.ListBindings(ctx)
	if err != nil {
		return nil, err
	}
	wsFilter := strings.ToLower(strings.TrimSpace(workspaceID))
	projFilter := strings.ToLower(strings.TrimSpace(projectID))
	var out []cli.LocalProjectBinding
	for _, item := range all {
		if wsFilter != "" && strings.ToLower(item.WorkspaceSlug) != wsFilter {
			continue
		}
		if projFilter != "" &&
			strings.ToLower(item.ProjectSlug) != projFilter &&
			strings.ToLower(item.ProjectID.String()) != projFilter {
			continue
		}
		out = append(out, toCLILocalBinding(ctx, item))
	}
	return out, nil
}

func toCLILocalBinding(ctx context.Context, item localpath.LocalBinding) cli.LocalProjectBinding {
	commonDir := item.GitCommonDir
	relPath := item.GitRelPath
	if commonDir == "" && strings.TrimSpace(item.LocalAbsPath) != "" {
		gitMeta := localpath.InspectDirectoryGit(ctx, item.LocalAbsPath)
		commonDir = gitMeta.GitCommonDir
		if gitMeta.GitTopLevel != "" {
			if rel, err := filepath.Rel(gitMeta.GitTopLevel, item.LocalAbsPath); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
				relPath = filepath.ToSlash(rel)
			}
		}
	}
	return cli.LocalProjectBinding{
		LocalDir:     item.LocalAbsPath,
		WorkspaceID:  item.WorkspaceSlug,
		ProjectID:    item.ProjectSlug,
		ProjectKey:   strings.ToUpper(item.ProjectSlug),
		GitCommonDir: commonDir,
		GitRelPath:   relPath,
		LinkedAt:     item.UpdatedAt,
	}
}

// dbAdminBridge implements `cli.DatabaseAdmin` across SQLite and PostgreSQL.
type dbAdminBridge struct{}

func openAdminAdapter(driver, dsn string) (db.DatabaseAdapter, error) {
	switch strings.ToLower(strings.TrimSpace(driver)) {
	case "sqlite", "":
		path := strings.TrimSpace(dsn)
		if path == "" {
			var err error
			path, err = cli.DefaultLocalDBFile()
			if err != nil {
				return nil, err
			}
		}
		return sqlite.NewAdapter(sqlite.Config{
			FilePath:    path,
			EnableWAL:   true,
			ForeignKeys: true,
			BusyTimeout: 5 * time.Second,
			Synchronous: sqlite.SynchronousNormal,
		})
	case "postgres":
		return postgres.NewAdapter(postgres.Config{
			DSN: strings.TrimSpace(dsn),
		})
	default:
		return nil, errs.Validation("app.openAdminAdapter", fmt.Sprintf("unsupported db driver %q", driver))
	}
}

func (dbAdminBridge) MigrateUp(ctx context.Context, in cli.MigrateUpInput) (cli.MigrationResult, error) {
	adapter, err := openAdminAdapter(in.Driver, in.DSN)
	if err != nil {
		return cli.MigrationResult{}, err
	}
	defer func() { _ = adapter.Close() }()

	before, _ := adapter.Migrations().Status(ctx)
	appliedBefore := countApplied(before)
	ver, err := adapter.Migrations().Up(ctx)
	if err != nil {
		return cli.MigrationResult{}, err
	}
	after, _ := adapter.Migrations().Status(ctx)
	return cli.MigrationResult{
		Driver:         string(adapter.Dialect()),
		Direction:      "up",
		CurrentVersion: ver,
		AppliedCount:   max(0, countApplied(after)-appliedBefore),
	}, nil
}

func (dbAdminBridge) MigrateDown(ctx context.Context, in cli.MigrateDownInput) (cli.MigrationResult, error) {
	adapter, err := openAdminAdapter(in.Driver, in.DSN)
	if err != nil {
		return cli.MigrationResult{}, err
	}
	defer func() { _ = adapter.Close() }()

	steps := in.Steps
	if steps <= 0 {
		steps = 1
	}
	var ver int64
	applied := 0
	for i := 0; i < steps; i++ {
		v, stepErr := adapter.Migrations().Down(ctx)
		if stepErr != nil {
			return cli.MigrationResult{}, stepErr
		}
		ver = v
		applied++
	}
	return cli.MigrationResult{
		Driver:         string(adapter.Dialect()),
		Direction:      "down",
		CurrentVersion: ver,
		AppliedCount:   applied,
	}, nil
}

func (dbAdminBridge) MigrateStatus(ctx context.Context, driver, dsn string) ([]cli.MigrationStatusEntry, error) {
	adapter, err := openAdminAdapter(driver, dsn)
	if err != nil {
		return nil, err
	}
	defer func() { _ = adapter.Close() }()

	statuses, err := adapter.Migrations().Status(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]cli.MigrationStatusEntry, 0, len(statuses))
	for _, st := range statuses {
		var appliedAt *time.Time
		if st.IsApplied && !st.AppliedAt.IsZero() {
			ts := st.AppliedAt
			appliedAt = &ts
		}
		out = append(out, cli.MigrationStatusEntry{
			Version:   st.Version,
			Name:      st.Name,
			Applied:   st.IsApplied,
			AppliedAt: appliedAt,
		})
	}
	return out, nil
}

func (dbAdminBridge) PingDB(ctx context.Context, driver, dsn string) error {
	adapter, err := openAdminAdapter(driver, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = adapter.Close() }()
	return adapter.Ping(ctx)
}

func countApplied(items []db.MigrationVersionStatus) int {
	n := 0
	for _, it := range items {
		if it.IsApplied {
			n++
		}
	}
	return n
}

// dualModeClient routes every CLI client call either to the in-process local
// SQLite engine (when ServerURL is empty) or over HTTP/ConnectRPC to a remote
// `flowsk serve` node (when ServerURL is configured).
type dualModeClient struct {
	rt *Runtime
}

// ensure unused import guard for mcp in runtime.go.
var _ = mcp.TransportStdio
var _ = os.ErrNotExist
