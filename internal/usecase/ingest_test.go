package usecase

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/engine"
	"github.com/antoniojosev/trapline/internal/envelope"
	"github.com/antoniojosev/trapline/internal/ports"
)

func newIngest(t *testing.T, limiter ports.RateLimiter) (*Ingest, *fakeIssues, context.Context) {
	t.Helper()
	issues := newFakeIssues()
	if limiter == nil {
		limiter = allowAll{}
	}
	return NewIngest(issues, limiter, domain.NewScrubber(nil, nil), fixedClock{now: testNow}, false),
		issues, context.Background()
}

// envelopeWith builds an envelope carrying one item of the given type.
func envelopeWith(itemType, payload string) string {
	return fmt.Sprintf("{}\n{\"type\":%q,\"length\":%d}\n%s\n", itemType, len(payload), payload)
}

const errorPayload = `{"exception":{"values":[{"type":"ValueError","value":"invalid amount","stacktrace":{"frames":[{"abs_path":"/srv/app/views.py","function":"checkout","in_app":true}]}}]},"release":"v1.0.0","environment":"production","tags":{"server":"web-01"}}`

func TestProcessStoresAnError(t *testing.T) {
	ingest, issues, ctx := newIngest(t, nil)

	result, err := ingest.Process(ctx, 1, strings.NewReader(envelopeWith("event", errorPayload)), envelope.Limits{}, ClientInfo{})
	if err != nil {
		t.Fatalf("processing: %v", err)
	}

	if result.Accepted != 1 || result.NewIssues != 1 {
		t.Errorf("result = %+v", result)
	}
	if len(issues.recorded) != 1 {
		t.Fatalf("recorded %d events", len(issues.recorded))
	}

	recorded := issues.recorded[0]
	if recorded.Observation.Title != "ValueError: invalid amount" {
		t.Errorf("Title = %q", recorded.Observation.Title)
	}
	if recorded.Observation.Culprit != "/srv/app/views.py in checkout" {
		t.Errorf("Culprit = %q", recorded.Observation.Culprit)
	}
	if recorded.Observation.Release != "v1.0.0" || recorded.Environment != "production" {
		t.Errorf("release/environment = %q/%q", recorded.Observation.Release, recorded.Environment)
	}
	if recorded.Tags["server"] != "web-01" {
		t.Errorf("tags = %v", recorded.Tags)
	}
	if recorded.Fingerprint == "" {
		t.Error("no fingerprint was computed")
	}
	// The received time comes from the injected clock, not the wall clock, so
	// it is assertable rather than merely plausible.
	if !recorded.ReceivedAt.Equal(testNow) {
		t.Errorf("ReceivedAt = %v", recorded.ReceivedAt)
	}
}

func TestOneBadItemDoesNotCostTheEnvelope(t *testing.T) {
	// An SDK batches several events into one request. Rejecting all of them
	// because one was malformed would lose reports that were perfectly fine.
	ingest, issues, ctx := newIngest(t, nil)

	body := "{}\n" +
		envelopeItem("event", "no soy json") +
		envelopeItem("event", errorPayload)

	result, err := ingest.Process(ctx, 1, strings.NewReader(body), envelope.Limits{}, ClientInfo{})
	if err != nil {
		t.Fatalf("processing: %v", err)
	}

	if result.Accepted != 1 {
		t.Errorf("Accepted = %d, want the good event kept", result.Accepted)
	}
	if result.Dropped["invalid_event"] != 1 {
		t.Errorf("Dropped = %v, want the bad one counted", result.Dropped)
	}
	if len(issues.recorded) != 1 {
		t.Errorf("recorded %d events", len(issues.recorded))
	}
}

func envelopeItem(itemType, payload string) string {
	return fmt.Sprintf("{\"type\":%q,\"length\":%d}\n%s\n", itemType, len(payload), payload)
}

func TestUnknownItemTypesAreCountedNotHidden(t *testing.T) {
	// Counting rather than silently discarding is what makes the gap visible
	// when someone asks why their transactions are not showing up.
	ingest, _, ctx := newIngest(t, nil)

	body := "{}\n" + envelopeItem("replay_recording_from_2029", `{"a":1}`) + envelopeItem("event", errorPayload)

	result, err := ingest.Process(ctx, 1, strings.NewReader(body), envelope.Limits{}, ClientInfo{})
	if err != nil {
		t.Fatalf("processing: %v", err)
	}
	if result.Accepted != 1 {
		t.Errorf("Accepted = %d", result.Accepted)
	}
	if result.Dropped["unknown_type:replay_recording_from_2029"] != 1 {
		t.Errorf("Dropped = %v", result.Dropped)
	}
}

