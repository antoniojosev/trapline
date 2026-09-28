// Package notify delivers a notification through one of the five channels
// this product speaks.
//
// Everything here is the standard library. Each of these providers ships an
// SDK, and every one of them would bring a dependency tree, a release cadence
// and a vulnerability surface for what is, in all five cases, one HTTP POST
// with a JSON body — or, for email, an SMTP conversation that has not changed
// since before Go existed. The binary is promised at ≤30 MB and the
// dependencies are promised to be countable; five clients would spend both for
// nothing.
//
// The adapters are dumb on purpose: they take a rendered payload and a
// channel, and they either deliver it or return an error. Retrying, giving up,
// recording the failure and deciding what a person sees are all somebody
// else's job (usecase/notifier.go), because those decisions are the same for
// all five and would otherwise be written five times.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

var _ ports.AlertSender = (*Dispatcher)(nil)

// DefaultTimeout bounds one delivery attempt.
//
// Short on purpose. A notifier that waits thirty seconds on a hung endpoint
// holds up every other channel behind it, and the thing being reported is an
// incident — a message that arrives after the outage it describes has been
// noticed by other means is not a notification, it is a receipt.
const DefaultTimeout = 10 * time.Second

// maxErrorBody is how much of a failing response is read back to explain the
// failure. Enough for an API error message, far too little to be worth using
// as a memory-exhaustion vector.
const maxErrorBody = 4 * 1024

// Dispatcher routes a payload to the adapter for its channel type.
type Dispatcher struct {
	http *http.Client
	now  func() time.Time
	// dial is how the SMTP adapter reaches a relay. A field so a test can
	// substitute one without a real mail server, and nil in production, where
	// it means net.Dial.
	dial DialFunc
}

// Options are the seams a test needs. Every field is optional.
type Options struct {
	// HTTPClient replaces the shipped client, which has a timeout and nothing
	// else unusual about it.
	HTTPClient *http.Client
	// Now is the clock the webhook signature is stamped with.
	Now func() time.Time
	// Dial reaches the SMTP relay.
	Dial DialFunc
}

// New builds a dispatcher.
func New(options Options) *Dispatcher {
	dispatcher := &Dispatcher{
		http: options.HTTPClient,
		now:  options.Now,
		dial: options.Dial,
	}
	if dispatcher.http == nil {
		dispatcher.http = &http.Client{Timeout: DefaultTimeout}
	}
	if dispatcher.now == nil {
		dispatcher.now = func() time.Time { return time.Now().UTC() }
	}
	return dispatcher
}

// Send delivers one payload through one channel.
func (d *Dispatcher) Send(
	ctx context.Context, channel *domain.AlertChannel, payload *domain.AlertPayload, deliveryID string,
) error {
	switch channel.Type {
	case domain.ChannelTelegram:
		return d.sendTelegram(ctx, &channel.Config, payload)
	case domain.ChannelSlack:
		return d.sendSlack(ctx, &channel.Config, payload)
	case domain.ChannelDiscord:
		return d.sendDiscord(ctx, &channel.Config, payload)
	case domain.ChannelWebhook:
		return d.sendWebhook(ctx, &channel.Config, payload, deliveryID)
	case domain.ChannelEmail:
		return d.sendEmail(ctx, &channel.Config, payload)
	default:
		return fmt.Errorf("%w: this build cannot deliver to a %q channel",
			domain.ErrInvalidAlert, channel.Type)
	}
}

// postJSON sends a JSON body and treats any non-2xx as a failure worth
// reporting with what the far end said.
//
// The body of a failed response is the whole diagnostic value of an
// integration like this: "Slack said no_service" is actionable and "delivery
// failed" is not.
func (d *Dispatcher) postJSON(ctx context.Context, endpoint string, body any, headers map[string]string) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encoding the request: %w", err)
	}
	return d.post(ctx, endpoint, encoded, headers)
}

func (d *Dispatcher) post(ctx context.Context, endpoint string, body []byte, headers map[string]string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building the request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "trapline")
	for name, value := range headers {
		request.Header.Set(name, value)
	}

	response, err := d.http.Do(request)
	if err != nil {
		return fmt.Errorf("posting to %s: %w", redactURL(endpoint), err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode >= http.StatusMultipleChoices {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))
		return fmt.Errorf("%s answered %d: %s",
			redactURL(endpoint), response.StatusCode, strings.TrimSpace(string(detail)))
	}
	// Drained rather than abandoned, so the connection can be reused: a
	// notifier opens one per delivery otherwise, and an installation that
	// alerts often would leak sockets at the rate it alerts.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxErrorBody))
	return nil
}

// redactURL keeps the scheme, host and enough of the path to identify an
// endpoint, and drops the rest.
//
// Every one of these URLs is a credential: the path of a Slack or Discord
// incoming webhook is the entire authentication. This error message ends up in
// the notification log, which is readable over the API, so the URL that failed
// cannot be echoed into it whole.
func redactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "the endpoint"
	}
	return parsed.Scheme + "://" + parsed.Host + "/…"
}

// apiEndpoint resolves a provider call against an optional base URL.
//
// The base URL is the testing seam of ADR 015: with it, the delivery gate can
// point Telegram, Slack and Discord at one local receiver that imitates their
// routes, and prove that all five channels deliver without holding accounts on
// three services. Without it — which is every real installation — the
// provider's own address is used.
func apiEndpoint(baseURL, defaultRoot, path string) string {
	root := strings.TrimRight(baseURL, "/")
	if root == "" {
		root = strings.TrimRight(defaultRoot, "/")
	}
	return root + path
}
