#!/usr/bin/env bash
#
# The documentation as a gate: every example that claims to be runnable is run,
# and every internal link points at something that exists.
#
# Everything else this product says about itself is checked by something.
# Throughput is `make bench`; the footprint is `footprint.sh`; that an official
# SDK works is `compat.sh` with the real SDK; that the API is frozen is
# `TestRoutesMatchOpenAPI` walking the document and the route table in both
# directions. The documentation was the one surface nobody checked — and it is
# where the mistake is cheapest to make and most expensive to find. A flag that
# was renamed two months ago breaks no build, shows up in no metric, and is
# discovered by somebody who just installed this, pasted the command, and got a
# usage error. That person does not open an issue (ADR 040).
#
# How a document says what it wants:
#
#   ```sh          illustrative. Starting a server, `docker compose`,
#                  `curl | sh`. Nobody runs it.
#   ```sh test     executable. This script runs it against a real server, in
#                  its own shell, with `set -euo pipefail`. Non-zero fails the
#                  gate naming the file and the line the block starts on.
#
# The split is the line between what this repository can prove and what it
# cannot, and it is visible in the document itself so that nobody marks
# something executable by accident.
#
# Run it with PORT set to somewhere free; it takes a temporary database and
# leaves nothing behind.
set -euo pipefail

# 9953 by default: this gate's port block starts at 9950, and its first
# three are spoken for by smoke, compat and ui-smoke.
PORT="${PORT:-9953}"
# Never talk to a server this script did not start.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
# shellcheck source=lib/port.sh
. "$ROOT/scripts/lib/port.sh"

require_free_port "$PORT" "the documentation gate"

# The name of the binary the documentation calls.
NAME="trapline"
# A public-looking origin rather than this port: `doctor` checks that the
# origin is not pointing at the machine it runs on, and a gate whose fixture
# trips a real check teaches nothing.
ORIGIN="${ORIGIN:-https://errores.example.test}"

WORKDIR="$(mktemp -d)"
BINDIR="$WORKDIR/bin"
DB="$WORKDIR/$NAME.db"
SERVER_PID=""
SCRATCH="$WORKDIR/scratch"

cleanup() {
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null || true
  [ -n "$SERVER_PID" ] && wait "$SERVER_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

step() { printf '\n\033[1m== %s\033[0m\n' "$1"; }
ok()   { printf '   ok   %s\n' "$1"; }
fail() { printf '   FAIL %s\n' "$1" >&2; exit 1; }

mkdir -p "$BINDIR" "$SCRATCH"

step "build"
# Into a directory on PATH under the name the documentation uses, so a block
# runs the command exactly as written instead of a path this script invented.
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BINDIR/$NAME" ./cmd/trapline
export PATH="$BINDIR:$PATH"
ok "$NAME built"

step "a server, and the state the examples assume"
"$BINDIR/$NAME" serve -addr "127.0.0.1:$PORT" -db "$DB" -origin "$ORIGIN" \
  >"$WORKDIR/server.log" 2>&1 &
SERVER_PID=$!

export TRAPLINE_URL="http://127.0.0.1:$PORT"
export TRAPLINE_DB="$DB"
API="$TRAPLINE_URL/api/v1"

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
ok "admin account"

TRAPLINE_TOKEN="$("$BINDIR/$NAME" token create -db "$DB" -name docs --json |
  sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
[ -n "$TRAPLINE_TOKEN" ] || fail "no token returned"
export TRAPLINE_TOKEN
ok "token"

# Project 1, which is what every example in the documentation names. An
# example that only works with data the reader does not have is another way of
# being broken (ADR 040), so the seed is exactly what the documents assume: one
# project, one error with a release and a stacktrace, a second occurrence so
# the aggregates have something to show.
DSN="$("$BINDIR/$NAME" projects create -name mi-app)"
KEY="$(printf '%s' "$DSN" | sed -n 's|.*//\([^@]*\)@.*|\1|p')"
[ -n "$KEY" ] || fail "could not read the public key out of the DSN"
ok "project 1 (mi-app)"

send_event() { # event_id value
  local event
  event='{"event_id":"'"$1"'","timestamp":"'"$(date -u +%Y-%m-%dT%H:%M:%SZ)"'",'
  event+='"platform":"python","level":"error","release":"mi-app@1.4.2","environment":"production",'
  event+='"exception":{"values":[{"type":"ValueError","value":"'"$2"'","stacktrace":{"frames":['
  event+='{"filename":"app/checkout.py","abs_path":"/srv/app/checkout.py","function":"total","lineno":42,"in_app":true}'
  event+=']}}]},"tags":{"server":"web-01"}}'
  printf '{"dsn":"%s"}\n{"type":"event","length":%d}\n%s\n' "$DSN" "${#event}" "$event" |
    curl -fsS -X POST "$TRAPLINE_URL/api/1/envelope/" \
      -H "X-Sentry-Auth: Sentry sentry_version=7, sentry_key=$KEY" \
      --data-binary @- >/dev/null
}
send_event 9ec79c33ec9942ab8353589fcb2e04dc "invalid amount"
send_event 9ec79c33ec9942ab8353589fcb2e04dd "invalid amount"
ok "issue 1, seen twice"

step "the runnable examples"
# Extraction and execution are separate passes so that a document with no
# runnable block is a visible zero rather than silence.
BLOCKS="$WORKDIR/blocks"
mkdir -p "$BLOCKS"
count=0
while IFS= read -r doc; do
  # awk emits one file per runnable block plus a tab-separated line saying
  # where it came from, so the executing pass below can name the document and
  # the line when something fails.
  awk -v out="$BLOCKS" -v doc="$doc" '
    BEGIN { safe = doc; gsub(/\//, "_", safe) }
    /^```sh test[ \t]*$/ { inb = 1; n++;
                           file = out "/" safe "." n ".sh";
                           printf "" > file;
                           printf "%s\t%d\t%s\n", doc, NR, file;
                           next }
    inb && /^```[ \t]*$/ { inb = 0; close(file); next }
    inb { print > file }
  ' "$doc"
done < <(git ls-files --cached --others --exclude-standard '*.md') >"$WORKDIR/index"

while IFS=$'\t' read -r doc line file; do
  count=$((count + 1))
  if ! (cd "$SCRATCH" && bash -euo pipefail "$file") >"$file.log" 2>&1; then
    printf '\n\033[1m%s:%s\033[0m — the block failed\n\n' "$doc" "$line" >&2
    sed 's/^/    | /' "$file" >&2
    printf '\n    output:\n\n' >&2
    sed 's/^/    > /' "$file.log" >&2
    fail "a documented command does not work"
  fi
  ok "$doc:$line"
done <"$WORKDIR/index"

[ "$count" -gt 0 ] || fail 'no "sh test" blocks found — the extractor is broken, not the docs'
printf '   %d runnable examples, all of them ran\n' "$count"

step "internal links"
python3 "$ROOT/scripts/lib/docs_links.py" "$ROOT" || fail "broken internal links"

printf '\n\033[1;32mthe documentation does what it says\033[0m\n'
