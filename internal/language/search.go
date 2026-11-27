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

	// HasMarkdownDoc optionally filters tasks by extended Markdown document presence (`is:md` / `has:md`).
	HasMarkdownDoc *bool

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

// NormalizePriorityToken maps human/AI priority names or numbers ("none"/"p0"/"0",
// "urgent"/"critical"/"p1"/"1", "high"/"p2"/"2", "medium"/"normal"/"p3"/"3",
// "low"/"p4"/"4") to canonical unified domain task.Priority values (0..4).
func NormalizePriorityToken(raw string) (task.Priority, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "none", "unprioritized", "p0", "0":
		return task.PriorityNone, true
	case "critical", "urgent", "p1", "1":
		return task.PriorityUrgent, true
	case "high", "p2", "2":
		return task.PriorityHigh, true
	case "medium", "normal", "p3", "3":
		return task.PriorityMedium, true
	case "low", "p4", "4":
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
  board(opts?: { includeCompleted?: boolean; objective?: string; project?: string }): Board;
  projects: {
    list(): Project[];
    get(slugOrKey?: string): Project;
    create(in: { key?: string; name: string; slug?: string; description?: string }): Project;
    bind(parentKey: string, childKey: string, scopeSubpath?: string): ProjectBinding;
    unbind(parentKey: string, childKey: string): { parentKey: string; childKey: string; removed: boolean };
    bindings(): ProjectBinding[];
    getDoc(slugOrKey?: string): { id: string; key: string; slug: string; markdown: string; hasMarkdownDoc: boolean };
    setDoc(markdown: string): Project;
    setDoc(slugOrKey: string, markdown: string): Project;
    deleteDoc(slugOrKey?: string): Project;
  };
  goals: { // alias: flowsk.objectives
    list(filter?: string): Goal[];
    create(in: { title: string; project?: string; kind?: "goal" | "milestone" | "cycle"; status?: "planned" | "active" | "at_risk"; slug?: string; summary?: string; priority?: 0 | 1 | 2 | 3 | 4 | "none" | "urgent" | "high" | "medium" | "low"; targetDate?: string; successCriteria?: string[] }): Goal;
    update(slugOrId: string, patch: { title?: string; kind?: "goal" | "milestone" | "cycle"; status?: "planned" | "active" | "at_risk" | "achieved" | "dropped"; summary?: string; priority?: number | string; targetDate?: string | null; successCriteria?: string[] }): Goal;
    close(slugOrId: string, note?: string): Goal;
  };
  tasks: {
    list(filter?: string | { state?: string; tag?: string; objective?: string; externalId?: string; actor?: string; blocked?: boolean; limit?: number }): Task[];
    search(query: string): Task[]; // e.g. "state:done #storage ext:CU-123" or "is:open is:unclaimed is:md"
    ready(limit?: number): Task[];
    next(opts?: { claim?: boolean; ttlSeconds?: number }): TaskDetail | null;
    get(keyOrId: string): TaskDetail;
    getByExternal(externalId: string, project?: string): TaskDetail | null;
    create(in: { title: string; project?: string; kind?: "epic" | "feature" | "bug" | "chore" | "research"; externalId?: string; description?: string; markdown?: string; priority?: 0 | 1 | 2 | 3 | 4 | "none" | "urgent" | "high" | "medium" | "low"; state?: string; objective?: string; parent?: string; tags?: string[]; dependsOn?: string[]; acceptance?: string | string[] }): TaskDetail;
    upsertExternal(in: { externalId: string; project?: string; kind?: "epic" | "feature" | "bug" | "chore" | "research"; title: string; description?: string; markdown?: string; priority?: 0 | 1 | 2 | 3 | 4 | "none" | "urgent" | "high" | "medium" | "low"; state?: string; objective?: string; parent?: string; tags?: string[]; dependsOn?: string[]; acceptance?: string | string[] }): TaskDetail;
    update(keyOrId: string, patch: { title?: string; kind?: string; externalId?: string; description?: string; markdown?: string; priority?: number | string; tags?: string[]; acceptance?: string | string[]; objective?: string; parent?: string }): TaskDetail;
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
};`
}

// CodeModeSDKCatalog returns defensive copies of all JavaScript methods exposed
// on `flowsk` inside the Code Mode sandbox.
func CodeModeSDKCatalog() []SDKMethodDoc {
	return []SDKMethodDoc{
		{
			Name:      "flowsk.search",
			Category:  "search",
			Signature: "flowsk.search(query: string): { tasks: Task[], goals: Goal[] }",
			Summary:   "Searches tasks and strategic goals using GitHub-style filters (state:done, #tag, @actor, is:blocked) and free text.",
			Example:   `return flowsk.search("state:done #storage sqlite");`,
		},
		{
			Name:      "flowsk.board",
			Category:  "board",
			Signature: "flowsk.board(opts?: { includeCompleted?: boolean, objective?: string, project?: string }): Board",
			Summary:   "Returns the active project board snapshot with goals, completion progress, and filtered open/completed tasks.",
			Example:   `return flowsk.board({ includeCompleted: false, objective: "launch-v1" });`,
		},
		{
			Name:      "flowsk.projects.list",
			Category:  "projects",
			Signature: "flowsk.projects.list(): Project[]",
			Summary:   "Lists all non-deleted logical projects in the active workspace.",
			Example:   `return flowsk.projects.list();`,
		},
		{
			Name:      "flowsk.projects.get",
			Category:  "projects",
			Signature: "flowsk.projects.get(slugOrKey?: string): Project",
			Summary:   "Resolves a logical project by key prefix, slug, or UUID (defaulting to the active project when omitted).",
			Example:   `return flowsk.projects.get("CORE");`,
		},
		{
			Name:      "flowsk.projects.create",
			Category:  "projects",
			Signature: "flowsk.projects.create({ key?, name, slug?, description? }): Project",
			Summary:   "Creates a logical multi-tenant project inside the active workspace.",
			Example:   `const p = flowsk.projects.create({ key: "API", name: "API Gateway" });`,
		},
		{
			Name:      "flowsk.projects.bind",
			Category:  "projects",
			Signature: "flowsk.projects.bind(parentKey: string, childKey: string, scopeSubpath?: string): ProjectBinding",
			Summary:   "Binds a sub-project under a parent project in the workspace hierarchy DAG with an optional relative scope subpath.",
			Example:   `flowsk.projects.bind("CORE", "WEB", "apps/web");`,
		},
		{
			Name:      "flowsk.projects.unbind",
			Category:  "projects",
			Signature: "flowsk.projects.unbind(parentKey: string, childKey: string): { parentKey: string, childKey: string, removed: boolean }",
			Summary:   "Removes a parent-to-child sub-project binding without deleting either project.",
			Example:   `flowsk.projects.unbind("CORE", "WEB");`,
		},
		{
			Name:      "flowsk.projects.bindings",
			Category:  "projects",
			Signature: "flowsk.projects.bindings(): ProjectBinding[]",
			Summary:   "Lists all parent-child sub-project DAG bindings in the active workspace.",
			Example:   `return flowsk.projects.bindings();`,
		},
		{
			Name:      "flowsk.projects.getDoc",
			Category:  "projects",
			Signature: "flowsk.projects.getDoc(slugOrKey?: string): { id: string, key: string, slug: string, markdown: string, hasMarkdownDoc: boolean }",
			Summary:   "Reads the extended Markdown charter (<project_id>.md) attached to a project inside ~/.flowsk/projects.",
			Example:   `return flowsk.projects.getDoc("CORE");`,
		},
		{
			Name:      "flowsk.projects.setDoc",
			Category:  "projects",
			Signature: "flowsk.projects.setDoc(markdown: string): Project | flowsk.projects.setDoc(slugOrKey: string, markdown: string): Project",
			Summary:   "Creates or updates the extended Markdown charter (<project_id>.md) for the active session project (1 arg) or an explicit project key/slug (2 args).",
			Example:   `flowsk.projects.setDoc("# Core Platform Charter\n\nStrict 3-layer architecture.");`,
		},
		{
			Name:      "flowsk.projects.deleteDoc",
			Category:  "projects",
			Signature: "flowsk.projects.deleteDoc(slugOrKey?: string): Project",
			Summary:   "Deletes the extended Markdown charter (<project_id>.md) attached to a project.",
			Example:   `flowsk.projects.deleteDoc("CORE");`,
		},
		{
			Name:      "flowsk.goals.list",
			Category:  "goals",
			Signature: "flowsk.goals.list(filter?: string): Goal[]",
			Summary:   "Lists strategic goals/objectives with live task completion progress counts.",
			Example:   `return flowsk.goals.list();`,
		},
		{
			Name:      "flowsk.goals.create",
			Category:  "goals",
			Signature: "flowsk.goals.create({ title, project?, kind?, status?, slug?, summary?, priority?, targetDate?, successCriteria? }): Goal",
			Summary:   "Creates a strategic goal, milestone, or cycle within the active project.",
			Example:   `const g = flowsk.goals.create({ title: "Launch v1.0", priority: "urgent", targetDate: "2026-12-31" });`,
		},
		{
			Name:      "flowsk.goals.update",
			Category:  "goals",
			Signature: "flowsk.goals.update(slugOrId: string, { title?, kind?, status?, summary?, priority?, targetDate?, successCriteria? }): Goal",
			Summary:   "Updates metadata, status (planned, active, at_risk, achieved, dropped), targetDate, or success criteria of a goal/milestone.",
			Example:   `flowsk.goals.update("launch-v1", { status: "at_risk", summary: "Waiting on security review" });`,
		},
		{
			Name:      "flowsk.goals.close",
			Category:  "goals",
			Signature: "flowsk.goals.close(slugOrId: string, note?: string): Goal",
			Summary:   "Transitions a goal/objective to achieved status and optionally records a completion note.",
			Example:   `flowsk.goals.close("launch-v1", "All release milestones verified");`,
		},
		{
			Name:      "flowsk.tasks.list",
			Category:  "tasks",
			Signature: "flowsk.tasks.list(filter?: string | { state?, tag?, objective?, externalId?, actor?, blocked?, limit? }): Task[]",
			Summary:   "Lists tasks in the project matching a structured filter object or GitHub-style search string.",
			Example:   `return flowsk.tasks.list({ state: "ready", limit: 20 });`,
		},
		{
			Name:      "flowsk.tasks.search",
			Category:  "tasks",
			Signature: "flowsk.tasks.search(query: string): Task[]",
			Summary:   "Filters tasks by state (state:done, state:in_progress, state:archived, is:open, is:unclaimed), #label, @actor, obj:slug, and keywords.",
			Example:   `return flowsk.tasks.search("state:in_progress @claude");`,
		},
		{
			Name:      "flowsk.tasks.ready",
			Category:  "tasks",
			Signature: "flowsk.tasks.ready(limit?: number): Task[]",
			Summary:   "Lists unclaimed, unblocked actionable tasks ordered by priority.",
			Example:   `return flowsk.tasks.ready(10);`,
		},
		{
			Name:      "flowsk.tasks.next",
			Category:  "tasks",
			Signature: "flowsk.tasks.next({ claim?: boolean, ttlSeconds?: number }?): TaskDetail | null",
			Summary:   "Returns (and optionally claims with an exclusive lease) the highest-priority unblocked ready task.",
			Example:   `const next = flowsk.tasks.next({ claim: true, ttlSeconds: 1800 });`,
		},
		{
			Name:      "flowsk.tasks.get",
			Category:  "tasks",
			Signature: "flowsk.tasks.get(keyOrId: string): TaskDetail",
			Summary:   "Fetches full task details including description, acceptance criteria, lease holder, and blocker status.",
			Example:   `return flowsk.tasks.get("FLW-1");`,
		},
		{
			Name:      "flowsk.tasks.getByExternal",
			Category:  "tasks",
			Signature: "flowsk.tasks.getByExternal(externalId: string, project?: string): TaskDetail | null",
			Summary:   "Resolves a task in the project by its external tracker ID (ClickUp/Linear/GitHub), returning null when absent.",
			Example:   `const existing = flowsk.tasks.getByExternal("CU-42");`,
		},
		{
			Name:      "flowsk.tasks.create",
			Category:  "tasks",
			Signature: "flowsk.tasks.create({ title, project?, kind?, externalId?, description?, markdown?, priority?, state?, objective?, parent?, tags?, dependsOn?, acceptance? }): TaskDetail",
			Summary:   "Creates a task (with optional project-unique externalId, markdown doc, and upstream dependsOn blocker edges) inside the active ACID transaction.",
			Example:   `const t = flowsk.tasks.create({ title: "Add search index", externalId: "CU-42", priority: "high", tags: ["db"] });`,
		},
		{
			Name:      "flowsk.tasks.upsertExternal",
			Category:  "tasks",
			Signature: "flowsk.tasks.upsertExternal({ externalId, project?, kind?, title, description?, markdown?, priority?, state?, objective?, parent?, tags?, dependsOn?, acceptance? }): TaskDetail",
			Summary:   "Idempotently creates or updates an external tracker task scoped to (projectId, externalId).",
			Example:   `const t = flowsk.tasks.upsertExternal({ externalId: "CU-42", kind: "bug", title: "Sync billing webhook", priority: "high" });`,
		},
		{
			Name:      "flowsk.tasks.update",
			Category:  "tasks",
			Signature: "flowsk.tasks.update(keyOrId: string, { title?, kind?, externalId?, description?, markdown?, objective?, parent?, priority?, tags?, acceptance? }): TaskDetail",
			Summary:   "Applies partial metadata, objective/parent linkage, tag/acceptance slice updates, or Markdown doc changes to a task.",
			Example:   `flowsk.tasks.update("FLW-1", { priority: "urgent", tags: ["db", "wal"] });`,
		},
		{
			Name:      "flowsk.tasks.claim",
			Category:  "tasks",
			Signature: "flowsk.tasks.claim(keyOrId: string, ttlSeconds?: number): TaskDetail",
			Summary:   "Acquires or renews an exclusive time-bounded agent ClaimLease on a task.",
			Example:   `flowsk.tasks.claim("FLW-1", 1800);`,
		},
		{
			Name:      "flowsk.tasks.release",
			Category:  "tasks",
			Signature: "flowsk.tasks.release(keyOrId: string, { state?, note? }?): TaskDetail",
			Summary:   "Releases an active ClaimLease and optionally transitions task state.",
			Example:   `flowsk.tasks.release("FLW-2", { state: "in_review", note: "Ready for review" });`,
		},
		{
			Name:      "flowsk.tasks.move",
			Category:  "tasks",
			Signature: "flowsk.tasks.move(keyOrId: string, state: string, note?: string): TaskDetail",
			Summary:   "Transitions a task to backlog, todo/ready, in_progress, in_review, blocked, done, cancelled, or archived.",
			Example:   `flowsk.tasks.move("FLW-2", "in_progress", "Starting implementation");`,
		},
		{
			Name:      "flowsk.tasks.done",
			Category:  "tasks",
			Signature: "flowsk.tasks.done(keyOrId: string, note?: string): TaskDetail",
			Summary:   "Marks a task Done, records an optional completion note, and releases any active claim lease.",
			Example:   `flowsk.tasks.done("FLW-1", "All tests passing");`,
		},
		{
			Name:      "flowsk.tasks.block",
			Category:  "tasks",
			Signature: "flowsk.tasks.block(keyOrId: string, reasonOrOpts: string | { by?: string[], reason?: string }, by?: string[]): TaskDetail",
			Summary:   "Adds DAG-validated upstream blocker dependencies (keeping the task in its workflow column with blocked=true) or transitions to blocked when no 'by' tasks are given.",
			Example:   `flowsk.tasks.block("FLW-3", { by: ["FLW-2"], reason: "Needs RPC handler" });`,
		},
		{
			Name:      "flowsk.tasks.unblock",
			Category:  "tasks",
			Signature: "flowsk.tasks.unblock(keyOrId: string, clearedBy?: string[]): TaskDetail",
			Summary:   "Removes specified (or all) upstream blocker dependencies and transitions the task back to ready once unblocked.",
			Example:   `flowsk.tasks.unblock("FLW-3", ["FLW-2"]);`,
		},
		{
			Name:      "flowsk.tasks.comment",
			Category:  "tasks",
			Signature: "flowsk.tasks.comment(keyOrId: string, body: string, kind?: string): Comment",
			Summary:   "Appends an immutable progress, decision, handoff, blocker, or note comment to a task.",
			Example:   `flowsk.tasks.comment("FLW-1", "Verified WAL checkpointing", "progress");`,
		},
		{
			Name:      "flowsk.tasks.getDoc",
			Category:  "tasks",
			Signature: "flowsk.tasks.getDoc(keyOrId: string): { id: string, key: string, markdown: string, hasMarkdownDoc: boolean }",
			Summary:   "Reads the extended Markdown document (<task_id>.md) attached to a task inside ~/.flowsk/tasks.",
			Example:   `return flowsk.tasks.getDoc("FLW-1");`,
		},
		{
			Name:      "flowsk.tasks.setDoc",
			Category:  "tasks",
			Signature: "flowsk.tasks.setDoc(keyOrId: string, markdown: string): TaskDetail",
			Summary:   "Creates or updates the extended Markdown specification file (<task_id>.md) attached to a task atomically.",
			Example:   `flowsk.tasks.setDoc("FLW-1", "# Design Spec\n\n- Zero host paths");`,
		},
		{
			Name:      "flowsk.tasks.deleteDoc",
			Category:  "tasks",
			Signature: "flowsk.tasks.deleteDoc(keyOrId: string): TaskDetail",
			Summary:   "Deletes the extended Markdown specification file (<task_id>.md) attached to a task atomically.",
			Example:   `flowsk.tasks.deleteDoc("FLW-1");`,
		},
	}
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
			case "md", "doc", "markdown":
				md := true
				q.HasMarkdownDoc = &md
			}
		}
		return true
	case "has":
		switch strings.ToLower(strings.TrimSpace(val)) {
		case "md", "doc", "markdown":
			md := true
			q.HasMarkdownDoc = &md
			return true
		}
		return false
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
