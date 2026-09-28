package domain_test

import (
	"testing"

	"github.com/antoniojosev/trapline/internal/domain"
)

// commitWith builds a commit that touched the given paths, all modified.
func commitWith(sha string, ordinal int, paths ...string) domain.Commit {
	files := make([]domain.CommitFile, 0, len(paths))
	for _, path := range paths {
		files = append(files, domain.CommitFile{Path: path, ChangeType: domain.ChangeModified})
	}
	return domain.Commit{SHA: sha, Ordinal: ordinal, Files: files}
}

// framesFrom builds a stacktrace in call order: the first path is the failing
// call.
func framesFrom(paths ...string) []domain.SuspectFrame {
	frames := make([]domain.SuspectFrame, 0, len(paths))
	for depth, path := range paths {
		frames = append(frames, domain.SuspectFrame{Path: path, Depth: depth})
	}
	return frames
}

func TestSuspectCommitsNamesTheCommitThatTouchedTheFailingFrame(t *testing.T) {
	commits := []domain.Commit{
		commitWith("aaa", 0, "README.md"),
		commitWith("bbb", 1, "src/checkout.ts"),
		commitWith("ccc", 2, "src/cart.ts"),
	}
	frames := framesFrom("webpack:///./src/checkout.ts", "webpack:///./src/app.ts")

	suspects := domain.SuspectCommits(frames, commits, 0)
	if len(suspects) != 1 {
		t.Fatalf("got %d suspects, want exactly the one that touched the failing frame", len(suspects))
	}
	if suspects[0].Commit.SHA != "bbb" {
		t.Fatalf("suspect is %q, want bbb", suspects[0].Commit.SHA)
	}
	if len(suspects[0].Reasons) != 1 {
		t.Fatalf("got %d reasons, want one", len(suspects[0].Reasons))
	}
	reason := suspects[0].Reasons[0]
	if reason.Path != "src/checkout.ts" || reason.FrameDepth != 0 {
		t.Fatalf("reason is %+v, want src/checkout.ts at depth 0", reason)
	}
	if reason.FramePath != "webpack:///./src/checkout.ts" {
		t.Fatalf("the reason lost the frame's own spelling: %q", reason.FramePath)
	}
	if reason.Segments != 2 {
		t.Fatalf("shared %d segments, want 2", reason.Segments)
	}
}

func TestSuspectCommitsWeighsFramesNearTheFailingCallMore(t *testing.T) {
	// `deep` touched two frames far down the stack; `near` touched the one
	// that raised. The failing frame has to win, or the list ranks framework
	// plumbing above the change that broke.
	commits := []domain.Commit{
		commitWith("deep", 0, "src/router.ts", "src/boot.ts"),
		commitWith("near", 1, "src/checkout.ts"),
	}
	frames := framesFrom("src/checkout.ts", "src/router.ts", "src/boot.ts")

	suspects := domain.SuspectCommits(frames, commits, 0)
	if len(suspects) != 2 {
		t.Fatalf("got %d suspects, want both", len(suspects))
	}
	if suspects[0].Commit.SHA != "near" {
		t.Fatalf("ranked %q first, want the commit on the failing frame", suspects[0].Commit.SHA)
	}
	if !(suspects[0].Score > suspects[1].Score) {
		t.Fatalf("scores %v and %v do not separate them", suspects[0].Score, suspects[1].Score)
	}
}

func TestSuspectCommitsCountsEachFrameOnce(t *testing.T) {
	// A commit that touched a file, its test and its snapshot must not
	// outrank one that changed the failing line, purely by volume.
	wide := commitWith("wide", 0,
		"src/checkout.ts", "src/checkout.test.ts", "src/__snapshots__/checkout.ts")
	frames := framesFrom("src/checkout.ts")

	suspects := domain.SuspectCommits(frames, []domain.Commit{wide}, 0)
	if len(suspects) != 1 {
		t.Fatalf("got %d suspects, want one", len(suspects))
	}
	if suspects[0].Score != 1 {
		t.Fatalf("score is %v, want 1 — one frame matched, however many files did",
			suspects[0].Score)
	}
}

