// Package wiring assembles the application's use cases from an open database.
//
// It exists because there was a second assembly path. Production wired the
// stack one way and the tests wired it another, and the difference was
// invisible until a test asked whether a configuration change took effect and
// the answer was no — in the test only, for a reason that had nothing to do
// with what it was testing.
//
// One assembly, used by both, so a test cannot exercise a shape that never
// ships. It deliberately knows nothing about HTTP: the transport is an adapter
// over these, and keeping it out is what lets the HTTP adapter's own tests
// build a real stack without importing the thing that builds servers.
package wiring

import (
	"context"
	"log/slog"
	"time"

	"github.com/antoniojosev/trapline/internal/adapters/argon2id"
	"github.com/antoniojosev/trapline/internal/adapters/channelprobe"
	"github.com/antoniojosev/trapline/internal/adapters/notify"
	"github.com/antoniojosev/trapline/internal/adapters/ratelimit"
	"github.com/antoniojosev/trapline/internal/adapters/secrets"
	"github.com/antoniojosev/trapline/internal/adapters/sqlite"
	uptimecheck "github.com/antoniojosev/trapline/internal/adapters/uptime"
	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
	"github.com/antoniojosev/trapline/internal/scheduler"
	"github.com/antoniojosev/trapline/internal/sourcemap"
	"github.com/antoniojosev/trapline/internal/ssrfguard"
	"github.com/antoniojosev/trapline/internal/usecase"
)

// systemClock is the production clock.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// Options are the choices an assembly makes.
type Options struct {
	Origin domain.Origin
	// ScrubKeys are extra field names redacted before anything is persisted.
	ScrubKeys []string
	// Debug turns on the ingest decision log.
	Debug bool
	// Clock is injected so tests can assert timestamps. Nil means the system
	// clock.
	Clock ports.Clock
	// Logger is where the background jobs report. Nil means slog's default.
	Logger *slog.Logger
	// SecretKeyPath is the file holding the key that encrypts alert channel
	// credentials. Empty means `<database>.key`, which is where an
	// installation that never thinks about this gets one — created the first
	// time a channel is stored and not before (ADR 015).
	SecretKeyPath string
	// UptimeAllowPrivate is the installation's half of the permission for a
	// monitor to reach an address that is not globally routable. Off by
	// default, and useless on its own: the monitor must also carry
	// allow_private (ADR 016).
	UptimeAllowPrivate bool
	// Version identifies this build to whatever an uptime monitor checks. An
	// unattended request that arrives without a User-Agent is one somebody
	// eventually blocks without knowing what it was.
	Version string
	// SourceMapCacheBytes is the budget for parsed source maps held in memory.
	// Zero means sourcemap.DefaultCacheBytes. It is a budget rather than a
	// count of maps because a parsed map's size is decided by whoever uploads
	// it, and a cache bounded by entries is a cache whose memory use is chosen
	// by somebody else (ADR 018).
	SourceMapCacheBytes int64
	// SessionWindowSize is the ceiling on sessions release health tracks in
	// memory at once. Zero means domain.DefaultSessionWindowSize (ADR 008).
	SessionWindowSize int
}

