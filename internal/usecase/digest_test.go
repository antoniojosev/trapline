package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// fakeSettings is an in-memory ports.SettingsStore.
type fakeSettings struct {
	values map[string]json.RawMessage
	writes int
	err    error
}

func newFakeSettings() *fakeSettings {
	return &fakeSettings{values: map[string]json.RawMessage{}}
}

func (s *fakeSettings) Setting(_ context.Context, key string) (json.RawMessage, error) {
	if s.err != nil {
		return nil, s.err
	}
	value, found := s.values[key]
	if !found {
		return nil, domain.ErrSettingNotFound
	}
	return value, nil
}

func (s *fakeSettings) SetSetting(_ context.Context, key string, value json.RawMessage) error {
	if s.err != nil {
		return s.err
	}
	s.writes++
	s.values[key] = value
	return nil
}

// fakeDigestRepo answers the two transition questions with whatever a test
// hands it.
type fakeDigestRepo struct {
	fresh ports.IssueCounts
	back  ports.IssueCounts
	err   error
}

func (r *fakeDigestRepo) NewIssues(
	context.Context, int64, domain.Range, int,
) (ports.IssueCounts, error) {
	return r.fresh, r.err
}

func (r *fakeDigestRepo) RegressedIssues(
	context.Context, int64, domain.Range, int,
) (ports.IssueCounts, error) {
	return r.back, r.err
}

// fakeChannels is the notification subsystem, reduced to what the digest needs
// of it: who asked for the digest, and somewhere to hand one over.
type fakeChannels struct {
	channels []ports.AlertChannel
	queued   []queuedDigest
	listErr  error
	sendErr  error
}

type queuedDigest struct {
	channelID int64
	subject   string
	body      string
}

func (c *fakeChannels) Channels(context.Context) ([]ports.AlertChannel, error) {
	return c.channels, c.listErr
}

func (c *fakeChannels) EnqueueDigest(_ context.Context, channelID int64, subject, body string) error {
	if c.sendErr != nil {
		return c.sendErr
	}
	c.queued = append(c.queued, queuedDigest{channelID: channelID, subject: subject, body: body})
	return nil
}

// digestStats is a stats store with one project's week in it.
type digestStats struct {
	*fakeStats
	// byBucket is the count of each hourly bucket, so a test can put events
	// in this week and the week before without arithmetic.
	byBucket map[string]int64
	top      []ports.IssueCount
	err      error
}

func (s *digestStats) ProjectSeries(
	_ context.Context, _ int64, window domain.Range,
) ([]ports.LevelBucket, error) {
	if s.err != nil {
		return nil, s.err
	}
	var buckets []ports.LevelBucket
	for _, hour := range window.Buckets() {
		if count, found := s.byBucket[hour]; found {
			buckets = append(buckets, ports.LevelBucket{
				Hour: hour, Level: domain.LevelError, Count: count,
			})
		}
	}
	return buckets, nil
}

func (s *digestStats) TopIssues(
	context.Context, int64, domain.Range, int,
) ([]ports.IssueCount, error) {
	return s.top, s.err
}

// digestFixture is a wired digest with everything it reads in reach.
type digestFixture struct {
	digest   *Digest
	settings *fakeSettings
	stats    *digestStats
	repo     *fakeDigestRepo
	channels *fakeChannels
	clock    *movableClock
}

// monday is a Monday at 09:00 UTC — the default schedule's moment.
var monday = time.Date(2026, time.August, 31, 9, 0, 0, 0, time.UTC)

func newDigestFixture(t *testing.T) *digestFixture {
	t.Helper()

	projects := newFakeRepo()
	if _, _, err := projects.Create(context.Background(),
		domain.Project{Name: "venekambio"}, domain.Key{PublicKey: "k"}); err != nil {
		t.Fatalf("seeding a project: %v", err)
	}

	fixture := &digestFixture{
		settings: newFakeSettings(),
		stats:    &digestStats{fakeStats: newFakeStats(), byBucket: map[string]int64{}},
		repo:     &fakeDigestRepo{},
		channels: &fakeChannels{},
		clock:    &movableClock{now: monday},
	}
	origin, err := domain.ParseOrigin("https://errors.example.test")
	if err != nil {
		t.Fatalf("parsing the origin: %v", err)
	}
	fixture.digest = NewDigest(projects, fixture.stats, fixture.repo, fixture.settings,
		fixture.clock, origin).
		WithChannels(fixture.channels).
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	return fixture
}

