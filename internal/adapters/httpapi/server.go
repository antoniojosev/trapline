package httpapi

import (
	"net/http"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/usecase"
)

// APIVersion is the version prefix of the REST API.
//
// It was v1-beta and mutable while the product was young, because freezing an
// API before anyone has used it commits to the least grounded decisions the
// project will ever make (ADR 006). Real use has now shaped it: the whole
// surface is exercised end to end by the gates, by three clients of its own
// and by two foreign SDKs, and the shape stopped changing. So it is v1, and
// from here a route's shape is a promise rather than a suggestion.
//
// What "frozen" means, concretely, is in `docs/api/openapi.yaml`: an addition
// is fine, a removal or a rename is not, and `TestRoutesMatchOpenAPI` is what
// makes that more than a sentence in a document.
const APIVersion = "v1"

// LegacyAPIVersion is the beta prefix, still answered.
//
// Every route is served under it as well, with a `Deprecation` header, for one
// minor release (ADR 006, ADR 021). Not forever, and not silently: an alias
// that never announces its own end is a second permanent API, which is exactly
// what freezing the first one was meant to avoid.
const LegacyAPIVersion = "v1-beta"

const (
	apiPrefix       = "/api/" + APIVersion
	legacyAPIPrefix = "/api/" + LegacyAPIVersion
)

// Server is the HTTP adapter.
type Server struct {
	auth     *usecase.Auth
	projects *usecase.Projects
	tokens   *usecase.Tokens
	ingest   *usecase.Ingest
	issues   *usecase.Issues
	stats    *usecase.Stats
	releases *usecase.Releases
	origin   domain.Origin
	version  string
	panel    http.Handler
	// limits are the per-address defences. Always present: a server built
	// without configuring them still gets the shipped defaults, because a
	// rate limit that has to be switched on is one most installations run
	// without (ADR 023).
	limits *clientLimits
	// jobs is what is running in the background. Optional: a server can be
	// built without one, and then reports that nothing is running, which is
	// the truth for that process.
	jobs jobLister
	// feed is the live issue stream. Optional in the same way: without it the
	// stream route does not exist and the panel falls back to reloading, which
	// is what it did before the feed.
	feed *usecase.Broadcaster
	// alerts is where notifications are configured and read. Optional too:
	// without it there is no /alerts/ surface at all.
	alerts *usecase.Alerts
	// digest is the weekly report, and channels is the health of the places
	// it can be sent to. Optional together: a server built without them has
	// no digest routes at all, rather than routes that answer "not
	// configured" — which is the same rule the rest of this file follows.
	digest   *usecase.Digest
	channels *usecase.ChannelHealth
	// crons is cron monitoring. Optional like the rest, and its absence takes
	// the public /ping/ surface away with it: a server that watches no
	// monitors should not answer on a path that only exists to report to one.
	crons *usecase.Crons
	// uptime is the other half of the same area: the monitors this server
	// checks itself, and their history. Optional too — without it there is no
	// /monitors/uptime surface at all, which is what a build that cannot
	// check anything should look like.
	uptime *usecase.Uptime
	// status is the public status page. Optional too, and its absence takes
	// the whole /status/ path with it — an installation that publishes
	// nothing should not answer there at all.
	status *usecase.StatusPage
	// transactions is tracing: what is slow, what is failing, and the stored
	// waterfalls. Optional like the rest, and its absence takes the whole
	// /transactions and /traces surface with it rather than leaving routes
	// that answer "not configured".
	transactions *usecase.Transactions
	// artifacts is the source-map upload surface: this product's own
	// endpoints and the ones sentry-cli speaks. Optional like the rest, and
	// its absence takes both with it — a server that cannot store an artefact
	// should not advertise, in its capabilities document, that it can
	// (ADR 018).
	artifacts *usecase.Artifacts
	// health is release health: crash-free rate per release, read out of the
	// session counters. Optional like the rest, so a build assembled without
	// it simply has no /health routes rather than routes that answer 500.
	health *usecase.Health
	// suspects is "which change probably caused this issue". Optional like
	// the rest, and its absence takes the route with it rather than leaving
	// one that answers an empty guess — an empty guess and no guess at all
	// look identical to a client and mean opposite things (ADR 019).
	suspects *usecase.Suspects
	// bundle is one issue rendered as a document: the whole of what an agent
	// needs to fix a bug, in one request instead of five. Optional like the
	// rest, and its absence takes the route with it — a bundle endpoint that
	// answered with half the sections because half the use cases were missing
	// would be worse than none, since nothing in the document says which
	// parts were meant to be there.
	bundle *usecase.Bundle
	// mcp builds the MCP endpoint over this server's own API router. Optional
	// too, and it is the one surface whose absence is invisible from inside:
	// an MCP client that gets a 404 on /mcp reports "server not found", which
	// is the honest description of a build that does not have one (ADR 022).
	//
	// A factory rather than a handler, because the handler needs the router
	// and the router does not exist until Handler() builds it. That is not an
	// accident of ordering — it is the design: an MCP tool is an API call, so
	// the MCP endpoint is a client of the very table it is registered in
	// (adapters/mcp, ADR 022).
	mcp func(api http.Handler) http.Handler
}

