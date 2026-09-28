package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

var checkedAt = time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

// valid is a monitor that passes, so each case below can break exactly one
// thing and the failure names it.
func valid() *domain.UptimeMonitor {
	return &domain.UptimeMonitor{
		ProjectID: 1,
		Name:      "venekambio",
		URL:       "https://www.venekambio.com/health",
		Method:    "GET",
	}
}

func TestNewUptimeMonitorFillsDefaults(t *testing.T) {
	t.Parallel()

	monitor, err := domain.NewUptimeMonitor(valid(), checkedAt)
	if err != nil {
		t.Fatalf("NewUptimeMonitor: %v", err)
	}
	if monitor.IntervalSeconds != int(domain.DefaultCheckInterval.Seconds()) {
		t.Errorf("interval defaulted to %d", monitor.IntervalSeconds)
	}
	if monitor.TimeoutSeconds != int(domain.DefaultCheckTimeout.Seconds()) {
		t.Errorf("timeout defaulted to %d", monitor.TimeoutSeconds)
	}
	if monitor.ExpectedStatusMin != 200 || monitor.ExpectedStatusMax != 299 {
		t.Errorf("expected status defaulted to %d–%d", monitor.ExpectedStatusMin, monitor.ExpectedStatusMax)
	}
	if monitor.Status != domain.UptimeUnknown {
		t.Errorf("a monitor was born %s rather than unknown", monitor.Status)
	}
	// Due immediately: a first check a minute after creation looks like a
	// broken feature to whoever just configured it.
	if !monitor.NextCheckAt.Equal(checkedAt) {
		t.Errorf("next check is %s, want it due now", monitor.NextCheckAt)
	}
}

func TestNewUptimeMonitorDefaultsTheMethod(t *testing.T) {
	t.Parallel()

	input := valid()
	input.Method = ""
	monitor, err := domain.NewUptimeMonitor(input, checkedAt)
	if err != nil {
		t.Fatalf("NewUptimeMonitor: %v", err)
	}
	if monitor.Method != domain.MethodGET {
		t.Errorf("method defaulted to %q, want GET", monitor.Method)
	}

	input.Method = "head"
	monitor, err = domain.NewUptimeMonitor(input, checkedAt)
	if err != nil {
		t.Fatalf("NewUptimeMonitor: %v", err)
	}
	if monitor.Method != domain.MethodHEAD {
		t.Errorf("a lowercase method was not normalised: %q", monitor.Method)
	}
}

