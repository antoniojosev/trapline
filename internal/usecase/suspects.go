package usecase

import (
	"context"
	"errors"
	"fmt"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
	"github.com/antoniojosev/trapline/internal/sentry"
)

// suspectEventLookback is how many stored occurrences are read looking for one
// that carries the release being blamed.
//
// The stacktrace of an issue is the same stacktrace by construction — that is
// what grouping means — so any occurrence would do for the file paths. The
// lookback exists for the one case where it does not: an issue that has been
// seen in two releases, where only the events from the release under suspicion
// were symbolicated with that build's maps. Five, because the cost is five
// decompressed payloads on a screen that already reads ten.
const suspectEventLookback = 5

// maxListedCommits bounds the candidate list returned when nothing could be
// attributed.
//
// A release may carry five thousand commits (domain.MaxCommitsPerRelease) and
// this endpoint is not the release page: the list is here so that "nothing
// matched" arrives with the evidence rather than as a bare no, and twenty is
// as many as anybody reads before going to the release itself.
const maxListedCommits = 20

// Suspects answers "which change probably caused this?" for one issue.
//
// The answer is the intersection between the files a release changed and the
// files this crash walked through, and it is computed when asked rather than
// stored: both halves move — a commit set arrives after the first error as
// often as before it, and a frame's path changes the moment somebody uploads
// the source maps that resolve it (ADR 019).
type Suspects struct {
	issues     ports.IssueRepository
	releases   ports.ReleaseRepository
	resolution ports.IssueResolutionRepository
}

// NewSuspects wires the use case.
func NewSuspects(
	issues ports.IssueRepository,
	releases ports.ReleaseRepository,
	resolution ports.IssueResolutionRepository,
) *Suspects {
	return &Suspects{issues: issues, releases: releases, resolution: resolution}
}

// SuspectReport is what one issue's attribution looks like, answer or not.
//
// It carries the candidates and a warning as well as the suspects, because
// the useful outcomes are not only "here is the commit". "This release has no
// commit set", "the commits arrived without file paths" and "nothing matched,
// and by the way this stacktrace is still minified" are each a specific thing
// the reader can go and fix, and collapsing them into an empty list would
// throw that away (ADR 019).
type SuspectReport struct {
	// Release is the release whose commits were considered: the one the issue
	// was first seen in.
	Release string
	// Suspects is the ranked answer, best first, at most domain.MaxSuspects.
	Suspects []domain.Suspect
	// Commits are the candidates, most recent first and bounded. Present
	// whether or not anything was attributed: a reader told "no suspect"
	// should be able to see what was ruled out.
	Commits []domain.Commit
	// CommitCount is how many the release actually holds, which is what says
	// whether Commits was truncated.
	CommitCount int
	// Warning explains an empty answer in the terms of what to do about it.
	// Empty when there are suspects.
	Warning string
	// Symbolicated says whether any frame considered had been resolved from a
	// source map. It is what tells "this code was not changed" apart from
	// "this stacktrace is still minified, so nothing could match".
	Symbolicated bool
}

// The four ways this can come back empty, each naming what to do about it.
const (
	warnNoRelease = "This issue has never carried a release, so there is no deploy to attribute it to. " +
		"Set `release` in the SDK's init."
	warnUnknownRelease = "The release this issue was first seen in is not registered here, " +
		"so there is no commit set to look in."
	warnNoCommits = "No commits are associated with this release. Send them from the deploy pipeline: " +
		"`trapline releases commits -repo . -from <sha> -to <sha>`, or `sentry-cli releases set-commits --local`."
	warnNoPaths = "These commits arrived without the files they changed, which is the whole input " +
		"to attribution. Re-send them with `trapline releases commits -repo .` or " +
		"`sentry-cli releases set-commits --local`, which include a patch set."
	warnNoMatch  = "None of the files this release changed appears in the stacktrace."
	warnMinified = "None of the files this release changed appears in the stacktrace — which is expected, " +
		"because these frames are still minified. Upload this build's source maps " +
		"(`trapline artifacts upload`) and the next event will resolve."
)

