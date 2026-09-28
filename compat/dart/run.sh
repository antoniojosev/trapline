#!/usr/bin/env bash
#
# Entry point for the Dart suite: resolves its own package tree, then runs it
# in a container. The SDK lives here and only here — the product's dependency
# graph must never contain an SDK it exists to receive events from
# (compat/README.md).
#
# A container rather than a local `dart` because this is not a runtime a Go
# developer has installed, and a matrix that only runs where somebody happened
# to install Dart is a matrix that stops running.
set -euo pipefail

cd "$(dirname "$0")"

# Pinned, because a compatibility matrix that floats tells you the SDK broke on
# some unknown day rather than on the commit that bumped it (ADR 002).
DART_IMAGE="dart:3.13.2"
PREFIX="${DOCKER_PREFIX:-trapline-compat}"
UIDGID="$(id -u):$(id -g)"

# The pub cache lives beside the suite rather than in the container, so a
# second run costs no network. Keying the stamp on the lockfile's contents
# means a version bump is still picked up without one.
STAMP=".dart_tool/.pubspec.lock"
if [ ! -f "$STAMP" ] || ! cmp -s pubspec.lock "$STAMP"; then
  docker run --rm --name "$PREFIX-dart-pub" \
    -v "$PWD":/app -w /app -u "$UIDGID" -e HOME=/tmp -e PUB_CACHE=/app/.pub-cache \
    "$DART_IMAGE" dart pub get >/dev/null
  cp pubspec.lock "$STAMP"
fi

# --add-host is what lets the suite reach the server running on the host; the
# runner rewrites the DSN's host to match (scripts/compat.sh).
exec docker run --rm --name "$PREFIX-dart" \
  --add-host=host.docker.internal:host-gateway \
  -v "$PWD":/app -w /app -u "$UIDGID" -e HOME=/tmp -e PUB_CACHE=/app/.pub-cache \
  "$DART_IMAGE" dart run bin/main.dart "$@"
