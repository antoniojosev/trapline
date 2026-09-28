#!/usr/bin/env bash
#
# The demo, and the gate that keeps it true.
#
# It runs the loop this product exists for, end to end, against real
# containers: a checkout service with a bug in it sends a real error through
# the official Sentry SDK; the server groups it, attributes it to the commit
# that introduced it and serves the whole thing as one markdown document; an
# agent reads that document, writes the test that fails, patches the code and
# marks the issue resolved in the next release; the service is redeployed and
# the error does not come back — while the instance nobody redeployed yet keeps
# throwing it, and does not reopen anything.
#
# Two modes, and the difference is one step:
#
#   ./demo.sh              the real thing. Step 6 runs `claude -p "/fix-error"`
#                          with the skill in skills/fix-error/. This is what
#                          gets recorded.
#   ./demo.sh --no-agent   the same run with step 6 replaced by two committed
#                          patches — the test first, then the fix — so that CI
#                          can check every other step without an agent, a
#                          model, an API key or a network call to one.
#
# What `--no-agent` is worth saying out loud: it does not check that an agent
# can fix this bug. It checks that everything the agent needs is there and
# everything that happens after it works — which is the part that rots. The
# agent step is the only one a machine cannot assert, so it is the only one
# that is simulated, and it is simulated with a patch that lives in the
# repository where anybody can read what was assumed.
#
# Nothing here writes to the working copy. The service is copied to a
# temporary directory, given a git history there, and patched there, so the
# demo starts from the same state every time — which is what you want on the
# third take.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEMO="$ROOT/demo"
cd "$ROOT"

# 9960 is the demo's port block; every gate under scripts/ has its own so two
# can run at once. Three ports: the server,
# the redeployed service, and the instance still running the old build.
PORT="${PORT:-9960}"
APP_PORT="${APP_PORT:-$((PORT + 1))}"
APP_OLD_PORT="${APP_OLD_PORT:-$((PORT + 2))}"
DOCKER_PREFIX="${DOCKER_PREFIX:-trapline-demo}"

# shellcheck source=../scripts/lib/port.sh
. "$ROOT/scripts/lib/port.sh"

# The name of the binary the demo builds and calls.
NAME="trapline"

AGENT=1
for argument in "$@"; do
  case "$argument" in
    --no-agent) AGENT=0 ;;
    -h|--help) sed -n '2,33p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) printf 'demo.sh: unknown argument %s\n' "$argument" >&2; exit 2 ;;
  esac
done

WORKDIR="$(mktemp -d)"
APPDIR="$WORKDIR/app"
ENVFILE="$WORKDIR/.env"
BINARY="$WORKDIR/$NAME"
OLD_IMAGE="$DOCKER_PREFIX-app:checkout-1.0.0"
NEW_IMAGE="$DOCKER_PREFIX-app:checkout-1.1.0"

compose() {
  docker compose -p "$DOCKER_PREFIX" \
    --project-directory "$DEMO" -f "$DEMO/docker-compose.yml" \
    --env-file "$ENVFILE" "$@"
}

cleanup() {
  [ -f "$ENVFILE" ] && compose --profile legacy down -v --remove-orphans >/dev/null 2>&1 || true
  docker image rm -f "$OLD_IMAGE" "$NEW_IMAGE" >/dev/null 2>&1 || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

bold=$'\033[1m'; dim=$'\033[2m'; green=$'\033[32m'; reset=$'\033[0m'
step() { printf '\n%s== %s%s\n' "$bold" "$1" "$reset"; }
say()  { printf '   %s\n' "$1"; }
show() { printf '   %s$ %s%s\n' "$dim" "$1" "$reset"; }
ok()   { printf '   %sok%s   %s\n' "$green" "$reset" "$1"; }
fail() { printf '\n   FAIL %s\n\n' "$1" >&2; exit 1; }

# json reads one path out of a JSON document on stdin. python3 rather than jq
# because every gate in this repository already depends on python3 and none of
# them depends on jq (scripts/check-workflows.py, scripts/lib/docs_links.py).
json() {
  python3 -c '
import json, sys
value = json.load(sys.stdin)
for part in sys.argv[1].split("."):
    if part == "":
        continue
    value = value[int(part)] if isinstance(value, list) else value[part]
print("" if value is None else value)
' "$1"
}

# order places one. It sets ORDER_STATUS and ORDER_JSON rather than printing
# them, because a command substitution would run it in a subshell and the two
# values would not survive it — which is a thing this script got wrong once.
ORDER_STATUS=""
ORDER_JSON=""
order() { # port [discount-code]
  local port="$1" code="${2:-}" payload
  payload='{"id":"ord_4412","items":[{"sku":"cafe-500g","unitPrice":1200,"quantity":2},'
  payload+='{"sku":"filtros-x100","unitPrice":350,"quantity":1}]'
  [ -n "$code" ] && payload+=',"discountCode":"'"$code"'"'
  payload+='}'
  ORDER_STATUS="$(curl -sS -o "$WORKDIR/body" -w '%{http_code}' \
    -X POST "http://127.0.0.1:$port/checkout" \
    -H 'Content-Type: application/json' -d "$payload")"
  ORDER_JSON="$(cat "$WORKDIR/body")"
}

