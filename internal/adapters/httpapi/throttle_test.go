package httpapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/antoniojosev/trapline/internal/clientip"
	"github.com/antoniojosev/trapline/internal/domain"
)

// newLimitedServer serves the real application with per-address limits chosen
// by the test.
func newLimitedServer(t *testing.T, trusted []string, ingestPerMinute, authPerMinute int) *httptest.Server {
	t.Helper()

	stack, _ := newStack(t)
	resolver, err := clientip.New(trusted)
	if err != nil {
		t.Fatalf("building the resolver: %v", err)
	}

	origin := domain.Origin{Scheme: "https", Host: "errors.example.com"}
	api := NewServer(stack.Auth, stack.Projects, stack.Tokens, stack.Ingest, stack.Issues, stack.Stats, origin, "test").
		WithIPLimits(resolver, ingestPerMinute, authPerMinute)

	server := httptest.NewServer(api.Handler())
	t.Cleanup(server.Close)
	return server
}

func TestAFloodOfLoginsIsRefusedBeforeItSpendsArgon2id(t *testing.T) {
	// The bug this throttle exists for. Every login attempt costs a 19 MiB
	// Argon2id working set (ADR 005), so an endpoint with no ceiling is a
	// memory-exhaustion primitive against a product whose entire pitch is that
	// it runs in 30 MB. The measurable claim is not "some requests were
	// refused" but "only this many hashes were ever computed", because that
	// count is what the peak footprint is a multiple of.
	const (
		limit    = 5
		attempts = 60
	)

	server := newLimitedServer(t, nil, 0, limit)
	client := newClient(t, server)
	client.setUpAndLogIn() // one hash for the admin, one to log them straight in

	var (
		mu       sync.Mutex
		statuses = map[int]int{}
	)
	var wait sync.WaitGroup
	for range attempts {
		wait.Add(1)
		go func() {
			defer wait.Done()
			// The right password would return 200 and open a session; the
			// wrong one still pays for the hash, which is the cost under test.
			status := postLogin(t, server.URL, "la contraseña equivocada")
			mu.Lock()
			statuses[status]++
			mu.Unlock()
		}()
	}
	wait.Wait()

	// A 401 means Verify ran, which means Argon2id ran. Setup already consumed
	// part of the window, so the count can only be lower than the ceiling.
	hashed := statuses[http.StatusUnauthorized]
	if hashed > limit {
		t.Errorf("%d of %d attempts reached Argon2id with a ceiling of %d per minute; "+
			"that is %d MiB of working set an unauthenticated stranger can ask for",
			hashed, attempts, limit, hashed*19)
	}
	if refused := statuses[http.StatusTooManyRequests]; refused == 0 {
		t.Errorf("no attempt was refused out of %d; statuses were %v", attempts, statuses)
	}
	if statuses[http.StatusUnauthorized]+statuses[http.StatusTooManyRequests] != attempts {
		t.Errorf("unexpected statuses: %v", statuses)
	}
}

func TestARefusedLoginSaysWhenToComeBack(t *testing.T) {
	server := newLimitedServer(t, nil, 0, 1)
	client := newClient(t, server)
	client.setUpAndLogIn()

	var refused *http.Response
	for attempt := 0; attempt < 5 && refused == nil; attempt++ {
		response := client.do(http.MethodPost, api("/login"),
			setupRequest{Username: testUser, Password: "la contraseña equivocada"})
		if response.StatusCode == http.StatusTooManyRequests {
			refused = response
		}
	}
	if refused == nil {
		t.Fatal("five attempts against a ceiling of one were all allowed")
	}

	retryAfter := refused.Header.Get("Retry-After")
	seconds, err := strconv.Atoi(retryAfter)
	if err != nil || seconds < 1 {
		t.Errorf("Retry-After = %q; a refusal with no usable wait invites the client straight back", retryAfter)
	}

	// The panel has to be able to tell a locked-out admin what happened, so
	// the body keeps the API's single error shape.
	var body errorBody
	client.decode(refused, &body)
	if body.Error == "" {
		t.Error("the refusal carries no message")
	}
}

func TestSetupIsThrottledToo(t *testing.T) {
	// Setup hashes exactly like login does, and on a fresh installation it is
	// reachable by anyone. Throttling only /login would leave the cheaper half
	// of the same primitive open for as long as nobody has claimed the panel.
	server := newLimitedServer(t, nil, 0, 2)

	statuses := map[int]int{}
	for attempt := 0; attempt < 8; attempt++ {
		client := newClient(t, server)
		response := client.do(http.MethodPost, api("/setup"),
			setupRequest{Username: fmt.Sprintf("admin%d", attempt), Password: testPassword})
		statuses[response.StatusCode]++
	}

	if statuses[http.StatusTooManyRequests] == 0 {
		t.Errorf("eight setup attempts against a ceiling of two were all served: %v", statuses)
	}
}

func TestIngestIsRefusedPerAddressWithoutTellingTheSDKToStopSending(t *testing.T) {
	server := newLimitedServer(t, nil, 2, 0)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	var refused *http.Response
	for attempt := 0; attempt < 6 && refused == nil; attempt++ {
		response := sendEnvelope(t, server.URL, dsn, pythonEnvelope(dsn, "invalid amount"), "")
		if response.StatusCode == http.StatusTooManyRequests {
			refused = response
		}
	}
	if refused == nil {
		t.Fatal("six envelopes against a ceiling of two were all accepted")
	}

	// The whole point of a second limiter. X-Sentry-Rate-Limits is the
	// protocol's way of saying "this category of this project is off", and the
	// official SDKs obey it by dropping that category for the window they are
	// given (ADR 005). Sending it because an address had a burst would tell a
	// healthy application to stop reporting errors — a defence turned into
	// data loss.
	if header := refused.Header.Get("X-Sentry-Rate-Limits"); header != "" {
		t.Errorf("a per-address refusal carried X-Sentry-Rate-Limits: %q. "+
			"An SDK reading that will stop sending this category entirely.", header)
	}
	if refused.Header.Get("Retry-After") == "" {
		t.Error("a per-address refusal carries no Retry-After, so an SDK has nothing to back off by")
	}
}

