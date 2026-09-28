// Package app assembles the product from its parts.
//
// This is the only place that knows which adapter satisfies which port. Every
// other package depends on interfaces, which is what makes them testable and
// what keeps the wiring decisions visible in one file instead of scattered
// through constructors.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/antoniojosev/trapline/internal/adapters/httpapi"
	mcpadapter "github.com/antoniojosev/trapline/internal/adapters/mcp"
	"github.com/antoniojosev/trapline/internal/adapters/sqlite"
	"github.com/antoniojosev/trapline/internal/adapters/webui"
	"github.com/antoniojosev/trapline/internal/clientip"
	"github.com/antoniojosev/trapline/internal/config"
	"github.com/antoniojosev/trapline/internal/scheduler"
	"github.com/antoniojosev/trapline/internal/usecase"
	"github.com/antoniojosev/trapline/internal/wiring"
)

// shutdownGrace is how long in-flight requests get to finish on shutdown.
// Long enough for a slow response, short enough that a restart is not a
// noticeable outage.
const shutdownGrace = 10 * time.Second

// App holds an assembled installation.
type App struct {
	cfg      config.Config
	db       *sqlite.DB
	server   *http.Server
	Projects *usecase.Projects
	Tokens   *usecase.Tokens
	Auth     *usecase.Auth
	Issues   *usecase.Issues
	// Alerts is exposed so the local commands — the ones that read the
	// database rather than the API — can diagnose the alerting subsystem.
	Alerts *usecase.Alerts
	// Crons is exposed for the same reason Alerts is: the local commands can
	// diagnose the subsystem, and a test can drive one sweep by hand.
	Crons     *usecase.Crons
	retention *usecase.Retention
	// downsample is exposed for the same reason retention is: one pass can be
	// driven by hand, from the CLI and from the gate that has to prove an
	// hour built from sixty minutes reports the same percentiles.
	downsample *usecase.Downsample
	// health owns the in-memory session window, and is held here for one
	// reason: something has to drain it on the way down. ADR 008 accepts that
	// a crash loses the sessions in flight; it does not accept that a clean
	// stop does, and the difference between the two is this field.
	health *usecase.Health
	jobs   *scheduler.Scheduler
}

// Retention exposes the sweeper so the CLI can run one pass by hand.
func (a *App) Retention() *usecase.Retention { return a.retention }

// Downsample exposes the minute-to-hour fold so the CLI can run one pass by
// hand.
func (a *App) Downsample() *usecase.Downsample { return a.downsample }

