// File server_mcp.go implements `cli.ServerRunner` (`flowsk serve`) and
// `cli.MCPRunner` (`flowsk mcp serve`), mounting all 6 ConnectRPC bounded
// contexts, volume layout locks, bearer/header identity resolution, and MCP
// stdio/HTTP tools (`flowsk_search`, `flowsk_execute`, `flowsk_language_guide`, `flowsk_digest`).
package app

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/mindmain/flowsk/gen/go/flowsk/audit/v1/flowsk_audit_pbconnect"
	"github.com/mindmain/flowsk/gen/go/flowsk/language/v1/flowsk_language_pbconnect"
	"github.com/mindmain/flowsk/gen/go/flowsk/objective/v1/flowsk_objective_pbconnect"
	"github.com/mindmain/flowsk/gen/go/flowsk/project/v1/flowsk_project_pbconnect"
	"github.com/mindmain/flowsk/gen/go/flowsk/task/v1/flowsk_task_pbconnect"
	"github.com/mindmain/flowsk/gen/go/flowsk/workspace/v1/flowsk_workspace_pbconnect"
	"github.com/mindmain/flowsk/internal/adapter/formatter"
	"github.com/mindmain/flowsk/internal/adapter/volume"
	"github.com/mindmain/flowsk/internal/adapter/webui"
	"github.com/mindmain/flowsk/internal/cli"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/language"
	"github.com/mindmain/flowsk/internal/transport/connectrpc"
	"github.com/mindmain/flowsk/internal/transport/connectrpc/audithandler"
	"github.com/mindmain/flowsk/internal/transport/connectrpc/languagehandler"
	"github.com/mindmain/flowsk/internal/transport/connectrpc/objectivehandler"
	"github.com/mindmain/flowsk/internal/transport/connectrpc/projecthandler"
	"github.com/mindmain/flowsk/internal/transport/connectrpc/taskhandler"
	"github.com/mindmain/flowsk/internal/transport/connectrpc/workspacehandler"
	"github.com/mindmain/flowsk/internal/transport/mcp"
)

type appServerRunner struct {
	rt *Runtime
}

