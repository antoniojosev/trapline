package domain

import (
	"testing"
	"time"
)

// releaseObservation is an event that names a release, with the order the
// project's releases were first seen in.
func releaseObservation(at time.Time, release string, order ReleaseOrder) Observation {
	obs := observation(at)
	obs.Release = release
	obs.ReleaseOrder = order
	return obs
}

func TestNewIssueRecordsTheReleaseItWasBornIn(t *testing.T) {
	issue, err := NewIssue(1, "abc", releaseObservation(testNow, "app@1.0.0", nil))
	if err != nil {
		t.Fatalf("NewIssue: %v", err)
	}
	if issue.FirstRelease != "app@1.0.0" || issue.LastRelease != "app@1.0.0" {
		t.Errorf("first/last release = %q/%q", issue.FirstRelease, issue.LastRelease)
	}
}

func TestFirstReleaseNeverMoves(t *testing.T) {
	// LastRelease is a moving pointer; FirstRelease is a fact about the past.
	// Confusing the two would make "what shipped this" unanswerable after the
	// second deploy.
	issue, err := NewIssue(1, "abc", releaseObservation(testNow, "app@1.0.0", nil))
	if err != nil {
		t.Fatalf("NewIssue: %v", err)
	}

	updated, _ := issue.Observe(releaseObservation(testNow.Add(time.Hour), "app@2.0.0", nil))

	if updated.FirstRelease != "app@1.0.0" {
		t.Errorf("FirstRelease = %q, want it unchanged", updated.FirstRelease)
	}
	if updated.LastRelease != "app@2.0.0" {
		t.Errorf("LastRelease = %q, want it to follow the newest event", updated.LastRelease)
	}
}

func TestFirstReleaseBackfillsWhenItWasNeverKnown(t *testing.T) {
	// An issue that existed before releases did, or whose first events
	// carried none. The first event that names one fills it in.
	issue := newTestIssue(t)
	if issue.FirstRelease != "" {
		t.Fatalf("FirstRelease = %q, want empty for this fixture", issue.FirstRelease)
	}

	updated, _ := issue.Observe(releaseObservation(testNow.Add(time.Hour), "app@1.0.0", nil))

	if updated.FirstRelease != "app@1.0.0" {
		t.Errorf("FirstRelease = %q, want the first one seen", updated.FirstRelease)
	}
}

func TestResolveInNextReleasePinsTheCurrentRelease(t *testing.T) {
	issue, err := NewIssue(1, "abc", releaseObservation(testNow, "app@1.0.0", nil))
	if err != nil {
		t.Fatalf("NewIssue: %v", err)
	}

	resolved := issue.ResolveInNextRelease(testNow)

	if resolved.Status != StatusResolved {
		t.Errorf("Status = %q", resolved.Status)
	}
	if !resolved.ResolveNextRelease {
		t.Error("ResolveNextRelease is false")
	}
	if resolved.ResolvedInRelease != "app@1.0.0" {
		t.Errorf("ResolvedInRelease = %q, want the release it was last seen in", resolved.ResolvedInRelease)
	}
	if resolved.ResolvedAt == nil || !resolved.ResolvedAt.Equal(testNow) {
		t.Errorf("ResolvedAt = %v", resolved.ResolvedAt)
	}
}

func TestResolveInNextReleaseWithNoReleaseDegradesToPlainResolve(t *testing.T) {
	// Refusing would make the button fail on exactly the projects that have
	// not set up releases yet — the worst moment to teach someone a concept.
	resolved := newTestIssue(t).ResolveInNextRelease(testNow)

	if resolved.ResolveNextRelease {
		t.Error("ResolveNextRelease is true with no release to pin to")
	}
	updated, regressed := resolved.Observe(observation(testNow.Add(time.Hour)))
	if !regressed || updated.Status != StatusUnresolved {
		t.Error("with nothing to compare against, the next event must reopen")
	}
}

// TestAnOldPodDoesNotReopenWhatWasJustFixed is the reason this rule exists.
func TestAnOldPodDoesNotReopenWhatWasJustFixed(t *testing.T) {
	order := ReleaseOrder{"app@1.0.0": 1}
	issue, err := NewIssue(1, "abc", releaseObservation(testNow, "app@1.0.0", order))
	if err != nil {
		t.Fatalf("NewIssue: %v", err)
	}
	resolved := issue.ResolveInNextRelease(testNow)

	// The deploy is rolling. Two instances are still on the old build.
	first, regressed := resolved.Observe(releaseObservation(testNow.Add(time.Minute), "app@1.0.0", order))
	if regressed {
		t.Fatal("an event from the resolved release reopened the issue")
	}
	second, _ := first.Observe(releaseObservation(testNow.Add(2*time.Minute), "app@1.0.0", order))

	if second.Status != StatusResolved {
		t.Errorf("Status = %q, want it to stay resolved", second.Status)
	}
	if second.Times != 3 {
		t.Errorf("Times = %d, want 3 — a suppressed event still counts", second.Times)
	}
	if second.SeenInResolvedReleaseCount != 2 {
		t.Errorf("SeenInResolvedReleaseCount = %d, want 2", second.SeenInResolvedReleaseCount)
	}
	if second.Regressions != 0 {
		t.Errorf("Regressions = %d, want 0", second.Regressions)
	}
	// The pin survives, or the third old event would reopen it.
	if !second.ResolveNextRelease || second.ResolvedInRelease != "app@1.0.0" {
		t.Error("the resolution was forgotten while suppressing")
	}
}

