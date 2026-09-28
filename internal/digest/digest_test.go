package digest_test

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/digest"
)

// update rewrites the fixtures instead of comparing against them.
//
// It exists so that a deliberate change to the report is one command, and an
// accidental one is a failing test with a diff. Without it the temptation on
// a red test is to paste the new output over the old, which is the same thing
// with none of the deliberation.
var update = flag.Bool("update", false, "rewrite the golden files from the current output")

// TestRenderMatchesGoldenFiles is the guard on the shape of a weekly mail.
//
// Every case is a week somebody could actually have: a busy one, a quiet one,
// an installation with nothing in it, a first week with no history to compare
// to, and one with no public origin configured so no links can be printed.
func TestRenderMatchesGoldenFiles(t *testing.T) {
	for name, report := range map[string]digest.Report{
		"busy":       busyWeek(),
		"quiet":      quietWeek(),
		"empty":      emptyInstallation(),
		"first-week": firstWeek(),
		"no-origin":  withoutOrigin(),
		"overflow":   moreThanFits(),
	} {
		t.Run(name, func(t *testing.T) {
			rendered, err := digest.Render(report)
			if err != nil {
				t.Fatalf("rendering %s: %v", name, err)
			}
			compareGolden(t, name, rendered)
		})
	}
}

// TestRenderIsDeterministic is the property the fixtures cannot prove on their
// own: the same report rendered twice is the same bytes, whatever order the
// caller happened to assemble it in.
func TestRenderIsDeterministic(t *testing.T) {
	report := busyWeek()

	first, err := digest.Render(report)
	if err != nil {
		t.Fatalf("first render: %v", err)
	}

	// Reversed: a caller whose query returned rows the other way round must
	// not produce a different mail.
	shuffled := busyWeek()
	slicesReverse(shuffled.Projects)
	for index := range shuffled.Projects {
		slicesReverse(shuffled.Projects[index].New)
		slicesReverse(shuffled.Projects[index].Top)
	}

	second, err := digest.Render(shuffled)
	if err != nil {
		t.Fatalf("second render: %v", err)
	}
	if first != second {
		t.Errorf("the same week rendered two different ways:\n--- first ---\n%s\n--- second ---\n%s",
			first, second)
	}
}

// TestRenderDoesNotMutateItsArgument matters because Render sorts and fills in
// links. A caller that rendered a report and then read its own slice back
// would otherwise find it reordered and truncated under it.
func TestRenderDoesNotMutateItsArgument(t *testing.T) {
	report := moreThanFits()
	// Reversed first, so the copy the caller holds is in an order the render
	// definitely does not use. If Render sorted in place, this would come
	// back sorted.
	slicesReverse(report.Projects[0].Top)
	before := append([]digest.Issue(nil), report.Projects[0].Top...)

	if _, err := digest.Render(report); err != nil {
		t.Fatalf("rendering: %v", err)
	}

	after := report.Projects[0].Top
	if len(after) != len(before) {
		t.Fatalf("Render truncated the caller's slice: %d issues before, %d after", len(before), len(after))
	}
	for index := range before {
		if after[index].ID != before[index].ID || after[index].URL != before[index].URL {
			t.Fatalf("Render rewrote the caller's slice at %d: %+v became %+v",
				index, before[index], after[index])
		}
	}
}

func TestTrend(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		now    int64
		before int64
		want   string
	}{
		{"growth", 134, 100, "up 34% from 100 last week"},
		{"decline", 66, 100, "down 34% from 100 last week"},
		{"unchanged", 100, 100, "level with last week (100)"},
		{"a fraction of a percent is not news", 1004, 1000, "level with last week (1000)"},
		{"from nothing", 12, 0, "none last week"},
		{"to nothing", 0, 40, "down from 40 events last week"},
		{"to nothing, from one", 0, 1, "down from 1 event last week"},
		{"nothing at all", 0, 0, "none last week either"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := digest.Trend(testCase.now, testCase.before); got != testCase.want {
				t.Errorf("Trend(%d, %d) = %q, want %q",
					testCase.now, testCase.before, got, testCase.want)
			}
		})
	}
}

// compareGolden checks the render against testdata, or rewrites it.
func compareGolden(t *testing.T, name, got string) {
	t.Helper()

	path := filepath.Join("testdata", "digest", name+".golden")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("creating the fixture directory: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
		t.Logf("wrote %s", path)
		return
	}

	want, err := os.ReadFile(path) //nolint:gosec // a fixture path built from a test name.
	if err != nil {
		t.Fatalf("reading %s: %v\nRun `go test ./internal/digest -update` if this fixture is new.", path, err)
	}
	if got != string(want) {
		t.Errorf("the digest no longer renders the way %s says it does.\n"+
			"If the change is deliberate, run `go test ./internal/digest -update` and read the diff.\n"+
			"--- want ---\n%s\n--- got ---\n%s", path, want, got)
	}
}

func slicesReverse[T any](items []T) {
	for left, right := 0, len(items)-1; left < right; left, right = left+1, right-1 {
		items[left], items[right] = items[right], items[left]
	}
}

