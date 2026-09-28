package usecase

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// fakeHealthRepo is an in-memory ports.HealthRepository.
//
// The SQLite adapter is tested against a real database (ADR 009); this exists
// so the use case's own orchestration — when the window is drained, what
// happens to counters whose write failed — is testable without one.
type fakeHealthRepo struct {
	buckets  map[domain.HealthKey]domain.SessionCounts
	writes   int
	failNext error
	sessions bool
}

func newFakeHealthRepo() *fakeHealthRepo {
	return &fakeHealthRepo{buckets: map[domain.HealthKey]domain.SessionCounts{}}
}

func (r *fakeHealthRepo) AddSessionCounts(_ context.Context, buckets []ports.SessionBucket) error {
	if err := r.failNext; err != nil {
		r.failNext = nil
		return err
	}
	r.writes++
	for _, bucket := range buckets {
		r.buckets[bucket.Key] = r.buckets[bucket.Key].Add(bucket.Counts)
	}
	return nil
}

func (r *fakeHealthRepo) ReleaseSeries(
	_ context.Context, projectID int64, release string, from, to string,
) ([]ports.SessionHour, error) {
	var hours []ports.SessionHour
	for key, counts := range r.buckets {
		if key.ProjectID != projectID || key.Release != release {
			continue
		}
		if key.Hour < from || key.Hour > to {
			continue
		}
		hours = append(hours, ports.SessionHour{Hour: key.Hour, Counts: counts})
	}
	return hours, nil
}

func (r *fakeHealthRepo) ReleaseTotals(
	_ context.Context, projectID int64, from, to string, _ int,
) ([]ports.ReleaseSessions, error) {
	byRelease := map[string]ports.ReleaseSessions{}
	for key, counts := range r.buckets {
		if key.ProjectID != projectID || key.Hour < from || key.Hour > to {
			continue
		}
		release := byRelease[key.Release]
		release.Release = key.Release
		release.Counts = release.Counts.Add(counts)
		if release.FirstHour == "" || key.Hour < release.FirstHour {
			release.FirstHour = key.Hour
		}
		if key.Hour > release.LastHour {
			release.LastHour = key.Hour
		}
		byRelease[key.Release] = release
	}
	totals := make([]ports.ReleaseSessions, 0, len(byRelease))
	for _, release := range byRelease {
		totals = append(totals, release)
	}
	return totals, nil
}

func (r *fakeHealthRepo) HasSessions(context.Context) (bool, error) { return r.sessions, nil }

func (r *fakeHealthRepo) PruneSessionsBefore(
	context.Context, int64, time.Time, int,
) (int64, error) {
	return 0, nil
}

func sessionPayload(sid, status string, errorCount int, started time.Time) []byte {
	return fmt.Appendf(nil,
		`{"sid":%q,"started":%q,"status":%q,"errors":%d,"attrs":{"release":"shop@1.4.2","environment":"production"}}`,
		sid, started.Format(time.RFC3339Nano), status, errorCount)
}

func newTestHealth(repo *fakeHealthRepo, now time.Time, windowSize int) *Health {
	return NewHealth(repo, fixedClock{now: now}, nil, windowSize)
}

// TestOneHundredSessionsFiveOfThemCrashes is the gate's claim, at the level
// where it can be checked exactly rather than to a tolerance.
func TestOneHundredSessionsFiveOfThemCrashes(t *testing.T) {
	repo := newFakeHealthRepo()
	now := time.Date(2026, 8, 29, 14, 30, 0, 0, time.UTC)
	health := newTestHealth(repo, now, 0)

	for index := range 100 {
		sid := fmt.Sprintf("s%03d", index)
		status := "exited"
		if index%20 == 0 {
			status = "crashed"
		}
		for _, payload := range [][]byte{
			sessionPayload(sid, "ok", 0, now),
			sessionPayload(sid, status, 0, now),
		} {
			if _, err := health.Accept(t.Context(), 1, "session", payload); err != nil {
				t.Fatalf("accepting %s: %v", sid, err)
			}
		}
	}

	if _, err := health.Flush(t.Context()); err != nil {
		t.Fatalf("flushing: %v", err)
	}

	got := repo.buckets[domain.HealthKey{
		ProjectID: 1, Release: "shop@1.4.2", Environment: "production", Hour: "2026-08-29T14",
	}]
	want := domain.SessionCounts{Started: 100, Crashed: 5}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	rate, known := got.CrashFreeRate()
	if !known || rate != 0.95 {
		t.Fatalf("crash-free rate %v (known %v), want 0.95", rate, known)
	}
}

