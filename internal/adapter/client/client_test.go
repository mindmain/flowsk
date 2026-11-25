// Package client_test verifies ConnectRPC client provider construction, narrow DI
// provider interfaces, URL validation, outbound authentication/identity header
// injection, and live HTTP roundtrips against httptest.NewServer.
package client_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	workspacepb "github.com/mindmain/flowsk/gen/go/flowsk/workspace/v1"
	"github.com/mindmain/flowsk/gen/go/flowsk/workspace/v1/flowsk_workspace_pbconnect"
	"github.com/mindmain/flowsk/internal/adapter/client"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
)

type stubWorkspaceHandler struct {
	flowsk_workspace_pbconnect.UnimplementedWorkspaceServiceHandler

	mu          sync.Mutex
	lastHeaders http.Header
}

func (h *stubWorkspaceHandler) GetWorkspace(
	_ context.Context,
	req *connect.Request[workspacepb.GetWorkspaceRequest],
) (*connect.Response[workspacepb.GetWorkspaceResponse], error) {
	h.mu.Lock()
	h.lastHeaders = req.Header().Clone()
	h.mu.Unlock()

	return connect.NewResponse(&workspacepb.GetWorkspaceResponse{
		Workspace: &workspacepb.Workspace{
			Id:   req.Msg.GetWorkspaceId(),
			Slug: "acme-remote",
			Name: "Acme Remote Workspace",
		},
	}), nil
}

func (h *stubWorkspaceHandler) capturedHeaders() http.Header {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastHeaders.Clone()
}

// TestProvider_ClientsAndHeaderInjectionLiveRoundtrip verifies that NewProvider
// initializes all 6 ConnectRPC service clients, injects configured X-Flowsk-* and
// Authorization Bearer headers, and executes live RPC roundtrips against httptest.NewServer.
func TestProvider_ClientsAndHeaderInjectionLiveRoundtrip(t *testing.T) {
	t.Parallel()

	handler := &stubWorkspaceHandler{}
	mux := http.NewServeMux()
	path, h := flowsk_workspace_pbconnect.NewWorkspaceServiceHandler(handler)
	mux.Handle(path, h)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	wsID := uuid.New().String()
	actorID := uuid.New().String()

	provider := client.NewProvider(client.Config{
		BaseURL:     srv.URL + "/",
		Timeout:     5 * time.Second,
		WorkspaceID: wsID,
		ActorID:     actorID,
		ActorKind:   "agent",
		ActorHandle: "agent-alpha",
		AuthToken:   "secret-rpc-token-123",
		HTTPClient:  srv.Client(),
	})

	if provider.BaseURL() != srv.URL {
		t.Fatalf("expected trailing slash trimmed in BaseURL %q, got %q", srv.URL, provider.BaseURL())
	}
	if provider.WorkspaceClient() == nil ||
		provider.ProjectClient() == nil ||
		provider.ObjectiveClient() == nil ||
		provider.TaskClient() == nil ||
		provider.LanguageClient() == nil ||
		provider.AuditClient() == nil {
		t.Fatal("expected all 6 ConnectRPC service clients initialized non-nil")
	}

	resp, err := provider.WorkspaceClient().GetWorkspace(
		context.Background(),
		connect.NewRequest(&workspacepb.GetWorkspaceRequest{WorkspaceId: wsID}),
	)
	if err != nil {
		t.Fatalf("live GetWorkspace roundtrip failed: %v", err)
	}
	if resp.Msg.GetWorkspace().GetSlug() != "acme-remote" {
		t.Fatalf("unexpected workspace slug: %q", resp.Msg.GetWorkspace().GetSlug())
	}

	gotHeaders := handler.capturedHeaders()
	if got := gotHeaders.Get(client.HeaderWorkspaceID); got != wsID {
		t.Fatalf("expected %s=%q, got %q", client.HeaderWorkspaceID, wsID, got)
	}
	if got := gotHeaders.Get(client.HeaderActorID); got != actorID {
		t.Fatalf("expected %s=%q, got %q", client.HeaderActorID, actorID, got)
	}
	if got := gotHeaders.Get(client.HeaderActorKind); got != "agent" {
		t.Fatalf("expected %s=%q, got %q", client.HeaderActorKind, "agent", got)
	}
	if got := gotHeaders.Get(client.HeaderActorHandle); got != "agent-alpha" {
		t.Fatalf("expected %s=%q, got %q", client.HeaderActorHandle, "agent-alpha", got)
	}
	if got := gotHeaders.Get(client.HeaderAuthorization); got != "Bearer secret-rpc-token-123" {
		t.Fatalf("expected %s=%q, got %q", client.HeaderAuthorization, "Bearer secret-rpc-token-123", got)
	}
}

