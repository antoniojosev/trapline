package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

func TestNotificationStatusesAreClosed(t *testing.T) {
	for _, status := range domain.AllNotificationStatuses() {
		if !status.Valid() {
			t.Errorf("%q is listed but does not validate", status)
		}
	}
	if domain.NotificationStatus("sending").Valid() {
		t.Error("a status the schema does not store validated")
	}
}

// TestRetryDelayIsBoundedAndGrowing is the schedule an outage is survived on.
// Unbounded doubling reaches days, and a notification two days late is not
// late, it is noise.
func TestRetryDelayIsBoundedAndGrowing(t *testing.T) {
	if got := domain.RetryDelay(1); got != domain.FirstRetryDelay {
		t.Errorf("the first retry waits %s, want %s", got, domain.FirstRetryDelay)
	}
	if got := domain.RetryDelay(0); got != domain.FirstRetryDelay {
		t.Errorf("attempt 0 waits %s, want the first delay: a caller off by one must not get zero", got)
	}
	if got := domain.RetryDelay(2); got != 2*domain.FirstRetryDelay {
		t.Errorf("the second retry waits %s, want %s", got, 2*domain.FirstRetryDelay)
	}

	previous := time.Duration(0)
	for attempt := 1; attempt <= domain.MaxNotificationAttempts; attempt++ {
		delay := domain.RetryDelay(attempt)
		if delay < previous {
			t.Errorf("attempt %d waits %s, less than the %s before it", attempt, delay, previous)
		}
		if delay > domain.MaxRetryDelay {
			t.Errorf("attempt %d waits %s, past the %s cap", attempt, delay, domain.MaxRetryDelay)
		}
		previous = delay
	}
	if domain.RetryDelay(domain.MaxNotificationAttempts) != domain.MaxRetryDelay {
		t.Error("the schedule never reaches its cap, so the later attempts are closer together than intended")
	}
}

func samplePayload() domain.AlertPayload {
	return domain.NewAlertPayload("deploys", &domain.AlertEvent{
		Kind:        domain.TriggerNewIssue,
		ProjectID:   2,
		IssueID:     14,
		Title:       "ValueError: boom",
		Culprit:     "app/views.py in checkout",
		Level:       domain.Level("error"),
		Release:     "app@1.0.0",
		Environment: "production",
		Count:       4,
		Baseline:    1,
		At:          time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC),
	}, "https://errors.example.test/projects/2/issues/14")
}

func TestAlertPayloadRoundTrips(t *testing.T) {
	payload := samplePayload()
	encoded, err := payload.Encode()
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	decoded, err := domain.DecodeAlertPayload(encoded)
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if decoded != payload {
		t.Errorf("the payload changed across a round trip:\n got %+v\nwant %+v", decoded, payload)
	}
}

func TestDecodeAlertPayloadRejectsRubbish(t *testing.T) {
	_, err := domain.DecodeAlertPayload([]byte("not json"))
	if err == nil {
		t.Fatal("a stored payload that is not JSON decoded")
	}
	if !errors.Is(err, domain.ErrInvalidAlert) {
		t.Errorf("error %v does not wrap ErrInvalidAlert", err)
	}
}

// TestTextCarriesTheLink is the difference between an alert and an
// interruption: without the URL, the message tells you something broke and
// leaves you to go and find it.
func TestTextCarriesTheLink(t *testing.T) {
	sample := samplePayload()
	text := sample.Text()
	for _, want := range []string{
		"New issue", "production", "ValueError: boom", "app/views.py in checkout",
		"app@1.0.0", "https://errors.example.test/projects/2/issues/14", "deploys",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the message does not mention %q:\n%s", want, text)
		}
	}
}

