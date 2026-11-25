// File tools.go defines the token-frugal Model Context Protocol (MCP) tool
// schemas (`flowsk_search`, `flowsk_execute`, `flowsk_language_guide`, `flowsk_digest`)
// and strict argument-validated tool handlers.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
)

const (
	// ToolNameSearch implements Cloudflare Code Mode's `search()` primitive:
	// ultra-fast GitHub/Linear-style filtering across tasks (`state:done`,
	// `state:in_progress`, `is:open`, `is:blocked`, `#tag`, `@actor`, `ext:<id>`, free text),
	// strategic goals, and progressive `flowsk.*` JavaScript SDK discovery (`scope="sdk"`).
	ToolNameSearch = "flowsk_search"

	// ToolNameExecute implements Cloudflare Code Mode's `execute()` primitive:
	// runs sandboxed ES6 JavaScript against the global `flowsk` SDK inside a
	// single ACID database transaction.
	ToolNameExecute = "flowsk_execute"

	// ToolNameLanguageGuide returns the on-demand Flowsk Code Mode JavaScript
	// TypeScript `.d.ts` SDK reference and search qualifiers.
	ToolNameLanguageGuide = "flowsk_language_guide"

	// ToolNameDigest returns an ultra-low-token project board, active objective,
	// blocker, and ready-queue digest designed for frequent agent polling.
	ToolNameDigest = "flowsk_digest"

	defaultDigestLimit = 25
	maxDigestLimit     = 200
)

// ToolAnnotations conveys standard MCP behavioral hints to client harnesses
// regarding mutability, idempotency, and safety prompts.
type ToolAnnotations struct {
	ReadOnlyHint    bool `json:"readOnlyHint"`
	DestructiveHint bool `json:"destructiveHint"`
	IdempotentHint  bool `json:"idempotentHint"`
	OpenWorldHint   bool `json:"openWorldHint"`
}

