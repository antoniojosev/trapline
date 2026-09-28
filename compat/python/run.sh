#!/usr/bin/env bash
#
# Entry point for the Python suite: builds its own isolated environment, then
# runs it. The SDK lives here and only here — the product's dependency graph
# must never contain an SDK it exists to receive events from (compat/README.md).
set -euo pipefail

cd "$(dirname "$0")"

VENV=.venv
STAMP="$VENV/.requirements"

# Reinstalling on every run would make the matrix pay a network round trip per
# invocation; keying the stamp on the file's contents means a version bump is
# still picked up without one.
if [ ! -f "$STAMP" ] || ! cmp -s requirements.txt "$STAMP"; then
  python3 -m venv "$VENV"
  "$VENV/bin/pip" install --quiet --disable-pip-version-check --requirement requirements.txt
  cp requirements.txt "$STAMP"
fi

exec "$VENV/bin/python" main.py "$@"
