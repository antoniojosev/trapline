'use strict';

// A checkout service, small enough to read in one sitting and instrumented the
// way a real one is: the official Sentry SDK, configured with nothing but a
// DSN, a release and an environment.
//
// The DSN points at this demo's own server. That is the whole claim of the
// product — you change the DSN and the official SDK works — so the demo would
// be dishonest if it used anything else.

const Sentry = require('@sentry/node');

const RELEASE = process.env.APP_RELEASE || 'checkout@0.0.0-dev';
const ENVIRONMENT = process.env.APP_ENVIRONMENT || 'production';
const PORT = Number(process.env.APP_PORT || 3000);

Sentry.init({
  dsn: process.env.DEMO_DSN,
  release: RELEASE,
  environment: ENVIRONMENT,
  // Tracing on, at 100%: this is a demo with a handful of requests, and the
  // point is that the same binary that took the error also has the latency.
  tracesSampleRate: 1.0,
});

const http = require('node:http');
const { total } = require('./checkout');

// Which machine served the request. It ends up as a tag on the issue, which is
// what tells somebody reading it whether one instance is misbehaving or all of
// them are.
const INSTANCE = process.env.HOSTNAME || 'web-01';

function respond(response, status, body) {
  const encoded = JSON.stringify(body);
  response.writeHead(status, {
    'Content-Type': 'application/json',
    'Content-Length': Buffer.byteLength(encoded),
  });
  response.end(encoded);
}

async function readJSON(request) {
  const chunks = [];
  for await (const chunk of request) {
    chunks.push(chunk);
  }
  return JSON.parse(Buffer.concat(chunks).toString('utf8') || '{}');
}

async function checkout(request, response) {
  const order = await readJSON(request);

  // Breadcrumbs are the half of a bundle that says what happened *before*. The
  // demo leans on this: the order id and the code are in the trail, so the
  // person reading the issue knows which request to replay.
  Sentry.addBreadcrumb({
    category: 'checkout',
    level: 'info',
    message: `order ${order.id} received with ${order.items?.length ?? 0} lines`,
  });
  if (order.discountCode) {
    Sentry.addBreadcrumb({
      category: 'checkout',
      level: 'info',
      message: `discount code ${order.discountCode} presented`,
    });
  }

  // An explicit span rather than relying on auto-instrumentation, so the demo
  // asserts on a transaction that is guaranteed to exist rather than on one
  // that depends on which integrations the SDK enabled today.
  const amount = await Sentry.startSpan(
    { name: 'POST /checkout', op: 'http.server' },
    () => total(order),
  );

  respond(response, 200, { orderId: order.id, total: amount, currency: 'USD' });
}

const server = http.createServer((request, response) => {
  if (request.url === '/health') {
    respond(response, 200, { ok: true, release: RELEASE });
    return;
  }
  if (request.method !== 'POST' || request.url !== '/checkout') {
    respond(response, 404, { error: 'not found' });
    return;
  }

  Sentry.withScope(async (scope) => {
    scope.setTag('instance', INSTANCE);
    try {
      await checkout(request, response);
    } catch (error) {
      scope.setTag('endpoint', 'checkout');
      Sentry.captureException(error);
      // A demo that raced the SDK's background queue would be a demo that
      // sometimes works. The service is not under load; waiting is free.
      await Sentry.flush(5000);
      respond(response, 500, { error: 'checkout failed' });
    }
  });
});

server.listen(PORT, '0.0.0.0', () => {
  process.stdout.write(`checkout ${RELEASE} listening on ${PORT} (${ENVIRONMENT})\n`);
});

for (const signal of ['SIGINT', 'SIGTERM']) {
  process.on(signal, () => {
    server.close(() => process.exit(0));
  });
}
