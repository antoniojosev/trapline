# Architecture

A 5-minute read to get oriented before touching code. The decisions and their
reasoning live in [docs/adr/](docs/adr/); this is the map.

## The shape

One Go binary, hexagonal. The domain does not know what is outside; everything
outside implements ports the domain declares.

```
cmd/trapline/            main: startup and exit code only
internal/
  domain/                entities and rules. Stdlib only. No exceptions.
  ports/                 interfaces the domain needs
  usecase/               one use case end to end, no transport and no SQL
  scheduler/             the background jobs, and who gets a goroutine (ADR 014)
  engine/                generic event engine, its own boundary (ADR 004)
    sketch/              mergeable latency histogram, inside that boundary (ADR 020)
  clientip/              whose request this is, its own boundary (ADR 023)
  digest/                the weekly digest, rendered, its own boundary (ADR 035)
  ssrfguard/             may this server connect there? its own boundary (ADR 016)
  sourcemap/             from minified bundle to original code, its own boundary (ADR 018)
  adapters/
    cli/                 command line
    mcp/                 the fourth client: one table of tools, two transports (ADR 022)
    sqlite/              storage
    notify/              delivery of one alert over its five channels (ADR 015)
    secrets/             AES-256-GCM over those channels' credentials
    channelprobe/        is the alert channel reachable? without sending anything (ADR 035)
    ratelimit/           ceilings per project and per address (ADR 005, ADR 023)
    uptime/              the outbound HTTP check, with the guard's dialer (ADR 016)
  arch/                  the gate: a test that breaks the build if a boundary is crossed
docs/adr/                decisions, one per file, immutable
skills/fix-error/        what an agent does with all of this (ADR 022, ADR 040)
demo/                    the whole product in one pass, and the gate that keeps it true
```

## The two boundaries that are never crossed

1. **`internal/domain` imports stdlib only.** No SQL, no HTTP, no libraries.
2. **`internal/engine` imports nothing from the repo.** It is the generic event
   engine and will one day be a separate Apache-2.0 library (ADR 004). That
   discipline is the only thing that keeps the extraction cheap.

There are five more boundaries with the same shape and other reasons.
`internal/envelope` and `internal/clientip` are pure because both read input
chosen by an attacker and have to be testable exhaustively in isolation: the
first is fuzzed (ADR 002), the second decides whose request this is, and
everything that counts, limits or logs "the client" counts what it returned
(ADR 023). `internal/ssrfguard` is the third of that family and the most
literal: it decides whether this server may open a connection to an address
someone else chose, so the entire policy is a readable table in one file and
the dialer that enforces it lives next to it. That is what stops someone from
checking a host and then handing the **name** to `net.Dial`, which is textbook
DNS rebinding (ADR 016). `internal/digest` is pure for a different reason: its
output is pinned by golden fixtures, and that only means something while the
render is a function of its input and nothing else (ADR 035).
`internal/sourcemap` is the twin of `internal/envelope` one floor up: it reads
a file a user uploaded, produced by a bundler this product does not control,
and turns it into memory whose size the document itself chooses, so it is
fuzzed in isolation and imports nothing (ADR 018). `internal/artifactbundle` is
the door that file comes in through: it reads the ZIP with `manifest.json` that
`sentry-cli sourcemaps upload` sends, a compressed container chosen by whoever
holds a token, and its limits apply to what the archive **expands to**, not to
the size it declares (ADR 018).

All seven are verified by `internal/arch` on every `go test`, and by `depguard`
in the lint. The duplication is intentional: the test runs even if you do not
have golangci-lint installed.

## The three design invariants

- **No query scans payloads.** The full payload is stored compressed and intact
  for the detail view and the issue bundle. Listings, search and the dashboard
  hit only indexed columns and aggregate tables (ADR 001).
- **A subsystem that is off costs nothing.** Scheduled jobs do not start their
  goroutine; ingest tells the SDK to stop, using the protocol's own rate limit
  (ADR 005). `internal/scheduler` enforces it: a job is *offered* together with
  the question of whether its subsystem has work, and only the ones that answer
  yes exist. The question is asked again when the configuration changes, so
  switching something on does not require a restart (ADR 014).
- **A percentile is never persisted.** Percentiles do not compose: a mergeable
  sketch is stored per window and computed at query time (ADR 007).
  `internal/engine/sketch` enforces it, a DDSketch-style logarithmic histogram
  with 1 % relative error whose **merge is exact**, which is what lets an hour
  built by folding sixty minutes be, bit for bit, the hour those requests would
  have produced directly (ADR 020).
