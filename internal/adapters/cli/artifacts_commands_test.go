package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/antoniojosev/trapline/internal/artifactbundle"
	"github.com/antoniojosev/trapline/internal/domain"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("making %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func TestABuildDirectoryBecomesABundle(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "assets/bundle.min.js",
		"console.log(1)\n//# sourceMappingURL=bundle.min.js.map\n//# debugId=fc31aec3-520c-531b-836e-0d5e872e8a18")
	writeFile(t, dir, "assets/bundle.min.js.map", `{"version":3,"mappings":""}`)
	// Not a script and not a map: it must not end up in the archive.
	writeFile(t, dir, "assets/style.css", "body{}")

	bundle, err := collectBundle(dir, "~/", "app@1.0.0", "prod")
	if err != nil {
		t.Fatalf("collectBundle: %v", err)
	}
	if len(bundle.Files) != 2 {
		t.Fatalf("%d files, want the script and its map: %+v", len(bundle.Files), bundle.Files)
	}
	if bundle.Release != "app@1.0.0" || bundle.Dist != "prod" {
		t.Errorf("release/dist = %q/%q", bundle.Release, bundle.Dist)
	}

	byKind := map[artifactbundle.Kind]artifactbundle.File{}
	for _, file := range bundle.Files {
		byKind[file.Kind] = file
	}
	script := byKind[artifactbundle.KindMinifiedSource]
	sourceMap := byKind[artifactbundle.KindSourceMap]

	if script.DebugID != "fc31aec3-520c-531b-836e-0d5e872e8a18" {
		t.Errorf("the script's injected debug id was not read: %q", script.DebugID)
	}
	// The map has no id of its own in this build, and it has to inherit the
	// script's: the manifest gives both files one id, and that is what makes
	// one lookup return both (from the recording).
	if sourceMap.DebugID != script.DebugID {
		t.Errorf("the map got %q and the script %q", sourceMap.DebugID, script.DebugID)
	}
	if script.SourceMap != "bundle.min.js.map" {
		t.Errorf("the sourcemap header is %q; it names the map by file name", script.SourceMap)
	}
	if script.URL != "~/assets/bundle.min.js" {
		t.Errorf("url = %q, want the prefix plus the path inside the directory", script.URL)
	}
}

// A build where only the map carries the id, which is what some bundler
// plugins produce.
func TestTheDebugIDIsTakenFromWhicheverFileHasIt(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bundle.min.js", "console.log(1)")
	writeFile(t, dir, "bundle.min.js.map", `{"version":3,"debug_id":"aaaaaaaa-0000-5000-8000-000000000000"}`)

	bundle, err := collectBundle(dir, "~/", "", "")
	if err != nil {
		t.Fatalf("collectBundle: %v", err)
	}
	for _, file := range bundle.Files {
		if file.DebugID != "aaaaaaaa-0000-5000-8000-000000000000" {
			t.Fatalf("%s got %q", file.Path, file.DebugID)
		}
	}
	if !anyDebugID(bundle) {
		t.Fatal("anyDebugID says there is none")
	}
}

func TestPairingAScriptWithItsMap(t *testing.T) {
	dir := t.TempDir()
	// The conventional name, with no comment at all: a build that strips the
	// comment still ships the map beside it.
	writeFile(t, dir, "a.js", "console.log(1)")
	writeFile(t, dir, "a.js.map", `{"version":3}`)
	// An inline map: there is nothing separate to upload, so no header.
	writeFile(t, dir, "b.js", "console.log(2)\n//# sourceMappingURL=data:application/json;base64,e30=")
	// A comment naming a map that is not there.
	writeFile(t, dir, "c.js", "console.log(3)\n//# sourceMappingURL=missing.js.map")

	bundle, err := collectBundle(dir, "~/", "app@1.0.0", "")
	if err != nil {
		t.Fatalf("collectBundle: %v", err)
	}
	headers := map[string]string{}
	for _, file := range bundle.Files {
		if file.Kind == artifactbundle.KindMinifiedSource {
			headers[filepath.Base(file.URL)] = file.SourceMap
		}
	}
	if headers["a.js"] != "a.js.map" {
		t.Errorf("a.js paired with %q", headers["a.js"])
	}
	if headers["b.js"] != "" {
		t.Errorf("an inline map produced a header: %q", headers["b.js"])
	}
	if headers["c.js"] != "" {
		t.Errorf("a map that is not in the directory produced a header: %q", headers["c.js"])
	}
}

func TestUploadRefusesWhatNothingCouldEverFind(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bundle.min.js", "console.log(1)")

	stdout, stderr := &strings.Builder{}, &strings.Builder{}
	code := Run(t.Context(), []string{"artifacts", "upload", "-project", "1", dir},
		func(string) string { return "" }, stdout, stderr)
	if code == ExitOK {
		t.Fatal("an upload with neither a debug id nor a release was accepted")
	}
	// The message has to say what to do, because the alternative outcome is
	// an upload that reports success and resolves nothing.
	if !strings.Contains(stderr.String(), "-release") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestArtifactsUsageErrors(t *testing.T) {
	cases := map[string][]string{
		"no subcommand":               {"artifacts"},
		"unknown subcommand":          {"artifacts", "explode"},
		"upload with no project":      {"artifacts", "upload", t.TempDir()},
		"upload with no directory":    {"artifacts", "upload", "-project", "1"},
		"upload with two directories": {"artifacts", "upload", "-project", "1", ".", ".."},
		"list with no project":        {"artifacts", "list"},
		"delete with no id":           {"artifacts", "delete", "-project", "1"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			stdout, stderr := &strings.Builder{}, &strings.Builder{}
			if code := Run(t.Context(), args, func(string) string { return "" },
				stdout, stderr); code != ExitUsage {
				t.Fatalf("exit = %d, want %d (stderr: %s)", code, ExitUsage, stderr.String())
			}
		})
	}
}

