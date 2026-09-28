package domain

import (
	"fmt"
	"strings"
	"time"
)

// The two triggers a monitor produces.
//
// Declared here rather than beside the four in alert_rule.go because they
// belong to this subsystem: a reader asking "what can an uptime monitor
// notify about" should find the answer in the file about uptime monitors. They
// are members of AllTriggerKinds all the same, so a rule can be written
// against them exactly like any other (ADR 015, ADR 016).
const (
	// TriggerUptimeDown fires when a monitor crosses into down.
	TriggerUptimeDown TriggerKind = "uptime_down"
	// TriggerUptimeRecovered fires when one that was down answers again.
	//
	// A separate trigger and not a field on the first, because the two are
	// wanted in different places: "it broke" belongs where somebody is paged
	// and "it is back" belongs where the team is watching, and a single
	// trigger would force both into the same channel.
	TriggerUptimeRecovered TriggerKind = "uptime_recovered"
)

// UptimeStatus is where a monitor stands.
type UptimeStatus string

// The three states of a monitor.
const (
	// UptimeUnknown is a monitor that has not been checked yet. It is a
	// distinct state rather than an optimistic "up": a status page that
	// claims a service is fine before anything has looked at it is lying, and
	// a monitor created during an outage would report the outage as a
	// recovery the moment it first failed.
	UptimeUnknown UptimeStatus = "unknown"
	// UptimeUp is answering as expected.
	UptimeUp UptimeStatus = "up"
	// UptimeDown has failed enough consecutive checks to be believed.
	UptimeDown UptimeStatus = "down"
)

// Valid reports whether s is a status this build stores.
func (s UptimeStatus) Valid() bool {
	switch s {
	case UptimeUnknown, UptimeUp, UptimeDown:
		return true
	default:
		return false
	}
}

// The bounds on what a monitor can be configured to do.
const (
	// MinCheckInterval is the shortest gap between two checks.
	//
	// Thirty seconds, and it is a floor rather than a suggestion: every check
	// is an outbound request this server makes on somebody's behalf, and a
	// monitor at one second is a hundred and twenty times the traffic for a
	// resolution nobody reads. It is also what keeps a hundred monitors from
	// needing a hundred goroutines to stay on schedule (ADR 016).
	MinCheckInterval = 30 * time.Second
	// MaxCheckInterval is a day. Longer than that is not monitoring.
	MaxCheckInterval = 24 * time.Hour
	// DefaultCheckInterval is what a monitor gets when it does not say.
	DefaultCheckInterval = 60 * time.Second
	// MinCheckTimeout is the shortest a check may wait for an answer.
	MinCheckTimeout = time.Second
	// MaxCheckTimeout caps it in absolute terms. The rule that actually
	// matters is the relative one below — a timeout must be shorter than the
	// interval, or a slow target produces overlapping checks of the same
	// monitor — and this is the ceiling for the monitors whose interval is
	// long enough that the relative rule would allow an absurd wait.
	MaxCheckTimeout = 120 * time.Second
	// DefaultCheckTimeout is ten seconds, which is long past the point where
	// a user would have given up.
	DefaultCheckTimeout = 10 * time.Second

	// DownThreshold is how many consecutive failures make a monitor down.
	//
	// Two, and the second one is the whole point. A single failed request is
	// the most ordinary event on the internet — a dropped packet, a pod being
	// replaced, a TLS handshake that timed out by a hundred milliseconds —
	// and an alert for each of them is an alert nobody reads by the end of
	// the week. The cost is one interval of extra latency on a real outage,
	// which is a trade every on-call engineer would make (ADR 016).
	DownThreshold = 2
	// UpThreshold is how many consecutive successes bring it back. One:
	// asymmetry is deliberate, because being told something recovered late is
	// worse than being told early, and a false recovery corrects itself at
	// the next check.
	UpThreshold = 1

	// MaxMonitorName bounds the display name.
	MaxMonitorName = 80
	// MaxMonitorURL bounds the target. Two kilobytes is past what any server
	// accepts in a request line.
	MaxMonitorURL = 2048
	// MaxExpectedBody bounds the substring a check looks for in the body.
	MaxExpectedBody = 200
	// MaxCheckBody is how much of a response body is read looking for that
	// substring. A health endpoint answers in bytes; anything that answers in
	// megabytes is not being read, it is being downloaded, and this server
	// must not be made to download it once a minute.
	MaxCheckBody = 64 * 1024
	// MaxCheckRedirects bounds a redirect chain. Every hop is re-validated,
	// so this is not a security bound — it is what stops a loop from spending
	// the whole timeout.
	MaxCheckRedirects = 5
)

