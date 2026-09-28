package domain

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// The bounds on what a status page can be told to say.
const (
	// MaxStatusPageTitle bounds the heading. It is a page title, not a
	// paragraph, and a heading longer than this breaks the layout of the one
	// page in this product whose readers are not operators.
	MaxStatusPageTitle = 80
	// MaxStatusPageDescription bounds the line under it.
	MaxStatusPageDescription = 200
	// StatusPageDays is how many days the bar draws. Ninety, which is what
	// ADR 017 promises and what the daily roll-up is kept for.
	StatusPageDays = 90
	// StatusPageMaxIncidents bounds the incident list. A page that lists
	// every bad day for three months is a wall nobody reads; the bar above it
	// already shows all ninety.
	StatusPageMaxIncidents = 10
	// StatusPageTTL is how long a rendered page is reused.
	//
	// Thirty seconds, matched to the floor on a monitor's interval: a shorter
	// cache cannot show anything newer, because nothing newer exists. It is
	// also the whole defence of an unauthenticated page against being read a
	// thousand times a second — the work per hit becomes a map lookup
	// (ADR 017).
	StatusPageTTL = 30 * time.Second
)

// StatusPageSettings is what the operator writes on the page, installation
// wide.
//
// Installation-wide rather than per project because the thing it names is the
// organisation, not the project: "Acme status" is the same sentence whichever
// project's page you are on, and asking for it once is one field instead of
// one per project. Whether a given project has a page at all is a per-project
// decision and lives in its config (ADR 017).
type StatusPageSettings struct {
	// Title is the heading. Empty means the default, which is built from the
	// project's name.
	Title string `json:"title,omitempty"`
	// Description is the line under it. Empty means none, and none is a
	// perfectly good status page.
	Description string `json:"description,omitempty"`
}

// StatusPageSettingsKey is where StatusPageSettings is stored (ports.SettingsStore).
const StatusPageSettingsKey = "status_page"

// Validate trims and bounds what an operator typed.
func (s *StatusPageSettings) Validate() error {
	s.Title = strings.TrimSpace(s.Title)
	s.Description = strings.TrimSpace(s.Description)
	if len(s.Title) > MaxStatusPageTitle {
		return fmt.Errorf("%w: the title exceeds %d characters", ErrInvalidSetting, MaxStatusPageTitle)
	}
	if len(s.Description) > MaxStatusPageDescription {
		return fmt.Errorf("%w: the description exceeds %d characters",
			ErrInvalidSetting, MaxStatusPageDescription)
	}
	return nil
}

// DecodeStatusPageSettings reads a stored value.
//
// Unreadable settings fall back to the defaults rather than failing, for the
// same reason DecodeProjectConfig does: a corrupted cosmetic field must not
// take a public page down.
func DecodeStatusPageSettings(raw []byte) StatusPageSettings {
	var settings StatusPageSettings
	if len(raw) == 0 {
		return settings
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return StatusPageSettings{}
	}
	if err := settings.Validate(); err != nil {
		return StatusPageSettings{}
	}
	return settings
}

// StatusPageWindow is one of the three uptime figures the page shows.
type StatusPageWindow struct {
	// Label is what the page calls it: "24 h", "7 d", "90 d".
	Label string
	// Uptime is the percentage of checks that passed in the window.
	Uptime float64
	// Checks is how many checks the figure is made of. Zero means there is no
	// evidence either way, and the page says so instead of printing 100 %.
	Checks int64
}

// HasData reports whether the window rests on any check at all.
func (w StatusPageWindow) HasData() bool { return w.Checks > 0 }

// StatusPageBar is one day of the ninety-day bar.
type StatusPageBar struct {
	// Day is the date, midnight UTC.
	Day time.Time
	// Checks is how many checks ran that day.
	Checks int64
	// Failures is how many of them failed.
	Failures int64
	// BeforeMonitor is true for a day earlier than the monitor itself.
	//
	// It is the distinction uptime left the status page to make: a day with no checks
	// because nothing was watching yet is not the same as a day with no
	// checks because the server was off, and drawing both as a green bar
	// claims ninety days of uptime for a monitor created this morning.
	BeforeMonitor bool
}

// State is what the bar is drawn as: "up", "down", "partial" or "none".
func (b StatusPageBar) State() string {
	switch {
	case b.BeforeMonitor || b.Checks == 0:
		return "none"
	case b.Failures == 0:
		return "up"
	case b.Failures >= b.Checks:
		return "down"
	default:
		return "partial"
	}
}

