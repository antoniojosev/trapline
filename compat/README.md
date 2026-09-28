# Compatibility matrix

Real SDKs, sending real events, against a real server.

This is the claim the whole product rests on: point an official SDK's DSN here
and it works, with nothing else changed (ADR 002). A claim like that cannot be
tested with a hand-written envelope, because a hand-written envelope is this
project's idea of what an SDK sends — which is exactly the thing in question.

## Why it is a separate module

The main module's dependency list is part of the product: a binary that
promises to be small and self-contained should not carry an SDK it exists to
receive events *from*. Keeping the matrix in its own module means the SDKs are
test fixtures, not dependencies.

## The matrix

Versions are pinned, and the lockfile of every suite is committed. A matrix
that floats tells you an SDK broke on some unknown day rather than on the
commit that moved it.

Last run against these versions: **2026-08-29**.

| SDK | Package | Version | Runtime | Tier |
|---|---|---|---|---|
| Go | `github.com/getsentry/sentry-go` | 0.48.0 | the host's Go toolchain | smoke |
| Python | `sentry-sdk` | 2.68.1 | the host's Python, own virtualenv | smoke |
| Node | `@sentry/node` | 10.71.0 | the host's Node | smoke |
| Browser | `@sentry/browser` | 10.71.0 | `mcr.microsoft.com/playwright:v1.56.0-noble`, headless Chromium | full |
| PHP | `sentry/sentry` | 4.16.0 | `php:8.3-cli`, vendor tree built with `composer:2` | full |
| Dart | `sentry` | 9.7.0 | `dart:3.13.2` | full |

`compat/tracing` is a sixth suite of a different kind, and it is not in the
table because it is not testing grouping: it sends transactions through the Go
SDK with latencies it chose, and prints the exact percentiles of what it sent
before serialising them. `scripts/tracing.sh` compares those against the ones
the server computed by merging its sketches. Computing the reference on the
sending side is the whole point — a gate that asked the server for both numbers
would only be checking that the server agrees with itself (ADR 020).

`compat/python/health.py` is a seventh entry point, and it is not in the table
for the same reason: it is not testing grouping. It tracks sessions through
`sentry_sdk.sessions.track_session` — the SDK's own machinery, the one
`auto_session_tracking` switches on — and produces crashes by capturing an event
whose mechanism says `handled: false`, which is what the SDK's excepthook
attaches and the only thing that makes it mark a session crashed.
`scripts/health.sh` checks that a hundred sessions with five crashes come out as
exactly 0.95, and that nothing about the in-memory window (ADR 008) leaks into
the number.

Flutter is deliberately absent. `sentry_flutter` wraps the Dart package and
shares its transport and its event shape, so the bytes this server sees are the
Dart ones; adding an emulator to the matrix would cost a great deal and test
nothing that is not already covered here.

## Running it

```sh
make compat          # the smoke tier: Go, Python, Node. Seconds, no containers.
make compat-full     # everything, containers included. Minutes, needs Docker.
make tracing         # the Go SDK sending transactions, for the latency gate.
make health          # the Python SDK tracking sessions, for the crash-free gate.
```

Two tiers because a slow gate is a gate somebody switches off, and a
switched-off gate is worse than no gate at all (ADR 002). The smoke tier runs
on every pull request; the full matrix runs nightly and as a release gate
(`.github/workflows/nightly.yml`).

Each language directory sends a small set of deliberately different errors and
asserts what arrived: that they grouped the way they should, that the release
and tags survived, and that nothing sensitive was stored. Deliberately
different matters — four copies of one error test counting, not grouping.

The containerised suites talk to a server running on the host. There is no
portable address for "the host": a native Linux daemon reaches it through the
bridge gateway, while Docker Desktop runs containers in a separate virtual
machine where that address means something else entirely. `scripts/compat.sh`
probes the candidates and prints the one that worked rather than encoding any
one setup's answer.

## What it has found so far

Five bugs, none of which the hand-written tests could have found, because the
hand-written tests contained this project's idea of what SDKs send.

- **Go**: every error from `errors.New` shares one type and, raised from one
  function, one set of frames. Four different errors became one issue.
- **Python**: `str(KeyError("x"))` is `'x'` with quotes, and the message
  normaliser erased anything quoted — so different missing keys merged. Also, a
  parameterised log line split into one issue per parameter value.
- **Node**: an over-eager normaliser treated any letters-then-digits token as a
  machine name, so `utf8` and `base64`, `sha256` and `md5`, `http2` and `ipv4`
  were all erased. Unrelated errors merged and the second title disappeared
  from the product entirely.
