package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
	"github.com/antoniojosev/trapline/internal/sourcemap"
	"github.com/antoniojosev/trapline/internal/usecase"
)

// The recorded fixture, read from where the recorder put it rather than copied.
// The bundle it describes is one the debug id actually belongs to, so the
// resolution this gate measures is the same work a real event causes — not a
// lookup that misses and returns early, which would measure nothing and pass
// forever.
const (
	benchMapPath = "../sourcemap/testdata/bundle.min.js.map"
	benchDebugID = "fc31aec3-520c-531b-836e-0d5e872e8a18"
	benchCodeURL = "http://127.0.0.1:8080/bundle.min.js"
)

// symbolicationBudget is how much of the layer symbolication may cost.
//
// The published budget is ten per cent of the ingest path end to end. This gate
// measures a slice of that path — see the comment on the test for why the
// whole path could not be made to hold still — so the budget has to be
// restated against the slice it is checked on:
//
//	the domain layer is 457 µs of the ~1270 µs an event costs end to end
//	(the breakdown TestIngestThroughputGate prints), so about 36% of it
//	10% of the whole path is therefore about 28% of this layer
//
// The gate is set at 20%, not 28%: stricter than the budget asks, because a
// budget checked on a stable measurement can afford to be, and because the
// measured cost is 12% — so this leaves eight points of margin rather than
// sixteen, which is a gate that would still notice a real regression.
//
// It is a ceiling on a feature that runs on every event of a JavaScript
// project, which is the hot path this product's whole claim rests on (ADR 001).
const symbolicationBudget = 0.80

// domainLayerShare is how much of an end-to-end event the domain layer is, from
// the breakdown TestIngestThroughputGate prints (457 µs of ~1270 µs). Used only
// to restate this gate's figure in the terms the budget uses, never to assert.
const domainLayerShare = 0.36

// benchSymbolicationEvents is how many events each repetition times.
const benchSymbolicationEvents = 4000

// benchSymbolicationReps is how many repetitions each side gets. The gate
// asserts on the best of them, like the throughput gate: interference can only
// subtract, so the best repetition is the closest estimate of what the code
// can do.
const benchSymbolicationReps = 3

func readBenchMap(tb testing.TB) []byte {
	tb.Helper()
	data, err := os.ReadFile(filepath.Clean(benchMapPath))
	if err != nil {
		tb.Fatalf("reading the recorded source map: %v", err)
	}
	return data
}

