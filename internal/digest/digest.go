// Package digest renders the weekly report.
//
// It is a pure package: the numbers arrive already gathered and it turns them
// into text. Nothing here reads a database, a clock or a configuration file,
// which is what makes the output testable against a fixture instead of
// against a running installation.
//
// That matters more here than it looks. A weekly mail is read by somebody who
// has stopped reading it — the way anyone reads a report that arrives every
// Monday — and the failure mode of such a thing is not an error, it is a
// slow drift into a shape nobody parses any more. The golden fixtures in
// testdata are the guard: a change to this output is a change somebody wrote
// down on purpose, or it is a failing test.
package digest

import (
	"cmp"
	_ "embed"
	"fmt"
	"slices"
	"strings"
	"text/template"
	"time"
)

// reportText is the digest's one template.
//
// A file rather than a string constant in this source, so that a change to
// what the report says shows up in a diff as a change to prose — which is
// what it is — instead of as a change to Go. It is embedded, so the binary
// still carries everything it needs (the same rule as the migrations and the
// panel).
//
//go:embed digest.tmpl
var reportText string

// Report is one digest: an installation's week, project by project.
//
// Plain fields and no methods that reach anywhere. Everything the template
// prints is either in here or derived from it by a function in this file, so
// "what can the digest say" is answerable by reading one struct.
type Report struct {
	// SentAt is the scheduled moment this report is for. It is not "now": two
	// renders of the same period must produce the same bytes, and a
	// generated-at line would be the one thing that made them differ.
	SentAt time.Time `json:"sent_at"`
	// Covers is the week being reported on, as hour buckets, inclusive.
	Covers Window `json:"covers"`
	// Previous is the week before it, which is what the trend compares to.
	Previous Window `json:"previous"`
	// Origin is the public base URL, used to build links. Empty means no
	// links are printed: a digest full of http://127.0.0.1 links is worse
	// than one with none, because the reader clicks them.
	Origin string `json:"origin,omitempty"`
	// Projects is every project with something to report. A project with no
	// events in either week is left out entirely rather than printed as a
	// row of zeroes — a digest is read for what changed.
	Projects []Project `json:"projects"`
}

// Window is a range of hourly buckets, as text, inclusive at both ends.
type Window struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Project is one project's week.
type Project struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Events is the whole week's event count, from the hourly buckets.
	Events int64 `json:"events"`
	// PreviousEvents is the week before's, for the trend.
	PreviousEvents int64 `json:"previous_events"`
	// NewIssues counts every issue first seen in the week; New lists the
	// loudest few. The two are separate because "47 new issues" and "here are
	// five of them" are both worth saying, and a list that silently stood for
	// a count would be the more misleading of the two.
	NewIssues int64   `json:"new_issues"`
	New       []Issue `json:"new"`
	// RegressedIssues counts everything that came back in the week;
	// Regressions lists the loudest few.
	RegressedIssues int64   `json:"regressed_issues"`
	Regressions     []Issue `json:"regressions"`
	// Top is the loudest issues of the week, new or not.
	Top []Issue `json:"top"`
}

// Issue is one line of a digest list.
type Issue struct {
	ID      int64  `json:"id"`
	Title   string `json:"title"`
	Culprit string `json:"culprit"`
	Level   string `json:"level"`
	// Count is how many events landed in the week being reported on, which
	// is not the issue's lifetime total.
	Count int64 `json:"count"`
	// URL is the panel link, filled in from the report's origin before
	// rendering. It is a field rather than a template expression so that the
	// decision "no origin means no link" is made once, in one place, instead
	// of in each of the three lists that print an issue.
	URL string `json:"url,omitempty"`
}

// ListLimit is how many issues each list in a project's section shows.
//
// Five, because that is the number the design asks for and because the point
// of a digest is to be read: a list of forty is the issue page with worse
// navigation, and the issue page is one link away from every line here.
const ListLimit = 5

// Render writes the report as text.
//
// Text and not HTML. The five channels this can be delivered through
// (ADR 015) are three chat APIs, a webhook and email, and the only format all
// five render is plain text; an HTML mail would also have to exist as text
// anyway, for the ones that cannot show it. When a channel wants richer
// output it can be built from the same Report, which is why the Report is
// exported and this is only one of the things that can consume it.
func Render(report Report) (string, error) {
	normalise(&report)

	var out strings.Builder
	if err := reportTemplate.Execute(&out, report); err != nil {
		return "", fmt.Errorf("rendering the digest: %w", err)
	}
	return out.String(), nil
}