func TestHeadlineNamesTheTrigger(t *testing.T) {
	cases := map[domain.TriggerKind]string{
		domain.TriggerNewIssue:        "New issue",
		domain.TriggerRegression:      "Regression",
		domain.TriggerIssueSpike:      "Spike",
		domain.TriggerErrorRate:       "Error rate",
		domain.TriggerCronMissed:      "Cron missed",
		domain.TriggerCronTimeout:     "Cron timed out",
		domain.TriggerCronFailed:      "Cron failed",
		domain.TriggerCronRecovered:   "Cron recovered",
		domain.TriggerUptimeDown:      "Down",
		domain.TriggerUptimeRecovered: "Recovered",
		// The fallback: a kind this build does not know still produces a
		// headline rather than an empty line.
		domain.TriggerKind("cost_ceiling"): "cost_ceiling",
	}
	for kind, want := range cases {
		one := domain.AlertPayload{Event: kind}
		headline := one.Headline()
		if !strings.HasPrefix(headline, want) {
			t.Errorf("a %s headline is %q, want it to start with %q", kind, headline, want)
		}
	}
}

func TestTextOfAProjectWideAlert(t *testing.T) {
	payload := domain.NewAlertPayload("rate", &domain.AlertEvent{
		Kind: domain.TriggerErrorRate, ProjectID: 2, Count: 240,
		At: time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC),
	}, "")
	text := payload.Text()
	if !strings.Contains(text, "240") {
		t.Errorf("the rate is missing from the message:\n%s", text)
	}
	if strings.Contains(text, "baseline") {
		t.Errorf("a baseline of zero was rendered as a comparison:\n%s", text)
	}
}

// TestSignatureVerifies pins the format, because it is a published contract:
// somebody writes a receiver against docs/alerts/webhooks.md and this is what
// they check against.
func TestSignatureVerifies(t *testing.T) {
	const secret = "a-secret-long-enough"
	now := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	body := []byte(`{"event":"new_issue"}`)

	header := domain.SignWebhook(secret, now, body)
	if !strings.HasPrefix(header, "t=") || !strings.Contains(header, ",v1=") {
		t.Fatalf("the signature is %q, want t=<unix>,v1=<hex>", header)
	}
	if err := domain.VerifyWebhook(secret, header, body, now, domain.WebhookSignatureTolerance); err != nil {
		t.Fatalf("a signature this code produced did not verify: %v", err)
	}
}

func TestSignatureRejects(t *testing.T) {
	const secret = "a-secret-long-enough"
	now := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	body := []byte(`{"event":"new_issue"}`)
	header := domain.SignWebhook(secret, now, body)

	cases := []struct {
		name     string
		secret   string
		header   string
		body     []byte
		now      time.Time
		mentions string
	}{
		{name: "a changed body", secret: secret, header: header,
			body: []byte(`{"event":"regression"}`), now: now, mentions: "does not match"},
		{name: "the wrong secret", secret: "another-secret-entirely", header: header,
			body: body, now: now, mentions: "does not match"},
		// A signature over the body alone can be replayed forever. This is the
		// check that stops it.
		{name: "a captured request replayed an hour later", secret: secret, header: header,
			body: body, now: now.Add(time.Hour), mentions: "tolerance"},
		{name: "a request from the future", secret: secret, header: header,
			body: body, now: now.Add(-time.Hour), mentions: "tolerance"},
		{name: "no signature at all", secret: secret, header: "", body: body, now: now, mentions: "expected"},
		{name: "only a timestamp", secret: secret, header: "t=100", body: body, now: now, mentions: "expected"},
		{name: "a timestamp that is not one", secret: secret, header: "t=yesterday,v1=abcd",
			body: body, now: now, mentions: "unix time"},
		{name: "junk between the fields is skipped, and the rest still has to verify",
			secret: secret, header: strings.Replace(header, ",v1=", ",nonsense,v1=", 1),
			body: []byte("{}"), now: now, mentions: "does not match"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := domain.VerifyWebhook(testCase.secret, testCase.header, testCase.body,
				testCase.now, domain.WebhookSignatureTolerance)
			if err == nil {
				t.Fatal("the signature verified; it should not have")
			}
			if !errors.Is(err, domain.ErrInvalidSignature) {
				t.Errorf("error %v does not wrap ErrInvalidSignature", err)
			}
			if !strings.Contains(err.Error(), testCase.mentions) {
				t.Errorf("error %q does not mention %q", err, testCase.mentions)
			}
		})
	}
}

