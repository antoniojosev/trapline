package notify_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/adapters/notify"
	"github.com/antoniojosev/trapline/internal/domain"
)

func payload() *domain.AlertPayload {
	built := domain.NewAlertPayload("deploys", &domain.AlertEvent{
		Kind:        domain.TriggerNewIssue,
		ProjectID:   2,
		IssueID:     14,
		Title:       "ValueError: boom",
		Culprit:     "app/views.py in checkout",
		Level:       domain.Level("error"),
		Environment: "production",
		Count:       1,
		At:          time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC),
	}, "https://errors.example.test/projects/2/issues/14")
	return &built
}

// recorder is a stand-in for whichever service is being addressed.
type recorder struct {
	mu     sync.Mutex
	path   string
	body   []byte
	header http.Header
	status int
}

func (r *recorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.path, r.body, r.header = req.URL.Path, body, req.Header.Clone()
		status := r.status
		r.mu.Unlock()

		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
}

func (r *recorder) seen() (path string, body []byte, header http.Header) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.path, r.body, r.header
}

func TestTelegramPostsThroughTheBotAPI(t *testing.T) {
	seen := &recorder{}
	server := httptest.NewServer(seen.handler())
	defer server.Close()

	dispatcher := notify.New(notify.Options{})
	channel := domain.AlertChannel{Type: domain.ChannelTelegram, Config: domain.ChannelConfig{
		BotToken: "123:abc", ChatID: "-100", BaseURL: server.URL,
	}}
	if err := dispatcher.Send(context.Background(), &channel, payload(), "delivery-1"); err != nil {
		t.Fatalf("sending: %v", err)
	}

	path, body, _ := seen.seen()
	// The route matters: a test that posted to "/" would prove this adapter
	// can make an HTTP request, which was never in doubt.
	if path != "/bot123:abc/sendMessage" {
		t.Errorf("posted to %q, want Telegram's own route", path)
	}
	var sent struct {
		ChatID  string `json:"chat_id"`
		Text    string `json:"text"`
		Preview bool   `json:"disable_web_page_preview"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("the body is not what Telegram expects: %v", err)
	}
	if sent.ChatID != "-100" {
		t.Errorf("chat_id is %q", sent.ChatID)
	}
	if !strings.Contains(sent.Text, "ValueError: boom") {
		t.Errorf("the message does not carry the issue: %q", sent.Text)
	}
	if !sent.Preview {
		t.Error("link previews are on, so Telegram would fetch this installation's own panel once per alert")
	}
}

func TestSlackAndDiscordKeepTheirWebhookPath(t *testing.T) {
	cases := []struct {
		name        string
		channelType domain.ChannelType
		webhookPath string
		field       string
	}{
		{"slack", domain.ChannelSlack, "/services/T000/B000/xxxx", "text"},
		{"discord", domain.ChannelDiscord, "/api/webhooks/1/xxxx", "content"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			seen := &recorder{}
			server := httptest.NewServer(seen.handler())
			defer server.Close()

			dispatcher := notify.New(notify.Options{})
			channel := domain.AlertChannel{Type: testCase.channelType, Config: domain.ChannelConfig{
				URL:     "https://example.test" + testCase.webhookPath,
				BaseURL: server.URL,
			}}
			if err := dispatcher.Send(context.Background(), &channel, payload(), "delivery-1"); err != nil {
				t.Fatalf("sending: %v", err)
			}

			path, body, _ := seen.seen()
			if path != testCase.webhookPath {
				t.Errorf("posted to %q, want %q: the base URL replaces the origin, not the route",
					path, testCase.webhookPath)
			}
			var sent map[string]any
			if err := json.Unmarshal(body, &sent); err != nil {
				t.Fatalf("the body is not JSON: %v", err)
			}
			text, found := sent[testCase.field].(string)
			if !found {
				t.Fatalf("the body has no %q field: %s", testCase.field, body)
			}
			if !strings.Contains(text, "https://errors.example.test/projects/2/issues/14") {
				t.Errorf("the message does not carry the link: %q", text)
			}
		})
	}
}

func TestWithoutABaseURLTheWebhookURLIsUsedWhole(t *testing.T) {
	seen := &recorder{}
	server := httptest.NewServer(seen.handler())
	defer server.Close()

	dispatcher := notify.New(notify.Options{})
	channel := domain.AlertChannel{Type: domain.ChannelSlack, Config: domain.ChannelConfig{
		URL: server.URL + "/services/A/B/C",
	}}
	if err := dispatcher.Send(context.Background(), &channel, payload(), "d"); err != nil {
		t.Fatalf("sending: %v", err)
	}
	if path, _, _ := seen.seen(); path != "/services/A/B/C" {
		t.Errorf("posted to %q", path)
	}
}

// TestWebhookIsSignedOverExactlyTheBytesSent is the contract in
// docs/alerts/webhooks.md. A receiver that re-encodes the payload and verifies
// that would fail on any difference in key order, which is how a webhook
// signature ends up documented as "does not work, disable the check".
func TestWebhookIsSignedOverExactlyTheBytesSent(t *testing.T) {
	seen := &recorder{}
	server := httptest.NewServer(seen.handler())
	defer server.Close()

	now := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	dispatcher := notify.New(notify.Options{Now: func() time.Time { return now }})
	channel := domain.AlertChannel{Type: domain.ChannelWebhook, Config: domain.ChannelConfig{
		URL: server.URL + "/hook", Secret: "a-secret-long-enough",
	}}
	if err := dispatcher.Send(context.Background(), &channel, payload(), "delivery-42"); err != nil {
		t.Fatalf("sending: %v", err)
	}

	_, body, header := seen.seen()
	if got := header.Get(domain.WebhookEventHeader); got != string(domain.TriggerNewIssue) {
		t.Errorf("%s is %q", domain.WebhookEventHeader, got)
	}
	if got := header.Get(domain.WebhookDeliveryHeader); got != "delivery-42" {
		t.Errorf("%s is %q; a receiver needs it as an idempotency key", domain.WebhookDeliveryHeader, got)
	}
	signature := header.Get(domain.WebhookSignatureHeader)
	if err := domain.VerifyWebhook(channel.Config.Secret, signature, body, now,
		domain.WebhookSignatureTolerance); err != nil {
		t.Fatalf("the signature does not verify against the bytes that were sent: %v", err)
	}
	if err := domain.VerifyWebhook(channel.Config.Secret, signature, append(body, ' '), now,
		domain.WebhookSignatureTolerance); err == nil {
		t.Error("the signature verified against a modified body")
	}
}

// TestAFailureCarriesWhatTheFarEndSaid: "Slack said no_service" is actionable,
// "delivery failed" is not.
func TestAFailureCarriesWhatTheFarEndSaid(t *testing.T) {
	seen := &recorder{status: http.StatusBadRequest}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("no_service"))
	}))
	defer server.Close()
	_ = seen

	dispatcher := notify.New(notify.Options{})
	channel := domain.AlertChannel{Type: domain.ChannelSlack,
		Config: domain.ChannelConfig{URL: server.URL + "/services/A/B/C"}}

	err := dispatcher.Send(context.Background(), &channel, payload(), "d")
	if err == nil {
		t.Fatal("a 400 was reported as a delivery")
	}
	if !strings.Contains(err.Error(), "no_service") {
		t.Errorf("error %q does not carry what the far end said", err)
	}
}

// TestAFailureDoesNotEchoTheCredential: the path of an incoming webhook IS the
// authentication, and this error ends up in the notification log, which is
// readable over the API.
func TestAFailureDoesNotEchoTheCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	dispatcher := notify.New(notify.Options{})
	channel := domain.AlertChannel{Type: domain.ChannelSlack,
		Config: domain.ChannelConfig{URL: server.URL + "/services/A/B/SUPERSECRET"}}

	err := dispatcher.Send(context.Background(), &channel, payload(), "d")
	if err == nil {
		t.Fatal("a 500 was reported as a delivery")
	}
	if strings.Contains(err.Error(), "SUPERSECRET") {
		t.Errorf("error %q leaks the webhook path, which is the credential", err)
	}
}

func TestAnUnknownChannelTypeIsRefused(t *testing.T) {
	dispatcher := notify.New(notify.Options{})
	err := dispatcher.Send(context.Background(),
		&domain.AlertChannel{Type: domain.ChannelType("pigeon")}, payload(), "d")
	if err == nil {
		t.Fatal("a channel type this build cannot deliver was accepted")
	}
}

func TestAnUnreachableEndpointIsAnError(t *testing.T) {
	dispatcher := notify.New(notify.Options{})
	channel := domain.AlertChannel{Type: domain.ChannelWebhook, Config: domain.ChannelConfig{
		// Port 0 never connects, and does so quickly.
		URL: "http://127.0.0.1:0/hook", Secret: "a-secret-long-enough",
	}}
	if err := dispatcher.Send(context.Background(), &channel, payload(), "d"); err == nil {
		t.Fatal("posting to a closed port succeeded")
	}
}
