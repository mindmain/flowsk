// Package mcp defines the pure-Go Model Context Protocol (MCP) transport
// contracts, JSON-RPC 2.0 stdio and Streamable HTTP servers, canonical tool
// descriptors, and on-demand resource specifications for Flowsk.
//
// Architectural Role & Token-Efficiency Rationale:
// Traditional issue-tracker MCP integrations expose dozens of granular CRUD
// tools, forcing autonomous agents (Claude, Gemini, ChatGPT, Codex, Antigravity)
// into 20+ sequential JSON-RPC round-trips—re-sending massive conversation
// histories and burning thousands of context tokens on verbose JSON payloads.
//
// Flowsk collapses its MCP surface into four Cloudflare Code Mode primitives
// exposed identically over stdio and Streamable HTTP transports:
//  1. flowsk_search: GitHub/Linear-style task/goal search and SDK discovery.
//  2. flowsk_execute: Atomic execution of sandboxed ES6 JavaScript (`flowsk.*`).
//  3. flowsk_language_guide (+ flowsk://language/guide): On-demand TypeScript
//     `.d.ts` SDK reference and recipes so static system prompts stay tiny.
//  4. flowsk_digest: Low-token board, blocker, and ready-queue polling snapshot.
//
// Security & Isolation Invariants:
//   - Every execution and query contract requires explicit multi-tenant
//     workspace ownership context; unscoped cross-tenant queries are forbidden.
//   - Server-side MCP contracts NEVER accept, store, log, or return client host
//     filesystem paths. Local directory-to-project mappings remain exclusively
//     inside the client's local registry (~/.flowsk/local_paths.json).
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ErrNotImplemented is returned when an MCP server or handler is invoked without
// configured domain/language dependencies (for example, unconfigured CLI skeleton runs).
var ErrNotImplemented = errors.New("not implemented: interface skeleton only")

// ErrInvalidArguments indicates that caller-supplied MCP tool or resource arguments
// failed JSON decoding or schema/path validation.
var ErrInvalidArguments = errors.New("invalid mcp arguments")

const (
	defaultMCPProtocolVersion = "2025-03-26"
	mcpServerName             = "flowsk-mcp"
	mcpServerVersion          = "1.0.0"
	maxMCPFrameBytes          = 4 << 20 // 4 MiB ceiling per stdio line or HTTP POST body

	jsonRPCCodeParseError     = -32700
	jsonRPCCodeInvalidRequest = -32600
	jsonRPCCodeMethodNotFound = -32601
	jsonRPCCodeInvalidParams  = -32602
	jsonRPCCodeInternalError  = -32603

	defaultHTTPReadHeaderTimeout = 10 * time.Second
	defaultHTTPReadTimeout       = 30 * time.Second
	defaultHTTPWriteTimeout      = 30 * time.Second
	defaultHTTPIdleTimeout       = 120 * time.Second
	defaultHTTPShutdownTimeout   = 10 * time.Second
)

// TransportKind identifies the underlying wire transport used by the MCP server.
type TransportKind string

const (
	// TransportStdio serves MCP JSON-RPC 2.0 frames over standard input and
	// standard output. When running in stdio mode, stdout is reserved strictly
	// for protocol frames; all human/diagnostic logs must be written to stderr.
	TransportStdio TransportKind = "stdio"

	// TransportHTTP serves MCP over Streamable HTTP for shared local or office
	// Docker deployments where multiple agent harnesses connect concurrently.
	TransportHTTP TransportKind = "http"
)

// Valid reports whether k is a supported MCP transport mode.
func (k TransportKind) Valid() bool {
	switch k {
	case TransportStdio, TransportHTTP:
		return true
	default:
		return false
	}
}

// ResponseDensity controls the token verbosity of MCP tool outputs returned to
// LLM agents.
type ResponseDensity string

const (
	// DensityMinimal emits only assigned keys, symbolic alias bindings, status
	// transitions, and error codes—optimal for autonomous batch mutations.
	DensityMinimal ResponseDensity = "minimal"

	// DensityStandard emits compact single-line summaries per affected or
	// queried entity.
	DensityStandard ResponseDensity = "standard"

	// DensityVerbose emits full structured entity fields for human inspection
	// or deep debugging.
	DensityVerbose ResponseDensity = "verbose"
)

// Valid reports whether d is a supported response density tier.
func (d ResponseDensity) Valid() bool {
	switch d {
	case DensityMinimal, DensityStandard, DensityVerbose:
		return true
	default:
		return false
	}
}

