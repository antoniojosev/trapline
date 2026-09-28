package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

// pingPrefix is the path the ping surface lives under.
//
// Outside /api/, deliberately. Everything under /api/ is either the protocol's
// (an SDK builds it from a DSN) or this product's REST API (JSON, a bearer
// token, a CSRF header). This is neither: it is a URL a person pastes at the
// end of a line in a crontab, and it has to be short enough that they will.
const pingPrefix = "/ping/"

// The three things a ping can say.
//
// One path each rather than a query parameter, because the caller is a shell
// and `?status=error` inside a crontab line needs quoting that people get
// wrong — a bare `&` or `?` in a crontab is a footgun with a long fuse.
const (
	pingStartSuffix = "/start"
	pingFailSuffix  = "/fail"
)

// handlePing is the whole of the curl-able surface.
//
// No authentication, and that is the design rather than an omission: the key
// in the path *is* the credential (ADR 016). The case it exists for is a
// backup script — a cron entry that ends `&& curl -fsS https://…/ping/abc` —
// and a cron entry cannot hold a bearer token, cannot read a config file it
// was not given, and will not be rewritten to import an SDK. Everything else
// here follows from that: one line of text/plain so `curl -f` behaves,
// GET as well as POST because half the tools that do this only send GET, and
// a per-address rate limit because the endpoint is public and unauthenticated
// (ADR 023).
func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, pingPrefix)
	status := domain.CheckInOK
	switch {
	case strings.HasSuffix(path, pingStartSuffix):
		path, status = strings.TrimSuffix(path, pingStartSuffix), domain.CheckInProgress
	case strings.HasSuffix(path, pingFailSuffix):
		path, status = strings.TrimSuffix(path, pingFailSuffix), domain.CheckInError
	}

	key := strings.Trim(path, "/")
	if key == "" || strings.Contains(key, "/") {
		// Not "bad request": an unrecognised path under this prefix is
		// indistinguishable from a key that does not exist, and answering
		// differently would tell an anonymous caller which shapes are real.
		writePing(w, http.StatusNotFound, "unknown ping key")
		return
	}

	result, err := s.crons.Ping(r.Context(), key, status)
	if err != nil {
		writePingError(w, err)
		return
	}

	switch status {
	case domain.CheckInProgress:
		writePing(w, http.StatusOK, "started "+result.Monitor.Slug)
	case domain.CheckInError:
		writePing(w, http.StatusOK, "failed "+result.Monitor.Slug)
	default:
		writePing(w, http.StatusOK, "ok "+result.Monitor.Slug)
	}
}

// writePingError maps a failure onto a status a shell script can act on.
func writePingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrMonitorDisabled):
		// 410 and not 404, and the distinction earns its place: a script that
		// has been pinging the same key for a year has no other way to learn
		// that somebody switched its monitor off, and a 404 would read as a
		// typo in a key that has not changed.
		writePing(w, http.StatusGone, "this monitor is disabled")
	case errors.Is(err, domain.ErrMonitorNotFound):
		writePing(w, http.StatusNotFound, "unknown ping key")
	default:
		writePing(w, http.StatusInternalServerError, "internal error")
	}
}

// writePing answers in one line of text.
//
// text/plain because the reader is a person looking at terminal output or a
// script piping it into a log, and a JSON object would be noise in both. The
// newline is there so the line does not run into a shell prompt.
func writePing(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// A ping is an action with an effect, and a proxy or a browser prefetch
	// that cached a GET to it would report a job as having run when nothing
	// did.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(message + "\n"))
}

// denyPing is the per-address refusal, in the same one-line shape.
//
// Not denyAPI: a JSON body here would be the one response from this surface
// that is not a line of text, and the caller parsing it is `curl -f` and a
// shell.
func denyPing(w http.ResponseWriter, retryAfter time.Duration) {
	setRetryAfter(w, retryAfter)
	writePing(w, http.StatusTooManyRequests, "too many requests from this address")
}

// throttlePing is the per-address ceiling on the unauthenticated ping surface.
//
// It reuses the ingest limiter's ceiling rather than minting a third number.
// The two are the same kind of traffic — public, unauthenticated, one cheap
// write per request — and an operator who has already tuned one ceiling for
// their deployment should not discover a second one with a different default
// the day a monitor starts answering 429 (ADR 023).
func (s *Server) throttlePing(next http.Handler) http.Handler {
	return limitByIP(s.limits.ingest, s.clientAddr, denyPing, next)
}
