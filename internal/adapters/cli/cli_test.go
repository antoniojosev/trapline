package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func noEnv(string) string { return "" }

func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = Run(context.Background(), args, noEnv, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestVersion(t *testing.T) {
	code, stdout, _ := run(t, "version")
	if code != ExitOK {
		t.Errorf("exit code = %d, want %d", code, ExitOK)
	}
	if strings.TrimSpace(stdout) != Version {
		t.Errorf("stdout = %q, want %q", stdout, Version)
	}
}

func TestVersionJSON(t *testing.T) {
	code, stdout, _ := run(t, "version", "--json")
	if code != ExitOK {
		t.Errorf("exit code = %d, want %d", code, ExitOK)
	}

	var payload struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout is not valid JSON (%v): %q", err, stdout)
	}
	if payload.Version != Version {
		t.Errorf("version = %q, want %q", payload.Version, Version)
	}
	if !strings.HasSuffix(stdout, "\n") {
		t.Error("JSON output must end in a newline so it can be read line by line")
	}
}

func TestUsageErrorsGoToStderr(t *testing.T) {
	// An agent parses stdout. Diagnostics must never contaminate it.
	cases := map[string][]string{
		"no arguments":          {},
		"unknown command":       {"nope"},
		"missing subcommand":    {"projects"},
		"unknown subcommand":    {"projects", "teleport"},
		"missing required flag": {"projects", "create"},
		"missing backup target": {"backup"},
		"missing token name":    {"token", "create"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := run(t, args...)
			if code != ExitUsage {
				t.Errorf("exit code = %d, want %d", code, ExitUsage)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want it empty on a usage error", stdout)
			}
			if stderr == "" {
				t.Error("stderr is empty; the user gets no diagnostic")
			}
		})
	}
}

func TestHelpGoesToStdout(t *testing.T) {
	// Asking for help is success, and its output is what the user asked for.
	code, stdout, _ := run(t, "help")
	if code != ExitOK {
		t.Errorf("exit code = %d, want %d", code, ExitOK)
	}
	for _, expected := range []string{"Usage:", "serve", "backup", "token", "projects", "keys", "doctor", "--json"} {
		if !strings.Contains(stdout, expected) {
			t.Errorf("help output does not mention %q", expected)
		}
	}
}

func TestRemoteCommandsRefuseWithoutAToken(t *testing.T) {
	// Failing with an explanation of how to get a token beats failing with a
	// 401 from a server the user cannot see.
	cases := map[string][]string{
		"projects list": {"projects", "list"},
		"keys rotate":   {"keys", "rotate", "-project", "1"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := run(t, args...)
			if code != ExitError {
				t.Errorf("exit code = %d, want %d", code, ExitError)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want it empty", stdout)
			}
			if !strings.Contains(stderr, "TRAPLINE_TOKEN") {
				t.Errorf("stderr = %q, want it to say how to get a token", stderr)
			}
		})
	}
}
