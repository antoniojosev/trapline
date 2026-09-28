#!/usr/bin/env bash
#
# The minified bundle with a source map beside it, built somewhere else.
#
# Two things need this bundle and they are not in the same place: the recording
# of what `sentry-cli sourcemaps upload` sends (compat/sentry-cli/record.sh),
# and the capture of the event a browser raises from it
# (compat/browser/record-envelope.sh). Building it twice, in two scripts, is
# how the two halves of the source-map story would drift apart — the recorded
# upload would describe one bundle and the recorded event another, and the
# debug id that is supposed to join them would not match.
#
# Reproducibility is the point, so the build happens inside the destination
# directory: a source map's `sources` are relative to the map, and building
# from anywhere else writes the absolute path of whoever ran it into a
# committed fixture.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# build_bundle <destination>
#
# Leaves <destination>/dist/{bundle.min.js,bundle.min.js.map} and a copy of the
# source the map points at.
build_bundle() {
  destination="$1"

  # The browsers come from the Playwright image; this only needs esbuild.
  PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 npm --prefix "$HERE" ci --no-fund --no-audit --silent

  mkdir -p "$destination/src" "$destination/dist"
  # Unless the destination *is* this directory, which is the case when the
  # capture next door builds in place.
  if [ "$(cd "$destination" && pwd)" != "$HERE" ]; then
    cp "$HERE/src/checkout.js" "$destination/src/checkout.js"
  fi

  # Run from the destination so `sources` comes out as ../src/checkout.js
  # rather than a chain of ../ from wherever this was invoked.
  ( cd "$destination" && "$HERE/node_modules/.bin/esbuild" src/checkout.js \
      --bundle --minify --sourcemap --format=iife --global-name=__checkoutBundle \
      --target=chrome120 --outfile=dist/bundle.min.js --log-level=warning )
}
