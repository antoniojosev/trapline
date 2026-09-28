#!/usr/bin/env bash
#
# The real sentry-cli, against a real installation of this server.
#
# The recorded fixtures pin what the tool *sends*; only the tool itself can
# prove it accepts what this server *answers*. Those are two different claims
# and this project has already been burnt by proving the first and assuming the
# second: a response the client cannot parse ends a deploy pipeline halfway
# through, with the release created and its commits lost (ADR 013).
#
# So this runs the four commands a pipeline runs — new, set-commits --local,
# finalize, deploys new — and then asks the product's own API whether any of it
# arrived. The assertions are deliberately on the native side: if the emulated
# surface were the only place the state existed, it would have stored nothing.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

# shellcheck source=compat/sentry-cli/version.env
. compat/sentry-cli/version.env
# shellcheck source=compat/sentry-cli/fixture-repo.sh
. compat/sentry-cli/fixture-repo.sh

PORT="${PORT:-9501}"
DOCKER_PREFIX="${DOCKER_PREFIX:-trapline-sentry-cli}"
IMAGE="getsentry/sentry-cli:$SENTRY_CLI_VERSION"
RUNTIME_IMAGE="alpine:3.22"
NETWORK="$DOCKER_PREFIX-verify"
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

# A gate must not talk to a server it did not start. Without this check it
# connects to whatever is on the port — a leftover from another run, an
# unrelated project — and fails much later with a symptom that looks nothing
# like the cause.
if ss -ltn 2>/dev/null | grep -q ":$PORT[[:space:]]"; then
  fail "port $PORT is already in use; set PORT to something free"
fi

step "build"
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BINARY" ./cmd/trapline
ok "built"

step "an installation"
docker network create "$NETWORK" >/dev/null
# The server runs in the container network because that is the only place the
# sentry-cli container can reach it: there is no portable address for "the
# host", and on a Docker Desktop installation the containers live in a separate
# virtual machine where the host's interfaces mean something else entirely.
# Putting both sides on one user-defined network sidesteps the question, and
# publishing the port gives this script the same server to assert against.
#
# The database lives inside the container rather than on the bind mount: a
# SQLite file on a virtualised bind mount has file locking that nobody should
# be trusting a gate to.
docker run -d --name "$SERVER" \
  --network "$NETWORK" --network-alias trapline \
  -p "127.0.0.1:$PORT:$PORT" \
  -v "$WORKDIR:/w:ro" \
  "$RUNTIME_IMAGE" /w/trapline serve -addr ":$PORT" -db /tmp/trapline.db \
  -origin "http://127.0.0.1:$PORT" >/dev/null

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

# The token command reads the database directly, and the database is inside the
# container — so the command runs there too.
TOKEN="$(docker exec "$SERVER" /w/trapline token create -db /tmp/trapline.db -name sentry-cli --json \
  | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
[ -n "$TOKEN" ] || fail "no token returned"

export TRAPLINE_URL="http://127.0.0.1:$PORT"
export TRAPLINE_TOKEN="$TOKEN"

