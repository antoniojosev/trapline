package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/engine/sketch"
	"github.com/antoniojosev/trapline/internal/ports"
)

// Against a real SQLite file, never a mock (ADR 009). The interesting
// operation here is a read-modify-write of a sketch inside the same
// transaction that counts the transaction, and a mock would happily pretend
// that worked.

func recordN(
	t *testing.T, repo *TraceRepository, projectID int64,
	name string, at time.Time, durations []float64,
) {
	t.Helper()
	ctx := context.Background()
	for index, duration := range durations {
		record := ports.TransactionRecord{
			ProjectID:  projectID,
			Name:       name,
			At:         at,
			DurationMS: duration,
			Failed:     index%10 == 0,
		}
		if err := repo.RecordTransaction(ctx, &record); err != nil {
			t.Fatalf("recording: %v", err)
		}
	}
}

func mergedSketch(t *testing.T, windows []ports.TransactionWindow) *sketch.Sketch {
	t.Helper()
	merged := &sketch.Sketch{}
	for index := range windows {
		var stored sketch.Sketch
		if err := stored.UnmarshalBinary(windows[index].Sketch); err != nil {
			t.Fatalf("decoding a stored sketch: %v", err)
		}
		merged.Merge(&stored)
	}
	return merged
}

func TestRecordTransactionFoldsIntoItsMinute(t *testing.T) {
	db := openTemp(t)
	repo := NewTraceRepository(db)
	projectID := newProjectFor(t, db)
	at := time.Date(2026, 8, 29, 14, 32, 10, 0, time.UTC)

	durations := make([]float64, 0, 100)
	for index := range 100 {
		durations = append(durations, float64(index+1))
	}
	recordN(t, repo, projectID, "GET /api/checkout", at, durations)

	windows, err := repo.Windows(context.Background(), projectID, "",
		domain.ResolutionMinute, "2026-08-29T14:00", "2026-08-29T14:59")
	if err != nil {
		t.Fatalf("reading windows: %v", err)
	}
	if len(windows) != 1 {
		t.Fatalf("%d windows, want 1 — every transaction of one minute belongs in one row", len(windows))
	}
	window := windows[0]
	if window.Bucket != "2026-08-29T14:32" {
		t.Errorf("bucket %q", window.Bucket)
	}
	if window.Count != 100 {
		t.Errorf("count %d, want 100", window.Count)
	}
	if window.Failed != 10 {
		t.Errorf("failed %d, want 10", window.Failed)
	}
	if window.Sampled != 0 {
		t.Errorf("sampled %d, want 0 — nothing was handed a trace", window.Sampled)
	}

	merged := mergedSketch(t, windows)
	if merged.Count() != 100 {
		t.Fatalf("the sketch holds %d values, want 100", merged.Count())
	}
	// The exact 50th of 1..100 under the nearest-rank convention is 50.
	p50, _ := merged.Quantile(0.5)
	if p50 < 50*(1-sketch.Alpha) || p50 > 50*(1+sketch.Alpha) {
		t.Errorf("p50 %v, want 50 within alpha", p50)
	}
}

func TestRecordTransactionStoresTheWaterfallOnlyWhenSampled(t *testing.T) {
	db := openTemp(t)
	repo := NewTraceRepository(db)
	ctx := context.Background()
	projectID := newProjectFor(t, db)
	at := time.Date(2026, 8, 29, 14, 32, 10, 0, time.UTC)

	spans := []byte(`[{"span_id":"a","op":"http.server","duration_ms":250}]`)
	record := ports.TransactionRecord{
		ProjectID: projectID, Name: "GET /api/checkout", At: at, DurationMS: 250,
		Trace: &ports.TraceRecord{TraceID: "trace-1", Status: "ok", Op: "http.server", Spans: spans},
	}
	if err := repo.RecordTransaction(ctx, &record); err != nil {
		t.Fatalf("recording: %v", err)
	}

	// And one that was not sampled, into the same minute.
	unsampled := ports.TransactionRecord{
		ProjectID: projectID, Name: "GET /api/checkout", At: at, DurationMS: 100,
	}
	if err := repo.RecordTransaction(ctx, &unsampled); err != nil {
		t.Fatalf("recording: %v", err)
	}

	windows, err := repo.Windows(ctx, projectID, "", domain.ResolutionMinute,
		"2026-08-29T14:00", "2026-08-29T14:59")
	if err != nil {
		t.Fatalf("reading windows: %v", err)
	}
	if windows[0].Count != 2 {
		t.Errorf("count %d, want 2 — the aggregate covers both", windows[0].Count)
	}
	if windows[0].Sampled != 1 {
		t.Errorf("sampled %d, want 1 — only one had its spans kept", windows[0].Sampled)
	}

	stored, err := repo.Trace(ctx, projectID, "trace-1")
	if err != nil {
		t.Fatalf("reading the trace: %v", err)
	}
	if !bytes.Equal(stored.Spans, spans) {
		t.Errorf("spans came back as %s", stored.Spans)
	}
	if stored.Name != "GET /api/checkout" || stored.DurationMS != 250 || stored.Status != "ok" {
		t.Errorf("trace header %+v", stored)
	}
	if !stored.At.Equal(at) {
		t.Errorf("trace at %s, want %s", stored.At, at)
	}
}

