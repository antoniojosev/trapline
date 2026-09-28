#!/usr/bin/env bash
#
# The weekly digest as a gate.
#
# A weekly mail is the one feature nobody watches. It arrives on a Monday, it
# is skimmed, and its failure mode is not an error — it is a slow drift into a
# shape nobody reads, or a number that quietly means something else. So the
# checks here are about the claims the report makes rather than about whether
# it renders: that "new this week" is new *this week*, that "came back" is not
# the same issue that came back in March, that the trend compares two weeks
# that do not overlap, and that two renders of the same period are the same
# bytes.
#
# The last section is the one that matters most: the events are deleted and
# every number is asked for again. Whatever still answers is what the digest
# actually promises (ADR 001, ADR 010).
set -euo pipefail

PORT="${PORT:-9610}"
# The rule every gate in here obeys: never talk to a server this script did
# not start.
# shellcheck source=lib/port.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/port.sh"

require_free_port "$PORT" "the digest gate"

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

# The alerts scopes are asked for explicitly: `token create` defaults to
# projects:* and nothing else, which is the right default (a channel holds a
# credential for somebody else's system) and means the digest section below
# has to say so.
TOKEN="$("$BINARY" token create -db "$DB" -name digest \
  -scopes projects:read,projects:write,alerts:read,alerts:write --json \
  | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
[ -n "$TOKEN" ] || fail "no token returned"
export TRAPLINE_URL="http://127.0.0.1:$PORT"
export TRAPLINE_TOKEN="$TOKEN"

DSN="$("$BINARY" projects create -name venekambio)"
KEY="$(printf '%s' "$DSN" | sed -n 's|.*//\([^@]*\)@.*|\1|p')"
PROJECT_ID=1
ok "project created"

step "ingest"
# Two weeks of history, placed relative to now because that is what the digest
# window is relative to. The report covers the seven days ending with the hour
# before now, so "two days ago" is inside it and "nine days ago" is inside the
# week it is compared against.
THIS_WEEK="$(date -u -d '2 days ago' +%Y-%m-%dT%H:%M:%SZ)"
LAST_WEEK="$(date -u -d '9 days ago' +%Y-%m-%dT%H:%M:%SZ)"
LONG_AGO="$(date -u -d '20 days ago' +%Y-%m-%dT%H:%M:%SZ)"

# send posts `count` copies of one event in a single envelope.
send() { # kind message timestamp count
  local kind="$1" message="$2" when="$3" count="$4"
  local event length item body code

  event="$(printf '{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","timestamp":"%s",' "$when")"
  event+='"platform":"python","level":"error","release":"app@1.0.0","environment":"production",'
  event+="$(printf '"exception":{"values":[{"type":"%s","value":"%s","stacktrace":{"frames":[' "$kind" "$message")"
  event+='{"filename":"app/views.py","abs_path":"/srv/app/views.py","function":"checkout","lineno":42,"in_app":true}'
  event+=']}}]}}'

  # Byte length, not character length: the item header declares bytes and the
  # accents in these payloads are two of them each.
  length="$(printf '%s' "$event" | wc -c)"
  item="$(printf '{"type":"event","length":%d}\n%s' "$length" "$event")"$'\n'
  body="{}"$'\n'
  for _ in $(seq 1 "$count"); do body+="$item"; done

  code="$(printf '%s' "$body" | curl -sS -o /dev/null -w '%{http_code}' \
    -X POST "http://127.0.0.1:$PORT/api/$PROJECT_ID/envelope/" \
    -H "X-Sentry-Auth: Sentry sentry_version=7, sentry_key=$KEY" \
    --data-binary @-)"
  [ "$code" = 200 ] || fail "ingesting $count× $kind answered $code"
}

# Born long before the reported week, and still loud inside it: this is the
# issue that must NOT be called new. Without it the query could be "every
# issue with events this week" and still pass.
send OldError "esto lleva semanas fallando" "$LONG_AGO" 5
send OldError "esto lleva semanas fallando" "$THIS_WEEK" 30

# Born inside the reported week.
send PaymentError "no se pudo procesar la conexión" "$THIS_WEEK" 60
send TypeError    "undefined is not a function"     "$THIS_WEEK" 10

