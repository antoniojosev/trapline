# Migrating from Sentry

The migration is **one line**: the DSN. The official SDKs speak the envelope
protocol, and this server speaks it from the other side
([ADR 002](adr/0002-compatibilidad-envelope-sentry.md)). There is no SDK of its
own to install, no instrumentation to rewrite, and the day you want to go back
you go back by changing the same line.

What you do have to decide first is **whether what you use is here**. This page
is the two lists: what works the same, and what does not exist and will not.

---

## 1. The change

```diff
-dsn="https://abc123@o123456.ingest.sentry.io/4505..."
+dsn="https://<public_key>@errors.example.com/1"
```

`trapline projects create -name mi-app` gives you the DSN, or the panel does.
The SDK derives the ingest endpoint from it; there is no other variable to
touch.

The SDKs tested against this server on every change, at their pinned versions,
are in [compat/README.md](../compat/README.md):

| SDK | Package | Tested version |
|---|---|---|
| Go | `github.com/getsentry/sentry-go` | 0.48.0 |
| Python | `sentry-sdk` | 2.68.1 |
| Node | `@sentry/node` | 10.71.0 |
| Browser | `@sentry/browser` | 10.71.0 |
| PHP | `sentry/sentry` | 4.16.0 |
| Dart | `sentry` | 9.7.0 |

It is not a list written from memory: each of those rows is a suite that sends
real errors to a real server and checks what arrived. A hand-written envelope
would only test this project's idea of what an SDK sends, which is exactly the
thing in question. `sentry_flutter` is not in the table because it wraps the
Dart package and shares its transport: the bytes this server sees are Dart's.

## 2. The deploy pipeline stays untouched too

`sentry-cli` works with `SENTRY_URL` pointed here; trapline's own token works
as `SENTRY_AUTH_TOKEN` and the organization slug is ignored (this is a
single-organization installation).

```sh
export SENTRY_URL=https://errors.example.com
export SENTRY_AUTH_TOKEN=ek_...
export SENTRY_ORG=anything
export SENTRY_PROJECT=mi-app

sentry-cli releases new "mi-app@$(git rev-parse --short HEAD)"
sentry-cli releases set-commits --local "mi-app@$(git rev-parse --short HEAD)"
sentry-cli sourcemaps upload ./dist
sentry-cli releases finalize "mi-app@$(git rev-parse --short HEAD)"
sentry-cli deploys new -e production
```

That surface **was not implemented by reading documentation**: the real tool's
traffic was recorded against a recording server, and what was recorded is what
got implemented
([ADR 013](adr/0013-compatibilidad-con-sentry-cli-a-partir-de-trafico-grabado.md),
[ADR 018](adr/0018-source-maps-debug-ids-primero.md)). The fixtures are
committed in `compat/sentry-cli/fixtures/` and the nightly gate runs the real
`sentry-cli` against this server.

If you would rather not install `sentry-cli`, trapline's own CLI does the same:

```sh
trapline releases create -project 1 -version "mi-app@1.4.2"
trapline releases commits -project 1 -version "mi-app@1.4.2" -repo . -from <sha> -to <sha>
trapline artifacts upload -project 1 -release "mi-app@1.4.2" ./dist
```

## 3. What works the same

| | |
|---|---|
| Errors, grouped | yes; the grouping algorithm is versioned ([ADR 003](adr/0003-algoritmo-de-grouping-versionado.md)) |
| Breadcrumbs, tags, contexts, user, request | yes, as they arrive |
| Releases, deploys, commits | yes, including `set-commits --local` and suspect commits ([ADR 019](adr/0019-suspect-commits-por-interseccion-de-rutas.md)) |
| "Resolve in the next release" | yes, with the reopen rule ([ADR 012](adr/0012-releases-orden-y-resolucion-en-proxima-release.md)) |
| Source maps (debug ids and legacy release+URL) | yes, resolved **at ingest** ([ADR 018](adr/0018-source-maps-debug-ids-primero.md)) |
| Tracing: transactions, spans, waterfall, p50/p95/p99 | yes, with deterministic per-trace sampling ([ADR 021](adr/0021-tracing-sampling-determinista-y-downsampling.md)) |
| Release health / crash-free | yes, without storing a row per session ([ADR 008](adr/0008-ventana-de-sessions-en-memoria.md)) |
| Cron monitors (SDK check-ins and ping by URL) | yes ([ADR 016](adr/0016-crons-compatibles-y-ping-curl-able.md)) |
| Uptime monitors and public status page | yes, included, with no extra containers ([ADR 017](adr/0017-status-page-publica-renderizada-en-servidor.md)) |
| Alerts to Telegram, Slack, Discord, webhook, email | yes, with a persistent outbox ([ADR 015](adr/0015-notificaciones-por-outbox-persistente.md)) |
| Issue text search | yes, over issues (FTS5), never over payloads ([ADR 011](adr/0011-busqueda-de-texto-con-fts5-sobre-issues.md)) |

