#!/usr/bin/env bash
#
# Release health as a gate: a crash-free rate that has to be exactly right, a
# window that lives in memory, and the two ways that memory goes away.
#
# The feature rests on one decision that is easy to state and easy to get
# wrong: **never a row per session** (ADR 008). An SDK does not send finished
# sessions, it sends updates of one, so counting distinct sessions needs state
# — and that state is a bounded in-memory window that only ever writes down
# totals per (release, environment, hour). Three consequences follow, and each
# is a section below:
#
#   1. A hundred sessions with five crashes have to come out as exactly 0.95,
#      through the official SDK's own session tracking and not through
#      hand-written envelopes: what is under test is the shape sentry_sdk puts
#      on the wire (ADR 002).
#   2. A restart loses the window. That is accepted, not accidental — but only
#      for what has not been written yet. An hour already written must be
#      identical afterwards, and only the open hour may move. A clean stop
#      drains first, so it loses nothing that had reached a verdict; an unclean
#      one does not, and that difference is what this section measures.
#   3. A full window sacrifices precision and never stability. A thousand
#      sessions into a window of a hundred must leave the process alive, the
#      window at its ceiling, and the loss visible in the response rather than
#      only in a log.
#
# Run it with PORT set to somewhere free; it takes a temporary database and
# leaves nothing behind.
set -euo pipefail

# 9913 by default: this gate's port block starts at 9910, and its first
# three are spoken for by smoke, compat and ui-smoke.
PORT="${PORT:-9913}"
# The rule every gate in here obeys: never talk to a server this script did not
# start.
# shellcheck source=lib/port.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/port.sh"

require_free_port "$PORT" "the release-health gate"

# The origin is the loopback address this gate serves on, and that is not
# cosmetic: an SDK derives its ingest URL from the DSN alone. A DSN naming
# errors.example.test would send a hundred sessions somewhere that is not this
# server, and the gate would fail with "counted 0 of 100" — which points
# nowhere near the cause. (The tracing gate learned this the expensive way.)
ORIGIN="${ORIGIN:-http://127.0.0.1:$PORT}"
WORKDIR="$(mktemp -d)"
BINARY="$PWD/trapline"
DB="$WORKDIR/trapline.db"
SERVER_PID=""
# The ceiling used only by the last section. Small enough to fill from a
# script, and passed as a real server flag rather than a test hook, because a
# machine with less memory than this product assumes needs the same knob.
SMALL_WINDOW="${SMALL_WINDOW:-100}"

cleanup() {
  [ -n "$SERVER_PID" ] && kill -9 "$SERVER_PID" 2>/dev/null || true
  [ -n "$SERVER_PID" ] && wait "$SERVER_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

step() { printf '\n\033[1m== %s\033[0m\n' "$1"; }
ok()   { printf '   ok   %s\n' "$1"; }
fail() { printf '   FAIL %s\n' "$1"; exit 1; }

API="http://127.0.0.1:$PORT/api/v1"

# number pulls a numeric JSON field out of a body, taking the **first**
# occurrence.
#
# First and not last, and the distinction is not pedantry: a health response
# carries the totals and then a series whose points use the same field names,
# so a greedy match would silently read the last hour of the chart instead of
# the total above it — a number that is right whenever the range holds one hour
# and wrong the moment it holds two. `null` is matched too, because a release
# with no sessions has no crash-free rate and the difference between "null" and
# "nothing was there" is a failure worth reading.
number() { # body key
  printf '%s' "$1" | grep -o "\"$2\":\(null\|[-0-9.eE+]\+\)" | head -1 | sed 's/^[^:]*://'
}

# object cuts one nested object out of a response, so a field that appears in
# several of them is read from the one that was meant.
object() { # body key
  printf '%s' "$1" | sed -n 's/.*"'"$2"'":{\([^}]*\)}.*/\1/p'
}

# hour_object cuts one hour out of a series, by its bucket name.
hour_object() { # body hour
  printf '%s' "$1" | sed -n 's/.*{"hour":"'"$2"'"\([^}]*\)}.*/\1/p'
}

within() { # got want tolerance description
  local got="$1" want="$2" tolerance="$3" description="$4"
  [ -n "$got" ] && [ -n "$want" ] || fail "$description: could not read both numbers (got '$got', want '$want')"
  awk -v got="$got" -v want="$want" -v tol="$tolerance" 'BEGIN {
    diff = got - want
    if (diff < 0) diff = -diff
    exit (diff <= tol) ? 0 : 1
  }' || fail "$(printf '%s: %s is further than %s from %s' "$description" "$got" "$tolerance" "$want")"
  ok "$(printf '%s: %s' "$description" "$got")"
}

