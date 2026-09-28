// Command compat-load generates the two kinds of traffic the hardening gates need
// and reports, as one line of JSON, what the server did with it.
//
// Two modes, and they exist for the same reason:
//
//	-mode seed   fills an installation with a chosen shape of history, so the
//	             footprint gate can measure a server that has something in it
//	             rather than an empty one.
//	-mode flood  sends the same hostile body from many goroutines at once, so
//	             the hardening gate can ask what a *flood* costs rather than
//	             what one request costs.
//
// The second mode is the whole point of this program existing. This repository
// has published three wrong memory figures, and all three were wrong the same
// way: a measurement of one operation was reported as if it answered the
// question an attacker asks. Argon2id was budgeted at 64 MiB because nobody
// multiplied by the number of concurrent logins; zstd reserved a window per
// core because the measurement was taken on a machine with two; and `/login`
// had no per-address ceiling at all, which turned a 30 MB footprint into
// 1.03 GB under two hundred concurrent attempts (ADR 023, smoke.sh). A gate
// that sends one request cannot see any of those. So this sends N.
//
// Everything here is written by hand against the wire protocol rather than
// through an SDK. That is deliberate and is the opposite of the rule the
// compatibility suites follow (ADR 002): an SDK will not send a decompression
// bomb, and what is under test here is not whether this server understands
// real clients — the compat matrix answers that — but what it costs when
// somebody who is not a client talks to it.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
)

func main() {
	mode := flag.String("mode", "seed", "seed or flood")
	url := flag.String("url", "http://127.0.0.1:9000", "base URL of the server under test")
	project := flag.Int("project", 1, "project id the ingest path writes to")
	key := flag.String("key", "", "the DSN's public key (required)")
	timeout := flag.Duration("timeout", 60*time.Second, "per-request timeout")
	workers := flag.Int("workers", 4, "how many requests are in flight at once")

	// seed
	issues := flag.Int("issues", 100, "how many distinct issues the errors group into")
	events := flag.Int("events", 1000, "how many error events to send")
	transactions := flag.Int("transactions", 0, "how many transactions to send")
	sessions := flag.Int("sessions", 0, "how many sessions to send")
	batch := flag.Int("batch", 50, "items per envelope")
	seed := flag.Uint64("seed", 0x105AD, "seed for the generated shapes")

	// flood
	codec := flag.String("codec", "gzip", "content encoding of the flooded body: gzip, zstd or none")
	ratio := flag.Int("ratio", 1000, "compression ratio of the bomb, expanded:compressed")
	expand := flag.Int("expand-mb", 24, "how many MiB the bomb expands to")
	requests := flag.Int("requests", 0, "how many requests to send; defaults to -workers")
	path := flag.String("path", "", "override the request path, e.g. /ping/<key>")
	forwarded := flag.String("forwarded-for", "", "X-Forwarded-For to send; 'rotate' invents a new address per request")

	flag.Parse()

	if *key == "" && *path == "" {
		fmt.Fprintln(os.Stderr, "usage: compat-load -mode seed|flood -url ... -key <public key>")
		os.Exit(2)
	}

	var (
		summary any
		err     error
	)
	switch *mode {
	case "seed":
		summary, err = runSeed(seedOptions{
			base:   baseOptions{url: *url, project: *project, key: *key, timeout: *timeout, workers: *workers},
			issues: *issues, events: *events, transactions: *transactions,
			sessions: *sessions, batch: *batch, seed: *seed,
		})
	case "flood":
		count := *requests
		if count == 0 {
			count = *workers
		}
		summary, err = runFlood(floodOptions{
			base:  baseOptions{url: *url, project: *project, key: *key, timeout: *timeout, workers: *workers},
			codec: *codec, ratio: *ratio, expandMB: *expand, requests: count,
			path: *path, forwardedFor: *forwarded,
		})
	default:
		err = fmt.Errorf("unknown mode %q", *mode)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL %v\n", err)
		os.Exit(1)
	}

	// One line of compact JSON on stdout, so a shell can pull a field out with
	// sed and nothing else has to be installed — the same contract the other
	// helpers under compat/ answer with.
	encoded, err := json.Marshal(summary)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL encoding the summary: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(encoded))
}

type baseOptions struct {
	url     string
	project int
	key     string
	timeout time.Duration
	workers int
}

