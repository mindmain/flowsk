// File search.go implements a zero-dependency GitHub/Linear-style structured
// search query parser (`ParseSearchQuery`), conjunctive text matching, and the
// Cloudflare Code Mode progressive SDK catalog (`CodeModeSDKCatalog` /
// `SearchCodeModeSDK`) so AI agents and CLI users can filter tasks, goals, and
// SDK methods effortlessly without memorizing custom DSL syntax.
package language

import (
	"slices"
	"strings"
	"unicode"

	"github.com/mindmain/flowsk/internal/domain/task"
)

// SearchScope identifies the entity domain targeted by a search query.
type SearchScope string

const (
	// SearchScopeAll searches across tasks and strategic objectives/goals.
	SearchScopeAll SearchScope = "all"

	// SearchScopeTasks restricts search results to tasks.
	SearchScopeTasks SearchScope = "tasks"

	// SearchScopeGoals restricts search results to strategic objectives/goals.
	SearchScopeGoals SearchScope = "goals"

	// SearchScopeSDK searches the Flowsk Code Mode JavaScript SDK method catalog
	// (Cloudflare Code Mode progressive discovery).
	SearchScopeSDK SearchScope = "sdk"
)

// SearchQuery represents a normalized structured filter and free-text query
// parsed from natural GitHub/Linear-style expressions such as:
// `state:done #storage @claude obj:v1 prio:high is:blocked "sqlite wal"`.
type SearchQuery struct {
	// Raw preserves the original query string passed by the caller.
	Raw string

	// Scope selects whether to search tasks, goals, SDK methods, or all entities.
	Scope SearchScope

	// Statuses restricts tasks to one or more canonical workflow states.
	Statuses []task.Status

	// Tags requires all listed capability tags/labels to be present on a task.
	Tags []string

	// Actor filters tasks by active lease holder or assignee handle.
	Actor string

	// Objective filters tasks or objectives by objective slug, key, or UUID.
	Objective string

	// Parent filters tasks by parent task key or UUID.
	Parent string

	// ExternalID filters tasks by exact project-scoped external tracker ID (e.g., "CU-869abc").
	ExternalID string

	// Priority optionally filters tasks by canonical urgency rank.
	Priority *task.Priority

	// Blocked optionally filters tasks by blocked (`true`) or unblocked (`false`) state.
	Blocked *bool

	// Claimed optionally filters tasks by active claim lease presence (`true` / `false`).
	Claimed *bool

	// Terms holds lowercase free-text keywords and quoted phrases that must all
	// appear across the candidate entity's searchable text fields.
	Terms []string
}

// SDKMethodDoc describes a single method on the sandboxed `flowsk` JavaScript
// SDK exposed to autonomous agents in Cloudflare Code Mode (`flowsk_execute`).
type SDKMethodDoc struct {
	// Name is the fully qualified JavaScript method path (for example, "flowsk.tasks.claim").
	Name string

	// Category groups related SDK methods ("tasks", "goals", "projects", "search").
	Category string

	// Signature gives the concise TypeScript signature for LLM consumption.
	Signature string

	// Summary explains what the method does in one sentence.
	Summary string

	// Example provides a one-line copy-pasteable JavaScript call.
	Example string
}

// ParseSearchQuery parses a natural GitHub/Linear-style search expression into
// a normalized SearchQuery. Unknown `key:value` qualifiers fall back safely to
// literal text search terms so user queries never fail with syntax errors.
func ParseSearchQuery(raw string) SearchQuery {
	q := SearchQuery{
		Raw:   raw,
		Scope: SearchScopeAll,
	}
	tokens := tokenizeSearch(raw)
	for _, tok := range tokens {
		if tok.quoted {
			if t := strings.ToLower(strings.TrimSpace(tok.value)); t != "" {
				q.Terms = append(q.Terms, t)
			}
			continue
		}
		v := strings.TrimSpace(tok.value)
		if v == "" {
			continue
		}
		if strings.HasPrefix(v, "#") && len(v) > 1 {
			addUniqueString(&q.Tags, strings.ToLower(strings.TrimPrefix(v, "#")))
			continue
		}
		if strings.HasPrefix(v, "@") && len(v) > 1 {
			q.Actor = strings.TrimPrefix(v, "@")
			continue
		}
		colonIdx := strings.IndexByte(v, ':')
		if colonIdx > 0 && colonIdx < len(v)-1 {
			key := strings.ToLower(v[:colonIdx])
			val := strings.Trim(v[colonIdx+1:], `"'`)
			if applyQualifier(&q, key, val) {
				continue
			}
		}
		q.Terms = append(q.Terms, strings.ToLower(v))
	}
	return q
}

