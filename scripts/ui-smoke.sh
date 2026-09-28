#!/usr/bin/env bash
#
# The panel, in a real browser.
#
# smoke.sh proves the product works through the CLI and compat.sh proves the
# ingest protocol works for real SDKs. Between them the web panel was the one
# thing shipping untested: it type-checked, it built, its API was covered from
# both sides — and nothing had ever rendered it. A screen that does not render
# passes every one of those checks.
#
# So this boots the same binary a user installs, serving the same embedded
# panel, and drives it with Chromium from the official Playwright container.
# The browser is in a container because pinning a browser build is the only way
# a UI check means the same thing on this machine as it does in CI.
#
# The server runs in a container too, on a network of its own with the browser,
# and that is not for isolation's sake. A container calling back to the machine
# that started it is the one part of this arrangement with no portable answer:
# `host-gateway` points at the Windows host rather than at the WSL distribution
# the server is listening in, and the distribution's own address is unroutable
# from the engine's VM the moment any unrelated project on the machine has
# claimed the same private range. That failure is indistinguishable from a
# broken panel and it costs an afternoon to tell apart. Putting both ends on one
# user-defined network replaces the whole question with container DNS. The
# binary is the same static one a user installs — CGO-free is what makes it
# runnable in any image at all (ADR 009) — and the port is published so the
# checks that run before the browser does still speak to it from here.
set -euo pipefail

PORT="${PORT:-9002}"
# Where the alerting story delivers. It is a second process because the claim
# being checked is that a notification leaves this installation and arrives
# somewhere else: a receiver inside the server would be the server agreeing
# with itself.
RECEIVER_PORT="${RECEIVER_PORT:-$((PORT + 1))}"
# The rule every gate in here obeys: never talk to a server this script did
# not start.
# shellcheck source=lib/port.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/port.sh"

require_free_port "$PORT" "the panel gate"
require_free_port "$RECEIVER_PORT" "the panel gate's webhook receiver"

DOCKER_PREFIX="${DOCKER_PREFIX:-trapline-ui-smoke}"
# Pinned to the exact version of @playwright/test in web/e2e/package.json: the
# image ships the browser builds that release expects, so any drift between the
# two is a download at best and a mismatch at worst.
PLAYWRIGHT_IMAGE="${PLAYWRIGHT_IMAGE:-mcr.microsoft.com/playwright:v1.62.1-noble}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKDIR="$(mktemp -d)"
ARTIFACTS="$ROOT/artifacts/ui"
BINARY="$WORKDIR/trapline"
RECEIVER_BINARY="$WORKDIR/receiver"
CONTAINER="$DOCKER_PREFIX-playwright"
SERVER="$DOCKER_PREFIX-server"
RECEIVER="$DOCKER_PREFIX-receiver"
NETWORK="$DOCKER_PREFIX-net"

# The signing secret the panel will be told to type into the channel form. The
# receiver verifies against it with code that imports nothing from trapline,
# so a signature that only this project agrees with does not pass.
WEBHOOK_SECRET="${WEBHOOK_SECRET:-a-secret-long-enough-for-the-panel-gate}"

# The DSN the panel shows has to be one the browser's container can post to, so
# the public origin is the server's name on the network they share. That is not
# a test fixture standing in for the real thing: it is the real thing, and it is
# why the DSN is read off the screen rather than out of the database.
ORIGIN="${ORIGIN:-http://$SERVER:$PORT}"

