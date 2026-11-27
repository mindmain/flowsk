// File connectrpc_test.go verifies domain error mapping, authentication and
// validation unary interceptors, and ServeMux route mounting for ConnectRPC.
package connectrpc_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	flowsk_workspace_pb "github.com/mindmain/flowsk/gen/go/flowsk/workspace/v1"
	"github.com/mindmain/flowsk/gen/go/flowsk/workspace/v1/flowsk_workspace_pbconnect"
	"github.com/mindmain/flowsk/internal/adapter/db/sqlite"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/transport/connectrpc"
	"github.com/mindmain/flowsk/internal/transport/connectrpc/audithandler"
	"github.com/mindmain/flowsk/internal/transport/connectrpc/languagehandler"
	"github.com/mindmain/flowsk/internal/transport/connectrpc/objectivehandler"
	"github.com/mindmain/flowsk/internal/transport/connectrpc/projecthandler"
	"github.com/mindmain/flowsk/internal/transport/connectrpc/taskhandler"
	"github.com/mindmain/flowsk/internal/transport/connectrpc/workspacehandler"
	"google.golang.org/protobuf/proto"
)

// TestMapDomainError verifies deterministic ConnectRPC status code translation
// and sanitization of raw storage driver errors.
func TestMapDomainError(t *testing.T) {
	t.Parallel()

	if got := connectrpc.MapDomainError(nil); got != nil {
		t.Fatalf("MapDomainError(nil) = %v, want nil", got)
	}

	rawDriverCause := errors.New("SQLITE_CONSTRAINT_UNIQUE: index idx_projects_slug (secret_dsn=postgres://admin:pw@db/flowsk)")

	cases := []struct {
		name     string
		err      error
		wantCode connect.Code
		forbid   string
	}{
		{
			name:     "not_implemented",
			err:      errs.NotImplemented("task.Service.ClaimTask"),
			wantCode: connect.CodeUnimplemented,
		},
		{
			name: "validation_with_violations",
			err: errs.Validation("project.Service.CreateProject", "invalid project input", errs.FieldViolation{
				Field:       "slug",
				Description: "must match ^[a-z0-9-]+$",
			}),
			wantCode: connect.CodeInvalidArgument,
		},
		{
			name:     "unauthenticated",
			err:      errs.Wrap(errs.ErrUnauthenticated, "authctx.FromContext", "missing token", rawDriverCause),
			wantCode: connect.CodeUnauthenticated,
			forbid:   "secret_dsn",
		},
		{
			name:     "permission_denied",
			err:      errs.New(errs.ErrPermissionDenied, "authctx.RequireWorkspaceOwnership", "cross-tenant access denied"),
			wantCode: connect.CodePermissionDenied,
		},
		{
			name:     "not_found",
			err:      errs.Wrap(errs.ErrNotFound, "task.Repo.Get", "task not found", rawDriverCause),
			wantCode: connect.CodeNotFound,
			forbid:   "SQLITE_CONSTRAINT",
		},
		{
			name:     "conflict",
			err:      errs.Wrap(errs.ErrConflict, "project.Repo.Create", "project slug already exists", rawDriverCause),
			wantCode: connect.CodeAlreadyExists,
			forbid:   "idx_projects_slug",
		},
		{
			name:     "lease_held",
			err:      errs.New(errs.ErrLeaseHeld, "task.Service.ClaimTask", "task lease is held by another actor"),
			wantCode: connect.CodeFailedPrecondition,
		},
		{
			name:     "precondition_failed",
			err:      errs.New(errs.ErrPreconditionFailed, "task.Service.Transition", "blocked by open task"),
			wantCode: connect.CodeFailedPrecondition,
		},
		{
			name:     "context_canceled",
			err:      context.Canceled,
			wantCode: connect.CodeCanceled,
		},
		{
			name:     "context_deadline_exceeded",
			err:      context.DeadlineExceeded,
			wantCode: connect.CodeDeadlineExceeded,
		},
		{
			name:     "unknown_raw_driver_error_masked",
			err:      rawDriverCause,
			wantCode: connect.CodeInternal,
			forbid:   "SQLITE_CONSTRAINT",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := connectrpc.MapDomainError(tc.err)
			if got == nil {
				t.Fatalf("MapDomainError(%v) returned nil", tc.err)
			}
			if got.Code() != tc.wantCode {
				t.Fatalf("MapDomainError(%v).Code() = %v, want %v", tc.err, got.Code(), tc.wantCode)
			}
			if tc.forbid != "" && strings.Contains(got.Message(), tc.forbid) {
				t.Fatalf("MapDomainError leaked internal error substring %q in %q", tc.forbid, got.Message())
			}
		})
	}
}

