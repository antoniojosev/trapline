package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/adapters/secrets"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// cronFixture is a repository, a project and a monitor over a real database.
// No mocks: an adapter is tested against the store it adapts (ADR 009).
func cronFixture(t *testing.T, schedule string) (*CronRepository, domain.CronMonitor) {
	t.Helper()
	db := openTemp(t)
	project, _ := createProject(t, NewProjectRepository(db), "venekambio")

	repo := NewCronRepository(db)
	spec, err := domain.ParseCronSpec(domain.ScheduleCrontab, schedule)
	if err != nil {
		t.Fatalf("parsing the schedule: %v", err)
	}
	monitor, err := domain.NewCronMonitor(project.ID, "nightly-backup", spec, "America/Caracas",
		10*time.Second, 5*time.Second, true, testNow)
	if err != nil {
		t.Fatalf("building the monitor: %v", err)
	}
	stored, err := repo.CreateMonitor(context.Background(), &monitor)
	if err != nil {
		t.Fatalf("creating the monitor: %v", err)
	}
	return repo, stored
}

func TestAMonitorRoundTrips(t *testing.T) {
	repo, monitor := cronFixture(t, "0 3 * * *")
	ctx := context.Background()

	read, err := repo.FindMonitor(ctx, monitor.ID)
	if err != nil {
		t.Fatalf("finding: %v", err)
	}
	switch {
	case read.Slug != "nightly-backup":
		t.Fatalf("slug = %q", read.Slug)
	case read.Schedule.String() != "0 3 * * *":
		t.Fatalf("schedule = %q", read.Schedule.String())
	case read.Schedule.Type != domain.ScheduleCrontab:
		t.Fatalf("schedule type = %q", read.Schedule.Type)
	case read.Timezone != "America/Caracas":
		t.Fatalf("timezone = %q", read.Timezone)
	case read.CheckinMargin != 10*time.Second:
		t.Fatalf("margin = %s", read.CheckinMargin)
	case read.MaxRuntime != 5*time.Second:
		t.Fatalf("max runtime = %s", read.MaxRuntime)
	case read.Status != domain.CronUnknown:
		t.Fatalf("status = %q", read.Status)
	case read.NextExpectedAt == nil:
		t.Fatal("no deadline was stored")
	case !read.Enabled:
		t.Fatal("the monitor came back disabled")
	}

	bySlug, err := repo.FindMonitorBySlug(ctx, monitor.ProjectID, "nightly-backup")
	if err != nil || bySlug.ID != monitor.ID {
		t.Fatalf("by slug: %+v, %v", bySlug, err)
	}
	byKey, err := repo.FindMonitorByPingKey(ctx, monitor.PingKey)
	if err != nil || byKey.ID != monitor.ID {
		t.Fatalf("by ping key: %+v, %v", byKey, err)
	}

	monitors, err := repo.ListMonitors(ctx, monitor.ProjectID)
	if err != nil || len(monitors) != 1 {
		t.Fatalf("listing returned %d monitors: %v", len(monitors), err)
	}
}

func TestAnIntervalScheduleRoundTrips(t *testing.T) {
	db := openTemp(t)
	project, _ := createProject(t, NewProjectRepository(db), "venekambio")
	repo := NewCronRepository(db)
	ctx := context.Background()

	spec, err := domain.ParseCronSpec(domain.ScheduleInterval, "5 minutes")
	if err != nil {
		t.Fatal(err)
	}
	monitor, err := domain.NewCronMonitor(project.ID, "poller", spec, "", 0, 0, true, testNow)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := repo.CreateMonitor(ctx, &monitor)
	if err != nil {
		t.Fatal(err)
	}

	read, err := repo.FindMonitor(ctx, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if read.Schedule.Type != domain.ScheduleInterval || read.Schedule.String() != "5 minute" {
		t.Fatalf("schedule came back as %q (%s)", read.Schedule.String(), read.Schedule.Type)
	}
}

func TestMissingMonitorsAreNotFound(t *testing.T) {
	repo, monitor := cronFixture(t, "@daily")
	ctx := context.Background()

	for name, call := range map[string]func() error{
		"by id":       func() error { _, err := repo.FindMonitor(ctx, 9999); return err },
		"by slug":     func() error { _, err := repo.FindMonitorBySlug(ctx, monitor.ProjectID, "nope"); return err },
		"by ping key": func() error { _, err := repo.FindMonitorByPingKey(ctx, "nope"); return err },
		"delete":      func() error { return repo.DeleteMonitor(ctx, 9999) },
		"update": func() error {
			ghost := monitor
			ghost.ID = 9999
			_, err := repo.UpdateMonitor(ctx, &ghost)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, domain.ErrMonitorNotFound) {
				t.Fatalf("err = %v, want ErrMonitorNotFound", err)
			}
		})
	}
}

