package sqlite

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// seedIssues records one event per fingerprint, spaced a minute apart, so the
// ordering under test is unambiguous.
func seedIssues(t *testing.T, repo *IssueRepository, projectID int64, count int) {
	t.Helper()
	ctx := context.Background()
	for i := range count {
		input := recordInput(projectID, "fp-"+strconv.Itoa(i), testNow.Add(time.Duration(i)*time.Minute))
		if _, err := repo.RecordEvent(ctx, input); err != nil {
			t.Fatalf("recording %d: %v", i, err)
		}
	}
}

func TestPagingWalksEveryIssueExactlyOnce(t *testing.T) {
	repo, projectID := newIssueRepo(t)
	ctx := context.Background()
	seedIssues(t, repo, projectID, 25)

	seen := map[int64]int{}
	cursor := ""
	pages := 0

	for {
		page, err := repo.List(ctx, ports.IssueFilter{ProjectID: projectID, Limit: 7, Cursor: cursor})
		if err != nil {
			t.Fatalf("listing: %v", err)
		}
		pages++
		if pages > 10 {
			t.Fatal("paging did not terminate")
		}
		for index := range page.Issues {
			seen[page.Issues[index].ID]++
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	if len(seen) != 25 {
		t.Errorf("walked %d issues, want 25", len(seen))
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("issue %d appeared %d times across pages", id, count)
		}
	}
}

func TestPagingIsStableWhileEventsArrive(t *testing.T) {
	// Keyset rather than an offset precisely for this: the list is ordered by
	// last-seen and events keep arriving, so an offset would skip rows and
	// repeat others between one page and the next — silently, and only under
	// load, which is when someone is actually paging through issues.
	repo, projectID := newIssueRepo(t)
	ctx := context.Background()
	seedIssues(t, repo, projectID, 12)

	first, err := repo.List(ctx, ports.IssueFilter{ProjectID: projectID, Limit: 5})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}

	// An issue that was already on page one fires again and jumps to the top.
	if _, err := repo.RecordEvent(ctx, recordInput(projectID, "fp-0", testNow.Add(time.Hour))); err != nil {
		t.Fatalf("recording: %v", err)
	}

	second, err := repo.List(ctx, ports.IssueFilter{ProjectID: projectID, Limit: 5, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("listing page two: %v", err)
	}

	onFirst := map[int64]bool{}
	for index := range first.Issues {
		onFirst[first.Issues[index].ID] = true
	}
	for index := range second.Issues {
		if onFirst[second.Issues[index].ID] {
			t.Errorf("issue %d appeared on both pages after it moved", second.Issues[index].ID)
		}
	}
}

func TestTheLastPageHasNoCursor(t *testing.T) {
	repo, projectID := newIssueRepo(t)
	ctx := context.Background()
	seedIssues(t, repo, projectID, 3)

	page, err := repo.List(ctx, ports.IssueFilter{ProjectID: projectID, Limit: 10})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if page.NextCursor != "" {
		t.Error("a page that fit everything still offered a next one")
	}

	// And an exactly-full page must not claim there is more.
	exact, err := repo.List(ctx, ports.IssueFilter{ProjectID: projectID, Limit: 3})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if exact.NextCursor != "" {
		t.Error("a page holding exactly everything offered a next one")
	}
}

func TestAMalformedCursorIsRejected(t *testing.T) {
	// Rather than being passed to SQLite as text that compares against
	// timestamps and quietly returns the wrong window.
	repo, projectID := newIssueRepo(t)
	ctx := context.Background()

	for name, cursor := range map[string]string{
		"not base64":    "!!!!",
		"no separator":  "aGVsbG8",
		"bad id":        "MjAyNi0wMS0wMVQwMDowMDowMFp8bm90LWEtbnVtYmVy",
		"bad timestamp": "aGFjZSB1biByYXRvfDE",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := repo.List(ctx, ports.IssueFilter{ProjectID: projectID, Cursor: cursor}); !errors.Is(err, domain.ErrInvalidIssue) {
				t.Errorf("error = %v, want ErrInvalidIssue", err)
			}
		})
	}
}

