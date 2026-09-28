package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

// TestDigestPreviewNeedsNoBody: "what would go out if it went out now" is the
// whole of the common call, and an endpoint that demanded arguments for it
// would be the stats endpoint with a different name.
func TestDigestPreviewNeedsNoBody(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	client.setUpAndLogIn()

	response := client.do(http.MethodPost, api("/digest/preview"), nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}

	var preview digestPreviewResponse
	client.decode(response, &preview)

	if preview.Schedule.Weekday != "Monday" || preview.Schedule.Hour != 9 {
		t.Errorf("the default schedule came back as %+v", preview.Schedule)
	}
	if preview.Schedule.Timezone != "UTC" {
		t.Errorf("the timezone is %q", preview.Schedule.Timezone)
	}
	if !strings.HasPrefix(preview.Text, "trapline — the week of ") {
		t.Errorf("the rendered digest starts with %q", firstLine(preview.Text))
	}
	// An installation with no projects has nothing to report, and says so
	// rather than sending an empty page.
	if !strings.Contains(preview.Text, "Nothing to report") {
		t.Errorf("an empty installation rendered:\n%s", preview.Text)
	}
}

// TestDigestPreviewCountsWhatWasIngested is the end-to-end claim: an event
// that arrived through the public ingest endpoint appears in the weekly
// report, and it got there through the hourly buckets rather than the events.
func TestDigestPreviewCountsWhatWasIngested(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	// An hour inside the reported week: the report covers the seven days
	// ending with the hour before now, so "an hour ago" is safely inside it.
	hour := time.Now().UTC().Add(-2 * time.Hour)
	for range 3 {
		sendEnvelope(t, server.URL, dsn, envelopeAt(dsn, "the payment function failed", hour), "")
	}

	var preview digestPreviewResponse
	client.decode(client.do(http.MethodPost, api("/digest/preview"), nil), &preview)

	if len(preview.Report.Projects) != 1 {
		t.Fatalf("the report has %d projects, want 1:\n%s", len(preview.Report.Projects), preview.Text)
	}
	project := preview.Report.Projects[0]
	if project.Events != 3 {
		t.Errorf("the week counted %d events, want 3", project.Events)
	}
	if project.NewIssues != 1 {
		t.Errorf("the week counted %d new issues, want 1", project.NewIssues)
	}
	if !strings.Contains(preview.Text, "3 events") {
		t.Errorf("the rendered digest does not carry the count:\n%s", preview.Text)
	}
	// The links use the configured public origin, not the address this
	// request happened to arrive on.
	if !strings.Contains(preview.Text, "https://errors.example.com/projects/") {
		t.Errorf("the digest has no usable links:\n%s", preview.Text)
	}
}

func TestDigestPreviewCanNarrowToOneProject(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)
	sendEnvelope(t, server.URL, dsn,
		envelopeAt(dsn, "boom", time.Now().UTC().Add(-2*time.Hour)), "")

	var preview digestPreviewResponse
	client.decode(client.do(http.MethodPost, api("/digest/preview"),
		map[string]any{"project_id": 9999}), &preview)

	if len(preview.Report.Projects) != 0 {
		t.Errorf("a project id that matches nothing produced %d sections", len(preview.Report.Projects))
	}
}

func TestDigestPreviewRefusesATimeItCannotRead(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	client.setUpAndLogIn()

	response := client.do(http.MethodPost, api("/digest/preview"), map[string]any{"at": "last tuesday"})
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", response.StatusCode)
	}
}

func TestDigestScheduleRoundTripsThroughTheAPI(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	client.setUpAndLogIn()

	var written digestScheduleBody
	response := client.do(http.MethodPut, api("/system/settings/digest"),
		map[string]any{"weekday": "friday", "hour": 17})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	client.decode(response, &written)
	if written.Weekday != "Friday" || written.Hour != 17 {
		t.Errorf("the write came back as %+v", written)
	}

	var read digestScheduleBody
	client.decode(client.do(http.MethodGet, api("/system/settings/digest"), nil), &read)
	if read != written {
		t.Errorf("read back %+v, want %+v", read, written)
	}
}

func TestDigestScheduleRefusesATimeThatDoesNotExist(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	client.setUpAndLogIn()

	for name, body := range map[string]map[string]any{
		"a day that is not a day": {"weekday": "caturday", "hour": 9},
		"an hour that is not one": {"weekday": "monday", "hour": 25},
		"nothing at all":          {},
	} {
		t.Run(name, func(t *testing.T) {
			response := client.do(http.MethodPut, api("/system/settings/digest"), body)
			if response.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", response.StatusCode)
			}
		})
	}
}

