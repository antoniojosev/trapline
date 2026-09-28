package domain

import (
	"sort"
	"strings"
)

// MaxSuspects is how many commits a suspect list names.
//
// Three, because the list is read as an accusation and its value collapses
// with its length: a page that names three commits is a lead, and one that
// names fifteen is the release page again with extra confidence. Three is also
// what fits beside a stacktrace without pushing it off the screen (ADR 019).
const MaxSuspects = 3

// MaxScoredFrames is how deep into a stacktrace the scorer looks.
//
// A stacktrace can be hundreds of frames of framework plumbing, and the frames
// past the first twenty-five have a weight under 4 % of the top one's — small
// enough that including them changes an order only by accident. Bounding it
// also bounds the work: this runs per request over every commit in a release,
// and a release may carry five thousand of them (MaxCommitsPerRelease).
const MaxScoredFrames = 25

// MaxSuspectReasons is how many matches are kept as the explanation of one
// suspect.
//
// The reason is what makes the accusation legible — "it touched
// src/checkout.ts, which is frame #1" — and a reader checks one or two. A
// commit that touched forty files in the stacktrace would otherwise return
// forty lines of justification, which is the same as returning none.
const MaxSuspectReasons = 3

// SuspectFrame is one stack frame, reduced to what attribution needs.
//
// A type of its own rather than the protocol's frame, because the scorer is
// domain code and the protocol is not: the only facts it uses are which file
// the frame names and how close to the failing call it is (ADR 019).
type SuspectFrame struct {
	// Path is the file the frame names, after symbolication. A minified
	// bundle's url is a legitimate value here and simply matches nothing,
	// which is the correct answer for a build whose maps were never uploaded.
	Path string
	// Depth is the distance from the call that actually raised: 0 is the
	// failing frame, 1 is its caller, and so on.
	Depth int
}

// SuspectReason is one match between a commit and a stacktrace: the evidence
// a suspect is shown with.
//
// It carries both paths rather than one, because they are usually spelled
// differently — a repository says `src/checkout.ts` and a symbolicated frame
// says `webpack:///./src/checkout.ts` — and a reader who is being told a
// commit is to blame should be able to see exactly what was compared.
type SuspectReason struct {
	// Path is the file the commit touched, as the repository spells it.
	Path       string
	ChangeType ChangeType
	// FramePath is the file the frame named, as the event spells it.
	FramePath string
	// FrameDepth is that frame's distance from the failing call.
	FrameDepth int
	// Segments is how many trailing path segments the two shared. It is the
	// strength of the match: one means the file names agree and nothing else,
	// and three means three directory levels agree as well.
	Segments int
}

// Suspect is one commit that touched a file in the stacktrace.
type Suspect struct {
	Commit Commit
	// Score is the sum of the matched frames' weights. It is comparable
	// within one call and meaningless between two: it is an ordering, not a
	// probability, and dressing it up as a percentage would be inventing a
	// confidence nothing here measures.
	Score float64
	// Reasons is why, strongest match first.
	Reasons []SuspectReason
}

// SuspectCommits ranks the commits that touched a file this stacktrace names.
//
// The whole of ADR 019's scoring, and it is deliberately this small: the
// intersection between the files a release changed and the files a crash
// walked through is the entire signal, and everything more elaborate — commit
// size, authorship, churn — is a guess dressed as evidence. A frame closer to
// the failing call weighs more, because the call that raised is more likely to
// be the one that broke than the framework that called it; a more recent
// commit breaks a tie, because it is the later of two equally plausible
// changes.
//
// Frames are given in call order, failing call first. Commits are given in the
// order they were sent, which every tool that sends them means as newest
// first (CleanCommits assigns the ordinal). Nothing here reads a clock.
func SuspectCommits(frames []SuspectFrame, commits []Commit, limit int) []Suspect {
	if limit <= 0 {
		limit = MaxSuspects
	}
	if len(frames) == 0 || len(commits) == 0 {
		return nil
	}
	if len(frames) > MaxScoredFrames {
		frames = frames[:MaxScoredFrames]
	}

	// The frames are split into segments once, not once per commit: a release
	// of five thousand commits would otherwise re-parse the same twenty-five
	// paths five thousand times.
	framePaths := make([][]string, len(frames))
	for index, frame := range frames {
		framePaths[index] = pathSegments(frame.Path)
	}

	suspects := make([]Suspect, 0, len(commits))
	for _, commit := range commits {
		if suspect, found := scoreCommit(commit, frames, framePaths); found {
			suspects = append(suspects, suspect)
		}
	}

	sort.SliceStable(suspects, func(a, b int) bool {
		return moreSuspect(suspects[a], suspects[b])
	})
	if len(suspects) > limit {
		suspects = suspects[:limit]
	}
	return suspects
}

