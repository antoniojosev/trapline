package httpapi

import (
	"fmt"
	"math"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/antoniojosev/trapline/internal/domain"
)

// The synthetic hour every assertion below is written against. Fixed rather
// than relative to now, for the same reason the stats tests fix theirs: a
// latency chart is a statement about particular minutes, and a test that
// drifted with the clock would assert a different thing every afternoon.
const (
	txnDay      = "2026-08-24"
	txnFrom     = txnDay + "T10"
	txnTo       = txnDay + "T10"
	txnRangeQry = "from=" + txnFrom + "&to=" + txnTo
)

// transactionEnvelope is a transaction item as the Go SDK sends one, with the
// duration, the name, the status and the trace id chosen by the caller.
func transactionEnvelope(
	dsn domain.DSN, name, traceID, status string, minute int, durationMS float64,
) string {
	payload := fmt.Sprintf(`{"event_id":"2ee5a2a1e29e4a3ea1ea1de4f7c1b3d5","type":"transaction",`+
		`"transaction":%q,"start_timestamp":"%sT10:%02d:00.000000Z","timestamp":"%sT10:%02d:%09.6fZ",`+
		`"platform":"go","release":"billing@4.11.2","environment":"production",`+
		`"contexts":{"trace":{"trace_id":%q,"span_id":"fa90fdead5f74052","op":"http.server","status":%q}},`+
		`"spans":[{"span_id":"1111111111111111","parent_span_id":"fa90fdead5f74052","op":"db.sql",`+
		`"description":"SELECT 1","status":"ok","start_timestamp":1787050800.0,"timestamp":1787050800.01,`+
		`"data":{"db.system":"postgresql","password":"hunter2"}}]}`,
		name, txnDay, minute, txnDay, minute, durationMS/1000, traceID, status)

	return fmt.Sprintf("{\"event_id\":\"2ee5a2a1e29e4a3ea1ea1de4f7c1b3d5\",\"dsn\":%q}\n"+
		"{\"type\":\"transaction\",\"length\":%d}\n%s\n", dsn.String(), len(payload), payload)
}

// enableTracing switches the transaction category on and pins the sample rate.
//
// Both are needed and for different reasons: without the category the limiter
// refuses the item before anything is parsed (ADR 005), and without a pinned
// rate the default one in ten would make every assertion about stored traces a
// coin toss.
func enableTracing(t *testing.T, c *client, projectID int64, rate float64) {
	t.Helper()
	path := api("/projects/") + strconv.FormatInt(projectID, 10) + "/config"
	response := c.do(http.MethodPut, path, map[string]any{
		"enabled_categories": []string{"error", "transaction"},
		"traces_sample_rate": rate,
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("enabling tracing returned %d", response.StatusCode)
	}
}

func txnPath(projectID int64, suffix, query string) string {
	path := api("/projects/") + strconv.FormatInt(projectID, 10) + suffix
	if query != "" {
		path += "?" + query
	}
	return path
}

func TestTransactionsAreAggregatedAndRanked(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)
	enableTracing(t, client, dsn.ProjectID, 1)

	// A slow endpoint that never fails and a fast one that fails a third of
	// the time. The pair is the point of the three rankings existing: a
	// request that fails fast looks fast, so a latency chart alone would
	// never show it.
	for index := range 30 {
		sendEnvelope(t, server.URL, dsn, transactionEnvelope(
			dsn, "GET /api/checkout", fmt.Sprintf("%032x", index), "ok", 5, 400), "")
	}
	for index := range 30 {
		status := "ok"
		if index%3 == 0 {
			status = "internal_error"
		}
		sendEnvelope(t, server.URL, dsn, transactionEnvelope(
			dsn, "GET /health", fmt.Sprintf("%032x", 1000+index), status, 5, 8), "")
	}

	var payload transactionsResponse
	client.decode(client.do(http.MethodGet, txnPath(dsn.ProjectID, "/transactions", txnRangeQry), nil), &payload)

	if payload.Total != 60 {
		t.Fatalf("total = %d, want 60", payload.Total)
	}
	if payload.TotalFailed != 10 {
		t.Errorf("failures = %d, want 10", payload.TotalFailed)
	}
	if len(payload.Rows) != 2 {
		t.Fatalf("%d rows, want 2: %+v", len(payload.Rows), payload.Rows)
	}

	// The default ranking is p95, so the slow endpoint is first.
	if payload.Sort != string(domain.SortP95) {
		t.Errorf("sort = %q, want p95 by default", payload.Sort)
	}
	if payload.Rows[0].Name != "GET /api/checkout" {
		t.Errorf("first row is %q, want the slow endpoint", payload.Rows[0].Name)
	}
	if math.Abs(payload.Rows[0].P95-400) > 400*0.01 {
		t.Errorf("p95 = %v, want 400 within one percent", payload.Rows[0].P95)
	}
	if payload.Rows[0].FailRate != 0 {
		t.Errorf("the slow endpoint reports a %v failure rate", payload.Rows[0].FailRate)
	}

	// Ranked by failures, the fast one comes first — which is the whole
	// reason the ranking is a parameter.
	var byFail transactionsResponse
	client.decode(client.do(http.MethodGet,
		txnPath(dsn.ProjectID, "/transactions", txnRangeQry+"&sort=fail"), nil), &byFail)
	if byFail.Rows[0].Name != "GET /health" {
		t.Errorf("ranked by failures, the first row is %q", byFail.Rows[0].Name)
	}
	if math.Abs(byFail.Rows[0].FailRate-10.0/30.0) > 0.001 {
		t.Errorf("fail rate = %v, want a third", byFail.Rows[0].FailRate)
	}

	// And by count, which is what decides whether slow matters.
	var byCount transactionsResponse
	client.decode(client.do(http.MethodGet,
		txnPath(dsn.ProjectID, "/transactions", txnRangeQry+"&sort=count"), nil), &byCount)
	if byCount.Rows[0].Count != 30 {
		t.Errorf("ranked by count, the first row holds %d", byCount.Rows[0].Count)
	}
}

