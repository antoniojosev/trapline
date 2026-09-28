package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

func everyMinute(t *testing.T) domain.CronSpec {
	t.Helper()
	spec, err := domain.ParseCronSpec(domain.ScheduleCrontab, "*/1 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func TestParseCronSpecCrontab(t *testing.T) {
	spec, err := domain.ParseCronSpec(domain.ScheduleCrontab, "0 3 * * *")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Type != domain.ScheduleCrontab || spec.String() != "0 3 * * *" {
		t.Fatalf("got %+v", spec)
	}
	next, err := spec.Next(time.Date(2026, 3, 1, 1, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !next.Equal(time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("Next = %s", next)
	}
}

func TestParseCronSpecInterval(t *testing.T) {
	cases := map[string]time.Duration{
		"5 minute":  5 * time.Minute,
		"5 minutes": 5 * time.Minute,
		"2 hour":    2 * time.Hour,
		"1 day":     24 * time.Hour,
		"1 week":    7 * 24 * time.Hour,
	}
	from := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	for value, want := range cases {
		t.Run(value, func(t *testing.T) {
			spec, err := domain.ParseCronSpec(domain.ScheduleInterval, value)
			if err != nil {
				t.Fatal(err)
			}
			next, err := spec.Next(from, time.UTC)
			if err != nil {
				t.Fatal(err)
			}
			if next.Sub(from) != want {
				t.Fatalf("Next - from = %s, want %s", next.Sub(from), want)
			}
			// Stored and shown in the normalised singular, whichever way it
			// was written: it is one column and one vocabulary.
			if !strings.HasSuffix(spec.String(), strings.TrimSuffix(strings.Fields(value)[1], "s")) {
				t.Fatalf("String() = %q for %q", spec.String(), value)
			}
		})
	}
}

// Months and years are calendar steps, not multiples of a duration: a monthly
// job that ran on 31 January is next due in February, not 30 days later.
func TestParseCronSpecCalendarIntervals(t *testing.T) {
	from := time.Date(2026, 1, 31, 10, 0, 0, 0, time.UTC)

	monthly, err := domain.ParseCronSpec(domain.ScheduleInterval, "1 month")
	if err != nil {
		t.Fatal(err)
	}
	next, err := monthly.Next(from, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if next.Month() != time.March {
		// Go normalises 31 February to 3 March, which is the standard library's
		// documented behaviour and is still a month later, not thirty days.
		t.Logf("31 January + 1 month normalises to %s", next)
	}
	if !next.After(from) {
		t.Fatalf("Next = %s", next)
	}

	yearly, err := domain.ParseCronSpec(domain.ScheduleInterval, "1 year")
	if err != nil {
		t.Fatal(err)
	}
	next, err = yearly.Next(from, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if next.Year() != 2027 {
		t.Fatalf("a yearly interval landed in %d", next.Year())
	}
}

func TestParseCronSpecRejects(t *testing.T) {
	cases := []struct {
		name  string
		kind  domain.ScheduleType
		value string
	}{
		{"unknown type", domain.ScheduleType("weekly-ish"), "1"},
		{"interval with one word", domain.ScheduleInterval, "5"},
		{"interval with three words", domain.ScheduleInterval, "5 minutes exactly"},
		{"interval with no count", domain.ScheduleInterval, "many minutes"},
		{"interval of zero", domain.ScheduleInterval, "0 minute"},
		{"interval of a negative", domain.ScheduleInterval, "-3 minute"},
		{"unknown unit", domain.ScheduleInterval, "5 fortnight"},
		{"bad crontab", domain.ScheduleCrontab, "not a crontab"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := domain.ParseCronSpec(testCase.kind, testCase.value)
			if !errors.Is(err, domain.ErrInvalidMonitor) {
				t.Fatalf("err = %v, want ErrInvalidMonitor", err)
			}
		})
	}
}

func TestCronSpecZeroValueNextFails(t *testing.T) {
	var spec domain.CronSpec
	if _, err := spec.Next(time.Now(), nil); !errors.Is(err, domain.ErrInvalidMonitor) {
		t.Fatalf("err = %v", err)
	}
}

func TestNewCronMonitorFillsDefaults(t *testing.T) {
	now := time.Date(2026, 3, 1, 10, 0, 30, 0, time.UTC)
	monitor, err := domain.NewCronMonitor(1, "Nightly_Backup", everyMinute(t), "", 0, 0, true, now)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case monitor.Slug != "nightly_backup":
		t.Fatalf("slug = %q", monitor.Slug)
	case monitor.Timezone != "UTC":
		t.Fatalf("timezone = %q", monitor.Timezone)
	case monitor.CheckinMargin != domain.DefaultCheckinMargin:
		t.Fatalf("margin = %s", monitor.CheckinMargin)
	case monitor.MaxRuntime != domain.DefaultMaxRuntime:
		t.Fatalf("max runtime = %s", monitor.MaxRuntime)
	case monitor.Status != domain.CronUnknown:
		t.Fatalf("a brand-new monitor is %q, and must be unknown", monitor.Status)
	case len(monitor.PingKey) != domain.PingKeyBytes*2:
		t.Fatalf("ping key %q is not %d hex characters", monitor.PingKey, domain.PingKeyBytes*2)
	case monitor.NextExpectedAt == nil:
		t.Fatal("a new monitor has no deadline, so a job that never runs would never be missed")
	case !monitor.NextExpectedAt.After(now):
		t.Fatalf("the first deadline %s is not in the future", monitor.NextExpectedAt)
	}
}

func TestPingKeysAreNotPredictable(t *testing.T) {
	seen := map[string]bool{}
	for range 32 {
		key, err := domain.NewPingKey()
		if err != nil {
			t.Fatal(err)
		}
		if seen[key] {
			t.Fatalf("ping key %q was minted twice", key)
		}
		seen[key] = true
	}
}

func TestNewCronMonitorRejects(t *testing.T) {
	now := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	schedule := everyMinute(t)

	cases := []struct {
		name    string
		project int64
		slug    string
		zone    string
		margin  time.Duration
		runtime time.Duration
		kind    domain.ScheduleType
	}{
		{name: "no project", project: 0, slug: "backup"},
		{name: "no slug", project: 1, slug: "  "},
		{name: "slug with spaces", project: 1, slug: "nightly backup"},
		{name: "slug with a slash", project: 1, slug: "nightly/backup"},
		{name: "slug too long", project: 1, slug: strings.Repeat("a", 65)},
		{name: "fixed offset instead of a zone", project: 1, slug: "backup", zone: "-04:00"},
		{name: "unknown zone", project: 1, slug: "backup", zone: "Mars/Olympus"},
		{name: "negative margin", project: 1, slug: "backup", margin: -time.Second},
		{name: "margin over a day", project: 1, slug: "backup", margin: 25 * time.Hour},
		{name: "runtime over a day", project: 1, slug: "backup", runtime: 25 * time.Hour},
		{name: "negative runtime", project: 1, slug: "backup", runtime: -time.Second},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := domain.NewCronMonitor(testCase.project, testCase.slug, schedule,
				testCase.zone, testCase.margin, testCase.runtime, true, now)
			if !errors.Is(err, domain.ErrInvalidMonitor) {
				t.Fatalf("err = %v, want ErrInvalidMonitor", err)
			}
		})
	}
}

func TestNewCronMonitorRejectsAnUnknownScheduleType(t *testing.T) {
	_, err := domain.NewCronMonitor(1, "backup",
		domain.CronSpec{Type: domain.ScheduleType("guesswork")}, "", 0, 0, true, time.Now())
	if !errors.Is(err, domain.ErrInvalidMonitor) {
		t.Fatalf("err = %v", err)
	}
}

func TestNewCronMonitorRejectsAScheduleThatNeverHappens(t *testing.T) {
	never, err := domain.ParseCronSpec(domain.ScheduleCrontab, "0 0 30 2 *")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := domain.NewCronMonitor(1, "backup", never, "", 0, 0, true, time.Now()); err == nil {
		t.Fatal("a monitor was created for 30 February")
	}
}

func TestCronMonitorLocation(t *testing.T) {
	monitor := domain.CronMonitor{}
	loc, err := monitor.Location()
	if err != nil || loc != time.UTC {
		t.Fatalf("an empty timezone gave %v, %v", loc, err)
	}

	monitor.Timezone = "Mars/Olympus"
	if _, err := monitor.Location(); !errors.Is(err, domain.ErrInvalidMonitor) {
		t.Fatalf("err = %v", err)
	}
}

func TestCleanMonitorSlugAndCheckInID(t *testing.T) {
	if slug, err := domain.CleanMonitorSlug("  DAILY-Backup_1 "); err != nil || slug != "daily-backup_1" {
		t.Fatalf("slug = %q, err = %v", slug, err)
	}
	if id, err := domain.CleanCheckInID("  "); err != nil || id != "" {
		t.Fatalf("an empty check-in id is legitimate: %q, %v", id, err)
	}
	if _, err := domain.CleanCheckInID(strings.Repeat("a", 65)); !errors.Is(err, domain.ErrInvalidMonitor) {
		t.Fatalf("err = %v", err)
	}
	if _, err := domain.CleanCheckInID("a b"); !errors.Is(err, domain.ErrInvalidMonitor) {
		t.Fatalf("err = %v", err)
	}
}

func TestCronStatusVocabulary(t *testing.T) {
	for _, status := range domain.AllCronStatuses() {
		if !status.Valid() {
			t.Fatalf("%q is listed and not valid", status)
		}
	}
	if domain.CronStatus("green").Valid() {
		t.Fatal("an invented status validated")
	}
	for _, status := range []domain.CronStatus{domain.CronMissed, domain.CronTimeout, domain.CronError} {
		if !status.Failing() {
			t.Fatalf("%q is not reported as failing", status)
		}
	}
	for _, status := range []domain.CronStatus{domain.CronOK, domain.CronUnknown} {
		if status.Failing() {
			t.Fatalf("%q is reported as failing", status)
		}
	}
	for _, status := range domain.AllCheckInStatuses() {
		if !status.Valid() {
			t.Fatalf("%q is listed and not valid", status)
		}
	}
	if domain.CheckInStatus("done").Valid() {
		t.Fatal("an invented check-in status validated")
	}
	if !domain.ScheduleCrontab.Valid() || !domain.ScheduleInterval.Valid() {
		t.Fatal("a documented schedule type is not valid")
	}
	if domain.ScheduleType("sometimes").Valid() {
		t.Fatal("an invented schedule type validated")
	}
	if len(domain.IntervalUnits()) == 0 {
		t.Fatal("no interval units are offered")
	}
}

// The state rules, as a table. This is the half of the feature that decides
// whether somebody gets woken up, and it is a pure function precisely so that
// every case here costs microseconds instead of a real minute.
func TestApplyCheckIn(t *testing.T) {
	now := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		from        domain.CronStatus
		reported    domain.CheckInStatus
		wantStatus  domain.CronStatus
		wantTrigger domain.TriggerKind
	}{
		{"a start reports nothing", domain.CronOK, domain.CheckInProgress, domain.CronOK, ""},
		{"a start on a missed monitor still reports nothing", domain.CronMissed, domain.CheckInProgress, domain.CronMissed, ""},
		{"success on a healthy monitor is not news", domain.CronOK, domain.CheckInOK, domain.CronOK, ""},
		{"the first success is not a recovery", domain.CronUnknown, domain.CheckInOK, domain.CronOK, ""},
		{"success after missed is a recovery", domain.CronMissed, domain.CheckInOK, domain.CronOK, domain.TriggerCronRecovered},
		{"success after a timeout is a recovery", domain.CronTimeout, domain.CheckInOK, domain.CronOK, domain.TriggerCronRecovered},
		{"success after an error is a recovery", domain.CronError, domain.CheckInOK, domain.CronOK, domain.TriggerCronRecovered},
		{"a failure is reported", domain.CronOK, domain.CheckInError, domain.CronError, domain.TriggerCronFailed},
		{"a failure after a miss is still reported", domain.CronMissed, domain.CheckInError, domain.CronError, domain.TriggerCronFailed},
		{"a second failure is not", domain.CronError, domain.CheckInError, domain.CronError, ""},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			monitor, err := domain.NewCronMonitor(1, "backup", everyMinute(t), "", 0, 0, true, now)
			if err != nil {
				t.Fatal(err)
			}
			monitor.Status = testCase.from

			outcome, err := domain.ApplyCheckIn(&monitor, testCase.reported, now)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Status != testCase.wantStatus {
				t.Fatalf("status = %q, want %q", outcome.Status, testCase.wantStatus)
			}
			if outcome.Trigger != testCase.wantTrigger {
				t.Fatalf("trigger = %q, want %q", outcome.Trigger, testCase.wantTrigger)
			}
			if !outcome.NextExpectedAt.After(now) {
				t.Fatalf("the new deadline %s is not in the future", outcome.NextExpectedAt)
			}
			if outcome.Changed(testCase.from) != (testCase.from != testCase.wantStatus) {
				t.Fatal("Changed disagrees with the statuses")
			}
		})
	}
}

