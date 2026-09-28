#!/usr/bin/env bash
#
# The dashboard as a gate: hourly aggregates, full-text search, and the one
# property that decides whether either was worth building — that the numbers
# survive the events they were counted from.
#
# Two hundred synthetic events across three hours, two releases and two
# environments, with timestamps chosen here rather than taken from the clock.
# Then every question the dashboard asks, answered through the public API and
# the CLI, and finally a retention sweep with the event window set to nothing.
# Whatever is still answerable afterwards is what the product actually
# promises (ADR 010, ADR 011).
set -euo pipefail

PORT="${PORT:-9300}"
# The rule every gate in here obeys: never talk to a server this script did
# not start.
# shellcheck source=lib/port.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/port.sh"

require_free_port "$PORT" "the statistics gate"

ORIGIN="${ORIGIN:-https://errors.example.test}"
WORKDIR="$(mktemp -d)"
BINARY="$PWD/trapline"
DB="$WORKDIR/trapline.db"
SERVER_PID=""

cleanup() {
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null || true
  [ -n "$SERVER_PID" ] && wait "$SERVER_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

step() { printf '\n\033[1m== %s\033[0m\n' "$1"; }
ok()   { printf '   ok   %s\n' "$1"; }
fail() { printf '   FAIL %s\n' "$1"; exit 1; }

# expect greps a body for a literal and says what was missing when it is not
# there. The bodies are compact JSON from encoding/json, so field order is
# fixed and a literal is a precise assertion rather than a hopeful one.
expect() { # body needle description
  printf '%s' "$1" | grep -qF -- "$2" || { printf '%s\n' "$1"; fail "$3"; }
  ok "$3"
}

refute() { # body needle description
  printf '%s' "$1" | grep -qF -- "$2" && { printf '%s\n' "$1"; fail "$3"; }
  ok "$3"
}

step "build"
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BINARY" ./cmd/trapline

step "boot"
"$BINARY" serve -addr "127.0.0.1:$PORT" -db "$DB" -origin "$ORIGIN" \
  >"$WORKDIR/server.log" 2>&1 &
SERVER_PID=$!

API="http://127.0.0.1:$PORT/api/v1"
for _ in $(seq 1 50); do
  curl -fsS "$API/health" >/dev/null 2>&1 && break
  sleep 0.1
done
curl -fsS "$API/health" >/dev/null || { cat "$WORKDIR/server.log"; fail "server never became healthy"; }
ok "healthy on port $PORT"

curl -fsS -X POST "$API/setup" \
  -H 'Content-Type: application/json' -H 'X-Trapline-Request: 1' \
  -d '{"username":"antonio","password":"una contraseña larga y buena"}' >/dev/null \
  || fail "setup failed"

TOKEN="$("$BINARY" token create -db "$DB" -name stats --json | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
[ -n "$TOKEN" ] || fail "no token returned"
export TRAPLINE_URL="http://127.0.0.1:$PORT"
export TRAPLINE_TOKEN="$TOKEN"

DSN="$("$BINARY" projects create -name venekambio)"
KEY="$(printf '%s' "$DSN" | sed -n 's|.*//\([^@]*\)@.*|\1|p')"
PROJECT_ID=1
ok "project created"

step "ingest"
# Three hours, named relative to now so the default 24-hour window covers them
# too: a gate that only ever asked with explicit ends would never exercise the
# window a dashboard actually opens with.
H0="$(date -u -d '3 hours ago' +%Y-%m-%dT%H)"
H1="$(date -u -d '2 hours ago' +%Y-%m-%dT%H)"
H2="$(date -u -d '1 hour ago' +%Y-%m-%dT%H)"
FROM="$(date -u -d '4 hours ago' +%Y-%m-%dT%H)"
TO="$(date -u +%Y-%m-%dT%H)"

# send builds one envelope carrying `count` copies of the same event and posts
# it in a single request.
#
# One request per group rather than per event, because the thing under test is
# the aggregate, not the transport: two hundred separate curls would spend
# most of this gate's runtime forking processes. The envelope protocol carries
# a batch natively, which is also what a busy SDK sends.
send() { # kind value hour release environment count
  local kind="$1" value="$2" hour="$3" release="$4" environment="$5" count="$6"
  local event length body item

  event="$(printf '{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","timestamp":"%s:15:00Z",' "$hour")"
  event+="$(printf '"platform":"python","level":"error","release":"%s","environment":"%s",' "$release" "$environment")"
  event+="$(printf '"message":"la función de pago falló","tags":{"server":"web-01"},')"
  event+="$(printf '"exception":{"values":[{"type":"%s","value":"%s","stacktrace":{"frames":[' "$kind" "$value")"
  event+='{"filename":"app/views.py","abs_path":"/srv/app/views.py","function":"checkout","lineno":42,"in_app":true}'
  event+=']}}]}}'

  # Byte length, not character length: the item header declares bytes and the
  # accents in this payload are two of them each. Getting it from the shell's
  # ${#var} would silently truncate every event and the failure would look
  # like a parser bug.
  length="$(printf '%s' "$event" | wc -c)"
  # The trailing newline is added after the substitution, not inside it:
  # $(...) strips trailing newlines, and an envelope whose payload runs
  # straight into the next item header is a 400 with a baffling message.
  item="$(printf '{"type":"event","length":%d}\n%s' "$length" "$event")"$'\n'

  body="{}"$'\n'
  for _ in $(seq 1 "$count"); do body+="$item"; done

  local code
  code="$(printf '%s' "$body" | curl -sS -o /dev/null -w '%{http_code}' \
    -X POST "http://127.0.0.1:$PORT/api/$PROJECT_ID/envelope/" \
    -H "X-Sentry-Auth: Sentry sentry_version=7, sentry_key=$KEY" \
    --data-binary @-)"
  [ "$code" = 200 ] || fail "ingesting $count× $kind answered $code"
}

