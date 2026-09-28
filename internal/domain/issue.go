package domain

import (
	"fmt"
	"strings"
	"time"
)

// Level is an issue's severity, mirroring the protocol's levels.
//
// The domain keeps its own type rather than importing the protocol package:
// the rule about what a level means belongs here, and the wire format is one
// adapter's problem.
type Level string

// The levels an issue can carry.
const (
	LevelFatal   Level = "fatal"
	LevelError   Level = "error"
	LevelWarning Level = "warning"
	LevelInfo    Level = "info"
	LevelDebug   Level = "debug"
)

// ValidLevel reports whether a level is known, defaulting to error.
func ValidLevel(level Level) Level {
	switch level {
	case LevelFatal, LevelError, LevelWarning, LevelInfo, LevelDebug:
		return level
	default:
		return LevelError
	}
}

// IssueStatus is where an issue sits in its lifecycle.
type IssueStatus string

// The statuses an issue can be in.
const (
	// StatusUnresolved is the default: something is wrong and nobody has said
	// otherwise.
	StatusUnresolved IssueStatus = "unresolved"
	// StatusResolved means someone declared it fixed. A new event reopens it
	// as a regression, which is the signal the product exists to give.
	StatusResolved IssueStatus = "resolved"
	// StatusIgnored means someone decided not to care. New events still
	// count — the number is how you notice you were wrong — but they do not
	// reopen it or raise an alert.
	StatusIgnored IssueStatus = "ignored"
)

// ValidStatus reports whether s is a known status.
func (s IssueStatus) ValidStatus() bool {
	switch s {
	case StatusUnresolved, StatusResolved, StatusIgnored:
		return true
	default:
		return false
	}
}

// MaxTitleLen bounds a title so an issue list stays readable and a row stays
// small.
const MaxTitleLen = 300

// Issue is a group of events that are the same problem.
type Issue struct {
	ID          int64
	ProjectID   int64
	Fingerprint string
	// GroupingVersion records which algorithm produced the fingerprint, so a
	// future change is a migration with a plan rather than a morning where
	// everyone's history is silently wrong (ADR 003).
	GroupingVersion int
	Title           string
	// Culprit is the human-readable location, e.g. "myapp.views in checkout".
	Culprit   string
	Level     Level
	Status    IssueStatus
	FirstSeen time.Time
	LastSeen  time.Time
	// Times is how many events have landed in this issue.
	Times int64
	// LastRelease is the most recent release an event arrived from.
	LastRelease string
	// FirstRelease is the release the very first event came from. It is the
	// answer to "what shipped this", and it never changes: unlike LastRelease
	// it is a fact about the past, not a moving pointer.
	FirstRelease string
	// ResolvedAt is when someone declared it fixed, nil while it is not.
	ResolvedAt *time.Time
	// ResolvedInRelease is the release the issue was last seen in at the
	// moment it was resolved. It is the line an event has to cross to count
	// as a regression under ResolveNextRelease.
	ResolvedInRelease string
	// ResolveNextRelease means "fixed, and the fix ships next". Events from
	// the release it was resolved in, or from anything older, are expected:
	// they are the machines that have not been redeployed yet.
	ResolveNextRelease bool
	// Regressions counts how many times this issue has come back after being
	// resolved. An issue with three of them is a different kind of problem
	// from one with none, and the number is the only thing that says so.
	Regressions int64
	// RegressedInRelease is the release of the most recent regression, which
	// is what lets a release page say "this deploy brought two issues back".
	// Recorded rather than inferred from LastRelease, which keeps moving with
	// every later event and would hand the blame to whichever release
	// happened to be deployed next.
	RegressedInRelease string
	// SeenInResolvedReleaseCount is how many events arrived from the resolved
	// release after it was resolved. It is what makes "we suppressed 400 of
	// these" visible instead of the product looking like it lost them.
	SeenInResolvedReleaseCount int64
}

// Observation is what a new event tells an issue.
type Observation struct {
	At      time.Time
	Level   Level
	Title   string
	Culprit string
	Release string
	// ReleaseOrder is the order this project's releases were first seen in,
	// which is the only way to order release identifiers that are not
	// versions (a git sha, a build number). It is optional: without it the
	// comparison still orders anything that parses as semver, and treats the
	// rest as indistinguishable rather than guessing.
	ReleaseOrder ReleaseOrder
}

