#!/usr/bin/env bash
#
# The security pass, as something that runs rather than something that was
# once read.
#
# Every item here is a claim this product makes to somebody thinking about
# putting it on the open internet: the ingest endpoint cannot be used to
# exhaust memory, a forged X-Forwarded-For buys nothing, the panel sets the
# headers, the CSRF check defends the credential it exists for and nothing
# else, an expired token is dead, and the decoders that read attacker-chosen
# bytes have been fuzzed for longer than a review will wait.
#
#   ./scripts/hardening.sh              everything
#   ./scripts/hardening.sh --only live  the checks that need a server
#   ./scripts/hardening.sh --only fuzz  the four fuzz targets
#
# The rule that shaped this file: **a gate that measures what one request costs
# does not answer the question an attacker asks.** This repository has
# published three wrong memory figures and all three were that mistake —
# Argon2id budgeted per hash, the zstd encoder measured on a two-core machine,
# and `/login` with a per-attempt cost and no ceiling on attempts, which turned
# a 30 MB footprint into 1.03 GB under two hundred concurrent logins (ADR 023,
# smoke.sh). So nothing below is measured once. The bomb section sends the same
# bomb at two concurrencies and asserts that the peak does not follow the
# flood, which is a claim a single request can neither make nor break.
set -euo pipefail

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/.."

ONLY="all"
while [ $# -gt 0 ]; do
  case "$1" in
    --only) ONLY="${2:-all}"; shift 2 ;;
    --only=*) ONLY="${1#--only=}"; shift ;;
    *) printf 'usage: %s [--only live|fuzz]\n' "$0" >&2; exit 2 ;;
  esac
done
case "$ONLY" in all|live|fuzz) ;; *) printf 'unknown section "%s"\n' "$ONLY" >&2; exit 2 ;; esac

# Four servers, because three of the checks below spend a per-address budget
# and a budget spent by one check is not available to the next. Sharing one
# server would make the second assertion pass for the wrong reason — it would
# be reading the first check's exhaustion, not its own.
PORT="${PORT:-9000}"
PING_PORT=$((PORT + 1))
PROXY_PORT=$((PORT + 2))
MAIN_PORT=$((PORT + 3))

# shellcheck source=lib/port.sh
. "$PWD/scripts/lib/port.sh"

ORIGIN="https://errors.example.test"
BINARY="$PWD/trapline"
LOADER="$PWD/compat/load/compat-load"
WORKDIR="$(mktemp -d)"
PASSWORD='una contraseña larga y buena'

# The ceiling on what a decompression-bomb flood may cost, in kilobytes of
# resident memory.
#
# 256 MB, and it is a ceiling on the ceiling rather than a measurement: the
# ingest budget is 32 MiB of live bytes and the rest is the collector's slack,
# which varies with the machine. What is asserted is not the figure but its
# shape — measured on this branch, quadrupling the flood moved the peak from
# 121 MB to 127 MB. Before the budget existed the same step moved it from
# 649 MB towards 2.6 GB, because there was no ceiling to quote at all
# (docs/benchmarks/footprint.md, ADR 039).
MAX_FLOOD_RSS_KB="${MAX_FLOOD_RSS_KB:-262144}"
# How much the peak may grow when the flood grows fourfold. A defence that
# works is flat here; one that does not is linear.
MAX_FLOOD_GROWTH_PCT="${MAX_FLOOD_GROWTH_PCT:-250}"
# What a refusal may cost, in milliseconds. A server that says no only after
# expanding a bomb has still paid for the expansion, and the time it took is
# the only externally visible evidence of that.
MAX_BOMB_MS="${MAX_BOMB_MS:-50}"
# The per-address ingest ceiling the three limit servers run with. The shipped
# default is 48 000 a minute (domain/ratelimit.go) and exercising *that* would
# mean sending 48 000 requests; what is under test here is the mechanism and
# whose address it is charged to, not the magnitude, which has a unit test of
# its own next to the constant.
LIMIT="${LIMIT:-20}"
FUZZTIME="${FUZZTIME:-10m}"

