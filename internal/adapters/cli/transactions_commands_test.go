package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

// newTracingHarness sets an installation up with tracing on and a known hour
// of transactions already recorded.
//
// The transactions go in through the repository rather than over the ingest
// endpoint: what is under test here is the CLI, and driving the wire format
// again would only be a slower copy of the HTTP suite's own coverage.
func newTracingHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.setUp()
	h.mustRun("projects", "create", "-name", "venekambio")
	return h
}

// tracingHour is the hour every assertion below is written against. It is
// "now" as the harness's clock sees it, because the CLI's default range is the
// last twenty-four hours and a fixed date in the past would fall outside it.
func tracingHour(t *testing.T) time.Time {
	t.Helper()
	return testClock{}.Now().Truncate(time.Hour)
}

func recordTransactions(t *testing.T, h *harness, name string, durations []float64, withTraces bool) {
	t.Helper()
	ctx := context.Background()
	at := tracingHour(t).Add(5 * time.Minute)
	for index, duration := range durations {
		payload := fmt.Appendf(nil,
			`{"transaction":%q,"start_timestamp":%q,"timestamp":%q,`+
				`"contexts":{"trace":{"trace_id":%q,"span_id":"fa90fdead5f74052",`+
				`"op":"http.server","status":%q}},`+
				`"spans":[{"span_id":"1111111111111111","parent_span_id":"fa90fdead5f74052",`+
				`"op":"db.sql","description":"SELECT 1",`+
				`"start_timestamp":%q,"timestamp":%q}]}`,
			name, at.Format(time.RFC3339Nano),
			at.Add(time.Duration(duration)*time.Millisecond).Format(time.RFC3339Nano),
			fmt.Sprintf("%s%028x", "abcd", index), statusFor(index),
			at.Format(time.RFC3339Nano),
			at.Add(time.Duration(duration/2)*time.Millisecond).Format(time.RFC3339Nano))

		rate := 0.0
		if withTraces {
			rate = 1
		}
		if err := h.stack.Projects.SetConfig(ctx, 1, domain.ProjectConfig{
			EnabledCategories:       []string{"error", "transaction"},
			TracesSampleRateSetting: &rate,
		}); err != nil {
			t.Fatalf("configuring: %v", err)
		}
		if _, err := h.stack.Transactions.Accept(ctx, 1, payload, 0); err != nil {
			t.Fatalf("recording: %v", err)
		}
	}
}

// statusFor makes every fifth transaction a failure, so the fail column is not
// always zero.
func statusFor(index int) string {
	if index%5 == 0 {
		return "internal_error"
	}
	return "ok"
}

func TestTransactionsListThroughTheCLI(t *testing.T) {
	h := newTracingHarness(t)
	recordTransactions(t, h, "GET /api/checkout", []float64{100, 200, 300, 400, 500}, false)
	recordTransactions(t, h, "GET /health", []float64{5, 6, 7}, false)

	raw := h.mustRun("transactions", "list", "-project", "1", "--json")
	var payload transactionsPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("list did not emit JSON: %v (%s)", err, raw)
	}
	if payload.Total != 8 {
		t.Fatalf("total = %d, want 8: %s", payload.Total, raw)
	}
	if len(payload.Rows) != 2 {
		t.Fatalf("%d rows, want 2", len(payload.Rows))
	}
	if payload.Rows[0].Transaction != "GET /api/checkout" {
		t.Errorf("ranked by p95, the first row is %q", payload.Rows[0].Transaction)
	}

	// The text form is a table somebody can read, and it has to name the
	// sampling: "0 of 8 traces stored" is the sentence that stops a reader
	// concluding the product lost their data.
	text := h.mustRun("transactions", "list", "-project", "1")
	if !strings.Contains(text, "GET /api/checkout") {
		t.Errorf("the table does not name the transaction:\n%s", text)
	}
	if !strings.Contains(text, "traces stored") {
		t.Errorf("the table does not report the sampling:\n%s", text)
	}
	if !strings.Contains(text, "p95") {
		t.Errorf("the table does not show a p95:\n%s", text)
	}

	// The other two rankings, because a ranking that cannot be asked for from
	// a terminal is a ranking an agent does not have (ADR 006).
	byFail := h.mustRun("transactions", "list", "-project", "1", "-sort", "fail", "--json")
	if !strings.Contains(byFail, `"sort":"fail"`) {
		t.Errorf("the response does not echo the ranking: %s", byFail)
	}
	byCount := h.mustRun("transactions", "list", "-project", "1", "-sort", "count", "-limit", "1", "--json")
	var limited transactionsPayload
	if err := json.Unmarshal([]byte(byCount), &limited); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(limited.Rows) != 1 {
		t.Errorf("-limit 1 returned %d rows", len(limited.Rows))
	}
}