func TestCategoriesNotYetStoredAreCounted(t *testing.T) {
	// Transactions and sessions are accepted by the protocol and stored in a
	// later phase. The count is what stops that gap from being invisible.
	ingest, _, ctx := newIngest(t, allowAll{})

	for _, itemType := range []string{"transaction", "session", "check_in"} {
		t.Run(itemType, func(t *testing.T) {
			result, err := ingest.Process(ctx, 1, strings.NewReader(envelopeWith(itemType, `{"a":1}`)), envelope.Limits{}, ClientInfo{})
			if err != nil {
				t.Fatalf("processing: %v", err)
			}
			if result.Accepted != 0 {
				t.Errorf("Accepted = %d", result.Accepted)
			}
			if len(result.Dropped) == 0 {
				t.Errorf("nothing was counted for %q", itemType)
			}
		})
	}
}

func TestRateLimitedCategoriesAreNamedForBackpressure(t *testing.T) {
	// The caller turns this list into the protocol's rate-limit header, which
	// is what makes a refused category free on the wire (ADR 005).
	ingest, issues, ctx := newIngest(t, refuseAll{})

	result, err := ingest.Process(ctx, 1, strings.NewReader(envelopeWith("event", errorPayload)), envelope.Limits{}, ClientInfo{})
	if err != nil {
		t.Fatalf("processing: %v", err)
	}

	if result.Accepted != 0 || len(issues.recorded) != 0 {
		t.Error("a refused event still reached storage")
	}
	if len(result.RateLimited) != 1 || result.RateLimited[0] != engine.CategoryError {
		t.Errorf("RateLimited = %v", result.RateLimited)
	}
}

func TestACategoryIsNamedOnceEvenWithManyItems(t *testing.T) {
	// The header names categories, not occurrences. Repeating one would make
	// it longer for no additional meaning.
	ingest, _, ctx := newIngest(t, refuseAll{})

	body := "{}\n" + strings.Repeat(envelopeItem("event", errorPayload), 5)

	result, err := ingest.Process(ctx, 1, strings.NewReader(body), envelope.Limits{}, ClientInfo{})
	if err != nil {
		t.Fatalf("processing: %v", err)
	}
	if len(result.RateLimited) != 1 {
		t.Errorf("RateLimited = %v, want one entry for five refused items", result.RateLimited)
	}
}

func TestSecretsNeverReachTheRepository(t *testing.T) {
	// Asserted at the port, which is the last point before persistence: if it
	// is clean here, it never touches the disk.
	ingest, issues, ctx := newIngest(t, nil)

	payload := `{"exception":{"values":[{"type":"E","value":"x"}]},"request":{"headers":{"Authorization":"Bearer sk_live_secret"}},"extra":{"password":"hunter2","order":"A-123"}}`

	if _, err := ingest.Process(ctx, 1, strings.NewReader(envelopeWith("event", payload)), envelope.Limits{}, ClientInfo{}); err != nil {
		t.Fatalf("processing: %v", err)
	}
	if len(issues.recorded) != 1 {
		t.Fatalf("recorded %d events", len(issues.recorded))
	}

	stored := string(issues.recorded[0].Payload)
	for _, secret := range []string{"sk_live_secret", "hunter2"} {
		if strings.Contains(stored, secret) {
			t.Errorf("%q reached the repository", secret)
		}
	}
	if !strings.Contains(stored, "A-123") {
		t.Error("harmless context was scrubbed away")
	}
}

func TestMalformedEnvelopesAreReportedAsSuch(t *testing.T) {
	ingest, _, ctx := newIngest(t, nil)

	if _, err := ingest.Process(ctx, 1, strings.NewReader("no soy un envelope"), envelope.Limits{}, ClientInfo{}); err == nil {
		t.Error("a malformed envelope was accepted")
	}
}

