package httpapi

import (
	"net/http"
	"strings"

	"github.com/antoniojosev/trapline/internal/domain"
)

// The route table, as data.
//
// It used to be four `map[string]struct{…}` literals inside Handler, built and
// registered in one pass. That was fine while the only consumer was net/http,
// and it stopped being fine the moment the API froze (ADR 006, ADR 021): a
// frozen contract needs something that can *enumerate* it, because the gate
// that keeps `docs/api/openapi.yaml` honest has to compare the document
// against the routes this binary actually serves, and a route registered
// inside a closure is invisible to everything except the router.
//
// So the table is a slice of values with no server attached, `Handler` walks
// it to register, and `TestRoutesMatchOpenAPI` walks the same slice to check
// the document. There is exactly one list, and the only way to add a route
// without documenting it is to fail the build.
//
// A slice rather than a map, and in reading order: the order is now part of
// what a person reads to answer "what does this API expose", and a map would
// shuffle it on every run.

// route is one entry of the versioned API.
type route struct {
	// Method and Path are the pattern, with Path relative to the version
	// prefix — `/projects/{id}/issues`, never `/api/v1/projects/{id}/issues`.
	// Relative because the same route is served under two prefixes while the
	// beta alias lives, and a table that spelled the prefix out would have to
	// be read twice to be trusted once.
	Method string
	Path   string

	// Scope is the permission the route demands. The zero value means the
	// route is public, and `Public` says so out loud rather than leaving it
	// to be inferred from an empty string — "which endpoints are public" is
	// the first question an auditor asks and it should be answerable by
	// grepping one word.
	Scope  domain.Scope
	Public bool

	// Throttled marks a public route that spends an Argon2id hash, and is
	// therefore rate-limited per address before it can allocate 19 MiB on
	// somebody's behalf (ADR 023).
	Throttled bool

	// Subsystem names the optional use case this route needs, or "" when the
	// route always exists. A server built without that subsystem does not
	// serve the route at all, rather than serving one that answers "not
	// configured": a 404 is the honest description of a surface this build
	// does not have.
	Subsystem string

	// Handler resolves the method off a server. A function rather than a
	// bound method value so the table can be built without a server, which
	// is the whole point of it being data.
	Handler func(*Server) http.HandlerFunc
}

// The optional subsystems a route can depend on. Named constants rather than
// bare strings so a typo is a compile error instead of a route that silently
// never registers.
const (
	subsystemReleases     = "releases"
	subsystemAlerts       = "alerts"
	subsystemDigest       = "digest"
	subsystemChannels     = "channels"
	subsystemStatus       = "status"
	subsystemCrons        = "crons"
	subsystemUptime       = "uptime"
	subsystemTransactions = "transactions"
	subsystemArtifacts    = "artifacts"
	subsystemHealth       = "health"
	subsystemSuspects     = "suspects"
	subsystemBundle       = "bundle"
)

// available reports whether this build serves the route.
func (rt route) available(s *Server) bool {
	switch rt.Subsystem {
	case "":
		return true
	case subsystemReleases:
		return s.releases != nil
	case subsystemAlerts:
		return s.alerts != nil
	case subsystemDigest:
		return s.digest != nil
	case subsystemChannels:
		return s.channels != nil
	case subsystemStatus:
		return s.status != nil
	case subsystemCrons:
		return s.crons != nil
	case subsystemUptime:
		return s.uptime != nil
	case subsystemTransactions:
		return s.transactions != nil
	case subsystemArtifacts:
		return s.artifacts != nil
	case subsystemHealth:
		return s.health != nil
	case subsystemSuspects:
		return s.suspects != nil
	case subsystemBundle:
		return s.bundle != nil
	default:
		// An unknown subsystem name is a typo in this file, and the safe
		// reading of a typo is "this route does not exist" — a route that
		// mounted anyway would be a permission surface nobody declared.
		return false
	}
}