- **Never a row per session.** Release health is four counters per (release,
  environment, hour) and nothing else. Counting distinct sessions requires
  state, and that state is a **bounded in-memory window** that only writes
  totals: 50,000 entries with a one-hour TTL, flushed every 60 s and on
  shutdown. A restart loses what was in flight, and when the window fills up
  precision is sacrificed, never stability. Both facts are stated to the user
  in every response, not hidden (ADR 008). A product that stores sessions one
  by one is a different product: session analytics, which is where the
  competitor spends its infrastructure.

## One API, four clients

Use cases are implemented once and projected onto REST, web UI, CLI and MCP:
four adapters of the same port. A feature is not done until it is in all four;
that is how the CLI and MCP avoid becoming second-class citizens, which would
be fatal in a product whose differentiator is that an agent operates it
(ADR 006).

The fourth arrived last and is the one that could most easily have stopped
being a client. `internal/adapters/mcp` does not call a use case: each tool
declares **one request to the REST API**, and the only thing that separates the
two transports is who carries it. `trapline mcp` goes over the network with
`TRAPLINE_TOKEN`, and `POST /mcp` goes to this same process's router with the
credential the request arrived with. That is why a tool cannot skip a scope
check or answer anything different from the endpoint: it is the endpoint
(ADR 022).

Since 2026-09-20 that API is **frozen**: it is `/api/v1/`, things can be added
but not removed or renamed, and `/api/v1-beta/` keeps answering for one minor
with a `Deprecation` header. The route table is data,
`internal/adapters/httpapi/routes.go`, which the router walks to register and
`TestRoutesMatchOpenAPI` walks to check against `docs/api/openapi.yaml` in both
directions. What is **not** frozen is `/api/0/`: that is `sentry-cli`'s
protocol, not ours (ADR 013).

## Where to start reading

- What an event looks like and what the engine guarantees:
  `internal/engine/engine.go`.
- The DSN, which is the entire migration story: `internal/domain/dsn.go`.
- The CLI contract: `internal/adapters/cli/cli.go`.
- What protects the public ingest endpoint:
  `internal/adapters/httpapi/throttle.go` for the per-address ceilings, and
  `internal/adapters/httpapi/ingest_budget.go` for the one cap that answers the
  question an attacker actually asks. The two files sit next to each other on
  purpose: the first rations the **rate** of one address, the second the
  **memory** of all of them at once, and a thousand addresses each politely
  under their own ceiling still arrive together. What an attack costs, and how
  it is measured, is in `docs/benchmarks/footprint.md` (ADR 023, ADR 039).
- What runs when nobody is asking for anything:
  `internal/scheduler/scheduler.go`, and `GET /api/v1/system/jobs` to watch it
  run.
- What this binary exposes, all of it in one pass:
  `internal/adapters/httpapi/routes.go`, with method, path, scope and which
  subsystem each route depends on. Next to it, `openapi_test.go`, which is
  what turns "the API is frozen" into something that breaks the build.
- Where the dashboard numbers come from, and why not from a `GROUP BY`:
  `internal/adapters/sqlite/stats_repository.go` and migration `0006`
  (ADR 010).
- What a third-party tool really speaks, and why it is not implemented from
  memory: `compat/sentry-cli/fixtures/` (recorded traffic, committed) and
  `internal/adapters/httpapi/sentrycompat_handler.go` (ADR 013).
- Why advertising only the modern protocol switches the modern protocol off:
  the `accept` field in `internal/usecase/artifacts.go`
  (`AcceptedUploadKinds`). The capabilities response is what decides which
  protocol the client speaks, and with a bare `["artifact_bundles"]`
  `sentry-cli` stops using debug ids (ADR 018). The other half is in the same
  file: `ok` only after writing, because the tool believes that `ok` without
  checking anything.
- What an artifact bundle is inside: `internal/artifactbundle/bundle.go`, a
  ZIP with `manifest.json`, where the script and its map **share** a debug id
  and the bundle has one of its own that belongs to neither.
- How something that does **not** arrive is watched (a backup that stopped
  running): `internal/domain/cron.go` (the crontab parser and its `Next`),
  `internal/domain/cron_monitor.go` (the state rules, a pure function) and
  `internal/usecase/crons.go` (the `cron-watch` sweep). The surface that makes
  instrumenting a cron cost one line is
  `internal/adapters/httpapi/ping_handler.go`: unauthenticated, because the key
  in the URL *is* the credential (ADR 016).
- Why a cron monitor and an uptime monitor are the same concept, and what
  tells them apart anyway: `internal/domain/monitor.go`. The family goes into
  the silence key and into the link because the ids come from two tables and
  collide (ADR 037).