// TestAggregatesCoverEverythingWhileTracesAreSampled is the central claim of
// ADR 021, asserted through the public endpoints.
//
// The percentiles must be computed over 100% of what arrived; the sampling
// decides only how many waterfalls exist. Getting this backwards would make
// the p95 a percentile of a tenth of the traffic, which is a different number
// that looks identical.
func TestAggregatesCoverEverythingWhileTracesAreSampled(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)
	enableTracing(t, client, dsn.ProjectID, 0.1)

	const sent = 400
	for index := range sent {
		sendEnvelope(t, server.URL, dsn, transactionEnvelope(
			dsn, "GET /", fmt.Sprintf("%032x", index), "ok", 5, float64(index%100+1)), "")
	}

	var payload transactionsResponse
	client.decode(client.do(http.MethodGet, txnPath(dsn.ProjectID, "/transactions", txnRangeQry), nil), &payload)

	if payload.Total != sent {
		t.Errorf("the aggregate counted %d of %d transactions", payload.Total, sent)
	}
	if payload.Sampling.Received != sent {
		t.Errorf("sampling.received = %d, want %d", payload.Sampling.Received, sent)
	}
	if payload.Sampling.ServerRate != 0.1 {
		t.Errorf("sampling.server_rate = %v, want 0.1", payload.Sampling.ServerRate)
	}

	// About a tenth of the traces stored. Wide bounds: 400 draws of a hash is
	// not a large sample, and the property under test is "far fewer than all
	// of them, and not none", not the exact figure.
	stored := payload.Sampling.Stored
	if stored < sent/40 || stored > sent/4 {
		t.Errorf("%d of %d traces stored, want roughly a tenth", stored, sent)
	}
	if payload.Sampling.EffectiveRate <= 0 || payload.Sampling.EffectiveRate > 0.3 {
		t.Errorf("effective rate = %v", payload.Sampling.EffectiveRate)
	}
}

