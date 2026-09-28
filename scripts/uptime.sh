#!/usr/bin/env bash
#
# Uptime monitoring, end to end, against a container that is switched off and
# back on.
#
# This is the executable form of what uptime monitoring claims: point the server at
# something, be told when it stops answering — once, not on every failed
# request — and be told again when it comes back. And the other half, which is
# the part that makes the feature safe to ship at all: a URL typed by a user
# makes *this server* connect somewhere, so the addresses it will visit are
# decided by a guard rather than by whoever wrote the URL (ADR 016).
#
# What is stood up beside the server:
#
#   - an nginx container, which is the thing being watched. A real server on a
#     real socket: `docker stop` is the outage, and it is a far better model of
#     one than a handler that returns 500.
#   - compat/alerts/receiver, the same webhook receiver `alerts.sh` uses, so
#     the notification is verified by something that is not this codebase
#     pretending to deliver.
#
# Two servers are started, one after the other, and the second one is the point
# of the exercise. The permissive server runs with -uptime-allow-private,
# because the target is a container on loopback and nothing else about the
# feature could be tested otherwise; the strict one runs without it, and is
# what proves the flag is load-bearing rather than decorative.
#
# Run it with PORT set to somewhere free; it takes a temporary database and
# leaves nothing behind.
set -euo pipefail

PORT="${PORT:-9713}"
STRICT_PORT="${STRICT_PORT:-$((PORT + 1))}"
RECEIVER_PORT="${RECEIVER_PORT:-$((PORT + 2))}"
TARGET_PORT="${TARGET_PORT:-$((PORT + 3))}"

# shellcheck source=lib/port.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/port.sh"

require_free_port "$PORT" "the uptime gate"
require_free_port "$STRICT_PORT" "the uptime gate's strict server"
require_free_port "$RECEIVER_PORT" "the uptime gate's webhook receiver"
require_free_port "$TARGET_PORT" "the uptime gate's target container"

DOCKER_PREFIX="${DOCKER_PREFIX:-trapline-uptime}"
TARGET="$DOCKER_PREFIX-target"
TARGET_IMAGE="${TARGET_IMAGE:-nginx:1.27-alpine}"

ORIGIN="${ORIGIN:-https://errors.example.test}"
WORKDIR="$(mktemp -d)"
BINARY="$PWD/trapline"
RECEIVER_BINARY="$WORKDIR/receiver"
WEBHOOK_SECRET="a-secret-long-enough-for-the-gate"
SERVER_PID=""
STRICT_PID=""
RECEIVER_PID=""

