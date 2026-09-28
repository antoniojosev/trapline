package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/engine/sketch"
	"github.com/antoniojosev/trapline/internal/ports"
	"github.com/antoniojosev/trapline/internal/sentry"
)

// DefaultTransactionLimit is how many transactions a listing returns when the
// caller does not say. Twenty-five is a screen.
const DefaultTransactionLimit = 25

// MaxTransactionLimit bounds what a caller may ask for. Past it the answer is
// not a ranking any more, it is an export.
const MaxTransactionLimit = 200

// Transactions is the performance side of the product: what is slow, what is
// busy, what is failing, and one example of each.
//
// Every number it reports comes from the windows written during ingest, and
// the percentiles are computed here, at read time, by merging the sketches of
// the range. Nothing persists a percentile — that is the whole of ADR 007, and
// the reason the aggregate carries a histogram rather than three floats.
//
// It deliberately holds no clock. Every range it answers over is decided by the
// caller and arrives as an argument, so a clock here would be a dependency
// nothing reads — and the one thing in this area that does need to know the
// time, the downsampling job, has its own.
type Transactions struct {
	traces   ports.TraceRepository
	config   ports.ProjectConfigStore
	scrubber *domain.Scrubber
}

// NewTransactions wires the use case.
func NewTransactions(
	traces ports.TraceRepository,
	config ports.ProjectConfigStore,
	scrubber *domain.Scrubber,
) *Transactions {
	return &Transactions{traces: traces, config: config, scrubber: scrubber}
}

// TransactionReceipt says what one ingested transaction did.
type TransactionReceipt struct {
	// Name is the aggregation key it landed under.
	Name string
	// Sampled is whether its raw spans were stored.
	Sampled bool
	// EffectiveRate is the share of traces this installation is keeping for
	// this project, the SDK's own sampling included. It is reported so that a
	// panel can say what a reader is actually looking at rather than echoing
	// a server setting that is only half the story (ADR 021).
	EffectiveRate float64
}

// Accept records one transaction from an envelope.
//
// The order is the point. The aggregate is written for every transaction that
// arrives, and only then is sampling consulted — and it decides one thing:
// whether the raw spans are kept beside the counts. A design that sampled
// first would make the p95 a percentile of a tenth of the traffic, which is a
// different number that looks identical (ADR 021).
func (t *Transactions) Accept(
	ctx context.Context, projectID int64, payload []byte, sdkRate float64,
) (TransactionReceipt, error) {
	decoded, err := sentry.DecodeTransaction(payload)
	if err != nil {
		return TransactionReceipt{}, err
	}

	config, err := t.config.ProjectConfig(ctx, projectID)
	if err != nil {
		return TransactionReceipt{}, err
	}

	// The server's knob applied on top of whatever the SDK already did. They
	// compose: an SDK sending one in four and a server keeping one in ten
	// together keep one in forty, and reporting only the server's half would
	// be off by the SDK's factor for anybody who configured one.
	serverRate := config.TracesSampleRate()
	effective := serverRate
	if sdkRate > 0 && sdkRate <= 1 {
		effective = serverRate * sdkRate
	}

	name := domain.TransactionName(decoded.Name)
	record := ports.TransactionRecord{
		ProjectID:  projectID,
		Name:       name,
		At:         decoded.Start,
		DurationMS: float64(decoded.Duration()) / float64(time.Millisecond),
		Failed:     domain.TransactionStatus(decoded.Trace.Status).Failed(),
	}

	sampled := domain.SampleTrace(decoded.Trace.TraceID, serverRate)
	if sampled {
		spans, err := t.renderSpans(&decoded)
		if err != nil {
			return TransactionReceipt{}, err
		}
		record.Trace = &ports.TraceRecord{
			TraceID: decoded.Trace.TraceID,
			Status:  decoded.Trace.Status,
			Op:      decoded.Trace.Op,
			Spans:   spans,
		}
	}

	if err := t.traces.RecordTransaction(ctx, &record); err != nil {
		return TransactionReceipt{}, err
	}
	return TransactionReceipt{Name: name, Sampled: sampled, EffectiveRate: effective}, nil
}

