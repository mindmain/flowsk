// File guide_test.go verifies offline availability and defensive copying of the
// embedded Cloudflare Code Mode JavaScript SDK specification and workflow examples.
package language_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mindmain/flowsk/internal/language"
)

// TestStaticGuideProvider verifies that the embedded Flowsk Code Mode JavaScript
// SDK specification and defensive example copies are available offline.
func TestStaticGuideProvider(t *testing.T) {
	t.Parallel()

	provider := language.NewGuideProvider()

	spec, err := provider.MarkdownSpec(context.Background())
	if err != nil {
		t.Fatalf("unexpected error fetching MarkdownSpec: %v", err)
	}
	if !strings.Contains(spec, "Cloudflare Code Mode") || !strings.Contains(spec, "declare const flowsk") {
		t.Fatalf("expected MarkdownSpec to contain Cloudflare Code Mode TypeScript SDK declaration")
	}

	examples1, err := provider.Examples(context.Background())
	if err != nil {
		t.Fatalf("unexpected error fetching Examples: %v", err)
	}
	if len(examples1) == 0 {
		t.Fatalf("expected at least one built-in Code Mode JavaScript example")
	}
	if pb := examples1[0].ToProto(); pb.GetSlug() == "" || !strings.Contains(pb.GetScript(), "flowsk.") {
		t.Fatalf("expected valid JavaScript example proto conversion, got %+v", pb)
	}

	// Verify defensive slice copy across calls.
	origSlug := examples1[0].Slug
	examples1[0].Slug = "mutated-slug"

	examples2, err := provider.Examples(context.Background())
	if err != nil {
		t.Fatalf("unexpected error fetching Examples second time: %v", err)
	}
	if examples2[0].Slug != origSlug {
		t.Fatalf("expected defensive copy, got mutated slug %q", examples2[0].Slug)
	}
}