func TestTraceNotFound(t *testing.T) {
	db := openTemp(t)
	repo := NewTraceRepository(db)
	projectID := newProjectFor(t, db)

	_, err := repo.Trace(context.Background(), projectID, "nothing")
	if !errors.Is(err, domain.ErrTraceNotFound) {
		t.Errorf("error is %v, want ErrTraceNotFound", err)
	}
}

// TestASecondTransactionOfOneTraceExtendsTheWaterfall is the distributed case.
//
// A request that crosses three services produces three transactions, each with
// its own spans, arriving separately and out of order. One row per trace, the
// header describing the earliest-starting transaction, and the spans
// accumulating — otherwise a waterfall shows one service and looks like
// instrumentation nobody added to the others.
func TestASecondTransactionOfOneTraceExtendsTheWaterfall(t *testing.T) {
	db := openTemp(t)
	repo := NewTraceRepository(db)
	ctx := context.Background()
	projectID := newProjectFor(t, db)

	later := time.Date(2026, 8, 29, 14, 32, 10, 0, time.UTC)
	earlier := later.Add(-50 * time.Millisecond)

	// The downstream service reports first, as it usually does: it finished
	// earlier and its SDK flushed sooner.
	downstream := ports.TransactionRecord{
		ProjectID: projectID, Name: "POST /internal/charge", At: later, DurationMS: 120,
		Trace: &ports.TraceRecord{
			TraceID: "trace-1", Status: "ok",
			Spans: []byte(`[{"span_id":"b","op":"db.sql"}]`),
		},
	}
	if err := repo.RecordTransaction(ctx, &downstream); err != nil {
		t.Fatalf("recording downstream: %v", err)
	}

	upstream := ports.TransactionRecord{
		ProjectID: projectID, Name: "GET /api/checkout", At: earlier, DurationMS: 250,
		Trace: &ports.TraceRecord{
			TraceID: "trace-1", Status: "ok", Op: "http.server",
			Spans: []byte(`[{"span_id":"a","op":"http.client"}]`),
		},
	}
	if err := repo.RecordTransaction(ctx, &upstream); err != nil {
		t.Fatalf("recording upstream: %v", err)
	}

	stored, err := repo.Trace(ctx, projectID, "trace-1")
	if err != nil {
		t.Fatalf("reading the trace: %v", err)
	}

	var spans []map[string]any
	if err := json.Unmarshal(stored.Spans, &spans); err != nil {
		t.Fatalf("the merged waterfall is not JSON: %v (%s)", err, stored.Spans)
	}
	if len(spans) != 2 {
		t.Fatalf("%d spans after merging two transactions, want 2: %s", len(spans), stored.Spans)
	}
	// The earliest-starting transaction is the root, and it is what the
	// listing shows.
	if stored.Name != "GET /api/checkout" {
		t.Errorf("the trace is named %q, want the root's name", stored.Name)
	}
	if !stored.At.Equal(earlier) {
		t.Errorf("the trace starts at %s, want the root's %s", stored.At, earlier)
	}
}

