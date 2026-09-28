package httpapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1" //nolint:gosec // the protocol's hash.
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antoniojosev/trapline/internal/artifactbundle"
)

// These tests exercise the upload surface the way sentry-cli does, and the
// assertions are the findings of the recorded sentry-cli traffic.
//
// The recording is what they are anchored to: the paths come out of the
// committed fixtures rather than being retyped here, and the parts are shaped
// the way the recorder described them — one field name for every chunk, the
// checksum in the filename, gzip on the body.

const sourcemapFixtureDir = fixtureRoot + "/2.57.0/sourcemaps-debugid"

// recordedPath reads a path out of a committed fixture, so a change in the
// protocol shows up here as a failing test rather than as a route nobody
// noticed had moved.
func recordedPath(t *testing.T, dir, file string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, file)) //nolint:gosec // a committed fixture path.
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	var entry recordedRequest
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatalf("decoding %s: %v", file, err)
	}
	return entry.Path
}

func chunkUploadPath(t *testing.T) string {
	t.Helper()
	return recordedPath(t, sourcemapFixtureDir, "001-get-api-0-organizations-acme-chunk-upload.json")
}

func assemblePath(t *testing.T) string {
	t.Helper()
	return recordedPath(t, sourcemapFixtureDir,
		"003-post-api-0-organizations-acme-artifactbundle-assemble.json")
}

// TestTheCapabilitiesDocumentIsWhatSteersTheClient is the single most
// consequential assertion in this file.
//
// Every field named here was found by a run that failed without it, and two of
// them decide which protocol the client speaks rather than describing the
// server (from the recording).
func TestTheCapabilitiesDocumentIsWhatSteersTheClient(t *testing.T) {
	stack := newCompatStack(t)

	response := stack.get(t, chunkUploadPath(t))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("capabilities answered %d", response.StatusCode)
	}

	var document map[string]any
	decodeInto(t, response, &document)

	// The six the client refuses to start without, naming the first one
	// missing.
	for _, field := range []string{
		"url", "chunkSize", "chunksPerRequest", "maxRequestSize", "hashAlgorithm", "concurrency",
	} {
		if _, present := document[field]; !present {
			t.Errorf("no %q: sentry-cli aborts with `missing field %s` and does nothing else", field, field)
		}
	}

	// An enum, not free text: "sha256" is a parse error rather than a
	// different hash.
	if document["hashAlgorithm"] != "sha1" {
		t.Errorf("hashAlgorithm = %v, and the only value the client parses is \"sha1\"", document["hashAlgorithm"])
	}

	// Absolute, and pointing at this installation's configured origin. The
	// client posts chunks to this field and not to the path it asked on, so a
	// relative or wrongly-hosted value sends an installation's source maps
	// somewhere else.
	url, _ := document["url"].(string)
	if !strings.HasPrefix(url, "https://errors.example.com/") {
		t.Errorf("url = %q; it has to be absolute and built from the configured origin", url)
	}

	accept := stringsOf(document["accept"])
	if !contains(accept, "artifact_bundles") {
		t.Error("artifact_bundles is not offered, so no debug-id upload can happen at all")
	}
	// The trap. `["artifact_bundles"]` alone makes sentry-cli fall back and
	// report "a release is required for this upload" — announcing only the
	// modern path is what switches the modern path off (from the recording).
	if !contains(accept, "release_files") {
		t.Error("release_files is missing from `accept`. It is not a compatibility gesture: " +
			"without it sentry-cli stops using debug ids entirely")
	}
	// And the one that must not be there. With it, the client calls assemble
	// first and, against a server that answers optimistically, uploads no
	// chunks at all while reporting success (from the recording).
	if contains(accept, "artifact_bundles_v2") {
		t.Error("artifact_bundles_v2 is advertised. It puts assemble before the upload, " +
			"which is one more way to lose a build's source maps and buys nothing here")
	}

	if compression := stringsOf(document["compression"]); contains(compression, "gzip") {
		// Announcing gzip renames the multipart field, so a server that
		// announces it must accept both names. TestBothChunkFieldNamesWork is
		// the other half of this assertion.
		t.Log("gzip announced: the parts will arrive as `file_gzip`")
	}
}

