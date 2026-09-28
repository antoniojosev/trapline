#!/usr/bin/env bash
#
# Entry point for the browser suite: installs and bundles on the host, then
# drives a real Chromium in a container. The SDK lives here and only here — the
# product's dependency graph must never contain an SDK it exists to receive
# events from (compat/README.md).
#
# The bundle is built rather than loaded from a CDN, because that is what every
# application using this SDK does: a build tool inlines it into a script the
# browser fetches from the application's own origin. Loading a pre-built bundle
# from somebody else's host would test a deployment shape almost nobody has,
# and would put a network round trip inside a gate.
set -euo pipefail

cd "$(dirname "$0")"

# Pinned, and the image's version has to match the `playwright` package's or
# the driver refuses to talk to the browsers baked into it.
PLAYWRIGHT_IMAGE="mcr.microsoft.com/playwright:v1.56.0-noble"
PREFIX="${DOCKER_PREFIX:-trapline-compat}"
UIDGID="$(id -u):$(id -g)"

# The browsers come from the image, so the package must not try to download its
# own; they would not be used and would cost hundreds of megabytes per run.
export PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1
npm ci --no-fund --no-audit --silent

rm -rf dist
mkdir -p dist
cp src/index.html dist/index.html

# Two bundles, deliberately. The page's own script keeps its function names so
# that a culprit derived from its frames is readable; the second is minified
# with a source map beside it, which is the shape a production application
# actually ships and the fixture the symbolication work will consume.
npx --no-install esbuild src/app.js \
  --bundle --format=iife --target=chrome120 --outfile=dist/app.js --log-level=warning
npx --no-install esbuild src/checkout.js \
  --bundle --minify --sourcemap --format=iife --global-name=__checkoutBundle \
  --target=chrome120 --outfile=dist/bundle.min.js --log-level=warning

# --add-host is what lets the suite reach the server running on the host; the
# runner rewrites the DSN's host to match (scripts/compat.sh).
exec docker run --rm --name "$PREFIX-browser" \
  --add-host=host.docker.internal:host-gateway \
  --ipc=host \
  -v "$PWD":/app -w /app -u "$UIDGID" -e HOME=/tmp \
  "$PLAYWRIGHT_IMAGE" node runner.js "$@"