// SpanView is one bar of a waterfall.
//
// The stored shape and the API shape are the same struct on purpose: a
// waterfall is read whole and handed to a client unchanged, so a second
// mapping would only be a second place for the two to drift apart.
type SpanView struct {
	SpanID       string            `json:"span_id,omitempty"`
	ParentSpanID string            `json:"parent_span_id,omitempty"`
	Op           string            `json:"op,omitempty"`
	Description  string            `json:"description,omitempty"`
	Status       string            `json:"status,omitempty"`
	Start        time.Time         `json:"start"`
	End          time.Time         `json:"end"`
	DurationMS   float64           `json:"duration_ms"`
	Tags         map[string]string `json:"tags,omitempty"`
	Data         map[string]any    `json:"data,omitempty"`
}

// renderSpans normalises and scrubs the waterfall before it is stored.
//
// Normalised, because the wire shape differs between SDKs and a client should
// not have to know which one produced a trace. Scrubbed, because a span's data
// is where a database driver puts the statement it ran and an HTTP client puts
// the headers it sent — so it is exactly as likely to carry a credential as an
// event's request map, and redacting at display time would mean the secret is
// already in the database, the backup and whatever an operator greps
// (SECURITY.md).
//
// The transaction's own span comes first, so a client can draw the root
// without looking for it.
func (t *Transactions) renderSpans(decoded *sentry.Transaction) ([]byte, error) {
	views := make([]SpanView, 0, len(decoded.Spans)+1)
	views = append(views, SpanView{
		SpanID:       decoded.Trace.SpanID,
		ParentSpanID: decoded.Trace.ParentSpanID,
		Op:           decoded.Trace.Op,
		Description:  domain.TransactionName(decoded.Name),
		Status:       decoded.Trace.Status,
		Start:        decoded.Start,
		End:          decoded.End,
		DurationMS:   float64(decoded.Duration()) / float64(time.Millisecond),
	})

	for index := range decoded.Spans {
		span := &decoded.Spans[index]
		views = append(views, SpanView{
			SpanID:       span.SpanID,
			ParentSpanID: span.ParentSpanID,
			Op:           span.Op,
			Description:  t.scrubber.ScrubString(span.Description),
			Status:       span.Status,
			Start:        span.Start,
			End:          span.End,
			DurationMS:   float64(span.End.Sub(span.Start)) / float64(time.Millisecond),
			Tags:         span.Tags,
			Data:         t.scrubber.ScrubMap(span.Data),
		})
	}

	encoded, err := json.Marshal(views)
	if err != nil {
		return nil, fmt.Errorf("encoding the waterfall: %w", err)
	}
	return encoded, nil
}

// TransactionSummary is one row of the listing.
//
// The three percentiles are computed here and never stored. Count and failures
// are stored, because they are the two numbers that compose.
type TransactionSummary struct {
	Name  string `json:"transaction"`
	Count int64  `json:"count"`
	// Failed is how many of them failed, and FailRate is the share, computed
	// rather than stored for the same reason a percentile is: a rate cannot
	// be merged with another window's.
	Failed   int64   `json:"failed"`
	FailRate float64 `json:"fail_rate"`
	// Sampled is how many of them had their raw spans kept, which is what
	// decides whether "show me a slow one" has anything to show.
	Sampled int64   `json:"sampled"`
	P50     float64 `json:"p50_ms"`
	P95     float64 `json:"p95_ms"`
	P99     float64 `json:"p99_ms"`
	// MeanMS, MinMS and MaxMS come out of the sketch exactly rather than from
	// its buckets: the sum and the two ends are kept as they were recorded.
	MeanMS float64 `json:"mean_ms"`
	MinMS  float64 `json:"min_ms"`
	MaxMS  float64 `json:"max_ms"`
}