// Config holds validated transport parameters for starting a Flowsk MCP server.
// Notice that Config contains zero target-repository or client host path fields.
type Config struct {
	// Transport selects between stdio ("stdio") and HTTP ("http").
	Transport TransportKind

	// HTTPAddr is the TCP listen address (e.g., "127.0.0.1:8081") used when
	// Transport is TransportHTTP.
	HTTPAddr string

	// Stdin supplies incoming JSON-RPC frames when Transport is TransportStdio.
	Stdin io.Reader

	// Stdout receives outgoing JSON-RPC frames when Transport is TransportStdio.
	Stdout io.Writer

	// Stderr receives diagnostic logs without corrupting stdio protocol streams.
	Stderr io.Writer

	// DefaultWorkspaceID provides an optional default tenant workspace UUID or
	// slug scoped to the authenticated MCP session.
	DefaultWorkspaceID string

	// DefaultProjectID provides an optional default logical project UUID or key.
	DefaultProjectID string

	// DefaultActorID identifies the default agent or human actor handle bound
	// to mutations in this MCP session.
	DefaultActorID string

	// MaxTokensDefault caps serialized tool output budgets when a tool call omits
	// an explicit max_tokens override (0 delegates to domain defaults).
	MaxTokensDefault int
}

// ExecuteScriptInput represents a tenant-scoped request to execute an atomic
// Cloudflare Code Mode ES6 JavaScript snippet. Host filesystem paths are strictly excluded.
type ExecuteScriptInput struct {
	WorkspaceID    string
	ProjectID      string
	ActorID        string
	Script         string
	DryRun         bool
	IdempotencyKey string
	Density        ResponseDensity
	MaxTokens      int
}

// ExecuteScriptOutput holds the compact, token-budgeted result of executing a
// Code Mode ES6 JavaScript snippet.
type ExecuteScriptOutput struct {
	FormattedSummary string
	AppliedCount     int
	DryRun           bool
	Truncated        bool
}

// LanguageExecutor is the Code Mode port invoked by flowsk_execute to run
// sandboxed ES6 JavaScript scripts inside a single ACID transaction with
// workspace ownership enforcement.
type LanguageExecutor interface {
	Execute(ctx context.Context, in ExecuteScriptInput) (ExecuteScriptOutput, error)
}

// GuideSection filters which portion of the Code Mode JavaScript SDK
// specification is returned to an agent.
type GuideSection string

const (
	// GuideSectionAll returns the complete Code Mode JavaScript SDK reference and recipes.
	GuideSectionAll GuideSection = "all"
	// GuideSectionSyntax returns TypeScript `.d.ts` declarations and sandbox rules.
	GuideSectionSyntax GuideSection = "syntax"
	// GuideSectionMutations returns task/goal/project mutation SDK methods.
	GuideSectionMutations GuideSection = "mutations"
	// GuideSectionQueries returns search qualifiers and query SDK methods.
	GuideSectionQueries GuideSection = "queries"
	// GuideSectionExamples returns end-to-end multi-agent JavaScript recipes.
	GuideSectionExamples GuideSection = "examples"
)

// Valid reports whether g is a supported GuideSection value.
func (g GuideSection) Valid() bool {
	switch g {
	case GuideSectionAll, GuideSectionSyntax, GuideSectionMutations, GuideSectionQueries, GuideSectionExamples:
		return true
	default:
		return false
	}
}

// GuideInput configures an on-demand Code Mode JavaScript SDK documentation lookup.
type GuideInput struct {
	Section   GuideSection
	MaxTokens int
}

// GuideOutput contains Markdown documentation teaching an LLM agent how to use
// Flowsk's Cloudflare Code Mode JavaScript SDK and search filters.
type GuideOutput struct {
	Version  string
	MIMEType string
	Markdown string
}

// LanguageGuideProvider supplies the self-describing Code Mode JavaScript SDK
// specification for both the flowsk_language_guide tool and the
// flowsk://language/guide MCP resource.
type LanguageGuideProvider interface {
	Guide(ctx context.Context, in GuideInput) (GuideOutput, error)
}

// DigestInput scopes an ultra-low-token project and task queue snapshot to an
// authorized workspace and logical project.
type DigestInput struct {
	WorkspaceID    string
	ProjectID      string
	ObjectiveID    string
	IncludeBlocked bool
	Limit          int
	MaxTokens      int
}

