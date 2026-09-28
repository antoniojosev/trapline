package domain

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The four states a queued notification can be in.
const (
	// NotificationPending is queued, waiting for its next attempt.
	NotificationPending NotificationStatus = "pending"
	// NotificationSent was delivered.
	NotificationSent NotificationStatus = "sent"
	// NotificationFailed failed its last attempt and will be retried.
	NotificationFailed NotificationStatus = "failed"
	// NotificationDead ran out of attempts. It stays in the log: a
	// notification that was never delivered and left no trace is the worst
	// outcome this subsystem has.
	NotificationDead NotificationStatus = "dead"
)

// NotificationStatus is where one queued notification stands.
type NotificationStatus string

// AllNotificationStatuses lists every status, for validating a filter.
func AllNotificationStatuses() []NotificationStatus {
	return []NotificationStatus{
		NotificationPending, NotificationSent, NotificationFailed, NotificationDead,
	}
}

// Valid reports whether s is a status this build stores.
func (s NotificationStatus) Valid() bool { return slices.Contains(AllNotificationStatuses(), s) }

// The retry schedule.
const (
	// MaxNotificationAttempts is how many times delivery is tried before the
	// row is declared dead. Ten attempts on the schedule below span a little
	// over eight hours, which covers an outage somebody sleeps through.
	MaxNotificationAttempts = 10
	// FirstRetryDelay is the wait after the first failure.
	FirstRetryDelay = 30 * time.Second
	// MaxRetryDelay caps the exponential backoff.
	MaxRetryDelay = time.Hour
)

// RetryDelay is how long to wait before attempt number attempts+1.
//
// Exponential from 30 s, capped at an hour. The cap matters more than the
// curve: an unbounded doubling reaches days, and a notification that arrives
// two days after the incident is not late, it is noise.
func RetryDelay(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	delay := FirstRetryDelay
	for range attempts - 1 {
		delay *= 2
		if delay >= MaxRetryDelay {
			return MaxRetryDelay
		}
	}
	return delay
}

// EventDigest marks a notification that is the weekly report rather than an
// alert about something that just happened.
//
// A TriggerKind by type and deliberately not one by membership: it is absent
// from AllTriggerKinds, so no rule can be written against it and Trigger's
// validation rejects it at the door. What it names is the other way a row
// gets into the outbox — the digest job hands one over directly (ADR 035) —
// and it is spelled in the same field as the four triggers because every
// consumer of that field, from the delivery log to the mail header a client
// threads on, wants one answer to "what is this message".
const EventDigest TriggerKind = "digest"

// Notification is one queued delivery: one payload, to one channel.
//
// One row per channel rather than one per rule, because delivery succeeds and
// fails per destination: a Slack outage must not hold up the email, and a
// retry must not re-post to the channel that already got it.
type Notification struct {
	// ID is the storage identity.
	ID int64
	// RuleID is the rule that produced it, zero for a test send.
	RuleID int64
	// ChannelID is where it goes.
	ChannelID int64
	// SubjectKey is what it is about (see AlertEvent.SubjectKey).
	SubjectKey string
	// Payload is the rendered alert, stored as JSON so a retry sends exactly
	// what the original attempt would have — the issue may have moved on, and
	// re-deriving the message hours later would report the present as the past.
	Payload AlertPayload
	// Status is where it stands.
	Status NotificationStatus
	// Attempts is how many deliveries have been tried.
	Attempts int
	// NextAttemptAt is when the notifier will pick it up.
	NextAttemptAt time.Time
	// LastError is why the last attempt failed.
	LastError string
	// CreatedAt is when it was enqueued.
	CreatedAt time.Time
	// SentAt is when it was delivered, nil until then.
	SentAt *time.Time
}

// AlertPayload is what a notification says, in the shape a webhook receives it.
//
// It is a value type with JSON tags because it is three things at once: the
// body of a signed webhook, the row stored in the outbox, and the source the
// chat and email adapters render their text from. Keeping one shape means a
// message cannot say something over Slack that the webhook did not carry.
type AlertPayload struct {
	// Event is which trigger fired.
	Event TriggerKind `json:"event"`
	// Rule is the name of the rule that fired, so a person receiving four
	// different alerts can tell which of their rules is noisy.
	Rule string `json:"rule"`
	// ProjectID is whose event it was.
	ProjectID int64 `json:"project_id"`
	// IssueID is the issue, absent for a project-wide alert.
	IssueID int64 `json:"issue_id,omitempty"`
	// MonitorKind, MonitorID and Monitor name the monitor an alert is about,
	// absent for everything else. The kind says which family — the two number
	// their monitors from separate tables, so the id alone is ambiguous to a
	// receiver routing on it. The readable name travels with them because a
	// message that says "monitor 7" is one nobody can act on without opening
	// the panel, which is the thing an alert exists to save somebody from.
	MonitorKind MonitorKind `json:"monitor_kind,omitempty"`
	MonitorID   int64       `json:"monitor_id,omitempty"`
	Monitor     string      `json:"monitor,omitempty"`
	// Title is the issue's title.
	Title string `json:"title,omitempty"`
	// Culprit is where it happened.
	Culprit string `json:"culprit,omitempty"`
	// Level is the severity.
	Level string `json:"level,omitempty"`
	// Release is the build it was seen in.
	Release string `json:"release,omitempty"`
	// Environment is the deployment it came from.
	Environment string `json:"environment,omitempty"`
	// Count is the number the rule fired on.
	Count int64 `json:"count,omitempty"`
	// Baseline is what that number was compared against.
	Baseline int64 `json:"baseline,omitempty"`
	// URL links straight to the issue in the panel. This is the field that
	// makes the difference between an alert and an interruption: it is the
	// whole of "break an app and get a message with a link to the issue".
	URL string `json:"url,omitempty"`
	// Body is a message that was written somewhere else and is delivered as
	// it stands, which today means the weekly digest.
	//
	// It exists because the digest is the one thing in the outbox that is not
	// assembled from these fields: it is a rendered report from a template
	// with golden fixtures, and re-deriving it here from a headline and a
	// count would be a second renderer producing a different document. When
	// it is set, Text returns it unchanged and adds nothing — no rule
	// signature, no summary — because whatever wrote it already decided what
	// the whole message says.
	Body string `json:"body,omitempty"`
	// At is when it happened.
	At time.Time `json:"at"`
}

