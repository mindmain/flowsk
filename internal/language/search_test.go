// File search_test.go verifies GitHub/Linear-style structured search query
// parsing, state/status/tag/actor/priority/is-filter normalization, free-text
// matching, and Cloudflare Code Mode progressive SDK catalog discovery.
package language_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/mindmain/flowsk/internal/domain/task"
	"github.com/mindmain/flowsk/internal/language"
)

// TestParseSearchQuery_GitHubAndLinearStyleFilters verifies structured filter
// tokens, #tag/@actor shorthands, quoted phrases, and open/done presets.
func TestParseSearchQuery_GitHubAndLinearStyleFilters(t *testing.T) {
	t.Parallel()

	q := language.ParseSearchQuery(`state:done,in-progress #storage label:rpc @claude-dev obj:v1-launch prio:high is:blocked "sqlite wal" migration`)

	if !slices.Contains(q.Statuses, task.StatusDone) || !slices.Contains(q.Statuses, task.StatusInProgress) {
		t.Fatalf("expected StatusDone and StatusInProgress in Statuses, got %v", q.Statuses)
	}
	if !slices.Contains(q.Tags, "storage") || !slices.Contains(q.Tags, "rpc") {
		t.Fatalf("expected storage and rpc in Tags, got %v", q.Tags)
	}
	if q.Actor != "claude-dev" {
		t.Fatalf("expected Actor claude-dev, got %q", q.Actor)
	}
	if q.Objective != "v1-launch" {
		t.Fatalf("expected Objective v1-launch, got %q", q.Objective)
	}
	if q.Priority == nil || *q.Priority != task.PriorityHigh {
		t.Fatalf("expected PriorityHigh, got %v", q.Priority)
	}
	if q.Blocked == nil || !*q.Blocked {
		t.Fatalf("expected Blocked=true from is:blocked, got %v", q.Blocked)
	}
	if !slices.Equal(q.Terms, []string{"sqlite wal", "migration"}) {
		t.Fatalf("expected Terms [\"sqlite wal\", \"migration\"], got %v", q.Terms)
	}
}

// TestParseSearchQuery_IsOpenDoneClaimedAndScope verifies `is:open`, `is:done`,
// `is:claimed`, `is:unclaimed`, and `type:sdk` progressive discovery routing.
func TestParseSearchQuery_IsOpenDoneClaimedAndScope(t *testing.T) {
	t.Parallel()

	openQ := language.ParseSearchQuery("is:open is:unclaimed")
	if openQ.Claimed == nil || *openQ.Claimed {
		t.Fatalf("expected Claimed=false for is:unclaimed, got %v", openQ.Claimed)
	}
	if slices.Contains(openQ.Statuses, task.StatusDone) || !slices.Contains(openQ.Statuses, task.StatusInProgress) {
		t.Fatalf("expected non-terminal statuses for is:open, got %v", openQ.Statuses)
	}

	sdkQ := language.ParseSearchQuery("type:sdk claim lease")
	if sdkQ.Scope != language.SearchScopeSDK {
		t.Fatalf("expected SearchScopeSDK, got %q", sdkQ.Scope)
	}
	hits := language.SearchCodeModeSDK("claim")
	if len(hits) == 0 {
		t.Fatal("expected SDK search for 'claim' to return matching flowsk.tasks.* methods")
	}
	foundClaim := false
	for _, h := range hits {
		if strings.Contains(h.Name, "flowsk.tasks.claim") {
			foundClaim = true
			break
		}
	}
	if !foundClaim {
		t.Fatalf("expected flowsk.tasks.claim in SDK search hits, got %+v", hits)
	}
}

// TestSearchQuery_MatchesTextFields verifies conjunctive case-insensitive
// matching across multiple candidate text fields.
func TestSearchQuery_MatchesTextFields(t *testing.T) {
	t.Parallel()

	q := language.ParseSearchQuery(`"wal mode" sqlite`)
	if !q.MatchesText("FLW-12", "Enable SQLite persistence", "Uses WAL mode for zero-lock reads") {
		t.Fatal("expected query terms to match across title and description")
	}
	if q.MatchesText("FLW-13", "Enable Postgres pool", "Uses pgxpool") {
		t.Fatal("expected query terms not to match unrelated fields")
	}
}

// TestCodeModeSDKCatalogAndSpecCompleteness verifies that every method bound in
// codemode/engine.go is present in both CodeModeSDKCatalog() and CodeModeTypeScriptSpec().
func TestCodeModeSDKCatalogAndSpecCompleteness(t *testing.T) {
	t.Parallel()

	wantMethods := []string{
		"flowsk.search",
		"flowsk.board",
		"flowsk.projects.list",
		"flowsk.projects.get",
		"flowsk.projects.create",
		"flowsk.projects.bind",
		"flowsk.projects.unbind",
		"flowsk.projects.bindings",
		"flowsk.projects.getDoc",
		"flowsk.projects.setDoc",
		"flowsk.projects.deleteDoc",
		"flowsk.goals.list",
		"flowsk.goals.create",
		"flowsk.goals.update",
		"flowsk.goals.close",
		"flowsk.tasks.list",
		"flowsk.tasks.search",
		"flowsk.tasks.ready",
		"flowsk.tasks.next",
		"flowsk.tasks.get",
		"flowsk.tasks.getByExternal",
		"flowsk.tasks.create",
		"flowsk.tasks.upsertExternal",
		"flowsk.tasks.update",
		"flowsk.tasks.claim",
		"flowsk.tasks.release",
		"flowsk.tasks.move",
		"flowsk.tasks.done",
		"flowsk.tasks.block",
		"flowsk.tasks.unblock",
		"flowsk.tasks.comment",
		"flowsk.tasks.getDoc",
		"flowsk.tasks.setDoc",
		"flowsk.tasks.deleteDoc",
	}

	catalog := language.CodeModeSDKCatalog()
	catalogSet := make(map[string]bool, len(catalog))
	for _, doc := range catalog {
		catalogSet[doc.Name] = true
	}

	tsSpec := language.CodeModeTypeScriptSpec()
	for _, m := range wantMethods {
		if !catalogSet[m] {
			t.Errorf("CodeModeSDKCatalog() missing method %q", m)
		}
		parts := strings.Split(m, ".")
		shortMethod := parts[len(parts)-1] + "("
		if !strings.Contains(tsSpec, shortMethod) {
			t.Errorf("CodeModeTypeScriptSpec() missing method signature %q for %q", shortMethod, m)
		}
	}
}