// WithBundle mounts the issue-bundle endpoint.
//
// A setter rather than another constructor argument, like every area before
// it: adding the next one should not rewrite every call site again.
func (s *Server) WithBundle(bundle *usecase.Bundle) *Server {
	s.bundle = bundle
	return s
}

// WithMCP mounts the MCP endpoint, built over this server's own API router.
func (s *Server) WithMCP(build func(api http.Handler) http.Handler) *Server {
	s.mcp = build
	return s
}

// WithSuspects mounts the suspect-commit endpoint.
//
// A setter rather than another constructor argument, like every area before
// it: adding the next one should not rewrite every call site again.
func (s *Server) WithSuspects(suspects *usecase.Suspects) *Server {
	s.suspects = suspects
	return s
}

// WithArtifacts mounts the source-map upload endpoints, native and emulated.
//
// A setter rather than another constructor argument, like every area before
// it: adding the next one should not rewrite every call site again.
func (s *Server) WithArtifacts(artifacts *usecase.Artifacts) *Server {
	s.artifacts = artifacts
	return s
}

// WithHealth mounts the release-health endpoints.
//
// A setter rather than another constructor argument, for the reason the others
// are: adding the next area should not rewrite every call site again.
func (s *Server) WithHealth(health *usecase.Health) *Server {
	s.health = health
	return s
}

// WithTransactions mounts the tracing endpoints.
//
// A setter rather than another constructor argument, for the reason the others
// are: adding the next area should not rewrite every call site again.
func (s *Server) WithTransactions(transactions *usecase.Transactions) *Server {
	s.transactions = transactions
	return s
}

// WithFeed mounts the live event stream.
func (s *Server) WithFeed(feed *usecase.Broadcaster) *Server {
	s.feed = feed
	return s
}

// WithReleases mounts the release endpoints.
//
// Optional in the same way the panel is: a server can be built without them,
// and the routes simply do not exist. It is a setter rather than a seventh
// constructor argument so that adding the next area does not rewrite every
// call site again.
func (s *Server) WithReleases(releases *usecase.Releases) *Server {
	s.releases = releases
	return s
}

// WithPanel serves the embedded web UI on every path the API does not claim.
// It is optional so a test can exercise the API without the panel, and so a
// deployment that only ingests can run without it.
func (s *Server) WithPanel(panel http.Handler) *Server {
	s.panel = panel
	return s
}

// NewServer wires the adapter to the use cases.
func NewServer(
	auth *usecase.Auth,
	projects *usecase.Projects,
	tokens *usecase.Tokens,
	ingest *usecase.Ingest,
	issues *usecase.Issues,
	stats *usecase.Stats,
	origin domain.Origin,
	version string,
) *Server {
	return &Server{
		auth:     auth,
		projects: projects,
		tokens:   tokens,
		ingest:   ingest,
		issues:   issues,
		stats:    stats,
		origin:   origin,
		version:  version,
		limits:   defaultClientLimits(),
	}
}