SERVER_PIDS=()

cleanup() {
  for pid in ${SERVER_PIDS[@]+"${SERVER_PIDS[@]}"}; do
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  done
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

step() { printf '\n\033[1m== %s\033[0m\n' "$1"; }
ok()   { printf '   ok   %s\n' "$1"; }
note() { printf '   ..   %s\n' "$1"; }
fail() { printf '   \033[31mFAIL\033[0m %s\n' "$1"; exit 1; }

# count pulls one integer field out of the loader's one-line JSON summary.
# The key is passed bare — "200", "ratio" — and quoted here, once.
count() {
  local value
  value="$(printf '%s' "$1" | sed -n "s/.*\"$2\":\([0-9][0-9]*\).*/\1/p" | head -1)"
  printf '%s' "${value:-0}"
}

# rss_peak reports a process's high-water resident set, in kilobytes.
#
# VmHWM and not a sample of VmRSS: a spike that has already been collected is
# invisible to a sample taken afterwards, and a spike is exactly what is being
# looked for. Whether the memory is still held is a different question from
# whether it was ever held, and only the second one is a denial of service.
rss_peak() { awk '/VmHWM/ {print $2}' "/proc/$1/status"; }

# rss_reset makes the next measurement a delta rather than a total. Writing 5
# to clear_refs resets the kernel's high-water mark to the current resident
# set, so what is measured afterwards belongs to what happened afterwards and
# not to the Argon2id burst during setup.
rss_reset() { echo 5 >"/proc/$1/clear_refs" 2>/dev/null || true; }

# start_server sets SERVER_PID.
#
# It assigns to a global rather than printing the pid, because a helper called
# inside $(...) runs in a subshell and its `fail` only kills the subshell: the
# run carried on with an empty variable and reported a missing rate limit that
# was really a missing token scope. A gate that can mis-diagnose itself is
# worse than no gate.
start_server() { # $1 port, $2 name, $3... extra flags
  local port="$1" name="$2"; shift 2
  require_free_port "$port" "the hardening gate"
  "$BINARY" serve -addr "127.0.0.1:$port" -db "$WORKDIR/$name.db" -origin "$ORIGIN" "$@" \
    >"$WORKDIR/$name.log" 2>&1 &
  local pid=$!
  SERVER_PIDS+=("$pid")
  local attempt
  for attempt in $(seq 1 80); do
    curl -fsS "http://127.0.0.1:$port/api/v1/health" >/dev/null 2>&1 && break
    sleep 0.1
  done
  curl -fsS "http://127.0.0.1:$port/api/v1/health" >/dev/null \
    || { cat "$WORKDIR/$name.log"; fail "the server on $port never became healthy"; }
  SERVER_PID="$pid"
}

