// Package language implements Flowsk's GitHub/Linear-style structured search
// parser (`ParseSearchQuery`), the Cloudflare Code Mode JavaScript SDK method
// catalog (`CodeModeSDKCatalog` / `SearchCodeModeSDK`), and the self-describing
// Code Mode TypeScript `.d.ts` specification & runnable ES6 examples (`GuideProvider`).
package language

import (
	"context"
	"fmt"
	"strings"

	flowsk_language_pb "github.com/mindmain/flowsk/gen/go/flowsk/language/v1"
)

// Example represents a curated, copy-pasteable Cloudflare Code Mode ES6
// JavaScript workflow snippet exposed to AI agents via MCP resources/tools and
// ConnectRPC discovery endpoints.
type Example struct {
	// Slug is the stable identifier of the example (for example, "sync-external-and-claim").
	Slug string

	// Category groups related Code Mode recipes (for example, "planning", "claiming", "sync").
	Category string

	// Title summarizes the agent workflow demonstrated.
	Title string

	// Description explains when an autonomous agent should use this pattern.
	Description string

	// Script contains a valid, runnable ES6 JavaScript snippet targeting `flowsk.*`.
	Script string
}

// ToProto converts an Example into its ConnectRPC `flowsk.language.v1.LanguageExample` wire message.
func (e Example) ToProto() *flowsk_language_pb.LanguageExample {
	cat := e.Category
	if cat == "" {
		cat = "codemode"
	}
	return &flowsk_language_pb.LanguageExample{
		Slug:        e.Slug,
		Title:       e.Title,
		Category:    cat,
		Description: e.Description,
		Script:      e.Script,
	}
}

// GuideProvider serves self-documenting Cloudflare Code Mode TypeScript `.d.ts`
// SDK specifications, search qualifiers, and runnable ES6 JavaScript examples to
// MCP clients, ConnectRPC callers, and LLM harnesses.
type GuideProvider interface {
	// MarkdownSpec returns the authoritative English Markdown specification of the
	// Flowsk Code Mode JavaScript SDK (`flowsk.*`) and search query qualifiers.
	MarkdownSpec(ctx context.Context) (string, error)

	// Examples returns defensive copies of canonical JavaScript Code Mode examples.
	Examples(ctx context.Context) ([]Example, error)
}

// StaticGuideProvider implements GuideProvider using the compiled-in
// DefaultLanguageGuideMarkdown specification and canonical examples so agents can
// always retrieve the Code Mode SDK manual offline with zero database or network I/O.
type StaticGuideProvider struct{}

// Compile-time check ensuring *StaticGuideProvider satisfies GuideProvider.
var _ GuideProvider = (*StaticGuideProvider)(nil)

// NewGuideProvider returns an immutable StaticGuideProvider backed by DefaultLanguageGuideMarkdown.
func NewGuideProvider() *StaticGuideProvider {
	return &StaticGuideProvider{}
}

// MarkdownSpec returns DefaultLanguageGuideMarkdown after checking context cancellation.
func (g *StaticGuideProvider) MarkdownSpec(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("language.GuideProvider.MarkdownSpec: %w", err)
	}
	return DefaultLanguageGuideMarkdown, nil
}

// SectionMarkdown returns the requested section ("overview", "syntax", "verbs",
// "filters", or "examples", or the full guide when section is empty/"all") from
// DefaultLanguageGuideMarkdown after checking context cancellation.
func (g *StaticGuideProvider) SectionMarkdown(ctx context.Context, section string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("language.GuideProvider.SectionMarkdown: %w", err)
	}
	extracted := ExtractGuideSection(DefaultLanguageGuideMarkdown, section)
	if extracted == "" {
		return DefaultLanguageGuideMarkdown, nil
	}
	return extracted, nil
}

