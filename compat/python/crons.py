"""Declares and reports a cron monitor through the official SDK's crons API.

It is a second entry point beside main.py rather than another mode of it,
because it answers a different question. main.py asks whether an error
reported by the real SDK becomes the right issue; this asks whether a monitor
*declared* by the real SDK comes into existence — which is the whole claim of
ADR 016's first entrance, and the one thing a hand-written envelope cannot
test, since what is under test is the shape sentry_sdk itself puts on the wire.

Nothing here is adapted to this server. The decorator below is the one from
the SDK's documentation, unchanged, and the monitor_config is the documented
shape. Anything this program had to work around would be an incompatibility.
"""

import argparse
import json
import sys
import urllib.error
import urllib.request

import sentry_sdk
from sentry_sdk.crons import monitor

MONITOR_SLUG = "compat-nightly-backup"
ENVIRONMENT = "compat-test"

# Read in America/Caracas, so the gate can check that a schedule is a
# wall-clock statement in a named zone and not an instant in the server's.
TIMEZONE = "America/Caracas"

MONITOR_CONFIG = {
    "schedule": {"type": "crontab", "value": "0 3 * * *"},
    # The protocol counts these in minutes. The server stores seconds, and a
    # conversion that went the wrong way would report a monitor as missed
    # sixty times too early — which is exactly what the gate asserts.
    "checkin_margin": 5,
    "max_runtime": 10,
    "timezone": TIMEZONE,
}


class CompatError(Exception):
    """A failure of the compatibility claim, not of this program."""


@monitor(monitor_slug=MONITOR_SLUG, monitor_config=MONITOR_CONFIG)
def nightly_backup():
    """The instrumented job. One decorator, which is the entire pitch."""
    return "done"


def send_check_in(dsn):
    # The whole configuration. If anything else were needed here, the
    # compatibility claim would be false.
    sentry_sdk.init(dsn=dsn, environment=ENVIRONMENT)
    nightly_backup()
    sentry_sdk.flush(timeout=10)


def read_monitors(api_url, token, project_id):
    request = urllib.request.Request(
        f"{api_url}/api/v1/projects/{project_id}/monitors/cron",
        headers={"Authorization": f"Bearer {token}"},
    )
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            return json.load(response)["monitors"]
    except urllib.error.HTTPError as failure:
        raise CompatError(f"reading the monitors returned {failure.code}: {failure.read()!r}") from failure


def check(monitors):
    declared = [m for m in monitors if m["slug"] == MONITOR_SLUG]
    if len(declared) != 1:
        raise CompatError(
            f"the SDK's declaration produced {len(declared)} monitors called {MONITOR_SLUG!r}; "
            f"the server knows about {[m['slug'] for m in monitors]}"
        )
    found = declared[0]

    if found["schedule"] != "0 3 * * *":
        raise CompatError(f"the declared schedule did not survive: {found['schedule']!r}")
    if found["schedule_type"] != "crontab":
        raise CompatError(f"the schedule type is {found['schedule_type']!r}")
    if found["timezone"] != TIMEZONE:
        raise CompatError(f"the declared timezone did not survive: {found['timezone']!r}")
    if found["checkin_margin_s"] != 300:
        raise CompatError(
            f"checkin_margin is {found['checkin_margin_s']}s; the protocol sends 5 minutes, "
            "so anything but 300 is a unit conversion going the wrong way"
        )
    if found["max_runtime_s"] != 600:
        raise CompatError(f"max_runtime is {found['max_runtime_s']}s, want 600")
    if found["status"] != "ok":
        raise CompatError(f"a job that ran and returned is {found['status']!r}, want ok")
    if not found["ping_url"].endswith("/ping/" + found["ping_key"]):
        raise CompatError(f"the ping url is not usable: {found['ping_url']!r}")


def main():
    parser = argparse.ArgumentParser(add_help=True)
    # Both spellings, because the matrix runner passes Go-style single-dash
    # flags and a suite that only accepts one of them is a suite that only
    # runs when invoked by hand.
    parser.add_argument("--dsn", "-dsn", dest="dsn", required=True)
    parser.add_argument("--api", "-api", dest="api", required=True)
    parser.add_argument("--token", "-token", dest="token", required=True)
    parser.add_argument("--project", "-project", dest="project", type=int, default=1)
    arguments = parser.parse_args()

    try:
        send_check_in(arguments.dsn)
        check(read_monitors(arguments.api, arguments.token, arguments.project))
    except CompatError as failure:
        print(f"FAIL {failure}", file=sys.stderr)
        return 1

    print("ok   the official Python SDK declares a cron monitor against this server, DSN only")
    return 0


if __name__ == "__main__":
    sys.exit(main())
