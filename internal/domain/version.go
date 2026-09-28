package domain

import (
	"strconv"
	"strings"
)

// MaxVersionLen bounds a release version so one row stays small and a version
// cannot be used to write a megabyte into an indexed column.
const MaxVersionLen = 250

// ReleaseOrder is the order a project's releases were first seen in: one rank
// per version, higher meaning later.
//
// It exists because most real release identifiers are not versions at all.
// A git sha, a build number from CI, a date stamp — none of them can be
// ordered by reading them, and the only fact the product actually has about
// them is which one showed up first. That fact lives in storage, so the
// domain takes it as an argument rather than reaching for it, which is what
// keeps the comparison a pure function that can be tested exhaustively.
//
// A version absent from the map has never been seen.
type ReleaseOrder map[string]int64

// Version is a release identifier that parses as semantic versioning.
//
// Package is the part before the "@" in the "myapp@1.2.3" form the SDKs
// recommend. It is kept because two packages' version numbers are not
// comparable to each other: "api@2.0.0" is not "newer" than "web@3.1.0", they
// are different things that happen to both count upwards.
type Version struct {
	// Package is the name before "@", empty when the version stands alone.
	Package string
	// Major, Minor and Patch are the three numeric components.
	Major, Minor, Patch int64
	// PreRelease is what followed a "-", empty when the version is final.
	PreRelease string
}

// ParseVersion reads "pkg@1.2.3-rc.1+build" or "1.2.3", reporting whether it
// is semantic versioning at all.
//
// Exactly three numeric components are required. Being lenient here — reading
// "1.2" as "1.2.0", or "2026.08.29-nightly" as a version — would silently
// take identifiers that are not versions into the semver branch of the
// comparison, and being wrong about which release is newer is precisely the
// failure this whole feature exists to prevent. Anything that does not parse
// falls back to the order it was first seen in, which is never wrong, only
// less informative.
func ParseVersion(raw string) (Version, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > MaxVersionLen {
		return Version{}, false
	}

	var version Version
	// The last "@", not the first: a package name is a package name, but an
	// email-looking prefix should not silently swallow the separator.
	if at := strings.LastIndexByte(raw, '@'); at >= 0 {
		version.Package = raw[:at]
		raw = raw[at+1:]
	}
	// A leading "v" is what half the ecosystem writes and it means nothing
	// beyond "this is a version".
	raw = strings.TrimPrefix(raw, "v")

	// Build metadata is explicitly not part of precedence (semver §10), so it
	// is dropped rather than kept and ignored later.
	if plus := strings.IndexByte(raw, '+'); plus >= 0 {
		raw = raw[:plus]
	}
	if dash := strings.IndexByte(raw, '-'); dash >= 0 {
		version.PreRelease = raw[dash+1:]
		raw = raw[:dash]
		if version.PreRelease == "" {
			return Version{}, false
		}
	}

	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	numbers := [3]int64{}
	for index, part := range parts {
		number, err := strconv.ParseInt(part, 10, 64)
		if err != nil || number < 0 || !isDigits(part) {
			return Version{}, false
		}
		numbers[index] = number
	}
	version.Major, version.Minor, version.Patch = numbers[0], numbers[1], numbers[2]
	return version, true
}

// isDigits rejects the forms strconv accepts but semver does not: a sign, and
// the underscores Go's literal syntax allows.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Compare orders two versions of the same package: -1, 0 or 1.
func (v Version) Compare(other Version) int {
	if result := compareInt(v.Major, other.Major); result != 0 {
		return result
	}
	if result := compareInt(v.Minor, other.Minor); result != 0 {
		return result
	}
	if result := compareInt(v.Patch, other.Patch); result != 0 {
		return result
	}
	return comparePreRelease(v.PreRelease, other.PreRelease)
}

// comparePreRelease implements semver §11: a version with a pre-release is
// older than the same version without one, and identifiers are compared
// left to right, numerically when both are numeric.
func comparePreRelease(a, b string) int {
	switch {
	case a == b:
		return 0
	case a == "":
		// 1.0.0 is newer than 1.0.0-rc.1. This is the rule that makes
		// "resolved in 1.0.0-rc.2, event from 1.0.0" a genuine regression.
		return 1
	case b == "":
		return -1
	}

	left, right := strings.Split(a, "."), strings.Split(b, ".")
	for index := 0; index < len(left) && index < len(right); index++ {
		if result := comparePreReleaseIdentifier(left[index], right[index]); result != 0 {
			return result
		}
	}
	// Everything shared is equal, so the longer set of identifiers wins.
	return compareInt(int64(len(left)), int64(len(right)))
}

func comparePreReleaseIdentifier(a, b string) int {
	leftNumeric, rightNumeric := isDigits(a), isDigits(b)
	switch {
	case leftNumeric && rightNumeric:
		left, _ := strconv.ParseInt(a, 10, 64)
		right, _ := strconv.ParseInt(b, 10, 64)
		return compareInt(left, right)
	case leftNumeric:
		// Numeric identifiers always have lower precedence than alphanumeric
		// ones, so rc.1 is newer than 1.
		return -1
	case rightNumeric:
		return 1
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func compareInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// CompareReleases orders two release versions of one project: -1 when a came
// before b, 1 when a came after, 0 when they cannot be told apart.
//
// The rules, in the order they apply:
//
//  1. An absent release is never newer than a named one. An event that
//     carries no release at all says nothing about which deploy it came from,
//     and guessing "newer" there would reopen an issue on no evidence —
//     exactly the false positive resolving in the next release exists to
//     avoid.
//  2. Two versions of the same package that both parse as semver are ordered
//     as semver. This is the only branch that can order a release the server
//     has never seen, which is what makes a deploy visible before its first
//     error arrives.
//  3. Otherwise, the order they were first seen in.
//  4. A release nobody has seen, against one that has been seen, is newer.
//     Something is emitting it now and it was not emitting it before.
//
// Two different unseen releases return 0: there is genuinely no information
// with which to order them, and inventing an answer would be worse than
// admitting it.
func CompareReleases(a, b string, order ReleaseOrder) int {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == b {
		return 0
	}
	switch {
	case a == "":
		return -1
	case b == "":
		return 1
	}

	versionA, okA := ParseVersion(a)
	versionB, okB := ParseVersion(b)
	if okA && okB && versionA.Package == versionB.Package {
		return versionA.Compare(versionB)
	}

	rankA, seenA := order[a]
	rankB, seenB := order[b]
	switch {
	case seenA && seenB:
		return compareInt(rankA, rankB)
	case seenA:
		return -1
	case seenB:
		return 1
	default:
		return 0
	}
}