// TestAWholeUploadArrivesAndIsFindable walks the three requests of the modern
// flow and then asks the product's own API what it has.
func TestAWholeUploadArrivesAndIsFindable(t *testing.T) {
	stack := newCompatStack(t)
	bundle := testBundle(t, "", "")

	stack.uploadChunks(t, splitIntoChunks(bundle, 1024), true)
	result := stack.assemble(t, assemblePath(t), map[string]any{
		"checksum": sha1Of(bundle),
		"chunks":   checksumsOf(splitIntoChunks(bundle, 1024)),
		"projects": []string{"venekambio"},
	})
	if result.State != "ok" {
		t.Fatalf("state = %q, detail = %v", result.State, result.Detail)
	}

	var listing artifactListResponse
	decodeInto(t, tokenRequest(t, stack.baseURL, stack.token, http.MethodGet,
		api("/projects/1/artifacts"), nil), &listing)
	if len(listing.Artifacts) != 2 {
		t.Fatalf("the project holds %d artifacts, want the script and its map", len(listing.Artifacts))
	}

	byKind := map[string]artifactPayload{}
	for _, artifact := range listing.Artifacts {
		byKind[artifact.Kind] = artifact
	}
	script, hasScript := byKind["minified_source"]
	sourceMap, hasMap := byKind["source_map"]
	if !hasScript || !hasMap {
		t.Fatalf("kinds stored: %v; the manifest's vocabulary is minified_source/source_map", byKind)
	}
	if script.DebugID == "" || script.DebugID != sourceMap.DebugID {
		t.Errorf("script %q and map %q must share one debug id", script.DebugID, sourceMap.DebugID)
	}
	if script.SourceMapRef == "" {
		t.Error("the script's `sourcemap` header did not survive; without it there is no way " +
			"to get from a script to its map when there is no debug id")
	}
	if script.Name != "~/bundle.min.js" {
		t.Errorf("the script is stored as %q, want the ~/ form", script.Name)
	}
}

// TestAnAssemblyWithoutItsChunksIsNeverOK is the honesty rule.
//
// Answering `ok` here would make sentry-cli report a successful upload of
// nothing, and nobody would find out until somebody opened a minified stack
// trace weeks later (from the recording).
func TestAnAssemblyWithoutItsChunksIsNeverOK(t *testing.T) {
	stack := newCompatStack(t)
	bundle := testBundle(t, "", "")
	chunks := splitIntoChunks(bundle, 1024)

	// Nothing uploaded at all.
	result := stack.assemble(t, assemblePath(t), map[string]any{
		"checksum": sha1Of(bundle),
		"chunks":   checksumsOf(chunks),
		"projects": []string{"venekambio"},
	})
	if result.State == "ok" {
		t.Fatal("the server said ok while holding none of the chunks")
	}
	if result.State != "created" {
		t.Fatalf("state = %q, want created — which is what makes the client come back", result.State)
	}
	if len(result.MissingChunks) != len(chunks) {
		t.Errorf("missingChunks = %v, want all %d", result.MissingChunks, len(chunks))
	}

	// And with all but the last.
	stack.uploadChunks(t, chunks[:len(chunks)-1], true)
	result = stack.assemble(t, assemblePath(t), map[string]any{
		"checksum": sha1Of(bundle),
		"chunks":   checksumsOf(chunks),
		"projects": []string{"venekambio"},
	})
	if result.State != "created" || len(result.MissingChunks) != 1 {
		t.Fatalf("state = %q missing = %v, want created with the one chunk that is short",
			result.State, result.MissingChunks)
	}

	var listing artifactListResponse
	decodeInto(t, tokenRequest(t, stack.baseURL, stack.token, http.MethodGet,
		api("/projects/1/artifacts"), nil), &listing)
	if len(listing.Artifacts) != 0 {
		t.Fatalf("%d artifacts stored from an incomplete upload", len(listing.Artifacts))
	}
}