func TestApplyCheckInRejectsAnUnknownStatus(t *testing.T) {
	monitor, err := domain.NewCronMonitor(1, "backup", everyMinute(t), "", 0, 0, true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, err = domain.ApplyCheckIn(&monitor, domain.CheckInStatus("finished"), time.Now())
	if !errors.Is(err, domain.ErrInvalidMonitor) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "in_progress") {
		t.Fatalf("the error does not name the statuses that exist: %v", err)
	}
}

func TestApplyCheckInReportsABrokenTimezone(t *testing.T) {
	monitor := domain.CronMonitor{Timezone: "Mars/Olympus", Schedule: everyMinute(t)}
	if _, err := domain.ApplyCheckIn(&monitor, domain.CheckInOK, time.Now()); err == nil {
		t.Fatal("a monitor whose zone this machine does not have was evaluated anyway")
	}
	if _, _, err := domain.SweepCron(&monitor, nil, time.Now()); err == nil {
		t.Fatal("the sweep evaluated it too")
	}
}

func TestSweepCron(t *testing.T) {
	base := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	newMonitor := func(t *testing.T, status domain.CronStatus, due time.Time) domain.CronMonitor {
		t.Helper()
		monitor, err := domain.NewCronMonitor(1, "backup", everyMinute(t), "", 10*time.Second, 5*time.Second, true, base)
		if err != nil {
			t.Fatal(err)
		}
		monitor.Status = status
		deadline := due
		monitor.NextExpectedAt = &deadline
		return monitor
	}

	t.Run("nothing to do before the deadline", func(t *testing.T) {
		monitor := newMonitor(t, domain.CronOK, base.Add(time.Minute))
		_, changed, err := domain.SweepCron(&monitor, nil, base.Add(30*time.Second))
		if err != nil || changed {
			t.Fatalf("changed = %v, err = %v", changed, err)
		}
	})

	t.Run("nothing to do inside the margin", func(t *testing.T) {
		monitor := newMonitor(t, domain.CronOK, base)
		// Five seconds late, and the margin is ten.
		_, changed, err := domain.SweepCron(&monitor, nil, base.Add(5*time.Second))
		if err != nil || changed {
			t.Fatalf("changed = %v, err = %v", changed, err)
		}
	})

	t.Run("past the margin is missed, once", func(t *testing.T) {
		monitor := newMonitor(t, domain.CronOK, base)
		now := base.Add(30 * time.Second)
		outcome, changed, err := domain.SweepCron(&monitor, nil, now)
		if err != nil || !changed {
			t.Fatalf("changed = %v, err = %v", changed, err)
		}
		if outcome.Status != domain.CronMissed || outcome.Trigger != domain.TriggerCronMissed {
			t.Fatalf("got %+v", outcome)
		}
		if !outcome.NextExpectedAt.After(now) {
			t.Fatalf("the deadline did not move past now: %s", outcome.NextExpectedAt)
		}

		// The second sweep still moves the deadline — otherwise a monitor
		// that has been down all week is re-evaluated against a week-old
		// deadline every thirty seconds — but says nothing.
		monitor.Status = outcome.Status
		monitor.NextExpectedAt = &outcome.NextExpectedAt
		later := outcome.NextExpectedAt.Add(time.Minute)
		second, changed, err := domain.SweepCron(&monitor, nil, later)
		if err != nil || !changed {
			t.Fatalf("changed = %v, err = %v", changed, err)
		}
		if second.Trigger != "" {
			t.Fatalf("a monitor that is already missed reported %q again", second.Trigger)
		}
	})

	t.Run("an unfinished run past its runtime is a timeout", func(t *testing.T) {
		monitor := newMonitor(t, domain.CronOK, base.Add(time.Hour))
		started := base
		now := base.Add(30 * time.Second) // max runtime is five seconds
		outcome, changed, err := domain.SweepCron(&monitor, &started, now)
		if err != nil || !changed {
			t.Fatalf("changed = %v, err = %v", changed, err)
		}
		if outcome.Status != domain.CronTimeout || outcome.Trigger != domain.TriggerCronTimeout {
			t.Fatalf("got %+v", outcome)
		}

		// And it is not reported twice.
		monitor.Status = domain.CronTimeout
		_, changed, err = domain.SweepCron(&monitor, &started, now.Add(time.Second))
		if err != nil || changed {
			t.Fatalf("a timeout was reported twice: changed = %v, err = %v", changed, err)
		}
	})

	t.Run("a run inside its runtime is left alone", func(t *testing.T) {
		monitor := newMonitor(t, domain.CronOK, base.Add(time.Hour))
		started := base
		_, changed, err := domain.SweepCron(&monitor, &started, base.Add(2*time.Second))
		if err != nil || changed {
			t.Fatalf("changed = %v, err = %v", changed, err)
		}
	})

	t.Run("a timeout outranks a missed deadline", func(t *testing.T) {
		// Both conditions hold at once. Reporting "missed" would send
		// somebody looking for a job that never started, when in fact it
		// started and hung.
		monitor := newMonitor(t, domain.CronOK, base)
		started := base
		outcome, changed, err := domain.SweepCron(&monitor, &started, base.Add(time.Minute))
		if err != nil || !changed {
			t.Fatalf("changed = %v, err = %v", changed, err)
		}
		if outcome.Status != domain.CronTimeout {
			t.Fatalf("status = %q, want timeout", outcome.Status)
		}
	})

	t.Run("a monitor with no deadline is left alone", func(t *testing.T) {
		monitor := newMonitor(t, domain.CronUnknown, base)
		monitor.NextExpectedAt = nil
		_, changed, err := domain.SweepCron(&monitor, nil, base.Add(time.Hour))
		if err != nil || changed {
			t.Fatalf("changed = %v, err = %v", changed, err)
		}
	})
}

