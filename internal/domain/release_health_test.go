package domain_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

func at(hour, minute int) time.Time {
	return time.Date(2026, 8, 29, hour, minute, 0, 0, time.UTC)
}

func update(id string, status domain.SessionStatus) domain.SessionUpdate {
	return domain.SessionUpdate{
		ID:          id,
		ProjectID:   1,
		Release:     "shop@1.4.2",
		Environment: "production",
		Started:     at(14, 30),
		Status:      status,
	}
}

func key(hour string) domain.HealthKey {
	return domain.HealthKey{
		ProjectID: 1, Release: "shop@1.4.2", Environment: "production", Hour: hour,
	}
}

// TestASessionIsCountedOnceHoweverManyUpdatesItSends is the claim the whole
// window exists for. An SDK sends an init and then an ending, and counting
// updates instead of sessions would multiply every release's numbers by
// however often its SDK flushes.
func TestASessionIsCountedOnceHoweverManyUpdatesItSends(t *testing.T) {
	window := domain.NewSessionWindow(100, time.Hour)

	for _, status := range []domain.SessionStatus{domain.SessionOK, domain.SessionOK, domain.SessionExited} {
		if err := window.Observe(update("s1", status), at(14, 31)); err != nil {
			t.Fatalf("observing: %v", err)
		}
	}

	counts := window.Drain()[key("2026-08-29T14")]
	if counts.Started != 1 {
		t.Fatalf("three updates about one session counted %d sessions, want 1", counts.Started)
	}
	if counts.Crashed != 0 || counts.Errored != 0 || counts.Abnormal != 0 {
		t.Fatalf("a clean session was counted as something else: %+v", counts)
	}
}

