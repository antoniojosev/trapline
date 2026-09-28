package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/antoniojosev/trapline/internal/domain"
)

func alertsPath(suffix string) string { return api("/alerts") + suffix }

func webhookChannelRequest(url string) *channelRequest {
	return &channelRequest{
		Type: string(domain.ChannelWebhook),
		Name: "ops",
		Config: domain.ChannelConfig{
			URL:    url,
			Secret: "a-secret-long-enough",
		},
	}
}

func createChannel(t *testing.T, client *client, request *channelRequest) channelResponse {
	t.Helper()
	var channel channelResponse
	response := client.do(http.MethodPost, alertsPath("/channels"), request)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("creating a channel answered %d", response.StatusCode)
	}
	client.decode(response, &channel)
	return channel
}

// TestAChannelNeverEchoesItsCredential is the API half of encryption at rest:
// a secret that goes in and comes back out again would leak through the front
// door instead of through the file.
func TestAChannelNeverEchoesItsCredential(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	client.setUpAndLogIn()

	created := createChannel(t, client, webhookChannelRequest("https://example.test/hook"))
	if created.ID == 0 {
		t.Fatal("the channel came back without an id")
	}
	if created.Config.Secret != "" {
		t.Error("the signing secret came back in the response")
	}

	var page struct {
		Channels []channelResponse `json:"channels"`
	}
	client.decode(client.do(http.MethodGet, alertsPath("/channels"), nil), &page)
	if len(page.Channels) != 1 {
		t.Fatalf("listing returned %d channels", len(page.Channels))
	}
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatalf("re-encoding the listing: %v", err)
	}
	if strings.Contains(string(encoded), "a-secret-long-enough") {
		t.Errorf("the listing carries the secret: %s", encoded)
	}
}

func TestAnInvalidChannelIsRejectedWithAReason(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	client.setUpAndLogIn()

	request := webhookChannelRequest("https://example.test/hook")
	request.Config.Secret = ""
	response := client.do(http.MethodPost, alertsPath("/channels"), request)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unsigned webhook answered %d, want 400", response.StatusCode)
	}
	var body errorBody
	client.decode(response, &body)
	if !strings.Contains(body.Error, "secret") {
		t.Errorf("the error is %q, which does not say what to fix", body.Error)
	}
}

// TestTestingAChannelReportsTheFarEnd: a misconfigured channel has to be
// fixable without a packet capture.
func TestTestingAChannelReportsTheFarEnd(t *testing.T) {
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	server := newTestServer(t)
	client := newClient(t, server)
	client.setUpAndLogIn()

	channel := createChannel(t, client, webhookChannelRequest(receiver.URL+"/hook"))
	response := client.do(http.MethodPost,
		alertsPath("/channels/"+strconv.FormatInt(channel.ID, 10)+"/test"), nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("testing a working channel answered %d", response.StatusCode)
	}

	// And one pointing at nothing answers 502: the problem is at the far end,
	// not in this server.
	broken := createChannel(t, client, &channelRequest{
		Type: string(domain.ChannelWebhook), Name: "broken",
		Config: domain.ChannelConfig{URL: "http://127.0.0.1:0/hook", Secret: "a-secret-long-enough"},
	})
	failed := client.do(http.MethodPost,
		alertsPath("/channels/"+strconv.FormatInt(broken.ID, 10)+"/test"), nil)
	if failed.StatusCode != http.StatusBadGateway {
		t.Errorf("testing an unreachable channel answered %d, want 502", failed.StatusCode)
	}

	missing := client.do(http.MethodPost, alertsPath("/channels/999/test"), nil)
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("testing a channel that does not exist answered %d", missing.StatusCode)
	}
}

