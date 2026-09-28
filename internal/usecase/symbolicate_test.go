package usecase

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/envelope"
	"github.com/antoniojosev/trapline/internal/ports"
	"github.com/antoniojosev/trapline/internal/sentry"
	"github.com/antoniojosev/trapline/internal/sourcemap"
)

// The fixtures are the recorder's, read from where it left them rather than
// copied here. A copy is a fixture that stops being the one the recorder
// produces the first time somebody regenerates it, and this whole test rests
// on the map being the *injected* one — the map that pays for the two lines
// `sourcemaps inject` added to the script (from the recording).
const (
	fixtureDir      = "../sourcemap/testdata"
	fixtureEnvelope = "browser-debugid.envelope"
	fixtureMapFile  = "bundle.min.js.map"
	fixtureDebugID  = "fc31aec3-520c-531b-836e-0d5e872e8a18"
	fixtureCodeFile = "http://127.0.0.1:8080/bundle.min.js"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}

// countingArtifacts is an in-memory ports.ArtifactRepository that counts what
// was asked of it. The counts are the point of several tests below: the
// difference between a working cache and a broken one is invisible in the
// result and obvious in the number of lookups.
//
// Named apart from the fake in artifacts_test.go because the two answer
// different questions — that one is the upload side's store, this one is a
// tally — and a package with one fake serving both would make every count
// here depend on what the other file's tests happen to do.
type countingArtifacts struct {
	saved    []domain.Artifact
	contents map[int64][]byte
	nextID   int64

	debugLookups   int
	nameLookups    int
	contentReads   int
	existenceCalls int

	failExistence error
	failContent   error
}

var _ ports.ArtifactRepository = (*countingArtifacts)(nil)

func newCountingArtifacts() *countingArtifacts {
	return &countingArtifacts{contents: map[int64][]byte{}, nextID: 1}
}

func (f *countingArtifacts) Store(
	_ context.Context, artifact domain.Artifact, content []byte,
) (domain.Artifact, error) {
	artifact.ID = f.nextID
	f.nextID++
	artifact.Size = int64(len(content))
	f.saved = append(f.saved, artifact)
	f.contents[artifact.ID] = content
	return artifact, nil
}

// content is the half of a lookup that used to be a second call. It is
// counted separately from the lookup itself so a cache that resolved the row
// and re-read the bytes would still be visible here.
func (f *countingArtifacts) content(artifact domain.Artifact) ([]byte, error) {
	f.contentReads++
	if f.failContent != nil {
		return nil, f.failContent
	}
	return f.contents[artifact.ID], nil
}

func (f *countingArtifacts) ByDebugID(
	_ context.Context, projectID int64, debugID string, kind domain.ArtifactKind,
) (domain.Artifact, []byte, error) {
	f.debugLookups++
	for _, artifact := range f.saved {
		if artifact.ProjectID == projectID && artifact.DebugID == debugID && artifact.Kind == kind {
			content, err := f.content(artifact)
			if err != nil {
				return domain.Artifact{}, nil, err
			}
			return artifact, content, nil
		}
	}
	return domain.Artifact{}, nil, domain.ErrArtifactNotFound
}

func (f *countingArtifacts) ByReleaseURL(
	_ context.Context, projectID, releaseID int64, dist, name string,
) (domain.Artifact, []byte, error) {
	f.nameLookups++
	for _, artifact := range f.saved {
		if artifact.ProjectID == projectID && releaseIDOf(artifact) == releaseID &&
			artifact.Dist == dist && artifact.Name == domain.ArtifactURL(name) {
			content, err := f.content(artifact)
			if err != nil {
				return domain.Artifact{}, nil, err
			}
			return artifact, content, nil
		}
	}
	return domain.Artifact{}, nil, domain.ErrArtifactNotFound
}

func (f *countingArtifacts) HasArtifacts(_ context.Context, projectID int64) (bool, error) {
	f.existenceCalls++
	if f.failExistence != nil {
		return false, f.failExistence
	}
	for _, artifact := range f.saved {
		if artifact.ProjectID == projectID {
			return true, nil
		}
	}
	return false, nil
}

