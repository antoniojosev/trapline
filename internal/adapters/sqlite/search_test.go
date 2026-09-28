package sqlite

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

func TestFTSMatchQuotesEveryTermAsAPhrase(t *testing.T) {
	cases := map[string]string{
		"valueerror":    `"valueerror"`,
		"value error":   `"value" "error"`,
		"  spaced   ":   `"spaced"`,
		`say "hi"`:      `"say" """hi"""`,
		"NOT NULL":      `"NOT" "NULL"`,
		"views.py:42":   `"views.py:42"`,
		"función":       `"función"`,
		"a NOT b":       `"NOT"`,
		"drop* table*":  `"drop*" "table*"`,
		"trailing -- x": `"trailing"`,
		"net/http":      `"net/http"`,
		"SQLSTATE[23]":  `"SQLSTATE[23]"`,
	}
	for query, want := range cases {
		got, err := ftsMatch(query)
		if err != nil {
			t.Errorf("ftsMatch(%q): %v", query, err)
			continue
		}
		if got != want {
			t.Errorf("ftsMatch(%q) = %q, want %q", query, got, want)
		}
	}
}

func TestFTSMatchRefusesAQueryTheIndexCannotLookUp(t *testing.T) {
	// One or two characters have no three-character window to look up, so the
	// index cannot serve them at all. Returning an empty page would be
	// indistinguishable from "nothing matched"; the error says which term is
	// the problem and what the minimum is.
	for _, query := range []string{"ab", "a", "-", "x y", "  q  "} {
		got, err := ftsMatch(query)
		if !errors.Is(err, domain.ErrInvalidSearch) {
			t.Errorf("ftsMatch(%q) = %q, %v; want ErrInvalidSearch", query, got, err)
			continue
		}
		if !strings.Contains(err.Error(), strconv.Itoa(MinSearchTermRunes)) {
			t.Errorf("ftsMatch(%q) error = %q, want it to name the minimum", query, err)
		}
	}

	// A term that is short in bytes but long enough in characters is fine, and
	// one that is long in bytes but short in characters is not: the window the
	// index builds is counted in characters.
	if _, err := ftsMatch("añó"); err != nil {
		t.Errorf("a three-character accented term was refused: %v", err)
	}
	if _, err := ftsMatch("añ"); !errors.Is(err, domain.ErrInvalidSearch) {
		t.Errorf("a two-character term measured in bytes was accepted: %v", err)
	}
}

func TestFTSMatchDropsShortTermsAlongsideUsableOnes(t *testing.T) {
	// "de" and "la" are two letters and carry no information the index could
	// use anyway. Dropping them narrows less than the user asked for, which is
	// a superset — never a wrong row silently excluded.
	got, err := ftsMatch("la función de pago")
	if err != nil {
		t.Fatalf("ftsMatch: %v", err)
	}
	if want := `"función" "pago"`; got != want {
		t.Errorf("ftsMatch = %q, want %q", got, want)
	}
}

// searchTitles runs a listing query and returns the titles it found, which is
// the shape every assertion below wants.
func searchTitles(t *testing.T, repo *IssueRepository, projectID int64, query string) []string {
	t.Helper()
	page, err := repo.List(context.Background(), ports.IssueFilter{ProjectID: projectID, Query: query})
	if err != nil {
		t.Fatalf("searching for %q: %v", query, err)
	}
	titles := make([]string, 0, len(page.Issues))
	for index := range page.Issues {
		titles = append(titles, page.Issues[index].Title)
	}
	// The counts label the filter buttons and describe the project, not the
	// query, so they are present even when nothing matched.
	if page.Counts == nil {
		t.Errorf("a search for %q came back with no status counts", query)
	}
	return titles
}

func seedForSearch(t *testing.T) (repo *IssueRepository, projectID int64) {
	t.Helper()
	repo, projectID = newIssueRepo(t)

	record(t, repo, projectID, "value", at(10), nil) // ValueError: invalid amount
	record(t, repo, projectID, "conn", at(10), func(input *ports.RecordEventInput) {
		input.Observation.Title = "ConnectionError: database is unreachable"
		input.Observation.Culprit = "myapp.db in connect"
		input.Message = "no se pudo abrir la conexión"
	})
	record(t, repo, projectID, "func", at(10), func(input *ports.RecordEventInput) {
		input.Observation.Title = "TypeError: undefined is not a function"
		input.Observation.Culprit = "web/checkout.js in submit"
		input.Message = "la función de pago falló"
	})
	return repo, projectID
}

func TestSearchMatchesAPrefix(t *testing.T) {
	repo, projectID := seedForSearch(t)

	// Typing "Val" has to find ValueError before the word is finished, or the
	// search box is something you use after you already know the answer.
	titles := searchTitles(t, repo, projectID, "Val")
	if len(titles) != 1 || titles[0] != "ValueError: invalid amount" {
		t.Errorf("search for a prefix = %v, want the ValueError issue", titles)
	}
}

