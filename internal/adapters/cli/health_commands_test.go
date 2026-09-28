package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// newHealthHarness sets an installation up with one project and a known hour
// of sessions already counted.
//
// The sessions go in through the use case rather than over the ingest
// endpoint: what is under test here is the CLI, and driving the wire format
// again would only be a slower copy of the HTTP suite's coverage.
func newHealthHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.setUp()
	h.mustRun("projects", "create", "-name", "venekambio")
	return h
}

// recordSessions counts `count` sessions of one release, `crashed` of which
// crashed, in the hour the harness's clock is in — because the CLI's default
// range is the last twenty-four hours and a fixed date would fall outside it.
func recordSessions(t *testing.T, h *harness, release string, count, crashed int) {
	t.Helper()
	at := testClock{}.Now().Truncate(time.Hour).Add(5 * time.Minute)

	for index := range count {
		status := "exited"
		if index < crashed {
			status = "crashed"
		}
		payload := fmt.Appendf(nil,
			`{"sid":"%s-%04d","started":%q,"status":%q,`+
				`"attrs":{"release":%q,"environment":"production"}}`,
			release, index, at.Format(time.RFC3339Nano), status, release)
		if _, err := h.stack.Health.Accept(t.Context(), 1, "session", payload); err != nil {
			t.Fatalf("recording a session: %v", err)
		}
	}
	if _, err := h.stack.Health.Flush(t.Context()); err != nil {
		t.Fatalf("flushing: %v", err)
	}
}

func TestReleasesHealthReportsOneRelease(t *testing.T) {
	h := newHealthHarness(t)
	recordSessions(t, h, "shop@1.4.2", 100, 5)

	output := h.mustRun("releases", "health", "-project", "1", "-version", "shop@1.4.2", "--json")
	var payload releaseHealthPayload
	if err := json.Unmarshal([]byte(output), &payload); err != nil {
		t.Fatalf("decoding: %v\n%s", err, output)
	}
	if payload.Started != 100 || payload.Crashed != 5 || payload.Healthy != 95 {
		t.Fatalf("got %+v", payload)
	}
	if payload.CrashFreeRate == nil || *payload.CrashFreeRate != 0.95 {
		t.Fatalf("crash-free rate %v, want 0.95", payload.CrashFreeRate)
	}

	text := h.mustRun("releases", "health", "-project", "1", "-version", "shop@1.4.2")
	if !strings.Contains(text, "95.00%") {
		t.Fatalf("the rate is not in the text output:\n%s", text)
	}
	// The caveat is printed, not swallowed. A terminal that showed the number
	// and dropped the sentence would be less honest than the API (ADR 008).
	if !strings.Contains(text, "restart") {
		t.Fatalf("the volatility note is missing:\n%s", text)
	}
}

func TestReleasesHealthRanksEveryRelease(t *testing.T) {
	h := newHealthHarness(t)
	recordSessions(t, h, "shop@1.4.2", 200, 10)
	recordSessions(t, h, "shop@1.4.1", 50, 0)

	output := h.mustRun("releases", "health", "-project", "1", "--json")
	var payload projectHealthPayload
	if err := json.Unmarshal([]byte(output), &payload); err != nil {
		t.Fatalf("decoding: %v\n%s", err, output)
	}
	if len(payload.Releases) != 2 || payload.Releases[0].Release != "shop@1.4.2" {
		t.Fatalf("ranking wrong: %+v", payload.Releases)
	}
	if payload.Started != 250 {
		t.Fatalf("project total %d, want 250", payload.Started)
	}

	text := h.mustRun("releases", "health", "-project", "1")
	for _, want := range []string{"shop@1.4.2", "shop@1.4.1", "95.00%", "100.00%"} {
		if !strings.Contains(text, want) {
			t.Fatalf("%q missing from:\n%s", want, text)
		}
	}

	limited := h.mustRun("releases", "health", "-project", "1", "-limit", "1", "--json")
	var one projectHealthPayload
	if err := json.Unmarshal([]byte(limited), &one); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(one.Releases) != 1 {
		t.Fatalf("the limit was ignored: %d releases", len(one.Releases))
	}
}

// TestReleasesHealthSaysNothingRatherThanZeroPercent: a release with no
// sessions has no crash-free rate, and printing 100% would say the release is
// perfect at the moment the truth is that nothing has reported in.
func TestReleasesHealthSaysNothingRatherThanZeroPercent(t *testing.T) {
	h := newHealthHarness(t)

	text := h.mustRun("releases", "health", "-project", "1")
	if !strings.Contains(text, "no sessions in this range") {
		t.Fatalf("an empty project printed:\n%s", text)
	}

	one := h.mustRun("releases", "health", "-project", "1", "-version", "never@0.0.1")
	if strings.Contains(one, "100.00%") || strings.Contains(one, "0.00%") {
		t.Fatalf("a release nobody reported was given a rate:\n%s", one)
	}
	if !strings.Contains(one, "no sessions in this range") {
		t.Fatalf("an empty release printed:\n%s", one)
	}
}

func TestReleasesHealthRefusesBadArguments(t *testing.T) {
	h := newHealthHarness(t)

	for _, args := range [][]string{
		{"releases", "health"},
		{"releases", "health", "-project", "0"},
		{"releases", "health", "-project", "1", "-from", "yesterday"},
		{"releases", "unknown-verb"},
	} {
		code, _, stderr := h.run(args...)
		if code == ExitOK {
			t.Fatalf("%v succeeded", args)
		}
		if stderr == "" {
			t.Fatalf("%v failed without saying why", args)
		}
	}
}

// TestTruncateBoundsAColumn keeps one long release version from shifting every
// other row of the table.
func TestTruncateBoundsAColumn(t *testing.T) {
	if got := truncate("short", 28); got != "short" {
		t.Fatalf("truncate shortened a short value: %q", got)
	}
	long := strings.Repeat("v", 40)
	if got := truncate(long, 10); len([]rune(got)) != 10 {
		t.Fatalf("truncate produced %q", got)
	}
}