func TestWhatFailsAnAssemblyAndHowItIsReported(t *testing.T) {
	stack := newCompatStack(t)
	bundle := testBundle(t, "", "")
	chunks := splitIntoChunks(bundle, 1024)
	stack.uploadChunks(t, chunks, true)

	t.Run("a checksum that does not match", func(t *testing.T) {
		result := stack.assemble(t, assemblePath(t), map[string]any{
			"checksum": strings.Repeat("0", 40),
			"chunks":   checksumsOf(chunks),
			"projects": []string{"venekambio"},
		})
		if result.State != "error" {
			t.Fatalf("state = %q, want error", result.State)
		}
		if result.Detail == nil || *result.Detail == "" {
			t.Fatal("no detail: it is the only channel this protocol has for saying why, " +
				"and the tool prints it to the user")
		}
	})

	t.Run("bytes that are not an archive", func(t *testing.T) {
		junk := []byte(strings.Repeat("not a zip", 200))
		stack.uploadChunks(t, splitIntoChunks(junk, 1024), true)
		result := stack.assemble(t, assemblePath(t), map[string]any{
			"checksum": sha1Of(junk),
			"chunks":   checksumsOf(splitIntoChunks(junk, 1024)),
			"projects": []string{"venekambio"},
		})
		if result.State != "error" {
			t.Fatalf("state = %q, want error", result.State)
		}
		if result.Detail == nil || !strings.Contains(*result.Detail, "bad zip") {
			t.Errorf("detail = %v; the wording is the tool's own so a user searching for it "+
				"finds the same answers", result.Detail)
		}
	})

	t.Run("a project this server has never heard of", func(t *testing.T) {
		result := stack.assemble(t, assemblePath(t), map[string]any{
			"checksum": sha1Of(bundle),
			"chunks":   checksumsOf(chunks),
			"projects": []string{"somebody-elses-project"},
		})
		if result.State != "error" {
			t.Fatalf("state = %q, want error", result.State)
		}
	})
}

