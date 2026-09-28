package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/envelope"
	"github.com/antoniojosev/trapline/internal/usecase"
)

// The day the assertions are written against, fixed so a chart's contents do
// not depend on when the suite runs.
const (
	cliStatsDay   = "2026-08-24"
	cliStatsRange = "-from=" + cliStatsDay + "T08 -to=" + cliStatsDay + "T13"
)

// ingest puts an event into the harness through the real use case, which is
// the only way the hourly buckets ever get written (ADR 010): they are a side
// effect of storing an event, not something a test can insert.
func (h *harness) ingest(projectID int64, kind, environment, release string, hour int) {
	h.t.Helper()

	event := fmt.Sprintf(
		`{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","timestamp":"%sT%02d:30:00Z",`+
			`"platform":"python","level":"error","release":%q,"environment":%q,`+
			`"exception":{"values":[{"type":%q,"value":"la función de pago falló"}]}}`,
		cliStatsDay, hour, release, environment, kind)
	body := fmt.Sprintf("{}\n{\"type\":\"event\",\"length\":%d}\n%s\n", len(event), event)

	result, err := h.stack.Ingest.Process(context.Background(), projectID,
		strings.NewReader(body), envelope.DefaultLimits(), usecase.ClientInfo{})
	if err != nil {
		h.t.Fatalf("ingesting: %v", err)
	}
	if result.Accepted != 1 {
		h.t.Fatalf("ingest accepted %d events, want 1 (%+v)", result.Accepted, result.Dropped)
	}
}

// newStatsHarness is a harness with a project and a small, uneven day in it.
func newStatsHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.setUp()
	h.mustRun("projects", "create", "-name", "venekambio")

	for range 3 {
		h.ingest(1, "ValueError", "production", "app@1.0.0", 10)
	}
	h.ingest(1, "TypeError", "staging", "app@1.1.0", 12)
	return h
}

func TestStatsPrintsASeries(t *testing.T) {
	h := newStatsHarness(t)

	text := h.mustRun(append([]string{"stats", "-project", "1"}, strings.Fields(cliStatsRange)...)...)

	if !strings.Contains(text, cliStatsDay+"T10") || !strings.Contains(text, "error=3") {
		t.Errorf("output = %q, want the 10:00 hour with its three errors", text)
	}
	// A terminal full of zeroes hides the hour that was not one.
	if strings.Contains(text, cliStatsDay+"T09") {
		t.Errorf("output = %q, want quiet hours left out of the text form", text)
	}
}

func TestStatsJSONCarriesEveryHourIncludingTheQuietOnes(t *testing.T) {
	h := newStatsHarness(t)

	raw := h.mustRun(append([]string{"stats", "-project", "1", "--json"}, strings.Fields(cliStatsRange)...)...)

	var payload seriesPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("the JSON form is not parseable: %v\n%s", err, raw)
	}
	// The JSON is what a chart reads, and a chart needs the axis whole.
	if len(payload.Series) != 6 {
		t.Errorf("series has %d points, want 6", len(payload.Series))
	}
	if payload.Total != 4 {
		t.Errorf("total = %d, want 4", payload.Total)
	}
	if payload.Project.Name != "venekambio" {
		t.Errorf("project = %+v, want it named", payload.Project)
	}
}

func TestStatsTopAndBreakdown(t *testing.T) {
	h := newStatsHarness(t)
	rangeArgs := strings.Fields(cliStatsRange)

	top := h.mustRun(append([]string{"stats", "-project", "1", "-top", "5"}, rangeArgs...)...)
	if !strings.Contains(top, "3×") || !strings.Contains(top, "ValueError") {
		t.Errorf("top = %q, want the ValueError issue with its three events", top)
	}

	releases := h.mustRun(append([]string{"stats", "-project", "1", "-by", "release"}, rangeArgs...)...)
	if !strings.Contains(releases, "app@1.0.0\t3") {
		t.Errorf("breakdown = %q, want app@1.0.0 with 3", releases)
	}

	environments := h.mustRun(append([]string{"stats", "-project", "1", "-by", "environment"}, rangeArgs...)...)
	if !strings.Contains(environments, "staging\t1") {
		t.Errorf("breakdown = %q, want staging with 1", environments)
	}
}