// MatchesText reports whether every free-text term in q.Terms appears
// case-insensitively across the concatenation of fields. Returns true when
// q.Terms is empty.
func (q SearchQuery) MatchesText(fields ...string) bool {
	if len(q.Terms) == 0 {
		return true
	}
	var b strings.Builder
	for _, f := range fields {
		if f == "" {
			continue
		}
		b.WriteString(strings.ToLower(f))
		b.WriteByte('\n')
	}
	haystack := b.String()
	for _, term := range q.Terms {
		if !strings.Contains(haystack, term) {
			return false
		}
	}
	return true
}

// NormalizeStatusToken maps natural human/AI state aliases ("done", "wip",
// "in-progress", "todo", "review", "blocked", "backlog", "canceled") to
// canonical domain task.Status values.
func NormalizeStatusToken(raw string) (task.Status, bool) {
	switch strings.ToLower(strings.ReplaceAll(strings.TrimSpace(raw), "-", "_")) {
	case "backlog", "triage":
		return task.StatusBacklog, true
	case "ready", "todo", "unstarted", "queued":
		return task.StatusReady, true
	case "in_progress", "inprogress", "wip", "active", "doing", "started":
		return task.StatusInProgress, true
	case "in_review", "review", "qa":
		return task.StatusInReview, true
	case "blocked":
		return task.StatusBlocked, true
	case "done", "completed", "complete", "closed", "shipped":
		return task.StatusDone, true
	case "cancelled", "canceled", "dropped":
		return task.StatusCancelled, true
	case "archived", "archive":
		return task.StatusArchived, true
	default:
		return "", false
	}
}

// NormalizePriorityToken maps human/AI priority names or numbers ("urgent"/"0",
// "high"/"1", "medium"/"2", "low"/"3") to canonical domain task.Priority values.
func NormalizePriorityToken(raw string) (task.Priority, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "critical", "urgent", "p0", "0":
		return task.PriorityCritical, true
	case "high", "p1", "1":
		return task.PriorityHigh, true
	case "medium", "normal", "p2", "2":
		return task.PriorityMedium, true
	case "low", "p3", "p4", "3", "4":
		return task.PriorityLow, true
	default:
		return 0, false
	}
}

