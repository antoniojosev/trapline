#!/usr/bin/env bash
#
# Cron monitoring, end to end, with a real minute of real time.
#
# This is the executable form of the claim cron monitoring makes: a backup that stops
# running produces a message, and instrumenting the backup costs one line.
# Everything here serves that claim or one of the three ways the feature
# fails in the real world — the job never starts, the job starts and hangs,
# and the job is in a timezone that is not the server's (ADR 016).
#
# The unit tests drive the clock by hand, because every case in them is about
# timing and a suite that waited a real minute per case is a suite nobody
# runs. This script does the opposite exactly once: it lets ninety seconds of
# wall clock pass, with the real scheduler running its real thirty-second
# sweep, because "the job did not run and nobody was told" is a claim about
# time passing and an injected clock cannot make it.
#
# Two things are stood up beside the server:
#
#   - compat/alerts/receiver, the same webhook receiver alerts.sh uses, so
#     that "and a notification arrived" is checked against a program that
#     verifies the signature rather than against this server's own opinion.
#   - compat/python, the official Python SDK, whose @monitor decorator is the
#     only way to test what an SDK actually puts on the wire.
#
# Run it with PORT set to somewhere free; it takes a temporary database and
# leaves nothing behind.
set -euo pipefail

PORT="${PORT:-9703}"
RECEIVER_PORT="${RECEIVER_PORT:-$((PORT + 1))}"

# shellcheck source=lib/port.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/port.sh"

require_free_port "$PORT" "the crons gate"
require_free_port "$RECEIVER_PORT" "the crons gate's webhook receiver"

WORKDIR="$(mktemp -d)"
BINARY="$PWD/trapline"
RECEIVER_BINARY="$WORKDIR/receiver"
WEBHOOK_SECRET="a-secret-long-enough-for-the-gate"
ORIGIN="http://127.0.0.1:$PORT"
SERVER_PID=""
RECEIVER_PID=""
PINGER_PID=""

# How long the one real wait lasts. Eighty seconds is the shortest window in
# which a `*/1 * * * *` monitor with a ten-second margin must have been
# declared missed: one minute for the deadline, ten seconds of margin, and
# one thirty-second sweep to notice. Ninety gives the notifier its second to
# deliver as well.
REAL_WAIT="${REAL_WAIT:-90}"

