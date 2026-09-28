package sqlite

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/adapters/secrets"
	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// newAlertRepo builds the pairing under test: the alert repository, the issue
// repository that enqueues through it inside the ingest transaction, and a
// project for both to talk about.
//
// A real database, as ADR 009 requires: what is being verified is a
// transaction, a savepoint and an index, and a mock of SQLite is a mock of the
// thing that could be wrong.
func newAlertRepo(t *testing.T) (*AlertRepository, *IssueRepository, int64) {
	t.Helper()
	db := openTemp(t)
	project, _ := createProject(t, NewProjectRepository(db), "venekambio")
	cipher := secrets.At(filepath.Join(t.TempDir(), "trapline.db.key"))
	alerts := NewAlertRepository(db, cipher)
	origin, err := domain.ParseOrigin("https://errors.example.test")
	if err != nil {
		t.Fatalf("the fixture origin does not parse: %v", err)
	}
	return alerts, NewIssueRepository(db).WithAlerts(alerts, origin), project.ID
}

var alertNow = time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

func webhookChannel(t *testing.T, repo *AlertRepository, name string) domain.AlertChannel {
	t.Helper()
	channel, err := domain.NewAlertChannel(domain.ChannelWebhook, name, domain.ChannelConfig{
		URL: "https://example.test/hook", Secret: "a-secret-long-enough",
	}, false, alertNow)
	if err != nil {
		t.Fatalf("building the channel: %v", err)
	}
	saved, err := repo.CreateChannel(context.Background(), &channel)
	if err != nil {
		t.Fatalf("creating the channel: %v", err)
	}
	return saved
}

func addRule(t *testing.T, repo *AlertRepository, projectID *int64, triggerJSON string,
	channels []int64, silence time.Duration) domain.AlertRule {
	t.Helper()
	trigger, err := domain.ParseTrigger([]byte(triggerJSON))
	if err != nil {
		t.Fatalf("the fixture trigger does not parse: %v", err)
	}
	rule, err := domain.NewAlertRule(projectID, "a rule", trigger, channels, silence, true)
	if err != nil {
		t.Fatalf("building the rule: %v", err)
	}
	saved, err := repo.CreateRule(context.Background(), rule)
	if err != nil {
		t.Fatalf("creating the rule: %v", err)
	}
	return saved
}

// TestChannelCredentialsAreEncryptedAtRest is the claim ADR 015 makes about
// this table. What lands in the file — and therefore in every backup and every
// `.dump` — must not be the credential.
func TestChannelCredentialsAreEncryptedAtRest(t *testing.T) {
	repo, _, _ := newAlertRepo(t)
	ctx := context.Background()

	saved := webhookChannel(t, repo, "ops")

	var stored []byte
	if err := repo.db.QueryRowContext(ctx,
		"SELECT config_enc FROM alert_channels WHERE id = ?", saved.ID).Scan(&stored); err != nil {
		t.Fatalf("reading the stored configuration: %v", err)
	}
	if bytes.Contains(stored, []byte("a-secret-long-enough")) {
		t.Error("the signing secret is in the database in the clear")
	}

	read, err := repo.FindChannel(ctx, saved.ID)
	if err != nil {
		t.Fatalf("reading the channel back: %v", err)
	}
	if read.Config.Secret != "a-secret-long-enough" {
		t.Errorf("the secret came back as %q", read.Config.Secret)
	}
	if read.Name != "ops" || read.Type != domain.ChannelWebhook {
		t.Errorf("the channel came back as %+v", read)
	}
}

func TestChannelLifecycle(t *testing.T) {
	repo, _, _ := newAlertRepo(t)
	ctx := context.Background()

	has, err := repo.HasChannels(ctx)
	if err != nil {
		t.Fatalf("asking whether channels exist: %v", err)
	}
	if has {
		t.Error("a fresh installation reports channels; the notifier job would start with nothing to do")
	}

	first := webhookChannel(t, repo, "ops")
	second := webhookChannel(t, repo, "oncall")

	if has, _ = repo.HasChannels(ctx); !has {
		t.Error("a configured channel is not visible to the scheduler's question")
	}
	channels, err := repo.ListChannels(ctx)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(channels) != 2 || channels[0].ID != first.ID || channels[1].ID != second.ID {
		t.Errorf("listing returned %d channels, oldest first expected", len(channels))
	}

	if err := repo.DeleteChannel(ctx, second.ID); err != nil {
		t.Fatalf("deleting: %v", err)
	}
	if _, err := repo.FindChannel(ctx, second.ID); !errors.Is(err, domain.ErrAlertNotFound) {
		t.Errorf("reading a deleted channel gave %v, want ErrAlertNotFound", err)
	}
	if err := repo.DeleteChannel(ctx, second.ID); !errors.Is(err, domain.ErrAlertNotFound) {
		t.Errorf("deleting twice gave %v, want ErrAlertNotFound", err)
	}
}