// ExtractGuideSection extracts a logical Markdown section ("overview", "syntax",
// "verbs", "filters", or "examples") from a Flowsk language guide Markdown document.
// When section is empty, "all", or "full", markdown is returned unchanged; when no
// section matches, an empty string is returned.
func ExtractGuideSection(markdown, section string) string {
	sec := strings.ToLower(strings.TrimSpace(section))
	if sec == "" || sec == "all" || sec == "full" {
		return markdown
	}

	type mdBlock struct {
		heading string
		content string
	}

	lines := strings.Split(markdown, "\n")
	var preambleLines []string
	var blocks []mdBlock
	curIdx := -1

	cleanLines := func(raw []string) string {
		start := 0
		for start < len(raw) && strings.TrimSpace(raw[start]) == "" {
			start++
		}
		end := len(raw)
		for end > start {
			t := strings.TrimSpace(raw[end-1])
			if t == "" || t == "---" {
				end--
				continue
			}
			break
		}
		if start >= end {
			return ""
		}
		return strings.Join(raw[start:end], "\n") + "\n"
	}

	var curLines []string
	for _, line := range lines {
		if strings.HasPrefix(line, "## ") {
			if curIdx >= 0 {
				blocks[curIdx].content = cleanLines(curLines)
			}
			blocks = append(blocks, mdBlock{
				heading: strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "## "))),
			})
			curIdx = len(blocks) - 1
			curLines = []string{line}
			continue
		}
		if curIdx < 0 {
			preambleLines = append(preambleLines, line)
		} else {
			curLines = append(curLines, line)
		}
	}
	if curIdx >= 0 {
		blocks[curIdx].content = cleanLines(curLines)
	}
	preamble := cleanLines(preambleLines)

	var matchKeywords []string
	includePreamble := false
	switch sec {
	case "overview", "intro", "introduction", "guarantees":
		includePreamble = true
		matchKeywords = []string{"overview", "core guarantees", "introduction"}
	case "filters", "filter", "search", "qualifiers":
		matchKeywords = []string{"search qualifiers", "filters", "qualifiers"}
	case "syntax", "typescript", "declaration", "spec", "grammar":
		matchKeywords = []string{"syntax", "typescript declaration", "javascript sdk"}
	case "verbs", "sdk", "methods", "actions", "operations", "api":
		matchKeywords = []string{"verbs", "sdk", "methods", "typescript declaration"}
	case "examples", "example", "recipes", "cookbook":
		matchKeywords = []string{"examples", "recipes"}
	default:
		matchKeywords = []string{sec}
	}

	var matched []string
	if includePreamble && preamble != "" {
		matched = append(matched, strings.TrimSuffix(preamble, "\n"))
	}
	for _, b := range blocks {
		for _, kw := range matchKeywords {
			if strings.Contains(b.heading, kw) {
				if b.content != "" {
					matched = append(matched, strings.TrimSuffix(b.content, "\n"))
				}
				break
			}
		}
	}
	if len(matched) == 0 {
		return ""
	}
	return strings.Join(matched, "\n\n") + "\n"
}

// Examples returns defensive copies of built-in canonical Code Mode JavaScript examples.
func (g *StaticGuideProvider) Examples(ctx context.Context) ([]Example, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("language.GuideProvider.Examples: %w", err)
	}
	out := make([]Example, len(defaultExamples))
	copy(out, defaultExamples)
	return out, nil
}

