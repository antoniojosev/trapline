package bench

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"
)

// TestTransactionThroughputGate is the ADR 001 volume claim applied to the
// second thing this product ingests.
//
// It measures the same public endpoint the error workloads do, over the same
// real SQLite file, with the same threshold — because a transaction is an
// event by another name as far as the promise goes: an installation that can
// take a hundred errors a second and forty transactions a second has not kept
// the promise, it has kept half of it.
//
// The per-transaction work is genuinely different from an error's, which is
// why it needs its own measurement rather than an extrapolation:
//
//   - Every transaction is a read-modify-write of a latency sketch inside its
//     own transaction. That is one SELECT the error path does not do, plus a
//     decode and an encode of a blob that grows with the endpoint's spread.
//   - One in ten also compresses and stores a waterfall, which is the
//     shipped sampling rate. Measuring at rate 0 would quote a throughput no
//     installation gets; measuring at 1 would quote one nobody asked for.
//   - There is no grouping, no fingerprint, no FTS index and no tag upsert,
//     all of which the error path pays for.
//
// The workload deliberately spreads latencies over three orders of magnitude.
// A sketch of identical values occupies one bucket and encodes to a few dozen
// bytes; a real endpoint occupies a few hundred, and the blob being read and
// written is what this test exists to keep honest.
func TestTransactionThroughputGate(t *testing.T) {
	requireBenchEnabled(t)

	events := envInt(t, "TRAPLINE_BENCH_EVENTS", defaultEvents)
	reps := envInt(t, "TRAPLINE_BENCH_REPS", defaultReps)
	threshold := float64(envInt(t, "TRAPLINE_BENCH_MIN_EPS", gateEventsPerSecond))

	t.Logf("machine: %s", machineDescription())
	t.Logf("workload: %d timed transactions after %d warm-up, %d repetitions; each is %d bytes "+
		"of JSON with %d spans, %d bytes gzipped on the wire",
		events, defaultWarmup, reps,
		len(transactionEnvelopeFor(t, 0)), benchSpansPerTransaction,
		len(gzipBytes(t, transactionEnvelopeFor(t, 0))))

	warmupEnvelopes := buildTransactionEnvelopes(t, 0, defaultWarmup)
	timedEnvelopes := buildTransactionEnvelopes(t, timedOffset, events)

	samples := make([]sample, 0, reps)
	for rep := range reps {
		t.Run(fmt.Sprintf("transaction/rep%d", rep), func(t *testing.T) {
			samples = append(samples, measureHTTP(t, warmupEnvelopes, timedEnvelopes))
		})
	}
	if len(samples) != reps {
		t.Fatalf("only %d of %d repetitions completed", len(samples), reps)
	}

	best, median, worst := summarise(samples)
	t.Logf("%-14s best %7.1f tx/s | median %7.1f | worst %7.1f | spread %4.1f%% | p50 %6s p99 %6s",
		"transaction", best.eventsPerSecond(), median.eventsPerSecond(), worst.eventsPerSecond(),
		spreadPercent(best, worst), round(best.percentile(0.50)), round(best.percentile(0.99)))

	// Asserted on the best repetition, for the reason the error gate gives:
	// contention on a shared runner can only subtract throughput, and a real
	// regression slows every repetition and still trips this.
	if got := best.eventsPerSecond(); got < threshold {
		t.Errorf("transactions: %.1f/s, gate is %.0f/s.\n"+
			"The per-transaction cost is a sketch read-modify-write inside the ingest "+
			"transaction plus, one time in ten, a compressed waterfall. If this fell while "+
			"the error gate held, look there first: a sketch whose encoding grew, a missing "+
			"index on txn_minute, or a sampling rate that stopped being sampled.", got, threshold)
	}
}

// benchSpansPerTransaction is how many children a measured transaction has.
//
// Twelve, matching the stacktrace depth of the error workload, because the
// two numbers are measuring the same thing from opposite ends: how much
// nested structure the decoder and the scrubber walk per item. A transaction
// with no spans would measure the framing and none of the work.
const benchSpansPerTransaction = 12

// buildTransactionEnvelopes prepares count gzipped envelopes ahead of the
// clock, for the reason the error builder gives: compressing is the SDK's
// work, and charging the server for it would understate throughput.
func buildTransactionEnvelopes(tb testing.TB, offset, count int) [][]byte {
	tb.Helper()
	envelopes := make([][]byte, count)
	for index := range envelopes {
		envelopes[index] = gzipBytes(tb, transactionEnvelopeFor(tb, offset+index))
	}
	return envelopes
}

