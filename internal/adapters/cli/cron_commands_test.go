package cli

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// TestCronMonitorsAreManageableFromTheCommandLine is ADR 006 for this area:
// the first thing anybody does with a cron monitor is create it on the machine
// the cron job runs on, over ssh, and paste the ping URL into a crontab —
// which cannot be done from a panel that has never been opened.
func TestCronMonitorsAreManageableFromTheCommandLine(t *testing.T) {
	h := newHarness(t)
	h.setUp()

	var project projectPayload
	if err := json.Unmarshal([]byte(h.mustRun("projects", "create", "-name", "venekambio", "--json")),
		&project); err != nil {
		t.Fatalf("creating a project: %v", err)
	}
	id := strconv.FormatInt(project.ID, 10)

	if got := strings.TrimSpace(h.mustRun("monitors", "cron", "list", "-project", id)); got != "no cron monitors" {
		t.Errorf("an empty list said %q", got)
	}

	created := h.mustRun("monitors", "cron", "add", "-project", id,
		"-slug", "nightly-backup", "-schedule", "0 3 * * *",
		"-timezone", "America/Caracas", "-margin", "30", "-max-runtime", "600")
	if !strings.Contains(created, "curl -fsS https://errors.example.test/ping/") {
		// The line somebody pastes into a crontab. Printing the bare key
		// instead would leave them assembling a URL by hand.
		t.Fatalf("the CLI did not print a usable ping line:\n%s", created)
	}

	var monitor monitorPayload
	if err := json.Unmarshal([]byte(h.mustRun("monitors", "cron", "show", "-project", id,
		"-id", "1", "--json")), &monitor); err != nil {
		t.Fatalf("--json output is not JSON: %v", err)
	}
	switch {
	case monitor.Slug != "nightly-backup":
		t.Fatalf("slug = %q", monitor.Slug)
	case monitor.Timezone != "America/Caracas":
		t.Fatalf("timezone = %q", monitor.Timezone)
	case monitor.CheckinMarginSeconds != 30 || monitor.MaxRuntimeSeconds != 600:
		t.Fatalf("margins = %d/%d", monitor.CheckinMarginSeconds, monitor.MaxRuntimeSeconds)
	case monitor.Status != "unknown":
		t.Fatalf("status = %q", monitor.Status)
	}

	listed := h.mustRun("monitors", "cron", "list", "-project", id)
	if !strings.Contains(listed, "nightly-backup") || !strings.Contains(listed, "America/Caracas") {
		t.Fatalf("the listing does not show the monitor:\n%s", listed)
	}

	updated := h.mustRun("monitors", "cron", "update", "-project", id, "-id", "1",
		"-schedule", "@hourly", "-disable")
	if !strings.Contains(updated, "@hourly") || !strings.Contains(updated, "enabled:   false") {
		t.Fatalf("the update did not take:\n%s", updated)
	}

	if got := strings.TrimSpace(h.mustRun("monitors", "cron", "checkins", "-project", id, "-id", "1")); got != "no check-ins" {
		t.Errorf("an empty history said %q", got)
	}

	if got := strings.TrimSpace(h.mustRun("monitors", "cron", "remove", "-project", id, "-id", "1")); got != "monitor 1 removed" {
		t.Errorf("removal said %q", got)
	}
	if got := strings.TrimSpace(h.mustRun("monitors", "cron", "list", "-project", id)); got != "no cron monitors" {
		t.Errorf("the monitor survived removal: %q", got)
	}
}

// A ping's history is readable from the CLI, which is what makes "when did
// this last actually work" answerable over ssh.
func TestCheckInsAreReadableFromTheCommandLine(t *testing.T) {
	h := newHarness(t)
	h.setUp()

	var project projectPayload
	if err := json.Unmarshal([]byte(h.mustRun("projects", "create", "-name", "venekambio", "--json")),
		&project); err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(project.ID, 10)

	var monitor monitorPayload
	if err := json.Unmarshal([]byte(h.mustRun("monitors", "cron", "add", "-project", id,
		"-slug", "backup", "-schedule", "*/1 * * * *", "--json")), &monitor); err != nil {
		t.Fatal(err)
	}
	if _, err := h.stack.Crons.Ping(t.Context(), monitor.PingKey, "ok"); err != nil {
		t.Fatalf("pinging: %v", err)
	}

	history := h.mustRun("monitors", "cron", "checkins", "-project", id,
		"-id", strconv.FormatInt(monitor.ID, 10), "-limit", "10")
	if !strings.Contains(history, "ok") {
		t.Fatalf("the history does not show the run:\n%s", history)
	}
}

func TestCronCommandsRefuseIncompleteInvocations(t *testing.T) {
	h := newHarness(t)
	h.setUp()

	cases := [][]string{
		{"monitors"},
		{"monitors", "uptime"},
		{"monitors", "cron"},
		{"monitors", "cron", "nonsense"},
		{"monitors", "cron", "add"},
		{"monitors", "cron", "add", "-project", "1", "-slug", "backup"},
		{"monitors", "cron", "list"},
		{"monitors", "cron", "show", "-project", "1"},
		{"monitors", "cron", "update", "-project", "1", "-id", "1"},
		{"monitors", "cron", "update", "-project", "1", "-id", "1", "-enable", "-disable"},
		{"monitors", "cron", "remove", "-project", "1"},
		{"monitors", "cron", "checkins", "-project", "1"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, _, stderr := h.run(args...)
			if code != ExitUsage {
				t.Fatalf("exited %d, want a usage error; stderr: %s", code, stderr)
			}
			if strings.TrimSpace(stderr) == "" {
				t.Fatal("a usage error said nothing about what was wrong")
			}
		})
	}
}

func TestCronCommandsReportServerErrors(t *testing.T) {
	h := newHarness(t)
	h.setUp()

	for _, args := range [][]string{
		{"monitors", "cron", "add", "-project", "999", "-slug", "backup", "-schedule", "@daily"},
		{"monitors", "cron", "show", "-project", "1", "-id", "999"},
		{"monitors", "cron", "update", "-project", "1", "-id", "999", "-schedule", "@daily"},
		{"monitors", "cron", "remove", "-project", "1", "-id", "999"},
		{"monitors", "cron", "checkins", "-project", "1", "-id", "999"},
		{"monitors", "cron", "list", "-project", "999"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, _, stderr := h.run(args...)
			if args[2] == "list" {
				// A project with no monitors is not an error; it is an empty
				// list, and the distinction matters to a script.
				if code != ExitOK {
					t.Fatalf("listing an empty project exited %d", code)
				}
				return
			}
			if code != ExitError {
				t.Fatalf("exited %d, want a runtime error; stderr: %s", code, stderr)
			}
		})
	}
}

// The CLI names the new command, or nobody discovers it.
func TestUsageMentionsMonitors(t *testing.T) {
	h := newHarness(t)
	code, stdout, _ := h.run("help")
	if code != ExitOK {
		t.Fatalf("help exited %d", code)
	}
	if !strings.Contains(stdout, "monitors") {
		t.Fatalf("usage does not mention monitors:\n%s", stdout)
	}
}