// DigestOutput holds the pre-formatted compact project digest engineered for
// frequent agent harness polling.
type DigestOutput struct {
	FormattedDigest string
	ReadyTasks      int
	InProgressTasks int
	BlockedTasks    int
	Truncated       bool
}

// ProjectDigestProvider computes compact workspace/project/task digests with
// strict tenant ownership isolation.
type ProjectDigestProvider interface {
	Digest(ctx context.Context, in DigestInput) (DigestOutput, error)
}

// SearchInput scopes a Cloudflare Code Mode `flowsk_search` query across tasks,
// strategic goals/objectives, or `flowsk.*` JavaScript SDK method signatures.
type SearchInput struct {
	WorkspaceID string
	ProjectID   string
	Query       string
	Scope       string
	State       string
	Limit       int
	MaxTokens   int
}

// SearchOutput wraps the compact formatted output returned by `flowsk_search`.
type SearchOutput struct {
	FormattedResults string
	TaskMatches      int
	GoalMatches      int
	SDKMatches       int
}

// SearchProvider executes GitHub/Linear-style task/goal queries and Code Mode
// progressive SDK discovery (`scope="sdk"`).
type SearchProvider interface {
	Search(ctx context.Context, in SearchInput) (SearchOutput, error)
}

// SearchProviderDependency is an optional single-method accessor resolved by
// NewServer when implemented on Dependencies.
type SearchProviderDependency interface {
	SearchProvider() SearchProvider
}

// LanguageExecutorDependency is the single-method accessor interface for
// resolving a LanguageExecutor inside NewServer.
type LanguageExecutorDependency interface {
	LanguageExecutor() LanguageExecutor
}

// LanguageGuideDependency is the single-method accessor interface for
// resolving a LanguageGuideProvider inside NewServer.
type LanguageGuideDependency interface {
	LanguageGuideProvider() LanguageGuideProvider
}

// ProjectDigestDependency is the single-method accessor interface for
// resolving a ProjectDigestProvider inside NewServer.
type ProjectDigestDependency interface {
	ProjectDigestProvider() ProjectDigestProvider
}

// Dependencies aggregates the fine-grained provider accessors required by the
// MCP server constructor. Following constructor resolution invariants, these
// accessors are invoked immediately inside NewServer and are never retained on
// ServerSkeleton.
type Dependencies interface {
	LanguageExecutorDependency
	LanguageGuideDependency
	ProjectDigestDependency
}

// Server defines the lifecycle and capability registration contract for the
// Flowsk MCP transport adapter.
type Server interface {
	// Serve runs the configured MCP transport (stdio or http) until ctx is
	// canceled or a fatal transport error occurs.
	Serve(ctx context.Context) error

	// RegisterTools registers and returns a defensive copy of all MCP tools
	// exposed by Flowsk.
	RegisterTools() []ToolSpec

	// RegisterResources registers and returns a defensive copy of all MCP
	// resources exposed by Flowsk.
	RegisterResources() []ResourceSpec
}

// ServerSkeleton is the constructor-resolved MCP server holding concrete domain
// service ports and canonical tool/resource descriptors.
type ServerSkeleton struct {
	cfg            Config
	executor       LanguageExecutor
	guideProvider  LanguageGuideProvider
	digestProvider ProjectDigestProvider
	searchProvider SearchProvider
	tools          []ToolSpec
	resources      []ResourceSpec

	mu        sync.RWMutex
	boundAddr string
}

var (
	_ Server       = (*ServerSkeleton)(nil)
	_ http.Handler = (*ServerSkeleton)(nil)
)

// NewServer resolves segregated service dependencies immediately at construction
// time and initializes the canonical Flowsk MCP tool and resource registries.
func NewServer(cfg Config, deps Dependencies) *ServerSkeleton {
	if cfg.Transport == "" {
		cfg.Transport = TransportStdio
	}
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = "127.0.0.1:8081"
	}

	var (
		executor       LanguageExecutor
		guideProvider  LanguageGuideProvider
		digestProvider ProjectDigestProvider
		searchProvider SearchProvider
	)
	if deps != nil {
		executor = deps.LanguageExecutor()
		guideProvider = deps.LanguageGuideProvider()
		digestProvider = deps.ProjectDigestProvider()
		if sp, ok := deps.(SearchProviderDependency); ok {
			searchProvider = sp.SearchProvider()
		}
	}

	s := &ServerSkeleton{
		cfg:            cfg,
		executor:       executor,
		guideProvider:  guideProvider,
		digestProvider: digestProvider,
		searchProvider: searchProvider,
		boundAddr:      cfg.HTTPAddr,
	}
	s.tools = CanonicalTools()
	s.resources = CanonicalResources()
	return s
}

