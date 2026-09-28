#!/usr/bin/env bash
#
# Record what sentry-cli actually sends.
#
# The real tool, in its pinned container, runs a whole release through a
# stand-in server that answers just enough to keep it walking and writes every
# request down. The result is committed under fixtures/<version>/ and is what
# the handler is implemented and tested against (ADR 013).
#
# This is not a convenience. The API sentry-cli speaks is documented partially
# and drifts, and this project already paid for guessing once: @sentry/node
# sends no authentication header at all, only ?sentry_key= in the query, and a
# server built from the documented header alone would have rejected every Node
# installation while its own tests stayed green (ADR 002). The recording is the
# only thing that can say what the wire looks like.
#
# Re-record when the pin in version.env moves, and read the diff.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

# shellcheck source=compat/sentry-cli/version.env
. compat/sentry-cli/version.env
# shellcheck source=compat/sentry-cli/fixture-repo.sh
. compat/sentry-cli/fixture-repo.sh
# shellcheck source=compat/browser/build-bundle.sh
. compat/browser/build-bundle.sh

DOCKER_PREFIX="${DOCKER_PREFIX:-trapline-sentry-cli}"
IMAGE="getsentry/sentry-cli:$SENTRY_CLI_VERSION"
# The smallest image that can hold a static binary and a network namespace.
RUNTIME_IMAGE="alpine:3.22"
NETWORK="$DOCKER_PREFIX-record"
FIXTURES="compat/sentry-cli/fixtures/$SENTRY_CLI_VERSION"
WORKDIR="$(mktemp -d)"

# --check records into a scratch directory and diffs the result against what is
# committed, instead of overwriting it.
#
# A recording nobody can reproduce is a screenshot: it says what happened once
# on one machine and cannot say whether it still happens. This is the mode that
# makes the fixtures a claim rather than an anecdote, and it is the gate.
CHECK=""
if [ "${1:-}" = "--check" ]; then
  CHECK="$FIXTURES"
  FIXTURES="$WORKDIR/fixtures"
fi

# The recorder listens inside the container network only. Nothing is published
# to the host, so this script cannot collide with anything else's ports —
# and cannot reach a server it did not start.
RECORDER_PORT=9000

