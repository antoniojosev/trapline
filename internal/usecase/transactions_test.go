package usecase

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/engine/sketch"
	"github.com/antoniojosev/trapline/internal/ports"
)

// fakeTraces is a trace repository that records what it was asked to do.
//
// A fake and not the real one here on purpose: what these tests are about is
// the use case's own decisions — the order of aggregate-then-sample, the
// merging of a range, the batching of a fold — and the storage adapter has its
// own suite against a real SQLite file (ADR 009).
type fakeTraces struct {
	records []ports.TransactionRecord
	windows []ports.TransactionWindow
	traces  map[string]ports.StoredTrace

	hasTracing bool
	// foldBatches is what FoldMinutes returns, one per call, so a test can
	// drive the batching loop without a database.
	foldBatches []int64
	foldCutoffs []time.Time
	pruned      map[string]int
	failWith    error
}

func newFakeTraces() *fakeTraces {
	return &fakeTraces{traces: map[string]ports.StoredTrace{}, pruned: map[string]int{}}
}

func (f *fakeTraces) RecordTransaction(_ context.Context, record *ports.TransactionRecord) error {
	if f.failWith != nil {
		return f.failWith
	}
	f.records = append(f.records, *record)
	return nil
}

func (f *fakeTraces) Windows(
	_ context.Context, _ int64, name string, resolution domain.Resolution, from, to string,
) ([]ports.TransactionWindow, error) {
	if f.failWith != nil {
		return nil, f.failWith
	}
	var matched []ports.TransactionWindow
	for _, window := range f.windows {
		// The fake distinguishes the two tables the way the real one does: a
		// minute bucket is longer than an hour bucket.
		isMinute := len(window.Bucket) > len(domain.HourLayout)
		if isMinute != (resolution == domain.ResolutionMinute) {
			continue
		}
		if name != "" && window.Name != name {
			continue
		}
		if window.Bucket < from || window.Bucket > to {
			continue
		}
		matched = append(matched, window)
	}
	return matched, nil
}

func (f *fakeTraces) Trace(_ context.Context, _ int64, traceID string) (ports.StoredTrace, error) {
	trace, found := f.traces[traceID]
	if !found {
		return ports.StoredTrace{}, fmt.Errorf("%w: %s", domain.ErrTraceNotFound, traceID)
	}
	return trace, nil
}

func (f *fakeTraces) TracesFor(_ context.Context, _ int64, name string, _ int) ([]ports.StoredTrace, error) {
	var found []ports.StoredTrace
	for _, trace := range f.traces {
		if trace.Name == name {
			found = append(found, trace)
		}
	}
	return found, nil
}

func (f *fakeTraces) HasTracing(context.Context) (bool, error) { return f.hasTracing, nil }

func (f *fakeTraces) FoldMinutes(_ context.Context, cutoff time.Time, _ int) (int64, error) {
	f.foldCutoffs = append(f.foldCutoffs, cutoff)
	if f.failWith != nil {
		return 0, f.failWith
	}
	if len(f.foldBatches) == 0 {
		return 0, nil
	}
	batch := f.foldBatches[0]
	f.foldBatches = f.foldBatches[1:]
	return batch, nil
}

func (f *fakeTraces) PruneMinutes(context.Context, time.Time, int) (int64, error) {
	f.pruned["minutes"]++
	return 0, nil
}

func (f *fakeTraces) PruneHours(context.Context, int64, time.Time, int) (int64, error) {
	f.pruned["hours"]++
	return 0, nil
}

func (f *fakeTraces) PruneTraces(context.Context, int64, time.Time, int) (int64, error) {
	f.pruned["traces"]++
	return 0, nil
}

var _ ports.TraceRepository = (*fakeTraces)(nil)

