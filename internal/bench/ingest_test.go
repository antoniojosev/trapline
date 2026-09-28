package bench

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/adapters/compression"
	"github.com/antoniojosev/trapline/internal/envelope"
	"github.com/antoniojosev/trapline/internal/usecase"
)

// benchMaxBody mirrors the handler's maxIngestBody, which is unexported. The
// value matters: it is the decompression budget, and measuring with a
// different one would measure a different guard than the product ships.
const benchMaxBody = 20 << 20

// benchUserAgent is an ordinary browser's header, so the measurement includes
// the work an event from a browser actually causes on this path.
const benchUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

// The gate's defaults.
//
// 2000 events is long enough that per-repetition setup is noise and that
// SQLite has grown past its first few pages, and short enough that the whole
// job stays under a minute on a CI runner. Five repetitions is the smallest
// number from which "best of" means anything.
const (
	defaultEvents = 2000
	defaultReps   = 5
	// Warm-up events are ingested and thrown away so the timed run does not
	// pay for the first transaction, the driver's lazily built statements or
	// the zstd encoder's one-time construction.
	defaultWarmup = 200
	// timedOffset separates the timed events' sequence numbers from the
	// warm-up's, so that in the new-issue workload a warm-up event can never
	// pre-create an issue a timed event was supposed to create.
	timedOffset = 1 << 20
)

// gateEventsPerSecond is the threshold this gate enforces, per workload.
//
// # What was measured
//
// Reference machine: Intel Core i7-14650HX, 24 logical cores, linux/amd64 under
// WSL2, go1.26.6, CGO_ENABLED=0 — and deliberately not an idle machine, since a
// number taken on a quiet desktop flatters itself. Four full runs, each the
// best of five repetitions of 2000 timed events, end to end over HTTP:
//
//	                run 1  run 2  run 3  run 4   worst best-of-five
//	new issue        730    548    670    671    548 events/s
//	existing issue   705    717    747    711    705 events/s
//
// Both are several times the published target, so ADR 001 holds comfortably and
// ADR 009's pure-Go driver carries it. Within a single run the spread between
// repetitions reached 61% on the new-issue workload while the existing-issue
// one stayed at 3-19%. That asymmetry is real — creating an issue grows the
// issues table and its unique index on every event — and it is the reason the
// gate asserts on the best repetition rather than the mean.
//
// # Why 150 and not 700
//
// ADR 001 publishes a target of "<100 eventos/s sostenidos". The threshold has
// to satisfy two constraints that pull in opposite directions:
//
//   - Above the claim, or a green gate proves nothing. 150 is 1.5x the
//     published target, so passing says the ADR 001 figure holds with half
//     again in hand.
//   - Far below what the slowest plausible runner manages. A shared CI runner
//     is commonly 2-3x slower per core than this desktop, which puts its
//     best-of-five somewhere around 200-350 events/s. At 150 the gate still
//     has 1.5-2x of headroom there, on top of asserting on the best
//     repetition, which interference can only push down and never up.
//
// Setting it anywhere near the 700 actually measured would produce a gate that
// goes red on a busy afternoon, and a gate that cries wolf gets commented out —
// taking with it the reason anyone would ever look at these numbers again.
//
// # What it does and does not catch
//
// It catches a change of kind: a lost index, an fsync per statement, a
// transaction per statement, a driver several times slower. It will not catch
// a 20% drift, and it is not meant to — a threshold tight enough to see drift
// on a shared runner is a threshold that flakes. Drift is visible in the layer
// table this test prints on every run, and in `go test -bench`.
//
// If a real runner turns out tighter than estimated, the honest fix is to
// measure that runner and record its numbers here, not to guess a second time.
// TRAPLINE_BENCH_MIN_EPS exists so that can be done without a code change
// while the measurement is being taken.
const gateEventsPerSecond = 150

// sample is one repetition: how many events went through and how long each took.
type sample struct {
	elapsed   time.Duration
	latencies []time.Duration
}

func (s sample) eventsPerSecond() float64 {
	if s.elapsed <= 0 {
		return 0
	}
	return float64(len(s.latencies)) / s.elapsed.Seconds()
}