func TestAnOlderReleaseDoesNotReopenEither(t *testing.T) {
	// A mobile client two versions behind, or a queue draining stale jobs.
	order := ReleaseOrder{"app@0.9.0": 1, "app@1.0.0": 2}
	issue, err := NewIssue(1, "abc", releaseObservation(testNow, "app@1.0.0", order))
	if err != nil {
		t.Fatalf("NewIssue: %v", err)
	}

	updated, regressed := issue.ResolveInNextRelease(testNow).
		Observe(releaseObservation(testNow.Add(time.Minute), "app@0.9.0", order))

	if regressed || updated.Status != StatusResolved {
		t.Error("an event from an older release reopened the issue")
	}
	if updated.SeenInResolvedReleaseCount != 1 {
		t.Errorf("SeenInResolvedReleaseCount = %d, want 1", updated.SeenInResolvedReleaseCount)
	}
}

func TestANewerReleaseIsARealRegression(t *testing.T) {
	order := ReleaseOrder{"app@1.0.0": 1}
	issue, err := NewIssue(1, "abc", releaseObservation(testNow, "app@1.0.0", order))
	if err != nil {
		t.Fatalf("NewIssue: %v", err)
	}

	updated, regressed := issue.ResolveInNextRelease(testNow).
		Observe(releaseObservation(testNow.Add(time.Hour), "app@1.0.1", order))

	if !regressed {
		t.Fatal("the fix did not work and the product did not say so")
	}
	if updated.Status != StatusUnresolved {
		t.Errorf("Status = %q, want unresolved", updated.Status)
	}
	if updated.Regressions != 1 {
		t.Errorf("Regressions = %d, want 1", updated.Regressions)
	}
	if updated.FirstRelease != "app@1.0.0" {
		t.Errorf("FirstRelease = %q, want the release it was born in", updated.FirstRelease)
	}
	// The pin is gone: the issue is open again and has nothing to compare to.
	if updated.ResolveNextRelease || updated.ResolvedInRelease != "" || updated.ResolvedAt != nil {
		t.Error("a reopened issue kept its resolution")
	}
	if updated.SeenInResolvedReleaseCount != 0 {
		t.Errorf("SeenInResolvedReleaseCount = %d, want 0 — nothing was suppressed", updated.SeenInResolvedReleaseCount)
	}
}

func TestSuppressionThenRegressionInOneTimeline(t *testing.T) {
	// The whole story, in the order it happens in production.
	order := ReleaseOrder{"app@1.0.0": 1, "app@1.0.1": 2}
	issue, err := NewIssue(1, "abc", releaseObservation(testNow, "app@1.0.0", order))
	if err != nil {
		t.Fatalf("NewIssue: %v", err)
	}

	state := issue.ResolveInNextRelease(testNow)
	for step := 1; step <= 3; step++ {
		var regressed bool
		state, regressed = state.Observe(
			releaseObservation(testNow.Add(time.Duration(step)*time.Minute), "app@1.0.0", order))
		if regressed {
			t.Fatalf("event %d from the resolved release reopened the issue", step)
		}
	}
	state, regressed := state.Observe(releaseObservation(testNow.Add(time.Hour), "app@1.0.1", order))

	if !regressed {
		t.Fatal("the event from the new build was not reported as a regression")
	}
	if state.SeenInResolvedReleaseCount != 3 {
		t.Errorf("SeenInResolvedReleaseCount = %d, want the three suppressed ones", state.SeenInResolvedReleaseCount)
	}
	if state.Times != 5 {
		t.Errorf("Times = %d, want every event counted", state.Times)
	}
}