func TestSuspectCommitsBreaksTiesByRecencyThenBySHA(t *testing.T) {
	older := commitWith("zzz", 4, "src/checkout.ts")
	newer := commitWith("aaa", 1, "src/checkout.ts")
	frames := framesFrom("src/checkout.ts")

	suspects := domain.SuspectCommits(frames, []domain.Commit{older, newer}, 0)
	if len(suspects) != 2 || suspects[0].Commit.SHA != "aaa" {
		t.Fatalf("got %+v, want the more recent commit first", suspects)
	}

	// Same ordinal is not something a clean commit set produces, but a
	// hand-built payload can: the order still has to be the same on every
	// call, or the suspect changes when somebody reloads the page.
	tied := []domain.Commit{commitWith("bbb", 7, "src/checkout.ts"), commitWith("aaa", 7, "src/checkout.ts")}
	first := domain.SuspectCommits(frames, tied, 0)
	second := domain.SuspectCommits(frames, []domain.Commit{tied[1], tied[0]}, 0)
	if first[0].Commit.SHA != "aaa" || second[0].Commit.SHA != "aaa" {
		t.Fatalf("the tie is not resolved deterministically: %q then %q",
			first[0].Commit.SHA, second[0].Commit.SHA)
	}
}

func TestSuspectCommitsPrefersTheMoreSpecificMatchOnATie(t *testing.T) {
	// Both match the same frame at the same depth, so the scores are equal.
	// The one that agrees on the directory as well is the better answer:
	// `index.ts` exists in thirty directories of a large front end.
	vague := commitWith("vague", 0, "index.ts")
	exact := commitWith("exact", 1, "src/checkout/index.ts")
	frames := framesFrom("webpack:///./src/checkout/index.ts")

	suspects := domain.SuspectCommits(frames, []domain.Commit{vague, exact}, 0)
	if len(suspects) != 2 {
		t.Fatalf("got %d suspects, want both", len(suspects))
	}
	if suspects[0].Commit.SHA != "exact" {
		t.Fatalf("ranked %q first, want the deeper match", suspects[0].Commit.SHA)
	}
}

func TestSuspectCommitsIgnoresAPathThatOnlySharesAFileName(t *testing.T) {
	// `lib/util.ts` and `src/util.ts` are two files. Scoring them as a
	// partial match is how a suspect list fills with plausible nonsense.
	commits := []domain.Commit{commitWith("aaa", 0, "lib/util.ts")}
	frames := framesFrom("webpack:///./src/util.ts")

	if suspects := domain.SuspectCommits(frames, commits, 0); len(suspects) != 0 {
		t.Fatalf("got %+v, want nothing: the directories disagree", suspects)
	}
}

func TestSuspectCommitsMatchesTheSpellingsRealToolsProduce(t *testing.T) {
	cases := []struct {
		name  string
		frame string
		path  string
		match bool
	}{
		{"a bundler's webpack scheme", "webpack:///./src/checkout.ts", "src/checkout.ts", true},
		{"a webpack-internal frame", "webpack-internal:///./src/checkout.ts", "src/checkout.ts", true},
		{"an uploader's tilde url", "~/src/checkout.ts", "src/checkout.ts", true},
		{"a browser url with a cache buster", "https://app.example.com/src/checkout.ts?v=8f3a", "src/checkout.ts", true},
		{"a browser url with a fragment", "https://app.example.com/src/checkout.ts#x", "src/checkout.ts", true},
		{"a windows toolchain", `src\checkout.ts`, "src/checkout.ts", true},
		{"a monorepo path longer than the frame's", "src/checkout.ts", "apps/web/src/checkout.ts", true},
		{"a source relative to its map", "../src/checkout.ts", "src/checkout.ts", true},
		{"an absolute build path", "/home/ci/build/src/checkout.ts", "src/checkout.ts", true},
		{"a different extension", "src/checkout.tsx", "src/checkout.ts", false},
		{"a host with no path", "https://app.example.com", "index.ts", false},
		{"an empty frame path", "", "src/checkout.ts", false},
		{"a different directory", "src/pay/checkout.ts", "src/cart/checkout.ts", false},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			suspects := domain.SuspectCommits(
				framesFrom(test.frame),
				[]domain.Commit{commitWith("aaa", 0, test.path)},
				0,
			)
			if matched := len(suspects) == 1; matched != test.match {
				t.Fatalf("frame %q against %q matched=%v, want %v",
					test.frame, test.path, matched, test.match)
			}
		})
	}
}

func TestSuspectCommitsKeepsTheStrongestReasonsFirstAndBoundsThem(t *testing.T) {
	commit := commitWith("aaa", 0,
		"src/a.ts", "src/b.ts", "src/c.ts", "src/d.ts", "src/e.ts")
	frames := framesFrom("src/a.ts", "src/b.ts", "src/c.ts", "src/d.ts", "src/e.ts")

	suspects := domain.SuspectCommits(frames, []domain.Commit{commit}, 0)
	if len(suspects) != 1 {
		t.Fatalf("got %d suspects, want one", len(suspects))
	}
	reasons := suspects[0].Reasons
	if len(reasons) != domain.MaxSuspectReasons {
		t.Fatalf("kept %d reasons, want %d", len(reasons), domain.MaxSuspectReasons)
	}
	for index := range reasons {
		if reasons[index].FrameDepth != index {
			t.Fatalf("reason %d is at depth %d, want the frames nearest the failure first",
				index, reasons[index].FrameDepth)
		}
	}
}