# set_up_server sets SETUP_TOKEN, SETUP_KEY and SETUP_PING, for the reason
# start_server sets SERVER_PID.
set_up_server() { # $1 port, $2 name
  local port="$1" name="$2"
  curl -fsS -X POST "http://127.0.0.1:$port/api/v1/setup" \
    -H 'Content-Type: application/json' -H 'X-Trapline-Request: 1' \
    -d "{\"username\":\"antonio\",\"password\":\"$PASSWORD\"}" >/dev/null \
    || fail "setup failed on $port"

  local token dsn ping
  # Every scope, because this gate drives the whole surface. The scopes
  # themselves are checked where they mean something — a token with only
  # projects:* is refused a monitor in crons.sh — and a gate that ran out of
  # permissions halfway would report the symptom of the next check instead.
  token="$("$BINARY" token create -db "$WORKDIR/$name.db" -name hardening \
    -scopes projects:read,projects:write,alerts:read,alerts:write,monitors:read,monitors:write --json \
    | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
  [ -n "$token" ] || fail "no token on $port"
  export TRAPLINE_URL="http://127.0.0.1:$port" TRAPLINE_TOKEN="$token"
  dsn="$("$BINARY" projects create -name venekambio)"
  # A cron monitor exists on every server because its ping key is the only
  # credential this product has that travels in a URL, which makes /ping the
  # second surface a stranger can reach at will.
  ping="$("$BINARY" monitors cron add -project 1 -slug nightly-backup \
    -schedule '*/5 * * * *' --json | sed -n 's/.*"ping_key":"\([^"]*\)".*/\1/p')"
  [ "${#ping}" -eq 32 ] || fail "no ping key on $port: '$ping'"
  SETUP_TOKEN="$token"
  SETUP_KEY="$(printf '%s' "$dsn" | sed -n 's|.*//\([^@]*\)@.*|\1|p')"
  SETUP_PING="$ping"
  [ "${#SETUP_KEY}" -eq 32 ] || fail "the DSN on $port has no usable public key: '$dsn'"
}

# --------------------------------------------------------------------- build

if [ "$ONLY" != fuzz ]; then

step "build"
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BINARY" ./cmd/trapline
ok "$(du -h "$BINARY" | cut -f1) binary"
( cd compat/load && go build -o compat-load . ) || fail "the load generator did not build"
ok "the load generator built"

# ------------------------------------------------------- per-address ceilings

step "the ingest ceiling, with no proxy trusted"
start_server "$PORT" plain -ingest-ip-rate-limit "$LIMIT"
set_up_server "$PORT" plain
PLAIN_KEY="$SETUP_KEY"
ok "server on $PORT, ingest ceiling $LIMIT a minute, nothing trusted to forward"

# Every request claims a different address. With no trusted proxy configured
# the header must change nothing: if it did, the limiter's keys would be chosen
# by whoever is attacking it and this check would go green with no effective
# limit in place at all. That is the mistake smoke.sh's login flood was written
# to catch, one endpoint over.
FLOOD="$("$LOADER" -mode flood -url "http://127.0.0.1:$PORT" -key "$PLAIN_KEY" \
  -codec none -workers 8 -requests $((LIMIT * 4)) -forwarded-for rotate)"
SERVED="$(count "$FLOOD" 200)"
REFUSED="$(count "$FLOOD" 429)"
[ "$REFUSED" -gt 0 ] \
  || { printf '   %s\n' "$FLOOD"; fail "$((LIMIT * 4)) ingest requests were all served: there is no per-address ceiling"; }
[ "$SERVED" -le "$LIMIT" ] \
  || { printf '   %s\n' "$FLOOD"; fail "$SERVED got through a ceiling of $LIMIT: a forged X-Forwarded-For bought budget"; }
ok "$SERVED of $((LIMIT * 4)) served, $REFUSED refused; rotating the header bought nothing"

RETRY="$(curl -sS -D - -o /dev/null -X POST "http://127.0.0.1:$PORT/api/1/envelope/" \
  -H "X-Sentry-Auth: Sentry sentry_key=$PLAIN_KEY" --data-binary '{}')"
printf '%s' "$RETRY" | grep -q '^HTTP/[0-9.]* 429' || fail "the ingest endpoint stopped refusing"
printf '%s' "$RETRY" | tr 'A-Z' 'a-z' | grep -q '^retry-after:' \
  || fail "a refused ingest request carries no Retry-After, so a client comes straight back"
ok "refusals carry Retry-After"

# An SDK must not read an address-level refusal as "this category is switched
# off": the official clients obey X-Sentry-Rate-Limits by dropping the category
# for the whole window they are given, which would turn one burst into minutes
# of lost errors from a healthy application (ADR 005, ADR 023).
# Anchored at the start of a line, because the response also carries
# `Access-Control-Expose-Headers: X-Sentry-Rate-Limits, …` — the CORS list that
# tells a browser SDK it is *allowed to read* that header when there is one
# (cors.go). An unanchored match finds that list and reports a header the
# server never sent; the first version of this check did exactly that and
# accused the product of a bug it does not have.
if printf '%s' "$RETRY" | tr 'A-Z' 'a-z' | grep -q '^x-sentry-rate-limits:'; then
  fail "a per-address refusal carries the protocol's category header; SDKs would stop sending"