// ToolContent represents a single typed block in an MCP tool response payload.
type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ToolResult wraps structured textual tool output and error state for MCP
// callers.
type ToolResult struct {
	Content []ToolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// ToolHandler processes raw JSON tool arguments under a request-scoped context.
type ToolHandler func(ctx context.Context, rawArgs json.RawMessage) (ToolResult, error)

// ToolSpec defines the canonical metadata and JSON Schema input contract for a
// single Flowsk MCP tool.
type ToolSpec struct {
	Name        string          `json:"name"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations ToolAnnotations `json:"annotations"`
}

// ExecuteToolArgs defines the JSON input payload accepted by flowsk_execute.
// Notice that only logical workspace_id and project_id identifiers are accepted;
// local host filesystem directories are never sent to the server.
type ExecuteToolArgs struct {
	WorkspaceID    string          `json:"workspace_id,omitempty"`
	ProjectID      string          `json:"project_id,omitempty"`
	ActorID        string          `json:"actor_id,omitempty"`
	Script         string          `json:"script"`
	DryRun         bool            `json:"dry_run,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	Density        ResponseDensity `json:"density,omitempty"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
}

// LanguageGuideToolArgs defines the JSON input payload accepted by
// flowsk_language_guide.
type LanguageGuideToolArgs struct {
	Section   GuideSection `json:"section,omitempty"`
	MaxTokens int          `json:"max_tokens,omitempty"`
}

// DigestToolArgs defines the JSON input payload accepted by flowsk_digest.
type DigestToolArgs struct {
	WorkspaceID    string `json:"workspace_id,omitempty"`
	ProjectID      string `json:"project_id,omitempty"`
	ObjectiveID    string `json:"objective_id,omitempty"`
	IncludeBlocked bool   `json:"include_blocked,omitempty"`
	Limit          int    `json:"limit,omitempty"`
	MaxTokens      int    `json:"max_tokens,omitempty"`
}

// SearchToolArgs defines the JSON input payload accepted by flowsk_search.
type SearchToolArgs struct {
	WorkspaceID string `json:"workspace_id,omitempty"`
	ProjectID   string `json:"project_id,omitempty"`
	Query       string `json:"query,omitempty"`
	Scope       string `json:"scope,omitempty"`
	State       string `json:"state,omitempty"`
	Limit       int    `json:"limit,omitempty"`
	MaxTokens   int    `json:"max_tokens,omitempty"`
}

var (
	searchInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "workspace_id": {
      "type": "string",
      "description": "Optional workspace UUID or slug."
    },
    "project_id": {
      "type": "string",
      "description": "Optional logical project UUID or key (e.g. 'CORE'). Never pass host paths."
    },
    "query": {
      "type": "string",
      "description": "GitHub/Linear-style search query, e.g. 'state:done #storage sqlite', 'state:in_progress @claude', 'is:open is:unclaimed', or SDK method keywords when scope='sdk'."
    },
    "scope": {
      "type": "string",
      "enum": ["all", "tasks", "goals", "sdk"],
      "default": "all",
      "description": "Target domain: 'tasks' (only tasks), 'goals' (strategic objectives), 'sdk' (discover JavaScript flowsk.* methods for flowsk_execute), or 'all'."
    },
    "state": {
      "type": "string",
      "description": "Quick state filter shorthand: 'done', 'in_progress', 'todo', 'review', 'blocked', 'backlog'."
    },
    "limit": {
      "type": "integer",
      "minimum": 1,
      "maximum": 200,
      "default": 25,
      "description": "Maximum matching items to return."
    },
    "max_tokens": {
      "type": "integer",
      "minimum": 0,
      "description": "Optional token ceiling for the search output."
    }
  }
}`)

	executeInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["script"],
  "properties": {
    "workspace_id": {
      "type": "string",
      "description": "Tenant workspace UUID or slug. Overrides MCP session default if provided."
    },
    "project_id": {
      "type": "string",
      "description": "Logical project UUID or key (e.g. 'CORE'). Never pass host filesystem paths."
    },
    "actor_id": {
      "type": "string",
      "description": "Registered agent or human actor handle performing the batch operation."
    },
    "script": {
      "type": "string",
      "description": "Sandboxed ES6 JavaScript code using the global 'flowsk' SDK (e.g. const t = flowsk.tasks.create({title: 'Fix bug', priority: 'high'}); flowsk.tasks.done(t.key); return flowsk.tasks.search('state:done');) executed inside ONE atomic ACID transaction."
    },
    "dry_run": {
      "type": "boolean",
      "description": "Simulate JavaScript execution in a rolled-back transaction without committing mutations."
    },
    "idempotency_key": {
      "type": "string",
      "description": "Deterministic client deduplication token for safe retries across transient failures."
    },
    "density": {
      "type": "string",
      "enum": ["minimal", "standard", "verbose"],
      "default": "minimal",
      "description": "Token verbosity of the execution summary ('minimal' recommended for agents)."
    },
    "max_tokens": {
      "type": "integer",
      "minimum": 0,
      "description": "Optional token ceiling for the returned execution output."
    }
  }
}`)

	languageGuideInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "section": {
      "type": "string",
      "enum": ["all", "syntax", "mutations", "queries", "examples"],
      "default": "all",
      "description": "Specific Code Mode JavaScript SDK documentation slice to fetch on demand."
    },
    "max_tokens": {
      "type": "integer",
      "minimum": 0,
      "description": "Optional token budget cap for the returned specification."
    }
  }
}`)

	digestInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "workspace_id": {
      "type": "string",
      "description": "Tenant workspace UUID or slug."
    },
    "project_id": {
      "type": "string",
      "description": "Logical project UUID or key (never a local folder path)."
    },
    "objective_id": {
      "type": "string",
      "description": "Optional objective UUID or key to narrow the task queue snapshot."
    },
    "include_blocked": {
      "type": "boolean",
      "default": true,
      "description": "Include active dependency blockers alongside claimable ready tasks."
    },
    "limit": {
      "type": "integer",
      "minimum": 1,
      "maximum": 200,
      "default": 25,
      "description": "Maximum number of tasks to summarize in the ready/active queue."
    },
    "max_tokens": {
      "type": "integer",
      "minimum": 0,
      "description": "Optional upper token ceiling for the compact digest output."
    }
  }
}`)
)