// For attributes one issue to the commits of the release it first appeared in.
func (s *Suspects) For(ctx context.Context, projectID, issueID int64) (SuspectReport, error) {
	// The issue first, so an id nobody owns is a 404 rather than an empty
	// report that looks exactly like an issue nothing could be attributed to.
	if _, err := s.issues.FindByID(ctx, projectID, issueID); err != nil {
		return SuspectReport{}, err
	}

	resolution, err := s.resolution.Resolution(ctx, projectID, issueID)
	if err != nil {
		return SuspectReport{}, err
	}
	// The release the issue was *first* seen in, and never the one it was
	// last seen in: the change that introduced a bug shipped with its first
	// occurrence, and the latest release is just whatever is deployed now
	// (ADR 019).
	version := resolution.FirstRelease
	if version == "" {
		return SuspectReport{Warning: warnNoRelease}, nil
	}

	release, err := s.releases.Find(ctx, projectID, version)
	if err != nil {
		if errors.Is(err, domain.ErrReleaseNotFound) {
			return SuspectReport{Release: version, Warning: warnUnknownRelease}, nil
		}
		return SuspectReport{}, err
	}

	commits, err := s.releases.Commits(ctx, release.ID)
	if err != nil {
		return SuspectReport{}, err
	}
	report := SuspectReport{
		Release:     version,
		Commits:     commits,
		CommitCount: len(commits),
	}
	if len(commits) > maxListedCommits {
		report.Commits = commits[:maxListedCommits]
	}
	if len(commits) == 0 {
		report.Warning = warnNoCommits
		return report, nil
	}

	frames, symbolicated, err := s.framesOf(ctx, issueID, version)
	if err != nil {
		return SuspectReport{}, err
	}
	report.Symbolicated = symbolicated

	report.Suspects = domain.SuspectCommits(frames, commits, domain.MaxSuspects)
	if len(report.Suspects) > 0 {
		return report, nil
	}

	switch {
	case !anyCommitHasPaths(commits):
		report.Warning = warnNoPaths
	case symbolicated || len(frames) == 0:
		report.Warning = warnNoMatch
	default:
		// Frames, paths, and nothing in common. Saying so plainly would send
		// the reader to look for a bug in the scoring, when the likely
		// answer is that they are comparing `~/assets/index-a1b2.js` against
		// `src/checkout.ts` and no scoring could join those.
		report.Warning = warnMinified
	}
	return report, nil
}

// framesOf reads the stacktrace to attribute, in call order.
//
// It prefers an occurrence from the release under suspicion. The frames of an
// issue are the same frames by construction — that is what grouping means —
// but their *paths* are not: an event ingested before the source maps were
// uploaded carries the minified bundle's url, and one ingested afterwards
// carries `src/checkout.ts`. Taking whichever occurrence is newest would make
// the answer depend on which of those two happened to arrive last.
func (s *Suspects) framesOf(
	ctx context.Context, issueID int64, version string,
) ([]domain.SuspectFrame, bool, error) {
	events, err := s.issues.LatestEvents(ctx, issueID, suspectEventLookback)
	if err != nil {
		return nil, false, err
	}
	if len(events) == 0 {
		return nil, false, nil
	}

	chosen := &events[0]
	for index := range events {
		if events[index].Release == version {
			chosen = &events[index]
			break
		}
	}

	event, err := sentry.DecodeEvent(chosen.Payload)
	if err != nil {
		// A payload this build cannot read is a bug worth surfacing, not a
		// silently empty answer: everything else on the issue page decoded
		// the same bytes.
		return nil, false, fmt.Errorf("reading the stored occurrence of issue %d: %w", issueID, err)
	}
	stacktrace := event.BestStacktrace()
	if stacktrace == nil {
		return nil, false, nil
	}
	frames, symbolicated := suspectFrames(stacktrace.Frames)
	return frames, symbolicated, nil
}

// suspectFrames turns a protocol stacktrace into what the scorer takes, and
// reports whether any of it had been symbolicated.
//
// Frames arrive oldest first, so the call that raised is the last one. They
// are reversed here, once, because "depth" in ADR 019 means distance from the
// failure and everything downstream would otherwise have to remember that the
// protocol's order is the other way round.
//
// Only `in_app` frames, as ADR 019 says — with one exception it does not
// anticipate: several browser SDKs never set the flag at all, and applying the
// rule literally to an event where nothing is in_app would attribute nothing
// for the one platform this whole phase exists for. When no frame claims to be
// application code, all of them are candidates; the suffix match is strict
// enough that a framework path matches nothing in a repository anyway.
func suspectFrames(frames []sentry.Frame) ([]domain.SuspectFrame, bool) {
	inApp := false
	symbolicated := false
	for index := range frames {
		if frames[index].InApp {
			inApp = true
		}
		if frames[index].Raw != nil {
			symbolicated = true
		}
	}

	// The depth counts the frames that survive the filter, and not every
	// frame, because it is what the reason shows as "frame #1" — and the
	// panel's stacktrace hides library frames by default, so the first line a
	// reader sees is the first *application* frame. Counting the hidden ones
	// would make the accusation point at a line nobody is looking at.
	suspects := make([]domain.SuspectFrame, 0, len(frames))
	for index := len(frames) - 1; index >= 0; index-- {
		frame := frames[index]
		if inApp && !frame.InApp {
			continue
		}
		path := frame.Path()
		if path == "" {
			continue
		}
		suspects = append(suspects, domain.SuspectFrame{Path: path, Depth: len(suspects)})
	}
	if len(suspects) == 0 {
		return nil, symbolicated
	}
	return suspects, symbolicated
}

// anyCommitHasPaths reports whether the set carries a patch set at all.
func anyCommitHasPaths(commits []domain.Commit) bool {
	for index := range commits {
		if len(commits[index].Files) > 0 {
			return true
		}
	}
	return false
}