// TransactionList is a ranked page of transactions over a range.
type TransactionList struct {
	Range domain.Range
	Sort  domain.TransactionSort
	Rows  []TransactionSummary
	// Total is how many transactions the range holds across every name, so a
	// header can say "12 340 requests" without the client adding up a page
	// that may have been truncated.
	Total int64
	// TotalFailed is the same for failures.
	TotalFailed int64
	// Sampling is what a reader is actually looking at.
	Sampling SamplingView
}

// SamplingView reports the two rates and what they produced.
//
// It exists because a sampled feature that does not say so is a feature that
// misleads: somebody comparing "12 traces" against their load balancer's
// request count needs to know which of the two numbers was sampled and by how
// much (ADR 021).
type SamplingView struct {
	// ServerRate is this project's traces_sample_rate.
	ServerRate float64 `json:"server_rate"`
	// Stored and Received are what actually happened over the range, so the
	// effective rate is observed rather than assumed — the SDK's own sampling
	// included, and any period during which the knob was set differently.
	Stored   int64 `json:"stored"`
	Received int64 `json:"received"`
	// EffectiveRate is Stored/Received, or the server rate when nothing was
	// received and there is nothing to observe.
	EffectiveRate float64 `json:"effective_rate"`
}

// List ranks a project's transactions over a range.
func (t *Transactions) List(
	ctx context.Context, projectID int64, window domain.Range,
	order domain.TransactionSort, limit int,
) (TransactionList, error) {
	if limit <= 0 {
		limit = DefaultTransactionLimit
	}
	if limit > MaxTransactionLimit {
		limit = MaxTransactionLimit
	}

	merged, err := t.mergeRange(ctx, projectID, "", window)
	if err != nil {
		return TransactionList{}, err
	}

	config, err := t.config.ProjectConfig(ctx, projectID)
	if err != nil {
		return TransactionList{}, err
	}

	result := TransactionList{
		Range: window,
		Sort:  order,
		Rows:  make([]TransactionSummary, 0, len(merged)),
		Sampling: SamplingView{
			ServerRate:    config.TracesSampleRate(),
			EffectiveRate: config.TracesSampleRate(),
		},
	}
	for _, entry := range merged {
		result.Rows = append(result.Rows, entry.summarise())
		result.Total += entry.count
		result.TotalFailed += entry.failed
		result.Sampling.Stored += entry.sampled
	}
	result.Sampling.Received = result.Total
	if result.Total > 0 {
		result.Sampling.EffectiveRate = float64(result.Sampling.Stored) / float64(result.Total)
	}

	rank(result.Rows, order)
	if len(result.Rows) > limit {
		result.Rows = result.Rows[:limit]
	}
	return result, nil
}

// rank orders a listing by the question that was asked.
//
// The tiebreaker is the name, always, so two transactions with identical
// numbers come out in the same order on every request. Without it a page would
// reshuffle under a reader between reloads, which reads as data changing when
// nothing did.
func rank(rows []TransactionSummary, order domain.TransactionSort) {
	sort.Slice(rows, func(a, b int) bool {
		left, right := &rows[a], &rows[b]
		switch order {
		case domain.SortCount:
			if left.Count != right.Count {
				return left.Count > right.Count
			}
		case domain.SortFail:
			if left.FailRate != right.FailRate {
				return left.FailRate > right.FailRate
			}
			// A hundred percent of two requests is not the same news as a
			// hundred percent of ten thousand, so volume breaks the tie
			// before the name does.
			if left.Failed != right.Failed {
				return left.Failed > right.Failed
			}
		case domain.SortP95:
			if left.P95 != right.P95 {
				return left.P95 > right.P95
			}
		}
		return left.Name < right.Name
	})
}

// TransactionPoint is one bucket of a series.
type TransactionPoint struct {
	Bucket   string  `json:"bucket"`
	Count    int64   `json:"count"`
	Failed   int64   `json:"failed"`
	Sampled  int64   `json:"sampled"`
	P50      float64 `json:"p50_ms"`
	P95      float64 `json:"p95_ms"`
	P99      float64 `json:"p99_ms"`
	FailRate float64 `json:"fail_rate"`
}