func TestSearchMatchesTheMiddleOfAWord(t *testing.T) {
	repo, projectID := seedForSearch(t)

	// The reason the index is trigram and not unicode61. Exception names are
	// compound words and the part somebody remembers is usually in the middle:
	// nobody types ConnectionError from the start, they type Connection, or
	// Error, or Timeout.
	titles := searchTitles(t, repo, projectID, "Error")
	if len(titles) != 3 {
		t.Errorf("infix search = %v, want all three *Error issues", titles)
	}
	if got := searchTitles(t, repo, projectID, "nection"); len(got) != 1 {
		t.Errorf("search for the middle of ConnectionError = %v, want one match", got)
	}
	// And the other direction, which is what stops this from being a search
	// that matches everything: a substring nothing contains finds nothing.
	if got := searchTitles(t, repo, projectID, "Kafka"); len(got) != 0 {
		t.Errorf("search for a substring nothing has = %v, want nothing", got)
	}
}

func TestSearchFindsCompoundExceptionNamesByTheirParts(t *testing.T) {
	repo, projectID := newIssueRepo(t)
	record(t, repo, projectID, "compound", at(10), func(input *ports.RecordEventInput) {
		input.Observation.Title = "ECONNREFUSED: SQLSTATE[23000] duplicate key"
		input.Observation.Culprit = "net/http in dial"
		input.Message = ""
	})

	// Every one of these is a fragment of a single token. Under unicode61 not
	// one of them matched.
	for _, query := range []string{"CONNREFUSED", "SQLSTATE", "23000", "net/http", "duplicate"} {
		if got := searchTitles(t, repo, projectID, query); len(got) != 1 {
			t.Errorf("search for %q = %v, want the issue", query, got)
		}
	}
}

func TestSearchRefusesATermTheIndexCannotLookUp(t *testing.T) {
	repo, projectID := seedForSearch(t)

	_, err := repo.List(context.Background(), ports.IssueFilter{ProjectID: projectID, Query: "ab"})
	if !errors.Is(err, domain.ErrInvalidSearch) {
		t.Errorf("error = %v, want ErrInvalidSearch rather than an empty page", err)
	}
	// A short term next to a usable one is dropped, not refused: there is
	// still something for the index to do.
	if got := searchTitles(t, repo, projectID, "ab ValueError"); len(got) != 1 {
		t.Errorf("search = %v, want the ValueError issue", got)
	}
}

func TestSearchIsCaseInsensitive(t *testing.T) {
	repo, projectID := seedForSearch(t)
	if got := searchTitles(t, repo, projectID, "CONNECTIONERROR"); len(got) != 1 {
		t.Errorf("search = %v, want one match regardless of case", got)
	}
}

func TestTwoTermsAreAndedNotOred(t *testing.T) {
	repo, projectID := seedForSearch(t)

	// Both words appear across the corpus but only one issue has both. An
	// implicit OR would return everything, which is the failure that makes a
	// search box feel broken without ever erroring.
	titles := searchTitles(t, repo, projectID, "invalid amount")
	if len(titles) != 1 || titles[0] != "ValueError: invalid amount" {
		t.Errorf("two terms = %v, want only the issue carrying both", titles)
	}

	if got := searchTitles(t, repo, projectID, "invalid unreachable"); len(got) != 0 {
		t.Errorf("two terms in different issues = %v, want nothing", got)
	}
}

func TestSearchIgnoresAccents(t *testing.T) {
	repo, projectID := seedForSearch(t)

	// Both spellings have to find it. Somebody typing on a phone keyboard
	// without accents and somebody typing on a Spanish layout are looking for
	// the same issue.
	for _, query := range []string{"función", "funcion", "conexion", "conexión"} {
		if got := searchTitles(t, repo, projectID, query); len(got) != 1 {
			t.Errorf("search for %q = %v, want one match", query, got)
		}
	}
}

func TestSearchCoversTheCulpritAndTheLastMessage(t *testing.T) {
	repo, projectID := seedForSearch(t)

	// The culprit is where the filename lives, and searching for a filename is
	// what people actually do.
	if got := searchTitles(t, repo, projectID, "checkout.js"); len(got) != 1 {
		t.Errorf("search by filename = %v, want the TypeError issue", got)
	}
	// The message is the third indexed column, and the reason an issue titled
	// only with an exception type is findable at all.
	if got := searchTitles(t, repo, projectID, "pago"); len(got) != 1 {
		t.Errorf("search by message = %v, want the TypeError issue", got)
	}
}