// Config returns the normalized transport configuration bound to the server.
func (s *ServerSkeleton) Config() Config {
	return s.cfg
}

// Addr returns the active TCP listener address when running TransportHTTP.
func (s *ServerSkeleton) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.boundAddr
}

// RegisterTools initializes (if needed) and returns a defensive copy of the
// canonical Flowsk MCP tool specifications.
func (s *ServerSkeleton) RegisterTools() []ToolSpec {
	if len(s.tools) == 0 {
		s.tools = CanonicalTools()
	}
	return cloneToolSpecs(s.tools)
}

// RegisterResources initializes (if needed) and returns a defensive copy of the
// canonical Flowsk MCP resource specifications.
func (s *ServerSkeleton) RegisterResources() []ResourceSpec {
	if len(s.resources) == 0 {
		s.resources = CanonicalResources()
	}
	return cloneResourceSpecs(s.resources)
}

func (s *ServerSkeleton) hasCapabilities() bool {
	return s != nil && (s.executor != nil || s.guideProvider != nil || s.digestProvider != nil)
}

// ServeHTTP dispatches Streamable HTTP MCP JSON-RPC 2.0 requests.
func (s *ServerSkeleton) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.HTTPHandler().ServeHTTP(w, r)
}

// HTTPHandler returns a standard net/http handler serving Streamable HTTP MCP
// JSON-RPC 2.0 POST requests (or 501 Not Implemented when no dependencies are wired).
func (s *ServerSkeleton) HTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.Context().Err(); err != nil {
			http.Error(w, err.Error(), http.StatusRequestTimeout)
			return
		}
		if !s.hasCapabilities() {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusNotImplemented)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": ErrNotImplemented.Error(),
			})
			return
		}

		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}

		limited := http.MaxBytesReader(w, r.Body, maxMCPFrameBytes)
		defer func() { _ = limited.Close() }()

		rawBody, err := io.ReadAll(limited)
		if err != nil {
			writeJSONRPCResponse(w, http.StatusOK, newJSONRPCError(json.RawMessage("null"), jsonRPCCodeParseError, "failed to read JSON-RPC request body"))
			return
		}

		resp, isNotification := s.dispatchFrame(r.Context(), rawBody)
		if isNotification {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSONRPCResponse(w, http.StatusOK, resp)
	})
}

// Serve runs the configured MCP transport (`stdio` or `http`) until ctx is canceled
// or EOF/fatal listener error occurs. When invoked with nil capabilities, returns
// ErrNotImplemented to preserve unconfigured skeleton compatibility.
func (s *ServerSkeleton) Serve(ctx context.Context) error {
	if ctx == nil {
		return errors.New("mcp serve: nil context")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("mcp serve context canceled: %w", err)
	}
	if !s.cfg.Transport.Valid() {
		return fmt.Errorf("mcp serve: unsupported transport %q", s.cfg.Transport)
	}
	if !s.hasCapabilities() {
		return ErrNotImplemented
	}

	switch s.cfg.Transport {
	case TransportStdio:
		return s.serveStdio(ctx)
	case TransportHTTP:
		return s.serveHTTP(ctx)
	default:
		return fmt.Errorf("mcp serve: unsupported transport %q", s.cfg.Transport)
	}
}

