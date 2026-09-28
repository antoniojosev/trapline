package httpapi

import (
	"log/slog"
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/antoniojosev/trapline/internal/adapters/ratelimit"
	"github.com/antoniojosev/trapline/internal/clientip"
	"github.com/antoniojosev/trapline/internal/domain"
)

// maxConcurrentAuth is how many password hashes may be in flight at once,
// across every address.
//
// The per-address ceiling rations one attacker; this rations all of them at
// once, and it is the number that actually bounds memory. Argon2id holds
// 19 MiB for the duration of a hash (ADR 005), so peak resident memory is
// roughly the idle footprint plus this many working sets — which is how the
// published 80 MB peak budget stays true no matter how many addresses a
// distributed flood arrives from. Per-address limiting alone cannot promise
// that: a thousand addresses each politely under their own ceiling still
// arrive together.
//
// Two, because this product has exactly one admin account. There is no fleet
// of users to serve, so the queue this creates for a legitimate login is
// empty, and refusing rather than queueing is deliberate: a queue in front of
// a memory-hard function is a way to hold the memory anyway, just later.
const maxConcurrentAuth = 2

// clientLimits is the per-address defence, assembled once per server.
//
// Two limiters, not one shared instance, because they exist for different
// reasons and are refused differently — see ratelimit.IPLimiter and ADR 023.
type clientLimits struct {
	resolver *clientip.Resolver
	ingest   *ratelimit.IPLimiter
	auth     *ratelimit.IPLimiter
	// authSlots is a semaphore, held for the duration of a request that will
	// hash a password.
	authSlots chan struct{}
	// ingestArena is the same idea one endpoint over and denominated in bytes:
	// the ceiling on what every in-flight ingest request holds between them
	// (ingest_budget.go, ADR 039). The per-address limiter above rations one
	// attacker's *rate*; this rations everybody's *memory*, which a thousand
	// addresses each politely under their own ceiling would otherwise exhaust
	// together.
	ingestArena *ingestArena
}

// defaultClientLimits is what a server gets when nothing configures it.
//
// On by default, and it has to be: a defence that must be switched on is one
// every installation that never read the docs is running without. Nothing is
// trusted to forward for anyone, which is the safe end of the only setting
// here that can be wrong in a dangerous direction.
func defaultClientLimits() *clientLimits {
	resolver, err := clientip.New(nil)
	if err != nil {
		// clientip.New only fails on an unparseable entry and there are none.
		panic("clientip.New(nil) failed, which is impossible: " + err.Error())
	}
	return newClientLimits(resolver, domain.DefaultIngestIPRateLimitPerMinute, domain.DefaultAuthRateLimitPerMinute)
}

func newClientLimits(resolver *clientip.Resolver, ingestPerMinute, authPerMinute int) *clientLimits {
	return &clientLimits{
		resolver:    resolver,
		ingest:      ratelimit.NewIP(ingestPerMinute, ratelimit.IPWindow, domain.MaxIngestIPEntries),
		auth:        ratelimit.NewIP(authPerMinute, ratelimit.IPWindow, domain.MaxAuthIPEntries),
		authSlots:   make(chan struct{}, maxConcurrentAuth),
		ingestArena: newIngestArena(ingestArenaBytes),
	}
}

// WithIPLimits replaces the per-address defences with configured ones.
//
// A builder rather than a constructor argument so the existing call sites, and
// the tests that build a server without caring about any of this, keep the
// shipped defaults instead of being handed the chance to pass zero.
func (s *Server) WithIPLimits(resolver *clientip.Resolver, ingestPerMinute, authPerMinute int) *Server {
	s.limits = newClientLimits(resolver, ingestPerMinute, authPerMinute)
	return s
}

// clientAddr is the address this request is attributed to.
func (s *Server) clientAddr(r *http.Request) netip.Addr {
	addr, ok := s.limits.resolver.Resolve(r.RemoteAddr, r.Header.Values("X-Forwarded-For"))
	if !ok {
		return netip.Addr{}
	}
	return addr
}

