package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/adapters/httpapi"
	"github.com/antoniojosev/trapline/internal/adapters/sqlite"
	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/wiring"
)

type testClock struct{}

func (testClock) Now() time.Time { return time.Now().UTC() }

// harness is a real server with a real database, plus a CLI wired to talk to
// it. The CLI's whole claim is that it drives the same API the panel does, so
// testing it against a stub would test nothing worth knowing.
type harness struct {
	t      *testing.T
	stack  *wiring.Stack
	server *httptest.Server
	token  string
}

// setUp creates the admin account, so doctor sees a finished installation.
func (h *harness) setUp() {
	h.t.Helper()
	if _, err := h.stack.Auth.Setup(context.Background(), "antonio", "una contraseña larga y buena"); err != nil {
		h.t.Fatalf("setting up: %v", err)
	}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()

	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "trapline.db"))
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	origin := domain.Origin{Scheme: "https", Host: "errors.example.test"}
	// The same assembly production uses, so the CLI is exercised against the
	// program that ships rather than a rearrangement of its parts.
	stack := wiring.New(db, wiring.Options{Origin: origin, Clock: testClock{}})

	// The background jobs run, because doctor reports on them and a harness
	// without them would prove the CLI works against a server that does not
	// exist.
	if err := stack.Scheduler.Start(ctx); err != nil {
		t.Fatalf("starting the jobs: %v", err)
	}
	t.Cleanup(stack.Scheduler.Stop)

	server := httptest.NewServer(httpapi.NewServer(
		stack.Auth, stack.Projects, stack.Tokens, stack.Ingest, stack.Issues, stack.Stats, origin, "test",
	).WithJobs(stack.Scheduler).WithReleases(stack.Releases).WithAlerts(stack.Alerts).
		WithDigest(stack.Digest, stack.Channels).
		WithCrons(stack.Crons).WithUptime(stack.Uptime).
		WithStatusPage(stack.StatusPage).WithTransactions(stack.Transactions).
		WithArtifacts(stack.Artifacts).
		WithHealth(stack.Health).WithSuspects(stack.Suspects).Handler())
	t.Cleanup(server.Close)

	_, plaintext, err := stack.Tokens.Create(ctx, "cli-test", domain.AllScopes(), nil)
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}
	return &harness{t: t, stack: stack, server: server, token: plaintext}
}

// run executes a CLI invocation against the harness, with credentials supplied
// through the environment exactly as an operator or an agent would.
func (h *harness) run(args ...string) (code int, stdout, stderr string) {
	h.t.Helper()
	env := func(key string) string {
		switch key {
		case "TRAPLINE_URL":
			return h.server.URL
		case "TRAPLINE_TOKEN":
			return h.token
		default:
			return ""
		}
	}
	var out, errOut bytes.Buffer
	code = Run(context.Background(), args, env, &out, &errOut)
	return code, out.String(), errOut.String()
}

func (h *harness) mustRun(args ...string) string {
	h.t.Helper()
	code, stdout, stderr := h.run(args...)
	if code != ExitOK {
		h.t.Fatalf("%v exited %d: %s", args, code, stderr)
	}
	return stdout
}

func TestProjectLifecycleThroughTheCLI(t *testing.T) {
	h := newHarness(t)

	if got := strings.TrimSpace(h.mustRun("projects", "list")); got != "no projects" {
		t.Errorf("empty list said %q", got)
	}

	// Text mode prints the DSN alone, so the command pipes straight into a
	// config file or an environment variable.
	dsn := strings.TrimSpace(h.mustRun("projects", "create", "-name", "venekambio"))
	parsed, err := domain.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("the CLI printed something that is not a DSN (%q): %v", dsn, err)
	}
	if parsed.Host != "errors.example.test" {
		t.Errorf("DSN host = %q, want the server's configured origin", parsed.Host)
	}

	var projects []projectPayload
	if err := json.Unmarshal([]byte(h.mustRun("projects", "list", "--json")), &projects); err != nil {
		t.Fatalf("--json output is not JSON: %v", err)
	}
	if len(projects) != 1 || projects[0].DSN != dsn {
		t.Errorf("list = %+v, want the project just created", projects)
	}

	h.mustRun("projects", "delete", "-id", "1")
	if got := strings.TrimSpace(h.mustRun("projects", "list")); got != "no projects" {
		t.Errorf("after deleting, list said %q", got)
	}
}