// TestChannelHealthOnAnInstallationWithNoChannels: a server that has the
// notification subsystem — which, since the digest was wired, is every server — and
// no channels in it says exactly that. "Nothing configured" is a legitimate
// state and must not read as a fault.
func TestChannelHealthOnAnInstallationWithNoChannels(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	client.setUpAndLogIn()

	response := client.do(http.MethodGet, api("/system/channels"), nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}

	var health channelHealthResponse
	client.decode(response, &health)
	if !health.Configured {
		t.Error("the notification subsystem is assembled and the server says it is not")
	}
	if health.Channels == nil {
		t.Error("the channel list is null; it must be an empty array so a client can range over it")
	}
	if len(health.Channels) != 0 {
		t.Errorf("a fresh installation reported %d channels", len(health.Channels))
	}
}

// TestChannelHealthNeverCarriesACredential is the reason this endpoint reports
// an endpoint rather than a delivery URL.
//
// It is guarded by projects:read — a weaker scope than the alerts:read that
// guards the channel listing — and for Telegram, Slack and Discord the
// delivery URL *is* the credential: the bot token is in the path and an
// incoming webhook's path is the whole of its authentication. A dashboard
// token that could read this would otherwise be able to post to the team's
// chat.
func TestChannelHealthNeverCarriesACredential(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	client.setUpAndLogIn()

	const botToken = "8100000000:AAH-this-is-the-credential"
	const slackPath = "/services/T00000000/B00000000/thisisthesecretpath"

	for _, channel := range []map[string]any{
		{"type": "telegram", "name": "ops", "config": map[string]any{
			"bot_token": botToken, "chat_id": "-1001",
		}},
		{"type": "slack", "name": "#alerts", "config": map[string]any{
			"url": "https://hooks.slack.com" + slackPath,
		}},
	} {
		if response := client.do(http.MethodPost, api("/alerts/channels"), channel); response.StatusCode != http.StatusCreated {
			t.Fatalf("creating the %v channel = %d", channel["type"], response.StatusCode)
		}
	}

	response := client.do(http.MethodGet, api("/system/channels"), nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	body := readBody(t, response)
	for _, secret := range []string{botToken, slackPath} {
		if strings.Contains(body, secret) {
			t.Errorf("the channel health endpoint echoes a credential (%q):\n%s", secret, body)
		}
	}
	if !strings.Contains(body, "https://api.telegram.org") {
		t.Errorf("the telegram channel does not say where it delivers:\n%s", body)
	}
	if !strings.Contains(body, "https://hooks.slack.com") {
		t.Errorf("the slack channel does not say where it delivers:\n%s", body)
	}
}

func TestDigestEndpointsNeedAuthentication(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)

	for _, request := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, api("/digest/preview")},
		{http.MethodGet, api("/system/settings/digest")},
		{http.MethodPut, api("/system/settings/digest")},
		{http.MethodGet, api("/system/channels")},
	} {
		response := client.do(request.method, request.path, nil)
		if response.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without a session = %d, want 401",
				request.method, request.path, response.StatusCode)
		}
	}
}

func firstLine(text string) string {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		return text[:index]
	}
	return text
}

// envelopeAt is one python-SDK envelope whose event happened at a chosen
// instant.
//
// The instant is what matters here, unlike the stats tests' fixed synthetic
// day: the digest's window is relative to now, so an event has to be placed
// relative to now too.
func envelopeAt(dsn domain.DSN, message string, when time.Time) string {
	event := fmt.Sprintf(`{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc",`+
		`"timestamp":%q,"platform":"python","level":"error",`+
		`"release":"app@1.0.0","environment":"production",`+
		`"exception":{"values":[{"type":"ValueError","value":%q,"stacktrace":{"frames":[`+
		`{"filename":"app/views.py","abs_path":"/srv/app/views.py","function":"checkout",`+
		`"lineno":42,"in_app":true}]}}]}}`,
		when.UTC().Format(time.RFC3339), message)

	return fmt.Sprintf("{\"event_id\":\"9ec79c33ec9942ab8353589fcb2e04dc\",\"dsn\":%q}\n"+
		"{\"type\":\"event\",\"length\":%d}\n%s\n", dsn.String(), len(event), event)
}