// percentile reports the latency at a quantile.
//
// Nearest-rank on a sorted copy rather than an interpolating estimator: the
// sample is complete and in memory, so there is nothing to estimate, and ADR
// 007's rule about never persisting percentiles is about aggregates over time,
// not about a benchmark holding its own measurements.
func (s sample) percentile(quantile float64) time.Duration {
	if len(s.latencies) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(s.latencies))
	copy(sorted, s.latencies)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a] < sorted[b] })
	rank := int(math.Ceil(quantile*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

// TestIngestThroughputGate is the verifiable half of ADR 001's promise and the
// judge ADR 009 named.
//
// It fails when sustained end-to-end ingest throughput falls below
// gateEventsPerSecond on either workload. Failing is the useful outcome: it
// means either the ingest path regressed or the pure-Go SQLite driver stopped
// being able to carry the declared volume, and ADR 009 says explicitly that
// the second reopens the driver decision with data.
func TestIngestThroughputGate(t *testing.T) {
	requireBenchEnabled(t)

	events := envInt(t, "TRAPLINE_BENCH_EVENTS", defaultEvents)
	reps := envInt(t, "TRAPLINE_BENCH_REPS", defaultReps)
	threshold := float64(envInt(t, "TRAPLINE_BENCH_MIN_EPS", gateEventsPerSecond))

	t.Logf("machine: %s", machineDescription())
	t.Logf("workload: %d timed events after %d warm-up, %d repetitions; each event is %d bytes "+
		"of JSON, %d bytes gzipped on the wire",
		events, defaultWarmup, reps,
		len(envelopeFor(t, existingIssue, 0)), len(gzipBytes(t, envelopeFor(t, existingIssue, 0))))

	for _, kind := range []workload{newIssue, existingIssue} {
		warmupEnvelopes := buildEnvelopes(t, kind, 0, defaultWarmup)
		timedEnvelopes := buildEnvelopes(t, kind, timedOffset, events)

		samples := make([]sample, 0, reps)
		for rep := range reps {
			t.Run(fmt.Sprintf("%s/rep%d", kind, rep), func(t *testing.T) {
				samples = append(samples, measureHTTP(t, warmupEnvelopes, timedEnvelopes))
			})
		}
		if len(samples) != reps {
			t.Fatalf("%s: only %d of %d repetitions completed", kind, len(samples), reps)
		}

		best, median, worst := summarise(samples)
		t.Logf("%-14s best %7.1f ev/s | median %7.1f | worst %7.1f | spread %4.1f%% | p50 %6s p99 %6s",
			kind, best.eventsPerSecond(), median.eventsPerSecond(), worst.eventsPerSecond(),
			spreadPercent(best, worst), round(best.percentile(0.50)), round(best.percentile(0.99)))

		// Asserted on the best repetition. Contention on a shared runner can
		// only ever subtract throughput, so the best repetition is the closest
		// available estimate of the machine's real capability; a genuine
		// regression slows every repetition and still trips this.
		if got := best.eventsPerSecond(); got < threshold {
			t.Errorf("%s: %.1f events/s, gate is %.0f events/s.\n"+
				"ADR 001 publishes a target of <100 events/s sustained and ADR 009 makes this gate "+
				"the judge of the pure-Go SQLite driver. Either the ingest path regressed or the "+
				"driver no longer carries the declared volume; the second reopens ADR 009.\n"+
				"Check the layer breakdown below to see which step grew.", kind, got, threshold)
		}
	}

	reportLayers(t, events)
	reportConcurrency(t, events)
}

// measureHTTP times one repetition of the full path over a real socket.
func measureHTTP(tb testing.TB, warmupEnvelopes, timedEnvelopes [][]byte) sample {
	tb.Helper()
	stack := newStack(tb)
	ctx := context.Background()

	for _, body := range warmupEnvelopes {
		if err := stack.post(ctx, body); err != nil {
			tb.Fatalf("warming up: %v", err)
		}
	}

	latencies := make([]time.Duration, len(timedEnvelopes))
	start := time.Now()
	for index, body := range timedEnvelopes {
		sent := time.Now()
		if err := stack.post(ctx, body); err != nil {
			tb.Fatalf("event %d: %v", index, err)
		}
		latencies[index] = time.Since(sent)
	}
	return sample{elapsed: time.Since(start), latencies: latencies}
}

// reportLayers attributes the per-event cost to the step that spends it.
//
// Five nested measurements over the identical workload, each adding one stage.
// The differences are the answer to "where does the time go", which is what
// makes a failing gate actionable instead of merely alarming.
func reportLayers(tb testing.TB, events int) {
	tb.Helper()

	// The steady-state workload: an error that already has an issue, which is
	// what a server in trouble is actually receiving.
	warmupEnvelopes := buildEnvelopes(tb, existingIssue, 0, defaultWarmup)
	timedEnvelopes := buildEnvelopes(tb, existingIssue, timedOffset, events)

	type stage struct {
		name string
		run  func(testing.TB, [][]byte, [][]byte) sample
	}
	stages := []stage{
		{"gunzip", measureGunzip},
		{"+ envelope parse", measureParse},
		{"+ decode/scrub/group", measureDomain},
		{"+ sqlite write", measureStore},
		{"+ http, auth, limiter", measureHTTP},
	}

	tb.Log("where the time goes (existing-issue workload, best of 3, microseconds per event):")
	previous := 0.0
	for _, current := range stages {
		best := bestOf(3, func() sample { return current.run(tb, warmupEnvelopes, timedEnvelopes) })
		cumulative := float64(best.elapsed.Microseconds()) / float64(len(timedEnvelopes))
		delta := cumulative - previous
		// The layers are separate measurements taken minutes apart, so on a
		// loaded machine a step can come out cheaper than the one it contains.
		// That is impossible, and saying so is better than printing a negative
		// cost as if it meant something.
		note := ""
		if delta < 0 {
			note = "  <- noise: this layer measured faster than the one nested inside it"
		}
		tb.Logf("  %-22s %8.1f µs cumulative  %+8.1f µs this step  (%7.1f ev/s)%s",
			current.name, cumulative, delta, best.eventsPerSecond(), note)
		previous = cumulative
	}
}

// measureGunzip is the first layer: decompress the body and throw it away.
func measureGunzip(tb testing.TB, _, timed [][]byte) sample {
	tb.Helper()
	return timeEach(timed, func(body []byte) error {
		reader, err := compression.DecompressRequest(bytes.NewReader(body), "gzip", benchMaxBody)
		if err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, reader); err != nil {
			return err
		}
		if closer, ok := reader.(io.Closer); ok {
			return closer.Close()
		}
		return nil
	})
}

// measureParse adds the envelope framing.
func measureParse(tb testing.TB, _, timed [][]byte) sample {
	tb.Helper()
	return timeEach(timed, func(body []byte) error {
		reader, err := compression.DecompressRequest(bytes.NewReader(body), "gzip", benchMaxBody)
		if err != nil {
			return err
		}
		_, err = envelope.Parse(reader, envelope.Limits{MaxEnvelopeBytes: benchMaxBody})
		return err
	})
}

// measureDomain adds decoding, scrubbing, re-encoding and fingerprinting, with
// a storage port that discards. What is left out is exactly one thing: the
// database.
func measureDomain(tb testing.TB, _, timed [][]byte) sample {
	tb.Helper()
	ingest := usecase.NewIngest(&discardIssues{}, alwaysAllow{}, benchScrubber(), benchClock{}, false)
	return timeIngest(tb, ingest, 1, timed)
}

// measureStore adds the real SQLite transaction. The difference between this
// and measureDomain is the driver's share of the ingest path — the figure ADR
// 009 asks for.
func measureStore(tb testing.TB, warmup, timed [][]byte) sample {
	tb.Helper()
	stack := newStack(tb)
	if _, err := runIngest(stack.ingestUnlimited, stack.projectID, warmup); err != nil {
		tb.Fatalf("warming up storage: %v", err)
	}
	return timeIngest(tb, stack.ingestUnlimited, stack.projectID, timed)
}

// reportConcurrency shows what happens when several SDKs report at once.
//
// Informational, never asserted on. SQLite here is a single writer by design
// (ADR 001) and the pool is pinned to one connection, so this says whether
// concurrency buys anything or merely converts waiting into queueing — useful
// context for reading the sequential number, but far too sensitive to a
// runner's core count to gate on.
func reportConcurrency(tb testing.TB, events int) {
	tb.Helper()
	const workers = 8

	warmupEnvelopes := buildEnvelopes(tb, existingIssue, 0, defaultWarmup)
	timedEnvelopes := buildEnvelopes(tb, existingIssue, timedOffset, events)

	stack := newStack(tb)
	ctx := context.Background()
	for _, body := range warmupEnvelopes {
		if err := stack.post(ctx, body); err != nil {
			tb.Fatalf("warming up: %v", err)
		}
	}

	queue := make(chan []byte, len(timedEnvelopes))
	for _, body := range timedEnvelopes {
		queue <- body
	}
	close(queue)

	var group sync.WaitGroup
	failures := make(chan error, workers)
	start := time.Now()
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for body := range queue {
				if err := stack.post(ctx, body); err != nil {
					failures <- err
					return
				}
			}
		}()
	}
	group.Wait()
	elapsed := time.Since(start)
	close(failures)
	if err := <-failures; err != nil {
		tb.Fatalf("concurrent ingest: %v", err)
	}

	tb.Logf("concurrency: %d clients sustain %.1f ev/s (single client is the gated figure)",
		workers, float64(len(timedEnvelopes))/elapsed.Seconds())
}