// DefaultLanguageGuideMarkdown is the token-compact English Cloudflare Code Mode
// Reference Manual teaching LLM agents and developers how to filter entities with
// `flowsk_search` and execute multi-step ACID JavaScript workflows in `flowsk_execute`.
const DefaultLanguageGuideMarkdown = "# Flowsk Cloudflare Code Mode — JavaScript SDK & Search Reference\n\n" +
	"Flowsk implements **Cloudflare Code Mode**: instead of making dozens of sequential CRUD tool calls, filter tasks/goals/SDK signatures with `flowsk_search` (`flowsk search`) and run standard **ES6 JavaScript** against the global `flowsk` SDK inside `flowsk_execute` (`flowsk code -e '...'`).\n\n" +
	"## Core Guarantees\n" +
	"1. **Single-Transaction ACID Atomicity**: Every read and write in a JavaScript snippet runs inside **one outer database transaction** with atomic audit logging. Any runtime exception or domain validation failure rolls back the entire script cleanly, and `dry_run: true` simulates execution without committing changes.\n" +
	"2. **Hermetic Pure-Go Sandbox (`dop251/goja`)**: Zero filesystem, network, environment variable, or OS process access, with preemptive watchdog timeout cancellation.\n" +
	"3. **Zero Host Paths**: Projects are referenced exclusively by key (`CORE`), slug (`core-platform`), or UUID — never pass local filesystem paths.\n" +
	"4. **External Tracker Sync (`externalId`)**: Link ClickUp, Linear, Jira, or GitHub tickets deterministically per project via `flowsk.tasks.upsertExternal`, `flowsk.tasks.getByExternal`, and `ext:<id>` search filters.\n\n" +
	"---\n\n" +
	"## 1. Search Qualifiers (`flowsk_search` / `flowsk search`)\n" +
	"- **State filters**: `state:done`, `state:in_progress`, `state:todo`, `state:review`, `state:blocked`, `state:archived`, `is:open`, `is:done`, `is:claimed`, `is:unclaimed`, `is:blocked`, `is:archived`, `is:md`\n" +
	"- **Metadata**: `#tag` / `label:rpc`, `@actor` / `assignee:claude`, `obj:v1`, `parent:FLW-1`, `ext:CU-869abc`, `prio:urgent|high|medium|low`\n" +
	"- **SDK discovery**: `scope=\"sdk\"` or `type:sdk claim` (lists matching `flowsk.*` JavaScript methods)\n\n" +
	"---\n\n" +
	"## 2. JavaScript SDK Syntax & Verbs (`flowsk_execute` / `flowsk code`)\n\n" +
	"```ts\n" +
	`declare const flowsk: {
  search(query: string): { tasks: Task[]; goals: Goal[] };
  board(opts?: { includeCompleted?: boolean; objective?: string; project?: string }): Board;
  projects: {
    list(): Project[];
    get(slugOrKey?: string): Project;
    create(in: { key?: string; name: string; slug?: string; description?: string }): Project;
    bind(parentKey: string, childKey: string, scopeSubpath?: string): ProjectBinding;
    unbind(parentKey: string, childKey: string): { parentKey: string; childKey: string; removed: boolean };
    bindings(): ProjectBinding[];
    getDoc(slugOrKey?: string): { id: string; key: string; slug: string; markdown: string; hasMarkdownDoc: boolean };
    setDoc(slugOrKey: string, markdown: string): Project;
    deleteDoc(slugOrKey?: string): Project;
  };
  goals: { // alias: flowsk.objectives
    list(filter?: string): Goal[];
    create(in: {
      title: string;
      project?: string;
      kind?: "goal" | "milestone" | "cycle";
      status?: "planned" | "active" | "at_risk";
      slug?: string;
      summary?: string;
      priority?: number | "urgent" | "high" | "medium" | "low";
      targetDate?: string; // YYYY-MM-DD or RFC3339
      successCriteria?: string[];
    }): Goal;
    update(slugOrId: string, patch: {
      title?: string;
      kind?: "goal" | "milestone" | "cycle";
      status?: "planned" | "active" | "at_risk" | "achieved" | "dropped";
      summary?: string;
      priority?: number | string;
      targetDate?: string | null; // "" or null clears targetDate
      successCriteria?: string[];
    }): Goal;
    close(slugOrId: string, note?: string): Goal;
  };
  tasks: {
    list(filter?: string | { state?: string; tag?: string; objective?: string; externalId?: string; actor?: string; blocked?: boolean; limit?: number }): Task[];
    search(query: string): Task[];
    ready(limit?: number): Task[];
    next(opts?: { claim?: boolean; ttlSeconds?: number }): TaskDetail | null;
    get(keyOrId: string): TaskDetail;
    getByExternal(externalId: string, project?: string): TaskDetail | null;
    create(in: {
      title: string;
      project?: string;
      externalId?: string;
      kind?: "epic" | "feature" | "bug" | "chore" | "research";
      description?: string;
      markdown?: string; // Extended .md specification saved in ~/.flowsk/tasks/
      priority?: number | "urgent" | "high" | "medium" | "low";
      state?: string;
      objective?: string;
      parent?: string;
      tags?: string[];
      dependsOn?: string[];
      acceptance?: string | string[];
    }): TaskDetail;
    upsertExternal(in: {
      externalId: string;
      project?: string;
      kind?: "epic" | "feature" | "bug" | "chore" | "research";
      title: string;
      description?: string;
      markdown?: string;
      priority?: number | "urgent" | "high" | "medium" | "low";
      state?: string;
      objective?: string;
      parent?: string;
      tags?: string[];
      dependsOn?: string[];
      acceptance?: string | string[];
    }): TaskDetail;
    update(keyOrId: string, patch: { title?: string; kind?: string; externalId?: string; description?: string; markdown?: string; objective?: string; parent?: string; priority?: number | string; tags?: string[]; acceptance?: string | string[] }): TaskDetail;
    claim(keyOrId: string, ttlSeconds?: number): TaskDetail;
    release(keyOrId: string, opts?: { state?: string; note?: string }): TaskDetail;
    move(keyOrId: string, state: string, note?: string): TaskDetail;
    done(keyOrId: string, note?: string): TaskDetail;
    block(keyOrId: string, reasonOrOpts: string | { by?: string[]; reason?: string }, by?: string[]): TaskDetail;
    unblock(keyOrId: string, clearedBy?: string[]): TaskDetail;
    comment(keyOrId: string, body: string, kind?: "note" | "progress" | "decision" | "handoff" | "blocker"): Comment;
    getDoc(keyOrId: string): { id: string; key: string; markdown: string; hasMarkdownDoc: boolean };
    setDoc(keyOrId: string, markdown: string): TaskDetail;
    deleteDoc(keyOrId: string): TaskDetail;
  };
};
// Task & TaskDetail expose lastSignalAt: string | null (RFC3339Nano UTC timestamp of latest agent/CLI/MCP signal up to in_review).` + "\n```\n\n---\n\n## 3. Multi-Step Atomic JavaScript Recipes & Examples\n\n### Decompose a Goal, Wire DAG Dependencies & Claim First Task\n```js\nconst goal = flowsk.goals.create({\n  title: \"Ship Storage & MCP v1\",\n  priority: \"urgent\",\n  targetDate: \"2026-12-31\"\n});\nconst epic = flowsk.tasks.create({\n  title: \"Dual SQLite & Postgres Persistence\",\n  kind: \"epic\",\n  objective: goal.key,\n  priority: \"urgent\",\n  tags: [\"db\", \"golang\"]\n});\nconst t1 = flowsk.tasks.create({\n  title: \"Implement SQLite WAL pool & Goose runner\",\n  parent: epic.key,\n  objective: goal.key,\n  priority: \"urgent\"\n});\nconst t2 = flowsk.tasks.create({\n  title: \"Add cross-tenant isolation integration tests\",\n  parent: epic.key,\n  objective: goal.key,\n  priority: \"high\",\n  dependsOn: [t1.key]\n});\nreturn flowsk.tasks.claim(t1.key, 1800);\n```\n\n### Complete Leased Work & Claim Next Unblocked Ready Task\n```js\nflowsk.tasks.comment(\"FLW-1\", \"Race-enabled test suite green\", \"progress\");\nflowsk.tasks.done(\"FLW-1\", \"Verified with task verify\");\nreturn flowsk.tasks.next({ claim: true, ttlSeconds: 1800 });\n```\n"