func TestStatsForOneIssue(t *testing.T) {
	h := newStatsHarness(t)

	raw := h.mustRun("stats", "-project", "1", "-issue", "1", "-range", "14d", "--json")
	var payload issueStatsPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("the JSON form is not parseable: %v\n%s", err, raw)
	}
	if payload.Window != string(domain.Window14d) || len(payload.Series) != 14*24 {
		t.Errorf("payload = %q with %d points, want 14d with %d", payload.Window, len(payload.Series), 14*24)
	}
}

func TestStatsRefusesArgumentsThatContradictEachOther(t *testing.T) {
	h := newStatsHarness(t)

	cases := [][]string{
		{"stats"},
		{"stats", "-project", "0"},
		{"stats", "-project", "1", "-issue", "1", "-by", "release"},
		{"stats", "-project", "1", "-issue", "1", "-top", "5"},
	}
	for _, args := range cases {
		code, _, stderr := h.run(args...)
		if code != ExitUsage {
			t.Errorf("%v exited %d, want %d (usage)", args, code, ExitUsage)
		}
		if stderr == "" {
			t.Errorf("%v said nothing on stderr", args)
		}
	}
}

func TestStatsReportsAServerRefusalAsARuntimeError(t *testing.T) {
	h := newStatsHarness(t)

	// A bad range is the server's decision, not a usage error the CLI could
	// have caught: the exit code has to say "it ran and failed", because an
	// agent branches on exactly that difference.
	code, _, stderr := h.run("stats", "-project", "1", "-by", "constellation")
	if code != ExitError {
		t.Errorf("exit = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr, "release") {
		t.Errorf("stderr = %q, want it to name the dimensions that exist", stderr)
	}
}

func TestIssueSearchGoesThroughTheIndex(t *testing.T) {
	h := newStatsHarness(t)

	// The flag already existed; what changed is what it searches. An infix, a
	// prefix, a second term and an accent are what the LIKE it replaced either
	// could not do or did only by scanning every issue (ADR 011).
	for _, query := range []string{"Value", "Error", "función", "funcion"} {
		raw := h.mustRun("issues", "list", "-project", "1", "-q", query, "--json")
		var page issuePagePayload
		if err := json.Unmarshal([]byte(raw), &page); err != nil {
			t.Fatalf("parsing the listing: %v", err)
		}
		if len(page.Issues) == 0 {
			t.Errorf("search for %q found nothing", query)
		}
	}

	raw := h.mustRun("issues", "list", "-project", "1", "-q", "Value nonexistent", "--json")
	var page issuePagePayload
	if err := json.Unmarshal([]byte(raw), &page); err != nil {
		t.Fatalf("parsing the listing: %v", err)
	}
	if len(page.Issues) != 0 {
		t.Errorf("two terms found %d issues, want none: they are ANDed", len(page.Issues))
	}
	// And the counts survive a search that matched nothing, because they label
	// the filter buttons rather than describe the query.
	if page.Counts["unresolved"] != 2 {
		t.Errorf("counts = %+v, want the project's two unresolved issues", page.Counts)
	}
}

func TestIssueSearchRefusesATermTooShortToLookUp(t *testing.T) {
	h := newStatsHarness(t)

	// Exit 1, not 0 with an empty list: an agent branches on the code, and
	// "your query was unrunnable" is not "there is nothing wrong in this
	// project".
	code, _, stderr := h.run("issues", "list", "-project", "1", "-q", "ab", "--json")
	if code != ExitError {
		t.Errorf("exit = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr, "3 characters") {
		t.Errorf("stderr = %q, want it to say how long a term has to be", stderr)
	}
}