// TransactionSeries is one transaction's history at one resolution.
type TransactionSeries struct {
	Name       string
	Range      domain.Range
	Resolution domain.Resolution
	Points     []TransactionPoint
	Total      int64
	// Summary is the whole range merged: the percentiles somebody compares
	// the chart against. It is computed from the merged sketch and not from
	// the points, because averaging the points' percentiles is exactly the
	// mistake ADR 007 exists to prevent.
	Summary TransactionSummary
}

// Series returns one transaction's buckets over a range.
//
// The empty buckets are filled here rather than in the store, for the reason
// the error series gives: only this layer knows what was asked for, and a
// chart drawn from the buckets that had traffic draws a quiet night as a
// straight line between two spikes.
func (t *Transactions) Series(
	ctx context.Context, projectID int64, name string,
	window domain.Range, resolution domain.Resolution,
) (TransactionSeries, error) {
	if name == "" {
		return TransactionSeries{}, fmt.Errorf("%w: a series needs a transaction name",
			domain.ErrInvalidTransaction)
	}

	windows, err := t.readWindows(ctx, projectID, name, window, resolution)
	if err != nil {
		return TransactionSeries{}, err
	}

	byBucket := make(map[string]*aggregate, len(windows))
	whole := &aggregate{name: name}
	for index := range windows {
		row := &windows[index]
		bucket := byBucket[row.Bucket]
		if bucket == nil {
			bucket = &aggregate{name: name}
			byBucket[row.Bucket] = bucket
		}
		bucket.absorb(row)
		whole.absorb(row)
	}

	series := TransactionSeries{
		Name:       name,
		Range:      window,
		Resolution: resolution,
		Summary:    whole.summarise(),
		Total:      whole.count,
	}
	for _, bucket := range resolution.Buckets(window) {
		point := TransactionPoint{Bucket: bucket}
		if found := byBucket[bucket]; found != nil {
			summary := found.summarise()
			point.Count, point.Failed, point.Sampled = found.count, found.failed, found.sampled
			point.P50, point.P95, point.P99 = summary.P50, summary.P95, summary.P99
			point.FailRate = summary.FailRate
		}
		series.Points = append(series.Points, point)
	}
	return series, nil
}

// readWindows fetches the buckets a series needs.
//
// At hour resolution it reads both tables and folds the minutes that have not
// been downsampled yet into their hour. That is not an optimisation, it is the
// only correct answer: the last two hours live in txn_minute and everything
// older lives in txn_hour, so an hourly chart that read one table would end
// with two empty hours or begin with a wall of them, depending which.
func (t *Transactions) readWindows(
	ctx context.Context, projectID int64, name string,
	window domain.Range, resolution domain.Resolution,
) ([]ports.TransactionWindow, error) {
	if resolution == domain.ResolutionMinute {
		return t.traces.Windows(ctx, projectID, name, domain.ResolutionMinute,
			window.FirstBucket()+":00", window.LastBucket()+":59")
	}

	hours, err := t.traces.Windows(ctx, projectID, name, domain.ResolutionHour,
		window.FirstBucket(), window.LastBucket())
	if err != nil {
		return nil, err
	}
	minutes, err := t.traces.Windows(ctx, projectID, name, domain.ResolutionMinute,
		window.FirstBucket()+":00", window.LastBucket()+":59")
	if err != nil {
		return nil, err
	}
	for index := range minutes {
		// A minute bucket is its hour plus ":MM", so the hour is a prefix.
		// Cheaper than parsing and re-formatting, and it cannot disagree with
		// the layout because it is the layout.
		if len(minutes[index].Bucket) > len(domain.HourLayout) {
			minutes[index].Bucket = minutes[index].Bucket[:len(domain.HourLayout)]
		}
		hours = append(hours, minutes[index])
	}
	return hours, nil
}

// mergeRange merges every window of a range into one aggregate per
// transaction name, in a stable order.
func (t *Transactions) mergeRange(
	ctx context.Context, projectID int64, name string, window domain.Range,
) ([]*aggregate, error) {
	windows, err := t.readWindows(ctx, projectID, name, window, domain.ResolutionHour)
	if err != nil {
		return nil, err
	}

	byName := map[string]*aggregate{}
	order := make([]*aggregate, 0, 16)
	for index := range windows {
		row := &windows[index]
		entry := byName[row.Name]
		if entry == nil {
			entry = &aggregate{name: row.Name}
			byName[row.Name] = entry
			order = append(order, entry)
		}
		entry.absorb(row)
	}
	return order, nil
}