// timeEach runs step once per envelope and records the per-event latency.
func timeEach(bodies [][]byte, step func([]byte) error) sample {
	latencies := make([]time.Duration, len(bodies))
	start := time.Now()
	for index, body := range bodies {
		began := time.Now()
		if err := step(body); err != nil {
			panic("bench: a prepared envelope failed its own layer: " + err.Error())
		}
		latencies[index] = time.Since(began)
	}
	return sample{elapsed: time.Since(start), latencies: latencies}
}

func timeIngest(tb testing.TB, ingest *usecase.Ingest, projectID int64, bodies [][]byte) sample {
	tb.Helper()
	result, err := runIngest(ingest, projectID, bodies)
	if err != nil {
		tb.Fatalf("ingesting: %v", err)
	}
	return result
}

func runIngest(ingest *usecase.Ingest, projectID int64, bodies [][]byte) (sample, error) {
	ctx := context.Background()
	limits := envelope.Limits{MaxEnvelopeBytes: benchMaxBody}

	latencies := make([]time.Duration, len(bodies))
	start := time.Now()
	for index, body := range bodies {
		began := time.Now()
		reader, err := compression.DecompressRequest(bytes.NewReader(body), "gzip", benchMaxBody)
		if err != nil {
			return sample{}, err
		}
		// A User-Agent, because a real request carries one and the ingest
		// path reads it: measuring without it would report the throughput of
		// a request nobody sends.
		result, err := ingest.Process(ctx, projectID, reader, limits, usecase.ClientInfo{
			UserAgent: benchUserAgent,
		})
		if err != nil {
			return sample{}, err
		}
		if result.Accepted != 1 {
			// A silently dropped event would make the benchmark report the
			// throughput of doing nothing, which is the classic way a
			// performance gate turns green and stops meaning anything.
			return sample{}, fmt.Errorf("event %d: accepted %d, dropped %v", index, result.Accepted, result.Dropped)
		}
		latencies[index] = time.Since(began)
	}
	return sample{elapsed: time.Since(start), latencies: latencies}, nil
}

func bestOf(repetitions int, run func() sample) sample {
	best := run()
	for range repetitions - 1 {
		if candidate := run(); candidate.eventsPerSecond() > best.eventsPerSecond() {
			best = candidate
		}
	}
	return best
}

// summarise orders repetitions by throughput and returns best, median, worst.
func summarise(samples []sample) (best, median, worst sample) {
	ordered := make([]sample, len(samples))
	copy(ordered, samples)
	sort.Slice(ordered, func(a, b int) bool {
		return ordered[a].eventsPerSecond() > ordered[b].eventsPerSecond()
	})
	return ordered[0], ordered[len(ordered)/2], ordered[len(ordered)-1]
}

// spreadPercent is how far the worst repetition fell below the best. It is the
// honest measure of how noisy the machine was during this run, and the number
// to look at before trusting or moving the threshold.
func spreadPercent(best, worst sample) float64 {
	if best.eventsPerSecond() == 0 {
		return 0
	}
	return 100 * (best.eventsPerSecond() - worst.eventsPerSecond()) / best.eventsPerSecond()
}

// round trims a duration to something readable in a log line.
func round(d time.Duration) time.Duration { return d.Round(10 * time.Microsecond) }
