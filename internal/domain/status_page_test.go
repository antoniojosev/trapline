package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

// pageNow is the moment every case below is rendered at: mid-afternoon UTC, so
// the day boundary is nowhere near it and a proration is not accidentally
// zero or one.
var pageNow = time.Date(2026, 8, 29, 15, 0, 0, 0, time.UTC)

func pageMonitor(created time.Time) *domain.UptimeMonitor {
	return &domain.UptimeMonitor{
		ID: 3, ProjectID: 1, Name: "api", URL: "https://api.example.test/health",
		Method: domain.MethodGET, IntervalSeconds: 60,
		Public: true, Enabled: true, Status: domain.UptimeUp,
		CreatedAt: created,
	}
}

func day(offset int, checks, failures int64) domain.UptimeDay {
	return domain.UptimeDay{
		Day:      pageNow.Truncate(24*time.Hour).AddDate(0, 0, -offset),
		Checks:   checks,
		Failures: failures,
	}
}

func TestStatusPageDrawsNinetyDaysOldestFirst(t *testing.T) {
	t.Parallel()

	monitor := pageMonitor(pageNow.AddDate(0, 0, -200))
	view := domain.NewStatusPageMonitor(monitor, nil, pageNow)

	if len(view.Bars) != domain.StatusPageDays {
		t.Fatalf("%d bars, want %d", len(view.Bars), domain.StatusPageDays)
	}
	if !view.Bars[len(view.Bars)-1].Day.Equal(pageNow.Truncate(24 * time.Hour)) {
		t.Errorf("the last bar is %s, want today", view.Bars[len(view.Bars)-1].Day)
	}
	for i := 1; i < len(view.Bars); i++ {
		if !view.Bars[i-1].Day.Before(view.Bars[i].Day) {
			t.Fatalf("bar %d is not after bar %d", i, i-1)
		}
	}
}

// The distinction uptime left the status page to make. A monitor added this
// morning has no history, and drawing eighty-nine green bars behind it would
// be a claim of three months of uptime that nothing supports.
func TestDaysBeforeTheMonitorAreNotDrawnAsUptime(t *testing.T) {
	t.Parallel()

	monitor := pageMonitor(pageNow.AddDate(0, 0, -2))
	view := domain.NewStatusPageMonitor(monitor, []domain.UptimeDay{day(0, 100, 0)}, pageNow)

	if state := view.Bars[0].State(); state != "none" {
		t.Errorf("a day before the monitor existed is drawn %q, want none", state)
	}
	if !view.Bars[len(view.Bars)-1].BeforeMonitor && view.Bars[len(view.Bars)-1].State() != "up" {
		t.Errorf("today is drawn %q, want up", view.Bars[len(view.Bars)-1].State())
	}
	// And a day inside the monitor's life on which nothing ran is *also*
	// "none" — but for the other reason, which the bar's tooltip says.
	yesterday := view.Bars[len(view.Bars)-2]
	if yesterday.BeforeMonitor {
		t.Errorf("yesterday is marked as before a monitor created the day before")
	}
	if yesterday.State() != "none" {
		t.Errorf("a day with no checks is drawn %q, want none", yesterday.State())
	}
}

func TestBarStates(t *testing.T) {
	t.Parallel()

	cases := map[string]domain.StatusPageBar{
		"none":    {Checks: 0},
		"up":      {Checks: 10, Failures: 0},
		"partial": {Checks: 10, Failures: 3},
		"down":    {Checks: 10, Failures: 10},
	}
	for want, bar := range cases {
		if got := bar.State(); got != want {
			t.Errorf("%+v is drawn %q, want %q", bar, got, want)
		}
	}
}