// The rest of the port. Symbolication never calls these — it only reads — and
// they are here because an interface is implemented whole.
func (f *countingArtifacts) List(
	context.Context, int64, ports.ArtifactFilter,
) ([]domain.Artifact, error) {
	return append([]domain.Artifact(nil), f.saved...), nil
}

func (f *countingArtifacts) Delete(_ context.Context, projectID, artifactID int64) error {
	for index, artifact := range f.saved {
		if artifact.ProjectID == projectID && artifact.ID == artifactID {
			f.saved = append(f.saved[:index], f.saved[index+1:]...)
			delete(f.contents, artifactID)
			return nil
		}
	}
	return domain.ErrArtifactNotFound
}

func (f *countingArtifacts) UsedBytes(_ context.Context, projectID int64) (int64, error) {
	var used int64
	for _, artifact := range f.saved {
		if artifact.ProjectID == projectID {
			used += artifact.Size
		}
	}
	return used, nil
}

func (f *countingArtifacts) PruneOrphansBefore(
	context.Context, time.Time, int,
) (int64, error) {
	return 0, nil
}

// releaseRef is the other direction: a literal release id as the domain wants
// it, since "belongs to no release" has to stay distinguishable from zero.
func releaseRef(id int64) *int64 { return &id }

// releaseIDOf reads the release of an artefact, which the domain models as a
// pointer so that "no release" is a different thing from release zero.
func releaseIDOf(artifact domain.Artifact) int64 {
	if artifact.ReleaseID == nil {
		return 0
	}
	return *artifact.ReleaseID
}

// fakeReleaseLookup resolves a version to an id, which is the only thing the legacy
// route needs a release for.
type fakeReleaseLookup struct {
	byVersion map[string]int64
	lookups   int
}

func (f *fakeReleaseLookup) Find(_ context.Context, projectID int64, version string) (domain.Release, error) {
	f.lookups++
	id, found := f.byVersion[version]
	if !found {
		return domain.Release{}, domain.ErrReleaseNotFound
	}
	return domain.Release{ID: id, ProjectID: projectID, Version: version}, nil
}

// newSymbolicator wires one over the fakes.
func newSymbolicator(artifacts *countingArtifacts, releases ports.ReleaseIDLookup) *Symbolicator {
	return NewSymbolicator(artifacts, releases, sourcemap.NewCache(1<<20), fixedClock{now: testNow})
}

// storeRecordedMap puts the recorded source map in the repository under the
// debug id the recorded event names.
func storeRecordedMap(t *testing.T, artifacts *countingArtifacts) {
	t.Helper()
	if _, err := artifacts.Store(context.Background(), domain.Artifact{
		ProjectID: 1,
		DebugID:   fixtureDebugID,
		Name:      "~/bundle.min.js.map",
		Kind:      domain.ArtifactSourceMap,
		CreatedAt: testNow,
	}, fixture(t, fixtureMapFile)); err != nil {
		t.Fatalf("storing the recorded map: %v", err)
	}
}

// decodeRecordedEvent pulls the event item out of the recorded envelope.
//
// The item, not the first one: `Sentry.init` sends a session before the event,
// so the first item of that envelope carries no event at all — the recording
// shows it, and a test that took item zero would be asserting about a
// session (from the recording).
func decodeRecordedEvent(t *testing.T) sentry.Event {
	t.Helper()
	parsed, err := envelope.Parse(bytes.NewReader(fixture(t, fixtureEnvelope)), envelope.Limits{})
	if err != nil {
		t.Fatalf("parsing the recorded envelope: %v", err)
	}
	for index := range parsed.Items {
		if parsed.Items[index].Type != "event" {
			continue
		}
		event, err := sentry.DecodeEvent(parsed.Items[index].Payload)
		if err != nil {
			t.Fatalf("decoding the recorded event: %v", err)
		}
		return event
	}
	t.Fatal("the recorded envelope has no event item")
	return sentry.Event{}
}