// CanonicalTools returns defensive copies of the Flowsk Cloudflare Code Mode MCP
// tool descriptors (`flowsk_search`, `flowsk_execute`, `flowsk_language_guide`, `flowsk_digest`).
func CanonicalTools() []ToolSpec {
	specs := []ToolSpec{
		{
			Name:        ToolNameSearch,
			Title:       "Search Flowsk Tasks, Goals, or Code Mode JS SDK",
			Description: "CLOUDFLARE CODE MODE SEARCH. Quickly filter tasks, goals, or SDK signatures using natural GitHub-style queries ('state:done', 'state:in_progress', 'is:open is:unclaimed', '#tag', '@actor', free text) or scope='sdk' to inspect `flowsk.*` JavaScript methods.",
			InputSchema: searchInputSchema,
			Annotations: ToolAnnotations{
				ReadOnlyHint:    true,
				DestructiveHint: false,
				IdempotentHint:  true,
				OpenWorldHint:   false,
			},
		},
		{
			Name:        ToolNameExecute,
			Title:       "Execute Sandboxed JavaScript (Code Mode) in One ACID Transaction",
			Description: "CLOUDFLARE CODE MODE EXECUTE. Runs standard ES6 JavaScript in a hermetic pure-Go sandbox against the global `flowsk` SDK (`flowsk.tasks.{create,search,list,ready,next,claim,move,done,release,block,unblock,comment}`, `flowsk.goals.{create,list,close}`, `flowsk.projects.{create,list,get}`, `flowsk.search(q)`, `flowsk.board()`) inside ONE atomic ACID transaction. Zero custom DSL guide required—write normal JS and `return` what you want to see.",
			InputSchema: executeInputSchema,
			Annotations: ToolAnnotations{
				ReadOnlyHint:    false,
				DestructiveHint: false,
				IdempotentHint:  false,
				OpenWorldHint:   false,
			},
		},
		{
			Name:        ToolNameLanguageGuide,
			Title:       "Read Flowsk Code Mode TypeScript SDK Reference",
			Description: "Returns the TypeScript `.d.ts` declaration of the `flowsk` JavaScript SDK, GitHub-style search qualifiers, and runnable ES6 recipes.",
			InputSchema: languageGuideInputSchema,
			Annotations: ToolAnnotations{
				ReadOnlyHint:    true,
				DestructiveHint: false,
				IdempotentHint:  true,
				OpenWorldHint:   false,
			},
		},
		{
			Name:        ToolNameDigest,
			Title:       "Poll Ultra-Low-Token Project & Task Queue Digest",
			Description: "Fetches an ultra-compact snapshot of active objectives, in-progress agent leases, unblocked claimable tasks, and dependency blockers within a strict token budget.",
			InputSchema: digestInputSchema,
			Annotations: ToolAnnotations{
				ReadOnlyHint:    true,
				DestructiveHint: false,
				IdempotentHint:  true,
				OpenWorldHint:   false,
			},
		},
	}
	return cloneToolSpecs(specs)
}