cleanup() {
  stop_server
  stop_strict
  stop_receiver
  docker rm -f "$TARGET" >/dev/null 2>&1 || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

step() { printf '\n\033[1m== %s\033[0m\n' "$1"; }
ok()   { printf '   ok   %s\n' "$1"; }
fail() {
  printf '   FAIL %s\n' "$1"
  [ -f "$WORKDIR/server.log" ] && tail -40 "$WORKDIR/server.log" | sed 's/^/        /'
  exit 1
}

command -v docker >/dev/null 2>&1 \
  || fail "docker is required: the target of a check has to be a real server that can be switched off"

API="http://127.0.0.1:$PORT/api/v1"
RECEIVER="http://127.0.0.1:$RECEIVER_PORT"

start_server() {
  # -uptime-allow-private, because the target is a container on loopback.
  # Everything this server checks still needs allow_private on the monitor
  # itself, which is what the refusals below demonstrate.
  "$BINARY" serve -addr "127.0.0.1:$PORT" -db "$WORKDIR/trapline.db" -origin "$ORIGIN" \
    -uptime-allow-private \
    >>"$WORKDIR/server.log" 2>&1 &
  SERVER_PID=$!
  for _ in $(seq 1 80); do
    if curl -fsS "$API/health" >/dev/null 2>&1; then return 0; fi
    sleep 0.1
  done
  fail "server never became healthy"
}

stop_server() {
  [ -n "$SERVER_PID" ] || return 0
  kill "$SERVER_PID" 2>/dev/null || true
  wait "$SERVER_PID" 2>/dev/null || true
  SERVER_PID=""
}

# The strict server: a second installation, its own database, started without
# the flag. It exists for one assertion — that a monitor asking for a private
# target is still refused — and is stopped immediately after.
start_strict() {
  "$BINARY" serve -addr "127.0.0.1:$STRICT_PORT" -db "$WORKDIR/strict.db" -origin "$ORIGIN" \
    >>"$WORKDIR/strict.log" 2>&1 &
  STRICT_PID=$!
  for _ in $(seq 1 80); do
    if curl -fsS "http://127.0.0.1:$STRICT_PORT/api/v1/health" >/dev/null 2>&1; then return 0; fi
    sleep 0.1
  done
  fail "the strict server never became healthy"
}

stop_strict() {
  [ -n "$STRICT_PID" ] || return 0
  kill "$STRICT_PID" 2>/dev/null || true
  wait "$STRICT_PID" 2>/dev/null || true
  STRICT_PID=""
}

start_receiver() {
  "$RECEIVER_BINARY" -addr "127.0.0.1:$RECEIVER_PORT" -secret "$WEBHOOK_SECRET" \
    >>"$WORKDIR/receiver.log" 2>&1 &
  RECEIVER_PID=$!
  for _ in $(seq 1 50); do
    if curl -fsS "$RECEIVER/health" >/dev/null 2>&1; then return 0; fi
    sleep 0.1
  done
  fail "the receiver never became healthy"
}

stop_receiver() {
  [ -n "$RECEIVER_PID" ] || return 0
  kill "$RECEIVER_PID" 2>/dev/null || true
  wait "$RECEIVER_PID" 2>/dev/null || true
  RECEIVER_PID=""
}

# wait_for <seconds> <command...> — polls until the command succeeds.
wait_for() {
  local deadline=$1; shift
  local waited=0
  while [ "$waited" -lt "$((deadline * 10))" ]; do
    if "$@"; then return 0; fi
    sleep 0.1
    waited=$((waited + 1))
  done
  return 1
}

# create_monitor <name> <url> [extra flags…] — returns the HTTP status and
# writes the body to $WORKDIR/last.json, so a refusal can be read as well as
# counted. The CLI is deliberately not used here: what is under test is the
# status code, and the CLI turns one into an exit code (the CLI's own path is
# checked further down and in internal/adapters/cli).
create_monitor_status() {
  local project="$1" body="$2"
  curl -sS -o "$WORKDIR/last.json" -w '%{http_code}' \
    -X POST "$API/projects/$project/monitors/uptime" \
    -H 'Content-Type: application/json' \
    -H "Authorization: Bearer $TOKEN" \
    -H 'X-Trapline-Request: 1' \
    -d "$body"
}

monitor_status() {
  "$BINARY" monitors uptime show -id "$1" --json | sed -n 's/.*"status":"\([a-z]*\)".*/\1/p'
}

monitor_failures() {
  "$BINARY" monitors uptime show -id "$1" --json \
    | sed -n 's/.*"consecutive_failures":\([0-9]*\).*/\1/p'
}

count_event() {
  "$BINARY" alerts log -limit 100 --json | grep -o "\"event\":\"$1\"" | wc -l | tr -d ' '
}

step "build"
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BINARY" ./cmd/trapline
(cd compat/alerts/receiver && CGO_ENABLED=0 go build -trimpath -o "$RECEIVER_BINARY" .)
ok "server and receiver built"

step "something to watch"
docker rm -f "$TARGET" >/dev/null 2>&1 || true
docker run -d --name "$TARGET" -p "127.0.0.1:$TARGET_PORT:80" "$TARGET_IMAGE" >/dev/null
for _ in $(seq 1 100); do
  if curl -fsS "http://127.0.0.1:$TARGET_PORT/" >/dev/null 2>&1; then break; fi
  sleep 0.2
done
curl -fsS "http://127.0.0.1:$TARGET_PORT/" >/dev/null || fail "the target container never answered"
start_receiver
ok "nginx on $TARGET_PORT, receiver on $RECEIVER_PORT"

step "an installation"
start_server
curl -fsS -X POST "$API/setup" \
  -H 'Content-Type: application/json' -H 'X-Trapline-Request: 1' \
  -d '{"username":"antonio","password":"una contraseña larga y buena"}' >/dev/null \
  || fail "setup failed"

# monitors:* is asked for explicitly. `token create` defaults to projects:*,
# and it is the right default: monitors:write is the permission to make this
# installation issue outbound requests to an address of the caller's choosing.
TOKEN="$("$BINARY" token create -db "$WORKDIR/trapline.db" -name uptime \
  -scopes projects:read,projects:write,alerts:read,alerts:write,monitors:read,monitors:write --json \
  | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
[ -n "$TOKEN" ] || fail "no token returned"

export TRAPLINE_URL="http://127.0.0.1:$PORT"
export TRAPLINE_TOKEN="$TOKEN"

"$BINARY" projects create -name venekambio >/dev/null
ok "a project and a token"

step "the uptime job does not exist until a monitor does"
# ADR 005, made checkable: a subsystem nobody switched on has no goroutine, no
# timer and no row in the jobs endpoint.
JOBS="$(curl -fsS "$API/system/jobs" -H "Authorization: Bearer $TOKEN")"
if printf '%s' "$JOBS" | grep -q '"name":"uptime"'; then
  fail "the uptime job is running with no monitor configured"
fi
ok "no uptime job before the first monitor"

step "the guard refuses what it should"
# http://127.0.0.1:1 — the canonical case. This server *does* have
# -uptime-allow-private, and the monitor does not ask for it, so it is refused
# all the same: both halves are required, and this is the half the operator
# holds.
CODE="$(create_monitor_status 1 '{"name":"loopback","url":"http://127.0.0.1:1/"}')"
[ "$CODE" = 422 ] || fail "a monitor pointed at 127.0.0.1:1 answered $CODE, want 422"
grep -q 'loopback' "$WORKDIR/last.json" \
  || fail "the refusal does not name the category: $(cat "$WORKDIR/last.json")"
grep -q 'allow_private' "$WORKDIR/last.json" \
  || fail "the refusal does not say what would have to change: $(cat "$WORKDIR/last.json")"
ok "127.0.0.1:1 refused with 422 and a message that says why"

CODE="$(create_monitor_status 1 '{"name":"internal","url":"http://10.1.2.3/health"}')"
[ "$CODE" = 422 ] || fail "a monitor pointed at 10.1.2.3 answered $CODE, want 422"
grep -q 'private' "$WORKDIR/last.json" || fail "the refusal does not name the category"
ok "a private address refused, with this server's flag already on"

CODE="$(create_monitor_status 1 '{"name":"metadata","url":"http://169.254.169.254/latest/meta-data/"}')"
[ "$CODE" = 422 ] || fail "the cloud metadata endpoint answered $CODE, want 422"
grep -q 'link-local' "$WORKDIR/last.json" || fail "the refusal does not name the category"
ok "the cloud metadata endpoint refused"

step "the server's flag is the other half"
# The same request, on an installation started without -uptime-allow-private,
# by a monitor that asks for it. If this passed, the flag would be decorative.
start_strict
curl -fsS -X POST "http://127.0.0.1:$STRICT_PORT/api/v1/setup" \
  -H 'Content-Type: application/json' -H 'X-Trapline-Request: 1' \
  -d '{"username":"antonio","password":"una contraseña larga y buena"}' >/dev/null \
  || fail "setup of the strict server failed"
STRICT_TOKEN="$("$BINARY" token create -db "$WORKDIR/strict.db" -name uptime \
  -scopes projects:read,projects:write,monitors:read,monitors:write --json \
  | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
TRAPLINE_URL="http://127.0.0.1:$STRICT_PORT" TRAPLINE_TOKEN="$STRICT_TOKEN" \
  "$BINARY" projects create -name venekambio >/dev/null

STRICT_CODE="$(curl -sS -o "$WORKDIR/strict.json" -w '%{http_code}' \
  -X POST "http://127.0.0.1:$STRICT_PORT/api/v1/projects/1/monitors/uptime" \
  -H 'Content-Type: application/json' -H "Authorization: Bearer $STRICT_TOKEN" \
  -H 'X-Trapline-Request: 1' \
  -d "{\"name\":\"target\",\"url\":\"http://127.0.0.1:$TARGET_PORT/\",\"allow_private\":true}")"