- **Browser**: the ingest endpoint served no cross-origin policy. The failure
  was invisible from the server's side and this is why: a cross-origin POST
  with a plain body needs no preflight, so the browser sent every event, the
  server stored them and logged 200s, and everything looked healthy — while the
  browser blocked the page from reading a single response. What is in the
  response is the protocol's backpressure. `X-Sentry-Rate-Limits` is how this
  server tells an SDK to stop sending a category that is switched off, and it
  is the entire reason a switched-off subsystem costs nothing *on the wire*
  rather than merely nothing on disk (ADR 005). Every browser SDK was deaf to
  it, and would have kept paying full price for a category nothing would store.
  A preflighting configuration would have delivered nothing at all, with no
  trace on either side. Fixed by allowing any origin on ingest — and only on
  ingest, which has no ambient credential to abuse — and exposing the
  rate-limit headers. The browser suite now proves it by asking the question
  the SDK cannot: it posts a session envelope from the page and requires a
  readable 429 carrying a readable limit.
- **Browser**: an error from a browser recorded nothing about which browser.
  "Only on Safari" is the answer to a large share of front-end bugs, and the
  SDK cannot help: a page has no trustworthy name for itself, so nothing about
  the browser is in the payload. It is in the one field the browser writes
  itself, on every request, and the server was throwing it away. The
  `User-Agent` header is now read into `contexts.browser` on ingest.

Every grouping bug in that list is a case in the golden corpus, in both
directions.

## Per-SDK notes worth knowing

### Node

- **`@sentry/node` 10 sends no authentication header at all** — only
  `?sentry_key=` in the query string. A server that implemented `X-Sentry-Auth`
  alone would reject every Node client while passing its own tests.
- **`@sentry/node` 10 no longer sends `abs_path`**, only `filename`. The
  `file://` handling in `normalizePath` is still needed for the browser SDK,
  and for Node itself under ES modules, where the runtime reports the same file
  as a `file://` URL that CommonJS reports as a plain path. One codebase run
  two ways has to be one set of issues; the corpus now pins that.
- **A custom `Error` subclass in JS reports as `Error`** unless it sets
  `this.name`; nothing on the wire reveals the real class. Not fixable here.

### Browser

- **The SDK cannot tell whether delivery worked.** `Sentry.flush()` resolves
  true whether or not the browser let it read the answer, which is why the
  suite probes the response directly rather than trusting the SDK's report.
- **An unhandled rejection in the same tick as an uncaught throw is dropped by
  the SDK, not by this server.** The `setTimeout` wrapper catches the throw,
  reports it itself, and asks the global `onerror` handler to ignore the next
  error it sees — a flag it clears on the following task. Anything landing
  inside that window never reaches a transport. It looks exactly like a lost
  event; the suite separates the two by a tick so that it keeps measuring the
  server.
- **Minified frames are a wall.** A production bundle reports
  `Object.t [as decode]` in a file called `bundle.min.js`, and the mangled name
  changes on every rebuild, so the same bug can get a new identity on every
  deploy. The hashed-filename rule handles the file; nothing handles the
  function. That is what source maps are for, and the suite builds a minified
  bundle with a map beside it as the fixture for that work.

### PHP

- **`values[]` arrives in the order the protocol specifies**: the causes first,
  the exception that was actually raised last. Anyone assuming `values[0]` is
  the raised one would give every wrapped error in a PHP application a title
  and a culprit describing its cause. (The same is true of `error.cause` chains
  in Node.)
- **The engine writes messages with structure in them.** A `TypeError` under
  `strict_types` reads `Foo\bar(): Argument #1 ($amount) must be of type int,
  string given, called in /app/main.php on line 161` — a qualified function
  name, an argument position, two type names, an absolute path and a line
  number, all inside the exception's *value*. The normaliser has to leave
  enough of it to tell two argument-type mistakes apart.
- **Every application frame is `in_app`, and `module` is always empty.** The
  culprit therefore renders as a path rather than a module name, which is
  correct but longer than the other SDKs' culprits.
- **A quoted value inside a longer message is normalised away**, which merges
  `Undefined array key "billing_address"` with `Undefined array key
  "shipping_country"`. This is the same shape as the `field 'nombre' is
  required` case that the corpus deliberately keeps grouped, and there is no
  structural difference between them — so it is a known limitation rather than
  a bug, and changing it would be a grouping change with a version bump
  (ADR 003). The Python `KeyError` fix only covers messages that are *nothing
  but* a quoted token.

### Dart

- **`Exception('...')` is a factory for one private class**, so every
  idiomatic Dart exception arrives with the type `_Exception` — the same
  problem `errors.New` created in Go, in an ecosystem that reaches for it more
  often.
- **The value is `toString()`, which conventionally repeats the type**, so a
  built-in exception produces the title `_Exception: Exception: unsupported
  encoding utf8`. Nothing on the wire distinguishes the repetition from a
  message that genuinely starts with a type name.
- **An async throw carries frames that are not frames.** Dart records the
  boundary between an async caller and its callee as an entry with no function,
  no file and `<asynchronous suspension>` where a path would be. How many of
  them a stacktrace has depends on how many `await`s are on the path, so
  counting them would turn an ordinary refactor into a wave of new issues. Both
  directions of that are now in the corpus.
- **Frames name the file, not the path**: `main.dart`, or `package:sentry/...`
  for library code. There is no filesystem path to normalise.

