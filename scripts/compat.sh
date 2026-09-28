#!/usr/bin/env bash
#
# The compatibility matrix: real SDKs against a real server.
#
# This is the claim the product rests on — point an official SDK's DSN here and
# it works — and it cannot be tested with a hand-written envelope, because a
# hand-written envelope is this project's idea of what an SDK sends, which is
# the very thing in question.
#
# Two tiers, because a matrix that is slow is a matrix somebody switches off,
# and a switched-off gate is worse than no gate (ADR 002):
#
#   --tier smoke  the runtimes a Go developer already has — Go, Python, Node.
#                 Seconds, no containers, runs on every pull request.
#   --tier full   the above plus the ones that need a container: a real browser,
#                 PHP and Dart. Minutes, pulls images, runs nightly and as a
#                 release gate.
set -euo pipefail

TIER="smoke"
while [ $# -gt 0 ]; do
  case "$1" in
    --tier)
      TIER="${2:-}"
      shift 2
      ;;
    --tier=*)
      TIER="${1#*=}"
      shift
      ;;
    -h | --help)
      sed -n '2,20p' "$0"
      exit 0
      ;;
    *)
      printf 'unknown argument: %s\n' "$1" >&2
      exit 2
      ;;
  esac
done
case "$TIER" in
  smoke | full) ;;
  *)
    printf 'unknown tier %q: expected smoke or full\n' "$TIER" >&2
    exit 2
    ;;
esac

PORT="${PORT:-9100}"
# The rule every gate in here obeys: never talk to a server this script did
# not start.
# shellcheck source=lib/port.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/port.sh"

require_free_port "$PORT" "the compatibility matrix"

WORKDIR="$(mktemp -d)"
BINARY="$PWD/trapline"
SERVER_PID=""
# Every container this run starts carries this prefix, so a parallel session's
# containers are never touched and this run's can be removed by name.
DOCKER_PREFIX="${DOCKER_PREFIX:-trapline-compat}"
export DOCKER_PREFIX

# The containerised suites reach the server through the host gateway, so the
# server has to listen on more than the loopback interface. The smoke tier
# keeps 127.0.0.1: nothing outside this machine has any business reaching a
# throwaway server with an open setup endpoint.
# An empty host means every interface, IPv4 and IPv6 both — and since the
# server opens one socket per family it now means that literally rather than
# hopefully. Both halves are load-bearing: host.docker.internal resolves to an
# IPv6 address on some Docker installations, and Docker Desktop's WSL2
# localhost relay mirrors only IPv4-bound sockets, so a server on either family
# alone is invisible to somebody's containers (internal/app/app.go, listen).
BIND_ADDRESS="127.0.0.1"
if [ "$TIER" = "full" ]; then
  BIND_ADDRESS=""
fi