# ---------------------------------------------------------------------------

step "0 · what this needs"
command -v docker >/dev/null || fail "the demo is two containers, so it needs Docker"
docker compose version >/dev/null 2>&1 || fail "it needs the compose plugin (docker compose, not docker-compose)"
command -v node >/dev/null || fail "step 7 runs the service's own test suite, which needs Node"
command -v git >/dev/null || fail "suspect commits are read out of a git history, so it needs git"
command -v python3 >/dev/null || fail "it reads JSON with python3, like every other gate here"
if [ "$AGENT" -eq 1 ] && ! command -v claude >/dev/null; then
  fail "no \`claude\` on PATH. Run with --no-agent to use the committed patches instead."
fi
for port in "$PORT" "$APP_PORT" "$APP_OLD_PORT"; do
  require_free_port "$port" "the demo"
done
ok "docker, node, git, python3, and ports $PORT/$APP_PORT/$APP_OLD_PORT free"

step "1 · a disposable copy of the service, with a history"
# A repository with one commit has nothing to attribute a crash to, so the
# history is built rather than faked: the pricing code first, the discount
# codes second — that second commit is the one that introduced the bug and the
# one the product has to name — and an unrelated third so that "the commit that
# touched the file in the stacktrace" is a choice and not the only option.
mkdir -p "$APPDIR"
cp -r "$DEMO/app/." "$APPDIR/"
git -C "$APPDIR" init -q
git -C "$APPDIR" config user.name "Ana Restrepo"
git -C "$APPDIR" config user.email "ana@tienda.example"
git -C "$APPDIR" config commit.gpgsign false

cp "$DEMO/history/checkout.js" "$APPDIR/src/checkout.js"
rm -f "$APPDIR/test/checkout.test.js"
git -C "$APPDIR" add -A
git -C "$APPDIR" commit -qm "feat(checkout): order totals with tax"
BASE_SHA="$(git -C "$APPDIR" rev-parse HEAD)"

cp "$DEMO/app/src/checkout.js" "$APPDIR/src/checkout.js"
cp "$DEMO/app/test/checkout.test.js" "$APPDIR/test/checkout.test.js"
git -C "$APPDIR" add -A
git -C "$APPDIR" commit -qm "feat(checkout): apply discount codes at checkout

Marketing wants WELCOME10 and BLACKFRIDAY live for the launch week."

printf '\n# Cafe Tienda — checkout service\n' >> "$APPDIR/README.md"
git -C "$APPDIR" add -A
git -C "$APPDIR" commit -qm "docs: say what this service is"

BUG_SHA="$(git -C "$APPDIR" log --format=%H --grep='apply discount codes')"
[ -n "$BUG_SHA" ] || fail "could not find the commit that introduces the bug"
ok "3 commits; the one that added discounts is ${BUG_SHA:0:8}"

step "2 · the server"
cat > "$ENVFILE" <<EOF
TRAPLINE_PORT=$PORT
APP_PORT=$APP_PORT
APP_OLD_PORT=$APP_OLD_PORT
TRAPLINE_ORIGIN=http://trapline:9000
TRAPLINE_IMAGE=$DOCKER_PREFIX-server:local
DEMO_APP_DIR=$APPDIR
DEMO_APP_IMAGE=$OLD_IMAGE
DEMO_APP_OLD_IMAGE=$OLD_IMAGE
APP_RELEASE=checkout@1.0.0
DEMO_DSN=
EOF
show "docker compose up -d trapline"
compose up -d --build trapline >/dev/null 2>&1 || { compose logs trapline; fail "the server did not start"; }