var defaultExamples = []Example{
	{
		Slug:        "claim-complete-and-next",
		Category:    "claiming",
		Title:       "Complete Current Task & Claim Next Unblocked Task Atomically",
		Description: "Closes an active leased task with a verification note and claims the next highest-priority unblocked ready task in one transaction.",
		Script: `flowsk.tasks.done("FLW-1", "Implemented and verified with go test -race");
return flowsk.tasks.next({ claim: true, ttlSeconds: 1800 });
`,
	},
	{
		Slug:        "epic-dag-decomposition",
		Category:    "planning",
		Title:       "Decompose Goal & Epic with Directed Blocker Graph and Immediate Lease",
		Description: "Creates a strategic goal, parent epic, ordered subtasks with DAG blocker edges, and claims the root unblocked subtask atomically.",
		Script: `const goal = flowsk.goals.create({ title: "Strict Tenant Ownership Isolation", priority: "urgent" });
const epic = flowsk.tasks.create({ kind: "epic", objective: goal.key, priority: "urgent", title: "Tenant Isolation Rollout" });
const t1 = flowsk.tasks.create({ parent: epic.key, objective: goal.key, priority: "urgent", title: "Add authctx.RequireWorkspaceOwnership" });
const t2 = flowsk.tasks.create({ parent: epic.key, objective: goal.key, priority: "high", title: "Enforce workspace_id in sqlc queries", dependsOn: [t1.key] });
return flowsk.tasks.claim(t1.key, 1800);
`,
	},
	{
		Slug:        "external-tracker-sync",
		Category:    "sync",
		Title:       "Idempotently Upsert External Ticket & Decompose Subtasks",
		Description: "Syncs a ClickUp/Linear/Jira/GitHub issue by project-unique externalId and creates an actionable child task.",
		Script: `const parent = flowsk.tasks.upsertExternal({
  externalId: "CU-869abc",
  title: "Implement webhook HMAC verifier",
  priority: "high",
  tags: ["security", "api"]
});
const sub = flowsk.tasks.create({
  title: "Write replay-protection unit tests",
  parent: parent.key,
  priority: "high"
});
return { parent: parent.key, subtask: sub.key };
`,
	},
}
