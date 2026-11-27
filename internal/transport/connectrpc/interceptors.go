// File interceptors.go implements ConnectRPC unary interceptors for header/token
// caller identity extraction (`authctx.Identity`) and `protovalidate` verification.
package connectrpc

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"google.golang.org/protobuf/proto"
)

// Canonical HTTP metadata header names used by Flowsk CLI and remote agent harnesses
// to transmit authenticated workspace/actor credentials to the ConnectRPC transport edge.
const (
	// HeaderWorkspaceID carries the UUID of the caller's authorized tenant workspace.
	HeaderWorkspaceID = "X-Flowsk-Workspace-Id"

	// HeaderActorID carries the UUID of the authenticated human operator or AI agent.
	HeaderActorID = "X-Flowsk-Actor-Id"

	// HeaderActorKind carries the principal classification (`"human"` or `"agent"`).
	HeaderActorKind = "X-Flowsk-Actor-Kind"

	// HeaderActorHandle carries the human-readable actor handle (for example, `"mario"` or `"agent-01"`).
	HeaderActorHandle = "X-Flowsk-Actor-Handle"

	// HeaderClientSurface optionally identifies the calling client surface (for example, `"webui"`),
	// allowing human Web UI interactions to suppress agent `last_signal_at` updates.
	HeaderClientSurface = "X-Flowsk-Client"

	// HeaderSuppressSignal explicitly suppresses `last_signal_at` updates on internal
	// pre-mutation lookups over ConnectRPC when set to `"1"` or `"true"`.
	HeaderSuppressSignal = "X-Flowsk-Suppress-Signal"
)

// ShouldSuppressSignalHeaders reports whether inbound HTTP headers originate from the
// browser Web UI or carry an explicit internal signal-suppression flag.
func ShouldSuppressSignalHeaders(headers http.Header) bool {
	if headers == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(headers.Get(HeaderClientSurface)), "webui") {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(headers.Get(HeaderSuppressSignal))) {
	case "1", "true", "yes":
		return true
	}
	return strings.HasPrefix(strings.TrimSpace(headers.Get("X-Request-Id")), "ui-")
}

