#!/usr/bin/env bash
#
# Proves that install.sh installs a real release.
#
# The installer is the first code a stranger runs, on a machine nobody here has
# seen, in a shell that is not bash, against artifacts nobody has checked. Every
# other gate in this repo tests code that runs after all of that worked. Until
# this script existed, install.sh had never been executed once — it was a file
# that looked correct.
#
# So the shape is: build an actual release with the actual release tooling,
# serve it the way a release is served, and let a container that has never seen
# this project install from it. Nothing is stubbed except systemd's refusal to
# reload inside a container, and that stub is replaced by validating the unit
# file that was really installed.
#
#   ./scripts/install-test.sh          # PORT and DOCKER_PREFIX are overridable
#
# goreleaser runs in a container because it is not installed on this machine and
# should not have to be: the release tool is a dependency of releasing, not of
# developing.
set -euo pipefail

PORT="${PORT:-9223}"
DOCKER_PREFIX="${DOCKER_PREFIX:-trapline-install-test}"

# Pinned, both of them. A gate whose result depends on what latest happened to
# mean this morning reports on the internet, not on this repository.
GORELEASER_IMAGE="goreleaser/goreleaser:v2.18.0"
TARGET_IMAGE="debian:bookworm-slim"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

WORKDIR="$(mktemp -d)"
SERVER_PID=""

