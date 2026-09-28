package ports

import (
	"context"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

// TransactionRecord is one transaction on its way into storage.
//
// It carries the duration already computed and the failure already decided,
// rather than the two timestamps and the raw status. That is deliberate: both
// are domain judgements — what counts as failed is a rule, not a column — and
// a repository that made them would put a rule in the one layer that cannot be
// tested without a database.
type TransactionRecord struct {
	ProjectID int64
	// Name is the aggregation key, already normalised by the domain.
	Name string
	// At is when the transaction started; it decides which bucket it lands in.
	At time.Time
	// DurationMS is what goes into the latency sketch.
	DurationMS float64
	// Failed is whether it counts against the failure rate.
	Failed bool
	// Trace is the raw waterfall, or nil when this transaction was not
	// sampled. The aggregate above is written either way — the sampling
	// decides storage, never accuracy (ADR 021).
	Trace *TraceRecord
}

// TraceRecord is a sampled trace's raw form.
type TraceRecord struct {
	TraceID string
	Status  string
	Op      string
	// Spans is the normalised, scrubbed waterfall as JSON. The repository
	// compresses it; it does not decide its shape.
	Spans []byte
}

// TransactionWindow is one bucket of one transaction, as read back.
//
// Sketch is the encoded histogram exactly as stored. It is bytes rather than a
// decoded type because ports/ may not import the engine's sketch package
// without dragging the engine's boundary into this one — and because the
// caller merges the whole range at once, so decoding per row here would be
// work thrown away.
type TransactionWindow struct {
	Name    string
	Bucket  string
	Count   int64
	Failed  int64
	Sampled int64
	Sketch  []byte
}

// StoredTrace is a waterfall read back.
type StoredTrace struct {
	TraceID    string
	ProjectID  int64
	Name       string
	At         time.Time
	DurationMS int64
	Status     string
	Op         string
	// Spans is the decompressed JSON array.
	Spans []byte
}

// TraceRepository stores transaction aggregates and the sampled waterfalls.
//
// One port for both, because one call writes both: the minute bucket and the
// trace row are the same fact seen at two resolutions, and committing them
// apart would let a restart leave a waterfall whose transaction was never
// counted. It is the same argument the uptime port makes for its three tables.
//
// Nothing here reads a payload to answer a listing or a chart. The windows
// carry their own counts and their own sketch, and the spans blob is only ever
// fetched by trace id, for one waterfall, after somebody asked for it by name
// (ADR 001).
type TraceRepository interface {
	// RecordTransaction writes one transaction: its bucket, its sketch, and
	// its trace when it was sampled — in one transaction.
	//
	// The sketch is read, merged and written back inside it. SQLite has a
	// single writer, which is what makes that safe without a lock of this
	// product's own (ADR 001).
	RecordTransaction(ctx context.Context, record *TransactionRecord) error

	// Windows reads the buckets of a range at one resolution, for every
	// transaction of a project or for one named transaction.
	//
	// Absent buckets are absent rather than zero: the caller knows the range
	// it asked for and fills the gaps, and a store that invented rows would
	// be inventing traffic.
	Windows(
		ctx context.Context, projectID int64, name string,
		resolution domain.Resolution, from, to string,
	) ([]TransactionWindow, error)

	// Trace reads one waterfall, or domain.ErrTraceNotFound.
	Trace(ctx context.Context, projectID int64, traceID string) (StoredTrace, error)
	// TracesFor lists the sampled traces of one transaction, slowest first.
	// It is what "show me a slow one" resolves to, and the only path from an
	// aggregate to an example.
	TracesFor(ctx context.Context, projectID int64, name string, limit int) ([]StoredTrace, error)

	// HasTracing reports whether any project accepts transactions. It is the
	// question the scheduler asks before the downsampling job exists at all
	// (ADR 014).
	HasTracing(ctx context.Context) (bool, error)

	// FoldMinutes merges every minute bucket closed before cutoff into its
	// hour and deletes it, at most limit minutes per call.
	//
	// Batched for the reason every sweep in this product is: the store has one
	// writer shared with ingestion, and a long write transaction stalls the
	// endpoint the product exists to keep answering.
	FoldMinutes(ctx context.Context, cutoff time.Time, limit int) (folded int64, err error)

	// PruneMinutes deletes minute buckets older than cutoff, whether or not
	// they were folded. It is the backstop for a window that was never folded
	// because tracing was switched off before the job ran.
	PruneMinutes(ctx context.Context, cutoff time.Time, limit int) (int64, error)
	// PruneHours deletes hour buckets older than cutoff for one project.
	PruneHours(ctx context.Context, projectID int64, cutoff time.Time, limit int) (int64, error)
	// PruneTraces deletes sampled waterfalls older than cutoff for one
	// project. They go on the transaction keep-window, not the aggregates'.
	PruneTraces(ctx context.Context, projectID int64, cutoff time.Time, limit int) (int64, error)
}
