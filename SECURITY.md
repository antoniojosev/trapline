# Security policy

## Reporting a vulnerability

**Do not open a public issue.** Use
[GitHub Security Advisories](../../security/advisories/new), or write to
`antoniovila.dev@gmail.com` with `[security]` in the subject.

Response within 72 h. No bug bounty program (single-maintainer project); public
credit in the advisory and the CHANGELOG, if you want it.

## Attack surface

It deserves naming explicitly because it shapes the design:

**The ingest endpoint is public by design.** Authenticated only by DSN, which
ships inside web clients, so it is a public credential. Everything that arrives
there is hostile input from the internet:

- Hard limit on body size; protection against gzip/zstd bombs.
- **One memory budget shared by every in-flight ingest request**: 32 MiB,
  reserved up front for the decompressor's working set and charged as bytes are
  produced. Once exhausted, the answer is `429` with `Retry-After` and nothing
  is queued. The per-request caps (20 MiB per envelope, 4 MiB per item, 100
  items) answer "how much does one cost?"; this one answers "how much do a
  thousand cost?", which is the attacker's question. Without it, 128 concurrent
  zstd bombs, 151 KB of upload in total, took the process RSS to 1.46 GB, and
  it grew with the attack (ADR 039,
  [docs/benchmarks/footprint.md](docs/benchmarks/footprint.md)).
- **A zstd frame's window is capped at 8 MiB.** A frame declares its own window
  and the decompressor reserves that memory before producing a byte; without a
  ceiling, the cheapest possible request reserves the most expensive possible
  buffer.
- **Rate limit per IP in addition to per project**, applied **before reading
  and decoding the body**: what is being avoided is paying for the `Read` and
  the gunzip, so a limit applied later would be useless. Default 48,000 req/min
  per address, derived from ADR 001's design volume and configurable with
  `-ingest-ip-rate-limit`. Deliberately generous: all of a backend's legitimate
  traffic comes from one IP.
- **Trust in `X-Forwarded-For` is explicit configuration**
  (`-trusted-proxies`, IPs or CIDRs). Without it the header is **ignored
  entirely** and the direct peer is counted. With it, the chain is walked right
  to left, skipping trusted hops. An XFF believed without checking who sent it
  is a list of IPs chosen by the attacker (ADR 023).
- A per-IP 429 carries `Retry-After` and **not** `X-Sentry-Rate-Limits`: that
  header makes an SDK stop sending that category for the window, and using it
  as a defense would turn a limit into data loss (ADR 005).
- The envelope parser is a pure package and is fuzzed in CI.
- A valid DSN allows writing events to its project and nothing else.
- **Open CORS (`Access-Control-Allow-Origin: *`) only on ingest**, never on the
  panel API. This is correct rather than lax: ingest has no ambient credential
  (it authenticates with a key the caller presents explicitly, one that already
  ships inside public bundles), so an origin allowlist would protect nothing
  and would break the normal case of one installation receiving events from
  several applications. **Credentials are not allowed**: `*` and
  `Allow-Credentials: true` are incompatible by specification, and asking for
  them would mean this endpoint could be reached with someone's session cookie
  attached. The panel API, which does authenticate by cookie, serves no CORS
  header at all, and a test verifies that.

**The cron monitor ping endpoint is public and unauthenticated, on purpose.**
`GET|POST /ping/{key}` (and `/start`, `/fail`) ask for no token, cookie or
header: the 32-hex key in the URL **is** the credential. The case it exists for
is a crontab line ending in `&& curl -fsS https://…/ping/abc`, and a crontab
line cannot carry a bearer token or read a configuration file nobody handed it
(ADR 016). What bounds it:

- **The key is minted with `crypto/rand`** (16 bytes, 32 hex) and is unique
  across the whole installation. It is not derived from the slug or the
  project, so it cannot be guessed from anything visible.