// The three windows come from the daily roll-up, and the boundary day is
// counted in proportion (ADR 001, ADR 017). At 15:00 UTC the trailing day is
// today in full plus the last nine hours of yesterday.
func TestTrailingWindowsProrateTheBoundaryDay(t *testing.T) {
	t.Parallel()

	monitor := pageMonitor(pageNow.AddDate(0, 0, -200))
	// Today: 100 checks, none failed. Yesterday: 100 checks, all failed.
	// Nine of yesterday's twenty-four hours are inside the window, so the
	// figure must land near 100 / (100 + 37.5) — not at 100 % and not at 50 %.
	view := domain.NewStatusPageMonitor(monitor, []domain.UptimeDay{
		day(0, 100, 0), day(1, 100, 100),
	}, pageNow)

	window := view.Windows[0]
	if window.Label != "24 h" {
		t.Fatalf("the first window is %q", window.Label)
	}
	if window.Uptime <= 60 || window.Uptime >= 90 {
		t.Errorf("the 24 h figure is %.2f, want it between a full day of failure and none", window.Uptime)
	}
	// The seven-day figure sees both days whole, so it is a clean half.
	if seven := view.Windows[1]; seven.Uptime < 49 || seven.Uptime > 51 {
		t.Errorf("the 7 d figure is %.2f, want about 50", seven.Uptime)
	}
	if ninety := view.Windows[2]; ninety.Checks != 200 {
		t.Errorf("the 90 d figure rests on %d checks, want every one of them", ninety.Checks)
	}
}

// A window with no checks behind it is not "100 %". It is "we do not know",
// and a status page that prints the first for the second is the one failure
// mode this page cannot have.
func TestAWindowWithNoChecksHasNoFigure(t *testing.T) {
	t.Parallel()

	monitor := pageMonitor(pageNow)
	view := domain.NewStatusPageMonitor(monitor, nil, pageNow)
	for _, window := range view.Windows {
		if window.HasData() {
			t.Errorf("the %s window claims %d checks", window.Label, window.Checks)
		}
	}
}

func TestIncidentsAreTheDaysThatFailed(t *testing.T) {
	t.Parallel()

	monitor := pageMonitor(pageNow.AddDate(0, 0, -200))
	monitor.Status = domain.UptimeDown
	view := domain.NewStatusPageMonitor(monitor, []domain.UptimeDay{
		day(0, 100, 4), day(1, 100, 0), day(2, 100, 30),
	}, pageNow)

	if len(view.Incidents) != 2 {
		t.Fatalf("%d incidents, want the two days with failures", len(view.Incidents))
	}
	// Newest first: the page is read during an outage, and the one happening
	// now has to be the first thing under the bar.
	if !view.Incidents[0].Day.After(view.Incidents[1].Day) {
		t.Errorf("incidents are not newest first")
	}
	if !view.Incidents[0].Ongoing {
		t.Errorf("today's incident is not marked ongoing while the monitor is down")
	}
	if view.Incidents[1].Ongoing {
		t.Errorf("an incident from two days ago is marked ongoing")
	}
	// Four failed checks at a minute apart is about four minutes.
	if want := 4 * time.Minute; view.Incidents[0].Downtime != want {
		t.Errorf("downtime = %s, want %s", view.Incidents[0].Downtime, want)
	}
}

func TestIncidentsAreCapped(t *testing.T) {
	t.Parallel()

	monitor := pageMonitor(pageNow.AddDate(0, 0, -200))
	var days []domain.UptimeDay
	for offset := range 40 {
		days = append(days, day(offset, 100, 1))
	}
	view := domain.NewStatusPageMonitor(monitor, days, pageNow)
	if len(view.Incidents) != domain.StatusPageMaxIncidents {
		t.Errorf("%d incidents, want them capped at %d", len(view.Incidents), domain.StatusPageMaxIncidents)
	}
}

func TestAPageWithNoMonitorsIsNotAllOperational(t *testing.T) {
	t.Parallel()

	page := domain.NewStatusPage(domain.StatusPageSettings{}, "checkout", nil, pageNow)
	if page.AllOperational {
		t.Error("a page reporting on nothing says everything is fine")
	}
	if page.Title != "checkout status" {
		t.Errorf("title = %q, want it built from the project name", page.Title)
	}
}

func TestAPageCountsWhatIsNotUp(t *testing.T) {
	t.Parallel()

	up := domain.StatusPageMonitor{Status: domain.UptimeUp}
	down := domain.StatusPageMonitor{Status: domain.UptimeDown}
	unknown := domain.StatusPageMonitor{Status: domain.UptimeUnknown}

	page := domain.NewStatusPage(domain.StatusPageSettings{Title: "Acme"}, "acme",
		[]domain.StatusPageMonitor{up, down, unknown}, pageNow)
	if page.AllOperational {
		t.Error("a page with a monitor down says everything is fine")
	}
	// Unknown counts as degraded: a monitor nobody has heard from is not
	// evidence that the thing it watches is working.
	if page.Degraded != 2 {
		t.Errorf("degraded = %d, want the down one and the unknown one", page.Degraded)
	}
	if page.Title != "Acme" {
		t.Errorf("a configured title was overwritten: %q", page.Title)
	}
}

