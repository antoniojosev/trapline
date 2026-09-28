package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// receiverServer stands in for whatever a channel points at.
func receiverServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	return server
}

func addWebhookChannel(t *testing.T, h *harness, name, url string) int64 {
	t.Helper()
	config := `{"url":"` + url + `","secret":"a-secret-long-enough"}`
	out := h.mustRun("alerts", "channels", "add", "-type", "webhook", "-name", name,
		"-config", config, "--json")
	var channel channelPayload
	if err := json.Unmarshal([]byte(out), &channel); err != nil {
		t.Fatalf("--json output is not JSON (%q): %v", out, err)
	}
	if channel.ID == 0 {
		t.Fatalf("the channel came back without an id: %s", out)
	}
	return channel.ID
}

// TestAlertingIsConfigurableFromTheCommandLine is ADR 006 for this area: the
// first thing anybody does with a new alerting subsystem is configure it over
// ssh, before the panel has ever been opened.
func TestAlertingIsConfigurableFromTheCommandLine(t *testing.T) {
	h := newHarness(t)
	h.setUp()
	receiver := receiverServer(t, http.StatusOK)

	if got := strings.TrimSpace(h.mustRun("alerts", "channels", "list")); got != "no channels" {
		t.Errorf("an empty list said %q", got)
	}
	if got := strings.TrimSpace(h.mustRun("alerts", "rules", "list")); got != "no rules" {
		t.Errorf("an empty rule list said %q", got)
	}
	if got := strings.TrimSpace(h.mustRun("alerts", "log")); got != "no notifications" {
		t.Errorf("an empty log said %q", got)
	}

	channelID := addWebhookChannel(t, h, "ops", receiver.URL+"/hook")

	listed := h.mustRun("alerts", "channels", "list")
	if !strings.Contains(listed, "ops") || !strings.Contains(listed, "webhook") {
		t.Errorf("the listing does not show the channel:\n%s", listed)
	}
	// The credential must not come back out through the CLI any more than
	// through the API.
	if strings.Contains(h.mustRun("alerts", "channels", "list", "--json"), "a-secret-long-enough") {
		t.Error("--json echoed the signing secret")
	}

	if got := strings.TrimSpace(h.mustRun("alerts", "channels", "test",
		"-id", strconv.FormatInt(channelID, 10))); got != "delivered" {
		t.Errorf("testing a working channel said %q", got)
	}

	out := h.mustRun("alerts", "rules", "add", "-name", "anything new",
		"-trigger", `{"kind":"new_issue"}`, "-channels", strconv.FormatInt(channelID, 10),
		"-silence", "60", "--json")
	var rule rulePayload
	if err := json.Unmarshal([]byte(out), &rule); err != nil {
		t.Fatalf("--json output is not JSON (%q): %v", out, err)
	}
	if rule.SilenceSeconds != 60 || !rule.Enabled {
		t.Errorf("the rule came back as %+v", rule)
	}
	if rule.ProjectID != nil {
		t.Error("a rule created without -project is not global")
	}

	rules := h.mustRun("alerts", "rules", "list")
	if !strings.Contains(rules, "new_issue") || !strings.Contains(rules, "all projects") {
		t.Errorf("the rule listing does not describe the rule:\n%s", rules)
	}

	if code, _, _ := h.run("alerts", "rules", "test", "-id", strconv.FormatInt(rule.ID, 10)); code != ExitOK {
		t.Errorf("testing a rule whose channel works exited %d", code)
	}

	h.mustRun("alerts", "rules", "remove", "-id", strconv.FormatInt(rule.ID, 10))
	h.mustRun("alerts", "channels", "remove", "-id", strconv.FormatInt(channelID, 10))
	if got := strings.TrimSpace(h.mustRun("alerts", "channels", "list")); got != "no channels" {
		t.Errorf("after removing everything the list said %q", got)
	}
}

// TestARuleWithABrokenChannelExitsNonZero: an agent branches on the exit code,
// and "it works except for one channel" has to be a failure it can act on.
func TestARuleWithABrokenChannelExitsNonZero(t *testing.T) {
	h := newHarness(t)
	h.setUp()

	broken := addWebhookChannel(t, h, "broken", "http://127.0.0.1:0/hook")
	out := h.mustRun("alerts", "rules", "add", "-name", "broken",
		"-trigger", `{"kind":"new_issue"}`, "-channels", strconv.FormatInt(broken, 10), "--json")
	var rule rulePayload
	if err := json.Unmarshal([]byte(out), &rule); err != nil {
		t.Fatalf("--json output is not JSON: %v", err)
	}

	code, stdout, _ := h.run("alerts", "rules", "test", "-id", strconv.FormatInt(rule.ID, 10))
	if code != ExitError {
		t.Errorf("testing a rule with an unreachable channel exited %d, want %d", code, ExitError)
	}
	// And it still says which channel, on stdout, because that is the useful
	// half of the answer.
	if !strings.Contains(stdout, "broken") {
		t.Errorf("the output does not name the channel:\n%s", stdout)
	}

	if code, _, _ := h.run("alerts", "channels", "test", "-id", strconv.FormatInt(broken, 10)); code != ExitError {
		t.Errorf("testing an unreachable channel exited %d", code)
	}
}