# 200 events. The shape is deliberately uneven: one issue much louder than the
# rest, one release and one environment ahead of the other, and a middle hour
# quieter than the last. A flat distribution would let a query that ignored
# its range still look right.
send PaymentError  "no se pudo procesar la conexión" "$H0" "app@2.0.0" production 50
send TypeError     "undefined is not a function"     "$H0" "app@2.0.0" staging    10
send PaymentError  "no se pudo procesar la conexión" "$H1" "app@2.1.0" production 40
send KeyError      "missing key order_id"            "$H1" "app@2.1.0" staging    20
send PaymentError  "no se pudo procesar la conexión" "$H2" "app@2.1.0" production 30
send TimeoutError  "upstream took too long"          "$H2" "app@2.0.0" staging    50
ok "200 events in 3 hours, 2 releases, 2 environments"

auth() { curl -fsS -H "Authorization: Bearer $TOKEN" "$@"; }
STATS="$API/projects/$PROJECT_ID/stats"
RANGE="from=$FROM&to=$TO"

step "series"
SERIES="$(auth "$STATS?$RANGE")"
expect "$SERIES" '"total":200' "every event reached a bucket"
expect "$SERIES" "{\"hour\":\"$H0\",\"count\":60" "the first hour holds 60"
expect "$SERIES" "{\"hour\":\"$H1\",\"count\":60" "the second hour holds 60"
expect "$SERIES" "{\"hour\":\"$H2\",\"count\":80" "the third hour holds 80"
expect "$SERIES" '"by_level":{"error":200}' "the legend totals by level"

# The hours between the range's start and the first event have to be present
# and empty, or a chart draws a straight line across a quiet night.
expect "$SERIES" "{\"hour\":\"$FROM\",\"count\":0,\"by_level\":{}}" "a quiet hour is a zero, not a gap"

# And the window a dashboard opens with, which names no ends at all.
DEFAULT_SERIES="$(auth "$STATS")"
expect "$DEFAULT_SERIES" '"total":200' "the default 24-hour window finds the same events"
expect "$DEFAULT_SERIES" '"hours":24' "the default window is a day"

step "top issues"
TOP="$(auth "$STATS/top?$RANGE")"
FIRST_COUNT="$(printf '%s' "$TOP" | sed -n 's/.*"issues":\[{[^}]*"count":\([0-9]*\).*/\1/p')"
[ "$FIRST_COUNT" = 120 ] || { printf '%s\n' "$TOP"; fail "the loudest issue counted $FIRST_COUNT, want 120"; }
ok "the loudest issue is first, with the events inside the range"
expect "$TOP" '"count":50' "the second is the 50-event issue"
expect "$TOP" '"count":10' "and the quietest is still listed"

# Half the range: the ranking has to change, or it is reading issues.times and
# the buckets are decoration.
NARROW="$(auth "$STATS/top?from=$H2&to=$TO")"
NARROW_COUNT="$(printf '%s' "$NARROW" | sed -n 's/.*"issues":\[{[^}]*"count":\([0-9]*\).*/\1/p')"
[ "$NARROW_COUNT" = 50 ] || { printf '%s\n' "$NARROW"; fail "the last hour's leader counted $NARROW_COUNT, want 50"; }
ok "narrowing the range changes who is loudest"

