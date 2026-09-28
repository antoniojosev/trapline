package cli

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// runTransactions dispatches the performance subcommands.
//
// Two verbs and not one with flags, unlike `stats`: a listing and one
// transaction's history are genuinely different shapes — a table against a
// series with a waterfall attached — and squeezing them into one command would
// mean half the flags being invalid half the time.
func runTransactions(c *context_, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, "transactions: expected list or show")
		return ExitUsage
	}
	switch args[0] {
	case "list":
		return runTransactionsList(c, args[1:])
	case "show":
		return runTransactionsShow(c, args[1:])
	default:
		fmt.Fprintf(c.stderr, "transactions: unknown subcommand %q\n", args[0])
		return ExitUsage
	}
}

// samplingPayload mirrors what the API says about how much it kept.
type samplingPayload struct {
	ServerRate    float64 `json:"server_rate"`
	Stored        int64   `json:"stored"`
	Received      int64   `json:"received"`
	EffectiveRate float64 `json:"effective_rate"`
}

type transactionRow struct {
	Transaction string  `json:"transaction"`
	Count       int64   `json:"count"`
	Failed      int64   `json:"failed"`
	FailRate    float64 `json:"fail_rate"`
	Sampled     int64   `json:"sampled"`
	P50         float64 `json:"p50_ms"`
	P95         float64 `json:"p95_ms"`
	P99         float64 `json:"p99_ms"`
	MeanMS      float64 `json:"mean_ms"`
	MinMS       float64 `json:"min_ms"`
	MaxMS       float64 `json:"max_ms"`
}

type transactionsPayload struct {
	Project     projectRef        `json:"project"`
	Range       statsRangePayload `json:"range"`
	Sort        string            `json:"sort"`
	Total       int64             `json:"total"`
	TotalFailed int64             `json:"total_failed"`
	Sampling    samplingPayload   `json:"sampling"`
	Rows        []transactionRow  `json:"transactions"`
}