// TestTheWorstOutcomeWins pins the merge rule for updates that arrive before
// the session is settled. A session that both crashed and exited crashed;
// taking the last update to arrive would make the crash-free rate depend on
// network timing.
func TestTheWorstOutcomeWins(t *testing.T) {
	window := domain.NewSessionWindow(100, time.Hour)

	crashing := update("s1", domain.SessionCrashed)
	crashing.Status = domain.SessionOK
	crashing.Errors = 1
	if err := window.Observe(crashing, at(14, 30)); err != nil {
		t.Fatalf("observing: %v", err)
	}
	crashing.Status = domain.SessionCrashed
	if err := window.Observe(crashing, at(14, 31)); err != nil {
		t.Fatalf("observing: %v", err)
	}

	got := window.Drain()[key("2026-08-29T14")]
	want := domain.SessionCounts{Started: 1, Crashed: 1}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// TestATerminalUpdateSettlesAndForgets documents the one imprecision this
// design accepts on purpose, so that nobody later reads it as a bug.
//
// A terminal update ends a session, so its entry is settled and dropped rather
// than held until the TTL. An update about that same id afterwards therefore
// starts a fresh session and is counted again. The alternative is a tombstone
// per finished session, which is memory held for something already decided —
// and under load it would fill the window with the dead and evict the living,
// costing precision exactly where the window is meant to protect it. The SDKs
// send a session's updates in order over one connection, so what this gives up
// is a duplicate that needs a retry to arrive out of order at all (ADR 008).
func TestATerminalUpdateSettlesAndForgets(t *testing.T) {
	window := domain.NewSessionWindow(100, time.Hour)

	if err := window.Observe(update("s1", domain.SessionCrashed), at(14, 30)); err != nil {
		t.Fatalf("observing: %v", err)
	}
	if window.InFlight() != 0 {
		t.Fatalf("a finished session is still occupying the window")
	}
	if err := window.Observe(update("s1", domain.SessionExited), at(14, 31)); err != nil {
		t.Fatalf("observing: %v", err)
	}

	got := window.Drain()[key("2026-08-29T14")]
	want := domain.SessionCounts{Started: 2, Crashed: 1}
	if got != want {
		t.Fatalf("got %+v, want %+v — the accepted double count changed shape", got, want)
	}
}

// TestAnErroredSessionIsNotACrash keeps the four counters disjoint, which is
// what makes `healthy = started - errored - crashed - abnormal` true.
func TestAnErroredSessionIsNotACrash(t *testing.T) {
	window := domain.NewSessionWindow(100, time.Hour)

	errored := update("s1", domain.SessionOK)
	errored.Errors = 3
	if err := window.Observe(errored, at(14, 31)); err != nil {
		t.Fatalf("observing: %v", err)
	}
	if err := window.Observe(update("s1", domain.SessionExited), at(14, 32)); err != nil {
		t.Fatalf("observing: %v", err)
	}

	counts := window.Drain()[key("2026-08-29T14")]
	want := domain.SessionCounts{Started: 1, Errored: 1}
	if counts != want {
		t.Fatalf("got %+v, want %+v", counts, want)
	}
	if counts.Healthy() != 0 {
		t.Fatalf("an errored session counted as healthy: %d", counts.Healthy())
	}

	crashed := update("s2", domain.SessionCrashed)
	crashed.Errors = 1
	if err := window.Observe(crashed, at(14, 33)); err != nil {
		t.Fatalf("observing: %v", err)
	}
	counts = window.Drain()[key("2026-08-29T14")]
	if counts.Errored != 0 || counts.Crashed != 1 {
		t.Fatalf("a crash was counted twice: %+v", counts)
	}
}

// TestTheHourIsWhenTheSessionStarted is what makes "the 14:00 release was bad"
// a statement about the release rather than about when people closed their
// laptops.
func TestTheHourIsWhenTheSessionStarted(t *testing.T) {
	window := domain.NewSessionWindow(100, time.Hour)

	begun := update("s1", domain.SessionOK)
	begun.Started = at(13, 58)
	if err := window.Observe(begun, at(13, 58)); err != nil {
		t.Fatalf("observing: %v", err)
	}
	ended := begun
	ended.Status = domain.SessionCrashed
	if err := window.Observe(ended, at(14, 3)); err != nil {
		t.Fatalf("observing: %v", err)
	}

	drained := window.Drain()
	if _, found := drained[key("2026-08-29T14")]; found {
		t.Fatal("a session that began at 13:58 was counted in the 14:00 hour")
	}
	if drained[key("2026-08-29T13")].Crashed != 1 {
		t.Fatalf("the 13:00 hour did not get the crash: %+v", drained)
	}
}

// TestAFullWindowSacrificesPrecisionAndNotStability is ADR 008's promise as a
// test: a thousand sessions into a window of a hundred must leave the window
// at its ceiling, must count every session that arrived, and must not lose the
// newest arrivals — a window that discarded the incoming update would stop
// counting new sessions entirely under load.
func TestAFullWindowSacrificesPrecisionAndNotStability(t *testing.T) {
	const capacity = 100
	const sessions = 1000

	window := domain.NewSessionWindow(capacity, time.Hour)
	for index := range sessions {
		open := update(fmt.Sprintf("s%d", index), domain.SessionOK)
		if err := window.Observe(open, at(14, 30)); err != nil {
			t.Fatalf("observing session %d: %v", index, err)
		}
		if window.InFlight() > capacity {
			t.Fatalf("the window grew to %d, past its ceiling of %d", window.InFlight(), capacity)
		}
	}

	if window.InFlight() != capacity {
		t.Fatalf("the window holds %d sessions, want it full at %d", window.InFlight(), capacity)
	}
	if want := int64(sessions - capacity); window.Evicted() != want {
		t.Fatalf("evicted %d sessions, want %d", window.Evicted(), want)
	}

	// Every evicted session was still counted. Precision was lost — none of
	// them will ever report how it ended — but no session was forgotten.
	counts := window.Drain()[key("2026-08-29T14")]
	if want := int64(sessions - capacity); counts.Started != want {
		t.Fatalf("counted %d of the %d evicted sessions", counts.Started, want)
	}

	// The newest session is the one still there, not the one thrown away.
	last := update(fmt.Sprintf("s%d", sessions-1), domain.SessionCrashed)
	if err := window.Observe(last, at(14, 31)); err != nil {
		t.Fatalf("observing: %v", err)
	}
	if crashed := window.Drain()[key("2026-08-29T14")].Crashed; crashed != 1 {
		t.Fatalf("the newest session was the one evicted; its crash did not land")
	}
}

// TestEvictionTakesTheLeastRecentlyTouched pins the L in LRU. A session that
// keeps sending heartbeats must outlive one that went quiet, or a long-lived
// session would be evicted at exactly the moment it was busiest.
func TestEvictionTakesTheLeastRecentlyTouched(t *testing.T) {
	window := domain.NewSessionWindow(2, time.Hour)

	if err := window.Observe(update("old", domain.SessionOK), at(14, 0)); err != nil {
		t.Fatalf("observing: %v", err)
	}
	if err := window.Observe(update("busy", domain.SessionOK), at(14, 1)); err != nil {
		t.Fatalf("observing: %v", err)
	}
	// "busy" speaks again, so "old" becomes the least recently touched.
	if err := window.Observe(update("busy", domain.SessionOK), at(14, 2)); err != nil {
		t.Fatalf("observing: %v", err)
	}
	if err := window.Observe(update("new", domain.SessionOK), at(14, 3)); err != nil {
		t.Fatalf("observing: %v", err)
	}

	// Only "old" left, so only its crash is lost.
	if err := window.Observe(update("busy", domain.SessionCrashed), at(14, 4)); err != nil {
		t.Fatalf("observing: %v", err)
	}
	if crashed := window.Drain()[key("2026-08-29T14")].Crashed; crashed != 1 {
		t.Fatal("the session that kept reporting was the one evicted")
	}
}

// TestASilentSessionIsSettledNotRemembered stops a client that vanished from
// occupying the window forever.
func TestASilentSessionIsSettledNotRemembered(t *testing.T) {
	window := domain.NewSessionWindow(100, 30*time.Minute)

	if err := window.Observe(update("gone", domain.SessionOK), at(14, 0)); err != nil {
		t.Fatalf("observing: %v", err)
	}
	if err := window.Observe(update("alive", domain.SessionOK), at(14, 20)); err != nil {
		t.Fatalf("observing: %v", err)
	}

	if expired := window.Expire(at(14, 35)); expired != 1 {
		t.Fatalf("expired %d sessions, want 1", expired)
	}
	if window.InFlight() != 1 {
		t.Fatalf("%d sessions left in flight, want 1", window.InFlight())
	}

	counts := window.Drain()[key("2026-08-29T14")]
	want := domain.SessionCounts{Started: 1}
	if counts != want {
		t.Fatalf("an expired session counted as %+v, want %+v", counts, want)
	}
	if window.Expired() != 1 {
		t.Fatalf("the window reports %d expirations, want 1", window.Expired())
	}
}

// TestDrainLeavesSessionsStillInFlightAlone is what stops a session being
// counted twice: once by the flush that happened while it was running and once
// when it ends.
func TestDrainLeavesSessionsStillInFlightAlone(t *testing.T) {
	window := domain.NewSessionWindow(100, time.Hour)

	if err := window.Observe(update("running", domain.SessionOK), at(14, 0)); err != nil {
		t.Fatalf("observing: %v", err)
	}
	if drained := window.Drain(); drained != nil {
		t.Fatalf("a running session was written down: %+v", drained)
	}
	if err := window.Observe(update("running", domain.SessionExited), at(14, 5)); err != nil {
		t.Fatalf("observing: %v", err)
	}
	if started := window.Drain()[key("2026-08-29T14")].Started; started != 1 {
		t.Fatalf("counted %d sessions across two drains, want 1", started)
	}
}

// TestAnAggregateNeverEntersTheWindow: a `sessions` item carries no session
// ids, so there is nothing to deduplicate and no ending to wait for.
func TestAnAggregateNeverEntersTheWindow(t *testing.T) {
	window := domain.NewSessionWindow(2, time.Hour)

	counts := domain.SessionCounts{Started: 400, Errored: 12, Crashed: 8, Abnormal: 1}
	window.AddAggregate(key("2026-08-29T14"), counts)
	window.AddAggregate(key("2026-08-29T14"), counts)
	window.AddAggregate(key("2026-08-29T14"), domain.SessionCounts{})

	if window.InFlight() != 0 {
		t.Fatalf("an aggregate occupied %d window slots", window.InFlight())
	}
	got := window.Drain()[key("2026-08-29T14")]
	want := counts.Add(counts)
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// TestPendingCountsSurviveAFailedWrite: a disk that was full for a minute must
// cost a minute of latency in a chart, not a hole in a release's numbers.
func TestPendingCountsSurviveAFailedWrite(t *testing.T) {
	window := domain.NewSessionWindow(10, time.Hour)
	if err := window.Observe(update("s1", domain.SessionCrashed), at(14, 0)); err != nil {
		t.Fatalf("observing: %v", err)
	}

	rejected := window.Drain()
	for bucket, counts := range rejected {
		window.AddPending(bucket, counts)
	}
	window.AddPending(key("2026-08-29T14"), domain.SessionCounts{})

	if got := window.Drain()[key("2026-08-29T14")].Crashed; got != 1 {
		t.Fatalf("the crash was lost when the write failed: %d", got)
	}
	if window.Pending() != 0 {
		t.Fatalf("%d buckets left pending after a drain", window.Pending())
	}
}

// TestAnUncountableUpdateIsRefused: the release is what a session is
// attributed to, so a session without one answers no question this table
// exists for.
func TestAnUncountableUpdateIsRefused(t *testing.T) {
	window := domain.NewSessionWindow(10, time.Hour)

	for _, test := range []struct {
		name    string
		mutate  func(*domain.SessionUpdate)
		wantErr error
	}{
		{"no id", func(u *domain.SessionUpdate) { u.ID = "  " }, domain.ErrInvalidSessionUpdate},
		{"no project", func(u *domain.SessionUpdate) { u.ProjectID = 0 }, domain.ErrInvalidSessionUpdate},
		{"no release", func(u *domain.SessionUpdate) { u.Release = "" }, domain.ErrInvalidSessionUpdate},
		{"no start", func(u *domain.SessionUpdate) { u.Started = time.Time{} }, domain.ErrInvalidSessionUpdate},
	} {
		t.Run(test.name, func(t *testing.T) {
			broken := update("s1", domain.SessionOK)
			test.mutate(&broken)
			err := window.Observe(broken, at(14, 0))
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("got %v, want %v", err, test.wantErr)
			}
		})
	}
	if window.InFlight() != 0 {
		t.Fatalf("a refused update left %d entries behind", window.InFlight())
	}
}

func TestCrashFreeRateIsUnknownWithoutSessions(t *testing.T) {
	if _, known := (domain.SessionCounts{}).CrashFreeRate(); known {
		t.Fatal("an empty bucket reported a crash-free rate")
	}

	counts := domain.SessionCounts{Started: 100, Crashed: 5}
	rate, known := counts.CrashFreeRate()
	if !known {
		t.Fatal("a bucket with sessions reported no rate")
	}
	if rate != 0.95 {
		t.Fatalf("crash-free rate %v, want 0.95", rate)
	}
	if counts.Healthy() != 95 {
		t.Fatalf("healthy %d, want 95", counts.Healthy())
	}
	// Counters written by an older build, or edited by hand. Reporting a
	// negative count of healthy sessions would be worse than reporting none.
	if broken := (domain.SessionCounts{Started: 1, Crashed: 5}).Healthy(); broken != 0 {
		t.Fatalf("healthy %d from impossible counters, want 0", broken)
	}
	if (domain.SessionCounts{}).Empty() != true {
		t.Fatal("an empty bucket did not report itself empty")
	}
}

func TestParseSessionStatus(t *testing.T) {
	// A table rather than a map literal, so that the whitespace and the case
	// in "  EXITED " are read as the deliberate test they are: an SDK is free
	// to send either, and normalising here is cheaper than a release whose
	// sessions all counted as still running.
	for _, test := range []struct {
		raw  string
		want domain.SessionStatus
	}{
		{"", domain.SessionOK},
		{"ok", domain.SessionOK},
		{"  EXITED ", domain.SessionExited},
		{"crashed", domain.SessionCrashed},
		{"abnormal", domain.SessionAbnormal},
		{"something-newer", domain.SessionOK},
	} {
		raw, want := test.raw, test.want
		if got := domain.ParseSessionStatus(raw); got != want {
			t.Fatalf("ParseSessionStatus(%q) = %q, want %q", raw, got, want)
		}
	}
	if domain.SessionOK.Terminal() {
		t.Fatal("ok is not a terminal status")
	}
	for _, status := range []domain.SessionStatus{
		domain.SessionExited, domain.SessionCrashed, domain.SessionAbnormal,
	} {
		if !status.Terminal() {
			t.Fatalf("%q should be terminal", status)
		}
	}
}

// TestAWindowWithoutBoundsGetsTheDefaultOnes: a misconfigured installation
// must get the bounded behaviour, never the unbounded one.
func TestAWindowWithoutBoundsGetsTheDefaultOnes(t *testing.T) {
	window := domain.NewSessionWindow(0, 0)
	if window.Capacity() != domain.DefaultSessionWindowSize {
		t.Fatalf("capacity %d, want %d", window.Capacity(), domain.DefaultSessionWindowSize)
	}
	if window.TTL() != domain.SessionTTL {
		t.Fatalf("ttl %v, want %v", window.TTL(), domain.SessionTTL)
	}
	if window.Settled() != 0 {
		t.Fatalf("a fresh window has settled %d sessions", window.Settled())
	}
}