# The week before, so the trend has something to compare against.
send OldError "esto lleva semanas fallando" "$LAST_WEEK" 25
ok "105 events this week, 25 the week before, across three issues"

step "preview"
PREVIEW="$("$BINARY" digest preview)"
printf '%s\n' "$PREVIEW" | sed 's/^/        /'

expect "$PREVIEW" "trapline — the week of" "the report names the week it covers"
expect "$PREVIEW" "venekambio" "and the project"
expect "$PREVIEW" "100 events, up 300% from 25 last week" "the trend compares two weeks that do not overlap"
expect "$PREVIEW" "2 new issues, 0 regressions" "only the issues born this week are new"
expect "$PREVIEW" "New this week" "the new-issue section is there"
expect "$PREVIEW" "PaymentError" "with the issue that appeared"
NEW_SECTION="$(printf '%s' "$PREVIEW" | sed -n '/New this week/,/^$/p')"
refute "$NEW_SECTION" "OldError" "and an issue that has been failing for weeks is not new"
expect "$PREVIEW" "OldError" "though it is still among the loudest"
expect "$PREVIEW" "Loudest" "the loudest list is there"
expect "$PREVIEW" "$ORIGIN/projects/1/issues/" "every line links to the issue"

step "the structured form"
# ADR 006: the fourth client of this API is an agent, and an agent asked "how
# many new issues" should get a number rather than a paragraph to parse.
JSON="$("$BINARY" digest preview --json)"
expect "$JSON" '"new_issues":2' "--json carries the counts as fields"
expect "$JSON" '"events":100' "including the week's total"
expect "$JSON" '"previous_events":25' "and the week before's"
expect "$JSON" '"weekday":"Monday","hour":9,"timezone":"UTC"' "and the schedule it would go out on"

step "determinism"
# The property the golden fixtures pin in a unit test, checked here against a
# real database: a report about a fixed moment is the same bytes every time.
AT="$(date -u -d '1 hour ago' +%Y-%m-%dT%H:00:00Z)"
FIRST="$("$BINARY" digest preview -at "$AT")"
SECOND="$("$BINARY" digest preview -at "$AT")"
[ "$FIRST" = "$SECOND" ] || {
  diff <(printf '%s\n' "$FIRST") <(printf '%s\n' "$SECOND") || true
  fail "two renders of the same period differ"
}
ok "the same period renders identically twice"

step "regressions"
# "What came back" is one of the three lines this report exists to write, and
# it is the one that cannot be answered by counting events.
ISSUE_ID="$("$BINARY" issues list -project "$PROJECT_ID" -q PaymentError --json \
  | sed -n 's/.*"issues":\[{"id":\([0-9]*\).*/\1/p')"
[ -n "$ISSUE_ID" ] || fail "could not find the issue to resolve"

"$BINARY" issues resolve -project "$PROJECT_ID" -issue "$ISSUE_ID" >/dev/null \
  || fail "could not resolve the issue"
send PaymentError "no se pudo procesar la conexión" "$THIS_WEEK" 1

BACK="$("$BINARY" digest preview)"
expect "$BACK" "Came back" "a reopened issue has its own section"
expect "$BACK" "1 regression" "and is counted as one"
expect "$(printf '%s' "$BACK" | sed -n '/Came back/,/^$/p')" "PaymentError" \
  "the section names the issue that came back"

step "schedule"
expect "$("$BINARY" digest schedule)" "Monday at 09:00 UTC" "the default is Monday morning"
expect "$("$BINARY" digest schedule -day thursday -hour 17)" "Thursday at 17:00 UTC" "it can be changed"
expect "$("$BINARY" digest schedule)" "Thursday at 17:00 UTC" "and the change is stored"

# Through the API as well, because the panel and the CLI are the same client
# of the same endpoint (ADR 006).
REST="$(curl -fsS -H "Authorization: Bearer $TOKEN" "$API/system/settings/digest")"
expect "$REST" '"weekday":"Thursday"' "the API reports the same schedule"

BAD="$(curl -sS -o /dev/null -w '%{http_code}' -X PUT "$API/system/settings/digest" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -H 'X-Trapline-Request: 1' -d '{"weekday":"caturday","hour":9}')"
[ "$BAD" = 400 ] || fail "a day that does not exist answered $BAD, want 400"
ok "a day that does not exist is refused, not guessed"