// apiRoutes is the whole versioned API, in reading order.
//
// that matters: that the entire public surface of this product is readable in
// one place, in one pass, by somebody who has never seen the code.
//
//nolint:funlen // It is a table. Splitting it would hide the one property
func apiRoutes() []route {
	var routes []route
	routes = append(routes, publicRoutes()...)
	routes = append(routes, coreRoutes()...)
	routes = append(routes, releaseRoutes()...)
	routes = append(routes, alertRoutes()...)
	routes = append(routes, monitorRoutes()...)
	routes = append(routes, observabilityRoutes()...)
	return routes
}

// publicRoutes need no session: liveness, the setup flow that closes itself
// after one use, and the two ends of a session.
func publicRoutes() []route {
	return []route{
		{Method: "GET", Path: "/health", Public: true,
			Handler: func(s *Server) http.HandlerFunc { return s.handleHealth }},
		{Method: "GET", Path: "/setup", Public: true,
			Handler: func(s *Server) http.HandlerFunc { return s.handleSetupStatus }},
		// The two endpoints that spend an Argon2id hash. They are marked here
		// rather than inside the handlers so that "which unauthenticated
		// endpoint can make this server allocate 19 MiB" is answerable from
		// the same screen as "which endpoints are public" (ADR 023).
		{Method: "POST", Path: "/setup", Public: true, Throttled: true,
			Handler: func(s *Server) http.HandlerFunc { return s.handleSetup }},
		{Method: "POST", Path: "/login", Public: true, Throttled: true,
			Handler: func(s *Server) http.HandlerFunc { return s.handleLogin }},
		// Logout is public because a request with a dead cookie must still be
		// able to clear it; requiring a live session to log out would strand
		// a user whose session already expired.
		{Method: "POST", Path: "/logout", Public: true,
			Handler: func(s *Server) http.HandlerFunc { return s.handleLogout }},
	}
}

