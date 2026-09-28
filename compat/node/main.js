#!/usr/bin/env node
// Command compat-node sends events through the official Node SDK and checks
// what arrived.
//
// It uses the real SDK, unmodified, configured with nothing but a DSN — which
// is the exact claim being tested. Anything this program has to work around is
// an incompatibility, and it says so rather than adapting.
//
// CommonJS rather than ESM on purpose. The two module systems produce
// different frame shapes — under ESM the runtime reports a file:// URL where
// CommonJS reports a plain absolute path — and the SDK resolves both back to
// an absolute path before sending. CommonJS is the shape the server sees more
// often, and running one of the two keeps this file a single program the way
// the Go suite is.
'use strict';

const Sentry = require('@sentry/node');

const RELEASE = 'compat@1.0.0';
const ENVIRONMENT = 'compat-test';

async function main() {
  const options = parseArguments(process.argv.slice(2));
  if (!options.dsn || !options.api || !options.token) {
    process.stderr.write('usage: compat-node -dsn ... -api ... -token ... [-project N]\n');
    process.exit(2);
  }

  try {
    await run(options);
  } catch (error) {
    process.stderr.write(`FAIL ${error.message}\n`);
    process.exit(1);
  }
  process.stdout.write('ok   the official Node SDK works against this server, DSN only\n');
}

// parseArguments accepts the single-dash spelling as well as the double-dash
// one. Go's flag package treats them as the same thing, so the runner passes
// `-dsn` to every suite and a Node suite that only understood `--dsn` would
// look broken for a reason that has nothing to do with the SDK.
function parseArguments(argv) {
  const options = { dsn: '', api: '', token: '', project: '1' };
  for (let index = 0; index < argv.length; index++) {
    const [name, inlineValue] = argv[index].replace(/^--?/, '').split(/=(.*)/s);
    if (!(name in options)) {
      continue;
    }
    options[name] = inlineValue !== undefined ? inlineValue : argv[++index] ?? '';
  }
  return options;
}

async function run(options) {
  // The whole configuration. If anything else were needed here, the
  // compatibility claim would be false.
  Sentry.init({
    dsn: options.dsn,
    release: RELEASE,
    environment: ENVIRONMENT,
  });

  Sentry.setTag('suite', 'compat-node');

  // The same error three times with a value that differs each time, so this
  // exercises grouping rather than counting.
  for (let attempt = 0; attempt < 3; attempt++) {
    capture(() => declineOrder(4800 + attempt));
  }
  // A genuinely different error, which must land in its own issue.
  capture(callUpstream);
  // And a message with no exception at all, which takes a different path
  // through grouping.
  Sentry.captureMessage('cache warm-up skipped');
  // A throw after an await. The runtime unwinds the synchronous call stack at
  // the first suspension point, so this arrives with a much shorter frame list
  // than a synchronous throw from the same file — no module-loader frames at
  // all. If grouping ever came to depend on frames the runtime happens to
  // supply, this is the event that would prove it.
  await captureAsync(runNightlyWorker);
  // A subclass that sets `name`, which is what makes the SDK report a type
  // other than "Error". A subclass that does not set it is indistinguishable
  // on the wire from a plain Error — see the note on decodeBody below for why
  // that matters here.
  capture(() => {
    throw new PaymentDeclined('card network unreachable');
  });
  // Two different errors from one function: same exception type, same frames,
  // messages differing only in one alphanumeric token. Nothing but the message
  // can separate them, which is exactly the case the fallback in the grouping
  // algorithm exists to handle.
  capture(() => decodeBody('utf8'));
  capture(() => decodeBody('base64'));

  if (!(await Sentry.flush(10_000))) {
    throw new Error('the SDK could not flush its events within ten seconds');
  }

  const issues = await readIssues(options);
  const grouped = issues.find((issue) => issue.times === 3);
  if (!grouped) {
    throw new Error(`no issue collected the three occurrences of one error:\n${render(issues)}`);
  }

  // What arrived is checked before how it grouped. A payload that lost a field
  // on the way in explains a mis-grouping; a mis-grouping explains nothing
  // about the payload, so reporting it first would send the reader the wrong
  // way.
  await checkStoredEvent(options, grouped);
  check(issues, grouped);
}