- Why a notification is written in the same transaction as the issue it is
  about: `internal/adapters/sqlite/alert_repository.go` and the `SAVEPOINT` in
  `issue_repository.go` (ADR 015). The rest of the subsystem (who delivers,
  with what backoff, and what happens if the process dies halfway) is in
  `internal/usecase/notifier.go`.
- Why a URL typed by a user does not turn this server into a proxy into its
  own network: `internal/ssrfguard/ssrfguard.go`, the table of rejected ranges
  and, below it, the `DialContext` that connects to the address it just
  validated and not to the name it was given (ADR 016).
- The only screen someone outside the team reads, and why it is not React:
  `internal/adapters/httpapi/templates/status.html` and its handler next to
  it. The rules (which monitor is shown, what a day without checks means, what
  an incident is) are a pure function in `internal/domain/status_page.go`, and
  the numbers come from the daily aggregate and never from the checks
  (ADR 001, ADR 017).
- Why the p95 of an hour is not the average of the p95s of its minutes, and
  what is stored instead: `internal/engine/sketch/sketch.go`. The logarithmic
  mapping fits in two lines, and `Merge` is adding counts. Its consumers are
  `internal/adapters/sqlite/trace_repository.go`, which reads, merges and
  rewrites the sketch **inside the transaction** that counts the transaction,
  and `internal/usecase/transactions.go`, which computes the percentiles at
  query time and never stores them (ADR 007, ADR 020, ADR 021).
- Why a front-end error is resolved on the way **in** and not when read, and
  what happens to grouping when someone switches source maps on:
  `internal/usecase/symbolicate.go`. Two paths, debug id first, with the
  minified frame kept in `frame.raw`. The parser is next to it, in
  `internal/sourcemap`, and the cache behind it is bounded **by bytes** because
  the size of a map is chosen by whoever uploads it (ADR 018).
- Which change introduced an error, and why the answer does not need to talk
  to GitHub: `internal/domain/suspect.go`. The match is by **path suffix**
  between the files a commit touched and the ones the stacktrace names, with
  the frames closest to the failing call weighing more. It is a pure function
  with no clock; `internal/usecase/suspects.go` joins the three reads and turns
  every empty outcome into a sentence that says what to change, which is the
  half of the feature people notice (ADR 019).
- Why trace sampling is decided with a hash of the `trace_id` and not a coin
  flip per transaction: `internal/domain/transaction.go`. A waterfall is stored
  whole or not at all; the gaps the alternative leaves look exactly like
  instrumentation nobody added. And the aggregates are written for 100 % of
  what arrives, whatever the sampling does (ADR 021).
- How a session is counted without storing any:
  `internal/domain/release_health.go`, a pure data structure with no clock and
  no lock, with the LRU and the TTL inside; `internal/usecase/health.go` adds
  the mutex, the clock and the write, and `internal/app/app.go` drains it after
  the HTTP server has stopped accepting and before the process exits. What is
  lost on a restart, and why that is accepted, is in the annex of ADR 008.
- What an agent reads before touching code, and why it is not JSON:
  `internal/usecase/bundle.go`, a whole issue as a markdown document, with the
  symbolicated stacktrace, the breadcrumbs, the frequency read from the
  aggregates and the suspect commits. The render is a pure function of what was
  gathered and has golden fixtures, because "deterministic" without fixtures is
  an intention: an agent's prompt cache is keyed by the text (ADR 022).
- Why an MCP tool cannot do more than a token: `internal/adapters/mcp/tools.go`,
  the table, which is a list of calls to `/api/v1/`, and
  `internal/adapters/mcp/caller.go`, the half that carries them: over a socket
  for stdio, through this process's router for `POST /mcp` (ADR 022).
- What an agent does with all of the above, written as a procedure and not a
  promise: `skills/fix-error/SKILL.md`. Read the bundle before opening any
  file, write the failing test, patch, run the user's suite, close the issue
  for the next release. And the hard limits, which are the half that matters:
  never `push`, never deploy, never resolve something whose test does not pass
  (ADR 022).
- What stops the documentation from aging silently: `scripts/docs.sh`. The
  blocks marked ```` ```sh test ```` run against a real server and the internal
  links are resolved, so renaming a flag breaks the build the same way renaming
  it in the OpenAPI does (ADR 040).
- What proves all of the above together, and what happens when it does not:
  `demo/demo.sh`. A service with a real bug, the bundle, the patch,
  `resolve -next-release`, the redeploy, and the pod nobody redeployed yet
  still broken without reopening anything (ADR 012). It is the only check that
  the subsystems add up: each has its own gate and none of them looks at the
  one next door. It runs every night with `--no-agent`, the same walk with the
  one step a machine cannot claim replaced by the patches in `demo/fix/`.
- What was decided and why: `docs/adr/README.md`.