// TestRecordedEventSymbolicates is the end-to-end claim of symbolication, against
// the envelope a real Chromium produced.
func TestRecordedEventSymbolicates(t *testing.T) {
	artifacts := newCountingArtifacts()
	storeRecordedMap(t, artifacts)
	event := decodeRecordedEvent(t)

	if event.DebugMeta == nil || len(event.DebugMeta.Images) != 1 {
		t.Fatalf("the recorded event's debug_meta did not decode: %+v", event.DebugMeta)
	}
	if image := event.DebugMeta.Images[0]; image.DebugID != fixtureDebugID || image.CodeFile != fixtureCodeFile {
		t.Fatalf("debug image = %+v", image)
	}

	resolved := newSymbolicator(artifacts, nil).Apply(context.Background(), 1, &event)
	if resolved != 1 {
		t.Fatalf("resolved %d frames, want exactly the one from the injected bundle", resolved)
	}

	frames := event.Exceptions[len(event.Exceptions)-1].Stacktrace.Frames
	if len(frames) != 4 {
		t.Fatalf("the recorded stacktrace has %d frames, want 4", len(frames))
	}

	// The last frame is the one from the injected bundle.
	deepest := frames[3]
	if deepest.Filename != "../src/checkout.js" {
		t.Errorf("filename = %q, want ../src/checkout.js", deepest.Filename)
	}
	if deepest.Lineno != 10 || deepest.Colno != 9 {
		t.Errorf("resolved to %d:%d, want 10:9", deepest.Lineno, deepest.Colno)
	}
	if !strings.Contains(deepest.ContextLine, "throw new Error") {
		t.Errorf("context line = %q, want the throw", deepest.ContextLine)
	}
	if len(deepest.PreContext) == 0 || len(deepest.PostContext) == 0 {
		t.Errorf("no surrounding context: pre=%q post=%q", deepest.PreContext, deepest.PostContext)
	}
	if deepest.AbsPath != "http://127.0.0.1:8080/src/checkout.js" {
		t.Errorf("abs_path = %q, want the source resolved against the script's URL", deepest.AbsPath)
	}

	// And what it replaced is still there.
	if deepest.Raw == nil {
		t.Fatal("the raw frame was not kept")
	}
	if deepest.Raw.AbsPath != "" || deepest.Raw.Filename != fixtureCodeFile {
		t.Errorf("raw frame = %+v, want the minified filename", deepest.Raw)
	}
	if deepest.Raw.Lineno != 3 || deepest.Raw.Colno != 489 {
		t.Errorf("raw position = %d:%d, want 3:489", deepest.Raw.Lineno, deepest.Raw.Colno)
	}
	if deepest.Raw.Function != "Object.t [as decode]" {
		t.Errorf("raw function = %q, want the minified name", deepest.Raw.Function)
	}
}

// TestFramesWithoutADebugIDAreLeftAlone. The recorded event carries three
// frames from the page's own script, which was never injected. Leaving them
// untouched is deliberate — the fixture was recorded with them present exactly
// so this could be checked (from the recording).
func TestFramesWithoutADebugIDAreLeftAlone(t *testing.T) {
	artifacts := newCountingArtifacts()
	storeRecordedMap(t, artifacts)
	event := decodeRecordedEvent(t)

	newSymbolicator(artifacts, nil).Apply(context.Background(), 1, &event)

	for index, frame := range event.Exceptions[len(event.Exceptions)-1].Stacktrace.Frames[:3] {
		if frame.Raw != nil {
			t.Errorf("frame %d was rewritten: %+v", index, frame)
		}
		if !strings.HasSuffix(frame.Filename, "/envelope.js") {
			t.Errorf("frame %d filename = %q, want the page's own script", index, frame.Filename)
		}
	}
}

// TestSymbolicationRunsBeforeGrouping is the ADR 018 consequence stated as a
// test: the fingerprint is computed over the resolved frame, so switching
// source maps on can split one minified issue into a before and an after.
//
// Written down rather than left to be discovered, because it is the kind of
// change that looks like a grouping bug from the outside (ADR 003).
func TestSymbolicationChangesTheFingerprint(t *testing.T) {
	artifacts := newCountingArtifacts()
	storeRecordedMap(t, artifacts)

	minified := decodeRecordedEvent(t)
	before := domain.Fingerprint(groupingInputFor(&minified))

	resolvedEvent := decodeRecordedEvent(t)
	newSymbolicator(artifacts, nil).Apply(context.Background(), 1, &resolvedEvent)
	after := domain.Fingerprint(groupingInputFor(&resolvedEvent))

	if before == after {
		t.Fatal("grouping did not notice the symbolicated frame; " +
			"either the frame was not rewritten or grouping is reading the raw one")
	}
	if !strings.Contains(culpritFor(&resolvedEvent), "checkout.js") {
		t.Errorf("culprit = %q, want it to name the original file", culpritFor(&resolvedEvent))
	}
}