func TestMonitorSubjectKeyIsItsOwnNamespace(t *testing.T) {
	event := domain.AlertEvent{
		ProjectID: 1, MonitorKind: domain.MonitorKindCron, MonitorID: 7, MonitorSlug: "backup",
	}
	if event.SubjectKey() != "monitor:cron:7" {
		t.Fatalf("subject = %q", event.SubjectKey())
	}
	// The kind is in the key because the two families number their monitors
	// from separate tables. Without it, an uptime monitor going down would
	// silence a cron monitor that happens to share its id — the exact bug the
	// silence window exists to avoid, delivered by the fix for it.
	sameID := domain.AlertEvent{ProjectID: 1, MonitorKind: domain.MonitorKindUptime, MonitorID: 7}
	if sameID.SubjectKey() == event.SubjectKey() {
		t.Fatalf("a cron and an uptime monitor share the subject %q", event.SubjectKey())
	}
	// An issue still wins, because a monitor event never carries one.
	event.IssueID = 3
	if event.SubjectKey() != "issue:3" {
		t.Fatalf("subject = %q", event.SubjectKey())
	}
}

func TestMonitorTriggersCarryNoParameters(t *testing.T) {
	for _, kind := range domain.MonitorTriggerKinds() {
		trigger := domain.Trigger{Kind: kind}
		if err := trigger.Validate(); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		withParameter := domain.Trigger{Kind: kind, WindowSeconds: 60}
		if err := withParameter.Validate(); err == nil {
			t.Fatalf("%s accepted a window it will never read", kind)
		}

		rule, err := domain.NewAlertRule(nil, "monitors", trigger, []int64{1}, 0, true)
		if err != nil {
			t.Fatal(err)
		}
		if !rule.Matches(&domain.AlertEvent{Kind: kind, ProjectID: 1, MonitorID: 2}) {
			t.Fatalf("a %s rule did not match its own event", kind)
		}
	}
}