// Uptime is the day's percentage, and it is only meaningful when Checks > 0.
func (b StatusPageBar) Uptime() float64 {
	if b.Checks <= 0 {
		return 0
	}
	return float64(b.Checks-b.Failures) / float64(b.Checks) * 100
}

// StatusPageIncident is a stretch of a monitor not answering.
//
// A day rather than a precise range, because the evidence the page is allowed
// to read is the daily roll-up and not the checks themselves (ADR 001, and
// ADR 017 says so in as many words). What the day carries is enough to be
// useful: how many checks failed, and roughly how long that is at this
// monitor's interval. Approximating is stated on the page; scanning ninety
// days of results to be exact is the query this product exists to not run.
type StatusPageIncident struct {
	// Day is when it happened.
	Day time.Time
	// Failures is how many checks failed that day.
	Failures int64
	// Checks is how many ran.
	Checks int64
	// Downtime is Failures at the monitor's interval, rounded to a minute.
	Downtime time.Duration
	// Ongoing marks the incident the monitor is still in.
	Ongoing bool
}

// StatusPageMonitor is one monitor as the page shows it.
type StatusPageMonitor struct {
	// Name is the monitor's name.
	Name string
	// Status is up, down or unknown.
	Status UptimeStatus
	// Since is when it entered that status, nil while unknown.
	Since *time.Time
	// Windows are the three uptime figures, in the order they are drawn.
	Windows []StatusPageWindow
	// Bars is StatusPageDays days, oldest first.
	Bars []StatusPageBar
	// Incidents are the days with failures, newest first.
	Incidents []StatusPageIncident
}

// StatusPage is everything one rendering of the page needs.
type StatusPage struct {
	// Title is the heading.
	Title string
	// Description is the line under it, possibly empty.
	Description string
	// Monitors are the public monitors, in the order they were created.
	Monitors []StatusPageMonitor
	// GeneratedAt is when this rendering was built. The page prints it,
	// because a cached page that does not say how old it is invites somebody
	// to read a thirty-second-old outage as the present state of the world.
	GeneratedAt time.Time
	// AllOperational is true when every monitor is up. It is the one-line
	// answer the page leads with, which is the only thing most readers want.
	AllOperational bool
	// Degraded is how many monitors are not up.
	Degraded int
}

// NewStatusPageMonitor projects one monitor and its history onto the page.
//
// Everything here is a pure function of the monitor row and the daily
// roll-up: no query decides what the page says, so the whole of "what does a
// day with no data look like", "what counts as an incident" and "what is the
// 24-hour figure" is testable without a database or a clock.
//
// days may have gaps and need not be sorted; absent days are absent on
// purpose, and this is where they become either "nothing ran" or "before this
// monitor existed".
func NewStatusPageMonitor(monitor *UptimeMonitor, days []UptimeDay, now time.Time) StatusPageMonitor {
	now = now.UTC()
	byDay := make(map[string]UptimeDay, len(days))
	for _, day := range days {
		byDay[UptimeDayKey(day.Day)] = day
	}

	created := monitor.CreatedAt.UTC().Truncate(24 * time.Hour)
	today := now.Truncate(24 * time.Hour)

	bars := make([]StatusPageBar, 0, StatusPageDays)
	for offset := StatusPageDays - 1; offset >= 0; offset-- {
		day := today.AddDate(0, 0, -offset)
		stored := byDay[UptimeDayKey(day)]
		bars = append(bars, StatusPageBar{
			Day:           day,
			Checks:        stored.Checks,
			Failures:      stored.Failures,
			BeforeMonitor: day.Before(created),
		})
	}

	return StatusPageMonitor{
		Name:   monitor.Name,
		Status: monitor.Status,
		Since:  monitor.LastStatusChangeAt,
		Windows: []StatusPageWindow{
			uptimeWindow("24 h", bars, 24*time.Hour, now),
			uptimeWindow("7 d", bars, 7*24*time.Hour, now),
			uptimeWindow("90 d", bars, StatusPageDays*24*time.Hour, now),
		},
		Bars:      bars,
		Incidents: incidentsFrom(bars, monitor, now),
	}
}