// NewIssue builds an issue from the first event that created it.
func NewIssue(projectID int64, fingerprint string, first Observation) (Issue, error) {
	if projectID <= 0 {
		return Issue{}, fmt.Errorf("%w: project id must be positive", ErrInvalidProject)
	}
	if fingerprint == "" {
		return Issue{}, fmt.Errorf("%w: fingerprint is required", ErrInvalidIssue)
	}
	at := first.At.UTC()
	if at.IsZero() {
		return Issue{}, fmt.Errorf("%w: timestamp is required", ErrInvalidIssue)
	}

	return Issue{
		ProjectID:       projectID,
		Fingerprint:     fingerprint,
		GroupingVersion: GroupingVersion,
		Title:           truncateTitle(first.Title),
		Culprit:         first.Culprit,
		Level:           ValidLevel(first.Level),
		Status:          StatusUnresolved,
		FirstSeen:       at,
		LastSeen:        at,
		Times:           1,
		LastRelease:     first.Release,
		// The first event is, by definition, the one that named the release
		// this issue was born in.
		FirstRelease: first.Release,
	}, nil
}

// Observe records a new event and reports whether this reopened the issue.
//
// Reopening is the product's most valuable single signal: "you thought this
// was fixed and it is back". Everything else here is bookkeeping around
// getting that one right.
func (i Issue) Observe(observation Observation) (updated Issue, regressed bool) {
	at := observation.At.UTC()

	// Events can arrive late — a mobile client that was offline, a retry after
	// an outage — so last-seen only moves forward. Letting it move backwards
	// would make an active issue look stale.
	newest := at.After(i.LastSeen)
	if newest {
		i.LastSeen = at
	}
	if at.Before(i.FirstSeen) {
		i.FirstSeen = at
	}
	i.Times++

	// The release follows the newest occurrence, not the most recently
	// ingested event. The two are different whenever something arrives late,
	// and the panel found the discrepancy: the header read one release while
	// the newest occurrence beneath it read another, because the issue tracked
	// arrival order and the event list tracked occurrence order. Two rules for
	// "the release" is one rule too many, and occurrence order is the one a
	// human means.
	if observation.Release != "" && (newest || i.LastRelease == "") {
		i.LastRelease = observation.Release
	}
	// The level tracks the worst seen: an issue that has ever been fatal
	// should not be quietly downgraded because the next occurrence was
	// reported as a warning.
	if severity(observation.Level) > severity(i.Level) {
		i.Level = ValidLevel(observation.Level)
	}
	// A title only fills in when it was missing. Rewriting it on every event
	// would make an issue's name flicker between occurrences that differ only
	// in an interpolated value.
	if i.Title == "" && observation.Title != "" {
		i.Title = truncateTitle(observation.Title)
	}
	if i.Culprit == "" {
		i.Culprit = observation.Culprit
	}
	// Backfill for an issue that existed before releases did, or one whose
	// first events carried no release. Only when empty: the first release is
	// a fact about the past and must not follow the newest event.
	if i.FirstRelease == "" && observation.Release != "" {
		i.FirstRelease = observation.Release
	}

	if i.Status == StatusResolved {
		if i.Verdict(observation.Release, observation.ReleaseOrder) == VerdictExpected {
			return i.SeeInResolvedRelease(), false
		}
		return i.Regress(observation.Release), true
	}
	// An ignored issue keeps counting but does not reopen. The count is how
	// someone notices they were wrong to ignore it; reopening would make
	// "ignore" mean nothing.
	return i, false
}

// Verdict is what an event from some release means for a resolved issue.
type Verdict int

const (
	// VerdictRegression means the event is proof the fix did not work, or
	// that there was no fix. The issue reopens.
	VerdictRegression Verdict = iota
	// VerdictExpected means the event came from the release the issue was
	// resolved in, or from an older one — the pod that has not been
	// redeployed, the mobile client still on last week's build. It is
	// counted and stored, but it does not reopen anything.
	VerdictExpected
)

