#!/usr/bin/env bash
#
# End-to-end check of a real installation: build, boot, set up, create a
# project through the CLI, verify the DSN, back up, and measure idle memory.
#
# This is the fresh-install exit criterion as a script. Every claim the README makes about
# a fresh install is checked here, because a promise with no test is a promise
# that quietly stops being true.
set -euo pipefail

PORT="${PORT:-9000}"
# The rule every gate in here obeys: never talk to a server this script did
# not start.
# shellcheck source=lib/port.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/port.sh"

require_free_port "$PORT" "the smoke gate"

ORIGIN="${ORIGIN:-https://errors.example.test}"
WORKDIR="$(mktemp -d)"
BINARY="$PWD/trapline"
SERVER_PID=""

# Memory budgets in kilobytes. Two numbers, because there are two honest
# questions and one of them was very nearly published wrong.
#
#   MAX_IDLE_RSS_KB  at rest, which is the figure the README claims
#   MAX_PEAK_RSS_KB  during an Argon2id burst, which is transient by design
#
# The at-rest check runs after authenticating and letting the scavenge settle,
# not on a server nobody has logged into. A footprint that only holds until
# first login would be technically true and materially misleading.
#
# And the peak is measured after a *flood* of logins, not one. Measuring one
# was the same category of mistake one layer down: it answered "what does a
# login cost" when the question an attacker asks is "what do a thousand cost".
# Without the per-address limit the answer was 1.03 GB (ADR 023).
MAX_IDLE_RSS_KB="${MAX_IDLE_RSS_KB:-30720}"
MAX_PEAK_RSS_KB="${MAX_PEAK_RSS_KB:-81920}"
SETTLE_SECONDS="${SETTLE_SECONDS:-8}"

cleanup() {
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null || true
  [ -n "$SERVER_PID" ] && wait "$SERVER_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

step() { printf '\n\033[1m== %s\033[0m\n' "$1"; }
ok()   { printf '   ok   %s\n' "$1"; }
fail() { printf '   FAIL %s\n' "$1"; exit 1; }

step "build"
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BINARY" ./cmd/trapline
ok "$(du -h "$BINARY" | cut -f1) binary"

step "boot"
"$BINARY" serve -addr "127.0.0.1:$PORT" -db "$WORKDIR/trapline.db" -origin "$ORIGIN" \
  >"$WORKDIR/server.log" 2>&1 &
SERVER_PID=$!

API="http://127.0.0.1:$PORT/api/v1"
for _ in $(seq 1 50); do
  if curl -fsS "$API/health" >/dev/null 2>&1; then break; fi
  sleep 0.1
done
curl -fsS "$API/health" >/dev/null || { cat "$WORKDIR/server.log"; fail "server never became healthy"; }
ok "healthy on port $PORT"

step "panel"
PANEL="$(curl -fsS "http://127.0.0.1:$PORT/")"
printf '%s' "$PANEL" | grep -q 'id="root"' || fail "the panel is not being served"
ok "served from the binary, no separate static host"
curl -fsS -o /dev/null "http://127.0.0.1:$PORT/assets/panel.js" || fail "the panel's script is missing"
curl -fsS -o /dev/null "http://127.0.0.1:$PORT/assets/panel.css" || fail "the panel's stylesheet is missing"
ok "assets embedded"
# A reload on a client-side route must not 404.
curl -fsS "http://127.0.0.1:$PORT/some/deep/route" | grep -q 'id="root"' || fail "no SPA fallback"
ok "client-side routes fall back to the panel"
# And the panel must not have swallowed the API.
curl -fsS "$API/health" | grep -q '"status"' || fail "the panel is shadowing the API"
ok "the API is not shadowed"

step "setup"
curl -fsS "$API/setup" | grep -q '"needs_setup":true' || fail "a fresh install should need setup"
ok "reports needing setup"

curl -fsS -X POST "$API/setup" \
  -H 'Content-Type: application/json' -H 'X-Trapline-Request: 1' \
  -d '{"username":"antonio","password":"una contraseña larga y buena"}' \
  >/dev/null || fail "setup failed"
ok "first admin created"

curl -fsS "$API/setup" | grep -q '"needs_setup":false' || fail "setup did not close behind itself"
ok "setup closed itself"

step "cli token"
TOKEN="$("$BINARY" token create -db "$WORKDIR/trapline.db" -name smoke --json | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
[ -n "$TOKEN" ] || fail "no token returned"
case "$TOKEN" in ek_*) ok "token minted with the scannable prefix" ;; *) fail "token lacks its prefix: $TOKEN" ;; esac

