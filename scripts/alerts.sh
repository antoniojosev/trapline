#!/usr/bin/env bash
#
# Alerting, end to end, against five real destinations.
#
# This is the executable form of the claim alerting makes: break an application and
# be told about it, with a link to the issue, in under a minute, without
# touching anything. Everything here serves that claim or the three ways it
# fails in the real world — the far end is down, the same thing breaks four
# hundred times, and the server restarts in the middle (ADR 015).
#
# Two things are stood up beside the server:
#
#   - mailpit, in Docker, which is a real SMTP server with an API to read what
#     it received. Email is the one channel with no HTTP in it, and testing it
#     against something this project wrote would test this project's idea of
#     SMTP.
#   - compat/alerts/receiver, a Go module of its own, which imitates Telegram,
#     Slack and Discord on the routes those services actually use, and which
#     verifies the webhook signature with code written only from
#     docs/alerts/webhooks.md.
#
# Run it with PORT set to somewhere free; it takes a temporary database and
# leaves nothing behind.
set -euo pipefail

PORT="${PORT:-9603}"
RECEIVER_PORT="${RECEIVER_PORT:-$((PORT + 1))}"
SMTP_PORT="${SMTP_PORT:-$((PORT + 2))}"
MAILPIT_API_PORT="${MAILPIT_API_PORT:-$((PORT + 3))}"

# shellcheck source=lib/port.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/port.sh"

require_free_port "$PORT" "the alerts gate"
require_free_port "$RECEIVER_PORT" "the alerts gate's webhook receiver"
require_free_port "$SMTP_PORT" "the alerts gate's SMTP server"
require_free_port "$MAILPIT_API_PORT" "the alerts gate's mail API"

DOCKER_PREFIX="${DOCKER_PREFIX:-trapline-alerts}"
MAILPIT="$DOCKER_PREFIX-mailpit"
MAILPIT_IMAGE="${MAILPIT_IMAGE:-axllent/mailpit:v1.21}"

ORIGIN="${ORIGIN:-https://errors.example.test}"
WORKDIR="$(mktemp -d)"
BINARY="$PWD/trapline"
RECEIVER_BINARY="$WORKDIR/receiver"
WEBHOOK_SECRET="a-secret-long-enough-for-the-gate"
SERVER_PID=""
RECEIVER_PID=""