// transactionEnvelopeFor renders one transaction envelope in the protocol's
// framing.
func transactionEnvelopeFor(tb testing.TB, seq int) []byte {
	tb.Helper()
	payload := transactionPayload(tb, seq)

	var envelope bytes.Buffer
	fmt.Fprintf(&envelope, "{\"event_id\":\"%s\",\"sent_at\":\"2026-08-24T10:00:00Z\"}\n", eventID(seq))
	fmt.Fprintf(&envelope, "{\"type\":\"transaction\",\"content_type\":\"application/json\",\"length\":%d}\n",
		len(payload))
	envelope.Write(payload)
	envelope.WriteByte('\n')
	return envelope.Bytes()
}

// benchTransactionNames is the cardinality of the aggregation key.
//
// Twenty routes, which is an ordinary service. It matters: every distinct name
// is a separate row in txn_minute, so a workload with one name would measure a
// single hot row that SQLite keeps in one page, and a workload with thousands
// would measure an index nobody real has.
var benchTransactionNames = []string{
	"GET /api/checkout", "POST /api/checkout", "GET /api/orders", "POST /api/orders",
	"GET /api/orders/{id}", "PATCH /api/orders/{id}", "GET /api/products",
	"GET /api/products/{id}", "POST /api/cart", "DELETE /api/cart/{id}",
	"GET /api/user", "PUT /api/user", "POST /api/login", "POST /api/logout",
	"GET /health", "GET /metrics", "celery.charge_card", "celery.send_receipt",
	"celery.reconcile", "GET /api/search",
}

// transactionPayload builds a transaction of the shape a real SDK sends.
//
// Latencies come from a log-normal draw with a fixed seed: log-normal because
// that is what request latency actually looks like, and fixed because a
// benchmark whose input changed between runs would report a different number
// for a reason nobody could reconstruct.
func transactionPayload(tb testing.TB, seq int) []byte {
	tb.Helper()
	// Seeded from the sequence number so the same index always produces the
	// same payload, whichever order the builder runs in.
	random := rand.New(rand.NewPCG(uint64(seq), 0x7EA))
	durationMS := 5 + rand.New(rand.NewPCG(uint64(seq), 1)).NormFloat64()*40
	if durationMS < 1 {
		durationMS = 1
	}
	// Three orders of magnitude of spread, which is what fills a sketch with
	// the few hundred buckets a real endpoint occupies.
	durationMS *= float64(1 + seq%97)

	start := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC).
		Add(time.Duration(seq%3600) * time.Second)
	end := start.Add(time.Duration(durationMS * float64(time.Millisecond)))

	spans := make([]any, 0, benchSpansPerTransaction)
	cursor := start
	for depth := range benchSpansPerTransaction {
		spanStart := cursor
		cursor = cursor.Add(time.Duration(durationMS/benchSpansPerTransaction) * time.Millisecond / 2)
		spans = append(spans, map[string]any{
			"span_id":         fmt.Sprintf("%016x", seq*100+depth),
			"parent_span_id":  fmt.Sprintf("%016x", seq),
			"trace_id":        fmt.Sprintf("%032x", seq),
			"op":              []string{"db.sql", "http.client", "cache.get", "serialize"}[depth%4],
			"description":     "SELECT id, total, currency FROM orders WHERE customer_id = $1 LIMIT 50",
			"status":          "ok",
			"start_timestamp": float64(spanStart.UnixNano()) / 1e9,
			"timestamp":       float64(cursor.UnixNano()) / 1e9,
			"data": map[string]any{
				"db.system":     "postgresql",
				"db.name":       "billing",
				"rows":          random.IntN(500),
				"authorization": "Bearer sk_live_notarealsecret",
			},
			"tags": map[string]string{"shard": fmt.Sprint(depth % 4)},
		})
	}

	transaction := map[string]any{
		"event_id":         eventID(seq),
		"type":             "transaction",
		"transaction":      benchTransactionNames[seq%len(benchTransactionNames)],
		"transaction_info": map[string]any{"source": "route"},
		"start_timestamp":  float64(start.UnixNano()) / 1e9,
		"timestamp":        float64(end.UnixNano()) / 1e9,
		"platform":         "python",
		"release":          "billing@4.11.2",
		"environment":      "production",
		"server_name":      "web-07",
		"contexts": map[string]any{
			"trace": map[string]any{
				// The trace id decides sampling, so it has to vary per
				// transaction or the whole run would be stored or none of it.
				"trace_id": fmt.Sprintf("%032x", seq),
				"span_id":  fmt.Sprintf("%016x", seq),
				"op":       "http.server",
				"status":   []string{"ok", "ok", "ok", "internal_error"}[seq%4],
			},
			"runtime": map[string]any{"name": "CPython", "version": "3.13.1"},
			"os":      map[string]any{"name": "Linux", "version": "6.6.87"},
		},
		"spans": spans,
		"tags": map[string]string{
			"region": "sa-east-1", "runtime": "CPython 3.13.1", "shard": fmt.Sprint(seq % 4),
		},
	}

	encoded, err := json.Marshal(transaction)
	if err != nil {
		tb.Fatalf("the benchmark's own transaction is not encodable: %v", err)
	}
	return encoded
}