[ "$STRICT_CODE" = 422 ] \
  || fail "a monitor with allow_private was accepted by a server without the flag: $STRICT_CODE"
grep -q 'uptime-allow-private' "$WORKDIR/strict.json" \
  || fail "the refusal does not name the missing flag: $(cat "$WORKDIR/strict.json")"
stop_strict
ok "allow_private alone buys nothing; the message names the flag that is missing"

step "an interval below the floor"
CODE="$(create_monitor_status 1 \
  "{\"name\":\"eager\",\"url\":\"http://127.0.0.1:$TARGET_PORT/\",\"allow_private\":true,\"interval_s\":10}")"
[ "$CODE" = 400 ] || fail "an interval of 10s answered $CODE, want 400"
grep -q 'interval_s must be between' "$WORKDIR/last.json" \
  || fail "the refusal does not say what the floor is"
ok "10s refused: every check is traffic somebody else pays for"

step "a channel and a rule, so a transition is heard"
CHANNEL_ID="$("$BINARY" alerts channels add -type webhook -name hook \
  -config "{\"url\":\"$RECEIVER/hook\",\"secret\":\"$WEBHOOK_SECRET\"}" --json \
  | sed -n 's/.*"id":\([0-9]*\).*/\1/p' | head -1)"
[ -n "$CHANNEL_ID" ] || fail "the channel was not created"
"$BINARY" alerts rules add -name "anything down" -trigger '{"kind":"uptime_down"}' \
  -channels "$CHANNEL_ID" -silence 5 >/dev/null || fail "the down rule was not created"