step "breakdown"
RELEASES="$(auth "$STATS/breakdown?by=release&$RANGE")"
expect "$RELEASES" '{"value":"app@2.0.0","count":110}' "releases are totalled, biggest first"
expect "$RELEASES" '{"value":"app@2.1.0","count":90}' "and the other release is there too"

ENVIRONMENTS="$(auth "$STATS/breakdown?by=environment&$RANGE")"
expect "$ENVIRONMENTS" '{"value":"production","count":120}' "environments are totalled"
expect "$ENVIRONMENTS" '{"value":"staging","count":80}' "including the quieter one"

BAD="$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" \
  "$STATS/breakdown?by=constellation&$RANGE")"
[ "$BAD" = 400 ] || fail "a breakdown by nonsense answered $BAD, want 400"
ok "a dimension that does not exist is refused, not guessed"

step "one issue's series"
LOUDEST="$(printf '%s' "$TOP" | sed -n 's/.*"issues":\[{"id":\([0-9]*\).*/\1/p')"
[ -n "$LOUDEST" ] || fail "could not read the loudest issue's id"
ISSUE_STATS="$(auth "$API/projects/$PROJECT_ID/issues/$LOUDEST/stats?range=24h")"
expect "$ISSUE_STATS" '"total":120' "the issue's own chart adds up"
expect "$ISSUE_STATS" '"range":"24h"' "and reports the window it used"
expect "$ISSUE_STATS" "{\"hour\":\"$H1\",\"count\":40}" "with the right count in the right hour"

step "search"
search() { "$BINARY" issues list -project "$PROJECT_ID" -q "$1" --json; }

expect "$(search Paym)" '"title":"PaymentError' "a prefix finds the issue before the word is finished"

# The property the whole tokenizer choice exists for, and the reason it is a
# gate and not a unit test: exception names are compound words, so the part
# somebody remembers is usually in the middle of the token. `Error` has to find
# `PaymentError`, `Timeout` has to find `TimeoutError`.
expect "$(search Error)" '"title":"PaymentError' "an infix finds it: Error matches PaymentError"
expect "$(search Timeout)" '"title":"TimeoutError' "and Timeout matches TimeoutError"
expect "$(search ymentErr)" '"title":"PaymentError' "even a fragment that straddles the middle"

# The other direction, which is what stops "matches everything" from passing
# as "matches the middle".
refute "$(search Kafka)" '"id":' "a substring nothing contains finds nothing"
refute "$(search 'PaymentError inexistente')" '"id":' "two terms are ANDed: one that misses finds nothing"
refute "$(search 'nada de esto existe')" '"id":' "a query nothing matches finds nothing"
refute "$(search '---')" '"id":' "a run of punctuation nothing contains finds nothing"
expect "$(search '---')" '"unresolved":4' "and still labels the filter buttons"

expect "$(search conexión)" '"title":"PaymentError' "an accent finds it"
expect "$(search conexion)" '"title":"PaymentError' "and so does the same word without one"
expect "$(search 'PaymentError procesar')" '"title":"PaymentError' "two terms both matching finds it"

# Too short for the index to look up. The one thing this must not do is answer
# "no results" to a query it never ran, so it is an error with a non-zero exit
# and a message that names the minimum (ADR 011).
if SHORT="$("$BINARY" issues list -project "$PROJECT_ID" -q ab --json 2>&1)"; then
  printf '%s\n' "$SHORT"
  fail "a two-character search succeeded; it cannot have been run against the index"
fi
printf '%s' "$SHORT" | grep -qF '3 characters' \
  || { printf '%s\n' "$SHORT"; fail "a refused search does not say how long a term has to be"; }
ok "a term shorter than the index window is refused, not answered empty"

# And a short term beside a usable one is dropped rather than refusing the lot,
# because "de" and "la" are how people type.
expect "$(search 'la conexion')" '"title":"PaymentError' "a short term beside a usable one is dropped, not fatal"