// coreRoutes are projects, issues, the dashboard and the process itself:
// everything a build of this product always has.
func coreRoutes() []route {
	return []route{
		{Method: "GET", Path: "/me", Scope: domain.ScopeProjectsRead,
			Handler: func(s *Server) http.HandlerFunc { return s.handleMe }},

		{Method: "GET", Path: "/projects", Scope: domain.ScopeProjectsRead,
			Handler: func(s *Server) http.HandlerFunc { return s.handleListProjects }},
		{Method: "POST", Path: "/projects", Scope: domain.ScopeProjectsWrite,
			Handler: func(s *Server) http.HandlerFunc { return s.handleCreateProject }},
		{Method: "GET", Path: "/projects/{id}", Scope: domain.ScopeProjectsRead,
			Handler: func(s *Server) http.HandlerFunc { return s.handleGetProject }},
		{Method: "DELETE", Path: "/projects/{id}", Scope: domain.ScopeProjectsWrite,
			Handler: func(s *Server) http.HandlerFunc { return s.handleDeleteProject }},
		{Method: "POST", Path: "/projects/{id}/keys", Scope: domain.ScopeProjectsWrite,
			Handler: func(s *Server) http.HandlerFunc { return s.handleRotateKey }},
		{Method: "DELETE", Path: "/keys/{publicKey}", Scope: domain.ScopeProjectsWrite,
			Handler: func(s *Server) http.HandlerFunc { return s.handleRevokeKey }},

		// Issues are read with projects:read and triaged with projects:write.
		// Resolving something writes no project, but it is a change to shared
		// state that another operator will see, so it belongs on the write
		// side.
		{Method: "GET", Path: "/projects/{id}/issues", Scope: domain.ScopeProjectsRead,
			Handler: func(s *Server) http.HandlerFunc { return s.handleListIssues }},
		{Method: "GET", Path: "/projects/{id}/issues/{issueID}", Scope: domain.ScopeProjectsRead,
			Handler: func(s *Server) http.HandlerFunc { return s.handleGetIssue }},
		{Method: "POST", Path: "/projects/{id}/issues/{issueID}/status", Scope: domain.ScopeProjectsWrite,
			Handler: func(s *Server) http.HandlerFunc { return s.handleSetIssueStatus }},

		// The dashboard, read from the hourly aggregates and never from the
		// events themselves (ADR 010). Read-only, so projects:read: none of
		// them changes anything an operator would have to be told about.
		{Method: "GET", Path: "/projects/{id}/stats", Scope: domain.ScopeProjectsRead,
			Handler: func(s *Server) http.HandlerFunc { return s.handleProjectStats }},
		{Method: "GET", Path: "/projects/{id}/stats/top", Scope: domain.ScopeProjectsRead,
			Handler: func(s *Server) http.HandlerFunc { return s.handleTopIssues }},
		{Method: "GET", Path: "/projects/{id}/stats/breakdown", Scope: domain.ScopeProjectsRead,
			Handler: func(s *Server) http.HandlerFunc { return s.handleBreakdown }},
		{Method: "GET", Path: "/projects/{id}/stats/series", Scope: domain.ScopeProjectsRead,
			Handler: func(s *Server) http.HandlerFunc { return s.handleIssueSparklines }},
		{Method: "GET", Path: "/projects/{id}/issues/{issueID}/stats", Scope: domain.ScopeProjectsRead,
			Handler: func(s *Server) http.HandlerFunc { return s.handleIssueStats }},

		// The ingest profile: which categories a project accepts, its spike
		// ceiling and its retention. Without these the opt-in subsystems of
		// ADR 005 could only be changed by editing the database by hand,
		// which is not a feature anyone has.
		{Method: "GET", Path: "/projects/{id}/config", Scope: domain.ScopeProjectsRead,
			Handler: func(s *Server) http.HandlerFunc { return s.handleGetProjectConfig }},
		{Method: "PUT", Path: "/projects/{id}/config", Scope: domain.ScopeProjectsWrite,
			Handler: func(s *Server) http.HandlerFunc { return s.handleSetProjectConfig }},

		// What the server is doing when nobody is asking it for anything.
		// Read with projects:read rather than a scope of its own: the scopes
		// this product defines are coarse on purpose (see domain/token.go),
		// the split that matters is read from write, and minting a new scope
		// would strand every token created before this build on an endpoint
		// that only tells you what is already in the log.
		{Method: "GET", Path: "/system/jobs", Scope: domain.ScopeProjectsRead,
			Handler: func(s *Server) http.HandlerFunc { return s.handleListJobs }},

		// The live feed. Read-only and therefore projects:read, and behind
		// the same guard as everything else — a stream that skipped
		// authentication because it is "only notifications" would leak every
		// issue title in the installation to anyone who could open a socket.
		{Method: "GET", Path: "/projects/{id}/events/stream", Scope: domain.ScopeProjectsRead,
			Handler: func(s *Server) http.HandlerFunc { return s.handleEventStream }},
	}
}

// releaseRoutes are the deploy pipeline's half of the API.
//
// Read with projects:read and written with projects:write, rather than getting
// a scope pair of their own. The scopes this product defines are coarse on
// purpose (domain/token.go): the split that matters is read from write, and a
// new scope would strand every token minted before this build — including the
// one in a deploy pipeline, which is precisely the caller these routes exist
// for.
func releaseRoutes() []route {
	return []route{
		{Method: "GET", Path: "/projects/{id}/releases", Scope: domain.ScopeProjectsRead, Subsystem: subsystemReleases,
			Handler: func(s *Server) http.HandlerFunc { return s.handleListReleases }},
		{Method: "POST", Path: "/projects/{id}/releases", Scope: domain.ScopeProjectsWrite, Subsystem: subsystemReleases,
			Handler: func(s *Server) http.HandlerFunc { return s.handleCreateRelease }},
		{Method: "GET", Path: "/projects/{id}/releases/{version}", Scope: domain.ScopeProjectsRead, Subsystem: subsystemReleases,
			Handler: func(s *Server) http.HandlerFunc { return s.handleGetRelease }},
		// PUT because finalising twice is the same as finalising once: the
		// second call keeps the first date.
		{Method: "PUT", Path: "/projects/{id}/releases/{version}", Scope: domain.ScopeProjectsWrite, Subsystem: subsystemReleases,
			Handler: func(s *Server) http.HandlerFunc { return s.handleFinalizeRelease }},
		{Method: "POST", Path: "/projects/{id}/releases/{version}/commits", Scope: domain.ScopeProjectsWrite, Subsystem: subsystemReleases,
			Handler: func(s *Server) http.HandlerFunc { return s.handleSetReleaseCommits }},
		{Method: "POST", Path: "/projects/{id}/releases/{version}/deploys", Scope: domain.ScopeProjectsWrite, Subsystem: subsystemReleases,
			Handler: func(s *Server) http.HandlerFunc { return s.handleCreateDeploy }},
	}
}

