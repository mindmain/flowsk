// File version_test.go verifies semantic version parsing, tag normalization,
// and SemVer 2.0.0 §11 numeric vs. alphanumeric prerelease ordering.
package version_test

import (
	"testing"

	"github.com/mindmain/flowsk/internal/version"
)

func TestCompareSemverPrereleasePrecedence(t *testing.T) {
	// Direct numeric prerelease check: alpha.2 < alpha.10 (must NOT compare lexically as "2" > "10").
	if c, ok := version.CompareSemver("v1.0.0-alpha.2", "v1.0.0-alpha.10"); !ok || c >= 0 {
		t.Fatalf("expected v1.0.0-alpha.2 < v1.0.0-alpha.10, got c=%d ok=%t", c, ok)
	}
	if c, ok := version.CompareSemver("v1.0.0-alpha.10", "v1.0.0-alpha.2"); !ok || c <= 0 {
		t.Fatalf("expected v1.0.0-alpha.10 > v1.0.0-alpha.2, got c=%d ok=%t", c, ok)
	}

	// Canonical SemVer 2.0.0 §11.4.4 ordered sequence.
	chain := []string{
		"1.0.0-1",
		"1.0.0-alpha",
		"1.0.0-alpha.1",
		"1.0.0-alpha.2",
		"1.0.0-alpha.10",
		"1.0.0-alpha.beta",
		"1.0.0-beta",
		"1.0.0-beta.2",
		"1.0.0-beta.11",
		"1.0.0-rc.1",
		"1.0.0",
	}
	for i := 0; i < len(chain); i++ {
		if c, ok := version.CompareSemver(chain[i], chain[i]); !ok || c != 0 {
			t.Fatalf("expected %s == %s, got c=%d ok=%t", chain[i], chain[i], c, ok)
		}
		for j := i + 1; j < len(chain); j++ {
			if c, ok := version.CompareSemver(chain[i], chain[j]); !ok || c >= 0 {
				t.Fatalf("expected %s < %s, got c=%d ok=%t", chain[i], chain[j], c, ok)
			}
			if c, ok := version.CompareSemver(chain[j], chain[i]); !ok || c <= 0 {
				t.Fatalf("expected %s > %s, got c=%d ok=%t", chain[j], chain[i], c, ok)
			}
		}
	}

	// Build metadata (+...) must not affect version precedence.
	if c, ok := version.CompareSemver("v0.2.0-rc.2+build.1", "0.2.0-rc.2+build.99"); !ok || c != 0 {
		t.Fatalf("expected equal precedence ignoring build metadata, got c=%d ok=%t", c, ok)
	}
}
