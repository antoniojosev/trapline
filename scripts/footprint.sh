#!/usr/bin/env bash
#
# What this product actually costs to run, measured rather than claimed.
#
# It produces the two figures the README publishes — a minimum install and one
# with every subsystem switched on — and it is also the gate that stops those
# figures from quietly becoming false. The methodology, and what each number
# does and does not cover, is docs/benchmarks/footprint.md; this file is the
# executable half of that document and the two are meant to be read together.
#
#   ./scripts/footprint.sh           measure, print a table, enforce the budgets
#   ./scripts/footprint.sh --json    the same run, as one JSON object
#   ./scripts/footprint.sh --quick   a tenth of the volume, for development
#
# Three decisions shape everything below.
#
# **Every profile is measured three ways, never one.** At rest, at its peak
# while ingesting, and at its peak while being read. A footprint quoted from an
# idle process is the number that is true when nobody is using the product,
# which is the one moment nobody cares about. This repository has published
# three memory figures that were wrong in exactly that way — Argon2id budgeted
# per hash, the zstd encoder measured on a two-core machine, and `/login` with
# no ceiling on concurrent attempts, which turned a 30 MB footprint into
# 1.03 GB (ADR 023, smoke.sh).
#
# **The peak is read from VmHWM, not sampled.** A spike that has already been
# collected is invisible to a sample taken after it, and a spike is the thing
# being looked for.
#
# **"Everything on" means everything.** Errors, the hourly aggregates and the
# search index; source-map artefacts and the symbolication cache; tracing,
# its sketches and the downsample job; release health and its in-memory
# session window; alert channels with their encrypted secrets; ten
# monitors, cron and uptime; the public status page; the weekly digest. A
# profile that leaves out the subsystems added last would publish the footprint
# of a product that no longer exists.
set -euo pipefail

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/.."

AS_JSON=false
QUICK=false
while [ $# -gt 0 ]; do
  case "$1" in
    --json) AS_JSON=true; shift ;;
    --quick) QUICK=true; shift ;;
    *) printf 'usage: %s [--json] [--quick]\n' "$0" >&2; exit 2 ;;
  esac
done

PORT="${PORT:-9000}"
LOADED_PORT=$((PORT + 1))

# shellcheck source=lib/port.sh
. "$PWD/scripts/lib/port.sh"

ORIGIN="https://errors.example.test"
BINARY="$PWD/trapline"
LOADER="$PWD/compat/load/compat-load"
WORKDIR="$(mktemp -d)"
PASSWORD='una contraseña larga y buena'

# The published shape of a loaded installation. A thousand distinct issues
# rather than a hundred thousand copies of one, because the index, the search
# table and the hourly buckets all scale with the number of *distinct* issues
# and not with the event count — a hundred thousand of the same error is one
# row in most of the places that cost anything.
EVENTS="${FOOTPRINT_EVENTS:-100000}"
ISSUES="${FOOTPRINT_ISSUES:-1000}"
TRANSACTIONS="${FOOTPRINT_TRANSACTIONS:-20000}"
SESSIONS="${FOOTPRINT_SESSIONS:-10000}"
MONITORS="${FOOTPRINT_MONITORS:-10}"
WORKERS="${FOOTPRINT_WORKERS:-8}"
if $QUICK; then
  EVENTS=10000; ISSUES=200; TRANSACTIONS=2000; SESSIONS=1000; MONITORS=4
fi

# How long to wait for the collector to give memory back before reading the
# at-rest figure. Go's scavenger is lazy on purpose, and a number read the
# instant a load stops is a measurement of the collector's schedule rather than
# of this program.
SETTLE_SECONDS="${SETTLE_SECONDS:-12}"

# The budgets. Each one is a published promise, so each one fails the build.
#
# The minimum figure is the README's headline and matches smoke.sh, which
# enforces the same ceiling on a fresh install; repeating it here is
# deliberate, because this is the script whose output the README quotes.
# The headroom over the measured figure is deliberate and is about the same
# ratio smoke.sh uses: a budget set to the last measurement is a budget that
# goes red on a busier runner, and a gate that goes red for no reason is a gate
# somebody disables. Roughly 1,8× for the resting figures and 2,4× for the
# peaks, which still leaves no room at all for the kind of regression these
# exist to catch — the one this branch found took the same peak past 1,4 GB.
MAX_MINIMUM_IDLE_KB="${MAX_MINIMUM_IDLE_KB:-30720}"
MAX_LOADED_IDLE_KB="${MAX_LOADED_IDLE_KB:-65536}"
MAX_LOADED_INGEST_PEAK_KB="${MAX_LOADED_INGEST_PEAK_KB:-98304}"
MAX_LOADED_QUERY_PEAK_KB="${MAX_LOADED_QUERY_PEAK_KB:-98304}"
MAX_BINARY_MB="${MAX_BINARY_MB:-30}"

