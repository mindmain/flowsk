// File server.go assembles the HTTP/1.1 + cleartext HTTP/2 (`h2c`) ServeMux,
// `/healthz` endpoint, and graceful-shutdown ConnectRPC server for `flowsk serve`.
package connectrpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/mindmain/flowsk/gen/go/flowsk/audit/v1/flowsk_audit_pbconnect"
	"github.com/mindmain/flowsk/gen/go/flowsk/language/v1/flowsk_language_pbconnect"
	"github.com/mindmain/flowsk/gen/go/flowsk/objective/v1/flowsk_objective_pbconnect"
	"github.com/mindmain/flowsk/gen/go/flowsk/project/v1/flowsk_project_pbconnect"
	"github.com/mindmain/flowsk/gen/go/flowsk/task/v1/flowsk_task_pbconnect"
	"github.com/mindmain/flowsk/gen/go/flowsk/workspace/v1/flowsk_workspace_pbconnect"
	"github.com/mindmain/flowsk/internal/domain/errs"
)

// Default network listener and Slowloris-safe HTTP timeout settings for Flowsk's
// ConnectRPC server.
const (
	// DefaultListenAddr binds exclusively to loopback unless explicitly overridden
	// (for example, `"0.0.0.0:8080"` inside an isolated Docker container).
	DefaultListenAddr = "127.0.0.1:8080"

	// DefaultReadHeaderTimeout bounds HTTP header parsing time to mitigate Slowloris attacks.
	DefaultReadHeaderTimeout = 10 * time.Second

	// DefaultReadTimeout bounds total request body read duration.
	DefaultReadTimeout = 30 * time.Second

	// DefaultWriteTimeout bounds total response write duration for unary RPCs.
	DefaultWriteTimeout = 30 * time.Second

	// DefaultIdleTimeout bounds keep-alive idle time between sequential RPC requests.
	DefaultIdleTimeout = 120 * time.Second

	// DefaultShutdownTimeout bounds graceful in-flight RPC drain duration on stop.
	DefaultShutdownTimeout = 15 * time.Second

	// HealthzPath is the lightweight HTTP readiness/liveness endpoint mounted on NewMux.
	HealthzPath = "/healthz"
)

// Config defines network listener and HTTP/1.1 + HTTP/2 (H2C) runtime parameters
// for the Flowsk ConnectRPC server.
//
// Zero Server Path Footprint Invariant:
//   - Config contains strictly network socket and timeout parameters; host repository
//     filesystem paths are never configured or exposed on the RPC server.
type Config struct {
	// ListenAddr is the TCP `host:port` address to bind (defaults to DefaultListenAddr).
	ListenAddr string

	// EnableH2C enables cleartext HTTP/2 (`h2c`) alongside HTTP/1.1 via Go 1.24's
	// native `http.Protocols` support for Connect and gRPC-compatible clients.
	EnableH2C bool

	// ReadHeaderTimeout bounds request header read time (defaults to DefaultReadHeaderTimeout).
	ReadHeaderTimeout time.Duration

	// ReadTimeout bounds entire request read duration (defaults to DefaultReadTimeout).
	ReadTimeout time.Duration

	// WriteTimeout bounds unary response write duration (defaults to DefaultWriteTimeout).
	WriteTimeout time.Duration

	// IdleTimeout bounds idle keep-alive connection lifetime (defaults to DefaultIdleTimeout).
	IdleTimeout time.Duration

	// ShutdownTimeout bounds graceful drain time when context cancels (defaults to DefaultShutdownTimeout).
	ShutdownTimeout time.Duration

	// OnListen is an optional callback invoked synchronously once the TCP listener binds,
	// receiving the resolved `host:port` address before accepting HTTP requests.
	OnListen func(boundAddr string)
}