// RunServer starts the Flowsk HTTP/1.1 + H2C ConnectRPC server (`flowsk serve` and `flowsk ui`).
func (s *appServerRunner) RunServer(ctx context.Context, cfg cli.ServeConfig) error {
	opts := s.rt.optionsFromCtx(ctx)
	expectedToken := firstNonEmpty(opts.AuthToken, os.Getenv("FLOWSK_AUTH_TOKEN"))

	if cfg.EnableUI && opts.IsRemoteMode() {
		return s.runRemoteUIProxy(ctx, cfg, opts, expectedToken)
	}

	driver := strings.ToLower(strings.TrimSpace(cfg.DBDriver))
	if driver == "" {
		driver = "sqlite"
	}

	if driver == "sqlite" && strings.TrimSpace(cfg.VolumeDir) != "" {
		if vol, vErr := volume.NewSharedVolumeAdapterWithDB(cfg.VolumeDir, cfg.DBDSN); vErr == nil {
			_, _ = vol.EnsureLayout(ctx)
		}
	}

	var (
		eng *localEngine
		err error
	)
	switch driver {
	case "sqlite":
		dsn := strings.TrimSpace(cfg.DBDSN)
		if dsn == "" {
			dsn, err = cli.DefaultLocalDBFile()
			if err != nil {
				return err
			}
		}
		eng, err = newSQLiteEngine(ctx, dsn, cfg.AutoMigrate)
	case "postgres":
		eng, err = newPostgresEngine(ctx, cfg.DBDSN, cfg.AutoMigrate)
	default:
		return errs.Validation("app.RunServer", fmt.Sprintf("unsupported --db-driver %q", driver))
	}
	if err != nil {
		return err
	}
	defer func() { _ = eng.adapter.Close() }()

	resolver := connectrpc.IdentityResolverFunc(func(rCtx context.Context, spec connect.Spec, headers http.Header) (authctx.Identity, error) {
		if expectedToken != "" {
			got := strings.TrimSpace(headers.Get("Authorization"))
			got = strings.TrimSpace(strings.TrimPrefix(got, "Bearer"))
			got = strings.TrimSpace(strings.TrimPrefix(got, "bearer"))
			if subtle.ConstantTimeCompare([]byte(got), []byte(expectedToken)) != 1 {
				return authctx.Identity{}, errs.New(errs.ErrUnauthenticated, "app.ServerAuth", "invalid or missing bearer token")
			}
		}

		rawWS := strings.TrimSpace(headers.Get(connectrpc.HeaderWorkspaceID))
		rawHandle := strings.TrimSpace(headers.Get(connectrpc.HeaderActorHandle))
		rawKind := strings.TrimSpace(headers.Get(connectrpc.HeaderActorKind))

		if rawWS == "" && rawHandle == "" {
			switch spec.Procedure {
			case "/flowsk.language.v1.LanguageService/GetLanguageGuide",
				"/flowsk.language.v1.LanguageService/ListLanguageExamples",
				"/flowsk.workspace.v1.WorkspaceService/CreateWorkspace":
				return authctx.Identity{}, nil
			}
			rawWS = firstNonEmpty(os.Getenv("FLOWSK_DEFAULT_WORKSPACE"), "default")
			rawHandle = "remote-peer"
		}

		_, _, ident := eng.ensureWorkspaceAndActor(rCtx, rawWS, rawHandle, rawKind)
		if ident.WorkspaceID == ([16]byte{}) {
			return authctx.Identity{}, errs.New(errs.ErrUnauthenticated, "app.ServerAuth", "workspace could not be resolved")
		}
		return ident, nil
	})

	handlers := connectrpc.HandlerSet{
		Workspace: workspacehandler.New(eng.adapter, eng),
		Project:   projecthandler.New(eng.adapter, eng),
		Objective: objectivehandler.New(eng.adapter, eng),
		Task:      taskhandler.New(eng.adapter, eng),
		Language:  languagehandler.New(eng.adapter, eng),
		Audit:     audithandler.New(eng.adapter, eng),
	}

	broker := connectrpc.NewEventBroker(connectrpc.DefaultSubscriberBuffer)

	srv, err := connectrpc.NewServer(
		connectrpc.Config{
			ListenAddr: cfg.ListenAddr,
			EnableH2C:  true,
			OnListen:   cfg.OnListen,
		},
		handlers,
		connect.WithInterceptors(
			connectrpc.NewAuthContextInterceptor(resolver),
			connectrpc.NewEventBroadcastInterceptor(broker),
		),
	)
	if err != nil {
		return err
	}

	relayCtx, stopRelay := context.WithCancel(ctx)
	var relayWG sync.WaitGroup
	relayWG.Add(1)
	go func() {
		defer relayWG.Done()
		broker.RunAuditRelay(
			relayCtx,
			eng.AuditService(),
			func(pCtx context.Context, wsID uuid.UUID) (authctx.Identity, bool) {
				_, _, ident := eng.ensureWorkspaceAndActor(pCtx, wsID.String(), "stream-relay", "human")
				if ident.WorkspaceID != wsID {
					return authctx.Identity{}, false
				}
				return ident, true
			},
			connectrpc.DefaultAuditPollInterval,
		)
	}()
	defer func() {
		stopRelay()
		relayWG.Wait()
	}()

	streamAuth := func(rCtx context.Context, r *http.Request) (authctx.Identity, uuid.UUID, error) {
		if expectedToken != "" {
			got := strings.TrimSpace(r.Header.Get("Authorization"))
			got = strings.TrimSpace(strings.TrimPrefix(got, "Bearer"))
			got = strings.TrimSpace(strings.TrimPrefix(got, "bearer"))
			if subtle.ConstantTimeCompare([]byte(got), []byte(expectedToken)) != 1 {
				return authctx.Identity{}, uuid.Nil, errs.New(errs.ErrUnauthenticated, "app.StreamAuth", "invalid or missing bearer token")
			}
		}

		headerWS := strings.TrimSpace(r.Header.Get(connectrpc.HeaderWorkspaceID))
		queryWS := strings.TrimSpace(r.URL.Query().Get("workspaceId"))
		if headerWS != "" && queryWS != "" && !strings.EqualFold(headerWS, queryWS) {
			return authctx.Identity{}, uuid.Nil, errs.New(errs.ErrPermissionDenied, "app.StreamAuth", "workspace header does not match requested stream workspace")
		}

		targetRef := firstNonEmpty(queryWS, headerWS, os.Getenv("FLOWSK_DEFAULT_WORKSPACE"), "default")
		handle := firstNonEmpty(
			strings.TrimSpace(r.Header.Get(connectrpc.HeaderActorHandle)),
			strings.TrimSpace(r.URL.Query().Get("actorHandle")),
			"web-operator",
		)
		kind := firstNonEmpty(strings.TrimSpace(r.Header.Get(connectrpc.HeaderActorKind)), "human")

		_, ws, ident := eng.ensureWorkspaceAndActor(rCtx, targetRef, handle, kind)
		if ws.ID == uuid.Nil || ident.WorkspaceID == uuid.Nil {
			return authctx.Identity{}, uuid.Nil, errs.New(errs.ErrUnauthenticated, "app.StreamAuth", "workspace could not be resolved")
		}
		return ident, ws.ID, nil
	}
	srv.Mux().Handle(connectrpc.EventsPath, connectrpc.NewSSEHandler(broker, streamAuth, connectrpc.DefaultStreamHeartbeat))

	if cfg.EnableUI {
		wsRef := firstNonEmpty(cfg.UIWorkspace, opts.Workspace, "default")
		actorRef := firstNonEmpty(cfg.UIActor, opts.Actor, "web-operator")
		_, ws, ident := eng.ensureWorkspaceAndActor(ctx, wsRef, actorRef, "human")
		bootCfg := webui.RuntimeBootstrap{
			WorkspaceSlug: firstNonEmpty(ws.Slug, wsRef),
			ProjectSlug:   firstNonEmpty(cfg.UIProject, opts.Project),
			ActorHandle:   firstNonEmpty(ident.Handle, actorRef),
		}
		if ws.ID != ([16]byte{}) {
			bootCfg.WorkspaceID = ws.ID.String()
		}
		if ident.ActorID != ([16]byte{}) {
			bootCfg.ActorID = ident.ActorID.String()
		}
		if err := mountWebUIRoutes(srv.Mux(), bootCfg); err != nil {
			return err
		}
	}

	return srv.Serve(ctx)
}