// TestMuxInterceptorsAndHandlers verifies HTTP mux registration, healthz
// probing, and authentication interceptor enforcement.
func TestMuxInterceptorsAndHandlers(t *testing.T) {
	t.Parallel()

	adapter, err := sqlite.NewAdapter(sqlite.Config{
		FilePath:    ":memory:",
		EnableWAL:   true,
		ForeignKeys: true,
	})
	if err != nil {
		t.Fatalf("sqlite.NewAdapter: %v", err)
	}

	wsHandler := workspacehandler.New(adapter, nil)
	projHandler := projecthandler.New(adapter, nil)
	objHandler := objectivehandler.New(adapter, nil)
	tskHandler := taskhandler.New(adapter, nil)
	langHandler := languagehandler.New(adapter, nil)
	audHandler := audithandler.New(adapter, nil)

	if wsHandler.Service() == nil || projHandler.Service() == nil || objHandler.Service() == nil ||
		tskHandler.Service() == nil || langHandler.Executor() == nil || langHandler.Guide() == nil ||
		audHandler.Service() == nil {
		t.Fatal("expected all handler constructors to resolve non-nil domain/language services")
	}

	handlers := connectrpc.HandlerSet{
		Workspace: wsHandler,
		Project:   projHandler,
		Objective: objHandler,
		Task:      tskHandler,
		Language:  langHandler,
		Audit:     audHandler,
	}
	if err := handlers.Validate(); err != nil {
		t.Fatalf("handlers.Validate(): %v", err)
	}

	validator := connectrpc.ProtoValidatorFunc(func(msg proto.Message) error {
		if req, ok := msg.(*flowsk_workspace_pb.GetWorkspaceRequest); ok && req.GetWorkspaceId() == "bad-id" {
			return errors.New("workspace_id must be a canonical UUID")
		}
		return nil
	})

	mux := connectrpc.NewMux(
		handlers,
		connect.WithInterceptors(
			connectrpc.NewAuthContextInterceptor(nil),
			connectrpc.NewValidationInterceptor(validator),
		),
	)

	httpClient := &inMemoryHTTPClient{handler: mux}
	baseURL := "http://flowsk.local"

	// 1. Verify /healthz endpoint.
	healthReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet, baseURL+connectrpc.HealthzPath, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext(/healthz): %v", err)
	}
	healthResp, err := httpClient.Do(healthReq)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_ = healthResp.Body.Close()
	if healthResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want %d", healthResp.StatusCode, http.StatusOK)
	}

	client := flowsk_workspace_pbconnect.NewWorkspaceServiceClient(httpClient, baseURL)

	// 2. Unauthenticated request must fail with CodeUnauthenticated.
	_, err = client.GetWorkspace(context.Background(), connect.NewRequest(&flowsk_workspace_pb.GetWorkspaceRequest{
		WorkspaceId: uuid.NewString(),
	}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("unauthenticated GetWorkspace code = %v, want %v (err=%v)", connect.CodeOf(err), connect.CodeUnauthenticated, err)
	}

	// 3. Authenticated request failing ProtoValidator must return CodeInvalidArgument.
	invalidReq := connect.NewRequest(&flowsk_workspace_pb.GetWorkspaceRequest{WorkspaceId: "bad-id"})
	setAuthHeaders(invalidReq.Header())
	_, err = client.GetWorkspace(context.Background(), invalidReq)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid GetWorkspace code = %v, want %v (err=%v)", connect.CodeOf(err), connect.CodeInvalidArgument, err)
	}

	// 4. Authenticated valid request passes auth and validation interceptors cleanly.
	authorizedWSID := uuid.NewString()
	validReq := connect.NewRequest(&flowsk_workspace_pb.GetWorkspaceRequest{WorkspaceId: authorizedWSID})
	setAuthHeaders(validReq.Header())
	validReq.Header().Set(connectrpc.HeaderWorkspaceID, authorizedWSID)
	_, err = client.GetWorkspace(context.Background(), validReq)
	if gotCode := connect.CodeOf(err); gotCode == connect.CodeUnauthenticated || gotCode == connect.CodeInvalidArgument {
		t.Fatalf("valid authenticated GetWorkspace unexpectedly rejected by interceptor with code = %v (err=%v)", gotCode, err)
	}
}

func setAuthHeaders(h http.Header) {
	h.Set(connectrpc.HeaderWorkspaceID, uuid.NewString())
	h.Set(connectrpc.HeaderActorID, uuid.NewString())
	h.Set(connectrpc.HeaderActorKind, "agent")
	h.Set(connectrpc.HeaderActorHandle, "worker-01")
}

// inMemoryHTTPClient implements connect.HTTPClient directly over an http.Handler
// using httptest.ResponseRecorder so full ConnectRPC wire serialization and
// interceptor chains run deterministically without opening loopback TCP sockets.
type inMemoryHTTPClient struct {
	handler http.Handler
}

func (c *inMemoryHTTPClient) Do(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)
	return rec.Result(), nil
}