// aggregate is one transaction's merged windows.
type aggregate struct {
	name    string
	count   int64
	failed  int64
	sampled int64
	latency sketch.Sketch
}

// absorb folds one stored window in.
//
// A sketch that cannot be decoded is skipped rather than fatal, and the counts
// beside it are still taken. The alternative — failing the whole listing
// because one row of one window is corrupt — turns a cosmetic problem into an
// endpoint that answers nothing, on the page somebody opened during an
// incident.
func (a *aggregate) absorb(window *ports.TransactionWindow) {
	a.count += window.Count
	a.failed += window.Failed
	a.sampled += window.Sampled

	var stored sketch.Sketch
	if err := stored.UnmarshalBinary(window.Sketch); err != nil {
		slog.Warn("a stored latency sketch could not be read",
			"transaction", window.Name, "bucket", window.Bucket, "error", err)
		return
	}
	a.latency.Merge(&stored)
}

// summarise computes the percentiles, at read time, from the merged sketch.
func (a *aggregate) summarise() TransactionSummary {
	summary := TransactionSummary{
		Name:    a.name,
		Count:   a.count,
		Failed:  a.failed,
		Sampled: a.sampled,
		MeanMS:  a.latency.Mean(),
		MinMS:   a.latency.Min(),
		MaxMS:   a.latency.Max(),
	}
	if a.count > 0 {
		summary.FailRate = float64(a.failed) / float64(a.count)
	}
	summary.P50, _ = a.latency.Quantile(0.50)
	summary.P95, _ = a.latency.Quantile(0.95)
	summary.P99, _ = a.latency.Quantile(0.99)
	return summary
}

// TraceView is one waterfall, ready to hand to a client.
type TraceView struct {
	TraceID    string     `json:"trace_id"`
	Name       string     `json:"transaction"`
	At         time.Time  `json:"timestamp"`
	DurationMS int64      `json:"duration_ms"`
	Status     string     `json:"status,omitempty"`
	Op         string     `json:"op,omitempty"`
	Spans      []SpanView `json:"spans"`
}

// Trace reads one stored waterfall.
func (t *Transactions) Trace(ctx context.Context, projectID int64, traceID string) (TraceView, error) {
	stored, err := t.traces.Trace(ctx, projectID, traceID)
	if err != nil {
		return TraceView{}, err
	}
	return newTraceView(&stored), nil
}

// Examples lists the stored waterfalls of one transaction, slowest first.
//
// It is the only path from an aggregate to something a person can read. With
// sampling on, most requests left no example at all, and this is where that
// becomes visible rather than surprising.
func (t *Transactions) Examples(
	ctx context.Context, projectID int64, name string, limit int,
) ([]TraceView, error) {
	stored, err := t.traces.TracesFor(ctx, projectID, name, limit)
	if err != nil {
		return nil, err
	}
	views := make([]TraceView, 0, len(stored))
	for index := range stored {
		views = append(views, newTraceView(&stored[index]))
	}
	return views, nil
}

func newTraceView(stored *ports.StoredTrace) TraceView {
	view := TraceView{
		TraceID:    stored.TraceID,
		Name:       stored.Name,
		At:         stored.At,
		DurationMS: stored.DurationMS,
		Status:     stored.Status,
		Op:         stored.Op,
		Spans:      []SpanView{},
	}
	if err := json.Unmarshal(stored.Spans, &view.Spans); err != nil {
		// The header is worth returning without them: a waterfall whose spans
		// cannot be decoded is a broken drawing, not a missing trace, and
		// answering nothing would send somebody looking for a trace id that
		// is right there.
		slog.Warn("a stored waterfall could not be decoded",
			"trace_id", stored.TraceID, "error", err)
		view.Spans = []SpanView{}
	}
	return view
}