func TestNewUptimeMonitorRejects(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		mutate   func(*domain.UptimeMonitor)
		contains string
	}{
		{"no project", func(m *domain.UptimeMonitor) { m.ProjectID = 0 }, "project id"},
		{"no name", func(m *domain.UptimeMonitor) { m.Name = "   " }, "needs a name"},
		{"a name that is too long", func(m *domain.UptimeMonitor) {
			m.Name = strings.Repeat("x", domain.MaxMonitorName+1)
		}, "exceeds"},
		{"no url", func(m *domain.UptimeMonitor) { m.URL = "" }, "needs a url"},
		{"a url that is too long", func(m *domain.UptimeMonitor) {
			m.URL = "https://example.com/" + strings.Repeat("x", domain.MaxMonitorURL)
		}, "exceeds"},
		{"a method that is not safe", func(m *domain.UptimeMonitor) { m.Method = "POST" }, "GET or HEAD"},
		// The one the gate checks: a floor, not a suggestion.
		{"an interval below the floor", func(m *domain.UptimeMonitor) { m.IntervalSeconds = 29 }, "interval_s must be between"},
		{"an interval above the ceiling", func(m *domain.UptimeMonitor) { m.IntervalSeconds = 90000 }, "interval_s must be between"},
		{"a negative interval", func(m *domain.UptimeMonitor) { m.IntervalSeconds = -1 }, "interval_s must be between"},
		{"a timeout below the floor", func(m *domain.UptimeMonitor) { m.TimeoutSeconds = -5 }, "timeout_s must be between"},
		{"a timeout above the ceiling", func(m *domain.UptimeMonitor) {
			m.IntervalSeconds, m.TimeoutSeconds = 3600, 300
		}, "timeout_s must be between"},
		{"a timeout as long as the interval", func(m *domain.UptimeMonitor) {
			m.IntervalSeconds, m.TimeoutSeconds = 30, 30
		}, "must be shorter than"},
		{"a timeout longer than the interval", func(m *domain.UptimeMonitor) {
			m.IntervalSeconds, m.TimeoutSeconds = 30, 90
		}, "must be shorter than"},
		{"an impossible status code", func(m *domain.UptimeMonitor) {
			m.ExpectedStatusMin, m.ExpectedStatusMax = 99, 200
		}, "between 100 and 599"},
		{"a status code past the end", func(m *domain.UptimeMonitor) {
			m.ExpectedStatusMin, m.ExpectedStatusMax = 200, 600
		}, "between 100 and 599"},
		{"an inverted status range", func(m *domain.UptimeMonitor) {
			m.ExpectedStatusMin, m.ExpectedStatusMax = 299, 200
		}, "inverted"},
		{"a substring that is too long", func(m *domain.UptimeMonitor) {
			m.ExpectedBodySubstring = strings.Repeat("x", domain.MaxExpectedBody+1)
		}, "exceeds"},
		// A HEAD response has no body, so this pair can never pass. Silently
		// accepting it would be a monitor that is down forever for a reason
		// nothing explains.
		{"a body substring on a HEAD check", func(m *domain.UptimeMonitor) {
			m.Method, m.ExpectedBodySubstring = "HEAD", "ok"
		}, "no body"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			input := valid()
			tc.mutate(input)
			_, err := domain.NewUptimeMonitor(input, checkedAt)
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !errors.Is(err, domain.ErrInvalidMonitor) {
				t.Errorf("error is not ErrInvalidMonitor: %v", err)
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("message %q does not carry %q", err, tc.contains)
			}
		})
	}
}

// TestMinimumIntervalIsThirtySeconds fixes the number itself, because it is a
// promise made to whoever owns the endpoint being checked.
func TestMinimumIntervalIsThirtySeconds(t *testing.T) {
	t.Parallel()

	if domain.MinCheckInterval != 30*time.Second {
		t.Errorf("MinCheckInterval is %s, want 30s (ADR 016)", domain.MinCheckInterval)
	}
	input := valid()
	input.IntervalSeconds = 30
	if _, err := domain.NewUptimeMonitor(input, checkedAt); err != nil {
		t.Errorf("exactly the floor was rejected: %v", err)
	}
}

func TestEvaluate(t *testing.T) {
	t.Parallel()

	monitor, err := domain.NewUptimeMonitor(valid(), checkedAt)
	if err != nil {
		t.Fatalf("NewUptimeMonitor: %v", err)
	}

	if reason := monitor.Evaluate(200, "anything"); reason != "" {
		t.Errorf("a 200 was judged a failure: %s", reason)
	}
	if reason := monitor.Evaluate(299, ""); reason != "" {
		t.Errorf("the top of the range was judged a failure: %s", reason)
	}
	if reason := monitor.Evaluate(500, ""); reason == "" {
		t.Error("a 500 was judged healthy")
	} else if !strings.Contains(reason, "200–299") {
		t.Errorf("the reason does not say what was expected: %s", reason)
	}
	if reason := monitor.Evaluate(301, ""); reason == "" {
		t.Error("a redirect that was not followed was judged healthy")
	}
}