// alertRoutes are the first area in this product with a scope pair of its own.
//
// The reason is in domain/token.go: a channel holds a credential for somebody
// else's system — a bot token, an SMTP password — so "can read the issues" and
// "can read where this installation sends its notifications" are genuinely
// different permissions, and a token minted for a dashboard should not carry
// the second.
func alertRoutes() []route {
	return []route{
		{Method: "GET", Path: "/alerts/channels", Scope: domain.ScopeAlertsRead, Subsystem: subsystemAlerts,
			Handler: func(s *Server) http.HandlerFunc { return s.handleListChannels }},
		{Method: "POST", Path: "/alerts/channels", Scope: domain.ScopeAlertsWrite, Subsystem: subsystemAlerts,
			Handler: func(s *Server) http.HandlerFunc { return s.handleCreateChannel }},
		{Method: "DELETE", Path: "/alerts/channels/{id}", Scope: domain.ScopeAlertsWrite, Subsystem: subsystemAlerts,
			Handler: func(s *Server) http.HandlerFunc { return s.handleDeleteChannel }},
		// Testing a channel spends a real delivery through somebody else's
		// rate-limited API, so it is a write even though it changes nothing
		// here.
		{Method: "POST", Path: "/alerts/channels/{id}/test", Scope: domain.ScopeAlertsWrite, Subsystem: subsystemAlerts,
			Handler: func(s *Server) http.HandlerFunc { return s.handleTestChannel }},

		{Method: "GET", Path: "/alerts/rules", Scope: domain.ScopeAlertsRead, Subsystem: subsystemAlerts,
			Handler: func(s *Server) http.HandlerFunc { return s.handleListRules }},
		{Method: "POST", Path: "/alerts/rules", Scope: domain.ScopeAlertsWrite, Subsystem: subsystemAlerts,
			Handler: func(s *Server) http.HandlerFunc { return s.handleCreateRule }},
		{Method: "DELETE", Path: "/alerts/rules/{id}", Scope: domain.ScopeAlertsWrite, Subsystem: subsystemAlerts,
			Handler: func(s *Server) http.HandlerFunc { return s.handleDeleteRule }},
		{Method: "POST", Path: "/alerts/rules/{id}/test", Scope: domain.ScopeAlertsWrite, Subsystem: subsystemAlerts,
			Handler: func(s *Server) http.HandlerFunc { return s.handleTestRule }},

		{Method: "GET", Path: "/alerts/notifications", Scope: domain.ScopeAlertsRead, Subsystem: subsystemAlerts,
			Handler: func(s *Server) http.HandlerFunc { return s.handleListNotifications }},
		{Method: "POST", Path: "/alerts/notifications/{id}/retry", Scope: domain.ScopeAlertsWrite, Subsystem: subsystemAlerts,
			Handler: func(s *Server) http.HandlerFunc { return s.handleRetryNotification }},

		// The weekly digest: what it would say, and when it goes out. Read
		// with projects:read and written with projects:write, like everything
		// else here. The scopes this product defines are coarse on purpose
		// (domain/token.go) and the split that matters is read from write;
		// minting a scope for a schedule would strand every token created
		// before this build for the sake of two fields.
		{Method: "POST", Path: "/digest/preview", Scope: domain.ScopeProjectsRead, Subsystem: subsystemDigest,
			Handler: func(s *Server) http.HandlerFunc { return s.handleDigestPreview }},
		{Method: "GET", Path: "/system/settings/digest", Scope: domain.ScopeProjectsRead, Subsystem: subsystemDigest,
			Handler: func(s *Server) http.HandlerFunc { return s.handleGetDigestSchedule }},
		{Method: "PUT", Path: "/system/settings/digest", Scope: domain.ScopeProjectsWrite, Subsystem: subsystemDigest,
			Handler: func(s *Server) http.HandlerFunc { return s.handleSetDigestSchedule }},

		// Whether the alarm would ring: every alert channel probed for
		// connectivity, and its stored secret checked against the key that is
		// supposed to open it. Nothing is sent (channelprobe).
		{Method: "GET", Path: "/system/channels", Scope: domain.ScopeProjectsRead, Subsystem: subsystemChannels,
			Handler: func(s *Server) http.HandlerFunc { return s.handleChannelHealth }},
	}
}