// TestTheChecksumIsOfTheInflatedBytes is the trap the recording found.
//
// A server that verified the sha1 of what arrived on the wire would reject
// every client that announces gzip — which is every client this server's own
// capabilities document tells to use it — while its own tests stayed green.
func TestTheChecksumIsOfTheInflatedBytes(t *testing.T) {
	stack := newCompatStack(t)
	content := []byte(strings.Repeat("payload", 100))

	compressed := gzipOf(t, content)
	wireChecksum := sha1Of(compressed)
	if wireChecksum == sha1Of(content) {
		t.Fatal("the compressed and uncompressed bytes hash the same, so this test proves nothing")
	}

	// Named by the hash of the *wire* bytes, which is what a server reading
	// the protocol carelessly would expect.
	response := stack.postParts(t, chunkUploadPath(t), []uploadPart{
		{field: "file_gzip", filename: wireChecksum, body: compressed},
	})
	if response.StatusCode < http.StatusBadRequest {
		t.Fatalf("a chunk named by the sha1 of its compressed form was accepted (%d); "+
			"the filename is the sha1 of what it decompresses to", response.StatusCode)
	}

	// And named correctly, it goes in.
	response = stack.postParts(t, chunkUploadPath(t), []uploadPart{
		{field: "file_gzip", filename: sha1Of(content), body: compressed},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("a correctly named gzip chunk answered %d", response.StatusCode)
	}
}

// TestBothChunkFieldNamesWork is the other half of announcing gzip.
//
// The field is `file_gzip` when the server announces compression and `file`
// when it does not. A server that only looked for one would turn away half its
// clients without an error anywhere (from the recording).
func TestBothChunkFieldNamesWork(t *testing.T) {
	stack := newCompatStack(t)
	content := []byte("a chunk of a bundle")

	plain := stack.postParts(t, chunkUploadPath(t), []uploadPart{
		{field: "file", filename: sha1Of(content), body: content},
	})
	if plain.StatusCode != http.StatusOK {
		t.Fatalf("an uncompressed `file` part answered %d", plain.StatusCode)
	}

	other := []byte("another chunk of a bundle")
	zipped := stack.postParts(t, chunkUploadPath(t), []uploadPart{
		{field: "file_gzip", filename: sha1Of(other), body: gzipOf(t, other)},
	})
	if zipped.StatusCode != http.StatusOK {
		t.Fatalf("a `file_gzip` part answered %d", zipped.StatusCode)
	}
}

// TestTheLegacyUploadPathStoresOneFile covers the only flow that never touches
// a chunk (from the recording).
func TestTheLegacyUploadPathStoresOneFile(t *testing.T) {
	stack := newCompatStack(t)
	path := recordedPath(t, fixtureRoot+"/2.57.0/legacy-release-files",
		"002-post-api-0-projects-acme-venekambio-releases-app-1-0-0-files.json")

	content := []byte("console.log('shipped')")
	response := stack.postParts(t, path, []uploadPart{
		{field: "file", filename: "bundle.min.js", body: content},
		{field: "name", body: []byte("~/bundle.min.js")},
	})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", response.StatusCode)
	}

	var stored map[string]any
	decodeInto(t, response, &stored)
	// `id` is not optional: the tool fails with "missing field `id`" against a
	// bare object (from the recording).
	if id, _ := stored["id"].(string); id == "" {
		t.Fatalf("no id in %v; the tool refuses a release file without one", stored)
	}
	if stored["name"] != "~/bundle.min.js" {
		t.Errorf("name = %v, want the url the `name` part carried", stored["name"])
	}

	var listing artifactListResponse
	decodeInto(t, tokenRequest(t, stack.baseURL, stack.token, http.MethodGet,
		api("/projects/1/artifacts"), nil), &listing)
	if len(listing.Artifacts) != 1 || listing.Artifacts[0].Name != "~/bundle.min.js" {
		t.Fatalf("the native API sees %+v", listing.Artifacts)
	}
	if listing.Artifacts[0].ReleaseID == nil {
		t.Error("a legacy upload with no release attached to it could never be found again")
	}
}

// TestTheDeduplicationQueryIsNotA404 covers a finding of the recording.
//
// It is emitted on every upload that names a release, so it is on the happy
// path: a 404 here would be answering 404 to something entirely normal.
func TestTheDeduplicationQueryIsNotA404(t *testing.T) {
	stack := newCompatStack(t)
	path := recordedPath(t, fixtureRoot+"/2.57.0/sourcemaps-release",
		"002-get-api-0-projects-acme-venekambio-releases-app-1-0-0-files.json")

	response := stack.get(t, path+"?checksum=deadbeef&checksum=cafebabe&cursor=")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}

	// And an array: the client reads lists as lists, and an object stops it
	// with "invalid type: map, expected a sequence" (ADR 013).
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
		t.Fatalf("body = %s, want a JSON array", body)
	}
}

