package artifactbundle_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/antoniojosev/trapline/internal/artifactbundle"
)

// recordedManifest is the manifest sentry-cli 2.57.0 actually put inside the
// bundle, read out of the committed recording rather than retyped.
//
// Retyping it would make this test an assertion about what somebody remembered
// the protocol to be, which is the exact failure ADR 013 exists to prevent.
const recordedFixture = "../../compat/sentry-cli/fixtures/2.57.0/" +
	"sourcemaps-debugid/003-post-api-0-organizations-acme-artifactbundle-assemble.json"

func TestTheRecordedManifestIsReadBack(t *testing.T) {
	manifest, files := recordedBundle(t)

	archive := zipWith(t, manifest, files)
	bundle, err := artifactbundle.Read(archive, artifactbundle.DefaultLimits)
	if err != nil {
		t.Fatalf("reading the recorded bundle: %v", err)
	}

	if bundle.DebugID != "9676ae7d-7fec-50bc-a1b9-085910f75007" {
		t.Errorf("the bundle's own debug id is %q", bundle.DebugID)
	}
	if bundle.Project != "venekambio" {
		t.Errorf("project is %q", bundle.Project)
	}
	if len(bundle.Files) != 2 {
		t.Fatalf("read %d files, want 2", len(bundle.Files))
	}

	script, sourceMap := bundle.Files[0], bundle.Files[1]
	if script.Kind != artifactbundle.KindMinifiedSource {
		t.Errorf("the script's kind is %q, and the manifest's vocabulary is "+
			"minified_source — not the `source` ADR 018 assumed", script.Kind)
	}
	if sourceMap.Kind != artifactbundle.KindSourceMap {
		t.Errorf("the map's kind is %q, want source_map", sourceMap.Kind)
	}
	if script.URL != "~/bundle.min.js" {
		t.Errorf("the script's url is %q", script.URL)
	}
	// The finding that costs a lookup if it is missed: both entries carry the
	// same debug id, so resolving by debug id returns two rows and the caller
	// has to pick the map.
	if script.DebugID != sourceMap.DebugID || script.DebugID == "" {
		t.Errorf("script %q and map %q do not share one debug id", script.DebugID, sourceMap.DebugID)
	}
	if script.DebugID == bundle.DebugID {
		t.Error("the bundle's debug id and its files' debug id are different values; " +
			"reading one as the other is how a lookup finds nothing")
	}
	if script.SourceMap != "bundle.min.js.map" {
		t.Errorf("the script's sourcemap header is %q, and it names a file name, not a url",
			script.SourceMap)
	}

	found := bundle.SourceMapFor(&bundle.Files[0])
	if found == nil || found.Path != sourceMap.Path {
		t.Fatal("the sourcemap header did not lead from the script to its map")
	}
	if !bytes.Contains(found.Content, []byte("checkout.js")) {
		t.Error("the map that came back is not the committed one")
	}
}

func TestARoundTripKeepsEveryField(t *testing.T) {
	original := &artifactbundle.Bundle{
		DebugID: "9676ae7d-7fec-50bc-a1b9-085910f75007",
		Org:     "acme",
		Project: "venekambio",
		Release: "app@1.0.0",
		Dist:    "prod",
		Files: []artifactbundle.File{
			{
				Path:      artifactbundle.EntryPath("bundle.min.js"),
				Kind:      artifactbundle.KindMinifiedSource,
				URL:       artifactbundle.ArtifactURL("bundle.min.js"),
				DebugID:   "fc31aec3-520c-531b-836e-0d5e872e8a18",
				SourceMap: "bundle.min.js.map",
				Content:   []byte("console.log(1)"),
			},
			{
				Path:    artifactbundle.EntryPath("bundle.min.js.map"),
				Kind:    artifactbundle.KindSourceMap,
				URL:     artifactbundle.ArtifactURL("bundle.min.js.map"),
				DebugID: "fc31aec3-520c-531b-836e-0d5e872e8a18",
				Content: []byte(`{"version":3}`),
			},
		},
	}

	encoded, err := artifactbundle.Write(original)
	if err != nil {
		t.Fatalf("writing: %v", err)
	}
	read, err := artifactbundle.Read(encoded, artifactbundle.DefaultLimits)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}

	if read.Release != "app@1.0.0" || read.Dist != "prod" || read.Org != "acme" {
		t.Errorf("the release, dist or org did not survive: %+v", read)
	}
	for index := range original.Files {
		want, got := original.Files[index], read.Files[index]
		if want.Path != got.Path || want.Kind != got.Kind || want.URL != got.URL ||
			want.DebugID != got.DebugID || want.SourceMap != got.SourceMap ||
			!bytes.Equal(want.Content, got.Content) {
			t.Errorf("file %d changed:\n want %+v\n got  %+v", index, want, got)
		}
	}
}

