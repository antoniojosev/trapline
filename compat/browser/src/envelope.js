// Raises one error from a bundle that has debug ids in it, so the envelope the
// SDK sends can be kept.
//
// The bundle is loaded first and separately, minified, with `sentry-cli
// sourcemaps inject` already run over it. That is the only way the event grows
// a `debug_meta`: the injected snippet registers the bundle's debug id on
// `window._sentryDebugIds` against a stack trace, and the SDK matches the
// frames of a real error against that map when it builds the event. Neither
// half works alone — an error from a bundle without the snippet carries no
// debug ids at all, and the snippet on its own says nothing until something
// throws — which is why the fixture has to be produced this way rather than
// written by hand.
import * as Sentry from '@sentry/browser';

Sentry.init({
  dsn: window.__COMPAT_DSN,
  release: 'compat@1.0.0',
  environment: 'compat-test',
});

async function main() {
  try {
    // From the minified bundle, so the frames are mangled and the resolution
    // this fixture exists to test has something to do.
    window.__checkout.decode('utf8');
  } catch (error) {
    Sentry.captureException(error);
  }
  window.__compatFlushed = await Sentry.flush(10_000);
  window.__compatDone = true;
}

main().catch((error) => {
  window.__compatCrashed = String((error && error.stack) || error);
  window.__compatDone = true;
});
