package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/envelope"
	"github.com/antoniojosev/trapline/internal/usecase"
)

// gitRepo builds a throwaway repository with three commits, one of which
// touches src/checkout.ts.
//
// A real repository rather than a recorded transcript, because the thing under
// test is what `git log --name-status` actually prints — including how it
// formats a rename and where it puts the blank line after a multi-line commit
// message. A fixture of that output would pin this file's idea of git.
func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		command := exec.CommandContext(t.Context(), "git", append([]string{"-C", repo}, args...)...)
		command.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Antonio Vila", "GIT_AUTHOR_EMAIL=antonio@example.com",
			"GIT_COMMITTER_NAME=Antonio Vila", "GIT_COMMITTER_EMAIL=antonio@example.com",
			"GIT_AUTHOR_DATE=2026-09-20T10:00:00Z", "GIT_COMMITTER_DATE=2026-09-20T10:00:00Z")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	write := func(name, content string) {
		t.Helper()
		full := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("creating %s: %v", name, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	run("init", "-q", "-b", "main")
	write("README.md", "# demo\n")
	run("add", ".")
	run("commit", "-qm", "first commit")

	write("src/cart.ts", "export const cart = [];\n")
	run("add", ".")
	// A multi-line message whose body contains a line that looks exactly like
	// a name-status entry. This is the input that breaks a parser which
	// splits the message from the file list by guessing.
	run("commit", "-qm", "add the cart\n\nM\tsrc/not-a-real-file.ts\n")

	write("src/checkout.ts", "export function total() { return undefined(); }\n")
	run("add", ".")
	run("commit", "-qm", "rewrite the totals")

	return repo
}

func TestGitCommitsReadsPathsOutOfARepository(t *testing.T) {
	repo := gitRepo(t)

	commits, err := gitCommits(context.Background(), repo, "", "HEAD", 0)
	if err != nil {
		t.Fatalf("gitCommits: %v", err)
	}
	if len(commits) != 3 {
		t.Fatalf("read %d commits, want 3: %+v", len(commits), commits)
	}

	// Newest first, which is the order every tool that sends a commit set
	// means and the order the domain reads ordinals from.
	newest := commits[0]
	if newest.Message != "rewrite the totals" {
		t.Fatalf("newest commit is %q", newest.Message)
	}
	if len(newest.PatchSet) != 1 || newest.PatchSet[0].Path != "src/checkout.ts" {
		t.Fatalf("patch set = %+v, want src/checkout.ts", newest.PatchSet)
	}
	if newest.PatchSet[0].Type != "A" {
		t.Errorf("change type = %q, want A for a file this commit created", newest.PatchSet[0].Type)
	}
	if newest.AuthorName != "Antonio Vila" || newest.AuthorEmail != "antonio@example.com" {
		t.Errorf("author = %q <%q>", newest.AuthorName, newest.AuthorEmail)
	}
	if newest.Timestamp == nil || newest.Timestamp.Year() != 2026 {
		t.Errorf("timestamp = %v", newest.Timestamp)
	}
	if len(newest.ID) != 40 {
		t.Errorf("sha = %q, want the full 40 characters", newest.ID)
	}

	// The commit whose *message* contains a name-status line. It touched one
	// file; a parser that could not tell the message from the file list would
	// say two.
	middle := commits[1]
	if len(middle.PatchSet) != 1 || middle.PatchSet[0].Path != "src/cart.ts" {
		t.Fatalf("a commit message that looks like a file list was parsed as one: %+v",
			middle.PatchSet)
	}
}

func TestGitCommitsReadsARangeAndARename(t *testing.T) {
	repo := gitRepo(t)

	head := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD"))
	previous := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD~1"))

	commits, err := gitCommits(context.Background(), repo, previous, head, 0)
	if err != nil {
		t.Fatalf("gitCommits: %v", err)
	}
	if len(commits) != 1 || commits[0].Message != "rewrite the totals" {
		t.Fatalf("a range of one commit read %d: %+v", len(commits), commits)
	}

	// A rename is two facts: the old path stopped existing and the new one
	// started. An event from before the rename still names the old file.
	runGit(t, repo, "mv", "src/cart.ts", "src/basket.ts")
	runGit(t, repo, "commit", "-qm", "rename the cart")
	renamed, err := gitCommits(context.Background(), repo, head, "HEAD", 0)
	if err != nil {
		t.Fatalf("gitCommits after the rename: %v", err)
	}
	if len(renamed) != 1 {
		t.Fatalf("read %d commits, want the rename", len(renamed))
	}
	paths := map[string]string{}
	for _, file := range renamed[0].PatchSet {
		paths[file.Path] = file.Type
	}
	if paths["src/basket.ts"] != "A" || paths["src/cart.ts"] != "D" {
		t.Fatalf("a rename came back as %+v, want the new path added and the old one deleted",
			renamed[0].PatchSet)
	}
}

func TestGitCommitsRefusesWhatItCannotRead(t *testing.T) {
	if _, err := gitCommits(context.Background(), t.TempDir(), "", "HEAD", 0); err == nil {
		t.Fatal("a directory that is not a repository came back as an empty commit set")
	}
	// A revision that starts with a dash would otherwise reach git as an
	// option. It never gets that far.
	if _, err := gitCommits(context.Background(), gitRepo(t), "--output=/tmp/x", "HEAD", 0); err == nil {
		t.Fatal("a revision that is an option was accepted")
	}
}

func runGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	command := exec.CommandContext(t.Context(), "git", append([]string{"-C", repo}, args...)...)
	command.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Antonio Vila", "GIT_AUTHOR_EMAIL=antonio@example.com",
		"GIT_COMMITTER_NAME=Antonio Vila", "GIT_COMMITTER_EMAIL=antonio@example.com")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func gitOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	command := exec.CommandContext(t.Context(), "git", append([]string{"-C", repo}, args...)...)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(output)
}