func TestAlertUsageErrors(t *testing.T) {
	h := newHarness(t)
	h.setUp()

	cases := [][]string{
		{"alerts"},
		{"alerts", "nonsense"},
		{"alerts", "channels"},
		{"alerts", "channels", "nonsense"},
		{"alerts", "channels", "add"},
		{"alerts", "channels", "add", "-type", "webhook", "-name", "x", "-config", "not json"},
		{"alerts", "channels", "test"},
		{"alerts", "channels", "remove"},
		{"alerts", "rules"},
		{"alerts", "rules", "nonsense"},
		{"alerts", "rules", "add"},
		{"alerts", "rules", "add", "-name", "x", "-trigger", "not json", "-channels", "1"},
		{"alerts", "rules", "add", "-name", "x", "-trigger", `{"kind":"new_issue"}`, "-channels", "nope"},
		{"alerts", "rules", "remove"},
		{"alerts", "rules", "test"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, stdout, _ := h.run(args...)
			if code == ExitOK {
				t.Errorf("%v succeeded", args)
			}
			// Diagnostics never go to stdout: an agent parses stdout, and a
			// message mixed into it corrupts what it is reading.
			if strings.TrimSpace(stdout) != "" {
				t.Errorf("%v wrote to stdout: %q", args, stdout)
			}
		})
	}
}

func TestARuleCanBeScopedToAProject(t *testing.T) {
	h := newHarness(t)
	h.setUp()
	h.mustRun("projects", "create", "-name", "venekambio")

	channelID := addWebhookChannel(t, h, "ops", "https://example.test/hook")
	out := h.mustRun("alerts", "rules", "add", "-name", "one project", "-project", "1",
		"-trigger", `{"kind":"regression"}`, "-channels", strconv.FormatInt(channelID, 10),
		"-disabled", "--json")

	var rule rulePayload
	if err := json.Unmarshal([]byte(out), &rule); err != nil {
		t.Fatalf("--json output is not JSON: %v", err)
	}
	if rule.ProjectID == nil || *rule.ProjectID != 1 {
		t.Errorf("the rule came back scoped to %v", rule.ProjectID)
	}
	if rule.Enabled {
		t.Error("-disabled created an enabled rule")
	}
	if listed := h.mustRun("alerts", "rules", "list", "-project", "1"); !strings.Contains(listed, "disabled") {
		t.Errorf("the listing does not show the rule as switched off:\n%s", listed)
	}
}

func TestTheLogIsReadableAndRetryable(t *testing.T) {
	h := newHarness(t)
	h.setUp()
	h.mustRun("projects", "create", "-name", "venekambio")

	channelID := addWebhookChannel(t, h, "ops", "http://127.0.0.1:0/hook")
	h.mustRun("alerts", "rules", "add", "-name", "anything new",
		"-trigger", `{"kind":"new_issue"}`, "-channels", strconv.FormatInt(channelID, 10))

	ingestWithRelease(t, h, "boom", "app@1.0.0", time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC))

	var log struct {
		Notifications []notificationPayload `json:"notifications"`
	}
	if err := json.Unmarshal([]byte(h.mustRun("alerts", "log", "--json")), &log); err != nil {
		t.Fatalf("--json output is not JSON: %v", err)
	}
	if len(log.Notifications) != 1 {
		t.Fatalf("the log holds %d notifications, want 1", len(log.Notifications))
	}
	if !strings.Contains(h.mustRun("alerts", "log"), "new_issue") {
		t.Error("the text log does not name the trigger")
	}
	if !strings.Contains(h.mustRun("alerts", "log", "-status", "pending", "-limit", "5"), "new_issue") {
		t.Error("filtering the log dropped the row")
	}

	id := strconv.FormatInt(log.Notifications[0].ID, 10)
	if !strings.Contains(h.mustRun("alerts", "log", "-retry", id), "queued again") {
		t.Error("a retry was not reported")
	}
	if code, _, _ := h.run("alerts", "log", "-status", "sending"); code == ExitOK {
		t.Error("an unknown status was accepted")
	}
}