// withDefaults returns a copy of c populated with safe production timeout defaults.
func (c Config) withDefaults() Config {
	if c.ListenAddr == "" {
		c.ListenAddr = DefaultListenAddr
	}
	if c.ReadHeaderTimeout <= 0 {
		c.ReadHeaderTimeout = DefaultReadHeaderTimeout
	}
	if c.ReadTimeout <= 0 {
		c.ReadTimeout = DefaultReadTimeout
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = DefaultWriteTimeout
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = DefaultIdleTimeout
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = DefaultShutdownTimeout
	}
	return c
}

// HandlerSet aggregates the six bounded-context ConnectRPC service handlers mounted
// onto the Flowsk HTTP multiplexer.
type HandlerSet struct {
	Workspace flowsk_workspace_pbconnect.WorkspaceServiceHandler
	Project   flowsk_project_pbconnect.ProjectServiceHandler
	Objective flowsk_objective_pbconnect.ObjectiveServiceHandler
	Task      flowsk_task_pbconnect.TaskServiceHandler
	Language  flowsk_language_pbconnect.LanguageServiceHandler
	Audit     flowsk_audit_pbconnect.AuditServiceHandler
}

// Validate checks that every bounded-context ConnectRPC service handler in the set
// is non-nil prior to starting a production server.
func (h HandlerSet) Validate() error {
	const op = "connectrpc.HandlerSet.Validate"
	var violations []errs.FieldViolation
	if h.Workspace == nil {
		violations = append(violations, errs.FieldViolation{
			Field:       "workspace",
			Description: "WorkspaceServiceHandler must not be nil",
		})
	}
	if h.Project == nil {
		violations = append(violations, errs.FieldViolation{
			Field:       "project",
			Description: "ProjectServiceHandler must not be nil",
		})
	}
	if h.Objective == nil {
		violations = append(violations, errs.FieldViolation{
			Field:       "objective",
			Description: "ObjectiveServiceHandler must not be nil",
		})
	}
	if h.Task == nil {
		violations = append(violations, errs.FieldViolation{
			Field:       "task",
			Description: "TaskServiceHandler must not be nil",
		})
	}
	if h.Language == nil {
		violations = append(violations, errs.FieldViolation{
			Field:       "language",
			Description: "LanguageServiceHandler must not be nil",
		})
	}
	if h.Audit == nil {
		violations = append(violations, errs.FieldViolation{
			Field:       "audit",
			Description: "AuditServiceHandler must not be nil",
		})
	}
	if len(violations) > 0 {
		return errs.Validation(op, "incomplete ConnectRPC handler set", violations...)
	}
	return nil
}

// Server defines the lifecycle contract for hosting Flowsk's ConnectRPC HTTP/H2C server.
type Server interface {
	// Addr returns the configured or bound TCP listener address.
	Addr() string

	// Mux returns the underlying `*http.ServeMux` with all ConnectRPC routes mounted.
	Mux() *http.ServeMux

	// Serve binds the TCP listener and serves requests until ctx is canceled or a fatal
	// listener error occurs, draining in-flight RPCs gracefully on shutdown.
	Serve(ctx context.Context) error

	// Shutdown initiates an explicit graceful drain of active HTTP/Connect connections.
	Shutdown(ctx context.Context) error
}

// NewMux constructs a standard `*http.ServeMux` and mounts all six generated Flowsk
// `flowsk_<domain>_pbconnect.New<Domain>ServiceHandler(...)` service prefixes along
// with `/healthz`.
//
// Shared `connect.HandlerOption` values (such as `connect.WithInterceptors(...)` carrying
// `NewAuthContextInterceptor` and `NewValidationInterceptor`) are applied uniformly across
// every registered service.
func NewMux(handlers HandlerSet, opts ...connect.HandlerOption) *http.ServeMux {
	mux := http.NewServeMux()

	if handlers.Workspace != nil {
		path, handler := flowsk_workspace_pbconnect.NewWorkspaceServiceHandler(handlers.Workspace, opts...)
		mux.Handle(path, handler)
	}
	if handlers.Project != nil {
		path, handler := flowsk_project_pbconnect.NewProjectServiceHandler(handlers.Project, opts...)
		mux.Handle(path, handler)
	}
	if handlers.Objective != nil {
		path, handler := flowsk_objective_pbconnect.NewObjectiveServiceHandler(handlers.Objective, opts...)
		mux.Handle(path, handler)
	}
	if handlers.Task != nil {
		path, handler := flowsk_task_pbconnect.NewTaskServiceHandler(handlers.Task, opts...)
		mux.Handle(path, handler)
	}
	if handlers.Language != nil {
		path, handler := flowsk_language_pbconnect.NewLanguageServiceHandler(handlers.Language, opts...)
		mux.Handle(path, handler)
	}
	if handlers.Audit != nil {
		path, handler := flowsk_audit_pbconnect.NewAuditServiceHandler(handlers.Audit, opts...)
		mux.Handle(path, handler)
	}

	mux.HandleFunc(HealthzPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("ok\n"))
		}
	})

	return mux
}