// ingestWithFrames pushes one event whose stacktrace names the given files,
// newest frame last, exactly as the protocol orders them.
func ingestWithFrames(t *testing.T, h *harness, release string, files ...string) {
	t.Helper()
	frames := make([]string, 0, len(files))
	for _, file := range files {
		frames = append(frames, fmt.Sprintf(
			`{"filename":%q,"function":"total","lineno":42,"in_app":true,`+
				`"raw":{"filename":"~/bundle.min.js","lineno":1,"colno":3120}}`, file))
	}
	event := fmt.Sprintf(`{"event_id":"2f3f7a1f16f34bd0a2b90e0f5f5a3c21","timestamp":%q,`+
		`"platform":"javascript","level":"error","release":%q,"environment":"production",`+
		`"exception":{"values":[{"type":"TypeError","value":"total is not a function",`+
		`"stacktrace":{"frames":[%s]}}]}}`,
		time.Now().UTC().Format(time.RFC3339), release, strings.Join(frames, ","))

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

func TestSuspectCommitsEndToEndThroughTheCLI(t *testing.T) {
	// The whole of ADR 019 driven the way a pipeline would: a real checkout,
	// `releases commits -repo`, and then the question.
	h := releaseHarness(t)
	repo := gitRepo(t)
	ingestWithFrames(t, h, "app@1.4.0", "src/main.ts", "src/checkout.ts")

	head := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD"))
	base := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD~2"))
	sent := h.mustRun("releases", "commits", "-project", "1", "-version", "app@1.4.0",
		"-repo", repo, "-from", base, "-to", head)
	if !strings.Contains(sent, "2 commits") {
		t.Fatalf("sent %q, want the two commits in the range", sent)
	}

	text := h.mustRun("issues", "suspects", "-project", "1", "-issue", "1")
	for _, want := range []string{"app@1.4.0", "rewrite the totals", "src/checkout.ts", "frame #1"} {
		if !strings.Contains(text, want) {
			t.Fatalf("suspects said %q, want it to mention %q", text, want)
		}
	}

	var report suspectsPayload
	if err := json.Unmarshal([]byte(h.mustRun("issues", "suspects",
		"-project", "1", "-issue", "1", "--json")), &report); err != nil {
		t.Fatalf("the JSON form is not JSON: %v", err)
	}
	if len(report.Suspects) != 1 {
		t.Fatalf("got %d suspects, want the one that touched the failing frame", len(report.Suspects))
	}
	if !report.Symbolicated {
		t.Error("the report says nothing was symbolicated")
	}
	if report.Suspects[0].Reasons[0].Path != "src/checkout.ts" {
		t.Errorf("the reason names %q", report.Suspects[0].Reasons[0].Path)
	}
}

func TestSuspectCommitsExplainsACommitSetWithNoPaths(t *testing.T) {
	// The ADR's fallback, and the reason `-repo` exists: a commit set sent
	// without paths lists its commits and says what to change.
	h := releaseHarness(t)
	ingestWithFrames(t, h, "app@1.4.0", "src/checkout.ts")

	file := filepath.Join(t.TempDir(), "commits.json")
	if err := os.WriteFile(file,
		[]byte(`[{"id":"0123456789abcdef0123456789abcdef01234567","message":"no paths"}]`),
		0o600); err != nil {
		t.Fatalf("writing the commit set: %v", err)
	}
	h.mustRun("releases", "commits", "-project", "1", "-version", "app@1.4.0", "-file", file)

	text := h.mustRun("issues", "suspects", "-project", "1", "-issue", "1")
	if !strings.Contains(text, "01234567") {
		t.Errorf("the candidates were not listed: %q", text)
	}
	if !strings.Contains(text, "without the files they changed") {
		t.Errorf("no warning naming what to change: %q", text)
	}
}

func TestSuspectCommitsUsageErrors(t *testing.T) {
	h := releaseHarness(t)

	if code, _, _ := h.run("issues", "suspects"); code != ExitUsage {
		t.Errorf("no flags exited %d, want %d", code, ExitUsage)
	}
	if code, _, _ := h.run("issues", "suspects", "-project", "1"); code != ExitUsage {
		t.Errorf("no issue exited %d, want %d", code, ExitUsage)
	}
	// An empty range is almost always a wrong one, and sending it would
	// replace a good commit set with nothing.
	repo := gitRepo(t)
	head := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD"))
	code, _, stderr := h.run("releases", "commits", "-project", "1", "-version", "app@1.4.0",
		"-repo", repo, "-from", head, "-to", head)
	if code == ExitOK {
		t.Error("an empty range was sent as a commit set")
	}
	if !strings.Contains(stderr, "nothing was sent") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestParseNameStatusReadsEveryStatusGitEmits(t *testing.T) {
	// Driven directly rather than through a repository, because producing a
	// copy detection or a type change on demand takes more setup than the
	// parser has code.
	cases := []struct {
		name string
		line []string
		want []commitFilePayload
	}{
		{"a copy", []string{"C75", "src/a.ts", "src/b.ts"},
			[]commitFilePayload{{Path: "src/b.ts", Type: "A"}}},
		{"a type change", []string{"T", "src/link.ts"},
			[]commitFilePayload{{Path: "src/link.ts", Type: "M"}}},
		{"a rename with no destination", []string{"R100", "src/a.ts"}, nil},
		{"a copy with no destination", []string{"C75", "src/a.ts"}, nil},
		{"an empty status", []string{"", "src/a.ts"}, nil},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := nameStatusEntry(test.line)
			if len(got) != len(test.want) {
				t.Fatalf("got %+v, want %+v", got, test.want)
			}
			for index := range got {
				if got[index] != test.want[index] {
					t.Fatalf("entry %d = %+v, want %+v", index, got[index], test.want[index])
				}
			}
		})
	}
}

func TestParseGitLogIgnoresWhatItCannotRead(t *testing.T) {
	// A truncated record — the output of a `git log` cut off by a killed
	// process — must produce nothing rather than a commit with a sha and no
	// identity, which would then be sent and stored.
	if commits := parseGitLog("\x1eabc\x1fAntonio"); len(commits) != 0 {
		t.Fatalf("a truncated record parsed as %+v", commits)
	}
	if commits := parseGitLog(""); len(commits) != 0 {
		t.Fatalf("empty output parsed as %+v", commits)
	}
	// A record whose sha is missing is the same case one field along.
	if commits := parseGitLog("\x1e\x1fa\x1fb\x1f\x1fmessage\x1f"); len(commits) != 0 {
		t.Fatalf("a record with no sha parsed as %+v", commits)
	}
}