// Handler builds the router.
//
// What it registers is in `routes.go`, as one table read in one pass:
// "which endpoints are public, and what does each one demand" is the question
// an auditor asks first, and it should not require tracing middleware through
// several files to answer. This function is only the wiring — the two routers,
// the three surfaces that live outside the versioned API, and the middleware
// that goes round the lot.
func (s *Server) Handler() http.Handler {
	// Two routers, split by prefix. The API gets its own so that a request
	// under /api/ can never fall through to the panel, and so a wrong method
	// still produces 405 rather than a page — mounting the panel on "/" in a
	// single router silently broke both, because a catch-all pattern matches
	// every method and wins over a method-specific one that did not.
	mux := http.NewServeMux()

	// One pass over the route table (routes.go). Every route is registered
	// twice, under the frozen prefix and under the beta alias, from the same
	// entry and with the same handler.
	//
	// From the same entry deliberately. An alias built by copying the table
	// would be an alias that drifts, and the shape of that bug is a route that
	// works on /api/v1/ and 404s on /api/v1-beta/ for the one caller that had
	// not migrated yet — which is the only caller the alias exists for.
	//
	// Saying that an answer came from the alias is not done here: it is
	// announceLegacyPrefix, below, outside the CSRF guard, and the comment
	// there explains why it cannot be done at this level.
	for _, rt := range apiRoutes() {
		if !rt.available(s) {
			continue
		}
		handler := rt.guard(s)
		mux.Handle(rt.Method+" "+apiPrefix+rt.Path, handler)
		mux.Handle(rt.Method+" "+legacyAPIPrefix+rt.Path, handler)
	}

	// The ingest endpoint's path is fixed by the protocol, not by us: SDKs
	// build it from a DSN and it cannot move (ADR 002). That puts it in the
	// same namespace as our own API, and the two patterns genuinely overlap —
	// "/api/v1-beta/" and "/api/{projectID}/envelope/" both match
	// "/api/v1-beta/envelope/" and neither is more specific, so net/http
	// refuses to guess and panics at registration. It is right to.
	//
	// Resolved by dispatching on the one thing that actually distinguishes
	// them: a project id is a number and our API version is not.
	ingestMux := http.NewServeMux()
	// Throttled by address before anything reads the body: the cost this
	// guard exists to avoid is the decompression and the parse, so a check
	// that runs after them protects nothing worth protecting (ADR 023).
	ingestMux.Handle("POST /api/{projectID}/envelope/", s.throttleIngest(http.HandlerFunc(s.handleIngest)))
	// The preflight a browser sends before a request it considers unsafe. It
	// is a route rather than a special case in the middleware so that the
	// router, not this file, decides which paths it applies to (cors.go).
	//
	// Deliberately not throttled. It reads nothing, parses nothing and
	// touches no storage — it answers from constants — so it is not the cost
	// ADR 023 exists to bound. Charging it would spend a page's budget twice
	// for one event: once on the preflight the browser sends on its own, and
	// again on the POST it authorises.
	ingestMux.HandleFunc("OPTIONS /api/{projectID}/envelope/", handleIngestPreflight)

	root := http.NewServeMux()
	// The CSRF header is required on the panel's API and NOT on ingest.
	//
	// It is a defence against a browser being tricked into using an ambient
	// credential — a session cookie. The ingest endpoint has no ambient
	// credential: it is authenticated by a DSN key that the caller must
	// present explicitly, so there is nothing to forge. Applying the check
	// there anyway would reject every official SDK, since none of them send a
	// header invented by this product. That failure would have been total and
	// silent: every API test passes, and no real SDK can deliver an event.
	//
	// Ingest also answers cross-origin, and only ingest does: it is the one
	// endpoint a browser reaches from a page this server did not serve, and
	// without a policy the SDK cannot read the rate-limit response that makes
	// a switched-off category free (cors.go, ADR 005).
	//
	// The emulated surface is the third branch, and it is deliberately not
	// behind requireCSRFHeader. That check defends an ambient credential — a
	// session cookie — and nothing on /api/0/ accepts one: it authenticates by
	// bearer token only (requireToken, sentrycompat_handler.go). Requiring a
	// header invented by this product would reject sentry-cli, which is the
	// only client the surface exists for, and the failure would be total and
	// silent — exactly the shape of the CORS bug the browser suite found.
	root.Handle("/api/", apiNamespace(
		allowCrossOriginIngest(jsonRouterErrors(ingestMux)),
		s.sentryCompatHandler(),
		announceLegacyPrefix(requireCSRFHeader(jsonRouterErrors(mux))),
	))
	// The ping surface: unauthenticated, per-address rate limited, and
	// answering one line of text (ping_handler.go, ADR 016). It is mounted on
	// the root router rather than under /api/ because the caller is a crontab
	// line and not a client of this API, and it is registered before the
	// panel's catch-all so a path under it can never fall through to HTML.
	//
	// Deliberately not behind requireCSRFHeader. That check defends an
	// ambient credential — a session cookie — and nothing here accepts one:
	// the key in the path is the whole authentication. Requiring a header
	// invented by this product would reject curl, which is the only client
	// this surface exists for, and the failure would be total and silent.
	if s.crons != nil {
		pingMux := http.NewServeMux()
		// GET as well as POST. Half the schedulers and uptime tools that can
		// call a URL only send GET, and a surface that refused it would work
		// for curl and quietly not for them.
		pingMux.HandleFunc("GET "+pingPrefix+"{pingKey...}", s.handlePing)
		pingMux.HandleFunc("POST "+pingPrefix+"{pingKey...}", s.handlePing)
		root.Handle(pingPrefix, s.throttlePing(pingMux))
	}

	// The fourth client (ADR 006, ADR 022), over HTTP. Three things about
	// where it is mounted, and all three are the same three the ping surface
	// and the emulated /api/0/ surface have:
	//
	// Outside the versioned prefix, because an MCP client's URL lives in a
	// configuration file that a version bump could not rewrite, and because
	// the path an MCP client expects is `/mcp` — this is somebody else's
	// convention, like the ingest path, not ours to decorate.
	//
	// Outside requireCSRFHeader, because that check defends an ambient
	// credential and nothing here accepts one: requireToken below takes a
	// bearer token and no cookie, so there is nothing to forge. Requiring a
	// header invented by this product would reject every MCP client there is,
	// and the failure would be total and silent.
	//
	// Before the panel's catch-all, so a POST to /mcp can never fall through
	// to HTML.
	//
	// projects:read to open the connection, and no more. Each tool's own call
	// goes back through the table above with the same token, so `resolve_issue`
	// is refused for a read-only token by the very line that refuses it over
	// REST — which is the property that makes this an adapter and not a second
	// API (adapters/mcp/caller.go).
	if s.mcp != nil {
		root.Handle("POST "+mcpPath, s.requireToken(domain.ScopeProjectsRead,
			s.mcp(jsonRouterErrors(mux))))
	}

	// The public status page (status_handler.go, ADR 017). Registered here
	// for the same three reasons as the ping surface: the reader is not a
	// client of this API, there is no ambient credential for requireCSRFHeader
	// to defend, and it must be claimed before the panel's catch-all so a
	// project slug can never fall through to the single-page app.
	if s.status != nil {
		statusMux := http.NewServeMux()
		statusMux.HandleFunc("GET "+statusPrefix+"{slug...}", s.handleStatusPage)
		root.Handle(statusPrefix, s.throttleStatus(statusMux))
	}

	// Everything under the API prefix belongs to the API router, including the
	// paths it does not recognise: a JSON client must never receive HTML.
	if s.panel != nil {
		root.Handle("/", s.panel)
	}

	return chain(root,
		recoverPanics,
		logRequests,
		securityHeaders,
	)
}

// handleHealth is the endpoint a supervisor or load balancer polls. It is
// unauthenticated by necessity and returns nothing an unauthenticated caller
// should not see: no version, no counts, no configuration.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// HTTPServer builds a net/http server with timeouts set.
//
// Every timeout here is deliberate. Go's zero value for all of them is "no
// timeout", which means one stalled client holds a connection forever; on a
// service whose ingest endpoint faces the open internet that is a
// denial-of-service waiting to be discovered.
func (s *Server) HTTPServer(addr string) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}
}
