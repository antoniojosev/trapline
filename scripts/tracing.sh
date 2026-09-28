#!/usr/bin/env bash
#
# Tracing as a gate: latencies whose distribution is known before it is sent,
# sampling that decides storage and nothing else, and a downsample that has to
# leave the percentiles where it found them.
#
# The whole feature rests on one claim that is easy to get wrong and impossible
# to notice: percentiles are not composable (ADR 007). Averaging the p95 of
# sixty minutes does not give the p95 of the hour, so the windows store a
# mergeable histogram and the percentile is computed at read time (ADR 020).
# Three things follow, and each is a section below:
#
#   1. The percentile the server reports has to match the percentile of what
#      was actually sent — measured on the sending side, before serialisation,
#      because a gate that asked the server for both numbers would only be
#      checking that the server agrees with itself.
#   2. With sampling at 10%, the aggregates still have to cover 100% of what
#      arrived. Sampling decides how many waterfalls exist, never what the
#      numbers say (ADR 021).
#   3. Folding sixty minutes into their hour has to be exact. If it were not,
#      every level of downsampling would drift and the drift would depend on
#      the order the minutes happened to be folded in.
#
# The sender is the official Go SDK, unmodified, configured with a DSN and
# EnableTracing. What is under test is the shape sentry-go puts on the wire,
# which a hand-written envelope cannot check (ADR 002).
set -euo pipefail

# 9903 by default: this gate's port block starts at 9900, and its first
# three are spoken for by smoke, compat and ui-smoke.
# A gate that defaulted to a port another gate uses would be fine until the
# afternoon somebody ran two of them at once.
PORT="${PORT:-9903}"
# The rule every gate in here obeys: never talk to a server this script did
# not start.
# shellcheck source=lib/port.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/port.sh"

require_free_port "$PORT" "the tracing gate"

# The origin is the loopback address this gate serves on, and that is not
# cosmetic: an SDK derives its ingest URL from the DSN alone, which is the whole
# point of a DSN. A DSN naming errors.example.test would send five hundred
# transactions somewhere that is not this server, and the gate would fail with
# "the server counted 0 of 500" — which points nowhere near the cause.
ORIGIN="${ORIGIN:-http://127.0.0.1:$PORT}"
WORKDIR="$(mktemp -d)"
BINARY="$PWD/trapline"
SENDER="$WORKDIR/compat-tracing"
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

# number pulls a numeric JSON field out of a flat object. The bodies are
# compact JSON from encoding/json, so field order is fixed and this is a
# precise extraction rather than a hopeful one.
number() { # object key
  printf '%s' "$1" | sed -n 's/.*"'"$2"'":\([0-9.eE+-]*\).*/\1/p' | head -1
}

# object cuts one nested object out of a response, so a field that appears in
# several of them is read from the one that was meant.
object() { # body key
  printf '%s' "$1" | sed -n 's/.*"'"$2"'":{\([^}]*\)}.*/\1/p'
}

# within asserts that two numbers agree to a relative tolerance.
within() { # got want tolerance description
  local got="$1" want="$2" tolerance="$3" description="$4"
  [ -n "$got" ] && [ -n "$want" ] || fail "$description: could not read both numbers (got '$got', want '$want')"
  awk -v got="$got" -v want="$want" -v tol="$tolerance" 'BEGIN {
    if (want == 0) { exit (got == 0) ? 0 : 1 }
    error = (got - want) / want
    if (error < 0) error = -error
    exit (error <= tol) ? 0 : 1
  }' || fail "$(printf '%s: %s against %s is more than %s%% off' \
        "$description" "$got" "$want" "$(awk -v t="$tolerance" 'BEGIN{printf "%.1f", t*100}')")"
  ok "$(printf '%s: %s against an exact %s' "$description" "$got" "$want")"
}

between() { # value low high description
  local value="$1" low="$2" high="$3" description="$4"
  [ -n "$value" ] || fail "$description: no value"
  awk -v v="$value" -v lo="$low" -v hi="$high" 'BEGIN { exit (v >= lo && v <= hi) ? 0 : 1 }' \
    || fail "$description: $value is outside [$low, $high]"
  ok "$description: $value"
}

step "build"
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BINARY" ./cmd/trapline
# The sender is its own module, so the SDK it exercises is a fixture and never
# a dependency of the binary being shipped (compat/README.md).
(cd compat/tracing && go build -o "$SENDER" .)
ok "server and sender built"

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

TOKEN="$("$BINARY" token create -db "$DB" -name tracing --json | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
[ -n "$TOKEN" ] || fail "no token returned"
export TRAPLINE_URL="http://127.0.0.1:$PORT"
export TRAPLINE_TOKEN="$TOKEN"

DSN="$("$BINARY" projects create -name venekambio)"
PROJECT_ID=1
ok "project created"

