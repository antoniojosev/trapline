package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/antoniojosev/trapline/internal/adapters/compression"
	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/engine/sketch"
	"github.com/antoniojosev/trapline/internal/ports"
)

var _ ports.TraceRepository = (*TraceRepository)(nil)

// maxTraceSpansBytes bounds one stored waterfall before compression.
//
// A trace can gather transactions from several services, and each one brings
// its own spans. Without a ceiling the blob for a single busy request could
// reach megabytes, and the row would be read whole every time somebody opened
// it. A quarter of a megabyte is several thousand spans, which is already more
// than a waterfall can usefully draw.
const maxTraceSpansBytes = 256 << 10

// emptySpans is what a transaction with no children stores. A valid empty
// array rather than an empty blob, so the read path never has to special-case
// the absence.
var emptySpans = []byte("[]")

// TraceRepository stores the transaction aggregates and the sampled
// waterfalls.
//
// One type for both tables because one call writes both, and because the
// interesting operation — read the window's sketch, merge one value into it,
// write it back — has to happen inside the transaction that also counts the
// transaction. SQLite serialises writers, so that read-modify-write is safe
// with no lock of this product's own; splitting it across two calls is what
// would make it unsafe.
type TraceRepository struct {
	db *DB
}

// NewTraceRepository wires the repository to an open database.
func NewTraceRepository(db *DB) *TraceRepository {
	return &TraceRepository{db: db}
}

// RecordTransaction counts one transaction and, when it was sampled, stores
// its waterfall.
func (r *TraceRepository) RecordTransaction(ctx context.Context, record *ports.TransactionRecord) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := upsertMinute(ctx, tx, record); err != nil {
		return err
	}
	if record.Trace != nil {
		if err := upsertTrace(ctx, tx, record); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}
	return nil
}