// HandleSearch validates arguments, enforces zero host-path leakage, and runs
// a Cloudflare Code Mode search across tasks, goals, or SDK methods.
func (s *ServerSkeleton) HandleSearch(ctx context.Context, rawArgs json.RawMessage) (ToolResult, error) {
	if ctx == nil {
		return ToolResult{}, errors.New("flowsk_search: nil context")
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, fmt.Errorf("flowsk_search context canceled: %w", err)
	}
	var args SearchToolArgs
	if err := decodeStrictJSON(rawArgs, &args); err != nil {
		return ToolResult{}, fmt.Errorf("flowsk_search %w: %w", ErrInvalidArguments, err)
	}
	if s.searchProvider == nil {
		return ToolResult{}, ErrNotImplemented
	}
	scope := strings.ToLower(strings.TrimSpace(args.Scope))
	if scope == "" {
		scope = "all"
	}
	switch scope {
	case "all", "tasks", "goals", "sdk":
	default:
		return ToolResult{}, fmt.Errorf("flowsk_search %w: unsupported scope %q", ErrInvalidArguments, args.Scope)
	}
	limit := args.Limit
	if limit == 0 {
		limit = defaultDigestLimit
	} else if limit < 1 || limit > maxDigestLimit {
		return ToolResult{}, fmt.Errorf("flowsk_search %w: limit must be between 1 and %d", ErrInvalidArguments, maxDigestLimit)
	}
	if args.MaxTokens < 0 {
		return ToolResult{}, fmt.Errorf("flowsk_search %w: max_tokens must be >= 0", ErrInvalidArguments)
	}
	workspaceID := firstNonEmpty(args.WorkspaceID, s.cfg.DefaultWorkspaceID)
	projectID := firstNonEmpty(args.ProjectID, s.cfg.DefaultProjectID)
	if err := rejectHostPath("workspace_id", workspaceID); err != nil {
		return ToolResult{}, fmt.Errorf("flowsk_search %w: %w", ErrInvalidArguments, err)
	}
	if err := rejectHostPath("project_id", projectID); err != nil {
		return ToolResult{}, fmt.Errorf("flowsk_search %w: %w", ErrInvalidArguments, err)
	}
	maxTokens := args.MaxTokens
	if maxTokens == 0 && s.cfg.MaxTokensDefault > 0 {
		maxTokens = s.cfg.MaxTokensDefault
	}
	ctx = ensureMCPAuthContext(ctx, workspaceID, s.cfg.DefaultActorID)
	out, err := s.searchProvider.Search(ctx, SearchInput{
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
		Query:       args.Query,
		Scope:       scope,
		State:       args.State,
		Limit:       limit,
		MaxTokens:   maxTokens,
	})
	if err != nil {
		if errors.Is(err, ErrNotImplemented) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return ToolResult{}, err
		}
		return ToolResult{
			Content: []ToolContent{{Type: "text", Text: formatSafeToolError(err)}},
			IsError: true,
		}, nil
	}
	text := strings.TrimSpace(out.FormattedResults)
	if text == "" {
		text = fmt.Sprintf("search matches tasks=%d goals=%d sdk=%d", out.TaskMatches, out.GoalMatches, out.SDKMatches)
	}
	return ToolResult{
		Content: []ToolContent{{Type: "text", Text: text}},
	}, nil
}