cleanup() {
  stop_pinger
  stop_server
  stop_receiver
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

step() { printf '\n\033[1m== %s\033[0m\n' "$1"; }
ok()   { printf '   ok   %s\n' "$1"; }
fail() {
  printf '   FAIL %s\n' "$1"
  [ -f "$WORKDIR/server.log" ] && tail -30 "$WORKDIR/server.log" | sed 's/^/        /'
  exit 1
}

API="http://127.0.0.1:$PORT/api/v1"
RECEIVER="http://127.0.0.1:$RECEIVER_PORT"

start_server() {
  "$BINARY" serve -addr "127.0.0.1:$PORT" -db "$WORKDIR/trapline.db" -origin "$ORIGIN" \
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

# The healthy monitor is kept healthy by an actual caller, because that is
# what a cron job running every minute is. Pinging it once and expecting it to
# still be ok ninety seconds later would be expecting the watcher not to work.
start_pinger() {
  (
    while :; do
      curl -fsS "$ORIGIN/ping/$HEALTHY_KEY" >/dev/null 2>&1 || true
      sleep 5
    done
  ) &
  PINGER_PID=$!
}

stop_pinger() {
  [ -n "$PINGER_PID" ] || return 0
  kill "$PINGER_PID" 2>/dev/null || true
  wait "$PINGER_PID" 2>/dev/null || true
  PINGER_PID=""
}

# monitor_field <slug> <field> — one field of one monitor, read through the
# CLI so the gate exercises the client an operator would use (ADR 006).
monitor_field() {
  "$BINARY" monitors cron list -project "$PROJECT_ID" --json \
    | tr '{' '\n' | grep "\"slug\":\"$1\"" | sed -n "s/.*\"$2\":\"\{0,1\}\([^,\"}]*\)\"\{0,1\}.*/\1/p" | head -1
}

# count_matching <substring> — webhook deliveries whose body carries a marker.
count_matching() { curl -fsS "$RECEIVER/count?channel=webhook&contains=$1"; }

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

step "build"
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BINARY" ./cmd/trapline
(cd compat/alerts/receiver && CGO_ENABLED=0 go build -trimpath -o "$RECEIVER_BINARY" .)
ok "server and receiver built"

step "an installation"
start_receiver
start_server
curl -fsS -X POST "$API/setup" \
  -H 'Content-Type: application/json' -H 'X-Trapline-Request: 1' \
  -d '{"username":"antonio","password":"una contraseña larga y buena"}' >/dev/null \
  || fail "setup failed"

# The monitors scopes are asked for explicitly. `token create` defaults to
# projects:* and nothing else, and a monitor carries a ping key — a credential
# that can report a backup as successful from anywhere on the internet — so it
# is not something a token minted for a dashboard should be able to read
# (ADR 016, internal/domain/token.go).
TOKEN="$("$BINARY" token create -db "$WORKDIR/trapline.db" -name crons \
  -scopes projects:read,projects:write,alerts:read,alerts:write,monitors:read,monitors:write --json \
  | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
[ -n "$TOKEN" ] || fail "no token returned"

export TRAPLINE_URL="http://127.0.0.1:$PORT"
export TRAPLINE_TOKEN="$TOKEN"

DSN="$("$BINARY" projects create -name venekambio)"
PROJECT_ID=1
KEY="$(printf '%s' "$DSN" | sed -n 's|.*//\([^@]*\)@.*|\1|p')"
[ -n "$KEY" ] || fail "no DSN key"
ok "a project and a token"

step "the watcher does not exist until a monitor does"
# ADR 005 and ADR 014, made checkable: a subsystem nobody switched on has no
# goroutine, no timer and no row in the jobs endpoint.
JOBS="$(curl -fsS "$API/system/jobs" -H "Authorization: Bearer $TOKEN")"
if printf '%s' "$JOBS" | grep -q '"name":"cron-watch"'; then
  fail "cron-watch is running with no monitor configured"
fi
ok "no cron-watch job before the first monitor"

step "somewhere to be told"
CHANNEL_ID="$("$BINARY" alerts channels add -type webhook -name ops \
  -config "{\"url\":\"$RECEIVER/hook\",\"secret\":\"$WEBHOOK_SECRET\"}" --json \
  | sed -n 's/.*"id":\([0-9]*\).*/\1/p' | head -1)"
[ -n "$CHANNEL_ID" ] || fail "the channel was not created"

for trigger in cron_missed cron_timeout cron_recovered; do
  "$BINARY" alerts rules add -name "$trigger" -trigger "{\"kind\":\"$trigger\"}" \
    -channels "$CHANNEL_ID" -silence 1 >/dev/null || fail "the $trigger rule was not created"
done
ok "a webhook channel and three monitor rules"

step "three monitors"
# A checks in on time. B never checks in. C announces a start and hangs.
#
# B's margin is ten seconds so the miss is detectable inside one real minute.
# C's deadline is deliberately far away, so what makes it fail is its runtime
# and not its schedule — otherwise the assertion could pass for the wrong
# reason.
"$BINARY" monitors cron add -project "$PROJECT_ID" -slug healthy \
  -schedule '*/1 * * * *' -margin 10 -max-runtime 3600 >/dev/null || fail "monitor A was not created"
"$BINARY" monitors cron add -project "$PROJECT_ID" -slug silent \
  -schedule '*/1 * * * *' -margin 10 -max-runtime 3600 >/dev/null || fail "monitor B was not created"
"$BINARY" monitors cron add -project "$PROJECT_ID" -slug hanging \
  -schedule '0 4 * * *' -margin 3600 -max-runtime 5 >/dev/null || fail "monitor C was not created"

HEALTHY_KEY="$(monitor_field healthy ping_key)"
SILENT_KEY="$(monitor_field silent ping_key)"
HANGING_KEY="$(monitor_field hanging ping_key)"
for key in "$HEALTHY_KEY" "$SILENT_KEY" "$HANGING_KEY"; do
  [ "${#key}" -eq 32 ] || fail "a ping key is not 32 hex characters: '$key'"
done
ok "healthy, silent and hanging"

step "the watcher now exists"
if ! wait_for 5 bash -c "curl -fsS '$API/system/jobs' -H 'Authorization: Bearer $TOKEN' | grep -q '\"name\":\"cron-watch\"'"; then
  fail "cron-watch never started after a monitor was created"
fi
ok "started without a restart, while the operator was still looking"

step "a ping is one line of text, with no credential anywhere"
# The case the whole surface exists for: a crontab entry that ends
# `&& curl -fsS .../ping/<key>`. No token, no header, no body.
PING_OUT="$(curl -fsS "$ORIGIN/ping/$HEALTHY_KEY")" || fail "the ping was refused"
[ "$PING_OUT" = "ok healthy" ] || fail "the ping said '$PING_OUT', want 'ok healthy'"
[ "$(monitor_field healthy status)" = ok ] || fail "the monitor did not go ok after its ping"
curl -fsS "$ORIGIN/ping/$HANGING_KEY/start" >/dev/null || fail "the start ping was refused"
ok "ok healthy, and a run opened on hanging"

step "a ping key that is wrong, and one that is switched off"
CODE="$(curl -sS -o /dev/null -w '%{http_code}' "$ORIGIN/ping/$(printf 'f%.0s' $(seq 32))")"
[ "$CODE" = 404 ] || fail "an unknown ping key answered $CODE, want 404"
"$BINARY" monitors cron add -project "$PROJECT_ID" -slug retired \
  -schedule '@daily' -disabled >/dev/null || fail "the disabled monitor was not created"
RETIRED_KEY="$(monitor_field retired ping_key)"
CODE="$(curl -sS -o /dev/null -w '%{http_code}' "$ORIGIN/ping/$RETIRED_KEY")"
# 410 and not 404: a script that has pinged the same key for a year has no
# other way to learn that somebody switched its monitor off.
[ "$CODE" = 410 ] || fail "a disabled monitor answered $CODE, want 410"
ok "404 for an unknown key, 410 for a disabled monitor"

step "a schedule is a wall-clock statement in a named zone"
# Ten at night in Caracas is two in the morning, the next day, in UTC. A
# monitor watched in the server's zone rather than its own would be four hours
# out and, for four hours a day, on the wrong date entirely — which is how
# "3 a.m." silently means two different things in two countries.
"$BINARY" monitors cron add -project "$PROJECT_ID" -slug caracas \
  -schedule '0 22 * * *' -timezone America/Caracas >/dev/null || fail "the Caracas monitor was not created"
"$BINARY" monitors cron add -project "$PROJECT_ID" -slug elsewhere \
  -schedule '0 22 * * *' -timezone UTC >/dev/null || fail "the UTC monitor was not created"

EXPECTED_UTC="$(python3 - <<'PY'
from datetime import datetime, timedelta, timezone
from zoneinfo import ZoneInfo

caracas = ZoneInfo("America/Caracas")
local = datetime.now(timezone.utc).astimezone(caracas)
candidate = local.replace(hour=22, minute=0, second=0, microsecond=0)
if candidate <= local:
    candidate += timedelta(days=1)
in_utc = candidate.astimezone(timezone.utc)
print(in_utc.strftime("%Y-%m-%dT%H:%M:%S"))
print(candidate.strftime("%Y-%m-%d"))
print(in_utc.strftime("%Y-%m-%d"))
PY
)"
WANT_INSTANT="$(printf '%s' "$EXPECTED_UTC" | sed -n 1p)"
LOCAL_DAY="$(printf '%s' "$EXPECTED_UTC" | sed -n 2p)"
UTC_DAY="$(printf '%s' "$EXPECTED_UTC" | sed -n 3p)"

CARACAS_NEXT="$(monitor_field caracas next_expected_at)"
ELSEWHERE_NEXT="$(monitor_field elsewhere next_expected_at)"
case "$CARACAS_NEXT" in
  "$WANT_INSTANT"*) ;;
  *) fail "the Caracas deadline is $CARACAS_NEXT, want $WANT_INSTANT (22:00 -04:00)" ;;