equals() { # got want description
  [ "$1" = "$2" ] || fail "$3: got '$1', want '$2'"
  ok "$3: $1"
}

start_server() { # [extra flags...]
  "$BINARY" serve -addr "127.0.0.1:$PORT" -db "$DB" -origin "$ORIGIN" "$@" \
    >>"$WORKDIR/server.log" 2>&1 &
  SERVER_PID=$!
  for _ in $(seq 1 100); do
    curl -fsS "$API/health" >/dev/null 2>&1 && return 0
    sleep 0.1
  done
  cat "$WORKDIR/server.log"
  fail "the server never became healthy on port $PORT"
}

# stop_server_cleanly is the flush this gate depends on, and saying so here is
# the point: settled counters are written down every sixty seconds *and on the
# way down* (ADR 008). A gate that waited a minute per assertion would be a
# gate nobody runs, and a gate that reached into the database would be checking
# something other than the path a real stop takes.
stop_server_cleanly() {
  [ -n "$SERVER_PID" ] || return 0
  kill -TERM "$SERVER_PID" 2>/dev/null || true
  wait "$SERVER_PID" 2>/dev/null || true
  SERVER_PID=""
}

# stop_server_abruptly is the other half: a process that dies without draining,
# which is what ADR 008 accepts losing.
stop_server_abruptly() {
  [ -n "$SERVER_PID" ] || return 0
  kill -9 "$SERVER_PID" 2>/dev/null || true
  wait "$SERVER_PID" 2>/dev/null || true
  SERVER_PID=""
}

health_json() { # [args...]
  "$BINARY" releases health -project "$PROJECT_ID" --json "$@"
}

step "build"
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BINARY" ./cmd/trapline
ok "server built"

step "boot"
start_server
ok "healthy on port $PORT"

curl -fsS -X POST "$API/setup" \
  -H 'Content-Type: application/json' -H 'X-Trapline-Request: 1' \
  -d '{"username":"antonio","password":"una contraseña larga y buena"}' >/dev/null \
  || fail "setup failed"

TOKEN="$("$BINARY" token create -db "$DB" -name health --json | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
[ -n "$TOKEN" ] || fail "no token returned"
export TRAPLINE_URL="http://127.0.0.1:$PORT"
export TRAPLINE_TOKEN="$TOKEN"

DSN="$("$BINARY" projects create -name venekambio)"
PROJECT_ID=1
SENTRY_KEY="$(printf '%s' "$DSN" | sed -n 's|.*//\([^@]*\)@.*|\1|p')"
ok "project created"

# Sessions are off by default, which is the whole of ADR 005: a subsystem
# nobody switched on costs nothing. The last section checks that the refusal
# before this line was a real one.
"$BINARY" config set -project "$PROJECT_ID" -categories error,session >/dev/null \
  || fail "could not switch sessions on"
ok "sessions enabled"

step "a hundred sessions, five of them crashes"
# Through the official Python SDK's own session tracking — `track_session`,
# which is what `auto_session_tracking` turns on — because what is under test
# is the shape sentry_sdk puts on the wire. A hand-written envelope would only
# check that this repository agrees with itself.
SENT="$(./compat/python/run-health.sh --dsn "$DSN" --release 'shop@1.4.2' \
  --count 100 --crashes 5 --errors 10)" \
  || { printf '%s\n' "$SENT"; fail "the Python SDK could not deliver its sessions"; }
