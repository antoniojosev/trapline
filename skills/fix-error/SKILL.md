---
name: fix-error
description: Fix a production error reported by trapline (the self-hosted error tracker). Use when the user asks to fix, investigate, triage or explain an issue by its id or its title, pastes an issue URL, or says something like "fix error 42", "why is checkout crashing", "what broke in production". Reads the issue bundle over MCP or the CLI, finds the code, proposes a patch, runs the project's tests, and marks the issue resolved in the next release. Never pushes and never deploys.
---

# Fix a production error

You are fixing one real error that happened in production. Everything you need
about it is one call away: `trapline` serves a whole issue as a markdown
document — stacktrace, breadcrumbs, frequency, release history, suspect
commits. Read that first, and only then open the repository.

## Hard limits

These are not style preferences. Breaking one turns a helpful patch into an
incident.

- **Never `git push`. Never deploy. Never touch a remote.** You produce a
  patch in the working tree and a commit at most, and only if the user asked
  for a commit. What reaches production is a person's decision.
- **Never mark an issue resolved before its test passes.** `ignore_issue`
  exists for "not worth fixing"; `resolve_issue` is a claim about the code.
- **Never edit an unrelated file to make a test pass.** If the failure is
  elsewhere, say so and stop.
- **Never invent the stacktrace.** If the bundle has no frames, say what is
  missing (usually source maps) instead of guessing at the code.
- **Do not run destructive commands against the installation**: no
  `projects delete`, no `keys revoke`, no `retention`, no `backup` overwriting
  anything.

## 0. What you need

```sh
export TRAPLINE_URL=https://errors.example.com
export TRAPLINE_TOKEN=ek_...
```

If the MCP server is connected, use its tools — they are the same API. If it
is not, every step below has a CLI equivalent and they return the same bytes.

If `TRAPLINE_TOKEN` is missing, stop and ask for one. Do not look for
credentials in the repository.

## 1. Identify the issue

If the user gave you an id, you have it. If they gave you words:

```sh
trapline issues list -project <project> -status unresolved -q "<their words>" --json
```

MCP: `list_issues(project, status: "unresolved", q: "...")`.

If more than one matches, show the candidates with their titles, last-seen
times and event counts, and ask which one. Do not pick for them: two issues
with similar titles are usually two different bugs, and fixing the wrong one
wastes the whole turn.

## 2. Read the bundle — before opening any file

```sh
trapline issues bundle -project <project> -issue <issue>
```

MCP: `get_issue_bundle(project, issue)`.

This is the step that makes the rest cheap. One document contains:

- the exception type and message, and the frame where it failed;
- the stacktrace, **symbolicated**, with code context around each frame, and
  the minified line each frame came from when source maps are in play;
- breadcrumbs: what the program did just before;
- tags and contexts aggregated across occurrences — which server, which
  browser, which user segment;
- how often: 24 h and 14 d, read from the hourly aggregates;
- release lifecycle: first seen in which release, resolved in which, how many
  regressions;
- **suspect commits**: which commit touched the files the stacktrace names,
  and why that one.

Read all of it before you form a hypothesis. The most common failure mode here
is patching the frame at the top of the stack when the breadcrumbs say the bad
value arrived three steps earlier.

If the bundle says the stacktrace is minified and no source maps are uploaded,
stop and say so: fixing a `t.n is not a function` in a bundle is guesswork, and
the fix is `trapline artifacts upload` in their build pipeline.

## 3. Find the code

Use the frames as the search, in order, starting from the one that failed:

- `filename` and `lineno` from the top `in_app` frame;
- the function name, if the file has moved since;
- the suspect commit's path, if the file name is ambiguous.

Read the surrounding function, not just the line. The bundle gives you the
line; the repository gives you what it was supposed to do.

If the file does not exist in this checkout, check the release in the bundle
against the current branch. You may be looking at a bug that was already
fixed, or at a different service's repository.

## 4. Understand before patching

Write down, for yourself, three things:

1. **The trigger**: what input or state reaches that line and breaks it. The
   tags and contexts usually say — one environment, one server, one locale.
2. **Why it was not caught**: no validation, a wrong assumption about a type,
   a race, a nil that the caller was allowed to pass.
3. **The blast radius**: who else calls this, and whether they rely on the
   current (broken) behaviour.

If you cannot answer all three, you do not understand the bug yet. Say what is
missing and ask, rather than patching the symptom.

## 5. Write a test that fails

Find the project's test convention first — look at a neighbouring test file,
not at what you would write by default. Then write the smallest test that
reproduces the error from the bundle, and **run it and see it fail**. A test
you did not see fail has not proven anything.

If the code has no test suite at all, say so and continue; a fix without a
test is still a fix, but flag it as one.

## 6. Patch

- Fix the cause, in the place the three answers in §4 point at.
- Keep it small enough to review in one sitting. A refactor is a separate
  conversation.
- Match the surrounding code's style and error-handling convention.
- If the fix needs a decision the user has to make (change an API, drop a
  field, accept a slower path), stop and present the options.

## 7. Run the project's tests

The whole suite, the way that project runs it — `make test`, `npm test`,
`pytest`, whatever the repository's own documentation says. Not just your new
test: the value of running the suite is the tests you did not think about.

If something unrelated breaks, **do not fix it to get green**. Report it
separately.

## 8. Close the loop

Only after the suite passes:

```sh
trapline issues resolve -project <project> -issue <issue> -next-release
```

MCP: `resolve_issue(project, issue, in_next_release: true)`.

`-next-release` is almost always the right flag. It says "fixed, and the fix
ships next", which means events that keep arriving from the *current* release
are the machines that have not been redeployed yet — they get counted and
shown, and they do **not** reopen the issue. A plain resolve would reopen it
on the next event from any release, which looks exactly like a fix that did not
work.

Use a plain `resolve` only when the fix is already live (a config change, a
revert that is deployed). Use `ignore` when the conclusion is "this is noise",
and say that out loud rather than resolving something you did not fix.

## 9. Report back

Tell the user, in this order:

1. **What was broken** — one sentence, in terms of what the user of their
   software experienced, not in terms of the stack frame.
2. **Why** — the trigger from §4.
3. **What you changed** — the files, and the shape of the change.
4. **What you ran** — the test you added and the suite result.
5. **What you did to the issue** — resolved in next release, and what that
   means for the events that will keep arriving until they deploy.
6. **What you did not do** — you did not push and you did not deploy. Say it,
   so nobody assumes otherwise.

If the suspect commit pointed at someone's change, name the commit, not the
person.

## When to stop instead of continuing

Stop, explain and ask when:

- the bundle has no `in_app` frames, or the stacktrace is minified with no
  source maps;
- the fix would change a public API, a database schema, or behaviour other
  code depends on;
- the error is in a dependency rather than in this repository;
- the test suite was already failing before you touched anything;
- the issue turns out to be several different bugs grouped together — say so,
  and fix one.

Stopping with a clear question is a good outcome. A patch that makes the
symptom disappear without explaining the cause is not.
