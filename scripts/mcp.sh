#!/usr/bin/env bash
#
# The fourth client as a gate: one table of tools, two transports, and the
# proof that neither of them is a second implementation of the API.
#
# The claim of ADR 022 is small and easy to break silently: an MCP tool is a
# call into this product's own REST API, made with the caller's own
# credential, so it can do exactly what that credential can do and answers
# exactly what the endpoint answers. Three things follow, and each is a
# section below:
#
#   1. `tools/list` is identical over stdio and over HTTP. There is one table;
#      two lists would mean an agent that can fix a bug from the operator's
#      laptop and cannot from CI, which is the one place this product is
#      supposed to be useful.
#   2. `get_issue_bundle` returns the same bytes as `GET .../bundle`. The
#      moment those differ, the tool has stopped being an adapter — and the
#      way anybody would find out is an agent acting on a stale answer.
#   3. A token without `projects:write` is refused by `resolve_issue`, as a
#      *tool* error naming the status. A protocol error would abort the
#      agent's turn; a tool error costs it one retry with a better token.
#
# The client is the official MCP Go SDK, unmodified, in compat/mcp — the same
# rule the SDK matrix follows (ADR 002): a hand-written JSON-RPC client would
# only prove this server agrees with itself.
#
# Run it with PORT set to somewhere free; it takes a temporary database and
# leaves nothing behind.
set -euo pipefail

# 9933 by default: this gate's port block starts at 9930, and its first
# three are spoken for by smoke, compat and ui-smoke.
PORT="${PORT:-9933}"
# The rule every gate in here obeys: never talk to a server this script did
# not start.
# shellcheck source=lib/port.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/port.sh"

require_free_port "$PORT" "the MCP gate"

ORIGIN="${ORIGIN:-https://errors.example.test}"
WORKDIR="$(mktemp -d)"
BINARY="$PWD/trapline"
CLIENT="$WORKDIR/compat-mcp"
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

step "build"
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BINARY" ./cmd/trapline
ok "server built"
# The client is its own module, like every other compat suite: its
# dependencies are a test client's and have no business in the product's
# go.mod. Built into the workdir so `git status` stays clean.
(cd compat/mcp && CGO_ENABLED=0 go build -o "$CLIENT" .)
ok "MCP client built (compat/mcp)"

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

TOKEN="$("$BINARY" token create -db "$DB" -name agent --json | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
[ -n "$TOKEN" ] || fail "no token returned"
# The second credential is the one the last section turns on: it can read
# everything and write nothing, which is what a dashboard or a read-only agent
# is handed.
READ_ONLY="$("$BINARY" token create -db "$DB" -name dashboard -scopes projects:read --json |
  sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
[ -n "$READ_ONLY" ] || fail "no read-only token returned"
export TRAPLINE_URL="http://127.0.0.1:$PORT"
export TRAPLINE_TOKEN="$TOKEN"

DSN="$("$BINARY" projects create -name venekambio)"
KEY="$(printf '%s' "$DSN" | sed -n 's|.*//\([^@]*\)@.*|\1|p')"
PROJECT_ID=1
ok "project created"

step "an error to ask about"
# Through the public ingest endpoint, with a stacktrace and a release, so the
# bundle has something in every section it can fill: what broke, where, how
# often, and which deploy it arrived with.
EVENT='{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","timestamp":"'"$(date -u +%Y-%m-%dT%H:%M:%SZ)"'",'
EVENT+='"platform":"python","level":"error","release":"venekambio@1.4.2","environment":"production",'
EVENT+='"exception":{"values":[{"type":"ValueError","value":"invalid amount","stacktrace":{"frames":['
EVENT+='{"filename":"app/checkout.py","abs_path":"/srv/app/checkout.py","function":"total","lineno":42,"in_app":true}'
EVENT+=']}}]},"tags":{"server":"web-01"}}'
printf '{"dsn":"%s"}\n{"type":"event","length":%d}\n%s\n' "$DSN" "${#EVENT}" "$EVENT" |
  curl -fsS -X POST "http://127.0.0.1:$PORT/api/$PROJECT_ID/envelope/" \
    -H "X-Sentry-Auth: Sentry sentry_version=7, sentry_key=$KEY" \
    --data-binary @- >/dev/null || fail "the event was refused"

ISSUES="$("$BINARY" issues list -project "$PROJECT_ID" --json)"
ISSUE_ID="$(printf '%s' "$ISSUES" | sed -n 's/.*"issues":\[{"id":\([0-9]*\).*/\1/p')"
[ -n "$ISSUE_ID" ] || { printf '%s\n' "$ISSUES"; fail "the event did not become an issue"; }
ok "issue #$ISSUE_ID"

step "the bundle is a document, over REST and over the CLI"
# The CLI first, because it is the half of ADR 006 that an MCP-only gate would
# leave untested: `trapline issues bundle` has to be the same answer, or the
# agent-first story only works for whoever has an MCP client.
VIA_CLI="$("$BINARY" issues bundle -project "$PROJECT_ID" -issue "$ISSUE_ID")"
expect "$VIA_CLI" "## Stacktrace" "the CLI prints the stacktrace"
expect "$VIA_CLI" "app/checkout.py" "…naming the file that failed"
expect "$VIA_CLI" "## Frequency" "…and how often it is happening"
expect "$VIA_CLI" "venekambio@1.4.2" "…and the release it arrived with"

HEADERS="$(curl -fsS -D - -o "$WORKDIR/bundle.md" \
  -H "Authorization: Bearer $TOKEN" \
  "$API/projects/$PROJECT_ID/issues/$ISSUE_ID/bundle")"
expect "$HEADERS" "text/markdown" "the REST endpoint answers text/markdown"

# The CLI is a client of that endpoint, so the bytes have to match exactly —
# which is why the command writes the body through untouched instead of
# through the usual text formatter. Captured to a file rather than compared
# against the shell variable above: `$( )` eats trailing newlines, and those
# are precisely the bytes this is about.
"$BINARY" issues bundle -project "$PROJECT_ID" -issue "$ISSUE_ID" >"$WORKDIR/cli.md"
diff "$WORKDIR/cli.md" "$WORKDIR/bundle.md" ||
  fail "the CLI and the REST endpoint disagree about the same issue"
ok "the CLI and the REST endpoint return the same document, byte for byte"

step "the same tools over stdio and over HTTP"
# Everything from here is the MCP client's: it launches `trapline mcp` as a
# subprocess for stdio, opens a session against POST /mcp for HTTP, compares
# the two tool lists, compares both bundles against the REST one, and ends on
# the refusal.
"$CLIENT" \
  -binary "$BINARY" \
  -url "http://127.0.0.1:$PORT" \
  -token "$TOKEN" \
  -read-only-token "$READ_ONLY" \
  -project venekambio \
  -project-id "$PROJECT_ID" \
  -issue "$ISSUE_ID" || fail "the MCP client rejected this server"

step "and the issue is still open"
# The last check of the last section wrote nothing, and this is the proof.
# Without it, "the refusal was well-formed" could be true of a server that
# refused the answer and performed the write.
AFTER="$("$BINARY" issues show -project "$PROJECT_ID" -issue "$ISSUE_ID" --json)"
expect "$AFTER" '"status":"unresolved"' "a refused resolve_issue changed nothing"

printf '\n\033[1;32mall checks passed\033[0m\n'