// The weeks. Written out as literals rather than generated, so a fixture and
// the data behind it can be read side by side.

const origin = "https://errors.example.test"

func week() (covers, previous digest.Window) {
	return digest.Window{From: "2026-08-24T09", To: "2026-08-31T08"},
		digest.Window{From: "2026-08-17T09", To: "2026-08-24T08"}
}

func busyWeek() digest.Report {
	covers, previous := week()
	return digest.Report{
		SentAt:   time.Date(2026, time.August, 31, 9, 0, 0, 0, time.UTC),
		Covers:   covers,
		Previous: previous,
		Origin:   origin,
		Projects: []digest.Project{
			{
				ID: 1, Name: "venekambio",
				Events: 2480, PreviousEvents: 1850,
				NewIssues: 3,
				New: []digest.Issue{
					{ID: 41, Title: "TypeError: rate is not a function", Culprit: "app/rates.js in refresh", Level: "error", Count: 612},
					{ID: 44, Title: "ValueError: invalid literal for int()", Culprit: "app/views.py in checkout", Level: "error", Count: 87},
					{ID: 47, Title: "context deadline exceeded", Culprit: "bcv.Client.Fetch", Level: "warning", Count: 12},
				},
				RegressedIssues: 1,
				Regressions: []digest.Issue{
					{ID: 12, Title: "IntegrityError: duplicate key", Culprit: "app/orders.py in create", Level: "error", Count: 143},
				},
				Top: []digest.Issue{
					{ID: 41, Title: "TypeError: rate is not a function", Culprit: "app/rates.js in refresh", Level: "error", Count: 612},
					{ID: 9, Title: "connection reset by peer", Culprit: "", Level: "error", Count: 402},
					{ID: 12, Title: "IntegrityError: duplicate key", Culprit: "app/orders.py in create", Level: "error", Count: 143},
				},
			},
			{
				ID: 2, Name: "despacha",
				Events: 96, PreviousEvents: 240,
				NewIssues: 1,
				New: []digest.Issue{
					{ID: 88, Title: "panic: runtime error: index out of range [3]", Culprit: "route.Plan", Level: "fatal", Count: 96},
				},
				Top: []digest.Issue{
					{ID: 88, Title: "panic: runtime error: index out of range [3]", Culprit: "route.Plan", Level: "fatal", Count: 96},
				},
			},
		},
	}
}

func quietWeek() digest.Report {
	covers, previous := week()
	return digest.Report{
		SentAt:   time.Date(2026, time.August, 31, 9, 0, 0, 0, time.UTC),
		Covers:   covers,
		Previous: previous,
		Origin:   origin,
		Projects: []digest.Project{
			{ID: 1, Name: "venekambio", Events: 0, PreviousEvents: 312},
		},
	}
}

func emptyInstallation() digest.Report {
	covers, previous := week()
	return digest.Report{
		SentAt:   time.Date(2026, time.August, 31, 9, 0, 0, 0, time.UTC),
		Covers:   covers,
		Previous: previous,
		Origin:   origin,
	}
}

func firstWeek() digest.Report {
	covers, previous := week()
	return digest.Report{
		SentAt:   time.Date(2026, time.August, 31, 9, 0, 0, 0, time.UTC),
		Covers:   covers,
		Previous: previous,
		Origin:   origin,
		Projects: []digest.Project{
			{
				ID: 1, Name: "venekambio",
				Events: 18, PreviousEvents: 0,
				NewIssues: 2,
				New: []digest.Issue{
					{ID: 1, Title: "sqlite: database is locked", Culprit: "store.Write", Level: "error", Count: 12},
					{ID: 2, Title: "EOF", Culprit: "", Level: "error", Count: 6},
				},
				Top: []digest.Issue{
					{ID: 1, Title: "sqlite: database is locked", Culprit: "store.Write", Level: "error", Count: 12},
					{ID: 2, Title: "EOF", Culprit: "", Level: "error", Count: 6},
				},
			},
		},
	}
}

func withoutOrigin() digest.Report {
	report := firstWeek()
	report.Origin = ""
	return report
}

// moreThanFits is the week where every list is longer than the digest shows.
// The fixture is what proves the cut happens and that it takes the loudest.
func moreThanFits() digest.Report {
	covers, previous := week()

	var top []digest.Issue
	for index := 1; index <= 9; index++ {
		top = append(top, digest.Issue{
			ID:      int64(index),
			Title:   "error number " + string(rune('a'+index-1)),
			Culprit: "pkg.Function",
			Level:   "error",
			Count:   int64(index * 10),
		})
	}

	return digest.Report{
		SentAt:   time.Date(2026, time.August, 31, 9, 0, 0, 0, time.UTC),
		Covers:   covers,
		Previous: previous,
		Origin:   origin,
		Projects: []digest.Project{
			{
				ID: 1, Name: "venekambio",
				Events: 450, PreviousEvents: 450,
				NewIssues: 9,
				New:       top,
				Top:       top,
			},
		},
	}
}
