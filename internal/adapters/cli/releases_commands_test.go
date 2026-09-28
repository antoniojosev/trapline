package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/envelope"
	"github.com/antoniojosev/trapline/internal/usecase"
)

// releaseHarness is the CLI harness with a project already created, because
// every release command needs one.
func releaseHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.setUp()
	h.mustRun("projects", "create", "-name", "venekambio")
	return h
}

// ingestWithRelease pushes one event straight through the ingest use case.
//
// Through the use case rather than over HTTP because what is under test here
// is the CLI, and building a signed envelope in this file would only test
// this file's idea of one.
func ingestWithRelease(t *testing.T, h *harness, message, release string, at time.Time) {
	t.Helper()
	event := fmt.Sprintf(`{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","timestamp":%q,`+
		`"platform":"python","level":"error","release":%q,"environment":"production",`+
		`"exception":{"values":[{"type":"ValueError","value":%q,"stacktrace":{"frames":[`+
		`{"filename":"app/views.py","function":"checkout","lineno":42,"in_app":true}]}}]}}`,
		at.Format(time.RFC3339), release, message)

	body := fmt.Sprintf("{}\n{\"type\":\"event\",\"length\":%d}\n%s\n", len(event), event)
	result, err := h.stack.Ingest.Process(context.Background(), 1,
		bytes.NewReader([]byte(body)), envelope.Limits{}, usecase.ClientInfo{})
	if err != nil {
		t.Fatalf("ingesting: %v", err)
	}
	if result.Accepted != 1 {
		t.Fatalf("ingest accepted %d events, dropped %v", result.Accepted, result.Dropped)
	}
}

func TestReleasesLifecycleThroughTheCLI(t *testing.T) {
	h := releaseHarness(t)

	if got := strings.TrimSpace(h.mustRun("releases", "list", "-project", "1")); got != "no releases" {
		t.Errorf("empty list said %q", got)
	}

	h.mustRun("releases", "create", "-project", "1", "-version", "app@1.0.0")
	listed := h.mustRun("releases", "list", "-project", "1")
	if !strings.Contains(listed, "app@1.0.0") || !strings.Contains(listed, "unreleased") {
		t.Errorf("list = %q, want the unreleased version", listed)
	}

	finalized := h.mustRun("releases", "finalize", "-project", "1", "-version", "app@1.0.0")
	if !strings.Contains(finalized, "released") {
		t.Errorf("finalize said %q", finalized)
	}

	h.mustRun("releases", "deploys", "-project", "1", "-version", "app@1.0.0",
		"-environment", "production", "-name", "pipeline-42")

	shown := h.mustRun("releases", "show", "-project", "1", "-version", "app@1.0.0")
	for _, want := range []string{"app@1.0.0", "deploy     production pipeline-42"} {
		if !strings.Contains(shown, want) {
			t.Errorf("show = %q, want it to contain %q", shown, want)
		}
	}
}

func TestReleasesCommitsFromAFile(t *testing.T) {
	// A pipeline already has its commits as JSON; inventing a flag syntax for
	// nested records would be a small language nobody asked for.
	h := releaseHarness(t)

	path := filepath.Join(t.TempDir(), "commits.json")
	commits := `[{"id":"a3f9c1e","message":"fix the total","author_name":"Ana",
		"patch_set":[{"path":"app/views.py","type":"M"}]},{"id":"b7d2f04","message":"tidy"}]`
	if err := os.WriteFile(path, []byte(commits), 0o600); err != nil {
		t.Fatalf("writing commits: %v", err)
	}

	// A bare array, which is what a `git log --format` pipeline produces.
	out := h.mustRun("releases", "commits", "-project", "1", "-version", "app@1.0.0", "-file", path)
	if !strings.Contains(out, "2 commits") {
		t.Errorf("commits said %q", out)
	}

	var detail map[string]any
	raw := h.mustRun("releases", "show", "-project", "1", "-version", "app@1.0.0", "--json")
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		t.Fatalf("decoding show --json: %v", err)
	}
	if detail["commit_count"].(float64) != 2 {
		t.Errorf("commit_count = %v, want 2", detail["commit_count"])
	}
}

