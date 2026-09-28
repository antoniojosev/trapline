// Drives the official browser SDK on a real page in a real Chromium.
//
// It uses the real SDK, unmodified, configured with nothing but a DSN — which
// is the exact claim being tested. Anything this program has to work around is
// an incompatibility, and it says so rather than adapting.
//
// The browser is the one client in the matrix that cannot be trusted to
// deliver what it was given. Every other SDK opens a socket and writes; this
// one asks a browser for permission first, from a page served by somebody
// else's origin, and the browser will refuse silently if the server does not
// say the right thing back. Nothing else in the matrix can discover that.
import * as Sentry from '@sentry/browser';

const RELEASE = 'compat@1.0.0';
const ENVIRONMENT = 'compat-test';

// The whole configuration. If anything else were needed here, the
// compatibility claim would be false.
Sentry.init({
  dsn: window.__COMPAT_DSN,
  release: RELEASE,
  environment: ENVIRONMENT,
});

Sentry.setTag('suite', 'compat-browser');

// The errors. Each throws from a named function so that the culprit the server
// derives names something, rather than being an artifact of where the suite
// happens to call from.

function declineOrder(orderID) {
  throw new Error(`payment declined for order ${orderID}`);
}

function callUpstream() {
  throw new Error('upstream timed out');
}

function capture(throwing) {
  try {
    throwing();
  } catch (error) {
    Sentry.captureException(error);
  }
}

async function main() {
  // The same error three times with a value that differs each time, so this
  // exercises grouping rather than counting.
  for (let attempt = 0; attempt < 3; attempt++) {
    capture(() => declineOrder(4800 + attempt));
  }
  // A genuinely different error, which must land in its own issue.
  capture(callUpstream);
  // A message with no exception at all, which takes a different path through
  // grouping.
  Sentry.captureMessage('cache warm-up skipped');

  // Two unrelated bugs raised from one minified function. Their frames are
  // identical down to the mangled name, so only the message separates them —
  // and `utf8` and `base64` are the exact tokens a normaliser once erased.
  capture(() => window.__checkout.decode('utf8'));
  capture(() => window.__checkout.decode('base64'));

  // An error nobody caught. The SDK's global handler picks it up through
  // window.onerror, which is a different path into the event builder than
  // captureException, and it is how a browser SDK sees most real errors.
  window.setTimeout(() => {
    throw new Error('the render loop crashed');
  }, 0);

  // A rejected promise nobody awaited, which arrives through
  // onunhandledrejection — a third path, and the one that produces an event
  // with no `throw` site at all.
  //
  // Deliberately a tick later than the uncaught throw above. The SDK's
  // setTimeout wrapper catches that throw, reports it itself, and then asks
  // the global onerror handler to ignore the next error it sees — a flag it
  // clears on the following task. A rejection landing inside that window is
  // dropped by the SDK before it reaches any transport, which looks from here
  // exactly like a server that lost an event. Separating them by a tick keeps
  // this suite measuring the server.
  await new Promise((resolve) => window.setTimeout(resolve, 100));
  Promise.reject(new Error('the checkout session expired'));

  // Long enough for the two global handlers to have fired before the flush.
  await new Promise((resolve) => window.setTimeout(resolve, 250));

  window.__compatFlushed = await Sentry.flush(10_000);
  await probeBackpressure();
  window.__compatDone = true;
}

// probeBackpressure asks the one question the SDK cannot answer for us.
//
// A cross-origin POST with a plain body is a "simple" request: the browser
// sends it whether or not the server allows the origin, and only blocks the
// page from reading the answer. So events arriving proves nothing about
// whether the SDK could see what came back — and what comes back is where the
// protocol's backpressure lives. X-Sentry-Rate-Limits is how a server tells an
// SDK to stop sending a category, and an SDK that cannot read it keeps sending
// a refused category forever, which is precisely the cost ADR 005 says a
// switched-off subsystem must not have.
//
// Sessions are off on a new project, so this envelope is refused by design:
// the answer must be a readable 429 carrying a readable rate-limit header.
async function probeBackpressure() {
  const envelope =
    `${JSON.stringify({ dsn: window.__COMPAT_DSN })}\n` +
    `${JSON.stringify({ type: 'session' })}\n` +
    `${JSON.stringify({ sid: 'c6a1f1e0c0f14a2f9d1b3e4a5c6d7e8f', status: 'ok', errors: 0 })}\n`;

  try {
    const response = await fetch(window.__COMPAT_INGEST_URL, { method: 'POST', body: envelope });
    window.__compatProbe = {
      readable: true,
      status: response.status,
      rateLimits: response.headers.get('x-sentry-rate-limits'),
      retryAfter: response.headers.get('retry-after'),
    };
  } catch (error) {
    window.__compatProbe = { readable: false, error: String(error) };
  }
}

main().catch((error) => {
  window.__compatCrashed = String((error && error.stack) || error);
  window.__compatDone = true;
});