SERVER_PIDS=()
cleanup() {
  for pid in ${SERVER_PIDS[@]+"${SERVER_PIDS[@]}"}; do
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  done
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

step() { $AS_JSON || printf '\n\033[1m== %s\033[0m\n' "$1"; }
note() { $AS_JSON || printf '   ..   %s\n' "$1"; }
ok()   { $AS_JSON || printf '   ok   %s\n' "$1"; }
fail() { printf '   \033[31mFAIL\033[0m %s\n' "$1" >&2; exit 1; }

rss_now()   { awk '/VmRSS/ {print $2}' "/proc/$1/status"; }
rss_peak()  { awk '/VmHWM/ {print $2}' "/proc/$1/status"; }
rss_reset() { echo 5 >"/proc/$1/clear_refs" 2>/dev/null || true; }
mb()        { awk -v kb="$1" 'BEGIN { printf "%.1f", kb / 1024 }'; }

start_server() { # $1 port, $2 name, $3... extra flags; sets SERVER_PID
  local port="$1" name="$2"; shift 2
  require_free_port "$port" "the footprint gate"
  "$BINARY" serve -addr "127.0.0.1:$port" -db "$WORKDIR/$name.db" -origin "$ORIGIN" "$@" \
    >"$WORKDIR/$name.log" 2>&1 &
  local pid=$! attempt
  SERVER_PIDS+=("$pid")
  for attempt in $(seq 1 100); do
    curl -fsS "http://127.0.0.1:$port/api/v1/health" >/dev/null 2>&1 && break
    sleep 0.1
  done
  curl -fsS "http://127.0.0.1:$port/api/v1/health" >/dev/null \
    || { cat "$WORKDIR/$name.log"; fail "the server on $port never became healthy"; }
  SERVER_PID="$pid"
}

set_up_server() { # $1 port, $2 name; sets SETUP_TOKEN, SETUP_KEY
  local port="$1" name="$2"
  curl -fsS -X POST "http://127.0.0.1:$port/api/v1/setup" \
    -H 'Content-Type: application/json' -H 'X-Trapline-Request: 1' \
    -d "{\"username\":\"antonio\",\"password\":\"$PASSWORD\"}" >/dev/null \
    || fail "setup failed on $port"
  SETUP_TOKEN="$("$BINARY" token create -db "$WORKDIR/$name.db" -name footprint \
    -scopes projects:read,projects:write,alerts:read,alerts:write,monitors:read,monitors:write --json \
    | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
  [ -n "$SETUP_TOKEN" ] || fail "no token on $port"
  export TRAPLINE_URL="http://127.0.0.1:$port" TRAPLINE_TOKEN="$SETUP_TOKEN"
  local dsn
  dsn="$("$BINARY" projects create -name venekambio)"
  SETUP_KEY="$(printf '%s' "$dsn" | sed -n 's|.*//\([^@]*\)@.*|\1|p')"
  [ "${#SETUP_KEY}" -eq 32 ] || fail "no usable public key in '$dsn'"
}

# query_burst reads the product the way a person and an agent do, concurrently.
#
# The read path has its own memory story and it is not the ingest one: the
# dashboard merges sketches, search goes through FTS5 and a listing pages
# through an index. Measuring only ingestion would publish half a figure.
query_burst() { # $1 port, $2 token
  local port="$1" token="$2" round
  local base="http://127.0.0.1:$port/api/v1"
  : >"$WORKDIR/urls"
  for round in $(seq 1 40); do
    {
      printf '%s\n' "$base/projects/1/issues?limit=50"
      printf '%s\n' "$base/projects/1/issues?q=checkout&limit=50"
      printf '%s\n' "$base/projects/1/stats"
      printf '%s\n' "$base/projects/1/stats/top?range=24h"
      printf '%s\n' "$base/projects/1/stats/breakdown?by=release"
      printf '%s\n' "$base/projects/1/transactions"
      printf '%s\n' "$base/projects/1/health"
      printf '%s\n' "$base/projects/1/issues/1/bundle"
      printf '%s\n' "http://127.0.0.1:$port/status/venekambio"
    } >>"$WORKDIR/urls"
  done
  # The status codes are collected and checked, not discarded. A burst of 404s
  # is cheap and would read as a reassuringly small figure — the exact way a
  # benchmark stops measuring the thing it is named after.
  xargs -P 16 -n 1 curl -sS -o /dev/null -w '%{http_code}\n' \
    -H "Authorization: Bearer $token" <"$WORKDIR/urls" >"$WORKDIR/query.codes"
  local served total
  total="$(wc -l <"$WORKDIR/query.codes")"
  served="$(grep -c '^200$' "$WORKDIR/query.codes" || true)"
  [ "$served" = "$total" ] \
    || { sort "$WORKDIR/query.codes" | uniq -c >&2; fail "only $served of $total reads answered 200"; }
  note "$served reads, all 200"
}

# ------------------------------------------------------------------- build

step "build"
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BINARY" ./cmd/trapline
BINARY_BYTES="$(stat -c%s "$BINARY")"
BINARY_MB="$(awk -v b="$BINARY_BYTES" 'BEGIN { printf "%.2f", b / 1048576 }')"
awk -v mb="$BINARY_MB" -v max="$MAX_BINARY_MB" 'BEGIN { exit (mb+0 <= max+0) ? 0 : 1 }' \
  || fail "the binary is ${BINARY_MB} MB, budget ${MAX_BINARY_MB} MB"
ok "${BINARY_MB} MB binary, budget ${MAX_BINARY_MB} MB"
( cd compat/load && go build -o compat-load . ) || fail "the load generator did not build"

# --------------------------------------------------------- minimum profile

step "profile: minimum"
# A fresh install with nothing switched on. One background job — retention,
# which is the only subsystem nobody opts into — and one project accepting
# errors, which is the default profile (ADR 005, ADR 014).
start_server "$PORT" minimum
MINIMUM_PID="$SERVER_PID"
set_up_server "$PORT" minimum

MINIMUM_JOBS="$(curl -fsS "http://127.0.0.1:$PORT/api/v1/system/jobs" \
  -H "Authorization: Bearer $SETUP_TOKEN" | grep -o '"name":' | wc -l)"
