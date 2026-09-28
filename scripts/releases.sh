#!/usr/bin/env bash
#
# The release lifecycle, end to end against a real installation.
#
# This is the executable form of the one claim the feature makes: after you
# resolve an issue "in the next release", the machines still running the old
# build must not reopen it, and the first event from a build that is genuinely
# newer must. Everything else here — ordering releases that are not versions,
# commits with their paths, deploys, finalising — exists to serve that claim
# or to be read next to it (ADR 012).
#
# Run it with PORT set to somewhere free; it takes a temporary database and
# leaves nothing behind.
set -euo pipefail

PORT="${PORT:-9400}"
# The rule every gate in here obeys: never talk to a server this script did
# not start.
# shellcheck source=lib/port.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/port.sh"

require_free_port "$PORT" "the releases gate"

ORIGIN="${ORIGIN:-https://errors.example.test}"
WORKDIR="$(mktemp -d)"
BINARY="$PWD/trapline"
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

step "build"
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BINARY" ./cmd/trapline
ok "built"

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

curl -fsS -X POST "$API/setup" \
  -H 'Content-Type: application/json' -H 'X-Trapline-Request: 1' \
  -d '{"username":"antonio","password":"una contraseña larga y buena"}' >/dev/null \
  || fail "setup failed"

TOKEN="$("$BINARY" token create -db "$WORKDIR/trapline.db" -name releases --json \
  | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
[ -n "$TOKEN" ] || fail "no token returned"

export TRAPLINE_URL="http://127.0.0.1:$PORT"
export TRAPLINE_TOKEN="$TOKEN"

DSN="$("$BINARY" projects create -name venekambio)"
PROJECT_ID=1
KEY="$(printf '%s' "$DSN" | sed -n 's|.*//\([^@]*\)@.*|\1|p')"
ok "installed, with a project and a token"

# send_event <fingerprint-marker> <release> <minute>
#
# The minute matters: an issue's release follows its newest occurrence rather
# than the newest arrival, so two events stamped the same instant would leave
# the release pointer where it was and this script would be testing a clock.
send_event() {
  local marker="$1" release="$2" minute="$3"
  local event
  event="$(printf '{"event_id":"9ec79c33ec9942ab8353589fcb2e04d%s","timestamp":"2026-08-24T10:%02d:00Z","platform":"python","level":"error","release":"%s","environment":"production","exception":{"values":[{"type":"%s","value":"boom","stacktrace":{"frames":[{"filename":"app/views.py","function":"checkout","lineno":42,"in_app":true}]}}]}}' \
    "$((minute % 10))" "$minute" "$release" "$marker")"
  local code
  code="$(printf '{}\n{"type":"event","length":%d}\n%s\n' "${#event}" "$event" \
    | curl -sS -o /dev/null -w '%{http_code}' \
        -X POST "http://127.0.0.1:$PORT/api/$PROJECT_ID/envelope/" \
        -H "X-Sentry-Auth: Sentry sentry_version=7, sentry_key=$KEY" \
        --data-binary @-)"
  [ "$code" = 200 ] || fail "ingest of $marker@$release answered $code"
}

# field <json> <name> — reads a numeric or string field out of a JSON blob.
field() { printf '%s' "$1" | sed -n "s/.*\"$2\":\\(\"[^\"]*\"\\|[0-9]*\\).*/\\1/p" | head -1 | tr -d '"'; }

issue_detail() { "$BINARY" issues show -project "$PROJECT_ID" -issue "$1" --json; }

expect_field() {
  local blob="$1" name="$2" want="$3" what="$4"
  local got
  got="$(printf '%s' "$blob" | grep -o "\"$name\":[^,}]*" | head -1 | cut -d: -f2- | tr -d '"')"
  [ "$got" = "$want" ] || fail "$what: $name is $got, want $want"
}

step "a release registered before its first error"
"$BINARY" releases create -project "$PROJECT_ID" -version "app@1.0.0" >/dev/null
"$BINARY" releases list -project "$PROJECT_ID" | grep -q 'app@1.0.0' || fail "the release was not listed"
"$BINARY" releases list -project "$PROJECT_ID" | grep -q 'unreleased' \
  || fail "a release nobody finalised should not read as shipped"
