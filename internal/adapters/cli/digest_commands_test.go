package cli

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antoniojosev/trapline/internal/adapters/httpapi"
	"github.com/antoniojosev/trapline/internal/adapters/sqlite"
	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/wiring"
)

// TestDigestPreviewThroughTheCLI: the text output is the mail itself, because
// what a person wants from `digest preview` is to read what will arrive.
func TestDigestPreviewThroughTheCLI(t *testing.T) {
	h := newHarness(t)
	h.setUp()

	text := h.mustRun("digest", "preview")
	if !strings.HasPrefix(text, "trapline — the week of ") {
		t.Errorf("the preview printed:\n%s", text)
	}

	var preview digestPreviewPayload
	if err := json.Unmarshal([]byte(h.mustRun("digest", "preview", "--json")), &preview); err != nil {
		t.Fatalf("--json output is not JSON: %v", err)
	}
	if preview.Schedule.Weekday != "Monday" || preview.Schedule.Hour != 9 {
		t.Errorf("the default schedule came back as %+v", preview.Schedule)
	}
	if preview.Report.Covers.From == "" || preview.Report.Covers.To == "" {
		t.Errorf("the report does not say which week it covers: %+v", preview.Report.Covers)
	}
	if preview.Text == "" {
		t.Error("--json dropped the rendered text, which is what a channel would deliver")
	}
}