// TestARuleCannotNameAChannelThatDoesNotExist: a rule pointing at nothing is
// the most common way to configure alerting that looks right and delivers
// nothing.
func TestARuleCannotNameAChannelThatDoesNotExist(t *testing.T) {
	repo, _, _ := newAlertRepo(t)
	trigger, _ := domain.ParseTrigger([]byte(`{"kind":"new_issue"}`))
	rule, err := domain.NewAlertRule(nil, "ghost", trigger, []int64{999}, 0, true)
	if err != nil {
		t.Fatalf("building the rule: %v", err)
	}
	if _, err := repo.CreateRule(context.Background(), rule); !errors.Is(err, domain.ErrAlertNotFound) {
		t.Errorf("creating a rule for a missing channel gave %v, want ErrAlertNotFound", err)
	}
}

func TestRuleLifecycleAndScoping(t *testing.T) {
	repo, _, projectID := newAlertRepo(t)
	ctx := context.Background()
	channel := webhookChannel(t, repo, "ops")

	global := addRule(t, repo, nil, `{"kind":"new_issue"}`, []int64{channel.ID}, 0)
	scoped := addRule(t, repo, &projectID, `{"kind":"regression"}`, []int64{channel.ID}, time.Minute)

	all, err := repo.ListRules(ctx, nil)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d rules, want 2", len(all))
	}

	other := projectID + 1
	forOther, err := repo.ListRules(ctx, &other)
	if err != nil {
		t.Fatalf("listing for another project: %v", err)
	}
	if len(forOther) != 1 || forOther[0].ID != global.ID {
		t.Errorf("another project sees %d rules, want only the global one", len(forOther))
	}

	read, err := repo.FindRule(ctx, scoped.ID)
	if err != nil {
		t.Fatalf("reading a rule: %v", err)
	}
	if read.ProjectID == nil || *read.ProjectID != projectID {
		t.Error("the scope did not survive a round trip")
	}
	if read.Trigger.Kind != domain.TriggerRegression || read.SilenceSeconds != 60 {
		t.Errorf("the rule came back as %+v", read)
	}

	if err := repo.DeleteRule(ctx, scoped.ID); err != nil {
		t.Fatalf("deleting: %v", err)
	}
	if _, err := repo.FindRule(ctx, scoped.ID); !errors.Is(err, domain.ErrAlertNotFound) {
		t.Errorf("reading a deleted rule gave %v", err)
	}
}

// TestANewIssueIsQueuedByTheTransactionThatCreatedIt is the whole of ADR 015:
// there must be no window in which the issue exists and its notification does
// not, because a restart lands in exactly that window.
func TestANewIssueIsQueuedByTheTransactionThatCreatedIt(t *testing.T) {
	repo, issues, projectID := newAlertRepo(t)
	ctx := context.Background()

	first := webhookChannel(t, repo, "ops")
	second := webhookChannel(t, repo, "oncall")
	addRule(t, repo, nil, `{"kind":"new_issue"}`, []int64{first.ID, second.ID}, time.Hour)

	record(t, issues, projectID, "abc", alertNow, nil)

	queued, err := repo.ListNotifications(ctx, ports.NotificationFilter{})
	if err != nil {
		t.Fatalf("reading the log: %v", err)
	}
	// One row per channel, not one per rule: delivery succeeds and fails per
	// destination.
	if len(queued) != 2 {
		t.Fatalf("got %d notifications, want one per channel", len(queued))
	}
	for _, notification := range queued {
		if notification.Status != domain.NotificationPending {
			t.Errorf("a fresh notification is %q", notification.Status)
		}
		if notification.Payload.Event != domain.TriggerNewIssue {
			t.Errorf("the payload says %q", notification.Payload.Event)
		}
		if notification.Payload.URL == "" {
			t.Error("the notification carries no link to the issue")
		}
		if notification.SubjectKey == "" {
			t.Error("the notification has no subject, so nothing can be silenced")
		}
	}
}

