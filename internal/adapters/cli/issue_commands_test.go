package cli

import (
	"strings"
	"testing"
)

// The usage line of a triage command names the command the user typed.
//
// It used to name the *status* the API is told — "Usage of issues resolved:"
// for `issues resolve`, "Usage of issues unresolved:" for `issues reopen`.
// Neither of those is a command, and the message appears in exactly one
// situation: somebody already got the invocation wrong and is reading the help
// to find out how. Sending them to a verb that does not exist is worse than
// saying nothing, and it is the kind of mistake nothing else in this
// repository would ever notice — no gate reads help text.
//
// The documentation gate (scripts/docs.sh) is what made this visible: it runs
// the commands the documents claim work, and writing those documents meant
// reading the help of every one of them.
func TestTriageUsageNamesTheCommand(t *testing.T) {
	h := newHarness(t)
	h.setUp()

	for _, verb := range []string{"resolve", "ignore", "reopen"} {
		t.Run(verb, func(t *testing.T) {
			// -h is a parse error for flag.ContinueOnError, so this is the
			// usage path and the exit code is the usage one.
			code, _, stderr := h.run("issues", verb, "-h")
			if code != ExitUsage {
				t.Fatalf("exit code %d, want %d", code, ExitUsage)
			}
			want := "Usage of issues " + verb + ":"
			if !strings.Contains(stderr, want) {
				t.Fatalf("usage line does not name the command\n got: %s\nwant it to contain: %q",
					firstLine(stderr), want)
			}
		})
	}
}

// Only `resolve` declares -next-release. On the other two the flag's only
// possible outcome would be an error, and a flag that can only fail is a
// worse experience than not having it.
func TestNextReleaseOnlyOnResolve(t *testing.T) {
	h := newHarness(t)
	h.setUp()

	if _, _, stderr := h.run("issues", "resolve", "-h"); !strings.Contains(stderr, "-next-release") {
		t.Fatalf("resolve should offer -next-release, got:\n%s", stderr)
	}
	for _, verb := range []string{"ignore", "reopen"} {
		if _, _, stderr := h.run("issues", verb, "-h"); strings.Contains(stderr, "-next-release") {
			t.Fatalf("%s should not offer -next-release, got:\n%s", verb, stderr)
		}
	}
}