[ "$MINIMUM_JOBS" -eq 1 ] \
  || fail "the minimum profile is running $MINIMUM_JOBS background jobs; a switched-off subsystem must cost nothing"
note "background jobs: $MINIMUM_JOBS (retention)"

sleep "$SETTLE_SECONDS"
MINIMUM_IDLE="$(rss_now "$MINIMUM_PID")"
MINIMUM_DISK="$(du -k "$WORKDIR/minimum.db" | cut -f1)"
note "at rest: $(mb "$MINIMUM_IDLE") MB, database $(mb "$MINIMUM_DISK") MB"
[ "$MINIMUM_IDLE" -le "$MAX_MINIMUM_IDLE_KB" ] \
  || fail "the minimum profile rests at ${MINIMUM_IDLE} KB, budget ${MAX_MINIMUM_IDLE_KB} KB"
ok "inside the published minimum"

kill "$MINIMUM_PID" 2>/dev/null || true
wait "$MINIMUM_PID" 2>/dev/null || true

# ------------------------------------------------------ everything-on profile

step "profile: everything on"
# -uptime-allow-private so the uptime monitors can watch this server's own
# health endpoint instead of the open internet. The subsystem under measurement
# is the checker, its scheduler job and its daily aggregate; pointing it at a
# site on the internet would measure somebody else's latency and make a nightly
# gate depend on the network (ADR 016).
start_server "$LOADED_PORT" loaded -uptime-allow-private
LOADED_PID="$SERVER_PID"
set_up_server "$LOADED_PORT" loaded
LOADED_TOKEN="$SETUP_TOKEN"
LOADED_KEY="$SETUP_KEY"

# The per-project ceiling is raised for the duration of the load, and only for
# that. It is spike protection — 12 000 events a minute per category by default
# (ADR 005) — so at the shipped figure, filling an installation with a hundred
# thousand events would take eight minutes of deliberate waiting and the gate
# would be measuring the limiter's clock. The first version of this script did
# not raise it and 1 913 of 2 600 envelopes came back 429; the check below is
# what caught that, and it stays because an installation seeded with a third of
# what it claims would publish a footprint for a product nobody has.
#
# It changes nothing about the figure: a ceiling is a counter, and what is being
# measured is what a hundred thousand events cost once they are there.
"$BINARY" config set -project 1 \
  -categories error,transaction,session,check_in,uptime \
  -traces-sample-rate 1 -artifacts-max-mb 200 -status-page on \
  -rate-limit "$((EVENTS + TRANSACTIONS + SESSIONS + 1000))" >/dev/null \
  || fail "could not switch the subsystems on"