// TestTheSilenceWindowIsPersisted is the reason alert_state is a table. In
// memory it would reset on restart, and a restart is exactly when the burst
// arrives.
func TestTheSilenceWindowIsPersisted(t *testing.T) {
	repo, issues, projectID := newAlertRepo(t)
	ctx := context.Background()

	channel := webhookChannel(t, repo, "ops")
	addRule(t, repo, nil, `{"kind":"new_issue"}`, []int64{channel.ID}, time.Hour)

	// Ten events on one fingerprint: one new issue, and nine occurrences that
	// are not news.
	for range 10 {
		record(t, issues, projectID, "abc", alertNow, nil)
	}
	// And a second issue, which is news, and must not be masked by the first.
	record(t, issues, projectID, "def", alertNow, nil)

	queued, err := repo.ListNotifications(ctx, ports.NotificationFilter{})
	if err != nil {
		t.Fatalf("reading the log: %v", err)
	}
	if len(queued) != 2 {
		t.Fatalf("got %d notifications, want one per issue", len(queued))
	}

	subjects := map[string]bool{}
	for _, notification := range queued {
		subjects[notification.SubjectKey] = true
	}
	if len(subjects) != 2 {
		t.Errorf("the two notifications share a subject: %v", subjects)
	}
}

func TestClaimLeasesARowSoTwoPassesCannotSendIt(t *testing.T) {
	repo, issues, projectID := newAlertRepo(t)
	ctx := context.Background()

	channel := webhookChannel(t, repo, "ops")
	addRule(t, repo, nil, `{"kind":"new_issue"}`, []int64{channel.ID}, time.Hour)
	record(t, issues, projectID, "abc", alertNow, nil)

	first, err := repo.Claim(ctx, alertNow, 10)
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("the first pass claimed %d rows, want 1", len(first))
	}
	if first[0].Channel.Config.Secret != "a-secret-long-enough" {
		t.Error("the claimed row does not carry a decrypted channel, so the sender would have to look it up")
	}
	if first[0].DeliveryID == "" {
		t.Error("the claimed row has no delivery id, so a receiver cannot deduplicate")
	}
	if first[0].Notification.Attempts != 1 {
		t.Errorf("the attempt count is %d after one claim", first[0].Notification.Attempts)
	}

	second, err := repo.Claim(ctx, alertNow, 10)
	if err != nil {
		t.Fatalf("claiming again: %v", err)
	}
	if len(second) != 0 {
		t.Error("a second pass claimed a row the first one is still sending; it would be delivered twice")
	}

	// And it comes back on its own when the backoff expires, which is what
	// makes a crash mid-delivery survivable without a reaper.
	later, err := repo.Claim(ctx, alertNow.Add(domain.FirstRetryDelay), 10)
	if err != nil {
		t.Fatalf("claiming after the backoff: %v", err)
	}
	if len(later) != 1 {
		t.Error("a row abandoned mid-delivery never became due again")
	}
}

func TestMarkSentAndMarkFailed(t *testing.T) {
	repo, issues, projectID := newAlertRepo(t)
	ctx := context.Background()

	channel := webhookChannel(t, repo, "ops")
	addRule(t, repo, nil, `{"kind":"new_issue"}`, []int64{channel.ID}, time.Hour)
	record(t, issues, projectID, "abc", alertNow, nil)

	claimed, err := repo.Claim(ctx, alertNow, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claiming: %v (%d rows)", err, len(claimed))
	}
	id := claimed[0].Notification.ID

	if err := repo.MarkFailed(ctx, id, alertNow, "connection refused"); err != nil {
		t.Fatalf("marking failed: %v", err)
	}
	failed := readNotification(t, repo, id)
	if failed.Status != domain.NotificationFailed {
		t.Errorf("status is %q after one failure, want failed", failed.Status)
	}
	if failed.LastError != "connection refused" {
		t.Errorf("the reason came back as %q", failed.LastError)
	}

	if err := repo.MarkSent(ctx, id, alertNow); err != nil {
		t.Fatalf("marking sent: %v", err)
	}
	sent := readNotification(t, repo, id)
	if sent.Status != domain.NotificationSent || sent.SentAt == nil {
		t.Errorf("the delivered row is %+v", sent)
	}
	if sent.LastError != "" {
		t.Error("a stale error survived a successful delivery, so a recovered notification looks broken")
	}
}