// monitorRoutes are cron and uptime: one concept watched from two sides
// (ADR 037), and the second area with a scope pair of its own.
//
// The reason is in domain/token.go. A cron monitor carries a ping key, and
// that key is a credential — anything holding it can report a backup as
// successful from anywhere on the internet. An uptime monitor is the mirror
// image: `monitors:write` is the permission to make this installation issue
// outbound HTTP requests to an address of the caller's choosing, unattended,
// forever. The SSRF guard decides which addresses; this decides who may ask
// (ADR 016). A token minted for a dashboard has business with neither.
func monitorRoutes() []route {
	return []route{
		{Method: "GET", Path: "/projects/{id}/monitors/cron", Scope: domain.ScopeMonitorsRead, Subsystem: subsystemCrons,
			Handler: func(s *Server) http.HandlerFunc { return s.handleListCronMonitors }},
		{Method: "POST", Path: "/projects/{id}/monitors/cron", Scope: domain.ScopeMonitorsWrite, Subsystem: subsystemCrons,
			Handler: func(s *Server) http.HandlerFunc { return s.handleCreateCronMonitor }},
		{Method: "GET", Path: "/projects/{id}/monitors/cron/{monitorID}", Scope: domain.ScopeMonitorsRead, Subsystem: subsystemCrons,
			Handler: func(s *Server) http.HandlerFunc { return s.handleGetCronMonitor }},
		// PUT because an edit is idempotent: sending the same body twice
		// leaves the same monitor, and every field is optional so a partial
		// edit is not a second verb.
		{Method: "PUT", Path: "/projects/{id}/monitors/cron/{monitorID}", Scope: domain.ScopeMonitorsWrite, Subsystem: subsystemCrons,
			Handler: func(s *Server) http.HandlerFunc { return s.handleUpdateCronMonitor }},
		{Method: "DELETE", Path: "/projects/{id}/monitors/cron/{monitorID}", Scope: domain.ScopeMonitorsWrite, Subsystem: subsystemCrons,
			Handler: func(s *Server) http.HandlerFunc { return s.handleDeleteCronMonitor }},
		{Method: "GET", Path: "/projects/{id}/monitors/cron/{monitorID}/checkins", Scope: domain.ScopeMonitorsRead, Subsystem: subsystemCrons,
			Handler: func(s *Server) http.HandlerFunc { return s.handleListCheckIns }},

		{Method: "GET", Path: "/projects/{id}/monitors/uptime", Scope: domain.ScopeMonitorsRead, Subsystem: subsystemUptime,
			Handler: func(s *Server) http.HandlerFunc { return s.handleListUptimeMonitors }},
		{Method: "POST", Path: "/projects/{id}/monitors/uptime", Scope: domain.ScopeMonitorsWrite, Subsystem: subsystemUptime,
			Handler: func(s *Server) http.HandlerFunc { return s.handleCreateUptimeMonitor }},
		{Method: "GET", Path: "/monitors/uptime/{monitorID}", Scope: domain.ScopeMonitorsRead, Subsystem: subsystemUptime,
			Handler: func(s *Server) http.HandlerFunc { return s.handleGetUptimeMonitor }},
		{Method: "DELETE", Path: "/monitors/uptime/{monitorID}", Scope: domain.ScopeMonitorsWrite, Subsystem: subsystemUptime,
			Handler: func(s *Server) http.HandlerFunc { return s.handleDeleteUptimeMonitor }},
		{Method: "POST", Path: "/monitors/uptime/{monitorID}/enabled", Scope: domain.ScopeMonitorsWrite, Subsystem: subsystemUptime,
			Handler: func(s *Server) http.HandlerFunc { return s.handleSetUptimeMonitorEnabled }},
		{Method: "GET", Path: "/monitors/uptime/{monitorID}/results", Scope: domain.ScopeMonitorsRead, Subsystem: subsystemUptime,
			Handler: func(s *Server) http.HandlerFunc { return s.handleUptimeResults }},
		{Method: "GET", Path: "/monitors/uptime/{monitorID}/daily", Scope: domain.ScopeMonitorsRead, Subsystem: subsystemUptime,
			Handler: func(s *Server) http.HandlerFunc { return s.handleUptimeDaily }},

		// The public status page's heading, on the same shelf as the digest
		// schedule and with the same scopes: it is an installation-wide
		// setting written by an operator, and what makes it public is the
		// page, not the endpoint that configures it (ADR 017).
		{Method: "GET", Path: "/system/settings/status-page", Scope: domain.ScopeProjectsRead, Subsystem: subsystemStatus,
			Handler: func(s *Server) http.HandlerFunc { return s.handleGetStatusPageSettings }},
		{Method: "PUT", Path: "/system/settings/status-page", Scope: domain.ScopeProjectsWrite, Subsystem: subsystemStatus,
			Handler: func(s *Server) http.HandlerFunc { return s.handleSetStatusPageSettings }},
	}
}