func TestCollectingFromSomethingThatIsNotADirectory(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.js", "x")

	if _, err := collectBundle(filepath.Join(dir, "a.js"), "~/", "app@1.0.0", ""); err == nil {
		t.Fatal("a file was accepted as a directory to upload")
	}
	if _, err := collectBundle(filepath.Join(dir, "nowhere"), "~/", "app@1.0.0", ""); err == nil {
		t.Fatal("a directory that is not there was accepted")
	}
}

func TestTheListingIsReadableAtAGlance(t *testing.T) {
	if got := shortDebugID(""); got != "-" {
		t.Errorf("a missing debug id renders as %q", got)
	}
	if got := shortDebugID("fc31aec3-520c-531b-836e-0d5e872e8a18"); got != "fc31aec3" {
		t.Errorf("shortDebugID = %q", got)
	}
	if got := shortDebugID("short"); got != "short" {
		t.Errorf("shortDebugID = %q", got)
	}
	if got := megabytes(512); got != "512 B" {
		t.Errorf("megabytes(512) = %q", got)
	}
	if got := megabytes(3 * 1024 * 1024); got != "3.0 MB" {
		t.Errorf("megabytes = %q", got)
	}
}

// TestArtifactsEndToEndThroughTheAPI drives the three commands against a real
// server with a real database.
//
// It is the claim of ADR 006 in its narrowest form: uploading source maps must
// be possible with the binary somebody already installed, and what it stores
// has to be visible to every other client of the same API.
func TestArtifactsEndToEndThroughTheAPI(t *testing.T) {
	harness := newHarness(t)
	harness.setUp()

	code, stdout, stderr := harness.run("projects", "create", "-name", "venekambio", "--json")
	if code != ExitOK {
		t.Fatalf("creating the project: %s%s", stdout, stderr)
	}

	dir := t.TempDir()
	writeFile(t, dir, "bundle.min.js",
		"console.log(1)\n//# sourceMappingURL=bundle.min.js.map\n//# debugId=fc31aec3-520c-531b-836e-0d5e872e8a18")
	writeFile(t, dir, "bundle.min.js.map", `{"version":3,"mappings":"","debug_id":"fc31aec3-520c-531b-836e-0d5e872e8a18"}`)

	code, stdout, stderr = harness.run("artifacts", "upload", "-project", "1", dir)
	if code != ExitOK {
		t.Fatalf("upload: %s%s", stdout, stderr)
	}
	if !strings.Contains(stdout, "~/bundle.min.js") || !strings.Contains(stdout, "source_map") {
		t.Fatalf("upload printed %q", stdout)
	}

	code, stdout, stderr = harness.run("artifacts", "list", "-project", "1", "--json")
	if code != ExitOK {
		t.Fatalf("list: %s%s", stdout, stderr)
	}
	var listing artifactListPayload
	if err := json.Unmarshal([]byte(stdout), &listing); err != nil {
		t.Fatalf("decoding the listing: %v (%s)", err, stdout)
	}
	if len(listing.Artifacts) != 2 {
		t.Fatalf("the server holds %d artefacts", len(listing.Artifacts))
	}
	if listing.Budget != int64(domain.DefaultArtifactsMaxMB)*1024*1024 {
		t.Errorf("budget = %d", listing.Budget)
	}

	// Narrowed by debug id, which is the lookup the whole modern path exists
	// for: one build, both its files.
	code, stdout, _ = harness.run("artifacts", "list", "-project", "1",
		"-debug-id", "fc31aec3-520c-531b-836e-0d5e872e8a18", "--json")
	if code != ExitOK {
		t.Fatalf("list by debug id: %s", stdout)
	}
	var narrowed artifactListPayload
	if err := json.Unmarshal([]byte(stdout), &narrowed); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(narrowed.Artifacts) != 2 {
		t.Fatalf("the debug id matched %d files, want the script and its map", len(narrowed.Artifacts))
	}

	code, stdout, stderr = harness.run("artifacts", "delete", "-project", "1",
		"-id", strconv.FormatInt(listing.Artifacts[0].ID, 10))
	if code != ExitOK {
		t.Fatalf("delete: %s%s", stdout, stderr)
	}
	code, stdout, _ = harness.run("artifacts", "delete", "-project", "1",
		"-id", strconv.FormatInt(listing.Artifacts[0].ID, 10))
	if code != ExitError {
		t.Fatalf("deleting twice exited %d (%s)", code, stdout)
	}

	// And a release-addressed upload, which is the path for a build nobody
	// injected debug ids into.
	plain := t.TempDir()
	writeFile(t, plain, "legacy.min.js", "console.log('old')")
	if code, stdout, stderr = harness.run("artifacts", "upload", "-project", "1",
		"-release", "app@1.0.0", plain); code != ExitOK {
		t.Fatalf("release upload: %s%s", stdout, stderr)
	}
	code, stdout, _ = harness.run("artifacts", "list", "-project", "1", "-release", "app@1.0.0", "--json")
	if code != ExitOK {
		t.Fatalf("list by release: %s", stdout)
	}
	if !strings.Contains(stdout, "~/legacy.min.js") {
		t.Fatalf("the release's artefacts are %s", stdout)
	}
}