// TestIngestSymbolicatesBeforeItStores runs the whole path, because the order
// of operations inside Process is the thing being asserted: the payload that
// reaches storage must already carry the resolved frame.
func TestIngestSymbolicatesBeforeItStores(t *testing.T) {
	artifacts := newCountingArtifacts()
	storeRecordedMap(t, artifacts)

	ingest, issues, ctx := newIngest(t, nil)
	ingest = ingest.WithSourceMaps(newSymbolicator(artifacts, nil))

	result, err := ingest.Process(ctx, 1, bytes.NewReader(fixture(t, fixtureEnvelope)),
		envelope.Limits{}, ClientInfo{})
	if err != nil {
		t.Fatalf("processing the recorded envelope: %v", err)
	}
	if result.Accepted == 0 || len(issues.recorded) != 1 {
		t.Fatalf("result = %+v, recorded %d", result, len(issues.recorded))
	}

	var stored struct {
		Exception struct {
			Values []struct {
				Stacktrace struct {
					Frames []sentry.Frame `json:"frames"`
				} `json:"stacktrace"`
			} `json:"values"`
		} `json:"exception"`
		DebugMeta *sentry.DebugMeta `json:"debug_meta"`
	}
	if err := json.Unmarshal(issues.recorded[0].Payload, &stored); err != nil {
		t.Fatalf("reading the stored payload: %v", err)
	}
	frames := stored.Exception.Values[0].Stacktrace.Frames
	deepest := frames[len(frames)-1]
	if deepest.Filename != "../src/checkout.js" || deepest.Lineno != 10 {
		t.Errorf("stored frame = %+v, want the resolved position", deepest)
	}
	if deepest.Raw == nil || deepest.Raw.Lineno != 3 {
		t.Errorf("the stored frame lost its raw half: %+v", deepest.Raw)
	}
	// The images survive the round trip, so a map uploaded tomorrow can still
	// be checked against an event stored today.
	if stored.DebugMeta == nil || len(stored.DebugMeta.Images) != 1 {
		t.Errorf("debug_meta did not survive storage: %+v", stored.DebugMeta)
	}
	if !strings.Contains(issues.recorded[0].Observation.Culprit, "checkout.js") {
		t.Errorf("culprit = %q", issues.recorded[0].Observation.Culprit)
	}
}

// legacyMap is a hand-written map for a script served at /static/app.min.js.
const legacyMap = `{"version":3,"file":"app.min.js","sources":["../src/pay.js"],` +
	`"sourcesContent":["function pay() {\n  throw new Error('nope');\n}\n"],` +
	`"names":["pay"],"mappings":"AAAA,SAASA,IAAI;AACX"}`

