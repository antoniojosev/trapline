package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// An IP literal rather than a host name: the guard resolves what it is given,
// and a test that named example.com would be a test of whatever DNS the
// machine running it happens to have.
const cliPublicTarget = "http://93.184.216.34/health"

func newMonitorHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.setUp()
	h.mustRun("projects", "create", "-name", "venekambio")
	return h
}

// TestMonitorLifecycleThroughTheCLI is ADR 006 for this area: everything the
// REST API can do, the CLI can do, with --json on every command and no prompt
// anywhere.
func TestMonitorLifecycleThroughTheCLI(t *testing.T) {
	h := newMonitorHarness(t)

	if got := strings.TrimSpace(h.mustRun("monitors", "uptime", "list", "-project", "1")); got != "no monitors" {
		t.Errorf("the empty list said %q", got)
	}

	raw := h.mustRun("monitors", "uptime", "add", "-project", "1",
		"-name", "api", "-target", cliPublicTarget, "-interval", "60", "--json")
	var created uptimeMonitorPayload
	if err := json.Unmarshal([]byte(raw), &created); err != nil {
		t.Fatalf("add did not emit JSON: %v (%s)", err, raw)
	}
	if created.ID == 0 || created.Name != "api" || created.IntervalSeconds != 60 {
		t.Fatalf("the monitor came back as %+v", created)
	}
	if created.Status != "unknown" {
		t.Errorf("a monitor was born %q", created.Status)
	}

	listing := h.mustRun("monitors", "uptime", "list", "-project", "1")
	if !strings.Contains(listing, "api") || !strings.Contains(listing, cliPublicTarget) {
		t.Errorf("the listing does not show the monitor:\n%s", listing)
	}

	shown := h.mustRun("monitors", "uptime", "show", "-id", "1")
	if !strings.Contains(shown, "unknown") {
		t.Errorf("show does not report the status:\n%s", shown)
	}

	// The history is empty and says so rather than printing nothing, which is
	// indistinguishable from a command that failed quietly.
	if got := strings.TrimSpace(h.mustRun("monitors", "uptime", "results", "-id", "1")); got != "no checks yet" {
		t.Errorf("results said %q", got)
	}
	if got := strings.TrimSpace(h.mustRun("monitors", "uptime", "daily", "-id", "1")); got != "no history yet" {
		t.Errorf("daily said %q", got)
	}

	disabled := h.mustRun("monitors", "uptime", "disable", "-id", "1", "--json")
	var switched uptimeMonitorPayload
	if err := json.Unmarshal([]byte(disabled), &switched); err != nil {
		t.Fatalf("disable did not emit JSON: %v", err)
	}
	if switched.Enabled {
		t.Error("the monitor came back enabled")
	}
	enabled := h.mustRun("monitors", "uptime", "enable", "-id", "1", "--json")
	if err := json.Unmarshal([]byte(enabled), &switched); err != nil {
		t.Fatalf("enable did not emit JSON: %v", err)
	}
	if !switched.Enabled {
		t.Error("the monitor stayed disabled")
	}

	h.mustRun("monitors", "uptime", "remove", "-id", "1")
	if got := strings.TrimSpace(h.mustRun("monitors", "uptime", "list", "-project", "1")); got != "no monitors" {
		t.Errorf("after removal the list said %q", got)
	}
}

// TestTheCLIReportsARefusedTarget: the message the server wrote has to reach
// the operator's terminal intact, on stderr, with a non-zero exit code. A
// refusal that arrives as "exit 1" is a refusal nobody can act on.
func TestTheCLIReportsARefusedTarget(t *testing.T) {
	h := newMonitorHarness(t)

	code, stdout, stderr := h.run("monitors", "uptime", "add", "-project", "1",
		"-name", "metadata", "-target", "http://169.254.169.254/latest/meta-data/")
	if code != ExitError {
		t.Fatalf("a refused target exited %d, want %d", code, ExitError)
	}
	if stdout != "" {
		t.Errorf("the diagnostic went to stdout, where an agent parses results: %q", stdout)
	}
	if !strings.Contains(stderr, "link-local") {
		t.Errorf("stderr does not name why: %q", stderr)
	}
	if !strings.Contains(stderr, "-uptime-allow-private") {
		t.Errorf("stderr does not say what would have to change: %q", stderr)
	}
}