# Tracing is off by default, which is the whole of ADR 005: a subsystem nobody
# switched on costs nothing. Switching it on is the first thing this gate does,
# and the last section checks that the refusal before it was a real one.
"$BINARY" config set -project "$PROJECT_ID" \
  -categories error,transaction -traces-sample-rate 1 >/dev/null \
  || fail "could not switch tracing on"
ok "transactions enabled, sampling at 1.0"

step "500 transactions of a known distribution"
SENT="$("$SENDER" -dsn "$DSN" -name 'GET /api/checkout' -count 500 -fail-every 10)" \
  || { printf '%s\n' "$SENT"; fail "the Go SDK could not deliver its transactions"; }
EXACT_P50="$(number "$SENT" p50_ms)"
EXACT_P95="$(number "$SENT" p95_ms)"
EXACT_P99="$(number "$SENT" p99_ms)"
[ -n "$EXACT_P50" ] || { printf '%s\n' "$SENT"; fail "the sender reported no percentiles"; }
printf '   ..   sent: p50 %s ms, p95 %s ms, p99 %s ms\n' "$EXACT_P50" "$EXACT_P95" "$EXACT_P99"

SERIES="$("$BINARY" transactions show -project "$PROJECT_ID" -name 'GET /api/checkout' --json)"
SUMMARY="$(object "$SERIES" summary)"
[ -n "$SUMMARY" ] || { printf '%s\n' "$SERIES"; fail "the server returned no summary"; }

COUNT="$(number "$SUMMARY" count)"
[ "$COUNT" = 500 ] || { printf '%s\n' "$SERIES"; fail "the server counted $COUNT of 500 transactions"; }
ok "every transaction reached a bucket"

FAILED="$(number "$SUMMARY" failed)"
[ "$FAILED" = 50 ] || { printf '%s\n' "$SERIES"; fail "the server counted $FAILED failures, want 50"; }
ok "the failure count is the status field, not the latency"

# The 2% is the gate; the sketch's own guarantee is 1% (ADR 020). The extra
# point of slack is for the rounding the wire format applies to a duration on
# its way through JSON, not for the estimator.
within "$(number "$SUMMARY" p50_ms)" "$EXACT_P50" 0.02 "p50 from merged sketches"
within "$(number "$SUMMARY" p95_ms)" "$EXACT_P95" 0.02 "p95 from merged sketches"
within "$(number "$SUMMARY" p99_ms)" "$EXACT_P99" 0.02 "p99 from merged sketches"