// transactionPayload renders a transaction item the way an SDK sends one.
func transactionPayload(name, traceID, status string, start time.Time, duration time.Duration) []byte {
	return fmt.Appendf(nil,
		`{"transaction":%q,"start_timestamp":%q,"timestamp":%q,`+
			`"contexts":{"trace":{"trace_id":%q,"span_id":"fa90fdead5f74052","op":"http.server","status":%q}},`+
			`"spans":[{"span_id":"1111111111111111","op":"db.sql","description":"SELECT 1",`+
			`"data":{"password":"hunter2"}}]}`,
		name, start.Format(time.RFC3339Nano), start.Add(duration).Format(time.RFC3339Nano),
		traceID, status)
}

func newTransactions(t *testing.T, rate float64) (*Transactions, *fakeTraces, *fakeRepo) {
	t.Helper()
	projects := newFakeRepo()
	ctx := context.Background()
	if _, _, err := projects.Create(ctx, domain.Project{Name: "a", CreatedAt: testNow}, mustKey(t)); err != nil {
		t.Fatalf("creating project: %v", err)
	}
	if rate >= 0 {
		if err := projects.SetProjectConfig(ctx, 1, domain.ProjectConfig{
			TracesSampleRateSetting: &rate,
		}); err != nil {
			t.Fatalf("configuring: %v", err)
		}
	}
	traces := newFakeTraces()
	return NewTransactions(traces, projects, domain.NewScrubber(nil, nil)), traces, projects
}

// TestTheAggregateIsWrittenWhateverTheSamplingSays is the central claim of
// ADR 021 at the layer that decides it.
func TestTheAggregateIsWrittenWhateverTheSamplingSays(t *testing.T) {
	transactions, traces, _ := newTransactions(t, 0)
	ctx := context.Background()

	payload := transactionPayload("GET /", "4c79f60c11214eb38604f4ae0781bfb2", "ok",
		testNow, 250*time.Millisecond)
	receipt, err := transactions.Accept(ctx, 1, payload, 0)
	if err != nil {
		t.Fatalf("accepting: %v", err)
	}

	if receipt.Sampled {
		t.Error("a rate of zero stored a waterfall")
	}
	if len(traces.records) != 1 {
		t.Fatalf("%d records written, want 1: the aggregate is not optional", len(traces.records))
	}
	record := traces.records[0]
	if record.Trace != nil {
		t.Error("an unsampled transaction carried a trace to storage")
	}
	if record.DurationMS != 250 {
		t.Errorf("duration = %v ms, want 250", record.DurationMS)
	}
	if record.Name != "GET /" {
		t.Errorf("name = %q", record.Name)
	}
	if record.Failed {
		t.Error("an ok transaction was counted as a failure")
	}
}

func TestASampledTransactionCarriesAScrubbedWaterfall(t *testing.T) {
	transactions, traces, _ := newTransactions(t, 1)
	ctx := context.Background()

	payload := transactionPayload("GET /", "4c79f60c11214eb38604f4ae0781bfb2", "internal_error",
		testNow, 100*time.Millisecond)
	receipt, err := transactions.Accept(ctx, 1, payload, 0)
	if err != nil {
		t.Fatalf("accepting: %v", err)
	}
	if !receipt.Sampled {
		t.Fatal("a rate of one dropped a waterfall")
	}

	record := traces.records[0]
	if !record.Failed {
		t.Error("internal_error was not counted as a failure")
	}
	if record.Trace == nil {
		t.Fatal("a sampled transaction reached storage with no trace")
	}
	spans := string(record.Trace.Spans)
	if strings.Contains(spans, "hunter2") {
		t.Error("a password survived into the stored waterfall")
	}
	// The transaction's own span comes first so a client can draw the root
	// without looking for it.
	if !strings.HasPrefix(spans, `[{"span_id":"fa90fdead5f74052"`) {
		t.Errorf("the root span is not first: %s", spans)
	}
}