// TestAnEventBecomesAQueuedNotification is alerting's claim, as far as the HTTP
// surface can see it: break something, and a delivery is waiting with a link
// to the issue.
func TestAnEventBecomesAQueuedNotification(t *testing.T) {
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	channel := createChannel(t, client, webhookChannelRequest(receiver.URL+"/hook"))

	var rule ruleResponse
	created := client.do(http.MethodPost, alertsPath("/rules"), ruleRequest{
		Name:       "anything new",
		Trigger:    domain.Trigger{Kind: domain.TriggerNewIssue},
		ChannelIDs: []int64{channel.ID},
	})
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("creating a rule answered %d", created.StatusCode)
	}
	client.decode(created, &rule)
	if !rule.Enabled {
		t.Error("a rule nobody asked to switch off was created disabled")
	}

	sendEnvelope(t, server.URL, dsn, releaseEnvelope(dsn, "boom", "app@1.0.0"), "")

	var log struct {
		Notifications []notificationResponse `json:"notifications"`
	}
	client.decode(client.do(http.MethodGet, alertsPath("/notifications"), nil), &log)
	if len(log.Notifications) != 1 {
		t.Fatalf("the log holds %d notifications, want 1", len(log.Notifications))
	}
	queued := log.Notifications[0]
	if queued.Payload.Event != domain.TriggerNewIssue {
		t.Errorf("the queued payload says %q", queued.Payload.Event)
	}
	if !strings.Contains(queued.Payload.URL, "/issues/") {
		t.Errorf("the notification links to %q, which is not an issue", queued.Payload.URL)
	}

	// The log filters, because "what is stuck" is the question it is read for.
	var pending struct {
		Notifications []notificationResponse `json:"notifications"`
	}
	client.decode(client.do(http.MethodGet, alertsPath("/notifications?status=pending"), nil), &pending)
	if len(pending.Notifications) != 1 {
		t.Errorf("filtering by pending returned %d rows", len(pending.Notifications))
	}
	bad := client.do(http.MethodGet, alertsPath("/notifications?status=sending"), nil)
	if bad.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown status answered %d", bad.StatusCode)
	}
	badLimit := client.do(http.MethodGet, alertsPath("/notifications?limit=none"), nil)
	if badLimit.StatusCode != http.StatusBadRequest {
		t.Errorf("a limit that is not a number answered %d", badLimit.StatusCode)
	}
}

func TestRulesAreScopedAndRemovable(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	createProjectForIngest(t, client)
	channel := createChannel(t, client, webhookChannelRequest("https://example.test/hook"))

	project := int64(1)
	var scoped ruleResponse
	client.decode(client.do(http.MethodPost, alertsPath("/rules"), ruleRequest{
		ProjectID: &project, Name: "one project",
		Trigger: domain.Trigger{Kind: domain.TriggerRegression}, ChannelIDs: []int64{channel.ID},
	}), &scoped)

	var page struct {
		Rules []ruleResponse `json:"rules"`
	}
	client.decode(client.do(http.MethodGet, alertsPath("/rules?project_id=2"), nil), &page)
	if len(page.Rules) != 0 {
		t.Errorf("a rule scoped to project 1 is visible to project 2: %+v", page.Rules)
	}
	client.decode(client.do(http.MethodGet, alertsPath("/rules?project_id=1"), nil), &page)
	if len(page.Rules) != 1 {
		t.Errorf("the project's own rule is not listed: %+v", page.Rules)
	}
	if bad := client.do(http.MethodGet, alertsPath("/rules?project_id=x"), nil); bad.StatusCode != http.StatusBadRequest {
		t.Errorf("a project_id that is not a number answered %d", bad.StatusCode)
	}

	deleted := client.do(http.MethodDelete, alertsPath("/rules/"+strconv.FormatInt(scoped.ID, 10)), nil)
	if deleted.StatusCode != http.StatusNoContent {
		t.Errorf("deleting a rule answered %d", deleted.StatusCode)
	}
	if again := client.do(http.MethodDelete,
		alertsPath("/rules/"+strconv.FormatInt(scoped.ID, 10)), nil); again.StatusCode != http.StatusNotFound {
		t.Errorf("deleting it twice answered %d", again.StatusCode)
	}
}

// TestTestingARuleReportsEveryChannel: the request succeeded, and the verdict
// is the payload — an error status would make a caller throw away the part
// that says which channel is broken.
func TestTestingARuleReportsEveryChannel(t *testing.T) {
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	server := newTestServer(t)
	client := newClient(t, server)
	client.setUpAndLogIn()

	working := createChannel(t, client, webhookChannelRequest(receiver.URL+"/hook"))
	broken := createChannel(t, client, &channelRequest{
		Type: string(domain.ChannelWebhook), Name: "broken",
		Config: domain.ChannelConfig{URL: "http://127.0.0.1:0/hook", Secret: "a-secret-long-enough"},
	})

	var rule ruleResponse
	client.decode(client.do(http.MethodPost, alertsPath("/rules"), ruleRequest{
		Name: "both", Trigger: domain.Trigger{Kind: domain.TriggerNewIssue},
		ChannelIDs: []int64{working.ID, broken.ID},
	}), &rule)

	var report struct {
		OK      bool `json:"ok"`
		Results []struct {
			ChannelID int64  `json:"channel_id"`
			OK        bool   `json:"ok"`
			Error     string `json:"error"`
		} `json:"results"`
	}
	response := client.do(http.MethodPost, alertsPath("/rules/"+strconv.FormatInt(rule.ID, 10)+"/test"), nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("testing a rule answered %d", response.StatusCode)
	}
	client.decode(response, &report)
	if report.OK {
		t.Error("a rule with one unreachable channel reported ok")
	}
	if len(report.Results) != 2 {
		t.Fatalf("got %d results, want one per channel", len(report.Results))
	}
	if !report.Results[0].OK || report.Results[1].OK {
		t.Errorf("the per-channel verdicts are wrong: %+v", report.Results)
	}
}

