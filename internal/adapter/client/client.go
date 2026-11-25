// Package client provides fine-grained Buf Connect (ConnectRPC) client
// adapters for remote CLI and MCP invocations against a Flowsk server.
//
// In alignment with strict Dependency Injection segregation rules, consumers
// depend only on the single-service accessor interface they require (for
// example, TaskClientProvider or LanguageClientProvider) and resolve concrete
// ConnectRPC clients inside their constructor (New(...)).
package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/mindmain/flowsk/gen/go/flowsk/audit/v1/flowsk_audit_pbconnect"
	"github.com/mindmain/flowsk/gen/go/flowsk/language/v1/flowsk_language_pbconnect"
	"github.com/mindmain/flowsk/gen/go/flowsk/objective/v1/flowsk_objective_pbconnect"
	"github.com/mindmain/flowsk/gen/go/flowsk/project/v1/flowsk_project_pbconnect"
	"github.com/mindmain/flowsk/gen/go/flowsk/task/v1/flowsk_task_pbconnect"
	"github.com/mindmain/flowsk/gen/go/flowsk/workspace/v1/flowsk_workspace_pbconnect"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
)

// DefaultBaseURL is the default local ConnectRPC endpoint used when running
// Flowsk on a developer workstation or attached Docker container.
const DefaultBaseURL = "http://127.0.0.1:8080"

// Canonical HTTP header names injected by the outbound Flowsk ConnectRPC client.
const (
	// HeaderWorkspaceID carries the caller's authorized tenant workspace UUID.
	HeaderWorkspaceID = "X-Flowsk-Workspace-Id"

	// HeaderActorID carries the caller's human or agent actor UUID.
	HeaderActorID = "X-Flowsk-Actor-Id"

	// HeaderActorKind carries the caller's actor classification ("human" or "agent").
	HeaderActorKind = "X-Flowsk-Actor-Kind"

	// HeaderActorHandle carries the caller's actor handle (for example, "mario" or "agent-1").
	HeaderActorHandle = "X-Flowsk-Actor-Handle"

	// HeaderAuthorization carries the standard HTTP Bearer credential.
	HeaderAuthorization = "Authorization"
)

// Config configures the ConnectRPC HTTP transport adapter used by CLI and
// remote MCP clients.
//
// Note: Config intentionally carries zero target-repository filesystem paths;
// local project directory resolution happens strictly on the client machine
// prior to constructing logical workspace/project RPC calls.
type Config struct {
	// BaseURL is the root HTTP/HTTPS URL of the Flowsk ConnectRPC server.
	BaseURL string

	// Timeout bounds individual unary RPC calls when the caller's context
	// does not already carry an earlier deadline.
	Timeout time.Duration

	// ActorID is an optional default actor UUID injected via request headers
	// for multi-agent attribution.
	ActorID string

	// ActorKind is an optional default actor classification ("human" or "agent")
	// injected via X-Flowsk-Actor-Kind.
	ActorKind string

	// ActorHandle is an optional default actor handle injected via X-Flowsk-Actor-Handle.
	ActorHandle string

	// WorkspaceID is an optional default workspace UUID injected via request
	// headers for tenant-scoped routing.
	WorkspaceID string

	// AuthToken is an optional bearer authentication credential injected via
	// the Authorization header ("Bearer <token>"). Never emitted in String() or Redacted().
	AuthToken string

	// HTTPClient overrides the underlying Connect HTTPClient implementation
	// (useful for in-memory httptest servers and custom Unix domain sockets on
	// shared Docker volumes).
	HTTPClient connect.HTTPClient

	// ClientOptions appends custom Connect client options such as auth or
	// telemetry interceptors.
	ClientOptions []connect.ClientOption
}

// Redacted returns a shallow copy of Config with AuthToken masked.
func (c Config) Redacted() Config {
	out := c
	if strings.TrimSpace(out.AuthToken) != "" {
		out.AuthToken = "REDACTED"
	}
	return out
}

// String renders a diagnostic representation of Config with AuthToken masked.
func (c Config) String() string {
	r := c.Redacted()
	return fmt.Sprintf(
		"client.Config{BaseURL:%q, WorkspaceID:%q, ActorID:%q, ActorKind:%q, ActorHandle:%q, AuthToken:%q, Timeout:%s}",
		r.BaseURL,
		r.WorkspaceID,
		r.ActorID,
		r.ActorKind,
		r.ActorHandle,
		r.AuthToken,
		r.Timeout,
	)
}