// New opens the database and wires everything together.
func New(ctx context.Context, cfg config.Config, version string) (*App, error) {
	db, err := sqlite.Open(ctx, cfg.DBPath)
	if err != nil {
		return nil, err
	}

	stack := wiring.New(db, wiring.Options{
		Origin:             cfg.Origin,
		ScrubKeys:          cfg.ScrubKeys,
		Debug:              cfg.Debug,
		SecretKeyPath:      cfg.SecretKeyFile,
		UptimeAllowPrivate: cfg.UptimeAllowPrivate,
		Version:            version,
		// Megabytes at the boundary, bytes inside: an operator types a number
		// of megabytes and every layer below counts bytes, so the conversion
		// happens once, here.
		SourceMapCacheBytes: int64(cfg.SourceMapCacheMB) << 20,

		SessionWindowSize: cfg.SessionWindowSize,
	})

	api := httpapi.NewServer(stack.Auth, stack.Projects, stack.Tokens, stack.Ingest, stack.Issues,
		stack.Stats, cfg.Origin, version)
	// What is running in the background, readable from outside the process.
	// A background job nobody can see is one nobody notices has been failing
	// for a week (ADR 014).
	api = api.WithJobs(stack.Scheduler)
	// What shipped, what went into it, and what it broke (ADR 012).
	api = api.WithReleases(stack.Releases)
	// The live feed, so a panel left open finds out that something broke
	// without its reader pressing reload every minute.
	api = api.WithFeed(stack.Feed)
	// And the way to find out without a panel open at all (ADR 015).
	api = api.WithAlerts(stack.Alerts)
	// The weekly report, and whether the places it would be sent to can be
	// reached at all. Both are read-only from the API's side: the digest is
	// previewed here and delivered by its job, and the channel check opens a
	// connection and hangs up without sending anything (ADR 035).
	api = api.WithDigest(stack.Digest, stack.Channels)
	// Cron monitors, and with them the one public path this product adds
	// outside the protocol's own: /ping/{key}, which is what makes a backup
	// script instrumentable by adding a curl to the end of its crontab line
	// (ADR 016).
	api = api.WithCrons(stack.Crons)
	// The other half of the same question — whether the things this
	// installation watches are answering, and what they have been doing for
	// the last ninety days (ADR 016).
	api = api.WithUptime(stack.Uptime)
	// And the one page in this product that somebody outside the team reads:
	// a project's public status, rendered on the server, with no panel and no
	// session behind it (ADR 017).
	api = api.WithStatusPage(stack.StatusPage)
	// What is slow, what is failing, and one request's waterfall. The
	// percentiles are computed here, at read time, by merging the range's
	// sketches — never read out of a column, because a stored percentile
	// cannot be merged and would be wrong in a way nothing downstream could
	// see (ADR 007, ADR 020).
	api = api.WithTransactions(stack.Transactions)
	// The source maps, and the surface that receives them. It mounts both
	// this product's own endpoints and the ones sentry-cli speaks, because
	// the claim is that pointing an existing pipeline at this server is a
	// change of one environment variable (ADR 013, ADR 018).
	api = api.WithArtifacts(stack.Artifacts)

	// How many sessions a release started and how many of them crashed —
	// counted in a bounded in-memory window and stored only as totals per
	// release-hour, never one row per session (ADR 008, ADR 021).
	api = api.WithHealth(stack.Health)

	// Which change probably caused an issue: the intersection between the
	// files a release changed and the files its stacktrace walks through. It
	// is the half of ADR 019 that only exists once both source maps and
	// commit sets are in, which is why it is mounted last (ADR 019).
	api = api.WithSuspects(stack.Suspects)

	// One issue as a document instead of five calls, which is the shape the
	// consumer of this product's differentiator actually reads (ADR 022).
	api = api.WithBundle(stack.Bundle)

	// And the fourth client of ADR 006, over HTTP. It is given the API router
	// rather than any use case: an MCP tool is a call into this same table,
	// with the same credential and therefore the same scope check, which is
	// the only way the two surfaces cannot drift apart (ADR 022).
	//
	// No credential of its own — the empty string — because every request
	// arrives with one and a fallback here would be a token the operator
	// never configured, quietly widening what an unauthenticated caller could
	// reach if the route's own guard were ever removed.
	api = api.WithMCP(func(router http.Handler) http.Handler {
		return mcpadapter.New(
			mcpadapter.NewLoopbackCaller(router, "/api/"+httpapi.APIVersion),
			"", version,
		).Handler()
	})

	// Who the client is, and what one client may cost. Built here because it
	// is the only place that has the configuration, and applied unconditionally
	// because a defence an installation has to switch on is one most
	// installations run without (ADR 023).
	resolver, err := clientip.New(cfg.TrustedProxies)
	if err != nil {
		return nil, fmt.Errorf("reading the trusted proxies: %w", err)
	}
	api = api.WithIPLimits(resolver, cfg.IngestIPRateLimitPerMinute, cfg.AuthRateLimitPerMinute)

	panel, err := webui.Handler()
	if err != nil {
		return nil, fmt.Errorf("loading the panel: %w", err)
	}
	api = api.WithPanel(panel)

	return &App{
		cfg:        cfg,
		db:         db,
		server:     api.HTTPServer(cfg.Addr),
		Projects:   stack.Projects,
		Tokens:     stack.Tokens,
		Auth:       stack.Auth,
		Issues:     stack.Issues,
		retention:  stack.Retention,
		downsample: stack.Downsample,
		health:     stack.Health,
		Alerts:     stack.Alerts,
		Crons:      stack.Crons,
		jobs:       stack.Scheduler,
	}, nil
}