// client is built per run rather than shared with http.DefaultClient so the
// connection pool is large enough for the configured concurrency. The default
// pool holds two idle connections per host, which would serialise a flood into
// a queue and measure the queue instead of the server.
func (o baseOptions) client() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = o.workers + 8
	transport.MaxConnsPerHost = 0
	return &http.Client{Transport: transport, Timeout: o.timeout}
}

func (o baseOptions) envelopeURL() string {
	return fmt.Sprintf("%s/api/%d/envelope/", strings.TrimRight(o.url, "/"), o.project)
}

func (o baseOptions) authHeader() string {
	return "Sentry sentry_version=7, sentry_client=compat-load/1.0, sentry_key=" + o.key
}

// ---------------------------------------------------------------- seeding

type seedOptions struct {
	base                                          baseOptions
	issues, events, transactions, sessions, batch int
	seed                                          uint64
}

type seedSummary struct {
	Events       int     `json:"events"`
	Transactions int     `json:"transactions"`
	Sessions     int     `json:"sessions"`
	Issues       int     `json:"issues"`
	Requests     int     `json:"requests"`
	Refused      int     `json:"refused"`
	ElapsedS     float64 `json:"elapsed_s"`
	EventsPerS   float64 `json:"events_per_s"`
}

// runSeed fills an installation with history of a chosen shape.
//
// The shape matters more than the volume. A hundred thousand copies of one
// error is a hundred thousand rows against one issue and one FTS entry, which
// is not what an installation with a hundred thousand events looks like; the
// index sizes, the hourly buckets and the search table all scale with the
// number of *distinct* issues. So the generator varies the exception type, the
// culprit and the frame set across -issues variants and then distributes the
// events over them.
func runSeed(o seedOptions) (seedSummary, error) {
	started := time.Now()
	client := o.base.client()

	type job struct {
		kind  string
		index int
		count int
	}
	jobs := make(chan job)
	var (
		refused atomic.Int64
		sent    atomic.Int64
		failure atomic.Pointer[error]
		wait    sync.WaitGroup
	)

	for worker := 0; worker < o.base.workers; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			random := rand.New(rand.NewPCG(o.seed, uint64(worker)))
			for j := range jobs {
				var body []byte
				switch j.kind {
				case "event":
					body = errorEnvelope(random, o.issues, j.index, j.count)
				case "transaction":
					body = transactionEnvelope(random, j.count)
				case "session":
					body = sessionEnvelope(random, j.index, j.count)
				}
				status, err := postEnvelope(client, o.base, body)
				if err != nil {
					failure.CompareAndSwap(nil, &err)
					return
				}
				sent.Add(1)
				if status != http.StatusOK {
					refused.Add(1)
				}
			}
		}(worker)
	}

	queue := func(kind string, total int) {
		for index := 0; index < total; index += o.batch {
			count := o.batch
			if index+count > total {
				count = total - index
			}
			jobs <- job{kind: kind, index: index, count: count}
		}
	}
	queue("event", o.events)
	queue("transaction", o.transactions)
	queue("session", o.sessions)
	close(jobs)
	wait.Wait()

	if err := failure.Load(); err != nil {
		return seedSummary{}, *err
	}

	elapsed := time.Since(started).Seconds()
	return seedSummary{
		Events: o.events, Transactions: o.transactions, Sessions: o.sessions,
		Issues:   o.issues,
		Requests: int(sent.Load()), Refused: int(refused.Load()),
		ElapsedS: round(elapsed, 2), EventsPerS: round(float64(o.events)/elapsed, 1),
	}, nil
}