// TestAnAggregateItemNeedsNoWindow: the SDKs that send `sessions` instead of
// `session` are the busy ones, and making them go through the window would be
// paying state for counts that arrived already added up.
func TestAnAggregateItemNeedsNoWindow(t *testing.T) {
	repo := newFakeHealthRepo()
	now := time.Date(2026, 8, 29, 14, 30, 0, 0, time.UTC)
	health := newTestHealth(repo, now, 0)

	payload := []byte(`{"attrs":{"release":"api@2.0.0","environment":"production"},
		"aggregates":[{"started":"2026-08-29T14:16:00Z","exited":40,"errored":3,"crashed":2}]}`)
	receipt, err := health.Accept(t.Context(), 1, "sessions", payload)
	if err != nil {
		t.Fatalf("accepting: %v", err)
	}
	if !receipt.Aggregated || receipt.Sessions != 45 {
		t.Fatalf("receipt %+v, want 45 aggregated sessions", receipt)
	}
	if inFlight := health.Window().InFlight; inFlight != 0 {
		t.Fatalf("an aggregate put %d sessions in the window", inFlight)
	}

	if _, err := health.Flush(t.Context()); err != nil {
		t.Fatalf("flushing: %v", err)
	}
	got := repo.buckets[domain.HealthKey{
		ProjectID: 1, Release: "api@2.0.0", Environment: "production", Hour: "2026-08-29T14",
	}]
	want := domain.SessionCounts{Started: 45, Errored: 3, Crashed: 2}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestAnAggregateWithoutAReleaseIsRefused(t *testing.T) {
	health := newTestHealth(newFakeHealthRepo(), time.Now().UTC(), 0)
	_, err := health.Accept(t.Context(), 1, "sessions",
		[]byte(`{"aggregates":[{"started":"2026-08-29T14:00:00Z","exited":1}]}`))
	if !errors.Is(err, domain.ErrInvalidSessionUpdate) {
		t.Fatalf("got %v, want ErrInvalidSessionUpdate", err)
	}
}

func TestASessionWithoutAReleaseIsRefused(t *testing.T) {
	health := newTestHealth(newFakeHealthRepo(), time.Now().UTC(), 0)
	_, err := health.Accept(t.Context(), 1, "session",
		[]byte(`{"sid":"s1","started":"2026-08-29T14:00:00Z","status":"exited"}`))
	if !errors.Is(err, domain.ErrInvalidSessionUpdate) {
		t.Fatalf("got %v, want ErrInvalidSessionUpdate", err)
	}
}

// TestAFailedWriteKeepsTheCountersForTheNextFlush: a disk that was full for a
// minute must cost a minute of latency in a chart, not a hole in a release's
// numbers.
func TestAFailedWriteKeepsTheCountersForTheNextFlush(t *testing.T) {
	repo := newFakeHealthRepo()
	now := time.Date(2026, 8, 29, 14, 30, 0, 0, time.UTC)
	health := newTestHealth(repo, now, 0)

	if _, err := health.Accept(t.Context(), 1, "session",
		sessionPayload("s1", "crashed", 1, now)); err != nil {
		t.Fatalf("accepting: %v", err)
	}

	repo.failNext = errors.New("disk full")
	if _, err := health.Flush(t.Context()); err == nil {
		t.Fatal("a failed write reported success")
	}
	if _, err := health.Flush(t.Context()); err != nil {
		t.Fatalf("the retry failed: %v", err)
	}

	got := repo.buckets[domain.HealthKey{
		ProjectID: 1, Release: "shop@1.4.2", Environment: "production", Hour: "2026-08-29T14",
	}]
	want := domain.SessionCounts{Started: 1, Crashed: 1}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// TestAnEmptyFlushWritesNothing keeps a store with one writer from taking a
// write transaction a minute for a subsystem nothing is using.
func TestAnEmptyFlushWritesNothing(t *testing.T) {
	repo := newFakeHealthRepo()
	health := newTestHealth(repo, time.Now().UTC(), 0)

	written, err := health.Flush(t.Context())
	if err != nil || written != 0 {
		t.Fatalf("flush wrote %d buckets (err %v), want none", written, err)
	}
	if repo.writes != 0 {
		t.Fatalf("%d write transactions for nothing", repo.writes)
	}
}

// TestDrainWritesWhatIsKnownAndSaysWhatIsLost is the clean-stop half of
// ADR 008: a crash loses the sessions in flight, a clean stop must not lose
// the verdicts already reached.
func TestDrainWritesWhatIsKnownAndSaysWhatIsLost(t *testing.T) {
	repo := newFakeHealthRepo()
	now := time.Date(2026, 8, 29, 14, 30, 0, 0, time.UTC)
	health := newTestHealth(repo, now, 0)

	if _, err := health.Accept(t.Context(), 1, "session",
		sessionPayload("finished", "exited", 0, now)); err != nil {
		t.Fatalf("accepting: %v", err)
	}
	if _, err := health.Accept(t.Context(), 1, "session",
		sessionPayload("running", "ok", 0, now)); err != nil {
		t.Fatalf("accepting: %v", err)
	}

	if err := health.Drain(t.Context()); err != nil {
		t.Fatalf("draining: %v", err)
	}

	key := domain.HealthKey{
		ProjectID: 1, Release: "shop@1.4.2", Environment: "production", Hour: "2026-08-29T14",
	}
	if got := repo.buckets[key].Started; got != 1 {
		t.Fatalf("the drain wrote %d sessions, want the one that finished", got)
	}
	if inFlight := health.Window().InFlight; inFlight != 1 {
		t.Fatalf("%d sessions still in flight, want the one that never ended", inFlight)
	}
}

func TestADrainThatCannotWriteReportsIt(t *testing.T) {
	repo := newFakeHealthRepo()
	now := time.Date(2026, 8, 29, 14, 30, 0, 0, time.UTC)
	health := newTestHealth(repo, now, 0)

	if _, err := health.Accept(t.Context(), 1, "session",
		sessionPayload("s1", "exited", 0, now)); err != nil {
		t.Fatalf("accepting: %v", err)
	}
	repo.failNext = errors.New("disk full")
	if err := health.Drain(t.Context()); err == nil {
		t.Fatal("a drain that could not write reported success")
	}
}

// TestTheWindowIsReportedWithEveryAnswer: the caveat has to be readable beside
// the number it applies to, not on a diagnostics endpoint nobody opens.
func TestTheWindowIsReportedWithEveryAnswer(t *testing.T) {
	repo := newFakeHealthRepo()
	now := time.Date(2026, 8, 29, 14, 30, 0, 0, time.UTC)
	health := newTestHealth(repo, now, 2)

	for index := range 10 {
		if _, err := health.Accept(t.Context(), 1, "session",
			sessionPayload(fmt.Sprintf("s%d", index), "ok", 0, now)); err != nil {
			t.Fatalf("accepting: %v", err)
		}
	}
	if _, err := health.Flush(t.Context()); err != nil {
		t.Fatalf("flushing: %v", err)
	}

	window := domain.Window24h.Range(now)
	release, err := health.Release(t.Context(), 1, "shop@1.4.2", window)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if release.Window.Capacity != 2 || release.Window.Evicted != 8 {
		t.Fatalf("window %+v, want capacity 2 and 8 evictions", release.Window)
	}
	if !strings.Contains(release.Window.Note, "restart") {
		t.Fatalf("the volatility note does not mention a restart: %q", release.Window.Note)
	}

	project, err := health.Project(t.Context(), 1, window, 0)
	if err != nil {
		t.Fatalf("reading the project: %v", err)
	}
	if project.Window.Note != release.Window.Note {
		t.Fatal("the two endpoints disagree about the caveat")
	}
}

// TestASeriesCoversEveryHourOfTheRange: a chart drawn only from the hours that
// had traffic draws a quiet night as a straight line between two spikes.
func TestASeriesCoversEveryHourOfTheRange(t *testing.T) {
	repo := newFakeHealthRepo()
	now := time.Date(2026, 8, 29, 14, 30, 0, 0, time.UTC)
	health := newTestHealth(repo, now, 0)

	if _, err := health.Accept(t.Context(), 1, "session",
		sessionPayload("s1", "crashed", 0, now)); err != nil {
		t.Fatalf("accepting: %v", err)
	}
	if _, err := health.Flush(t.Context()); err != nil {
		t.Fatalf("flushing: %v", err)
	}

	window := domain.Window24h.Range(now)
	release, err := health.Release(t.Context(), 1, "shop@1.4.2", window)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if len(release.Series) != len(window.Buckets()) {
		t.Fatalf("the series has %d points for a %d-hour range",
			len(release.Series), len(window.Buckets()))
	}

	quiet, busy := 0, 0
	for _, point := range release.Series {
		if point.Started == 0 {
			if point.CrashFreeRate != nil {
				t.Fatalf("an hour with no sessions reported a rate: %+v", point)
			}
			quiet++
			continue
		}
		busy++
		if point.CrashFreeRate == nil || *point.CrashFreeRate != 0 {
			t.Fatalf("the crashing hour reported %v, want 0", point.CrashFreeRate)
		}
	}
	if busy != 1 || quiet == 0 {
		t.Fatalf("%d busy and %d quiet hours", busy, quiet)
	}
	if release.CrashFreeRate == nil || *release.CrashFreeRate != 0 {
		t.Fatalf("total crash-free rate %v, want 0", release.CrashFreeRate)
	}
}

func TestAReleaseWithNoVersionIsRefused(t *testing.T) {
	health := newTestHealth(newFakeHealthRepo(), time.Now().UTC(), 0)
	_, err := health.Release(t.Context(), 1, "", domain.Window24h.Range(time.Now().UTC()))
	if !errors.Is(err, domain.ErrInvalidRelease) {
		t.Fatalf("got %v, want ErrInvalidRelease", err)
	}
}

func TestProjectHealthRanksReleasesAndTotalsThem(t *testing.T) {
	repo := newFakeHealthRepo()
	now := time.Date(2026, 8, 29, 14, 30, 0, 0, time.UTC)
	health := newTestHealth(repo, now, 0)

	repo.buckets[domain.HealthKey{ProjectID: 1, Release: "a@1", Hour: "2026-08-29T14"}] =
		domain.SessionCounts{Started: 100, Crashed: 5}
	repo.buckets[domain.HealthKey{ProjectID: 1, Release: "b@2", Hour: "2026-08-29T13"}] =
		domain.SessionCounts{Started: 10}

	project, err := health.Project(t.Context(), 1, domain.Window24h.Range(now), 0)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if len(project.Releases) != 2 {
		t.Fatalf("got %d releases, want 2", len(project.Releases))
	}
	if project.Started != 110 || project.Crashed != 5 {
		t.Fatalf("totals %+v", project.SessionCounts)
	}
	if project.CrashFreeRate == nil || *project.CrashFreeRate < 0.954 || *project.CrashFreeRate > 0.955 {
		t.Fatalf("crash-free rate %v, want about 0.9545", project.CrashFreeRate)
	}
}

// TestTheFlushJobOnlyExistsWhenAProjectAcceptsSessions is ADR 005 stated as a
// test: a subsystem nobody switched on has no goroutine and no timer.
func TestTheFlushJobOnlyExistsWhenAProjectAcceptsSessions(t *testing.T) {
	repo := newFakeHealthRepo()
	health := newTestHealth(repo, time.Now().UTC(), 0)

	wanted, err := health.HasWork(t.Context())
	if err != nil || wanted {
		t.Fatalf("HasWork = %v (err %v) with sessions off", wanted, err)
	}
	repo.sessions = true
	if wanted, _ := health.HasWork(t.Context()); !wanted {
		t.Fatal("HasWork = false with a project accepting sessions")
	}

	job := health.Job()
	if job.Name() != "session-flush" {
		t.Fatalf("job name %q", job.Name())
	}
	if job.Interval() != SessionFlushInterval {
		t.Fatalf("interval %v, want %v", job.Interval(), SessionFlushInterval)
	}
	if err := job.Run(t.Context()); err != nil {
		t.Fatalf("running the job: %v", err)
	}

	repo.failNext = errors.New("disk full")
	if _, err := health.Accept(t.Context(), 1, "session",
		sessionPayload("s1", "exited", 0, time.Now().UTC())); err != nil {
		t.Fatalf("accepting: %v", err)
	}
	if err := job.Run(t.Context()); err == nil {
		t.Fatal("a job whose write failed reported success")
	}
}

// TestASilentSessionIsCountedByTheFlush ties the TTL to the thing that
// actually applies it: a client that vanished must not hold a window slot
// forever, and what is counted for it is what is known.
func TestASilentSessionIsCountedByTheFlush(t *testing.T) {
	repo := newFakeHealthRepo()
	started := time.Date(2026, 8, 29, 14, 0, 0, 0, time.UTC)
	health := newTestHealth(repo, started, 0)

	if _, err := health.Accept(t.Context(), 1, "session",
		sessionPayload("gone", "ok", 0, started)); err != nil {
		t.Fatalf("accepting: %v", err)
	}
	if written, err := health.Flush(t.Context()); err != nil || written != 0 {
		t.Fatalf("a running session was written down (%d buckets, err %v)", written, err)
	}

	health.clock = fixedClock{now: started.Add(domain.SessionTTL + time.Minute)}
	if _, err := health.Flush(t.Context()); err != nil {
		t.Fatalf("flushing: %v", err)
	}

	got := repo.buckets[domain.HealthKey{
		ProjectID: 1, Release: "shop@1.4.2", Environment: "production", Hour: "2026-08-29T14",
	}]
	if got.Started != 1 || got.Crashed != 0 {
		t.Fatalf("an expired session counted as %+v", got)
	}
	if health.Window().Expired != 1 {
		t.Fatalf("the window reports %d expirations", health.Window().Expired)
	}
}

// TestConcurrentSessionsAndFlushes runs the window the way the server does.
//
// Ingest is the one genuinely concurrent path in this product: several requests
// decode and count at once while the flush job writes underneath them. The
// window itself is a plain data structure with no lock — deliberately, so the
// awkward rules in it are testable without one — which puts the whole burden on
// this use case holding the mutex correctly. Under `-race` that is a claim a
// test can check rather than a comment.
func TestConcurrentSessionsAndFlushes(t *testing.T) {
	repo := newFakeHealthRepo()
	now := time.Date(2026, 8, 29, 14, 30, 0, 0, time.UTC)
	health := newTestHealth(repo, now, 200)

	const senders = 8
	const each = 100

	var sending sync.WaitGroup
	for sender := range senders {
		sending.Add(1)
		go func() {
			defer sending.Done()
			for index := range each {
				sid := fmt.Sprintf("s%d-%d", sender, index)
				if _, err := health.Accept(t.Context(), 1, "session",
					sessionPayload(sid, "ok", 0, now)); err != nil {
					t.Errorf("accepting: %v", err)
					return
				}
				if _, err := health.Accept(t.Context(), 1, "session",
					sessionPayload(sid, "exited", 0, now)); err != nil {
					t.Errorf("accepting: %v", err)
					return
				}
			}
		}()
	}

	// A flusher running alongside, as the scheduler's job does.
	done := make(chan struct{})
	var flushing sync.WaitGroup
	flushing.Add(1)
	go func() {
		defer flushing.Done()
		for {
			select {
			case <-done:
				return
			default:
				if _, err := health.Flush(context.Background()); err != nil {
					t.Errorf("flushing: %v", err)
					return
				}
			}
		}
	}()

	sending.Wait()
	close(done)
	flushing.Wait()

	if _, err := health.Flush(t.Context()); err != nil {
		t.Fatalf("the last flush failed: %v", err)
	}

	got := repo.buckets[domain.HealthKey{
		ProjectID: 1, Release: "shop@1.4.2", Environment: "production", Hour: "2026-08-29T14",
	}]
	// Exactly once each, no matter how the flushes interleaved: every session
	// here ends, and a session that ends is settled once and forgotten.
	if got.Started != senders*each {
		t.Fatalf("counted %d sessions, want %d", got.Started, senders*each)
	}
	if got.Crashed != 0 || got.Errored != 0 || got.Abnormal != 0 {
		t.Fatalf("clean sessions counted as something else: %+v", got)
	}
	if inFlight := health.Window().InFlight; inFlight != 0 {
		t.Fatalf("%d sessions left in flight", inFlight)
	}
}
