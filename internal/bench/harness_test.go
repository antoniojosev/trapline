package bench

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/adapters/argon2id"
	"github.com/antoniojosev/trapline/internal/adapters/httpapi"
	"github.com/antoniojosev/trapline/internal/adapters/ratelimit"
	"github.com/antoniojosev/trapline/internal/adapters/sqlite"
	"github.com/antoniojosev/trapline/internal/clientip"
	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/engine"
	"github.com/antoniojosev/trapline/internal/ports"
	"github.com/antoniojosev/trapline/internal/sourcemap"
	"github.com/antoniojosev/trapline/internal/usecase"
)

// benchRateLimitPerMinute lifts spike protection out of the way.
//
// The shipped default is 2000/minute, which is ~33 events/s and therefore
// below the very target this gate exists to verify. Leaving it in place would
// make the benchmark measure domain.DefaultRateLimitPerMinute rather than the
// ingest path, and the gate would report the same number forever no matter
// what happened to the code.
const benchRateLimitPerMinute = 10_000_000

// benchIPRateLimit does the same for the per-address ceiling. Zero means "no
// threshold", not "no limiter": the guard still runs on every request, so
// whatever it costs is inside the throughput this gate publishes.
const benchIPRateLimit = 0

// requireBenchEnabled skips unless the caller asked for a measurement.
//
// Two independent guards. TRAPLINE_BENCH keeps a throughput measurement out of
// `go test ./...`, where it would add tens of seconds to every run and tempt
// someone to delete it. The -race guard is the important one: `make check`
// runs the suite under the race detector, whose instrumentation costs several
// times the work being measured, so a figure produced there would be fiction
// and a threshold applied to it would fail for reasons that have nothing to do
// with ingestion.
func requireBenchEnabled(tb testing.TB) {
	tb.Helper()
	if os.Getenv("TRAPLINE_BENCH") == "" {
		tb.Skip("set TRAPLINE_BENCH=1 to measure ingest throughput (see internal/bench/doc.go)")
	}
	if raceEnabled {
		tb.Skip("throughput under -race is instrumentation, not ingestion; run without it")
	}
}

// envInt reads a positive override, falling back to the default.
func envInt(tb testing.TB, name string, fallback int) int {
	tb.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		tb.Fatalf("%s = %q, want a positive integer", name, raw)
	}
	return value
}

// stack is one wired installation: real SQLite, real limiter, real HTTP.
//
// Nothing here is a stub. The claim under test is about what an operator's
// server does with an SDK's bytes, and a benchmark against a fake repository
// would measure this file rather than the product.
type stack struct {
	db     *sqlite.DB
	server *httptest.Server
	dsn    domain.DSN
	client *http.Client
	// projectID is the project the benchmark writes into.
	projectID int64
	// ingestUnlimited is the same use case over the same repository but with
	// the rate limiter replaced by one that always says yes. It exists so the
	// layered breakdown can isolate the SQLite write: the layer below it uses
	// the same limiter, so the difference between them is the database and
	// nothing else.
	ingestUnlimited *usecase.Ingest
}

// newStack builds a fresh installation on a fresh database file.
//
// Fresh per repetition on purpose: an empty table, a cold page cache and an
// empty WAL are the same starting conditions every time, so repetitions are
// independent samples instead of a decay curve.
func newStack(tb testing.TB) *stack { return newStackFor(tb, false) }