func TestPlainResolveStillReopensOnAnything(t *testing.T) {
	// Someone who resolved without naming a release is claiming it is fixed
	// now, not fixed soon. The old meaning has to survive intact.
	order := ReleaseOrder{"app@1.0.0": 1}
	issue, err := NewIssue(1, "abc", releaseObservation(testNow, "app@1.0.0", order))
	if err != nil {
		t.Fatalf("NewIssue: %v", err)
	}

	updated, regressed := issue.Resolve(testNow).
		Observe(releaseObservation(testNow.Add(time.Minute), "app@1.0.0", order))

	if !regressed || updated.Status != StatusUnresolved {
		t.Error("a plain resolve stopped reopening")
	}
	if updated.Regressions != 1 {
		t.Errorf("Regressions = %d, want 1", updated.Regressions)
	}
}

func TestAnUnknownReleaseReopensAResolvedIssue(t *testing.T) {
	// A build nobody has registered is emitting this now. That is weak
	// evidence, but treating it as older would silently swallow the
	// regression a brand new deploy introduced.
	order := ReleaseOrder{"a3f9c1e": 1}
	issue, err := NewIssue(1, "abc", releaseObservation(testNow, "a3f9c1e", order))
	if err != nil {
		t.Fatalf("NewIssue: %v", err)
	}

	_, regressed := issue.ResolveInNextRelease(testNow).
		Observe(releaseObservation(testNow.Add(time.Hour), "deadbee", order))

	if !regressed {
		t.Error("an event from an unregistered build did not reopen the issue")
	}
}

func TestAnEventWithNoReleaseDoesNotReopenAPinnedIssue(t *testing.T) {
	// It says nothing about which deploy it came from, and reopening on no
	// evidence is the false positive this feature exists to remove.
	order := ReleaseOrder{"app@1.0.0": 1}
	issue, err := NewIssue(1, "abc", releaseObservation(testNow, "app@1.0.0", order))
	if err != nil {
		t.Fatalf("NewIssue: %v", err)
	}

	updated, regressed := issue.ResolveInNextRelease(testNow).
		Observe(observation(testNow.Add(time.Hour)))

	if regressed || updated.Status != StatusResolved {
		t.Error("an event with no release reopened a pinned issue")
	}
}

func TestManualReopenIsNotARegression(t *testing.T) {
	// A regression is the product saying something came back. A manual reopen
	// is a person saying they were wrong. Counting it would corrupt the one
	// number that means "this keeps happening".
	issue := newTestIssue(t).Resolve(testNow).Reopen()

	if issue.Regressions != 0 {
		t.Errorf("Regressions = %d, want 0", issue.Regressions)
	}
	if issue.ResolvedAt != nil || issue.ResolveNextRelease {
		t.Error("a manual reopen kept the resolution")
	}
}

func TestIgnoringForgetsTheResolution(t *testing.T) {
	issue := newTestIssue(t).ResolveInNextRelease(testNow).Ignore()

	if issue.Status != StatusIgnored {
		t.Errorf("Status = %q", issue.Status)
	}
	if issue.ResolveNextRelease || issue.ResolvedAt != nil {
		t.Error("an ignored issue kept a resolution that no longer applies")
	}
}

func TestResolvingAgainResetsTheSuppressionCount(t *testing.T) {
	// The count answers "how many did we suppress for this resolution", so a
	// new resolution starts at zero or the number means nothing.
	order := ReleaseOrder{"app@1.0.0": 1}
	issue, err := NewIssue(1, "abc", releaseObservation(testNow, "app@1.0.0", order))
	if err != nil {
		t.Fatalf("NewIssue: %v", err)
	}
	suppressed, _ := issue.ResolveInNextRelease(testNow).
		Observe(releaseObservation(testNow.Add(time.Minute), "app@1.0.0", order))

	again := suppressed.ResolveInNextRelease(testNow.Add(time.Hour))

	if again.SeenInResolvedReleaseCount != 0 {
		t.Errorf("SeenInResolvedReleaseCount = %d, want 0", again.SeenInResolvedReleaseCount)
	}
}

func TestVerdictIsAPureDecision(t *testing.T) {
	pinned := Issue{ResolveNextRelease: true, ResolvedInRelease: "app@1.0.0"}
	plain := Issue{ResolvedInRelease: "app@1.0.0"}
	order := ReleaseOrder{"app@1.0.0": 1}

	cases := []struct {
		name    string
		issue   Issue
		release string
		want    Verdict
	}{
		{name: "pinned, same release", issue: pinned, release: "app@1.0.0", want: VerdictExpected},
		{name: "pinned, older", issue: pinned, release: "app@0.1.0", want: VerdictExpected},
		{name: "pinned, newer", issue: pinned, release: "app@1.0.1", want: VerdictRegression},
		{name: "not pinned, same release", issue: plain, release: "app@1.0.0", want: VerdictRegression},
		{name: "not pinned, no release", issue: plain, release: "", want: VerdictRegression},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := testCase.issue.Verdict(testCase.release, order); got != testCase.want {
				t.Errorf("Verdict = %v, want %v", got, testCase.want)
			}
		})
	}
}
