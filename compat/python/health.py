"""Reports release health through the official Python SDK's session tracking.

A third entry point beside main.py and crons.py rather than another mode of
either, because it answers a different question again. main.py asks whether an
error reported by the real SDK becomes the right issue; crons.py asks whether a
monitor *declared* by the real SDK comes into existence; this asks whether the
sessions the SDK tracks by itself add up to a crash-free rate.

Nothing here is adapted to this server. Sessions are started and ended through
`sentry_sdk.sessions.track_session`, which is the SDK's own auto-tracking
machinery and honours `auto_session_tracking` — so an installation that
switched the option off would send nothing at all, which is exactly the claim
worth testing. Crashes are produced by capturing an event whose mechanism says
`handled: false`, which is what the SDK's own excepthook does and the only
thing that makes the SDK mark a session crashed. Anything this program had to
work around would be an incompatibility.
"""

import argparse
import json
import sys

import sentry_sdk
from sentry_sdk.session import Session
from sentry_sdk.sessions import track_session
from sentry_sdk.utils import event_from_exception

ENVIRONMENT = "compat-test"


class CompatError(Exception):
    """A failure of the compatibility claim, not of this program."""


def crash():
    """Report a crash the way an unhandled exception does.

    `capture_exception` would not do: it stamps `handled: true`, which the SDK
    reads as an *errored* session rather than a crashed one. The mechanism
    below is the one the excepthook integration attaches, and the difference
    between the two is the entire numerator of the crash-free rate.
    """
    try:
        raise RuntimeError("checkout exploded")
    except RuntimeError as error:
        event, hint = event_from_exception(
            error,
            client_options=sentry_sdk.get_client().options,
            mechanism={"type": "compat-health", "handled": False},
        )
        sentry_sdk.capture_event(event, hint=hint)


def run_sessions(count, crashes, errors):
    """Track `count` sessions, of which `crashes` crash and `errors` error."""
    scope = sentry_sdk.get_isolation_scope()
    for index in range(count):
        with track_session(scope):
            if index < crashes:
                crash()
            elif index < crashes + errors:
                sentry_sdk.capture_exception(ValueError("a handled problem"))


def run_open_sessions(count, release):
    """Report `count` sessions that start and never end.

    This is the shape the in-memory window exists for and the only one that can
    fill it: a session that ends is settled and forgotten immediately, so a
    thousand complete sessions would never occupy more than one slot. A client
    that starts and goes quiet — a mobile app the OS reclaimed, a tab left open
    — is what a real window holds.

    `capture_session` is the client's own public entrance, the same one
    `end_session` uses; the only difference is that nothing here calls
    `close()` first, so the update goes out with status "ok".
    """
    client = sentry_sdk.get_client()
    for _ in range(count):
        session = Session(release=release, environment=ENVIRONMENT)
        client.capture_session(session)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dsn", required=True)
    parser.add_argument("--release", required=True)
    parser.add_argument("--count", type=int, default=0,
                        help="sessions to start and end")
    parser.add_argument("--crashes", type=int, default=0,
                        help="how many of them crash")
    parser.add_argument("--errors", type=int, default=0,
                        help="how many of them report a handled error and still exit")
    parser.add_argument("--open", type=int, default=0, dest="open_sessions",
                        help="sessions to start and never end")
    args = parser.parse_args()

    sentry_sdk.init(
        dsn=args.dsn,
        release=args.release,
        environment=ENVIRONMENT,
        # The option under test. With it off the SDK tracks no sessions at all
        # and this program would report zero, which is the failure worth being
        # able to see.
        auto_session_tracking=True,
        # Off, so an SDK-side sampling decision cannot quietly change the
        # counts this gate asserts on.
        traces_sample_rate=0.0,
    )

    if args.count:
        run_sessions(args.count, args.crashes, args.errors)
    if args.open_sessions:
        run_open_sessions(args.open_sessions, args.release)

    # The SDK batches sessions on a background thread with a flush interval of
    # its own, so without this the program would exit before anything left the
    # machine. `flush` returns nothing — it either drains within the timeout or
    # gives up quietly — so what the gate actually asserts on is the count the
    # server reports back, never this call's opinion of itself.
    sentry_sdk.flush(timeout=30)

    print(json.dumps({
        "release": args.release,
        "environment": ENVIRONMENT,
        "sessions": args.count,
        "crashes": args.crashes,
        "errors": args.errors,
        "open": args.open_sessions,
    }))


if __name__ == "__main__":
    try:
        main()
    except CompatError as failure:
        print(f"incompatible: {failure}", file=sys.stderr)
        sys.exit(1)