// newStackFor builds the same installation, optionally with symbolication
// switched on and the recorded source map already uploaded.
//
// A flag rather than a second builder: the whole value of the source-map gate
// is that the two measurements differ in one wire and nothing else, and two
// builders would eventually drift into measuring two different servers.
func newStackFor(tb testing.TB, sourceMaps bool) *stack {
	tb.Helper()
	ctx := context.Background()

	db, err := sqlite.Open(ctx, filepath.Join(tb.TempDir(), "trapline.db"))
	if err != nil {
		tb.Fatalf("opening database: %v", err)
	}
	tb.Cleanup(func() { _ = db.Close() })

	origin := domain.Origin{Scheme: "http", Host: "errors.example.test"}
	projectRepo := sqlite.NewProjectRepository(db)
	projects := usecase.NewProjects(projectRepo, projectRepo, benchClock{}, origin)
	limiter := ratelimit.New(projectRepo)

	issueRepo := sqlite.NewIssueRepository(db)
	// The events this gate sends name a release, because the ones an SDK
	// sends do: every such event pays for the upsert that records it, inside
	// the same transaction that stores it (ADR 032). A gate that measured an
	// assembly nobody ships would be measuring nothing.
	releaseRepo := sqlite.NewReleaseRepository(db)
	releases := usecase.NewReleases(releaseRepo, releaseRepo, projectRepo, benchClock{})
	ingest := usecase.NewIngest(issueRepo, limiter, benchScrubber(), benchClock{}, false)
	// Tracing is wired in and switched on below, because the transaction
	// workload measures the same public endpoint the error workloads do. A
	// harness that mounted it only for that test would be measuring a second
	// assembly, which is exactly what internal/wiring exists to prevent.
	traceRepo := sqlite.NewTraceRepository(db)
	ingest = ingest.WithTransactions(usecase.NewTransactions(
		traceRepo, projectRepo, benchScrubber()))

	view, err := projects.Create(ctx, "bench")
	if err != nil {
		tb.Fatalf("creating the benchmark project: %v", err)
	}
	// The transaction category is switched on and the sampling rate is pinned
	// rather than inherited. Pinned, because a benchmark whose cost per
	// transaction moved with a default somebody changed elsewhere would report
	// a different number for a reason unrelated to the code under test; pinned
	// to what ships, so the figure describes the shipped configuration.
	benchTracesSampleRate := domain.DefaultTracesSampleRate
	if err := projects.SetConfig(ctx, view.Project.ID, domain.ProjectConfig{
		RateLimitPerMinute:      benchRateLimitPerMinute,
		EnabledCategories:       []string{"error", "transaction"},
		TracesSampleRateSetting: &benchTracesSampleRate,
	}); err != nil {
		tb.Fatalf("raising the rate limit: %v", err)
	}
	dsn, found := view.PrimaryDSN()
	if !found {
		tb.Fatal("the new project has no DSN")
	}

	if sourceMaps {
		// The map goes in through the repository rather than through the
		// upload API: what is being measured is the resolution path, and
		// putting a multipart request in front of it would put the upload's
		// cost inside a number that is not about it.
		artifactRepo := sqlite.NewArtifactRepository(db)
		if _, err := artifactRepo.Store(ctx, domain.Artifact{
			ProjectID: view.Project.ID,
			DebugID:   benchDebugID,
			Name:      "~/bundle.min.js.map",
			Kind:      domain.ArtifactSourceMap,
			CreatedAt: benchClock{}.Now(),
		}, readBenchMap(tb)); err != nil {
			tb.Fatalf("uploading the benchmark's source map: %v", err)
		}
		ingest = ingest.WithSourceMaps(usecase.NewSymbolicator(
			artifactRepo, releaseRepo, sourcemap.NewCache(0), benchClock{}))
	}

	auth := usecase.NewAuth(
		sqlite.NewAdminRepository(db),
		sqlite.NewSessionRepository(db),
		argon2id.New(),
		benchClock{},
	)
	// No panel: the embedded assets are irrelevant to ingestion and mounting
	// them would only add router work to every measured request.
	api := httpapi.NewServer(auth, projects, usecase.NewTokens(sqlite.NewTokenRepository(db), benchClock{}),
		ingest, usecase.NewIssues(issueRepo).WithResolution(releaseRepo, benchClock{}),
		usecase.NewStats(sqlite.NewStatsRepository(db), issueRepo, benchClock{}), origin, "bench").
		WithReleases(releases)
	// The per-address ceiling is lifted for the same reason spike protection
	// is: every request in this run arrives from one loopback address, so the
	// shipped ceiling would eventually become the number this gate reports.
	// A zero limit keeps the limiter in the request path — its lock, its map
	// lookup and the X-Forwarded-For resolution are still charged to the
	// throughput figure — while removing the threshold (ADR 023).
	resolver, err := clientip.New(nil)
	if err != nil {
		tb.Fatalf("building the client-address resolver: %v", err)
	}
	api = api.WithIPLimits(resolver, benchIPRateLimit, benchIPRateLimit)
	server := httptest.NewServer(api.Handler())
	tb.Cleanup(server.Close)

	return &stack{
		db:              db,
		server:          server,
		dsn:             dsn,
		projectID:       view.Project.ID,
		ingestUnlimited: usecase.NewIngest(issueRepo, alwaysAllow{}, benchScrubber(), benchClock{}, false),
		// A client that actually reuses its connection, because an SDK does.
		// Reconnecting per event would charge the server a TCP handshake it
		// does not pay in production.
		client: &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64}},
	}
}