// uptimeWindow is the trailing window ending at now, from the daily roll-up.
//
// The figures come from `uptime_daily` and from nowhere else: ADR 017 says so
// in as many words, and ADR 001 is the reason — a public page must not be a
// way to make this server scan ninety days of checks per reader. The roll-up's
// finest resolution is a day, and a trailing window almost never lands on a
// day boundary, so the day the window starts inside is counted **in
// proportion** to the part of it the window covers.
//
// That assumes the day's failures were spread through it, which is an
// assumption and is why the page says the figures come from daily aggregates.
// It is only ever wrong about the one day at the far edge of the window, it is
// exactly right for the ninety-day figure, and the alternative — calling
// "today so far" the last twenty-four hours — is wrong by up to a whole day
// and wrong in a way nobody can see. The per-day bar underneath shows the
// unaveraged truth either way.
func uptimeWindow(label string, bars []StatusPageBar, window time.Duration, now time.Time) StatusPageWindow {
	from := now.Add(-window)
	var checks, failures float64
	for _, bar := range bars {
		if bar.Checks == 0 {
			continue
		}
		share := overlapShare(bar.Day, from, now)
		checks += float64(bar.Checks) * share
		failures += float64(bar.Failures) * share
	}

	result := StatusPageWindow{Label: label, Uptime: 100, Checks: int64(math.Round(checks))}
	if checks > 0 {
		result.Uptime = (checks - failures) / checks * 100
	}
	return result
}

// overlapShare is how much of one day's checks fall inside [from, to].
//
// The day's checks are spread over the part of it that has already happened,
// which for every day but today is the whole of it and for today is the hours
// up to now. Dividing by that — rather than by a flat twenty-four hours —
// is what stops the current day from being discounted for the hours it has
// not lived yet, which would make every figure on the page sag towards
// midnight and recover by evening.
func overlapShare(day, from, to time.Time) float64 {
	extentEnd := day.AddDate(0, 0, 1)
	if extentEnd.After(to) {
		extentEnd = to
	}
	extent := extentEnd.Sub(day)
	if extent <= 0 {
		return 0
	}

	start := day
	if start.Before(from) {
		start = from
	}
	covered := extentEnd.Sub(start)
	if covered <= 0 {
		return 0
	}
	return math.Min(1, covered.Seconds()/extent.Seconds())
}

// incidentsFrom lists the days a monitor failed on, newest first.
func incidentsFrom(bars []StatusPageBar, monitor *UptimeMonitor, now time.Time) []StatusPageIncident {
	interval := monitor.Interval()
	var incidents []StatusPageIncident
	for i := len(bars) - 1; i >= 0 && len(incidents) < StatusPageMaxIncidents; i-- {
		bar := bars[i]
		if bar.Failures == 0 {
			continue
		}
		downtime := time.Duration(bar.Failures) * interval
		incidents = append(incidents, StatusPageIncident{
			Day:      bar.Day,
			Failures: bar.Failures,
			Checks:   bar.Checks,
			Downtime: downtime.Round(time.Minute),
			// Today's row, while the monitor is still down, is the incident
			// somebody is on the page to read about.
			Ongoing: monitor.Status == UptimeDown && !bar.Day.Before(now.Truncate(24*time.Hour)),
		})
	}
	return incidents
}

// NewStatusPage assembles the page from its monitors.
func NewStatusPage(settings StatusPageSettings, project string, monitors []StatusPageMonitor, now time.Time) StatusPage {
	page := StatusPage{
		Title:       settings.Title,
		Description: settings.Description,
		Monitors:    monitors,
		GeneratedAt: now.UTC(),
	}
	if page.Title == "" {
		page.Title = project + " status"
	}
	for _, monitor := range monitors {
		if monitor.Status != UptimeUp {
			page.Degraded++
		}
	}
	// A page with no public monitors is not "all operational": there is
	// nothing being reported on, and saying everything is fine on no evidence
	// is the one thing a status page must never do.
	page.AllOperational = len(monitors) > 0 && page.Degraded == 0
	return page
}

// FormatUptime renders a percentage the way a status page should.
//
// Two decimals, and never rounded up to 100 when it is not: a page that says
// "100.00 %" for 99.996 % is telling a reader who just sat through an outage
// that it did not happen.
func FormatUptime(value float64) string {
	if value < 100 && value > 99.99 {
		return "99.99 %"
	}
	return fmt.Sprintf("%.2f %%", math.Floor(value*100)/100)
}