printf '   ..   sent: %s\n' "$SENT"

# The restart is the flush. Counters are written every sixty seconds and on the
# way down, and waiting the minute would make this gate a minute longer for
# nothing.
stop_server_cleanly
grep -q 'session window drained' "$WORKDIR/server.log" \
  || fail "the clean stop did not drain the window"
ok "a clean stop drained the window before exiting"
start_server

FIRST="$(health_json -version 'shop@1.4.2')"
equals "$(number "$FIRST" started)" 100 "sessions counted"
equals "$(number "$FIRST" crashed)" 5 "sessions that crashed"
equals "$(number "$FIRST" errored)" 10 "sessions that errored and still exited"
equals "$(number "$FIRST" healthy)" 85 "sessions with nothing wrong"
# The gate's headline number, to a tolerance of one point. The counters
# above are exact; this is the arithmetic over them.
within "$(number "$FIRST" crash_free_rate)" 0.95 0.01 "crash-free rate"

# A crashed session is not also an errored one. Without disjoint counters
# `crashed + errored > started` becomes possible, which is a number nobody can
# interpret and everybody reports as a bug.
TOTAL=$(( $(number "$FIRST" healthy) + $(number "$FIRST" errored) \
        + $(number "$FIRST" crashed) + $(number "$FIRST" abnormal) ))
equals "$TOTAL" 100 "the four counters are disjoint and add up"

WINDOW="$(object "$FIRST" window)"
equals "$(number "$WINDOW" in_flight)" 0 "no session left in flight"
printf '%s' "$WINDOW" | grep -q 'restart' \
  || fail "the response does not say that the newest hour can move after a restart"
ok "the answer carries its own caveat (ADR 008)"

step "an hour already written does not move; only the open one can"
# An SDK cannot be asked to report a session from two hours ago, so the closed
# hour is made with the protocol's own `sessions` item — counts an SDK already
# added up, which is exactly what a client sends when it has been offline and
# is reporting what happened while it was. It needs no window at all: an
# aggregate carries no session ids, so there is nothing to deduplicate.
CLOSED_HOUR="$(date -u -d '2 hours ago' +%Y-%m-%dT%H)"
AGGREGATE="$(printf '{"attrs":{"release":"shop@1.4.2","environment":"compat-test"},"aggregates":[{"started":"%s:16:00.000000Z","exited":80,"errored":15,"crashed":5}]}' "$CLOSED_HOUR")"
printf '{"dsn":"%s"}\n{"type":"sessions","length":%d}\n%s\n' \
  "$DSN" "${#AGGREGATE}" "$AGGREGATE" |
  curl -fsS -X POST "http://127.0.0.1:$PORT/api/$PROJECT_ID/envelope/" \
    -H "X-Sentry-Auth: Sentry sentry_version=7, sentry_key=$SENTRY_KEY" \
    --data-binary @- >/dev/null || fail "the aggregate item was refused"

stop_server_cleanly
start_server

BEFORE="$(health_json -version 'shop@1.4.2')"
CLOSED_BEFORE="$(hour_object "$BEFORE" "$CLOSED_HOUR")"
[ -n "$CLOSED_BEFORE" ] || { printf '%s\n' "$BEFORE"; fail "the aggregate never reached its hour"; }
equals "$(number "$CLOSED_BEFORE" started)" 100 "sessions in the closed hour"
equals "$(number "$CLOSED_BEFORE" crashed)" 5 "crashes in the closed hour"
WRITTEN_BEFORE=$(number "$BEFORE" started)
equals "$WRITTEN_BEFORE" 200 "sessions written down so far"

# Now: more sessions that finish, and a batch that starts and never ends. Then
# the process dies without draining — a crash, a kill -9, an OOM. This is
# exactly what ADR 008 says is lost.
SECOND="$(./compat/python/run-health.sh --dsn "$DSN" --release 'shop@1.4.2' \
  --count 40 --crashes 0 --open 25)" \
  || { printf '%s\n' "$SECOND"; fail "the second batch was not delivered"; }
printf '   ..   sent: %s\n' "$SECOND"