func TestWaterfallGrowthIsBounded(t *testing.T) {
	db := openTemp(t)
	repo := NewTraceRepository(db)
	ctx := context.Background()
	projectID := newProjectFor(t, db)
	at := time.Date(2026, 8, 29, 14, 32, 10, 0, time.UTC)

	// One span of about 4 KiB, a hundred times over: far past the budget.
	filler := strings.Repeat("x", 4000)
	for index := range 100 {
		record := ports.TransactionRecord{
			ProjectID: projectID, Name: "GET /", At: at.Add(time.Duration(index) * time.Millisecond),
			DurationMS: 10,
			Trace: &ports.TraceRecord{
				TraceID: "trace-1",
				Spans:   fmt.Appendf(nil, `[{"span_id":"%d","description":%q}]`, index, filler),
			},
		}
		if err := repo.RecordTransaction(ctx, &record); err != nil {
			t.Fatalf("recording %d: %v", index, err)
		}
	}

	stored, err := repo.Trace(ctx, projectID, "trace-1")
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if len(stored.Spans) > maxTraceSpansBytes+8000 {
		t.Errorf("the waterfall grew to %d bytes, past the %d budget",
			len(stored.Spans), maxTraceSpansBytes)
	}
	// And it is still a readable array rather than a truncated one.
	var spans []map[string]any
	if err := json.Unmarshal(stored.Spans, &spans); err != nil {
		t.Fatalf("the bounded waterfall is not JSON: %v", err)
	}
}