ok "errors, transactions, sessions, check-ins and uptime accepted; tracing at 100%"

# Source maps: a real bundle through the real upload path, so the artefact
# store, the symbolication cache and the zstd blob are all part of the figure
# (ADR 018).
BUNDLE="$WORKDIR/bundle"
mkdir -p "$BUNDLE"
{
  printf 'function h(o){throw new Error("boom "+o.id)}\n'
  for i in $(seq 1 200); do printf 'function f%d(a){return a*%d}\n' "$i" "$i"; done
  printf '//# sourceMappingURL=app.min.js.map\n'
} >"$BUNDLE/app.min.js"
{
  printf '{"version":3,"file":"app.min.js","sourceRoot":"","sources":["app.ts"],'
  printf '"names":["handle","order"],"mappings":"AAAA,SAASA,EAAOC,GAAK","sourcesContent":["'
  for i in $(seq 1 200); do printf 'export function f%d(a: number) { return a * %d }\\n' "$i" "$i"; done
  printf '"]}\n'
} >"$BUNDLE/app.min.js.map"
"$BINARY" artifacts upload -project 1 -release 'shop@1.8.0' "$BUNDLE" >/dev/null \
  || fail "the source-map bundle was refused"
ok "a source-map bundle uploaded and stored"

# Alerts: a channel with an encrypted secret and a rule, so the notifier job
# exists and the secrets file is on disk (ADR 015).
CHANNEL_ID="$("$BINARY" alerts channels add -type webhook -name ops \
  -config '{"url":"http://127.0.0.1:1/hook","secret":"un secreto largo de prueba"}' --json \
  | sed -n 's/.*"id":\([0-9]*\).*/\1/p')"
[ -n "$CHANNEL_ID" ] || fail "the alert channel was not created"
"$BINARY" alerts rules add -name "new issues" -trigger '{"kind":"new_issue"}' \
  -channels "$CHANNEL_ID" -silence 3600 >/dev/null || fail "the alert rule was not created"
"$BINARY" digest schedule -day monday -hour 9 >/dev/null || fail "the digest was not scheduled"
ok "an alert channel, a rule and the weekly digest"

# Ten monitors, half cron and half uptime, because they are different
# subsystems with different jobs behind them (ADR 016, ADR 037).
HALF=$((MONITORS / 2))
for i in $(seq 1 "$HALF"); do
  "$BINARY" monitors cron add -project 1 -slug "job-$i" -schedule '*/10 * * * *' \
    -margin 60 -max-runtime 600 >/dev/null || fail "cron monitor $i was not created"
done
for i in $(seq 1 $((MONITORS - HALF))); do
  "$BINARY" monitors uptime add -project 1 -name "site-$i" \
    -target "http://127.0.0.1:$LOADED_PORT/api/v1/health" -interval 30 -allow-private -public \
    >/dev/null || fail "uptime monitor $i was not created"
done
ok "$MONITORS monitors: $HALF cron, $((MONITORS - HALF)) uptime"

JOBS_JSON="$(curl -fsS "http://127.0.0.1:$LOADED_PORT/api/v1/system/jobs" \
  -H "Authorization: Bearer $LOADED_TOKEN")"
LOADED_JOBS="$(printf '%s' "$JOBS_JSON" | grep -o '"name":' | wc -l)"
note "background jobs: $LOADED_JOBS — $(printf '%s' "$JOBS_JSON" | grep -o '"name":"[a-z-]*"' | sed 's/"name":"//;s/"//' | paste -sd' ' -)"
# Six is what "everything on" means here: retention, the notifier, the two
# monitor watchers, the downsample job and the digest. A subsystem that failed
# to start would make the figures below *better*, which is the direction a
# benchmark lies in when nobody is checking (ADR 014).
[ "$LOADED_JOBS" -ge 6 ] \
  || { printf '   %s\n' "$JOBS_JSON" >&2; fail "only $LOADED_JOBS jobs are running with everything switched on; something did not start, and a figure measured without it would be flattering"; }

step "load"
rss_reset "$LOADED_PID"
SEED="$("$LOADER" -mode seed -url "http://127.0.0.1:$LOADED_PORT" -key "$LOADED_KEY" \
  -events "$EVENTS" -issues "$ISSUES" -transactions "$TRANSACTIONS" -sessions "$SESSIONS" \
  -workers "$WORKERS" -timeout 5m)"
