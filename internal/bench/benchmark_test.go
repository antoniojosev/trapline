package bench

import (
	"context"
	"testing"
)

// The Benchmark functions are for profiling, not for gating.
//
// TestIngestThroughputGate is the pass/fail judge; these exist so that when it
// goes red somebody can run the same workload under -cpuprofile or -benchmem
// and see which function grew, without having to write a harness first. They
// share the gate's envelope builders on purpose: a profile taken against a
// different payload would send whoever reads it somewhere else.
//
//	TRAPLINE_BENCH=1 go test -bench IngestHTTP -benchmem -cpuprofile cpu.out ./internal/bench/
//	go tool pprof -top cpu.out

// BenchmarkIngestHTTP profiles the whole path, per workload.
func BenchmarkIngestHTTP(b *testing.B) {
	requireBenchEnabled(b)
	for _, kind := range []workload{newIssue, existingIssue} {
		b.Run(kind.String(), func(b *testing.B) {
			// Built before the timer starts: gzipping is the SDK's cost, and
			// b.N envelopes of it would dominate the profile.
			envelopes := buildEnvelopes(b, kind, timedOffset, b.N)
			stack := newStack(b)
			ctx := context.Background()
			for _, body := range buildEnvelopes(b, kind, 0, defaultWarmup) {
				if err := stack.post(ctx, body); err != nil {
					b.Fatalf("warming up: %v", err)
				}
			}

			b.ResetTimer()
			for _, body := range envelopes {
				if err := stack.post(ctx, body); err != nil {
					b.Fatalf("posting: %v", err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "events/s")
		})
	}
}

// BenchmarkIngestUseCase profiles everything below HTTP: the same work without
// the socket, the router or the key lookup.
func BenchmarkIngestUseCase(b *testing.B) {
	requireBenchEnabled(b)
	for _, kind := range []workload{newIssue, existingIssue} {
		b.Run(kind.String(), func(b *testing.B) {
			envelopes := buildEnvelopes(b, kind, timedOffset, b.N)
			stack := newStack(b)
			if _, err := runIngest(stack.ingestUnlimited, stack.projectID,
				buildEnvelopes(b, kind, 0, defaultWarmup)); err != nil {
				b.Fatalf("warming up: %v", err)
			}

			b.ResetTimer()
			if _, err := runIngest(stack.ingestUnlimited, stack.projectID, envelopes); err != nil {
				b.Fatalf("ingesting: %v", err)
			}
			b.StopTimer()
			b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "events/s")
		})
	}
}