// ValidateBaseURL verifies that rawURL is a valid HTTP or HTTPS server endpoint
// with a non-empty host (rejecting local file paths and unsupported schemes)
// and returns the normalized URL with trailing slashes removed.
func ValidateBaseURL(rawURL string) (string, error) {
	const op = "client.ValidateBaseURL"
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return "", errs.Validation(op, "server base URL is required", errs.FieldViolation{
			Field:       "base_url",
			Description: "base_url must not be empty",
		})
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", errs.Validation(op, "malformed server base URL", errs.FieldViolation{
			Field:       "base_url",
			Description: fmt.Sprintf("invalid URL syntax: %v", err),
		})
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errs.Validation(op, "unsupported server URL scheme", errs.FieldViolation{
			Field:       "base_url",
			Description: fmt.Sprintf("scheme %q must be http or https", parsed.Scheme),
		})
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return "", errs.Validation(op, "server base URL host is required", errs.FieldViolation{
			Field:       "base_url",
			Description: "base_url must include a non-empty host",
		})
	}
	if parsed.User != nil {
		return "", errs.Validation(op, "embedded URL credentials are not permitted", errs.FieldViolation{
			Field:       "base_url",
			Description: "supply credentials via AuthToken instead of URL userinfo",
		})
	}

	return strings.TrimRight(trimmed, "/"), nil
}

// Validate verifies Config URL and timeout constraints.
func (c Config) Validate() error {
	const op = "client.Config.Validate"
	rawURL := strings.TrimSpace(c.BaseURL)
	if rawURL != "" {
		if _, err := ValidateBaseURL(rawURL); err != nil {
			return err
		}
	}
	if c.Timeout < 0 {
		return errs.Validation(op, "negative client timeout", errs.FieldViolation{
			Field:       "timeout",
			Description: "timeout must be >= 0",
		})
	}
	return nil
}

// NewHeaderInterceptor constructs an outbound Connect unary client interceptor
// that injects `X-Flowsk-*` workspace/actor headers (preferring in-flight
// `authctx.Identity` values on `ctx` and falling back to `cfg` defaults) plus
// the `Authorization: Bearer <token>` header when `cfg.AuthToken` is configured.
func NewHeaderInterceptor(cfg Config) connect.UnaryInterceptorFunc {
	defaultWS := strings.TrimSpace(cfg.WorkspaceID)
	defaultActorID := strings.TrimSpace(cfg.ActorID)
	defaultActorKind := strings.TrimSpace(cfg.ActorKind)
	defaultActorHandle := strings.TrimSpace(cfg.ActorHandle)
	authHeader := formatBearerHeader(cfg.AuthToken)

	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			headers := req.Header()

			if id, err := authctx.FromContext(ctx); err == nil {
				setHeaderIfEmpty(headers, HeaderWorkspaceID, id.WorkspaceID.String())
				setHeaderIfEmpty(headers, HeaderActorID, id.ActorID.String())
				setHeaderIfEmpty(headers, HeaderActorKind, string(id.Kind))
				setHeaderIfEmpty(headers, HeaderActorHandle, id.Handle)
			}

			setHeaderIfEmpty(headers, HeaderWorkspaceID, defaultWS)
			setHeaderIfEmpty(headers, HeaderActorID, defaultActorID)
			setHeaderIfEmpty(headers, HeaderActorKind, defaultActorKind)
			setHeaderIfEmpty(headers, HeaderActorHandle, defaultActorHandle)
			setHeaderIfEmpty(headers, HeaderAuthorization, authHeader)

			return next(ctx, req)
		})
	})
}

func setHeaderIfEmpty(h http.Header, key, val string) {
	if val != "" && strings.TrimSpace(h.Get(key)) == "" {
		h.Set(key, val)
	}
}

func formatBearerHeader(rawToken string) string {
	tok := strings.TrimSpace(rawToken)
	if tok == "" {
		return ""
	}
	if len(tok) >= 7 && strings.EqualFold(tok[:7], "Bearer ") {
		return "Bearer " + strings.TrimSpace(tok[7:])
	}
	return "Bearer " + tok
}

// WorkspaceClientProvider exposes the generated WorkspaceService Connect client.
type WorkspaceClientProvider interface {
	WorkspaceClient() flowsk_workspace_pbconnect.WorkspaceServiceClient
}

// ProjectClientProvider exposes the generated ProjectService Connect client.
type ProjectClientProvider interface {
	ProjectClient() flowsk_project_pbconnect.ProjectServiceClient
}

// ObjectiveClientProvider exposes the generated ObjectiveService Connect client.
type ObjectiveClientProvider interface {
	ObjectiveClient() flowsk_objective_pbconnect.ObjectiveServiceClient
}