step "cli parity"
# The same four answers through the CLI, because a feature that reaches the
# CLI late is a feature the agent-first story does not have (ADR 006).
CLI_SERIES="$("$BINARY" stats -project "$PROJECT_ID" -from "$FROM" -to "$TO" --json)"
expect "$CLI_SERIES" '"total":200' "stats --json returns the series"
CLI_TEXT="$("$BINARY" stats -project "$PROJECT_ID" -from "$FROM" -to "$TO")"
expect "$CLI_TEXT" "$H2	80	error=80" "and its text form is a readable table"
expect "$("$BINARY" stats -project "$PROJECT_ID" -top 3 -from "$FROM" -to "$TO")" '120×' "stats -top ranks issues"
expect "$("$BINARY" stats -project "$PROJECT_ID" -by release -from "$FROM" -to "$TO")" 'app@2.0.0	110' "stats -by breaks down"
expect "$("$BINARY" stats -project "$PROJECT_ID" -issue "$LOUDEST" -range 24h --json)" '"total":120' "stats -issue charts one issue"
# The bulk form the panel draws a list with. One request for a whole page,
# because one per row is twenty-five round trips for a decoration — and it is
# here rather than only in the UI because ADR 006 has no endpoints the panel
# gets to itself.
CLI_SPARK="$("$BINARY" stats -project "$PROJECT_ID" -issues "$LOUDEST" -range 24h --json)"
expect "$CLI_SPARK" '"issue_id":'"$LOUDEST" "stats -issues draws a page of sparklines"
expect "$CLI_SPARK" '"total":120' "and each one carries its own total"
# An id nobody owns still gets a row: a client draws one sparkline per row it
# is showing, and a missing entry would shift every chart onto the wrong row.
expect "$("$BINARY" stats -project "$PROJECT_ID" -issues "$LOUDEST,99999" --json)" '"issue_id":99999' "an unknown id gets an empty row, not a gap"

step "retention: the events go, the history stays"
# The whole reason the buckets are written during ingest instead of derived on
# read. Set the event window to nothing, sweep, and ask the same questions
# again: a GROUP BY over events would now answer zero, which is exactly the
# day a dashboard is most needed (ADR 010).
# First, that there is something to lose. Without this the whole section would
# still pass against a store that had somehow ended up empty, which is the one
# way it could go green while proving nothing.
BEFORE="$(auth "$API/projects/$PROJECT_ID/issues/$LOUDEST")"
refute "$BEFORE" '"events":[]' "the payloads are there to begin with"

"$BINARY" config set -project "$PROJECT_ID" -retention error=0 >/dev/null \
  || fail "could not set the event window to zero"
"$BINARY" config show -project "$PROJECT_ID" | grep -q 'error .*keeps nothing' \
  || { "$BINARY" config show -project "$PROJECT_ID"; fail "zero days does not read back as keeping nothing"; }
ok "the event window is set to nothing"

SWEPT="$("$BINARY" retention -db "$DB" --json)"
printf '   ..   sweep reported %s\n' "$SWEPT"

# The assertion is the outcome, not who produced it. The server sweeps on its
# own schedule too, so insisting that *this* sweep be the one that deleted the
# rows would make the gate depend on which of two processes got there first —
# and a gate that fails for that reason teaches nobody anything. What has to be
# true is that the payloads are gone and the counts are not.
DETAIL="$(auth "$API/projects/$PROJECT_ID/issues/$LOUDEST")"
printf '%s' "$DETAIL" | grep -qF '"events":[]' || {
  printf '%s\n' "$SWEPT"
  "$BINARY" config show -project "$PROJECT_ID"
  fail "the events outlived a keep-window of nothing"
}
ok "the payloads are gone"

AFTER="$(auth "$STATS?$RANGE")"
expect "$AFTER" '"total":200' "the series still reports every event"
expect "$AFTER" "{\"hour\":\"$H0\",\"count\":60" "hour by hour, unchanged"
AFTER_TOP="$(auth "$STATS/top?$RANGE")"
expect "$AFTER_TOP" '"count":120' "the loudest issue is still the loudest"
expect "$(auth "$STATS/breakdown?by=release&$RANGE")" '{"value":"app@2.0.0","count":110}' \
  "the release breakdown survived"
expect "$(auth "$API/projects/$PROJECT_ID/issues/$LOUDEST/stats?range=24h")" '"total":120' \
  "and so did the issue's own chart"

# Search still works, because it indexes the issue and never the events.
expect "$(search Paym)" '"title":"PaymentError' "search still finds it with no events left"

step "retention: the aggregates have a window of their own"
# The other half of the same claim. Independent means independent in both
# directions: setting the aggregates to nothing has to clear them without
# anything else being asked.
"$BINARY" config set -project "$PROJECT_ID" -retention aggregates=0 >/dev/null \
  || fail "aggregates is not a category a keep-window can be set for"
"$BINARY" retention -db "$DB" --json >/dev/null || fail "the second sweep failed"

EMPTIED="$(auth "$STATS?$RANGE")"
expect "$EMPTIED" '"total":0' "the buckets are gone once their own window says so"
expect "$EMPTIED" '"hours":' "and the endpoint still answers with a whole axis"

printf '\n\033[1;32mall checks passed\033[0m\n'