// TestARowRunsOutOfAttempts: ten attempts on the shipped schedule span about
// eight hours, and after them the row is evidence rather than a queue entry.
func TestARowRunsOutOfAttempts(t *testing.T) {
	repo, issues, projectID := newAlertRepo(t)
	ctx := context.Background()

	channel := webhookChannel(t, repo, "ops")
	addRule(t, repo, nil, `{"kind":"new_issue"}`, []int64{channel.ID}, time.Hour)
	record(t, issues, projectID, "abc", alertNow, nil)

	now := alertNow
	var id int64
	for attempt := 1; attempt <= domain.MaxNotificationAttempts; attempt++ {
		claimed, err := repo.Claim(ctx, now, 10)
		if err != nil {
			t.Fatalf("claiming on attempt %d: %v", attempt, err)
		}
		if len(claimed) != 1 {
			t.Fatalf("attempt %d claimed %d rows", attempt, len(claimed))
		}
		id = claimed[0].Notification.ID
		if err := repo.MarkFailed(ctx, id, now, "still refused"); err != nil {
			t.Fatalf("marking failed: %v", err)
		}
		now = now.Add(domain.MaxRetryDelay + time.Minute)
	}

	dead := readNotification(t, repo, id)
	if dead.Status != domain.NotificationDead {
		t.Errorf("after %d attempts the row is %q, want dead", domain.MaxNotificationAttempts, dead.Status)
	}
	if claimed, _ := repo.Claim(ctx, now, 10); len(claimed) != 0 {
		t.Error("a dead row is still being claimed")
	}

	// And an operator can put it back, with its budget restored: pressing
	// retry means the reason it failed has been dealt with.
	revived, err := repo.RetryNotification(ctx, id, now)
	if err != nil {
		t.Fatalf("retrying: %v", err)
	}
	if revived.Status != domain.NotificationPending || revived.Attempts != 0 {
		t.Errorf("the revived row is %+v", revived)
	}
	if claimed, _ := repo.Claim(ctx, now, 10); len(claimed) != 1 {
		t.Error("a revived row is not claimed")
	}
}

func TestRetryingSomethingAlreadyDeliveredIsRefused(t *testing.T) {
	repo, issues, projectID := newAlertRepo(t)
	ctx := context.Background()

	channel := webhookChannel(t, repo, "ops")
	addRule(t, repo, nil, `{"kind":"new_issue"}`, []int64{channel.ID}, time.Hour)
	record(t, issues, projectID, "abc", alertNow, nil)

	claimed, _ := repo.Claim(ctx, alertNow, 10)
	if err := repo.MarkSent(ctx, claimed[0].Notification.ID, alertNow); err != nil {
		t.Fatalf("marking sent: %v", err)
	}
	if _, err := repo.RetryNotification(ctx, claimed[0].Notification.ID, alertNow); !errors.Is(err, domain.ErrAlertNotFound) {
		t.Errorf("retrying a delivered notification gave %v", err)
	}
	if _, err := repo.RetryNotification(ctx, 9999, alertNow); !errors.Is(err, domain.ErrAlertNotFound) {
		t.Errorf("retrying a notification that does not exist gave %v", err)
	}
}

// Deleting a channel rewrites the rules that named it and closes what was
// queued for it: a rule pointing at nothing looks configured and notifies
// nobody, and a pending row for a deleted channel is ten failures reached
// slowly.
func TestDeletingAChannelDoesNotLeaveARulePointingAtNothing(t *testing.T) {
	repo, issues, projectID := newAlertRepo(t)
	ctx := context.Background()

	kept := webhookChannel(t, repo, "kept")
	removed := webhookChannel(t, repo, "removed")
	both := addRule(t, repo, nil, `{"kind":"new_issue"}`, []int64{kept.ID, removed.ID}, time.Hour)
	only := addRule(t, repo, nil, `{"kind":"regression"}`, []int64{removed.ID}, time.Hour)

	record(t, issues, projectID, "abc", alertNow, nil)
	if err := repo.DeleteChannel(ctx, removed.ID); err != nil {
		t.Fatalf("deleting: %v", err)
	}

	survived, err := repo.FindRule(ctx, both.ID)
	if err != nil {
		t.Fatalf("reading the rule: %v", err)
	}
	if len(survived.ChannelIDs) != 1 || survived.ChannelIDs[0] != kept.ID {
		t.Errorf("the rule still names %v", survived.ChannelIDs)
	}
	if !survived.Enabled {
		t.Error("a rule that still has a channel was switched off")
	}

	// A rule left with no channel is switched off rather than deleted:
	// somebody's configuration, visibly inert, is better than a rule that
	// looks alive and reaches nobody — and better than deleting it silently.
	orphan, err := repo.FindRule(ctx, only.ID)
	if err != nil {
		t.Fatalf("reading the orphaned rule: %v", err)
	}
	if orphan.Enabled {
		t.Error("a rule with no channels left is still enabled")
	}

	// And nothing is still queued for a channel that cannot be reached.
	for _, notification := range mustList(t, repo, ports.NotificationFilter{}) {
		if notification.ChannelID == removed.ID && notification.Status == domain.NotificationPending {
			t.Error("a notification is still pending for a deleted channel; it would be retried ten times")
		}
	}
}