func TestTheCLIReportsAnIntervalBelowTheFloor(t *testing.T) {
	h := newMonitorHarness(t)

	code, _, stderr := h.run("monitors", "uptime", "add", "-project", "1",
		"-name", "eager", "-target", cliPublicTarget, "-interval", "10")
	if code != ExitError {
		t.Fatalf("exited %d", code)
	}
	if !strings.Contains(stderr, "interval_s must be between") {
		t.Errorf("stderr is %q", stderr)
	}
}

// TestMonitorCommandsRefuseMissingArguments: usage errors are exit 2 and
// nothing is attempted, which is the contract an agent branches on.
func TestMonitorCommandsRefuseMissingArguments(t *testing.T) {
	h := newMonitorHarness(t)

	cases := [][]string{
		{"monitors"},
		{"monitors", "sideways"},
		{"monitors", "uptime"},
		{"monitors", "uptime", "sideways"},
		{"monitors", "uptime", "add"},
		{"monitors", "uptime", "add", "-project", "1", "-name", "api"},
		{"monitors", "uptime", "list"},
		{"monitors", "uptime", "show"},
		{"monitors", "uptime", "remove"},
		{"monitors", "uptime", "enable"},
		{"monitors", "uptime", "disable"},
		{"monitors", "uptime", "results"},
		{"monitors", "uptime", "daily"},
	}
	for _, args := range cases {
		code, stdout, _ := h.run(args...)
		if code != ExitUsage {
			t.Errorf("%v exited %d, want %d", args, code, ExitUsage)
		}
		if stdout != "" {
			t.Errorf("%v wrote %q to stdout", args, stdout)
		}
	}
}

// TestMonitorCommandsAcceptEveryOption walks the flags a monitor can carry, so
// a flag that stopped reaching the server is caught here rather than by
// somebody whose expectation was silently ignored.
func TestMonitorCommandsAcceptEveryOption(t *testing.T) {
	h := newMonitorHarness(t)

	raw := h.mustRun("monitors", "uptime", "add", "-project", "1",
		"-name", "api", "-target", cliPublicTarget,
		"-method", "get", "-interval", "120", "-timeout", "5",
		"-status-min", "200", "-status-max", "204",
		"-contains", `"database":"ok"`,
		"-follow-redirects=false", "-public", "-disabled", "--json")

	var created uptimeMonitorPayload
	if err := json.Unmarshal([]byte(raw), &created); err != nil {
		t.Fatalf("add did not emit JSON: %v (%s)", err, raw)
	}
	switch {
	case created.Method != "GET":
		t.Errorf("method = %q", created.Method)
	case created.IntervalSeconds != 120 || created.TimeoutSeconds != 5:
		t.Errorf("interval/timeout = %d/%d", created.IntervalSeconds, created.TimeoutSeconds)
	case created.ExpectedStatusMax != 204:
		t.Errorf("expected status max = %d", created.ExpectedStatusMax)
	case created.ExpectedBodySubstring != `"database":"ok"`:
		t.Errorf("substring = %q", created.ExpectedBodySubstring)
	case created.FollowRedirects:
		t.Error("-follow-redirects=false did not reach the server")
	case !created.Public:
		t.Error("-public did not reach the server")
	case created.Enabled:
		t.Error("-disabled did not reach the server")
	}

	// A disabled monitor is listed as disabled rather than as its stale
	// status, because "unknown" next to something nobody is checking reads as
	// a monitor that is broken.
	if listing := h.mustRun("monitors", "uptime", "list", "-project", "1"); !strings.Contains(listing, "disabled") {
		t.Errorf("the listing does not show it as disabled:\n%s", listing)
	}
}