func TestTracesForListsSlowestFirst(t *testing.T) {
	db := openTemp(t)
	repo := NewTraceRepository(db)
	ctx := context.Background()
	projectID := newProjectFor(t, db)
	at := time.Date(2026, 8, 29, 14, 32, 10, 0, time.UTC)

	for index, duration := range []float64{50, 900, 300} {
		record := ports.TransactionRecord{
			ProjectID: projectID, Name: "GET /", At: at, DurationMS: duration,
			Trace: &ports.TraceRecord{TraceID: fmt.Sprintf("trace-%d", index), Spans: []byte("[]")},
		}
		if err := repo.RecordTransaction(ctx, &record); err != nil {
			t.Fatalf("recording: %v", err)
		}
	}

	traces, err := repo.TracesFor(ctx, projectID, "GET /", 0)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(traces) != 3 {
		t.Fatalf("%d traces, want 3", len(traces))
	}
	if traces[0].DurationMS != 900 || traces[2].DurationMS != 50 {
		t.Errorf("order is %v, %v, %v",
			traces[0].DurationMS, traces[1].DurationMS, traces[2].DurationMS)
	}

	// Another transaction's name finds nothing, which is what stops a listing
	// from reaching across the aggregation key.
	other, err := repo.TracesFor(ctx, projectID, "POST /", 10)
	if err != nil {
		t.Fatalf("listing another name: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("%d traces for a name nothing used", len(other))
	}
}

// TestFoldingIsExact is the claim the whole storage design rests on.
//
// An hour built by folding sixty minutes has to be the same hour those
// transactions would have produced had they been recorded into it directly.
// If it were not, every level of downsampling would drift and the drift would
// depend on the order the minutes happened to be folded in (ADR 020).
func TestFoldingIsExact(t *testing.T) {
	db := openTemp(t)
	repo := NewTraceRepository(db)
	ctx := context.Background()
	projectID := newProjectFor(t, db)

	hour := time.Date(2026, 8, 29, 14, 0, 0, 0, time.UTC)
	// A deliberately uneven hour: one busy minute of fast requests and one
	// quiet minute of slow ones, which is exactly the shape that makes
	// averaging percentiles wrong.
	for minute := range 60 {
		at := hour.Add(time.Duration(minute) * time.Minute)
		count := 5
		base := 500.0
		if minute%10 != 0 {
			count, base = 100, 20
		}
		durations := make([]float64, 0, count)
		for index := range count {
			durations = append(durations, base+float64(index))
		}
		recordN(t, repo, projectID, "GET /", at, durations)
	}

	before, err := repo.Windows(ctx, projectID, "GET /", domain.ResolutionMinute,
		"2026-08-29T14:00", "2026-08-29T14:59")
	if err != nil {
		t.Fatalf("reading minutes: %v", err)
	}
	expected := mergedSketch(t, before)

	// Fold everything: a cutoff after the hour makes every minute eligible.
	folded, err := repo.FoldMinutes(ctx, hour.Add(2*time.Hour), 1000)
	if err != nil {
		t.Fatalf("folding: %v", err)
	}
	if folded != 60 {
		t.Fatalf("folded %d minutes, want 60", folded)
	}

	leftover, err := repo.Windows(ctx, projectID, "", domain.ResolutionMinute,
		"2026-08-29T14:00", "2026-08-29T14:59")
	if err != nil {
		t.Fatalf("re-reading minutes: %v", err)
	}
	if len(leftover) != 0 {
		t.Errorf("%d minute buckets survived the fold", len(leftover))
	}

	hours, err := repo.Windows(ctx, projectID, "GET /", domain.ResolutionHour,
		"2026-08-29T14", "2026-08-29T14")
	if err != nil {
		t.Fatalf("reading hours: %v", err)
	}
	if len(hours) != 1 {
		t.Fatalf("%d hour buckets, want 1", len(hours))
	}
	if uint64(hours[0].Count) != expected.Count() {
		t.Errorf("the hour counts %d, the minutes counted %d", hours[0].Count, expected.Count())
	}

	got := mergedSketch(t, hours)
	for _, q := range []float64{0.5, 0.95, 0.99} {
		fromHour, _ := got.Quantile(q)
		fromMinutes, _ := expected.Quantile(q)
		if fromHour != fromMinutes {
			t.Errorf("q=%v: the folded hour reports %v, the minutes reported %v",
				q, fromHour, fromMinutes)
		}
	}
}

func TestFoldingLeavesOpenMinutesAlone(t *testing.T) {
	db := openTemp(t)
	repo := NewTraceRepository(db)
	ctx := context.Background()
	projectID := newProjectFor(t, db)

	now := time.Date(2026, 8, 29, 14, 30, 0, 0, time.UTC)
	recordN(t, repo, projectID, "GET /", now, []float64{10, 20})
	recordN(t, repo, projectID, "GET /", now.Add(-3*time.Hour), []float64{30})

	// The cutoff the job uses: two hours ago.
	folded, err := repo.FoldMinutes(ctx, now.Add(-2*time.Hour), 100)
	if err != nil {
		t.Fatalf("folding: %v", err)
	}
	if folded != 1 {
		t.Fatalf("folded %d minutes, want only the closed one", folded)
	}

	minutes, err := repo.Windows(ctx, projectID, "", domain.ResolutionMinute,
		"2026-08-29T00:00", "2026-08-29T23:59")
	if err != nil {
		t.Fatalf("reading minutes: %v", err)
	}
	if len(minutes) != 1 || minutes[0].Bucket != "2026-08-29T14:30" {
		t.Errorf("the open minute did not survive: %+v", minutes)
	}

	// A second pass with nothing to do reports nothing and does not fail.
	again, err := repo.FoldMinutes(ctx, now.Add(-2*time.Hour), 100)
	if err != nil {
		t.Fatalf("second fold: %v", err)
	}
	if again != 0 {
		t.Errorf("a second fold moved %d minutes", again)
	}
}

func TestHasTracingReadsTheConfigurationAndNotTheTables(t *testing.T) {
	db := openTemp(t)
	repo := NewTraceRepository(db)
	projects := NewProjectRepository(db)
	ctx := context.Background()

	project, _ := createProject(t, projects, "venekambio")

	// A fresh project accepts errors only, so there is nothing to fold.
	has, err := repo.HasTracing(ctx)
	if err != nil {
		t.Fatalf("asking: %v", err)
	}
	if has {
		t.Error("a fresh installation reported tracing work")
	}

	// Switching the category on has to be enough, before any transaction has
	// arrived: a job that waited for the first one would never fold the
	// minute that first one landed in.
	if err := projects.SetProjectConfig(ctx, project.ID, domain.ProjectConfig{
		EnabledCategories: []string{"error", "transaction"},
	}); err != nil {
		t.Fatalf("enabling transactions: %v", err)
	}
	has, err = repo.HasTracing(ctx)
	if err != nil {
		t.Fatalf("asking again: %v", err)
	}
	if !has {
		t.Error("a project accepting transactions did not report work")
	}
}

func TestPruningDeletesEachKindOnItsOwnClock(t *testing.T) {
	db := openTemp(t)
	repo := NewTraceRepository(db)
	ctx := context.Background()
	projectID := newProjectFor(t, db)

	old := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)

	for _, at := range []time.Time{old, recent} {
		record := ports.TransactionRecord{
			ProjectID: projectID, Name: "GET /", At: at, DurationMS: 10,
			Trace: &ports.TraceRecord{TraceID: "trace-" + at.Format("0102"), Spans: []byte("[]")},
		}
		if err := repo.RecordTransaction(ctx, &record); err != nil {
			t.Fatalf("recording: %v", err)
		}
	}
	if _, err := repo.FoldMinutes(ctx, recent.Add(time.Hour), 100); err != nil {
		t.Fatalf("folding: %v", err)
	}

	cutoff := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	hours, err := repo.PruneHours(ctx, projectID, cutoff, 100)
	if err != nil {
		t.Fatalf("pruning hours: %v", err)
	}
	if hours != 1 {
		t.Errorf("pruned %d hours, want 1", hours)
	}

	traces, err := repo.PruneTraces(ctx, projectID, cutoff, 100)
	if err != nil {
		t.Fatalf("pruning traces: %v", err)
	}
	if traces != 1 {
		t.Errorf("pruned %d traces, want 1", traces)
	}
	if _, err := repo.Trace(ctx, projectID, "trace-0829"); err != nil {
		t.Errorf("the recent trace was pruned too: %v", err)
	}

	// And the minute sweep, which is on the fixed window rather than a
	// project setting.
	recordN(t, repo, projectID, "GET /", old, []float64{5})
	minutes, err := repo.PruneMinutes(ctx, cutoff, 0)
	if err != nil {
		t.Fatalf("pruning minutes: %v", err)
	}
	if minutes != 1 {
		t.Errorf("pruned %d minutes, want 1", minutes)
	}
}