INGEST_PEAK="$(rss_peak "$LOADED_PID")"
EVENTS_PER_S="$(printf '%s' "$SEED" | sed -n 's/.*"events_per_s":\([0-9.]*\).*/\1/p')"
REFUSED="$(printf '%s' "$SEED" | sed -n 's/.*"refused":\([0-9]*\).*/\1/p')"
[ "${REFUSED:-0}" -eq 0 ] \
  || { printf '   %s\n' "$SEED" >&2; fail "$REFUSED envelopes were refused while seeding; the figures below would be for a smaller installation than the one advertised"; }
note "$EVENTS events, $TRANSACTIONS transactions, $SESSIONS sessions at ${EVENTS_PER_S} ev/s"
note "peak while ingesting: $(mb "$INGEST_PEAK") MB"

# The sessions window only writes its counters every sixty seconds and on the
# way down, so the resident figure right after a session load includes a window
# that is about to be drained. Draining it here is what makes the at-rest
# number the one an operator would see a minute later (ADR 008).
sleep "$SETTLE_SECONDS"
LOADED_IDLE="$(rss_now "$LOADED_PID")"
note "at rest: $(mb "$LOADED_IDLE") MB"

step "read load"
rss_reset "$LOADED_PID"
query_burst "$LOADED_PORT" "$LOADED_TOKEN"
QUERY_PEAK="$(rss_peak "$LOADED_PID")"
note "peak while being read: $(mb "$QUERY_PEAK") MB"

LOADED_DISK="$(du -k "$WORKDIR/loaded.db" | cut -f1)"
DISK_PER_EVENT="$(awk -v kb="$LOADED_DISK" -v n="$((EVENTS + TRANSACTIONS + SESSIONS))" \
  'BEGIN { printf "%.0f", kb * 1024 / n }')"
note "database $(mb "$LOADED_DISK") MB, ${DISK_PER_EVENT} bytes an item"

# ------------------------------------------------------------------ verdict

step "budgets"
check_budget() { # $1 label, $2 measured kb, $3 budget kb
  [ "$2" -le "$3" ] || fail "$1 is $(mb "$2") MB, budget $(mb "$3") MB"
  ok "$1: $(mb "$2") MB of $(mb "$3") MB"
}
check_budget "everything on, at rest"        "$LOADED_IDLE"  "$MAX_LOADED_IDLE_KB"
check_budget "everything on, ingest peak"    "$INGEST_PEAK"  "$MAX_LOADED_INGEST_PEAK_KB"
check_budget "everything on, read peak"      "$QUERY_PEAK"   "$MAX_LOADED_QUERY_PEAK_KB"

if $AS_JSON; then
  printf '{"binary_mb":%s,' "$BINARY_MB"
  printf '"minimum":{"idle_kb":%s,"disk_kb":%s,"jobs":%s},' \
    "$MINIMUM_IDLE" "$MINIMUM_DISK" "$MINIMUM_JOBS"
  printf '"loaded":{"idle_kb":%s,"ingest_peak_kb":%s,"query_peak_kb":%s,"disk_kb":%s,' \
    "$LOADED_IDLE" "$INGEST_PEAK" "$QUERY_PEAK" "$LOADED_DISK"
  printf '"jobs":%s,"events":%s,"issues":%s,"transactions":%s,"sessions":%s,"monitors":%s,' \
    "$LOADED_JOBS" "$EVENTS" "$ISSUES" "$TRANSACTIONS" "$SESSIONS" "$MONITORS"
  printf '"events_per_s":%s,"disk_bytes_per_item":%s}}\n' "$EVENTS_PER_S" "$DISK_PER_EVENT"
else
  printf '\n'
  printf '| profile       | at rest  | ingest peak | read peak | on disk  |\n'
  printf '|---------------|----------|-------------|-----------|----------|\n'
  printf '| minimum       | %5s MB | %11s | %9s | %5s MB |\n' \
    "$(mb "$MINIMUM_IDLE")" "n/a" "n/a" "$(mb "$MINIMUM_DISK")"
  printf '| everything on | %5s MB | %8s MB | %6s MB | %5s MB |\n' \
    "$(mb "$LOADED_IDLE")" "$(mb "$INGEST_PEAK")" "$(mb "$QUERY_PEAK")" "$(mb "$LOADED_DISK")"
  printf '\nbinary %s MB · %s events over %s issues · %s transactions · %s sessions · %s monitors\n' \
    "$BINARY_MB" "$EVENTS" "$ISSUES" "$TRANSACTIONS" "$SESSIONS" "$MONITORS"
  printf '\n\033[1;32mevery budget met\033[0m\n'
fi
