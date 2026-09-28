#!/usr/bin/env bash
#
# Capture the event a browser raises from a bundle with debug ids in it.
#
# The upload side of source maps is recorded next door
# (compat/sentry-cli/record.sh); this is the other half. Both need to be real
# and both need to be the *same* bundle, because the only thing joining an
# event to an artefact in the modern protocol is a debug id, and a fixture pair
# whose debug ids do not match would look like a resolver bug forever.
#
# The result is committed under internal/sourcemap/testdata/ — the envelope
# exactly as it left the browser, plus the bundle and map its debug id names.
set -euo pipefail

cd "$(dirname "$0")"
ROOT="$(cd ../.. && pwd)"

# shellcheck source=compat/browser/build-bundle.sh
. ./build-bundle.sh
# shellcheck source=compat/sentry-cli/version.env
. "$ROOT/compat/sentry-cli/version.env"

# Pinned, and the image's version has to match the `playwright` package's or
# the driver refuses to talk to the browsers baked into it.
PLAYWRIGHT_IMAGE="mcr.microsoft.com/playwright:v1.56.0-noble"
CLI_IMAGE="getsentry/sentry-cli:$SENTRY_CLI_VERSION"
PREFIX="${DOCKER_PREFIX:-trapline-compat}"
UIDGID="$(id -u):$(id -g)"
TESTDATA="$ROOT/internal/sourcemap/testdata"

step() { printf '\n\033[1m== %s\033[0m\n' "$1"; }
ok()   { printf '   ok   %s\n' "$1"; }
fail() { printf '   FAIL %s\n' "$1"; exit 1; }

command -v docker >/dev/null 2>&1 || fail "this needs Docker: a real browser and the real sentry-cli"

step "the bundle"
rm -rf dist
build_bundle "$PWD"
cp src/envelope.html dist/envelope.html
ok "built with a source map"

step "debug ids"
# The real tool, pinned, doing to this bundle exactly what it does to a
# production build: a `//# debugId=` comment in the script, a `debug_id` in the
# map, and a snippet that registers the id on window._sentryDebugIds.
docker run --rm --name "$PREFIX-inject" \
  --user "$UIDGID" -v "$PWD:/work" -w /work -e HOME=/tmp \
  "$CLI_IMAGE" sourcemaps inject dist >/dev/null || fail "sourcemaps inject"

# The SDK is bundled after the injection so that its own file is not rewritten
# by it — and unminified, because nothing here is trying to resolve its frames.
npx --no-install esbuild src/envelope.js \
  --bundle --format=iife --target=chrome120 --outfile=dist/envelope.js --log-level=warning \
  || fail "bundling the page's script"

DEBUG_ID="$(sed -n 's/.*"debug_id":"\([^"]*\)".*/\1/p' dist/bundle.min.js.map)"
[ -n "$DEBUG_ID" ] || fail "inject wrote no debug id into the map"
ok "$DEBUG_ID"

step "one error, in a real browser"
docker run --rm --name "$PREFIX-envelope" \
  --ipc=host -v "$PWD:/app" -w /app -u "$UIDGID" -e HOME=/tmp \
  "$PLAYWRIGHT_IMAGE" node record-envelope.js -out /app/dist/captured \
  || fail "capturing the envelope"

step "fixtures"
mkdir -p "$TESTDATA"
cp dist/captured/browser-debugid.envelope dist/captured/bundle.min.js \
   dist/captured/bundle.min.js.map "$TESTDATA/"
grep -q "$DEBUG_ID" "$TESTDATA/browser-debugid.envelope" \
  || fail "the captured event does not name the debug id that was injected, so the two halves of the fixture do not join up"
ok "envelope, bundle and map in $TESTDATA"

printf '\n\033[1;32mcaptured an event with debug ids\033[0m\n'