// TestSymbolicationOverheadGate is the symbolication cost gate: an event with twelve
// frames, every one of them resolvable, must not cost more than a tenth of the
// ingest path.
//
// Twelve frames and all of them resolving is the worst case on purpose. A real
// browser stacktrace mixes frames from injected bundles with frames from
// scripts nobody uploaded a map for, and those cost a map lookup and nothing
// else; measuring that mix would report a number smaller than the one an
// application with a fully instrumented build pays.
//
// # Why this measures the domain layer and not the socket
//
// It first did measure end to end over HTTP, and it could not be made to hold
// still: three runs of the identical binary reported 78%, 97% and 117%. The
// reason is in the p99s — nine to eleven milliseconds — which is SQLite's WAL
// and the page cache, not ingestion. A ratio between two measurements
// compounds the error on each side, so a denominator that swings 25% between
// adjacent runs makes a 10% budget unmeasurable, and a gate that swings
// twenty-six points on unchanged code is not measuring the code.
//
// So it measures layer 3 of the breakdown in doc.go — decompress, parse,
// decode, symbolicate, scrub, re-encode, fingerprint — against a storage port
// that discards. That is where symbolication actually runs, it has no disk in
// it, and it is the STRICTER of the two: the denominator excludes the SQLite
// transaction and the socket, so the same absolute cost is a larger share of
// it. A gate that passes here passes end to end by construction.
//
// The two sides differ in exactly one wire — whether the ingest use case was
// given a symbolicator — and they are interleaved rather than run one after
// the other, so a slow stretch of the machine lands on both.
func TestSymbolicationOverheadGate(t *testing.T) {
	requireBenchEnabled(t)

	events := envInt(t, "TRAPLINE_BENCH_EVENTS", benchSymbolicationEvents)
	reps := envInt(t, "TRAPLINE_BENCH_REPS", benchSymbolicationReps)

	t.Logf("machine: %s", machineDescription())
	t.Logf("workload: %d timed events of a minified browser exception with 12 resolvable frames, "+
		"%d interleaved repetitions per side, measured at the domain layer", events, reps)

	timed := buildMinifiedEnvelopes(t, timedOffset, events)
	warmup := buildMinifiedEnvelopes(t, 0, defaultWarmup)

	plainSamples := make([]sample, 0, reps)
	mappedSamples := make([]sample, 0, reps)
	for range reps {
		plainSamples = append(plainSamples, measureMinifiedDomain(t, false, warmup, timed))
		mappedSamples = append(mappedSamples, measureMinifiedDomain(t, true, warmup, timed))
	}

	plain, _, _ := summarise(plainSamples)
	symbolicated, _, _ := summarise(mappedSamples)

	ratio := symbolicated.eventsPerSecond() / plain.eventsPerSecond()
	t.Logf("%-16s %7.1f ev/s | p50 %6s p99 %6s", "without maps",
		plain.eventsPerSecond(), round(plain.percentile(0.50)), round(plain.percentile(0.99)))
	t.Logf("%-16s %7.1f ev/s | p50 %6s p99 %6s", "with maps",
		symbolicated.eventsPerSecond(), round(symbolicated.percentile(0.50)),
		round(symbolicated.percentile(0.99)))
	// Both numbers, because the published budget is about the other
	// one and a reader should not have to do the arithmetic. The domain layer
	// is about 36% of the end-to-end path, so a cost of X% here is roughly
	// X/3 % of what an SDK observes.
	t.Logf("symbolication keeps %.1f%% of the domain layer (budget: %.0f%%) "+
		"— about %.1f%% of the end-to-end path, against the published 10%%",
		ratio*100, symbolicationBudget*100, (1-ratio)*100*domainLayerShare)

	if ratio < symbolicationBudget {
		t.Errorf("symbolication costs %.1f%% of the domain layer, budget is %.0f%%.\n"+
			"This runs on every event of every JavaScript project, so the cost is paid by the "+
			"path ADR 001's volume claim is about. Check the source map cache first: a miss per "+
			"event means a store read and a re-parse per event, and that is what a regression "+
			"here almost always is.",
			(1-ratio)*100, (1-symbolicationBudget)*100)
	}

	// A measurement of nothing would pass this gate quietly, so the run also
	// proves the frames actually resolved.
	assertResolves(t)
}

// measureMinifiedDomain times one repetition of the ingest path with storage
// discarding, which is where symbolication runs.
func measureMinifiedDomain(tb testing.TB, sourceMaps bool, warmupBodies, timedBodies [][]byte) sample {
	tb.Helper()
	ingest := usecase.NewIngest(&discardIssues{}, alwaysAllow{}, benchScrubber(), benchClock{}, false)
	if sourceMaps {
		ingest = ingest.WithSourceMaps(benchSymbolicator(tb))
	}
	if _, err := runIngest(ingest, 1, warmupBodies); err != nil {
		tb.Fatalf("warming up: %v", err)
	}
	return timeIngest(tb, ingest, 1, timedBodies)
}

// benchSymbolicator wires one over an in-memory artifact store holding the
// recorded map.
//
// In memory rather than SQLite because this measurement is about the
// resolution, not about reading a blob: the cache means the store is touched
// once per process either way, and a database here would put the layer this
// gate deliberately excluded back inside it.
func benchSymbolicator(tb testing.TB) *usecase.Symbolicator {
	tb.Helper()
	return usecase.NewSymbolicator(
		&benchArtifacts{content: readBenchMap(tb)}, nil, sourcemap.NewCache(0), benchClock{})
}

// benchArtifacts is an artifact store holding exactly the recorded map.
type benchArtifacts struct{ content []byte }

var _ ports.ArtifactRepository = (*benchArtifacts)(nil)

func (b *benchArtifacts) ByDebugID(
	_ context.Context, _ int64, debugID string, kind domain.ArtifactKind,
) (domain.Artifact, []byte, error) {
	if debugID != benchDebugID || kind != domain.ArtifactSourceMap {
		return domain.Artifact{}, nil, domain.ErrArtifactNotFound
	}
	return domain.Artifact{ID: 1, ProjectID: 1, DebugID: debugID, Kind: kind,
		Name: "~/bundle.min.js.map", Size: int64(len(b.content))}, b.content, nil
}

func (b *benchArtifacts) ByReleaseURL(
	context.Context, int64, int64, string, string,
) (domain.Artifact, []byte, error) {
	return domain.Artifact{}, nil, domain.ErrArtifactNotFound
}

func (b *benchArtifacts) HasArtifacts(context.Context, int64) (bool, error) { return true, nil }