# The waterfall, whole: the root plus the child the sender attached. A
# transaction stored without its spans is a row nobody can open.
TRACE_ID="$(printf '%s' "$SERIES" | sed -n 's/.*"examples":\[{"trace_id":"\([^"]*\)".*/\1/p')"
[ -n "$TRACE_ID" ] || { printf '%s\n' "$SERIES"; fail "no stored trace came back with the series"; }
WATERFALL="$("$BINARY" traces show -project "$PROJECT_ID" -trace "$TRACE_ID" --json)"
expect "$WATERFALL" '"op":"http.server"' "the waterfall carries its root span"
expect "$WATERFALL" '"op":"db.sql"' "and the child the SDK attached"
expect "$("$BINARY" traces show -project "$PROJECT_ID" -trace "$TRACE_ID")" '█' \
  "the CLI draws the waterfall rather than listing durations"

step "sampling decides storage, never the numbers"
"$BINARY" config set -project "$PROJECT_ID" -traces-sample-rate 0.1 >/dev/null \
  || fail "could not lower the sampling rate"

SAMPLED_SENT="$("$SENDER" -dsn "$DSN" -name 'GET /health' -count 500 -fail-every 0 -seed 99)" \
  || { printf '%s\n' "$SAMPLED_SENT"; fail "the second batch was not delivered"; }
SAMPLED_P95="$(number "$SAMPLED_SENT" p95_ms)"

LIST="$("$BINARY" transactions list -project "$PROJECT_ID" --json)"
SAMPLING="$(object "$LIST" sampling)"
[ -n "$SAMPLING" ] || { printf '%s\n' "$LIST"; fail "the listing does not report its sampling"; }

RECEIVED="$(number "$SAMPLING" received)"
[ "$RECEIVED" = 1000 ] || { printf '%s\n' "$LIST"; fail "the aggregates cover $RECEIVED of 1000 transactions"; }
ok "the aggregates cover 100% of what arrived, both batches"

STORED="$(number "$SAMPLING" stored)"
# The first batch was sent at 1.0 and the second at 0.1, so about 550 traces
# are expected. The bound is on the second batch's share alone: between 2% and
# 25% of 500, which is wide because five hundred draws of a hash is a small
# sample and the property under test is "a small fraction, and not none".
SECOND_BATCH_STORED=$((STORED - 500))
between "$SECOND_BATCH_STORED" 10 125 "traces stored at a rate of 0.1, out of 500"

HEALTH="$(object "$("$BINARY" transactions show -project "$PROJECT_ID" -name 'GET /health' --json)" summary)"
HEALTH_COUNT="$(number "$HEALTH" count)"
[ "$HEALTH_COUNT" = 500 ] || fail "the sampled transaction counted $HEALTH_COUNT of 500"
ok "a sampled transaction's count is still every transaction"
within "$(number "$HEALTH" p95_ms)" "$SAMPLED_P95" 0.02 "p95 of a 10%-sampled transaction"

step "downsampling keeps the percentiles"
BEFORE="$(object "$("$BINARY" transactions show -project "$PROJECT_ID" \
  -name 'GET /api/checkout' -resolution hour --json)" summary)"
BEFORE_P50="$(number "$BEFORE" p50_ms)"
BEFORE_P95="$(number "$BEFORE" p95_ms)"
BEFORE_P99="$(number "$BEFORE" p99_ms)"

# The clock, moved forward. `-age 0` makes every closed minute eligible, which
# is what a clock two hours ahead would have produced — and it says so, rather
# than substituting time itself and testing the substitute.
FOLDED="$("$BINARY" downsample -db "$DB" -age 0 --json)"
printf '   ..   %s\n' "$FOLDED"
FOLD_COUNT="$(number "$FOLDED" folded)"
[ -n "$FOLD_COUNT" ] && [ "$FOLD_COUNT" -gt 0 ] || fail "the fold moved nothing; it cannot have run"
ok "$FOLD_COUNT minute buckets folded into their hours"

# The minutes are gone. Without this the section below would pass against a
# server that had simply not folded anything and was still reading the minutes.
MINUTES="$("$BINARY" transactions show -project "$PROJECT_ID" \
  -name 'GET /api/checkout' -resolution minute --json)"
MINUTE_TOTAL="$(number "$MINUTES" total)"
[ "$MINUTE_TOTAL" = 0 ] || { printf '%s\n' "$MINUTES"; fail "$MINUTE_TOTAL transactions survived in minute buckets"; }
ok "the folded minutes were deleted"

AFTER="$(object "$("$BINARY" transactions show -project "$PROJECT_ID" \
  -name 'GET /api/checkout' -resolution hour --json)" summary)"
[ "$(number "$AFTER" count)" = 500 ] || { printf '%s\n' "$AFTER"; fail "the fold lost transactions"; }
ok "the hour holds every transaction the minutes did"

# One percent, and it is a formality: merging sketches is exact, so these
# should be equal to the bit. The tolerance is here so a failure reads as "the
# fold changed the distribution" rather than as a float comparison nobody can
# interpret.
within "$(number "$AFTER" p50_ms)" "$BEFORE_P50" 0.01 "p50 after downsampling"
within "$(number "$AFTER" p95_ms)" "$BEFORE_P95" 0.01 "p95 after downsampling"
within "$(number "$AFTER" p99_ms)" "$BEFORE_P99" 0.01 "p99 after downsampling"

# And the same figures against what was actually sent, which is the claim that
# matters: two lossy steps in a row still land within the sketch's guarantee.
within "$(number "$AFTER" p50_ms)" "$EXACT_P50" 0.02 "p50 after downsampling, against the exact sample"
within "$(number "$AFTER" p95_ms)" "$EXACT_P95" 0.02 "p95 after downsampling, against the exact sample"

# The default listing reads both tables, so it must be unchanged by a fold
# that moved rows between them.
AFTER_LIST="$(object "$("$BINARY" transactions list -project "$PROJECT_ID" --json)" sampling)"
[ "$(number "$AFTER_LIST" received)" = 1000 ] \
  || fail "the listing lost transactions to the fold"
ok "the listing reads both tables and is unchanged"

step "a project that never switched tracing on"
DSN2="$("$BINARY" projects create -name apagado)"
CODE="$(printf '{}\n{"type":"transaction","length":2}\n{}\n' | curl -sS -o /dev/null -w '%{http_code}' \
  -X POST "http://127.0.0.1:$PORT/api/2/envelope/" \
  -H "X-Sentry-Auth: Sentry sentry_version=7, sentry_key=$(printf '%s' "$DSN2" | sed -n 's|.*//\([^@]*\)@.*|\1|p')" \
  --data-binary @-)"
[ "$CODE" = 429 ] || fail "a transaction sent to a project with tracing off answered $CODE, want 429"
ok "the SDK is told to stop sending, which is what makes the toggle free (ADR 005)"

EMPTY="$("$BINARY" transactions list -project 2 --json)"
expect "$EMPTY" '"total":0' "and the performance page answers with nothing rather than an error"

printf '\n\033[1;32mall checks passed\033[0m\n'