// TaskClientProvider exposes the generated TaskService Connect client.
type TaskClientProvider interface {
	TaskClient() flowsk_task_pbconnect.TaskServiceClient
}

// LanguageClientProvider exposes the generated LanguageService Connect client
// used for executing sandboxed Cloudflare Code Mode ES6 JavaScript scripts and
// fetching the on-demand TypeScript SDK specification.
type LanguageClientProvider interface {
	LanguageClient() flowsk_language_pbconnect.LanguageServiceClient
}

// AuditClientProvider exposes the generated AuditService Connect client.
type AuditClientProvider interface {
	AuditClient() flowsk_audit_pbconnect.AuditServiceClient
}

// Provider aggregates all typed Buf Connect service clients over a shared
// HTTP transport pool. Higher-level packages should accept narrow provider
// interfaces (such as TaskClientProvider) rather than *Provider directly.
type Provider struct {
	baseURL         string
	workspaceClient flowsk_workspace_pbconnect.WorkspaceServiceClient
	projectClient   flowsk_project_pbconnect.ProjectServiceClient
	objectiveClient flowsk_objective_pbconnect.ObjectiveServiceClient
	taskClient      flowsk_task_pbconnect.TaskServiceClient
	languageClient  flowsk_language_pbconnect.LanguageServiceClient
	auditClient     flowsk_audit_pbconnect.AuditServiceClient
}

// Compile-time checks ensuring *Provider satisfies all segregated client provider interfaces.
var (
	_ WorkspaceClientProvider = (*Provider)(nil)
	_ ProjectClientProvider   = (*Provider)(nil)
	_ ObjectiveClientProvider = (*Provider)(nil)
	_ TaskClientProvider      = (*Provider)(nil)
	_ LanguageClientProvider  = (*Provider)(nil)
	_ AuditClientProvider     = (*Provider)(nil)
)

// NewValidatedProvider validates cfg (including HTTP/HTTPS BaseURL constraints)
// prior to constructing a Provider.
func NewValidatedProvider(cfg Config) (*Provider, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return NewProvider(cfg), nil
}

// NewProvider constructs and eagerly initializes all typed ConnectRPC clients
// against the configured Flowsk server endpoint with outbound identity and
// bearer token header injection wired.
func NewProvider(cfg Config) *Provider {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		httpClient = &http.Client{Timeout: timeout}
	}

	opts := make([]connect.ClientOption, 0, len(cfg.ClientOptions)+1)
	opts = append(opts, connect.WithInterceptors(NewHeaderInterceptor(cfg)))
	opts = append(opts, cfg.ClientOptions...)

	return &Provider{
		baseURL:         baseURL,
		workspaceClient: flowsk_workspace_pbconnect.NewWorkspaceServiceClient(httpClient, baseURL, opts...),
		projectClient:   flowsk_project_pbconnect.NewProjectServiceClient(httpClient, baseURL, opts...),
		objectiveClient: flowsk_objective_pbconnect.NewObjectiveServiceClient(httpClient, baseURL, opts...),
		taskClient:      flowsk_task_pbconnect.NewTaskServiceClient(httpClient, baseURL, opts...),
		languageClient:  flowsk_language_pbconnect.NewLanguageServiceClient(httpClient, baseURL, opts...),
		auditClient:     flowsk_audit_pbconnect.NewAuditServiceClient(httpClient, baseURL, opts...),
	}
}

// BaseURL returns the normalized Flowsk server root URL.
func (p *Provider) BaseURL() string {
	return p.baseURL
}

// WorkspaceClient returns the pre-initialized WorkspaceService Connect client.
func (p *Provider) WorkspaceClient() flowsk_workspace_pbconnect.WorkspaceServiceClient {
	return p.workspaceClient
}

// ProjectClient returns the pre-initialized ProjectService Connect client.
func (p *Provider) ProjectClient() flowsk_project_pbconnect.ProjectServiceClient {
	return p.projectClient
}

// ObjectiveClient returns the pre-initialized ObjectiveService Connect client.
func (p *Provider) ObjectiveClient() flowsk_objective_pbconnect.ObjectiveServiceClient {
	return p.objectiveClient
}

// TaskClient returns the pre-initialized TaskService Connect client.
func (p *Provider) TaskClient() flowsk_task_pbconnect.TaskServiceClient {
	return p.taskClient
}

// LanguageClient returns the pre-initialized LanguageService Connect client.
func (p *Provider) LanguageClient() flowsk_language_pbconnect.LanguageServiceClient {
	return p.languageClient
}

// AuditClient returns the pre-initialized AuditService Connect client.
func (p *Provider) AuditClient() flowsk_audit_pbconnect.AuditServiceClient {
	return p.auditClient
}
