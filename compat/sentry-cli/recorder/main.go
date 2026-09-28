// Command recorder is a stand-in server that answers sentry-cli and writes
// down everything it was asked.
//
// It exists because the API sentry-cli speaks is not the API its documentation
// describes. The documentation is partial, it lags the tool, and the tool
// sends things no page mentions — so implementing from memory produces a
// server that passes its own tests and rejects the real client. This project
// already has that scar: @sentry/node sends no authentication header at all,
// only ?sentry_key= in the query string, and a server built from the
// documented X-Sentry-Auth alone would have turned away every Node
// installation while its own suite stayed green (ADR 002).
//
// So: record first, implement second. This server answers whatever keeps the
// tool walking forward and persists each request as a fixture, and the fixtures
// — not anybody's memory — are what the real handler is written and tested
// against (ADR 013).
package main

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha1" //nolint:gosec // sha1 is what the chunk protocol names its checksums with; no security decision is being made here
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// maxBody bounds what is read from one request. A commit set is the largest
// thing sentry-cli sends and it is measured in megabytes, so the limit is
// generous; it is here only so a bug cannot turn the recorder into a memory
// sink.
const maxBody = 32 << 20

// recordedHeaders are the request headers worth keeping.
//
// An allowlist rather than the whole header map, for two reasons. The fixtures
// are committed, so anything recorded is published — and Authorization carries
// a real token. And a fixture that pinned every header would fail on the next
// version of the tool for reasons that do not matter, which is how a gate ends
// up disabled.
var recordedHeaders = []string{
	"authorization",
	"content-type",
	"content-encoding",
	"accept",
	"accept-encoding",
	"user-agent",
}