func (s *ServerSkeleton) serveStdio(ctx context.Context) error {
	if s.cfg.Stdin == nil {
		return errors.New("mcp stdio serve: stdin reader is required")
	}
	if s.cfg.Stdout == nil {
		return errors.New("mcp stdio serve: stdout writer is required")
	}

	type scanItem struct {
		line []byte
		err  error
	}

	itemCh := make(chan scanItem, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(itemCh)

		scanner := bufio.NewScanner(s.cfg.Stdin)
		scanner.Buffer(make([]byte, 0, 64*1024), maxMCPFrameBytes)
		for scanner.Scan() {
			copied := append([]byte(nil), scanner.Bytes()...)
			select {
			case itemCh <- scanItem{line: copied}:
			case <-ctx.Done():
				return
			}
		}
		if scanErr := scanner.Err(); scanErr != nil {
			select {
			case itemCh <- scanItem{err: scanErr}:
			case <-ctx.Done():
			}
		}
	}()

	var writeMu sync.Mutex
	for {
		select {
		case <-ctx.Done():
			if closer, ok := s.cfg.Stdin.(io.Closer); ok && s.cfg.Stdin != os.Stdin {
				_ = closer.Close()
			}
			wg.Wait()
			return nil
		case item, ok := <-itemCh:
			if !ok {
				wg.Wait()
				return nil
			}
			if item.err != nil {
				s.logDiagnosticf("mcp stdio read error: %v", item.err)
				wg.Wait()
				return fmt.Errorf("mcp stdio scan: %w", item.err)
			}
			trimmed := bytes.TrimSpace(item.line)
			if len(trimmed) == 0 {
				continue
			}

			resp, isNotification := s.dispatchFrame(ctx, trimmed)
			if isNotification {
				continue
			}
			encoded, err := json.Marshal(resp)
			if err != nil {
				s.logDiagnosticf("mcp stdio marshal error: %v", err)
				continue
			}
			encoded = append(encoded, '\n')

			writeMu.Lock()
			_, writeErr := s.cfg.Stdout.Write(encoded)
			writeMu.Unlock()
			if writeErr != nil {
				wg.Wait()
				return fmt.Errorf("mcp stdio write: %w", writeErr)
			}
		}
	}
}

func (s *ServerSkeleton) serveHTTP(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("mcp http listen %q: %w", s.cfg.HTTPAddr, err)
	}

	s.mu.Lock()
	s.boundAddr = ln.Addr().String()
	s.mu.Unlock()

	mux := http.NewServeMux()
	mux.Handle("/", s.HTTPHandler())

	httpServer := &http.Server{
		Addr:              s.cfg.HTTPAddr,
		Handler:           mux,
		ReadHeaderTimeout: defaultHTTPReadHeaderTimeout,
		ReadTimeout:       defaultHTTPReadTimeout,
		WriteTimeout:      defaultHTTPWriteTimeout,
		IdleTimeout:       defaultHTTPIdleTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		if serveErr := httpServer.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultHTTPShutdownTimeout)
		defer cancel()
		shutdownErr := httpServer.Shutdown(shutdownCtx)
		serveErr := <-errCh
		return errors.Join(shutdownErr, serveErr)
	case serveErr := <-errCh:
		if serveErr != nil {
			return fmt.Errorf("mcp http serve: %w", serveErr)
		}
		return nil
	}
}

func (s *ServerSkeleton) logDiagnosticf(format string, args ...any) {
	if s.cfg.Stderr == nil {
		return
	}
	_, _ = fmt.Fprintf(s.cfg.Stderr, format+"\n", args...)
}

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func newJSONRPCResult(id json.RawMessage, result any) jsonRPCResponse {
	return jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      normalizeID(id),
		Result:  result,
	}
}

func newJSONRPCError(id json.RawMessage, code int, msg string) jsonRPCResponse {
	return jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      normalizeID(id),
		Error: &jsonRPCError{
			Code:    code,
			Message: msg,
		},
	}
}