// NewAlertPayload renders an event for a rule.
//
// issueURL is passed in rather than derived, because the domain does not know
// the installation's public origin and should not learn it (ADR 004). The
// event is a pointer for the same reason AlertRule.Matches takes one.
func NewAlertPayload(rule string, event *AlertEvent, issueURL string) AlertPayload {
	return AlertPayload{
		Event:       event.Kind,
		Rule:        rule,
		ProjectID:   event.ProjectID,
		IssueID:     event.IssueID,
		MonitorKind: event.MonitorKind,
		MonitorID:   event.MonitorID,
		Monitor:     event.MonitorSlug,
		Title:       event.Title,
		Culprit:     event.Culprit,
		Level:       string(event.Level),
		Release:     event.Release,
		Environment: event.Environment,
		Count:       event.Count,
		Baseline:    event.Baseline,
		URL:         issueURL,
		At:          event.At.UTC(),
	}
}

// NewDigestPayload wraps a rendered weekly report as an outbox row.
//
// The digest goes through the same queue as every alert rather than being
// sent directly, for the reason ADR 015 gives about everything else in there:
// a weekly mail lost because a chat API was down for a moment would be noticed a
// week later, by nobody. RuleID stays zero — no rule produced it — which is
// the same shape a channel test already has.
func NewDigestPayload(subject, body string, at time.Time) AlertPayload {
	return AlertPayload{
		Event: EventDigest,
		Rule:  "weekly digest",
		Title: subject,
		Body:  body,
		At:    at.UTC(),
	}
}

// Encode renders the payload as the bytes a webhook is signed over and the
// outbox stores.
//
// A pointer receiver on a value type that is otherwise passed around by
// value, and the reason is the same for the three renderers below: an
// AlertPayload is a fifth of a kilobyte of strings, and copying it once per
// call in order to read it is the copy gocritic's hugeParam threshold exists
// to catch. None of the three mutates anything, so the pointer costs none of
// what passing domain values by value normally buys.
func (p *AlertPayload) Encode() ([]byte, error) {
	encoded, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("encoding alert payload: %w", err)
	}
	return encoded, nil
}

// DecodeAlertPayload reads a stored payload.
func DecodeAlertPayload(raw []byte) (AlertPayload, error) {
	var payload AlertPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return AlertPayload{}, fmt.Errorf("%w: stored alert payload: %w", ErrInvalidAlert, err)
	}
	return payload, nil
}

// Headline is the one-line summary every channel leads with.
func (p *AlertPayload) Headline() string {
	// The digest already has a subject line — the week it covers — and
	// prefixing it with the event name would produce "digest: trapline — the
	// week of …" in every mail client's subject column.
	if p.Event == EventDigest {
		return p.Title
	}

	var b strings.Builder
	switch p.Event {
	case TriggerNewIssue:
		b.WriteString("New issue")
	case TriggerRegression:
		b.WriteString("Regression")
	case TriggerIssueSpike:
		b.WriteString("Spike")
	case TriggerErrorRate:
		b.WriteString("Error rate")
	case TriggerCronMissed:
		b.WriteString("Cron missed")
	case TriggerCronTimeout:
		b.WriteString("Cron timed out")
	case TriggerCronFailed:
		b.WriteString("Cron failed")
	case TriggerCronRecovered:
		b.WriteString("Cron recovered")
	case TriggerUptimeDown:
		b.WriteString("Down")
	case TriggerUptimeRecovered:
		b.WriteString("Recovered")
	default:
		b.WriteString(string(p.Event))
	}
	if p.Environment != "" {
		b.WriteString(" in ")
		b.WriteString(p.Environment)
	}
	if p.Title != "" {
		b.WriteString(": ")
		b.WriteString(p.Title)
	}
	return b.String()
}

