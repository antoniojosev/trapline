#!/usr/bin/env bash
#
# The repository `set-commits --local` reads.
#
# Sourced by record.sh and verify.sh so both send the same commits: a recording
# taken from one tree and a verification run against another would agree on
# nothing, and the disagreement would look like a protocol change.
#
# Identity and dates are fixed, so the shas are the same on every machine and a
# re-recording diffs as protocol drift rather than as somebody else's clock.

build_fixture_repo() {
  repo="$1"

  export GIT_AUTHOR_NAME="Ana" GIT_AUTHOR_EMAIL="ana@example.test"
  export GIT_COMMITTER_NAME="Ana" GIT_COMMITTER_EMAIL="ana@example.test"
  export GIT_AUTHOR_DATE="2026-08-01T10:00:00+00:00"
  export GIT_COMMITTER_DATE="2026-08-01T10:00:00+00:00"

  mkdir -p "$repo/app"
  git -C "$repo" init -q -b main
  git -C "$repo" config user.name "Ana"
  git -C "$repo" config user.email "ana@example.test"

  printf 'def checkout():\n    return 0\n' >"$repo/app/views.py"
  printf 'venekambio\n' >"$repo/README.md"
  git -C "$repo" add -A
  git -C "$repo" commit -q -m "the checkout view"

  # A commit that both modifies and adds, so the patch set carries more than
  # one change type. A repository where every file is added would never have
  # proved that M survives the round trip.
  printf 'def checkout():\n    return total()\n' >"$repo/app/views.py"
  printf 'def total():\n    return 0\n' >"$repo/app/total.py"
  git -C "$repo" add -A
  git -C "$repo" commit -q -m "fix the checkout total"

  # And a deletion, for D.
  git -C "$repo" rm -q README.md
  git -C "$repo" commit -q -m "drop the placeholder readme"
}
