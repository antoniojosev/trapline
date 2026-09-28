// Package uptime is the outbound half of monitoring: the one place in this
// product that makes an HTTP request to an address a user chose.
//
// Everything here is arranged around that sentence. The client is built per
// check rather than shared, its dialer is the SSRF guard's rather than Go's,
// and every redirect is validated again before it is followed — because a
// redirect is a second URL, chosen by whoever controls the first one, and
// following it with the check already behind us would hand back exactly the
// capability the guard was there to remove (ADR 016).
package uptime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ssrfguard"
)

// Checker performs one HTTP check.
type Checker struct {
	guard *ssrfguard.Guard
	// userAgent identifies this server to whatever it is checking. A monitor
	// that arrives as an anonymous request is a monitor whose traffic somebody
	// will eventually block without knowing what it was.
	userAgent string
}

// New builds a checker over a guard.
func New(guard *ssrfguard.Guard, version string) *Checker {
	agent := "trapline-uptime/" + version
	return &Checker{guard: guard, userAgent: agent}
}

// Check fetches the monitor's URL once and reports what happened.
//
// It never returns an error. A target that is down, unreachable, slow or
// answering nonsense is the normal case for this function, not a failure of
// it, and expressing that as an error would make every caller unwrap one to
// find out whether anything was actually wrong on this side.
func (c *Checker) Check(
	ctx context.Context, monitor *domain.UptimeMonitor, now time.Time,
) domain.CheckResult {
	result := domain.CheckResult{At: now.UTC()}

	target, err := ssrfguard.ParseTarget(monitor.URL)
	if err != nil {
		result.Error = err.Error()
		return result
	}

	// The whole check gets the monitor's timeout, connection and body
	// included. A timeout on the client alone would let a target trickle a
	// response back for as long as it liked.
	ctx, cancel := context.WithTimeout(ctx, monitor.Timeout())
	defer cancel()

	transport := &http.Transport{
		// The guard's dialer, and this line is the security control. Go's own
		// would resolve the host a second time, and the second answer is the
		// one the connection uses (ssrfguard).
		DialContext: c.guard.DialContext(monitor.AllowPrivate),
		// No connection reuse between checks. A monitor is a question about
		// the target's current state — including whether it still resolves and
		// still accepts a connection — and a pooled connection answers a
		// question about the past.
		DisableKeepAlives:     true,
		TLSHandshakeTimeout:   monitor.Timeout(),
		ResponseHeaderTimeout: monitor.Timeout(),
		// Compression is left on: a health endpoint that gzips is ordinary,
		// and Go transparently decodes it before the body is read.
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{
		Transport:     transport,
		CheckRedirect: c.redirectPolicy(monitor),
	}

	request, err := http.NewRequestWithContext(ctx, monitor.Method, target.String(), http.NoBody)
	if err != nil {
		result.Error = fmt.Sprintf("building the request: %s", err)
		return result
	}
	request.Header.Set("User-Agent", c.userAgent)
	// Asking for anything means a server that content-negotiates does not
	// answer 406 to a monitor that never said what it wanted.
	request.Header.Set("Accept", "*/*")

	started := time.Now()
	response, err := client.Do(request)
	if err != nil {
		result.LatencyMS = int(time.Since(started).Milliseconds())
		result.Error = describe(err)
		return result
	}
	defer func() { _ = response.Body.Close() }()

	body := ""
	if monitor.ExpectedBodySubstring != "" {
		// Bounded, and read before the latency is taken so the number covers
		// the whole exchange. A health endpoint answers in bytes; anything
		// that answers in megabytes is not being read, it is being
		// downloaded, and this server must not be made to download it once a
		// minute.
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, domain.MaxCheckBody))
		if readErr != nil {
			result.LatencyMS = int(time.Since(started).Milliseconds())
			result.StatusCode = response.StatusCode
			result.Error = fmt.Sprintf("reading the body: %s", describe(readErr))
			return result
		}
		body = string(raw)
	} else {
		// Drained and discarded, so the connection is not abandoned
		// mid-response — which on a keep-alive-less transport is only tidy,
		// and on any other would leak a socket per check.
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, domain.MaxCheckBody))
	}

	result.LatencyMS = int(time.Since(started).Milliseconds())
	result.StatusCode = response.StatusCode
	if reason := monitor.Evaluate(response.StatusCode, body); reason != "" {
		result.Error = reason
		return result
	}
	result.OK = true
	return result
}

// redirectPolicy decides what happens at a 3xx.
//
// Two cases and both are deliberate. A monitor that does not follow redirects
// gets the 3xx handed back as the answer, so `Evaluate` judges it against the
// expected range — which is what somebody who set `follow_redirects: false`
// meant: "this URL should answer directly". A monitor that does follow them
// re-runs the guard on every hop, because the location header is a URL chosen
// by the target, and a target that can send this server anywhere is the same
// SSRF the guard exists to close, arrived at one hop later.
func (c *Checker) redirectPolicy(monitor *domain.UptimeMonitor) func(*http.Request, []*http.Request) error {
	return func(request *http.Request, via []*http.Request) error {
		if !monitor.FollowRedirects {
			// Not an error: the response is returned to the caller as it
			// stands, status code included.
			return http.ErrUseLastResponse
		}
		if len(via) >= domain.MaxCheckRedirects {
			return fmt.Errorf("stopped after %d redirects", domain.MaxCheckRedirects)
		}
		target, err := ssrfguard.ParseTarget(request.URL.String())
		if err != nil {
			return err
		}
		// Belt and braces: the dialer would refuse this connection anyway,
		// and refusing it here produces a message that says "a redirect went
		// somewhere it may not" rather than a dial error three layers down.
		if err := c.guard.Check(request.Context(), target, monitor.AllowPrivate); err != nil {
			return fmt.Errorf("a redirect pointed at somewhere this server does not visit: %w", err)
		}
		return nil
	}
}

// describe turns a transport failure into something an operator can act on.
//
// net/http wraps everything in *url.Error, whose message repeats the method
// and the whole URL — both of which the reader already has, since they are
// sitting in the monitor next to the error. What is left after unwrapping is
// the part that says what actually happened, and for a refusal by the guard
// that part is a sentence naming the address and which opt-in is missing.
func describe(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	var wrapped *url.Error
	if errors.As(err, &wrapped) && wrapped.Err != nil {
		return wrapped.Err.Error()
	}
	return err.Error()
}