- **What it allows is reporting that a job ran**, or started, or failed. It
  reads nothing, lists nothing and touches no other project. The damage from a
  leaked key is that someone silences *that* monitor.
- **Rate limit per IP**, the same limiter and the same ceiling as ingest: they
  are the same kind of traffic, and an operator who already tuned one ceiling
  should not run into a second one with a different default. Its 429 carries
  `Retry-After` and **not** `X-Sentry-Rate-Limits`, for the same reason as
  ingest's (ADR 023).
- **The response is one line of `text/plain` with `Cache-Control: no-store`.**
  Without the `no-store`, a proxy or a prefetch could report as run a job that
  did not run.
- **A disabled monitor answers 410, not 404.** That is deliberate information
  (the caller already holds the key), and without it a script that has been
  pinging for a year cannot tell "they switched it off" from a typo.
- **It sits outside the CSRF guard**, for the same reason as `/api/0/`: that
  header defends an ambient credential and there is none here, and requiring a
  header `curl` does not send would reject the only client the endpoint exists
  for.

**The status page is public, unauthenticated and JavaScript-free, on
purpose.** `GET /status/{slug}` asks for no token or cookie: it exists so that
someone whose service broke can open a link and read whether it is broken for
everyone else too (ADR 017). What bounds it:

- **It only answers if the project enabled it** (`config.status_page.enabled`),
  and **only shows monitors marked `public`**. A monitor nobody marked is the
  name of an internal service and does not leave the installation.
- **A project that does not publish and a project that does not exist answer
  the same: 404.** The difference between the two is precisely its operator's
  decision not to publish, and stating it would leak it.
- **In-memory cache of 30 s per project**, with `Cache-Control: public,
  max-age=30`. A miss (unknown slug, project without a page) is **not
  cached**: the key is chosen by whoever types the URL, and storing misses
  would be a way to grow a map in this server's memory from outside. What
  bounds the cost of those is the per-IP limit.
- **Rate limit per IP**, the same limiter and the same ceiling as ingest and
  the ping, for the same reason (ADR 023). The gate checks that two hundred
  consecutive reads go through: a ceiling that rejects a real reader is not a
  defense, it is an outage.