func TestSignatureToleranceCanBeSwitchedOff(t *testing.T) {
	const secret = "a-secret-long-enough"
	signedAt := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	body := []byte("{}")
	header := domain.SignWebhook(secret, signedAt, body)

	if err := domain.VerifyWebhook(secret, header, body, time.Now(), 0); err != nil {
		t.Fatalf("a tolerance of zero should mean no clock check, got %v", err)
	}
}

func TestDeliveryIDsAreUniqueAndHex(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id, err := domain.NewDeliveryID()
		if err != nil {
			t.Fatalf("minting a delivery id: %v", err)
		}
		if len(id) != domain.DeliveryIDBytes*2 {
			t.Fatalf("a delivery id is %d characters, want %d", len(id), domain.DeliveryIDBytes*2)
		}
		if seen[id] {
			t.Fatalf("delivery id %s was minted twice; it is an idempotency key and must not collide", id)
		}
		seen[id] = true
	}
}

// TestDigestIsDeliveredExactlyAsItWasWritten checks that nothing is added to
// the report on its way out.
//
// The weekly report is the one thing in the outbox that is not assembled from
// the payload's fields: it comes from a template with golden fixtures, and
// anything this type added to it — a headline, a level, the rule's name at
// the bottom — would appear after four hundred words of report in every
// channel that renders text.
func TestDigestIsDeliveredExactlyAsItWasWritten(t *testing.T) {
	const subject = "trapline — the week of 2026-08-24"
	const body = "venekambio\n  100 events, up 300% from 25 last week\n"

	payload := domain.NewDigestPayload(subject, body,
		time.Date(2026, 8, 31, 9, 0, 0, 0, time.UTC))

	if payload.Event != domain.EventDigest {
		t.Errorf("event = %q, want %q", payload.Event, domain.EventDigest)
	}
	if headline := payload.Headline(); headline != subject {
		t.Errorf("the subject line is %q, want %q", headline, subject)
	}
	if text := payload.Text(); text != body {
		t.Errorf("the delivered text is not the report:\n%s", text)
	}

	// And it survives the round trip through the outbox, because that is
	// where it lives between being written and being sent.
	encoded, err := payload.Encode()
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	back, err := domain.DecodeAlertPayload(encoded)
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if back.Text() != body {
		t.Errorf("the stored report came back different:\n%s", back.Text())
	}
}

// TestDigestIsNotATrigger: `digest` is a TriggerKind by type and not by
// membership, so the validation that guards rule creation refuses it. A rule watching for the weekly report is a rule that would
// never fire, and the API would have accepted it.
func TestDigestIsNotATrigger(t *testing.T) {
	if domain.EventDigest.Valid() {
		t.Error("digest is listed as a trigger a rule can watch")
	}
	if _, err := domain.ParseTrigger([]byte(`{"kind":"digest"}`)); err == nil {
		t.Error("a rule triggered by the weekly digest was accepted")
	}
}

// The webhook payload has to say which family a monitor belongs to, because
// the two number their ids from separate tables and a receiver that keys on
// monitor_id alone mixes them (ADR 037).
func TestAMonitorPayloadNamesItsFamily(t *testing.T) {
	payload := domain.NewAlertPayload("uptime", &domain.AlertEvent{
		Kind: domain.TriggerUptimeDown, ProjectID: 2,
		MonitorKind: domain.MonitorKindUptime, MonitorID: 7, MonitorSlug: "checkout",
		At: time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC),
	}, "https://errors.example.test/projects/2/monitors/uptime/7")

	encoded, err := payload.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for _, want := range []string{
		`"monitor_kind":"uptime"`, `"monitor_id":7`, `"monitor":"checkout"`,
		`/monitors/uptime/7`,
	} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("the payload does not carry %s:\n%s", want, encoded)
		}
	}
	// An alert about an issue carries none of it.
	issue := domain.NewAlertPayload("errors", &domain.AlertEvent{
		Kind: domain.TriggerNewIssue, ProjectID: 2, IssueID: 14,
	}, "")
	body, _ := issue.Encode()
	if strings.Contains(string(body), "monitor") {
		t.Errorf("an issue alert carries monitor fields:\n%s", body)
	}
	if !strings.Contains(payload.Text(), "monitor: checkout") {
		t.Errorf("the chat message does not name the monitor:\n%s", payload.Text())
	}
}
