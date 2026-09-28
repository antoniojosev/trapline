#!/usr/bin/env node
// Captures the envelope a browser SDK sends for an error raised from a bundle
// with debug ids in it, and writes it down unchanged.
//
// There is no server under test here. The whole point is to keep what the SDK
// puts on the wire before anything of ours has touched it, so the work that
// resolves minified frames later is written against an event that actually
// happened rather than against somebody's idea of one. This project's standing
// lesson is that those two differ in ways nobody guesses (ADR 002).
//
// It runs inside the Playwright image, where the browser already lives. Two
// servers, both on the container's loopback and on different ports: one serves
// the page, the other stands in for ingest. Different origins on purpose —
// that is the only situation a browser SDK is ever in, and a same-origin
// capture would quietly hide the CORS preflight from the recording.
'use strict';

const http = require('node:http');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('playwright');

const DIST = path.join(__dirname, 'dist');
const PROJECT_ID = '1';
// Fixed ports, inside the container's own network namespace where nothing else
// is listening. Letting the OS choose would put a different number into the
// committed event's abs_path on every run, and a fixture that churns for a
// reason nobody can name is a fixture nobody re-records.
const PAGE_PORT = 8080;
const INGEST_PORT = 8081;
const PUBLIC_KEY = 'e5b1a0c2d3f4456789abcdef01234567';

const CONTENT_TYPES = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.map': 'application/json; charset=utf-8',
};

async function main() {
  const out = parseArguments(process.argv.slice(2));
  if (!out) {
    process.stderr.write('usage: record-envelope.js -out <directory>\n');
    process.exit(2);
  }

  const ingest = await serveIngest();
  const dsn = `http://${PUBLIC_KEY}@127.0.0.1:${INGEST_PORT}/${PROJECT_ID}`;
  const page = await servePage(dsn);

  try {
    await raiseTheError(page.origin);
    if (ingest.envelopes.length === 0) {
      throw new Error('the SDK sent nothing at all');
    }
    write(out, ingest.envelopes);
  } catch (error) {
    process.stderr.write(`FAIL ${error.message}\n`);
    process.exitCode = 1;
  } finally {
    page.close();
    ingest.close();
  }
}

function parseArguments(argv) {
  const index = argv.indexOf('-out');
  return index >= 0 && argv[index + 1] ? argv[index + 1] : null;
}

// serveIngest stands in for the ingest endpoint and keeps the bodies.
//
// It answers the CORS preflight, because without it the browser never sends
// the POST and the capture would end with "the SDK sent nothing" for a reason
// that has nothing to do with the SDK.
function serveIngest() {
  const envelopes = [];
  return new Promise((resolve, reject) => {
    const server = http.createServer((request, response) => {
      response.setHeader('access-control-allow-origin', '*');
      response.setHeader('access-control-allow-headers', '*');
      response.setHeader('access-control-allow-methods', 'POST, OPTIONS');
      if (request.method === 'OPTIONS') {
        response.writeHead(204).end();
        return;
      }
      const chunks = [];
      request.on('data', (chunk) => chunks.push(chunk));
      request.on('end', () => {
        envelopes.push({ url: request.url, body: Buffer.concat(chunks) });
        response.writeHead(200, { 'content-type': 'application/json' }).end('{}');
      });
    });
    server.on('error', reject);
    server.listen(INGEST_PORT, '127.0.0.1', () =>
      resolve({ envelopes, close: () => server.close() }),
    );
  });
}

function servePage(dsn) {
  return new Promise((resolve, reject) => {
    const server = http.createServer((request, response) => {
      const url = new URL(request.url, 'http://127.0.0.1');
      if (url.pathname === '/config.js') {
        response.writeHead(200, { 'content-type': CONTENT_TYPES['.js'] });
        response.end(`window.__COMPAT_DSN = ${JSON.stringify(dsn)};\n`);
        return;
      }
      const name = url.pathname === '/' ? '/envelope.html' : url.pathname;
      const file = path.join(DIST, path.normalize(name).replace(/^(\.\.[/\\])+/, ''));
      fs.readFile(file, (error, body) => {
        if (error) {
          response.writeHead(404).end('not found');
          return;
        }
        response
          .writeHead(200, {
            'content-type': CONTENT_TYPES[path.extname(file)] || 'application/octet-stream',
          })
          .end(body);
      });
    });
    server.on('error', reject);
    server.listen(PAGE_PORT, '127.0.0.1', () =>
      resolve({ origin: `http://127.0.0.1:${PAGE_PORT}`, close: () => server.close() }),
    );
  });
}

async function raiseTheError(pageOrigin) {
  const browser = await chromium.launch({ args: ['--no-sandbox'] });
  const page = await browser.newPage();
  const consoleErrors = [];
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
      throw new Error(`the SDK could not flush: ${consoleErrors.join('; ')}`);
    }
  } finally {
    await browser.close();
  }
}

// itemType reads the type of an envelope's first item, which is the line after
// the envelope header.
function itemType(body) {
  const lines = body.toString('utf8').split('\n');
  if (lines.length < 2) {
    return 'malformed';
  }
  try {
    return JSON.parse(lines[1]).type || 'untyped';
  } catch {
    return 'malformed';
  }
}

// write saves the envelope and the artefacts it refers to, side by side.
//
// The bundle and its map go with it deliberately. An envelope naming a debug
// id is inert on its own: whoever resolves those frames needs the very map
// that debug id was injected into, and a fixture that kept only the event
// would send them looking for it.
function write(out, envelopes) {
  fs.mkdirSync(out, { recursive: true });

  // Not envelopes[0]. `Sentry.init` opens a session and sends it before
  // anything has gone wrong, so the first envelope on the wire carries no
  // event at all — a fixture taken from it would be an empty session object
  // and the mistake would only surface much later, in a resolver that finds
  // nothing to resolve.
  const captured = envelopes.find((envelope) => itemType(envelope.body) === 'event');
  if (!captured) {
    throw new Error(
      `no envelope carried an event; the ${envelopes.length} that arrived were ` +
        `${envelopes.map((envelope) => itemType(envelope.body)).join(', ')}`,
    );
  }
  fs.writeFileSync(path.join(out, 'browser-debugid.envelope'), captured.body);
  for (const name of ['bundle.min.js', 'bundle.min.js.map']) {
    fs.copyFileSync(path.join(DIST, name), path.join(out, name));
  }

  const event = JSON.parse(captured.body.toString('utf8').split('\n')[2]);
  const images = (event.debug_meta && event.debug_meta.images) || [];
  if (images.length === 0) {
    throw new Error(
      'the event carried no debug_meta. The bundle was served without the injected snippet, ' +
        'or the SDK found no frame it could attribute to it — either way the fixture would be ' +
        'useless for resolving anything.',
    );
  }
  process.stdout.write(
    `ok   ${captured.body.length} bytes, ${images.length} debug image(s): ` +
      `${images.map((image) => `${image.code_file} ${image.debug_id}`).join(', ')}\n`,
  );
}

main();
