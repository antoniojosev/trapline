package bench

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"testing"
)

// workload says which of the two database shapes an envelope exercises.
//
// They are separate because they are separate SQL. Creating an issue is a
// failed lookup followed by three INSERTs; appending to one is a hit followed
// by an UPDATE, an INSERT and a tag UPDATE. Measuring only the second — which
// is what any benchmark that sends the same error repeatedly does — would
// leave the INSERT path, the one a bad deploy hits hardest, unmeasured.
type workload int

const (
	// newIssue gives every event a fingerprint nothing has seen before.
	newIssue workload = iota
	// existingIssue sends the same error over and over, as a broken
	// production service actually does.
	existingIssue
)

func (w workload) String() string {
	if w == newIssue {
		return "new issue"
	}
	return "existing issue"
}

// buildEnvelopes prepares count gzipped envelopes ahead of the clock.
//
// Ahead of the clock on purpose: compressing is the SDK's work, and charging
// the server for it would understate throughput by whatever gzip costs. What
// the server pays for — decompression — is inside the measurement.
func buildEnvelopes(tb testing.TB, kind workload, offset, count int) [][]byte {
	tb.Helper()
	envelopes := make([][]byte, count)
	for index := range envelopes {
		envelopes[index] = gzipBytes(tb, envelopeFor(tb, kind, offset+index))
	}
	return envelopes
}

// envelopeFor renders one envelope in the protocol's framing.
func envelopeFor(tb testing.TB, kind workload, seq int) []byte {
	tb.Helper()
	payload := eventPayload(tb, kind, seq)

	var envelope bytes.Buffer
	fmt.Fprintf(&envelope, "{\"event_id\":\"%s\",\"sent_at\":\"2026-08-24T10:00:00Z\"}\n", eventID(seq))
	fmt.Fprintf(&envelope, "{\"type\":\"event\",\"content_type\":\"application/json\",\"length\":%d}\n", len(payload))
	envelope.Write(payload)
	envelope.WriteByte('\n')
	return envelope.Bytes()
}