// limitByIP refuses a request whose address has asked for too much.
//
// It is a middleware, and being one is the point rather than an implementation
// detail: it runs before the handler touches r.Body, so a refused request
// never costs a read, a decompression or a parse. A check inside the handler,
// after the body has been read, would defend the database and leave the
// expensive half of the request unprotected — which for an endpoint that
// accepts 20 MiB of compressed input is most of what there is to protect.
//
// deny is passed in because the two endpoints that use this must answer
// differently: see denyIngest.
func limitByIP(
	limiter *ratelimit.IPLimiter,
	addrOf func(*http.Request) netip.Addr,
	deny func(w http.ResponseWriter, retryAfter time.Duration),
	next http.Handler,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if allowed, retryAfter := limiter.Allow(addrOf(r)); !allowed {
			deny(w, retryAfter)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// throttleIngest is the per-address ceiling on the public ingest endpoint.
func (s *Server) throttleIngest(next http.Handler) http.Handler {
	return limitByIP(s.limits.ingest, s.clientAddr, denyIngest, next)
}

// throttleAuth is the per-address ceiling on the endpoints that hash a
// password, plus the global cap on how many may do so at once.
func (s *Server) throttleAuth(next http.Handler) http.Handler {
	limited := limitByIP(s.limits.auth, s.clientAddr, denyAPI, http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			select {
			case s.limits.authSlots <- struct{}{}:
				defer func() { <-s.limits.authSlots }()
			default:
				// Every slot is busy hashing. Refusing immediately is the
				// whole defence: waiting for one would mean this request is
				// holding a connection open precisely so it can allocate
				// 19 MiB a moment later.
				slog.Warn("refusing an authentication attempt: every hashing slot is busy",
					"slots", maxConcurrentAuth)
				denyAPI(w, time.Second)
				return
			}
			next.ServeHTTP(w, r)
		}))
	return limited
}

// denyIngest answers a per-address refusal in the shape SDKs parse.
//
// Deliberately without X-Sentry-Rate-Limits. That header is the protocol's way
// of saying "this category of this project is switched off or over its
// threshold", and the official SDKs obey it by dropping that category for the
// whole window they are given (ADR 005). Sending it because an address had a
// burst would tell a healthy application to stop reporting errors for minutes
// — turning a defence into data loss, and the loss would be of exactly the
// events someone is waiting for. A bare 429 with Retry-After is what an SDK
// treats as a transient failure and retries. The distinction is the reason
// this is a separate limiter (ADR 023).
func denyIngest(w http.ResponseWriter, retryAfter time.Duration) {
	setRetryAfter(w, retryAfter)
	writeIngestError(w, http.StatusTooManyRequests, "too many requests from this address")
}

// denyIngestBusy answers a refusal that is about this server's memory rather
// than about this client.
//
// A bare 429 with a short Retry-After, and deliberately without
// X-Sentry-Rate-Limits, for the reason denyIngest gives: that header tells an
// SDK to stop sending a whole category for minutes, and the condition here
// lasts as long as the flood causing it. An SDK that reads this drops nothing
// and comes back a second later, which is exactly right — it did nothing
// wrong.
func denyIngestBusy(w http.ResponseWriter) {
	setRetryAfter(w, time.Second)
	writeIngestError(w, http.StatusTooManyRequests, "server is at its ingest memory budget; retry shortly")
}

// denyAPI answers a per-address refusal in the API's own error shape.
func denyAPI(w http.ResponseWriter, retryAfter time.Duration) {
	setRetryAfter(w, retryAfter)
	writeJSON(w, http.StatusTooManyRequests, errorBody{Error: "too many attempts from this address; try again later"})
}

// setRetryAfter writes the header in whole seconds, never below one: a
// Retry-After of zero invites the client straight back.
func setRetryAfter(w http.ResponseWriter, retryAfter time.Duration) {
	seconds := math.Ceil(retryAfter.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(seconds)))
}