func postEnvelope(client *http.Client, o baseOptions, body []byte) (int, error) {
	// Gzipped, because that is what every SDK sends and because an ungzipped
	// seed would measure a path no real installation takes.
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(body); err != nil {
		return 0, fmt.Errorf("compressing the envelope: %w", err)
	}
	if err := writer.Close(); err != nil {
		return 0, fmt.Errorf("closing the compressor: %w", err)
	}

	request, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, o.envelopeURL(), bytes.NewReader(compressed.Bytes()))
	if err != nil {
		return 0, fmt.Errorf("building the request: %w", err)
	}
	request.Header.Set("X-Sentry-Auth", o.authHeader())
	request.Header.Set("Content-Encoding", "gzip")
	request.Header.Set("Content-Type", "application/x-sentry-envelope")

	response, err := client.Do(request)
	if err != nil {
		return 0, fmt.Errorf("sending the envelope: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode, nil
}

// exceptionKinds are the families the generated errors group into. Fixed
// rather than random text so that a seeded run is reproducible and two
// footprint measurements are comparable.
var exceptionKinds = []string{
	"ValueError", "TypeError", "KeyError", "TimeoutError", "ConnectionResetError",
	"IntegrityError", "PermissionDenied", "HTTPError", "RuntimeError", "OSError",
}

var modules = []string{
	"checkout", "billing", "inventory", "auth", "webhooks",
	"reports", "search", "uploads", "notifications", "sync",
}

func errorEnvelope(random *rand.Rand, issues, offset, count int) []byte {
	var out bytes.Buffer
	out.WriteString("{}\n")
	for i := 0; i < count; i++ {
		variant := (offset + i) % issues
		kind := exceptionKinds[variant%len(exceptionKinds)]
		module := modules[(variant/len(exceptionKinds))%len(modules)]
		payload := fmt.Sprintf(
			`{"event_id":"%s","timestamp":"%s","platform":"python","level":"error",`+
				`"release":"shop@1.%d.0","environment":"production","server_name":"web-%02d",`+
				`"exception":{"values":[{"type":"%s","value":"%s failed for order %d",`+
				`"stacktrace":{"frames":[`+
				`{"abs_path":"/srv/app/%s/service.py","function":"handle","lineno":%d,"in_app":true},`+
				`{"abs_path":"/srv/app/%s/repo.py","function":"fetch_%d","lineno":%d,"in_app":true},`+
				`{"abs_path":"/usr/lib/python3.12/json/decoder.py","function":"raw_decode","lineno":355,"in_app":false}`+
				`]}}]},`+
				`"tags":{"region":"sa-east-1","handler":"%s"},`+
				`"extra":{"order_total":%d,"attempt":%d}}`,
			hexID(random, 32), time.Now().UTC().Format(time.RFC3339),
			variant%9, variant%12,
			kind, module, random.IntN(99999),
			module, 40+variant%60,
			module, variant, 100+variant%80,
			module,
			random.IntN(5000), random.IntN(5))
		fmt.Fprintf(&out, "{\"type\":\"event\",\"length\":%d}\n%s\n", len(payload), payload)
	}
	return out.Bytes()
}

var routes = []string{
	"GET /api/checkout", "POST /api/orders", "GET /api/search", "POST /api/upload",
	"GET /api/reports/daily", "POST /webhooks/stripe", "GET /api/inventory",
}

func transactionEnvelope(random *rand.Rand, count int) []byte {
	var out bytes.Buffer
	out.WriteString("{}\n")
	for i := 0; i < count; i++ {
		// Log-normal-ish: most requests fast, a long tail. A uniform
		// distribution would exercise the sketch's buckets in a way nothing
		// real does.
		milliseconds := 8 + int(random.ExpFloat64()*120)
		end := time.Now().UTC()
		start := end.Add(-time.Duration(milliseconds) * time.Millisecond)
		status := "ok"
		if random.IntN(20) == 0 {
			status = "internal_error"
		}
		payload := fmt.Sprintf(
			`{"event_id":"%s","type":"transaction","transaction":"%s",`+
				`"transaction_info":{"source":"route"},`+
				`"start_timestamp":"%s","timestamp":"%s","platform":"python",`+
				`"release":"shop@1.8.0","environment":"production",`+
				`"contexts":{"trace":{"trace_id":"%s","span_id":"%s","op":"http.server","status":"%s"}},`+
				`"spans":[{"span_id":"%s","parent_span_id":"%s","trace_id":"%s","op":"db.sql",`+
				`"description":"SELECT * FROM orders WHERE id = $1","start_timestamp":"%s","timestamp":"%s"}]}`,
			hexID(random, 32), routes[random.IntN(len(routes))],
			start.Format("2006-01-02T15:04:05.000000Z"), end.Format("2006-01-02T15:04:05.000000Z"),
			hexID(random, 32), hexID(random, 16), status,
			hexID(random, 16), hexID(random, 16), hexID(random, 32),
			start.Format("2006-01-02T15:04:05.000000Z"),
			start.Add(time.Duration(milliseconds/2)*time.Millisecond).Format("2006-01-02T15:04:05.000000Z"))
		fmt.Fprintf(&out, "{\"type\":\"transaction\",\"length\":%d}\n%s\n", len(payload), payload)
	}
	return out.Bytes()
}

func sessionEnvelope(random *rand.Rand, offset, count int) []byte {
	var out bytes.Buffer
	out.WriteString("{}\n")
	for i := 0; i < count; i++ {
		status := "exited"
		errorCount := 0
		switch {
		case random.IntN(40) == 0:
			status = "crashed"
		case random.IntN(10) == 0:
			errorCount = 1
		}
		payload := fmt.Sprintf(
			`{"sid":"%s","did":"device-%d","started":"%s","status":"%s","errors":%d,`+
				`"attrs":{"release":"shop@1.8.0","environment":"production"}}`,
			hexID(random, 32), (offset+i)%500,
			time.Now().UTC().Format(time.RFC3339), status, errorCount)
		fmt.Fprintf(&out, "{\"type\":\"session\",\"length\":%d}\n%s\n", len(payload), payload)
	}
	return out.Bytes()
}

const hexDigits = "0123456789abcdef"

func hexID(random *rand.Rand, length int) string {
	out := make([]byte, length)
	for i := range out {
		out[i] = hexDigits[random.IntN(16)]
	}
	return string(out)
}

// ---------------------------------------------------------------- flooding

type floodOptions struct {
	base               baseOptions
	codec              string
	ratio, expandMB    int
	requests           int
	path, forwardedFor string
}

type floodSummary struct {
	Requests     int            `json:"requests"`
	Concurrency  int            `json:"concurrency"`
	Statuses     map[string]int `json:"statuses"`
	Errors       int            `json:"errors"`
	CompressedB  int            `json:"compressed_bytes"`
	ExpandedB    int            `json:"expanded_bytes"`
	Ratio        int            `json:"ratio"`
	P50MS        int64          `json:"p50_ms"`
	P95MS        int64          `json:"p95_ms"`
	MaxMS        int64          `json:"max_ms"`
	ElapsedS     float64        `json:"elapsed_s"`
	FirstFailure string         `json:"first_failure,omitempty"`
}

// runFlood sends the same body from -workers goroutines until -requests have
// been answered, and reports the status histogram and the latency spread.
//
// The latency matters as much as the status. A server that refuses a
// decompression bomb only after expanding it has still paid for the
// expansion, and the only externally visible evidence of that is how long the
// refusal took — which is why the gate asserts on a millisecond figure and
// not merely on a 413.
func runFlood(o floodOptions) (floodSummary, error) {
	body, expanded, err := buildBody(o)
	if err != nil {
		return floodSummary{}, err
	}

	target := o.base.envelopeURL()
	if o.path != "" {
		target = strings.TrimRight(o.base.url, "/") + o.path
	}

	client := o.base.client()
	var (
		mutex     sync.Mutex
		statuses  = map[string]int{}
		latencies []time.Duration
		firstFail string
		errs      atomic.Int64
	)

	started := time.Now()
	queue := make(chan int)
	var wait sync.WaitGroup
	for worker := 0; worker < o.base.workers; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for index := range queue {
				request, err := http.NewRequestWithContext(context.Background(),
					http.MethodPost, target, bytes.NewReader(body))
				if err != nil {
					errs.Add(1)
					continue
				}
				if o.path == "" {
					request.Header.Set("X-Sentry-Auth", o.base.authHeader())
					request.Header.Set("Content-Type", "application/x-sentry-envelope")
				}
				if o.codec != "none" {
					request.Header.Set("Content-Encoding", o.codec)
				}
				switch o.forwardedFor {
				case "":
				case "rotate":
					// A different claimed address every time. With no trusted
					// proxy configured this must change nothing at all; if it
					// did, the limiter's keys would be chosen by whoever is
					// attacking it.
					request.Header.Set("X-Forwarded-For",
						fmt.Sprintf("203.0.113.%d", 1+index%250))
				default:
					request.Header.Set("X-Forwarded-For", o.forwardedFor)
				}

				at := time.Now()
				response, err := client.Do(request)
				took := time.Since(at)

				mutex.Lock()
				latencies = append(latencies, took)
				if err != nil {
					errs.Add(1)
					if firstFail == "" {
						firstFail = err.Error()
					}
					mutex.Unlock()
					continue
				}
				statuses[fmt.Sprint(response.StatusCode)]++
				mutex.Unlock()

				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
			}
		}(worker)
	}
	for index := 0; index < o.requests; index++ {
		queue <- index
	}
	close(queue)
	wait.Wait()
	elapsed := time.Since(started)

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	return floodSummary{
		Requests: o.requests, Concurrency: o.base.workers,
		Statuses: statuses, Errors: int(errs.Load()),
		CompressedB: len(body), ExpandedB: expanded,
		Ratio:        safeRatio(expanded, len(body)),
		P50MS:        percentile(latencies, 0.50).Milliseconds(),
		P95MS:        percentile(latencies, 0.95).Milliseconds(),
		MaxMS:        percentile(latencies, 1.0).Milliseconds(),
		ElapsedS:     round(elapsed.Seconds(), 2),
		FirstFailure: firstFail,
	}, nil
}