### Python

- **Python and Node both send session envelopes unprompted**, and so does the
  browser. They are counted as not-yet-stored rather than silently dropped —
  and, on a project that accepts only errors, refused with the rate-limit
  header that tells the SDK to stop.

## The other client: `sentry-cli`

An SDK is how errors arrive; `sentry-cli` is how a deploy pipeline says what
shipped. It lives beside the matrix, under `compat/sentry-cli/`, and it is
tested the same way and for the same reason — with the real, pinned tool, not
with this project's idea of what it sends.

The method is stricter here, because the surface being emulated is an HTTP API
rather than a protocol with a specification. **Record first, implement second**
(ADR 013):

```sh
make sentry-cli      # record if needed, replay the fixtures, then run the real tool
```

- `recorder/` is a stand-in server that answers whatever keeps the tool walking
  and writes every request down. Its own Go module, so a development-only HTTP
  server never enters the product's dependency graph.
- `record.sh` runs the pinned `sentry-cli` in its container against the
  recorder and commits what it observed to `fixtures/<version>/`.
- `verify.sh` runs the same four commands against a real installation of this
  server and then asks the product's own API whether any of it arrived.
- `version.env` is the pin. Moving it means re-recording, and **reading the
  diff**: a fixture that changed is a protocol change somebody has to look at.

### What the recording found

Four commands — `releases new`, `releases set-commits --local`,
`releases finalize`, `releases deploys new` — make **seven** HTTP calls, and
almost nothing about them is guessable:

- **The namespace changes inside one command.** Creating a release is addressed
  to the project (`POST /api/0/projects/{org}/{project}/releases/`); setting its
  commits is addressed to the organisation
  (`PUT /api/0/organizations/{org}/releases/{version}/`). A server that
  implemented one of them would fail halfway through a pipeline, with the
  release already created.
- **`set-commits --local` makes four calls**, one of which creates the release
  again — so creating a release twice cannot be a conflict.
- **Two responses must be a JSON array.** Answering `{}` stops the tool with
  `invalid type: map, expected a sequence`.
- **`finalize` refuses a release object without a `version` field**, so an
  empty `200 {}` is not a valid answer anywhere on this surface.
- **`deploys new` needs `--release` or `SENTRY_RELEASE`**; it infers the release
  from nothing.
- **Every call authenticates with `Authorization: Bearer …`** and nothing else,
  which is what makes this product's own `ek_…` token work as
  `SENTRY_AUTH_TOKEN`.

That list is why the fixtures are committed rather than the endpoint list being
written down: the next version of the tool can change any of it, and the diff is
the only place it will show.

### Source maps, recorded the same way

`fixtures/<version>/` has four more directories, one per upload flow. They
hold the exact sequence, and what the server has to answer at each step for
the tool to call an upload good; the annex of ADR 018 walks through them. The
short version, because none of it is guessable either:

- **The default path today is debug ids, not `release + url`.** `sourcemaps
  upload` with no other argument uploads a ZIP — an *artifact bundle*, with a
  `manifest.json` inside — and never mentions a release.
- **The capabilities answer decides which protocol the tool speaks.** Advertise
  `artifact_bundles` without `release_files` beside it and the same command
  silently falls back to the old per-file upload.
- **Answering `{"state":"ok"}` to an assemble it has not really done makes the
  tool declare success** — and, on one of the flows, never send the files at
  all.
- **The chunk's checksum is the multipart *filename*, not the field name**, and
  it is the sha1 of the *uncompressed* chunk. A server hashing what arrived on
  the wire would reject every gzip-capable client with all of its own tests
  green.

`compat/browser/record-envelope.sh` captures the other half: one error raised
in a real Chromium from a bundle `sourcemaps inject` has been run over, saved
with its `debug_meta` exactly as it left the browser, next to the very bundle
and map its debug id names (`internal/sourcemap/testdata/`). Neither half is
useful alone — the debug id is the only thing joining them.

`./compat/sentry-cli/record.sh --check` re-records everything from scratch and
diffs it against what is committed, tolerating only the three dates the tool
stamps with its own clock. It runs inside `make sentry-cli`.

### Per-tool notes worth knowing

- **`set-commits --local` reads the repository with libgit2, not with git**, so
  the image needs no git binary — but libgit2 refuses a repository owned by a
  different user, which is exactly what a root container over a host bind mount
  looks like. The suites run the tool as the host user.
- **The release version travels unescaped in the path** (`app@1.0.0`), which is
  the shape `net/http` route patterns handle, and the reason a version
  containing `/` is refused when it is created (ADR 012).
- **Every path has a trailing slash.** In `net/http` a pattern ending in `/` is
  a subtree match, so each route is anchored with `{$}` — without it,
  `/releases/` would swallow `/releases/app@1.0.0/deploys/`.
- **The tool stamps `finalize` with its own clock on every run**, so a rerun
  arrives asking to move the release date. It does not move: the first date
  stands, which is what makes finalising twice the same as finalising once.
