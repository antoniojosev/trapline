#!/usr/bin/env bash
#
# Entry point for the PHP suite: installs its own vendor tree, then runs it in
# a container. The SDK lives here and only here — the product's dependency
# graph must never contain an SDK it exists to receive events from
# (compat/README.md).
#
# A container rather than a local `php` because this is the first suite whose
# runtime is not already on a Go developer's machine, and a matrix that only
# runs where someone happened to install PHP is a matrix that stops running.
set -euo pipefail

cd "$(dirname "$0")"

# Pinned, because a compatibility matrix that floats tells you the SDK broke on
# some unknown day rather than on the commit that bumped it (ADR 002).
PHP_IMAGE="php:8.3-cli"
COMPOSER_IMAGE="composer:2"
PREFIX="${DOCKER_PREFIX:-trapline-compat}"
UIDGID="$(id -u):$(id -g)"

# Installing on every run would make the matrix pay a network round trip per
# invocation; keying the stamp on the lockfile's contents means a version bump
# is still picked up without one.
STAMP="vendor/.composer.lock"
if [ ! -f "$STAMP" ] || ! cmp -s composer.lock "$STAMP"; then
  docker run --rm --name "$PREFIX-composer" \
    -v "$PWD":/app -w /app -u "$UIDGID" -e COMPOSER_HOME=/tmp/composer \
    "$COMPOSER_IMAGE" install --no-interaction --no-progress --quiet
  cp composer.lock "$STAMP"
fi

# --add-host is what lets the suite reach the server running on the host; the
# runner rewrites the DSN's host to match (scripts/compat.sh).
exec docker run --rm --name "$PREFIX-php" \
  --add-host=host.docker.internal:host-gateway \
  -v "$PWD":/app -w /app -u "$UIDGID" \
  "$PHP_IMAGE" php main.php "$@"