// wantsDigest puts one channel in that asked for the weekly report.
func (f *digestFixture) wantsDigest() {
	f.channels.channels = []ports.AlertChannel{
		{ID: 7, Type: "slack", Name: "#alerts", Digest: true, Endpoint: "https://hooks.example.test/x"},
	}
}

func TestScheduleFallsBackToTheDefault(t *testing.T) {
	fixture := newDigestFixture(t)

	schedule, err := fixture.digest.Schedule(context.Background())
	if err != nil {
		t.Fatalf("reading the schedule: %v", err)
	}
	if schedule != domain.DefaultDigestSchedule {
		t.Errorf("an unconfigured installation reports %v, want the default %v",
			schedule, domain.DefaultDigestSchedule)
	}
}

func TestSetScheduleRoundTrips(t *testing.T) {
	fixture := newDigestFixture(t)
	want := domain.DigestSchedule{Weekday: time.Friday, Hour: 17}

	if err := fixture.digest.SetSchedule(context.Background(), want); err != nil {
		t.Fatalf("setting the schedule: %v", err)
	}
	got, err := fixture.digest.Schedule(context.Background())
	if err != nil {
		t.Fatalf("reading it back: %v", err)
	}
	if got != want {
		t.Errorf("read back %v, want %v", got, want)
	}
}

func TestSetScheduleRefusesATimeThatDoesNotExist(t *testing.T) {
	fixture := newDigestFixture(t)
	err := fixture.digest.SetSchedule(context.Background(),
		domain.DigestSchedule{Weekday: time.Monday, Hour: 25})
	if !errors.Is(err, domain.ErrInvalidDigest) {
		t.Errorf("SetSchedule(hour 25) = %v, want ErrInvalidDigest", err)
	}
	if fixture.settings.writes != 0 {
		t.Errorf("an invalid schedule was written anyway")
	}
}

// TestScheduleSurvivesACorruptRow matters because the alternative is a feature
// that silently stops: a stored value nothing can read must fall back to the
// default and say so, not disable the digest.
func TestScheduleSurvivesACorruptRow(t *testing.T) {
	for name, stored := range map[string]string{
		"not json":    `{"weekday":`,
		"not a time":  `{"weekday":9,"hour":47}`,
		"wrong shape": `"monday"`,
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newDigestFixture(t)
			fixture.settings.values[DigestScheduleKey] = json.RawMessage(stored)

			schedule, err := fixture.digest.Schedule(context.Background())
			if err != nil {
				t.Fatalf("reading a corrupt schedule: %v", err)
			}
			if schedule != domain.DefaultDigestSchedule {
				t.Errorf("got %v, want the default", schedule)
			}
		})
	}
}

// TestHasDigestChannelsIsTheStartCondition is the whole of ADR 014 for this
// subsystem: no channel asked, no job.
func TestHasDigestChannelsIsTheStartCondition(t *testing.T) {
	fixture := newDigestFixture(t)

	wants, err := fixture.digest.HasDigestChannels(context.Background())
	if err != nil {
		t.Fatalf("asking: %v", err)
	}
	if wants {
		t.Error("an installation with no channels wants a digest job")
	}

	// A channel that exists but did not ask for the digest is not a reason to
	// run the job either — that is the difference between "alerting is on"
	// and "somebody wants a weekly mail".
	fixture.channels.channels = []ports.AlertChannel{{ID: 1, Type: "webhook", Digest: false}}
	if wants, _ = fixture.digest.HasDigestChannels(context.Background()); wants {
		t.Error("a channel that did not ask for the digest started the job")
	}

	fixture.wantsDigest()
	if wants, _ = fixture.digest.HasDigestChannels(context.Background()); !wants {
		t.Error("a channel that asked for the digest did not start the job")
	}
}