"$BINARY" alerts rules add -name "anything back" -trigger '{"kind":"uptime_recovered"}' \
  -channels "$CHANNEL_ID" -silence 5 >/dev/null || fail "the recovered rule was not created"
ok "uptime_down and uptime_recovered are rules like any other"

step "the target is up"
# 30s is the floor, and the gate runs at the floor so the waits below are as
# short as the product allows.
MONITOR_JSON="$("$BINARY" monitors uptime add -project 1 -name target \
  -target "http://127.0.0.1:$TARGET_PORT/" -interval 30 -timeout 5 \
  -contains "Welcome to nginx" -allow-private -public --json)"
MONITOR_ID="$(printf '%s' "$MONITOR_JSON" | sed -n 's/.*"id":\([0-9]*\).*/\1/p' | head -1)"
[ -n "$MONITOR_ID" ] || fail "the monitor was not created: $MONITOR_JSON"

if ! wait_for 20 bash -c "[ \"\$($BINARY monitors uptime show -id $MONITOR_ID --json | sed -n 's/.*\"status\":\"\\([a-z]*\\)\".*/\\1/p')\" = up ]"; then
  fail "a target that is answering never went up (status: $(monitor_status "$MONITOR_ID"))"
fi
ok "up, and the first check ran immediately rather than an interval later"

step "the uptime job exists now"
if ! wait_for 10 bash -c "curl -fsS '$API/system/jobs' -H 'Authorization: Bearer $TOKEN' | grep -q '\"name\":\"uptime\"'"; then
  fail "the uptime job never started after a monitor was configured"
fi
ok "started without a restart, while the operator was still looking"

step "docker stop: one failure is not an outage"
docker stop "$TARGET" >/dev/null
# Waited on the failure counter rather than on the clock. A fixed sleep would
# be a race against a thirty-second cadence this script does not control: too
# short and no check has run, too long and the second one has, and the
# assertion would mean whichever of the two the machine happened to produce.
if ! wait_for 60 bash -c "[ \"\$($BINARY monitors uptime show -id $MONITOR_ID --json | sed -n 's/.*\"consecutive_failures\":\([0-9]*\).*/\1/p')\" = 1 ]"; then
  fail "the first check after the outage never failed (failures: $(monitor_failures "$MONITOR_ID"))"
fi
STATUS="$(monitor_status "$MONITOR_ID")"
[ "$STATUS" = up ] || fail "a single failed check declared an outage (status: $STATUS)"
[ "$(count_event uptime_down)" = 0 ] || fail "a single failed check sent a notification"
ok "still up after one failure: a dropped packet is not an incident"

step "the second failure is the outage, and it is delivered"
if ! wait_for 60 bash -c "[ \"\$($BINARY monitors uptime show -id $MONITOR_ID --json | sed -n 's/.*\"status\":\"\\([a-z]*\\)\".*/\\1/p')\" = down ]"; then
  fail "two consecutive failures did not declare an outage (status: $(monitor_status "$MONITOR_ID"))"
fi
if ! wait_for 20 bash -c "[ \"\$(curl -fsS '$RECEIVER/count?channel=webhook')\" -ge 1 ]"; then
  printf '   log: %s\n' "$("$BINARY" alerts log -limit 20 --json)"
  fail "the outage was recorded and nobody was told"
fi
DELIVERIES="$(curl -fsS "$RECEIVER/deliveries")"
printf '%s' "$DELIVERIES" | grep -q 'uptime_down' \
  || fail "the delivered notification is not an uptime_down"