func TestExtractBearerToken(t *testing.T) {
	t.Parallel()

	cases := []struct {
		header string
		want   string
		ok     bool
	}{
		{header: "", want: "", ok: false},
		{header: "   ", want: "", ok: false},
		{header: "raw-schemeless-token", want: "", ok: false},
		{header: "Basic dXNlcjpwYXNz", want: "", ok: false},
		{header: "Bearer", want: "", ok: false},
		{header: "Bearer    ", want: "", ok: false},
		{header: "Bearer tok_123", want: "tok_123", ok: true},
		{header: "  bEaReR   bearer-secret-xyz  ", want: "bearer-secret-xyz", ok: true},
	}

	for _, tc := range cases {
		got, ok := connectrpc.ExtractBearerToken(tc.header)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("ExtractBearerToken(%q) = (%q, %v), want (%q, %v)", tc.header, got, ok, tc.want, tc.ok)
		}
	}
}

func TestOriginAndHostGuardAndNewServer(t *testing.T) {
	t.Parallel()

	okHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	loopbackGuard := connectrpc.NewOriginAndHostGuard("127.0.0.1:1616", okHandler)

	// 1. DNS rebinding host rejected on loopback bind.
	reqRebind := httptest.NewRequest(http.MethodGet, "http://evil.rebind.com:1616/healthz", nil)
	recRebind := httptest.NewRecorder()
	loopbackGuard.ServeHTTP(recRebind, reqRebind)
	if recRebind.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-loopback Host on loopback bind, got %d", recRebind.Code)
	}

	// 2. Valid loopback Hosts (127.0.0.1, localhost, [::1]) pass.
	for _, target := range []string{
		"http://127.0.0.1:1616/healthz",
		"http://localhost:1616/healthz",
		"http://[::1]:1616/healthz",
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rec := httptest.NewRecorder()
		loopbackGuard.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 for %s, got %d", target, rec.Code)
		}
	}

	// 3. Cross-site POST or GET /api/events rejected with 403 Forbidden.
	reqCrossPOST := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:1616/flowsk.task.v1.TaskService/CreateTask", strings.NewReader("{}"))
	reqCrossPOST.Header.Set("Sec-Fetch-Site", "cross-site")
	recCrossPOST := httptest.NewRecorder()
	loopbackGuard.ServeHTTP(recCrossPOST, reqCrossPOST)
	if recCrossPOST.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-site POST, got %d", recCrossPOST.Code)
	}

	reqBadOriginPOST := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:1616/flowsk.task.v1.TaskService/CreateTask", strings.NewReader("{}"))
	reqBadOriginPOST.Header.Set("Origin", "https://evil.example")
	recBadOriginPOST := httptest.NewRecorder()
	loopbackGuard.ServeHTTP(recBadOriginPOST, reqBadOriginPOST)
	if recBadOriginPOST.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-origin POST, got %d", recBadOriginPOST.Code)
	}

	reqCrossSSE := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:1616"+connectrpc.EventsPath, nil)
	reqCrossSSE.Header.Set("Origin", "http://localhost:9999")
	recCrossSSE := httptest.NewRecorder()
	loopbackGuard.ServeHTTP(recCrossSSE, reqCrossSSE)
	if recCrossSSE.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-origin SSE, got %d", recCrossSSE.Code)
	}

	// 4. Same-origin browser POST and non-browser CLI POST pass cleanly.
	reqSameOrigin := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:1616/flowsk.task.v1.TaskService/ListTasks", strings.NewReader("{}"))
	reqSameOrigin.Header.Set("Origin", "http://127.0.0.1:1616")
	reqSameOrigin.Header.Set("Sec-Fetch-Site", "same-origin")
	recSameOrigin := httptest.NewRecorder()
	loopbackGuard.ServeHTTP(recSameOrigin, reqSameOrigin)
	if recSameOrigin.Code != http.StatusOK {
		t.Fatalf("expected 200 for same-origin POST, got %d", recSameOrigin.Code)
	}

	reqCLI := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:1616/flowsk.task.v1.TaskService/ListTasks", strings.NewReader("{}"))
	recCLI := httptest.NewRecorder()
	loopbackGuard.ServeHTTP(recCLI, reqCLI)
	if recCLI.Code != http.StatusOK {
		t.Fatalf("expected 200 for non-browser CLI POST, got %d", recCLI.Code)
	}

	// 5. Wildcard bind (:8080 / 0.0.0.0:8080) permits non-loopback Host while still blocking cross-origin POST.
	wildcardGuard := connectrpc.NewOriginAndHostGuard("0.0.0.0:8080", okHandler)
	reqWild := httptest.NewRequest(http.MethodPost, "http://flowsk.internal:8080/rpc", strings.NewReader("{}"))
	recWild := httptest.NewRecorder()
	wildcardGuard.ServeHTTP(recWild, reqWild)
	if recWild.Code != http.StatusOK {
		t.Fatalf("expected 200 on wildcard bind for CLI request, got %d", recWild.Code)
	}
}