// upsertMinute folds one duration into its minute bucket.
func upsertMinute(ctx context.Context, tx *sql.Tx, record *ports.TransactionRecord) error {
	bucket := domain.MinuteBucket(record.At)

	var stored []byte
	err := tx.QueryRowContext(ctx,
		"SELECT sketch FROM txn_minute WHERE project_id = ? AND txn_name = ? AND minute = ?",
		record.ProjectID, record.Name, bucket).Scan(&stored)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading the minute bucket: %w", err)
	}

	var histogram sketch.Sketch
	if len(stored) > 0 {
		if err := histogram.UnmarshalBinary(stored); err != nil {
			// A corrupt sketch must not cost the transaction. Starting a new
			// histogram loses the window's history and keeps the count
			// honest, which is the better of two bad outcomes: the
			// alternative is refusing every transaction of a busy endpoint
			// until somebody notices one bad row.
			histogram = sketch.Sketch{}
		}
	}
	if err := histogram.Add(record.DurationMS); err != nil {
		return fmt.Errorf("recording the latency: %w", err)
	}
	encoded, err := histogram.MarshalBinary()
	if err != nil {
		return fmt.Errorf("encoding the latency sketch: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO txn_minute (project_id, txn_name, minute, count, failed, sampled, sketch)
		VALUES (?, ?, ?, 1, ?, ?, ?)
		ON CONFLICT (project_id, txn_name, minute) DO UPDATE SET
			count   = count + 1,
			failed  = failed + excluded.failed,
			sampled = sampled + excluded.sampled,
			sketch  = excluded.sketch`,
		record.ProjectID, record.Name, bucket,
		boolToInt(record.Failed), boolToInt(record.Trace != nil), encoded)
	if err != nil {
		return fmt.Errorf("writing the minute bucket: %w", err)
	}
	return nil
}

// upsertTrace stores or extends one waterfall.
//
// A trace is not one transaction. A request that crosses three services
// produces three, each with its own spans, and they arrive separately and out
// of order — so the row is written by whichever gets here first and extended
// by the others. The header columns describe the earliest-starting
// transaction, because that is the root of the waterfall and what a listing
// should show; the spans accumulate up to a budget.
func upsertTrace(ctx context.Context, tx *sql.Tx, record *ports.TransactionRecord) error {
	trace := record.Trace
	spans := trace.Spans
	if len(spans) == 0 {
		spans = emptySpans
	}

	var (
		existingAt    string
		existingSpans []byte
	)
	err := tx.QueryRowContext(ctx,
		"SELECT timestamp, spans FROM traces WHERE trace_id = ? AND project_id = ?",
		trace.TraceID, record.ProjectID).Scan(&existingAt, &existingSpans)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		compressed := compression.Compress(spans)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO traces (trace_id, project_id, txn_name, timestamp, duration_ms, status, op, spans)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			trace.TraceID, record.ProjectID, record.Name, formatTime(record.At),
			int64(record.DurationMS), trace.Status, trace.Op, compressed); err != nil {
			return fmt.Errorf("storing the trace: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("reading the trace: %w", err)
	}

	decompressed, err := compression.Decompress(existingSpans)
	if err != nil {
		// The stored waterfall cannot be read. Replacing it with this
		// transaction's spans is better than refusing the write: the
		// aggregate is already correct, and a partial waterfall beats a row
		// nobody can open.
		decompressed = emptySpans
	}
	merged := appendSpans(decompressed, spans)

	// Whether this transaction is the new root: it started earlier than
	// whatever is on the row. Compared as text, which is exactly the
	// comparison the storage layout was built for (ADR 033).
	at := formatTime(record.At)
	if at < existingAt {
		_, err = tx.ExecContext(ctx, `
			UPDATE traces
			SET txn_name = ?, timestamp = ?, duration_ms = ?, status = ?, op = ?, spans = ?
			WHERE trace_id = ? AND project_id = ?`,
			record.Name, at, int64(record.DurationMS), trace.Status, trace.Op,
			compression.Compress(merged), trace.TraceID, record.ProjectID)
	} else {
		_, err = tx.ExecContext(ctx,
			"UPDATE traces SET spans = ? WHERE trace_id = ? AND project_id = ?",
			compression.Compress(merged), trace.TraceID, record.ProjectID)
	}
	if err != nil {
		return fmt.Errorf("extending the trace: %w", err)
	}
	return nil
}

// appendSpans concatenates two JSON arrays, bounded.
//
// Textual rather than through encoding/json, because both sides were produced
// by this process one step earlier and re-parsing a quarter of a megabyte to
// append to it is work with no reader. The budget is checked before the join,
// so a trace that has already reached it simply stops growing rather than
// growing by one more span each time.
func appendSpans(existing, incoming []byte) []byte {
	switch {
	case len(existing) < 2 || existing[0] != '[':
		return incoming
	case len(incoming) < 2 || incoming[0] != '[':
		return existing
	case len(incoming) == 2: // "[]": nothing to add
		return existing
	case len(existing) == 2:
		return incoming
	case len(existing)+len(incoming) > maxTraceSpansBytes:
		return existing
	}

	merged := make([]byte, 0, len(existing)+len(incoming))
	merged = append(merged, existing[:len(existing)-1]...)
	merged = append(merged, ',')
	merged = append(merged, incoming[1:]...)
	return merged
}

// Windows reads the buckets of a range at one resolution.
//
// The name filter is applied in SQL rather than by the caller, because
// "everything in this project" and "this one transaction" are the two
// questions and the second must not read the first. Both are range scans of an
// index built for them; neither touches a span (ADR 001).
func (r *TraceRepository) Windows(
	ctx context.Context, projectID int64, name string,
	resolution domain.Resolution, from, to string,
) ([]ports.TransactionWindow, error) {
	table, column := tableFor(resolution)

	query := "SELECT txn_name, " + column + ", count, failed, sampled, sketch FROM " + table +
		" WHERE project_id = ? AND " + column + " >= ? AND " + column + " <= ?"
	args := []any{projectID, from, to}
	if name != "" {
		query += " AND txn_name = ?"
		args = append(args, name)
	}
	query += " ORDER BY txn_name, " + column

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading the transaction windows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var windows []ports.TransactionWindow
	for rows.Next() {
		var window ports.TransactionWindow
		if err := rows.Scan(&window.Name, &window.Bucket, &window.Count,
			&window.Failed, &window.Sampled, &window.Sketch); err != nil {
			return nil, fmt.Errorf("scanning a transaction window: %w", err)
		}
		windows = append(windows, window)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating the transaction windows: %w", err)
	}
	return windows, nil
}

// tableFor maps a resolution onto its table and bucket column.
//
// The two names are returned together and interpolated into the query as a
// pair, so there is exactly one place where a resolution becomes SQL. They are
// constants chosen by a closed enum and never by caller input — the only shape
// of interpolation that is not an injection.
func tableFor(resolution domain.Resolution) (table, column string) {
	if resolution == domain.ResolutionHour {
		return "txn_hour", "hour"
	}
	return "txn_minute", "minute"
}

// Trace reads one waterfall.
func (r *TraceRepository) Trace(
	ctx context.Context, projectID int64, traceID string,
) (ports.StoredTrace, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT trace_id, project_id, txn_name, timestamp, duration_ms, status, op, spans
		FROM traces WHERE trace_id = ? AND project_id = ?`, traceID, projectID)

	trace, err := scanTrace(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ports.StoredTrace{}, fmt.Errorf("%w: %s", domain.ErrTraceNotFound, traceID)
	}
	if err != nil {
		return ports.StoredTrace{}, fmt.Errorf("reading the trace: %w", err)
	}
	return trace, nil
}

