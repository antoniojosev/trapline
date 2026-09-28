// Package arch holds the architecture gate: a test that fails the build when
// a dependency crosses a boundary the design forbids.
//
// This lives as a plain Go test, with no third-party tooling, on purpose.
// golangci-lint enforces the same rules through depguard, but a contributor
// without that binary installed would silently skip them. `go test ./...`
// cannot be skipped, so the most important structural invariant of the repo
// is guarded by the one command everybody runs.
package arch

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/antoniojosev/trapline"

// boundary is a directory whose files may only import the standard library
// and packages living inside the boundary itself.
type boundary struct {
	dir string
	adr string
	why string
}

func TestBoundariesAreNotCrossed(t *testing.T) {
	boundaries := []boundary{
		{
			dir: "internal/domain",
			adr: "ADR 004",
			why: "the domain is pure: entities and rules, no infrastructure",
		},
		{
			dir: "internal/engine",
			adr: "ADR 004",
			why: "the engine knows nothing about the rest of the repo, which is what keeps its future extraction into a library cheap",
		},
		{
			dir: "internal/envelope",
			adr: "ADR 002",
			why: "the envelope parser stays a pure package so it can be fuzzed in isolation; it is the surface that reads attacker-chosen bytes from a public endpoint",
		},
		{
			dir: "internal/digest",
			adr: "ADR 035",
			why: "the weekly report is rendered from numbers somebody else gathered; the moment it can reach a store or a clock, its output stops being a pure function of its input and the golden fixtures stop meaning anything",
		},
		{
			dir: "internal/clientip",
			adr: "ADR 023",
			why: "deciding which address a request belongs to is a security boundary and stays a pure function of two strings; the moment it can reach a request, a config or a clock, it stops being exhaustively testable",
		},
		{
			dir: "internal/sourcemap",
			adr: "ADR 018",
			why: "the source map parser reads a file a user uploaded, produced by a bundler this product does not control; it stays standard-library-only so it can be fuzzed in isolation, exactly like the envelope parser",
		},
		{
			dir: "internal/artifactbundle",
			adr: "ADR 018",
			why: "the artifact bundle reader parses a compressed archive uploaded by whoever holds a token; it stays standard-library-only so it can be fuzzed in isolation, the way the envelope parser is",
		},
		{
			dir: "internal/ssrfguard",
			adr: "ADR 016",
			why: "deciding whether this server may connect to an address a user chose is a security boundary; it stays standard-library-only so the policy is one readable table and the dialer that enforces it cannot be handed a hostname by something that already checked a different one",
		},
	}

	root := repoRoot(t)

	for _, b := range boundaries {
		t.Run(b.dir, func(t *testing.T) {
			abs := filepath.Join(root, b.dir)
			if _, err := os.Stat(abs); err != nil {
				// A renamed or deleted boundary directory would turn this
				// gate into a no-op, which is worse than not having it.
				t.Fatalf("boundary directory %s is missing: %v\n"+
					"If it moved, update this test — do not let the gate go quiet.", b.dir, err)
			}

			selfPrefix := modulePath + "/" + b.dir
			checked := 0

			err := filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() || !strings.HasSuffix(path, ".go") {
					return nil
				}
				checked++

				file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
				if err != nil {
					return err
				}
				rel, _ := filepath.Rel(root, path)

				for _, spec := range file.Imports {
					imported, err := strconv.Unquote(spec.Path.Value)
					if err != nil {
						return err
					}
					if isStdlib(imported) || strings.HasPrefix(imported, selfPrefix) {
						continue
					}
					t.Errorf("%s imports %q, which crosses a boundary.\n  %s: %s",
						rel, imported, b.adr, b.why)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("walking %s: %v", b.dir, err)
			}
			if checked == 0 {
				t.Fatalf("no Go files found under %s — the gate is checking nothing", b.dir)
			}
		})
	}
}

// isStdlib reports whether an import path belongs to the standard library.
// Standard library paths never have a dot in their first segment, since a
// dot there means a domain name.
func isStdlib(importPath string) bool {
	first, _, _ := strings.Cut(importPath, "/")
	return !strings.Contains(first, ".")
}

// repoRoot walks up from the test's working directory until it finds go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from the test directory")
		}
		dir = parent
	}
}