// buildBody returns the request body and how many bytes it expands to.
//
// The bomb is a run of well-formed envelope items of a type this server does
// not recognise. That shape is chosen on purpose: an unknown item is skipped
// rather than rejected (ADR 002), so the parser keeps reading and the request
// is refused by the *decompression budget* rather than by the first malformed
// byte. A bomb made of junk would be refused in a microsecond by the header
// parser and would prove nothing about the guard it is aimed at.
func buildBody(o floodOptions) ([]byte, int, error) {
	if o.codec == "none" {
		// Not a bomb at all: an honest body, used to measure what an accepted
		// request costs so the refusal has something to be compared against.
		plain := errorEnvelope(rand.New(rand.NewPCG(1, 2)), 10, 0, 1)
		return plain, len(plain), nil
	}

	target := o.expandMB << 20
	// One big run of one byte per item, rather than many small ones. A gzip
	// stream cannot exceed about 1030:1 even on perfectly repetitive input, so
	// a bomb whose items are short spends most of its compressed budget on
	// item headers and never reaches the ratio the gate asks for. This is the
	// difference between testing the guard and testing the generator.
	const filler = 1 << 20
	item := fmt.Sprintf("{\"type\":\"attachment_unknown_to_this_server\",\"length\":%d}\n%s\n",
		filler, strings.Repeat("A", filler))

	var expanded bytes.Buffer
	expanded.Grow(target + len(item))
	expanded.WriteString("{}\n")
	for expanded.Len() < target {
		expanded.WriteString(item)
	}

	compressed, err := compress(o.codec, expanded.Bytes())
	if err != nil {
		return nil, 0, err
	}
	if got := safeRatio(expanded.Len(), len(compressed)); got < o.ratio {
		return nil, 0, fmt.Errorf(
			"the generated %s bomb only reaches %d:1 and the gate asks for %d:1; "+
				"make the filler more repetitive", o.codec, got, o.ratio)
	}
	return compressed, expanded.Len(), nil
}