export TRAPLINE_URL="http://127.0.0.1:$PORT"
for _ in $(seq 1 120); do
  curl -fsS "$TRAPLINE_URL/api/v1/health" >/dev/null 2>&1 && break
  sleep 0.5
done
curl -fsS "$TRAPLINE_URL/api/v1/health" >/dev/null \
  || { compose logs trapline; fail "the server never became healthy"; }

curl -fsS -X POST "$TRAPLINE_URL/api/v1/setup" \
  -H 'Content-Type: application/json' -H 'X-Trapline-Request: 1' \
  -d '{"username":"antonio","password":"una contraseña larga y buena"}' >/dev/null \
  || fail "setup failed"

# The CLI on the host is the same client the panel and the agent are (ADR 006),
# so the demo drives the installation with it rather than with curl.
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BINARY" ./cmd/trapline
TRAPLINE_TOKEN="$(docker exec "$(compose ps -q trapline)" \
  /trapline token create -db /data/trapline.db -name demo \
  -scopes projects:read,projects:write --json | json token)"
[ -n "$TRAPLINE_TOKEN" ] || fail "no token"
export TRAPLINE_TOKEN
ok "one container, one file of state, listening on $PORT"

step "3 · a project, a release, and the commits that went into it"
show "$NAME projects create -name tienda"
DSN="$("$BINARY" projects create -name tienda)"
SENTRY_KEY="$(printf '%s' "$DSN" | sed -n 's|.*//\([^@]*\)@.*|\1|p')"
[ -n "$SENTRY_KEY" ] || fail "could not read the ingest key out of the DSN"
say "$DSN"

# Tracing is off on a new project, and that is ADR 005 rather than an
# oversight: a subsystem nobody asked for does not cost anything, so it does
# not run. Turning it on is this line — not a second service, not a second
# datastore, not a second bill.
show "$NAME config set -project 1 -categories error,transaction -traces-sample-rate 1"
"$BINARY" config set -project 1 -categories error,transaction -traces-sample-rate 1 >/dev/null

# Last week's deploy, so that this week's carries the commits since it and not
# the whole history of the repository. That is not decoration: attribution is
# only as narrow as the range, and a first import that created every file in
# the stacktrace would out-match the commit that actually broke it — which is
# what happened the first time this demo ran (ADR 019).
"$BINARY" releases create -project 1 -version checkout@0.9.0 >/dev/null
"$BINARY" releases commits -project 1 -version checkout@0.9.0 -repo "$APPDIR" -to "$BASE_SHA" >/dev/null
"$BINARY" releases deploys -project 1 -version checkout@0.9.0 -environment production >/dev/null

show "$NAME releases create -project 1 -version checkout@1.0.0"
"$BINARY" releases create -project 1 -version checkout@1.0.0 >/dev/null
show "$NAME releases commits -project 1 -version checkout@1.0.0 -repo <the checkout> -from <0.9.0>"
# --repo reads `git log --name-status` locally. No GitHub integration, no
# credentials: the pipeline that deploys the code already has the checkout in
# front of it, and the paths a commit touched are the whole of ADR 019.
"$BINARY" releases commits -project 1 -version checkout@1.0.0 -repo "$APPDIR" -from "$BASE_SHA" >/dev/null
"$BINARY" releases deploys -project 1 -version checkout@1.0.0 -environment production >/dev/null
COMMIT_COUNT="$("$BINARY" releases show -project 1 -version checkout@1.0.0 --json | json commit_count)"
[ "$COMMIT_COUNT" = "2" ] || fail "the release should carry 2 commits since 0.9.0, it carries $COMMIT_COUNT"
ok "checkout@1.0.0 deployed, carrying the 2 commits since checkout@0.9.0"

step "4 · the service, in production, with the bug in it"
sed -i "s|^DEMO_DSN=.*|DEMO_DSN=$DSN|" "$ENVFILE"
show "docker compose up -d app"
compose up -d --build app >/dev/null 2>&1 || { compose logs app; fail "the app did not start"; }
for _ in $(seq 1 60); do
  curl -fsS "http://127.0.0.1:$APP_PORT/health" >/dev/null 2>&1 && break
  sleep 0.5