// Downsample folds closed minute buckets into their hours.
//
// It is the operation the whole storage design rests on, and the reason the
// windows carry a sketch rather than three numbers: merging sixty sketches is
// adding counts, so an hour built this way is exactly the hour those
// transactions would have produced had they been recorded into it directly
// (ADR 020). Folding pre-computed percentiles would not be exact, would not be
// order-independent, and would be wrong in a way nothing downstream could see.
type Downsample struct {
	traces ports.TraceRepository
	clock  ports.Clock
	log    *slog.Logger

	// Interval between passes. Ten minutes rather than one: a minute becomes
	// eligible two hours after it closes, so nothing is gained by looking
	// more often than that, and the store has one writer shared with
	// ingestion.
	Interval time.Duration
	// BatchSize bounds one write transaction, for the reason every sweep in
	// this product is batched: a long one stalls the endpoint the product
	// exists to keep answering.
	BatchSize int
	// Pause between batches, so a large fold leaves gaps for the writer that
	// is actually serving users.
	Pause time.Duration
}

// NewDownsample wires the job with defaults.
func NewDownsample(traces ports.TraceRepository, clock ports.Clock, logger *slog.Logger) *Downsample {
	if logger == nil {
		logger = slog.Default()
	}
	return &Downsample{
		traces:    traces,
		clock:     clock,
		log:       logger,
		Interval:  10 * time.Minute,
		BatchSize: 500,
		Pause:     20 * time.Millisecond,
	}
}

// Fold runs one pass over the minutes that are safely closed.
func (d *Downsample) Fold(ctx context.Context) (int64, error) {
	return d.FoldBefore(ctx, d.clock.Now().Add(-domain.DownsampleAge))
}

// FoldBefore runs one pass with an explicit cutoff.
//
// It exists for the operator who wants the fold to happen on their schedule
// rather than this product's, and for the gate that has to prove an hour built
// from sixty minutes reports the same percentiles as the minutes did. Naming
// the cutoff is how "the clock moved forward" is expressed without a fake
// clock: the property being checked is about which buckets are eligible, and
// substituting time itself to say that would test the substitute.
//
// it with errors.Is(context.Canceled), and wrapping would only obscure that.
//
//nolint:wrapcheck // ctx.Err() is returned verbatim on purpose: callers match
func (d *Downsample) FoldBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	var folded int64
	for {
		if err := ctx.Err(); err != nil {
			return folded, err
		}

		batch, err := d.traces.FoldMinutes(ctx, cutoff, d.BatchSize)
		if err != nil {
			return folded, err
		}
		folded += batch

		if batch < int64(d.BatchSize) {
			return folded, nil
		}
		select {
		case <-ctx.Done():
			return folded, ctx.Err()
		case <-time.After(d.Pause):
		}
	}
}

// Job presents downsampling as a scheduled job.
func (d *Downsample) Job() DownsampleJob { return DownsampleJob{downsample: d} }

// HasWork answers the scheduler's question: is any project accepting
// transactions at all?
//
// Asked of the configuration and not of the tables, so a project switched on a
// minute ago has a job before its first transaction rather than after — and so
// an installation that never enables tracing has no goroutine, no timer and no
// row in /system/jobs (ADR 005, ADR 014).
func (d *Downsample) HasWork(ctx context.Context) (bool, error) {
	return d.traces.HasTracing(ctx)
}

// DownsampleJob is the folder seen through the scheduler's contract.
type DownsampleJob struct{ downsample *Downsample }

// Name identifies the job wherever it is reported.
func (j DownsampleJob) Name() string { return "downsample" }

// Interval is how long the scheduler waits between passes.
func (j DownsampleJob) Interval() time.Duration { return j.downsample.Interval }

// Run performs one pass.
func (j DownsampleJob) Run(ctx context.Context) error {
	folded, err := j.downsample.Fold(ctx)
	if err != nil {
		return err
	}
	if folded > 0 {
		j.downsample.log.Info("minute buckets folded into hours", "minutes", folded)
	}
	return nil
}