// Text is the plain-text body the chat and email adapters send.
//
// One renderer for all of them rather than a template per channel: Slack,
// Discord and Telegram all accept plain text, none of them needs markup for a
// message this short, and a per-channel template is three places for the same
// message to drift.
func (p *AlertPayload) Text() string {
	// A message somebody else already finished. Appending the fields below to
	// a weekly report would put "level:" and "— weekly digest" after four
	// hundred words of it.
	if p.Body != "" {
		return p.Body
	}

	var b strings.Builder
	b.WriteString(p.Headline())
	b.WriteString("\n")

	if p.Monitor != "" {
		b.WriteString("\nmonitor: ")
		b.WriteString(p.Monitor)
	}
	if p.Culprit != "" {
		b.WriteString("\n")
		b.WriteString(p.Culprit)
	}
	if p.Level != "" {
		b.WriteString("\nlevel: ")
		b.WriteString(p.Level)
	}
	if p.Release != "" {
		b.WriteString("\nrelease: ")
		b.WriteString(p.Release)
	}
	if p.Count > 0 {
		b.WriteString("\ncount: ")
		b.WriteString(strconv.FormatInt(p.Count, 10))
		if p.Baseline > 0 {
			b.WriteString(" (baseline ")
			b.WriteString(strconv.FormatInt(p.Baseline, 10))
			b.WriteString(")")
		}
	}
	if p.URL != "" {
		b.WriteString("\n")
		b.WriteString(p.URL)
	}
	b.WriteString("\n\n— ")
	b.WriteString(p.Rule)
	return b.String()
}

// The headers a signed webhook carries.
const (
	// WebhookSignatureHeader carries the timestamp and the HMAC.
	WebhookSignatureHeader = "X-Trapline-Signature"
	// WebhookEventHeader names the trigger, so a receiver can route without
	// parsing the body.
	WebhookEventHeader = "X-Trapline-Event"
	// WebhookDeliveryHeader is a unique id per attempt-set. A receiver should
	// treat it as an idempotency key: delivery is at-least-once, and a crash
	// between a successful POST and the row being marked sent is exactly the
	// case that produces a second one.
	WebhookDeliveryHeader = "X-Trapline-Delivery"

	// WebhookSignatureTolerance is how far a signature's timestamp may be
	// from the receiver's clock. Five minutes is the usual figure and it is
	// what makes a captured request stop being replayable.
	WebhookSignatureTolerance = 5 * time.Minute
)

// SignWebhook renders the value of X-Trapline-Signature.
//
// The format is `t=<unix>,v1=<hex hmac-sha256(secret, t + "." + body)>`. The
// timestamp is inside the signed material, not beside it: a signature over the
// body alone can be replayed forever, and one over the body plus an unsigned
// timestamp can be replayed by anyone willing to edit a header.
//
// The `v1=` prefix is what makes a second scheme addable without breaking
// every receiver written against this one.
func SignWebhook(secret string, at time.Time, body []byte) string {
	unix := strconv.FormatInt(at.UTC().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(unix))
	mac.Write([]byte("."))
	mac.Write(body)
	return "t=" + unix + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// VerifyWebhook checks a signature the way a receiver should.
//
// It lives in the domain rather than only in the documentation because the
// gate's receiver uses it, and a verification routine that exists only as a
// snippet in a document is one nobody has ever run.
func VerifyWebhook(secret, header string, body []byte, now time.Time, tolerance time.Duration) error {
	var rawTime, rawMAC string
	for _, part := range strings.Split(header, ",") {
		key, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		switch key {
		case "t":
			rawTime = value
		case "v1":
			rawMAC = value
		}
	}
	if rawTime == "" || rawMAC == "" {
		return fmt.Errorf("%w: expected t=<unix>,v1=<hex>", ErrInvalidSignature)
	}

	unix, err := strconv.ParseInt(rawTime, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: timestamp %q is not a unix time", ErrInvalidSignature, rawTime)
	}
	signedAt := time.Unix(unix, 0).UTC()
	if tolerance > 0 {
		drift := now.UTC().Sub(signedAt)
		if drift < 0 {
			drift = -drift
		}
		if drift > tolerance {
			return fmt.Errorf("%w: signed %s from now, tolerance is %s",
				ErrInvalidSignature, drift.Round(time.Second), tolerance)
		}
	}

	expected := SignWebhook(secret, signedAt, body)
	// Constant time, over the whole header value. Comparing with == leaks the
	// length of the matching prefix, which is enough to forge a MAC one byte
	// at a time given enough attempts.
	if subtle.ConstantTimeCompare([]byte(expected), []byte("t="+rawTime+",v1="+rawMAC)) != 1 {
		return fmt.Errorf("%w: the body does not match the signature", ErrInvalidSignature)
	}
	return nil
}

// DeliveryIDBytes is the entropy of a delivery id.
const DeliveryIDBytes = 16

// NewDeliveryID mints the value of X-Trapline-Delivery.
func NewDeliveryID() (string, error) {
	raw := make([]byte, DeliveryIDBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating a delivery id: %w", err)
	}
	return hex.EncodeToString(raw), nil
}