func TestSuspectCommitsStopsAtTheFrameBudget(t *testing.T) {
	// A frame past the budget must not be able to name a suspect: the
	// bound is what keeps this bounded over a release of five thousand
	// commits.
	paths := make([]string, 0, domain.MaxScoredFrames+1)
	for index := 0; index <= domain.MaxScoredFrames; index++ {
		paths = append(paths, "src/frame.ts")
	}
	paths[domain.MaxScoredFrames] = "src/deep.ts"
	frames := framesFrom(paths...)

	suspects := domain.SuspectCommits(frames, []domain.Commit{commitWith("aaa", 0, "src/deep.ts")}, 0)
	if len(suspects) != 0 {
		t.Fatalf("got %+v, want nothing: the only match is past the frame budget", suspects)
	}
}

func TestSuspectCommitsBoundsTheList(t *testing.T) {
	commits := make([]domain.Commit, 0, 10)
	for index := 0; index < 10; index++ {
		commits = append(commits, commitWith(string(rune('a'+index)), index, "src/checkout.ts"))
	}
	frames := framesFrom("src/checkout.ts")

	if suspects := domain.SuspectCommits(frames, commits, 0); len(suspects) != domain.MaxSuspects {
		t.Fatalf("got %d suspects with no limit given, want the default of %d",
			len(suspects), domain.MaxSuspects)
	}
	if suspects := domain.SuspectCommits(frames, commits, 2); len(suspects) != 2 {
		t.Fatalf("got %d suspects, want the 2 that were asked for", len(suspects))
	}
}

func TestSuspectCommitsSaysNothingWithoutEvidence(t *testing.T) {
	commits := []domain.Commit{commitWith("aaa", 0, "src/checkout.ts")}
	frames := framesFrom("src/checkout.ts")

	if suspects := domain.SuspectCommits(nil, commits, 0); suspects != nil {
		t.Fatalf("got %+v from a stacktrace with no frames", suspects)
	}
	if suspects := domain.SuspectCommits(frames, nil, 0); suspects != nil {
		t.Fatalf("got %+v from a release with no commits", suspects)
	}
	// A commit set sent without a patch set is the ordinary case for
	// `sentry-cli set-commits` without `--local`, and it is not an error: it
	// is simply an attribution nobody can make (ADR 019).
	pathless := []domain.Commit{{SHA: "aaa", Message: "fix checkout"}}
	if suspects := domain.SuspectCommits(frames, pathless, 0); len(suspects) != 0 {
		t.Fatalf("got %+v from commits that carry no paths", suspects)
	}
}

func TestSuspectCommitsKeepsTheChangeType(t *testing.T) {
	commit := domain.Commit{SHA: "aaa", Files: []domain.CommitFile{
		{Path: "src/checkout.ts", ChangeType: domain.ChangeDeleted},
	}}

	suspects := domain.SuspectCommits(framesFrom("src/checkout.ts"), []domain.Commit{commit}, 0)
	if len(suspects) != 1 {
		t.Fatalf("got %d suspects, want one: a deleted file is still a suspect", len(suspects))
	}
	if suspects[0].Reasons[0].ChangeType != domain.ChangeDeleted {
		t.Fatalf("the reason says %q, want the change type the commit carried",
			suspects[0].Reasons[0].ChangeType)
	}
}

func TestSuspectCommitsTreatsAnImpossibleDepthAsTheTop(t *testing.T) {
	// A negative depth is not something the use case produces; the clamp is
	// here so that a future caller which computes depths some other way
	// cannot hand this function an infinite or negative weight and silently
	// invert the ranking.
	frames := []domain.SuspectFrame{{Path: "src/checkout.ts", Depth: -3}}

	suspects := domain.SuspectCommits(frames, []domain.Commit{commitWith("aaa", 0, "src/checkout.ts")}, 0)
	if len(suspects) != 1 {
		t.Fatalf("got %d suspects, want one", len(suspects))
	}
	if suspects[0].Score != 1 {
		t.Fatalf("score is %v, want the weight of the failing frame", suspects[0].Score)
	}
}