func TestASlugIsUniquePerProject(t *testing.T) {
	repo, monitor := cronFixture(t, "@daily")
	second := monitor
	second.ID = 0
	key, err := domain.NewPingKey()
	if err != nil {
		t.Fatal(err)
	}
	second.PingKey = key

	if _, err := repo.CreateMonitor(context.Background(), &second); !errors.Is(err, domain.ErrInvalidMonitor) {
		t.Fatalf("err = %v, want a duplicate slug to be a domain error", err)
	}
}

func TestHasEnabledMonitorsIsTheSchedulersQuestion(t *testing.T) {
	repo, monitor := cronFixture(t, "@daily")
	ctx := context.Background()

	has, err := repo.HasEnabledMonitors(ctx)
	if err != nil || !has {
		t.Fatalf("has = %v, err = %v", has, err)
	}

	monitor.Enabled = false
	if _, err := repo.UpdateMonitor(ctx, &monitor); err != nil {
		t.Fatal(err)
	}
	has, err = repo.HasEnabledMonitors(ctx)
	if err != nil || has {
		t.Fatalf("a disabled monitor still counts as work: has = %v, err = %v", has, err)
	}
}

// A start and its finish are one row, not two: the timeout case is the
// *absence* of the second write, so the first has to be a row to be closed.
func TestAFinishClosesTheRunItBelongsTo(t *testing.T) {
	repo, monitor := cronFixture(t, "*/1 * * * *")
	ctx := context.Background()
	started := testNow

	if _, err := repo.RecordCheckIn(ctx, &ports.CheckInWrite{
		Monitor: monitor, CheckInID: "run-1", Status: domain.CheckInProgress, At: started,
		Outcome: domain.CronOutcome{Status: domain.CronUnknown, NextExpectedAt: started.Add(time.Minute)},
	}); err != nil {
		t.Fatalf("recording a start: %v", err)
	}

	finished := started.Add(1500 * time.Millisecond)
	closed, err := repo.RecordCheckIn(ctx, &ports.CheckInWrite{
		Monitor: monitor, CheckInID: "run-1", Status: domain.CheckInOK, At: finished,
		Environment: "production",
		Outcome:     domain.CronOutcome{Status: domain.CronOK, NextExpectedAt: finished.Add(time.Minute)},
	})
	if err != nil {
		t.Fatalf("recording a finish: %v", err)
	}
	if closed.FinishedAt == nil || closed.DurationMS != 1500 {
		t.Fatalf("closed run = %+v, want a 1500ms duration", closed)
	}

	checkIns, err := repo.ListCheckIns(ctx, monitor.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(checkIns) != 1 {
		t.Fatalf("the history has %d rows, want the start and its finish to be one", len(checkIns))
	}

	read, err := repo.FindMonitor(ctx, monitor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if read.Status != domain.CronOK || read.LastCheckinAt == nil {
		t.Fatalf("the monitor did not move: %+v", read)
	}
}

// A ping has no correlation id, so a finish matches the oldest run still open.
func TestAFinishWithoutAnIDClosesTheOldestOpenRun(t *testing.T) {
	repo, monitor := cronFixture(t, "*/1 * * * *")
	ctx := context.Background()

	if _, err := repo.RecordCheckIn(ctx, &ports.CheckInWrite{
		Monitor: monitor, Status: domain.CheckInProgress, At: testNow,
		Outcome: domain.CronOutcome{Status: domain.CronUnknown, NextExpectedAt: testNow.Add(time.Minute)},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RecordCheckIn(ctx, &ports.CheckInWrite{
		Monitor: monitor, Status: domain.CheckInOK, At: testNow.Add(time.Second),
		Outcome: domain.CronOutcome{Status: domain.CronOK, NextExpectedAt: testNow.Add(time.Minute)},
	}); err != nil {
		t.Fatal(err)
	}

	checkIns, err := repo.ListCheckIns(ctx, monitor.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(checkIns) != 1 || checkIns[0].Status != domain.CheckInOK {
		t.Fatalf("history = %+v", checkIns)
	}
}

// And a bare ping with nothing open is a run of its own, recorded rather than
// invented: the commonest shape there is, a curl at the end of a crontab line.
func TestAPingWithNothingOpenIsItsOwnRun(t *testing.T) {
	repo, monitor := cronFixture(t, "*/1 * * * *")
	ctx := context.Background()

	checkIn, err := repo.RecordCheckIn(ctx, &ports.CheckInWrite{
		Monitor: monitor, Status: domain.CheckInOK, At: testNow,
		Outcome: domain.CronOutcome{Status: domain.CronOK, NextExpectedAt: testNow.Add(time.Minute)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if checkIn.FinishedAt == nil || !checkIn.StartedAt.Equal(*checkIn.FinishedAt) {
		t.Fatalf("check-in = %+v, want an instantaneous run", checkIn)
	}
}

func TestDueMonitorsCarriesTheOpenRun(t *testing.T) {
	repo, monitor := cronFixture(t, "*/1 * * * *")
	ctx := context.Background()

	// Nothing is due before the deadline.
	due, err := repo.DueMonitors(ctx, testNow, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("%d monitors are due before their deadline", len(due))
	}

	// Past the deadline it is.
	due, err = repo.DueMonitors(ctx, monitor.NextExpectedAt.Add(time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].Monitor.ID != monitor.ID || due[0].OpenSince != nil {
		t.Fatalf("due = %+v", due)
	}

	// And a run that started and never finished makes it due whatever its
	// deadline says, because a timeout is judged from the run and not from
	// the schedule.
	if _, err := repo.RecordCheckIn(ctx, &ports.CheckInWrite{
		Monitor: monitor, Status: domain.CheckInProgress, At: testNow,
		Outcome: domain.CronOutcome{Status: domain.CronUnknown, NextExpectedAt: testNow.Add(time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	due, err = repo.DueMonitors(ctx, testNow.Add(time.Second), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].OpenSince == nil || !due[0].OpenSince.Equal(testNow.UTC()) {
		t.Fatalf("due = %+v, want the open run's start", due)
	}

	// A disabled monitor is not watched at all.
	disabled := monitor
	disabled.Enabled = false
	if _, err := repo.UpdateMonitor(ctx, &disabled); err != nil {
		t.Fatal(err)
	}
	due, err = repo.DueMonitors(ctx, testNow.Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("a disabled monitor is still being swept: %+v", due)
	}
}

// A sweep moves the status and the deadline, and deliberately does not touch
// last_checkin_at: "when did this last actually run" must not answer with the
// moment somebody noticed it had not.
func TestApplySweepDoesNotForgeACheckIn(t *testing.T) {
	repo, monitor := cronFixture(t, "*/1 * * * *")
	ctx := context.Background()
	later := testNow.Add(time.Hour)

	if err := repo.ApplySweep(ctx, &ports.SweepWrite{
		MonitorID: monitor.ID,
		Outcome:   domain.CronOutcome{Status: domain.CronMissed, NextExpectedAt: later},
	}); err != nil {
		t.Fatal(err)
	}

	read, err := repo.FindMonitor(ctx, monitor.ID)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case read.Status != domain.CronMissed:
		t.Fatalf("status = %q", read.Status)
	case read.LastCheckinAt != nil:
		t.Fatal("a sweep recorded a check-in nobody sent")
	case !read.NextExpectedAt.Equal(later.UTC()):
		t.Fatalf("deadline = %s", read.NextExpectedAt)
	}
}

func TestDeletingAMonitorTakesItsHistory(t *testing.T) {
	repo, monitor := cronFixture(t, "*/1 * * * *")
	ctx := context.Background()

	if _, err := repo.RecordCheckIn(ctx, &ports.CheckInWrite{
		Monitor: monitor, Status: domain.CheckInOK, At: testNow,
		Outcome: domain.CronOutcome{Status: domain.CronOK, NextExpectedAt: testNow.Add(time.Minute)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteMonitor(ctx, monitor.ID); err != nil {
		t.Fatal(err)
	}
	checkIns, err := repo.ListCheckIns(ctx, monitor.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(checkIns) != 0 {
		t.Fatalf("%d check-ins outlived their monitor", len(checkIns))
	}
}

func TestPruningDropsOldCheckIns(t *testing.T) {
	repo, monitor := cronFixture(t, "*/1 * * * *")
	ctx := context.Background()

	old := testNow.Add(-200 * 24 * time.Hour)
	for _, at := range []time.Time{old, testNow} {
		if _, err := repo.RecordCheckIn(ctx, &ports.CheckInWrite{
			Monitor: monitor, Status: domain.CheckInOK, At: at,
			Outcome: domain.CronOutcome{Status: domain.CronOK, NextExpectedAt: at.Add(time.Minute)},
		}); err != nil {
			t.Fatal(err)
		}
	}

	deleted, err := repo.PruneCheckIns(ctx, testNow.Add(-90*24*time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("pruned %d rows, want 1", deleted)
	}
	checkIns, err := repo.ListCheckIns(ctx, monitor.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(checkIns) != 1 {
		t.Fatalf("%d rows survived", len(checkIns))
	}
}

func TestCheckInListingIsBounded(t *testing.T) {
	repo, monitor := cronFixture(t, "*/1 * * * *")
	ctx := context.Background()

	for index := range 5 {
		at := testNow.Add(time.Duration(index) * time.Minute)
		if _, err := repo.RecordCheckIn(ctx, &ports.CheckInWrite{
			Monitor: monitor, Status: domain.CheckInOK, At: at,
			Outcome: domain.CronOutcome{Status: domain.CronOK, NextExpectedAt: at.Add(time.Minute)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	checkIns, err := repo.ListCheckIns(ctx, monitor.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(checkIns) != 2 {
		t.Fatalf("asked for 2, got %d", len(checkIns))
	}
	// Newest first.
	if !checkIns[0].StartedAt.After(checkIns[1].StartedAt) {
		t.Fatal("the history is not newest first")
	}

	// And a caller asking for more than the ceiling gets the ceiling, not an
	// unbounded scan.
	if _, err := repo.ListCheckIns(ctx, monitor.ID, MaxCheckInPageSize+1000); err != nil {
		t.Fatal(err)
	}
}

// The half of ADR 015 that only a real transaction can prove: the
// notification about a monitor and the status change it is about are written
// by one commit, so there is no window in which a backup is declared missed
// and nobody has been queued to hear about it.
func TestASweepQueuesItsNotificationInTheSameTransaction(t *testing.T) {
	db := openTemp(t)
	project, _ := createProject(t, NewProjectRepository(db), "venekambio")
	alerts := NewAlertRepository(db, secrets.At(filepath.Join(t.TempDir(), "trapline.db.key")))
	origin, err := domain.ParseOrigin("https://errors.example.test")
	if err != nil {
		t.Fatal(err)
	}
	repo := NewCronRepository(db).WithAlerts(alerts, origin)
	ctx := context.Background()

	channel := webhookChannel(t, alerts, "ops")
	addRule(t, alerts, &project.ID, `{"kind":"cron_missed"}`, []int64{channel.ID}, time.Hour)

	spec, err := domain.ParseCronSpec(domain.ScheduleCrontab, "*/1 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	monitor, err := domain.NewCronMonitor(project.ID, "nightly-backup", spec, "", 0, 0, true, alertNow)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := repo.CreateMonitor(ctx, &monitor)
	if err != nil {
		t.Fatal(err)
	}

	if err := repo.ApplySweep(ctx, &ports.SweepWrite{
		MonitorID: stored.ID,
		Outcome: domain.CronOutcome{
			Status: domain.CronMissed, NextExpectedAt: alertNow.Add(time.Minute),
			Trigger: domain.TriggerCronMissed,
		},
		Event: &domain.AlertEvent{
			Kind: domain.TriggerCronMissed, ProjectID: project.ID,
			MonitorKind: domain.MonitorKindCron,
			MonitorID:   stored.ID, MonitorSlug: stored.Slug,
			Title: "nightly-backup did not check in", At: alertNow,
		},
	}); err != nil {
		t.Fatal(err)
	}

	queued, err := alerts.ListNotifications(ctx, ports.NotificationFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 {
		t.Fatalf("%d notifications were queued, want 1", len(queued))
	}
	payload := queued[0].Payload
	switch {
	case payload.Event != domain.TriggerCronMissed:
		t.Fatalf("event = %q", payload.Event)
	case payload.Monitor != "nightly-backup" || payload.MonitorID != stored.ID:
		t.Fatalf("the payload does not name the monitor: %+v", payload)
	case queued[0].SubjectKey != domain.MonitorSubjectKey(domain.MonitorKindCron, stored.ID):
		t.Fatalf("subject = %q, want its own namespace", queued[0].SubjectKey)
	case payload.URL != origin.MonitorURL(project.ID, domain.MonitorKindCron, stored.ID):
		t.Fatalf("url = %q, want a link to the monitor", payload.URL)
	}

	// And the silence window is per monitor: a second sweep inside it queues
	// nothing, so a job that has been down all week does not send a message
	// every thirty seconds.
	if err := repo.ApplySweep(ctx, &ports.SweepWrite{
		MonitorID: stored.ID,
		Outcome:   domain.CronOutcome{Status: domain.CronMissed, NextExpectedAt: alertNow.Add(2 * time.Minute)},
		Event: &domain.AlertEvent{
			Kind: domain.TriggerCronMissed, ProjectID: project.ID,
			MonitorKind: domain.MonitorKindCron,
			MonitorID:   stored.ID, MonitorSlug: stored.Slug, At: alertNow.Add(time.Minute),
		},
	}); err != nil {
		t.Fatal(err)
	}
	queued, err = alerts.ListNotifications(ctx, ports.NotificationFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 {
		t.Fatalf("the silence window did not hold: %d notifications", len(queued))
	}
}
