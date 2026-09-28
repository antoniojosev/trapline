package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

// openStream subscribes to a project's feed the way the panel does — a plain
// GET with the session cookie — and returns a reader positioned after the
// opening comment.
//
// The response body is deliberately not read with io.ReadAll anywhere in this
// file: a stream has no end, so anything that waits for one hangs.
func openStream(t *testing.T, c *client, projectID int64) (stream *bufio.Reader, stop func()) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+api("/projects/"+strconv.FormatInt(projectID, 10)+"/events/stream"), http.NoBody)
	if err != nil {
		cancel()
		t.Fatalf("building the stream request: %v", err)
	}

	response, err := c.http.Do(request) //nolint:bodyclose // closed by the returned stop function.
	if err != nil {
		cancel()
		t.Fatalf("opening the stream: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		cancel()
		_ = response.Body.Close()
		t.Fatalf("stream status = %d, want 200", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}

	reader := bufio.NewReader(response.Body)
	// The opening comment. Reading it here is what proves the response was
	// flushed rather than buffered until the connection closes — which for a
	// stream means never, and is the exact shape the ResponseWriter wrappers
	// caused before they learned to Unwrap.
	line, err := reader.ReadString('\n')
	if err != nil {
		cancel()
		_ = response.Body.Close()
		t.Fatalf("reading the stream's opening: %v", err)
	}
	if !strings.HasPrefix(line, "retry:") {
		t.Errorf("the stream opened with %q, want a retry directive", line)
	}

	return reader, func() {
		cancel()
		_ = response.Body.Close()
	}
}

// nextEvent reads until the next `event:`/`data:` pair, skipping comments and
// heartbeats.
func nextEvent(t *testing.T, reader *bufio.Reader) (string, streamEvent) {
	t.Helper()

	var kind string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading the stream: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, "event: "):
			kind = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var payload streamEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
				t.Fatalf("decoding a stream event: %v", err)
			}
			return kind, payload
		}
	}
}

func TestTheStreamAnnouncesANewIssueAndThenItsRegression(t *testing.T) {
	server := newTestServer(t)
	c := newClient(t, server)
	dsn := createProjectForIngest(t, c)

	reader, stop := openStream(t, c, dsn.ProjectID)
	defer stop()

	if got := sendEnvelope(t, server.URL, dsn, pythonEnvelope(dsn, "invalid amount"), "").StatusCode; got != http.StatusOK {
		t.Fatalf("ingest status = %d", got)
	}

	kind, event := nextEvent(t, reader)
	if kind != "issue.new" {
		t.Fatalf("kind = %q, want issue.new", kind)
	}
	if !strings.Contains(event.Title, "ValueError") {
		t.Errorf("title = %q", event.Title)
	}
	if event.ProjectID != dsn.ProjectID {
		t.Errorf("project_id = %d, want %d", event.ProjectID, dsn.ProjectID)
	}
	if event.IssueID == 0 {
		t.Error("the event names no issue, so nothing can be opened from it")
	}

	// Resolve it through the API, then send the same error again: what comes
	// back must be a regression and not another update, because that is the
	// distinction the whole feed exists to deliver.
	issues := listIssues(t, c, dsn.ProjectID)
	if len(issues) != 1 {
		t.Fatalf("listed %d issues", len(issues))
	}
	path := api("/projects/" + strconv.FormatInt(dsn.ProjectID, 10) + "/issues/" + strconv.FormatInt(issues[0].ID, 10) + "/status")
	if got := c.do(http.MethodPost, path, map[string]any{"status": "resolved"}).StatusCode; got != http.StatusOK {
		t.Fatalf("resolving returned %d", got)
	}

	if got := sendEnvelope(t, server.URL, dsn, pythonEnvelope(dsn, "invalid amount"), "").StatusCode; got != http.StatusOK {
		t.Fatalf("ingest status = %d", got)
	}
	kind, event = nextEvent(t, reader)
	if kind != "issue.regressed" {
		t.Fatalf("kind = %q, want issue.regressed", kind)
	}
	if event.Status != "unresolved" {
		t.Errorf("status = %q, want the reopened unresolved", event.Status)
	}
}