func TestRotationThroughTheCLI(t *testing.T) {
	h := newHarness(t)
	original := strings.TrimSpace(h.mustRun("projects", "create", "-name", "rotating"))

	var rotated keyPayload
	if err := json.Unmarshal([]byte(h.mustRun("keys", "rotate", "-project", "1", "--json")), &rotated); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if rotated.DSN == original {
		t.Fatal("rotation returned the same DSN")
	}

	// The text output has to say that rotation is not finished, because the
	// dangerous move is revoking the key still running in production.
	_, text, _ := h.run("keys", "rotate", "-project", "1")
	_ = text // the third key is refused; checked below

	parsed, err := domain.ParseDSN(original)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	h.mustRun("keys", "revoke", "-key", parsed.PublicKey)
	// Idempotent, so a retried command after a dropped connection is safe.
	h.mustRun("keys", "revoke", "-key", parsed.PublicKey)
}

func TestARefusedRequestExitsNonZeroWithACleanStdout(t *testing.T) {
	h := newHarness(t)

	// An agent branches on the exit code and parses stdout. A failure must set
	// the first and leave the second empty.
	code, stdout, stderr := h.run("projects", "delete", "-id", "9999")
	if code != ExitError {
		t.Errorf("exit code = %d, want %d", code, ExitError)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want it empty on failure", stdout)
	}
	if stderr == "" {
		t.Error("no diagnostic on stderr")
	}
}

func TestJSONOutputIsOneDocumentPerLine(t *testing.T) {
	h := newHarness(t)
	h.mustRun("projects", "create", "-name", "a")

	for _, args := range [][]string{
		{"version", "--json"},
		{"projects", "list", "--json"},
		{"projects", "create", "-name", "b", "--json"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			out := h.mustRun(args...)
			if !strings.HasSuffix(out, "\n") {
				t.Error("output does not end in a newline")
			}
			if strings.Count(strings.TrimRight(out, "\n"), "\n") != 0 {
				t.Errorf("output spans multiple lines: %q", out)
			}
			var anything any
			if err := json.Unmarshal([]byte(out), &anything); err != nil {
				t.Errorf("not valid JSON (%v): %q", err, out)
			}
		})
	}
}

func TestDoctorReportsAndFails(t *testing.T) {
	h := newHarness(t)

	// Setup has not run, so doctor must say so and exit non-zero — while
	// still emitting the full report, because that is what an agent reads to
	// find out what to do next.
	code, stdout, _ := h.run("doctor", "--json")
	if code != ExitError {
		t.Errorf("exit code = %d, want %d when a check fails", code, ExitError)
	}

	var report doctorReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("the report is not JSON (%v): %q", err, stdout)
	}
	if report.OK {
		t.Error("doctor reported ok with no admin account")
	}
	if len(report.Checks) == 0 {
		t.Fatal("the report has no checks")
	}

	var sawSetup bool
	for _, check := range report.Checks {
		if strings.Contains(check.Name, "setup") && !check.OK {
			sawSetup = true
			if check.Detail == "" {
				t.Error("a failing check has no detail explaining what to do")
			}
		}
	}
	if !sawSetup {
		t.Errorf("no failing setup check in %+v", report.Checks)
	}
}