done
curl -fsS "http://127.0.0.1:$APP_PORT/health" >/dev/null \
  || { compose logs app; fail "the app never became healthy"; }

say "three orders that work:"
for code in "" "WELCOME10" "BLACKFRIDAY"; do
  order "$APP_PORT" "$code"
  [ "$ORDER_STATUS" = "200" ] || fail "an order that should work returned $ORDER_STATUS: $ORDER_JSON"
  say "   ${code:-no code} → $ORDER_JSON"
done

say "and one with a code from a campaign that ended in July:"
order "$APP_PORT" "SUMMER20"
[ "$ORDER_STATUS" = "500" ] || fail "the broken order returned $ORDER_STATUS, so the demo has no bug to fix"
say "   SUMMER20 → HTTP 500  $ORDER_JSON"
ok "the customer got a 500; nobody has looked at a log"

step "5 · the error is already here"
ISSUE=""
for _ in $(seq 1 60); do
  ISSUE="$("$BINARY" issues list -project 1 -status unresolved --json | json issues.0.id 2>/dev/null || true)"
  [ -n "$ISSUE" ] && break
  sleep 0.5
done
[ -n "$ISSUE" ] || fail "no issue arrived in 30 seconds"
show "$NAME issues list -project 1 -status unresolved"
"$BINARY" issues list -project 1 -status unresolved

DETAIL="$("$BINARY" issues show -project 1 -issue "$ISSUE" --json)"
TITLE="$(printf '%s' "$DETAIL" | json title)"
case "$TITLE" in
  *TypeError*) : ;;
  *) fail "the issue is titled '$TITLE', which is not the error the service raised" ;;
esac
FIRST_RELEASE="$(printf '%s' "$DETAIL" | json first_release)"
[ "$FIRST_RELEASE" = "checkout@1.0.0" ] || fail "first seen in '$FIRST_RELEASE', expected checkout@1.0.0"
ok "issue #$ISSUE — $TITLE"

step "6 · what an agent reads, before it opens a single file"
show "$NAME issues bundle -project 1 -issue $ISSUE"
BUNDLE="$WORKDIR/bundle.md"
"$BINARY" issues bundle -project 1 -issue "$ISSUE" > "$BUNDLE"
sed 's/^/   │ /' "$BUNDLE"

# What the bundle has to contain for the next step to be possible at all. Each
# of these is a separate subsystem answering: the stacktrace is the ingest
# path, the breadcrumb is the SDK's trail, the suspect commit is ADR 019
# crossing paths against frames, the frequency is the hourly aggregates.
grep -q 'src/checkout.js' "$BUNDLE" || fail "the bundle does not name the file that failed"
grep -q 'applyDiscount' "$BUNDLE" || fail "the bundle does not name the function that failed"
grep -q 'SUMMER20' "$BUNDLE" || fail "the bundle does not carry the breadcrumb with the code that was presented"
# Not "the sha appears somewhere": the first suspect, which is the one a
# reader acts on. A list that names the right commit third is a list that was
# right by accident.
TOP_SUSPECT="$(sed -n 's/^1\. `\([0-9a-f]*\)`.*/\1/p' "$BUNDLE" | head -1)"
[ "$TOP_SUSPECT" = "${BUG_SHA:0:8}" ] \
  || fail "the top suspect is '$TOP_SUSPECT', and the commit that broke it is ${BUG_SHA:0:8}"
grep -qi 'web-01' "$BUNDLE" || fail "the bundle does not say which instance served it"
ok "one document: the exception, the frames, the trail, the frequency, and the commit"

step "7 · the fix"
if [ "$AGENT" -eq 1 ]; then
  say "running the agent with skills/fix-error/SKILL.md, in $APPDIR"
  show "claude -p \"/fix-error $ISSUE\""
  # The agent is a real client of this installation: it reaches it over MCP or
  # the CLI with exactly these two variables, and it has no other credential.
  (
    cd "$APPDIR"
    TRAPLINE_URL="$TRAPLINE_URL" TRAPLINE_TOKEN="$TRAPLINE_TOKEN" PATH="$WORKDIR:$PATH" \
      claude -p "/fix-error $ISSUE" --add-dir "$ROOT/skills/fix-error"
  ) || fail "the agent did not finish"