// CodeModeTypeScriptSpec returns the ultra-compact (~250 tokens) TypeScript
// declaration summary of the `flowsk` JavaScript SDK injected into `goja` so
// AI agents can inspect the entire programmable surface in a single glance.
func CodeModeTypeScriptSpec() string {
	return `// Flowsk Code Mode SDK (ES6 JavaScript — runs inside a single ACID transaction)
declare const flowsk: {
  search(query: string): { tasks: Task[]; goals: Goal[] };
  board(opts?: { includeCompleted?: boolean; objective?: string }): Board;
  projects: {
    list(): Project[];
    get(slugOrKey?: string): Project;
    create(in: { key: string; name: string; slug?: string; description?: string }): Project;
  };
  goals: { // alias: flowsk.objectives
    list(filter?: string): Goal[];
    create(in: { title: string; slug?: string; summary?: string; priority?: number | "urgent" | "high" | "medium" | "low"; targetDate?: string }): Goal;
    close(slugOrId: string, note?: string): Goal;
  };
  tasks: {
    list(filter?: string | { state?: string; tag?: string; objective?: string; externalId?: string; actor?: string; blocked?: boolean; limit?: number }): Task[];
    search(query: string): Task[]; // e.g. "state:done #storage ext:CU-123" or "is:open is:unclaimed"
    ready(limit?: number): Task[];
    get(keyOrId: string): TaskDetail;
    getByExternal(externalId: string, project?: string): TaskDetail | null;
    next(opts?: { claim?: boolean; ttlSeconds?: number }): Task | null;
    create(in: { title: string; externalId?: string; description?: string; priority?: number | "urgent" | "high" | "medium" | "low"; state?: string; objective?: string; parent?: string; tags?: string[]; dependsOn?: string[]; acceptance?: string | string[] }): Task;
    upsertExternal(in: { externalId: string; project?: string; title: string; description?: string; priority?: number | "urgent" | "high" | "medium" | "low"; state?: string; objective?: string; parent?: string; tags?: string[]; dependsOn?: string[]; acceptance?: string | string[] }): TaskDetail;
    update(keyOrId: string, patch: { title?: string; externalId?: string; description?: string; priority?: number | string; tags?: string[]; acceptance?: string | string[]; objective?: string }): Task;
    move(keyOrId: string, state: string, note?: string): Task;
    done(keyOrId: string, note?: string): Task;
    claim(keyOrId: string, ttlSeconds?: number): Task;
    release(keyOrId: string, opts?: { state?: string; note?: string }): Task;
    block(keyOrId: string, opts: { by?: string[]; reason?: string }): Task;
    unblock(keyOrId: string, clearedBy?: string[]): Task;
    comment(keyOrId: string, body: string, kind?: "note" | "progress" | "decision" | "handoff" | "blocker"): Comment;
  };
};`
}