ok "created, and visibly not finalised yet"

step "an error, and the fix"
send_event ValueError "app@1.0.0" 1
ISSUE_ID="$("$BINARY" issues list -project "$PROJECT_ID" --json | sed -n 's/.*"id":\([0-9]*\).*/\1/p' | head -1)"
[ -n "$ISSUE_ID" ] || fail "the event did not produce an issue"

"$BINARY" issues resolve -project "$PROJECT_ID" -issue "$ISSUE_ID" --next-release >/dev/null
DETAIL="$(issue_detail "$ISSUE_ID")"
expect_field "$DETAIL" status "resolved" "after resolving"
expect_field "$DETAIL" resolve_next_release "true" "after resolving"
expect_field "$DETAIL" resolved_in_release "app@1.0.0" "after resolving"
ok "resolved in the next release, pinned to app@1.0.0"

step "the pod that has not been redeployed yet"
# This is the whole reason the feature exists. Without it the issue reopens
# here, seconds after being resolved, and the status stops meaning anything.
send_event ValueError "app@1.0.0" 2
DETAIL="$(issue_detail "$ISSUE_ID")"
expect_field "$DETAIL" status "resolved" "after an event from the resolved release"
expect_field "$DETAIL" seen_in_resolved_release_count 1 "after an event from the resolved release"
expect_field "$DETAIL" regressions 0 "after an event from the resolved release"
expect_field "$DETAIL" times 2 "after an event from the resolved release"
ok "counted, stored, and not reopened"

step "the new build, still broken"
send_event ValueError "app@1.0.1" 30
DETAIL="$(issue_detail "$ISSUE_ID")"
expect_field "$DETAIL" status "unresolved" "after an event from a newer release"
expect_field "$DETAIL" regressions 1 "after an event from a newer release"
expect_field "$DETAIL" first_release "app@1.0.0" "after an event from a newer release"
expect_field "$DETAIL" regressed_in_release "app@1.0.1" "after an event from a newer release"
ok "reopened as a regression, with both releases named"

step "releases that are not versions"
# Two git shas: nothing about the strings says which came first, so the only
# fact available is which one this installation saw first. Registered in this
# order deliberately — b7d2f04 is the OLDER one here, even though it sorts
# later as text.
"$BINARY" releases create -project "$PROJECT_ID" -version "b7d2f04" >/dev/null
"$BINARY" releases create -project "$PROJECT_ID" -version "a3f9c1e" >/dev/null

send_event KeyError "a3f9c1e" 40
SHA_ISSUE="$("$BINARY" issues list -project "$PROJECT_ID" --json \
  | tr '}' '\n' | grep KeyError | sed -n 's/.*"id":\([0-9]*\).*/\1/p' | head -1)"
[ -n "$SHA_ISSUE" ] || fail "the sha-released error did not produce an issue"

"$BINARY" issues resolve -project "$PROJECT_ID" -issue "$SHA_ISSUE" --next-release >/dev/null
send_event KeyError "b7d2f04" 41
DETAIL="$(issue_detail "$SHA_ISSUE")"
expect_field "$DETAIL" status "resolved" "after an event from a sha seen earlier"
expect_field "$DETAIL" seen_in_resolved_release_count 1 "after an event from a sha seen earlier"
ok "a sha this installation saw first is treated as older, text order ignored"

send_event KeyError "c0ffee1" 42
DETAIL="$(issue_detail "$SHA_ISSUE")"
expect_field "$DETAIL" status "unresolved" "after an event from a sha nobody has seen"
expect_field "$DETAIL" regressions 1 "after an event from a sha nobody has seen"
ok "a build nobody has seen before reopens it"

step "commits and their paths"
cat >"$WORKDIR/commits.json" <<'JSON'
[
  {"id":"a3f9c1e","message":"fix the checkout total","author_name":"Ana","author_email":"ana@example.test",
   "patch_set":[{"path":"app/views.py","type":"M"},{"path":"app/total.py","type":"A"}]},
  {"id":"b7d2f04","message":"tidy up"}
]
JSON
"$BINARY" releases commits -project "$PROJECT_ID" -version "app@1.0.0" -file "$WORKDIR/commits.json" \
  | grep -q '2 commits' || fail "the commit set was not stored"