// Verdict decides what an event from eventRelease does to this issue.
//
// This is the rule the whole feature exists for. Without it, resolving an
// issue and deploying the fix produces a reopened issue within seconds —
// from the instances that were still running the broken build when the deploy
// started. A user who sees that twice stops believing the status field, and a
// status nobody believes is worse than no status at all.
//
// It only means anything for a resolved issue; the caller is what knows the
// issue is resolved, so this does not re-check it.
func (i Issue) Verdict(eventRelease string, order ReleaseOrder) Verdict {
	if !i.ResolveNextRelease {
		// Plain "resolved" keeps its old meaning: anything at all reopens it.
		// That is the right default, because someone who resolved without
		// naming a release is claiming it is fixed now, not fixed soon.
		return VerdictRegression
	}
	if CompareReleases(eventRelease, i.ResolvedInRelease, order) > 0 {
		return VerdictRegression
	}
	return VerdictExpected
}

// Regress reopens a resolved issue and records that it came back, and in
// which release.
func (i Issue) Regress(inRelease string) Issue {
	i.Status = StatusUnresolved
	i.Regressions++
	i.RegressedInRelease = inRelease
	return i.clearResolution()
}

// SeeInResolvedRelease counts an event that arrived from the release the
// issue was already resolved in, without reopening it.
func (i Issue) SeeInResolvedRelease() Issue {
	i.SeenInResolvedReleaseCount++
	return i
}

// Resolve marks the issue fixed as of now.
func (i Issue) Resolve(at time.Time) Issue {
	i.Status = StatusResolved
	resolved := at.UTC()
	i.ResolvedAt = &resolved
	i.ResolvedInRelease = ""
	i.ResolveNextRelease = false
	i.SeenInResolvedReleaseCount = 0
	return i
}

// ResolveInNextRelease marks the issue fixed as of the release it is
// currently being seen in, so only something newer counts as a regression.
//
// Resolving with no release recorded is allowed and degrades to the plain
// meaning: with no line to cross, the first event of any kind reopens it.
// Refusing instead would make the button fail on exactly the projects that
// have not set up releases yet, which is the worst moment to teach someone a
// concept.
func (i Issue) ResolveInNextRelease(at time.Time) Issue {
	i = i.Resolve(at)
	i.ResolvedInRelease = i.LastRelease
	i.ResolveNextRelease = i.LastRelease != ""
	return i
}

// Ignore mutes the issue without claiming it is fixed.
func (i Issue) Ignore() Issue {
	i.Status = StatusIgnored
	return i.clearResolution()
}

// Reopen puts the issue back in the unresolved state by hand.
//
// It does not count as a regression: a regression is the product telling you
// something came back, and this is a person saying they were wrong. Counting
// it would corrupt the one number that is supposed to mean "this keeps
// happening".
func (i Issue) Reopen() Issue {
	i.Status = StatusUnresolved
	return i.clearResolution()
}

// clearResolution forgets a resolution that no longer applies.
//
// The suppression count deliberately survives: "three events arrived from the
// resolved build before it really came back" is exactly what somebody reading
// a regression wants to know, and zeroing it here would throw it away at the
// only moment it becomes interesting. It resets when a new resolution starts,
// which is the point at which it would otherwise start meaning two things.
func (i Issue) clearResolution() Issue {
	i.ResolvedAt = nil
	i.ResolvedInRelease = ""
	i.ResolveNextRelease = false
	return i
}

// severity orders levels so the worst one wins.
func severity(level Level) int {
	switch ValidLevel(level) {
	case LevelFatal:
		return 5
	case LevelError:
		return 4
	case LevelWarning:
		return 3
	case LevelInfo:
		return 2
	case LevelDebug:
		return 1
	default:
		return 0
	}
}

func truncateTitle(title string) string {
	title = strings.Join(strings.Fields(title), " ")
	if len(title) <= MaxTitleLen {
		return title
	}
	// Cut on a rune boundary so a multi-byte character is never split into
	// invalid UTF-8 that a JSON encoder would then mangle.
	runes := []rune(title)
	if len(runes) > MaxTitleLen {
		runes = runes[:MaxTitleLen]
	}
	return string(runes) + "…"
}