// HandleExecute validates arguments, enforces zero host-path leakage, applies
// session defaults, and invokes the configured LanguageExecutor.
func (s *ServerSkeleton) HandleExecute(ctx context.Context, rawArgs json.RawMessage) (ToolResult, error) {
	if ctx == nil {
		return ToolResult{}, errors.New("flowsk_execute: nil context")
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, fmt.Errorf("flowsk_execute context canceled: %w", err)
	}

	var args ExecuteToolArgs
	if err := decodeStrictJSON(rawArgs, &args); err != nil {
		return ToolResult{}, fmt.Errorf("flowsk_execute %w: %w", ErrInvalidArguments, err)
	}

	if s.executor == nil {
		return ToolResult{}, ErrNotImplemented
	}

	if strings.TrimSpace(args.Script) == "" {
		return ToolResult{}, fmt.Errorf("flowsk_execute %w: script is required", ErrInvalidArguments)
	}
	if args.MaxTokens < 0 {
		return ToolResult{}, fmt.Errorf("flowsk_execute %w: max_tokens must be >= 0", ErrInvalidArguments)
	}

	density := args.Density
	if density == "" {
		density = DensityMinimal
	} else if !density.Valid() {
		return ToolResult{}, fmt.Errorf("flowsk_execute %w: unsupported density %q", ErrInvalidArguments, density)
	}

	workspaceID := firstNonEmpty(args.WorkspaceID, s.cfg.DefaultWorkspaceID)
	projectID := firstNonEmpty(args.ProjectID, s.cfg.DefaultProjectID)
	actorID := firstNonEmpty(args.ActorID, s.cfg.DefaultActorID)

	if err := rejectHostPath("workspace_id", workspaceID); err != nil {
		return ToolResult{}, fmt.Errorf("flowsk_execute %w: %w", ErrInvalidArguments, err)
	}
	if err := rejectHostPath("project_id", projectID); err != nil {
		return ToolResult{}, fmt.Errorf("flowsk_execute %w: %w", ErrInvalidArguments, err)
	}

	maxTokens := args.MaxTokens
	if maxTokens == 0 && s.cfg.MaxTokensDefault > 0 {
		maxTokens = s.cfg.MaxTokensDefault
	}

	ctx = ensureMCPAuthContext(ctx, workspaceID, actorID)

	out, err := s.executor.Execute(ctx, ExecuteScriptInput{
		WorkspaceID:    workspaceID,
		ProjectID:      projectID,
		ActorID:        actorID,
		Script:         args.Script,
		DryRun:         args.DryRun,
		IdempotencyKey: args.IdempotencyKey,
		Density:        density,
		MaxTokens:      maxTokens,
	})
	if err != nil {
		if errors.Is(err, ErrNotImplemented) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return ToolResult{}, err
		}
		return ToolResult{
			Content: []ToolContent{{Type: "text", Text: formatSafeToolError(err)}},
			IsError: true,
		}, nil
	}

	summary := strings.TrimSpace(out.FormattedSummary)
	if summary == "" {
		summary = formatFallbackExecSummary(out)
	}
	return ToolResult{
		Content: []ToolContent{{Type: "text", Text: summary}},
	}, nil
}

// HandleLanguageGuide validates section/token parameters and returns on-demand
// Code Mode JavaScript SDK documentation from the configured LanguageGuideProvider.
func (s *ServerSkeleton) HandleLanguageGuide(ctx context.Context, rawArgs json.RawMessage) (ToolResult, error) {
	if ctx == nil {
		return ToolResult{}, errors.New("flowsk_language_guide: nil context")
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, fmt.Errorf("flowsk_language_guide context canceled: %w", err)
	}

	var args LanguageGuideToolArgs
	if err := decodeStrictJSON(rawArgs, &args); err != nil {
		return ToolResult{}, fmt.Errorf("flowsk_language_guide %w: %w", ErrInvalidArguments, err)
	}

	if s.guideProvider == nil {
		return ToolResult{}, ErrNotImplemented
	}

	section := args.Section
	if section == "" {
		section = GuideSectionAll
	} else if !section.Valid() {
		return ToolResult{}, fmt.Errorf("flowsk_language_guide %w: unsupported section %q", ErrInvalidArguments, section)
	}
	if args.MaxTokens < 0 {
		return ToolResult{}, fmt.Errorf("flowsk_language_guide %w: max_tokens must be >= 0", ErrInvalidArguments)
	}

	maxTokens := args.MaxTokens
	if maxTokens == 0 && s.cfg.MaxTokensDefault > 0 {
		maxTokens = s.cfg.MaxTokensDefault
	}

	out, err := s.guideProvider.Guide(ctx, GuideInput{
		Section:   section,
		MaxTokens: maxTokens,
	})
	if err != nil {
		if errors.Is(err, ErrNotImplemented) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return ToolResult{}, err
		}
		return ToolResult{
			Content: []ToolContent{{Type: "text", Text: formatSafeToolError(err)}},
			IsError: true,
		}, nil
	}

	return ToolResult{
		Content: []ToolContent{{Type: "text", Text: out.Markdown}},
	}, nil
}

