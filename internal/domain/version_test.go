package domain

import "testing"

func TestParseVersionAcceptsTheFormsSDKsSend(t *testing.T) {
	cases := []struct {
		raw     string
		want    Version
		wantOK  bool
		comment string
	}{
		{raw: "1.2.3", want: Version{Major: 1, Minor: 2, Patch: 3}, wantOK: true,
			comment: "the bare form"},
		{raw: "myapp@1.2.3", want: Version{Package: "myapp", Major: 1, Minor: 2, Patch: 3}, wantOK: true,
			comment: "the form the SDKs recommend"},
		{raw: "v1.2.3", want: Version{Major: 1, Minor: 2, Patch: 3}, wantOK: true,
			comment: "half the ecosystem writes the v"},
		{raw: "myapp@v1.2.3", want: Version{Package: "myapp", Major: 1, Minor: 2, Patch: 3}, wantOK: true},
		{raw: "1.2.3-rc.1", want: Version{Major: 1, Minor: 2, Patch: 3, PreRelease: "rc.1"}, wantOK: true},
		{raw: "1.2.3+build.7", want: Version{Major: 1, Minor: 2, Patch: 3}, wantOK: true,
			comment: "build metadata is not part of precedence (semver §10)"},
		{raw: "1.2.3-rc.1+build.7", want: Version{Major: 1, Minor: 2, Patch: 3, PreRelease: "rc.1"}, wantOK: true},
		{raw: "  1.2.3  ", want: Version{Major: 1, Minor: 2, Patch: 3}, wantOK: true,
			comment: "git describe leaves a newline behind"},
		{raw: "0.0.0", want: Version{}, wantOK: true},

		{raw: "", comment: "no version at all"},
		{raw: "1.2", comment: "two components is not semver, and reading it as 1.2.0 would be a guess"},
		{raw: "1.2.3.4", comment: "four is not semver either"},
		{raw: "a3f9c1e", comment: "a git sha, which is the commonest release identifier there is"},
		{raw: "2026-08-29", comment: "a date stamp"},
		{raw: "1.2.x", comment: "a range, not a version"},
		{raw: "-1.2.3", comment: "a sign is not a digit"},
		{raw: "1.2.-3"},
		{raw: "1.2.3-", comment: "an empty pre-release is malformed"},
		{raw: "build-42"},
		{raw: "myapp@", comment: "a package with no version"},
	}

	for _, testCase := range cases {
		t.Run(testCase.raw, func(t *testing.T) {
			got, ok := ParseVersion(testCase.raw)
			if ok != testCase.wantOK {
				t.Fatalf("ParseVersion(%q) ok = %v, want %v (%s)",
					testCase.raw, ok, testCase.wantOK, testCase.comment)
			}
			if ok && got != testCase.want {
				t.Errorf("ParseVersion(%q) = %+v, want %+v", testCase.raw, got, testCase.want)
			}
		})
	}
}

func TestParseVersionRejectsAnOverlongIdentifier(t *testing.T) {
	// The version is an indexed column and a path segment. Something the size
	// of a payload is not a version, whatever it parses as.
	long := make([]byte, MaxVersionLen+1)
	for index := range long {
		long[index] = 'a'
	}
	if _, ok := ParseVersion(string(long)); ok {
		t.Error("an over-long identifier parsed as a version")
	}
}

func TestVersionCompareFollowsSemver(t *testing.T) {
	cases := []struct {
		a, b    string
		want    int
		comment string
	}{
		{a: "1.0.0", b: "1.0.0", want: 0},
		{a: "1.0.1", b: "1.0.0", want: 1},
		{a: "1.1.0", b: "1.0.9", want: 1, comment: "minor beats patch"},
		{a: "2.0.0", b: "1.99.99", want: 1, comment: "major beats everything"},
		{a: "1.0.0", b: "1.0.0-rc.1", want: 1, comment: "a final release is newer than its own pre-release"},
		{a: "1.0.0-rc.1", b: "1.0.0-rc.2", want: -1},
		{a: "1.0.0-alpha", b: "1.0.0-beta", want: -1},
		{a: "1.0.0-alpha", b: "1.0.0-alpha.1", want: -1, comment: "more identifiers wins when the shared ones tie"},
		{a: "1.0.0-1", b: "1.0.0-alpha", want: -1, comment: "numeric identifiers rank below alphanumeric (semver §11)"},
		{a: "1.0.0-2", b: "1.0.0-10", want: -1, comment: "numeric identifiers compare numerically, not as text"},
		{a: "1.0.0+a", b: "1.0.0+b", want: 0, comment: "build metadata is ignored entirely"},
		// The ordering example from the specification, checked as a chain.
		{a: "1.0.0-alpha.1", b: "1.0.0-alpha.beta", want: -1},
		{a: "1.0.0-beta.2", b: "1.0.0-beta.11", want: -1},
		{a: "1.0.0-beta.11", b: "1.0.0-rc.1", want: -1},
	}

	for _, testCase := range cases {
		t.Run(testCase.a+" vs "+testCase.b, func(t *testing.T) {
			left, ok := ParseVersion(testCase.a)
			if !ok {
				t.Fatalf("%q did not parse", testCase.a)
			}
			right, ok := ParseVersion(testCase.b)
			if !ok {
				t.Fatalf("%q did not parse", testCase.b)
			}
			if got := left.Compare(right); got != testCase.want {
				t.Errorf("Compare(%q, %q) = %d, want %d (%s)",
					testCase.a, testCase.b, got, testCase.want, testCase.comment)
			}
			// Antisymmetry, checked on every pair rather than trusted.
			if got := right.Compare(left); got != -testCase.want {
				t.Errorf("Compare(%q, %q) = %d, want %d — not antisymmetric",
					testCase.b, testCase.a, got, -testCase.want)
			}
		})
	}
}