// TestTheFallbackAssemblyEndpointWorks covers a finding of the recording: a client that was
// told the server has no artifact bundles assembles somewhere else, with a
// body that names no project at all.
func TestTheFallbackAssemblyEndpointWorks(t *testing.T) {
	stack := newCompatStack(t)
	path := recordedPath(t, fixtureRoot+"/2.57.0/release-bundle-fallback",
		"006-post-api-0-organizations-acme-releases-app-1-0-0-assemble.json")

	// The release has to exist for the version in the path to resolve to a
	// project, which is the same resolution a commit set uses (ADR 013 §6).
	if got := tokenRequest(t, stack.baseURL, stack.token, http.MethodPost,
		api("/projects/1/releases"), map[string]any{"version": "app@1.0.0"}).StatusCode; got/100 != 2 {
		t.Fatalf("creating the release answered %d", got)
	}

	bundle := testBundle(t, "app@1.0.0", "")
	chunks := splitIntoChunks(bundle, 1024)
	stack.uploadChunks(t, chunks, true)

	result := stack.assemble(t, path, map[string]any{
		"checksum": sha1Of(bundle),
		"chunks":   checksumsOf(chunks),
	})
	if result.State != "ok" {
		t.Fatalf("state = %q detail = %v", result.State, result.Detail)
	}
}

// TestAnUploadPastTheBudgetIsReportedInsideTheBody is a finding of the recording.
//
// A 413 on the assembly endpoint reaches the user as "unknown error"; the
// state machine is the only channel that carries a sentence.
func TestAnUploadPastTheBudgetIsReportedInsideTheBody(t *testing.T) {
	stack := newCompatStack(t)

	// Zero megabytes: a decision, and the smallest way to be over budget.
	if got := tokenRequest(t, stack.baseURL, stack.token, http.MethodPut,
		api("/projects/1/config"), map[string]any{"artifacts_max_mb": 0}).StatusCode; got != http.StatusOK {
		t.Fatalf("setting the budget answered %d", got)
	}

	bundle := testBundle(t, "", "")
	chunks := splitIntoChunks(bundle, 1024)
	stack.uploadChunks(t, chunks, true)

	result := stack.assemble(t, assemblePath(t), map[string]any{
		"checksum": sha1Of(bundle),
		"chunks":   checksumsOf(chunks),
		"projects": []string{"venekambio"},
	})
	if result.State != "error" {
		t.Fatalf("state = %q, want error", result.State)
	}
	if result.Detail == nil || !strings.Contains(*result.Detail, "budget") {
		t.Fatalf("detail = %v, and it is what the user is shown", result.Detail)
	}

	// The paths that do name a project answer with a status code, because
	// there the client can read one.
	legacy := stack.postParts(t, recordedPath(t, fixtureRoot+"/2.57.0/legacy-release-files",
		"002-post-api-0-projects-acme-venekambio-releases-app-1-0-0-files.json"), []uploadPart{
		{field: "file", filename: "bundle.min.js", body: []byte("console.log(1)")},
		{field: "name", body: []byte("~/bundle.min.js")},
	})
	if legacy.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("the legacy path answered %d, want 413", legacy.StatusCode)
	}
}

// --- helpers -----------------------------------------------------------

type assembleResult struct {
	State         string   `json:"state"`
	MissingChunks []string `json:"missingChunks"`
	Detail        *string  `json:"detail"`
}

type uploadPart struct {
	field    string
	filename string
	body     []byte
}

func (s compatStack) get(t *testing.T, path string) *http.Response {
	t.Helper()
	return tokenRequest(t, s.baseURL, s.token, http.MethodGet, path, nil)
}

func (s compatStack) assemble(t *testing.T, path string, body map[string]any) assembleResult {
	t.Helper()
	response := tokenRequest(t, s.baseURL, s.token, http.MethodPost, path, body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("assemble answered %d; every outcome the caller could have caused is a state, "+
			"not a status", response.StatusCode)
	}
	var result assembleResult
	decodeInto(t, response, &result)
	if result.MissingChunks == nil {
		t.Fatal("missingChunks is null; the client expects a list")
	}
	return result
}

func (s compatStack) uploadChunks(t *testing.T, chunks [][]byte, compress bool) {
	t.Helper()
	parts := make([]uploadPart, 0, len(chunks))
	for _, chunk := range chunks {
		part := uploadPart{field: "file", filename: sha1Of(chunk), body: chunk}
		if compress {
			part.field, part.body = "file_gzip", gzipOf(t, chunk)
		}
		parts = append(parts, part)
	}
	response := s.postParts(t, chunkUploadPath(t), parts)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("uploading %d chunks answered %d", len(chunks), response.StatusCode)
	}
}

