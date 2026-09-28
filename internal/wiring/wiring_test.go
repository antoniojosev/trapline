// The cables, tested as cables.
//
// Everything this package connects is covered on both ends already: the
// scheduler knows how to start a job when its condition turns true, the alert
// use case knows how to call something after a channel is written, the digest
// knows how to answer whether any channel asked for it. What nothing covered
// was the wire between them — and a wire that is not connected looks exactly
// like one that is, until an operator ticks a box and nothing happens until
// the next restart. That was written down as debt when the scheduler was
// built, to be closed once a genuinely conditional job
// existed. Two of them now do.
package wiring_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/adapters/sqlite"
	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/usecase"
	"github.com/antoniojosev/trapline/internal/wiring"
)

// slackConfig is a channel that is valid and delivers nowhere anybody will
// notice: nothing in these tests sends, and the endpoint only has to parse.
func slackConfig() domain.ChannelConfig {
	return domain.ChannelConfig{URL: "https://hooks.slack.com/services/T0/B0/xxxx"}
}

func newStack(t *testing.T) *wiring.Stack {
	t.Helper()
	ctx := context.Background()

	directory := t.TempDir()
	db, err := sqlite.Open(ctx, filepath.Join(directory, "trapline.db"))
	if err != nil {
		t.Fatalf("opening the database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	stack := wiring.New(db, wiring.Options{
		Origin:        domain.Origin{Scheme: "https", Host: "errors.example.test"},
		SecretKeyPath: filepath.Join(directory, "trapline.key"),
	})
	if err := stack.Scheduler.Start(ctx); err != nil {
		t.Fatalf("starting the jobs: %v", err)
	}
	t.Cleanup(stack.Scheduler.Stop)
	return stack
}

// running reports whether a job exists right now.
//
// Exists, not "is enabled": a job whose subsystem has nothing to do has no
// row here at all, which is the claim ADR 005 makes and the thing these tests
// are checking (ADR 014).
func running(t *testing.T, stack *wiring.Stack, name string) bool {
	t.Helper()
	for _, status := range stack.Scheduler.Jobs() {
		if status.Name == name {
			return true
		}
	}
	return false
}

// waitFor gives the scheduler a moment to act on a re-evaluation.
//
// Re-evaluation is deliberately non-blocking for its caller — an operator's
// PUT must not wait on somebody else's disk — so the state it produces is
// observed rather than returned. The wait is short and the failure is the
// timeout, not a flake: if the wire is connected this settles in microseconds.
func waitFor(t *testing.T, condition func() bool, complaint string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(complaint)
}

// TestTheNotifierAppearsWithTheFirstChannel is ADR 014's claim about the
// subsystem nobody has switched on: no channel, no goroutine.
func TestTheNotifierAppearsWithTheFirstChannel(t *testing.T) {
	ctx := context.Background()
	stack := newStack(t)

	if running(t, stack, "notifier") {
		t.Fatal("the notifier is running on an installation with no channels")
	}
	if !running(t, stack, "retention") {
		t.Error("retention is not running, and it is the one job nobody opts into")
	}

	channel, err := stack.Alerts.AddChannel(ctx, domain.ChannelSlack, "#alerts", slackConfig(), false)
	if err != nil {
		t.Fatalf("adding a channel: %v", err)
	}
	waitFor(t, func() bool { return running(t, stack, "notifier") },
		"the notifier never started after a channel was added, so alerting only begins at the next restart")

	if err := stack.Alerts.RemoveChannel(ctx, channel.ID); err != nil {
		t.Fatalf("removing the channel: %v", err)
	}
	waitFor(t, func() bool { return !running(t, stack, "notifier") },
		"the notifier is still running with no channel left")
}