// TestACorruptSketchDoesNotCostTheTransaction: the count has to stay honest
// even when one window's blob is unreadable, because the alternative is
// refusing every transaction of a busy endpoint until somebody notices.
func TestACorruptSketchDoesNotCostTheTransaction(t *testing.T) {
	db := openTemp(t)
	repo := NewTraceRepository(db)
	ctx := context.Background()
	projectID := newProjectFor(t, db)
	at := time.Date(2026, 8, 29, 14, 32, 0, 0, time.UTC)

	recordN(t, repo, projectID, "GET /", at, []float64{10})
	if _, err := db.ExecContext(ctx,
		"UPDATE txn_minute SET sketch = ?", []byte("not a sketch")); err != nil {
		t.Fatalf("corrupting: %v", err)
	}

	recordN(t, repo, projectID, "GET /", at, []float64{20})

	windows, err := repo.Windows(ctx, projectID, "", domain.ResolutionMinute,
		"2026-08-29T14:00", "2026-08-29T14:59")
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if windows[0].Count != 2 {
		t.Errorf("count %d, want 2 — the corruption cost history, not the count", windows[0].Count)
	}
	merged := mergedSketch(t, windows)
	if merged.Count() != 1 {
		t.Errorf("the rebuilt sketch holds %d values, want the one recorded after the corruption",
			merged.Count())
	}
}

func TestAppendSpansHandlesEveryShape(t *testing.T) {
	cases := []struct {
		name     string
		existing string
		incoming string
		want     string
	}{
		{"both empty arrays", "[]", "[]", "[]"},
		{"nothing to add", `[{"a":1}]`, "[]", `[{"a":1}]`},
		{"nothing there yet", "[]", `[{"a":1}]`, `[{"a":1}]`},
		{"both present", `[{"a":1}]`, `[{"b":2}]`, `[{"a":1},{"b":2}]`},
		{"existing is not an array", "x", `[{"b":2}]`, `[{"b":2}]`},
		{"incoming is not an array", `[{"a":1}]`, "x", `[{"a":1}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(appendSpans([]byte(tc.existing), []byte(tc.incoming)))
			if got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}
