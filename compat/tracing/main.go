// Command compat-tracing sends transactions through the official Go SDK with
// latencies this program chose, and prints the exact percentiles of what it
// sent.
//
// It exists because the tracing gate has to compare two numbers that come from
// different places: the percentile the server computed from its merged
// sketches, and the percentile of the sample as it really was. Computing the
// second here — on the sending side, from the durations before they were
// serialised — is what makes the comparison mean anything. A gate that asked
// the server for both numbers would only be checking that the server agrees
// with itself.
//
// The SDK is the real one, unmodified, configured with a DSN and
// `EnableTracing`. That is the compatibility claim (ADR 002): what is under
// test is the shape sentry-go puts on the wire, which a hand-written envelope
// cannot check.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"sort"
	"time"

	"github.com/getsentry/sentry-go"
)

func main() {
	dsn := flag.String("dsn", "", "DSN of a project on the server under test")
	name := flag.String("name", "GET /api/checkout", "transaction name to send under")
	count := flag.Int("count", 500, "how many transactions to send")
	seed := flag.Uint64("seed", 0x5EED, "seed for the latency distribution")
	failEvery := flag.Int("fail-every", 10, "make every Nth transaction fail; 0 for none")
	spread := flag.Duration("spread", 8*time.Minute, "how far back to spread the transactions")
	offset := flag.Duration("offset", 3*time.Minute, "how far in the past the newest transaction is")
	flag.Parse()

	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "usage: compat-tracing -dsn ... [-name N] [-count N]")
		os.Exit(2)
	}

	summary, err := run(*dsn, *name, *count, *seed, *failEvery, *spread, *offset)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL %v\n", err)
		os.Exit(1)
	}

	// The gate reads this from stdout. One line of compact JSON, so a shell
	// can pull a field out of it with sed and nothing else has to be
	// installed.
	encoded, err := json.Marshal(summary)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL encoding the summary: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(encoded))
}

// Summary is what was sent, as the sender knows it.
type Summary struct {
	Transaction string  `json:"transaction"`
	Count       int     `json:"count"`
	Failed      int     `json:"failed"`
	P50         float64 `json:"p50_ms"`
	P95         float64 `json:"p95_ms"`
	P99         float64 `json:"p99_ms"`
	Min         float64 `json:"min_ms"`
	Max         float64 `json:"max_ms"`
	// Traces is every trace id sent, so the gate can check that the server
	// stored a subset of them and not something it made up.
	Traces []string `json:"traces"`
}

func run(
	dsn, name string, count int, seed uint64, failEvery int, spread, offset time.Duration,
) (Summary, error) {
	// The whole configuration. If anything else were needed here, the
	// compatibility claim would be false.
	if err := sentry.Init(sentry.ClientOptions{
		Dsn:              dsn,
		Release:          "compat@1.0.0",
		Environment:      "compat-test",
		EnableTracing:    true,
		TracesSampleRate: 1.0,
	}); err != nil {
		return Summary{}, fmt.Errorf("initialising the SDK: %w", err)
	}
	defer sentry.Flush(30 * time.Second)

	durations := latencies(count, seed)

	// The timestamps are chosen rather than taken from the clock, and they sit
	// a few minutes in the past on purpose: the downsampling job only folds
	// minutes that have closed, so a gate whose transactions all landed in the
	// current minute could never exercise it.
	newest := time.Now().UTC().Add(-offset)
	step := spread / time.Duration(max(count, 1))

	summary := Summary{Transaction: name, Count: count, Traces: make([]string, 0, count)}
	for index := range count {
		start := newest.Add(-spread + time.Duration(index)*step)
		duration := time.Duration(durations[index] * float64(time.Millisecond))

		span := sentry.StartSpan(context.Background(), "http.server",
			sentry.WithTransactionName(name),
			withTimes(start, start.Add(duration)))
		span.SetTag("suite", "compat-tracing")

		// A child, because a transaction with no spans is not a waterfall and
		// the gate checks that one comes back with its children.
		child := span.StartChild("db.sql",
			withTimes(start, start.Add(duration/2)))
		child.Description = "SELECT id, total FROM orders WHERE customer_id = $1"
		child.Status = sentry.SpanStatusOK
		child.Finish()

		span.Status = sentry.SpanStatusOK
		if failEvery > 0 && index%failEvery == 0 {
			span.Status = sentry.SpanStatusInternalError
			summary.Failed++
		}
		summary.Traces = append(summary.Traces, span.TraceID.String())
		span.Finish()
	}

	if !sentry.Flush(30 * time.Second) {
		return Summary{}, fmt.Errorf("the SDK could not flush %d transactions within thirty seconds", count)
	}

	sorted := append([]float64(nil), durations...)
	sort.Float64s(sorted)
	summary.P50 = quantile(sorted, 0.50)
	summary.P95 = quantile(sorted, 0.95)
	summary.P99 = quantile(sorted, 0.99)
	summary.Min = sorted[0]
	summary.Max = sorted[len(sorted)-1]
	return summary, nil
}

// withTimes pins a span's two ends.
//
// The SDK measures elapsed time by default, which would mean sending five
// hundred transactions of a known distribution took as long as the
// distribution says — minutes, for a p99 worth measuring. Both fields are
// exported and Finish only fills EndTime when it is zero, so this is the
// SDK's own supported way of reporting work that happened earlier: exactly
// what a queue worker or a mobile client replaying an offline session does.
func withTimes(start, end time.Time) sentry.SpanOption {
	return func(s *sentry.Span) {
		s.StartTime = start
		s.EndTime = end
	}
}

// latencies draws a log-normal sample, which is what request latency actually
// looks like: a tight body and a long tail, so the p50 and the p95 are far
// apart and a broken estimator cannot accidentally land on both.
//
// Seeded, because the gate asserts on the numbers this produces and a sample
// that changed between runs would make a failure impossible to reproduce.
func latencies(count int, seed uint64) []float64 {
	random := rand.New(rand.NewPCG(seed, 0xF5A))
	values := make([]float64, count)
	for index := range values {
		// median about 20 ms, p95 about 100 ms, a tail past half a second.
		values[index] = math.Exp(random.NormFloat64()*0.9 + 3.0)
	}
	return values
}

// quantile is nearest-rank on a sorted sample: the element at
// floor(q·(n−1)), zero-based.
//
// The same convention internal/engine/sketch documents, and it is stated in
// both places on purpose. A gate that used a different one would be measuring
// the disagreement between two definitions rather than the error of the
// estimator, and "off by one element" and "the sketch is broken" look
// identical in a failure message.
func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(q * float64(len(sorted)-1))
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}