// TestEvaluateReadsTheBody is the difference between "the load balancer
// answers" and "the application behind it works".
func TestEvaluateReadsTheBody(t *testing.T) {
	t.Parallel()

	input := valid()
	input.ExpectedBodySubstring = "\"database\":\"ok\""
	monitor, err := domain.NewUptimeMonitor(input, checkedAt)
	if err != nil {
		t.Fatalf("NewUptimeMonitor: %v", err)
	}

	if reason := monitor.Evaluate(200, `{"database":"ok"}`); reason != "" {
		t.Errorf("a matching body was judged a failure: %s", reason)
	}
	reason := monitor.Evaluate(200, `{"database":"unreachable"}`)
	if reason == "" {
		t.Fatal("a 200 whose body says the database is down was judged healthy")
	}
	if !strings.Contains(reason, "database") {
		t.Errorf("the reason does not name the substring: %s", reason)
	}
}

// TestApply walks the state machine. Each row is a status, a result, and what
// the pair must produce — the whole of "when does somebody get woken up".
func TestApply(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		status         domain.UptimeStatus
		failures       int
		ok             bool
		wantStatus     domain.UptimeStatus
		wantFailures   int
		wantTransition domain.UptimeTransition
	}{
		{
			name:   "the first check of something that works",
			status: domain.UptimeUnknown, ok: true,
			wantStatus: domain.UptimeUp, wantTransition: domain.TransitionNone,
		},
		{
			name:   "the first check of something that does not",
			status: domain.UptimeUnknown, ok: false,
			wantStatus: domain.UptimeUnknown, wantFailures: 1, wantTransition: domain.TransitionNone,
		},
		{
			name:   "one failure is noise, not an outage",
			status: domain.UptimeUp, ok: false,
			wantStatus: domain.UptimeUp, wantFailures: 1, wantTransition: domain.TransitionNone,
		},
		{
			name:   "the second consecutive failure is the outage",
			status: domain.UptimeUp, failures: 1, ok: false,
			wantStatus: domain.UptimeDown, wantFailures: 2, wantTransition: domain.TransitionDown,
		},
		{
			name:   "a monitor already down does not report going down again",
			status: domain.UptimeDown, failures: 7, ok: false,
			wantStatus: domain.UptimeDown, wantFailures: 8, wantTransition: domain.TransitionNone,
		},
		{
			name:   "one success is enough to come back",
			status: domain.UptimeDown, failures: 9, ok: true,
			wantStatus: domain.UptimeUp, wantTransition: domain.TransitionRecovered,
		},
		{
			name:   "a working monitor that keeps working says nothing",
			status: domain.UptimeUp, ok: true,
			wantStatus: domain.UptimeUp, wantTransition: domain.TransitionNone,
		},
		{
			name:   "a failure that is not the second resets nothing",
			status: domain.UptimeUnknown, failures: 1, ok: false,
			wantStatus: domain.UptimeDown, wantFailures: 2, wantTransition: domain.TransitionDown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			monitor := valid()
			monitor.ID = 3
			monitor.IntervalSeconds = 60
			monitor.Status = tc.status
			monitor.ConsecutiveFailures = tc.failures

			updated, transition := monitor.Apply(domain.CheckResult{At: checkedAt, OK: tc.ok})

			if updated.Status != tc.wantStatus {
				t.Errorf("status = %s, want %s", updated.Status, tc.wantStatus)
			}
			if updated.ConsecutiveFailures != tc.wantFailures {
				t.Errorf("failures = %d, want %d", updated.ConsecutiveFailures, tc.wantFailures)
			}
			if transition != tc.wantTransition {
				t.Errorf("transition = %q, want %q", transition, tc.wantTransition)
			}
			if updated.LastCheckedAt == nil || !updated.LastCheckedAt.Equal(checkedAt) {
				t.Errorf("last checked = %v", updated.LastCheckedAt)
			}
			if want := checkedAt.Add(time.Minute); !updated.NextCheckAt.Equal(want) {
				t.Errorf("next check = %s, want %s", updated.NextCheckAt, want)
			}
			// The original is untouched: Apply returns a new value rather
			// than mutating, which is what lets a caller decide whether to
			// persist the result at all.
			if monitor.Status != tc.status {
				t.Errorf("Apply mutated its receiver")
			}
		})
	}
}

