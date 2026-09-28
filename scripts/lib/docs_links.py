#!/usr/bin/env python3
"""Every internal link in the documentation points at something that exists.

A broken link is the cheapest documentation bug to make and one of the most
expensive to find: nothing fails, nothing is measured, and the person who hits
it is usually the one who has not yet decided whether to install this at all.

Python rather than shell for one reason: anchors. GitHub's heading slugs are
Unicode-aware (`#continuación-2026-08-29` is a real link in this repository),
and doing that correctly in `sed` depends on the locale the runner happened to
set — which is the kind of gate that passes on a laptop and fails in CI for a
reason nobody can reproduce.

What it deliberately does NOT check:

  * external URLs. A gate that goes out to the network fails for reasons that
    are not its own and ends up switched off, which is worse than not having
    it (the same reasoning as the two tiers of the SDK matrix, ADR 002).
  * links that resolve outside the repository root. `../../security/advisories
    /new` in SECURITY.md is a GitHub-relative URL, correct on github.com and
    meaningless on disk.

Usage: scripts/lib/docs_links.py [root]   (default: the repository root)
"""

from __future__ import annotations

import os
import re
import subprocess
import sys
import unicodedata

# Inline links: ](target). Reference definitions and autolinks are not used
# anywhere in this repository; if they ever are, this is where they go.
LINK = re.compile(r"\]\(\s*([^)\s]+)")
# ``` or ~~~ fences, at any indentation.
FENCE = re.compile(r"^\s*(```|~~~)")
INLINE_CODE = re.compile(r"`[^`]*`")
ATX_HEADING = re.compile(r"^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$")
SKIP_SCHEMES = ("http://", "https://", "mailto:", "ftp://", "data:")


def strip_code(text: str) -> list[tuple[int, str]]:
    """Return (line number, line) for every line outside a fenced block.

    Inline code spans are blanked rather than dropped: a link on the same line
    as a code span is still a link.
    """
    out: list[tuple[int, str]] = []
    fence: str | None = None
    for number, line in enumerate(text.splitlines(), start=1):
        match = FENCE.match(line)
        if fence is not None:
            if match and match.group(1) == fence:
                fence = None
            continue
        if match:
            fence = match.group(1)
            continue
        out.append((number, INLINE_CODE.sub("", line)))
    return out


def slug(heading: str) -> str:
    """GitHub's heading slug, as far as this repository needs it."""
    text = re.sub(r"`([^`]*)`", r"\1", heading)          # `code` -> code
    text = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", text)  # [a](b) -> a
    text = re.sub(r"[*_~]", "", text)                     # **bold**, _em_
    text = text.strip().lower()
    kept = []
    for char in text:
        if char in " -_":
            kept.append("-" if char == " " else char)
        elif unicodedata.category(char)[0] in ("L", "N"):
            kept.append(char)
    return "".join(kept)


def anchors(path: str) -> set[str]:
    with open(path, encoding="utf-8") as handle:
        body = handle.read()
    found: set[str] = set()
    for _, line in strip_code(body):
        match = ATX_HEADING.match(line)
        if not match:
            continue
        base = slug(match.group(2))
        if not base:
            continue
        # GitHub disambiguates repeats with -1, -2, ...
        candidate, n = base, 0
        while candidate in found:
            n += 1
            candidate = f"{base}-{n}"
        found.add(candidate)
    return found


def main() -> int:
    root = os.path.abspath(sys.argv[1] if len(sys.argv) > 1 else ".")
    listed = subprocess.run(
        # --others --exclude-standard so a document still being written is
        # checked before it is committed, which is when the link is wrong.
        ["git", "-C", root, "ls-files", "--cached", "--others",
         "--exclude-standard", "*.md", "docs/agents/llms.txt"],
        capture_output=True, text=True, check=True,
    ).stdout.split()

    anchor_cache: dict[str, set[str]] = {}
    problems: list[str] = []
    checked = 0

    for relative in listed:
        path = os.path.join(root, relative)
        with open(path, encoding="utf-8") as handle:
            body = handle.read()
        for number, line in strip_code(body):
            for target in LINK.findall(line):
                if target.startswith(SKIP_SCHEMES):
                    continue
                file_part, _, anchor = target.partition("#")
                resolved = path if not file_part else os.path.normpath(
                    os.path.join(os.path.dirname(path), file_part))
                # Outside the tree: a GitHub-relative URL, not a file.
                if os.path.relpath(resolved, root).startswith(".."):
                    continue
                checked += 1
                if not os.path.exists(resolved):
                    problems.append(
                        f"{relative}:{number}: {target} -> no such file")
                    continue
                if not anchor:
                    continue
                if not resolved.endswith(".md"):
                    problems.append(
                        f"{relative}:{number}: {target} -> "
                        "anchors only exist in markdown files")
                    continue
                if resolved not in anchor_cache:
                    anchor_cache[resolved] = anchors(resolved)
                if anchor not in anchor_cache[resolved]:
                    problems.append(
                        f"{relative}:{number}: {target} -> no such heading")

    for problem in problems:
        print(f"   FAIL {problem}")
    if problems:
        print(f"\n{len(problems)} broken link(s) out of {checked} checked")
        return 1
    print(f"   ok   {checked} internal links in {len(listed)} files resolve")
    return 0


if __name__ == "__main__":
    sys.exit(main())