"$BINARY" projects create -name "venekambio" >/dev/null || fail "creating the project"
SLUG="$("$BINARY" projects list --json | sed -n 's/.*"slug":"\([^"]*\)".*/\1/p' | head -1)"
[ "$SLUG" = "venekambio" ] || fail "the project's slug is $SLUG, and the pipeline below addresses it as venekambio"
ok "server up on $PORT, project addressable as '$SLUG'"

step "a repository to deploy"
REPO="$WORKDIR/repo"
build_fixture_repo "$REPO"
ok "three commits"

# cli is the real tool, pinned, with nothing about this server configured
# beyond SENTRY_URL — which is the entire claim: point it here and it works.
#
# --user because libgit2 refuses to read a repository owned by another user,
# which is what a root container over a host bind mount looks like to it.
cli() {
  docker run --rm --name "$DOCKER_PREFIX-cli" \
    --network "$NETWORK" \
    --user "$(id -u):$(id -g)" \
    -v "$REPO:/work" -w /work \
    -e "SENTRY_URL=http://trapline:$PORT" \
    -e "SENTRY_AUTH_TOKEN=$TOKEN" \
    -e "SENTRY_ORG=acme" \
    -e "SENTRY_PROJECT=venekambio" \
    -e "HOME=/tmp" \
    "$IMAGE" "$@"
}

step "the pipeline"
cli releases new app@1.0.0 || fail "releases new"
cli releases set-commits --local app@1.0.0 || fail "releases set-commits --local"
cli releases finalize app@1.0.0 || fail "releases finalize"
cli releases deploys new -r app@1.0.0 -e production -n pipeline-42 \
  --url https://ci.example.test/42 || fail "releases deploys new"
ok "four commands, none of which knows this is not the server they were written for"

step "what the product's own API sees"
SHOWN="$("$BINARY" releases show -project 1 -version "app@1.0.0" --json)"

printf '%s' "$SHOWN" | grep -q '"version":"app@1.0.0"' \
  || fail "the release is not there: \`releases new\` did not land"
printf '%s' "$SHOWN" | grep -q '"date_released":"' \
  || fail "the release is not finalised: \`releases finalize\` did not land"
printf '%s' "$SHOWN" | grep -q '"commit_count":3' \
  || fail "the commit set is not three commits: \`set-commits --local\` did not land"
# The changed paths are the whole input to suspect commits later, and they
# arrive only inside the commit set. Shas without paths would look like success.
for path in app/views.py app/total.py README.md; do
  printf '%s' "$SHOWN" | grep -q "$path" || fail "the changed path $path was not stored"
done
printf '%s' "$SHOWN" | grep -q '"type":"M"' || fail "no modification survived the commit set"
printf '%s' "$SHOWN" | grep -q '"type":"D"' || fail "no deletion survived the commit set"

"$BINARY" releases show -project 1 -version "app@1.0.0" \
  | grep -q 'deploy     production pipeline-42' \
  || fail "the deploy is not there: \`releases deploys new\` did not land"
ok "release, commits with their paths and change types, and the deploy"

step "rerunning the pipeline"
# A pipeline reruns. The tool stamps `finalize` with its own clock every time,
# so a server that took the newest date would rewrite when the release shipped.
FIRST="$(printf '%s' "$SHOWN" | grep -o '"date_released":"[^"]*"')"
cli releases new app@1.0.0 >/dev/null || fail "rerunning releases new"
cli releases set-commits --local app@1.0.0 >/dev/null || fail "rerunning set-commits"
cli releases finalize app@1.0.0 >/dev/null || fail "rerunning finalize"
AGAIN="$("$BINARY" releases show -project 1 -version "app@1.0.0" --json)"
SECOND="$(printf '%s' "$AGAIN" | grep -o '"date_released":"[^"]*"')"
[ "$FIRST" = "$SECOND" ] || fail "a rerun moved the release date from $FIRST to $SECOND"
printf '%s' "$AGAIN" | grep -q '"commit_count":3' || fail "a rerun doubled the commit set"
ok "the second run changes nothing"

step "the project by its numeric id"
# The id is what the ingest path already forces on every installation, so it
# has to keep working wherever the slug does — a pipeline configured with an
# integer must not be the one that breaks.
docker run --rm --name "$DOCKER_PREFIX-cli-byid" \
  --network "$NETWORK" --user "$(id -u):$(id -g)" \
  -v "$REPO:/work" -w /work \
  -e "SENTRY_URL=http://trapline:$PORT" -e "SENTRY_AUTH_TOKEN=$TOKEN" \
  -e "SENTRY_ORG=whatever-org-they-configured" -e "SENTRY_PROJECT=1" -e "HOME=/tmp" \
  "$IMAGE" releases new by-numeric-id >/dev/null || fail "addressing the project by id"
"$BINARY" releases list -project 1 | grep -q 'by-numeric-id' \
  || fail "the release created by id is not there"
ok "id and slug both resolve, and the organisation name is ignored"

printf '\n\033[1;32msentry-cli %s runs a release through this server\033[0m\n' "$SENTRY_CLI_VERSION"