step "cli project"
export TRAPLINE_URL="http://127.0.0.1:$PORT"
export TRAPLINE_TOKEN="$TOKEN"

DSN="$("$BINARY" projects create -name venekambio)"
ok "created: $DSN"

# The DSN is the product's entire migration story, so check its exact shape
# rather than that it merely exists. The shape is scheme://public_key@host/id,
# so the origin's host appears after the key, not as a prefix.
ORIGIN_HOST="${ORIGIN#*://}"
case "$DSN" in
  "${ORIGIN%%://*}://"*"@$ORIGIN_HOST/"*) ok "DSN uses the configured public origin" ;;
  *) fail "DSN does not match scheme://key@$ORIGIN_HOST/id: $DSN" ;;
esac

"$BINARY" projects list --json | grep -q '"dsn"' || fail "the project list has no DSN"
ok "listed through the API"

step "rotation"
NEW_DSN="$("$BINARY" keys rotate -project 1 --json | sed -n 's/.*"dsn":"\([^"]*\)".*/\1/p')"
[ "$NEW_DSN" != "$DSN" ] || fail "rotation returned the same DSN"
ok "second key issued, both live"

OLD_KEY="$(printf '%s' "$DSN" | sed -n 's|.*//\([^@]*\)@.*|\1|p')"
"$BINARY" keys revoke -key "$OLD_KEY" >/dev/null || fail "revoke failed"
"$BINARY" keys revoke -key "$OLD_KEY" >/dev/null || fail "revoke is not idempotent"
ok "old key revoked, idempotently"

step "ingest"
# An envelope in the shape a real SDK sends: protocol path, protocol auth
# header, gzipped body, a secret in the payload that must never reach storage.
PROJECT_ID=1
KEY="$(printf '%s' "$NEW_DSN" | sed -n 's|.*//\([^@]*\)@.*|\1|p')"
EVENT='{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","timestamp":"2026-08-24T10:00:00Z","platform":"python","level":"error","release":"myapp@2.3.1","environment":"production","exception":{"values":[{"type":"ValueError","value":"invalid amount 4821","stacktrace":{"frames":[{"abs_path":"/srv/app/views.py","function":"checkout","lineno":42,"in_app":true}]}}]},"tags":{"server":"web-01"},"extra":{"password":"hunter2","order_total":19.99}}'
LEN=${#EVENT}

send_event() {
  printf '{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}\n{"type":"event","length":%d}\n%s\n' "$LEN" "$EVENT" \
    | gzip \
    | curl -fsS -o /dev/null -w '%{http_code}' \
        -X POST "http://127.0.0.1:$PORT/api/$PROJECT_ID/envelope/" \
        -H "X-Sentry-Auth: Sentry sentry_version=7, sentry_client=sentry.python/2.0.0, sentry_key=$KEY" \
        -H 'Content-Encoding: gzip' \
        --data-binary @-
}

CODE="$(send_event)"
[ "$CODE" = 200 ] || fail "the ingest endpoint answered $CODE"
ok "a gzipped envelope was accepted, with no CSRF header and no cookie"

# Three more of the same error with a value that differs, so this tests
# grouping rather than counting.
for _ in 1 2 3; do send_event >/dev/null; done

ISSUES="$("$BINARY" issues list -project "$PROJECT_ID" --json)"
COUNT="$(printf '%s' "$ISSUES" | grep -o '"id"' | wc -l)"
[ "$COUNT" = 1 ] || fail "four occurrences of one error produced $COUNT issues"
ok "four occurrences grouped into one issue"

printf '%s' "$ISSUES" | grep -q '"times":4' || fail "the issue count is wrong: $ISSUES"
ok "the occurrence count is right"

printf '%s' "$ISSUES" | grep -q 'ValueError' || fail "the issue has no usable title"
ok "titled from the exception"

ISSUE_ID="$(printf '%s' "$ISSUES" | sed -n 's/.*"id":\([0-9]*\).*/\1/p' | head -1)"
DETAIL="$("$BINARY" issues show -project "$PROJECT_ID" -issue "$ISSUE_ID" --json)"

# The secret must not be in storage at all — scrubbing happens before the
# write, so this is checking the database and not a redaction on the way out.
printf '%s' "$DETAIL" | grep -q hunter2 && fail "a secret reached storage"
ok "the secret was scrubbed before being written"
printf '%s' "$DETAIL" | grep -q 'order_total' || fail "useful context was scrubbed away"
ok "the surrounding context survived"

# A transaction on a fresh project must come back with the protocol's own
# backpressure header: that is what makes a switched-off subsystem free.
LIMIT_HEADERS="$(printf '{}\n{"type":"transaction"}\n{"a":1}\n' \
  | curl -sS -D - -o /dev/null \
      -X POST "http://127.0.0.1:$PORT/api/$PROJECT_ID/envelope/" \
      -H "X-Sentry-Auth: Sentry sentry_key=$KEY" --data-binary @-)"
printf '%s' "$LIMIT_HEADERS" | grep -qi '429' || fail "a disabled category was not refused"
printf '%s' "$LIMIT_HEADERS" | grep -qi 'X-Sentry-Rate-Limits' || fail "no backpressure header"
ok "a disabled category answers with the protocol's rate-limit header"

step "config"
# Anchored to the categories line, not to "(default)" appearing anywhere in the
# output. The loose version passed on the rate-limit line while the line above
# it said the project accepted nothing and listed error among the categories
# being refused — about a project that was accepting errors perfectly well.
# A check that can be satisfied by a neighbouring line is not checking the line
# it was written for.
CONFIG="$("$BINARY" config show -project "$PROJECT_ID")"
printf '%s\n' "$CONFIG" | grep -qE '^categories +error +\(default\)$' \
  || { printf '%s\n' "$CONFIG"; fail "a fresh project does not report errors as inherited"; }
ok "a fresh project reports the minimum profile as inherited"

# And the opposite state has to look different, which is the whole reason the
# API returns null and [] rather than one shape for both.
"$BINARY" config set -project "$PROJECT_ID" -categories none >/dev/null
"$BINARY" config show -project "$PROJECT_ID" | grep -q "this project accepts nothing" \
  || fail "switching every category off looks the same as never having chosen"
ok "switching everything off is a state of its own, not an absence"

"$BINARY" config set -project "$PROJECT_ID" -categories default >/dev/null
"$BINARY" config show -project "$PROJECT_ID" | grep -qE '^categories +error +\(default\)$' \
  || fail "there is no way back to inheriting the default profile"
ok "and there is a way back to inheriting it"

# Turning a category on must take effect on the very next event, not after the
# limiter's cache expires.
"$BINARY" config set -project "$PROJECT_ID" -categories error,transaction >/dev/null
TXN_CODE="$(printf '{}\n{"type":"transaction"}\n{"a":1}\n' \
  | curl -sS -o /dev/null -w '%{http_code}' \
      -X POST "http://127.0.0.1:$PORT/api/$PROJECT_ID/envelope/" \
      -H "X-Sentry-Auth: Sentry sentry_key=$KEY" --data-binary @-)"
[ "$TXN_CODE" = 200 ] || fail "enabling a category did not take effect (got $TXN_CODE)"
ok "enabling a category takes effect on the next event"

"$BINARY" config set -project "$PROJECT_ID" -categories error >/dev/null
TXN_CODE="$(printf '{}\n{"type":"transaction"}\n{"a":1}\n' \
  | curl -sS -o /dev/null -w '%{http_code}' \
      -X POST "http://127.0.0.1:$PORT/api/$PROJECT_ID/envelope/" \
      -H "X-Sentry-Auth: Sentry sentry_key=$KEY" --data-binary @-)"
