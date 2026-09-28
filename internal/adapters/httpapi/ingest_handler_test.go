package httpapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/antoniojosev/trapline/internal/domain"
)

// A realistic envelope, in the shape a Python SDK puts on the wire.
func pythonEnvelope(dsn domain.DSN, message string) string {
	return pythonEnvelopeIn(dsn, message, "production")
}

// pythonEnvelopeIn builds the same envelope for a named environment.
//
// The environment is a parameter rather than something a caller rewrites in
// the finished string: the item header carries a byte length, and editing the
// payload afterwards desynchronises it into a malformed envelope and a
// baffling failure.
func pythonEnvelopeIn(dsn domain.DSN, message, environment string) string {
	event := fmt.Sprintf(`{
		"event_id":"9ec79c33ec9942ab8353589fcb2e04dc",
		"timestamp":"2026-08-24T10:00:00Z",
		"platform":"python",
		"level":"error",
		"release":"myapp@2.3.1",
		"environment":%q,
		"exception":{"values":[{"type":"ValueError","value":%q,"stacktrace":{"frames":[
			{"filename":"app/views.py","abs_path":"/srv/app/views.py","function":"checkout","lineno":42,"in_app":true}
		]}}]},
		"tags":{"server":"web-01"},
		"request":{"url":"/checkout","headers":{"Authorization":"Bearer sk_live_supersecret","Accept":"application/json"}},
		"extra":{"order_total":19.99,"password":"hunter2"}
	}`, environment, message)
	// Compacted properly rather than by stripping whitespace: doing the
	// latter also eats the spaces inside string values, which quietly changes
	// the data under test.
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(event)); err != nil {
		panic("the test's own event payload is not valid JSON: " + err.Error())
	}
	event = compact.String()

	return fmt.Sprintf("{\"event_id\":\"9ec79c33ec9942ab8353589fcb2e04dc\",\"dsn\":%q}\n"+
		"{\"type\":\"event\",\"length\":%d}\n%s\n", dsn.String(), len(event), event)
}

// sendEnvelope posts an envelope exactly as an SDK would: the protocol's path,
// the protocol's auth header, no cookie and no CSRF header.
func sendEnvelope(t *testing.T, baseURL string, dsn domain.DSN, body, encoding string) *http.Response {
	t.Helper()

	var payload *bytes.Reader
	if encoding == "gzip" {
		var buffer bytes.Buffer
		writer := gzip.NewWriter(&buffer)
		if _, err := writer.Write([]byte(body)); err != nil {
			t.Fatalf("compressing: %v", err)
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("closing: %v", err)
		}
		payload = bytes.NewReader(buffer.Bytes())
	} else {
		payload = bytes.NewReader([]byte(body))
	}

	url := fmt.Sprintf("%s/api/%d/envelope/", baseURL, dsn.ProjectID)
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, payload)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	request.Header.Set("Content-Type", "application/x-sentry-envelope")
	request.Header.Set("X-Sentry-Auth",
		fmt.Sprintf("Sentry sentry_version=7, sentry_client=sentry.python/2.0.0, sentry_key=%s", dsn.PublicKey))
	if encoding != "" {
		request.Header.Set("Content-Encoding", encoding)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("sending envelope: %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

// createProjectForIngest sets the server up and returns a usable DSN.
func createProjectForIngest(t *testing.T, client *client) domain.DSN {
	t.Helper()
	client.setUpAndLogIn()

	var project projectResponse
	client.decode(client.do(http.MethodPost, api("/projects"), createProjectRequest{Name: "venekambio"}), &project)

	dsn, err := domain.ParseDSN(project.DSN)
	if err != nil {
		t.Fatalf("parsing the DSN the panel gave us: %v", err)
	}
	return dsn
}

func TestAnEventBecomesAnIssue(t *testing.T) {
	// The end-to-end claim of the whole product: point an SDK's DSN here and
	// the error shows up, with nothing else changed.
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	response := sendEnvelope(t, server.URL, dsn, pythonEnvelope(dsn, "invalid amount"), "")
	if response.StatusCode != http.StatusOK {
		body := make([]byte, 512)
		n, _ := response.Body.Read(body)
		t.Fatalf("ingest status = %d: %s", response.StatusCode, body[:n])
	}

	var accepted struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&accepted); err != nil {
		t.Fatalf("decoding the ingest response: %v", err)
	}
	if len(accepted.ID) != 32 {
		t.Errorf("event id = %q, want 32 hex characters as the protocol specifies", accepted.ID)
	}
}

func TestRepeatedEventsBecomeOneIssue(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	// The same error five times, with a value that differs each time — which
	// is what makes this a grouping test and not a counting test.
	for i := range 5 {
		body := pythonEnvelope(dsn, fmt.Sprintf("invalid amount %d", i*1000))
		if got := sendEnvelope(t, server.URL, dsn, body, "").StatusCode; got != http.StatusOK {
			t.Fatalf("event %d: status = %d", i, got)
		}
	}

	issues := listIssues(t, client, dsn.ProjectID)
	if len(issues) != 1 {
		t.Fatalf("got %d issues from five occurrences of one error, want 1", len(issues))
	}
	if issues[0].Times != 5 {
		t.Errorf("Times = %d, want 5", issues[0].Times)
	}
	if issues[0].Title != "ValueError: invalid amount 0" {
		t.Errorf("Title = %q", issues[0].Title)
	}
	if issues[0].Culprit != "app/views.py in checkout" && issues[0].Culprit != "/srv/app/views.py in checkout" {
		t.Errorf("Culprit = %q", issues[0].Culprit)
	}
}

func TestSecretsAreScrubbedBeforeStorage(t *testing.T) {
	// The timing is the point: scrubbing at display time would mean the secret
	// is already in the database and the backup.
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	sendEnvelope(t, server.URL, dsn, pythonEnvelope(dsn, "invalid amount"), "")

	issues := listIssues(t, client, dsn.ProjectID)
	if len(issues) != 1 {
		t.Fatalf("got %d issues", len(issues))
	}
	events := listEvents(t, client, dsn.ProjectID, issues[0].ID)
	if len(events) != 1 {
		t.Fatalf("got %d events", len(events))
	}

	stored := string(events[0].Payload)
	for _, secret := range []string{"sk_live_supersecret", "hunter2"} {
		if strings.Contains(stored, secret) {
			t.Errorf("%q reached storage unscrubbed", secret)
		}
	}
	// And the context that is not a secret has to survive, or a scrubbed
	// report is useless.
	for _, kept := range []string{"/checkout", "order_total", "views.py"} {
		if !strings.Contains(stored, kept) {
			t.Errorf("%q was lost from the stored payload", kept)
		}
	}
	if !strings.Contains(stored, domain.Redacted) {
		t.Error("nothing was marked as redacted; a reader cannot tell the field was removed")
	}
}

func TestGzippedEnvelopesAreAccepted(t *testing.T) {
	// Every official SDK compresses by default.
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	if got := sendEnvelope(t, server.URL, dsn, pythonEnvelope(dsn, "comprimido"), "gzip").StatusCode; got != http.StatusOK {
		t.Fatalf("status = %d", got)
	}
	if issues := listIssues(t, client, dsn.ProjectID); len(issues) != 1 {
		t.Errorf("got %d issues from a gzipped envelope", len(issues))
	}
}

func TestIngestAuthentication(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	t.Run("a wrong key is refused", func(t *testing.T) {
		wrong := dsn
		wrong.PublicKey = "ffffffffffffffffffffffffffffffff"
		if got := sendEnvelope(t, server.URL, wrong, pythonEnvelope(dsn, "x"), "").StatusCode; got != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", got)
		}
	})

	t.Run("a key from another project is refused", func(t *testing.T) {
		// The path says one project and the credential says another. Trusting
		// the path would let any valid key write into any project.
		var other projectResponse
		client.decode(client.do(http.MethodPost, api("/projects"), createProjectRequest{Name: "otro"}), &other)

		mismatched := dsn
		mismatched.ProjectID = other.ID
		if got := sendEnvelope(t, server.URL, mismatched, pythonEnvelope(dsn, "x"), "").StatusCode; got != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", got)
		}
	})

	t.Run("no key at all is refused", func(t *testing.T) {
		url := fmt.Sprintf("%s/api/%d/envelope/", server.URL, dsn.ProjectID)
		request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url,
			strings.NewReader(pythonEnvelope(dsn, "x")))
		if err != nil {
			t.Fatalf("building request: %v", err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("sending: %v", err)
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", response.StatusCode)
		}
	})
}