fi
ok "and they do not carry X-Sentry-Rate-Limits, which would silence a healthy app"

step "the same ceiling on the unauthenticated ping surface"
start_server "$PING_PORT" ping -ingest-ip-rate-limit "$LIMIT"
set_up_server "$PING_PORT" ping
PING_PING="$SETUP_PING"
# A server of its own, not the one above: /ping shares the ingest limiter
# deliberately (ping_handler.go), so running both checks against one server
# would have the second read the first one's exhaustion and pass without
# proving anything about /ping at all.
PING_FLOOD="$("$LOADER" -mode flood -url "http://127.0.0.1:$PING_PORT" -key unused \
  -path "/ping/$PING_PING" -codec none -workers 8 -requests $((LIMIT * 4)) -forwarded-for rotate)"
PING_SERVED="$(count "$PING_FLOOD" 200)"
PING_REFUSED="$(count "$PING_FLOOD" 429)"
[ "$PING_REFUSED" -gt 0 ] \
  || { printf '   %s\n' "$PING_FLOOD"; fail "the ping surface has no per-address ceiling"; }
[ "$PING_SERVED" -le "$LIMIT" ] \
  || { printf '   %s\n' "$PING_FLOOD"; fail "$PING_SERVED pings got through a ceiling of $LIMIT"; }
ok "$PING_SERVED of $((LIMIT * 4)) pings served, $PING_REFUSED refused"

step "the ceilings with this machine trusted to forward"
start_server "$PROXY_PORT" proxy -ingest-ip-rate-limit "$LIMIT" -trusted-proxies 127.0.0.1
set_up_server "$PROXY_PORT" proxy
PROXY_KEY="$SETUP_KEY"
ok "server on $PROXY_PORT, the loopback trusted to forward"

# Half one: the header is believed now, so distinct claimed addresses get
# distinct budgets. Without this an installation behind a reverse proxy sees
# every client as the proxy and rate-limits its whole userbase as one address.
SPREAD="$("$LOADER" -mode flood -url "http://127.0.0.1:$PROXY_PORT" -key "$PROXY_KEY" \
  -codec none -workers 8 -requests $((LIMIT * 4)) -forwarded-for rotate)"
SPREAD_SERVED="$(count "$SPREAD" 200)"
[ "$SPREAD_SERVED" -eq $((LIMIT * 4)) ] \
  || { printf '   %s\n' "$SPREAD"; fail "with the proxy trusted, only $SPREAD_SERVED of $((LIMIT * 4)) got through; the header is not being read"; }
ok "$SPREAD_SERVED of $((LIMIT * 4)) served: each claimed address has its own budget"

# Half two, and the one that matters: trusting a proxy must not switch the
# ceiling off. One claimed address is still one address.
SAME="$("$LOADER" -mode flood -url "http://127.0.0.1:$PROXY_PORT" -key "$PROXY_KEY" \
  -codec none -workers 8 -requests $((LIMIT * 4)) -forwarded-for 198.51.100.7)"
SAME_SERVED="$(count "$SAME" 200)"
SAME_REFUSED="$(count "$SAME" 429)"
[ "$SAME_REFUSED" -gt 0 ] \
  || { printf '   %s\n' "$SAME"; fail "one forwarded address was served $((LIMIT * 4)) times: trusting a proxy disabled the ceiling"; }
[ "$SAME_SERVED" -le "$LIMIT" ] \
  || { printf '   %s\n' "$SAME"; fail "$SAME_SERVED got through a ceiling of $LIMIT behind a trusted proxy"; }
ok "$SAME_SERVED served, $SAME_REFUSED refused: one forwarded address is still one address"

# ------------------------------------------------------- decompression bombs