cleanup() {
  docker rm -f "$CONTAINER" "$CONTAINER-health" "$SERVER" "$RECEIVER" >/dev/null 2>&1 || true
  docker network rm "$NETWORK" >/dev/null 2>&1 || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

step() { printf '\n\033[1m== %s\033[0m\n' "$1"; }
ok()   { printf '   ok   %s\n' "$1"; }
fail() { printf '   FAIL %s\n' "$1"; exit 1; }

# What to print when the browser is unhappy. The screenshots alone are rarely
# the answer: half the panel's failures are the server's, and the other half
# are the far end of an alert not receiving what it should have.
browser_failed() {
  printf '\n   screenshots and traces: %s\n' "$ARTIFACTS"
  printf '   server log:\n'
  docker logs "$SERVER" 2>&1 | sed 's/^/     /'
  printf '   what the receiver got:\n'
  curl -fsS "http://127.0.0.1:$RECEIVER_PORT/deliveries" 2>&1 | sed 's/^/     /'
  fail "the panel did not behave"
}

command -v docker >/dev/null 2>&1 || fail "docker is required to run a pinned browser"

step "build"
# The panel is not built here. internal/adapters/webui/dist is committed and
# `make web` is the only thing that regenerates it, so this checks the bytes
# that would actually ship rather than a fresh build of the current source.
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BINARY" "$ROOT/cmd/trapline"
(cd "$ROOT/compat/alerts/receiver" && CGO_ENABLED=0 go build -trimpath -o "$RECEIVER_BINARY" .)
ok "$(du -h "$BINARY" | cut -f1) binary, panel embedded, and the webhook receiver"

step "boot"
docker network create "$NETWORK" >/dev/null
# The database lives inside the container and dies with it. The binary is
# mounted read-only rather than baked into an image: it is the artefact that was
# just built, and copying it into a layer would only add a step at which the two
# could differ. The port is published on loopback so the checks below, and
# anyone debugging a failure, can reach the same server from here.
docker run -d --name "$SERVER" --network "$NETWORK" \
  -v "$BINARY:/trapline:ro" \
  -p "127.0.0.1:$PORT:$PORT" \
  --entrypoint /trapline \
  "$PLAYWRIGHT_IMAGE" \
  serve -addr ":$PORT" -db /tmp/trapline.db -origin "$ORIGIN" >/dev/null

API="http://127.0.0.1:$PORT/api/v1"
for _ in $(seq 1 100); do
  if curl -fsS "$API/health" >/dev/null 2>&1; then break; fi
  sleep 0.1
done
curl -fsS "$API/health" >/dev/null || { docker logs "$SERVER" 2>&1; fail "server never became healthy"; }
ok "serving on port $PORT, origin $ORIGIN"

curl -fsS "http://127.0.0.1:$PORT/" | grep -q 'id="root"' || fail "the panel is not being served"
ok "the panel is where the browser will look for it"

# The far end of the alerting story, on the same network so the server can
# reach it by name and the browser can read back what arrived. It binds every
# interface rather than loopback: inside a container, loopback is a network
# nobody else is on.
docker run -d --name "$RECEIVER" --network "$NETWORK" \
  -v "$RECEIVER_BINARY:/receiver:ro" \
  -p "127.0.0.1:$RECEIVER_PORT:$RECEIVER_PORT" \
  --entrypoint /receiver \
  "$PLAYWRIGHT_IMAGE" \
  -addr ":$RECEIVER_PORT" -secret "$WEBHOOK_SECRET" >/dev/null

for _ in $(seq 1 100); do
  if curl -fsS "http://127.0.0.1:$RECEIVER_PORT/health" >/dev/null 2>&1; then break; fi
  sleep 0.1
done
curl -fsS "http://127.0.0.1:$RECEIVER_PORT/health" >/dev/null \
  || { docker logs "$RECEIVER" 2>&1; fail "the webhook receiver never became healthy"; }
ok "a webhook receiver on port $RECEIVER_PORT, verifying signatures on its own"

step "browser"
mkdir -p "$ARTIFACTS"
rm -rf "${ARTIFACTS:?}/"*

# --user keeps node_modules and any screenshots owned by whoever ran this,
# instead of by root inside the container. PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD is
# safe precisely because the image tag and the package version are pinned
# together: the browsers are already in the image, at the build the package
# expects.
docker run --rm --name "$CONTAINER" \
  --network "$NETWORK" \
  --user "$(id -u):$(id -g)" \
  -e HOME=/tmp \
  -e npm_config_cache=/tmp/.npm \
  -e PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 \
  -e TRAPLINE_BASE_URL="http://$SERVER:$PORT" \
  -e TRAPLINE_RECEIVER_URL="http://$RECEIVER:$RECEIVER_PORT" \
  -e TRAPLINE_WEBHOOK_SECRET="$WEBHOOK_SECRET" \
  -e CI="${CI:-}" \
  -v "$ROOT:/work" \
  -w /work/web/e2e \
  "$PLAYWRIGHT_IMAGE" \
  bash -c 'npm ci --no-fund --no-audit --loglevel=error && npx playwright test smoke.spec.ts' \
  || {
    browser_failed
  }
ok "every screen rendered and did what it says"

step "restart"
# Release health is the one screen that cannot be checked in the same breath as
# it is fed. Sessions are counted in a bounded window in memory and written
# down on a timer or on a clean shutdown (ADR 008), so reading the crash-free
# rate straight after reporting it would be reading a number the server has
# not committed to yet — and waiting out the sixty-second timer would put a
# minute of sleep into a pull-request gate.
#
# So the flush is provoked the way production provokes it: SIGTERM. The
# container's filesystem survives `docker restart`, so the database is the same
# one, which also makes the second half prove something the first cannot — that
# a session cookie outlives the process, because it is a hash in that database
# and not state in memory.
docker restart -t 15 "$SERVER" >/dev/null
docker logs "$SERVER" 2>&1 | grep -q 'session window drained' \
  || { docker logs "$SERVER" 2>&1; fail "the session window was never drained on shutdown"; }

for _ in $(seq 1 100); do
  if curl -fsS "$API/health" >/dev/null 2>&1; then break; fi
  sleep 0.1
done
curl -fsS "$API/health" >/dev/null \
  || { docker logs "$SERVER" 2>&1; fail "the server never came back"; }
ok "drained on SIGTERM and back up, with the same database"

step "browser, after the restart"
docker run --rm --name "$CONTAINER-health" \
  --network "$NETWORK" \
  --user "$(id -u):$(id -g)" \
  -e HOME=/tmp \
  -e npm_config_cache=/tmp/.npm \
  -e PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 \
  -e TRAPLINE_BASE_URL="http://$SERVER:$PORT" \
  -e TRAPLINE_AFTER_RESTART=1 \
  -e CI="${CI:-}" \
  -v "$ROOT:/work" \
  -w /work/web/e2e \
  "$PLAYWRIGHT_IMAGE" \
  bash -c 'npx playwright test health.spec.ts' \
  || {
    browser_failed
  }
ok "the crash-free rate is on the screen, with the caveat that belongs beside it"

step "shutdown"
# `docker stop` sends SIGTERM and waits, which is the same signal systemd sends
# and the same question: does it drain, or does it have to be killed?
docker stop -t 40 "$SERVER" >/dev/null
docker logs "$SERVER" 2>&1 | grep -q stopped || fail "no clean shutdown in the log"
docker rm -f "$SERVER" >/dev/null
docker rm -f "$RECEIVER" >/dev/null
ok "drained and stopped cleanly"

printf '\n\033[1;32mall checks passed\033[0m\n'