// TestApplyRecordsWhenTheStatusChanged: the status page draws incidents from
// this timestamp, so a transition that does not move it is an incident with
// no beginning.
func TestApplyRecordsWhenTheStatusChanged(t *testing.T) {
	t.Parallel()

	monitor := valid()
	monitor.IntervalSeconds = 60
	monitor.Status = domain.UptimeUp
	monitor.ConsecutiveFailures = 1

	down, transition := monitor.Apply(domain.CheckResult{At: checkedAt, OK: false})
	if transition != domain.TransitionDown {
		t.Fatalf("transition = %q", transition)
	}
	if down.LastStatusChangeAt == nil || !down.LastStatusChangeAt.Equal(checkedAt) {
		t.Fatalf("the change was not timestamped: %v", down.LastStatusChangeAt)
	}

	later := checkedAt.Add(5 * time.Minute)
	stillDown, transition := down.Apply(domain.CheckResult{At: later, OK: false})
	if transition != domain.TransitionNone {
		t.Errorf("a monitor that stayed down reported %q", transition)
	}
	if !stillDown.LastStatusChangeAt.Equal(checkedAt) {
		t.Error("staying down moved the timestamp of when it went down")
	}
}

func TestTransitionTrigger(t *testing.T) {
	t.Parallel()

	if kind, ok := domain.TransitionDown.Trigger(); !ok || kind != domain.TriggerUptimeDown {
		t.Errorf("down maps to %q, %v", kind, ok)
	}
	if kind, ok := domain.TransitionRecovered.Trigger(); !ok || kind != domain.TriggerUptimeRecovered {
		t.Errorf("recovered maps to %q, %v", kind, ok)
	}
	if _, ok := domain.TransitionNone.Trigger(); ok {
		t.Error("no transition produced a trigger, which would notify about nothing")
	}
}

// TestAlertEventCarriesTheMonitor: the silence window is keyed on the subject,
// so two monitors failing together must be two subjects.
func TestAlertEventCarriesTheMonitor(t *testing.T) {
	t.Parallel()

	first := valid()
	first.ID, first.ProjectID = 1, 7
	second := valid()
	second.ID, second.ProjectID = 2, 7

	result := domain.CheckResult{At: checkedAt, LatencyMS: 431, Error: "connection refused"}

	one, ok := first.AlertEventFor(domain.TransitionDown, result)
	if !ok {
		t.Fatal("a down transition produced no event")
	}
	other, _ := second.AlertEventFor(domain.TransitionDown, result)

	if one.SubjectKey() == other.SubjectKey() {
		t.Errorf("two monitors share the subject %q, so one would silence the other", one.SubjectKey())
	}
	if one.SubjectKey() != "monitor:uptime:1" {
		t.Errorf("subject key is %q, want monitor:uptime:1", one.SubjectKey())
	}
	if one.MonitorKind != domain.MonitorKindUptime || one.MonitorSlug != first.Name {
		t.Errorf("the event does not say which monitor it is about: %+v", one)
	}
	if one.Kind != domain.TriggerUptimeDown {
		t.Errorf("kind = %q", one.Kind)
	}
	if one.Count != 431 {
		t.Errorf("the latency did not travel: %d", one.Count)
	}
	if !strings.Contains(one.Culprit, "connection refused") {
		t.Errorf("the reason did not travel: %q", one.Culprit)
	}

	if _, ok := first.AlertEventFor(domain.TransitionNone, result); ok {
		t.Error("a non-transition produced an alert event")
	}
}

// TestAlertEventWithoutAnError covers the recovery shape, where there is no
// reason to append.
func TestAlertEventWithoutAnError(t *testing.T) {
	t.Parallel()

	monitor := valid()
	monitor.ID = 4
	event, ok := monitor.AlertEventFor(domain.TransitionRecovered,
		domain.CheckResult{At: checkedAt, OK: true, LatencyMS: 12})
	if !ok {
		t.Fatal("a recovery produced no event")
	}
	if event.Culprit != monitor.URL {
		t.Errorf("culprit = %q, want just the url", event.Culprit)
	}
}