- **No JavaScript, and the CSS inline.** There is no script surface to attack,
  and everything an operator types (title, description, a monitor's name) is
  rendered with `html/template`, which escapes by context. A test puts
  `<img src=x onerror=…>` in a monitor's name and checks that it comes out
  escaped.
- **It carries no `id` of anything**, no target URLs, no status codes, no
  errors: only names, percentages and days. What is published is what the
  operator chose to publish.

**The `ping_key` is returned by the API, and it is the only credential in this
product that is.** It has to be: the whole feature is a URL someone pastes into
a crontab, and a key visible only once would mean losing it costs a new monitor
and an edit to a server's crontab. What protects it is that **the
`monitors:read` / `monitors:write` scopes are their own and not
`projects:*`**: a token minted for a dashboard cannot read it. A token created
before this build does not have them.

**Other decisions.** The ones describing something that does not exist yet are
marked: a present-tense promise about unwritten code is exactly the failure
this document had before (ADR 023).

- Panel behind login (single admin, argon2id, sessions), **with per-IP rate
  limiting on `/setup` and `/login`** (10/min by default, `-auth-rate-limit`)
  and a global cap of **2 Argon2id hashes in flight**. Each attempt costs
  19 MiB of working set: without these two limits, 200 concurrent logins took
  the RSS to 1.03 GB. With them, 52.9 MB. `scripts/smoke.sh` runs that flood
  right before measuring the memory budget.
- API tokens with scopes and expiry. DSN keys rotatable without downtime (two
  active during the rotation).
- **The `/api/0/` compatibility surface authenticates by bearer only, and that
  is why it sits outside the CSRF guard.** That header (`X-Trapline-Request`)
  defends an **ambient** credential: a browser can be tricked into resending a
  cookie it already holds. `/api/0/` does not accept the session cookie
  (`requireToken` demands a bearer and looks at nothing else), so there is
  nothing to forge, and requiring a header `sentry-cli` does not know would
  reject the only client the surface exists for. The condition that makes it
  safe is exactly "does not accept a cookie", and a test pins it in both
  directions. It serves no CORS header either, so a page cannot read its
  responses even if it manages to make the request. Scope: only the routes
  `sentry-cli` calls for releases (ADR 013) and for uploading source maps
  (ADR 018); anything else answers 404.
- **`POST /mcp` authenticates by bearer only, and that is why it also sits
  outside the CSRF guard.** Same reasoning as `/api/0/`, point by point: that
  header defends an ambient credential, and this route does not accept the
  session cookie (`requireToken` demands a bearer and looks at nothing else),
  so there is nothing to forge; requiring a header invented by this product
  would reject every MCP client in existence. It serves no CORS header either,
  so a page cannot read its responses even if it manages to make the request.
  What is worth understanding is **what it authorizes**: connecting requires
  `projects:read`, and every tool re-enters through the route table with **the
  same token**, so `resolve_issue` is rejected for a token without
  `projects:write` by the same line that rejects `POST …/status`. An MCP
  server that called the use cases directly would be a second implementation
  of that check; this one cannot be, because it calls none (ADR 022). The
  transport is **stateless**: there is no session identifier to steal or to
  expire, and `GET` and `DELETE` on that route answer 405.
- **`trapline mcp` is a client, not a privileged mode.** It speaks the protocol
  over stdin/stdout for an agent that launched it as a subprocess, and reaches
  the product over HTTP with `TRAPLINE_TOKEN`: it does not open the database
  and has access to nothing that token does not have. If the token is missing,
  the process refuses to start instead of connecting and failing on every tool.
- **The issue bundle is a document generated from data an attacker wrote.** An
  issue's title, an exception's message, a tag value and a breadcrumb are
  filled in by whoever holds a DSN (for a browser SDK, anyone), and the
  document will be read by an agent that then acts. That is why every value
  coming from an event goes through a function that collapses line breaks
  before being written: without it, a `\n` in an exception message would end
  its line and let the rest start a heading of its own. A forged
  "## Suspect commits" inside a title is an instruction to go change someone
  else's code. A test pins this. It **bounds the shape, not the content**: an
  event's text is still text chosen by whoever sent it, and an agent acting on
  a bundle is reading untrusted input, just as it would be reading the issue
  in the panel.
- **Uploading source maps is writing bytes to this installation's disk, and
  that is why it has three ceilings and not one.** The surface receives a
  compressed archive chosen by whoever holds a token with `projects:write`, and
  a ZIP is a format with a compression ratio: a few kilobytes can name
  gigabytes. What bounds it:
  - **The reader's limits apply to what the archive EXPANDS to**, not to the
    size it declares, which is a number written by the sender. A ceiling per
    entry, a ceiling for the whole archive and a ceiling on the number of files
    (`internal/artifactbundle`, a pure package fuzzed for the same reason as
    the envelope parser).
  - **A budget per project** (`artifacts_max_mb`, 200 MB by default), and an
    **installation-wide ceiling on the chunk staging area**, which is where the
    client that uploads and never assembles is bounded. There are two because
    the endpoint that receives chunks does not name a project: see ADR 038.
  - **Chunks expire on their own** after 24 h, so an abandoned upload is not a
    leak.
  What a token with that capability can do is fill a project's budget; it
  cannot read another installation's artifacts or step outside the limits
  above. Uploaded files are stored compressed and **served to nobody**: they
  are read only to resolve a frame at ingest.
- **PII scrubbing before persisting**, not cosmetic: known fields (password,
  token, authorization, cookies, cards) plus configurable patterns.
- **Nothing identifiable is stored from a session, because the session is not
  stored.** The protocol sends `did` (the user identifier the SDK chose),
  `attrs.ip_address` and `attrs.user_agent`; of the whole item, `sid`,
  `started`, `status`, `errors`, `release` and `environment` are read, and of
  those only four counters per (release, environment, hour) survive. The `sid`
  lives in memory while the session is alive and never reaches disk. This is
  not a privacy measure bolted on top: it is the direct consequence of the rule
  of not storing a row per session (ADR 008), which is why there is nothing to
  scrub on this path.
- **Uptime checks do not visit private addresses without a double opt-in.** A
  monitor is a URL a user types for **the server** to visit: that is the
  definition of an SSRF, and the server sits inside its administrator's
  network, with access to the cloud metadata endpoint, internal panels and
  databases that trust anyone on their subnet. Before connecting, the host is
  resolved and rejected if **any** of the resolved IPs is loopback, private
  (RFC 1918/4193), link-local, multicast, `0.0.0.0/8`, broadcast, CGNAT or
  reserved. Any, not the first, because a host that resolves to a public
  address and to `127.0.0.1` can connect to both and the resolver promises no
  order. The exception requires **both** authorizations: `allow_private` on
  the monitor **and** `-uptime-allow-private` on the installation. Two because
  two different people hold them: whoever operates the server decides whether
  this installation may enter its own network, and whoever writes a monitor
  decides whether that particular check needs it. With only one, enabling it
  for an internal service would silently open every monitor anyone adds
  later. The server warns at startup when the flag is set.
- **The `Dial` goes to the already-validated IP, without resolving again.**
  This is the line that makes the above worth anything: checking a host and
  then handing the **name** to `net.Dial` asks DNS twice and connects to the
  second answer, which is textbook rebinding. Redirects are revalidated hop by
  hop, because the `Location` header is a URL the target chose. Only `GET` and
  `HEAD`, only `http`/`https`, no credentials in the URL, body read with a cap.
  All of that lives in `internal/ssrfguard`, a package with no dependencies
  beyond the stdlib (a boundary verified by `internal/arch` and by depguard)
  with 100 % coverage (ADR 016).
- **The `monitors:read` / `monitors:write` scopes are their own and not
  `projects:*`.** `monitors:write` is the permission to make this installation
  emit outbound requests to an address the caller chooses, unsupervised and
  forever. That is not the same authority as creating a project, even with the
  guard behind it. A token created before this build does not have them.
- **Outbound webhooks are signed with HMAC-SHA256**, with a per-channel secret
  of at least 16 characters and no option to disable it: an unsigned webhook is
  an endpoint anyone who learns the URL can post to. The header is
  `X-Trapline-Signature: t=<unix>,v1=<hex>`, and the timestamp goes **inside**
  the signed material; a signature over the body alone is replayable forever.
  The body is signed as sent, byte for byte. Verification is documented with
  runnable code in `docs/alerts/webhooks.md`, and the receiver in the
  `scripts/alerts.sh` gate implements it **without importing anything from
  this repo**, so that what gets verified is that a third party can write a
  receiver from the document alone (ADR 015).
- **Alert channel credentials are encrypted at rest** with AES-256-GCM. A
  channel holds credentials for third-party systems (a bot token, an incoming
  webhook URL, which *is* the authentication, an SMTP password), and in the
  clear the blast radius of a leaked `.db` would stop being "the error
  reports". The key lives in `<db>.key` with 0600 permissions, **outside** what
  it encrypts, generated on first use or set with `-secret-key-file` /
  `TRAPLINE_SECRET_KEY_FILE`. The API returns the configuration redacted: what
  goes in as a secret does not come back out.
- **`backup` does not copy the `.key`, and says so on every run.** It is the
  unavoidable consequence of not storing the key inside what it encrypts: a
  restore without that file leaves the channels listed and mute, with nothing
  in the logs pointing at the cause. `trapline doctor --quick` checks the key's
  permissions and that it decrypts a real channel, and never creates it: a
  diagnostic that minted a new key would turn "you are missing a file" into
  "you lost all your channels".
- **The `alerts:read` / `alerts:write` scopes are their own and not
  `projects:*`.** Reading where an installation sends its alerts is not the
  same as reading its issue list, because the former touches third-party
  credentials. A token created before this build does not have them.
- **`GET /system/channels` returns a destination to connect to, not a delivery
  URL.** That endpoint, the one `doctor` reads to know whether the alarm would
  ring, is guarded by `projects:read`, weaker than the `alerts:read` of the
  channel listing, and for Telegram, Slack and Discord the delivery URL **is**
  the credential: the bot token goes in the path and an incoming webhook's path
  is its entire authentication. So for those three only scheme, host and port
  come out, which is exactly what the connectivity probe needs. The signed
  webhook does return its full URL, because it authenticates by HMAC and its
  path is not a secret.

## What this does NOT protect

Stated explicitly, because a promise ahead of the code is worse than no promise
at all, and this section exists because the two previous claims were exactly
that for a while (ADR 023).

- **The rest of the authenticated API has no rate limit.** Its credentials are
  revocable tokens with scope and expiry, so the remedy for abuse is revoking,
  not limiting.
- **There is no limit by bandwidth or by bytes**, only by number of requests
  and by the size of each body. The panel API's body cap is 64 KB, with **one
  declared exception**: a release's commit set
  (`POST /projects/{id}/releases/{version}/commits`) accepts up to 8 MB,
  because the first deploy of an existing repository sends thousands of commits
  with their paths. It is an authenticated endpoint with a write scope, and the
  domain additionally bounds the number of commits per release.
- **A distributed attacker with enough addresses can saturate the network or
  the disk.** What they cannot do is exhaust memory, and it takes **two**
  global caps for that to be true: the one on in-flight password hashes, and
  the byte budget shared by ingest. Both bound the peak regardless of how many
  addresses arrive, which is exactly what a per-address limit cannot promise:
  a thousand addresses each under their own ceiling still arrive at once. This
  sentence was here before the second cap existed, and it was false: measured,
  ingest reached 1.46 GB (ADR 039).
- **Behind a proxy without `-trusted-proxies` configured, every visitor counts
  as the proxy** and the per-IP limit stops discriminating. The server warns
  about it at startup, but cannot detect it on its own.
- **An alert channel can point anywhere, including the internal network.**
  There is no SSRF guard on outbound delivery, and there will not be one:
  unlike uptime checks, where the URL is chosen by whoever configures a monitor
  for the server to visit, here the URL is the deliberate destination of
  whoever administers the installation, and a webhook to an internal service
  is the normal use case. Configuring a channel requires `alerts:write`.
- **The SSRF guard does not stop a monitor from measuring a public target that
  is not yours.** It checks where this server may connect, not who owns what
  is on the other side: anyone with `monitors:write` can request a `GET` every
  thirty seconds against any public address. The minimum interval, the cap on
  the body read and the safe methods bound the cost; the authorization to
  create monitors is what decides who can pay it.
- **A target that resolves to a public address and answers `302` toward a
  private one does not get through**, but the guard only sees addresses: it
  does not tell an internal service from a public one hosted on the same IP,
  so a deployment that needs to monitor the internal network has to open it
  entirely with `-uptime-allow-private` and choose monitor by monitor.
- **Notification delivery is at-least-once.** A process that dies between a
  successful POST and recording it produces a second delivery.
  `X-Trapline-Delivery` is emitted, stable across retries, for deduplication.
- **The limiter's maps are bounded and evict under pressure**: with very many
  distinct addresses precision is lost (never stability), so at that scale the
  per-project limit is what protects.

## Supported versions

Before the first release: `main` only. Once there are releases, the latest
minor.
