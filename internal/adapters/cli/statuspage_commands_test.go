package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// ADR 006 for the status page: everything the panel's settings section can do,
// the CLI can do, with --json and without a prompt. The public page itself is
// HTML and has no CLI — reading it is `curl`, which is the point of it.
func TestStatusPageSettingsThroughTheCLI(t *testing.T) {
	h := newMonitorHarness(t)

	// Unset is said in words rather than shown as a blank line, because a
	// blank line reads like a command that failed quietly — and what actually
	// happens is that each page falls back to its own project's name.
	shown := h.mustRun("monitors", "status-page", "show")
	if !strings.Contains(shown, "unset") || !strings.Contains(shown, "(none)") {
		t.Errorf("the empty settings said:\n%s", shown)
	}

	raw := h.mustRun("monitors", "status-page", "set",
		"-title", "  Acme systems  ", "-description", "live service status", "--json")
	var written statusPagePayload
	if err := json.Unmarshal([]byte(raw), &written); err != nil {
		t.Fatalf("set did not emit JSON: %v (%s)", err, raw)
	}
	if written.Title != "Acme systems" {
		t.Errorf("the title was not trimmed: %q", written.Title)
	}

	shown = h.mustRun("monitors", "status-page", "show")
	if !strings.Contains(shown, "Acme systems") || !strings.Contains(shown, "live service status") {
		t.Errorf("show does not report what was written:\n%s", shown)
	}

	// Empty is how a bad title is undone, so it has to reach the server rather
	// than being read as "leave it alone".
	h.mustRun("monitors", "status-page", "set", "-title", "", "-description", "")
	if !strings.Contains(h.mustRun("monitors", "status-page", "show"), "unset") {
		t.Error("clearing the title did not clear it")
	}

	// An over-long title is a refusal, not a truncation.
	h.mustRunFailing("monitors", "status-page", "set", "-title", strings.Repeat("a", 200))
}

// The per-project switch lives with the rest of that project's configuration,
// because that is where it is stored (ADR 017).
func TestStatusPageSwitchThroughConfig(t *testing.T) {
	h := newMonitorHarness(t)

	if !strings.Contains(h.mustRun("config", "show", "-project", "1"), "status page  off") {
		t.Error("a fresh project does not report its status page as off")
	}

	h.mustRun("config", "set", "-project", "1", "-status-page", "on")
	if !strings.Contains(h.mustRun("config", "show", "-project", "1"), "status page  on") {
		t.Error("switching the page on did not stick")
	}

	// Only the page changed: a command that posted everything it was showing
	// would turn every inherited default into a decision nobody made.
	shown := h.mustRun("config", "show", "-project", "1")
	if !strings.Contains(shown, "(default)") {
		t.Errorf("the other settings stopped being inherited:\n%s", shown)
	}

	h.mustRun("config", "set", "-project", "1", "-status-page", "off")
	if !strings.Contains(h.mustRun("config", "show", "-project", "1"), "status page  off") {
		t.Error("switching the page off did not stick")
	}

	// Anything but on or off is a usage error and names what it wanted.
	code, _, stderr := h.run("config", "set", "-project", "1", "-status-page", "sí")
	if code != ExitUsage {
		t.Errorf("a bad value exited %d, want a usage error", code)
	}
	if !strings.Contains(stderr, `"on"`) {
		t.Errorf("the refusal does not say what it wanted: %s", stderr)
	}
}

// The dispatcher is one verb with three families under it, and every one of
// them has to be reachable from it (ADR 037).
func TestMonitorsDispatchesAllThreeFamilies(t *testing.T) {
	h := newMonitorHarness(t)

	for _, args := range [][]string{
		{"monitors", "cron", "list", "-project", "1"},
		{"monitors", "uptime", "list", "-project", "1"},
		{"monitors", "status-page", "show"},
	} {
		if code, _, stderr := h.run(args...); code != ExitOK {
			t.Errorf("%v exited %d: %s", args, code, stderr)
		}
	}

	code, _, stderr := h.run("monitors")
	if code != ExitUsage {
		t.Errorf("a bare `monitors` exited %d", code)
	}
	// The message has to name all three, or the second family added is one
	// nobody discovers.
	for _, family := range []string{"cron", "uptime", "status-page"} {
		if !strings.Contains(stderr, family) {
			t.Errorf("the usage line does not mention %s: %s", family, stderr)
		}
	}
}