// TestTheSDKsOwnRateComposesWithTheServers: an SDK sending one in four and a
// server keeping one in ten together keep one in forty, and reporting only the
// server's half would be off by the SDK's factor.
func TestTheSDKsOwnRateComposesWithTheServers(t *testing.T) {
	transactions, _, _ := newTransactions(t, 0.5)
	ctx := context.Background()
	payload := transactionPayload("GET /", "4c79f60c11214eb38604f4ae0781bfb2", "ok",
		testNow, 10*time.Millisecond)

	alone, err := transactions.Accept(ctx, 1, payload, 0)
	if err != nil {
		t.Fatalf("accepting: %v", err)
	}
	if alone.EffectiveRate != 0.5 {
		t.Errorf("effective rate = %v with no SDK sampling, want the server's own", alone.EffectiveRate)
	}

	composed, err := transactions.Accept(ctx, 1, payload, 0.25)
	if err != nil {
		t.Fatalf("accepting: %v", err)
	}
	if composed.EffectiveRate != 0.125 {
		t.Errorf("effective rate = %v, want 0.125", composed.EffectiveRate)
	}
}

func TestAcceptRefusesAPayloadThatIsNotATransaction(t *testing.T) {
	transactions, traces, _ := newTransactions(t, 1)
	if _, err := transactions.Accept(context.Background(), 1, []byte(`{"transaction":"GET /"}`), 0); err == nil {
		t.Fatal("a transaction with no timestamps was accepted")
	}
	if len(traces.records) != 0 {
		t.Error("a refused transaction still reached storage")
	}
}

// windowWith builds a stored window whose sketch holds the given durations.
func windowWith(t *testing.T, name, bucket string, failed, sampled int64, durations ...float64) ports.TransactionWindow {
	t.Helper()
	var histogram sketch.Sketch
	for _, duration := range durations {
		if err := histogram.Add(duration); err != nil {
			t.Fatalf("building a sketch: %v", err)
		}
	}
	encoded, err := histogram.MarshalBinary()
	if err != nil {
		t.Fatalf("encoding a sketch: %v", err)
	}
	return ports.TransactionWindow{
		Name: name, Bucket: bucket, Count: int64(len(durations)),
		Failed: failed, Sampled: sampled, Sketch: encoded,
	}
}

func txnRange(t *testing.T) domain.Range {
	t.Helper()
	from := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	window, err := domain.NewRange(from, from.Add(time.Hour), from)
	if err != nil {
		t.Fatalf("building a range: %v", err)
	}
	return window
}

// TestListReadsBothTablesForOneRange is the correctness requirement the
// downsampling schedule creates.
//
// The last two hours live in txn_minute and everything older lives in
// txn_hour, so a listing that read one table would either lose the most recent
// traffic or all of the older traffic — and it would look like a quiet period
// rather than like a missing read.
func TestListReadsBothTablesForOneRange(t *testing.T) {
	transactions, traces, _ := newTransactions(t, 0.1)
	traces.windows = []ports.TransactionWindow{
		windowWith(t, "GET /", "2026-08-24T10", 1, 2, 10, 20, 30),
		windowWith(t, "GET /", "2026-08-24T10:45", 0, 1, 40, 50),
	}

	list, err := transactions.List(context.Background(), 1, txnRange(t), domain.SortP95, 0)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if list.Total != 5 {
		t.Errorf("total = %d, want 5 — the folded hour plus the open minutes", list.Total)
	}
	if len(list.Rows) != 1 {
		t.Fatalf("%d rows, want 1", len(list.Rows))
	}
	row := list.Rows[0]
	if row.Count != 5 || row.Failed != 1 || row.Sampled != 3 {
		t.Errorf("row = %+v", row)
	}
	if row.MinMS != 10 || row.MaxMS != 50 {
		t.Errorf("range [%v, %v], want [10, 50] — the exact ends survive the merge", row.MinMS, row.MaxMS)
	}
	if list.Sampling.Received != 5 || list.Sampling.Stored != 3 {
		t.Errorf("sampling = %+v", list.Sampling)
	}
	if list.Sampling.EffectiveRate != 3.0/5.0 {
		t.Errorf("effective rate = %v, want the observed 3 of 5", list.Sampling.EffectiveRate)
	}
	if list.Sampling.ServerRate != 0.1 {
		t.Errorf("server rate = %v", list.Sampling.ServerRate)
	}
}