// TestLegacyReleaseAndURLRoute is route two: no debug id anywhere, a release
// and a dist on the event, and the frame's URL in its `~/path` form.
//
// It also covers the correction the recording forced: the map is found through the
// script's `sourcemap` header, which names it by file name. A lookup that
// appended `.map` to the script's URL would find nothing here, which is the
// point of naming the map differently.
func TestLegacyReleaseAndURLRoute(t *testing.T) {
	artifacts := newCountingArtifacts()
	ctx := context.Background()
	if _, err := artifacts.Store(ctx, domain.Artifact{
		ProjectID: 1, ReleaseID: releaseRef(7), Dist: "prod",
		Name: "~/static/app.min.js", Kind: domain.ArtifactMinifiedSource,
		SourceMapRef: "app.js.map", CreatedAt: testNow,
	}, []byte("//minified")); err != nil {
		t.Fatalf("storing the script: %v", err)
	}
	if _, err := artifacts.Store(ctx, domain.Artifact{
		ProjectID: 1, ReleaseID: releaseRef(7), Dist: "prod",
		Name: "~/static/app.js.map", Kind: domain.ArtifactSourceMap, CreatedAt: testNow,
	}, []byte(legacyMap)); err != nil {
		t.Fatalf("storing the map: %v", err)
	}

	releases := &fakeReleaseLookup{byVersion: map[string]int64{"app@1.0.0": 7}}
	event := sentry.Event{
		Release: "app@1.0.0",
		Dist:    "prod",
		Exceptions: []sentry.Exception{{
			Type: "Error", Value: "nope",
			Stacktrace: &sentry.Stacktrace{Frames: []sentry.Frame{{
				AbsPath: "https://shop.example/static/app.min.js?v=9",
				Lineno:  2, Colno: 3, InApp: true,
			}}},
		}},
	}

	if resolved := newSymbolicator(artifacts, releases).Apply(ctx, 1, &event); resolved != 1 {
		t.Fatalf("resolved %d frames by the legacy route, want 1", resolved)
	}
	frame := event.Exceptions[0].Stacktrace.Frames[0]
	if frame.Filename != "../src/pay.js" || frame.Lineno != 2 {
		t.Errorf("frame = %+v, want ../src/pay.js line 2", frame)
	}
	if !strings.Contains(frame.ContextLine, "throw new Error") {
		t.Errorf("context line = %q", frame.ContextLine)
	}
	if releases.lookups != 1 {
		t.Errorf("the release was looked up %d times for one event, want 1", releases.lookups)
	}
}

// TestDebugIDWinsOverTheLegacyRoute pins the order ADR 018 fixes. Both keys
// are present and they point at different maps; the debug id has to be the one
// that answers, because it names the exact build that ran while the URL only
// names a path somebody uploaded under.
func TestDebugIDWinsOverTheLegacyRoute(t *testing.T) {
	artifacts := newCountingArtifacts()
	ctx := context.Background()
	storeRecordedMap(t, artifacts)
	if _, err := artifacts.Store(ctx, domain.Artifact{
		ProjectID: 1, ReleaseID: releaseRef(7), Name: "~/bundle.min.js",
		Kind: domain.ArtifactMinifiedSource, SourceMapRef: "wrong.js.map", CreatedAt: testNow,
	}, []byte("//minified")); err != nil {
		t.Fatalf("storing the script: %v", err)
	}
	if _, err := artifacts.Store(ctx, domain.Artifact{
		ProjectID: 1, ReleaseID: releaseRef(7), Name: "~/wrong.js.map",
		Kind: domain.ArtifactSourceMap, CreatedAt: testNow,
	}, []byte(legacyMap)); err != nil {
		t.Fatalf("storing the wrong map: %v", err)
	}

	releases := &fakeReleaseLookup{byVersion: map[string]int64{"compat@1.0.0": 7}}
	// Only the injected frame, so the count below is about this frame and not
	// about the page's own script — which has no debug id and does, correctly,
	// fall through to the legacy route.
	event := sentry.Event{
		Release: "compat@1.0.0",
		DebugMeta: &sentry.DebugMeta{Images: []sentry.DebugImage{{
			Type: "sourcemap", CodeFile: fixtureCodeFile, DebugID: fixtureDebugID,
		}}},
		Exceptions: []sentry.Exception{{Stacktrace: &sentry.Stacktrace{Frames: []sentry.Frame{
			{Filename: fixtureCodeFile, Lineno: 3, Colno: 489, InApp: true},
		}}}},
	}
	newSymbolicator(artifacts, releases).Apply(ctx, 1, &event)

	frames := event.Exceptions[0].Stacktrace.Frames
	if got := frames[0].Filename; got != "../src/checkout.js" {
		t.Errorf("filename = %q, want the debug id's map to have won", got)
	}
	if artifacts.nameLookups != 0 {
		t.Errorf("the legacy route ran %d lookups even though the debug id resolved", artifacts.nameLookups)
	}
	if releases.lookups != 0 {
		t.Errorf("the release was resolved %d times even though the debug id answered", releases.lookups)
	}
}

