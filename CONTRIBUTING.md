# Contributing

## License commitment

This goes first because it is the question that matters when you decide to
invest your time in a project.

This product is **AGPL-3.0**, and the event engine, once extracted as a
library, will be **Apache-2.0**.

**The commitments, specifically:**

1. **No published release ever changes license.** What was published as AGPL
   stays AGPL forever.
2. **The core is not relicensed** to BSL, FSL, SSPL, the Elastic License or any
   other license not approved by the OSI. That move (go open source, build a
   community, close the door when revenue pressure arrives) is precisely what
   gives this project its reason to exist. Doing it would mean becoming the
   villain of its own argument.
3. **Self-hosted gets all the observability, with nothing cut.** No feature is
   ever removed from the free binary to push the paid one.
4. **The revenue plan is explicit**, not a future surprise: managed hosting,
   and features a single developer does not need (SSO/SAML, organizations and
   granular permissions, long retention). The hosted service's control plane
   is a separate program that talks to this one through its public API. It does
   not live in this repo and does not affect its license.
5. **There is no CLA.** You are not asked to assign the copyright of your code
   to me. You contribute under the DCO (below) and your code stays yours,
   licensed AGPL-3.0 like the rest.

Point 5 is the honest counterpart of the rest: without full copyright I
*cannot* relicense unilaterally either. The commitment does not depend on my
good will.

## DCO: Developer Certificate of Origin

Every commit needs a sign-off line:

```
Signed-off-by: First Last <email@example.com>
```

`git commit -s` adds it for you. It only certifies that you have the right to
contribute that code (full text: <https://developercertificate.org/>). It
assigns nothing to anyone.

## Code standards

They are not aspirations: CI verifies them and they break the build.

- **Hexagonal architecture.** `internal/domain` and the core of
  `internal/engine` do not import infrastructure. `depguard` checks it.
- Interfaces defined on the consumer side. Zero global state.
  `context.Context` on every operation that crosses a boundary.
- Errors wrapped with `%w` and typed in the domain. No `panic` outside `main`.
- Domain at ~90% coverage. Adapters tested against real dependencies (real
  SQLite, real SDKs), not database mocks.
- `go test -race` always: ingest is pure concurrency.
- Conventional commits. `main` always green and installable.

## Before opening a PR

```sh
make bootstrap  # once: checks and installs what the gates need
make check      # fmt + vet + lint + govulncheck + test -race + architecture gate
make smoke      # end-to-end: starts a real server and operates it
make compat     # the official SDKs that do not need a container
make ui-smoke   # the panel in a real browser (needs Docker)
```

`make bootstrap` is idempotent and ends by saying what is missing and the exact
command that fixes it; `make tools` installs only the pinned Go tooling
(golangci-lint, govulncheck).

`make ui-smoke` and `make compat` need Docker. If you do not have it, say so in
the PR instead of skipping them silently: CI runs them anyway, and a failure is
better understood with the context of whoever wrote the change.

The full SDK matrix, the long fuzz run, the installation of a real release and
the container image run at night (`.github/workflows/nightly.yml`). The rule: a
gate that takes more than ~3 minutes, or that depends on something outside this
repo, does not go in the PR.

## Changelog

**Do not edit `CHANGELOG.md`.** Leave your own fragment in
`changelog.d/<something>.md` with what changes for the person using the
product:

```md
### Added
- What can be done now that could not be done before.

### Fixed
- The symptom first, then the cause.

### Changed
- What behaves differently, and what someone already using it has to do.
```

It is split up because `CHANGELOG.md` was the most expensive conflict point in
the repo: several parallel changes appended to the same sections and every
rebase meant merging prose by hand. One file per unit of work does not collide.

`make changelog` shows the next release already assembled, without touching
anything. `make changelog VERSION=v0.1.0` pastes it into `CHANGELOG.md` and
deletes the consumed fragments: that is done by whoever cuts the release, not
by a PR.

## Scope

Before investing time in a feature, look at the anti-scope list in the README.
It is a contract, not a backlog: a PR that adds session replay, profiling or
SSO gets closed however good the code is. What keeps this in a 30 MB binary is
saying no.