step "decompression bombs"
start_server "$MAIN_PORT" main
MAIN_PID="$SERVER_PID"
set_up_server "$MAIN_PORT" main
MAIN_TOKEN="$SETUP_TOKEN"; MAIN_KEY="$SETUP_KEY"; MAIN_PING="$SETUP_PING"
MAIN_API="http://127.0.0.1:$MAIN_PORT/api/v1"
ok "server on $MAIN_PORT with the shipped ceilings, so the bombs reach the decompressor"

# The documented maximum first. maxIngestBody promises that a 20 MiB envelope
# is accepted, and a memory budget unable to admit one would quietly turn that
# promise into a lie — a worse failure than the one being fixed, because it
# only shows up for the customer with the largest events.
LEGIT="$("$LOADER" -mode flood -url "http://127.0.0.1:$MAIN_PORT" -key "$MAIN_KEY" \
  -codec gzip -ratio 900 -expand-mb 18 -workers 1 -requests 2)"
[ "$(count "$LEGIT" 200)" = 2 ] \
  || { printf '   %s\n' "$LEGIT"; fail "an 18 MiB envelope, inside the documented 20 MiB limit, was refused"; }
ok "an 18 MiB envelope is still accepted"

for codec in gzip zstd; do
  # gzip is asked for 900:1 rather than the 1000:1 the zstd check uses because deflate
  # cannot do better: its ceiling on perfectly repetitive input is about
  # 1032:1 and the envelope framing spends part of that. zstd reaches 21 000:1
  # on the same body, which is the real shape of this threat — 1 179 bytes of
  # upload against 24 MiB of expansion.
  want_ratio=900
  [ "$codec" = zstd ] && want_ratio=1000

  rss_reset "$MAIN_PID"
  SINGLE="$("$LOADER" -mode flood -url "http://127.0.0.1:$MAIN_PORT" -key "$MAIN_KEY" \
    -codec "$codec" -ratio "$want_ratio" -expand-mb 24 -workers 1 -requests 5)"
  RATIO="$(count "$SINGLE" ratio)"
  P50="$(count "$SINGLE" p50_ms)"; P50="${P50:-9999}"

  # 413 and not 500, five times out of five. It used to be 500: the sentinel
  # meaning "the client sent too much" was translated on the path that reads a
  # header line and not on the one that reads an item payload, which is the
  # path every bomb and every real SDK takes. An SDK reads 5xx as the server's
  # fault and retries for ever, and each attempt wrote an ERROR line — which
  # made the log a cheaper target than the memory ever was.
  [ "$(count "$SINGLE" 413)" = 5 ] \
    || { printf '   %s\n' "$SINGLE"; fail "a $codec bomb was not refused with 413 five times out of five"; }
  ok "$codec bomb at ${RATIO}:1 refused with 413, five for five"

  [ "$P50" -lt "$MAX_BOMB_MS" ] \
    || { printf '   %s\n' "$SINGLE"; fail "a $codec bomb took ${P50}ms to refuse, budget ${MAX_BOMB_MS}ms: it is being expanded before it is refused"; }
  ok "refused in ${P50}ms, inside the ${MAX_BOMB_MS}ms budget"

  SMALL_PEAK=0
  BIG_PEAK=0
  for workers in 32 128; do
    sleep 1
    rss_reset "$MAIN_PID"
    RESULT="$("$LOADER" -mode flood -url "http://127.0.0.1:$MAIN_PORT" -key "$MAIN_KEY" \
      -codec "$codec" -ratio "$want_ratio" -expand-mb 24 -workers "$workers" -requests $((workers * 4)))"
    PEAK="$(rss_peak "$MAIN_PID")"
    UPLOAD="$(count "$RESULT" compressed_bytes)"
    note "$codec × $workers concurrent, $((workers * 4)) requests of ${UPLOAD} B each: peak ${PEAK} KB"
    [ "$PEAK" -le "$MAX_FLOOD_RSS_KB" ] \
      || fail "a $codec flood took resident memory to ${PEAK} KB, ceiling ${MAX_FLOOD_RSS_KB} KB"
    if [ "$workers" = 32 ]; then SMALL_PEAK="$PEAK"; else BIG_PEAK="$PEAK"; fi
  done

  # The assertion the per-request ceilings could never make. Quadrupling the
  # flood must not quadruple the cost; if it does there is no figure to
  # publish, because the figure is whatever the attacker chooses.
  GROWTH=$((BIG_PEAK * 100 / SMALL_PEAK))
  [ "$GROWTH" -le "$MAX_FLOOD_GROWTH_PCT" ] \
    || fail "four times the $codec flood cost ${GROWTH}% of the memory, ceiling ${MAX_FLOOD_GROWTH_PCT}%: the cost still follows the attack"
  ok "four times the flood cost ${GROWTH}% of the memory, not 400%"