// HandleDigest validates arguments, enforces zero host-path leakage, and returns
// a compact project and ready-queue digest from ProjectDigestProvider.
func (s *ServerSkeleton) HandleDigest(ctx context.Context, rawArgs json.RawMessage) (ToolResult, error) {
	if ctx == nil {
		return ToolResult{}, errors.New("flowsk_digest: nil context")
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, fmt.Errorf("flowsk_digest context canceled: %w", err)
	}

	type wireDigestArgs struct {
		WorkspaceID    string `json:"workspace_id,omitempty"`
		ProjectID      string `json:"project_id,omitempty"`
		ObjectiveID    string `json:"objective_id,omitempty"`
		IncludeBlocked *bool  `json:"include_blocked,omitempty"`
		Limit          *int   `json:"limit,omitempty"`
		MaxTokens      int    `json:"max_tokens,omitempty"`
	}
	var parsed wireDigestArgs
	if err := decodeStrictJSON(rawArgs, &parsed); err != nil {
		return ToolResult{}, fmt.Errorf("flowsk_digest %w: %w", ErrInvalidArguments, err)
	}

	if s.digestProvider == nil {
		return ToolResult{}, ErrNotImplemented
	}

	limit := defaultDigestLimit
	if parsed.Limit != nil {
		if *parsed.Limit < 1 || *parsed.Limit > maxDigestLimit {
			return ToolResult{}, fmt.Errorf("flowsk_digest %w: limit must be between 1 and %d", ErrInvalidArguments, maxDigestLimit)
		}
		limit = *parsed.Limit
	}
	if parsed.MaxTokens < 0 {
		return ToolResult{}, fmt.Errorf("flowsk_digest %w: max_tokens must be >= 0", ErrInvalidArguments)
	}

	includeBlocked := true
	if parsed.IncludeBlocked != nil {
		includeBlocked = *parsed.IncludeBlocked
	}

	workspaceID := firstNonEmpty(parsed.WorkspaceID, s.cfg.DefaultWorkspaceID)
	projectID := firstNonEmpty(parsed.ProjectID, s.cfg.DefaultProjectID)

	if err := rejectHostPath("workspace_id", workspaceID); err != nil {
		return ToolResult{}, fmt.Errorf("flowsk_digest %w: %w", ErrInvalidArguments, err)
	}
	if err := rejectHostPath("project_id", projectID); err != nil {
		return ToolResult{}, fmt.Errorf("flowsk_digest %w: %w", ErrInvalidArguments, err)
	}
	if err := rejectHostPath("objective_id", parsed.ObjectiveID); err != nil {
		return ToolResult{}, fmt.Errorf("flowsk_digest %w: %w", ErrInvalidArguments, err)
	}

	maxTokens := parsed.MaxTokens
	if maxTokens == 0 && s.cfg.MaxTokensDefault > 0 {
		maxTokens = s.cfg.MaxTokensDefault
	}

	ctx = ensureMCPAuthContext(ctx, workspaceID, s.cfg.DefaultActorID)

	out, err := s.digestProvider.Digest(ctx, DigestInput{
		WorkspaceID:    workspaceID,
		ProjectID:      projectID,
		ObjectiveID:    parsed.ObjectiveID,
		IncludeBlocked: includeBlocked,
		Limit:          limit,
		MaxTokens:      maxTokens,
	})
	if err != nil {
		if errors.Is(err, ErrNotImplemented) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return ToolResult{}, err
		}
		return ToolResult{
			Content: []ToolContent{{Type: "text", Text: formatSafeToolError(err)}},
			IsError: true,
		}, nil
	}

	text := strings.TrimSpace(out.FormattedDigest)
	if text == "" {
		text = fmt.Sprintf("digest ready=%d in_progress=%d blocked=%d", out.ReadyTasks, out.InProgressTasks, out.BlockedTasks)
	}
	return ToolResult{
		Content: []ToolContent{{Type: "text", Text: text}},
	}, nil
}