// postParts sends a multipart body shaped the way the recording describes it:
// one field name for every chunk, the checksum in the filename.
func (s compatStack) postParts(t *testing.T, path string, parts []uploadPart) *http.Response {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, part := range parts {
		var target io.Writer
		var err error
		if part.filename == "" {
			target, err = writer.CreateFormField(part.field)
		} else {
			header := textproto.MIMEHeader{}
			header.Set("Content-Disposition", fmt.Sprintf(
				`form-data; name=%q; filename=%q`, part.field, part.filename))
			header.Set("Content-Type", "application/octet-stream")
			target, err = writer.CreatePart(header)
		}
		if err != nil {
			t.Fatalf("building the multipart body: %v", err)
		}
		if _, err := target.Write(part.body); err != nil {
			t.Fatalf("building the multipart body: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("closing the multipart body: %v", err)
	}

	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		s.baseURL+path, bytes.NewReader(body.Bytes()))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+s.token)
	request.Header.Set("Content-Type", writer.FormDataContentType())

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

// testBundle builds the archive an upload carries, from the committed bundle
// and map the browser suite raises errors from.
func testBundle(t *testing.T, release, dist string) []byte {
	t.Helper()

	script, err := os.ReadFile(filepath.Join("..", "..", "sourcemap", "testdata", "bundle.min.js"))
	if err != nil {
		t.Fatalf("reading the committed bundle: %v", err)
	}
	sourceMap, err := os.ReadFile(filepath.Join("..", "..", "sourcemap", "testdata", "bundle.min.js.map"))
	if err != nil {
		t.Fatalf("reading the committed map: %v", err)
	}

	const debugID = "fc31aec3-520c-531b-836e-0d5e872e8a18"
	encoded, err := artifactbundle.Write(&artifactbundle.Bundle{
		DebugID: "9676ae7d-7fec-50bc-a1b9-085910f75007",
		Org:     "acme",
		Project: "venekambio",
		Release: release,
		Dist:    dist,
		Files: []artifactbundle.File{
			{
				Path:      artifactbundle.EntryPath("bundle.min.js"),
				Kind:      artifactbundle.KindMinifiedSource,
				URL:       artifactbundle.ArtifactURL("bundle.min.js"),
				DebugID:   debugID,
				SourceMap: "bundle.min.js.map",
				Content:   script,
			},
			{
				Path:    artifactbundle.EntryPath("bundle.min.js.map"),
				Kind:    artifactbundle.KindSourceMap,
				URL:     artifactbundle.ArtifactURL("bundle.min.js.map"),
				DebugID: debugID,
				Content: sourceMap,
			},
		},
	})
	if err != nil {
		t.Fatalf("building the bundle: %v", err)
	}
	return encoded
}

// splitIntoChunks cuts a bundle up the way the client does: every piece
// exactly the announced size but the last.
func splitIntoChunks(data []byte, size int) [][]byte {
	var chunks [][]byte
	for start := 0; start < len(data); start += size {
		end := start + size
		if end > len(data) {
			end = len(data)
		}
		chunks = append(chunks, data[start:end])
	}
	return chunks
}

func checksumsOf(chunks [][]byte) []string {
	names := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		names = append(names, sha1Of(chunk))
	}
	return names
}

func sha1Of(data []byte) string {
	sum := sha1.Sum(data) //nolint:gosec // the protocol's hash.
	return hex.EncodeToString(sum[:])
}

func gzipOf(t *testing.T, data []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	writer := gzip.NewWriter(&out)
	if _, err := writer.Write(data); err != nil {
		t.Fatalf("compressing: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("compressing: %v", err)
	}
	return out.Bytes()
}

func stringsOf(value any) []string {
	items, _ := value.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}