// observabilityRoutes are the three observability areas: tracing, uploaded
// artefacts, release health, and the suspect commit that ties them together.
//
// All read with projects:read and written with projects:write, and none gets a
// scope pair of its own. Nothing here is a credential for somebody else's
// system and nothing here makes this server talk to an address a caller chose,
// which are the two reasons the areas above have one; a third pair would
// strand every token minted before this build for the sake of a read
// (domain/token.go).
func observabilityRoutes() []route {
	return []route{
		// Tracing: the same dashboard question the stats endpoints answer,
		// asked about latency instead of errors.
		//
		// The series route puts the transaction name in the path rather than
		// in a query parameter. It is an identifier, not a filter, and a path
		// segment is what makes "this transaction" a resource a client can
		// link to.
		{Method: "GET", Path: "/projects/{id}/transactions", Scope: domain.ScopeProjectsRead, Subsystem: subsystemTransactions,
			Handler: func(s *Server) http.HandlerFunc { return s.handleListTransactions }},
		{Method: "GET", Path: "/projects/{id}/transactions/{name}/series", Scope: domain.ScopeProjectsRead, Subsystem: subsystemTransactions,
			Handler: func(s *Server) http.HandlerFunc { return s.handleTransactionSeries }},
		{Method: "GET", Path: "/projects/{id}/traces/{traceID}", Scope: domain.ScopeProjectsRead, Subsystem: subsystemTransactions,
			Handler: func(s *Server) http.HandlerFunc { return s.handleGetTrace }},

		// Uploaded scripts and source maps. The upload is a POST of the
		// archive itself, so it is the one route in this table whose body is
		// not JSON (artifacts_handler.go).
		{Method: "GET", Path: "/projects/{id}/artifacts", Scope: domain.ScopeProjectsRead, Subsystem: subsystemArtifacts,
			Handler: func(s *Server) http.HandlerFunc { return s.handleListArtifacts }},
		{Method: "POST", Path: "/projects/{id}/artifacts", Scope: domain.ScopeProjectsWrite, Subsystem: subsystemArtifacts,
			Handler: func(s *Server) http.HandlerFunc { return s.handleUploadArtifacts }},
		{Method: "DELETE", Path: "/projects/{id}/artifacts/{artifactID}", Scope: domain.ScopeProjectsWrite, Subsystem: subsystemArtifacts,
			Handler: func(s *Server) http.HandlerFunc { return s.handleDeleteArtifact }},

		// Release health. Two routes because they answer two questions and
		// only one of them is about a release anybody has named yet. "Which
		// of my releases is the bad one" is what somebody opens the page
		// with, and making them guess a version to find out would be the page
		// refusing to answer its own first question.
		{Method: "GET", Path: "/projects/{id}/health", Scope: domain.ScopeProjectsRead, Subsystem: subsystemHealth,
			Handler: func(s *Server) http.HandlerFunc { return s.handleProjectHealth }},
		{Method: "GET", Path: "/projects/{id}/releases/{version}/health", Scope: domain.ScopeProjectsRead, Subsystem: subsystemHealth,
			Handler: func(s *Server) http.HandlerFunc { return s.handleReleaseHealth }},

		// Which change probably caused an issue. Read with projects:read,
		// like the issue it hangs off: it names commits, but every one of
		// them is already on the release page that the same scope opens.
		{Method: "GET", Path: "/projects/{id}/issues/{issueID}/suspects", Scope: domain.ScopeProjectsRead, Subsystem: subsystemSuspects,
			Handler: func(s *Server) http.HandlerFunc { return s.handleIssueSuspects }},

		// The same issue, as one document instead of five calls. It is the
		// only route in this table that does not answer JSON: the body is
		// markdown, because its reader is a language model and a document
		// with headings costs it less than a record it has to re-derive the
		// shape of (bundle_handler.go, ADR 022).
		//
		// projects:read, like everything it is assembled from. It exposes
		// nothing the four endpoints it replaces do not, and giving the
		// convenient form of a read a stricter scope than the inconvenient
		// one would only teach people to use the inconvenient one.
		{Method: "GET", Path: "/projects/{id}/issues/{issueID}/bundle", Scope: domain.ScopeProjectsRead, Subsystem: subsystemBundle,
			Handler: func(s *Server) http.HandlerFunc { return s.handleIssueBundle }},
	}
}