// A stream for one project must never carry another's, or the feed becomes a
// way to read every issue title in the installation from a project you were
// given access to.
func TestTheStreamCarriesOnlyItsOwnProject(t *testing.T) {
	server := newTestServer(t)
	c := newClient(t, server)
	dsn := createProjectForIngest(t, c)

	var other projectResponse
	c.decode(c.do(http.MethodPost, api("/projects"), createProjectRequest{Name: "other"}), &other)

	reader, stop := openStream(t, c, other.ID)
	defer stop()

	if got := sendEnvelope(t, server.URL, dsn, pythonEnvelope(dsn, "invalid amount"), "").StatusCode; got != http.StatusOK {
		t.Fatalf("ingest status = %d", got)
	}

	// Nothing should arrive. Proved by a deadline rather than by a sleep and
	// a length check: the read blocks until something comes or the clock runs
	// out, and only the second outcome is correct.
	//
	// The goroutine reports through a channel instead of failing the test
	// itself: it outlives the test body, and once the deferred stop cancels
	// the request its read fails with a cancellation that says nothing.
	arrived := make(chan string, 1)
	go func() {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(line, "event: ") {
				arrived <- strings.TrimSpace(strings.TrimPrefix(line, "event: "))
				return
			}
		}
	}()
	select {
	case kind := <-arrived:
		t.Fatalf("a %q event from another project reached this stream", kind)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestTheStreamRefusesWhatEveryOtherEndpointRefuses(t *testing.T) {
	server := newTestServer(t)
	c := newClient(t, server)
	dsn := createProjectForIngest(t, c)

	t.Run("a project nobody owns", func(t *testing.T) {
		response := c.do(http.MethodGet, api("/projects/9999/events/stream"), nil)
		if response.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404: an open stream on a project that does not "+
				"exist looks exactly like a working one on a quiet project", response.StatusCode)
		}
	})

	t.Run("no credential at all", func(t *testing.T) {
		anonymous := newClient(t, server)
		response := anonymous.do(http.MethodGet,
			api("/projects/"+strconv.FormatInt(dsn.ProjectID, 10)+"/events/stream"), nil)
		if response.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", response.StatusCode)
		}
	})
}

// The subscription has to go when the reader does, or every closed tab leaves
// a channel that ingestion keeps writing to for the life of the process.
func TestClosingTheStreamReleasesItsSubscription(t *testing.T) {
	stack, _ := newStack(t)
	server := serve(t, stack, false)
	c := newClient(t, server)
	dsn := createProjectForIngest(t, c)

	_, stop := openStream(t, c, dsn.ProjectID)
	if stack.Feed.Watching() != 1 {
		t.Fatalf("watching = %d, want 1", stack.Feed.Watching())
	}
	stop()

	// The server notices asynchronously, through the request context.
	deadline := time.Now().Add(2 * time.Second)
	for stack.Feed.Watching() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := stack.Feed.Watching(); got != 0 {
		t.Fatalf("watching = %d after the reader left, want 0", got)
	}
}

// A server can be built without a feed — that is what `WithFeed` being a
// setter means — and then the route has to say so rather than opening a stream
// that will never carry anything.
func TestAServerWithoutAFeedSaysSoRatherThanHangingOpen(t *testing.T) {
	stack, _ := newStack(t)

	origin := domain.Origin{Scheme: "https", Host: "errors.example.com"}
	// Built here rather than through the shared helper, which always mounts a
	// feed — the whole point is a server that has none.
	unfed := NewServer(stack.Auth, stack.Projects, stack.Tokens, stack.Ingest,
		stack.Issues, stack.Stats, origin, "test")
	server := httptest.NewServer(unfed.Handler())
	t.Cleanup(server.Close)

	c := newClient(t, server)
	dsn := createProjectForIngest(t, c)

	response := c.do(http.MethodGet,
		api("/projects/"+strconv.FormatInt(dsn.ProjectID, 10)+"/events/stream"), nil)
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", response.StatusCode)
	}
}

// A bearer token is the other credential the route accepts, and it is the one
// a script would use — an EventSource cannot set a header, but nothing says
// the only reader of this feed has to be a browser.
func TestTheStreamAcceptsABearerToken(t *testing.T) {
	server, tokens := newTestServerWithTokens(t)
	c := newClient(t, server)
	dsn := createProjectForIngest(t, c)

	_, issued, err := tokens.Create(
		context.Background(), "stream", []domain.Scope{domain.ScopeProjectsRead}, nil)
	if err != nil {
		t.Fatalf("minting a token: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		server.URL+api("/projects/"+strconv.FormatInt(dsn.ProjectID, 10)+"/events/stream"),
		http.NoBody)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+issued)

	response, err := http.DefaultClient.Do(request) //nolint:bodyclose // closed below.
	if err != nil {
		t.Fatalf("opening the stream: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading the stream's opening: %v", err)
	}
	if !strings.HasPrefix(line, "retry:") {
		t.Errorf("the stream opened with %q", line)
	}
}
