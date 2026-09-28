#!/usr/bin/env bash
#
# Entry point for the release-health suite: the same isolated environment
# run.sh and run-crons.sh build, running health.py. The SDK lives here and only
# here — the product's dependency graph must never contain an SDK it exists to
# receive events from (compat/README.md).
set -euo pipefail

cd "$(dirname "$0")"

VENV=.venv
STAMP="$VENV/.requirements"

if [ ! -f "$STAMP" ] || ! cmp -s requirements.txt "$STAMP"; then
  python3 -m venv "$VENV"
  "$VENV/bin/pip" install --quiet --disable-pip-version-check --requirement requirements.txt
  cp requirements.txt "$STAMP"
fi

exec "$VENV/bin/python" health.py "$@"