[ "$TXN_CODE" = 429 ] || fail "disabling a category did not take effect (got $TXN_CODE)"
ok "and switching it back off takes effect just as fast"

step "listing"
LIST="$("$BINARY" issues list -project "$PROJECT_ID" --json)"
printf '%s' "$LIST" | grep -q '"counts"' || fail "the listing carries no status counts"
printf '%s' "$LIST" | grep -q '"unresolved":1' || fail "the unresolved count is wrong: $LIST"
ok "the listing reports counts for the whole project"

"$BINARY" issues list -project "$PROJECT_ID" -environment production --json | grep -q '"id"' \
  || fail "filtering by environment found nothing"
ok "filtering by environment works, because it is recorded as a tag on ingest"

"$BINARY" issues list -project "$PROJECT_ID" -environment marte --json | grep -q '"issues":null\|"issues":\[\]' \
  || fail "filtering by an environment nothing uses returned issues"
ok "and an environment nothing uses returns nothing"

step "triage"
"$BINARY" issues resolve -project "$PROJECT_ID" -issue "$ISSUE_ID" >/dev/null || fail "resolve failed"
"$BINARY" issues list -project "$PROJECT_ID" -status unresolved --json | grep -q '"id"' \
  && fail "the resolved issue is still listed as unresolved"
