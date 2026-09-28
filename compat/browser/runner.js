#!/usr/bin/env node
// Command compat-browser drives the official browser SDK in a real Chromium
// and checks what arrived.
//
// It runs inside the Playwright image, where the browser already lives. The
// page it loads is served from a throwaway server on the container's own
// loopback, so the page's origin is NOT the server under test — which is the
// situation every real browser deployment is in, and the one a suite serving
// the page from the product itself would never reproduce.
'use strict';

const http = require('node:http');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('playwright');

const RELEASE = 'compat@1.0.0';
const ENVIRONMENT = 'compat-test';
const DIST = path.join(__dirname, 'dist');

// Three occurrences of one error, one of another, a message, two from the
// minified bundle, one uncaught throw and one unhandled rejection — seven
// issues if grouping is right, and a different number for every way it can be
// wrong.
const EXPECTED_ISSUES = 7;

const CONTENT_TYPES = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.map': 'application/json; charset=utf-8',
};

async function main() {
  const options = parseArguments(process.argv.slice(2));
  if (!options.dsn || !options.api || !options.token) {
    process.stderr.write('usage: runner.js -dsn ... -api ... -token ... [-project N]\n');
    process.exit(2);
  }

  const server = await serve(options.dsn);
  try {
    await run(options, server.origin);
  } catch (error) {
    process.stderr.write(`FAIL ${error.message}\n`);
    process.exitCode = 1;
    return;
  } finally {
    server.close();
  }
  process.stdout.write('ok   the official browser SDK works against this server, DSN only\n');
}

// serve publishes the built page on the container's loopback. A different
// origin from the server under test on purpose: same-origin would hide every
// cross-origin problem, and cross-origin is the only situation a browser SDK
// is ever in.
// ingestURL derives the endpoint from the DSN exactly as an SDK does. The
// backpressure probe needs it directly, because what it is testing is the
// response an SDK would have received.
function ingestURL(dsn) {
  const parsed = new URL(dsn);
  const projectID = parsed.pathname.replace(/^\//, '');
  return `${parsed.protocol}//${parsed.host}/api/${projectID}/envelope/?sentry_key=${parsed.username}`;
}

function serve(dsn) {
  return new Promise((resolve, reject) => {
    const server = http.createServer((request, response) => {
      const url = new URL(request.url, 'http://127.0.0.1');
      if (url.pathname === '/config.js') {
        response.writeHead(200, { 'content-type': CONTENT_TYPES['.js'] });
        response.end(
          `window.__COMPAT_DSN = ${JSON.stringify(dsn)};\n` +
            `window.__COMPAT_INGEST_URL = ${JSON.stringify(ingestURL(dsn))};\n`,
        );
        return;
      }
      const name = url.pathname === '/' ? '/index.html' : url.pathname;
      const file = path.join(DIST, path.normalize(name).replace(/^(\.\.[/\\])+/, ''));
      fs.readFile(file, (error, body) => {
        if (error) {
          response.writeHead(404).end('not found');
          return;
        }
        response.writeHead(200, { 'content-type': CONTENT_TYPES[path.extname(file)] || 'application/octet-stream' });
        response.end(body);
      });
    });
    server.on('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const { port } = server.address();
      resolve({ origin: `http://127.0.0.1:${port}`, close: () => server.close() });
    });
  });
}