func compress(codec string, payload []byte) ([]byte, error) {
	var out bytes.Buffer
	switch codec {
	case "gzip":
		writer, err := gzip.NewWriterLevel(&out, gzip.BestCompression)
		if err != nil {
			return nil, fmt.Errorf("building the gzip writer: %w", err)
		}
		if _, err := writer.Write(payload); err != nil {
			return nil, fmt.Errorf("writing the gzip bomb: %w", err)
		}
		if err := writer.Close(); err != nil {
			return nil, fmt.Errorf("closing the gzip bomb: %w", err)
		}
	case "zstd":
		writer, err := zstd.NewWriter(&out, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
		if err != nil {
			return nil, fmt.Errorf("building the zstd writer: %w", err)
		}
		if _, err := writer.Write(payload); err != nil {
			return nil, fmt.Errorf("writing the zstd bomb: %w", err)
		}
		if err := writer.Close(); err != nil {
			return nil, fmt.Errorf("closing the zstd bomb: %w", err)
		}
	default:
		return nil, fmt.Errorf("unknown codec %q", codec)
	}
	return out.Bytes(), nil
}

func safeRatio(expanded, compressed int) int {
	if compressed == 0 {
		return 0
	}
	return expanded / compressed
}

func percentile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	index := int(q * float64(len(sorted)-1))
	return sorted[index]
}

func round(value float64, places int) float64 {
	factor := 1.0
	for i := 0; i < places; i++ {
		factor *= 10
	}
	return float64(int64(value*factor+0.5)) / factor
}