// TestTheDigestJobFollowsTheDigestFlag is the wire the digest depends on.
//
// A channel is not enough: the digest job's condition is that some channel
// asked for the weekly report, which is a different question from "is
// alerting configured at all". Getting this wrong in either direction is
// quiet — a job that runs for nobody, or a report somebody ticked a box for
// that never arrives.
func TestTheDigestJobFollowsTheDigestFlag(t *testing.T) {
	ctx := context.Background()
	stack := newStack(t)

	if running(t, stack, "digest") {
		t.Fatal("the digest job is running on an installation with no channels")
	}

	plain, err := stack.Alerts.AddChannel(ctx, domain.ChannelSlack, "#alerts", slackConfig(), false)
	if err != nil {
		t.Fatalf("adding a channel: %v", err)
	}
	// The notifier is proof the re-evaluation happened at all, so the
	// assertion below is about the digest's condition rather than about
	// whether anything was re-evaluated.
	waitFor(t, func() bool { return running(t, stack, "notifier") }, "the notifier never started")
	if running(t, stack, "digest") {
		t.Error("a channel that did not ask for the weekly report started the digest job anyway")
	}

	weekly, err := stack.Alerts.AddChannel(ctx, domain.ChannelSlack, "#weekly", slackConfig(), true)
	if err != nil {
		t.Fatalf("adding a digest channel: %v", err)
	}
	waitFor(t, func() bool { return running(t, stack, "digest") },
		"a channel asked for the weekly report and the job did not start until a restart")

	if err := stack.Alerts.RemoveChannel(ctx, weekly.ID); err != nil {
		t.Fatalf("removing the digest channel: %v", err)
	}
	waitFor(t, func() bool { return !running(t, stack, "digest") },
		"the digest job outlived the last channel that wanted it")
	if !running(t, stack, "notifier") {
		t.Error("removing one channel of two stopped the notifier")
	}

	if err := stack.Alerts.RemoveChannel(ctx, plain.ID); err != nil {
		t.Fatalf("removing the remaining channel: %v", err)
	}
}

// TestTheDigestIsPreviewableBeforeAnyChannelExists is the other half of
// ADR 035's rule, and the order people actually work in: see what the report
// says, then decide where to send it.
func TestTheDigestIsPreviewableBeforeAnyChannelExists(t *testing.T) {
	stack := newStack(t)

	if _, text, err := stack.Digest.Preview(context.Background(), usecase.PreviewOptions{}); err != nil {
		t.Fatalf("previewing with no channels: %v", err)
	} else if text == "" {
		t.Error("the preview rendered nothing")
	}
}

// TestTheDigestReachesTheOutbox proves the second half of the seam: handing a
// rendered report over puts a row in the same outbox every alert goes
// through, rather than sending it from the job and losing it if the far end
// is down (ADR 015).
func TestTheDigestReachesTheOutbox(t *testing.T) {
	ctx := context.Background()
	stack := newStack(t)

	if _, err := stack.Alerts.AddChannel(ctx, domain.ChannelSlack, "#weekly", slackConfig(), true); err != nil {
		t.Fatalf("adding a digest channel: %v", err)
	}

	sent, err := stack.Digest.Send(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("sending the digest: %v", err)
	}
	if sent != 1 {
		t.Fatalf("the digest was queued for %d channels, want 1", sent)
	}

	queued, err := stack.Alerts.Notifications(ctx, string(domain.NotificationPending), 10)
	if err != nil {
		t.Fatalf("reading the delivery log: %v", err)
	}
	if len(queued) != 1 {
		t.Fatalf("the outbox holds %d rows, want 1", len(queued))
	}
	if queued[0].Payload.Event != domain.EventDigest {
		t.Errorf("the queued row is a %q, want %q", queued[0].Payload.Event, domain.EventDigest)
	}
	if queued[0].Payload.Body == "" {
		t.Error("the queued digest carries no body, so a retry would deliver an empty message")
	}
	if queued[0].Payload.Text() != queued[0].Payload.Body {
		t.Error("the delivered text is not the rendered report")
	}
}