// unversionedRoute is a path this server answers outside the versioned API.
//
// They are in a table for the same reason the versioned ones are: the
// OpenAPI gate has to know about them. They are in a *separate* table because
// they are not part of the frozen contract in the same sense — an SDK builds
// the ingest path out of a DSN and it cannot move (ADR 002), a crontab line
// builds the ping path out of a key, and a stranger opens the status page in
// a browser. None of them will ever be versioned, because none of their
// callers could follow a version bump.
type unversionedRoute struct {
	Method string
	Path   string
	// Why is the one-line reason this path lives outside /api/v1/. It is
	// carried here rather than in a comment so the OpenAPI document and this
	// table can be checked to say the same thing.
	Why string
}

// unversionedRoutes are the three public surfaces with no version in them.
//
// The emulated /api/0/ surface is deliberately absent: it is not this
// product's API at all, it is a foreign protocol this product answers so that
// `sentry-cli` works, and its shape is fixed by recorded traffic rather than
// by a decision anyone here gets to make (ADR 013). Freezing it would be
// claiming authorship of somebody else's contract, and documenting it in this
// product's OpenAPI would tell a reader it is a surface they may build on.
// `compat/sentry-cli/fixtures/` is where that surface is specified, and
// regrabbing is how it changes.
func unversionedRoutes() []unversionedRoute {
	return []unversionedRoute{
		{Method: "POST", Path: "/api/{projectID}/envelope/",
			Why: "an SDK builds this path out of a DSN; the protocol owns it, not us (ADR 002)"},
		{Method: "OPTIONS", Path: "/api/{projectID}/envelope/",
			Why: "the preflight a browser sends before the POST above (cors.go, ADR 030)"},
		{Method: "GET", Path: "/ping/{pingKey}",
			Why: "a crontab line is the client, and the key in the path is the whole credential (ADR 016)"},
		{Method: "POST", Path: "/ping/{pingKey}",
			Why: "the same surface for a client that can only POST (ADR 016)"},
		{Method: "GET", Path: "/status/{slug}",
			Why: "a stranger opens this in a browser; it is a page, not an endpoint (ADR 017)"},
		{Method: "POST", Path: mcpPath,
			Why: "an MCP client's URL lives in a config file a version bump cannot rewrite, " +
				"and `/mcp` is that protocol's convention, not ours (ADR 022)"},
	}
}