// A bundle built twice from the same input has to be the same bytes, or a
// pipeline that changed nothing re-uploads everything and orphans the
// artefacts an event already points at.
func TestWritingIsDeterministic(t *testing.T) {
	build := func() []byte {
		encoded, err := artifactbundle.Write(&artifactbundle.Bundle{
			Project: "venekambio",
			Files: []artifactbundle.File{
				{Path: "files/_/_/b.js", Kind: artifactbundle.KindMinifiedSource, Content: []byte("b")},
				{Path: "files/_/_/a.js", Kind: artifactbundle.KindMinifiedSource, Content: []byte("a")},
			},
		})
		if err != nil {
			t.Fatalf("writing: %v", err)
		}
		return encoded
	}
	if !bytes.Equal(build(), build()) {
		t.Fatal("two writes of one bundle produced different bytes")
	}
}

func TestWhatIsNotABundle(t *testing.T) {
	cases := map[string][]byte{
		"empty":       {},
		"not a zip":   []byte("PK not really"),
		"no manifest": zipOf(t, map[string][]byte{"files/_/_/a.js": []byte("a")}),
		"manifest is not json": zipOf(t, map[string][]byte{
			artifactbundle.ManifestName: []byte("{"),
		}),
		"manifest describes nothing": zipOf(t, map[string][]byte{
			artifactbundle.ManifestName: []byte(`{"files":{}}`),
		}),
		"manifest names a file that is not there": zipOf(t, map[string][]byte{
			artifactbundle.ManifestName: []byte(
				`{"files":{"files/_/_/gone.js":{"type":"minified_source","url":"~/gone.js"}}}`),
		}),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := artifactbundle.Read(data, artifactbundle.DefaultLimits); !errors.Is(err, artifactbundle.ErrNotABundle) {
				t.Fatalf("error is %v, want one that wraps ErrNotABundle", err)
			}
		})
	}
}

// The archive is chosen by whoever holds an upload token, and a ZIP is a
// format with a compression ratio: a few kilobytes can name gigabytes. The
// limit has to be enforced on what comes out, not on what the entry claims.
func TestAnEntryLargerThanTheLimitIsRefused(t *testing.T) {
	big := bytes.Repeat([]byte("a"), 4096)
	archive := zipOf(t, map[string][]byte{
		artifactbundle.ManifestName: []byte(
			`{"files":{"files/_/_/big.js":{"type":"minified_source","url":"~/big.js"}}}`),
		"files/_/_/big.js": big,
	})
	limits := artifactbundle.DefaultLimits
	limits.MaxFileBytes = 1024

	_, err := artifactbundle.Read(archive, limits)
	if !errors.Is(err, artifactbundle.ErrNotABundle) || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("error is %v; a bomb has to be refused by what it expands to", err)
	}
}

func TestAnUnknownTypeIsCarriedRatherThanRefused(t *testing.T) {
	archive := zipOf(t, map[string][]byte{
		artifactbundle.ManifestName: []byte(
			`{"files":{"files/_/_/x.wasm":{"type":"indexed_ram_bundle","url":"~/x.wasm"}}}`),
		"files/_/_/x.wasm": []byte("x"),
	})
	bundle, err := artifactbundle.Read(archive, artifactbundle.DefaultLimits)
	if err != nil {
		t.Fatalf("a type this product does not store must not fail the read: %v", err)
	}
	if bundle.Files[0].Kind.Known() {
		t.Fatal("indexed_ram_bundle is not a kind this product stores")
	}
}