// TestAProjectWithNoArtifactsIsNeverQueried is ADR 005 applied to this
// subsystem: an installation that never uploaded a source map must not pay a
// database read per JavaScript event.
func TestAProjectWithNoArtifactsIsNeverQueried(t *testing.T) {
	artifacts := newCountingArtifacts()
	symbolicator := newSymbolicator(artifacts, nil)

	for range 5 {
		event := decodeRecordedEvent(t)
		if resolved := symbolicator.Apply(context.Background(), 1, &event); resolved != 0 {
			t.Fatalf("resolved %d frames with nothing uploaded", resolved)
		}
	}
	if artifacts.debugLookups != 0 || artifacts.nameLookups != 0 {
		t.Errorf("looked up artifacts %d/%d times for a project that has none",
			artifacts.debugLookups, artifacts.nameLookups)
	}
	if artifacts.existenceCalls != 1 {
		t.Errorf("asked whether the project has artifacts %d times in one TTL window, want 1",
			artifacts.existenceCalls)
	}
}

// TestAnEventWithNoKeyCostsNothing. Without debug images and without a
// release, neither route has anything to look up — and the existence check
// itself must not run either.
func TestAnEventWithNoKeyCostsNothing(t *testing.T) {
	artifacts := newCountingArtifacts()
	storeRecordedMap(t, artifacts)

	event := sentry.Event{Exceptions: []sentry.Exception{{
		Stacktrace: &sentry.Stacktrace{Frames: []sentry.Frame{{
			AbsPath: "/srv/app/views.py", Lineno: 10,
		}}},
	}}}
	if resolved := newSymbolicator(artifacts, nil).Apply(context.Background(), 1, &event); resolved != 0 {
		t.Fatalf("resolved %d frames of an event with no key", resolved)
	}
	if artifacts.existenceCalls != 0 {
		t.Errorf("an event with no key asked the store %d questions", artifacts.existenceCalls)
	}
}

// TestAnUnknownDebugIDIsLookedUpOnce. Remembering the miss is what stops a
// front end whose pipeline never uploads maps from querying once per event
// forever.
func TestAnUnknownDebugIDIsLookedUpOnce(t *testing.T) {
	artifacts := newCountingArtifacts()
	ctx := context.Background()
	// Some other artifact, so the project passes the existence check and the
	// lookup for this debug id actually happens.
	if _, err := artifacts.Store(ctx, domain.Artifact{
		ProjectID: 1, DebugID: "00000000-0000-4000-8000-000000000000",
		Name: "~/other.js.map", Kind: domain.ArtifactSourceMap, CreatedAt: testNow,
	}, []byte(legacyMap)); err != nil {
		t.Fatalf("storing: %v", err)
	}

	symbolicator := newSymbolicator(artifacts, nil)
	for range 5 {
		event := decodeRecordedEvent(t)
		symbolicator.Apply(ctx, 1, &event)
	}
	if artifacts.debugLookups != 1 {
		t.Errorf("an unknown debug id was looked up %d times, want 1", artifacts.debugLookups)
	}
}

// TestAKnownDebugIDIsParsedOnce. The parsed map is what the cache holds; a
// cache that stored the bytes and re-parsed them would cost the expensive half
// of the work on every event.
func TestAKnownDebugIDIsParsedOnce(t *testing.T) {
	artifacts := newCountingArtifacts()
	storeRecordedMap(t, artifacts)

	symbolicator := newSymbolicator(artifacts, nil)
	for range 5 {
		event := decodeRecordedEvent(t)
		if resolved := symbolicator.Apply(context.Background(), 1, &event); resolved != 1 {
			t.Fatal("a repeated event stopped resolving")
		}
	}
	if artifacts.contentReads != 1 {
		t.Errorf("the map was read from the store %d times, want 1", artifacts.contentReads)
	}
}

