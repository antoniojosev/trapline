// The application bundle, minified on the way out.
//
// It exists as a separate file so that the errors it raises carry mangled
// function names and a URL distinct from the page's other script — the shape a
// browser reports for every production build, and the one a hand-written
// fixture never has. The source map beside it is what the symbolication work
// will consume later; nothing here reads it.

export function decode(encoding) {
  throw new Error(`unsupported encoding ${encoding}`);
}

// Exposed on the global object because this bundle is loaded as a plain
// script, exactly as a built application would be.
window.__checkout = { decode };