func TestHasDigestChannelsWithoutASubsystem(t *testing.T) {
	fixture := newDigestFixture(t)
	// The shape of a build whose alerting has not been assembled at all.
	bare := NewDigest(newFakeRepo(), fixture.stats, fixture.repo, fixture.settings,
		fixture.clock, domain.Origin{Scheme: "https", Host: "errors.example.test"})

	wants, err := bare.HasDigestChannels(context.Background())
	if err != nil {
		t.Fatalf("asking: %v", err)
	}
	if wants {
		t.Error("a build with no notification subsystem wants a digest job")
	}
}

// TestFirstTickSendsNothing is the rule that keeps switching the digest on
// from being startling: the first tick records the period and stays quiet.
func TestFirstTickSendsNothing(t *testing.T) {
	fixture := newDigestFixture(t)
	fixture.wantsDigest()

	if err := fixture.digest.Tick(context.Background()); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	if len(fixture.channels.queued) != 0 {
		t.Errorf("the first tick sent %d digests", len(fixture.channels.queued))
	}
	if _, found := fixture.settings.values[DigestLastSentKey]; !found {
		t.Error("the first tick did not record the period, so the next one would send a stale week")
	}
}

// TestTickSendsOncePerPeriod is the restart story: whatever happens between
// two scheduled moments, exactly one digest goes out.
func TestTickSendsOncePerPeriod(t *testing.T) {
	fixture := newDigestFixture(t)
	fixture.wantsDigest()
	ctx := context.Background()

	// The week before the send, so there is something to report.
	fixture.stats.byBucket["2026-08-25T10"] = 40

	// First tick, a day before the scheduled moment: records the period.
	fixture.clock.now = monday.AddDate(0, 0, -1)
	if err := fixture.digest.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	// The moment arrives.
	fixture.clock.now = monday
	if err := fixture.digest.Tick(ctx); err != nil {
		t.Fatalf("tick at the scheduled moment: %v", err)
	}
	if len(fixture.channels.queued) != 1 {
		t.Fatalf("the scheduled moment queued %d digests, want 1", len(fixture.channels.queued))
	}

	// Ten minutes later, and an hour later, and a day later: nothing more.
	for _, later := range []time.Duration{10 * time.Minute, time.Hour, 24 * time.Hour} {
		fixture.clock.now = monday.Add(later)
		if err := fixture.digest.Tick(ctx); err != nil {
			t.Fatalf("tick %s later: %v", later, err)
		}
	}
	if len(fixture.channels.queued) != 1 {
		t.Fatalf("the same period was sent %d times", len(fixture.channels.queued))
	}

	// The next week's moment sends again.
	fixture.clock.now = monday.AddDate(0, 0, 7)
	if err := fixture.digest.Tick(ctx); err != nil {
		t.Fatalf("tick a week later: %v", err)
	}
	if len(fixture.channels.queued) != 2 {
		t.Fatalf("the next period queued %d digests in total, want 2", len(fixture.channels.queued))
	}
}

// TestTickAfterAnOutageStillSendsOne is the other half of that story: a server
// that was down at nine and comes back at eleven sends the week's digest, once.
func TestTickAfterAnOutageStillSendsOne(t *testing.T) {
	fixture := newDigestFixture(t)
	fixture.wantsDigest()
	ctx := context.Background()

	fixture.settings.values[DigestLastSentKey] = mustJSON(t,
		map[string]any{"at": monday.AddDate(0, 0, -7)})

	fixture.clock.now = monday.Add(2 * time.Hour)
	if err := fixture.digest.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(fixture.channels.queued) != 1 {
		t.Fatalf("queued %d digests after an outage, want 1", len(fixture.channels.queued))
	}
}