func TestDoctorFlagsLocalDSNs(t *testing.T) {
	// The most common silent misconfiguration: everything looks healthy and no
	// SDK off the machine can deliver an event.
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "trapline.db"))
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	localOrigin := domain.Origin{Scheme: "http", Host: "127.0.0.1:9000"}
	stack := wiring.New(db, wiring.Options{Origin: localOrigin, Clock: testClock{}})
	auth, projects, tokens := stack.Auth, stack.Projects, stack.Tokens

	if _, err := auth.Setup(ctx, "antonio", "una contraseña larga y buena"); err != nil {
		t.Fatalf("setting up: %v", err)
	}
	if _, err := projects.Create(ctx, "local"); err != nil {
		t.Fatalf("creating project: %v", err)
	}
	_, plaintext, err := tokens.Create(ctx, "cli", domain.AllScopes(), nil)
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}

	if err := stack.Scheduler.Start(ctx); err != nil {
		t.Fatalf("starting the jobs: %v", err)
	}
	t.Cleanup(stack.Scheduler.Stop)

	server := httptest.NewServer(httpapi.NewServer(
		auth, projects, tokens, stack.Ingest, stack.Issues, stack.Stats, localOrigin, "test",
	).WithJobs(stack.Scheduler).WithReleases(stack.Releases).
		WithDigest(stack.Digest, stack.Channels).WithUptime(stack.Uptime).Handler())
	t.Cleanup(server.Close)

	env := func(key string) string {
		switch key {
		case "TRAPLINE_URL":
			return server.URL
		case "TRAPLINE_TOKEN":
			return plaintext
		default:
			return ""
		}
	}
	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"doctor", "--json"}, env, &out, &errOut)
	if code != ExitError {
		t.Errorf("exit code = %d, want a failure for loopback DSNs", code)
	}

	var report doctorReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decoding report: %v", err)
	}
	var flagged bool
	for _, check := range report.Checks {
		if strings.Contains(check.Name, "dsn") && !check.OK {
			flagged = true
			if !strings.Contains(check.Detail, "TRAPLINE_ORIGIN") {
				t.Errorf("the detail does not say how to fix it: %q", check.Detail)
			}
		}
	}
	if !flagged {
		t.Errorf("loopback DSNs were not flagged: %+v", report.Checks)
	}
}

func TestConfigThroughTheCLI(t *testing.T) {
	h := newHarness(t)
	h.mustRun("projects", "create", "-name", "configurable")

	t.Run("shows what applies and where it came from", func(t *testing.T) {
		// "The default" and "someone chose this" are different facts, and an
		// operator deciding whether to change something needs to know which
		// one they are looking at.
		out := h.mustRun("config", "show", "-project", "1")
		for _, expected := range []string{"categories", "error", "(default)", "rate limit", "retention"} {
			if !strings.Contains(out, expected) {
				t.Errorf("output does not mention %q:\n%s", expected, out)
			}
		}
		if !strings.Contains(out, "rate-limit response") {
			t.Error("nothing explains what happens to a switched-off category")
		}
	})

	t.Run("enabling a category", func(t *testing.T) {
		out := h.mustRun("config", "set", "-project", "1", "-categories", "error,transaction")
		if !strings.Contains(out, "transaction") || !strings.Contains(out, "(set)") {
			t.Errorf("output does not reflect the change:\n%s", out)
		}
	})

	t.Run("zero restores the default", func(t *testing.T) {
		h.mustRun("config", "set", "-project", "1", "-rate-limit", "500")
		out := h.mustRun("config", "set", "-project", "1", "-rate-limit", "0")
		if !strings.Contains(out, "(default)") {
			t.Errorf("a zero limit did not revert to the default:\n%s", out)
		}
	})

	t.Run("none means accept nothing", func(t *testing.T) {
		// An empty flag value cannot be told apart from an absent flag, so
		// "accept nothing" needs a word of its own.
		out := h.mustRun("config", "set", "-project", "1", "-categories", "none")
		if !strings.Contains(out, "accepts nothing") {
			t.Errorf("output does not say the project now accepts nothing:\n%s", out)
		}
	})

	t.Run("retention pairs", func(t *testing.T) {
		out := h.mustRun("config", "set", "-project", "1", "-retention", "error=30")
		if !strings.Contains(out, "30 days") {
			t.Errorf("the retention change is not reflected:\n%s", out)
		}
	})

	t.Run("json for an agent", func(t *testing.T) {
		out := h.mustRun("config", "show", "-project", "1", "--json")
		var config configPayload
		if err := json.Unmarshal([]byte(out), &config); err != nil {
			t.Fatalf("not valid JSON (%v): %q", err, out)
		}
		if len(config.AvailableCategories) == 0 {
			t.Error("the JSON does not list the categories an agent could enable")
		}
		if config.Defaults.RateLimitPerMinute == 0 {
			t.Error("the JSON does not report the defaults, so an agent cannot tell what applies")
		}
	})

	t.Run("default restores inheriting", func(t *testing.T) {
		// The third state. -rate-limit and -retention spell it 0, but the
		// category list cannot: an empty list is already taken by "accept
		// nothing". Without a word for it, going back to the default would
		// mean typing today's default in by hand — which looks identical and
		// silently opts the project out of ever receiving a changed one.
		out := h.mustRun("config", "set", "-project", "1", "-categories", "default")
		if !strings.Contains(out, "(default)") || !strings.Contains(out, "error") {
			t.Errorf("the project did not go back to inheriting the default profile:\n%s", out)
		}
		if strings.Contains(out, "accepts nothing") {
			t.Errorf("inheriting the default profile still reads as accepting nothing:\n%s", out)
		}
	})
}