// HTTPServer is the concrete `net/http` + Go 1.24 `http.Protocols` implementation of `Server`.
type HTTPServer struct {
	cfg        Config
	mux        *http.ServeMux
	httpServer *http.Server

	mu        sync.RWMutex
	boundAddr string
}

// Compile-time check ensuring *HTTPServer satisfies Server.
var _ Server = (*HTTPServer)(nil)

// NewServer validates handlers, mounts all six ConnectRPC routes via `NewMux`, and
// configures a hardened `*http.Server` ready to serve HTTP/1.1 and optional H2C traffic.
func NewServer(cfg Config, handlers HandlerSet, opts ...connect.HandlerOption) (*HTTPServer, error) {
	if err := handlers.Validate(); err != nil {
		return nil, err
	}

	resolvedCfg := cfg.withDefaults()
	mux := NewMux(handlers, opts...)

	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	if resolvedCfg.EnableH2C {
		protocols.SetUnencryptedHTTP2(true)
	}

	srv := &http.Server{
		Addr:              resolvedCfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: resolvedCfg.ReadHeaderTimeout,
		ReadTimeout:       resolvedCfg.ReadTimeout,
		WriteTimeout:      resolvedCfg.WriteTimeout,
		IdleTimeout:       resolvedCfg.IdleTimeout,
		Protocols:         protocols,
	}

	return &HTTPServer{
		cfg:        resolvedCfg,
		mux:        mux,
		httpServer: srv,
		boundAddr:  resolvedCfg.ListenAddr,
	}, nil
}

// Addr returns the active TCP listener address (updating to the OS-assigned port when
// configured with `:0` once Serve binds the socket).
func (s *HTTPServer) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.boundAddr
}

// Mux returns the mounted `*http.ServeMux`.
func (s *HTTPServer) Mux() *http.ServeMux {
	return s.mux
}

// Serve opens the TCP listener on `cfg.ListenAddr` and serves ConnectRPC requests until
// ctx is canceled, performing bounded graceful shutdown before returning.
func (s *HTTPServer) Serve(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("connectrpc.HTTPServer.Serve: %w", err)
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("connectrpc.HTTPServer.Serve listen %q: %w", s.cfg.ListenAddr, err)
	}

	s.mu.Lock()
	s.boundAddr = ln.Addr().String()
	s.mu.Unlock()

	if s.cfg.OnListen != nil {
		s.cfg.OnListen(ln.Addr().String())
	}

	errCh := make(chan error, 1)
	go func() {
		if serveErr := s.httpServer.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ShutdownTimeout)
		defer cancel()
		shutdownErr := s.httpServer.Shutdown(shutdownCtx)
		serveErr := <-errCh
		return errors.Join(shutdownErr, serveErr)
	case serveErr := <-errCh:
		if serveErr != nil {
			return fmt.Errorf("connectrpc.HTTPServer.Serve: %w", serveErr)
		}
		return nil
	}
}

// Shutdown gracefully drains active HTTP/Connect connections within ctx's deadline.
func (s *HTTPServer) Shutdown(ctx context.Context) error {
	if err := s.httpServer.Shutdown(ctx); err != nil {
		return fmt.Errorf("connectrpc.HTTPServer.Shutdown: %w", err)
	}
	return nil
}