func TestUptimeStatusValid(t *testing.T) {
	t.Parallel()

	for _, status := range []domain.UptimeStatus{domain.UptimeUp, domain.UptimeDown, domain.UptimeUnknown} {
		if !status.Valid() {
			t.Errorf("%s is not accepted as a status", status)
		}
	}
	if domain.UptimeStatus("degraded").Valid() {
		t.Error("a status this build does not store was accepted")
	}
	// An unreadable stored status becomes unknown rather than being rejected:
	// a row written by a newer build must not stop this one from booting.
	input := valid()
	input.Status = "degraded"
	monitor, err := domain.NewUptimeMonitor(input, checkedAt)
	if err != nil {
		t.Fatalf("NewUptimeMonitor: %v", err)
	}
	if monitor.Status != domain.UptimeUnknown {
		t.Errorf("an unknown status became %q", monitor.Status)
	}
}

func TestUptimeDay(t *testing.T) {
	t.Parallel()

	day := domain.UptimeDay{Checks: 1440, Failures: 14, LatencySum: 144000}
	if got := day.Uptime(); got < 99.02 || got > 99.03 {
		t.Errorf("uptime = %.4f, want ~99.028", got)
	}
	if got := day.MeanLatencyMS(); got != 100 {
		t.Errorf("mean latency = %d, want 100", got)
	}

	// A day nothing ran on is not an outage. Drawing one would report the
	// server being switched off as the service being down.
	empty := domain.UptimeDay{}
	if got := empty.Uptime(); got != 100 {
		t.Errorf("an empty day reports %.1f%% uptime", got)
	}
	if got := empty.MeanLatencyMS(); got != 0 {
		t.Errorf("an empty day reports a latency of %d", got)
	}
}

func TestUptimeDayKey(t *testing.T) {
	t.Parallel()

	// Deliberately an instant that is a different date in a nearby zone: the
	// aggregate is in UTC and must not move with the reader.
	at := time.Date(2026, 8, 29, 2, 30, 0, 0, time.FixedZone("America/Caracas", -4*3600))
	if got := domain.UptimeDayKey(at); got != "2026-08-29" {
		t.Errorf("UptimeDayKey = %q, want 2026-08-29", got)
	}
}

func TestUptimeTriggersAreRules(t *testing.T) {
	t.Parallel()

	for _, kind := range []domain.TriggerKind{domain.TriggerUptimeDown, domain.TriggerUptimeRecovered} {
		if !kind.Valid() {
			t.Errorf("%s cannot be written as a rule", kind)
		}
		trigger := domain.Trigger{Kind: kind}
		if err := trigger.Validate(); err != nil {
			t.Errorf("%s does not validate: %v", kind, err)
		}
		// It carries no parameters: a monitor's threshold belongs to the
		// monitor, not to the rule watching it.
		withParams := domain.Trigger{Kind: kind, MinCount: 3}
		if err := withParams.Validate(); err == nil {
			t.Errorf("%s accepted a parameter it does not use", kind)
		}
	}
}

func TestMonitorSubjectKey(t *testing.T) {
	t.Parallel()

	monitor := valid()
	monitor.ID = 12
	if got := monitor.SubjectKey(); got != "monitor:uptime:12" {
		t.Errorf("SubjectKey = %q", got)
	}
}

func TestIntervalAndTimeoutAsDurations(t *testing.T) {
	t.Parallel()

	monitor := domain.UptimeMonitor{IntervalSeconds: 90, TimeoutSeconds: 5}
	if monitor.Interval() != 90*time.Second {
		t.Errorf("Interval = %s", monitor.Interval())
	}
	if monitor.Timeout() != 5*time.Second {
		t.Errorf("Timeout = %s", monitor.Timeout())
	}
}