func mountWebUIRoutes(mux *http.ServeMux, bootCfg webui.RuntimeBootstrap) error {
	spaHandler, err := webui.NewSPAHandler()
	if err != nil {
		return fmt.Errorf("mount embedded webui: %w", err)
	}
	mux.Handle("/api/ui-config", webui.NewBootstrapHandler(bootCfg))
	mux.Handle("/", spaHandler)
	return nil
}

func (s *appServerRunner) runRemoteUIProxy(ctx context.Context, cfg cli.ServeConfig, opts cli.GlobalOptions, bearerToken string) error {
	targetURL, err := url.Parse(strings.TrimSpace(opts.ServerURL))
	if err != nil || targetURL.Scheme == "" || targetURL.Host == "" {
		return errs.Validation("app.runRemoteUIProxy", fmt.Sprintf("invalid remote server URL %q", opts.ServerURL))
	}

	proxy := &httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(targetURL)
			pr.Out.Host = targetURL.Host
			if bearerToken != "" && strings.TrimSpace(pr.In.Header.Get("Authorization")) == "" {
				pr.Out.Header.Set("Authorization", "Bearer "+bearerToken)
			}
		},
	}

	mux := http.NewServeMux()
	for _, svcName := range []string{
		flowsk_workspace_pbconnect.WorkspaceServiceName,
		flowsk_project_pbconnect.ProjectServiceName,
		flowsk_objective_pbconnect.ObjectiveServiceName,
		flowsk_task_pbconnect.TaskServiceName,
		flowsk_language_pbconnect.LanguageServiceName,
		flowsk_audit_pbconnect.AuditServiceName,
	} {
		mux.Handle("/"+svcName+"/", proxy)
	}
	mux.Handle(connectrpc.EventsPath, proxy)
	mux.HandleFunc(connectrpc.HealthzPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	if err := mountWebUIRoutes(mux, webui.RuntimeBootstrap{
		WorkspaceSlug: firstNonEmpty(cfg.UIWorkspace, opts.Workspace, "default"),
		ProjectSlug:   firstNonEmpty(cfg.UIProject, opts.Project),
		ActorHandle:   firstNonEmpty(cfg.UIActor, opts.Actor, "web-operator"),
	}); err != nil {
		return err
	}

	listenAddr := strings.TrimSpace(cfg.ListenAddr)
	if listenAddr == "" {
		listenAddr = connectrpc.DefaultListenAddr
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listen %q: %w", listenAddr, err)
	}
	if cfg.OnListen != nil {
		cfg.OnListen(ln.Addr().String())
	}

	httpSrv := &http.Server{
		Addr:              ln.Addr().String(),
		Handler:           mux,
		ReadHeaderTimeout: connectrpc.DefaultReadHeaderTimeout,
		ReadTimeout:       connectrpc.DefaultReadTimeout,
		WriteTimeout:      connectrpc.DefaultWriteTimeout,
		IdleTimeout:       connectrpc.DefaultIdleTimeout,
	}
	errCh := make(chan error, 1)
	go func() {
		if serveErr := httpSrv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return errors.Join(httpSrv.Shutdown(shutdownCtx), <-errCh)
	case serveErr := <-errCh:
		return serveErr
	}
}