func TestTheAddressLimitRunsBeforeTheBodyIsRead(t *testing.T) {
	// The cost being avoided is reading and decompressing up to 20 MiB, so a
	// check that runs after the read defends the database and leaves the
	// expensive half of the request unprotected. Asserted directly rather than
	// inferred from a status code: this is the kind of ordering that survives
	// a refactor by accident and breaks by accident too.
	resolver, err := clientip.New(nil)
	if err != nil {
		t.Fatalf("building the resolver: %v", err)
	}
	server := NewServer(nil, nil, nil, nil, nil, nil, domain.Origin{}, "test").
		WithIPLimits(resolver, 1, 1)

	var reached int
	handler := server.throttleIngest(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached++
		_, _ = io.Copy(io.Discard, r.Body)
	}))

	body := &countingBody{}
	first := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/1/envelope/", body)
	first.RemoteAddr = "203.0.113.9:44321"
	handler.ServeHTTP(httptest.NewRecorder(), first)
	if reached != 1 || body.reads == 0 {
		t.Fatalf("the allowed request did not reach the handler (reached=%d, reads=%d)", reached, body.reads)
	}

	refusedBody := &countingBody{}
	second := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/1/envelope/", refusedBody)
	second.RemoteAddr = "203.0.113.9:44322"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, second)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("the second request over a ceiling of one returned %d", recorder.Code)
	}
	if reached != 1 {
		t.Error("a refused request still reached the ingest handler")
	}
	if refusedBody.reads != 0 {
		t.Errorf("a refused request's body was read %d times; the limit is meant to avoid paying for it",
			refusedBody.reads)
	}
	if refusedBody.closed {
		t.Error("a refused request's body was closed by the middleware, which is the server's job, not ours")
	}
}

func TestForwardedForIsIgnoredUnlessThePeerIsTrusted(t *testing.T) {
	// Over real HTTP, because this is where the header actually arrives and
	// where a mistake would be invisible: httptest's peer is loopback, which
	// is exactly the address a careless implementation would trust by default.
	const limit = 2

	t.Run("untrusted peer", func(t *testing.T) {
		server := newLimitedServer(t, nil, 0, limit)
		newClient(t, server).setUpAndLogIn()

		// Every request claims a different origin. If the header were
		// believed, each would get its own budget and none would be refused.
		refused := 0
		for attempt := 0; attempt < 10; attempt++ {
			if postLoginAs(t, server.URL, fmt.Sprintf("203.0.113.%d", attempt)) == http.StatusTooManyRequests {
				refused++
			}
		}
		if refused == 0 {
			t.Error("ten attempts with ten different X-Forwarded-For values against a ceiling of two " +
				"were all served: the header is being believed from an untrusted peer, so the " +
				"limiter's keys are chosen by whoever is attacking it")
		}
	})

	t.Run("trusted peer", func(t *testing.T) {
		// Loopback is the peer httptest produces, so trusting it is what a
		// real deployment behind a reverse proxy on the same host looks like.
		server := newLimitedServer(t, []string{"127.0.0.0/8", "::1"}, 0, limit)
		newClient(t, server).setUpAndLogIn()

		for attempt := 0; attempt < 6; attempt++ {
			// A different client every time, each within its own budget.
			status := postLoginAs(t, server.URL, fmt.Sprintf("198.51.100.%d", attempt))
			if status == http.StatusTooManyRequests {
				t.Fatalf("attempt %d from a distinct forwarded client was refused; "+
					"the header is not being honoured behind a trusted proxy", attempt)
			}
		}

		// And one client that does exceed its own budget still gets refused.
		refused := false
		for attempt := 0; attempt < 6; attempt++ {
			if postLoginAs(t, server.URL, "198.51.100.200") == http.StatusTooManyRequests {
				refused = true
			}
		}
		if !refused {
			t.Error("six attempts from one forwarded client against a ceiling of two were all served")
		}
	})
}

// postLogin sends one wrong-password login from the default peer.
func postLogin(t *testing.T, baseURL, password string) int {
	t.Helper()
	return postLoginWith(t, baseURL, password, "")
}

// postLoginAs sends one wrong-password login claiming to be forwarded for the
// given address.
func postLoginAs(t *testing.T, baseURL, forwardedFor string) int {
	t.Helper()
	return postLoginWith(t, baseURL, "la contraseña equivocada", forwardedFor)
}

func postLoginWith(t *testing.T, baseURL, password, forwardedFor string) int {
	t.Helper()

	body := fmt.Sprintf(`{"username":%q,"password":%q}`, testUser, password)
	request, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, baseURL+api("/login"), strings.NewReader(body))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(CSRFHeader, "1")
	if forwardedFor != "" {
		request.Header.Set("X-Forwarded-For", forwardedFor)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("posting a login: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}

// countingBody records whether anything read it.
type countingBody struct {
	reads  int
	closed bool
}

func (b *countingBody) Read(p []byte) (int, error) {
	b.reads++
	if len(p) > 0 {
		p[0] = 'x'
		return 1, io.EOF
	}
	return 0, io.EOF
}

func (b *countingBody) Close() error {
	b.closed = true
	return nil
}