func TestTransactionsShowAndTracesShowThroughTheCLI(t *testing.T) {
	h := newTracingHarness(t)
	recordTransactions(t, h, "GET /api/checkout", []float64{100, 200, 300}, true)

	raw := h.mustRun("transactions", "show", "-project", "1", "-name", "GET /api/checkout", "--json")
	var series transactionSeriesPayload
	if err := json.Unmarshal([]byte(raw), &series); err != nil {
		t.Fatalf("show did not emit JSON: %v (%s)", err, raw)
	}
	if series.Total != 3 {
		t.Fatalf("total = %d, want 3: %s", series.Total, raw)
	}
	if len(series.Examples) == 0 {
		t.Fatalf("no stored traces came back: %s", raw)
	}

	// A transaction called "GET /api/checkout" has a slash in it, and the
	// name goes in the path: this is the assertion that the escaping works,
	// because an unescaped slash would silently address a different route.
	text := h.mustRun("transactions", "show", "-project", "1", "-name", "GET /api/checkout")
	if !strings.Contains(text, "GET /api/checkout") {
		t.Errorf("the series does not name the transaction:\n%s", text)
	}
	if !strings.Contains(text, "stored traces") {
		t.Errorf("the series does not list its examples:\n%s", text)
	}

	// Hour resolution reads the same data folded up.
	hourly := h.mustRun("transactions", "show", "-project", "1",
		"-name", "GET /api/checkout", "-resolution", "hour", "--json")
	if !strings.Contains(hourly, `"resolution":"hour"`) {
		t.Errorf("the hourly series does not echo its resolution: %s", hourly)
	}

	traceID := series.Examples[0].TraceID
	waterfall := h.mustRun("traces", "show", "-project", "1", "-trace", traceID)
	if !strings.Contains(waterfall, traceID) {
		t.Errorf("the waterfall does not name the trace:\n%s", waterfall)
	}
	if !strings.Contains(waterfall, "█") {
		t.Errorf("the waterfall drew no bars, so it is a list of durations:\n%s", waterfall)
	}
	if !strings.Contains(waterfall, "db.sql") {
		t.Errorf("the child span is missing:\n%s", waterfall)
	}

	asJSON := h.mustRun("traces", "show", "-project", "1", "-trace", traceID, "--json")
	var trace tracePayload
	if err := json.Unmarshal([]byte(asJSON), &trace); err != nil {
		t.Fatalf("traces show did not emit JSON: %v", err)
	}
	if len(trace.Spans) != 2 {
		t.Errorf("%d spans, want the root plus its child", len(trace.Spans))
	}
}

func TestTracingCommandsRefuseMissingArguments(t *testing.T) {
	h := newTracingHarness(t)

	cases := [][]string{
		{"transactions"},
		{"transactions", "nonsense"},
		{"transactions", "list"},
		{"transactions", "list", "-project", "0"},
		{"transactions", "show", "-project", "1"},
		{"transactions", "show", "-name", "x"},
		{"traces"},
		{"traces", "nonsense"},
		{"traces", "show", "-project", "1"},
		{"traces", "show", "-trace", "abc"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, stdout, _ := h.run(args...)
			if code != ExitUsage {
				t.Errorf("exit %d, want %d", code, ExitUsage)
			}
			if stdout != "" {
				// An agent parses stdout; a diagnostic mixed into it would
				// corrupt what it is trying to read.
				t.Errorf("a usage error wrote to stdout: %q", stdout)
			}
		})
	}
}