func TestTheDebugIDDerivationMatchesTheMeasuredOne(t *testing.T) {
	// Measured on the recorded bundle: sha1 fc31aec3520cb31b836e0d5e872e8a184de8fa3a
	// of the uninjected map gives debug id fc31aec3-520c-531b-836e-0d5e872e8a18.
	sum := []byte{
		0xfc, 0x31, 0xae, 0xc3, 0x52, 0x0c, 0xb3, 0x1b,
		0x83, 0x6e, 0x0d, 0x5e, 0x87, 0x2e, 0x8a, 0x18,
		0x4d, 0xe8, 0xfa, 0x3a,
	}
	if got := artifactbundle.DeriveDebugID(sum); got != "fc31aec3-520c-531b-836e-0d5e872e8a18" {
		t.Fatalf("derived %q", got)
	}
	if artifactbundle.DeriveDebugID([]byte{1, 2, 3}) != "" {
		t.Fatal("a sum too short to hold sixteen bytes is not a debug id")
	}
}

func TestTheDebugIDIsFoundInBothSpellings(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("..", "sourcemap", "testdata", "bundle.min.js"))
	if err != nil {
		t.Skipf("the committed bundle is not here: %v", err)
	}
	sourceMap, err := os.ReadFile(filepath.Join("..", "sourcemap", "testdata", "bundle.min.js.map"))
	if err != nil {
		t.Fatalf("reading the committed map: %v", err)
	}
	// The committed pair is the *uninjected* build the browser suite raises
	// errors from, so neither spelling is necessarily present; what has to
	// hold is that when one is, both readers agree.
	inScript := artifactbundle.DebugIDInScript(script)
	inMap := artifactbundle.DebugIDInSourceMap(sourceMap)
	if inScript != "" && inMap != "" && inScript != inMap {
		t.Fatalf("the script says %q and its map says %q", inScript, inMap)
	}

	injected := append(append([]byte{}, script...), []byte("\n//# debugId=fc31aec3-520c-531b-836e-0d5e872e8a18\n")...)
	if got := artifactbundle.DebugIDInScript(injected); got != "fc31aec3-520c-531b-836e-0d5e872e8a18" {
		t.Fatalf("the injected comment read back as %q", got)
	}
	if got := artifactbundle.DebugIDInSourceMap([]byte(`{"version":3,"debug_id":"abc"}`)); got != "abc" {
		t.Fatalf("the map's underscore spelling read back as %q", got)
	}
}

// recordedBundle returns the manifest sentry-cli wrote and the file contents
// it wrote it about, both from committed material.
func recordedBundle(t *testing.T) (manifest json.RawMessage, files map[string][]byte) {
	t.Helper()
	raw, err := os.ReadFile(recordedFixture)
	if err != nil {
		t.Fatalf("reading the recording: %v", err)
	}
	var recording struct {
		Assembled struct {
			Manifest json.RawMessage `json:"manifest"`
		} `json:"assembled"`
	}
	if err := json.Unmarshal(raw, &recording); err != nil {
		t.Fatalf("reading the recording: %v", err)
	}
	if len(recording.Assembled.Manifest) == 0 {
		t.Fatal("the recording carries no manifest")
	}

	files = map[string][]byte{}
	for _, name := range []string{"bundle.min.js", "bundle.min.js.map"} {
		content, err := os.ReadFile(filepath.Join("..", "sourcemap", "testdata", name))
		if err != nil {
			t.Fatalf("reading the committed %s: %v", name, err)
		}
		files["files/_/_/"+name] = content
	}
	return recording.Assembled.Manifest, files
}

func zipWith(t *testing.T, manifest json.RawMessage, files map[string][]byte) []byte {
	t.Helper()
	entries := map[string][]byte{artifactbundle.ManifestName: manifest}
	for name, content := range files {
		entries[name] = content
	}
	return zipOf(t, entries)
}