func TestListRanksByTheQuestionAsked(t *testing.T) {
	transactions, traces, _ := newTransactions(t, 1)
	traces.windows = []ports.TransactionWindow{
		// Slow, busy, never fails.
		windowWith(t, "slow", "2026-08-24T10", 0, 0, 400, 410, 420, 430),
		// Fast, quiet, always fails — invisible on a latency chart.
		windowWith(t, "broken", "2026-08-24T10", 2, 0, 5, 6),
	}
	window := txnRange(t)
	ctx := context.Background()

	byLatency, err := transactions.List(ctx, 1, window, domain.SortP95, 0)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if byLatency.Rows[0].Name != "slow" {
		t.Errorf("ranked by p95, the first row is %q", byLatency.Rows[0].Name)
	}

	byFail, err := transactions.List(ctx, 1, window, domain.SortFail, 0)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if byFail.Rows[0].Name != "broken" {
		t.Errorf("ranked by failures, the first row is %q", byFail.Rows[0].Name)
	}

	byCount, err := transactions.List(ctx, 1, window, domain.SortCount, 0)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if byCount.Rows[0].Name != "slow" {
		t.Errorf("ranked by count, the first row is %q", byCount.Rows[0].Name)
	}
}

func TestListIsBoundedAndStable(t *testing.T) {
	transactions, traces, _ := newTransactions(t, 1)
	// Fifty transactions with identical numbers: only the tiebreaker decides
	// the order, and a page that reshuffled between reloads would read as
	// data changing when nothing did.
	for index := range 50 {
		traces.windows = append(traces.windows,
			windowWith(t, fmt.Sprintf("txn-%02d", index), "2026-08-24T10", 0, 0, 10))
	}
	window := txnRange(t)
	ctx := context.Background()

	first, err := transactions.List(ctx, 1, window, domain.SortP95, 5)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(first.Rows) != 5 {
		t.Fatalf("%d rows for a limit of 5", len(first.Rows))
	}
	second, err := transactions.List(ctx, 1, window, domain.SortP95, 5)
	if err != nil {
		t.Fatalf("listing again: %v", err)
	}
	for index := range first.Rows {
		if first.Rows[index].Name != second.Rows[index].Name {
			t.Fatalf("row %d moved between two identical requests: %q then %q",
				index, first.Rows[index].Name, second.Rows[index].Name)
		}
	}

	// A limit past the ceiling is capped rather than honoured.
	capped, err := transactions.List(ctx, 1, window, domain.SortP95, MaxTransactionLimit*10)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(capped.Rows) != 50 {
		t.Errorf("%d rows, want every one of the 50 that exist", len(capped.Rows))
	}
}

func TestSeriesFillsTheQuietBuckets(t *testing.T) {
	transactions, traces, _ := newTransactions(t, 1)
	traces.windows = []ports.TransactionWindow{
		windowWith(t, "GET /", "2026-08-24T10:05", 0, 0, 10, 20),
		windowWith(t, "GET /", "2026-08-24T10:07", 1, 0, 30),
	}

	series, err := transactions.Series(context.Background(), 1, "GET /", txnRange(t), domain.ResolutionMinute)
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	// Two whole hours of minutes: the range is closed at both ends and the
	// last hour contributes all sixty of its minutes.
	if len(series.Points) != 120 {
		t.Fatalf("%d points, want 120", len(series.Points))
	}
	if series.Total != 3 {
		t.Errorf("total = %d, want 3", series.Total)
	}

	busy := map[string]int64{}
	for _, point := range series.Points {
		if point.Count > 0 {
			busy[point.Bucket] = point.Count
		}
	}
	if len(busy) != 2 || busy["2026-08-24T10:05"] != 2 || busy["2026-08-24T10:07"] != 1 {
		t.Errorf("busy buckets = %v", busy)
	}

	// The summary is the whole range merged rather than an average of the
	// points' percentiles, which is exactly the mistake ADR 007 exists to
	// prevent.
	if series.Summary.Count != 3 || series.Summary.MaxMS != 30 {
		t.Errorf("summary = %+v", series.Summary)
	}
}