func (b *benchArtifacts) Store(
	_ context.Context, artifact domain.Artifact, _ []byte,
) (domain.Artifact, error) {
	return artifact, nil
}

func (b *benchArtifacts) List(
	context.Context, int64, ports.ArtifactFilter,
) ([]domain.Artifact, error) {
	return nil, nil
}

func (b *benchArtifacts) Delete(context.Context, int64, int64) error {
	return domain.ErrArtifactNotFound
}

func (b *benchArtifacts) UsedBytes(context.Context, int64) (int64, error) {
	return int64(len(b.content)), nil
}

func (b *benchArtifacts) PruneOrphansBefore(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

// assertResolves proves the workload is one symbolication actually does work
// on. Without it a change that made symbolication silently skip every frame
// would make this gate greener, which is the wrong direction for a budget.
func assertResolves(tb testing.TB) {
	tb.Helper()
	stack := newStackFor(tb, true)
	if err := stack.post(context.Background(), gzipBytes(tb, minifiedEnvelope(tb, 1))); err != nil {
		tb.Fatalf("posting: %v", err)
	}

	var culprit []byte
	if err := stack.db.QueryRowContext(context.Background(),
		"SELECT culprit FROM issues WHERE project_id = ? LIMIT 1", stack.projectID).Scan(&culprit); err != nil {
		tb.Fatalf("reading the issue the benchmark's event created: %v", err)
	}
	if !bytes.Contains(culprit, []byte("checkout.js")) {
		tb.Fatalf("the benchmark's event did not symbolicate: culprit is %q — "+
			"this gate would then be measuring a lookup that misses", culprit)
	}
}

func buildMinifiedEnvelopes(tb testing.TB, offset, count int) [][]byte {
	tb.Helper()
	envelopes := make([][]byte, count)
	for index := range envelopes {
		envelopes[index] = gzipBytes(tb, minifiedEnvelope(tb, offset+index))
	}
	return envelopes
}

// minifiedEnvelope renders one browser exception whose twelve frames all live
// in the injected bundle.
//
// The columns are the ones the recorded map has segments at, so every frame
// resolves to a different original line — the shape a real minified stacktrace
// has, and the one that defeats any per-event memo cleverer than the map
// lookup itself.
func minifiedEnvelope(tb testing.TB, seq int) []byte {
	tb.Helper()

	columns := []int{489, 493, 499, 523, 524, 526, 527, 528, 535, 546, 547, 554}
	frames := make([]map[string]any, 0, len(columns))
	for _, column := range columns {
		frames = append(frames, map[string]any{
			"filename": benchCodeURL,
			"function": "Object.t [as decode]",
			"in_app":   true,
			"lineno":   3,
			"colno":    column,
		})
	}

	event := map[string]any{
		"event_id":    eventID(seq),
		"platform":    "javascript",
		"level":       "error",
		"release":     "bench@1.0.0",
		"environment": "production",
		"timestamp":   1788052283.126,
		"exception": map[string]any{"values": []any{map[string]any{
			"type":       "Error",
			"value":      "unsupported encoding utf8",
			"stacktrace": map[string]any{"frames": frames},
			"mechanism":  map[string]any{"type": "generic", "handled": true},
		}}},
		"request": map[string]any{
			"url":     "http://127.0.0.1:8080/checkout",
			"headers": map[string]any{"User-Agent": benchUserAgent},
			// A secret, because the scrubber walks this map on every real
			// event and a payload with nothing to redact would skip work the
			// measured path always pays.
			"cookies": "session=eyJhbGciOiJIUzI1NiJ9.bench; token=sk_live_51H8bench",
		},
		"tags": map[string]any{"browser.name": "Chrome"},
		"debug_meta": map[string]any{"images": []any{map[string]any{
			"type":      "sourcemap",
			"code_file": benchCodeURL,
			"debug_id":  benchDebugID,
		}}},
	}

	payload, err := json.Marshal(event)
	if err != nil {
		tb.Fatalf("the benchmark's own event is not encodable: %v", err)
	}

	var envelope bytes.Buffer
	fmt.Fprintf(&envelope, "{\"event_id\":\"%s\",\"sent_at\":\"2026-08-29T10:00:00Z\"}\n", eventID(seq))
	fmt.Fprintf(&envelope, "{\"type\":\"event\",\"content_type\":\"application/json\",\"length\":%d}\n",
		len(payload))
	envelope.Write(payload)
	envelope.WriteByte('\n')
	return envelope.Bytes()
}