// TracesFor lists one transaction's stored examples, slowest first.
func (r *TraceRepository) TracesFor(
	ctx context.Context, projectID int64, name string, limit int,
) ([]ports.StoredTrace, error) {
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT trace_id, project_id, txn_name, timestamp, duration_ms, status, op, spans
		FROM traces
		WHERE project_id = ? AND txn_name = ?
		ORDER BY duration_ms DESC, timestamp DESC
		LIMIT ?`, projectID, name, limit)
	if err != nil {
		return nil, fmt.Errorf("listing traces: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var traces []ports.StoredTrace
	for rows.Next() {
		trace, err := scanTrace(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning a trace: %w", err)
		}
		traces = append(traces, trace)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating traces: %w", err)
	}
	return traces, nil
}

func scanTrace(row scanner) (ports.StoredTrace, error) {
	var (
		trace     ports.StoredTrace
		timestamp string
		spans     []byte
	)
	if err := row.Scan(&trace.TraceID, &trace.ProjectID, &trace.Name, &timestamp,
		&trace.DurationMS, &trace.Status, &trace.Op, &spans); err != nil {
		return ports.StoredTrace{}, err
	}

	at, err := parseTime(timestamp)
	if err != nil {
		return ports.StoredTrace{}, err
	}
	trace.At = at

	decompressed, err := compression.Decompress(spans)
	if err != nil {
		// The header is still readable and still worth returning: a trace
		// whose spans cannot be decompressed is a broken waterfall, not a
		// missing transaction, and answering 404 would send somebody looking
		// for a trace id that is right there.
		decompressed = emptySpans
	}
	trace.Spans = decompressed
	return trace, nil
}

// HasTracing reports whether any project accepts transactions.
//
// It reads the configuration rather than the tables, and the difference
// matters: a project that has just been switched on has no rows yet, and a job
// that waited for the first transaction before existing would never fold the
// minute that transaction landed in.
func (r *TraceRepository) HasTracing(ctx context.Context) (bool, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT config FROM projects")
	if err != nil {
		return false, fmt.Errorf("reading project configuration: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var raw sql.NullString
		if err := rows.Scan(&raw); err != nil {
			return false, fmt.Errorf("scanning project configuration: %w", err)
		}
		if domain.DecodeProjectConfig(raw.String).CategoryEnabled(string(categoryTransaction)) {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterating project configuration: %w", err)
	}
	return false, nil
}

// categoryTransaction is the engine's category name, spelled here so this file
// does not import the engine for one string. The architecture test does not
// forbid it; the ingest path already resolves the category, and a storage
// adapter that reached for the engine to answer a configuration question would
// be the beginning of one that reached for it to answer a storage one.
const categoryTransaction = "transaction"

// FoldMinutes merges closed minutes into their hours and deletes them.
//
// This is the operation ADR 020 exists for. Folding sixty sketches into one is
// adding counts, so the hour is exactly the sketch the same transactions would
// have produced had they been recorded into it directly — which is what makes
// "the same percentiles after downsampling" a property rather than a hope.
func (r *TraceRepository) FoldMinutes(
	ctx context.Context, cutoff time.Time, limit int,
) (int64, error) {
	if limit <= 0 {
		limit = 500
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("beginning the fold: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	minutes, err := closedMinutes(ctx, tx, domain.MinuteBucket(cutoff), limit)
	if err != nil {
		return 0, err
	}
	if len(minutes) == 0 {
		return 0, nil
	}

	for index := range minutes {
		if err := foldOne(ctx, tx, &minutes[index]); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing the fold: %w", err)
	}
	return int64(len(minutes)), nil
}

// minuteRow is one bucket about to be folded.
type minuteRow struct {
	projectID int64
	name      string
	minute    string
	count     int64
	failed    int64
	sampled   int64
	sketch    []byte
}

func closedMinutes(ctx context.Context, tx *sql.Tx, cutoff string, limit int) ([]minuteRow, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT project_id, txn_name, minute, count, failed, sampled, sketch
		FROM txn_minute
		WHERE minute < ?
		ORDER BY minute
		LIMIT ?`, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("reading the closed minutes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var minutes []minuteRow
	for rows.Next() {
		var minute minuteRow
		if err := rows.Scan(&minute.projectID, &minute.name, &minute.minute,
			&minute.count, &minute.failed, &minute.sampled, &minute.sketch); err != nil {
			return nil, fmt.Errorf("scanning a closed minute: %w", err)
		}
		minutes = append(minutes, minute)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating the closed minutes: %w", err)
	}
	return minutes, nil
}

// foldOne merges one minute into its hour and deletes the minute.
func foldOne(ctx context.Context, tx *sql.Tx, minute *minuteRow) error {
	at, err := domain.ParseMinuteBucket(minute.minute)
	if err != nil {
		// A bucket that is not a bucket cannot be folded into anything. It is
		// deleted rather than left behind, or every later pass would find it
		// again and make no progress — a sweep that cannot terminate is worse
		// than one row of lost history.
		return deleteMinute(ctx, tx, minute)
	}
	hour := domain.HourBucket(at)

	var stored []byte
	err = tx.QueryRowContext(ctx,
		"SELECT sketch FROM txn_hour WHERE project_id = ? AND txn_name = ? AND hour = ?",
		minute.projectID, minute.name, hour).Scan(&stored)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading the hour bucket: %w", err)
	}

	var merged sketch.Sketch
	if len(stored) > 0 {
		if err := merged.UnmarshalBinary(stored); err != nil {
			merged = sketch.Sketch{}
		}
	}
	var incoming sketch.Sketch
	if err := incoming.UnmarshalBinary(minute.sketch); err == nil {
		merged.Merge(&incoming)
	}
	encoded, err := merged.MarshalBinary()
	if err != nil {
		return fmt.Errorf("encoding the folded sketch: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO txn_hour (project_id, txn_name, hour, count, failed, sampled, sketch)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (project_id, txn_name, hour) DO UPDATE SET
			count   = count + excluded.count,
			failed  = failed + excluded.failed,
			sampled = sampled + excluded.sampled,
			sketch  = excluded.sketch`,
		minute.projectID, minute.name, hour,
		minute.count, minute.failed, minute.sampled, encoded); err != nil {
		return fmt.Errorf("writing the hour bucket: %w", err)
	}
	return deleteMinute(ctx, tx, minute)
}

func deleteMinute(ctx context.Context, tx *sql.Tx, minute *minuteRow) error {
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM txn_minute WHERE project_id = ? AND txn_name = ? AND minute = ?",
		minute.projectID, minute.name, minute.minute); err != nil {
		return fmt.Errorf("deleting a folded minute: %w", err)
	}
	return nil
}

// PruneMinutes deletes minute buckets past their keep-window.
func (r *TraceRepository) PruneMinutes(
	ctx context.Context, cutoff time.Time, limit int,
) (int64, error) {
	return r.deleteBatch(ctx, `
		DELETE FROM txn_minute
		WHERE rowid IN (SELECT rowid FROM txn_minute WHERE minute < ? LIMIT ?)`,
		domain.MinuteBucket(cutoff), batchSize(limit))
}

// PruneHours deletes one project's hour buckets past their keep-window.
func (r *TraceRepository) PruneHours(
	ctx context.Context, projectID int64, cutoff time.Time, limit int,
) (int64, error) {
	return r.deleteBatch(ctx, `
		DELETE FROM txn_hour
		WHERE rowid IN (
			SELECT rowid FROM txn_hour WHERE project_id = ? AND hour < ? LIMIT ?
		)`, projectID, domain.HourBucket(cutoff), batchSize(limit))
}

// PruneTraces deletes one project's sampled waterfalls past their keep-window.
func (r *TraceRepository) PruneTraces(
	ctx context.Context, projectID int64, cutoff time.Time, limit int,
) (int64, error) {
	return r.deleteBatch(ctx, `
		DELETE FROM traces
		WHERE rowid IN (
			SELECT rowid FROM traces WHERE project_id = ? AND timestamp < ? LIMIT ?
		)`, projectID, formatTime(cutoff), batchSize(limit))
}

// batchSize is the shared default for a sweep's batch: large enough that a
// sweep makes progress, small enough that the write transaction it holds is
// measured in milliseconds.
func batchSize(limit int) int {
	if limit <= 0 {
		return 1000
	}
	return limit
}

// deleteBatch runs one bounded delete, the way every sweep in this product
// does: never one long write transaction on a store whose single writer is
// shared with ingestion.
func (r *TraceRepository) deleteBatch(ctx context.Context, query string, args ...any) (int64, error) {
	result, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("deleting expired transaction data: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("reading delete result: %w", err)
	}
	return deleted, nil
}
