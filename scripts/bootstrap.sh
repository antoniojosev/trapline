#!/usr/bin/env bash
#
# Prepare a working copy so `make check` and the gates can run.
#
# Idempotent by construction: every step checks whether it already happened and
# says so instead of doing it again. Running this twice costs a second.
#
# It is also deliberately non-fatal about tools that only some gates need. A
# machine without Docker can still run `make check`; telling that person their
# bootstrap "failed" teaches them to ignore the output, which is how a real
# failure gets missed later. So the rule is: exit non-zero only if the tree
# cannot build and test, and end with a list of what is missing and the exact
# command that fixes each one.
#
#   ./scripts/bootstrap.sh
#
# The version numbers are read from the repository — go.mod for the toolchain,
# the Makefile for the linter — so this file never becomes the second place a
# version is written down and the first place it goes stale.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

bold=$'\033[1m'; reset=$'\033[0m'
green=$'\033[32m'; yellow=$'\033[33m'; red=$'\033[31m'

step()  { printf '\n%s== %s%s\n' "$bold" "$1" "$reset"; }
ok()    { printf '   %sok%s   %s\n' "$green" "$reset" "$1"; }
skip()  { printf '   --   %s\n' "$1"; }

# Two ways of being wrong, kept apart because they call for different reactions.
# A gap stops a gate from running; a blocker stops the build.
gaps=()
blockers=()
gap()     { printf '   %swarn%s %s\n' "$yellow" "$reset" "$1"; gaps+=("$2"); }
blocker() { printf '   %sFAIL%s %s\n' "$red" "$reset" "$1"; blockers+=("$2"); }

have() { command -v "$1" >/dev/null 2>&1; }

# --- Go -----------------------------------------------------------------
step "Go"

want_go="$(sed -n 's/^toolchain go//p' go.mod)"
[ -n "$want_go" ] || want_go="$(sed -n 's/^go //p' go.mod)"

if ! have go; then
  blocker "go is not installed; the whole build needs it" \
    "install Go $want_go from https://go.dev/dl/"
else
  have_go="$(go env GOVERSION 2>/dev/null | sed 's/^go//')"
  if [ "$have_go" = "$want_go" ]; then
    ok "go $have_go, the pinned toolchain"
  else
    # Not a blocker: Go fetches the pinned toolchain on demand, so a different
    # local version still builds. It is worth naming because govulncheck
    # results depend on which toolchain actually compiled the code, and go.mod
    # explains why this one is pinned.
    gap "go $have_go installed, go.mod pins $want_go" \
      "install Go $want_go, or let the toolchain directive fetch it (GOTOOLCHAIN=auto)"
  fi

  if go build ./... >/dev/null 2>&1; then
    ok "the tree builds"
  else
    blocker "the tree does not build; run 'go build ./...' to see why" \
      "fix the build before anything else here is useful"
  fi
fi

# --- Go tooling ---------------------------------------------------------
step "Go tooling"

want_lint="$(sed -n 's/^LINT_VERSION *:= *//p' Makefile)"
lint_ok=false
if have golangci-lint; then
  have_lint="v$(golangci-lint version --short 2>/dev/null | sed 's/^v//')"
  [ "$have_lint" = "$want_lint" ] && lint_ok=true
fi

vuln_ok=false
have govulncheck && vuln_ok=true

# gosec is what `make hardening` runs; it is not needed by `make check`, so its
# absence is a gap and never a blocker.
want_gosec="$(sed -n 's/^GOSEC_VERSION *:= *//p' Makefile)"
gosec_ok=false
have gosec && gosec_ok=true

if $lint_ok && $vuln_ok && $gosec_ok; then
  ok "golangci-lint $want_lint, govulncheck and gosec $want_gosec are installed"
elif have go; then
  printf '   installing the pinned tooling (make tools)...\n'
  if make tools >/dev/null 2>&1; then
    ok "golangci-lint $want_lint, govulncheck and gosec $want_gosec installed"
  else
    gap "make tools failed; 'make lint' and 'make vuln' will not run" \
      "run 'make tools' and read the error"
  fi
  # go install puts them in GOBIN, which is not on every PATH by default.
  if ! have golangci-lint || ! have govulncheck || ! have gosec; then
    gap "$(go env GOPATH)/bin is not on PATH, so the tools are installed but not callable" \
      "add \"export PATH=\\\$PATH:\$(go env GOPATH)/bin\" to your shell profile"
  fi
else
  skip "no Go, so the Go tooling cannot be installed"
fi

# --- Node ---------------------------------------------------------------
#
# Node is needed to rebuild the panel and to run the Node compatibility suite.
# It is not needed to build the product: internal/adapters/webui/dist is
# committed precisely so that `go build` never requires npm.
step "Node"

if ! have npm; then
  gap "npm is not installed; 'make web' and the Node part of 'make compat' will not run" \
    "install Node 24 (nodejs.org, or your version manager)"
else
  ok "node $(node --version), npm $(npm --version)"
  for dir in web compat/node; do
    if [ ! -f "$dir/package-lock.json" ]; then
      skip "$dir has no package-lock.json"
      continue
    fi
    # npm ci wipes and reinstalls unconditionally, which would make this script
    # expensive to run twice. The lockfile being newer than the install is the
    # cheap, correct signal that it needs to happen again.
    if [ -d "$dir/node_modules" ] && [ ! "$dir/package-lock.json" -nt "$dir/node_modules" ]; then
      ok "$dir dependencies are current"
      continue
    fi
    printf '   installing %s dependencies...\n' "$dir"
    if ( cd "$dir" && npm ci --no-fund --no-audit --silent ); then
      # node_modules keeps the mtime npm gave it, which can predate the
      # lockfile; touching it is what makes the check above conclusive.
      touch "$dir/node_modules"
      ok "$dir dependencies installed"
    else
      gap "npm ci failed in $dir" "run 'cd $dir && npm ci' and read the error"
    fi
  done
fi

# --- Python -------------------------------------------------------------
step "Python"

if have python3; then
  ok "$(python3 --version)"
  skip "the compat suite builds its own venv on first run (compat/python/run.sh)"
else
  gap "python3 is not installed; the Python compat suite and install-test.sh will not run" \
    "install Python 3.12 or newer"
fi

# --- Docker -------------------------------------------------------------
step "Docker"

if ! have docker; then
  gap "docker is not installed; install-test.sh and the full compat matrix will not run" \
    "install Docker Engine (docs.docker.com/engine/install)"
elif ! docker info >/dev/null 2>&1; then
  gap "docker is installed but the daemon is not reachable" \
    "start Docker, or add yourself to the docker group and log in again"
else
  ok "docker $(docker version --format '{{.Server.Version}}' 2>/dev/null), daemon reachable"
fi

# --- Summary ------------------------------------------------------------
step "summary"

if [ ${#blockers[@]} -eq 0 ] && [ ${#gaps[@]} -eq 0 ]; then
  printf '   %severything the gates need is here%s\n\n   next: make check\n\n' "$green" "$reset"
  exit 0
fi

for item in "${blockers[@]}"; do
  printf '   %sblocking%s  %s\n' "$red" "$reset" "$item"
done
for item in "${gaps[@]}"; do
  printf '   %soptional%s  %s\n' "$yellow" "$reset" "$item"
done

if [ ${#blockers[@]} -gt 0 ]; then
  printf '\n   %d thing(s) must be fixed before the tree builds.\n\n' "${#blockers[@]}"
  exit 1
fi

printf '\n   %s can run; the gates listed above cannot until you fix them.\n\n' "make check"
exit 0