else
  say "no agent in this run; applying what one would have written."
  say "(demo/fix/*.patch — two patches, because the test comes first)"

  show "git apply demo/fix/0001-test-unknown-discount-code.patch"
  git -C "$APPDIR" apply "$DEMO/fix/0001-test-unknown-discount-code.patch" \
    || fail "the test patch no longer applies to demo/app — regenerate it"

  show "node --test test/*.test.js   # and watch it fail"
  if (cd "$APPDIR" && node --test test/*.test.js >"$WORKDIR/test-before.log" 2>&1); then
    fail "the new test passed before the fix, so it does not reproduce anything"
  fi
  grep -q 'TypeError' "$WORKDIR/test-before.log" \
    || fail "the test failed for a reason that is not the bug"
  say "   2 tests failing, with the same TypeError the bundle showed"

  show "git apply demo/fix/0002-fix-unknown-discount-code-is-no-discount.patch"
  git -C "$APPDIR" apply "$DEMO/fix/0002-fix-unknown-discount-code-is-no-discount.patch" \
    || fail "the fix patch no longer applies to demo/app — regenerate it"
fi

show "node --test test/*.test.js   # the whole suite, not just the new test"
(cd "$APPDIR" && node --test test/*.test.js >"$WORKDIR/test-after.log" 2>&1) \
  || { sed 's/^/   > /' "$WORKDIR/test-after.log"; fail "the suite is not green after the fix"; }
awk '$2 ~ /^(tests|pass|fail)$/ { printf "   %s %s\n", $2, $3 }' "$WORKDIR/test-after.log"
git -C "$APPDIR" add -A
git -C "$APPDIR" commit -qm "fix(checkout): an unknown discount code is no discount

A campaign that ends leaves its code in circulation, so an unknown code is
what every promotion eventually becomes rather than an exceptional condition.

Fixes issue #$ISSUE."
FIX_SHA="$(git -C "$APPDIR" rev-parse HEAD)"
ok "the suite is green, and the fix is a commit (${FIX_SHA:0:8})"

step "8 · resolved in the next release"
show "$NAME issues resolve -project 1 -issue $ISSUE -next-release"
"$BINARY" issues resolve -project 1 -issue "$ISSUE" -next-release >/dev/null
STATUS="$("$BINARY" issues show -project 1 -issue "$ISSUE" --json | json status)"
[ "$STATUS" = "resolved" ] || fail "the issue is '$STATUS' after resolving it"
ok "resolved — and waiting for a release newer than checkout@1.0.0"

step "9 · the redeploy"
# The instance nobody has redeployed yet is left running on purpose. It is the
# reason "resolve in the next release" exists: a plain resolve would reopen the
# issue on its very next event, which looks exactly like a fix that did not
# work (ADR 012).
show "docker compose --profile legacy up -d app-old   # the pod still on 1.0.0"
compose --profile legacy up -d app-old >/dev/null 2>&1 || fail "the old instance did not start"
for _ in $(seq 1 60); do
  curl -fsS "http://127.0.0.1:$APP_OLD_PORT/health" >/dev/null 2>&1 && break
  sleep 0.5
done

"$BINARY" releases create -project 1 -version checkout@1.1.0 >/dev/null
"$BINARY" releases commits -project 1 -version checkout@1.1.0 -repo "$APPDIR" -from "$BUG_SHA" >/dev/null
"$BINARY" releases deploys -project 1 -version checkout@1.1.0 -environment production >/dev/null

sed -i "s|^DEMO_APP_IMAGE=.*|DEMO_APP_IMAGE=$NEW_IMAGE|" "$ENVFILE"
sed -i "s|^APP_RELEASE=.*|APP_RELEASE=checkout@1.1.0|" "$ENVFILE"
show "docker compose up -d --build app   # checkout@1.1.0"
compose up -d --build app >/dev/null 2>&1 || { compose logs app; fail "the redeploy failed"; }
for _ in $(seq 1 60); do
  curl -fsS "http://127.0.0.1:$APP_PORT/health" 2>/dev/null | grep -q '1.1.0' && break
  sleep 0.5
done
curl -fsS "http://127.0.0.1:$APP_PORT/health" | grep -q '1.1.0' || fail "the new release never came up"
ok "checkout@1.1.0 is serving; checkout@1.0.0 is still up on $APP_OLD_PORT"

step "10 · the same order, again"
order "$APP_PORT" "SUMMER20"
[ "$ORDER_STATUS" = "200" ] || fail "the fixed release still returns $ORDER_STATUS for the order that broke"
say "   SUMMER20 → HTTP $ORDER_STATUS  $ORDER_JSON"
ok "full price, no discount, no 500"

BEFORE_TIMES="$("$BINARY" issues show -project 1 -issue "$ISSUE" --json | json times)"

say ""
say "and now the instance nobody redeployed, which is still broken:"
order "$APP_OLD_PORT" "SUMMER20"
[ "$ORDER_STATUS" = "500" ] || fail "the old instance returned $ORDER_STATUS; it was supposed to still be broken"
say "   SUMMER20 on checkout@1.0.0 → HTTP $ORDER_STATUS"

SUPPRESSED=0
for _ in $(seq 1 60); do
  DETAIL="$("$BINARY" issues show -project 1 -issue "$ISSUE" --json)"
  SUPPRESSED="$(printf '%s' "$DETAIL" | json seen_in_resolved_release_count)"
  [ "$SUPPRESSED" -gt 0 ] 2>/dev/null && break
  sleep 0.5
done
STATUS="$(printf '%s' "$DETAIL" | json status)"
REGRESSIONS="$(printf '%s' "$DETAIL" | json regressions)"
AFTER_TIMES="$(printf '%s' "$DETAIL" | json times)"

[ "$SUPPRESSED" -gt 0 ] || fail "the event from the old release never arrived, so this proves nothing"
[ "$STATUS" = "resolved" ] || fail "the old release reopened the issue: status is '$STATUS'"
[ "$REGRESSIONS" = "0" ] || fail "the old release was counted as a regression ($REGRESSIONS)"
[ "$AFTER_TIMES" -gt "$BEFORE_TIMES" ] || fail "the event was dropped instead of counted"
show "$NAME issues show -project 1 -issue $ISSUE"
"$BINARY" issues show -project 1 -issue "$ISSUE" | sed 's/^/   │ /'
ok "counted, shown, still resolved, zero regressions"

step "11 · and the latency was there the whole time"
# Same binary, same installation, no second product: the transaction the
# service reported alongside its errors, with the percentiles computed at read
# time out of a mergeable sketch and never stored (ADR 007, ADR 020).
show "$NAME transactions list -project 1"
"$BINARY" transactions list -project 1 | sed 's/^/   │ /'
# By name, not by position: the list is sorted by p95 and which row comes
# first is a property of the latencies this run happened to produce.
TX_COUNT="$("$BINARY" transactions list -project 1 --json | python3 -c '
import json, sys
rows = json.load(sys.stdin)["transactions"]
print(next((row["count"] for row in rows if row["transaction"] == "POST /checkout"), 0))
')"
[ "${TX_COUNT:-0}" -gt 0 ] || fail "no transactions arrived, so the tracing half of the demo is dead"
ok "$TX_COUNT transactions of POST /checkout, p50/p95/p99 computed on read"

step "the whole loop"
say "error → bundle → patch → tests → resolved in next release → redeploy → gone,"
say "with the pod that has not been redeployed still counted and still quiet."
say ""
# Measured here, now, rather than quoted: this is the container that just did
# all of the above. It is a small installation and a handful of events, so it
# is *below* the published figures rather than a substitute for them — those
# are 19,7 MB at rest and 34,2 MB with everything switched on and a hundred
# thousand events in, measured by scripts/footprint.sh and re-measured nightly
# (docs/benchmarks/footprint.md). Printing the live number keeps this script
# from becoming a fourth place where a number has to be kept in sync.
RSS="$(docker stats --no-stream --format '{{.MemUsage}}' "$(compose ps -q trapline)" 2>/dev/null | cut -d/ -f1 | tr -d ' ')"
printf '   %sone binary, one file of state, %s of RAM right now%s\n' "$bold" "${RSS:-?}" "$reset"
say "(published: 19,7 MB at rest, 34,2 MB with everything on — docs/benchmarks/footprint.md)"
printf '\n%s%sthe demo does what it says%s\n' "$bold" "$green" "$reset"