"$BINARY" digest schedule -day monday -hour 9 >/dev/null

step "the job that does not exist"
# ADR 014, stated as an observable fact: with no channel asking for a digest,
# there is no digest job — not an idle one, none.
JOBS="$(curl -fsS -H "Authorization: Bearer $TOKEN" "$API/system/jobs")"
expect "$JOBS" '"name":"retention"' "retention runs, because nobody opts into it"
refute "$JOBS" '"name":"digest"' "and the digest job does not exist until a channel asks for it"

DOCTOR="$("$BINARY" doctor)"
expect "$DOCTOR" "alert channels" "doctor reports on the channels"
# The subsystem is assembled in every build since the digest was wired (ADR 035), so
# what `doctor` reports here is the truthful "there are none", not "there is
# nowhere to look". Choosing not to configure alerting is a choice, so it
# stays green.
expect "$DOCTOR" "none configured" "and is honest about there being none to check"
refute "$DOCTOR" "no notification subsystem" "without claiming the subsystem is missing, because it is not"
"$BINARY" doctor >/dev/null || fail "doctor failed on a healthy installation"

step "the job that appears when a channel asks for it"
# The other direction of ADR 014, and the cable the digest depends on: ticking the
# digest box on a channel starts the job while the operator is still looking,
# not at the next restart.
CHANNEL_ID="$("$BINARY" alerts channels add -type webhook -name weekly -digest \
  -config '{"url":"http://127.0.0.1:1/hook","secret":"a-secret-long-enough-for-the-gate"}' --json \
  | sed -n 's/.*"id":\([0-9]*\).*/\1/p' | head -1)"
[ -n "$CHANNEL_ID" ] || fail "the digest channel was not created"

for _ in $(seq 1 50); do
  JOBS="$(curl -fsS -H "Authorization: Bearer $TOKEN" "$API/system/jobs")"
  printf '%s' "$JOBS" | grep -q '"name":"digest"' && break
  sleep 0.1
done
expect "$JOBS" '"name":"digest"' "the digest job starts without a restart"

DOCTOR="$("$BINARY" doctor 2>&1 || true)"
refute "$DOCTOR" "no channel asked for it" "and doctor stops explaining an absent job that is now running"

"$BINARY" alerts channels remove -id "$CHANNEL_ID" >/dev/null || fail "the channel was not removed"
for _ in $(seq 1 50); do
  JOBS="$(curl -fsS -H "Authorization: Bearer $TOKEN" "$API/system/jobs")"
  printf '%s' "$JOBS" | grep -q '"name":"digest"' || break
  sleep 0.1
done
refute "$JOBS" '"name":"digest"' "and it goes away with the last channel that wanted it"

step "retention: the events go, the digest stays"
# The whole reason the numbers come from the hourly buckets rather than from a
# GROUP BY over events. Set the event window to nothing, sweep, and ask for the
# same report: a digest derived from events would now be blank, on precisely
# the week somebody most wants to read one (ADR 001, ADR 010).
BEFORE="$(curl -fsS -H "Authorization: Bearer $TOKEN" "$API/projects/$PROJECT_ID/issues/$ISSUE_ID")"
refute "$BEFORE" '"events":[]' "the payloads are there to begin with"

"$BINARY" config set -project "$PROJECT_ID" -retention error=0 >/dev/null \
  || fail "could not set the event window to zero"
"$BINARY" retention -db "$DB" >/dev/null || fail "the sweep failed"

AFTER_EVENTS="$(curl -fsS -H "Authorization: Bearer $TOKEN" "$API/projects/$PROJECT_ID/issues/$ISSUE_ID")"
expect "$AFTER_EVENTS" '"events":[]' "the payloads are gone"

SURVIVED="$("$BINARY" digest preview --json)"
expect "$SURVIVED" '"events":101' "the week's total survives its own events"
expect "$SURVIVED" '"new_issues":2' "and so does what appeared"
expect "$SURVIVED" '"regressed_issues":1' "and what came back"

printf '\n\033[1mdigest gate: green\033[0m\n\n'