esac
[ "$LOCAL_DAY" != "$UTC_DAY" ] || fail "the fixture no longer crosses a day boundary; pick another hour"
[ "$CARACAS_NEXT" != "$ELSEWHERE_NEXT" ] \
  || fail "the same expression in two zones produced the same deadline, so the zone is being ignored"
ok "22:00 in Caracas is ${WANT_INSTANT}Z — the next day in UTC"

step "an SDK declares a monitor with its first check-in"
# The other entrance (ADR 016): the real Python SDK's @monitor decorator,
# unchanged, with the documented monitor_config. Check-ins are opt-in like
# every other category, so the project has to accept them first (ADR 005).
"$BINARY" config set -project "$PROJECT_ID" -categories error,check_in >/dev/null \
  || fail "check-ins could not be enabled"
if ! ./compat/python/run-crons.sh -dsn "$DSN" -api "$TRAPLINE_URL" \
      -token "$TOKEN" -project "$PROJECT_ID"; then
  fail "the official Python SDK could not declare a cron monitor"
fi
ok "compat-nightly-backup created from monitor_config, margins converted from minutes"

step "ninety seconds of real time"
# The one real wait in this gate, and the only claim in the feature that an
# injected clock cannot make: time passing with nobody checking in. The unit
# tests cover every state transition in microseconds; this covers the fact
# that the scheduler, the sweep and the notifier are actually wired to each
# other in the binary that ships.
start_pinger
printf '   waiting %ss for the sweep to notice (this is the only real wait)\n' "$REAL_WAIT"
sleep "$REAL_WAIT"
stop_pinger