ok "resolved"

send_event >/dev/null
"$BINARY" issues list -project "$PROJECT_ID" -status unresolved --json | grep -q '"id"' \
  || fail "a new event did not reopen the resolved issue"
ok "a new event reopened it as a regression"

step "timestamp ordering"
# Two errors in the same second, one on the second exactly and one half a
# second later. Timestamps are stored as text, so the listing's ORDER BY is a
# string comparison — and while the stored layout dropped trailing zeros from
# the fraction, "…10:00:00.5Z" sorted *before* "…10:00:00Z" because '.' is less
# than 'Z'. The same comparison drives the keyset cursor and the retention
# sweep, where it deleted events that were inside the keep-window (ADR 033).
#
# Exact seconds are what any SDK that rounds sends, so this is the ordinary
# case and not a corner of it. Checked end to end rather than only in a unit
# test because the claim is about what SQLite does with the bytes.
send_timestamped() {  # $1 exception type, $2 timestamp, $3 event id
  local event
  event="{\"event_id\":\"$3\",\"timestamp\":\"$2\",\"platform\":\"python\",\"level\":\"error\",\"exception\":{\"values\":[{\"type\":\"$1\",\"value\":\"same second\",\"stacktrace\":{\"frames\":[{\"abs_path\":\"/srv/app/clock.py\",\"function\":\"tick\",\"lineno\":7,\"in_app\":true}]}}]}}"
  printf '{"event_id":"%s"}\n{"type":"event","length":%d}\n%s\n' "$3" "${#event}" "$event" \
    | curl -fsS -o /dev/null \
        -X POST "http://127.0.0.1:$PORT/api/$PROJECT_ID/envelope/" \
        -H "X-Sentry-Auth: Sentry sentry_key=$KEY" \
        --data-binary @-
}

# Sent oldest first, so an ordering that merely preserved insertion order would
# also be wrong.
send_timestamped ExactSecondError  '2026-08-25T10:00:00Z'  'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1' \
  || fail "the exact-second event was refused"
send_timestamped HalfSecondError   '2026-08-25T10:00:00.5Z' 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa2' \
  || fail "the fractional event was refused"