// ExtractBearerToken parses an HTTP `Authorization` header according to RFC 9110,
// matching the `"Bearer"` scheme case-insensitively without mutating or stripping
// `"bearer"` prefixes from the token credential itself. Scheme-less raw tokens and
// empty credentials return `("", false)`.
func ExtractBearerToken(authHeader string) (string, bool) {
	scheme, rest, ok := strings.Cut(strings.TrimSpace(authHeader), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token := strings.TrimSpace(rest)
	if token == "" {
		return "", false
	}
	return token, true
}

// IdentityResolver authenticates inbound ConnectRPC request metadata and resolves an
// immutable `authctx.Identity` prior to invoking any bounded-context service handler.
//
// Implementations may verify bearer tokens, session cookies, mTLS client certificates,
// or trusted local/header credentials depending on deployment topology.
type IdentityResolver interface {
	// ResolveIdentity inspects inbound request headers for the target Connect procedure
	// and returns a validated `authctx.Identity` or an `errs.ErrUnauthenticated` /
	// `errs.ErrPermissionDenied` domain error.
	ResolveIdentity(ctx context.Context, spec connect.Spec, headers http.Header) (authctx.Identity, error)
}

// IdentityResolverFunc adapts an ordinary function to the `IdentityResolver` interface.
type IdentityResolverFunc func(ctx context.Context, spec connect.Spec, headers http.Header) (authctx.Identity, error)

// ResolveIdentity invokes fn(ctx, spec, headers).
func (fn IdentityResolverFunc) ResolveIdentity(ctx context.Context, spec connect.Spec, headers http.Header) (authctx.Identity, error) {
	return fn(ctx, spec, headers)
}

// HeaderIdentityResolver is the baseline `IdentityResolver` skeleton that extracts
// `X-Flowsk-*` identity headers and validates the resulting `authctx.Identity`.
//
// Security Note:
//   - Intended for trusted local workstation sockets, authenticated reverse-proxy edges,
//     and transport unit tests; networked multi-tenant deployments wrap or replace this
//     resolver with cryptographic token verification.
type HeaderIdentityResolver struct {
	// PublicProcedures optionally lists Connect procedure paths (such as
	// `"/flowsk.language.v1.LanguageService/GetLanguageGuide"`) that may proceed
	// without caller credentials when no identity headers are supplied.
	PublicProcedures map[string]bool
}

// Compile-time check ensuring *HeaderIdentityResolver satisfies IdentityResolver.
var _ IdentityResolver = (*HeaderIdentityResolver)(nil)

// ResolveIdentity parses and validates `X-Flowsk-*` headers into an `authctx.Identity`.
func (r *HeaderIdentityResolver) ResolveIdentity(_ context.Context, spec connect.Spec, headers http.Header) (authctx.Identity, error) {
	const op = "connectrpc.HeaderIdentityResolver.ResolveIdentity"

	rawWorkspaceID := strings.TrimSpace(headers.Get(HeaderWorkspaceID))
	rawActorID := strings.TrimSpace(headers.Get(HeaderActorID))
	rawKind := strings.TrimSpace(headers.Get(HeaderActorKind))
	rawHandle := strings.TrimSpace(headers.Get(HeaderActorHandle))

	if rawWorkspaceID == "" && rawActorID == "" && rawKind == "" && rawHandle == "" {
		if r != nil && r.PublicProcedures[spec.Procedure] {
			return authctx.Identity{}, nil
		}
		return authctx.Identity{}, errs.New(errs.ErrUnauthenticated, op, "missing authentication credentials")
	}

	workspaceID, err := uuid.Parse(rawWorkspaceID)
	if err != nil || workspaceID == uuid.Nil {
		return authctx.Identity{}, errs.New(errs.ErrUnauthenticated, op, "invalid workspace credential")
	}

	actorID, err := uuid.Parse(rawActorID)
	if err != nil || actorID == uuid.Nil {
		return authctx.Identity{}, errs.New(errs.ErrUnauthenticated, op, "invalid actor credential")
	}

	identity := authctx.Identity{
		WorkspaceID: workspaceID,
		ActorID:     actorID,
		Kind:        authctx.ActorKind(strings.ToLower(rawKind)),
		Handle:      rawHandle,
	}
	if err := identity.Validate(); err != nil {
		return authctx.Identity{}, errs.Wrap(errs.ErrUnauthenticated, op, "malformed caller identity credentials", err)
	}
	return identity, nil
}

// NewAuthContextInterceptor constructs a Connect unary interceptor that resolves the
// caller's `authctx.Identity` at the transport boundary and injects it into `ctx` via
// `authctx.WithIdentity`.
//
// Behavior:
//   - If ctx already carries a valid `authctx.Identity` (for example, injected by an
//     upstream HTTP middleware or in-process test harness), it is preserved.
//   - If resolver is nil, a strict `&HeaderIdentityResolver{}` is used by default.
//   - Any authentication error is translated deterministically via `MapDomainError`.
func NewAuthContextInterceptor(resolver IdentityResolver) connect.UnaryInterceptorFunc {
	if resolver == nil {
		resolver = &HeaderIdentityResolver{}
	}

	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if err := ctx.Err(); err != nil {
				return nil, MapDomainError(err)
			}

			if ShouldSuppressSignalHeaders(req.Header()) {
				ctx = authctx.WithSuppressTaskSignal(ctx)
			}

			if _, err := authctx.FromContext(ctx); err == nil {
				return next(ctx, req)
			}

			identity, err := resolver.ResolveIdentity(ctx, req.Spec(), req.Header())
			if err != nil {
				return nil, MapDomainError(err)
			}
			if identity != (authctx.Identity{}) {
				ctx = authctx.WithIdentity(ctx, identity)
			}
			return next(ctx, req)
		})
	})
}

// ProtoValidator abstracts structural Protobuf request validation backed by
// `github.com/bufbuild/protovalidate-go` (`*protovalidate.Validator` satisfies this
// interface directly via structural typing) or custom test validators, complementing
// `connectrpc.com/validate` interceptors.
type ProtoValidator interface {
	// Validate evaluates `buf.validate` CEL/field constraints on msg, returning nil
	// when valid or a descriptive validation error when constraints fail.
	Validate(msg proto.Message) error
}

// ProtoValidatorFunc adapts an ordinary function to the `ProtoValidator` interface.
type ProtoValidatorFunc func(msg proto.Message) error

// Validate invokes fn(msg).
func (fn ProtoValidatorFunc) Validate(msg proto.Message) error {
	return fn(msg)
}

// NewValidationInterceptor constructs a Connect unary interceptor that enforces
// Protobuf request schema constraints before any handler or domain service executes.
//
// Contract:
//   - Accepts any `ProtoValidator` implementation (including `protovalidate.Validator`
//     from `github.com/bufbuild/protovalidate-go`); callers may also pass
//     `connectrpc.com/validate.NewInterceptor()` directly in `connect.WithInterceptors(...)`.
//   - Rejects non-Protobuf or nil request payloads with `connect.CodeInvalidArgument`.
//   - Translates constraint violations into `connect.CodeInvalidArgument` without
//     invoking downstream handlers.
func NewValidationInterceptor(validator ProtoValidator) connect.UnaryInterceptorFunc {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if err := ctx.Err(); err != nil {
				return nil, MapDomainError(err)
			}
			if validator == nil {
				return next(ctx, req)
			}

			raw := req.Any()
			if raw == nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("request message must not be nil"))
			}
			msg, ok := raw.(proto.Message)
			if !ok {
				return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("request payload is not a protobuf message"))
			}
			if err := validator.Validate(msg); err != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, errors.New(err.Error()))
			}
			return next(ctx, req)
		})
	})
}
