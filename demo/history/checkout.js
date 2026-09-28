'use strict';

// Pricing an order.
//
// This is `src/checkout.js` as it was *before* discount codes existed. It is
// kept because the demo needs a repository with real history: suspect-commit
// attribution crosses the files a commit touched against the frames of a
// stacktrace (ADR 019), and a repository with one commit has nothing to
// attribute. `demo.sh` commits this file first and the current one second, so
// the commit that introduced the bug is a real commit that really touched
// `src/checkout.js`.

const { subtotal, withTax } = require('./pricing');

// total prices a whole order.
function total(order) {
  return Math.round(withTax(subtotal(order.items)));
}

module.exports = { total };