// The errors. Each throws from a named function so that the culprit the server
// derives names something, rather than being an artifact of where the suite
// happens to call from.

function declineOrder(orderID) {
  throw new Error(`payment declined for order ${orderID}`);
}

function callUpstream() {
  throw new Error('upstream timed out');
}

async function runNightlyWorker() {
  await new Promise((resolve) => setTimeout(resolve, 1));
  throw new Error('the nightly worker crashed');
}

// decodeBody is the one that hurts. Encoding names, digest names and protocol
// versions — utf8, base64, sha256, http2, ipv6 — are everywhere in Node error
// messages, and they are the whole difference between two unrelated bugs.
function decodeBody(encoding) {
  throw new Error(`unsupported encoding ${encoding}`);
}

class PaymentDeclined extends Error {
  constructor(message) {
    super(message);
    // Without this the SDK reports the type as "Error", because that is what
    // the runtime puts on the instance: `name` is inherited, not derived from
    // the class. A subclass that forgets it groups with every plain Error that
    // shares its message and call site, and nothing on the wire reveals that
    // two different classes were involved.
    this.name = 'PaymentDeclined';
  }
}

function capture(throwing) {
  try {
    throwing();
  } catch (error) {
    Sentry.captureException(error);
  }
}

async function captureAsync(throwing) {
  try {
    await throwing();
  } catch (error) {
    Sentry.captureException(error);
  }
}

async function readIssues(options) {
  // The listing is a page, not a bare array: it carries the status counts and
  // a cursor alongside the issues.
  const page = await getJSON(options, `/api/v1/projects/${options.project}/issues`);
  return page.issues;
}

async function getJSON(options, path) {
  const response = await fetch(`${options.api}${path}`, {
    headers: {
      Authorization: `Bearer ${options.token}`,
      'X-Trapline-Request': '1',
    },
  });
  if (!response.ok) {
    throw new Error(`reading ${path}: status ${response.status}: ${await response.text()}`);
  }
  return response.json();
}

// check reads the issue list, most specific assertion first.
//
// The total count is checked last on purpose. It is the assertion that fails
// for every possible reason, so putting it first would report "got 6, want 7"
// for a mis-grouping that the assertions below name exactly.
function check(issues, grouped) {
  if (grouped.last_release !== RELEASE) {
    throw new Error(`the release did not survive: ${JSON.stringify(grouped.last_release)}`);
  }
  // Not merely non-empty: the culprit has to name the function that threw, or
  // it was derived from something other than the stacktrace and would still
  // look right while pointing at the wrong place.
  if (!grouped.culprit.endsWith(' in declineOrder')) {
    throw new Error(`the culprit was not derived from the stacktrace: ${JSON.stringify(grouped.culprit)}`);
  }

  requireOwnIssue(issues, 'Error: upstream timed out', 'a second, unrelated exception');
  requireOwnIssue(issues, 'cache warm-up skipped', 'a message with no exception');

  // The async throw must produce a culprit too. It arrives with none of the
  // module-loader frames a synchronous throw carries, so a server that leaned
  // on those would come up empty here and nowhere else.
  const asynchronous = requireOwnIssue(issues, 'Error: the nightly worker crashed', 'a throw after an await');
  if (!asynchronous.culprit.endsWith(' in runNightlyWorker')) {
    throw new Error(
      `no culprit was derived from the async frames: ${JSON.stringify(asynchronous.culprit)}`,
    );
  }

  // The subclass has to keep its own type, or every custom error in an
  // application collapses into one issue per message.
  requireOwnIssue(issues, 'PaymentDeclined: card network unreachable', 'a custom Error subclass');

  checkEncodingPair(issues);

  // Seven distinct issues: the repeated exception, the other exception, the
  // message, the async one, the subclass, and the two that differ only by an
  // encoding name.
  if (issues.length !== 7) {
    throw new Error(`got ${issues.length} issues, want 7:\n${render(issues)}`);
  }
}