type appMCPRunner struct {
	rt *Runtime
}

// NewMCPServer builds a functional MCP server wired to the dual-mode runtime.
func (m *appMCPRunner) NewMCPServer(cfg mcp.Config) mcp.Server {
	bridge := &mcpDepsBridge{
		client: &dualModeClient{rt: m.rt},
		fmt:    formatter.NewOutputFormatter(),
	}
	return mcp.NewServer(cfg, bridge)
}

type mcpDepsBridge struct {
	client *dualModeClient
	fmt    *formatter.Formatter
}

func (b *mcpDepsBridge) LanguageExecutor() mcp.LanguageExecutor         { return b }
func (b *mcpDepsBridge) LanguageGuideProvider() mcp.LanguageGuideProvider { return b }
func (b *mcpDepsBridge) ProjectDigestProvider() mcp.ProjectDigestProvider { return b }
func (b *mcpDepsBridge) SearchProvider() mcp.SearchProvider               { return b }

func (b *mcpDepsBridge) Search(ctx context.Context, in mcp.SearchInput) (mcp.SearchOutput, error) {
	scope := strings.ToLower(strings.TrimSpace(in.Scope))
	parsed := language.ParseSearchQuery(in.Query)
	if scope == "" || scope == "all" {
		if parsed.Scope != language.SearchScopeAll {
			scope = string(parsed.Scope)
		} else {
			scope = "all"
		}
	}
	if scope == "sdk" {
		docs := language.SearchCodeModeSDK(in.Query)
		sdkViews := make([]cli.SDKMethodView, 0, len(docs))
		for _, d := range docs {
			sdkViews = append(sdkViews, cli.SDKMethodView{
				Name:      d.Name,
				Category:  d.Category,
				Signature: d.Signature,
				Summary:   d.Summary,
				Example:   d.Example,
			})
		}
		var buf bytes.Buffer
		_ = cli.Render(&buf, cli.FormatCompact, in.MaxTokens, cli.SearchView{
			Query:      in.Query,
			Scope:      "sdk",
			SDKMethods: sdkViews,
		})
		return mcp.SearchOutput{
			FormattedResults: buf.String(),
			SDKMatches:       len(sdkViews),
		}, nil
	}

	view := cli.SearchView{
		Query: in.Query,
		Scope: scope,
	}
	if scope == "all" || scope == "tasks" {
		tRes, err := b.client.ListTasks(ctx, cli.ListTasksInput{
			Workspace: in.WorkspaceID,
			Project:   in.ProjectID,
			State:     in.State,
			Query:     in.Query,
			Priority:  -1,
			Limit:     in.Limit,
		})
		if err != nil {
			return mcp.SearchOutput{}, err
		}
		view.Tasks = tRes.Items
	}
	if (scope == "all" && in.State == "" && len(parsed.Statuses) == 0 && len(parsed.Tags) == 0) || scope == "goals" {
		oRes, err := b.client.ListObjectives(ctx, cli.ListObjectivesInput{
			Workspace: in.WorkspaceID,
			Project:   in.ProjectID,
			Limit:     in.Limit,
		})
		if err != nil {
			return mcp.SearchOutput{}, err
		}
		for _, g := range oRes.Items {
			if parsed.MatchesText(g.Key, g.Title, g.Summary, g.Status, g.Owner) {
				view.Goals = append(view.Goals, g)
			}
		}
	}
	var buf bytes.Buffer
	_ = cli.Render(&buf, cli.FormatCompact, in.MaxTokens, view)
	return mcp.SearchOutput{
		FormattedResults: buf.String(),
		TaskMatches:      len(view.Tasks),
		GoalMatches:      len(view.Goals),
	}, nil
}

