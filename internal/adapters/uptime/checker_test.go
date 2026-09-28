package uptime_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/adapters/uptime"
	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ssrfguard"
)

var checkedAt = time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

// Every target here is an httptest server, which lives on loopback — an
// address the guard refuses by default. That is not an inconvenience the tests
// work around, it is half of what they check: the permissive checker below
// needs *both* opt-ins, and the last two tests are what happens without them.
func permissiveChecker() *uptime.Checker {
	return uptime.New(ssrfguard.New(nil, true), "test")
}

func monitorFor(t *testing.T, server *httptest.Server, mutate func(*domain.UptimeMonitor)) *domain.UptimeMonitor {
	t.Helper()
	monitor := domain.UptimeMonitor{
		ProjectID: 1, Name: "api", URL: server.URL, Method: "GET",
		IntervalSeconds: 30, Enabled: true, FollowRedirects: true, AllowPrivate: true,
	}
	if mutate != nil {
		mutate(&monitor)
	}
	built, err := domain.NewUptimeMonitor(&monitor, checkedAt)
	if err != nil {
		t.Fatalf("building a monitor: %v", err)
	}
	return &built
}

func TestCheckASucceedingTarget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	result := permissiveChecker().Check(context.Background(),
		monitorFor(t, server, nil), checkedAt)

	if !result.OK {
		t.Fatalf("a healthy target was judged down: %s", result.Error)
	}
	if result.StatusCode != http.StatusOK {
		t.Errorf("status = %d", result.StatusCode)
	}
	if !result.At.Equal(checkedAt) {
		t.Errorf("the result is timestamped %s, want the injected clock", result.At)
	}
}

// TestCheckIdentifiesItself: an unattended request that arrives with no
// User-Agent is one somebody eventually blocks without knowing what it was.
func TestCheckIdentifiesItself(t *testing.T) {
	var agent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent = r.UserAgent()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	permissiveChecker().Check(context.Background(), monitorFor(t, server, nil), checkedAt)
	if !strings.HasPrefix(agent, "trapline-uptime/") {
		t.Errorf("the check identified itself as %q", agent)
	}
}

func TestCheckUsesTheConfiguredMethod(t *testing.T) {
	var method string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	result := permissiveChecker().Check(context.Background(),
		monitorFor(t, server, func(m *domain.UptimeMonitor) { m.Method = "HEAD" }), checkedAt)

	if !result.OK {
		t.Fatalf("a HEAD check failed: %s", result.Error)
	}
	if method != http.MethodHead {
		t.Errorf("the request used %s", method)
	}
}

func TestCheckFailsOnAnUnexpectedStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	result := permissiveChecker().Check(context.Background(),
		monitorFor(t, server, nil), checkedAt)

	if result.OK {
		t.Fatal("a 500 was judged healthy")
	}
	if result.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d", result.StatusCode)
	}
	if !strings.Contains(result.Error, "200–299") {
		t.Errorf("the reason does not say what was expected: %q", result.Error)
	}
}

// TestCheckReadsTheBody is the gate's last case: a 200 whose body does not
// carry what it should is a failure, because a 200 from an error page is still
// a 200.
func TestCheckReadsTheBody(t *testing.T) {
	body := `{"database":"unreachable"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	result := permissiveChecker().Check(context.Background(),
		monitorFor(t, server, func(m *domain.UptimeMonitor) {
			m.ExpectedBodySubstring = `"database":"ok"`
		}), checkedAt)

	if result.OK {
		t.Fatal("a 200 whose body says the database is down was judged healthy")
	}
	if result.StatusCode != http.StatusOK {
		t.Errorf("status = %d, and the status really was 200", result.StatusCode)
	}
	if !strings.Contains(result.Error, "database") {
		t.Errorf("the reason does not name the substring: %q", result.Error)
	}
}

func TestCheckPassesOnAMatchingBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"database":"ok","queue":"ok"}`))
	}))
	defer server.Close()

	result := permissiveChecker().Check(context.Background(),
		monitorFor(t, server, func(m *domain.UptimeMonitor) {
			m.ExpectedBodySubstring = `"database":"ok"`
		}), checkedAt)

	if !result.OK {
		t.Fatalf("a matching body was judged a failure: %s", result.Error)
	}
}

func TestCheckReportsAnUnreachableTarget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	monitor := monitorFor(t, server, nil)
	// Closed before the check: this is what `docker stop` looks like from
	// here, and it is the case the gate reproduces with a container.
	server.Close()

	result := permissiveChecker().Check(context.Background(), monitor, checkedAt)

	if result.OK {
		t.Fatal("a target that is not listening was judged healthy")
	}
	if result.StatusCode != 0 {
		t.Errorf("status = %d, want zero: nothing answered", result.StatusCode)
	}
	if result.Error == "" {
		t.Fatal("nothing was recorded about why the check failed")
	}
	// The reason must not be the whole URL again: it is already on the
	// monitor, and repeating it pushes the cause off the end of a message.
	if strings.HasPrefix(result.Error, "Get \"") {
		t.Errorf("the error was not unwrapped: %q", result.Error)
	}
}