// TestAnUnreadableMapDoesNotCostTheEvent. A bad build artefact must never turn
// an error report into a dropped item: one wrong upload would otherwise stop
// error tracking for a whole front end, silently.
func TestAnUnreadableMapDoesNotCostTheEvent(t *testing.T) {
	for name, content := range map[string][]byte{
		"not json":     []byte("<!doctype html>"),
		"wrong verson": []byte(`{"version":2,"sources":[],"names":[],"mappings":""}`),
		"truncated":    []byte(`{"version":3,"sources":["a.js"],"names":[],"mappings":"AAAAg`),
	} {
		artifacts := newCountingArtifacts()
		if _, err := artifacts.Store(context.Background(), domain.Artifact{
			ProjectID: 1, DebugID: fixtureDebugID, Name: "~/bundle.min.js.map",
			Kind: domain.ArtifactSourceMap, CreatedAt: testNow,
		}, content); err != nil {
			t.Fatalf("%s: storing: %v", name, err)
		}

		ingest, issues, ctx := newIngest(t, nil)
		ingest = ingest.WithSourceMaps(newSymbolicator(artifacts, nil))
		result, err := ingest.Process(ctx, 1, bytes.NewReader(fixture(t, fixtureEnvelope)),
			envelope.Limits{}, ClientInfo{})
		if err != nil {
			t.Fatalf("%s: processing: %v", name, err)
		}
		if result.Accepted == 0 || len(issues.recorded) != 1 {
			t.Errorf("%s: a bad source map cost the event: %+v", name, result)
		}
	}
}

// TestAStoreThatFailsDoesNotCostTheEvent covers the other half: not a bad
// file, but a store that cannot answer.
func TestAStoreThatFailsDoesNotCostTheEvent(t *testing.T) {
	artifacts := newCountingArtifacts()
	storeRecordedMap(t, artifacts)
	artifacts.failContent = os.ErrPermission

	event := decodeRecordedEvent(t)
	if resolved := newSymbolicator(artifacts, nil).Apply(context.Background(), 1, &event); resolved != 0 {
		t.Errorf("resolved %d frames with an unreadable store", resolved)
	}
	if event.Exceptions[0].Stacktrace.Frames[3].Raw != nil {
		t.Error("a frame was rewritten even though nothing was read")
	}

	artifacts.failExistence = os.ErrPermission
	artifacts.existenceCalls = 0
	other := decodeRecordedEvent(t)
	if resolved := newSymbolicator(artifacts, nil).Apply(context.Background(), 1, &other); resolved != 0 {
		t.Errorf("resolved %d frames with a store that cannot be asked", resolved)
	}
}

// TestNilSymbolicatorIngestsAsBefore. An assembly without source maps has to
// behave exactly like every build without source maps, which is what makes the feature
// optional rather than a new requirement.
func TestNilSymbolicatorIngestsAsBefore(t *testing.T) {
	var symbolicator *Symbolicator
	event := decodeRecordedEvent(t)
	if resolved := symbolicator.Apply(context.Background(), 1, &event); resolved != 0 {
		t.Errorf("a nil symbolicator resolved %d frames", resolved)
	}
	if event.Exceptions[0].Stacktrace.Frames[3].Lineno != 3 {
		t.Error("a nil symbolicator rewrote a frame")
	}
}

// TestEveryStacktraceIsResolved. A chained exception carries a stacktrace per
// link, and the cause is the interesting half — resolving only the primary
// would leave it minified.
func TestEveryStacktraceIsResolved(t *testing.T) {
	artifacts := newCountingArtifacts()
	storeRecordedMap(t, artifacts)

	frame := sentry.Frame{Filename: fixtureCodeFile, Lineno: 3, Colno: 489, InApp: true}
	event := sentry.Event{
		DebugMeta: &sentry.DebugMeta{Images: []sentry.DebugImage{{
			Type: "sourcemap", CodeFile: fixtureCodeFile, DebugID: fixtureDebugID,
		}}},
		Exceptions: []sentry.Exception{
			{Type: "Error", Value: "cause", Stacktrace: &sentry.Stacktrace{Frames: []sentry.Frame{frame}}},
			{Type: "Error", Value: "wrapper", Stacktrace: &sentry.Stacktrace{Frames: []sentry.Frame{frame}}},
		},
		Stacktrace: &sentry.Stacktrace{Frames: []sentry.Frame{frame}},
	}

	if resolved := newSymbolicator(artifacts, nil).Apply(context.Background(), 1, &event); resolved != 3 {
		t.Errorf("resolved %d frames, want all three stacktraces", resolved)
	}
}

