#!/usr/bin/env bash
#
# The sentry-cli compatibility gate, in the order the work was done: record,
# then implement, then check both halves.
#
#   1. Record  — only when there are no fixtures for the pinned version. The
#                fixtures are committed, so the usual run skips this entirely.
#   2. Replay  — the recorded requests through the handler, in process, with
#                the product's own API asked afterwards what arrived.
#   3. Verify  — the real, pinned sentry-cli against a real installation.
#
# Two and three are not redundant. The fixtures pin what the tool *sends*; only
# the tool can prove it accepts what this server *answers*, and a response it
# cannot parse ends a deploy halfway through with the release already created
# (ADR 013). The replay is fast and needs nothing; the verification pulls two
# images and is why this gate runs nightly rather than on a pull request.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# shellcheck source=compat/sentry-cli/version.env
. compat/sentry-cli/version.env

PORT="${PORT:-9501}"
export PORT
DOCKER_PREFIX="${DOCKER_PREFIX:-trapline-sentry-cli}"
export DOCKER_PREFIX

FIXTURES="compat/sentry-cli/fixtures/$SENTRY_CLI_VERSION"

step() { printf '\n\033[1m== %s\033[0m\n' "$1"; }
ok()   { printf '   ok   %s\n' "$1"; }
fail() { printf '   FAIL %s\n' "$1"; exit 1; }

step "fixtures for sentry-cli $SENTRY_CLI_VERSION"
if [ -d "$FIXTURES" ] && [ -n "$(find "$FIXTURES" -name '*.json' -print -quit)" ]; then
  ok "$(find "$FIXTURES" -name '*.json' | wc -l | tr -d ' ') recorded requests, committed"
  # And they are re-recorded from scratch and compared, because a recording
  # nobody can reproduce is a screenshot: it says what happened once on one
  # machine and cannot say whether it still happens. A difference here is
  # either a protocol change or something in the recording that was never
  # reproducible — both need reading, neither should pass quietly.
  ./compat/sentry-cli/record.sh --check || fail "the recording no longer reproduces"
else
  printf '   ..   nothing recorded for this version; recording now\n'
  ./compat/sentry-cli/record.sh || fail "recording"
  printf '   ..   read the diff under %s before committing it: a fixture that\n' "$FIXTURES"
  printf '        changed is a protocol change somebody has to look at\n'
fi

step "the handler, against the recording"
go test -count=1 -race ./internal/adapters/httpapi/ \
  -run 'TestTheRecordedPipelineRuns|TestTheRecordingUsesOnlyABearerToken|TestTheCompatSurface|TestTheOrgSlugIsIgnored|TestAProjectIsAddressableByIdAndBySlug|TestAnUnknownCompatPathAnswersInTheClientsErrorShape|TestUnknownFieldsAreIgnoredOnTheCompatSurface' \
  || fail "the recorded requests do not run through the handler"
ok "every recorded request replays, and the native API sees the result"

step "the real tool, against a real installation"
./compat/sentry-cli/verify.sh || fail "the real sentry-cli"

printf '\n\033[1;32msentry-cli compatibility: all good\033[0m\n'