cleanup() {
  docker ps -aq --filter "name=^${DOCKER_PREFIX}-" | xargs -r docker rm -f >/dev/null 2>&1 || true
  docker network rm "$NETWORK" >/dev/null 2>&1 || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

step() { printf '\n\033[1m== %s\033[0m\n' "$1"; }
ok()   { printf '   ok   %s\n' "$1"; }
fail() { printf '   FAIL %s\n' "$1"; exit 1; }

command -v docker >/dev/null 2>&1 || fail "recording needs Docker: it runs the real, pinned sentry-cli"
command -v npm >/dev/null 2>&1 || fail "recording needs npm: the source-map flows are recorded over a real bundle"

step "the recorder"
# Its own module, so a development-only HTTP server never enters the product's
# dependency graph. Static, because it runs in a container with no libc.
( cd compat/sentry-cli/recorder && CGO_ENABLED=0 go build -trimpath -o "$WORKDIR/recorder" . ) \
  || fail "building the recorder"
mkdir -p "$WORKDIR/out"
ok "built"

step "a repository to have commits in"
REPO="$WORKDIR/repo"
build_fixture_repo "$REPO"
ok "three commits, covering added, modified and deleted"

step "the network"
docker network create "$NETWORK" >/dev/null

# start_recorder <flow> [recorder flags...]
#
# One recorder per flow, each with its own output directory and its own
# sequence numbering, because these are separate stories: a single recorder
# would number the source-map upload's first request 8 and hide where one flow
# ends and the next begins. And the flags matter — what the recorder claims to
# accept is what decides which protocol the tool then speaks, so a flow is the
# pairing of a server's answers with the client's behaviour, not just a command.
start_recorder() {
  flow="$1"; shift
  docker rm -f "$DOCKER_PREFIX-recorder" >/dev/null 2>&1 || true
  mkdir -p "$WORKDIR/out/$flow"
  docker run -d --rm --name "$DOCKER_PREFIX-recorder" \
    --network "$NETWORK" --network-alias recorder \
    --user "$(id -u):$(id -g)" \
    -v "$WORKDIR:/work" -w /work \
    "$RUNTIME_IMAGE" /work/recorder -addr ":$RECORDER_PORT" -out "/work/out/$flow" "$@" >/dev/null
  for _ in $(seq 1 50); do
    docker run --rm --network "$NETWORK" "$RUNTIME_IMAGE" \
      wget -q -T 2 -O /dev/null "http://recorder:$RECORDER_PORT/__ready" >/dev/null 2>&1 && return 0
    sleep 0.2
  done
  fail "the recorder never answered inside $NETWORK"
}

# cli runs the pinned tool against the recorder.
#
# --user is not hygiene: libgit2 refuses to read a repository owned by another
# user, so `set-commits --local` fails with "not owned by current user" when the
# container runs as root over a bind mount from the host.
cli_in() {
  directory="$1"; shift
  docker run --rm --name "$DOCKER_PREFIX-cli" \
    --network "$NETWORK" \
    --user "$(id -u):$(id -g)" \
    -v "$directory:/work" -w /work \
    -e "SENTRY_URL=http://recorder:$RECORDER_PORT" \
    -e "SENTRY_AUTH_TOKEN=ek_recording_placeholder" \
    -e "SENTRY_ORG=acme" \
    -e "SENTRY_PROJECT=venekambio" \
    -e "HOME=/tmp" \
    "$IMAGE" "$@"
}

cli() { cli_in "$REPO" "$@"; }

# keep <flow> moves a flow's requests into the committed fixtures.
keep() {
  flow="$1"
  count="$(find "$WORKDIR/out/$flow" -name '*.json' | wc -l | tr -d ' ')"
  [ "$count" -gt 0 ] || fail "the tool made no requests during '$flow', which cannot be right"
  target="$FIXTURES"
  [ "$flow" = "releases" ] || target="$FIXTURES/$flow"
  mkdir -p "$target"
  cp "$WORKDIR/out/$flow"/*.json "$target/"
  printf '   ok   %s requests -> %s\n' "$count" "$target"
}

rm -rf "$FIXTURES"

step "the release lifecycle, as a pipeline runs it"
start_recorder releases
cli releases new app@1.0.0 || fail "releases new"
cli releases set-commits --local app@1.0.0 || fail "releases set-commits --local"
cli releases finalize app@1.0.0 || fail "releases finalize"
cli releases deploys new -r app@1.0.0 -e production -n pipeline-42 \
  --url https://ci.example.test/42 || fail "releases deploys new"
ok "four commands, all succeeded against the recorder"
keep releases

step "a bundle with debug ids injected into it"
# The same bundle the browser suite raises errors from, so the debug id in the
# upload below and the debug id in the event that suite captures are one value.
# Built into its own directory per flow: `sourcemaps upload` reads the whole
# directory, and a flow that saw another flow's leftovers would record an
# upload nobody asked for.
BUNDLE="$WORKDIR/bundle"
build_bundle "$BUNDLE" || fail "building the bundle"
cli_in "$BUNDLE" sourcemaps inject dist >/dev/null 2>&1 || fail "sourcemaps inject"
DEBUG_ID="$(sed -n 's/.*"debug_id":"\([^"]*\)".*/\1/p' "$BUNDLE/dist/bundle.min.js.map")"
[ -n "$DEBUG_ID" ] || fail "inject wrote no debug id into the source map"
grep -q "//# debugId=$DEBUG_ID" "$BUNDLE/dist/bundle.min.js" \
  || fail "inject wrote no matching debugId comment into the bundle"
ok "debug id $DEBUG_ID, in both the bundle and its map"

step "source maps, resolved by debug id alone"
# No --release: the modern path, where nothing but the debug id joins the
# event to the artefact. Three requests, and the release never comes up.
start_recorder sourcemaps-debugid
cli_in "$BUNDLE" sourcemaps upload dist >/dev/null 2>&1 || fail "sourcemaps upload"
ok "uploaded"
keep sourcemaps-debugid

step "source maps, with a release and a dist"
start_recorder sourcemaps-release
cli_in "$BUNDLE" sourcemaps upload --release app@1.0.0 --dist prod dist >/dev/null 2>&1 \
  || fail "sourcemaps upload --release --dist"
ok "uploaded"
keep sourcemaps-release

step "one artefact, uploaded the old way"
# `releases files ... upload` is the pre-debug-id path and the only one that
# never touches a chunk: one multipart POST carrying the file and the URL to
# serve it under. It is deprecated in the tool and still in a great many
# pipelines.
start_recorder legacy-release-files
cli_in "$BUNDLE" releases files app@1.0.0 upload dist/bundle.min.js '~/bundle.min.js' >/dev/null 2>&1 \
  || fail "releases files upload"
ok "uploaded"
keep legacy-release-files

step "the same command against a server that does not offer artifact bundles"
# Not a hypothetical: `accept` is what decides the protocol, and a server that
# leaves artifact_bundles out gets this instead — a release bundle, assembled
# at a different endpoint, with the release created first. Recorded so the
# choice of what to advertise is made against evidence rather than taste.
start_recorder release-bundle-fallback -accept release_files
cli_in "$BUNDLE" sourcemaps upload --release app@1.0.0 dist >/dev/null 2>&1 \
  || fail "sourcemaps upload against a release_files-only server"
ok "uploaded, by the other route"
keep release-bundle-fallback

if [ -n "$CHECK" ]; then
  step "against what is committed"
  # The clocks are the exception, and only the clocks. sentry-cli stamps a
  # release with the moment it ran, so those three fields differ on every run
  # by design; everything else — checksums, debug ids, chunk boundaries, the
  # manifest inside the bundle — is derived from the input and must not move.
  normalise() {
    find "$1" -name '*.json' -printf '%P\n' | sort | while read -r name; do
      printf '### %s\n' "$name"
      sed -E 's/"(dateStarted|dateReleased|dateFinished)": "[^"]*"/"\1": "<clock>"/' "$1/$name"
    done
  }
  if diff -u <(normalise "$CHECK") <(normalise "$FIXTURES"); then
    ok "every request reproduced, byte for byte apart from the clock"
  else
    fail "the recording no longer matches the committed fixtures — read the diff above: it is either a protocol change or something in the recording that is not reproducible"
  fi
fi

printf '\n\033[1;32mrecorded sentry-cli %s\033[0m\n' "$SENTRY_CLI_VERSION"