func TestSeriesNeedsAName(t *testing.T) {
	transactions, _, _ := newTransactions(t, 1)
	_, err := transactions.Series(context.Background(), 1, "", txnRange(t), domain.ResolutionMinute)
	if !errors.Is(err, domain.ErrInvalidTransaction) {
		t.Errorf("error = %v, want ErrInvalidTransaction", err)
	}
}

func TestACorruptWindowDoesNotFailTheWholeListing(t *testing.T) {
	transactions, traces, _ := newTransactions(t, 1)
	traces.windows = []ports.TransactionWindow{
		windowWith(t, "GET /", "2026-08-24T10", 0, 0, 10, 20),
		{Name: "GET /", Bucket: "2026-08-24T10:30", Count: 5, Sketch: []byte("not a sketch")},
	}

	list, err := transactions.List(context.Background(), 1, txnRange(t), domain.SortP95, 0)
	if err != nil {
		t.Fatalf("a corrupt window failed the whole listing: %v", err)
	}
	if list.Total != 7 {
		t.Errorf("total = %d, want 7 — the counts beside a bad sketch are still honest", list.Total)
	}
	if list.Rows[0].MaxMS != 20 {
		t.Errorf("max = %v, want the readable window's", list.Rows[0].MaxMS)
	}
}

func TestTraceAndExamples(t *testing.T) {
	transactions, traces, _ := newTransactions(t, 1)
	traces.traces["abc"] = ports.StoredTrace{
		TraceID: "abc", Name: "GET /", At: testNow, DurationMS: 250, Status: "ok",
		Spans: []byte(`[{"span_id":"a","op":"http.server","duration_ms":250}]`),
	}
	ctx := context.Background()

	view, err := transactions.Trace(ctx, 1, "abc")
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if len(view.Spans) != 1 || view.Spans[0].Op != "http.server" {
		t.Errorf("spans = %+v", view.Spans)
	}

	examples, err := transactions.Examples(ctx, 1, "GET /", 5)
	if err != nil {
		t.Fatalf("examples: %v", err)
	}
	if len(examples) != 1 {
		t.Errorf("%d examples, want 1", len(examples))
	}

	if _, err := transactions.Trace(ctx, 1, "nothing"); !errors.Is(err, domain.ErrTraceNotFound) {
		t.Errorf("error = %v, want ErrTraceNotFound", err)
	}
}

func TestATraceWithAnUnreadableWaterfallStillAnswers(t *testing.T) {
	transactions, traces, _ := newTransactions(t, 1)
	traces.traces["abc"] = ports.StoredTrace{
		TraceID: "abc", Name: "GET /", At: testNow, DurationMS: 250,
		Spans: []byte("not json"),
	}

	view, err := transactions.Trace(context.Background(), 1, "abc")
	if err != nil {
		// Answering nothing would send somebody looking for a trace id that
		// is right there.
		t.Fatalf("a broken waterfall took the whole trace with it: %v", err)
	}
	if view.TraceID != "abc" || view.Spans == nil {
		t.Errorf("view = %+v, want the header and an empty span list", view)
	}
}