// mcpPath is where the fourth client connects.
//
// Outside /api/ deliberately: everything under that prefix is either this
// product's versioned API or a protocol somebody else owns, and MCP is a third
// thing — a transport for the same API, addressed the way every MCP server is
// addressed so that pointing a client at it needs no documentation.
const mcpPath = "/mcp"

// guard wraps a route's handler in what its table entry declares.
//
// The authentication decision is made here, from the data, rather than at the
// call site: a route that forgot `requireAuth` used to be a line that looked
// exactly like every other line, and the only thing standing between that
// typo and an open endpoint was somebody noticing it in review.
func (rt route) guard(s *Server) http.Handler {
	handler := http.Handler(rt.Handler(s))
	if !rt.Public {
		return s.requireAuth(rt.Scope, handler)
	}
	if rt.Throttled {
		return s.throttleAuth(handler)
	}
	return handler
}

// deprecationHeader is set on every answer served under the beta prefix.
//
// The value is the literal `true` of the Deprecation header's boolean form:
// the resource is deprecated, and no date is claimed for when it became so
// because the honest answer is "the release that froze v1" and a client cannot
// do anything with a date it cannot compare against.
const deprecationHeader = "Deprecation"

// successorLink points a reader at the route that replaces the one they used.
//
// One constant rather than a per-route Link header: every beta path maps to
// the same path under the frozen prefix, so the useful thing to say is the
// prefix, and a header computed per request would be five string joins per
// call to say something that never varies.
const successorLink = `</api/` + APIVersion + `>; rel="successor-version"`

// announceLegacyPrefix marks every answer served under the beta prefix.
//
// It decides from the request path, from outside the whole API router, rather
// than wrapping each aliased route. That is not tidiness — it is the only
// placement that works, and the gate found out the hard way.
//
// A per-route wrapper sits *inside* requireCSRFHeader, and requireCSRFHeader
// answers 403 on its own for every request that changes something and carries
// no CSRF header. So the wrapper never ran for POST, PUT or DELETE, and the
// deprecation notice appeared on exactly the half of the API nobody has to
// migrate — the reads. A client would have been told its writes were fine
// right up until the release that deleted them. Deciding out here also covers
// the router's own 404s and 405s under the beta prefix, which is the right
// answer to "is this path still there": deprecated, and no.
//
// The headers go on before the handler runs, because a handler that has
// already written its status line — the event stream, and anything that fails
// early — cannot have a header added afterwards.
func announceLegacyPrefix(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == legacyAPIPrefix || strings.HasPrefix(r.URL.Path, legacyAPIPrefix+"/") {
			header := w.Header()
			header.Set(deprecationHeader, "true")
			header.Set("Link", successorLink)
		}
		next.ServeHTTP(w, r)
	})
}