## 4. What does NOT exist, and is not a to-do list

This is a contract, not a backlog. Each line is one reason the left-hand
column of the [footprint comparison](benchmarks/footprint.md#5-la-comparativa-con-sus-asteriscos)
is possible.

| Missing | What to do instead |
|---|---|
| **Session replay** | nothing: it is the feature that requires storing and serving session video. It does not fit in one binary |
| **Continuous profiling** | `pprof` in your own service; the tracing here answers "which endpoint got slow", not "which function" |
| **Native symbolication** (iOS, Android, DWARF) | native crashes arrive and are stored, but without symbols. JS source maps **are** resolved |
| **Custom metrics** | Prometheus, or whatever you already have. This counts errors, not arbitrary series |
| **SSO / SAML** | one admin user and scoped tokens. It is one installation per team, not a multi-tenant |
| **Log management** | Loki, or your platform's logs. Breadcrumbs cover "what happened right before this error" |
| **Query builder / discover** | listings, search and the dashboard hit indexed columns and aggregates, and **never** scan payloads ([ADR 001](adr/0001-sqlite-como-unico-estado.md)). A query builder requires the event search engine that does not exist here |
| **Multi-node / HA** | one binary, one file, one machine. That is the product, not a stage of it |
| **Session analytics** | crash-free per release, yes; counting distinct sessions one by one, no ([ADR 008](adr/0008-ventana-de-sessions-en-memoria.md)) |
| **Multiple organizations** | the organization slug is accepted and ignored |

## 5. What is not migrated: the history

**Old events stay where they are.** There is no importer, and there will not be
one: the incumbent's payloads are in its ClickHouse, their shape is an internal
detail of theirs, and an importer would be a contract with a system this
project neither controls nor tests.

The migration that works is the boring one:

1. Point **one** low-traffic service here, leaving the other DSN where it is.
2. Live with both for a week. New errors arrive in both places.
3. When the panel here answers the questions you used to ask there, move the
   rest and leave the other one read-only until its retention expires.

Nothing has to be switched off to start. An SDK with a different DSN per
environment is an environment variable.

## 6. Differences you notice on day one

- **There are no "organizations" or "teams".** There are projects and one
  admin.
- **An issue resolved "in the next release" is not reopened by events from the
  old release.** Pods that were not redeployed keep emitting, and here that is
  counted and said in words instead of reopening the issue
  ([ADR 012](adr/0012-releases-orden-y-resolucion-en-proxima-release.md)).
- **Percentiles are computed at query time**, never stored. The p95 of an hour
  is that hour's p95, not the average of the p95s of its minutes
  ([ADR 007](adr/0007-percentiles-por-sketch.md), [ADR 020](adr/0020-sketch-de-latencias-fusionable.md)).
- **Switching a subsystem off costs nothing.** A new installation starts with
  error tracking and one background job only. Switching tracing off does not
  even spend bandwidth: the protocol's own rate limit tells the SDK to stop
  sending that category ([ADR 005](adr/0005-toggles-con-backpressure.md)).
- **The last hour's numbers can move after a restart.** In-flight sessions
  live in a bounded in-memory window, and that is stated to the user instead
  of hidden ([ADR 008](adr/0008-ventana-de-sessions-en-memoria.md)).
- **The panel has no query builder.** If your workflow was writing queries in
  Discover, this product does not replace it.

## 7. Check it yourself before moving anything

Stand up a test installation, point a staging service's DSN at it, and see
whether the questions you ask on a Tuesday afternoon have answers:

```sh test
trapline issues list -project 1 --json
trapline stats -project 1 -top 5
trapline releases health -project 1 --json
```

If something you use daily is in neither the §3 table nor the §4 table, that
is a gap in this page and can be reported like any other
([CONTRIBUTING.md](../CONTRIBUTING.md)). The §4 list does not grow; the §3
list does.

---

## Going back

Change the DSN again. The events that arrived here stay here, and the database
is a file you can copy and read with `sqlite3` without this binary in front of
it. There is nothing to export because nothing is in a format only this product
understands.
