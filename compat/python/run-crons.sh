#!/usr/bin/env bash
#
# Entry point for the cron half of the Python suite. It shares run.sh's
# virtualenv — same requirements file, same stamp — so the crons gate does not
# pay a second `pip install` and a version bump is still picked up by both.
set -euo pipefail

cd "$(dirname "$0")"

VENV=.venv
STAMP="$VENV/.requirements"

if [ ! -f "$STAMP" ] || ! cmp -s requirements.txt "$STAMP"; then
  python3 -m venv "$VENV"
  "$VENV/bin/pip" install --quiet --disable-pip-version-check --requirement requirements.txt
  cp requirements.txt "$STAMP"
fi

exec "$VENV/bin/python" crons.py "$@"