func TestTransactionSeriesAndItsWaterfall(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)
	enableTracing(t, client, dsn.ProjectID, 1)

	for minute := range 3 {
		for index := range 10 {
			sendEnvelope(t, server.URL, dsn, transactionEnvelope(
				dsn, "GET /api/checkout", fmt.Sprintf("%032x", minute*100+index),
				"ok", minute, float64(index+1)*10), "")
		}
	}

	name := neturl.PathEscape("GET /api/checkout")
	var series transactionSeriesResponse
	client.decode(client.do(http.MethodGet,
		txnPath(dsn.ProjectID, "/transactions/"+name+"/series", txnRangeQry), nil), &series)

	if series.Total != 30 {
		t.Fatalf("total = %d, want 30", series.Total)
	}
	if series.Resolution != string(domain.ResolutionMinute) {
		t.Errorf("resolution = %q, want minute by default", series.Resolution)
	}
	// A whole hour of minutes, quiet ones included: a chart drawn from only
	// the buckets that had traffic draws a quiet stretch as a straight line.
	if len(series.Points) != 60 {
		t.Fatalf("%d points for an hour at minute resolution, want 60", len(series.Points))
	}
	busy := 0
	for _, point := range series.Points {
		if point.Count > 0 {
			busy++
			if point.Count != 10 {
				t.Errorf("bucket %s holds %d, want 10", point.Bucket, point.Count)
			}
		}
	}
	if busy != 3 {
		t.Errorf("%d busy minutes, want 3", busy)
	}

	// The waterfall rides along, because the question after "this got slower"
	// is always "show me one".
	if len(series.Examples) == 0 {
		t.Fatal("no stored traces came back with the series")
	}
	example := series.Examples[0]
	if len(example.Spans) != 2 {
		t.Fatalf("the waterfall has %d spans, want the root plus its child", len(example.Spans))
	}
	if example.Spans[0].Op != "http.server" {
		t.Errorf("the first span is %q, want the root", example.Spans[0].Op)
	}
	// Scrubbed on the way in, not on the way out: a credential in a span's
	// data must never reach the disk (SECURITY.md).
	if got := example.Spans[1].Data["password"]; got == "hunter2" {
		t.Error("a password survived into a stored span")
	}

	// And the same trace by id.
	var trace map[string]any
	client.decode(client.do(http.MethodGet,
		txnPath(dsn.ProjectID, "/traces/"+example.TraceID, ""), nil), &trace)
	if trace["trace_id"] != example.TraceID {
		t.Errorf("the trace endpoint returned %v", trace["trace_id"])
	}

	// Hour resolution reads the same data folded up, and has to agree.
	var hourly transactionSeriesResponse
	client.decode(client.do(http.MethodGet,
		txnPath(dsn.ProjectID, "/transactions/"+name+"/series",
			txnRangeQry+"&resolution=hour"), nil), &hourly)
	if hourly.Total != 30 {
		t.Errorf("the hourly series totals %d, want 30", hourly.Total)
	}
	if len(hourly.Points) != 1 {
		t.Errorf("%d hourly points for one hour", len(hourly.Points))
	}
	if hourly.Summary.P95 != series.Summary.P95 {
		t.Errorf("the two resolutions disagree on p95: %v against %v",
			hourly.Summary.P95, series.Summary.P95)
	}
}

func TestTracingEndpointsRefuseWhatDoesNotExist(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)
	enableTracing(t, client, dsn.ProjectID, 1)

	cases := map[string]struct {
		path   string
		status int
	}{
		"a sort that is not one": {
			txnPath(dsn.ProjectID, "/transactions", "sort=alphabetical"), http.StatusBadRequest},
		"a resolution that is not one": {
			txnPath(dsn.ProjectID, "/transactions/x/series", "resolution=fortnight"), http.StatusBadRequest},
		"a trace nobody kept": {
			txnPath(dsn.ProjectID, "/traces/deadbeef", ""), http.StatusNotFound},
		"a limit that is not a limit": {
			txnPath(dsn.ProjectID, "/transactions", "limit=0"), http.StatusBadRequest},
		"a project nobody owns": {
			txnPath(9999, "/transactions", ""), http.StatusNotFound},
		"a project id that is not a number": {
			// Reachable: the router matches {id} against any segment, so the
			// handler is the thing that has to refuse it — and 400 rather than
			// 404, because "abc" is not an id somebody could own.
			api("/projects/abc/traces/deadbeef"), http.StatusBadRequest},
		"a trace in a project nobody owns": {
			txnPath(9999, "/traces/deadbeef", ""), http.StatusNotFound},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := client.do(http.MethodGet, tc.path, nil).StatusCode; got != tc.status {
				t.Errorf("status = %d, want %d", got, tc.status)
			}
		})
	}
}