// eventPayload builds a full exception event of the shape a real SDK sends.
//
// Everything here is present because it costs something on the ingest path and
// a real event has it: a chained exception and a deep stacktrace with source
// context drive grouping and JSON decoding, breadcrumbs and contexts drive the
// scrubber's map walk, and the whole ~9 KB drives the zstd compression of the
// stored payload. An empty document would measure the framing and none of the
// work.
//
// The secrets are not decoration either. Scrubbing runs before the write, so a
// payload with nothing to redact would skip the copying the scrubber does on
// every real event.
func eventPayload(tb testing.TB, kind workload, seq int) []byte {
	tb.Helper()

	// The identity-bearing fields. Grouping hashes the in-app frames plus the
	// exception type and a NORMALISED message, so varying digits is not enough
	// to make a distinct issue — the frame has to move. That is exactly why
	// the two workloads differ in the function name rather than in the value.
	function := "process_payment"
	if kind == newIssue {
		function = fmt.Sprintf("process_payment_v%d", seq)
	}

	frames := make([]any, 0, 12)
	for depth := range 12 {
		frames = append(frames, map[string]any{
			"filename":     fmt.Sprintf("app/services/billing/step_%d.py", depth),
			"abs_path":     fmt.Sprintf("/srv/app/services/billing/step_%d.py", depth),
			"function":     fmt.Sprintf("%s_step_%d", function, depth),
			"module":       fmt.Sprintf("app.services.billing.step_%d", depth),
			"lineno":       120 + depth,
			"colno":        4,
			"in_app":       depth >= 8,
			"context_line": "    charge = gateway.capture(order.total, idempotency_key=key)",
			"pre_context": []string{
				"    key = idempotency_key_for(order)",
				"    gateway = Gateway.for_currency(order.currency)",
				"    log.info('capturing', extra={'order': order.id})",
			},
			"post_context": []string{
				"    if charge.status != 'succeeded':",
				"        raise PaymentDeclined(charge.failure_reason)",
				"    return charge",
			},
		})
	}

	breadcrumbs := make([]any, 0, 20)
	for step := range 20 {
		breadcrumbs = append(breadcrumbs, map[string]any{
			"timestamp": "2026-08-24T09:59:5" + fmt.Sprint(step%10) + "Z",
			"type":      "http",
			"category":  "httplib",
			"level":     "info",
			"message":   fmt.Sprintf("GET https://api.gateway.test/v2/charges/%d", step),
			"data": map[string]any{
				"url":           fmt.Sprintf("https://api.gateway.test/v2/charges/%d", step),
				"method":        "GET",
				"status_code":   200,
				"duration_ms":   12 + step,
				"authorization": "Bearer sk_live_notarealsecret",
			},
		})
	}

	event := map[string]any{
		"event_id":    eventID(seq),
		"timestamp":   "2026-08-24T10:00:00.123456Z",
		"platform":    "python",
		"level":       "error",
		"logger":      "app.services.billing",
		"release":     "billing@4.11.2",
		"environment": "production",
		"server_name": "web-07",
		"transaction": "POST /api/checkout",
		"exception": map[string]any{"values": []any{
			// A chain, because real exceptions wrap. Only the last one
			// identifies the issue, and the decoder walks both.
			map[string]any{
				"type":   "ConnectionResetError",
				"value":  "[Errno 104] Connection reset by peer",
				"module": "builtins",
			},
			map[string]any{
				"type":       "PaymentDeclined",
				"value":      fmt.Sprintf("card declined for order %d", 480000+seq),
				"module":     "app.services.billing.errors",
				"stacktrace": map[string]any{"frames": frames},
			},
		}},
		"breadcrumbs": map[string]any{"values": breadcrumbs},
		"tags": map[string]string{
			"server_name":   "web-07",
			"runtime":       "CPython 3.13.1",
			"gateway":       "stripe",
			"region":        "sa-east-1",
			"customer_tier": []string{"free", "pro", "enterprise"}[seq%3],
		},
		"user": map[string]any{
			"id": 918273, "email": "cliente@example.test", "ip_address": "203.0.113.44",
		},
		"request": map[string]any{
			"url":    "https://shop.example.test/api/checkout",
			"method": "POST",
			"headers": map[string]any{
				"Authorization": "Bearer sk_live_notarealsecret",
				"Cookie":        "session=abcdef0123456789; cart=17",
				"User-Agent":    "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36",
				"Accept":        "application/json",
			},
			"data": map[string]any{"order_id": 480000 + seq, "total": 129.95, "currency": "USD"},
		},
		"contexts": map[string]any{
			"runtime": map[string]any{"name": "CPython", "version": "3.13.1"},
			"os":      map[string]any{"name": "Linux", "version": "6.6.87", "kernel_version": "6.6.87-generic"},
			"device":  map[string]any{"arch": "x86_64", "memory_size": 8589934592, "free_memory": 2147483648},
			"trace": map[string]any{
				"trace_id": "4c79f60c11214eb38604f4ae0781bfb2",
				"span_id":  "fa90fdead5f74052",
				"op":       "http.server",
			},
		},
		"extra": map[string]any{
			"order_total":   129.95,
			"retry_attempt": seq % 4,
			"api_key":       "sk_live_notarealsecret",
			"idempotency":   fmt.Sprintf("idem_%016x", seq),
			"cart_items":    []any{"sku-1188", "sku-2044", "sku-9931"},
		},
	}

	encoded, err := json.Marshal(event)
	if err != nil {
		tb.Fatalf("the benchmark's own event is not encodable: %v", err)
	}
	return encoded
}

// eventID renders the protocol's 32 hex characters.
func eventID(seq int) string { return fmt.Sprintf("%032x", seq) }

func gzipBytes(tb testing.TB, raw []byte) []byte {
	tb.Helper()
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(raw); err != nil {
		tb.Fatalf("compressing: %v", err)
	}
	if err := writer.Close(); err != nil {
		tb.Fatalf("closing the gzip writer: %v", err)
	}
	return compressed.Bytes()
}
