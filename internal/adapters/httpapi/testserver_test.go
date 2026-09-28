package httpapi

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/adapters/mcp"
	"github.com/antoniojosev/trapline/internal/adapters/sqlite"
	"github.com/antoniojosev/trapline/internal/adapters/webui"
	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/usecase"
	"github.com/antoniojosev/trapline/internal/wiring"
)

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// newStack builds the real application over a temporary database, through the
// same assembly production uses.
//
// Going through `wiring` rather than calling the constructors here is the
// whole point: the previous version of this helper wired the pieces its own
// way, and a configuration change that took effect immediately in production
// silently did not in tests. A test that assembles its own stack is a test of
// a program that never ships.
func newStack(t *testing.T) (*wiring.Stack, *sqlite.DB) {
	t.Helper()

	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "trapline.db"))
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return wiring.New(db, wiring.Options{
		Origin: domain.Origin{Scheme: "https", Host: "errors.example.com"},
		Clock:  systemClock{},
	}), db
}

// newTestServer serves the real application, panel included.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	stack, _ := newStack(t)
	return serve(t, stack, true)
}

// newTestServerWithTokens is newTestServer plus the token use case, for the
// tests that exercise bearer authentication.
func newTestServerWithTokens(t *testing.T) (*httptest.Server, *usecase.Tokens) {
	t.Helper()
	stack, _ := newStack(t)
	return serve(t, stack, false), stack.Tokens
}

func serve(t *testing.T, stack *wiring.Stack, withPanel bool) *httptest.Server {
	t.Helper()

	origin := domain.Origin{Scheme: "https", Host: "errors.example.com"}
	api := NewServer(stack.Auth, stack.Projects, stack.Tokens, stack.Ingest, stack.Issues, stack.Stats, origin, "test")
	api = api.WithJobs(stack.Scheduler)
	api = api.WithReleases(stack.Releases)
	api = api.WithFeed(stack.Feed)
	api = api.WithAlerts(stack.Alerts)
	api = api.WithDigest(stack.Digest, stack.Channels)
	api = api.WithCrons(stack.Crons)
	api = api.WithUptime(stack.Uptime)
	api = api.WithStatusPage(stack.StatusPage)
	api = api.WithTransactions(stack.Transactions)
	api = api.WithArtifacts(stack.Artifacts)

	api = api.WithHealth(stack.Health)
	api = api.WithSuspects(stack.Suspects)
	api = api.WithBundle(stack.Bundle)
	api = api.WithMCP(func(router http.Handler) http.Handler {
		return mcp.New(mcp.NewLoopbackCaller(router, "/api/"+APIVersion), "", "test").Handler()
	})

	if withPanel {
		panel, err := webui.Handler()
		if err != nil {
			t.Fatalf("loading the panel: %v", err)
		}
		api = api.WithPanel(panel)
	}

	server := httptest.NewServer(api.Handler())
	t.Cleanup(server.Close)
	return server
}

func newCookieJar(t *testing.T) *cookiejar.Jar {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("building cookie jar: %v", err)
	}
	return jar
}