func TestCheckTimesOut(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer func() { close(release); server.Close() }()

	result := permissiveChecker().Check(context.Background(),
		monitorFor(t, server, func(m *domain.UptimeMonitor) {
			m.TimeoutSeconds = 1
		}), checkedAt)

	if result.OK {
		t.Fatal("a target that never answered was judged healthy")
	}
	if !strings.Contains(result.Error, "timed out") {
		t.Errorf("the reason is %q, want it to say it timed out", result.Error)
	}
}

func TestCheckFollowsRedirects(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("arrived"))
	}))
	defer final.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer redirector.Close()

	result := permissiveChecker().Check(context.Background(),
		monitorFor(t, redirector, func(m *domain.UptimeMonitor) {
			m.ExpectedBodySubstring = "arrived"
		}), checkedAt)

	if !result.OK {
		t.Fatalf("a followed redirect did not arrive: %s", result.Error)
	}
}

// TestCheckDoesNotFollowRedirectsWhenAskedNotTo: the 3xx is the answer, and
// it is judged against the expected range — which is what "this URL should
// answer directly" means.
func TestCheckDoesNotFollowRedirects(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("arrived"))
	}))
	defer final.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer redirector.Close()

	result := permissiveChecker().Check(context.Background(),
		monitorFor(t, redirector, func(m *domain.UptimeMonitor) {
			m.FollowRedirects = false
		}), checkedAt)

	if result.OK {
		t.Fatal("a 302 was judged healthy against an expectation of 200–299")
	}
	if result.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want 302", result.StatusCode)
	}
}

// TestCheckRevalidatesEveryRedirect is the second half of the SSRF guard, and
// the half that is easy to leave out: the Location header is a URL chosen by
// the target, so a target that answers 302 has chosen where this server goes
// next.
//
// The proof is a hop that this package refuses on its own terms rather than an
// address the guard refuses, because both opt-ins are on here and with them on
// no address is refused — which is exactly the point of them. Credentials in a
// URL are rejected by ParseTarget at every hop, so a redirect carrying them
// only fails if the hop was parsed and checked at all.
func TestCheckRevalidatesEveryRedirect(t *testing.T) {
	reached := false
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer final.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, strings.Replace(final.URL, "http://", "http://root:hunter2@", 1),
			http.StatusFound)
	}))
	defer redirector.Close()

	result := permissiveChecker().Check(context.Background(),
		monitorFor(t, redirector, nil), checkedAt)

	if result.OK {
		t.Fatal("a redirect was followed without being checked")
	}
	if reached {
		t.Fatal("the redirect was followed to a hop that should have been refused")
	}
	if !strings.Contains(result.Error, "credentials") {
		t.Errorf("the reason is %q, want it to name what was wrong with the hop", result.Error)
	}
}

// TestCheckStopsAfterTooManyRedirects: every hop is validated, so this is not
// a security bound — it is what stops a redirect loop from spending the whole
// timeout on a target that will never answer.
func TestCheckStopsAfterTooManyRedirects(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, server.URL+"/again", http.StatusFound)
	}))
	defer server.Close()

	result := permissiveChecker().Check(context.Background(),
		monitorFor(t, server, nil), checkedAt)

	if result.OK {
		t.Fatal("a redirect loop was judged healthy")
	}
	if !strings.Contains(result.Error, "redirects") {
		t.Errorf("the reason is %q, want it to name the redirect limit", result.Error)
	}
}

// TestCheckRefusesALoopbackTargetWithoutTheOptIn is the guard, at the layer
// that actually connects: without both switches, no packet leaves.
func TestCheckRefusesLoopbackWithoutTheOptIn(t *testing.T) {
	reached := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Neither switch.
	checker := uptime.New(ssrfguard.New(nil, false), "test")
	monitor := monitorFor(t, server, func(m *domain.UptimeMonitor) { m.AllowPrivate = false })

	result := checker.Check(context.Background(), monitor, checkedAt)
	if result.OK {
		t.Fatal("a loopback target was checked with neither opt-in set")
	}
	if reached {
		t.Fatal("the request reached the target: the guard is advisory, not enforcing")
	}
	if !strings.Contains(result.Error, "loopback") {
		t.Errorf("the reason does not name the category: %q", result.Error)
	}
}

// TestCheckRefusesASchemeThatIsNotHTTP covers the monitor whose URL was
// tampered with in storage: the check refuses it rather than handing
// "file:///etc/passwd" to an HTTP client and hoping.
func TestCheckRefusesANonHTTPScheme(t *testing.T) {
	monitor := domain.UptimeMonitor{
		ProjectID: 1, Name: "bad", URL: "file:///etc/passwd", Method: "GET",
		IntervalSeconds: 30, TimeoutSeconds: 5, ExpectedStatusMin: 200, ExpectedStatusMax: 299,
	}
	result := permissiveChecker().Check(context.Background(), &monitor, checkedAt)

	if result.OK {
		t.Fatal("a file:// url was checked")
	}
	if !strings.Contains(result.Error, "http or https") {
		t.Errorf("the reason is %q", result.Error)
	}
}