done

# The server has to still be a server afterwards. A budget that is not returned
# is permanent, and that failure would look exactly like the attack it stops.
curl -fsS "$MAIN_API/health" >/dev/null || fail "the server is unhealthy after the floods"
AFTER="$("$LOADER" -mode flood -url "http://127.0.0.1:$MAIN_PORT" -key "$MAIN_KEY" \
  -codec none -workers 1 -requests 1)"
[ "$(count "$AFTER" 200)" = 1 ] \
  || { printf '   %s\n' "$AFTER"; fail "an ordinary event is refused after the floods: the memory budget leaked"; }
ok "an ordinary event is still accepted: nothing leaked"

# ---------------------------------------------------------- response headers

step "security headers"
export TRAPLINE_URL="http://127.0.0.1:$MAIN_PORT" TRAPLINE_TOKEN="$MAIN_TOKEN"
"$BINARY" config set -project 1 -status-page on >/dev/null || fail "could not switch the status page on"

check_headers() { # $1 label, $2 url
  local label="$1" url="$2" headers
  headers="$(curl -sS -D - -o /dev/null "$url" | tr 'A-Z' 'a-z')"
  local header
  for header in 'x-content-type-options: nosniff' 'x-frame-options: deny' 'referrer-policy: no-referrer'; do
    printf '%s' "$headers" | grep -q "^${header}" \
      || { printf '%s\n' "$headers"; fail "$label is served without '$header'"; }
  done
  printf '%s' "$headers" | grep -q '^content-security-policy:' \
    || { printf '%s\n' "$headers"; fail "$label is served with no Content-Security-Policy"; }
  printf '%s' "$headers" | grep -q "frame-ancestors 'none'" \
    || fail "$label's CSP does not forbid framing"
  ok "$label"
}

check_headers "the panel"        "http://127.0.0.1:$MAIN_PORT/"
check_headers "the API"          "$MAIN_API/health"
check_headers "the status page"  "http://127.0.0.1:$MAIN_PORT/status/venekambio"
check_headers "the ping surface" "http://127.0.0.1:$MAIN_PORT/ping/$MAIN_PING"
# An error response is a response. These headers come from middleware wrapped
# around everything, so checking a 404 is checking that nothing writes a reply
# before that middleware has run.
check_headers "a 404"            "$MAIN_API/there-is-nothing-here"

# ------------------------------------------------------------------- CSRF

step "the CSRF header defends the cookie, and only the cookie"
COOKIES="$WORKDIR/cookies"
curl -fsS -c "$COOKIES" -X POST "$MAIN_API/login" \
  -H 'Content-Type: application/json' -H 'X-Trapline-Request: 1' \
  -d "{\"username\":\"antonio\",\"password\":\"$PASSWORD\"}" >/dev/null \
  || fail "could not log in"

NO_HEADER="$(curl -sS -b "$COOKIES" -o /dev/null -w '%{http_code}' \
  -X POST "$MAIN_API/projects" -H 'Content-Type: application/json' -d '{"name":"forjado"}')"