func TestTheIndexFollowsTheLatestMessage(t *testing.T) {
	repo, projectID := newIssueRepo(t)
	record(t, repo, projectID, "drift", at(10), func(input *ports.RecordEventInput) {
		input.Message = "primero"
	})
	record(t, repo, projectID, "drift", at(11), func(input *ports.RecordEventInput) {
		input.Message = "segundo"
	})

	if got := searchTitles(t, repo, projectID, "segundo"); len(got) != 1 {
		t.Errorf("search for the newest message = %v, want the issue", got)
	}
	// And the previous one stops matching, or the index accumulates every
	// message an issue ever had and the search returns issues on the strength
	// of text nobody can see any more.
	if got := searchTitles(t, repo, projectID, "primero"); len(got) != 0 {
		t.Errorf("search for a replaced message = %v, want nothing", got)
	}
}

func TestSearchForPunctuationFindsNothingRatherThanEverything(t *testing.T) {
	repo, projectID := seedForSearch(t)
	// Long enough for the index to look up, and matching nothing — which is
	// the honest answer, not the whole project.
	if got := searchTitles(t, repo, projectID, "---"); len(got) != 0 {
		t.Errorf("search = %v, want nothing: a query nothing contains is not a missing filter", got)
	}
}

func TestSearchDoesNotLetFTSSyntaxThrough(t *testing.T) {
	repo, projectID := seedForSearch(t)

	// Every one of these is an FTS5 operator. They have to be searched for as
	// text — no syntax error, and no boolean behaviour the user did not ask
	// for (ADR 011). A term too short for the index is refused with the
	// product's own error, which is a different thing from FTS5 complaining
	// about syntax.
	for _, query := range []string{`"`, `NEAR(a b)`, `value OR connection`, `^value`, `value AND`} {
		_, err := repo.List(context.Background(),
			ports.IssueFilter{ProjectID: projectID, Query: query})
		if err != nil && !errors.Is(err, domain.ErrInvalidSearch) {
			t.Errorf("search for %q failed: %v", query, err)
		}
	}
	// "value OR connection" is three literal words; only an issue containing
	// all three would match, and none does.
	if got := searchTitles(t, repo, projectID, "value OR connection"); len(got) != 0 {
		t.Errorf("search = %v, want nothing: OR is a word here, not an operator", got)
	}
}

func TestSearchCombinesWithTheOtherFilters(t *testing.T) {
	repo, projectID := seedForSearch(t)

	page, err := repo.List(context.Background(), ports.IssueFilter{
		ProjectID: projectID,
		Query:     "database",
		Tags:      map[string]string{"server": "web-01"},
	})
	if err != nil {
		t.Fatalf("searching: %v", err)
	}
	if len(page.Issues) != 1 {
		t.Errorf("search plus tag = %d issues, want the one that matches both", len(page.Issues))
	}

	page, err = repo.List(context.Background(), ports.IssueFilter{
		ProjectID: projectID,
		Query:     "database",
		Tags:      map[string]string{"server": "nowhere"},
	})
	if err != nil {
		t.Fatalf("searching: %v", err)
	}
	if len(page.Issues) != 0 {
		t.Errorf("search plus a tag nothing carries = %d issues, want none", len(page.Issues))
	}
}

func TestOpenBuildsTheSearchIndexAndLeavesNoProbeBehind(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	var tables int
	if err := db.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_master WHERE name = 'issues_fts'").Scan(&tables); err != nil {
		t.Fatalf("looking for the index: %v", err)
	}
	if tables != 1 {
		t.Error("issues_fts is missing: the search index was never created")
	}

	// The startup check builds a virtual table to prove the module works. It
	// goes in the temp schema and is dropped again — with a single connection
	// in the pool, one left behind would outlive the check and be visible to
	// every query that followed.
	var probes int
	if err := db.QueryRowContext(ctx,
		"SELECT count(*) FROM temp.sqlite_master WHERE name LIKE '%fts5_probe%'").Scan(&probes); err != nil {
		t.Fatalf("looking for the probe: %v", err)
	}
	if probes != 0 {
		t.Error("the FTS5 probe table was left behind")
	}
}

func TestTheSearchIndexIsBackfilledForIssuesThatPredateIt(t *testing.T) {
	repo, projectID := newIssueRepo(t)
	ctx := context.Background()

	// Simulating the upgrade: an issue whose row exists but whose index entry
	// was never written. A search that silently ignored everything recorded
	// before the upgrade would be worse than no search at all, so the
	// migration backfills — this proves the trigger is not the only writer.
	record(t, repo, projectID, "old", at(10), nil)
	if _, err := repo.db.ExecContext(ctx,
		"INSERT INTO issues_fts (issues_fts) VALUES ('rebuild')"); err != nil {
		t.Fatalf("rebuilding the index: %v", err)
	}

	if got := searchTitles(t, repo, projectID, "ValueError"); len(got) != 1 {
		t.Errorf("search after a rebuild = %v, want the issue", got)
	}
}
