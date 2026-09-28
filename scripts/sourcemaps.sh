#!/usr/bin/env bash
#
# Source-map uploads as a gate: the real, pinned sentry-cli against a real
# installation of this server, and then this server's own API asked whether
# anything arrived.
#
# Three claims are being checked, and they are not the same claim.
#
#   1. The tool accepts what this server answers. Only the tool can say that.
#      The recorded fixtures pin what it *sends*, and internal/adapters/httpapi
#      replays them on every `make check`; a response it cannot parse is
#      invisible to that and ends an upload halfway (ADR 013).
#   2. What it sent is actually here. This is the half the protocol makes easy
#      to get wrong: `{"state":"ok"}` is taken at face value, so a server that
#      answers it without holding the bundle has told the tool the upload
#      succeeded and nobody finds out until somebody opens a minified stack
#      trace weeks later (see the annex of ADR 018).
#
#   3. Having them here is worth something. A stored artefact nobody resolves a
#      frame with is a backup of somebody's build directory. The last two
#      sections ingest the recorded browser envelope and require
#      the issue it produces to name `src/checkout.js` with the line that threw
#      — and then require the product to point at the commit that wrote it
#      (ADR 019). That is the exit criterion of this whole phase, stated as a
#      script.
#
# The negative-case section is what makes the second claim mean anything: it points the
# same tool at a server that lies — the recorder under compat/, which answers `ok`
# and stores nothing — and requires that the tool report success while this
# gate's own check finds nothing. A check that cannot fail is not a check.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# shellcheck source=compat/sentry-cli/version.env
. compat/sentry-cli/version.env
# shellcheck source=compat/browser/build-bundle.sh
. compat/browser/build-bundle.sh

# 9813 by default: this gate's port block starts at 9810, and its first three
# are spoken for by smoke, compat and ui-smoke.
PORT="${PORT:-9813}"
# shellcheck source=lib/port.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/port.sh"

DOCKER_PREFIX="${DOCKER_PREFIX:-trapline-sourcemaps}"
IMAGE="getsentry/sentry-cli:$SENTRY_CLI_VERSION"
RUNTIME_IMAGE="alpine:3.22"
NETWORK="$DOCKER_PREFIX-net"
SERVER="$DOCKER_PREFIX-server"
WORKDIR="$(mktemp -d)"
BINARY="$WORKDIR/trapline"