// checkEncodingPair asserts that two unrelated errors stayed apart.
//
// `unsupported encoding utf8` and `unsupported encoding base64` are two
// different bugs. They share an exception type and a stacktrace, so the
// normalised message is the only thing that can separate them — and both
// tokens are a letter followed by a digit, which the normaliser treats as an
// instance identity (host-01, worker3) and replaces. Both messages become
// `unsupported encoding <id>` and the two bugs become one issue, titled after
// whichever arrived last. The other one is not merged-and-visible; it is gone.
//
// This is the failure the algorithm's own notes call the one a user cannot
// see, and Node is where it bites hardest: utf8, base64, sha256, md5, http2,
// ipv6, oauth2, es2015 are ordinary words in this ecosystem's error messages,
// and they are usually the entire difference between two reports.
function checkEncodingPair(issues) {
  const titles = ['Error: unsupported encoding utf8', 'Error: unsupported encoding base64'];
  const missing = titles.filter((title) => !issues.some((issue) => issue.title === title));
  if (missing.length > 0) {
    throw new Error(
      'two errors differing only by an encoding name were grouped together; ' +
        `${missing.map((title) => JSON.stringify(title)).join(' and ')} never appeared:\n${render(issues)}`,
    );
  }
  for (const title of titles) {
    requireOwnIssue(issues, title, `the error ${JSON.stringify(title)}`);
  }
}

function requireOwnIssue(issues, title, description) {
  const matching = issues.filter((issue) => issue.title === title);
  if (matching.length !== 1) {
    throw new Error(`${description} did not land in exactly one issue of its own:\n${render(issues)}`);
  }
  if (matching[0].times !== 1) {
    throw new Error(`${description} was counted ${matching[0].times} times, want 1:\n${render(issues)}`);
  }
  return matching[0];
}

// checkStoredEvent looks at the payload the server kept, which is the only
// place the frame shape is visible. The issue list shows what grouping decided;
// this shows what it decided it from.
async function checkStoredEvent(options, grouped) {
  const detail = await getJSON(
    options,
    `/api/v1/projects/${options.project}/issues/${grouped.id}`,
  );

  const event = detail.events?.[0];
  if (!event) {
    throw new Error('the issue kept no event payload');
  }
  if (event.environment !== ENVIRONMENT) {
    throw new Error(`the environment did not survive: ${JSON.stringify(event.environment)}`);
  }

  const values = event.payload?.exception?.values ?? [];
  const frames = values[values.length - 1]?.stacktrace?.frames ?? [];
  if (frames.length === 0) {
    throw new Error('the stored event carries no stack frames');
  }

  // This SDK marks in_app per file rather than per module path: everything
  // outside node_modules and outside the `node:` namespace is the user's code.
  // Both halves matter. Without in-app frames grouping falls back to the whole
  // stack, and the runtime's own frames are identical for every error raised at
  // module scope — every unrelated bug in a process would be one issue.
  const inApp = frames.filter((frame) => frame.in_app);
  if (inApp.length === 0) {
    throw new Error('no frame was marked in_app, so grouping fell back to the whole stack');
  }
  const runtime = frames.filter((frame) => !frame.in_app && frame.filename?.startsWith('node:'));
  if (runtime.length === 0) {
    throw new Error("the runtime's own frames were marked as the user's code");
  }

  // As of 10.71.0 this SDK sends no abs_path at all — the absolute path is in
  // filename, and under ESM it is the file:// URL already resolved back to a
  // path. Grouping and the culprit both depend on there being a path here, so
  // the assertion is on the path being present rather than on which field
  // carried it.
  const pathless = inApp.find((frame) => !(frame.abs_path || frame.filename));
  if (pathless) {
    throw new Error(`an in-app frame arrived with no path at all: ${JSON.stringify(pathless)}`);
  }
}

function render(issues) {
  return `  ${JSON.stringify(
    issues.map(({ title, culprit, level, times, last_release: lastRelease }) => ({
      title,
      culprit,
      level,
      times,
      last_release: lastRelease,
    })),
    null,
    2,
  )}`;
}

main();