func TestCompareReleasesOrdersSemverWithoutEverHavingSeenIt(t *testing.T) {
	// This is what lets a deploy be understood before its first error
	// arrives: neither version is in the order map.
	if got := CompareReleases("app@1.0.1", "app@1.0.0", nil); got != 1 {
		t.Errorf("CompareReleases = %d, want 1", got)
	}
}

func TestCompareReleasesFallsBackToFirstSight(t *testing.T) {
	// Two git shas. Nothing about the strings says which came first; the only
	// fact the product has is which one it saw first.
	order := ReleaseOrder{"a3f9c1e": 1, "b7d2f04": 2}

	if got := CompareReleases("b7d2f04", "a3f9c1e", order); got != 1 {
		t.Errorf("the later-seen sha compared %d, want 1", got)
	}
	if got := CompareReleases("a3f9c1e", "b7d2f04", order); got != -1 {
		t.Errorf("the earlier-seen sha compared %d, want -1", got)
	}
	if got := CompareReleases("a3f9c1e", "a3f9c1e", order); got != 0 {
		t.Errorf("a sha compared to itself = %d, want 0", got)
	}
}

func TestCompareReleasesTreatsAnUnseenReleaseAsNewer(t *testing.T) {
	// Something is emitting it now and was not emitting it before. That is
	// weak evidence, but it points one way, and the alternative — treating it
	// as older — would silently suppress the regression that a brand new
	// build introduced.
	order := ReleaseOrder{"a3f9c1e": 1}

	if got := CompareReleases("deadbee", "a3f9c1e", order); got != 1 {
		t.Errorf("unseen vs seen = %d, want 1", got)
	}
	if got := CompareReleases("a3f9c1e", "deadbee", order); got != -1 {
		t.Errorf("seen vs unseen = %d, want -1", got)
	}
	if got := CompareReleases("deadbee", "cafe123", order); got != 0 {
		t.Errorf("two unseen releases = %d, want 0 — there is nothing to order them by", got)
	}
}

func TestCompareReleasesRefusesToOrderDifferentPackages(t *testing.T) {
	// "api@2.0.0" is not newer than "web@3.1.0": they are different things
	// that both happen to count upwards. Only first sight can order them.
	order := ReleaseOrder{"web@3.1.0": 1, "api@2.0.0": 2}

	if got := CompareReleases("api@2.0.0", "web@3.1.0", order); got != 1 {
		t.Errorf("different packages ordered by number instead of first sight: %d", got)
	}
	if got := CompareReleases("api@2.0.0", "web@3.1.0", nil); got != 0 {
		t.Errorf("different packages with nothing to go on = %d, want 0", got)
	}
}

func TestCompareReleasesTreatsAnAbsentReleaseAsOlder(t *testing.T) {
	// An event with no release says nothing about which deploy it came from.
	// Calling that "newer" would reopen an issue on no evidence at all.
	if got := CompareReleases("", "app@1.0.0", nil); got != -1 {
		t.Errorf("no release vs a named one = %d, want -1", got)
	}
	if got := CompareReleases("app@1.0.0", "", nil); got != 1 {
		t.Errorf("a named release vs none = %d, want 1", got)
	}
	if got := CompareReleases("", "", nil); got != 0 {
		t.Errorf("two absences = %d, want 0", got)
	}
}

func TestCompareReleasesIgnoresSurroundingWhitespace(t *testing.T) {
	if got := CompareReleases(" app@1.0.0\n", "app@1.0.0", nil); got != 0 {
		t.Errorf("a trailing newline made a release into a different one: %d", got)
	}
}

func TestCompareReleasesIsAntisymmetricAcrossEveryBranch(t *testing.T) {
	order := ReleaseOrder{"seen-a": 1, "seen-b": 2, "app@1.0.0": 3}
	values := []string{"", "app@1.0.0", "app@1.0.1", "app@1.0.0-rc.1", "seen-a", "seen-b", "unseen", "lib@2.0.0"}

	for _, a := range values {
		for _, b := range values {
			forward := CompareReleases(a, b, order)
			backward := CompareReleases(b, a, order)
			if forward != -backward {
				t.Errorf("CompareReleases(%q,%q)=%d but (%q,%q)=%d", a, b, forward, b, a, backward)
			}
		}
	}
}