func normalizeID(id json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(id)) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func writeJSONRPCResponse(w http.ResponseWriter, statusCode int, resp jsonRPCResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *ServerSkeleton) dispatchFrame(ctx context.Context, raw []byte) (resp jsonRPCResponse, isNotification bool) {
	var req jsonRPCRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return newJSONRPCError(nil, jsonRPCCodeParseError, "Parse error: malformed JSON-RPC payload"), false
	}
	if req.JSONRPC != "2.0" || strings.TrimSpace(req.Method) == "" {
		return newJSONRPCError(req.ID, jsonRPCCodeInvalidRequest, "Invalid Request: jsonrpc must be '2.0' and method must be non-empty"), false
	}

	hasID := len(bytes.TrimSpace(req.ID)) > 0

	switch req.Method {
	case "notifications/initialized", "notifications/cancelled":
		if !hasID {
			return jsonRPCResponse{}, true
		}
		return newJSONRPCResult(req.ID, map[string]any{}), false

	case "initialize":
		var initParams struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &initParams); err != nil {
				return newJSONRPCError(req.ID, jsonRPCCodeInvalidParams, "Invalid params: malformed initialize parameters"), false
			}
		}
		negotiated := defaultMCPProtocolVersion
		if initParams.ProtocolVersion == "2024-11-05" || initParams.ProtocolVersion == "2025-03-26" {
			negotiated = initParams.ProtocolVersion
		}
		return newJSONRPCResult(req.ID, map[string]any{
			"protocolVersion": negotiated,
			"capabilities": map[string]any{
				"tools":     map[string]bool{"listChanged": false},
				"resources": map[string]bool{"subscribe": false, "listChanged": false},
			},
			"serverInfo": map[string]string{
				"name":    mcpServerName,
				"version": mcpServerVersion,
			},
			"instructions": "Cloudflare Code Mode MCP: use flowsk_search (GitHub-style filters like 'state:done #tag sqlite', 'state:in_progress', or scope='sdk' to inspect SDK methods) and flowsk_execute to run standard ES6 JavaScript against the sandboxed `flowsk` SDK inside a single ACID transaction.",
		}), false

	case "ping":
		return newJSONRPCResult(req.ID, map[string]any{}), false

	case "tools/list":
		return newJSONRPCResult(req.ID, map[string]any{
			"tools": s.RegisterTools(),
		}), false

	case "tools/call":
		var callParams struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if len(req.Params) == 0 {
			return newJSONRPCError(req.ID, jsonRPCCodeInvalidParams, "Invalid params: tools/call requires name"), false
		}
		if err := json.Unmarshal(req.Params, &callParams); err != nil || strings.TrimSpace(callParams.Name) == "" {
			return newJSONRPCError(req.ID, jsonRPCCodeInvalidParams, "Invalid params: malformed tools/call payload"), false
		}

		var (
			toolRes ToolResult
			callErr error
		)
		switch callParams.Name {
		case ToolNameSearch:
			toolRes, callErr = s.HandleSearch(ctx, callParams.Arguments)
		case ToolNameExecute:
			toolRes, callErr = s.HandleExecute(ctx, callParams.Arguments)
		case ToolNameLanguageGuide:
			toolRes, callErr = s.HandleLanguageGuide(ctx, callParams.Arguments)
		case ToolNameDigest:
			toolRes, callErr = s.HandleDigest(ctx, callParams.Arguments)
		default:
			return newJSONRPCError(req.ID, jsonRPCCodeInvalidParams, fmt.Sprintf("Invalid params: unknown tool %q", callParams.Name)), false
		}

		if callErr != nil {
			switch {
			case errors.Is(callErr, ErrInvalidArguments):
				return newJSONRPCError(req.ID, jsonRPCCodeInvalidParams, callErr.Error()), false
			case errors.Is(callErr, ErrNotImplemented):
				return newJSONRPCError(req.ID, jsonRPCCodeMethodNotFound, callErr.Error()), false
			default:
				return newJSONRPCError(req.ID, jsonRPCCodeInternalError, "internal MCP tool execution error"), false
			}
		}
		return newJSONRPCResult(req.ID, toolRes), false

	case "resources/list":
		return newJSONRPCResult(req.ID, map[string]any{
			"resources": s.RegisterResources(),
		}), false

	case "resources/read":
		var readParams struct {
			URI string `json:"uri"`
		}
		if len(req.Params) == 0 {
			return newJSONRPCError(req.ID, jsonRPCCodeInvalidParams, "Invalid params: resources/read requires uri"), false
		}
		if err := json.Unmarshal(req.Params, &readParams); err != nil || strings.TrimSpace(readParams.URI) == "" {
			return newJSONRPCError(req.ID, jsonRPCCodeInvalidParams, "Invalid params: malformed resources/read payload"), false
		}

		contents, readErr := s.ReadResource(ctx, readParams.URI)
		if readErr != nil {
			switch {
			case errors.Is(readErr, ErrInvalidArguments):
				return newJSONRPCError(req.ID, jsonRPCCodeInvalidParams, readErr.Error()), false
			case errors.Is(readErr, ErrNotImplemented):
				return newJSONRPCError(req.ID, jsonRPCCodeMethodNotFound, readErr.Error()), false
			default:
				return newJSONRPCError(req.ID, jsonRPCCodeInternalError, "internal MCP resource read error"), false
			}
		}
		return newJSONRPCResult(req.ID, map[string]any{
			"contents": []ResourceContents{contents},
		}), false

	default:
		if !hasID {
			s.logDiagnosticf("ignoring unknown MCP notification %q", req.Method)
			return jsonRPCResponse{}, true
		}
		return newJSONRPCError(req.ID, jsonRPCCodeMethodNotFound, fmt.Sprintf("Method not found: %q", req.Method)), false
	}
}