printf '%s' "$DELIVERIES" | grep -q '"signed":true' \
  || fail "the webhook signature did not verify against an independent implementation"
printf '%s' "$DELIVERIES" | grep -q "/projects/1/monitors/uptime/$MONITOR_ID" \
  || fail "the notification carries no link to the monitor"
# The family is in the link and in the payload because the two kinds of
# monitor number their ids from separate tables: without it, this link and a
# cron monitor's are the same URL pointing at two different things, and a
# receiver keying on monitor_id mixes them (ADR 037).
# The receiver keeps the body as a JSON string, so its quotes arrive escaped;
# matching on the pair rather than on literal quotes is what makes this assert
# the field and not the receiver's encoding.
printf '%s' "$DELIVERIES" | grep -qE 'monitor_kind[^,]*uptime' \
  || fail "the payload does not say which family of monitor it is about: $DELIVERIES"
ok "down after two failures, delivered and signed"

step "docker start: back up, and said so"
docker start "$TARGET" >/dev/null
for _ in $(seq 1 100); do
  if curl -fsS "http://127.0.0.1:$TARGET_PORT/" >/dev/null 2>&1; then break; fi
  sleep 0.2
done

if ! wait_for 60 bash -c "[ \"\$($BINARY monitors uptime show -id $MONITOR_ID --json | sed -n 's/.*\"status\":\"\\([a-z]*\\)\".*/\\1/p')\" = up ]"; then
  fail "the target came back and the monitor did not (status: $(monitor_status "$MONITOR_ID"))"
fi
if ! wait_for 20 bash -c "[ \"\$(curl -fsS '$RECEIVER/count?channel=webhook&contains=uptime_recovered')\" -ge 1 ]"; then
  printf '   log: %s\n' "$("$BINARY" alerts log -limit 20 --json)"
  fail "the recovery was not delivered"
fi
ok "one success is enough to come back, and the recovery was delivered"

step "the history and the daily roll-up"
RESULTS="$("$BINARY" monitors uptime results -id "$MONITOR_ID" --json)"
printf '%s' "$RESULTS" | grep -q '"ok":false' || fail "the history kept no failed check"
printf '%s' "$RESULTS" | grep -q '"ok":true' || fail "the history kept no successful check"
printf '%s' "$RESULTS" | grep -q '"error":"' || fail "a failed check recorded no reason"

DAILY="$("$BINARY" monitors uptime daily -id "$MONITOR_ID" --json)"
printf '%s' "$DAILY" | grep -q '"failures":[1-9]' \
  || fail "the daily roll-up did not count the failures: $DAILY"
printf '%s' "$DAILY" | grep -q '"checks":[1-9]' \
  || fail "the daily roll-up counted no checks: $DAILY"
ok "the checks are kept, and the day they belong to is already rolled up (the status page reads this)"

step "an expected substring that is not there counts as a failure"
# The difference between "the load balancer answers" and "the application
# works": nginx is up and answering 200, and this monitor still fails.
WRONG_ID="$("$BINARY" monitors uptime add -project 1 -name substring \
  -target "http://127.0.0.1:$TARGET_PORT/" -interval 30 -timeout 5 \
  -contains "esta cadena no aparece en la página de nginx" -allow-private --json \
  | sed -n 's/.*"id":\([0-9]*\).*/\1/p' | head -1)"
[ -n "$WRONG_ID" ] || fail "the substring monitor was not created"

if ! wait_for 20 bash -c "[ \"\$($BINARY monitors uptime results -id $WRONG_ID --json | grep -c '\"ok\":false')\" -ge 1 ]"; then
  fail "a check whose body does not carry the expected substring passed"
fi
SUBSTRING_RESULT="$("$BINARY" monitors uptime results -id "$WRONG_ID" --json)"
printf '%s' "$SUBSTRING_RESULT" | grep -q '"status_code":200' \
  || fail "the failing check did not record the 200 it actually received: $SUBSTRING_RESULT"
printf '%s' "$SUBSTRING_RESULT" | grep -q 'does not contain' \
  || fail "the failure does not say the body was the problem: $SUBSTRING_RESULT"
ok "a 200 from the wrong page is still a failure, and the reason says so"

step "the public status page"
# ADR 017, end to end and over real checks: the monitor above has already been
# up, down and up again, and the daily roll-up behind these figures is the one
# the step before verified. Everything here is read with plain curl and no
# credential of any kind, because that is the contract of the page.
STATUS_URL="http://127.0.0.1:$PORT/status/venekambio"