func TestMonitorAlertPayloadNamesTheMonitor(t *testing.T) {
	payload := domain.NewAlertPayload("backups", &domain.AlertEvent{
		Kind:        domain.TriggerCronMissed,
		ProjectID:   1,
		MonitorID:   7,
		MonitorSlug: "nightly-backup",
		Title:       "nightly-backup has not checked in",
		At:          time.Now(),
	}, "https://errors.example.test/projects/1/monitors/7")

	if payload.Monitor != "nightly-backup" || payload.MonitorID != 7 {
		t.Fatalf("payload = %+v", payload)
	}
	if !strings.Contains(payload.Headline(), "Cron missed") {
		t.Fatalf("headline = %q", payload.Headline())
	}
	if !strings.Contains(payload.Text(), "monitor: nightly-backup") {
		t.Fatalf("text = %q", payload.Text())
	}
	for _, kind := range domain.MonitorTriggerKinds() {
		other := payload
		other.Event = kind
		if strings.HasPrefix(other.Headline(), string(kind)) {
			t.Fatalf("%s has no headline of its own", kind)
		}
	}
}

func TestMonitorURL(t *testing.T) {
	origin, err := domain.ParseOrigin("https://errors.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if got := origin.MonitorURL(1, domain.MonitorKindCron, 7); got != "https://errors.example.test/projects/1/monitors/cron/7" {
		t.Fatalf("MonitorURL = %q", got)
	}
}