func TestFormatRateLimitHeader(t *testing.T) {
	cases := map[string]struct {
		categories []engine.Category
		want       string
	}{
		"none":    {nil, ""},
		"one":     {[]engine.Category{engine.CategoryError}, "300:error:organization"},
		"several": {[]engine.Category{engine.CategoryError, engine.CategoryTransaction}, "300:error;transaction:organization"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := FormatRateLimitHeader(tc.categories, 300); got != tc.want {
				t.Errorf("FormatRateLimitHeader = %q, want %q", got, tc.want)
			}
		})
	}
}

// The browser suite in the compatibility matrix found the gap these tests
// hold closed: a browser error arrived recording nothing about which browser
// it came from. The SDK does not put it in the payload — a page has no
// trustworthy name for itself — so the only source is the header the browser
// writes on every request.
const chromeUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

func TestTheBrowserIsRecordedFromTheRequest(t *testing.T) {
	ingest, issues, ctx := newIngest(t, nil)

	_, err := ingest.Process(ctx, 1, strings.NewReader(envelopeWith("event", errorPayload)),
		envelope.Limits{}, ClientInfo{UserAgent: chromeUserAgent})
	if err != nil {
		t.Fatalf("processing: %v", err)
	}
	if len(issues.recorded) != 1 {
		t.Fatalf("recorded %d events", len(issues.recorded))
	}

	stored := string(issues.recorded[0].Payload)
	if !strings.Contains(stored, `"browser":{"name":"Chrome","version":"141.0.0.0"}`) {
		t.Errorf("the stored payload records no browser: %s", stored)
	}
}

// A context the SDK sent wins. Somebody who set it deliberately knows
// something about their application that a header cannot say — an embedded
// WebView reporting the host app, for one.
func TestAnSDKsOwnBrowserContextIsNotOverwritten(t *testing.T) {
	ingest, issues, ctx := newIngest(t, nil)

	payload := `{"exception":{"values":[{"type":"Error","value":"boom"}]},` +
		`"contexts":{"browser":{"name":"Firefox","version":"131.0"}}}`
	_, err := ingest.Process(ctx, 1, strings.NewReader(envelopeWith("event", payload)),
		envelope.Limits{}, ClientInfo{UserAgent: chromeUserAgent})
	if err != nil {
		t.Fatalf("processing: %v", err)
	}

	stored := string(issues.recorded[0].Payload)
	if !strings.Contains(stored, `"name":"Firefox"`) {
		t.Errorf("the SDK's own browser context was overwritten by the header: %s", stored)
	}
	if strings.Contains(stored, `"name":"Chrome"`) {
		t.Errorf("the header's browser was recorded over the SDK's: %s", stored)
	}
}

// Every other client in the matrix sends a User-Agent naming itself, and a
// server-side SDK's is not a browser. Recording one would put a browser name
// on issues from a Python worker.
func TestANonBrowserUserAgentRecordsNothing(t *testing.T) {
	ingest, issues, ctx := newIngest(t, nil)

	_, err := ingest.Process(ctx, 1, strings.NewReader(envelopeWith("event", errorPayload)),
		envelope.Limits{}, ClientInfo{UserAgent: "sentry.python/2.68.1"})
	if err != nil {
		t.Fatalf("processing: %v", err)
	}
	if stored := string(issues.recorded[0].Payload); strings.Contains(stored, `"browser"`) {
		t.Errorf("a server-side SDK was recorded as a browser: %s", stored)
	}
}

// A context is not a tag, and only tags are filterable. Recording the browser
// and then being unable to ask "does this only happen in Safari?" of a listing
// was the gap this closes.
func TestTheBrowserIsAlsoATagSoItCanBeFilteredOn(t *testing.T) {
	ingest, issues, ctx := newIngest(t, nil)

	_, err := ingest.Process(ctx, 1, strings.NewReader(envelopeWith("event", errorPayload)),
		envelope.Limits{}, ClientInfo{UserAgent: chromeUserAgent})
	if err != nil {
		t.Fatalf("processing: %v", err)
	}

	tags := issues.recorded[0].Tags
	if tags["browser.name"] != "Chrome" {
		t.Errorf("browser.name = %q, want Chrome", tags["browser.name"])
	}
	if tags["browser"] != "Chrome 141.0.0.0" {
		t.Errorf("browser = %q, want the name and the version", tags["browser"])
	}
}

