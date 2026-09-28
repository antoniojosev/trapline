// Package bench holds the ingest throughput gate: the executable version of
// the volume claim in ADR 001 and the driver verdict ADR 009 asks for.
//
// It contains no production code — everything lives in _test.go files, so the
// harness never reaches the shipped binary and cannot spend any of its size
// budget. This file exists only to carry the methodology, because a number
// nobody can reproduce is a number nobody should believe.
//
// # What is claimed
//
// ADR 001 declares a target volume of "<100 eventos/s sostenidos" and calls it
// "una promesa verificable": an ingest benchmark as a CI gate, not a README
// boast. ADR 009 chose a pure-Go SQLite driver knowing it is slower than the
// C binding, and named this gate as the judge of whether that cost is
// affordable. Both ADRs point here.
//
// # What is measured
//
// The full HTTP path, because that is what the claim is about. An SDK's cost
// is the whole request — gzip, auth, envelope framing, decode, scrub,
// grouping, transaction — and measuring at the use-case boundary would quote a
// number no operator can observe.
//
// A single number would say nothing actionable, so the gate measures five
// nested layers over the same workload and reports the cost of each step as a
// delta:
//
//  1. gunzip           — decompress the request body and discard it
//  2. + envelope parse — plus envelope.Parse
//  3. + domain work    — plus decode, scrub, re-encode and fingerprint,
//     against a storage port that discards
//  4. + SQLite write   — plus the real transaction
//  5. + HTTP           — the whole thing over a real socket, as an SDK sends it
//
// Layer 5 is what the gate asserts on. Layers 1-4 exist so that a regression
// says where the time went instead of only that it got worse.
//
// # Two workloads, not one
//
// Creating an issue and appending to one are different database shapes and are
// measured separately:
//
//   - new: SELECT (miss), INSERT issue, INSERT event, INSERT tags
//   - existing: SELECT (hit), UPDATE issue, INSERT event, UPDATE tags
//
// A benchmark that only sent one error would measure the second shape almost
// exclusively and quietly stop covering the first, which is the path a traffic
// spike after a bad deploy actually hits.
//
// # Realistic events
//
// The payload is a full Python-SDK exception: a chained exception with a
// twelve-frame stacktrace carrying source context lines, twenty breadcrumbs,
// tags, request, user, contexts and extra. That is 14.6 KB of JSON, 1.8 KB
// gzipped on the wire, which is an ordinary event rather than a large one. An
// empty envelope would measure the framing and nothing that costs money: JSON
// decoding, scrubbing and zstd compression all scale with payload size, and
// together they are most of the CPU this path spends.
//
// The secrets in it are load-bearing too. Scrubbing runs before the write, so
// a payload with nothing to redact would skip work every real event pays for.
//
// Envelopes are built and compressed before the clock starts. Gzipping is the
// SDK's cost, not the server's, and charging it to the server would understate
// throughput.
//
// # Rate limiting is raised on purpose
//
// The benchmark project sets RateLimitPerMinute far above the shipped default.
// This is not the benchmark cheating; it is the benchmark measuring capacity
// rather than policy. The shipped default of domain.DefaultRateLimitPerMinute
// (2000/minute, i.e. ~33 events/s per category) is a separate design question
// from how fast the machine can write, and conflating them would make this
// gate report the constant rather than the code.
//
// # Robustness on a noisy machine
//
// CI runners are shared, throttled and unpredictable. The defence is threefold:
//
//   - Each repetition gets a fresh database and a warm-up whose events are not
//     timed, so no repetition inherits another's page cache or table size.
//   - The gate asserts on the BEST of several repetitions. Interference from a
//     co-tenant can only ever make a measurement slower, never faster, so the
//     best repetition is the closest estimate of what the machine can do. A
//     real regression slows every repetition and still trips the gate; one
//     unlucky repetition does not.
//   - The threshold sits several times above the published claim and several
//     times below the measured figure, so the band absorbing noise is enormous.
//
// # Running it
//
//	TRAPLINE_BENCH=1 go test -count=1 -run TestIngestThroughputGate -v -timeout 20m ./internal/bench/
//	TRAPLINE_BENCH=1 go test -bench . -benchmem ./internal/bench/
//
// The Makefile target and CI job that wrap the first command are reported
// alongside this change rather than added here.
//
// It is skipped unless TRAPLINE_BENCH is set, and skipped under -race even
// then. Both guards are deliberate. `make check` runs `go test -race ./...`,
// where the race detector's instrumentation makes any throughput figure
// meaningless — a gate that runs there would either measure nothing real or
// fail for a reason unrelated to the code. Being explicitly wired into its own
// CI job is what keeps it honest.
//
// # Environment overrides
//
//	TRAPLINE_BENCH=1                    required, or every measurement skips
//	TRAPLINE_BENCH_EVENTS=2000          timed events per repetition
//	TRAPLINE_BENCH_REPS=5               repetitions per workload
//	TRAPLINE_BENCH_MIN_EPS=<threshold>  override the gate for a slow machine
package bench