// The methods a check may use.
//
// GET and HEAD only. Both are safe and idempotent, which matters more here
// than anywhere else in this product: whatever URL somebody types will be
// requested by this server, unattended, forever. A monitor that could POST
// would be a scheduled side effect aimed at a target chosen by whoever can
// write a monitor.
const (
	MethodGET  = "GET"
	MethodHEAD = "HEAD"
)

// UptimeMonitor is one HTTP check and everything decided about it.
type UptimeMonitor struct {
	// ID is the storage identity, zero before it is saved.
	ID int64
	// ProjectID is who it belongs to.
	ProjectID int64
	// Name is what an operator recognises it by, and what an alert says broke.
	Name string
	// URL is what gets fetched. Its syntax is checked here; whether the
	// address behind it may be visited is not a question the domain can
	// answer, and is asked of the SSRF guard by the use case (ADR 004).
	URL string
	// Method is GET or HEAD.
	Method string
	// IntervalSeconds is how often it runs.
	IntervalSeconds int
	// TimeoutSeconds is how long one check waits.
	TimeoutSeconds int
	// ExpectedStatusMin and ExpectedStatusMax bound the status code that
	// counts as healthy, inclusive. A range rather than a code because a
	// health endpoint that answers 204 and one that answers 200 are both fine
	// and neither should need a second monitor.
	ExpectedStatusMin int
	// ExpectedStatusMax is the top of that range.
	ExpectedStatusMax int
	// ExpectedBodySubstring must appear in the response body. Empty means the
	// body is not read at all. This is what tells "the load balancer is up"
	// from "the application behind it is up": a 200 from an error page is
	// still a 200.
	ExpectedBodySubstring string
	// FollowRedirects is whether a 3xx is followed. Each hop is re-validated
	// against the SSRF guard, because a redirect is a URL somebody else chose.
	FollowRedirects bool
	// AllowPrivate is this monitor's half of the permission to reach an
	// address that is not globally routable. The other half is the
	// installation's -uptime-allow-private, and both are required (ADR 016).
	AllowPrivate bool
	// Public is whether this monitor appears on the status page.
	Public bool
	// Enabled is whether it is checked at all.
	Enabled bool

	// Status is where it stands.
	Status UptimeStatus
	// ConsecutiveFailures is how many checks in a row have failed. It is
	// stored rather than recomputed from the results, because the results are
	// pruned at ninety days and the state must survive that — and because
	// recomputing it would be a query per check for a number the last write
	// already knew.
	ConsecutiveFailures int
	// LastCheckedAt is when it last ran, nil before the first check.
	LastCheckedAt *time.Time
	// NextCheckAt is when it is next due. Stored so "which monitors are due"
	// is an indexed range scan rather than a computation over every row.
	NextCheckAt time.Time
	// LastStatusChangeAt is when Status last changed, nil while unknown.
	LastStatusChangeAt *time.Time
	// CreatedAt is when it was added.
	CreatedAt time.Time
}