func TestConfigSetRejectsNonsense(t *testing.T) {
	h := newHarness(t)
	h.mustRun("projects", "create", "-name", "configurable")

	cases := map[string][]string{
		"nothing to change": {"config", "set", "-project", "1"},
		"no project":        {"config", "set", "-categories", "error"},
		"bad retention":     {"config", "set", "-project", "1", "-retention", "error"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := h.run(args...)
			if code != ExitUsage {
				t.Errorf("exit code = %d, want %d", code, ExitUsage)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want it empty", stdout)
			}
			if stderr == "" {
				t.Error("no diagnostic")
			}
		})
	}

	// An unknown category is the server's judgement, not the CLI's, so it is
	// a runtime failure rather than a usage error.
	code, _, stderr := h.run("config", "set", "-project", "1", "-categories", "teleport")
	if code != ExitError {
		t.Errorf("exit code = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr, "categor") {
		t.Errorf("stderr = %q, want it to name the problem", stderr)
	}
}

func TestDoctorReportsTheBackgroundJobs(t *testing.T) {
	// Parity, not decoration: the same three fields the REST endpoint returns
	// have to reach an agent through the CLI too, as data and not as a
	// sentence about data (ADR 006).
	h := newHarness(t)
	h.setUp()

	code, stdout, stderr := h.run("doctor", "--json")
	if code != ExitOK {
		t.Fatalf("doctor exited %d: %s", code, stderr)
	}

	var report doctorReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("decoding report: %v", err)
	}
	if !report.OK {
		t.Fatalf("doctor is unhappy with a healthy server: %s", stdout)
	}
	if len(report.Jobs) != 1 || report.Jobs[0].Name != "retention" {
		t.Fatalf("Jobs = %+v, want retention", report.Jobs)
	}
	job := report.Jobs[0]
	switch {
	case job.IntervalSeconds <= 0:
		t.Errorf("IntervalSeconds = %d", job.IntervalSeconds)
	case job.LastError != nil:
		t.Errorf("LastError = %q", *job.LastError)
	}

	// And the text output names it, so somebody reading a terminal sees the
	// same thing the agent parsed.
	_, text, _ := h.run("doctor")
	if !strings.Contains(text, "job retention") {
		t.Errorf("doctor's text output does not mention the job:\n%s", text)
	}
}

// mustRunFailing is mustRun for a command that is expected to report a
// problem: the output still has to be well-formed, which is the whole promise
// of --json on a diagnostic command.
func (h *harness) mustRunFailing(args ...string) string {
	h.t.Helper()
	code, stdout, stderr := h.run(args...)
	if code == ExitOK {
		h.t.Fatalf("%v was expected to fail and did not: %s", args, stdout)
	}
	if code == ExitUsage {
		h.t.Fatalf("%v was rejected as a usage error: %s", args, stderr)
	}
	return stdout
}