async function run(options, pageOrigin) {
  const browser = await chromium.launch({ args: ['--no-sandbox'] });
  const page = await browser.newPage();
  let probe = null;

  // Everything the browser refused to do is collected here. A browser does not
  // fail loudly when it blocks a request: the page keeps running and the
  // events simply never arrive, so without this the symptom would be an empty
  // issue list and no explanation anywhere.
  const blocked = [];
  const consoleErrors = [];
  page.on('requestfailed', (request) => {
    blocked.push(`${request.method()} ${request.url()} — ${request.failure()?.errorText}`);
  });
  page.on('console', (message) => {
    if (message.type() === 'error') {
      consoleErrors.push(message.text());
    }
  });

  try {
    await page.goto(`${pageOrigin}/`, { waitUntil: 'load' });
    await page.waitForFunction(() => window.__compatDone === true, null, { timeout: 30_000 });

    const crashed = await page.evaluate(() => window.__compatCrashed);
    if (crashed) {
      throw new Error(`the page itself failed: ${crashed}`);
    }
    if (!(await page.evaluate(() => window.__compatFlushed))) {
      throw new Error(
        'the SDK could not deliver its events from a page the server did not serve.' +
          diagnose(blocked, consoleErrors),
      );
    }
    probe = await page.evaluate(() => window.__compatProbe);
  } finally {
    await browser.close();
  }

  checkBackpressure(probe);

  const issues = await readIssues(options);
  if (issues.length === 0) {
    throw new Error(`nothing arrived at all.${diagnose(blocked, consoleErrors)}`);
  }
  // What arrived is checked before how it grouped. A payload that lost a field
  // on the way in explains a mis-grouping; a mis-grouping explains nothing
  // about the payload, so reporting it first would send the reader the wrong
  // way.
  await checkStoredEvent(options, issues);
  check(issues);
}

// checkBackpressure asserts that an SDK in a browser can read the answer.
//
// This is the assertion the rest of the suite cannot make. A cross-origin POST
// is delivered whether or not the server allows the origin — the browser only
// blocks the page from reading the response — so a server with no policy looks
// perfectly healthy from the outside: events arrive, the log says 200. What is
// lost is the response, and the response is where the protocol's backpressure
// lives (ADR 005). Without it the SDK cannot see a 429, cannot see for how
// long, and keeps sending a category the project switched off.
function checkBackpressure(probe) {
  if (!probe) {
    throw new Error('the page never reported whether it could read the ingest response');
  }
  if (!probe.readable) {
    throw new Error(
      'the browser would not let the page read the ingest response, so the SDK cannot see ' +
        `rate limits at all and will keep sending a refused category: ${probe.error}`,
    );
  }
  if (probe.status !== 429) {
    throw new Error(
      `a session envelope on a project that accepts only errors answered ${probe.status}, want 429`,
    );
  }
  if (!probe.rateLimits) {
    throw new Error(
      'the 429 carried no readable X-Sentry-Rate-Limits header. The server sends it, but a ' +
        'browser hides every response header that is not exposed, so the SDK cannot honour it ' +
        'and a switched-off category costs full price on the wire (ADR 005).',
    );
  }
  if (!probe.retryAfter) {
    throw new Error('the 429 carried no readable Retry-After header');
  }
}

function diagnose(blocked, consoleErrors) {
  let report = '';
  if (blocked.length > 0) {
    report += `\n  the browser blocked ${blocked.length} request(s):\n    ${blocked.join('\n    ')}`;
  }
  if (consoleErrors.length > 0) {
    report += `\n  the page logged:\n    ${consoleErrors.join('\n    ')}`;
  }
  return report;
}

// check reads the issue list, most specific assertion first.
//
// The total count is checked last on purpose. It is the assertion that fails
// for every possible reason, so putting it first would report "got 6, want 7"
// for a mis-grouping that the assertions below name exactly.
function check(issues) {
  const grouped = issues.find((issue) => issue.times === 3);
  if (!grouped) {
    throw new Error(`no issue collected the three occurrences of one error:\n${render(issues)}`);
  }
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

  // An uncaught throw and an unhandled rejection reach the SDK through the
  // browser's global handlers rather than through a capture call. They are how
  // a browser SDK sees most real errors, and they build their events on a
  // different path.
  const uncaught = requireOwnIssue(issues, 'Error: the render loop crashed', 'an uncaught throw');
  if (uncaught.culprit === '') {
    throw new Error('an uncaught throw produced no culprit, so its stacktrace did not survive');
  }
  requireOwnIssue(issues, 'Error: the checkout session expired', 'an unhandled promise rejection');

  checkMinifiedPair(issues);

  if (issues.length !== EXPECTED_ISSUES) {
    throw new Error(`got ${issues.length} issues, want ${EXPECTED_ISSUES}:\n${render(issues)}`);
  }
}