// NewUptimeMonitor validates a monitor and fills the defaults that have one.
//
// The URL arrives already parsed and approved by the caller — the domain
// checks its shape and length, and knows nothing about addresses (ADR 004).
func NewUptimeMonitor(input *UptimeMonitor, now time.Time) (UptimeMonitor, error) {
	monitor := *input
	if monitor.ProjectID <= 0 {
		return UptimeMonitor{}, fmt.Errorf("%w: %d is not a project id", ErrInvalidMonitor, monitor.ProjectID)
	}

	monitor.Name = strings.TrimSpace(monitor.Name)
	if monitor.Name == "" {
		return UptimeMonitor{}, fmt.Errorf("%w: a monitor needs a name", ErrInvalidMonitor)
	}
	if len(monitor.Name) > MaxMonitorName {
		return UptimeMonitor{}, fmt.Errorf("%w: name exceeds %d characters", ErrInvalidMonitor, MaxMonitorName)
	}

	monitor.URL = strings.TrimSpace(monitor.URL)
	if monitor.URL == "" {
		return UptimeMonitor{}, fmt.Errorf("%w: a monitor needs a url", ErrInvalidMonitor)
	}
	if len(monitor.URL) > MaxMonitorURL {
		return UptimeMonitor{}, fmt.Errorf("%w: url exceeds %d characters", ErrInvalidMonitor, MaxMonitorURL)
	}

	monitor.Method = strings.ToUpper(strings.TrimSpace(monitor.Method))
	if monitor.Method == "" {
		monitor.Method = MethodGET
	}
	if monitor.Method != MethodGET && monitor.Method != MethodHEAD {
		return UptimeMonitor{}, fmt.Errorf("%w: method must be %s or %s, got %q",
			ErrInvalidMonitor, MethodGET, MethodHEAD, monitor.Method)
	}

	if monitor.IntervalSeconds == 0 {
		monitor.IntervalSeconds = int(DefaultCheckInterval.Seconds())
	}
	interval := time.Duration(monitor.IntervalSeconds) * time.Second
	if interval < MinCheckInterval || interval > MaxCheckInterval {
		return UptimeMonitor{}, fmt.Errorf(
			"%w: interval_s must be between %d and %d seconds, got %d; a check more often than "+
				"every %s is traffic somebody else pays for",
			ErrInvalidMonitor, int(MinCheckInterval.Seconds()), int(MaxCheckInterval.Seconds()),
			monitor.IntervalSeconds, MinCheckInterval)
	}

	if monitor.TimeoutSeconds == 0 {
		monitor.TimeoutSeconds = int(DefaultCheckTimeout.Seconds())
	}
	timeout := time.Duration(monitor.TimeoutSeconds) * time.Second
	if timeout < MinCheckTimeout || timeout > MaxCheckTimeout {
		return UptimeMonitor{}, fmt.Errorf("%w: timeout_s must be between %d and %d seconds, got %d",
			ErrInvalidMonitor, int(MinCheckTimeout.Seconds()), int(MaxCheckTimeout.Seconds()),
			monitor.TimeoutSeconds)
	}
	if timeout >= interval {
		// Otherwise a slow target produces overlapping checks of the same
		// monitor, and the failure counter starts describing concurrency
		// rather than the target.
		return UptimeMonitor{}, fmt.Errorf("%w: timeout_s (%d) must be shorter than interval_s (%d)",
			ErrInvalidMonitor, monitor.TimeoutSeconds, monitor.IntervalSeconds)
	}

	if monitor.ExpectedStatusMin == 0 && monitor.ExpectedStatusMax == 0 {
		// The default that means "any success", which is what somebody who
		// did not think about it wants.
		monitor.ExpectedStatusMin, monitor.ExpectedStatusMax = 200, 299
	}
	if monitor.ExpectedStatusMin < 100 || monitor.ExpectedStatusMin > 599 ||
		monitor.ExpectedStatusMax < 100 || monitor.ExpectedStatusMax > 599 {
		return UptimeMonitor{}, fmt.Errorf("%w: expected status codes must be between 100 and 599, got %d–%d",
			ErrInvalidMonitor, monitor.ExpectedStatusMin, monitor.ExpectedStatusMax)
	}
	if monitor.ExpectedStatusMin > monitor.ExpectedStatusMax {
		return UptimeMonitor{}, fmt.Errorf("%w: expected status range is inverted: %d–%d",
			ErrInvalidMonitor, monitor.ExpectedStatusMin, monitor.ExpectedStatusMax)
	}

	monitor.ExpectedBodySubstring = strings.TrimSpace(monitor.ExpectedBodySubstring)
	if len(monitor.ExpectedBodySubstring) > MaxExpectedBody {
		return UptimeMonitor{}, fmt.Errorf("%w: expected_body_substring exceeds %d characters",
			ErrInvalidMonitor, MaxExpectedBody)
	}
	if monitor.ExpectedBodySubstring != "" && monitor.Method == MethodHEAD {
		// A HEAD response has no body by definition, so this pair describes a
		// monitor that can never pass. Refusing it is the only reading that
		// is not a silent trap.
		return UptimeMonitor{}, fmt.Errorf(
			"%w: a HEAD request has no body, so expected_body_substring would never match; "+
				"use GET or drop the substring", ErrInvalidMonitor)
	}

	if !monitor.Status.Valid() {
		monitor.Status = UptimeUnknown
	}
	monitor.CreatedAt = now.UTC()
	if monitor.NextCheckAt.IsZero() {
		// Due immediately. A monitor that waited a full interval before its
		// first check would leave somebody watching a blank status page for a
		// minute wondering whether they had configured it correctly.
		monitor.NextCheckAt = now.UTC()
	}
	return monitor, nil
}