// CodeModeSDKCatalog returns defensive copies of all JavaScript methods exposed
// on `flowsk` inside the Code Mode sandbox.
func CodeModeSDKCatalog() []SDKMethodDoc {
	catalog := []SDKMethodDoc{
		{
			Name:      "flowsk.search",
			Category:  "search",
			Signature: "flowsk.search(query: string): { tasks: Task[], goals: Goal[] }",
			Summary:   "Searches tasks and strategic goals using GitHub-style filters (state:done, #tag, @actor, is:blocked) and free text.",
			Example:   `return flowsk.search("state:done #storage sqlite");`,
		},
		{
			Name:      "flowsk.tasks.search",
			Category:  "tasks",
			Signature: "flowsk.tasks.search(query: string): Task[]",
			Summary:   "Filters tasks by state (state:done, state:in_progress, is:open, is:unclaimed), #label, @actor, obj:slug, and keywords.",
			Example:   `return flowsk.tasks.search("state:in_progress @claude");`,
		},
		{
			Name:      "flowsk.tasks.create",
			Category:  "tasks",
			Signature: "flowsk.tasks.create({ title, externalId?, description?, priority?, state?, objective?, parent?, tags?, dependsOn?, acceptance? }): Task",
			Summary:   "Creates a task (with optional project-unique externalId and upstream dependsOn blocker edges) inside the active ACID transaction.",
			Example:   `const t = flowsk.tasks.create({ title: "Add search index", externalId: "CU-42", priority: "high", tags: ["db"] });`,
		},
		{
			Name:      "flowsk.tasks.getByExternal",
			Category:  "tasks",
			Signature: "flowsk.tasks.getByExternal(externalId: string, project?: string): Task | null",
			Summary:   "Resolves a task in the project by its external tracker ID (ClickUp/Linear/GitHub), returning null when absent.",
			Example:   `const existing = flowsk.tasks.getByExternal("CU-42");`,
		},
		{
			Name:      "flowsk.tasks.upsertExternal",
			Category:  "tasks",
			Signature: "flowsk.tasks.upsertExternal({ externalId, project?, title, description?, priority?, state?, tags?, dependsOn? }): Task",
			Summary:   "Idempotently creates or updates an external tracker task scoped to (projectId, externalId).",
			Example:   `const t = flowsk.tasks.upsertExternal({ externalId: "CU-42", title: "Sync billing webhook", priority: "high" });`,
		},
		{
			Name:      "flowsk.tasks.next",
			Category:  "tasks",
			Signature: "flowsk.tasks.next({ claim?: boolean, ttlSeconds?: number }): Task | null",
			Summary:   "Returns (and optionally claims with an exclusive lease) the highest-priority unblocked ready task.",
			Example:   `const next = flowsk.tasks.next({ claim: true, ttlSeconds: 1800 });`,
		},
		{
			Name:      "flowsk.tasks.claim",
			Category:  "tasks",
			Signature: "flowsk.tasks.claim(keyOrId: string, ttlSeconds?: number): Task",
			Summary:   "Acquires or renews an exclusive time-bounded agent ClaimLease on a task.",
			Example:   `flowsk.tasks.claim("FLW-1", 1800);`,
		},
		{
			Name:      "flowsk.tasks.done",
			Category:  "tasks",
			Signature: "flowsk.tasks.done(keyOrId: string, note?: string): Task",
			Summary:   "Marks a task Done, records an optional completion note, and releases any active claim lease.",
			Example:   `flowsk.tasks.done("FLW-1", "All tests passing");`,
		},
		{
			Name:      "flowsk.tasks.move",
			Category:  "tasks",
			Signature: "flowsk.tasks.move(keyOrId: string, state: string, note?: string): Task",
			Summary:   "Transitions a task to backlog, todo/ready, in_progress, in_review, blocked, done, or cancelled.",
			Example:   `flowsk.tasks.move("FLW-2", "in_progress", "Starting implementation");`,
		},
		{
			Name:      "flowsk.tasks.release",
			Category:  "tasks",
			Signature: "flowsk.tasks.release(keyOrId: string, { state?, note? }?): Task",
			Summary:   "Releases an active ClaimLease and optionally transitions task state.",
			Example:   `flowsk.tasks.release("FLW-2", { state: "in_review", note: "Ready for review" });`,
		},
		{
			Name:      "flowsk.tasks.block",
			Category:  "tasks",
			Signature: "flowsk.tasks.block(keyOrId: string, { by?: string[], reason?: string }): Task",
			Summary:   "Adds DAG-validated upstream blocker dependencies and marks the task blocked.",
			Example:   `flowsk.tasks.block("FLW-3", { by: ["FLW-2"], reason: "Needs RPC handler" });`,
		},
		{
			Name:      "flowsk.tasks.comment",
			Category:  "tasks",
			Signature: "flowsk.tasks.comment(keyOrId: string, body: string, kind?: string): Comment",
			Summary:   "Appends an immutable progress, decision, handoff, blocker, or note comment to a task.",
			Example:   `flowsk.tasks.comment("FLW-1", "Verified WAL checkpointing", "progress");`,
		},
		{
			Name:      "flowsk.goals.create",
			Category:  "goals",
			Signature: "flowsk.goals.create({ title, slug?, summary?, priority?, targetDate? }): Goal",
			Summary:   "Creates a strategic goal/milestone within the active project.",
			Example:   `const g = flowsk.goals.create({ title: "Launch v1.0", priority: "urgent", targetDate: "2026-12-31" });`,
		},
		{
			Name:      "flowsk.goals.list",
			Category:  "goals",
			Signature: "flowsk.goals.list(filter?: string): Goal[]",
			Summary:   "Lists strategic goals/objectives with live task completion progress counts.",
			Example:   `return flowsk.goals.list();`,
		},
		{
			Name:      "flowsk.projects.create",
			Category:  "projects",
			Signature: "flowsk.projects.create({ key, name, slug?, description? }): Project",
			Summary:   "Creates a logical multi-tenant project inside the active workspace.",
			Example:   `const p = flowsk.projects.create({ key: "API", name: "API Gateway" });`,
		},
	}
	return catalog
}

// SearchCodeModeSDK searches the Flowsk JavaScript SDK method catalog by
// free-text keywords (returning all methods when query is empty).
func SearchCodeModeSDK(query string) []SDKMethodDoc {
	parsed := ParseSearchQuery(query)
	catalog := CodeModeSDKCatalog()
	if len(parsed.Terms) == 0 {
		return catalog
	}
	var out []SDKMethodDoc
	for _, doc := range catalog {
		if parsed.MatchesText(doc.Name, doc.Category, doc.Signature, doc.Summary, doc.Example) {
			out = append(out, doc)
		}
	}
	return out
}