cleanup() {
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null || true
  [ -n "$SERVER_PID" ] && wait "$SERVER_PID" 2>/dev/null || true
  docker rm -f "$DOCKER_PREFIX-goreleaser" "$DOCKER_PREFIX-install" >/dev/null 2>&1 || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

step() { printf '\n\033[1m== %s\033[0m\n' "$1"; }
ok()   { printf '   ok   %s\n' "$1"; }
fail() { printf '   \033[1;31mFAIL\033[0m %s\n' "$1" >&2; exit 1; }

# --- preflight ----------------------------------------------------------
step "preflight"
command -v docker  >/dev/null 2>&1 || fail "docker is required; see scripts/bootstrap.sh"
docker info >/dev/null 2>&1        || fail "the docker daemon is not reachable"
command -v python3 >/dev/null 2>&1 || fail "python3 is required to serve the release"
command -v git     >/dev/null 2>&1 || fail "git is required: goreleaser reads the tree's state"
ok "docker and python3 are available"

# --- build a real release ------------------------------------------------
step "release (snapshot)"

# The repository may be a worktree, in which case .git is a file pointing at a
# directory outside it. Mounting both at their real paths is what makes git
# work inside the container without teaching goreleaser about worktrees.
GIT_COMMON="$(cd "$(git rev-parse --git-common-dir)" && pwd)"

# The host's module and build caches are mounted, and the container runs as the
# host user, so the caches stay usable afterwards and the build does not have
# to reach the network. Go's pinned toolchain (go.mod) lives in the module
# cache too, which is how the container ends up compiling with the same
# compiler CI does rather than whatever the image happens to ship.
command -v go >/dev/null 2>&1 || fail "go is required to locate the module cache"

# Skipped, and why: `before` runs go mod tidy and the whole test suite. Both
# are `make check`'s job, and tidy would write to go.mod from inside a
# container, which is a fine way to end a session with a dirty tree. `sign`
# needs a GPG key that only a real release has.
docker run --rm --name "$DOCKER_PREFIX-goreleaser" \
  --user "$(id -u):$(id -g)" \
  -v "$ROOT:$ROOT" -w "$ROOT" \
  -v "$GIT_COMMON:$GIT_COMMON" \
  -v "$(go env GOMODCACHE):/gomodcache" \
  -v "$(go env GOCACHE):/gocache" \
  -e HOME=/tmp -e GOMODCACHE=/gomodcache -e GOCACHE=/gocache \
  "$GORELEASER_IMAGE" release --snapshot --clean --skip=before,sign \
  >"$WORKDIR/goreleaser.log" 2>&1 \
  || { cat "$WORKDIR/goreleaser.log"; fail "goreleaser could not build a snapshot"; }

VERSION="$(sed -n 's/.*"version":"\([^"]*\)".*/\1/p' dist/metadata.json)"
[ -n "$VERSION" ] || fail "could not read the version out of dist/metadata.json"
ARCHIVE="trapline_${VERSION}_linux_amd64.tar.gz"
[ -f "dist/$ARCHIVE" ] || fail "dist/$ARCHIVE was not built"
ok "built $VERSION"

# --- what the archive carries -------------------------------------------
#
# The archive is the product for anyone who is not building from source, so
# what goreleaser put in it is worth asserting rather than assuming. A rename
# in scripts/ that silently stopped shipping the installer would otherwise
# only surface as a stranger's bug report.
step "archive contents"
tar -xzf "dist/$ARCHIVE" -C "$WORKDIR"
for file in trapline install.sh trapline.service README.md LICENSE; do
  [ -f "$WORKDIR/$file" ] || fail "the archive does not contain $file"
done
cmp -s "$WORKDIR/install.sh" scripts/install.sh \
  || fail "the install.sh in the archive is not the one in scripts/"
cmp -s "$WORKDIR/trapline.service" deploy/trapline.service \
  || fail "the unit in the archive is not the one in deploy/"
ok "binary, installer, unit, README and LICENSE, all matching the tree"

# --- serve it, twice ----------------------------------------------------
#
# Two roots: the release as built, and the release with one byte changed. The
# second exists because a checksum verification that has never rejected
# anything is indistinguishable from one that does nothing, and install.sh's
# own comment says so.
step "serving the release on port $PORT"
mkdir -p "$WORKDIR/www/good" "$WORKDIR/www/tampered"
cp "dist/$ARCHIVE" dist/checksums.txt "$WORKDIR/www/good/"
cp "dist/$ARCHIVE" dist/checksums.txt "$WORKDIR/www/tampered/"
printf 'x' >>"$WORKDIR/www/tampered/$ARCHIVE"

# Bound to every interface because the container reaches the host through the
# gateway address, not loopback. It serves a public release artifact for the
# length of this script.
python3 -m http.server "$PORT" --bind 0.0.0.0 --directory "$WORKDIR/www" \
  >"$WORKDIR/http.log" 2>&1 &
SERVER_PID=$!

for _ in $(seq 1 50); do
  curl -fsS "http://127.0.0.1:$PORT/good/checksums.txt" >/dev/null 2>&1 && break
  sleep 0.1
done
curl -fsS "http://127.0.0.1:$PORT/good/checksums.txt" >/dev/null \
  || fail "the local release server never came up"
ok "http://127.0.0.1:$PORT"

# --- the container script -----------------------------------------------
cat >"$WORKDIR/verify.sh" <<'CONTAINER'
#!/bin/sh
# Runs inside a container that has never heard of this project.
set -eu
VERSION="$1"
BASE="$2"

fail() { printf '   FAIL %s\n' "$1" >&2; exit 1; }
ok()   { printf '   ok   %s\n' "$1"; }

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null
# curl and tar only: exactly what install.sh says it needs. systemd comes
# later, deliberately after the install, so that this first run really is on a
# machine with nothing on it.
apt-get install -y -qq --no-install-recommends curl ca-certificates >/dev/null 2>&1

command -v trapline >/dev/null 2>&1 && fail "trapline was already installed"
ok "clean machine: curl and tar, no trapline"

# --- the checksum has to bite -------------------------------------------
if TRAPLINE_BASE_URL="$BASE/tampered" sh /install.sh --version "$VERSION" \
     >/tmp/tampered.log 2>&1; then
  fail "a tampered archive installed successfully"
fi
grep -q 'checksum mismatch' /tmp/tampered.log \
  || { cat /tmp/tampered.log; fail "the tampered archive failed for the wrong reason"; }
[ -e /usr/local/bin/trapline ] && fail "a rejected download still left a binary behind"
ok "a tampered archive is refused, and installs nothing"

# --- the real thing ------------------------------------------------------
TRAPLINE_BASE_URL="$BASE/good" sh /install.sh --version "$VERSION" \
  >/tmp/install.log 2>&1 || { cat /tmp/install.log; fail "install.sh failed"; }
grep -q 'checksum verified' /tmp/install.log \
  || { cat /tmp/install.log; fail "the checksum was not verified"; }
ok "checksum verified against checksums.txt"

[ -x /usr/local/bin/trapline ] || fail "no executable at /usr/local/bin/trapline"
ok "installed at /usr/local/bin/trapline"

installed="$(trapline version)"
[ "$installed" = "$VERSION" ] || fail "trapline version said '$installed', want '$VERSION'"
ok "trapline version reports $installed"

# Piped into sh there is nobody to ask, and install.sh promises it will not
# install a system service unasked. That promise is only worth something if
# something checks it.
[ -e /etc/systemd/system/trapline.service ] \
  && fail "the service was installed without being asked for"
ok "no service installed without being asked"

# --- the service path ----------------------------------------------------
apt-get install -y -qq --no-install-recommends systemd >/dev/null 2>&1

# systemctl cannot reload a system that was never booted, which is a fact
# about containers rather than about the installer. The stub stands in for
# that one call; everything it was supposed to cause is asserted below on the
# real files. /usr/local/sbin precedes /usr/bin on Debian's root PATH.
mkdir -p /usr/local/sbin
cat >/usr/local/sbin/systemctl <<'STUB'
#!/bin/sh
echo "systemctl $*" >>/tmp/systemctl.log
STUB
chmod +x /usr/local/sbin/systemctl
hash -r 2>/dev/null || true

TRAPLINE_BASE_URL="$BASE/good" sh /install.sh --version "$VERSION" --service \
  >/tmp/install-service.log 2>&1 \
  || { cat /tmp/install-service.log; fail "install.sh --service failed"; }

[ -f /etc/systemd/system/trapline.service ] || fail "the unit was not installed"
cmp -s /etc/systemd/system/trapline.service /reference/trapline.service \
  || fail "the installed unit differs from deploy/trapline.service"
mode="$(stat -c '%a' /etc/systemd/system/trapline.service)"
[ "$mode" = "644" ] || fail "the unit is mode $mode, want 644"
ok "unit installed at /etc/systemd/system/trapline.service, mode 644"

[ -f /etc/trapline/trapline.env ] || fail "no /etc/trapline/trapline.env was created"
grep -q 'TRAPLINE_ORIGIN' /etc/trapline/trapline.env \
  || fail "the environment file does not mention TRAPLINE_ORIGIN"
ok "environment file created, naming the one setting that must be set"

grep -q 'daemon-reload' /tmp/systemctl.log || fail "systemctl daemon-reload was never called"
ok "systemctl daemon-reload was called"

# Installing over an existing install must not have broken it.
trapline version >/dev/null || fail "the binary stopped working after a second install"
ok "installing twice is safe"

# --- is the unit actually valid ------------------------------------------
#
# systemd-analyze is the only thing that knows whether twenty lines of
# hardening directives parse, and it checks the ExecStart binary exists — which
# it does here, because install.sh just put it there.
if command -v systemd-analyze >/dev/null 2>&1; then
  systemd-analyze verify /etc/systemd/system/trapline.service 2>/tmp/verify.log || {
    cat /tmp/verify.log; fail "systemd-analyze rejected the unit"; }
  if [ -s /tmp/verify.log ]; then
    cat /tmp/verify.log
    fail "systemd-analyze had something to say about the unit"
  fi
  ok "systemd-analyze verify is clean"
else
  printf '   --   systemd-analyze not available; the unit was not validated\n'
fi
CONTAINER

# --- run it -------------------------------------------------------------
step "installing on a clean $TARGET_IMAGE"
docker run --rm --name "$DOCKER_PREFIX-install" \
  --add-host=host.docker.internal:host-gateway \
  -v "$ROOT/scripts/install.sh:/install.sh:ro" \
  -v "$ROOT/deploy:/reference:ro" \
  -v "$WORKDIR/verify.sh:/verify.sh:ro" \
  "$TARGET_IMAGE" /bin/sh /verify.sh "$VERSION" "http://host.docker.internal:$PORT" \
  || fail "the installation did not pass"

printf '\n\033[1;32minstall.sh installs a real release, and refuses a tampered one\033[0m\n'