# Before the project publishes anything, there is no page. Not 403 and not an
# empty page: a project that keeps its status private must be indistinguishable
# from one that does not exist.
CODE="$(curl -sS -o /dev/null -w '%{http_code}' "$STATUS_URL")"
[ "$CODE" = 404 ] || fail "an unpublished project answered $CODE, want 404"
ok "no page until the project publishes one"

# A second monitor that nobody marked public. It is the name of an internal
# service, and the page is read by anybody with the link.
PRIVATE_ID="$("$BINARY" monitors uptime add -project 1 -name esto-es-privado \
  -target "http://127.0.0.1:$TARGET_PORT/" -interval 30 -timeout 5 \
  -allow-private --json | sed -n 's/.*"id":\([0-9]*\).*/\1/p' | head -1)"
[ -n "$PRIVATE_ID" ] || fail "the private monitor was not created"

"$BINARY" config set -project 1 -status-page on >/dev/null \
  || fail "the status page could not be switched on"
"$BINARY" monitors status-page set -title "Estado de Venekambio" \
  -description "servicios en vivo" >/dev/null \
  || fail "the heading could not be written"

PAGE="$WORKDIR/status.html"
CODE="$(curl -sS -D "$WORKDIR/status.headers" -o "$PAGE" -w '%{http_code}' "$STATUS_URL")"
[ "$CODE" = 200 ] || fail "a published page answered $CODE"

grep -qi 'content-type: text/html' "$WORKDIR/status.headers" \
  || fail "the page is not HTML: $(cat "$WORKDIR/status.headers")"
grep -qi 'cache-control: public, max-age=30' "$WORKDIR/status.headers" \
  || fail "the page carries no cache window: $(cat "$WORKDIR/status.headers")"
grep -q 'Estado de Venekambio' "$PAGE" || fail "the configured title is not on the page"
grep -q 'servicios en vivo' "$PAGE" || fail "the configured description is not on the page"
grep -q '>target<' "$PAGE" || fail "the public monitor is not on the page"
if grep -q 'esto-es-privado' "$PAGE"; then
  fail "the page shows a monitor nobody marked public"
fi
# No script tag anywhere: the page has to work with JavaScript switched off,
# which is the whole of "no React" in ADR 017.
if grep -qi '<script' "$PAGE"; then
  fail "the status page carries JavaScript"
fi
# And the figures are real, not placeholders: this monitor has been checked,
# so at least one window has a percentage in it.
grep -q '%' "$PAGE" || fail "the page shows no uptime figure at all"
ok "200 with the public monitor, not the private one, cacheable and script-free"

# The per-address ceiling has to be high enough for a real page. Two hundred
# reads in a row is a browser during an incident plus everybody else behind the
# same office NAT, and none of them may be refused.
REFUSED=0
for _ in $(seq 1 200); do
  CODE="$(curl -sS -o /dev/null -w '%{http_code}' "$STATUS_URL")"
  [ "$CODE" = 200 ] || REFUSED=$((REFUSED + 1))
done
[ "$REFUSED" = 0 ] || fail "$REFUSED of 200 consecutive reads were refused"
ok "two hundred consecutive reads, none refused"

# Switching it off has to take effect now and not in thirty seconds: the cache
# is dropped by the write that changed it.
"$BINARY" config set -project 1 -status-page off >/dev/null \
  || fail "the status page could not be switched off"
CODE="$(curl -sS -o /dev/null -w '%{http_code}' "$STATUS_URL")"
[ "$CODE" = 404 ] || fail "an unpublished page still answers $CODE from the cache"
ok "unpublishing is immediate, not thirty seconds later"

"$BINARY" monitors uptime remove -id "$PRIVATE_ID" >/dev/null \
  || fail "the private monitor was not removed"

step "the job goes away with the last monitor"
"$BINARY" monitors uptime remove -id "$MONITOR_ID" >/dev/null || fail "the monitor was not removed"
"$BINARY" monitors uptime remove -id "$WRONG_ID" >/dev/null || fail "the monitor was not removed"
if ! wait_for 10 bash -c "! curl -fsS '$API/system/jobs' -H 'Authorization: Bearer $TOKEN' | grep -q '\"name\":\"uptime\"'"; then
  fail "the uptime job is still running with no monitor left"
fi
ok "the goroutine came back"

printf '\n\033[1;32muptime: all good\033[0m\n'