func TestABrowserContextWithNothingUsableInItBecomesNoTag(t *testing.T) {
	for name, payload := range map[string]string{
		"no name at all":     `{"exception":{"values":[{"type":"Error"}]},"contexts":{"browser":{"version":"17.4"}}}`,
		"not even an object": `{"exception":{"values":[{"type":"Error"}]},"contexts":{"browser":"Safari"}}`,
		"a name that is not a string": `{"exception":{"values":[{"type":"Error"}]},` +
			`"contexts":{"browser":{"name":42}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			ingest, issues, ctx := newIngest(t, nil)
			if _, err := ingest.Process(ctx, 1, strings.NewReader(envelopeWith("event", payload)),
				envelope.Limits{}, ClientInfo{}); err != nil {
				t.Fatalf("processing: %v", err)
			}
			tags := issues.recorded[0].Tags
			if value, present := tags["browser.name"]; present {
				t.Errorf("browser.name = %q, want no tag at all", value)
			}
			if value, present := tags["browser"]; present {
				t.Errorf("browser = %q, want no tag at all", value)
			}
		})
	}
}

// A browser with no version still names a browser, and that is the facet
// worth filtering by.
func TestABrowserWithNoVersionStillTagsItsName(t *testing.T) {
	ingest, issues, ctx := newIngest(t, nil)
	payload := `{"exception":{"values":[{"type":"Error"}]},"contexts":{"browser":{"name":"Safari"}}}`

	if _, err := ingest.Process(ctx, 1, strings.NewReader(envelopeWith("event", payload)),
		envelope.Limits{}, ClientInfo{}); err != nil {
		t.Fatalf("processing: %v", err)
	}

	tags := issues.recorded[0].Tags
	if tags["browser.name"] != "Safari" || tags["browser"] != "Safari" {
		t.Errorf("tags = %v", tags)
	}
}

// A tag the SDK set by hand wins, the same way its own contexts do.
func TestAnSDKsOwnBrowserTagIsNotOverwritten(t *testing.T) {
	ingest, issues, ctx := newIngest(t, nil)
	payload := `{"exception":{"values":[{"type":"Error"}]},"tags":{"browser.name":"Wry"}}`

	if _, err := ingest.Process(ctx, 1, strings.NewReader(envelopeWith("event", payload)),
		envelope.Limits{}, ClientInfo{UserAgent: chromeUserAgent}); err != nil {
		t.Fatalf("processing: %v", err)
	}
	if got := issues.recorded[0].Tags["browser.name"]; got != "Wry" {
		t.Errorf("browser.name = %q, want the SDK's own Wry", got)
	}
}

func TestTheFeedHearsWhatAnEnvelopeDidToItsIssues(t *testing.T) {
	ingest, _, ctx := newIngest(t, nil)
	feed := NewBroadcaster()
	ingest = ingest.WithFeed(feed)

	events, stop := feed.Subscribe(1)
	defer stop()

	// The same error twice: the first creates the issue, the second is
	// another occurrence of it.
	for range 2 {
		if _, err := ingest.Process(ctx, 1, strings.NewReader(envelopeWith("event", errorPayload)),
			envelope.Limits{}, ClientInfo{}); err != nil {
			t.Fatalf("processing: %v", err)
		}
	}

	kinds := make([]IssueEventKind, 0, 2)
	for range 2 {
		select {
		case event := <-events:
			kinds = append(kinds, event.Kind)
		default:
			t.Fatalf("only %d events reached the feed", len(kinds))
		}
	}
	if kinds[0] != IssueEventNew || kinds[1] != IssueEventUpdated {
		t.Errorf("kinds = %v, want [issue.new issue.updated]", kinds)
	}
}

// Nothing that was refused is announced. A panel that lit up for an event the
// database never took would be showing an issue nobody can open.
func TestTheFeedHearsNothingAboutARefusedEnvelope(t *testing.T) {
	ingest, _, ctx := newIngest(t, refuseAll{})
	feed := NewBroadcaster()
	ingest = ingest.WithFeed(feed)

	events, stop := feed.Subscribe(1)
	defer stop()

	if _, err := ingest.Process(ctx, 1, strings.NewReader(envelopeWith("event", errorPayload)),
		envelope.Limits{}, ClientInfo{}); err != nil {
		t.Fatalf("processing: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("%d events announced for an envelope that was rate limited", len(events))
	}
}
