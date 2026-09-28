#!/usr/bin/env sh
#
# Installs trapline. Intended to be read before it is run.
#
# It is deliberately POSIX sh, does one thing per step, and prints what it is
# about to do. A `curl | sh` installer is a lot of trust to ask for; the least
# it can do is be short enough to audit in a minute.
#
#   curl -fsSL https://<host>/install.sh | sh
#   curl -fsSL https://<host>/install.sh | sh -s -- --version v0.2.0
#
# TRAPLINE_BASE_URL overrides where the archive and checksums are fetched from
# — a mirror, an air-gapped copy, or the snapshot build that scripts/install-
# test.sh serves to prove this file works. It is a directory: the archive and
# checksums.txt sit directly inside it.
set -eu

REPO="${TRAPLINE_REPO:-antoniojosev/trapline}"
VERSION="${TRAPLINE_VERSION:-latest}"
BIN_DIR="${TRAPLINE_BIN_DIR:-/usr/local/bin}"
INSTALL_SERVICE="${TRAPLINE_INSTALL_SERVICE:-ask}"
BASE_URL="${TRAPLINE_BASE_URL:-}"

while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="$2"; shift 2 ;;
    --bin-dir) BIN_DIR="$2"; shift 2 ;;
    --service) INSTALL_SERVICE=yes; shift ;;
    --no-service) INSTALL_SERVICE=no; shift ;;
    -h|--help)
      sed -n '3,15p' "$0" | sed 's/^# \{0,1\}//'
      exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

say()  { printf '%s\n' "$*"; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required"; }

need curl
need tar
need uname

# --- platform -----------------------------------------------------------
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$os" in
  linux|darwin) ;;
  *) die "unsupported operating system: $os" ;;
esac

arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "unsupported architecture: $arch" ;;
esac

# --- resolve version ----------------------------------------------------
if [ "$VERSION" = latest ] && [ -n "$BASE_URL" ]; then
  # "latest" is a GitHub concept. A plain directory has no opinion about which
  # of its files is newest, and guessing would install whatever sorted last.
  die "TRAPLINE_BASE_URL is set, so the version cannot be resolved automatically; pass --version"
fi

if [ "$VERSION" = latest ]; then
  say "resolving the latest release..."
  VERSION="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
    | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1)"
  [ -n "$VERSION" ] || die "could not resolve the latest version; pass --version"
fi
say "installing trapline $VERSION for $os/$arch"

# --- download and verify ------------------------------------------------
tmp="$(mktemp -d)"
# shellcheck disable=SC2064 # expand tmp now: it is what must be cleaned up.
trap "rm -rf '$tmp'" EXIT INT TERM

# Resolved here rather than at the top because the default contains the
# version, and the version is only known once "latest" has been looked up.
base="${BASE_URL:-https://github.com/$REPO/releases/download/$VERSION}"
archive="trapline_${VERSION#v}_${os}_${arch}.tar.gz"

say "downloading $archive"
curl -fsSL "$base/$archive" -o "$tmp/$archive" || die "download failed"

# Checksums are verified when the tooling is available, and skipped loudly
# rather than silently when it is not. A verification that quietly does
# nothing is worse than none, because it is believed.
if curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt" 2>/dev/null; then
  if command -v sha256sum >/dev/null 2>&1; then
    ( cd "$tmp" && grep " $archive\$" checksums.txt | sha256sum -c - ) \
      || die "checksum mismatch — do not use this download"
    say "checksum verified"
  elif command -v shasum >/dev/null 2>&1; then
    ( cd "$tmp" && grep " $archive\$" checksums.txt | shasum -a 256 -c - ) \
      || die "checksum mismatch — do not use this download"
    say "checksum verified"
  else
    say "WARNING: no sha256sum or shasum found; skipping checksum verification"
  fi
else
  say "WARNING: no checksums.txt published for $VERSION; skipping verification"
fi

tar -xzf "$tmp/$archive" -C "$tmp"
[ -f "$tmp/trapline" ] || die "the archive does not contain an trapline binary"

# --- install ------------------------------------------------------------
if [ -w "$BIN_DIR" ]; then
  sudo=""
elif command -v sudo >/dev/null 2>&1; then
  sudo="sudo"
  say "$BIN_DIR is not writable; using sudo"
else
  die "$BIN_DIR is not writable and sudo is unavailable; pass --bin-dir"
fi

$sudo install -m 0755 "$tmp/trapline" "$BIN_DIR/trapline"
say "installed $BIN_DIR/trapline"
"$BIN_DIR/trapline" version >/dev/null || die "the installed binary does not run"

# --- optional service ---------------------------------------------------
if [ "$INSTALL_SERVICE" = ask ]; then
  if [ -t 0 ] && command -v systemctl >/dev/null 2>&1; then
    printf 'install the systemd service? [y/N] '
    read -r answer
    case "$answer" in y|Y|yes) INSTALL_SERVICE=yes ;; *) INSTALL_SERVICE=no ;; esac
  else
    # Piped into sh there is no one to ask, and installing a system service
    # without being asked is not a default anyone should have to discover.
    INSTALL_SERVICE=no
  fi
fi

if [ "$INSTALL_SERVICE" = yes ]; then
  command -v systemctl >/dev/null 2>&1 || die "systemctl not found"
  [ -f "$tmp/trapline.service" ] || die "the archive has no unit file"

  $sudo install -m 0644 "$tmp/trapline.service" /etc/systemd/system/trapline.service
  $sudo mkdir -p /etc/trapline
  if [ ! -f /etc/trapline/trapline.env ]; then
    printf '%s\n' \
      '# The public address SDKs will send events to. Required in any real' \
      '# deployment: the server cannot infer it, and a wrong value hands out' \
      '# DSNs that only work on this machine.' \
      '# TRAPLINE_ORIGIN=https://errors.example.com' \
      '' \
      '# TRAPLINE_ADDR=127.0.0.1:9000' \
      '' \
      '# Addresses or CIDR blocks whose X-Forwarded-For is believed. Behind a' \
      '# reverse proxy this is what makes per-IP limits able to tell clients' \
      '# apart; left empty, every visitor is counted as the proxy.' \
      '# TRAPLINE_TRUSTED_PROXIES=127.0.0.1' \
      '' \
      '# TRAPLINE_INGEST_IP_RATE_LIMIT=48000' \
      '# TRAPLINE_AUTH_RATE_LIMIT=10' \
      | $sudo tee /etc/trapline/trapline.env >/dev/null
  fi
  $sudo systemctl daemon-reload
  say ""
  say "service installed but not started. Set TRAPLINE_ORIGIN first:"
  say "  sudo \$EDITOR /etc/trapline/trapline.env"
  say "  sudo systemctl enable --now trapline"
else
  say ""
  say "next:"
  say "  trapline serve -origin https://errors.example.com"
fi

say ""
say "then open the address in a browser to create the first admin account."