// The number people screenshot. Rounding 99.996 up to 100.00 tells somebody
// who just sat through an outage that it did not happen.
func TestFormatUptimeNeverRoundsUpToOneHundred(t *testing.T) {
	t.Parallel()

	cases := map[float64]string{
		100:      "100.00 %",
		99.996:   "99.99 %",
		99.9999:  "99.99 %",
		99.994:   "99.99 %",
		99.5:     "99.50 %",
		0:        "0.00 %",
		66.66666: "66.66 %",
	}
	for value, want := range cases {
		if got := domain.FormatUptime(value); got != want {
			t.Errorf("FormatUptime(%v) = %q, want %q", value, got, want)
		}
	}
}

func TestStatusPageSettingsAreBounded(t *testing.T) {
	t.Parallel()

	settings := domain.StatusPageSettings{Title: "  Acme  ", Description: "  everything  "}
	if err := settings.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if settings.Title != "Acme" || settings.Description != "everything" {
		t.Errorf("nothing was trimmed: %+v", settings)
	}

	long := domain.StatusPageSettings{Title: strings.Repeat("a", domain.MaxStatusPageTitle+1)}
	if err := long.Validate(); !errors.Is(err, domain.ErrInvalidSetting) {
		t.Errorf("an over-long title = %v, want ErrInvalidSetting", err)
	}
	wordy := domain.StatusPageSettings{Description: strings.Repeat("a", domain.MaxStatusPageDescription+1)}
	if err := wordy.Validate(); !errors.Is(err, domain.ErrInvalidSetting) {
		t.Errorf("an over-long description = %v, want ErrInvalidSetting", err)
	}
}

// The page is public and has to render. A settings document that a newer build
// wrote, or that somebody corrupted, must not be the reason a status page 500s
// during an outage.
func TestUnreadableSettingsFallBackToTheDefault(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"", "{", `{"title":123}`, `{"title":"` + strings.Repeat("a", 200) + `"}`} {
		settings := domain.DecodeStatusPageSettings([]byte(raw))
		if settings.Title != "" || settings.Description != "" {
			t.Errorf("%q decoded to %+v, want the empty default", raw, settings)
		}
	}
	good := domain.DecodeStatusPageSettings([]byte(`{"title":"Acme","description":"status"}`))
	if good.Title != "Acme" || good.Description != "status" {
		t.Errorf("a good document decoded to %+v", good)
	}
}

func TestMonitorKindsAreTheTwoFamilies(t *testing.T) {
	t.Parallel()

	kinds := domain.AllMonitorKinds()
	if len(kinds) != 2 {
		t.Fatalf("%d monitor kinds, want cron and uptime", len(kinds))
	}
	for _, kind := range kinds {
		if !kind.Valid() {
			t.Errorf("%q is listed and not valid", kind)
		}
	}
	if domain.MonitorKind("tcp").Valid() {
		t.Error("a kind no build has is valid")
	}
	// And the two never collide, which is the whole reason the kind is in the
	// key (ADR 037).
	if domain.MonitorSubjectKey(domain.MonitorKindCron, 7) ==
		domain.MonitorSubjectKey(domain.MonitorKindUptime, 7) {
		t.Error("two families share a subject key for the same id")
	}
}

func TestABarKnowsItsOwnPercentage(t *testing.T) {
	t.Parallel()

	if got := (domain.StatusPageBar{Checks: 200, Failures: 4}).Uptime(); got != 98 {
		t.Errorf("Uptime = %v, want 98", got)
	}
	// A day nothing ran on has no percentage at all, and the template asks
	// Checks before it asks this.
	if got := (domain.StatusPageBar{}).Uptime(); got != 0 {
		t.Errorf("a day with no checks reports %v", got)
	}
}