func TestListNotificationsFiltersByStatus(t *testing.T) {
	repo, issues, projectID := newAlertRepo(t)
	ctx := context.Background()

	channel := webhookChannel(t, repo, "ops")
	addRule(t, repo, nil, `{"kind":"new_issue"}`, []int64{channel.ID}, 0)
	record(t, issues, projectID, "abc", alertNow, nil)
	record(t, issues, projectID, "def", alertNow, nil)

	claimed, _ := repo.Claim(ctx, alertNow, 1)
	if err := repo.MarkSent(ctx, claimed[0].Notification.ID, alertNow); err != nil {
		t.Fatalf("marking sent: %v", err)
	}

	sent := mustList(t, repo, ports.NotificationFilter{Status: domain.NotificationSent})
	if len(sent) != 1 {
		t.Errorf("got %d delivered rows, want 1", len(sent))
	}
	pending := mustList(t, repo, ports.NotificationFilter{Status: domain.NotificationPending})
	if len(pending) != 1 {
		t.Errorf("got %d pending rows, want 1", len(pending))
	}
	if limited := mustList(t, repo, ports.NotificationFilter{Limit: 1}); len(limited) != 1 {
		t.Errorf("the limit was ignored: got %d rows", len(limited))
	}
}

// TestPruningKeepsTheEvidence: a log with no end is a disk that fills, but a
// dead row is what somebody reads when they ask why they never got the message.
func TestPruningKeepsTheEvidence(t *testing.T) {
	repo, issues, projectID := newAlertRepo(t)
	ctx := context.Background()

	channel := webhookChannel(t, repo, "ops")
	addRule(t, repo, nil, `{"kind":"new_issue"}`, []int64{channel.ID}, 0)
	record(t, issues, projectID, "abc", alertNow, nil)
	record(t, issues, projectID, "def", alertNow, nil)

	claimed, _ := repo.Claim(ctx, alertNow, 10)
	if err := repo.MarkSent(ctx, claimed[0].Notification.ID, alertNow); err != nil {
		t.Fatalf("marking sent: %v", err)
	}
	if err := repo.MarkFailed(ctx, claimed[1].Notification.ID, alertNow, "gone"); err != nil {
		t.Fatalf("marking failed: %v", err)
	}

	deleted, err := repo.PruneNotifications(ctx, alertNow.Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("pruning: %v", err)
	}
	if deleted != 1 {
		t.Errorf("pruned %d rows, want only the delivered one", deleted)
	}
	remaining := mustList(t, repo, ports.NotificationFilter{})
	if len(remaining) != 1 || remaining[0].Status == domain.NotificationSent {
		t.Error("pruning removed the row that explains a failure")
	}
}

func TestIssueHourlyCountsReadsTheAggregates(t *testing.T) {
	repo, issues, projectID := newAlertRepo(t)
	ctx := context.Background()

	base := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	result := record(t, issues, projectID, "abc", base, nil)
	record(t, issues, projectID, "abc", base.Add(time.Hour), nil)
	record(t, issues, projectID, "abc", base.Add(time.Hour), nil)
	record(t, issues, projectID, "abc", base.Add(2*time.Hour), nil)
	record(t, issues, projectID, "abc", base.Add(2*time.Hour), nil)
	record(t, issues, projectID, "abc", base.Add(2*time.Hour), nil)

	counts, err := repo.IssueHourlyCounts(ctx, projectID, result.Issue.ID, base.Add(2*time.Hour), 3)
	if err != nil {
		t.Fatalf("reading the counts: %v", err)
	}
	if len(counts) != 3 {
		t.Fatalf("got %d hours, want 3", len(counts))
	}
	// Oldest first, and absent hours are zero rather than absent: the caller
	// is averaging over a window and a missing hour is a real zero.
	if counts[0] != 1 || counts[1] != 2 || counts[2] != 3 {
		t.Errorf("the counts are %v, want [1 2 3]", counts)
	}

	empty, err := repo.IssueHourlyCounts(ctx, projectID, result.Issue.ID, base.Add(48*time.Hour), 2)
	if err != nil {
		t.Fatalf("reading a window with no events: %v", err)
	}
	if len(empty) != 2 || empty[0] != 0 || empty[1] != 0 {
		t.Errorf("a window with no events came back as %v", empty)
	}
}