func runTransactionsList(c *context_, args []string) int {
	flags := newFlagSet(c, "transactions list")
	projectID := flags.Int64("project", 0, "project id (required)")
	from := flags.String("from", "", "start of the range: 2006-01-02, 2006-01-02T15 or RFC 3339 (default: 24h ago)")
	to := flags.String("to", "", "end of the range, same formats (default: now)")
	sortBy := flags.String("sort", "", "p95, count or fail (default p95)")
	limit := flags.Int("limit", 0, "how many transactions to return")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *projectID <= 0 {
		fmt.Fprintln(c.stderr, "transactions list: -project is required and must be positive")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	extra := map[string]string{}
	if *sortBy != "" {
		extra["sort"] = *sortBy
	}
	if *limit > 0 {
		extra["limit"] = strconv.Itoa(*limit)
	}

	var payload transactionsPayload
	path := "/projects/" + strconv.FormatInt(*projectID, 10) + "/transactions" +
		rangeQuery(*from, *to, extra)
	if err := client.do(c.ctx, http.MethodGet, path, nil, &payload); err != nil {
		return c.fail(err)
	}
	return c.emit(*remote.asJSON, payload, renderTransactions(&payload))
}

// renderTransactions prints the table a terminal can read.
//
// The sampling line is printed even when nothing was sampled, and especially
// then: "0 of 12 340 traces stored" is the sentence that stops somebody
// concluding the product lost their data (ADR 021).
func renderTransactions(payload *transactionsPayload) string {
	if len(payload.Rows) == 0 {
		return "nothing in this range"
	}

	var text strings.Builder
	fmt.Fprintf(&text, "%s — %d transactions over %d hours, sorted by %s\n",
		payload.Project.Name, payload.Total, payload.Range.Hours, payload.Sort)
	fmt.Fprintf(&text, "%d of %d traces stored (%.1f%%; server rate %.2f)\n\n",
		payload.Sampling.Stored, payload.Sampling.Received,
		payload.Sampling.EffectiveRate*100, payload.Sampling.ServerRate)

	for index := range payload.Rows {
		row := &payload.Rows[index]
		fmt.Fprintf(&text, "%s\t%d\tp50 %s\tp95 %s\tp99 %s\tfail %.1f%%\n",
			row.Transaction, row.Count,
			renderMillis(row.P50), renderMillis(row.P95), renderMillis(row.P99),
			row.FailRate*100)
	}
	return strings.TrimRight(text.String(), "\n")
}

// renderMillis prints a duration the way somebody reads one, rather than as
// six decimal places of a millisecond.
func renderMillis(ms float64) string {
	switch {
	case ms >= 1000:
		return fmt.Sprintf("%.2fs", ms/1000)
	case ms >= 10:
		return fmt.Sprintf("%.0fms", ms)
	default:
		return fmt.Sprintf("%.1fms", ms)
	}
}

type transactionPoint struct {
	Bucket   string  `json:"bucket"`
	Count    int64   `json:"count"`
	Failed   int64   `json:"failed"`
	Sampled  int64   `json:"sampled"`
	P50      float64 `json:"p50_ms"`
	P95      float64 `json:"p95_ms"`
	P99      float64 `json:"p99_ms"`
	FailRate float64 `json:"fail_rate"`
}

type spanPayload struct {
	SpanID       string    `json:"span_id"`
	ParentSpanID string    `json:"parent_span_id"`
	Op           string    `json:"op"`
	Description  string    `json:"description"`
	Status       string    `json:"status"`
	Start        time.Time `json:"start"`
	End          time.Time `json:"end"`
	DurationMS   float64   `json:"duration_ms"`
}

type tracePayload struct {
	TraceID     string        `json:"trace_id"`
	Transaction string        `json:"transaction"`
	Timestamp   time.Time     `json:"timestamp"`
	DurationMS  int64         `json:"duration_ms"`
	Status      string        `json:"status"`
	Op          string        `json:"op"`
	Spans       []spanPayload `json:"spans"`
}

type transactionSeriesPayload struct {
	Project     projectRef         `json:"project"`
	Transaction string             `json:"transaction"`
	Range       statsRangePayload  `json:"range"`
	Resolution  string             `json:"resolution"`
	Total       int64              `json:"total"`
	Summary     transactionRow     `json:"summary"`
	Points      []transactionPoint `json:"series"`
	Examples    []tracePayload     `json:"examples"`
}

func runTransactionsShow(c *context_, args []string) int {
	flags := newFlagSet(c, "transactions show")
	projectID := flags.Int64("project", 0, "project id (required)")
	name := flags.String("name", "", "transaction name (required)")
	from := flags.String("from", "", "start of the range (default: 24h ago)")
	to := flags.String("to", "", "end of the range (default: now)")
	resolution := flags.String("resolution", "", "minute or hour (default minute)")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *projectID <= 0 {
		fmt.Fprintln(c.stderr, "transactions show: -project is required and must be positive")
		return ExitUsage
	}
	if strings.TrimSpace(*name) == "" {
		fmt.Fprintln(c.stderr, "transactions show: -name is required")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	extra := map[string]string{}
	if *resolution != "" {
		extra["resolution"] = *resolution
	}

	var payload transactionSeriesPayload
	// The name goes in the path because it identifies the resource, so it is
	// escaped as one segment: a transaction is called "GET /api/checkout"
	// more often than not, and an unescaped slash would silently address a
	// different route.
	path := "/projects/" + strconv.FormatInt(*projectID, 10) +
		"/transactions/" + url.PathEscape(*name) + "/series" + rangeQuery(*from, *to, extra)
	if err := client.do(c.ctx, http.MethodGet, path, nil, &payload); err != nil {
		return c.fail(err)
	}
	return c.emit(*remote.asJSON, payload, renderTransactionSeries(&payload))
}

func renderTransactionSeries(payload *transactionSeriesPayload) string {
	var text strings.Builder
	fmt.Fprintf(&text, "%s — %d transactions, p50 %s, p95 %s, p99 %s, fail %.1f%%\n\n",
		payload.Transaction, payload.Total,
		renderMillis(payload.Summary.P50), renderMillis(payload.Summary.P95),
		renderMillis(payload.Summary.P99), payload.Summary.FailRate*100)

	for index := range payload.Points {
		point := &payload.Points[index]
		if point.Count == 0 {
			// Quiet buckets are in the JSON, because a chart needs them, and
			// out of the text, because a terminal full of zeroes hides the
			// minute that was not one.
			continue
		}
		fmt.Fprintf(&text, "%s\t%d\tp95 %s\tfail %d\n",
			point.Bucket, point.Count, renderMillis(point.P95), point.Failed)
	}
	if payload.Total == 0 {
		text.WriteString("nothing in this range\n")
	}

	if len(payload.Examples) > 0 {
		text.WriteString("\nstored traces, slowest first:\n")
		for index := range payload.Examples {
			example := &payload.Examples[index]
			fmt.Fprintf(&text, "  %s\t%s\t%d spans\n",
				example.TraceID, renderMillis(float64(example.DurationMS)), len(example.Spans))
		}
	}
	return strings.TrimRight(text.String(), "\n")
}

// runTraces dispatches the trace subcommands.
func runTraces(c *context_, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, "traces: expected show")
		return ExitUsage
	}
	switch args[0] {
	case "show":
		return runTracesShow(c, args[1:])
	default:
		fmt.Fprintf(c.stderr, "traces: unknown subcommand %q\n", args[0])
		return ExitUsage
	}
}