// DB exposes the database for the commands that operate on the file itself,
// such as backup.
func (a *App) DB() *sqlite.DB { return a.db }

// Close releases the database.
func (a *App) Close() error {
	if err := a.db.Close(); err != nil {
		return fmt.Errorf("closing database: %w", err)
	}
	return nil
}

// Serve runs the HTTP server until the process is asked to stop, then drains
// in-flight requests.
//
// Graceful shutdown is not a nicety here: this is where other systems send
// their errors, and dropping a connection mid-envelope during a restart means
// losing the report of the very incident someone is deploying a fix for.
func (a *App) Serve(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	a.logStartup()

	// The background jobs run alongside the server rather than on a schedule
	// outside it: this product ships as one binary and must not need a cron
	// entry to keep its own disk usage bounded. They share the server's
	// context, so a SIGTERM drains requests and ends the sweeps together.
	//
	// Only the jobs whose subsystem has work are started, and a job that has
	// none is not a sleeping goroutine, it is absent (ADR 005, ADR 014).
	if err := a.jobs.Start(ctx); err != nil {
		return fmt.Errorf("starting the background jobs: %w", err)
	}
	// After Shutdown, so a sweep that is mid-batch finishes rather than being
	// abandoned along with the process.
	defer a.jobs.Stop()

	// The sockets are opened here rather than inside the goroutines, so a port
	// already in use is an error this call returns instead of one that arrives
	// on a channel after the jobs have started.
	listeners, err := listen(ctx, a.cfg.Addr)
	if err != nil {
		return err
	}

	// Buffered for every listener, so a goroutine reporting a failure never
	// blocks on a channel nobody is reading any more.
	failed := make(chan error, len(listeners))
	var serving sync.WaitGroup
	for _, listener := range listeners {
		serving.Add(1)
		go func() {
			defer serving.Done()
			if err := a.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				failed <- err
			}
		}()
	}
	go func() {
		serving.Wait()
		close(failed)
	}()

	select {
	case err := <-failed:
		if err != nil {
			return fmt.Errorf("serving: %w", err)
		}
		return nil
	case <-ctx.Done():
		slog.Info("shutting down", "grace", shutdownGrace.String())
	}

	// A fresh context: the one above is already cancelled, and shutdown needs
	// its own budget to drain with.
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()

	if err := a.server.Shutdown(drainCtx); err != nil {
		return fmt.Errorf("shutting down: %w", err)
	}

	// After the server has drained and before the process ends, and both ends
	// of that sentence matter. After, because until the last request is done a
	// session update can still arrive and the window would be written twice.
	// Before, because whatever is settled and unwritten at this moment is
	// exactly what a stop would otherwise throw away — and ADR 008 accepts
	// losing the sessions still in flight, never losing the verdicts already
	// reached. The error is logged rather than returned: a failed flush of a
	// counter must not turn a clean shutdown into a failed one.
	if a.health != nil {
		if err := a.health.Drain(drainCtx); err != nil {
			slog.Error("draining the session window", "error", err)
		}
	}

	slog.Info("stopped")
	return nil
}