func TestWatchesAndRulesOfKindAreAnsweredWithoutAQuery(t *testing.T) {
	repo, _, projectID := newAlertRepo(t)
	ctx := context.Background()

	watched, err := repo.Watches(ctx, domain.TriggerErrorRate)
	if err != nil {
		t.Fatalf("asking: %v", err)
	}
	if watched {
		t.Error("a fresh installation says it watches the error rate")
	}

	channel := webhookChannel(t, repo, "ops")
	addRule(t, repo, &projectID, `{"kind":"error_rate","window_s":60,"min_events_per_min":10}`,
		[]int64{channel.ID}, 0)

	// The cache is invalidated by the write, which is why this is true
	// immediately rather than after a restart.
	if watched, _ = repo.Watches(ctx, domain.TriggerErrorRate); !watched {
		t.Error("a rule created a moment ago is not visible to the detector")
	}
	if watched, _ = repo.Watches(ctx, domain.TriggerIssueSpike); watched {
		t.Error("a rule of one kind made another kind look watched")
	}

	rules, err := repo.RulesOfKind(ctx, domain.TriggerErrorRate, projectID)
	if err != nil {
		t.Fatalf("reading rules of a kind: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("got %d rules", len(rules))
	}
	other, err := repo.RulesOfKind(ctx, domain.TriggerErrorRate, projectID+1)
	if err != nil {
		t.Fatalf("reading rules for another project: %v", err)
	}
	if len(other) != 0 {
		t.Error("a project-scoped rule leaked into another project")
	}
}

func TestEnqueueWithNoRulesWritesNothing(t *testing.T) {
	repo, _, projectID := newAlertRepo(t)
	ctx := context.Background()

	written, err := repo.Enqueue(ctx, &domain.AlertEvent{
		Kind: domain.TriggerNewIssue, ProjectID: projectID, IssueID: 1, At: alertNow,
	}, "https://errors.example.test/projects/1/issues/1")
	if err != nil {
		t.Fatalf("enqueueing: %v", err)
	}
	if written != 0 {
		t.Errorf("wrote %d rows with no rule configured", written)
	}
}

func TestEnqueueRespectsTheSilenceWindow(t *testing.T) {
	repo, _, projectID := newAlertRepo(t)
	ctx := context.Background()

	channel := webhookChannel(t, repo, "ops")
	addRule(t, repo, nil, `{"kind":"issue_spike","window_s":3600,"min_count":5,"factor":2}`,
		[]int64{channel.ID}, time.Hour)

	spike := domain.AlertEvent{
		Kind: domain.TriggerIssueSpike, ProjectID: projectID, IssueID: 7,
		Count: 20, Baseline: 2, At: alertNow,
	}
	first, err := repo.Enqueue(ctx, &spike, "url")
	if err != nil {
		t.Fatalf("enqueueing: %v", err)
	}
	if first != 1 {
		t.Fatalf("wrote %d rows, want 1", first)
	}

	spike.At = alertNow.Add(time.Minute)
	again, err := repo.Enqueue(ctx, &spike, "url")
	if err != nil {
		t.Fatalf("enqueueing again: %v", err)
	}
	if again != 0 {
		t.Errorf("wrote %d rows inside the silence window", again)
	}

	spike.At = alertNow.Add(2 * time.Hour)
	after, err := repo.Enqueue(ctx, &spike, "url")
	if err != nil {
		t.Fatalf("enqueueing after the window: %v", err)
	}
	if after != 1 {
		t.Errorf("wrote %d rows after the silence expired", after)
	}

	// And a spike that does not clear the thresholds is not queued at all.
	quiet := spike
	quiet.Count, quiet.At = 3, alertNow.Add(4*time.Hour)
	if written, _ := repo.Enqueue(ctx, &quiet, "url"); written != 0 {
		t.Errorf("a spike under the floor queued %d rows", written)
	}
}

func TestARegressionIsQueuedAsARegression(t *testing.T) {
	repo, issues, projectID := newAlertRepo(t)
	ctx := context.Background()

	channel := webhookChannel(t, repo, "ops")
	addRule(t, repo, nil, `{"kind":"regression"}`, []int64{channel.ID}, time.Hour)

	result := record(t, issues, projectID, "abc", alertNow, nil)
	// A new issue is not a regression, and a rule watching one must not fire
	// on the other.
	if queued := mustList(t, repo, ports.NotificationFilter{}); len(queued) != 0 {
		t.Fatalf("a new issue queued %d rows against a regression rule", len(queued))
	}

	if err := issues.SetStatus(ctx, projectID, result.Issue.ID, domain.StatusResolved); err != nil {
		t.Fatalf("resolving: %v", err)
	}
	record(t, issues, projectID, "abc", alertNow.Add(time.Minute), nil)

	queued := mustList(t, repo, ports.NotificationFilter{})
	if len(queued) != 1 {
		t.Fatalf("a regression queued %d rows, want 1", len(queued))
	}
	if queued[0].Payload.Event != domain.TriggerRegression {
		t.Errorf("the payload says %q", queued[0].Payload.Event)
	}
}

func readNotification(t *testing.T, repo *AlertRepository, id int64) domain.Notification {
	t.Helper()
	notification, err := repo.ListNotificationByID(context.Background(), id)
	if err != nil {
		t.Fatalf("reading notification %d: %v", id, err)
	}
	return notification
}

func mustList(t *testing.T, repo *AlertRepository, filter ports.NotificationFilter) []domain.Notification {
	t.Helper()
	notifications, err := repo.ListNotifications(context.Background(), filter)
	if err != nil {
		t.Fatalf("listing notifications: %v", err)
	}
	return notifications
}

// TestChannelsDescribeWhereTheyDeliver covers the seam the digest and `doctor`
// read through (ports.AlertChannels, ADR 035): a list, a place to connect to,
// and nothing that could be pasted into somebody's chat.
func TestChannelsDescribeWhereTheyDeliver(t *testing.T) {
	repo, _, _ := newAlertRepo(t)
	ctx := context.Background()

	for _, fixture := range []struct {
		channelType domain.ChannelType
		name        string
		config      domain.ChannelConfig
		digest      bool
		endpoint    string
		secret      string
	}{
		{
			channelType: domain.ChannelTelegram, name: "ops", digest: true,
			config:   domain.ChannelConfig{BotToken: "8100:AAH-secret", ChatID: "-1001"},
			endpoint: "https://api.telegram.org", secret: "8100:AAH-secret",
		},
		{
			channelType: domain.ChannelSlack, name: "#alerts",
			config:   domain.ChannelConfig{URL: "https://hooks.slack.com/services/T0/B0/secretpath"},
			endpoint: "https://hooks.slack.com", secret: "secretpath",
		},
		{
			channelType: domain.ChannelDiscord, name: "#incidents",
			config:   domain.ChannelConfig{URL: "https://discord.com/api/webhooks/1/secrettoken"},
			endpoint: "https://discord.com", secret: "secrettoken",
		},
		{
			channelType: domain.ChannelWebhook, name: "pager",
			config: domain.ChannelConfig{
				URL: "https://pager.example.test/hook", Secret: "a-secret-long-enough",
			},
			// The one URL that survives whole: a signed webhook is
			// authenticated by its HMAC, not by its path.
			endpoint: "https://pager.example.test/hook", secret: "a-secret-long-enough",
		},
		{
			channelType: domain.ChannelEmail, name: "mail",
			config: domain.ChannelConfig{
				Host: "smtp.example.test", Port: 587, From: "trapline@example.test",
				To: []string{"ops@example.test"}, Password: "hunter2hunter2",
			},
			endpoint: "smtp.example.test:587", secret: "hunter2hunter2",
		},
	} {
		t.Run(string(fixture.channelType), func(t *testing.T) {
			built, err := domain.NewAlertChannel(
				fixture.channelType, fixture.name, fixture.config, fixture.digest, alertNow)
			if err != nil {
				t.Fatalf("building the channel: %v", err)
			}
			saved, err := repo.CreateChannel(ctx, &built)
			if err != nil {
				t.Fatalf("creating the channel: %v", err)
			}

			channels, err := repo.Channels(ctx)
			if err != nil {
				t.Fatalf("listing channels: %v", err)
			}
			var found *ports.AlertChannel
			for index := range channels {
				if channels[index].ID == saved.ID {
					found = &channels[index]
				}
			}
			if found == nil {
				t.Fatalf("the channel just created is not in the list")
			}
			if found.SecretError != "" {
				t.Fatalf("the key did not open a channel it had just sealed: %s", found.SecretError)
			}
			if found.Endpoint != fixture.endpoint {
				t.Errorf("endpoint = %q, want %q", found.Endpoint, fixture.endpoint)
			}
			if found.Digest != fixture.digest {
				t.Errorf("digest = %v, want %v", found.Digest, fixture.digest)
			}
			if strings.Contains(found.Endpoint, fixture.secret) &&
				fixture.channelType != domain.ChannelWebhook {
				t.Errorf("the endpoint carries the credential: %q", found.Endpoint)
			}
		})
	}
}

// TestChannelsReportTheOneThatWillNotDecrypt is why this call returns a field
// instead of an error.
//
// The whole point of asking is to find the channel whose secret has stopped
// working. Failing the call would report "the channel list is broken" on an
// installation whose other channels are fine, and would take `doctor`'s
// answer away at the exact moment it is the answer.
func TestChannelsReportTheOneThatWillNotDecrypt(t *testing.T) {
	repo, _, _ := newAlertRepo(t)
	ctx := context.Background()

	healthy := webhookChannel(t, repo, "still fine")
	broken := webhookChannel(t, repo, "restored without its key")

	// A blob that is not what the key sealed: the shape a backup restored
	// without its `.key` takes, reproduced on one row so the other stays good.
	if _, err := repo.db.ExecContext(ctx,
		"UPDATE alert_channels SET config_enc = ? WHERE id = ?",
		[]byte("not a ciphertext"), broken.ID); err != nil {
		t.Fatalf("corrupting the stored configuration: %v", err)
	}

	channels, err := repo.Channels(ctx)
	if err != nil {
		t.Fatalf("one unreadable channel failed the whole listing: %v", err)
	}
	if len(channels) != 2 {
		t.Fatalf("the listing dropped a channel: %d of 2", len(channels))
	}
	for _, channel := range channels {
		switch channel.ID {
		case healthy.ID:
			if channel.SecretError != "" {
				t.Errorf("a healthy channel reports %q", channel.SecretError)
			}
		case broken.ID:
			if channel.SecretError == "" {
				t.Error("the unreadable channel reports no problem")
			}
			if channel.Endpoint != "" {
				t.Errorf("a channel that did not decrypt still claims an endpoint: %q", channel.Endpoint)
			}
		}
	}
}

// TestEnqueueDigestQueuesTheReportAsWritten: the weekly digest goes through
// the same outbox as every alert, so a report lost because a chat API was
// down for a moment is retried instead of noticed a week later by nobody.
func TestEnqueueDigestQueuesTheReportAsWritten(t *testing.T) {
	repo, _, _ := newAlertRepo(t)
	ctx := context.Background()
	channel := webhookChannel(t, repo, "weekly")

	const subject = "trapline — the week of 2026-08-24"
	const body = "venekambio\n  100 events, up 300% from 25 last week\n"
	if err := repo.EnqueueDigest(ctx, channel.ID, subject, body); err != nil {
		t.Fatalf("queueing the digest: %v", err)
	}

	queued, err := repo.ListNotifications(ctx, ports.NotificationFilter{})
	if err != nil {
		t.Fatalf("reading the log: %v", err)
	}
	if len(queued) != 1 {
		t.Fatalf("the outbox holds %d rows, want 1", len(queued))
	}
	row := &queued[0]
	if row.Status != domain.NotificationPending {
		t.Errorf("status = %q, want pending", row.Status)
	}
	if row.ChannelID != channel.ID {
		t.Errorf("channel = %d, want %d", row.ChannelID, channel.ID)
	}
	// Zero, and deliberately: no rule produced it. The column has no foreign
	// key precisely so the log can hold rows whose origin is not a rule.
	if row.RuleID != 0 {
		t.Errorf("rule = %d, want 0 — no rule produced a digest", row.RuleID)
	}
	if row.Payload.Event != domain.EventDigest {
		t.Errorf("event = %q, want %q", row.Payload.Event, domain.EventDigest)
	}
	if row.Payload.Text() != body {
		t.Errorf("the queued text is not the report:\n%s", row.Payload.Text())
	}
	if !strings.HasPrefix(row.SubjectKey, "digest:") {
		t.Errorf("subject key = %q, want it to name what it is about", row.SubjectKey)
	}
}

// TestEnqueueDigestRefusesAChannelThatIsGone: the digest job reads the channel
// list, spends a moment building a report and only then queues it, so a
// channel deleted in between is a real window. A row for it would be retried
// and then declared dead — a delivery failure in the log for something nobody
// was owed.
func TestEnqueueDigestRefusesAChannelThatIsGone(t *testing.T) {
	repo, _, _ := newAlertRepo(t)
	ctx := context.Background()

	if err := repo.EnqueueDigest(ctx, 4242, "the week of never", "…"); !errors.Is(err, domain.ErrAlertNotFound) {
		t.Errorf("queueing for a channel that does not exist returned %v", err)
	}
	queued, err := repo.ListNotifications(ctx, ports.NotificationFilter{})
	if err != nil {
		t.Fatalf("reading the log: %v", err)
	}
	if len(queued) != 0 {
		t.Errorf("a refused digest still wrote %d rows", len(queued))
	}
}