// Stack is everything the transports need.
type Stack struct {
	Auth      *usecase.Auth
	Projects  *usecase.Projects
	Tokens    *usecase.Tokens
	Ingest    *usecase.Ingest
	Issues    *usecase.Issues
	Stats     *usecase.Stats
	Releases  *usecase.Releases
	Retention *usecase.Retention
	// Alerts is the channels, the rules and the delivery log.
	Alerts *usecase.Alerts
	// Notifier drains the outbox. Held so the CLI and the tests can run one
	// pass by hand, exactly as Retention is.
	Notifier *usecase.Notifier
	// Digest is the weekly report. It is always assembled — previewing one is
	// what somebody does before configuring anywhere to send it — but its job
	// is only registered when a channel has asked for it (ADR 014).
	Digest *usecase.Digest
	// Channels is the health of the alert channels: whether the secrets key
	// opens them and whether this machine can reach them.
	Channels *usecase.ChannelHealth
	// Crons is cron monitoring: the monitors, the two ways a run reports
	// itself, and the watcher that notices when one does not.
	Crons *usecase.Crons
	// Feed is the live issue stream. It is here rather than inside Ingest so
	// the transport can subscribe to the same object the ingest path
	// publishes to, without either of them knowing about the other.
	Feed *usecase.Broadcaster
	// Uptime is the monitors and their history. Always assembled — listing
	// monitors is what somebody does before adding one — but its job is only
	// registered when a monitor is enabled (ADR 014).
	Uptime *usecase.Uptime
	// StatusPage is the public page: which monitors it shows, what it says
	// about them, and the thirty seconds it remembers a rendering for. No job
	// and no goroutine — it is built when somebody asks for it (ADR 017).
	StatusPage *usecase.StatusPage
	// Artifacts is the uploaded scripts and source maps, and the chunked
	// protocol they arrive by. Always assembled — listing what a project
	// holds is what somebody does when an upload starts failing — and it
	// registers no job of its own: what expires here expires on the retention
	// pass, because it is the same promise on the same clock (ADR 018).
	Artifacts *usecase.Artifacts
	// Transactions is tracing: the per-minute aggregates, the percentiles
	// computed when somebody asks, and the sampled waterfalls. Always
	// assembled — reading an empty performance page is what somebody does
	// before switching the category on — but its downsampling job is only
	// registered when a project accepts transactions (ADR 014).
	Transactions *usecase.Transactions
	// Downsample folds closed minute buckets into their hours. Held so the
	// CLI and the gate can run one pass by hand, exactly as Retention and
	// Notifier are.
	Downsample *usecase.Downsample
	// Health is release health: the bounded in-memory window that turns
	// session updates into counters, and the crash-free rate read back out of
	// them. Always assembled — opening an empty release-health page is what
	// somebody does before switching the category on — but its flush job is
	// only registered when a project accepts sessions (ADR 005, ADR 014).
	//
	// It is exposed on the stack rather than hidden inside Ingest because
	// something has to drain it on the way down: a clean stop that lost the
	// last minute of counters would be losing them on purpose (ADR 008).
	Health *usecase.Health
	// Scheduler holds the background jobs. Nothing in it runs until Start is
	// called, so building a stack in a test costs no goroutines.
	Scheduler *scheduler.Scheduler
	// ArtifactStore is the storage under Artifacts, held as the port rather
	// than the adapter. Exposed beside the use case, and not instead of it,
	// because the tests that prove symbolication works have to put a source
	// map somewhere real and have no upload to do it with (ADR 009).
	ArtifactStore ports.ArtifactRepository
	// Symbolicator turns minified frames back into source on the ingest path.
	// Exposed so a gate can ask it to resolve one event without going through
	// HTTP.
	Symbolicator *usecase.Symbolicator
	// Suspects is "which change probably caused this issue". Always
	// assembled and it registers no job: it is computed when somebody asks,
	// because both halves of its input move — a commit set arrives after the
	// first error as often as before it, and a frame's path changes the
	// moment somebody uploads the maps that resolve it (ADR 019).
	Suspects *usecase.Suspects
	// Bundle is one issue rendered as a document. Always assembled, and it is
	// a projection rather than a reader of its own: it composes Issues, Stats
	// and Suspects, so there is no second answer to keep in step with the one
	// the panel shows (ADR 006, ADR 022).
	Bundle *usecase.Bundle
}

