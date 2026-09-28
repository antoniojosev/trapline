"""Sends events through the official Python SDK and checks what arrived.

It uses the real SDK, unmodified, configured with nothing but a DSN — which is
the exact claim being tested. Anything this program has to work around is an
incompatibility, and it says so rather than adapting.

Python earns its own suite rather than being assumed equivalent to Go because
its exceptions carry things Go's do not: a chain of causes, several frames of
its own code per error, and a logging integration that reports events nobody
wrote a `capture` call for. Each of those is a distinct path through grouping.
"""

import argparse
import json
import logging
import sys
import urllib.error
import urllib.request

import sentry_sdk

RELEASE = "compat@1.0.0"
ENVIRONMENT = "compat-test"

# Three occurrences of one error, one of another, one chain, one message, one
# parameterised log line called three times, and two different KeyErrors from
# a single function — seven issues if grouping is right, and a different number
# for every way it can be wrong.
EXPECTED_ISSUES = 7
GROUPED_OCCURRENCES = 3


class CompatError(Exception):
    """A failure of the compatibility claim, not of this program."""


class PaymentDeclined(Exception):
    pass


class UpstreamUnavailable(Exception):
    pass


class ConfigurationMissing(Exception):
    pass


def charge(order_id):
    # Raised one frame below the caller so the culprit has to come from the
    # stacktrace: a culprit derived from anything else would name `run`.
    raise PaymentDeclined(f"payment declined for order {order_id}")


def load_config():
    raise KeyError("SETTINGS_PATH")


def lookup(order, field):
    # Two different missing keys, raised from one function. Python renders
    # str(KeyError("x")) as "'x'" — quotes included — and this used to collapse
    # into a single issue, because the message normaliser rewrote anything
    # quoted and line numbers are excluded from grouping by design (ADR 003).
    # KeyError is the most common exception in Python, so the two must stay
    # apart.
    return order[field]


def boot():
    # `raise ... from ...` puts two exceptions on the wire, oldest cause first.
    # The last entry is the one actually raised, and everything the issue shows
    # — title, culprit, grouping — has to come from that one.
    try:
        load_config()
    except KeyError as cause:
        raise ConfigurationMissing("boot failed") from cause


def send_events(dsn):
    # The whole configuration. If anything else were needed here, the
    # compatibility claim would be false.
    sentry_sdk.init(dsn=dsn, release=RELEASE, environment=ENVIRONMENT)
    sentry_sdk.set_tag("suite", "compat-python")

    # The same error three times with a value that differs each time, so this
    # exercises grouping rather than counting.
    for attempt in range(GROUPED_OCCURRENCES):
        try:
            charge(4800 + attempt)
        except PaymentDeclined:
            sentry_sdk.capture_exception()

    # A genuinely different error, which must land in its own issue.
    try:
        raise UpstreamUnavailable("upstream timed out")
    except UpstreamUnavailable:
        sentry_sdk.capture_exception()

    try:
        boot()
    except ConfigurationMissing:
        sentry_sdk.capture_exception()

    for field in ("billing_address", "shipping_country"):
        try:
            lookup({"id": 1}, field)
        except KeyError:
            sentry_sdk.capture_exception()

    # A message with no exception at all, which takes a different path through
    # grouping: no type, no frames, nothing but the text.
    sentry_sdk.capture_message("cache warm-up skipped")

    # The logging integration is on by default, so a plain `logger.error` is an
    # event the application never asked to send — a path no other SDK in the
    # matrix has. The message is parameterised on purpose: the record carries
    # the template and the interpolated text side by side, and one log
    # statement must be one issue however many values it has been called with.
    logger = logging.getLogger("compat")
    # Without a handler, the standard library prints the record to stderr
    # itself, which would put a stray line in the matrix output next to this
    # suite's one line of result. The integration reads the record before any
    # handler runs, so silencing them costs nothing.
    logger.addHandler(logging.NullHandler())
    for name in ("alice", "bob", "carol"):
        logger.error("could not charge user %s", name)

    # Unlike the Go SDK's, this flush reports nothing: no return value, no
    # exception on timeout. Whether the events arrived is only knowable by
    # reading them back, which is what the rest of this program does.
    sentry_sdk.flush(timeout=10)


def get_json(url, token):
    request = urllib.request.Request(url)
    request.add_header("Authorization", "Bearer " + token)
    request.add_header("X-Trapline-Request", "1")
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            return json.loads(response.read())
    except urllib.error.HTTPError as error:
        raise CompatError(f"GET {url}: status {error.code}: {error.read().decode()}") from error
    except OSError as error:
        raise CompatError(f"GET {url}: {error}") from error


def read_issues(api_url, token, project_id):
    # The listing is a page, not a bare array: it carries the status counts and
    # a cursor alongside the issues.
    return get_json(f"{api_url}/api/v1/projects/{project_id}/issues", token)["issues"]


def read_issue(api_url, token, project_id, issue_id):
    return get_json(f"{api_url}/api/v1/projects/{project_id}/issues/{issue_id}", token)