// checkMinifiedPair asserts that two unrelated errors from one minified
// function stayed apart.
//
// This is the Node suite's encoding pair with everything that could have saved
// it removed. Both errors come from the same mangled function in the same
// bundle, so the frames are identical, and minification means the function
// name carries no meaning either. Only the normalised message is left, and
// both `utf8` and `base64` are the letters-then-digits shape a normaliser once
// mistook for a machine name.
function checkMinifiedPair(issues) {
  for (const encoding of ['utf8', 'base64']) {
    requireOwnIssue(issues, `Error: unsupported encoding ${encoding}`, `the error about ${encoding}`);
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
// place the frame shape is visible. The issue list shows what grouping
// decided; this shows what it decided it from.
async function checkStoredEvent(options, issues) {
  const minified = issues.find((issue) => issue.title === 'Error: unsupported encoding utf8');
  if (!minified) {
    return; // check() reports this with more context.
  }

  const detail = await getJSON(options, `/api/v1/projects/${options.project}/issues/${minified.id}`);
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
  const inApp = frames.filter((frame) => frame.in_app);
  if (inApp.length === 0) {
    throw new Error('no frame was marked in_app, so grouping fell back to the whole stack');
  }

  // A browser reports a frame's location as the script's URL, not as a path.
  // Everything downstream — grouping's path normalisation, the culprit, and
  // the source-map resolution that comes later — reads it out of this field,
  // so a frame arriving without one is not a cosmetic loss.
  const innermost = inApp[inApp.length - 1];
  const location = innermost.abs_path || innermost.filename || '';
  if (!location.startsWith('http://') && !location.startsWith('https://')) {
    throw new Error(`a browser frame arrived without an http(s) URL: ${JSON.stringify(innermost)}`);
  }
  if (!location.endsWith('/bundle.min.js')) {
    throw new Error(`the innermost frame does not name the bundle that threw: ${JSON.stringify(location)}`);
  }

  // The user agent is what makes a browser issue actionable — "only Safari"
  // is the answer to half of them — and the SDK sends it as a header rather
  // than in the payload, so it exists only if the server put it there.
  const browserContext = event.payload?.contexts?.browser;
  if (!browserContext || !browserContext.name) {
    throw new Error(
      'the event carries no browser context: the SDK sends the user agent as a request header, ' +
        `not in the payload, so nothing recorded which browser this was. contexts: ${JSON.stringify(
          Object.keys(event.payload?.contexts ?? {}),
        )}`,
    );
  }
  // This suite drives a headless Chromium, which says so in its User-Agent.
  // Asserting on the family rather than the exact name keeps the check honest
  // without pinning it to one spelling: what must not happen is a browser
  // being recorded as some other browser.
  if (!/chrome/i.test(browserContext.name)) {
    throw new Error(
      `a headless Chromium was recorded as ${JSON.stringify(browserContext.name)}`,
    );
  }
  if (!browserContext.version) {
    throw new Error(`the browser context carries no version: ${JSON.stringify(browserContext)}`);
  }
}

async function readIssues(options) {
  const page = await getJSON(options, `/api/v1/projects/${options.project}/issues`);
  return page.issues;
}

async function getJSON(options, urlPath) {
  const response = await fetch(`${options.api}${urlPath}`, {
    headers: { Authorization: `Bearer ${options.token}`, 'X-Trapline-Request': '1' },
  });
  if (!response.ok) {
    throw new Error(`reading ${urlPath}: status ${response.status}: ${await response.text()}`);
  }
  return response.json();
}

// parseArguments accepts the single-dash spelling as well as the double-dash
// one, so the runner can pass `-dsn` to every suite in the matrix.
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