// Interval is the gap between checks as a duration.
//
// Pointer receivers from here down, on a domain type that is otherwise passed
// around by value. The reason is the one AlertPayload gives: an UptimeMonitor
// is a little over two hundred bytes of strings and timestamps, and copying it
// once per call in order to read it is the copy gocritic's hugeParam threshold
// exists to catch — on the one path in this product that runs on a timer over
// every monitor in the installation. None of these mutates anything, so the
// pointer costs none of what passing domain values by value normally buys.
func (m *UptimeMonitor) Interval() time.Duration {
	return time.Duration(m.IntervalSeconds) * time.Second
}

// Timeout is how long one check waits, as a duration.
func (m *UptimeMonitor) Timeout() time.Duration {
	return time.Duration(m.TimeoutSeconds) * time.Second
}

// SubjectKey is what a rule stays quiet about after firing on this monitor
// (ADR 015).
func (m *UptimeMonitor) SubjectKey() string {
	return MonitorSubjectKey(MonitorKindUptime, m.ID)
}

// CheckResult is what one check found.
type CheckResult struct {
	// At is when the check ran.
	At time.Time
	// OK is whether it met every expectation.
	OK bool
	// StatusCode is what the far end answered, zero when nothing did.
	StatusCode int
	// LatencyMS is how long it took, including connection and TLS.
	LatencyMS int
	// Error is why it failed, empty when it did not. It is the only thing
	// that makes a failing monitor fixable without reproducing it by hand.
	Error string
}

// Evaluate turns a response into a verdict.
//
// A pure function of the monitor's expectations and what came back, so the
// rule that decides whether something is healthy has a table of tests behind
// it rather than living inside an HTTP client. The empty string means healthy.
func (m *UptimeMonitor) Evaluate(statusCode int, body string) string {
	if statusCode < m.ExpectedStatusMin || statusCode > m.ExpectedStatusMax {
		return fmt.Sprintf("status %d is outside the expected %d–%d",
			statusCode, m.ExpectedStatusMin, m.ExpectedStatusMax)
	}
	if m.ExpectedBodySubstring != "" && !strings.Contains(body, m.ExpectedBodySubstring) {
		// The substring is quoted and the body is not echoed: a body can be
		// a megabyte of HTML, and an error message that contains one is an
		// error message nobody reads.
		return fmt.Sprintf("the body does not contain %q", m.ExpectedBodySubstring)
	}
	return ""
}

// UptimeTransition is what a check did to a monitor's status.
type UptimeTransition string

// The three outcomes of applying a result.
const (
	// TransitionNone means the status did not change.
	TransitionNone UptimeTransition = ""
	// TransitionDown means the monitor has just been declared down.
	TransitionDown UptimeTransition = "down"
	// TransitionRecovered means it has just come back.
	TransitionRecovered UptimeTransition = "recovered"
)

// Trigger maps a transition onto the alert trigger it fires, if any.
func (t UptimeTransition) Trigger() (TriggerKind, bool) {
	switch t {
	case TransitionDown:
		return TriggerUptimeDown, true
	case TransitionRecovered:
		return TriggerUptimeRecovered, true
	default:
		return "", false
	}
}