cleanup() {
  stop_server
  stop_receiver
  docker rm -f "$MAILPIT" >/dev/null 2>&1 || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

step() { printf '\n\033[1m== %s\033[0m\n' "$1"; }
ok()   { printf '   ok   %s\n' "$1"; }
fail() { printf '   FAIL %s\n' "$1"; [ -f "$WORKDIR/server.log" ] && tail -30 "$WORKDIR/server.log" | sed 's/^/        /'; exit 1; }

command -v docker >/dev/null 2>&1 || fail "docker is required: the SMTP channel is tested against a real mail server"

API="http://127.0.0.1:$PORT/api/v1"
RECEIVER="http://127.0.0.1:$RECEIVER_PORT"
MAIL_API="http://127.0.0.1:$MAILPIT_API_PORT"

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

# count_deliveries <channel> — how many messages one imitated service received.
count_deliveries() { curl -fsS "$RECEIVER/count?channel=$1"; }

# count_matching <channel> <substring> — deliveries to one imitated service
# whose body carries a marker. Both filters, because a rule fans out to four of
# them and "how many times was this delivered" only means something per
# destination.
count_matching() { curl -fsS "$RECEIVER/count?channel=$1&contains=$2"; }

# notifications <status> — rows of the delivery log, as JSON.
notifications() {
  if [ -n "${1:-}" ]; then
    "$BINARY" alerts log -status "$1" -limit 100 --json
  else
    "$BINARY" alerts log -limit 100 --json
  fi
}

# count_event <trigger> — how many notifications of one kind exist.
count_event() { notifications "" | grep -o "\"event\":\"$1\"" | wc -l | tr -d ' '; }

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

send_event() {
  local marker="$1" minute="${2:-1}"
  local event
  event="$(printf '{"event_id":"9ec79c33ec9942ab8353589fcb2e04d%s","timestamp":"2026-08-24T10:%02d:00Z","platform":"python","level":"error","release":"app@1.0.0","environment":"production","exception":{"values":[{"type":"%s","value":"boom","stacktrace":{"frames":[{"filename":"app/views.py","function":"checkout","lineno":42,"in_app":true}]}}]}}' \
    "$((minute % 10))" "$minute" "$marker")"
  local code
  code="$(printf '{}\n{"type":"event","length":%d}\n%s\n' "${#event}" "$event" \
    | curl -sS -o /dev/null -w '%{http_code}' \
        -X POST "http://127.0.0.1:$PORT/api/1/envelope/" \
        -H "X-Sentry-Auth: Sentry sentry_version=7, sentry_key=$KEY" \
        --data-binary @-)"
  [ "$code" = 200 ] || fail "ingest of $marker answered $code"
}

step "build"
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BINARY" ./cmd/trapline
(cd compat/alerts/receiver && CGO_ENABLED=0 go build -trimpath -o "$RECEIVER_BINARY" .)
ok "server and receiver built"

step "the five destinations"
docker rm -f "$MAILPIT" >/dev/null 2>&1 || true
docker run -d --name "$MAILPIT" \
  -p "127.0.0.1:$SMTP_PORT:1025" -p "127.0.0.1:$MAILPIT_API_PORT:8025" \
  "$MAILPIT_IMAGE" >/dev/null
for _ in $(seq 1 100); do
  if curl -fsS "$MAIL_API/api/v1/messages" >/dev/null 2>&1; then break; fi
  sleep 0.2
done
curl -fsS "$MAIL_API/api/v1/messages" >/dev/null || fail "mailpit never became healthy"
start_receiver
ok "mailpit on $SMTP_PORT, receiver on $RECEIVER_PORT"

step "an installation"
start_server
curl -fsS -X POST "$API/setup" \
  -H 'Content-Type: application/json' -H 'X-Trapline-Request: 1' \
  -d '{"username":"antonio","password":"una contraseña larga y buena"}' >/dev/null \
  || fail "setup failed"

# The alerts scopes are asked for explicitly. `token create` defaults to
# projects:* and nothing else, which is the right default — a channel holds a
# credential for somebody else's system — and it means a token minted before
# this build cannot read where an installation sends its notifications.
TOKEN="$("$BINARY" token create -db "$WORKDIR/trapline.db" -name alerts \
  -scopes projects:read,projects:write,alerts:read,alerts:write --json \
  | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
[ -n "$TOKEN" ] || fail "no token returned"

export TRAPLINE_URL="http://127.0.0.1:$PORT"
export TRAPLINE_TOKEN="$TOKEN"

DSN="$("$BINARY" projects create -name venekambio)"
KEY="$(printf '%s' "$DSN" | sed -n 's|.*//\([^@]*\)@.*|\1|p')"
[ -n "$KEY" ] || fail "no DSN key"
ok "a project and a token"

step "the notifier does not exist until a channel does"
# ADR 005, made checkable: a subsystem nobody switched on has no goroutine, no
# timer and no row in the jobs endpoint.
JOBS="$(curl -fsS "$API/system/jobs" -H "Authorization: Bearer $TOKEN")"
if printf '%s' "$JOBS" | grep -q '"name":"notifier"'; then
  fail "the notifier is running with no channel configured"
fi
ok "no notifier job before the first channel"

step "five channels"
add_channel() {
  "$BINARY" alerts channels add -type "$1" -name "$2" -config "$3" --json \
    | sed -n 's/.*"id":\([0-9]*\).*/\1/p' | head -1
}

TELEGRAM_ID="$(add_channel telegram tg \
  "{\"bot_token\":\"123:ABC\",\"chat_id\":\"-1001\",\"base_url\":\"$RECEIVER\"}")"
SLACK_ID="$(add_channel slack slack \
  "{\"url\":\"https://hooks.slack.com/services/T000/B000/xxxx\",\"base_url\":\"$RECEIVER\"}")"
DISCORD_ID="$(add_channel discord discord \
  "{\"url\":\"https://discord.com/api/webhooks/1/xxxx\",\"base_url\":\"$RECEIVER\"}")"
WEBHOOK_ID="$(add_channel webhook hook \
  "{\"url\":\"$RECEIVER/hook\",\"secret\":\"$WEBHOOK_SECRET\"}")"
EMAIL_ID="$(add_channel email mail \
  "{\"host\":\"127.0.0.1\",\"port\":$SMTP_PORT,\"from\":\"trapline@example.test\",\"to\":[\"ops@example.test\"]}")"

for id in "$TELEGRAM_ID" "$SLACK_ID" "$DISCORD_ID" "$WEBHOOK_ID" "$EMAIL_ID"; do
  [ -n "$id" ] || fail "a channel was not created"
done
ALL_CHANNELS="$TELEGRAM_ID,$SLACK_ID,$DISCORD_ID,$WEBHOOK_ID,$EMAIL_ID"

# The credential must not come back out through the API it went in through.
if "$BINARY" alerts channels list --json | grep -q "$WEBHOOK_SECRET"; then
  fail "the listing echoes the signing secret"
fi
ok "telegram, slack, discord, webhook and email"

step "the notifier now exists"
if ! wait_for 5 bash -c "curl -fsS '$API/system/jobs' -H 'Authorization: Bearer $TOKEN' | grep -q '\"name\":\"notifier\"'"; then
  fail "the notifier job never started after a channel was configured"
fi
ok "started without a restart, while the operator was still looking"

step "a rule, and something breaking"
"$BINARY" alerts rules add -name "anything new" -trigger '{"kind":"new_issue"}' \
  -channels "$ALL_CHANNELS" -silence 3600 >/dev/null || fail "the rule was not created"

STARTED="$(date +%s)"
send_event ValueError 1

# Under five seconds, in all five channels. The clock starts at the event.
if ! wait_for 15 bash -c "[ \"\$(curl -fsS '$RECEIVER/count?channel=telegram')\" -ge 1 ] &&
                          [ \"\$(curl -fsS '$RECEIVER/count?channel=slack')\" -ge 1 ] &&
                          [ \"\$(curl -fsS '$RECEIVER/count?channel=discord')\" -ge 1 ] &&
                          [ \"\$(curl -fsS '$RECEIVER/count?channel=webhook')\" -ge 1 ] &&
                          [ \"\$(curl -fsS '$MAIL_API/api/v1/messages' | grep -o 'ValueError' | head -1)\" = ValueError ]"; then
  printf '   deliveries: %s\n' "$(curl -fsS "$RECEIVER/deliveries")"
  printf '   log: %s\n' "$(notifications '')"
  fail "the five channels did not all receive the alert"
fi
ELAPSED=$(( $(date +%s) - STARTED ))
[ "$ELAPSED" -le 5 ] || fail "the alert took ${ELAPSED}s, and the claim is under five"
ok "delivered to five channels in ${ELAPSED}s"

step "the link, and the signature"
DELIVERIES="$(curl -fsS "$RECEIVER/deliveries")"
printf '%s' "$DELIVERIES" | grep -q "/projects/1/issues/1" \
  || fail "the notification carries no link to the issue"
printf '%s' "$DELIVERIES" | grep -q '"signed":true' \
  || fail "the webhook signature did not verify against an independent implementation"
[ "$(curl -fsS "$RECEIVER/bad")" = 0 ] || fail "the receiver saw a signature that did not verify"
# Only the signed webhook carries the delivery header — Telegram, Slack and
# Discord have their own idea of a request and no place to put one — so the
# assertion is that the channel which needs an idempotency key got a real one.
if ! printf '%s' "$DELIVERIES" | grep -qE '"delivery_id":"[0-9a-f]{32}"'; then
  fail "no delivery carried X-Trapline-Delivery, so a receiver cannot deduplicate"
fi
ok "a link to the issue, a verified HMAC, and a delivery id"

step "silence: ten events, one notification"
# The rule above fires once per issue and would say nothing about silencing.
# error_rate fires on the project over and over, which is exactly the subject a
# silence window exists to quieten — and it is counted BEFORE the rate limiter,
# so a flood cannot silence the alert about itself (ADR 005).
"$BINARY" alerts rules add -name "the whole thing is on fire" \
  -trigger '{"kind":"error_rate","window_s":60,"min_events_per_min":3}' \
  -channels "$WEBHOOK_ID" -silence 3600 >/dev/null || fail "the rate rule was not created"

for minute in $(seq 10 19); do
  send_event RateError "$minute"
  sleep 1.1
done

RATE_COUNT="$(count_event error_rate)"
[ "$RATE_COUNT" = 1 ] || fail "ten events over the threshold produced $RATE_COUNT notifications, want 1"
ok "the silence window collapsed a flood into one message"

step "the far end is down"
stop_receiver
BEFORE_DOWN="$(count_event new_issue)"
send_event DownError 30

# The row is written whatever the far end is doing, and the attempt fails.
if ! wait_for 15 bash -c "[ \"\$('$BINARY' alerts log -status failed -limit 100 --json | grep -o '\"status\":\"failed\"' | wc -l)\" -ge 1 ]"; then
  printf '   log: %s\n' "$(notifications '')"
  fail "a notification for an unreachable endpoint was neither queued nor attempted"
fi
FAILED_JSON="$(notifications failed)"
printf '%s' "$FAILED_JSON" | grep -q '"attempts":[1-9]' \
  || fail "the failed row records no attempt"
printf '%s' "$FAILED_JSON" | grep -q '"last_error":"' \
  || fail "the failed row does not say why, which is the only thing that makes it fixable"
ok "queued, attempted, and the reason recorded"

step "the far end comes back"
start_receiver
# No manual retry: the backoff is 30 s, and what is being checked is that the
# thing recovers on its own. An operator who has to press a button has an
# alerting system that goes quiet whenever they are asleep.
if ! wait_for 90 bash -c "[ \"\$('$BINARY' alerts log -status failed -limit 100 --json | grep -o '\"status\":\"failed\"' | wc -l)\" = 0 ]"; then
  printf '   log: %s\n' "$(notifications '')"
  fail "the queued notification never recovered after the receiver came back"
fi
AFTER_UP="$(count_event new_issue)"
[ "$AFTER_UP" -gt "$BEFORE_DOWN" ] || fail "no new notification survived the outage"
ok "retried on its own and delivered"

step "the server restarts in the middle"
# The one this whole design exists for. The receiver is down, so the
# notification is written and cannot be delivered; the server is then killed
# outright — no drain, no shutdown hook, nothing in memory gets a chance to be
# flushed.
stop_receiver
send_event RestartError 40

kill -9 "$SERVER_PID" 2>/dev/null || true
wait "$SERVER_PID" 2>/dev/null || true
SERVER_PID=""

start_server
# A fresh receiver process, so its counters start at zero and the number below
# is the deliveries of this notification and nothing else.
start_receiver
if ! wait_for 90 bash -c "[ \"\$(curl -fsS '$RECEIVER/count?channel=webhook&contains=RestartError')\" -ge 1 ]"; then
  printf '   log: %s\n' "$(notifications '')"
  fail "a notification queued before a hard restart was lost"
fi
sleep 3
for channel in telegram slack discord webhook; do
  DELIVERED="$(count_matching "$channel" RestartError)"
  [ "$DELIVERED" = 1 ] \
    || fail "the $channel notification was delivered $DELIVERED times across a restart, want exactly 1"
done
ok "nothing lost, nothing doubled"

step "backup says what it is not copying"
BACKUP_OUTPUT="$("$BINARY" backup -db "$WORKDIR/trapline.db" -to "$WORKDIR/backup.db")"
printf '%s' "$BACKUP_OUTPUT" | grep -q 'trapline.db.key' \
  || fail "backup did not name the key file it leaves behind"
printf '%s' "$BACKUP_OUTPUT" | grep -qi 'NOT inside this backup' \
  || fail "backup did not warn that the key is not in the copy"
"$BINARY" backup -db "$WORKDIR/trapline.db" -to "$WORKDIR/backup2.db" --json \
  | grep -q '"warning"' || fail "--json carries no warning about the key"
ok "the warning names the file, in text and in JSON"

step "a restore without the key"
# The failure this warning exists for, run for real: the database arrives, the
# channels are listed, and nothing can decrypt them.
cp "$WORKDIR/backup.db" "$WORKDIR/restored.db"
# A backup is made with VACUUM INTO, and the copy it produces is a plain
# database rather than a WAL one — that is the point of it, an archive with no
# sidecar files. It becomes a WAL database the first time trapline opens it, so
# it is opened once here; otherwise `doctor` would report the journal mode and
# stop, and the check this step exists for would never run.
"$BINARY" retention -db "$WORKDIR/restored.db" >/dev/null || fail "the restored database does not open"

if "$BINARY" doctor -db "$WORKDIR/restored.db" --quick >/dev/null 2>&1; then
  fail "doctor passed on a database whose channels cannot be decrypted"
fi
DOCTOR_OUTPUT="$("$BINARY" doctor -db "$WORKDIR/restored.db" --quick 2>&1 || true)"
if ! printf '%s' "$DOCTOR_OUTPUT" | grep -q 'secret key'; then
  printf '   doctor said: %s\n' "$DOCTOR_OUTPUT"
  fail "doctor does not name the secret key as the problem"
fi

cp "$WORKDIR/trapline.db.key" "$WORKDIR/restored.db.key"
"$BINARY" doctor -db "$WORKDIR/restored.db" --quick >/dev/null \
  || fail "doctor still fails once the key is beside the restored database"
ok "doctor names the missing key, and passes once it is there"

step "the notifier goes away with the last channel"
for id in "$TELEGRAM_ID" "$SLACK_ID" "$DISCORD_ID" "$WEBHOOK_ID" "$EMAIL_ID"; do
  "$BINARY" alerts channels remove -id "$id" >/dev/null || fail "channel $id was not removed"
done
if ! wait_for 10 bash -c "! curl -fsS '$API/system/jobs' -H 'Authorization: Bearer $TOKEN' | grep -q '\"name\":\"notifier\"'"; then
  fail "the notifier is still running with no channel left"
fi
ok "the goroutine came back"

printf '\n\033[1;32malerts: all good\033[0m\n'
