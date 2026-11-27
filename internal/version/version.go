// Package version exposes build-time release metadata and semantic version
// comparison helpers used by the Flowsk CLI (`flowsk --version`, `flowsk doctor`,
// and `flowsk update`).
package version

import (
	"cmp"
	"strconv"
	"strings"
)

var (
	// Version is the semantic version of the running Flowsk binary (overridable via -ldflags).
	Version = "0.2.0"

	// Commit is the Git revision hash embedded at build time (overridable via -ldflags).
	Commit = "dev"

	// BuildDate is the UTC ISO-8601 build timestamp embedded at build time (overridable via -ldflags).
	BuildDate = "unknown"

	// DefaultGitHubRepo is the canonical GitHub `owner/repo` slug checked by `flowsk update`.
	DefaultGitHubRepo = "mindmain/flowsk"
)

// NormalizedTag returns the canonical `vMAJOR.MINOR.PATCH` tag representation of Version.
func NormalizedTag() string {
	trimmed := strings.TrimSpace(Version)
	if trimmed == "" {
		return "v0.0.0"
	}
	if strings.HasPrefix(trimmed, "v") || strings.HasPrefix(trimmed, "V") {
		return "v" + trimmed[1:]
	}
	return "v" + trimmed
}

// Semver represents a parsed `MAJOR.MINOR.PATCH[-PRERELEASE]` semantic version.
type Semver struct {
	// Major is the major version number.
	Major int

	// Minor is the minor version number.
	Minor int

	// Patch is the patch version number.
	Patch int

	// Prerelease holds any optional `-alpha.1` / `-rc.1` suffix (empty for stable releases).
	Prerelease string
}

// ParseSemver parses a `vX.Y.Z` or `X.Y.Z` semantic version string.
// Returns (Semver{}, false) when raw is not a valid semantic version.
func ParseSemver(raw string) (Semver, bool) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
	if s == "" {
		return Semver{}, false
	}
	// Strip build metadata (+...)
	if plusIdx := strings.IndexByte(s, '+'); plusIdx >= 0 {
		s = s[:plusIdx]
	}
	var prerelease string
	if dashIdx := strings.IndexByte(s, '-'); dashIdx >= 0 {
		prerelease = s[dashIdx+1:]
		s = s[:dashIdx]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return Semver{}, false
	}
	maj, err := strconv.Atoi(parts[0])
	if err != nil || maj < 0 {
		return Semver{}, false
	}
	minVer, err := strconv.Atoi(parts[1])
	if err != nil || minVer < 0 {
		return Semver{}, false
	}
	patch := 0
	if len(parts) == 3 {
		patch, err = strconv.Atoi(parts[2])
		if err != nil || patch < 0 {
			return Semver{}, false
		}
	}
	return Semver{
		Major:      maj,
		Minor:      minVer,
		Patch:      patch,
		Prerelease: prerelease,
	}, true
}

// CompareSemver compares two semantic version strings (`a` vs `b`), returning:
//   - `-1` when `a < b`
//   - `0`  when `a == b`
//   - `+1` when `a > b`
//
// The boolean return is false if either version string cannot be parsed as semver.
func CompareSemver(a, b string) (int, bool) {
	va, okA := ParseSemver(a)
	vb, okB := ParseSemver(b)
	if !okA || !okB {
		return 0, false
	}
	if c := cmp.Compare(va.Major, vb.Major); c != 0 {
		return c, true
	}
	if c := cmp.Compare(va.Minor, vb.Minor); c != 0 {
		return c, true
	}
	if c := cmp.Compare(va.Patch, vb.Patch); c != 0 {
		return c, true
	}
	switch {
	case va.Prerelease == "" && vb.Prerelease == "":
		return 0, true
	case va.Prerelease == "" && vb.Prerelease != "":
		// Stable release > prerelease of same X.Y.Z
		return 1, true
	case va.Prerelease != "" && vb.Prerelease == "":
		return -1, true
	default:
		return comparePrerelease(va.Prerelease, vb.Prerelease), true
	}
}

// comparePrerelease compares dot-separated prerelease identifiers according to
// SemVer 2.0.0 §11.4: pure-digit identifiers are compared numerically,
// numeric identifiers have lower precedence than alphanumeric identifiers,
// alphanumeric identifiers are compared lexically in ASCII order, and a larger
// set of prerelease fields has higher precedence when prefixes match.
func comparePrerelease(a, b string) int {
	if a == b {
		return 0
	}
	partsA := strings.Split(a, ".")
	partsB := strings.Split(b, ".")
	n := min(len(partsA), len(partsB))
	for i := range n {
		idA, idB := partsA[i], partsB[i]
		if idA == idB {
			continue
		}
		numA := isDigitsOnly(idA)
		numB := isDigitsOnly(idB)
		switch {
		case numA && numB:
			if c := compareNumericIdentifiers(idA, idB); c != 0 {
				return c
			}
		case numA && !numB:
			return -1
		case !numA && numB:
			return 1
		default:
			if c := cmp.Compare(idA, idB); c != 0 {
				return c
			}
		}
	}
	return cmp.Compare(len(partsA), len(partsB))
}

func isDigitsOnly(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func compareNumericIdentifiers(a, b string) int {
	ta := strings.TrimLeft(a, "0")
	tb := strings.TrimLeft(b, "0")
	if c := cmp.Compare(len(ta), len(tb)); c != 0 {
		return c
	}
	return cmp.Compare(ta, tb)
}