// Apply folds one result into a monitor and reports what changed.
//
// This is the whole of the state machine, and it is a pure function on
// purpose: everything about when somebody gets woken up is decided here, with
// a table of tests behind it, rather than inside a job that would need a
// network and a clock to exercise.
//
// The asymmetry is deliberate and is the reason this exists at all. Going down
// takes DownThreshold consecutive failures, because one failed request is
// noise and alerting on noise is how alerting gets muted. Coming back takes
// one success, because a monitor that stayed "down" through a working check
// would be reporting the past.
//
// The first check of a brand-new monitor never produces a transition when it
// succeeds: unknown → up is not a recovery and there was nobody to tell.
func (m *UptimeMonitor) Apply(result CheckResult) (UptimeMonitor, UptimeTransition) {
	updated := *m
	at := result.At.UTC()
	updated.LastCheckedAt = &at
	updated.NextCheckAt = at.Add(m.Interval())

	if result.OK {
		updated.ConsecutiveFailures = 0
		if m.Status == UptimeUp {
			return updated, TransitionNone
		}
		updated.Status = UptimeUp
		updated.LastStatusChangeAt = &at
		if m.Status == UptimeDown {
			return updated, TransitionRecovered
		}
		// unknown → up: the first look at something that was already fine.
		return updated, TransitionNone
	}

	updated.ConsecutiveFailures = m.ConsecutiveFailures + 1
	if updated.ConsecutiveFailures < DownThreshold || m.Status == UptimeDown {
		return updated, TransitionNone
	}
	updated.Status = UptimeDown
	updated.LastStatusChangeAt = &at
	return updated, TransitionDown
}

// AlertEventFor renders a transition as the event the alert rules see.
//
// Count carries the latency of the check that produced it, which is the one
// number worth putting in a message about a monitor: on a recovery it is how
// slow the thing is now, and on a failure it is how long this server waited
// before giving up.
func (m *UptimeMonitor) AlertEventFor(transition UptimeTransition, result CheckResult) (AlertEvent, bool) {
	kind, ok := transition.Trigger()
	if !ok {
		return AlertEvent{}, false
	}
	culprit := m.URL
	if result.Error != "" {
		culprit = m.URL + " — " + result.Error
	}
	return AlertEvent{
		Kind:        kind,
		ProjectID:   m.ProjectID,
		MonitorKind: MonitorKindUptime,
		MonitorID:   m.ID,
		MonitorSlug: m.Name,
		Title:       m.Name,
		Culprit:     culprit,
		Level:       LevelError,
		Count:       int64(result.LatencyMS),
		At:          result.At.UTC(),
	}, true
}

// UptimeDay is one day of a monitor's history, as the status page reads it.
//
// The aggregate exists so ninety days of a bar chart is ninety rows rather
// than a scan of every check ever made — the same rule the hourly issue
// aggregates follow, applied to the one other place this product draws a long
// timeline (ADR 001, ADR 017).
type UptimeDay struct {
	// Day is midnight UTC of the day this covers.
	Day time.Time
	// Checks is how many ran.
	Checks int64
	// Failures is how many of them failed.
	Failures int64
	// LatencySum is the total latency in milliseconds, so a mean can be taken
	// without storing one. A stored average cannot be merged with another
	// day's; a sum and a count can.
	LatencySum int64
}

// Uptime is the fraction of checks that passed, as a percentage. A day with no
// checks is reported as 100: there is no evidence of an outage, and drawing a
// red bar for "the server was switched off" would be inventing one.
func (d UptimeDay) Uptime() float64 {
	if d.Checks <= 0 {
		return 100
	}
	return float64(d.Checks-d.Failures) / float64(d.Checks) * 100
}

// MeanLatencyMS is the average check latency for the day, zero when nothing
// ran.
func (d UptimeDay) MeanLatencyMS() int64 {
	if d.Checks <= 0 {
		return 0
	}
	return d.LatencySum / d.Checks
}

// UptimeDayKey is how a day is written in the aggregate table: the date in
// UTC, which sorts lexicographically in the order it sorts chronologically and
// is readable by a person looking at the row.
func UptimeDayKey(at time.Time) string {
	return at.UTC().Format("2006-01-02")
}