[ "$NO_HEADER" = 403 ] || fail "a cookie-authenticated POST without the CSRF header answered $NO_HEADER, want 403"
ok "a cookie-authenticated write without the header is refused"

WITH_HEADER="$(curl -sS -b "$COOKIES" -o /dev/null -w '%{http_code}' \
  -X POST "$MAIN_API/projects" -H 'Content-Type: application/json' -H 'X-Trapline-Request: 1' \
  -d '{"name":"legitimo"}')"
[ "$WITH_HEADER" = 201 ] || fail "the same write with the header answered $WITH_HEADER, want 201"
ok "and is accepted with it"

# The three surfaces where requiring the header would be a total, silent
# outage: no official SDK, no curl in a crontab and no sentry-cli sends a
# header this product invented. Each of them authenticates explicitly, so
# there is no ambient credential to forge and nothing for the check to defend.
INGEST_CODE="$(printf '{}\n{"type":"event","length":2}\n{}\n' \
  | curl -sS -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$MAIN_PORT/api/1/envelope/" \
      -H "X-Sentry-Auth: Sentry sentry_key=$MAIN_KEY" --data-binary @-)"
[ "$INGEST_CODE" = 200 ] || fail "ingest without the CSRF header answered $INGEST_CODE; every SDK would break"
ok "ingest does not require it"

PING_CODE="$(curl -sS -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$MAIN_PORT/ping/$MAIN_PING")"
[ "$PING_CODE" = 200 ] || fail "a POST ping without the CSRF header answered $PING_CODE"
ok "the ping surface does not require it"

# A real path off the recorded surface, not an invented one: a 404 would
# satisfy "not 403" while proving nothing, which is how a check quietly stops
# checking (ADR 013, compat/sentry-cli/fixtures/).
COMPAT_CODE="$(curl -sS -o /dev/null -w '%{http_code}' \
  -X POST "http://127.0.0.1:$MAIN_PORT/api/0/projects/any-org/venekambio/releases/" \
  -H "Authorization: Bearer $MAIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"version":"shop@1.0.0","projects":["venekambio"]}')"
case "$COMPAT_CODE" in
  200|201) ok "the sentry-cli surface does not require it (it answered $COMPAT_CODE)" ;;
  403) fail "the sentry-cli surface requires a header sentry-cli does not send" ;;
  *) fail "the sentry-cli release path answered $COMPAT_CODE; this check is no longer checking anything" ;;
esac

# ------------------------------------------------------------------ tokens

step "an expired token is a dead token"
LIVE_TOKEN="$("$BINARY" token create -db "$WORKDIR/main.db" -name live -expires-in 24h --json \
  | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
DYING_TOKEN="$("$BINARY" token create -db "$WORKDIR/main.db" -name dying -expires-in 2s --json \
  | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
[ -n "$LIVE_TOKEN" ] && [ -n "$DYING_TOKEN" ] || fail "the tokens were not minted"

CODE="$(curl -sS -o /dev/null -w '%{http_code}' "$MAIN_API/projects" -H "Authorization: Bearer $DYING_TOKEN")"
[ "$CODE" = 200 ] || fail "a token that has not expired yet answered $CODE"
ok "a token with time left works"

sleep 3
CODE="$(curl -sS -o /dev/null -w '%{http_code}' "$MAIN_API/projects" -H "Authorization: Bearer $DYING_TOKEN")"
[ "$CODE" = 401 ] || fail "an expired token answered $CODE, want 401"
ok "and stops working the moment it expires"

CODE="$(curl -sS -o /dev/null -w '%{http_code}' "$MAIN_API/projects" -H "Authorization: Bearer $LIVE_TOKEN")"
[ "$CODE" = 200 ] || fail "a live token answered $CODE: expiry is reaching rows it should not"
ok "the token beside it is untouched"