func TestDigestPreviewRefusesATimeItCannotRead(t *testing.T) {
	h := newHarness(t)
	h.setUp()

	code, _, stderr := h.run("digest", "preview", "-at", "last tuesday")
	if code != ExitUsage {
		t.Errorf("exit code = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(stderr, "RFC 3339") {
		t.Errorf("the message does not say what is expected: %q", stderr)
	}
}

func TestDigestScheduleThroughTheCLI(t *testing.T) {
	h := newHarness(t)
	h.setUp()

	if got := strings.TrimSpace(h.mustRun("digest", "schedule")); got != "Monday at 09:00 UTC" {
		t.Errorf("the default schedule printed as %q", got)
	}

	if got := strings.TrimSpace(h.mustRun("digest", "schedule", "-day", "friday", "-hour", "17")); got != "Friday at 17:00 UTC" {
		t.Errorf("the changed schedule printed as %q", got)
	}
	// Read back through a second invocation, so the assertion is about what
	// was stored rather than what the write echoed.
	if got := strings.TrimSpace(h.mustRun("digest", "schedule")); got != "Friday at 17:00 UTC" {
		t.Errorf("read back as %q", got)
	}

	var schedule digestSchedulePayload
	if err := json.Unmarshal([]byte(h.mustRun("digest", "schedule", "--json")), &schedule); err != nil {
		t.Fatalf("--json output is not JSON: %v", err)
	}
	if schedule.Weekday != "Friday" || schedule.Hour != 17 || schedule.Timezone != "UTC" {
		t.Errorf("--json said %+v", schedule)
	}
}

// TestDigestScheduleRefusesHalfAChange: "Tuesday, at whatever hour it was"
// reads like it means something, and is a good way to move a report to an hour
// nobody chose.
func TestDigestScheduleRefusesHalfAChange(t *testing.T) {
	h := newHarness(t)
	h.setUp()

	for _, args := range [][]string{
		{"digest", "schedule", "-day", "friday"},
		{"digest", "schedule", "-hour", "17"},
	} {
		code, _, stderr := h.run(args...)
		if code != ExitUsage {
			t.Errorf("%v exited %d, want %d", args, code, ExitUsage)
		}
		if !strings.Contains(stderr, "both") {
			t.Errorf("%v said %q", args, stderr)
		}
	}
}

func TestDigestScheduleRefusesADayThatIsNotADay(t *testing.T) {
	h := newHarness(t)
	h.setUp()

	code, _, stderr := h.run("digest", "schedule", "-day", "caturday", "-hour", "9")
	if code != ExitError {
		t.Errorf("exit code = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr, "not a day") {
		t.Errorf("the server's message did not reach the terminal: %q", stderr)
	}
}

func TestDigestRejectsAnUnknownSubcommand(t *testing.T) {
	h := newHarness(t)

	for _, args := range [][]string{
		{"digest"},
		{"digest", "send-now"},
	} {
		if code, _, _ := h.run(args...); code != ExitUsage {
			t.Errorf("%v exited %d, want %d", args, code, ExitUsage)
		}
	}
}

// TestDoctorReportsTheChannels is the half of `doctor` the digest adds: a
// server with no notification subsystem says so, and says what it costs, and
// stays green — because choosing not to configure alerting is a choice, not a
// fault.
func TestDoctorReportsTheChannels(t *testing.T) {
	h := newHarness(t)
	h.setUp()

	output := h.mustRun("doctor")
	if !strings.Contains(output, "alert channels") {
		t.Errorf("doctor said nothing about the channels:\n%s", output)
	}

	var report doctorReport
	if err := json.Unmarshal([]byte(h.mustRun("doctor", "--json")), &report); err != nil {
		t.Fatalf("--json output is not JSON: %v", err)
	}
	if !report.OK {
		t.Errorf("an installation with no channels is not a broken one:\n%+v", report.Checks)
	}

	var found bool
	for _, check := range report.Checks {
		if check.Name == "alert channels" {
			found = true
			if !check.OK {
				t.Errorf("the channel check failed: %s", check.Detail)
			}
		}
	}
	if !found {
		t.Error("the channel check is missing from --json, so an agent cannot see it")
	}
}

// newChannelHarness is newHarness with real channels configured through the
// real use case.
//
// It used to attach a double for the notification subsystem, because the digest
// was built beside the channels and the seam between them was all that existed
// (ADR 035).
// Now that the seam is wired, a double would be testing the wrong program:
// what `doctor` reports has to come out of the same repository, through the
// same cipher, that a channel added from the panel goes into.
func newChannelHarness(t *testing.T, channels ...domain.AlertChannel) *harness {
	t.Helper()
	return channelHarness(t, false, channels...)
}

// newChannelHarnessWithoutItsKey configures the channels and then reassembles
// the same database against a different key file.
//
// That is not a contrived state: it is a backup restored without the `.key`
// that sat beside it, which is the one failure the encryption at rest of
// ADR 015 makes possible and the reason `doctor` checks the secret at all.
// Producing it, rather than handing the report a SecretError to print, is the
// difference between testing that the cipher notices and testing that a
// string travels.
func newChannelHarnessWithoutItsKey(t *testing.T, channels ...domain.AlertChannel) *harness {
	t.Helper()
	return channelHarness(t, true, channels...)
}

func channelHarness(t *testing.T, loseTheKey bool, channels ...domain.AlertChannel) *harness {
	t.Helper()
	ctx := context.Background()

	directory := t.TempDir()
	db, err := sqlite.Open(ctx, filepath.Join(directory, "trapline.db"))
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	origin := domain.Origin{Scheme: "https", Host: "errors.example.test"}
	options := wiring.Options{
		Origin:        origin,
		Clock:         testClock{},
		SecretKeyPath: filepath.Join(directory, "trapline.key"),
	}
	configured := wiring.New(db, options)

	for index := range channels {
		channel := &channels[index]
		if _, err := configured.Alerts.AddChannel(
			ctx, channel.Type, channel.Name, channel.Config, channel.Digest,
		); err != nil {
			t.Fatalf("adding the %s channel: %v", channel.Type, err)
		}
	}

	stack := configured
	if loseTheKey {
		// A second assembly over the same database, naming a key file that
		// does not exist yet: it is created on first use and it is not the
		// one the rows above were sealed with.
		options.SecretKeyPath = filepath.Join(directory, "somebody-elses.key")
		stack = wiring.New(db, options)
	}

	// After the channels, so the conditional jobs are asked their question
	// against the installation the test actually describes.
	if err := stack.Scheduler.Start(ctx); err != nil {
		t.Fatalf("starting the jobs: %v", err)
	}
	t.Cleanup(stack.Scheduler.Stop)

	server := httptest.NewServer(httpapi.NewServer(
		stack.Auth, stack.Projects, stack.Tokens, stack.Ingest, stack.Issues, stack.Stats, origin, "test",
	).WithJobs(stack.Scheduler).WithDigest(stack.Digest, stack.Channels).Handler())
	t.Cleanup(server.Close)

	_, plaintext, err := stack.Tokens.Create(ctx, "cli-test", domain.AllScopes(), nil)
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}
	h := &harness{t: t, stack: stack, server: server, token: plaintext}
	h.setUp()
	return h
}

// TestDoctorFailsOnAChannelThatCannotBeReached is the point of the whole
// check: a channel that will fail on the day something breaks has to fail
// here instead, on a day nobody is under pressure.
func TestDoctorFailsOnAChannelThatCannotBeReached(t *testing.T) {
	h := newChannelHarness(t, domain.AlertChannel{
		Type: domain.ChannelWebhook, Name: "pager", Digest: true,
		// Port 1 on loopback: nothing has ever listened there.
		Config: domain.ChannelConfig{
			URL:    "http://127.0.0.1:1/hook",
			Secret: "a-secret-long-enough-for-a-test",
		},
	})

	code, stdout, _ := h.run("doctor")
	if code != ExitError {
		t.Errorf("doctor exited %d for an unreachable channel, want %d", code, ExitError)
	}
	if !strings.Contains(stdout, "channel pager") {
		t.Errorf("the report does not name the channel:\n%s", stdout)
	}
	if !strings.Contains(stdout, "127.0.0.1:1") {
		t.Errorf("the report does not name what could not be reached:\n%s", stdout)
	}
}

// TestDoctorFailsOnAChannelWhoseSecretDoesNotOpen is the other half. It is not
// a network problem and must not read like one.
func TestDoctorFailsOnAChannelWhoseSecretDoesNotOpen(t *testing.T) {
	h := newChannelHarnessWithoutItsKey(t, domain.AlertChannel{
		Type: domain.ChannelTelegram, Name: "ops",
		Config: domain.ChannelConfig{BotToken: "123:ABC", ChatID: "-1001"},
	})

	code, stdout, _ := h.run("doctor")
	if code != ExitError {
		t.Errorf("doctor exited %d, want %d", code, ExitError)
	}
	if !strings.Contains(stdout, "decrypted") {
		t.Errorf("the report does not say the secret is the problem:\n%s", stdout)
	}

	var report doctorReport
	if err := json.Unmarshal([]byte(h.mustRunFailing("doctor", "--json")), &report); err != nil {
		t.Fatalf("--json output is not JSON: %v", err)
	}
	if len(report.Channels) != 1 {
		t.Fatalf("--json carried %d channels", len(report.Channels))
	}
	if report.Channels[0].SecretOK {
		t.Error("--json says the secret is fine")
	}
	if report.Channels[0].Reachable {
		t.Error("--json claims a connectivity check that could not have happened")
	}
}

// TestDoctorExplainsAMissingDigestJob closes the gap ADR 014 left open: an
// operator who expects a job and does not see it should not have to read the
// scheduler's source to learn why.
func TestDoctorExplainsAMissingDigestJob(t *testing.T) {
	h := newChannelHarness(t, domain.AlertChannel{
		Type: domain.ChannelSlack, Name: "#alerts", Digest: false,
		// Loopback with nothing listening, rather than a plausible-looking
		// hostname: a test that resolves a name depends on the DNS of
		// whatever machine it runs on, and this one is not about the network.
		Config: domain.ChannelConfig{URL: "http://127.0.0.1:1/services/x"},
	})

	_, stdout, _ := h.run("doctor")
	if !strings.Contains(stdout, "weekly digest") {
		t.Errorf("doctor does not explain the absent digest job:\n%s", stdout)
	}
	if !strings.Contains(stdout, "no channel asked for it") {
		t.Errorf("the explanation does not say why:\n%s", stdout)
	}
}
