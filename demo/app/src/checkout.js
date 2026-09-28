'use strict';

// Pricing an order, discount codes included.
//
// This file contains the bug the demo is about, and it is the shape of bug
// that actually reaches production: the test suite is green, the code is
// readable, and it only fails for an input nobody wrote a test for.

const { subtotal, withTax } = require('./pricing');

// The promotions marketing is running. Codes are added when a campaign starts
// and removed when it ends.
const DISCOUNTS = {
  WELCOME10: { percent: 10 },
  BLACKFRIDAY: { percent: 25 },
};

// applyDiscount takes the percentage off, in cents.
function applyDiscount(amount, code) {
  const rule = DISCOUNTS[code];
  return amount - (amount * rule.percent) / 100;
}

// total prices a whole order.
function total(order) {
  const base = subtotal(order.items);
  const discounted = order.discountCode ? applyDiscount(base, order.discountCode) : base;
  return Math.round(withTax(discounted));
}

module.exports = { total, applyDiscount, DISCOUNTS };
