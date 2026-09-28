package ports

import (
	"context"

	"github.com/antoniojosev/trapline/internal/domain"
)

// DigestRepository answers the two questions a weekly report asks that the
// dashboard never does: what appeared this week, and what came back.
//
// Separate from StatsRepository even though both read the same buckets. The
// dashboard's questions are all "how much, when", asked about a range a human
// is looking at; these two are about a *transition* — an issue that did not
// exist and now does, an issue that was closed and is not — and they are
// answered by joining the buckets to columns on `issues` that record when the
// transition happened. Bolting them onto StatsRepository would grow the
// interface every caller of the dashboard has to satisfy for the benefit of
// one job that runs once a week.
//
// Neither method reads an event. The counts come from issue_hourly and the
// membership from indexed columns on issues, which is what lets a digest be
// written about a week whose payloads retention has already deleted (ADR 001,
// ADR 010).
type DigestRepository interface {
	// NewIssues returns the issues first seen inside the window, with how
	// many events each took *within that window*, loudest first, and the
	// total number of them — which is not len(issues) when there are more
	// than limit.
	NewIssues(ctx context.Context, projectID int64, window domain.Range, limit int) (IssueCounts, error)
	// RegressedIssues returns the issues that reopened inside the window, on
	// the same terms.
	RegressedIssues(ctx context.Context, projectID int64, window domain.Range, limit int) (IssueCounts, error)
}

// IssueCounts is a shortlist and the total it was cut from.
//
// The total is carried rather than left to the caller to compute, because the
// caller cannot: it asked for five rows and the honest answer to "how many
// new issues this week" is often forty-seven. A digest that prints the length
// of its own list as the count is the kind of wrong that nobody notices.
type IssueCounts struct {
	Issues []IssueCount
	Total  int64
}