// scoreCommit weighs one commit against the stacktrace.
//
// Each frame contributes at most once, however many of the commit's files
// match it. Without that, a commit that touched a file and its test and its
// snapshot would outrank the commit that actually changed the line, purely by
// having touched more things near it.
func scoreCommit(commit Commit, frames []SuspectFrame, framePaths [][]string) (Suspect, bool) {
	if len(commit.Files) == 0 {
		return Suspect{}, false
	}

	fileSegments := make([][]string, len(commit.Files))
	for index, file := range commit.Files {
		fileSegments[index] = pathSegments(file.Path)
	}

	suspect := Suspect{Commit: commit}
	for frameIndex, frame := range frames {
		best := -1
		bestSegments := 0
		for fileIndex := range commit.Files {
			segments := sharedSuffix(framePaths[frameIndex], fileSegments[fileIndex])
			if segments > bestSegments {
				best, bestSegments = fileIndex, segments
			}
		}
		if best < 0 {
			continue
		}
		suspect.Score += frameWeight(frame.Depth)
		suspect.Reasons = append(suspect.Reasons, SuspectReason{
			Path:       commit.Files[best].Path,
			ChangeType: commit.Files[best].ChangeType,
			FramePath:  frame.Path,
			FrameDepth: frame.Depth,
			Segments:   bestSegments,
		})
	}
	if len(suspect.Reasons) == 0 {
		return Suspect{}, false
	}

	// Strongest evidence first, so the one line a reader actually reads is
	// the most specific match rather than whichever frame came first.
	sort.SliceStable(suspect.Reasons, func(a, b int) bool {
		left, right := suspect.Reasons[a], suspect.Reasons[b]
		if left.FrameDepth != right.FrameDepth {
			return left.FrameDepth < right.FrameDepth
		}
		return left.Segments > right.Segments
	})
	if len(suspect.Reasons) > MaxSuspectReasons {
		suspect.Reasons = suspect.Reasons[:MaxSuspectReasons]
	}
	return suspect, true
}

// frameWeight is how much a frame at a given depth counts.
//
// The reciprocal of its position, which decays fast enough that the failing
// call outweighs any three frames below it and slowly enough that a commit
// matching four mid-stack frames still beats one matching a single deep one.
func frameWeight(depth int) float64 {
	if depth < 0 {
		depth = 0
	}
	return 1 / float64(depth+1)
}

// moreSuspect is the order the list is presented in.
func moreSuspect(a, b Suspect) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	// A match three directories deep is worth more than one that agrees only
	// on a file name, and `index.ts` is why: in a large front end it exists
	// in thirty directories and matches all of them.
	if left, right := bestSegments(a), bestSegments(b); left != right {
		return left > right
	}
	// The more recent of two equally plausible changes. Ordinal 0 is the
	// newest, which is the order every tool sends a commit set in.
	if a.Commit.Ordinal != b.Commit.Ordinal {
		return a.Commit.Ordinal < b.Commit.Ordinal
	}
	// Never the order of a map or of a query without ORDER BY: two runs of
	// this must produce the same list, or the "suspect" changes on reload.
	return a.Commit.SHA < b.Commit.SHA
}

// bestSegments is the strength of a suspect's strongest match.
func bestSegments(suspect Suspect) int {
	best := 0
	for _, reason := range suspect.Reasons {
		if reason.Segments > best {
			best = reason.Segments
		}
	}
	return best
}

// sharedSuffix is how many trailing path segments two paths agree on, and
// zero unless one path is a suffix of the other.
//
// The "suffix of" requirement is what separates a match from a coincidence.
// A repository says `src/checkout.ts` and a symbolicated frame says
// `webpack:///./src/checkout.ts`; one is a suffix of the other and they are
// the same file. A repository says `src/util.ts` and a frame says
// `lib/util.ts`; they share a file name and are not the same file, and
// scoring that as a partial match is how a suspect list fills with plausible
// nonsense (ADR 019).
func sharedSuffix(a, b []string) int {
	shorter := len(a)
	if len(b) < shorter {
		shorter = len(b)
	}
	if shorter == 0 {
		return 0
	}
	for offset := 1; offset <= shorter; offset++ {
		if a[len(a)-offset] != b[len(b)-offset] {
			return 0
		}
	}
	return shorter
}

// pathSegments reduces an address to the path segments worth comparing.
//
// The two sides spell the same file very differently. A repository sends
// `src/checkout.ts`. A bundler writes `webpack:///./src/checkout.ts` into a
// source map, an uploader writes `~/static/app.js`, a browser reports
// `https://app.example.com/static/app.js?v=8f3a`, and a Windows toolchain
// writes backslashes. Reducing all of them here — a pure function, in the
// domain — is what keeps the scorer comparing files rather than spellings.
func pathSegments(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	raw = strings.ReplaceAll(raw, "\\", "/")

	// A cache-busting query is not part of a file's identity, and neither is
	// a fragment. Cut before anything else, or a `?v=8f3a` becomes part of
	// the last segment and nothing ever matches it.
	if cut := strings.IndexAny(raw, "?#"); cut >= 0 {
		raw = raw[:cut]
	}
	// Any scheme, not just http: `webpack://`, `webpack-internal://`,
	// `file://` and `app://` all reach here, and what follows the authority
	// is the path in every one of them.
	if index := strings.Index(raw, "://"); index >= 0 {
		rest := raw[index+3:]
		if slash := strings.IndexByte(rest, '/'); slash >= 0 {
			raw = rest[slash:]
		} else {
			// `https://host` names no file at all.
			return nil
		}
	}
	raw = strings.TrimPrefix(raw, "~/")

	parts := strings.Split(raw, "/")
	segments := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part {
		case "", ".":
			// `webpack:///./src/x.ts` is `src/x.ts`.
			continue
		case "..":
			// A map's source is relative to the map, so `../src/x.ts` walks
			// out of the output directory. There is nothing here to walk out
			// of — the base is unknown — so the step is dropped rather than
			// guessed at, which keeps the remaining segments comparable.
			if len(segments) > 0 {
				segments = segments[:len(segments)-1]
			}
			continue
		}
		segments = append(segments, part)
	}
	if len(segments) == 0 {
		return nil
	}
	return segments
}