// TestATransactionIsRefusedWhenTheCategoryIsOff is ADR 005 applied to
// tracing: a subsystem nobody switched on costs nothing, and the SDK is told
// to stop sending rather than being silently ignored.
func TestATransactionIsRefusedWhenTheCategoryIsOff(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	response := sendEnvelope(t, server.URL, dsn, transactionEnvelope(
		dsn, "GET /", "4c79f60c11214eb38604f4ae0781bfb2", "ok", 5, 100), "")
	// The whole envelope was one refused category, so the protocol's answer is
	// 429 with the header that tells the SDK to stop sending it — which is
	// what makes a switched-off subsystem cost nothing on the wire and not
	// merely nothing on disk.
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("ingest returned %d, want 429", response.StatusCode)
	}
	header := response.Header.Get("X-Sentry-Rate-Limits")
	if header == "" {
		t.Error("no rate-limit header, so the SDK will keep sending a category this project refuses")
	}
	if !strings.Contains(header, "transaction") {
		t.Errorf("the header does not name the refused category: %q", header)
	}

	// Nothing was aggregated, and the endpoint still answers rather than
	// erroring — an empty performance page is what a project without tracing
	// should look like.
	var payload transactionsResponse
	client.decode(client.do(http.MethodGet, txnPath(dsn.ProjectID, "/transactions", ""), nil), &payload)
	if payload.Total != 0 {
		t.Errorf("total = %d for a project that refuses transactions", payload.Total)
	}
	if payload.Rows == nil {
		t.Error("the rows came back null rather than an empty list")
	}
}

func TestAMalformedTransactionDoesNotCostTheEnvelope(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)
	enableTracing(t, client, dsn.ProjectID, 1)

	// One item with no timestamps at all, which the decoder refuses, followed
	// by a good one in the same envelope.
	bad := `{"type":"transaction","transaction":"GET /"}`
	good := fmt.Sprintf(`{"event_id":"2ee5a2a1e29e4a3ea1ea1de4f7c1b3d5","type":"transaction",`+
		`"transaction":"GET /ok","start_timestamp":"%sT10:05:00Z","timestamp":"%sT10:05:00.1Z",`+
		`"contexts":{"trace":{"trace_id":"4c79f60c11214eb38604f4ae0781bfb2","status":"ok"}}}`,
		txnDay, txnDay)
	body := fmt.Sprintf("{\"dsn\":%q}\n{\"type\":\"transaction\",\"length\":%d}\n%s\n"+
		"{\"type\":\"transaction\",\"length\":%d}\n%s\n",
		dsn.String(), len(bad), bad, len(good), good)

	if got := sendEnvelope(t, server.URL, dsn, body, "").StatusCode; got != http.StatusOK {
		t.Fatalf("ingest returned %d", got)
	}

	var payload transactionsResponse
	client.decode(client.do(http.MethodGet, txnPath(dsn.ProjectID, "/transactions", txnRangeQry), nil), &payload)
	if payload.Total != 1 {
		t.Errorf("total = %d, want 1: one bad item must not cost the rest of the envelope", payload.Total)
	}
}

func TestTracingRoutesNeedAuthentication(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	// A fresh client with no session: every one of these is a read of
	// somebody's production latency, and none of them is public.
	anonymous := newClient(t, server)
	for _, path := range []string{
		txnPath(dsn.ProjectID, "/transactions", ""),
		txnPath(dsn.ProjectID, "/transactions/x/series", ""),
		txnPath(dsn.ProjectID, "/traces/abc", ""),
	} {
		if got := anonymous.do(http.MethodGet, path, nil).StatusCode; got != http.StatusUnauthorized {
			t.Errorf("%s answered %d to an anonymous caller, want 401", path, got)
		}
	}
	_ = client
}