func runTracesShow(c *context_, args []string) int {
	flags := newFlagSet(c, "traces show")
	projectID := flags.Int64("project", 0, "project id (required)")
	traceID := flags.String("trace", "", "trace id (required)")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *projectID <= 0 {
		fmt.Fprintln(c.stderr, "traces show: -project is required and must be positive")
		return ExitUsage
	}
	if strings.TrimSpace(*traceID) == "" {
		fmt.Fprintln(c.stderr, "traces show: -trace is required")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	var payload tracePayload
	path := "/projects/" + strconv.FormatInt(*projectID, 10) + "/traces/" + url.PathEscape(*traceID)
	if err := client.do(c.ctx, http.MethodGet, path, nil, &payload); err != nil {
		return c.fail(err)
	}
	return c.emit(*remote.asJSON, payload, renderTrace(&payload))
}

// renderTrace draws the waterfall in a terminal.
//
// A bar per span, positioned and scaled against the root's window, because a
// list of durations is not a waterfall: what a reader is looking for is which
// span the gap is in, and that is a question about position rather than about
// length.
func renderTrace(payload *tracePayload) string {
	var text strings.Builder
	fmt.Fprintf(&text, "%s\n%s  %s  %s\n\n",
		payload.TraceID, payload.Transaction,
		renderMillis(float64(payload.DurationMS)), payload.Status)

	if len(payload.Spans) == 0 {
		text.WriteString("no spans stored for this trace\n")
		return strings.TrimRight(text.String(), "\n")
	}

	start := payload.Spans[0].Start
	total := payload.Spans[0].DurationMS
	if total <= 0 {
		total = 1
	}

	const width = 40
	for index := range payload.Spans {
		span := &payload.Spans[index]
		offset := int(float64(span.Start.Sub(start)) / float64(time.Millisecond) / total * width)
		length := int(span.DurationMS / total * width)
		if length < 1 {
			// A span faster than the resolution of the bar still gets a mark:
			// an invisible span reads as a missing one.
			length = 1
		}
		if offset < 0 {
			offset = 0
		}
		if offset+length > width {
			length = max(width-offset, 1)
		}

		label := span.Op
		if span.Description != "" {
			label += " " + span.Description
		}
		fmt.Fprintf(&text, "%s%s%s %8s  %s\n",
			strings.Repeat(" ", offset), strings.Repeat("█", length),
			strings.Repeat(" ", width-offset-length),
			renderMillis(span.DurationMS), strings.TrimSpace(label))
	}
	return strings.TrimRight(text.String(), "\n")
}