func TestDownsampleFoldsUntilThereIsNothingLeft(t *testing.T) {
	traces := newFakeTraces()
	downsample := NewDownsample(traces, fixedClock{now: testNow}, nil)
	downsample.BatchSize = 10
	downsample.Pause = 0
	// Two full batches then a short one, which is how the loop knows to stop.
	traces.foldBatches = []int64{10, 10, 4}

	folded, err := downsample.Fold(context.Background())
	if err != nil {
		t.Fatalf("folding: %v", err)
	}
	if folded != 24 {
		t.Errorf("folded %d, want 24", folded)
	}
	if len(traces.foldCutoffs) != 3 {
		t.Errorf("%d passes, want 3", len(traces.foldCutoffs))
	}
	// The cutoff is the margin behind the clock, never the clock itself: a
	// minute is only safe to fold once nothing more can land in it.
	want := testNow.Add(-domain.DownsampleAge)
	if !traces.foldCutoffs[0].Equal(want) {
		t.Errorf("cutoff = %v, want %v", traces.foldCutoffs[0], want)
	}
}

func TestDownsampleStopsWhenTheContextEnds(t *testing.T) {
	traces := newFakeTraces()
	downsample := NewDownsample(traces, fixedClock{now: testNow}, nil)
	downsample.BatchSize = 10
	downsample.Pause = 0
	traces.foldBatches = []int64{10, 10, 10}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := downsample.Fold(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestDownsampleJobIsOfferedWithItsCondition(t *testing.T) {
	traces := newFakeTraces()
	downsample := NewDownsample(traces, fixedClock{now: testNow}, nil)
	job := downsample.Job()

	if job.Name() != "downsample" {
		t.Errorf("name = %q", job.Name())
	}
	if job.Interval() <= 0 {
		t.Errorf("interval = %s, which the scheduler refuses", job.Interval())
	}

	ctx := context.Background()
	if has, _ := downsample.HasWork(ctx); has {
		t.Error("an installation with no tracing reported work")
	}
	traces.hasTracing = true
	if has, _ := downsample.HasWork(ctx); !has {
		t.Error("a project accepting transactions did not report work")
	}

	traces.foldBatches = []int64{1}
	if err := job.Run(ctx); err != nil {
		t.Errorf("running: %v", err)
	}

	traces.failWith = errors.New("disk full")
	if err := job.Run(ctx); err == nil {
		t.Error("a failing fold was reported as success")
	}
}

// TestRetentionSweepsTheTransactionTables: the hours and the traces go on the
// project's transaction window, the minutes on the fixed 48-hour one, and the
// minute sweep runs once per pass rather than once per project.
func TestRetentionSweepsTheTransactionTables(t *testing.T) {
	retention, projects, _, _, ctx := newRetentionWithStats(t)
	traces := newFakeTraces()
	retention.WithTraces(traces)

	for _, name := range []string{"a", "b"} {
		if _, _, err := projects.Create(ctx,
			domain.Project{Name: name, CreatedAt: testNow}, mustKey(t)); err != nil {
			t.Fatalf("creating project: %v", err)
		}
	}

	if _, err := retention.Sweep(ctx); err != nil {
		t.Fatalf("sweeping: %v", err)
	}

	if traces.pruned["hours"] != 2 || traces.pruned["traces"] != 2 {
		t.Errorf("per-project sweeps: %v, want one of each per project", traces.pruned)
	}
	if traces.pruned["minutes"] != 1 {
		t.Errorf("the minute sweep ran %d times, want once per pass", traces.pruned["minutes"])
	}
}

func TestRetentionWithoutTracingSweepsNothingExtra(t *testing.T) {
	retention, projects, _, _, ctx := newRetentionWithStats(t)
	if _, _, err := projects.Create(ctx,
		domain.Project{Name: "a", CreatedAt: testNow}, mustKey(t)); err != nil {
		t.Fatalf("creating project: %v", err)
	}
	// A Retention built without WithTraces is the assembly every build before
	// tracing shipped, and it must still sweep.
	if _, err := retention.Sweep(ctx); err != nil {
		t.Fatalf("sweeping: %v", err)
	}
}