func decodeStrictJSON(raw json.RawMessage, dst any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected trailing JSON tokens")
	}
	return nil
}

func firstNonEmpty(primary, fallback string) string {
	if v := strings.TrimSpace(primary); v != "" {
		return v
	}
	return strings.TrimSpace(fallback)
}

func rejectHostPath(field, val string) error {
	v := strings.TrimSpace(val)
	if v == "" {
		return nil
	}
	if strings.HasPrefix(v, "/") ||
		strings.HasPrefix(v, "~/") ||
		strings.HasPrefix(v, "./") ||
		strings.HasPrefix(v, "../") ||
		strings.HasPrefix(strings.ToLower(v), "file://") ||
		strings.Contains(v, `\`) ||
		(len(v) >= 3 && ((v[0] >= 'A' && v[0] <= 'Z') || (v[0] >= 'a' && v[0] <= 'z')) && v[1] == ':' && (v[2] == '/' || v[2] == '\\')) {
		return fmt.Errorf("%s must not contain host filesystem paths", field)
	}
	return nil
}

func ensureMCPAuthContext(ctx context.Context, workspaceRef, actorRef string) context.Context {
	if _, err := authctx.FromContext(ctx); err == nil {
		return ctx
	}
	wsID, err := uuid.Parse(strings.TrimSpace(workspaceRef))
	if err != nil || wsID == uuid.Nil {
		return ctx
	}
	actorHandle := strings.TrimSpace(actorRef)
	if actorHandle == "" {
		actorHandle = "mcp-agent"
	}
	actorID, parseErr := uuid.Parse(actorHandle)
	if parseErr != nil || actorID == uuid.Nil {
		actorID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("flowsk:mcp-actor:"+wsID.String()+":"+actorHandle))
	}
	return authctx.WithIdentity(ctx, authctx.Identity{
		WorkspaceID: wsID,
		ActorID:     actorID,
		Kind:        authctx.ActorKindAgent,
		Handle:      actorHandle,
	})
}

func formatSafeToolError(err error) string {
	var domErr *errs.DomainError
	if errors.As(err, &domErr) && domErr != nil {
		msg := strings.TrimSpace(domErr.Message)
		if msg == "" && domErr.Sentinel != nil {
			msg = domErr.Sentinel.Error()
		}
		violations := domErr.ViolationsCopy()
		if len(violations) == 0 {
			if msg != "" {
				return "error: " + msg
			}
			return "error: domain operation failed"
		}
		var b strings.Builder
		b.WriteString("error: ")
		b.WriteString(msg)
		b.WriteString(": ")
		for i, v := range violations {
			if i > 0 {
				b.WriteString("; ")
			}
			if v.Field != "" {
				b.WriteString(v.Field)
				b.WriteString(": ")
			}
			b.WriteString(v.Description)
		}
		return b.String()
	}
	return "error: tool execution failed"
}

func formatFallbackExecSummary(out ExecuteScriptOutput) string {
	mode := "commit"
	if out.DryRun {
		mode = "dry_run"
	}
	return fmt.Sprintf("ok mutations=%d mode=%s", out.AppliedCount, mode)
}

func cloneToolSpecs(in []ToolSpec) []ToolSpec {
	out := make([]ToolSpec, len(in))
	for i, spec := range in {
		cloned := spec
		if len(spec.InputSchema) > 0 {
			cloned.InputSchema = append(json.RawMessage(nil), spec.InputSchema...)
		}
		out[i] = cloned
	}
	return out
}