type benchClock struct{}

func (benchClock) Now() time.Time { return time.Now().UTC() }

// benchScrubber is the production default: the built-in key list, no operator
// additions. Scrubbing walks every event's maps before the write, so measuring
// with it switched off would quote a throughput no installation ever gets.
func benchScrubber() *domain.Scrubber { return domain.NewScrubber(nil, nil) }

// machineDescription records what produced a number, because a throughput
// figure without its machine is not reproducible and therefore not evidence.
func machineDescription() string {
	description := fmt.Sprintf("%s/%s, %d CPU, %s",
		runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version())
	if model, found := cpuModel(); found {
		description = model + " (" + description + ")"
	}
	return description
}

// cpuModel reads the CPU name on Linux, where CI runs. Elsewhere the
// GOOS/GOARCH line is all the identification available, which is enough to
// stop someone comparing an ARM laptop's number against a runner's.
func cpuModel() (string, bool) {
	raw, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		name, value, found := strings.Cut(line, ":")
		if found && strings.TrimSpace(name) == "model name" {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
}

// post sends one prepared envelope the way an SDK does.
func (s *stack) post(ctx context.Context, envelope []byte) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/%d/envelope/", s.server.URL, s.dsn.ProjectID), bytes.NewReader(envelope))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/x-sentry-envelope")
	request.Header.Set("Content-Encoding", "gzip")
	request.Header.Set("X-Sentry-Auth",
		"Sentry sentry_version=7, sentry_client=sentry.python/2.18.0, sentry_key="+s.dsn.PublicKey)

	response, err := s.client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	// Drained, not merely closed: an undrained body keeps the connection out
	// of the pool, so every event would pay for a fresh TCP handshake that a
	// real SDK never pays.
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("ingest answered %d", response.StatusCode)
	}
	return nil
}

// discardIssues is a storage port that accepts everything and stores nothing.
//
// It isolates the SQLite write: running the identical use case against this
// and against the real repository turns the difference into the database's
// share of the cost, which is the number ADR 009 needs.
type discardIssues struct{ seen int }

var _ ports.IssueRepository = (*discardIssues)(nil)

func (d *discardIssues) RecordEvent(context.Context, *ports.RecordEventInput) (ports.RecordEventResult, error) {
	d.seen++
	return ports.RecordEventResult{Issue: domain.Issue{ID: 1}, New: d.seen == 1}, nil
}

func (d *discardIssues) CountsByStatus(context.Context, int64) (map[domain.IssueStatus]int64, error) {
	return nil, nil
}

func (d *discardIssues) List(context.Context, ports.IssueFilter) (ports.IssuePage, error) {
	return ports.IssuePage{}, nil
}

func (d *discardIssues) FindByID(context.Context, int64, int64) (domain.Issue, error) {
	return domain.Issue{}, domain.ErrIssueNotFound
}

func (d *discardIssues) SetStatus(context.Context, int64, int64, domain.IssueStatus) error {
	return nil
}

func (d *discardIssues) LatestEvents(context.Context, int64, int) ([]ports.StoredEvent, error) {
	return nil, nil
}

func (d *discardIssues) Tags(context.Context, int64) (map[string][]ports.TagCount, error) {
	return nil, nil
}

func (d *discardIssues) DeleteEventsBefore(context.Context, int64, time.Time, int) (int64, error) {
	return 0, nil
}

// alwaysAllow is the limiter for the layers measured below HTTP.
//
// The real limiter is a mutex and a map lookup per event; keeping it in the
// layered breakdown would attribute its cost to whichever layer happened to
// contain it. The HTTP measurement — the one the gate asserts on — uses the
// real one.
type alwaysAllow struct{}

func (alwaysAllow) Allow(context.Context, int64, engine.Category) (bool, error) { return true, nil }

// TestMain silences the server's request log.
//
// Production really does log a line per request, so this is not free
// throughput being claimed — it is the removal of an artefact. Under `go test`
// slog writes to the harness's stderr pipe, which is unbuffered and captured,
// and at a few thousand lines per second that pipe becomes a bigger cost than
// the SQLite transaction being measured. What would be measured is the test
// runner, not the product.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}