// TestSendOnlyGoesToChannelsThatAskedForIt is the difference between alerting
// and a weekly mail: the same channel list serves both, and one flag decides
// who gets this.
func TestSendOnlyGoesToChannelsThatAskedForIt(t *testing.T) {
	fixture := newDigestFixture(t)
	fixture.channels.channels = []ports.AlertChannel{
		{ID: 1, Type: "slack", Name: "#alerts", Digest: true, Endpoint: "https://x.test/1"},
		{ID: 2, Type: "webhook", Name: "pager", Digest: false, Endpoint: "https://x.test/2"},
		{ID: 3, Type: "email", Name: "team", Digest: true, Endpoint: "smtp.x.test:587"},
	}

	queued, err := fixture.digest.Send(context.Background(), monday)
	if err != nil {
		t.Fatalf("sending: %v", err)
	}
	if queued != 2 {
		t.Errorf("queued %d, want 2", queued)
	}
	for _, one := range fixture.channels.queued {
		if one.channelID == 2 {
			t.Error("a channel that did not ask for the digest received one")
		}
		if !strings.HasPrefix(one.subject, "trapline — the week of ") {
			t.Errorf("subject is %q", one.subject)
		}
		if !strings.Contains(one.body, "trapline — the week of") {
			t.Errorf("body does not look like a digest:\n%s", one.body)
		}
	}
}

// TestSendKeepsGoingWhenAChannelRefuses: stopping at the first refusal would
// make the delivery order decide who gets a report.
func TestSendKeepsGoingWhenAChannelRefuses(t *testing.T) {
	fixture := newDigestFixture(t)
	fixture.wantsDigest()
	fixture.channels.sendErr = errors.New("the outbox is full")

	queued, err := fixture.digest.Send(context.Background(), monday)
	if err == nil {
		t.Fatal("a refused hand-over was reported as success")
	}
	if queued != 0 {
		t.Errorf("queued %d despite the failure", queued)
	}
	if !strings.Contains(err.Error(), "#alerts") {
		t.Errorf("the failure does not name the channel: %v", err)
	}
}

// TestTickDoesNotRecordAFailedSend, because a period marked as done is a
// period that never gets retried.
func TestTickDoesNotRecordAFailedSend(t *testing.T) {
	fixture := newDigestFixture(t)
	fixture.wantsDigest()
	ctx := context.Background()

	fixture.settings.values[DigestLastSentKey] = mustJSON(t,
		map[string]any{"at": monday.AddDate(0, 0, -7)})
	fixture.channels.sendErr = errors.New("the outbox is full")

	if err := fixture.digest.Tick(ctx); err == nil {
		t.Fatal("a failed send was reported as a successful tick")
	}

	var marker struct {
		At time.Time `json:"at"`
	}
	if err := json.Unmarshal(fixture.settings.values[DigestLastSentKey], &marker); err != nil {
		t.Fatalf("reading the marker: %v", err)
	}
	if !marker.At.Equal(monday.AddDate(0, 0, -7)) {
		t.Errorf("the marker moved to %s despite the failure", marker.At)
	}
}

// TestPreviewNeedsNoChannels: previewing is what somebody does before
// configuring anywhere to send it.
func TestPreviewNeedsNoChannels(t *testing.T) {
	fixture := newDigestFixture(t)
	fixture.stats.byBucket["2026-08-25T10"] = 12

	report, text, err := fixture.digest.Preview(context.Background(), PreviewOptions{})
	if err != nil {
		t.Fatalf("previewing: %v", err)
	}
	if len(report.Projects) != 1 {
		t.Fatalf("the report has %d projects, want 1", len(report.Projects))
	}
	if report.Projects[0].Events != 12 {
		t.Errorf("the week counted %d events, want 12", report.Projects[0].Events)
	}
	if !strings.Contains(text, "venekambio") {
		t.Errorf("the rendered digest does not name the project:\n%s", text)
	}
}

// TestPreviewCountsTheTrendAgainstThePreviousWeek is what makes the report a
// report rather than a list.
func TestPreviewCountsTheTrendAgainstThePreviousWeek(t *testing.T) {
	fixture := newDigestFixture(t)
	fixture.stats.byBucket["2026-08-25T10"] = 100 // inside the reported week
	fixture.stats.byBucket["2026-08-18T10"] = 50  // inside the week before

	report, text, err := fixture.digest.Preview(context.Background(), PreviewOptions{})
	if err != nil {
		t.Fatalf("previewing: %v", err)
	}
	project := report.Projects[0]
	if project.Events != 100 || project.PreviousEvents != 50 {
		t.Fatalf("counted %d this week and %d last, want 100 and 50",
			project.Events, project.PreviousEvents)
	}
	if !strings.Contains(text, "up 100% from 50 last week") {
		t.Errorf("the trend is not in the text:\n%s", text)
	}
}