# Both are newer than everything ingested above, so they are the first two rows
# of a listing ordered by last-seen descending.
ORDERED="$("$BINARY" issues list -project "$PROJECT_ID" --json \
  | grep -o '"title":"[^"]*"' | sed -n 's/"title":"\([^:"]*\).*/\1/p')"
FIRST="$(printf '%s\n' "$ORDERED" | sed -n 1p)"
SECOND="$(printf '%s\n' "$ORDERED" | sed -n 2p)"

[ "$FIRST" = "HalfSecondError" ] || {
  printf '%s\n' "$ORDERED"; fail "the newest issue is $FIRST, want HalfSecondError"; }
[ "$SECOND" = "ExactSecondError" ] || {
  printf '%s\n' "$ORDERED"; fail "the second-newest issue is $SECOND, want ExactSecondError"; }
ok "an event on an exact second and one half a second later list in the right order"

step "background jobs"
# A running server has exactly one background job in the minimum profile:
# retention, which is the only subsystem nobody opts into (ADR 014). Zero would
# mean the disk grows forever; more than one would mean something started that
# nobody switched on, which is the claim of ADR 005 failing.
JOBS="$(curl -fsS -H "Authorization: Bearer $TOKEN" "$API/system/jobs")"
printf '%s' "$JOBS" | grep -q '"name":"retention"' || fail "retention is not running: $JOBS"
[ "$(printf '%s' "$JOBS" | grep -o '"name":' | wc -l)" -eq 1 ] \
  || fail "something is running that nobody switched on: $JOBS"
printf '%s' "$JOBS" | grep -q '"last_error":null' || fail "the retention job is failing: $JOBS"
printf '%s' "$JOBS" | grep -q '"next_run":"' || fail "the job does not say when it runs next: $JOBS"
ok "one job, retention, healthy and scheduled"

step "doctor"
"$BINARY" doctor --json | grep -q '"ok":true' || {
  "$BINARY" doctor; fail "doctor reports a problem"
}
# doctor is the CLI half of the same answer: same fields, not a sentence about
# them (ADR 006).
"$BINARY" doctor --json | grep -q '"jobs":\[{"name":"retention"' \
  || fail "doctor does not report the background jobs"
"$BINARY" doctor | grep -q "job retention" || fail "doctor's text output hides the jobs"
ok "all checks pass, jobs included"

step "retention"
# The sweep is bounded by the keep-window, so nothing recent may disappear.
SWEPT="$("$BINARY" retention -db "$WORKDIR/trapline.db" --json)"
printf '%s' "$SWEPT" | grep -q '"deleted":0' || fail "the sweep deleted recent events: $SWEPT"
ok "a sweep leaves events inside the window alone"

"$BINARY" issues list -project "$PROJECT_ID" --json | grep -q '"id"' \
  || fail "the issue disappeared after a retention sweep"
ok "the issue survived"

step "backup"
"$BINARY" backup -db "$WORKDIR/trapline.db" -to "$WORKDIR/backup.db" >/dev/null || fail "backup failed"
[ -s "$WORKDIR/backup.db" ] || fail "the backup is empty"
# A backup that cannot be read back is not a backup.
"$BINARY" token list -db "$WORKDIR/backup.db" | grep -q smoke || fail "the backup does not contain the data"
ok "written and readable"
"$BINARY" backup -db "$WORKDIR/trapline.db" -to "$WORKDIR/backup.db" 2>/dev/null && fail "backup overwrote an existing file"
ok "refuses to overwrite"