func zipOf(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	archive := zip.NewWriter(&out)
	for name, content := range entries {
		entry, err := archive.Create(name)
		if err != nil {
			t.Fatalf("building the archive: %v", err)
		}
		if _, err := entry.Write(content); err != nil {
			t.Fatalf("building the archive: %v", err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatalf("building the archive: %v", err)
	}
	return out.Bytes()
}

func TestTheNamesABundleEntryMayNotHave(t *testing.T) {
	// Nothing here writes an entry to disk, so this is not a traversal fix.
	// It is a refusal to store a name under which nothing legitimate is ever
	// uploaded, taken before some later caller does write one out.
	for _, name := range []string{"../escaped.js", "/absolute.js"} {
		archive := zipOf(t, map[string][]byte{
			artifactbundle.ManifestName: []byte(
				`{"files":{"` + name + `":{"type":"minified_source","url":"~/x.js"}}}`),
			name: []byte("x"),
		})
		if _, err := artifactbundle.Read(archive, artifactbundle.DefaultLimits); !errors.Is(err, artifactbundle.ErrNotABundle) {
			t.Errorf("%q was accepted as an entry name (%v)", name, err)
		}
	}
}

func TestTheLimitsThatBoundAnArchive(t *testing.T) {
	t.Run("too many files", func(t *testing.T) {
		entries := map[string][]byte{}
		files := map[string]string{}
		for index := range 5 {
			name := "files/_/_/f" + strconv.Itoa(index) + ".js"
			entries[name] = []byte("x")
			files[name] = `{"type":"minified_source","url":"~/f` + strconv.Itoa(index) + `.js"}`
		}
		var manifest strings.Builder
		manifest.WriteString(`{"files":{`)
		first := true
		for name, described := range files {
			if !first {
				manifest.WriteString(",")
			}
			first = false
			manifest.WriteString(`"` + name + `":` + described)
		}
		manifest.WriteString(`}}`)
		entries[artifactbundle.ManifestName] = []byte(manifest.String())

		limits := artifactbundle.DefaultLimits
		limits.MaxFiles = 2
		if _, err := artifactbundle.Read(zipOf(t, entries), limits); !errors.Is(err, artifactbundle.ErrNotABundle) {
			t.Fatalf("a manifest with more files than the limit was accepted: %v", err)
		}
	})

	t.Run("the whole archive expands past the total", func(t *testing.T) {
		entries := map[string][]byte{
			artifactbundle.ManifestName: []byte(
				`{"files":{"files/_/_/a.js":{"type":"minified_source","url":"~/a.js"},` +
					`"files/_/_/b.js":{"type":"minified_source","url":"~/b.js"}}}`),
			"files/_/_/a.js": bytes.Repeat([]byte("a"), 600),
			"files/_/_/b.js": bytes.Repeat([]byte("b"), 600),
		}
		limits := artifactbundle.DefaultLimits
		limits.MaxTotalBytes = 1000
		if _, err := artifactbundle.Read(zipOf(t, entries), limits); !errors.Is(err, artifactbundle.ErrNotABundle) {
			t.Fatalf("an archive past the total was accepted: %v", err)
		}
	})

	t.Run("the manifest itself is bounded", func(t *testing.T) {
		limits := artifactbundle.DefaultLimits
		limits.MaxFileBytes = 4
		archive := zipOf(t, map[string][]byte{
			artifactbundle.ManifestName: []byte(`{"files":{"a":{"type":"source_map"}}}`),
		})
		if _, err := artifactbundle.Read(archive, limits); !errors.Is(err, artifactbundle.ErrNotABundle) {
			t.Fatalf("an oversized manifest was read: %v", err)
		}
	})
}

func TestFindingAMapThatIsNotThere(t *testing.T) {
	bundle := &artifactbundle.Bundle{Files: []artifactbundle.File{
		{Path: "files/_/_/a.js", Kind: artifactbundle.KindMinifiedSource, SourceMap: "a.js.map"},
	}}
	if bundle.SourceMapFor(&bundle.Files[0]) != nil {
		t.Error("a bundle uploaded without its maps must not claim to carry one")
	}
	if bundle.SourceMapFor(nil) != nil {
		t.Error("no script, no map")
	}
	// A script with no sourcemap header at all: the join simply does not
	// exist, and guessing one would be inventing a pairing.
	bundle.Files[0].SourceMap = ""
	if bundle.SourceMapFor(&bundle.Files[0]) != nil {
		t.Error("a script that names no map was paired with one anyway")
	}
}

func TestReadingADebugIDOutOfSomethingThatIsNotOne(t *testing.T) {
	if got := artifactbundle.DebugIDInSourceMap([]byte("not json")); got != "" {
		t.Errorf("read %q out of a map that is not JSON", got)
	}
	if got := artifactbundle.DebugIDInScript([]byte("console.log(1)")); got != "" {
		t.Errorf("read %q out of a script with no comment", got)
	}
	// The comment at the very end of the file, with no trailing newline,
	// which is exactly how injection writes it.
	if got := artifactbundle.DebugIDInScript([]byte("x\n//# debugId=abc")); got != "abc" {
		t.Errorf("read %q from a comment with no trailing newline", got)
	}
}