type searchToken struct {
	value  string
	quoted bool
}

func tokenizeSearch(raw string) []searchToken {
	var out []searchToken
	var cur strings.Builder
	var quote rune

	flush := func(quoted bool) {
		if cur.Len() > 0 {
			out = append(out, searchToken{value: cur.String(), quoted: quoted})
			cur.Reset()
		}
	}

	for _, r := range raw {
		if quote != 0 {
			if r == quote {
				flush(true)
				quote = 0
			} else {
				cur.WriteRune(r)
			}
			continue
		}
		if r == '"' || r == '\'' {
			if cur.Len() == 0 {
				quote = r
				continue
			}
		}
		if unicode.IsSpace(r) {
			flush(false)
			continue
		}
		cur.WriteRune(r)
	}
	flush(quote != 0)
	return out
}

func applyQualifier(q *SearchQuery, key, val string) bool {
	switch key {
	case "state", "status":
		for _, part := range strings.Split(val, ",") {
			if st, ok := NormalizeStatusToken(part); ok {
				addUniqueStatus(&q.Statuses, st)
			}
		}
		return true
	case "is":
		for _, part := range strings.Split(strings.ToLower(val), ",") {
			switch strings.ReplaceAll(strings.TrimSpace(part), "-", "_") {
			case "open", "active", "unfinished":
				for _, st := range []task.Status{task.StatusBacklog, task.StatusReady, task.StatusInProgress, task.StatusInReview, task.StatusBlocked} {
					addUniqueStatus(&q.Statuses, st)
				}
			case "done", "closed", "completed":
				addUniqueStatus(&q.Statuses, task.StatusDone)
			case "in_progress", "wip":
				addUniqueStatus(&q.Statuses, task.StatusInProgress)
			case "todo", "ready":
				addUniqueStatus(&q.Statuses, task.StatusReady)
			case "review", "in_review":
				addUniqueStatus(&q.Statuses, task.StatusInReview)
			case "archived":
				addUniqueStatus(&q.Statuses, task.StatusArchived)
			case "blocked":
				b := true
				q.Blocked = &b
			case "unblocked":
				b := false
				q.Blocked = &b
			case "claimed", "leased":
				c := true
				q.Claimed = &c
			case "unclaimed", "free":
				c := false
				q.Claimed = &c
			}
		}
		return true
	case "label", "tag", "labels", "tags":
		for _, part := range strings.Split(val, ",") {
			if t := strings.ToLower(strings.TrimSpace(part)); t != "" {
				addUniqueString(&q.Tags, t)
			}
		}
		return true
	case "actor", "assignee", "holder", "owner":
		q.Actor = strings.TrimPrefix(strings.TrimSpace(val), "@")
		return true
	case "obj", "objective", "goal", "milestone":
		q.Objective = strings.TrimSpace(val)
		return true
	case "parent":
		q.Parent = strings.TrimSpace(val)
		return true
	case "ext", "external", "external_id", "externalid":
		q.ExternalID = strings.TrimSpace(val)
		return true
	case "prio", "priority":
		if p, ok := NormalizePriorityToken(val); ok {
			q.Priority = &p
			return true
		}
		return false
	case "type", "scope", "kind":
		switch strings.ToLower(strings.TrimSpace(val)) {
		case "sdk", "api", "methods", "code":
			q.Scope = SearchScopeSDK
			return true
		case "task", "tasks", "issue", "issues":
			q.Scope = SearchScopeTasks
			return true
		case "goal", "goals", "objective", "objectives", "milestone", "milestones":
			q.Scope = SearchScopeGoals
			return true
		case "all":
			q.Scope = SearchScopeAll
			return true
		}
		return false
	default:
		return false
	}
}

func addUniqueStatus(dst *[]task.Status, st task.Status) {
	if !slices.Contains(*dst, st) {
		*dst = append(*dst, st)
	}
}

func addUniqueString(dst *[]string, s string) {
	if !slices.Contains(*dst, s) {
		*dst = append(*dst, s)
	}
}