# Rerunning the same pipeline step must not double the set.
"$BINARY" releases commits -project "$PROJECT_ID" -version "app@1.0.0" -file "$WORKDIR/commits.json" >/dev/null
SHOWN="$("$BINARY" releases show -project "$PROJECT_ID" -version "app@1.0.0" --json)"
printf '%s' "$SHOWN" | grep -q '"commit_count":2' || fail "a rerun changed the commit count"
# The paths are the entire input to suspect commits later (ADR 019).
printf '%s' "$SHOWN" | grep -q 'app/total.py' || fail "the changed paths were not stored"
ok "stored once, paths included, idempotent on rerun"

step "deploys"
"$BINARY" releases deploys -project "$PROJECT_ID" -version "app@1.0.0" \
  -environment production -name pipeline-42 -link "https://ci.example.test/42" >/dev/null
"$BINARY" releases show -project "$PROJECT_ID" -version "app@1.0.0" \
  | grep -q 'deploy     production pipeline-42' || fail "the deploy was not recorded"
ok "recorded against the release"

step "what each release did"
SHOWN="$("$BINARY" releases show -project "$PROJECT_ID" -version "app@1.0.0" --json)"
printf '%s' "$SHOWN" | grep -q '"new_issues":1' || fail "app@1.0.0 does not own the issue it introduced"
BROKE="$("$BINARY" releases show -project "$PROJECT_ID" -version "app@1.0.1" --json)"
printf '%s' "$BROKE" | grep -q '"regressed_issues":1' || fail "app@1.0.1 is not blamed for the regression"
# Two events came from app@1.0.0 and one from app@1.0.1. Each page has to say
# its own number: the mistake that hides here is reporting the project's total
# on every release, which looks right until there is a second release.
expect_field "$SHOWN" events 2 "app@1.0.0"
expect_field "$BROKE" events 1 "app@1.0.1"
ok "new issues, regressions and events attributed to the right build"

step "finalising"
"$BINARY" releases finalize -project "$PROJECT_ID" -version "app@1.0.0" >/dev/null
FIRST="$("$BINARY" releases show -project "$PROJECT_ID" -version "app@1.0.0" --json \
  | grep -o '"date_released":"[^"]*"')"
[ -n "$FIRST" ] || fail "PUT did not finalise the release"
# A retried deploy script must not be able to rewrite when something went out.
"$BINARY" releases finalize -project "$PROJECT_ID" -version "app@1.0.0" \
  -released "2027-01-01T00:00:00Z" >/dev/null
SECOND="$("$BINARY" releases show -project "$PROJECT_ID" -version "app@1.0.0" --json \
  | grep -o '"date_released":"[^"]*"')"
[ "$FIRST" = "$SECOND" ] || fail "a rerun moved the release date from $FIRST to $SECOND"
ok "finalised once, and a rerun leaves the date alone"

step "the release nobody registered"
# Most installations never run a deploy tool. app@1.0.1 and c0ffee1 exist
# because events named them, and they have to be as real as the rest.
"$BINARY" releases list -project "$PROJECT_ID" | grep -q 'app@1.0.1' \
  || fail "a release discovered from an event is not listed"
"$BINARY" releases show -project "$PROJECT_ID" -version "c0ffee1" --json \
  | grep -q '"first_event_at":"' || fail "a discovered release does not know when it was first seen"
ok "created from ingestion, with the window of events it produced"

step "errors"
"$BINARY" releases show -project "$PROJECT_ID" -version "app@9.9.9" >/dev/null 2>&1 \
  && fail "reading a release nobody shipped succeeded"
"$BINARY" issues resolve -project "$PROJECT_ID" -issue 9999 --next-release >/dev/null 2>&1 \
  && fail "resolving an issue that does not exist succeeded"
ok "a release and an issue that do not exist both fail, loudly"

printf '\n\033[1;32mreleases: all good\033[0m\n'