// request is one observed call, as it is committed.
type request struct {
	Seq    int                 `json:"seq"`
	Method string              `json:"method"`
	Path   string              `json:"path"`
	Query  map[string][]string `json:"query,omitempty"`
	Header map[string]string   `json:"headers"`
	// BodyJSON holds the body when it parses as JSON, which is the form the
	// handler tests want to read. Body holds the raw text otherwise.
	BodyJSON json.RawMessage `json:"body_json,omitempty"`
	Body     string          `json:"body,omitempty"`
	// Multipart describes a multipart/form-data body part by part, because the
	// only such body sentry-cli sends is a batch of gzipped chunks and its
	// bytes are neither readable nor stable.
	Multipart []part `json:"multipart,omitempty"`
	// Assembled describes what the chunks named by an assemble call add up
	// to. It is not something the client sent; it is what the server is
	// expected to be able to reconstruct and read, and the recording is the
	// only place that claim can be checked.
	Assembled *assembled `json:"assembled,omitempty"`
	// Status is what the recorder answered. It is part of the fixture because
	// the next request often depends on it: a client that gets a 404 takes a
	// different path than one that gets a 200.
	Status int `json:"response_status"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "address to listen on")
	out := flag.String("out", "", "directory to write fixtures into (required)")
	portFile := flag.String("port-file", "", "write the listening port here once bound")
	accept := flag.String("accept", strings.Join(defaultAccepted, ","),
		"the chunk-upload capabilities to advertise; the tool takes a different path for each")
	flag.Parse()

	accepted = strings.Split(*accept, ",")

	if *out == "" {
		log.Fatal("recorder: -out is required")
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatalf("recorder: creating %s: %v", *out, err)
	}

	rec := &recorder{dir: *out, chunks: make(map[string][]byte)}
	server := &http.Server{
		Handler:           rec,
		ReadHeaderTimeout: 10 * time.Second,
	}

	listener, err := listen(*addr)
	if err != nil {
		log.Fatalf("recorder: %v", err)
	}
	if *portFile != "" {
		port := listener.Addr().String()
		port = port[strings.LastIndexByte(port, ':')+1:]
		if err := os.WriteFile(*portFile, []byte(port), 0o644); err != nil { //nolint:gosec
			log.Fatalf("recorder: writing the port file: %v", err)
		}
	}
	log.Printf("recorder: listening on %s, writing to %s", listener.Addr(), *out)
	if err := server.Serve(listener); err != nil {
		log.Fatalf("recorder: %v", err)
	}
}

type recorder struct {
	dir string

	mu     sync.Mutex
	seq    int
	chunks map[string][]byte
}

// readinessPath is answered but never recorded. The script that starts this
// server has to know when it is listening, and a probe in the fixtures would
// be a request sentry-cli never made — which is the one thing a recording must
// not contain.
const readinessPath = "/__ready"

func (rec *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == readinessPath {
		w.WriteHeader(http.StatusOK)
		return
	}

	body, err := readBody(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	parts, err := multipartParts(r, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	status, payload := respond(r.Method, r.URL.Path, r.Host, body, parts)

	entry := request{
		Method: r.Method,
		Path:   r.URL.Path,
		Header: pickHeaders(r.Header),
		Status: status,
	}
	if query := r.URL.Query(); len(query) > 0 {
		entry.Query = query
	}
	switch {
	case parts != nil:
		// A chunk upload is a multipart body of gzipped binary. Recording it
		// verbatim would put megabytes of unreadable bytes in a committed
		// fixture and pin the compressor's output, which is not a protocol
		// claim. What the server has to know is the shape: which field names
		// carry chunks, and that the field name is the chunk's own checksum.
		entry.Multipart = parts
		rec.keepChunks(parts)
	case json.Valid(body) && len(bytes.TrimSpace(body)) > 0:
		entry.BodyJSON = json.RawMessage(indent(body))
	case len(body) > 0:
		entry.Body = string(body)
	}

	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/assemble/") {
		entry.Assembled = rec.assemble(body)
	}

	rec.write(&entry)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(payload); err != nil {
		log.Printf("recorder: writing the response: %v", err)
	}
}

// write persists one request, numbered so the fixtures read in the order the
// tool made them. The order is itself a finding: which call comes before which
// is how the client's flow is reconstructed later.
func (rec *recorder) write(entry *request) {
	rec.mu.Lock()
	rec.seq++
	entry.Seq = rec.seq
	rec.mu.Unlock()

	name := fmt.Sprintf("%03d-%s-%s.json", entry.Seq, strings.ToLower(entry.Method), slug(entry.Path))
	encoded, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		log.Printf("recorder: encoding %s: %v", name, err)
		return
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(filepath.Join(rec.dir, name), encoded, 0o644); err != nil { //nolint:gosec
		log.Printf("recorder: writing %s: %v", name, err)
	}
	log.Printf("recorder: %-6s %s -> %d", entry.Method, entry.Path, entry.Status)
}

// part is one section of a multipart/form-data body, described rather than
// copied.
//
// Both checksums are kept because which one the field name equals is the
// finding: sentry-cli names every chunk after a hash, and a server that
// verified the hash of what arrived on the wire — rather than of what the
// chunk decompresses to — would reject every upload while its own tests
// passed.
type part struct {
	Name     string `json:"name"`
	Filename string `json:"filename,omitempty"`
	// ContentType as the client declared it, if it declared one.
	ContentType string `json:"content_type,omitempty"`
	// Bytes and SHA1 describe what travelled on the wire.
	Bytes int    `json:"bytes"`
	SHA1  string `json:"sha1"`
	// Gzip says the part was gzipped, and Inflated* describe what it holds.
	Gzip          bool   `json:"gzip,omitempty"`
	InflatedBytes int    `json:"inflated_bytes,omitempty"`
	InflatedSHA1  string `json:"inflated_sha1,omitempty"`
	// FilenameIsSHA1Of says which of the two checksums the part's filename
	// equals, or is empty when it equals neither.
	FilenameIsSHA1Of string `json:"filename_is_sha1_of,omitempty"`
	// Value is the text a small non-file part carried. The legacy upload
	// sends the artifact's URL that way, and a fixture that recorded only
	// its length would hide the one field that says where the file goes.
	Value string `json:"value,omitempty"`

	// inflated is kept so the recorder can put the chunks back together and
	// look at what they were. It is not written to the fixture.
	inflated []byte
}

// maxRecordedValue bounds the text of a non-file part that lands in a fixture.
const maxRecordedValue = 512

// multipartParts describes a multipart/form-data body, or returns nil when the
// request is not one.
func multipartParts(r *http.Request, body []byte) ([]part, error) {
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return nil, nil //nolint:nilerr // not multipart is not an error here
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil, errors.New("a multipart body with no boundary")
	}

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var parts []part
	for {
		section, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return parts, nil
		}
		if err != nil {
			return nil, fmt.Errorf("reading a multipart section: %w", err)
		}
		raw, err := io.ReadAll(io.LimitReader(section, maxBody))
		_ = section.Close()
		if err != nil {
			return nil, fmt.Errorf("reading multipart section %q: %w", section.FormName(), err)
		}

		described := part{
			Name:        section.FormName(),
			Filename:    section.FileName(),
			ContentType: section.Header.Get("Content-Type"),
			Bytes:       len(raw),
			SHA1:        fmt.Sprintf("%x", sha1.Sum(raw)), //nolint:gosec // the protocol's own algorithm
		}
		if inflated, ok := gunzip(raw); ok {
			described.Gzip = true
			described.InflatedBytes = len(inflated)
			described.InflatedSHA1 = fmt.Sprintf("%x", sha1.Sum(inflated)) //nolint:gosec // as above
			described.inflated = inflated
		}
		// The checksum is in the *filename*, not the field name: every chunk
		// arrives under the same field, "file_gzip", and is told apart by the
		// filename it carries.
		switch described.Filename {
		case "":
			// A form field, not a file: it has no checksum to compare.
		case described.SHA1:
			described.FilenameIsSHA1Of = "wire bytes"
		case described.InflatedSHA1:
			described.FilenameIsSHA1Of = "inflated bytes"
		}
		// A part with no filename is a form field, and its value is the
		// interesting part. Bounded, because only a field is expected here.
		if described.Filename == "" && len(raw) <= maxRecordedValue && utf8.Valid(raw) {
			described.Value = string(raw)
		}
		parts = append(parts, described)
	}
}

// gunzip decompresses a gzip stream, reporting whether it was one.
func gunzip(raw []byte) ([]byte, bool) {
	reader, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, false
	}
	defer func() { _ = reader.Close() }()
	inflated, err := io.ReadAll(io.LimitReader(reader, maxBody))
	if err != nil {
		return nil, false
	}
	return inflated, true
}

// respond returns the least the client needs to keep going.
//
// A flat 200 {} to everything is the honest starting point and it is not
// enough: sentry-cli parses these answers and stops the moment one is the
// wrong *shape*, so a recorder that only said {} would capture the first two
// calls of a four-command flow and nothing after them. Every branch below was
// added because a run stopped without it, and what the tool refused to accept
// is a finding in its own right — it is the response contract the real handler
// has to honour, and it is not what the public documentation shows.
func respond(method, path, host string, body []byte, parts []part) (int, []byte) {
	version := releaseVersionIn(path)
	switch {
	// The capabilities document. Everything about a source map upload is
	// negotiated here — where the chunks go, how big they may be, how many
	// fit in one request, which hash names them, and which kinds of bundle
	// the server will assemble — and the tool refuses to start without it
	// ("missing field `url`" against a bare {}).
	case method == http.MethodGet && strings.HasSuffix(path, "/chunk-upload/"):
		return http.StatusOK, chunkUploadCapabilities(host, path)

	// The chunks themselves, and then the assembly poll. Both are answered
	// by dedicated helpers because their shapes are the whole finding.
	case method == http.MethodPost && strings.HasSuffix(path, "/chunk-upload/"):
		return http.StatusOK, []byte(`{}`)
	case method == http.MethodPost && strings.HasSuffix(path, "/assemble/"):
		return http.StatusOK, assembleBody()

	// One artifact stored the old way. The tool prints the object it gets
	// back and refuses a bare {} with "missing field `id`".
	case method == http.MethodPost && strings.HasSuffix(path, "/files/"):
		return http.StatusCreated, releaseFileBody(parts)
	// Nothing precedes the first release anyone ever set commits on, and
	// 404 is how the tool is told so. It logs "Could not find the previous
	// commit" and derives the range from the local repository, which is the
	// branch this recording is meant to capture.
	case method == http.MethodGet && strings.HasSuffix(path, "/previous-with-commits/"):
		return http.StatusNotFound, []byte(`{"detail":"Release not found"}`)

	// A release comes back as an object carrying its own version. `finalize`
	// fails with "missing field `version`" against a bare {}.
	case method == http.MethodPost && strings.HasSuffix(path, "/releases/"):
		return http.StatusCreated, releaseBody(versionFromBody(body))
	case method == http.MethodPut && version != "":
		return http.StatusOK, releaseBody(version)

	// A deploy comes back as an object with an id; the tool prints it.
	case method == http.MethodPost && strings.HasSuffix(path, "/deploys/"):
		return http.StatusCreated, []byte(`{"id":"1","name":"recorded","environment":"recorded",` +
			`"dateStarted":null,"dateFinished":"2026-08-29T00:00:00Z"}`)

	// Anything the tool reads as a list has to be a list. `set-commits`
	// fails with "invalid type: map, expected a sequence" against {} — and
	// an empty list is the answer a first deploy gets, which is the branch
	// that makes the tool derive the commit range from the local repository
	// instead of asking the server for one.
	case method == http.MethodGet && (strings.HasSuffix(path, "/repos/") ||
		strings.HasSuffix(path, "/releases/") ||
		strings.HasSuffix(path, "/commits/")):
		return http.StatusOK, []byte(`[]`)

	default:
		return http.StatusOK, []byte(`{}`)
	}
}

// chunkUploadCapabilities is the negotiation document.
//
// The url is absolute and points back at this server: the tool posts chunks to
// whatever this field says, not to the path it asked. Answering with somebody
// else's host would send an installation's source maps there.
func chunkUploadCapabilities(host, path string) []byte {
	encoded, err := json.Marshal(map[string]any{
		"url":              "http://" + host + path,
		"chunkSize":        chunkSize,
		"chunksPerRequest": 64,
		"maxFileSize":      2 << 30,
		"maxRequestSize":   32 << 20,
		"concurrency":      1,
		"hashAlgorithm":    "sha1",
		"compression":      []string{"gzip"},
		"accept":           accepted,
	})
	if err != nil {
		return []byte(`{}`)
	}
	return encoded
}

// chunkSize is deliberately small — smaller than any bundle this records — so
// that a recording of a few kilobytes still shows the tool splitting a file,
// naming each piece and listing them back at assembly. At Sentry's own 8 MiB
// every fixture here would be a single chunk and the multi-chunk case, which
// is the one a server gets wrong, would never appear in the recording.
const chunkSize = 1024

// accepted are the upload kinds this recorder claims to support, and the list
// is what steers the tool: drop artifact_bundles and the same command takes a
// different route entirely. It is a flag so one recorder can capture both, and
// so the choice a server makes here is visible as a choice.
var accepted = defaultAccepted

var defaultAccepted = []string{"release_files", "artifact_bundles"}

// assembleBody answers the assembly poll.
//
// The tool polls this until the state leaves "created", so a recorder that
// answered {} would hang; and the state has to be at the top level of the
// object, not keyed by checksum — a per-checksum map fails with
// "missing field `state`".
func assembleBody() []byte {
	return []byte(`{"state":"ok","missingChunks":[],"detail":null}`)
}

// releaseFileBody is the minimum release-file object the tool accepts. The
// name is echoed from the multipart field of the same name, so the fixture
// shows the artifact URL coming back rather than an invented one.
func releaseFileBody(parts []part) []byte {
	var uploaded part
	for index := range parts {
		if parts[index].Name == "file" {
			uploaded = parts[index]
		}
	}
	encoded, err := json.Marshal(map[string]any{
		"id":          "1",
		"name":        fieldValue(parts, "name"),
		"headers":     map[string]string{},
		"size":        uploaded.Bytes,
		"sha1":        uploaded.SHA1,
		"dateCreated": "2026-08-29T00:00:00Z",
	})
	if err != nil {
		return []byte(`{}`)
	}
	return encoded
}

// fieldValue returns the text of a named non-file part.
func fieldValue(parts []part, name string) string {
	for index := range parts {
		if parts[index].Name == name {
			return parts[index].Value
		}
	}
	return ""
}

// releaseBody is the minimum release object the tool accepts.
func releaseBody(version string) []byte {
	encoded, err := json.Marshal(map[string]any{
		"version":     version,
		"dateCreated": "2026-08-29T00:00:00Z",
		"projects":    []any{},
	})
	if err != nil {
		return []byte(`{}`)
	}
	return encoded
}

// releaseVersionIn pulls the version out of a path that addresses one release.
func releaseVersionIn(path string) string {
	const marker = "/releases/"
	index := strings.LastIndex(path, marker)
	if index < 0 {
		return ""
	}
	rest := strings.Trim(path[index+len(marker):], "/")
	if rest == "" || strings.Contains(rest, "/") {
		return ""
	}
	decoded, err := url.PathUnescape(rest)
	if err != nil {
		return rest
	}
	return decoded
}

// versionFromBody reads the version the client asked to create, so the answer
// echoes it rather than inventing one the client would then act on.
func versionFromBody(body []byte) string {
	var payload struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Version == "" {
		return "recorded"
	}
	return payload.Version
}

// readBody returns the body as the server would parse it, gunzipped when the
// client compressed it. A fixture full of gzip bytes would record that the
// tool compresses and nothing else, and what it sends is the interesting part.
func readBody(r *http.Request) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("reading the body: %w", err)
	}
	if !strings.Contains(strings.ToLower(r.Header.Get("Content-Encoding")), "gzip") {
		return raw, nil
	}
	reader, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("the body announced gzip and is not: %w", err)
	}
	defer func() { _ = reader.Close() }()
	decoded, err := io.ReadAll(io.LimitReader(reader, maxBody))
	if err != nil {
		return nil, fmt.Errorf("decompressing the body: %w", err)
	}
	return decoded, nil
}

// pickHeaders keeps the allowlist and redacts the credential.
//
// The scheme survives and the secret does not: which scheme the tool uses is
// the entire authentication finding, and the token is a real one that would
// otherwise be committed.
func pickHeaders(header http.Header) map[string]string {
	picked := make(map[string]string, len(recordedHeaders))
	for _, name := range recordedHeaders {
		value := header.Get(name)
		if value == "" {
			continue
		}
		switch {
		case name == "authorization":
			if scheme, _, found := strings.Cut(value, " "); found {
				value = scheme + " <redacted>"
			} else {
				value = "<redacted>"
			}
		case name == "content-type" && strings.Contains(value, "boundary="):
			// The boundary is random per request. Recording it would make
			// every fixture differ from the last recording for a reason that
			// is not a protocol change, and the gate that compares them would
			// be turned off within a week.
			value = boundaryValue.ReplaceAllString(value, "boundary=<random>")
		}
		picked[name] = value
	}
	return picked
}

func indent(body []byte) []byte {
	var buffer bytes.Buffer
	if err := json.Indent(&buffer, body, "", "  "); err != nil {
		return body
	}
	return buffer.Bytes()
}

// listen binds the address, so main can learn the port when it asked for
// zero. The record script needs that: it lets the recorder pick a free port
// rather than encoding one, which is what keeps parallel sessions from
// colliding.
func listen(addr string) (net.Listener, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", addr, err)
	}
	return listener, nil
}

var boundaryValue = regexp.MustCompile(`boundary=[^;]+`)

var unsafeInName = regexp.MustCompile(`[^a-zA-Z0-9]+`)

// slug turns a path into a filename that says what it was.
func slug(path string) string {
	cleaned := unsafeInName.ReplaceAllString(strings.Trim(path, "/"), "-")
	cleaned = strings.Trim(cleaned, "-")
	if cleaned == "" {
		return "root"
	}
	if len(cleaned) > 80 {
		cleaned = cleaned[:80]
	}
	return strings.ToLower(cleaned)
}

// assembled describes what an assemble call's chunks add up to.
//
// The client posts a checksum and an ordered list of chunk checksums, and
// stops there. Everything the server has to do afterwards — concatenate in
// that order, verify the whole, open it, read its manifest — happens where no
// request can be seen, so a recording that stopped at the request would leave
// the format of the thing being uploaded entirely undocumented. That format is
// the hard half of the work: the chunks are just bytes.
type assembled struct {
	Bytes int `json:"bytes"`
	// ChecksumMatches says whether the sha1 of the concatenation equals the
	// checksum the client announced. If it ever did not, the order or the
	// hashed form would be wrong and every other finding here suspect.
	ChecksumMatches bool `json:"checksum_matches"`
	// Format is what the bytes turned out to be.
	Format string `json:"format"`
	// Files lists the archive's entries, and Manifest is the manifest it
	// carries — the map from an entry to the URL, debug id and type the
	// server has to index it under.
	Files    []string        `json:"files,omitempty"`
	Manifest json.RawMessage `json:"manifest,omitempty"`
	// MissingChunks names chunks the client referred to and never sent, which
	// would mean the recording is incomplete.
	MissingChunks []string `json:"missing_chunks,omitempty"`
}

// keepChunks remembers the uploaded chunks by their checksum.
func (rec *recorder) keepChunks(parts []part) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for index := range parts {
		if parts[index].InflatedSHA1 == "" {
			continue
		}
		rec.chunks[parts[index].InflatedSHA1] = parts[index].inflated
	}
}

// assemble puts the named chunks back together and looks at the result.
func (rec *recorder) assemble(body []byte) *assembled {
	var asked struct {
		Checksum string   `json:"checksum"`
		Chunks   []string `json:"chunks"`
	}
	if err := json.Unmarshal(body, &asked); err != nil || len(asked.Chunks) == 0 {
		return nil
	}

	rec.mu.Lock()
	var whole []byte
	var missing []string
	for _, checksum := range asked.Chunks {
		chunk, ok := rec.chunks[checksum]
		if !ok {
			missing = append(missing, checksum)
			continue
		}
		whole = append(whole, chunk...)
	}
	rec.mu.Unlock()

	description := &assembled{
		Bytes:           len(whole),
		ChecksumMatches: fmt.Sprintf("%x", sha1.Sum(whole)) == asked.Checksum, //nolint:gosec // the protocol's algorithm
		Format:          "unknown",
		MissingChunks:   missing,
	}
	describeArchive(whole, description)
	return description
}

// manifestName is the entry an artifact bundle is indexed by.
const manifestName = "manifest.json"

// describeArchive opens the assembled bytes as a zip and reads the manifest.
func describeArchive(whole []byte, description *assembled) {
	archive, err := zip.NewReader(bytes.NewReader(whole), int64(len(whole)))
	if err != nil {
		return
	}
	description.Format = "zip"
	for _, file := range archive.File {
		description.Files = append(description.Files, file.Name)
		if file.Name != manifestName {
			continue
		}
		opened, err := file.Open()
		if err != nil {
			continue
		}
		manifest, err := io.ReadAll(io.LimitReader(opened, maxBody))
		_ = opened.Close()
		if err == nil && json.Valid(manifest) {
			description.Manifest = json.RawMessage(indent(manifest))
		}
	}
	sort.Strings(description.Files)
}