func TestFilteringByTag(t *testing.T) {
	repo, projectID := newIssueRepo(t)
	ctx := context.Background()

	production := recordInput(projectID, "prod", testNow)
	production.Tags = map[string]string{"environment": "production", "release": "v2.0.0"}
	staging := recordInput(projectID, "staging", testNow.Add(time.Minute))
	staging.Tags = map[string]string{"environment": "staging", "release": "v2.0.0"}

	for _, input := range []*ports.RecordEventInput{production, staging} {
		if _, err := repo.RecordEvent(ctx, input); err != nil {
			t.Fatalf("recording: %v", err)
		}
	}

	t.Run("one tag", func(t *testing.T) {
		// "What is broken in production" is the first question during an
		// incident, and it has to be answerable from the list.
		page, err := repo.List(ctx, ports.IssueFilter{
			ProjectID: projectID,
			Tags:      map[string]string{"environment": "production"},
		})
		if err != nil {
			t.Fatalf("listing: %v", err)
		}
		if len(page.Issues) != 1 || page.Issues[0].Fingerprint != "prod" {
			t.Errorf("issues = %+v", page.Issues)
		}
	})

	t.Run("several tags are ANDed", func(t *testing.T) {
		page, err := repo.List(ctx, ports.IssueFilter{
			ProjectID: projectID,
			Tags:      map[string]string{"environment": "staging", "release": "v2.0.0"},
		})
		if err != nil {
			t.Fatalf("listing: %v", err)
		}
		if len(page.Issues) != 1 || page.Issues[0].Fingerprint != "staging" {
			t.Errorf("issues = %+v", page.Issues)
		}
	})

	t.Run("a tag nothing carries matches nothing", func(t *testing.T) {
		page, err := repo.List(ctx, ports.IssueFilter{
			ProjectID: projectID,
			Tags:      map[string]string{"environment": "marte"},
		})
		if err != nil {
			t.Fatalf("listing: %v", err)
		}
		if len(page.Issues) != 0 {
			t.Errorf("issues = %+v", page.Issues)
		}
	})
}

func TestCountsCoverTheProjectNotTheFilter(t *testing.T) {
	// The numbers label the filter buttons. A count that changed depending on
	// which button was already pressed would tell nobody anything.
	repo, projectID := newIssueRepo(t)
	ctx := context.Background()
	seedIssues(t, repo, projectID, 4)

	all, err := repo.List(ctx, ports.IssueFilter{ProjectID: projectID})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if err := repo.SetStatus(ctx, projectID, all.Issues[0].ID, domain.StatusResolved); err != nil {
		t.Fatalf("resolving: %v", err)
	}

	filtered, err := repo.List(ctx, ports.IssueFilter{ProjectID: projectID, Status: domain.StatusResolved})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}

	if len(filtered.Issues) != 1 {
		t.Errorf("got %d resolved issues, want 1", len(filtered.Issues))
	}
	if filtered.Counts[domain.StatusUnresolved] != 3 {
		t.Errorf("unresolved count = %d while filtering by resolved, want 3", filtered.Counts[domain.StatusUnresolved])
	}
	if filtered.Counts[domain.StatusResolved] != 1 {
		t.Errorf("resolved count = %d, want 1", filtered.Counts[domain.StatusResolved])
	}
	// Every status is present even at zero: a button that vanishes when its
	// count is nought is a button that moves under the cursor.
	if _, present := filtered.Counts[domain.StatusIgnored]; !present {
		t.Error("a status with no issues was omitted from the counts")
	}
}

func TestAnOversizedLimitIsCapped(t *testing.T) {
	repo, projectID := newIssueRepo(t)
	ctx := context.Background()
	seedIssues(t, repo, projectID, 3)

	page, err := repo.List(ctx, ports.IssueFilter{ProjectID: projectID, Limit: 100000})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(page.Issues) != 3 {
		t.Errorf("got %d issues", len(page.Issues))
	}
}
