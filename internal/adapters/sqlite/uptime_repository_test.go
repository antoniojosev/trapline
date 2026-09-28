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

// Against a real SQLite file, never a mock (ADR 009). The transaction that
// writes a check, its aggregate, the monitor's new state and the notification
// about it either exists or it does not, and a mock would happily pretend.

func openUptimeDB(t *testing.T) *DB {
	t.Helper()
	return openTemp(t)
}

func newProjectFor(t *testing.T, db *DB) int64 {
	t.Helper()
	project, _ := createProject(t, NewProjectRepository(db), "venekambio")
	return project.ID
}

func storedMonitor(t *testing.T, repo *UptimeRepository, projectID int64, at time.Time) domain.UptimeMonitor {
	t.Helper()
	monitor, err := domain.NewUptimeMonitor(&domain.UptimeMonitor{
		ProjectID: projectID,
		Name:      "api",
		URL:       "https://api.example.com/health",
		Method:    "GET",
		Enabled:   true,
	}, at)
	if err != nil {
		t.Fatalf("building a monitor: %v", err)
	}
	saved, err := repo.CreateMonitor(context.Background(), &monitor)
	if err != nil {
		t.Fatalf("storing a monitor: %v", err)
	}
	return saved
}

func TestUptimeMonitorRoundTrip(t *testing.T) {
	db := openUptimeDB(t)
	repo := NewUptimeRepository(db)
	ctx := context.Background()
	at := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	projectID := newProjectFor(t, db)
	saved := storedMonitor(t, repo, projectID, at)
	if saved.ID == 0 {
		t.Fatal("the monitor came back without an id")
	}

	read, err := repo.FindMonitor(ctx, saved.ID)
	if err != nil {
		t.Fatalf("FindMonitor: %v", err)
	}
	if read.Name != "api" || read.URL != saved.URL || read.Method != "GET" {
		t.Errorf("the monitor did not survive the round trip: %+v", read)
	}
	if read.Status != domain.UptimeUnknown || !read.Enabled {
		t.Errorf("status/enabled did not survive: %s %v", read.Status, read.Enabled)
	}
	if !read.NextCheckAt.Equal(at) {
		t.Errorf("next check = %s, want %s", read.NextCheckAt, at)
	}
	if read.LastCheckedAt != nil || read.LastStatusChangeAt != nil {
		t.Error("a monitor that has never been checked came back with timestamps")
	}

	monitors, err := repo.ListMonitors(ctx, &projectID)
	if err != nil {
		t.Fatalf("ListMonitors: %v", err)
	}
	if len(monitors) != 1 {
		t.Fatalf("listed %d monitors, want 1", len(monitors))
	}

	all, err := repo.ListMonitors(ctx, nil)
	if err != nil {
		t.Fatalf("ListMonitors(nil): %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("listing every monitor returned %d", len(all))
	}

	if err := repo.DeleteMonitor(ctx, saved.ID); err != nil {
		t.Fatalf("DeleteMonitor: %v", err)
	}
	if _, err := repo.FindMonitor(ctx, saved.ID); !errors.Is(err, domain.ErrMonitorNotFound) {
		t.Errorf("a deleted monitor reads back as %v", err)
	}
	if err := repo.DeleteMonitor(ctx, saved.ID); !errors.Is(err, domain.ErrMonitorNotFound) {
		t.Errorf("deleting a monitor twice reported %v", err)
	}
}

func TestUptimeMonitorNotFound(t *testing.T) {
	repo := NewUptimeRepository(openUptimeDB(t))
	if _, err := repo.FindMonitor(context.Background(), 404); !errors.Is(err, domain.ErrMonitorNotFound) {
		t.Errorf("FindMonitor on nothing = %v", err)
	}
}

// TestHasEnabledMonitors is the scheduler's question: with nothing enabled the
// job does not exist at all (ADR 014).
func TestHasEnabledMonitors(t *testing.T) {
	db := openUptimeDB(t)
	repo := NewUptimeRepository(db)
	ctx := context.Background()
	at := time.Now().UTC()

	has, err := repo.HasEnabledMonitors(ctx)
	if err != nil {
		t.Fatalf("HasEnabledMonitors: %v", err)
	}
	if has {
		t.Fatal("an empty installation reports monitors to check")
	}

	projectID := newProjectFor(t, db)
	saved := storedMonitor(t, repo, projectID, at)
	if has, _ = repo.HasEnabledMonitors(ctx); !has {
		t.Fatal("an enabled monitor is not seen")
	}

	if _, err := repo.SetMonitorEnabled(ctx, saved.ID, false); err != nil {
		t.Fatalf("SetMonitorEnabled: %v", err)
	}
	if has, _ = repo.HasEnabledMonitors(ctx); has {
		t.Fatal("a disabled monitor still keeps the job alive")
	}

	// Switching it back on makes it due now rather than at whatever moment
	// the schedule it was switched off on would have reached.
	back, err := repo.SetMonitorEnabled(ctx, saved.ID, true)
	if err != nil {
		t.Fatalf("SetMonitorEnabled: %v", err)
	}
	if !back.Enabled {
		t.Error("the monitor did not come back enabled")
	}
	if back.NextCheckAt.Before(at) {
		t.Errorf("a re-enabled monitor is due at %s, before it was switched on", back.NextCheckAt)
	}

	if _, err := repo.SetMonitorEnabled(ctx, 999, true); !errors.Is(err, domain.ErrMonitorNotFound) {
		t.Errorf("switching a monitor that does not exist = %v", err)
	}
}

// TestDueMonitorsLeases is the property that lets several passes overlap
// safely: reading a due monitor pushes its next check forward, so a second
// pass cannot take the same one.
func TestDueMonitorsLeases(t *testing.T) {
	db := openUptimeDB(t)
	repo := NewUptimeRepository(db)
	ctx := context.Background()
	at := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	projectID := newProjectFor(t, db)
	saved := storedMonitor(t, repo, projectID, at)

	first, err := repo.DueMonitors(ctx, at, 10)
	if err != nil {
		t.Fatalf("DueMonitors: %v", err)
	}
	if len(first) != 1 || first[0].ID != saved.ID {
		t.Fatalf("the due monitor was not leased: %+v", first)
	}

	second, err := repo.DueMonitors(ctx, at, 10)
	if err != nil {
		t.Fatalf("DueMonitors: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("a second pass leased %d monitors that were already taken", len(second))
	}

	// And it becomes due again on its own schedule, which is what makes a
	// pass that died mid-check harmless.
	later, err := repo.DueMonitors(ctx, at.Add(saved.Interval()), 10)
	if err != nil {
		t.Fatalf("DueMonitors: %v", err)
	}
	if len(later) != 1 {
		t.Fatalf("the monitor never became due again")
	}
}

func TestDueMonitorsSkipsDisabled(t *testing.T) {
	db := openUptimeDB(t)
	repo := NewUptimeRepository(db)
	ctx := context.Background()
	at := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	projectID := newProjectFor(t, db)
	saved := storedMonitor(t, repo, projectID, at)
	if _, err := repo.SetMonitorEnabled(ctx, saved.ID, false); err != nil {
		t.Fatalf("SetMonitorEnabled: %v", err)
	}

	due, err := repo.DueMonitors(ctx, at.Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("DueMonitors: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("a disabled monitor was leased for checking")
	}
}

func TestRecordResultWritesStateAndHistory(t *testing.T) {
	db := openUptimeDB(t)
	repo := NewUptimeRepository(db)
	ctx := context.Background()
	at := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	projectID := newProjectFor(t, db)
	monitor := storedMonitor(t, repo, projectID, at)

	// Up, then two failures, then back.
	steps := []struct {
		ok   bool
		when time.Time
	}{
		{true, at},
		{false, at.Add(time.Minute)},
		{false, at.Add(2 * time.Minute)},
		{true, at.Add(3 * time.Minute)},
	}
	for _, step := range steps {
		result := domain.CheckResult{At: step.when, OK: step.ok, StatusCode: 200, LatencyMS: 20}
		if !step.ok {
			result.StatusCode, result.Error = 503, "status 503 is outside the expected 200–299"
		}
		after, transition := monitor.Apply(result)
		if _, err := repo.RecordResult(ctx, &monitor, &after, result, transition); err != nil {
			t.Fatalf("RecordResult: %v", err)
		}
		monitor = after
	}

	stored, err := repo.FindMonitor(ctx, monitor.ID)
	if err != nil {
		t.Fatalf("FindMonitor: %v", err)
	}
	if stored.Status != domain.UptimeUp {
		t.Errorf("status = %s, want up", stored.Status)
	}
	if stored.ConsecutiveFailures != 0 {
		t.Errorf("failures = %d after a recovery", stored.ConsecutiveFailures)
	}
	if stored.LastStatusChangeAt == nil || !stored.LastStatusChangeAt.Equal(at.Add(3*time.Minute)) {
		t.Errorf("the recovery was not timestamped: %v", stored.LastStatusChangeAt)
	}

	results, err := repo.Results(ctx, monitor.ID, 10)
	if err != nil {
		t.Fatalf("Results: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("stored %d results, want 4", len(results))
	}
	// Newest first, which is the order anything showing them wants.
	if !results[0].At.Equal(at.Add(3 * time.Minute)) {
		t.Errorf("results are not newest first: %s", results[0].At)
	}
	if results[1].Error == "" {
		t.Error("a failed check stored no reason, which is the only thing that makes it fixable")
	}
}

// TestRecordResultRollsUpTheDay is the aggregate the status page reads: ninety days of a
// bar chart must be ninety rows, not a scan of every check (ADR 001, ADR 017).
func TestRecordResultRollsUpTheDay(t *testing.T) {
	db := openUptimeDB(t)
	repo := NewUptimeRepository(db)
	ctx := context.Background()
	at := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	projectID := newProjectFor(t, db)
	monitor := storedMonitor(t, repo, projectID, at)

	// Three checks today, one of them failed; one check yesterday.
	for index, step := range []struct {
		ok      bool
		when    time.Time
		latency int
	}{
		{true, at.Add(-24 * time.Hour), 10},
		{true, at, 20},
		{false, at.Add(time.Minute), 30},
		{true, at.Add(2 * time.Minute), 40},
	} {
		result := domain.CheckResult{At: step.when, OK: step.ok, LatencyMS: step.latency}
		after, transition := monitor.Apply(result)
		if _, err := repo.RecordResult(ctx, &monitor, &after, result, transition); err != nil {
			t.Fatalf("RecordResult %d: %v", index, err)
		}
		monitor = after
	}

	days, err := repo.DailyUptime(ctx, monitor.ID, at.Add(-48*time.Hour), at)
	if err != nil {
		t.Fatalf("DailyUptime: %v", err)
	}
	if len(days) != 2 {
		t.Fatalf("rolled up into %d days, want 2", len(days))
	}
	// Oldest first.
	if domain.UptimeDayKey(days[0].Day) != "2026-08-28" {
		t.Errorf("first day is %s", domain.UptimeDayKey(days[0].Day))
	}
	today := days[1]
	if today.Checks != 3 || today.Failures != 1 {
		t.Errorf("today = %d checks, %d failures, want 3 and 1", today.Checks, today.Failures)
	}
	if today.LatencySum != 90 {
		t.Errorf("latency sum = %d, want 90", today.LatencySum)
	}
	if got := today.MeanLatencyMS(); got != 30 {
		t.Errorf("mean latency = %d, want 30", got)
	}
}

// TestRecordResultQueuesTheNotification is the guarantee the outbox rests on:
// the row that says a monitor went down and the notification about it are
// written by one transaction (ADR 015).
func TestRecordResultQueuesTheNotification(t *testing.T) {
	db := openUptimeDB(t)
	ctx := context.Background()
	at := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	alerts := NewAlertRepository(db, secrets.At(filepath.Join(t.TempDir(), "trapline.db.key")))
	origin := domain.Origin{Scheme: "https", Host: "errors.example.com"}
	repo := NewUptimeRepository(db).WithAlerts(alerts, origin)

	projectID := newProjectFor(t, db)
	monitor := storedMonitor(t, repo, projectID, at)

	channel := webhookChannel(t, alerts, "ops")
	rule, err := domain.NewAlertRule(nil, "anything down",
		domain.Trigger{Kind: domain.TriggerUptimeDown}, []int64{channel.ID}, time.Minute, true)
	if err != nil {
		t.Fatalf("building a rule: %v", err)
	}
	if _, err := alerts.CreateRule(ctx, rule); err != nil {
		t.Fatalf("creating a rule: %v", err)
	}

	// One failure is noise and must queue nothing.
	first := domain.CheckResult{At: at, StatusCode: 503, Error: "boom"}
	after, transition := monitor.Apply(first)
	queued, err := repo.RecordResult(ctx, &monitor, &after, first, transition)
	if err != nil {
		t.Fatalf("RecordResult: %v", err)
	}
	if queued != 0 {
		t.Fatalf("a single failure queued %d notifications", queued)
	}
	monitor = after

	// The second is the outage.
	second := domain.CheckResult{At: at.Add(time.Minute), StatusCode: 503, Error: "boom"}
	after, transition = monitor.Apply(second)
	if transition != domain.TransitionDown {
		t.Fatalf("transition = %q", transition)
	}
	queued, err = repo.RecordResult(ctx, &monitor, &after, second, transition)
	if err != nil {
		t.Fatalf("RecordResult: %v", err)
	}
	if queued != 1 {
		t.Fatalf("the outage queued %d notifications, want 1", queued)
	}

	notifications, err := alerts.ListNotifications(ctx, ports.NotificationFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListNotifications: %v", err)
	}
	if len(notifications) != 1 {
		t.Fatalf("the outbox holds %d rows, want 1", len(notifications))
	}
	payload := notifications[0].Payload
	if payload.Event != domain.TriggerUptimeDown {
		t.Errorf("event = %q", payload.Event)
	}
	if payload.MonitorID != monitor.ID {
		t.Errorf("the notification does not name the monitor: %d", payload.MonitorID)
	}
	if payload.URL != origin.MonitorURL(projectID, domain.MonitorKindUptime, monitor.ID) {
		t.Errorf("the notification carries no link to the monitor: %q", payload.URL)
	}
	if notifications[0].SubjectKey != monitor.SubjectKey() {
		t.Errorf("subject = %q, want %q", notifications[0].SubjectKey, monitor.SubjectKey())
	}
}

// TestRecordResultWithoutAlertsIsSilent: a repository assembled without the
// alerting collaboration records checks and notifies nobody, which is the
// truthful behaviour for an installation with no channels.
func TestRecordResultWithoutAlertsIsSilent(t *testing.T) {
	db := openUptimeDB(t)
	repo := NewUptimeRepository(db)
	ctx := context.Background()
	at := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	monitor := storedMonitor(t, repo, newProjectFor(t, db), at)
	monitor.Status = domain.UptimeUp
	monitor.ConsecutiveFailures = 1

	result := domain.CheckResult{At: at, Error: "boom"}
	after, transition := monitor.Apply(result)
	queued, err := repo.RecordResult(ctx, &monitor, &after, result, transition)
	if err != nil {
		t.Fatalf("RecordResult: %v", err)
	}
	if queued != 0 {
		t.Errorf("a repository with no alerting queued %d notifications", queued)
	}
}

func TestPruneUptimeHistory(t *testing.T) {
	db := openUptimeDB(t)
	repo := NewUptimeRepository(db)
	ctx := context.Background()
	at := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	monitor := storedMonitor(t, repo, newProjectFor(t, db), at)

	old := at.Add(-100 * 24 * time.Hour)
	for _, when := range []time.Time{old, old.Add(time.Minute), at} {
		result := domain.CheckResult{At: when, OK: true, LatencyMS: 5}
		after, transition := monitor.Apply(result)
		if _, err := repo.RecordResult(ctx, &monitor, &after, result, transition); err != nil {
			t.Fatalf("RecordResult: %v", err)
		}
		monitor = after
	}

	deleted, err := repo.PruneResults(ctx, at.Add(-90*24*time.Hour), 500)
	if err != nil {
		t.Fatalf("PruneResults: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("pruned %d results, want 2", deleted)
	}

	// The aggregate outlives the checks, which is the whole reason it exists:
	// after the sweep it is the only record that anything happened that day.
	days, err := repo.DailyUptime(ctx, monitor.ID, old.Add(-time.Hour), at)
	if err != nil {
		t.Fatalf("DailyUptime: %v", err)
	}
	if len(days) != 2 {
		t.Fatalf("the roll-up holds %d days after pruning the checks, want 2", len(days))
	}

	dropped, err := repo.PruneDaily(ctx, at.Add(-99*24*time.Hour), 500)
	if err != nil {
		t.Fatalf("PruneDaily: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("pruned %d aggregate rows, want 1", dropped)
	}
}

// TestDeletingAnUptimeMonitorTakesItsHistory: the cascade is declared in the schema,
// and a foreign key that is not enforced is a comment (db.go turns them on).
func TestDeletingAnUptimeMonitorTakesItsHistory(t *testing.T) {
	db := openUptimeDB(t)
	repo := NewUptimeRepository(db)
	ctx := context.Background()
	at := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	monitor := storedMonitor(t, repo, newProjectFor(t, db), at)
	result := domain.CheckResult{At: at, OK: true, LatencyMS: 5}
	after, transition := monitor.Apply(result)
	if _, err := repo.RecordResult(ctx, &monitor, &after, result, transition); err != nil {
		t.Fatalf("RecordResult: %v", err)
	}

	if err := repo.DeleteMonitor(ctx, monitor.ID); err != nil {
		t.Fatalf("DeleteMonitor: %v", err)
	}

	results, err := repo.Results(ctx, monitor.ID, 10)
	if err != nil {
		t.Fatalf("Results: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("%d checks outlived the monitor they belong to", len(results))
	}
	days, err := repo.DailyUptime(ctx, monitor.ID, at.Add(-time.Hour), at)
	if err != nil {
		t.Fatalf("DailyUptime: %v", err)
	}
	if len(days) != 0 {
		t.Errorf("%d aggregate rows outlived their monitor", len(days))
	}
}
