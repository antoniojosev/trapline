#!/usr/bin/env bash
#
# Assemble changelog.d/ into CHANGELOG.md.
#
#   ./scripts/changelog.sh                  preview on stdout, changes nothing
#   ./scripts/changelog.sh v0.1.0           cut the release: write it in and
#                                           delete the fragments it consumed
#
# The fragments exist because CHANGELOG.md was a conflict magnet: branches
# all appended to the same three lines, and every rebase meant merging prose
# by hand. One file per change never collides, and the order they are pasted
# in is decided here rather than by whoever rebased last.
#
# Order is the filenames, sorted. Not the file dates: mtimes do not survive a
# clone and commit dates need git, so neither is reproducible from a tarball.
# Name the fragments so that sorting them sorts history (a date or a sequence
# number in front does it).
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

FRAGMENTS=changelog.d
CHANGELOG=CHANGELOG.md
VERSION="${1:-}"

fail() { printf 'changelog: %s\n' "$1" >&2; exit 1; }

[ -d "$FRAGMENTS" ] || fail "$FRAGMENTS/ does not exist"

# README.md documents the directory; it is not a fragment.
mapfile -t files < <(find "$FRAGMENTS" -maxdepth 1 -name '*.md' ! -name 'README.md' -print | sort)

if [ "${#files[@]}" -eq 0 ]; then
  fail "no fragments in $FRAGMENTS/ — nothing to release"
fi

for file in "${files[@]}"; do
  # A fragment that is only whitespace would silently contribute a blank
  # stretch to a published changelog, which is worse than a loud failure now.
  if ! grep -q '[^[:space:]]' "$file"; then
    fail "$file is empty"
  fi
  if ! head -1 "$file" | grep -q '^#\{2,3\} '; then
    fail "$file must start with a '## ' or '### ' heading (see CONTRIBUTING.md)"
  fi
done

heading="## [No publicado]"
if [ -n "$VERSION" ]; then
  heading="## [$VERSION] — $(date -u +%Y-%m-%d)"
fi

section=$(
  printf '%s\n' "$heading"
  for file in "${files[@]}"; do
    printf '\n'
    # Trailing blank lines in a fragment would compound into gaps here.
    sed -e :a -e '/^\n*$/{$d;N;ba' -e '}' "$file"
  done
)

if [ -z "$VERSION" ]; then
  printf '%s\n' "$section"
  printf '\n--- %d fragment(s); pass a version to write this into %s\n' \
    "${#files[@]}" "$CHANGELOG" >&2
  exit 0
fi

[ -f "$CHANGELOG" ] || fail "$CHANGELOG is missing"

# The header is everything before the first '## ', and it is preserved
# verbatim: it explains the format and the versioning promise, and a generator
# that rewrote it would quietly drop whatever a human added there.
header=$(sed -n '1,/^## /{/^## /!p;}' "$CHANGELOG")

# Everything from the first released section onwards, i.e. the file minus its
# header and minus the "[No publicado]" placeholder block.
rest=$(awk '
  /^## / { started = 1 }
  !started { next }
  /^## \[No publicado\]/ { skipping = 1; next }
  skipping && /^## / { skipping = 0 }
  skipping { next }
  { print }
' "$CHANGELOG")

{
  printf '%s\n\n' "$header"
  printf '%s\n' "$section"
  if grep -q '[^[:space:]]' <<<"$rest"; then
    printf '\n%s\n' "$(printf '%s' "$rest" | sed -e :a -e '/^\n*$/{$d;N;ba' -e '}')"
  fi
} > "$CHANGELOG.tmp" && mv "$CHANGELOG.tmp" "$CHANGELOG"

rm -f "${files[@]}"

printf 'changelog: %s written from %d fragment(s); the fragments are gone.\n' \
  "$VERSION" "${#files[@]}"
printf 'changelog: review %s, then commit both the file and the deletions.\n' "$CHANGELOG"