cleanup() {
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null || true
  [ -n "$SERVER_PID" ] && wait "$SERVER_PID" 2>/dev/null || true
  # Containers are removed by name rather than by `docker ps -q`, so a run that
  # is killed mid-suite cannot leave one behind and cannot remove somebody
  # else's (scripts run in parallel worktrees share a Docker daemon).
  if [ "$TIER" = "full" ]; then
    docker ps -aq --filter "name=^${DOCKER_PREFIX}-" | xargs -r docker rm -f >/dev/null 2>&1 || true
  fi
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

step() { printf '\n\033[1m== %s\033[0m\n' "$1"; }
fail() { printf '   FAIL %s\n' "$1"; exit 1; }

step "server"
CGO_ENABLED=0 go build -trimpath -o "$BINARY" ./cmd/trapline
"$BINARY" serve -addr "$BIND_ADDRESS:$PORT" -db "$WORKDIR/trapline.db" \
  -origin "http://127.0.0.1:$PORT" >"$WORKDIR/server.log" 2>&1 &
SERVER_PID=$!

API="http://127.0.0.1:$PORT/api/v1"
for _ in $(seq 1 50); do
  curl -fsS "$API/health" >/dev/null 2>&1 && break
  sleep 0.1
done
curl -fsS "$API/health" >/dev/null || { cat "$WORKDIR/server.log"; fail "the server never became healthy"; }

curl -fsS -X POST "$API/setup" -H 'Content-Type: application/json' -H 'X-Trapline-Request: 1' \
  -d '{"username":"compat","password":"una contraseña larga y buena"}' >/dev/null
TOKEN="$("$BINARY" token create -db "$WORKDIR/trapline.db" -name compat --json \
  | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
export TRAPLINE_URL="http://127.0.0.1:$PORT" TRAPLINE_TOKEN="$TOKEN"

printf '   ok   ready on port %s (tier: %s)\n' "$PORT" "$TIER"

failures=0

# run_suite gives each suite its own project, so one language's events cannot
# be mistaken for another's when checking how things grouped.
#
# The third argument is the address the suite must use to reach this server.
# For a suite running on the host that is the loopback; for one in a container
# it is the host gateway, and the DSN has to say so too — an SDK derives its
# ingest URL from the DSN alone, which is the whole point of the DSN.
run_suite() {
  name="$1"
  binary="$2"
  reachable_host="${3:-127.0.0.1:$PORT}"
  step "$name"

  DSN="$("$BINARY" projects create -name "$name")"
  PROJECT_ID="$(printf '%s' "$DSN" | sed -n 's|.*/\([0-9]*\)$|\1|p')"
  DSN="${DSN/127.0.0.1:$PORT/$reachable_host}"

  if "$binary" -dsn "$DSN" -api "http://$reachable_host" -token "$TOKEN" -project "$PROJECT_ID"; then
    return 0
  fi
  failures=$((failures + 1))
  return 0
}

# Each suite is its own module so the SDKs never enter the product's dependency
# graph, which means building them is a separate step.
( cd compat/go && go build -o "$WORKDIR/compat-go" . ) || fail "building the Go suite"
run_suite go-sdk "$WORKDIR/compat-go"

# Python builds and caches its own virtualenv, so it needs no build step here.
run_suite python-sdk ./compat/python/run.sh

# Node installs rather than builds, but for the same reason the Go suite is its
# own module: the SDK is a fixture, not a dependency.
( cd compat/node && npm ci --no-fund --no-audit --silent ) || fail "installing the Node suite"
run_suite node-sdk "$PWD/compat/node/main.js"

# reachable_host finds an address for this server that a container can use.
#
# There is no portable answer. A native Linux daemon reaches the host through
# the bridge gateway or through host.docker.internal mapped with --add-host;
# Docker Desktop runs the containers in a separate virtual machine, where both
# of those name something else entirely and only one of this machine's real
# interface addresses works. Encoding one setup's answer would make the matrix
# pass on CI and fail on the maintainer's laptop, or the reverse — so the
# runner asks instead of assuming, and says which answer it got.
#
# The probe image is the smallest thing that can make an HTTP request; the
# suites' own images are hundreds of megabytes and this runs before them.
PROBE_IMAGE="alpine:3.22"

reachable_host() {
  candidate_hosts="host.docker.internal 172.17.0.1"
  if interfaces="$(hostname -I 2>/dev/null)"; then
    candidate_hosts="$candidate_hosts $interfaces"
  fi

  for candidate in $candidate_hosts; do
    if docker run --rm --name "$DOCKER_PREFIX-probe" \
      --add-host=host.docker.internal:host-gateway \
      "$PROBE_IMAGE" \
      wget -q -T 3 -O /dev/null "http://$candidate:$PORT/api/v1/health" >/dev/null 2>&1; then
      printf '%s:%s' "$candidate" "$PORT"
      return 0
    fi
  done
  return 1
}

if [ "$TIER" = "full" ]; then
  command -v docker >/dev/null 2>&1 || fail "the full tier needs Docker; use --tier smoke without it"

  step "reachable host"
  CONTAINER_HOST="$(reachable_host)" || fail "no address this machine offers was reachable from a container"
  printf '   ok   containers reach this server at %s\n' "$CONTAINER_HOST"

  # A real browser, because nothing else in the matrix sends events from a page
  # over an origin the server did not serve, and that turns out to be the
  # difference between an SDK that works and one that only appears to.
  run_suite browser-sdk ./compat/browser/run.sh "$CONTAINER_HOST"
  run_suite php-sdk ./compat/php/run.sh "$CONTAINER_HOST"
  run_suite dart-sdk ./compat/dart/run.sh "$CONTAINER_HOST"
fi

if [ "$failures" -gt 0 ]; then
  printf '\n\033[1;31m%d suite(s) failed\033[0m\n' "$failures"
  exit 1
fi
if [ "$TIER" = "smoke" ]; then
  printf '\n\033[1;32mthe fast SDKs work against this server; run --tier full for the whole matrix\033[0m\n'
else
  printf '\n\033[1;32mevery SDK in the published matrix works against this server\033[0m\n'
fi