func (b *mcpDepsBridge) Execute(ctx context.Context, in mcp.ExecuteScriptInput) (mcp.ExecuteScriptOutput, error) {
	res, err := b.client.ExecuteScript(ctx, cli.ExecuteScriptInput{
		Workspace:      in.WorkspaceID,
		Project:        in.ProjectID,
		Actor:          in.ActorID,
		Script:         in.Script,
		DryRun:         in.DryRun,
		IdempotencyKey: in.IdempotencyKey,
		Density:        string(in.Density),
		MaxTokens:      in.MaxTokens,
	})
	if err != nil {
		return mcp.ExecuteScriptOutput{}, err
	}
	return mcp.ExecuteScriptOutput{
		FormattedSummary: res.FormattedSummary,
		AppliedCount:     res.AppliedCount,
		DryRun:           res.DryRun,
	}, nil
}

func (b *mcpDepsBridge) Guide(ctx context.Context, in mcp.GuideInput) (mcp.GuideOutput, error) {
	g, err := b.client.GetGuide(ctx, string(in.Section))
	if err != nil {
		return mcp.GuideOutput{}, err
	}
	return mcp.GuideOutput{
		Version:  g.Version,
		MIMEType: mcp.ResourceMIMETextMarkdown,
		Markdown: g.Markdown,
	}, nil
}

func (b *mcpDepsBridge) Digest(ctx context.Context, in mcp.DigestInput) (mcp.DigestOutput, error) {
	dig, err := b.client.Digest(ctx, cli.TaskDigestInput{
		Workspace:      in.WorkspaceID,
		Project:        in.ProjectID,
		Objective:      in.ObjectiveID,
		IncludeBlocked: in.IncludeBlocked,
		Limit:          in.Limit,
		MaxTokens:      in.MaxTokens,
	})
	if err != nil {
		return mcp.DigestOutput{}, err
	}
	var buf bytes.Buffer
	_ = cli.Render(&buf, cli.FormatCompact, in.MaxTokens, dig)
	return mcp.DigestOutput{
		FormattedDigest: buf.String(),
		ReadyTasks:      len(dig.ReadyQueue),
		InProgressTasks: len(dig.ActiveLeases),
		BlockedTasks:    len(dig.BlockedTasks),
	}, nil
}