func TestReleasesCommitsRejectsAFileThatIsNotJSON(t *testing.T) {
	// The error has to name the operator's file, not arrive as a 400 from a
	// server whose logs they may not be able to read.
	h := releaseHarness(t)
	path := filepath.Join(t.TempDir(), "commits.json")
	if err := os.WriteFile(path, []byte("not json at all"), 0o600); err != nil {
		t.Fatalf("writing commits: %v", err)
	}

	code, _, stderr := h.run("releases", "commits", "-project", "1", "-version", "app@1.0.0", "-file", path)
	if code != ExitError {
		t.Errorf("exit = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr, "commit set") {
		t.Errorf("stderr = %q, want it to name what it could not read", stderr)
	}
}

// TestResolveInNextReleaseThroughTheCLI is the feature's story from the command
// line, which is where an agent lives.
func TestResolveInNextReleaseThroughTheCLI(t *testing.T) {
	h := releaseHarness(t)
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)

	ingestWithRelease(t, h, "invalid amount", "app@1.0.0", now)
	h.mustRun("issues", "resolve", "-project", "1", "-issue", "1", "--next-release")

	// The pod that has not been redeployed yet.
	ingestWithRelease(t, h, "invalid amount", "app@1.0.0", now.Add(time.Minute))

	shown := h.mustRun("issues", "show", "-project", "1", "-issue", "1")
	if !strings.Contains(shown, "resolved") {
		t.Errorf("issue = %q, want it still resolved", shown)
	}
	if !strings.Contains(shown, "suppressed 1 events from the resolved release") {
		t.Errorf("issue = %q, want the suppressed count visible", shown)
	}

	// The new build, still broken.
	ingestWithRelease(t, h, "invalid amount", "app@1.0.1", now.Add(time.Hour))

	shown = h.mustRun("issues", "show", "-project", "1", "-issue", "1")
	if !strings.Contains(shown, "regressed  1 times, most recently in app@1.0.1") {
		t.Errorf("issue = %q, want the regression named with its release", shown)
	}
	if !strings.Contains(shown, "first in   app@1.0.0") {
		t.Errorf("issue = %q, want the release it was born in", shown)
	}

	// And the release listing knows both builds without anyone registering
	// either of them.
	listed := h.mustRun("releases", "list", "-project", "1")
	for _, want := range []string{"app@1.0.0", "app@1.0.1"} {
		if !strings.Contains(listed, want) {
			t.Errorf("releases = %q, want %q", listed, want)
		}
	}
}

func TestReleasesUsageErrors(t *testing.T) {
	h := releaseHarness(t)

	cases := [][]string{
		{"releases"},
		{"releases", "nonsense"},
		{"releases", "list"},
		{"releases", "create", "-project", "1"},
		{"releases", "show", "-project", "1"},
		{"releases", "finalize", "-project", "1"},
		{"releases", "commits", "-project", "1"},
		{"releases", "deploys", "-project", "1", "-version", "app@1.0.0"},
		{"releases", "create", "-project", "1", "-version", "app@1.0.0", "-released", "yesterday"},
		{"releases", "finalize", "-project", "1", "-version", "app@1.0.0", "-released", "yesterday"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if code, _, _ := h.run(args...); code != ExitUsage {
				t.Errorf("exit = %d, want %d", code, ExitUsage)
			}
		})
	}
}

func TestReleasesJSONIsMachineReadable(t *testing.T) {
	// Every command emits JSON on demand, because an agent parses stdout
	// (ADR 006).
	h := releaseHarness(t)
	h.mustRun("releases", "create", "-project", "1", "-version", "app@1.0.0")

	for _, args := range [][]string{
		{"releases", "list", "-project", "1", "--json"},
		{"releases", "list", "-project", "1", "-limit", "5", "--json"},
		{"releases", "show", "-project", "1", "-version", "app@1.0.0", "--json"},
		{"releases", "finalize", "-project", "1", "-version", "app@1.0.0",
			"-released", "2026-08-24T10:00:00Z", "--json"},
		{"releases", "deploys", "-project", "1", "-version", "app@1.0.0",
			"-environment", "staging", "-link", "https://ci.example.test/7", "--json"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			out := h.mustRun(args...)
			var decoded any
			if err := json.Unmarshal([]byte(out), &decoded); err != nil {
				t.Errorf("%v emitted unparseable JSON %q: %v", args, out, err)
			}
		})
	}
}

func TestReleasesReportsAServerError(t *testing.T) {
	h := releaseHarness(t)

	code, _, stderr := h.run("releases", "show", "-project", "1", "-version", "app@9.9.9")
	if code != ExitError {
		t.Errorf("exit = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr, "not found") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestReleasesNeedsAToken(t *testing.T) {
	// The remote commands are useless without one, and saying so is better
	// than a 401 the operator has to interpret.
	var out, errOut bytes.Buffer
	env := func(string) string { return "" }
	code := Run(context.Background(), []string{"releases", "list", "-project", "1"}, env, &out, &errOut)
	if code != ExitError {
		t.Errorf("exit = %d, want %d", code, ExitError)
	}
	if !strings.Contains(errOut.String(), "TRAPLINE_TOKEN") {
		t.Errorf("stderr = %q, want it to name the variable to set", errOut.String())
	}
}

func TestReleasesPathEscapesTheVersion(t *testing.T) {
	// "myapp@1.0.0+build 7" is not a hypothetical.
	got := releasesPath(3, "myapp@1.0.0+build 7")
	if strings.Contains(got, " ") {
		t.Errorf("path = %q, want the version escaped", got)
	}
	if bare := releasesPath(3, ""); bare != "/projects/3/releases" {
		t.Errorf("path with no version = %q", bare)
	}
}

func TestDeployToAnEnvironmentThatIsNotNamed(t *testing.T) {
	h := releaseHarness(t)
	if code, _, _ := h.run("releases", "deploys", "-project", "1",
		"-version", "app@1.0.0", "-environment", ""); code != ExitUsage {
		t.Error("a deploy to nowhere was accepted")
	}
}