// TestImagesThatAreNotSourceMapsAreIgnored. The other image types name debug
// files for native platforms; sending a lookup after one can only ever miss,
// and doing so once per event is a cost with no upside.
func TestImagesThatAreNotSourceMapsAreIgnored(t *testing.T) {
	artifacts := newCountingArtifacts()
	storeRecordedMap(t, artifacts)

	event := sentry.Event{
		DebugMeta: &sentry.DebugMeta{Images: []sentry.DebugImage{
			{Type: "macho", CodeFile: "/usr/lib/libSystem.dylib", DebugID: fixtureDebugID},
		}},
		Exceptions: []sentry.Exception{{Stacktrace: &sentry.Stacktrace{Frames: []sentry.Frame{
			{Filename: "/usr/lib/libSystem.dylib", Lineno: 3, Colno: 489},
		}}}},
	}
	if resolved := newSymbolicator(artifacts, nil).Apply(context.Background(), 1, &event); resolved != 0 {
		t.Errorf("resolved %d frames from a non-sourcemap image", resolved)
	}
	if artifacts.debugLookups != 0 {
		t.Errorf("a non-sourcemap image caused %d lookups", artifacts.debugLookups)
	}
}

// TestFramesWithNoPositionAreLeftAlone. A frame without a line number is one
// the SDK could not locate either, and the mapping table is indexed by
// position — there is nothing to look up.
func TestFramesWithNoPositionAreLeftAlone(t *testing.T) {
	artifacts := newCountingArtifacts()
	storeRecordedMap(t, artifacts)

	event := sentry.Event{
		DebugMeta: &sentry.DebugMeta{Images: []sentry.DebugImage{{
			Type: "sourcemap", CodeFile: fixtureCodeFile, DebugID: fixtureDebugID,
		}}},
		Exceptions: []sentry.Exception{{Stacktrace: &sentry.Stacktrace{Frames: []sentry.Frame{
			{Filename: fixtureCodeFile},
			{Filename: "", Lineno: 3, Colno: 489},
			{Filename: fixtureCodeFile, Lineno: 9999, Colno: 1},
		}}}},
	}
	if resolved := newSymbolicator(artifacts, nil).Apply(context.Background(), 1, &event); resolved != 0 {
		t.Errorf("resolved %d frames that carry no resolvable position", resolved)
	}
}

// TestURLCandidatesCoverTheSpellingsAToolMayHaveUsed. The name an artifact was
// uploaded under is chosen by whoever ran the upload, and getting this list
// wrong makes the legacy route fail for a reason nobody can see from the
// outside.
func TestURLCandidatesCoverTheSpellings(t *testing.T) {
	got := urlCandidates("https://shop.example/static/app.min.js?v=9#x")
	want := []string{"~/static/app.min.js", "/static/app.min.js",
		"https://shop.example/static/app.min.js", "~/app.min.js"}
	if len(got) != len(want) {
		t.Fatalf("candidates = %q, want %q", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("candidate %d = %q, want %q", index, got[index], want[index])
		}
	}
}

// TestResolvedSourceURL. The map says where a file is relative to itself; the
// only base available is the script's URL, and a path that already says where
// it is must not be touched.
func TestResolvedSourceURL(t *testing.T) {
	for _, tc := range []struct{ codeFile, source, want string }{
		{"http://host/bundle.min.js", "../src/a.js", "http://host/src/a.js"},
		{"http://host/js/bundle.min.js", "./a.js", "http://host/js/a.js"},
		{"http://host/bundle.min.js", "webpack:///./src/a.js", "webpack:///./src/a.js"},
		{"http://host/bundle.min.js", "/abs/a.js", "/abs/a.js"},
		{"/local/bundle.min.js", "../src/a.js", "../src/a.js"},
		{"http://host/bundle.min.js", "", ""},
	} {
		if got := resolveAgainst(absoluteURL(tc.codeFile), tc.source); got != tc.want {
			t.Errorf("resolveAgainst(%q, %q) = %q, want %q", tc.codeFile, tc.source, got, tc.want)
		}
	}
}