func TestTracesShowReportsATraceNobodyKept(t *testing.T) {
	h := newTracingHarness(t)
	// With sampling on, most traces were never stored. A reader who followed
	// a trace id from a log line needs a non-zero exit and a message, not an
	// empty waterfall that reads as "this request had no spans".
	code, stdout, stderr := h.run("traces", "show", "-project", "1", "-trace", "deadbeef")
	if code != ExitError {
		t.Fatalf("exit %d, want %d", code, ExitError)
	}
	if stdout != "" {
		t.Errorf("a failure wrote to stdout, which an agent parses: %q", stdout)
	}
	if !strings.Contains(strings.ToLower(stderr), "not found") {
		t.Errorf("the message does not say the trace is missing: %q", stderr)
	}
}

func TestRenderMillisReadsLikeADuration(t *testing.T) {
	cases := map[float64]string{
		0.5:    "0.5ms",
		9.94:   "9.9ms",
		12.4:   "12ms",
		999:    "999ms",
		1500:   "1.50s",
		125000: "125.00s",
	}
	for value, want := range cases {
		if got := renderMillis(value); got != want {
			t.Errorf("renderMillis(%v) = %q, want %q", value, got, want)
		}
	}
}

func TestRenderTraceWithNoSpansSaysSo(t *testing.T) {
	// An empty waterfall and a broken one look identical on screen unless one
	// of them says which it is.
	text := renderTrace(&tracePayload{TraceID: "abc", Transaction: "GET /", DurationMS: 10})
	if !strings.Contains(text, "no spans stored") {
		t.Errorf("a trace with no spans rendered as:\n%s", text)
	}
}

func TestConfigShowReportsTheSamplingRate(t *testing.T) {
	h := newTracingHarness(t)

	text := h.mustRun("config", "show", "-project", "1")
	if !strings.Contains(text, "traces") {
		t.Errorf("config show does not mention tracing:\n%s", text)
	}
	if !strings.Contains(text, "every transaction") {
		// The sentence that stops the rate being read as a loss of accuracy.
		t.Errorf("config show does not say the aggregates cover everything:\n%s", text)
	}

	raw := h.mustRun("config", "set", "-project", "1", "-traces-sample-rate", "0.25", "--json")
	var config configPayload
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatalf("config set did not emit JSON: %v", err)
	}
	if config.TracesSampleRate == nil || *config.TracesSampleRate != 0.25 {
		t.Fatalf("the rate came back as %v", config.TracesSampleRate)
	}

	// Zero is a decision — aggregate everything, keep no waterfalls — and has
	// to survive a round trip as a set value rather than as an absence.
	raw = h.mustRun("config", "set", "-project", "1", "-traces-sample-rate", "0", "--json")
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if config.TracesSampleRate == nil || *config.TracesSampleRate != 0 {
		t.Errorf("a rate of zero came back as %v", config.TracesSampleRate)
	}

	// And "default" is the way back to inheriting.
	raw = h.mustRun("config", "set", "-project", "1", "-traces-sample-rate", "default", "--json")
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if config.TracesSampleRate != nil {
		t.Errorf("\"default\" left the rate at %v", *config.TracesSampleRate)
	}

	for _, bad := range []string{"2", "-1", "half"} {
		if code, _, _ := h.run("config", "set", "-project", "1", "-traces-sample-rate", bad); code != ExitUsage {
			t.Errorf("-traces-sample-rate %q exited %d, want a usage error", bad, code)
		}
	}
}