// New assembles the use cases over an open database.
func New(db *sqlite.DB, options Options) *Stack {
	clock := options.Clock
	if clock == nil {
		clock = systemClock{}
	}

	projectRepo := sqlite.NewProjectRepository(db)
	issueRepo := sqlite.NewIssueRepository(db)
	statsRepo := sqlite.NewStatsRepository(db)
	// The release repository also owns the resolution columns of `issues`.
	// One type for both because they are one subject — when something was
	// fixed is a fact about which release it was fixed in — and because it
	// keeps every write outside the ingest transaction in one file.
	releaseRepo := sqlite.NewReleaseRepository(db)

	// The limiter wraps the configuration store rather than sitting beside
	// it, so every write goes through the thing that caches and no caller has
	// to remember to invalidate anything.
	limiter := ratelimit.New(projectRepo)

	jobs := scheduler.New(scheduler.Options{Logger: options.Logger})

	// Configuration is what decides whether an opt-in subsystem has any work,
	// so a write to it has to reach the scheduler. Wrapping the store is the
	// same trick as the limiter's, for the same reason: the one way to write
	// configuration is through the thing that has to react to it, so no
	// assembly can be built that forgets to.
	config := &notifyOnWrite{store: limiter, notify: jobs.Reevaluate}

	releases := usecase.NewReleases(releaseRepo, releaseRepo, projectRepo, clock)

	// Built unconditionally, and it costs nothing until somebody subscribes:
	// publishing with no subscribers is one atomic load. Making it optional
	// would be a second assembly shape, which is the thing this package
	// exists to prevent.
	feed := usecase.NewBroadcaster()

	// Alerting. The cipher is deferred: naming a key file does not create one,
	// so an installation that never configures a channel never grows a
	// `<db>.key` beside its database (ADR 005 applied to a file).
	keyPath := options.SecretKeyPath
	if keyPath == "" {
		keyPath = secrets.KeyPathFor(db.Path())
	}
	alertRepo := sqlite.NewAlertRepository(db, secrets.At(keyPath))
	sender := notify.New(notify.Options{})
	// The scheduler is told to re-ask whether the notifier has work whenever
	// a channel is added or removed, and it is a constructor argument rather
	// than a setter for the reason ADR 014 gives: a hook that can be left
	// unset is a hook that will be, and the symptom is a subsystem that only
	// starts after a restart.
	alerts := usecase.NewAlerts(alertRepo, sender, clock, options.Origin, jobs.Reevaluate)
	notifier := usecase.NewNotifier(alertRepo, sender, clock, options.Logger)
	// Registered with its condition rather than unconditionally: with no
	// channel configured there is nobody to notify, so the job does not exist
	// — no timer, no goroutine, no row in /system/jobs (ADR 005, ADR 014).
	jobs.Register(notifier.Job(), alertRepo.HasChannels)

	// The one collaboration between two repositories in this assembly, and it
	// buys the guarantee the outbox rests on: the notification about a new
	// issue is written by the same transaction that created the issue.
	issueRepo = issueRepo.WithAlerts(alertRepo, options.Origin)

	// Cron monitoring. The repository gets the same alerting seam the issue
	// repository has, and for the same reason: the notification about a
	// backup that did not run is written by the transaction that decided it
	// did not run (ADR 015, ADR 016).
	cronRepo := sqlite.NewCronRepository(db).WithAlerts(alertRepo, options.Origin)
	crons := usecase.NewCrons(cronRepo, clock, options.Logger, jobs.Reevaluate)
	// Offered with its condition, like the notifier: with no monitor enabled
	// there is nothing to watch, so the job does not exist — no timer, no
	// goroutine, no row in /system/jobs (ADR 005, ADR 014). And the first
	// monitor brings it into existence without a restart, because creating
	// one calls the same Reevaluate a channel does.
	jobs.Register(crons.Job(), cronRepo.HasEnabledMonitors)

	// Tracing. The scrubber is the same one the ingest path uses, because a
	// span's `data` is where a database driver puts the statement it ran and
	// an HTTP client puts the headers it sent — as likely to carry a
	// credential as an event's request map, and redacted before it is written
	// rather than on the way out (SECURITY.md).
	traceRepo := sqlite.NewTraceRepository(db)
	transactions := usecase.NewTransactions(
		traceRepo, config, domain.NewScrubber(options.ScrubKeys, nil))
	downsample := usecase.NewDownsample(traceRepo, clock, options.Logger)
	// Offered with its condition, like every other optional subsystem: an
	// installation where no project accepts transactions has no goroutine, no
	// timer and no row in /system/jobs (ADR 005, ADR 014). The condition asks
	// the configuration rather than the tables, so switching tracing on
	// brings the job into existence before the first transaction rather than
	// after — the same Reevaluate a channel or a monitor triggers.
	jobs.Register(downsample.Job(), downsample.HasWork)

	// Named rather than built inline in the assembly below, because the
	// artefact use case needs it: an upload addresses its project the way a
	// deploy tool does — by slug or by numeric id — and that resolution lives
	// on this use case (ADR 013 §4). Two of them would be two answers to
	// "which project is `venekambio`".
	projects := usecase.NewProjects(projectRepo, config, clock, options.Origin)

	// Source maps, both directions, off one repository. It owns the artefacts
	// and the chunk staging area because one request turns the second into
	// the first; it is written by the upload surface and read on the ingest
	// path, and a second instance would be a second answer to what a project
	// currently holds. The cache is process-wide and bounded, because a parsed
	// map is the largest thing this product holds in memory whose size a user
	// chooses (ADR 018).
	artifactRepo := sqlite.NewArtifactRepository(db)
	artifacts := usecase.NewArtifacts(artifactRepo, artifactRepo, releases, projects, config, clock)
	symbolicator := usecase.NewSymbolicator(
		artifactRepo, releaseRepo, sourcemap.NewCache(options.SourceMapCacheBytes), clock).
		WithLogger(options.Logger)

	// Release health. The window is bounded here, from the installation-wide
	// setting, and never from anything a request carries: its whole purpose is
	// to be a ceiling somebody outside this process cannot move (ADR 008).
	healthRepo := sqlite.NewHealthRepository(db)
	health := usecase.NewHealth(healthRepo, clock, options.Logger, options.SessionWindowSize)
	// Offered with its condition, like every other optional subsystem: an
	// installation where no project accepts sessions has no goroutine, no
	// timer and no row in /system/jobs. The condition asks the configuration
	// rather than the table, so switching sessions on brings the flush into
	// existence before the first session rather than after.
	jobs.Register(health.Job(), health.HasWork)

	retention := usecase.NewRetention(projectRepo, limiter, issueRepo, statsRepo, clock).
		WithTraces(traceRepo).
		WithArtifacts(artifacts).
		WithSessions(healthRepo)
	// Registered without a condition, unlike everything that will follow it:
	// deleting what expired is what keeps the footprint promise, so it is the
	// one subsystem nobody opts into (ADR 014).
	jobs.Register(retention.Job(), nil)

	// The weekly digest, and the first genuinely conditional job in this
	// product. It is offered together with the question "has any channel
	// asked for it", so an installation that never configured one has no
	// goroutine, no timer and no row in the job list — which is the claim
	// ADR 005 makes, stated as code rather than as a comment.
	//
	// The answer changes while the server runs: the same Reevaluate that
	// brings the notifier into existence with the first channel brings this
	// job into existence with the first channel that ticks "digest", and
	// takes it away again with the last (usecase.Alerts, ADR 014).
	// The notification subsystem it asks is the channel repository itself
	// (ADR 035). It is taken from the assembly rather than offered as an
	// option, because the seam existed to let two branches be built in
	// parallel and there is now exactly one implementation of it: an Options
	// field would only be a way to assemble a server whose digest silently
	// has nowhere to go.
	weekly := usecase.NewDigest(projectRepo, statsRepo, sqlite.NewDigestRepository(db),
		sqlite.NewSettingsRepository(db), clock, options.Origin).
		WithChannels(alertRepo).
		WithLogger(options.Logger)
	jobs.Register(weekly.Job(), weekly.HasDigestChannels)

	// Uptime monitoring. The guard is built here, from the installation-wide
	// flag, and is the only thing in this assembly that decides whether an
	// outbound connection may happen at all — so it is a constructor argument
	// of both the checker and the use case rather than something either of
	// them could be assembled without (ADR 016, internal/ssrfguard).
	// `uptimecheck` and not `uptime`: the use case, the port and the adapter
	// all want that name, and the adapter is the one that loses because it is
	// named once, here, and nowhere else.
	guard := ssrfguard.New(nil, options.UptimeAllowPrivate)
	version := options.Version
	if version == "" {
		version = "dev"
	}
	uptimeRepo := sqlite.NewUptimeRepository(db).WithAlerts(alertRepo, options.Origin)
	monitors := usecase.NewUptime(
		uptimeRepo,
		uptimecheck.New(guard, version),
		guard,
		clock,
		options.Logger,
		jobs.Reevaluate,
	)
	// Registered with its condition, like the notifier and the digest: an
	// installation with no monitor enabled has no goroutine, no timer and no
	// row in /system/jobs, which is the claim ADR 005 makes stated as code.
	jobs.Register(monitors.Job(), uptimeRepo.HasEnabledMonitors)

	// The three the bundle projects, named rather than built inside the
	// literal: it composes them, and two assemblies of the same use case
	// would be two answers to keep in step.
	issues := usecase.NewIssues(issueRepo).WithResolution(releaseRepo, clock)
	stats := usecase.NewStats(statsRepo, issueRepo, clock)
	// Three repositories rather than one, because the question spans three:
	// which release the issue was first seen in, what went into that release,
	// and what the stacktrace says. None of them owns the answer, so none of
	// them is where it lives.
	suspects := usecase.NewSuspects(issueRepo, releaseRepo, releaseRepo)

	return &Stack{
		Uptime: monitors,
		// The public page reads the same repositories the panel does, and
		// decides nothing the panel does not: which project, whether it
		// publishes, which of its monitors are marked public.
		StatusPage: usecase.NewStatusPage(
			projectRepo, config, uptimeRepo, sqlite.NewSettingsRepository(db), clock,
		),
		Auth: usecase.NewAuth(
			sqlite.NewAdminRepository(db),
			sqlite.NewSessionRepository(db),
			argon2id.New(),
			clock,
		),
		Projects: projects,
		Tokens:   usecase.NewTokens(sqlite.NewTokenRepository(db), clock),
		Ingest: usecase.NewIngest(
			issueRepo,
			limiter,
			domain.NewScrubber(options.ScrubKeys, nil),
			clock,
			options.Debug,
		).WithFeed(feed).WithAlerts(alerts).WithCrons(crons).
			WithTransactions(transactions).WithSourceMaps(symbolicator).WithHealth(health),
		Issues:    issues,
		Stats:     stats,
		Releases:  releases,
		Retention: retention,
		Alerts:    alerts,
		Crons:     crons,
		Notifier:  notifier,
		Digest:    weekly,
		Channels: usecase.NewChannelHealth(
			alertRepo,
			// The prober is built here rather than passed in: connecting to a
			// host and hanging up is the same operation for all five channel
			// types, and there is nothing about it an assembly would want to
			// choose (channelprobe).
			channelprobe.New(),
		),
		Scheduler:     jobs,
		Artifacts:     artifacts,
		Feed:          feed,
		Transactions:  transactions,
		Downsample:    downsample,
		ArtifactStore: artifactRepo,
		Symbolicator:  symbolicator,
		Suspects:      suspects,
		// The document an agent reads. It is built from the three use cases
		// above rather than from the repositories under them, so the bundle
		// and the issue page can never disagree about the same issue.
		Bundle: usecase.NewBundle(issues, stats).
			WithSuspects(suspects).
			WithResolution(releaseRepo),
		Health: health,
	}
}

// notifyOnWrite is a configuration store that tells the scheduler when
// something changed.
//
// Reads pass straight through — they happen once per ingested event and must
// not grow a hop — and only a successful write notifies. A failed write that
// woke the scheduler would have it re-read the configuration that is still
// there, which is harmless but is also a lie about what happened.
type notifyOnWrite struct {
	store  ports.ProjectConfigStore
	notify func()
}

func (n *notifyOnWrite) ProjectConfig(ctx context.Context, projectID int64) (domain.ProjectConfig, error) {
	return n.store.ProjectConfig(ctx, projectID)
}

func (n *notifyOnWrite) SetProjectConfig(ctx context.Context, projectID int64, config domain.ProjectConfig) error {
	if err := n.store.SetProjectConfig(ctx, projectID, config); err != nil {
		return err
	}
	n.notify()
	return nil
}