// normalise puts the report into the one order it is ever printed in.
//
// Sorting here rather than trusting the caller is deliberate: the caller is a
// SQL query today and might be two of them merged tomorrow, and "the digest
// changed shape" is not a failure anybody would attribute to a missing ORDER
// BY. With the order decided here, the golden fixtures are the judge of it.
// It also copies every slice it touches. Render takes a Report by value, but
// a value carries the caller's backing arrays with it: sorting in place would
// silently reorder and re-link the very lists the caller passed in, which is
// the kind of surprise that costs an afternoon when the caller is a handler
// that also returns them as JSON.
func normalise(report *Report) {
	report.Projects = slices.Clone(report.Projects)
	slices.SortStableFunc(report.Projects, func(a, b Project) int {
		// By name, so a reader finds the same project in the same place every
		// week; by id when two share a name, so the order is total.
		if byName := strings.Compare(a.Name, b.Name); byName != 0 {
			return byName
		}
		return cmp.Compare(a.ID, b.ID)
	})
	for index := range report.Projects {
		project := &report.Projects[index]
		project.New = shortlist(report.Origin, project.ID, project.New)
		project.Regressions = shortlist(report.Origin, project.ID, project.Regressions)
		project.Top = shortlist(report.Origin, project.ID, project.Top)
	}
}

// shortlist is one printed list: a copy of the caller's issues, loudest
// first, cut to length, with the links filled in.
func shortlist(origin string, projectID int64, issues []Issue) []Issue {
	shortened := slices.Clone(issues)
	sortIssues(shortened)
	if len(shortened) > ListLimit {
		shortened = shortened[:ListLimit]
	}
	link(origin, projectID, shortened)
	return shortened
}

// sortIssues orders a list loudest first, then by id so ties do not move
// between two renders of the same data.
func sortIssues(issues []Issue) {
	slices.SortStableFunc(issues, func(a, b Issue) int {
		if byCount := cmp.Compare(b.Count, a.Count); byCount != 0 {
			return byCount
		}
		return cmp.Compare(a.ID, b.ID)
	})
}

// Trend is a week-over-week change, in the words a report uses.
//
// The wording is a function and not a template expression because the
// interesting cases are the degenerate ones: a week that starts from zero has
// no percentage, and a week that ends at zero is worth saying plainly. A
// template that tried to express this would express it slightly differently
// in each of the three places it appears.
func Trend(now, before int64) string {
	switch {
	case before == 0 && now == 0:
		return "none last week either"
	case before == 0:
		return "none last week"
	case now == 0:
		return fmt.Sprintf("down from %s last week", plural(before, "event", "events"))
	}

	change := float64(now-before) / float64(before) * 100
	// Rounded to a whole percent: a digest that says 33.7 % invites the
	// reader to believe the third digit, and the input is a count of errors
	// over seven days.
	rounded := int64(change + 0.5)
	if change < 0 {
		rounded = int64(change - 0.5)
	}
	switch {
	case rounded > 0:
		return fmt.Sprintf("up %d%% from %d last week", rounded, before)
	case rounded < 0:
		return fmt.Sprintf("down %d%% from %d last week", -rounded, before)
	default:
		return fmt.Sprintf("level with last week (%d)", before)
	}
}

// plural writes a count with the right noun.
func plural(n int64, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// link fills in each issue's panel URL.
//
// An empty origin leaves them empty, and the template prints no link at all.
// That is the deliberate choice: an installation that never set -origin would
// otherwise send a weekly mail full of http://127.0.0.1 links, which a reader
// clicks once and then distrusts the whole report (the same misconfiguration
// `doctor` already names for DSNs).
func link(origin string, projectID int64, issues []Issue) {
	if origin == "" {
		return
	}
	base := strings.TrimRight(origin, "/")
	for index := range issues {
		issues[index].URL = fmt.Sprintf("%s/projects/%d/issues/%d",
			base, projectID, issues[index].ID)
	}
}

// where is the one-line location of an issue: its culprit, or nothing at all.
//
// Nothing at all rather than "unknown": a digest is a list, and a column of
// the word "unknown" is noise in the place a reader's eye goes first.
func where(issue Issue) string {
	if issue.Culprit == "" {
		return ""
	}
	return "  ·  " + issue.Culprit
}

// day formats a bucket as the date a person would say.
func day(bucket string) string {
	parsed, err := time.Parse("2006-01-02T15", bucket)
	if err != nil {
		// A bucket that does not parse is a bug elsewhere, and a digest is
		// not the place to fail over it: printing what was actually stored is
		// more useful to whoever debugs it than a rendering error.
		return bucket
	}
	return parsed.Format("Mon 2 Jan")
}

// reportTemplate is parsed once, at start-up. A parse error here is a
// programming error in a file that ships inside the binary, so it panics
// rather than being returned from every render.
var reportTemplate = template.Must(template.New("digest").Funcs(template.FuncMap{
	"trend":  Trend,
	"where":  where,
	"day":    day,
	"plural": plural,
}).Parse(reportText))