step "auth rate limit"
# Every login attempt costs a 19 MiB Argon2id working set (ADR 005), so an
# unthrottled login endpoint is a memory-exhaustion primitive against a product
# whose headline is that it runs in 30 MB. Measured before this limit existed:
# 200 concurrent attempts took resident memory to 1.03 GB. That is the reason
# this step sits immediately before the memory budget rather than anywhere
# else — the figure the next step prints is the one that proves the fix.
#
# Every attempt also claims a different X-Forwarded-For. No proxy is trusted
# here, so the header must change nothing; if it did, the limiter's keys would
# be chosen by whoever is attacking it and this step would go green with no
# limit in place at all.
FLOOD="$WORKDIR/flood.codes"
: >"$FLOOD"
FLOOD_PIDS=""
for attempt in $(seq 1 40); do
  curl -s -o /dev/null -w '%{http_code}\n' \
    -X POST "$API/login" \
    -H 'Content-Type: application/json' -H 'X-Trapline-Request: 1' \
    -H "X-Forwarded-For: 203.0.113.$attempt" \
    -d '{"username":"antonio","password":"la contraseña equivocada"}' >>"$FLOOD" &
  FLOOD_PIDS="$FLOOD_PIDS $!"
done
# Waited on by pid rather than with a bare `wait`: the server is a background
# job of this script too, and waiting for every child would wait for it to exit.
for pid in $FLOOD_PIDS; do wait "$pid" || true; done

REFUSED="$(grep -c '^429$' "$FLOOD" || true)"
HASHED="$(grep -c '^401$' "$FLOOD" || true)"
SERVER_ERRORS="$(grep -c '^5' "$FLOOD" || true)"

[ "$SERVER_ERRORS" = 0 ] || { sort "$FLOOD" | uniq -c; fail "the flood produced $SERVER_ERRORS server errors"; }
[ "$REFUSED" -gt 0 ] || { sort "$FLOOD" | uniq -c; fail "40 concurrent logins were all served: there is no rate limit on auth"; }
ok "$REFUSED of 40 concurrent logins refused, $HASHED reached the hasher"

# The ceiling is per minute and setup already spent part of the window, so the
# count can only be at or below it. Well above it means the limit is not the
# thing doing the refusing.
[ "$HASHED" -le 10 ] || fail "$HASHED attempts reached Argon2id; the per-address ceiling is 10 a minute"
ok "a forged X-Forwarded-For bought no extra budget"

# And a refusal has to say when to come back, or a client retries immediately
# and the limit costs the server more than it saves.
REFUSAL_HEADERS="$(curl -sS -D - -o /dev/null \
  -X POST "$API/login" \
  -H 'Content-Type: application/json' -H 'X-Trapline-Request: 1' \
  -d '{"username":"antonio","password":"la contraseña equivocada"}')"
printf '%s' "$REFUSAL_HEADERS" | grep -qi '429' || fail "the login endpoint stopped refusing"
printf '%s' "$REFUSAL_HEADERS" | grep -qi '^retry-after:' || fail "a refused login carries no Retry-After"
ok "refusals carry Retry-After"

step "memory"
PEAK_KB="$(awk '/VmRSS/ {print $2}' "/proc/$SERVER_PID/status")"
printf '   peak after flood: %s KB (budget %s KB)\n' "$PEAK_KB" "$MAX_PEAK_RSS_KB"
[ "$PEAK_KB" -le "$MAX_PEAK_RSS_KB" ] || fail "peak memory is over budget"
ok "the Argon2id working set stayed within budget under a flood"

# Wait for the coalesced scavenge to return the hashing working set.
sleep "$SETTLE_SECONDS"
IDLE_KB="$(awk '/VmRSS/ {print $2}' "/proc/$SERVER_PID/status")"
printf '   at rest:         %s KB (budget %s KB)\n' "$IDLE_KB" "$MAX_IDLE_RSS_KB"
[ "$IDLE_KB" -le "$MAX_IDLE_RSS_KB" ] || fail "memory at rest is over budget"
ok "returns to the published footprint after authenticating"

step "shutdown"
kill -TERM "$SERVER_PID"
for _ in $(seq 1 50); do kill -0 "$SERVER_PID" 2>/dev/null || break; sleep 0.1; done
kill -0 "$SERVER_PID" 2>/dev/null && fail "the server ignored SIGTERM"
grep -q stopped "$WORKDIR/server.log" || fail "no clean shutdown in the log"
SERVER_PID=""
ok "drained and stopped cleanly"

printf '\n\033[1;32mall checks passed\033[0m\n'