// TestPreviewLeavesOutAProjectWithNothingToSay, because a column of zeroes is
// what makes somebody stop reading a weekly mail.
func TestPreviewLeavesOutAProjectWithNothingToSay(t *testing.T) {
	fixture := newDigestFixture(t)

	report, _, err := fixture.digest.Preview(context.Background(), PreviewOptions{})
	if err != nil {
		t.Fatalf("previewing: %v", err)
	}
	if len(report.Projects) != 0 {
		t.Errorf("a silent project appeared in the report: %+v", report.Projects)
	}

	// One new issue and no events at all is still worth reporting: it is the
	// transition, not the volume, that the section is about.
	fixture.repo.fresh = ports.IssueCounts{
		Total:  1,
		Issues: []ports.IssueCount{{Issue: domain.Issue{ID: 3, Title: "EOF"}, Count: 0}},
	}
	report, _, err = fixture.digest.Preview(context.Background(), PreviewOptions{})
	if err != nil {
		t.Fatalf("previewing: %v", err)
	}
	if len(report.Projects) != 1 {
		t.Errorf("a project with a new issue was left out")
	}
}

// TestPreviewLinksOnlyWhenTheOriginIsPublic: a mail full of localhost links is
// worse than one with none, because the reader clicks them.
func TestPreviewLinksOnlyWhenTheOriginIsPublic(t *testing.T) {
	fixture := newDigestFixture(t)
	fixture.stats.byBucket["2026-08-25T10"] = 5
	fixture.stats.top = []ports.IssueCount{
		{Issue: domain.Issue{ID: 4, ProjectID: 1, Title: "boom"}, Count: 5},
	}

	_, text, err := fixture.digest.Preview(context.Background(), PreviewOptions{})
	if err != nil {
		t.Fatalf("previewing: %v", err)
	}
	if !strings.Contains(text, "https://errors.example.test/projects/1/issues/4") {
		t.Errorf("a public origin produced no link:\n%s", text)
	}

	local := NewDigest(fixture.digest.projects, fixture.stats, fixture.repo, fixture.settings,
		fixture.clock, domain.Origin{Scheme: "http", Host: "127.0.0.1:9000"})
	_, text, err = local.Preview(context.Background(), PreviewOptions{})
	if err != nil {
		t.Fatalf("previewing: %v", err)
	}
	if strings.Contains(text, "127.0.0.1") {
		t.Errorf("an unconfigured origin was pasted into the digest:\n%s", text)
	}
}

func TestPreviewCanNarrowToOneProject(t *testing.T) {
	fixture := newDigestFixture(t)
	fixture.stats.byBucket["2026-08-25T10"] = 5

	report, _, err := fixture.digest.Preview(context.Background(), PreviewOptions{ProjectID: 999})
	if err != nil {
		t.Fatalf("previewing: %v", err)
	}
	if len(report.Projects) != 0 {
		t.Errorf("a project id that matches nothing still produced %d sections", len(report.Projects))
	}
}

func TestPreviewPropagatesAStoreFailure(t *testing.T) {
	fixture := newDigestFixture(t)
	fixture.stats.err = errors.New("the disk is on fire")

	if _, _, err := fixture.digest.Preview(context.Background(), PreviewOptions{}); err == nil {
		t.Error("a failing store produced a report anyway")
	}
}

func TestTheJobIsTheUseCaseSeenThroughTheScheduler(t *testing.T) {
	fixture := newDigestFixture(t)
	job := fixture.digest.Job()

	if job.Name() != "digest" {
		t.Errorf("the job is called %q", job.Name())
	}
	if job.Interval() != DigestInterval {
		t.Errorf("the job ticks every %s, want %s", job.Interval(), DigestInterval)
	}
	if err := job.Run(context.Background()); err != nil {
		t.Errorf("running the job: %v", err)
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encoding a fixture: %v", err)
	}
	return encoded
}