// TestProvider_AuthCtxIdentityPropagation verifies that an authctx.Identity
// bound on context.Context propagates X-Flowsk-* headers on outbound calls.
func TestProvider_AuthCtxIdentityPropagation(t *testing.T) {
	t.Parallel()

	handler := &stubWorkspaceHandler{}
	mux := http.NewServeMux()
	path, h := flowsk_workspace_pbconnect.NewWorkspaceServiceHandler(handler)
	mux.Handle(path, h)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	provider := client.NewProvider(client.Config{
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
	})

	id := authctx.Identity{
		WorkspaceID: uuid.New(),
		ActorID:     uuid.New(),
		Kind:        authctx.ActorKindHuman,
		Handle:      "mario",
	}
	ctx := authctx.WithIdentity(context.Background(), id)

	_, err := provider.WorkspaceClient().GetWorkspace(
		ctx,
		connect.NewRequest(&workspacepb.GetWorkspaceRequest{WorkspaceId: id.WorkspaceID.String()}),
	)
	if err != nil {
		t.Fatalf("GetWorkspace with authctx.Identity failed: %v", err)
	}

	gotHeaders := handler.capturedHeaders()
	if got := gotHeaders.Get(client.HeaderWorkspaceID); got != id.WorkspaceID.String() {
		t.Fatalf("expected context WorkspaceID %q, got %q", id.WorkspaceID, got)
	}
	if got := gotHeaders.Get(client.HeaderActorID); got != id.ActorID.String() {
		t.Fatalf("expected context ActorID %q, got %q", id.ActorID, got)
	}
	if got := gotHeaders.Get(client.HeaderActorKind); got != "human" {
		t.Fatalf("expected context ActorKind %q, got %q", "human", got)
	}
	if got := gotHeaders.Get(client.HeaderActorHandle); got != "mario" {
		t.Fatalf("expected context ActorHandle %q, got %q", "mario", got)
	}
}

// TestValidateBaseURLAndNewValidatedProvider verifies URL scheme/host validation
// and rejection of local filesystem paths or unsupported schemes.
func TestValidateBaseURLAndNewValidatedProvider(t *testing.T) {
	t.Parallel()

	validCases := map[string]string{
		"http://127.0.0.1:8080/":                 "http://127.0.0.1:8080",
		"https://flowsk.example.com:8443/api///": "https://flowsk.example.com:8443/api",
	}
	for raw, want := range validCases {
		got, err := client.ValidateBaseURL(raw)
		if err != nil {
			t.Fatalf("ValidateBaseURL(%q) unexpected error: %v", raw, err)
		}
		if got != want {
			t.Fatalf("ValidateBaseURL(%q) = %q, want %q", raw, got, want)
		}
	}

	invalidCases := []string{
		"",
		"   ",
		"ftp://example.com:21",
		"file:///Users/mario/Desktop/repo",
		"/Users/mario/Desktop/repo",
		"http://",
		"https:///missing-host",
	}
	for _, bad := range invalidCases {
		if _, err := client.ValidateBaseURL(bad); !errors.Is(err, errs.ErrValidation) {
			t.Fatalf("ValidateBaseURL(%q) expected ErrValidation, got %v", bad, err)
		}
	}

	if _, err := client.NewValidatedProvider(client.Config{BaseURL: "file:///tmp/flowsk.sock"}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("NewValidatedProvider expected ErrValidation for file:// URL, got %v", err)
	}

	defaultProv, err := client.NewValidatedProvider(client.Config{})
	if err != nil {
		t.Fatalf("NewValidatedProvider with empty BaseURL should default to DefaultBaseURL, got error: %v", err)
	}
	if defaultProv.BaseURL() != client.DefaultBaseURL {
		t.Fatalf("expected BaseURL %q, got %q", client.DefaultBaseURL, defaultProv.BaseURL())
	}
}