cleanup() {
  docker ps -aq --filter "name=^${DOCKER_PREFIX}-" | xargs -r docker rm -f >/dev/null 2>&1 || true
  docker network rm "$NETWORK" >/dev/null 2>&1 || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

step() { printf '\n\033[1m== %s\033[0m\n' "$1"; }
ok()   { printf '   ok   %s\n' "$1"; }
fail() { printf '   FAIL %s\n' "$1"; exit 1; }

command -v docker >/dev/null 2>&1 || fail "this gate runs the real sentry-cli, which needs Docker"
command -v npm >/dev/null 2>&1 || fail "this gate builds a real bundle, which needs npm"

require_free_port "$PORT" "the source-map gate"

step "build"
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BINARY" ./cmd/trapline
( cd compat/sentry-cli/recorder && CGO_ENABLED=0 go build -trimpath -o "$WORKDIR/recorder" . ) \
  || fail "building the stand-in server for the negative case"
ok "server and the lying stand-in"

step "an installation"
docker network create "$NETWORK" >/dev/null
# The server runs inside the container network because that is the only place
# the sentry-cli container can reach it, and the port is published so this
# script can assert against the same server (compat/sentry-cli/verify.sh).
#
# -origin is the network alias and NOT 127.0.0.1, and that is not cosmetic: the
# capabilities document's `url` is built from the configured origin, and the
# client posts its chunks to whatever that field says rather than to the path
# it asked on. An installation behind a proxy whose origin is wrong sends its
# source maps to an address that is not its server, silently (recorded).
docker run -d --name "$SERVER" \
  --network "$NETWORK" --network-alias trapline \
  -p "127.0.0.1:$PORT:$PORT" \
  -v "$WORKDIR:/w:ro" \
  "$RUNTIME_IMAGE" /w/trapline serve -addr ":$PORT" -db /tmp/trapline.db \
  -origin "http://trapline:$PORT" >/dev/null

API="http://127.0.0.1:$PORT/api/v1"
for _ in $(seq 1 100); do
  curl -fsS "$API/health" >/dev/null 2>&1 && break
  sleep 0.1
done
curl -fsS "$API/health" >/dev/null || { docker logs "$SERVER"; fail "the server never became healthy"; }

curl -fsS -X POST "$API/setup" \
  -H 'Content-Type: application/json' -H 'X-Trapline-Request: 1' \
  -d '{"username":"antonio","password":"una contraseña larga y buena"}' >/dev/null \
  || fail "setup failed"

TOKEN="$(docker exec "$SERVER" /w/trapline token create -db /tmp/trapline.db -name sourcemaps --json \
  | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
[ -n "$TOKEN" ] || fail "no token returned"

export TRAPLINE_URL="http://127.0.0.1:$PORT"
export TRAPLINE_TOKEN="$TOKEN"

DSN="$("$BINARY" projects create -name "venekambio")" || fail "creating the project"
SENTRY_KEY="$(printf '%s' "$DSN" | sed -n 's|.*//\([^@]*\)@.*|\1|p')"
[ -n "$SENTRY_KEY" ] || fail "could not read the ingest key out of the DSN"
ok "server up on $PORT, project addressable as 'venekambio'"

# artifacts lists what the server holds, through the product's own API. Every
# assertion below goes through this rather than through the emulated surface:
# if the state the upload produced were only visible to the surface that
# produced it, it would have produced nothing.
artifacts() { "$BINARY" artifacts list -project 1 --json "$@"; }

expect() { # haystack needle description
  printf '%s' "$1" | grep -qF -- "$2" || { printf '%s\n' "$1"; fail "$3"; }
  ok "$3"
}

refute() { # haystack needle description
  # An `if`, not a `&&` chain: under `set -e` a grep that finds nothing makes
  # the whole chain return non-zero and the script exits — which would turn
  # the one assertion here that is supposed to pass on absence into a silent
  # abort that reads like a success.
  if printf '%s' "$1" | grep -qF -- "$2"; then
    printf '%s\n' "$1"
    fail "$3"
  fi
  ok "$3"
}

count_artifacts() { printf '%s' "$1" | grep -o '"id":' | wc -l | tr -d ' '; }

step "a bundle with debug ids injected into it"
BUNDLE="$WORKDIR/bundle"
build_bundle "$BUNDLE" || fail "building the bundle"

# cli is the real tool, pinned, configured with nothing about this server but
# SENTRY_URL — which is the entire claim.
cli() {
  docker run --rm --name "$DOCKER_PREFIX-cli" \
    --network "$NETWORK" --user "$(id -u):$(id -g)" \
    -v "$BUNDLE:/work" -w /work \
    -e "SENTRY_URL=http://trapline:$PORT" \
    -e "SENTRY_AUTH_TOKEN=$TOKEN" \
    -e "SENTRY_ORG=acme" -e "SENTRY_PROJECT=venekambio" -e "HOME=/tmp" \
    "$IMAGE" "$@"
}

cli sourcemaps inject dist >/dev/null 2>&1 || fail "sourcemaps inject"
DEBUG_ID="$(sed -n 's/.*"debug_id":"\([^"]*\)".*/\1/p' "$BUNDLE/dist/bundle.min.js.map")"
[ -n "$DEBUG_ID" ] || fail "inject wrote no debug id into the source map"
ok "debug id $DEBUG_ID"

step "sourcemaps upload, resolved by debug id alone"
# No --release. Three requests, and the release never comes up: the debug id is
# the whole join (recorded). This is the flow that breaks entirely if the
# capabilities document announces only artifact_bundles.
cli sourcemaps upload dist >/dev/null 2>&1 || fail "sourcemaps upload"

STORED="$(artifacts)"
[ "$(count_artifacts "$STORED")" = "2" ] \
  || { printf '%s\n' "$STORED"; fail "the server holds $(count_artifacts "$STORED") artifacts, want the script and its map"; }
expect "$STORED" '"kind":"minified_source"' "the script is stored, under the manifest's own vocabulary"
expect "$STORED" '"kind":"source_map"' "and its map"
expect "$STORED" "\"debug_id\":\"$DEBUG_ID\"" "both carry the debug id that was injected into the build"
expect "$STORED" '"sourcemap_ref":"bundle.min.js.map"' \
  "the script's sourcemap header survived, which is what joins it to its map without a debug id"
expect "$STORED" '"name":"~/bundle.min.js"' "and the ~/ url, which is what the legacy lookup needs"

# Narrowed by debug id: one build, both its files. This is the query
# symbolication makes on the ingest path.
BY_ID="$(artifacts -debug-id "$DEBUG_ID")"
[ "$(count_artifacts "$BY_ID")" = "2" ] \
  || { printf '%s\n' "$BY_ID"; fail "the debug id matched $(count_artifacts "$BY_ID") files"; }
ok "the debug id resolves to exactly the script and the map"

step "sourcemaps upload --release --dist"
cli sourcemaps upload --release app@1.0.0 --dist prod dist >/dev/null 2>&1 \
  || fail "sourcemaps upload --release --dist"
# The deduplication query GET …/releases/{v}/files/?checksum=…&cursor= is
# emitted here, and it is on the happy path: had it answered 404, this command
# would be the one that failed (recorded).
RELEASED="$(artifacts -release app@1.0.0)"
[ "$(count_artifacts "$RELEASED")" = "2" ] \
  || { printf '%s\n' "$RELEASED"; fail "the release holds $(count_artifacts "$RELEASED") artifacts"; }
expect "$RELEASED" '"dist":"prod"' "the dist arrived with them"
ok "the release-addressed upload landed on the release"

step "the old way: releases files upload"
# The pre-debug-id path, and the only one that never touches a chunk: one
# multipart POST carrying the file and the url to serve it under (recorded).
# Deprecated in the tool, and in a great many pipelines.
cli releases files app@1.0.0 upload dist/bundle.min.js '~/legacy.min.js' >/dev/null 2>&1 \
  || fail "releases files upload"
LEGACY="$(artifacts -release app@1.0.0)"
expect "$LEGACY" '"name":"~/legacy.min.js"' "the legacy upload is attached to the release"

step "a project over its artifact budget"
# The budget is charged where the project is known. On the chunked path that is
# the assembly, and the refusal has to travel inside the body as
# {"state":"error","detail":…}: a 413 there reaches the user as "unknown error"
# and tells them nothing about what to change (recorded; ADR 038).
"$BINARY" config set -project 1 -artifacts-max-mb 0 >/dev/null || fail "setting the budget to zero"

BEFORE="$(count_artifacts "$(artifacts)")"
set +e
REFUSED="$(cli sourcemaps upload --release app@2.0.0 dist 2>&1)"
REFUSED_CODE=$?
set -e
[ "$REFUSED_CODE" -ne 0 ] || { printf '%s\n' "$REFUSED"; fail "an upload past the budget was reported as a success"; }
# The detail is what the tool prints, so it is the only thing the person who
# has to fix this ever sees. A refusal they cannot act on is an outage.
printf '%s' "$REFUSED" | grep -qi 'budget\|artifacts_max_mb' \
  || { printf '%s\n' "$REFUSED"; fail "the refusal reached the user without saying what to change"; }
ok "refused, and the reason reached the user: $(printf '%s' "$REFUSED" | grep -io 'artifacts_max_mb' | head -1)"

AFTER="$(count_artifacts "$(artifacts)")"
[ "$BEFORE" = "$AFTER" ] || fail "a refused upload stored $((AFTER - BEFORE)) artifacts anyway"
ok "and nothing was stored"

"$BINARY" config set -project 1 -artifacts-max-mb default >/dev/null || fail "restoring the budget"

step "this product's own CLI, with no sentry-cli anywhere"
# ADR 006: uploading source maps must not be an operation that requires
# installing somebody else's tool. It builds the same archive and posts it to
# this server's own endpoint, so both paths converge on one reader.
#
# A *different* build of the same files, so this also checks the thing an old
# event depends on: two builds of one url are two artefacts, and re-uploading
# replaces only its own. A schema that identified an artefact by url alone
# would have thrown the first build's map away here.
OTHER_ID="aaaaaaaa-520c-531b-836e-0d5e872e8a18"
OWN="$WORKDIR/own"
mkdir -p "$OWN"
cp "$BUNDLE/dist/bundle.min.js" "$OWN/app.min.js"
cp "$BUNDLE/dist/bundle.min.js.map" "$OWN/app.min.js.map"
# The map's sourceMappingURL still names the old file; rewrite it so the pair
# is internally consistent, which is what a real build produces.
sed -i "s|bundle.min.js.map|app.min.js.map|; s|$DEBUG_ID|$OTHER_ID|" "$OWN/app.min.js"
sed -i "s|$DEBUG_ID|$OTHER_ID|" "$OWN/app.min.js.map"

"$BINARY" artifacts upload -project 1 "$OWN" >/dev/null || fail "trapline artifacts upload"
MINE="$(artifacts)"
expect "$MINE" '"name":"~/app.min.js"' "the CLI's own upload arrived"
expect "$MINE" "\"debug_id\":\"$OTHER_ID\"" \
  "carrying the debug id that build already had, read from the files rather than invented"
expect "$MINE" "\"debug_id\":\"$DEBUG_ID\"" \
  "and the earlier build is still there, which is what an event from the old deploy needs"

ARTIFACT_ID="$(printf '%s' "$MINE" | grep -o '"id":[0-9]*' | head -1 | cut -d: -f2)"
[ -n "$ARTIFACT_ID" ] || fail "could not read an artifact id out of the listing"
"$BINARY" artifacts delete -project 1 -id "$ARTIFACT_ID" >/dev/null || fail "trapline artifacts delete"
set +e
"$BINARY" artifacts delete -project 1 -id "$ARTIFACT_ID" >/dev/null 2>&1
DELETED_TWICE=$?
set -e
[ "$DELETED_TWICE" -ne 0 ] || fail "deleting the same artifact twice reported success"
ok "upload, list and delete, all through the product's own API"

step "the whole point: a minified crash, resolved"
# Everything above proves the files arrived. This proves they are *used*: the
# recorded browser envelope, ingested for real, and the issue it
# produces read back through the product's own API.
#
# The bundle and map uploaded here are the ones that event was recorded
# against (internal/sourcemap/testdata), and not the freshly built pair above:
# the only thing joining an event to an artefact in the modern protocol is a
# debug id, and a pair built here would join up only by coincidence — a
# coincidence that would turn into a mystery the first time esbuild changed a
# byte.
RECORDED="$WORKDIR/recorded"
mkdir -p "$RECORDED"
cp internal/sourcemap/testdata/bundle.min.js internal/sourcemap/testdata/bundle.min.js.map "$RECORDED/"
"$BINARY" artifacts upload -project 1 "$RECORDED" >/dev/null \
  || fail "uploading the bundle the recorded event was raised from"

curl -fsS -o /dev/null -w '%{http_code}' \
  -X POST "http://127.0.0.1:$PORT/api/1/envelope/" \
  -H "X-Sentry-Auth: Sentry sentry_version=7, sentry_client=sentry.javascript.browser/10.71.0, sentry_key=$SENTRY_KEY" \
  --data-binary @internal/sourcemap/testdata/browser-debugid.envelope \
  | grep -q '^200$' || fail "the recorded browser envelope was not accepted"

ISSUE_ID="$("$BINARY" issues list -project 1 --json \
  | grep -o '"id":[0-9]*' | head -1 | cut -d: -f2)"
[ -n "$ISSUE_ID" ] || fail "the event produced no issue"

DETAIL="$("$BINARY" issues show -project 1 -issue "$ISSUE_ID" --json)"
expect "$DETAIL" '"filename":"../src/checkout.js"' \
  "the failing frame names the original file and not the bundle"
expect "$DETAIL" '"context_line":"  throw new Error' \
  "and carries the line of source that actually threw"
# The key order is the JSON encoder's, not this product's: `issues show --json`
# passes the stored payload through a generic decode, which sorts.
expect "$DETAIL" '"raw":{"colno":489,' \
  "with the minified position kept underneath it, which is the only thing that can be compared against a deployed build"

step "a repository, and the commit that did it"
# The other half of ADR 019, driven the way a deploy pipeline would: a real
# checkout, `releases commits -repo`, and then the question. Three commits, and
# exactly one of them touches the file the failing frame names.
REPO="$WORKDIR/repo"
mkdir -p "$REPO/src"
git -C "$REPO" init -q -b main || fail "the gate needs git"
git -C "$REPO" config user.email "gate@example.test"
git -C "$REPO" config user.name "Source-map gate"

printf '# demo\n' > "$REPO/README.md"
git -C "$REPO" add . && git -C "$REPO" commit -qm "first commit"
printf 'export const cart = [];\n' > "$REPO/src/cart.js"
git -C "$REPO" add . && git -C "$REPO" commit -qm "add the cart"
cp compat/browser/src/checkout.js "$REPO/src/checkout.js"
git -C "$REPO" add . && git -C "$REPO" commit -qm "rewrite the decoder"

# No -from: a first deploy has no previous release to start at, which is the
# case this defaults for.
SENT="$("$BINARY" releases commits -project 1 -version compat@1.0.0 -repo "$REPO")" \
  || fail "trapline releases commits -repo"
printf '%s' "$SENT" | grep -q '^3 commits$' \
  || { printf '%s\n' "$SENT"; fail "the git log did not arrive as three commits"; }
ok "three commits, with the paths sentry-cli only sends with --local"

SUSPECTS="$("$BINARY" issues suspects -project 1 -issue "$ISSUE_ID")" \
  || fail "trapline issues suspects"
expect "$SUSPECTS" "1. " "a suspect is named"
expect "$SUSPECTS" "rewrite the decoder" "and it is the commit that touched the failing frame's file"
expect "$SUSPECTS" "touched src/checkout.js" "with the reason it is accused"
expect "$SUSPECTS" "frame #1" "naming which frame"
refute "$SUSPECTS" "add the cart" "and the commits that touched nothing in the stacktrace are not accused"

step "commits without a patch set"
# ADR 019's explicit fallback, and the common one: `sentry-cli releases
# set-commits` *without* --local sends no paths at all. The honest answer is
# the commit list, nothing marked, and a line saying what to change.
printf '[{"id":"0123456789abcdef0123456789abcdef01234567","message":"no patch set"}]' \
  > "$WORKDIR/pathless.json"
"$BINARY" releases commits -project 1 -version compat@1.0.0 -file "$WORKDIR/pathless.json" >/dev/null \
  || fail "sending a commit set without paths"

PATHLESS="$("$BINARY" issues suspects -project 1 -issue "$ISSUE_ID")" \
  || fail "trapline issues suspects, with no paths to go on"
refute "$PATHLESS" "1. " "nothing is accused when nothing can be"
expect "$PATHLESS" "01234567" "the candidate commits are listed anyway"
expect "$PATHLESS" "without the files they changed" "and the warning names what to change"

step "the negative case: a server that says ok and stores nothing"
# This is what gives every assertion above its meaning. The protocol takes
# `{"state":"ok"}` at face value, so a server that answers it optimistically
# ends with sentry-cli reporting a successful upload of nothing at all (recorded;
# ADR 018). The recorder under compat/sentry-cli/ is exactly such a server.
#
# What has to hold is BOTH halves: the tool must report success — otherwise
# the trap is not the trap — and this gate's own check must find nothing.
mkdir -p "$WORKDIR/liar-out"
docker run -d --rm --name "$DOCKER_PREFIX-liar" \
  --network "$NETWORK" --network-alias liar \
  --user "$(id -u):$(id -g)" \
  -v "$WORKDIR:/work" -w /work \
  "$RUNTIME_IMAGE" /work/recorder -addr ":9000" -out /work/liar-out >/dev/null
for _ in $(seq 1 50); do
  docker run --rm --network "$NETWORK" "$RUNTIME_IMAGE" \
    wget -q -T 2 -O /dev/null "http://liar:9000/__ready" >/dev/null 2>&1 && break
  sleep 0.2
done

# A project of its own, so "nothing arrived" is a fact about this upload and
# not about a table the successful uploads above already filled.
"$BINARY" projects create -name "fooled" >/dev/null || fail "creating the second project"

set +e
docker run --rm --name "$DOCKER_PREFIX-cli-liar" \
  --network "$NETWORK" --user "$(id -u):$(id -g)" \
  -v "$BUNDLE:/work" -w /work \
  -e "SENTRY_URL=http://liar:9000" -e "SENTRY_AUTH_TOKEN=$TOKEN" \
  -e "SENTRY_ORG=acme" -e "SENTRY_PROJECT=fooled" -e "HOME=/tmp" \
  "$IMAGE" sourcemaps upload dist >/dev/null 2>&1
LIED_CODE=$?
set -e
[ "$LIED_CODE" -eq 0 ] \
  || fail "the stand-in server did not manage to fool the tool, so this gate proves nothing about the trap"
ok "sentry-cli reported a successful upload against a server that stored nothing"

# And the check that matters: the same assertion this gate makes everywhere
# else, run against that outcome, has to FAIL. If it passed, every `ok` above
# would be worth nothing.
LIAR_STORED="$("$BINARY" artifacts list -project 2 --json)"
refute "$LIAR_STORED" '"kind"' \
  "and this gate's own check found nothing there, which is what makes it a check"

docker rm -f "$DOCKER_PREFIX-liar" >/dev/null 2>&1 || true

printf '\n\033[1;32msentry-cli %s uploads source maps to this server, they are actually here,\nand a minified crash comes back as original source with the commit that caused it\033[0m\n' \
  "$SENTRY_CLI_VERSION"