# A credential that is wrong in a way nobody anticipated must be 401 and never
# 500: a parser that panics on a malformed Authorization header is reachable by
# anyone, with no credential at all.
# `Bearer  <token>` with two spaces is deliberately absent from this list: RFC
# 7235 spells the separator 1*SP, so it is a valid credential and accepting it
# is correct. It was in the list once, and what it caught was the gate's own
# reading of the RFC.
for bad in "Bearer ek_nonexistent" "Bearer" "Basic $LIVE_TOKEN" "$LIVE_TOKEN" "Bearer ek_"; do
  CODE="$(curl -sS -o /dev/null -w '%{http_code}' "$MAIN_API/projects" -H "Authorization: $bad")"
  [ "$CODE" = 401 ] || fail "the credential '$bad' answered $CODE, want 401"
done
ok "a malformed, misspelt or unknown credential is 401 and never 500"

# ------------------------------------------------------------------- gosec

step "gosec"
command -v gosec >/dev/null 2>&1 || export PATH="$PATH:$(go env GOPATH)/bin"
command -v gosec >/dev/null 2>&1 \
  || fail "gosec is not installed; run: go install github.com/securego/gosec/v2/cmd/gosec@latest"
# Two choices worth defending.
#
# **High severity at medium confidence or better.** The whole ruleset on a
# codebase this size is mostly G304 on paths the operator chose and G104 on
# deliberate discards, and a gate that reports fifty findings nobody will
# action is a gate that gets switched off — the reasoning nightly.yml already
# applies to slow jobs, applied here to noisy ones.
#
# **Shipped code only, not ./... .** `compat/` is test tooling: load
# generators and SDK harnesses that seed deterministic pseudo-random data on
# purpose, so G404 ("weak random number generator") is exactly right about them
# and exactly irrelevant. Scanning them would put three permanent findings in
# front of whoever reads this, which is how a security gate teaches people to
# scroll past it. What goes in the binary is what has to be clean.
if ! gosec -quiet -severity high -confidence medium -fmt text \
    ./cmd/... ./internal/... >"$WORKDIR/gosec.txt" 2>&1; then
  cat "$WORKDIR/gosec.txt"
  fail "gosec found high-severity issues"
fi
ok "no high-severity findings at medium confidence or better"

fi  # --- end of the live section

# -------------------------------------------------------------------- fuzz

if [ "$ONLY" != live ]; then

step "fuzzing the decoders that read attacker-chosen bytes ($FUZZTIME each)"
# Four targets, and the list is not arbitrary: it is every parser in this
# product whose input is chosen by somebody else. Three read from the public
# ingest endpoint, whose only credential ships inside browser bundles; the
# fourth reads a file a user uploaded, produced by a bundler this product does
# not control (ADR 002, ADR 018).
#
# Ten minutes rather than the sixty seconds a pull request runs. A minute finds
# regressions against the corpus already on disk, which is worth having on
# every change; finding something *new* in a parser this well covered takes
# longer than anybody will wait for a review, which is the whole reason this
# script's home is the nightly workflow.
fuzz_one() { # $1 package, $2 target, $3 what it reads
  printf '   ..   %s — %s\n' "$2" "$3"
  go test -run=XXX -fuzz="$2" -fuzztime="$FUZZTIME" "$1" >"$WORKDIR/$2.log" 2>&1 || {
    tail -40 "$WORKDIR/$2.log"
    fail "$2 found a crasher; the input that caused it is in ${1#./}testdata/fuzz/$2"
  }
  ok "$2: $(sed -n 's/.*execs: \([0-9]*\) .*/\1/p' "$WORKDIR/$2.log" | tail -1) executions, nothing found"
}

fuzz_one ./internal/envelope/      FuzzParse           "the envelope framing on the public endpoint"
fuzz_one ./internal/sentry/        FuzzDecodeEvent     "the event payload inside it"
fuzz_one ./internal/sourcemap/     FuzzSourceMap       "a source map somebody uploaded"
fuzz_one ./internal/engine/sketch/ FuzzUnmarshalBinary "a latency sketch read back off disk"

fi

printf '\n\033[1;32mall checks passed\033[0m\n'