def render(issues):
    summary = [
        {
            "title": issue["title"],
            "culprit": issue["culprit"],
            "level": issue["level"],
            "times": issue["times"],
            "last_release": issue.get("last_release", ""),
        }
        for issue in issues
    ]
    return "  " + json.dumps(summary, indent=2).replace("\n", "\n  ")


def find(issues, predicate, what):
    matches = [issue for issue in issues if predicate(issue)]
    if len(matches) != 1:
        raise CompatError(f"{len(matches)} issues match {what}, want exactly 1:\n{render(issues)}")
    return matches[0]


def check(issues, api_url, token, project_id):
    if len(issues) != EXPECTED_ISSUES:
        raise CompatError(f"got {len(issues)} issues, want {EXPECTED_ISSUES}:\n{render(issues)}")

    grouped = find(issues, lambda issue: issue["title"].startswith("PaymentDeclined"), "the repeated exception")
    if grouped["times"] != GROUPED_OCCURRENCES:
        raise CompatError(
            f"the repeated exception collected {grouped['times']} occurrences, "
            f"want {GROUPED_OCCURRENCES}:\n{render(issues)}"
        )
    if grouped.get("last_release") != RELEASE:
        raise CompatError(f"the release did not survive: {grouped.get('last_release')!r}")
    if not grouped["culprit"].endswith("in charge"):
        raise CompatError(
            f"the culprit was not derived from the frame that raised: {grouped['culprit']!r}"
        )

    distinct = find(issues, lambda issue: issue["title"].startswith("UpstreamUnavailable"), "the other exception")
    if distinct["times"] != 1:
        raise CompatError(f"a different exception did not get its own issue:\n{render(issues)}")

    # A chain must be reported as the exception that was raised, never as the
    # cause it wrapped — the cause is a KeyError here, so getting this backwards
    # is visible rather than subtle.
    chained = find(issues, lambda issue: issue["title"].startswith("ConfigurationMissing"), "the chained exception")
    if not chained["culprit"].endswith("in boot"):
        raise CompatError(
            f"the culprit came from the wrong entry in the exception chain: {chained['culprit']!r}"
        )

    message = find(issues, lambda issue: issue["title"] == "cache warm-up skipped", "the message")
    if message["level"] != "info":
        raise CompatError(f"the message's level did not survive: {message['level']!r}")

    # One log statement, three parameter values, one issue. Grouping on the
    # interpolated text instead of the template would make this three.
    logged = find(
        issues,
        lambda issue: issue["title"].startswith("could not charge user"),
        "the parameterised log record",
    )
    if logged["times"] != 3:
        raise CompatError(
            f"a parameterised log line split into {logged['times']} occurrences "
            f"across issues; it must be one issue with three:\n{render(issues)}"
        )

    # Two different missing keys must stay two issues.
    missing_keys = [issue for issue in issues if issue["title"].startswith("KeyError")]
    if len(missing_keys) != 2:
        raise CompatError(
            f"{len(missing_keys)} KeyError issues, want 2 — different missing keys "
            f"were merged into one bug:\n{render(issues)}"
        )

    # Environment is stored per event rather than on the issue, so it is only
    # observable through the detail view.
    detail = read_issue(api_url, token, project_id, grouped["id"])
    environments = {event.get("environment") for event in detail["events"]}
    if environments != {ENVIRONMENT}:
        raise CompatError(f"the environment did not survive: {sorted(environments)}")


def main():
    parser = argparse.ArgumentParser(add_help=True)
    # Both spellings, because the matrix runner passes Go-style single-dash
    # flags and a suite that only accepts one of them is a suite that only runs
    # when invoked by hand.
    parser.add_argument("--dsn", "-dsn", dest="dsn", required=True)
    parser.add_argument("--api", "-api", dest="api", required=True)
    parser.add_argument("--token", "-token", dest="token", required=True)
    parser.add_argument("--project", "-project", dest="project", type=int, default=1)
    arguments = parser.parse_args()

    try:
        send_events(arguments.dsn)
        issues = read_issues(arguments.api, arguments.token, arguments.project)
        check(issues, arguments.api, arguments.token, arguments.project)
    except CompatError as failure:
        print(f"FAIL {failure}", file=sys.stderr)
        return 1

    print("ok   the official Python SDK works against this server, DSN only")
    return 0


if __name__ == "__main__":
    sys.exit(main())

# Two grouping defects were found by the first run of this suite and are now
# fixed upstream, so both are asserted above rather than described here:
#
# 1. A parameterised log record split into one issue per parameter value,
#    because the decoder preferred logentry.formatted over the template that
#    identifies the log statement. Both are now kept, and grouping uses the
#    template.
#
# 2. Two different KeyErrors raised from one function collapsed into a single
#    issue, because str(KeyError("x")) is "'x'" and the message normaliser
#    rewrote anything quoted to <str>. A quoted run inside a longer sentence is
#    still normalised — that is what keeps "field 'a' is required" and
#    "field 'b' is required" one issue — but a message that is nothing but a
#    quoted token is now left alone, because that token is the only identity
#    it has.