func TestDisabledCategoriesGetBackpressure(t *testing.T) {
	// A fresh project is the minimum profile: errors only. A transaction must
	// come back with the protocol's own rate-limit header, which is what makes
	// a switched-off subsystem free on the wire rather than merely on disk.
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	body := fmt.Sprintf("{\"dsn\":%q}\n{\"type\":\"transaction\"}\n{\"a\":1}\n", dsn.String())
	response := sendEnvelope(t, server.URL, dsn, body, "")

	if response.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", response.StatusCode)
	}
	header := response.Header.Get("X-Sentry-Rate-Limits")
	if !strings.Contains(header, "transaction") {
		t.Errorf("X-Sentry-Rate-Limits = %q, want it to name the transaction category", header)
	}
	if response.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After header on a 429")
	}
}

func TestUnknownItemTypesDoNotCostTheEnvelope(t *testing.T) {
	// ADR 002: an SDK that starts sending something new must not break an
	// installation that has not been upgraded.
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	event := `{"exception":{"values":[{"type":"ValueError","value":"real"}]}}`
	body := fmt.Sprintf("{\"dsn\":%q}\n", dsn.String()) +
		"{\"type\":\"replay_recording_from_2029\",\"length\":7}\n{\"b\":2}\n" +
		fmt.Sprintf("{\"type\":\"event\",\"length\":%d}\n%s\n", len(event), event)

	if got := sendEnvelope(t, server.URL, dsn, body, "").StatusCode; got != http.StatusOK {
		t.Fatalf("status = %d, want the known item to still be accepted", got)
	}
	if issues := listIssues(t, client, dsn.ProjectID); len(issues) != 1 {
		t.Errorf("got %d issues; the event beside an unknown item was lost", len(issues))
	}
}

func TestMalformedEnvelopesAreRejectedCleanly(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	for name, body := range map[string]string{
		"empty":           "",
		"not an envelope": "no soy un envelope",
		"header not JSON": "{{{\n",
		"length overruns": "{}\n{\"type\":\"event\",\"length\":99999}\nshort\n",
	} {
		t.Run(name, func(t *testing.T) {
			response := sendEnvelope(t, server.URL, dsn, body, "")
			if response.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", response.StatusCode)
			}
			// SDKs parse this endpoint's errors, and the protocol's shape uses
			// "detail" rather than this API's "error".
			var payload map[string]string
			if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
				t.Fatalf("the error body is not JSON: %v", err)
			}
			if payload["detail"] == "" {
				t.Errorf("body = %v, want a detail field", payload)
			}
		})
	}
}