step "the silent monitor was missed, and somebody was told"
[ "$(monitor_field silent status)" = missed ] \
  || fail "silent is '$(monitor_field silent status)' after ${REAL_WAIT}s, want missed"
if ! wait_for 10 bash -c "[ \"\$(curl -fsS '$RECEIVER/count?channel=webhook&contains=cron_missed')\" -ge 1 ]"; then
  printf '   deliveries: %s\n' "$(curl -fsS "$RECEIVER/deliveries")"
  fail "the missed monitor produced no notification"
fi
# The message has to name the monitor. "Monitor 2 is missed" is not something
# anybody can act on.
[ "$(count_matching silent)" -ge 1 ] || fail "the notification does not name the monitor"
ok "missed, and a signed webhook naming it arrived"

step "the hanging run timed out"
[ "$(monitor_field hanging status)" = timeout ] \
  || fail "hanging is '$(monitor_field hanging status)', want timeout"
[ "$(count_matching cron_timeout)" -ge 1 ] || fail "the timeout produced no notification"
ok "a start with no finish is a timeout, not a miss"

step "the monitor that checked in was left alone"
[ "$(monitor_field healthy status)" = ok ] \
  || fail "healthy is '$(monitor_field healthy status)', want ok"
[ "$(count_matching healthy)" -eq 0 ] || fail "a healthy monitor produced a notification"
ok "no false alarm"

step "a miss is reported once, not once per sweep"
MISSED_COUNT="$(count_matching cron_missed)"
[ "$MISSED_COUNT" -le 2 ] \
  || fail "the sweep reported the same miss $MISSED_COUNT times; it ran three times in ${REAL_WAIT}s"
ok "$MISSED_COUNT notification(s) for a monitor that has been silent for ${REAL_WAIT}s"

step "and it recovers"
RECOVER_OUT="$(curl -fsS "$ORIGIN/ping/$SILENT_KEY")" || fail "the recovery ping was refused"
[ "$RECOVER_OUT" = "ok silent" ] || fail "the recovery ping said '$RECOVER_OUT'"
[ "$(monitor_field silent status)" = ok ] || fail "silent did not come back to ok"
if ! wait_for 10 bash -c "[ \"\$(curl -fsS '$RECEIVER/count?channel=webhook&contains=cron_recovered')\" -ge 1 ]"; then
  fail "a monitor that came back told nobody"
fi
ok "recovered, and said so"

step "the history survives a restart"
# The whole point of ADR 015 applied here: none of this lives in memory.
BEFORE="$("$BINARY" monitors cron checkins -project "$PROJECT_ID" \
  -id "$(monitor_field silent id)" --json)"
stop_server
start_server
AFTER="$("$BINARY" monitors cron checkins -project "$PROJECT_ID" \
  -id "$(monitor_field silent id)" --json)"
[ "$BEFORE" = "$AFTER" ] || fail "the check-in history changed across a restart"
[ "$(monitor_field silent status)" = ok ] || fail "the monitor's status did not survive a restart"
ok "check-ins and status are on disk"

printf '\n\033[1mcron monitoring works: a job that stops running produces a message.\033[0m\n\n'