// listen opens the sockets the server accepts on.
//
// Not http.Server.ListenAndServe, which always asks Go for "tcp" and takes
// what it gets. On a dual-stack machine that is one socket bound to the IPv6
// wildcard which also accepts IPv4 — correct for every client that connects to
// it, and wrong for anything outside the kernel that cares which family a
// listener is *bound* to.
//
// Something does. Docker Desktop's WSL2 localhost relay mirrors only
// IPv4-bound sockets, so a server on the dual-stack wildcard is unreachable
// from every container on the machine while `ss` cheerfully reports it
// listening. The compatibility matrix failed with "no address this machine
// offers was reachable from a container" — a sentence that points nowhere near
// a socket family, and cost an afternoon to trace.
//
// So the address is read as what it says:
//
//   - an IPv4 literal (`0.0.0.0:9000`, `127.0.0.1:9000`) gets an IPv4 socket,
//     because that is what the operator asked for;
//   - an IPv6 literal gets an IPv6 socket, for the same reason;
//   - a hostname gets whatever it resolves to;
//   - **no host at all** (`:9000`) means every interface, and now actually
//     means it: two sockets, one per family, so neither a container that
//     resolves host.docker.internal to an IPv6 address nor a relay that only
//     watches IPv4 can miss it.
//
// The IPv4 socket is opened first on purpose. Go sets IPV6_V6ONLY on a "tcp6"
// listener, so the pair does not collide — but only in that order, because a
// dual-stack IPv6 socket would already own the port.
func listen(ctx context.Context, addr string) ([]net.Listener, error) {
	// A ListenConfig rather than net.Listen, so opening the socket is bound to
	// the same context everything else here is: a shutdown signal that arrives
	// during a slow bind is honoured instead of waited out.
	var sockets net.ListenConfig

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("reading the listen address %q: %w", addr, err)
	}

	if host != "" {
		network := "tcp"
		if ip := net.ParseIP(host); ip != nil {
			if ip.To4() != nil {
				network = "tcp4"
			} else {
				network = "tcp6"
			}
		}
		listener, err := sockets.Listen(ctx, network, addr)
		if err != nil {
			return nil, fmt.Errorf("listening on %s: %w", addr, err)
		}
		return []net.Listener{listener}, nil
	}

	var listeners []net.Listener
	for _, network := range []string{"tcp4", "tcp6"} {
		listener, err := sockets.Listen(ctx, network, addr)
		if err != nil {
			// One family missing is normal — a host with IPv6 disabled, a
			// container with no IPv4 route — and is not a reason to refuse to
			// start. Both missing is, and is reported below with the last
			// reason rather than a summary nobody can act on.
			slog.Debug("this machine offers no listener for a family",
				"network", network, "addr", addr, "error", err)
			continue
		}
		listeners = append(listeners, listener)
	}
	if len(listeners) == 0 {
		return nil, fmt.Errorf("listening on %s: no address family is available", addr)
	}
	return listeners, nil
}

func (a *App) logStartup() {
	slog.Info("listening",
		"addr", a.cfg.Addr,
		"origin", a.cfg.Origin.String(),
		"db", a.db.Path(),
	)
	if a.cfg.OriginIsLocal() {
		// Said plainly, because the symptom otherwise appears far from the
		// cause: DSNs would be handed out pointing at localhost and no SDK
		// outside this machine could ever reach them.
		slog.Warn("the public origin points at this machine, so the DSNs shown in the panel will only work locally; set -origin or TRAPLINE_ORIGIN to the address SDKs will actually use",
			"origin", a.cfg.Origin.String())
	}
	if a.cfg.UptimeAllowPrivate {
		// Said at warn level, once, at startup. This is the flag that lets a
		// URL somebody typed reach the metadata endpoint — with the monitor's
		// own opt-in as well — and an installation running with it should
		// have said so out loud at least once rather than only in a file
		// nobody reads after the first day (ADR 016, SECURITY.md).
		slog.Warn("uptime monitors that set allow_private may reach loopback, private and " +
			"link-local addresses, including the cloud metadata endpoint; " +
			"this is -uptime-allow-private, and only monitors that also ask for it are affected")
	}
	slog.Info("per-address limits",
		"ingest_per_minute", a.cfg.IngestIPRateLimitPerMinute,
		"auth_per_minute", a.cfg.AuthRateLimitPerMinute,
		"trusted_proxies", len(a.cfg.TrustedProxies),
	)
	if len(a.cfg.TrustedProxies) == 0 {
		// Said at info level, not debug. "Behind a proxy trusting nobody" and
		// "not behind a proxy" are the same process state and very different
		// deployments: in the first, every visitor is counted as the proxy and
		// the per-address limit looks like it works until the day it matters.
		slog.Info("no trusted proxies configured, so X-Forwarded-For is ignored and per-address limits count the direct peer; " +
			"set -trusted-proxies or TRAPLINE_TRUSTED_PROXIES if this server sits behind a reverse proxy")
	}
}