IN_FLIGHT="$(number "$(object "$(health_json -version 'shop@1.4.2')" window)" in_flight)"
[ "$IN_FLIGHT" -ge 1 ] || fail "the 25 unfinished sessions never reached the window"
ok "$IN_FLIGHT sessions are in flight, which is what a crash would lose"

stop_server_abruptly
start_server

AFTER="$(health_json -version 'shop@1.4.2')"
CLOSED_AFTER="$(hour_object "$AFTER" "$CLOSED_HOUR")"
equals "$CLOSED_AFTER" "$CLOSED_BEFORE" "the closed hour is identical after an unclean restart"
equals "$(number "$AFTER" started)" "$WRITTEN_BEFORE" "everything already written survived"

# And the sessions that had not been written are gone, which is the trade-off
# stated as a measurement rather than as a promise.
[ "$(number "$AFTER" started)" -lt $((WRITTEN_BEFORE + 40)) ] \
  || fail "an unclean stop lost nothing; the window cannot have been in memory"
ok "the 65 sessions still in the window were lost, and only those (ADR 008)"

step "a full window sacrifices precision, never stability"
stop_server_cleanly
start_server -session-window "$SMALL_WINDOW"
ok "restarted with a window of $SMALL_WINDOW"

FULL="$(./compat/python/run-health.sh --dsn "$DSN" --release 'flood@9.9.9' --open 1000)" \
  || { printf '%s\n' "$FULL"; fail "the flood was not delivered"; }
printf '   ..   sent: %s\n' "$FULL"

# Still answering. "Did not fall over" is the claim, and the only way to check
# it is to ask the process something after the flood.
curl -fsS "$API/health" >/dev/null || fail "the server stopped answering after a thousand sessions"
ok "the server is still serving"

FLOODED="$(object "$(health_json -version 'flood@9.9.9')" window)"
equals "$(number "$FLOODED" capacity)" "$SMALL_WINDOW" "the window's ceiling"
equals "$(number "$FLOODED" in_flight)" "$SMALL_WINDOW" "the window is full and no larger"
equals "$(number "$FLOODED" evicted)" $((1000 - SMALL_WINDOW)) "sessions settled early to stay inside it"

# Said where somebody is reading the number, not only in a log. Checked before
# the restart below, because the eviction count belongs to the process that did
# the evicting: a fresh window has evicted nothing and would rightly say so.
"$BINARY" releases health -project "$PROJECT_ID" -version 'flood@9.9.9' \
  | grep -q 'window was full' \
  || fail "the terminal does not say the counts are a lower bound"
ok "and the terminal says the counts are a lower bound"

# Precision degraded, stability not: an evicted session is still counted as
# started, it simply will never report how it ended.
stop_server_cleanly
start_server -session-window "$SMALL_WINDOW"
FLOOD_HEALTH="$(health_json -version 'flood@9.9.9')"
equals "$(number "$FLOOD_HEALTH" started)" $((1000 - SMALL_WINDOW)) \
  "every evicted session was still counted"

step "a project that never switched sessions on"
DSN2="$("$BINARY" projects create -name apagado)"
CODE="$(printf '{}\n{"type":"session","length":2}\n{}\n' | curl -sS -o /dev/null -w '%{http_code}' \
  -X POST "http://127.0.0.1:$PORT/api/2/envelope/" \
  -H "X-Sentry-Auth: Sentry sentry_version=7, sentry_key=$(printf '%s' "$DSN2" | sed -n 's|.*//\([^@]*\)@.*|\1|p')" \
  --data-binary @-)"
equals "$CODE" 429 "a session sent to a project with sessions off is refused on the wire"

EMPTY="$("$BINARY" releases health -project 2 --json)"
printf '%s' "$EMPTY" | grep -q '"crash_free_rate":null' \
  || { printf '%s\n' "$EMPTY"; fail "a project with no sessions was given a crash-free rate"; }
ok "and a project with no sessions has no rate, rather than a perfect one"

stop_server_cleanly
printf '\n\033[1;32mall checks passed\033[0m\n'