func TestRetryingANotification(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)
	channel := createChannel(t, client, &channelRequest{
		Type: string(domain.ChannelWebhook), Name: "broken",
		Config: domain.ChannelConfig{URL: "http://127.0.0.1:0/hook", Secret: "a-secret-long-enough"},
	})

	var rule ruleResponse
	client.decode(client.do(http.MethodPost, alertsPath("/rules"), ruleRequest{
		Name: "anything new", Trigger: domain.Trigger{Kind: domain.TriggerNewIssue},
		ChannelIDs: []int64{channel.ID},
	}), &rule)

	sendEnvelope(t, server.URL, dsn, releaseEnvelope(dsn, "boom", "app@1.0.0"), "")

	var log struct {
		Notifications []notificationResponse `json:"notifications"`
	}
	client.decode(client.do(http.MethodGet, alertsPath("/notifications"), nil), &log)
	if len(log.Notifications) != 1 {
		t.Fatalf("the log holds %d notifications", len(log.Notifications))
	}

	var revived notificationResponse
	response := client.do(http.MethodPost,
		alertsPath("/notifications/"+strconv.FormatInt(log.Notifications[0].ID, 10)+"/retry"), nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("retrying answered %d", response.StatusCode)
	}
	client.decode(response, &revived)
	if revived.Status != domain.NotificationPending || revived.Attempts != 0 {
		t.Errorf("the revived row is %+v", revived)
	}

	if missing := client.do(http.MethodPost,
		alertsPath("/notifications/999/retry"), nil); missing.StatusCode != http.StatusNotFound {
		t.Errorf("retrying a row that does not exist answered %d", missing.StatusCode)
	}
}

// TestAlertRoutesDemandTheirOwnScope is the reason this area has a scope pair
// at all: a channel holds a credential for somebody else's system, so a token
// minted for a dashboard must not be able to read where this installation
// sends its notifications.
func TestAlertRoutesDemandTheirOwnScope(t *testing.T) {
	server, tokens := newTestServerWithTokens(t)

	_, projectsOnly, err := tokens.Create(t.Context(), "dashboard",
		[]domain.Scope{domain.ScopeProjectsRead, domain.ScopeProjectsWrite}, nil)
	if err != nil {
		t.Fatalf("minting a token: %v", err)
	}
	_, readOnly, err := tokens.Create(t.Context(), "alerts reader",
		[]domain.Scope{domain.ScopeAlertsRead}, nil)
	if err != nil {
		t.Fatalf("minting a token: %v", err)
	}

	cases := []struct {
		name   string
		method string
		path   string
		token  string
		want   int
	}{
		{"a projects-only token cannot read the channels",
			http.MethodGet, alertsPath("/channels"), projectsOnly, http.StatusForbidden},
		{"a projects-only token cannot read the rules",
			http.MethodGet, alertsPath("/rules"), projectsOnly, http.StatusForbidden},
		{"a projects-only token cannot read the log",
			http.MethodGet, alertsPath("/notifications"), projectsOnly, http.StatusForbidden},
		{"alerts:read reads the channels",
			http.MethodGet, alertsPath("/channels"), readOnly, http.StatusOK},
		// Reading is not writing, and testing a channel spends a real
		// delivery through somebody else's rate-limited API.
		{"alerts:read cannot create a channel",
			http.MethodPost, alertsPath("/channels"), readOnly, http.StatusForbidden},
		{"alerts:read cannot test a channel",
			http.MethodPost, alertsPath("/channels/1/test"), readOnly, http.StatusForbidden},
		{"alerts:read cannot create a rule",
			http.MethodPost, alertsPath("/rules"), readOnly, http.StatusForbidden},
		{"alerts:read cannot retry a notification",
			http.MethodPost, alertsPath("/notifications/1/retry"), readOnly, http.StatusForbidden},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request, err := http.NewRequestWithContext(t.Context(), testCase.method,
				server.URL+testCase.path, strings.NewReader(`{}`))
			if err != nil {
				t.Fatalf("building request: %v", err)
			}
			request.Header.Set("Authorization", "Bearer "+testCase.token)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(CSRFHeader, "1")

			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatalf("sending: %v", err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != testCase.want {
				t.Errorf("status = %d, want %d", response.StatusCode, testCase.want)
			}
		})
	}
}
