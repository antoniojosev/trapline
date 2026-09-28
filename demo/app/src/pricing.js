'use strict';

// The arithmetic of an order, with no knowledge of promotions, HTTP or error
// tracking. It exists so that the bug in checkout.js lives in one file and the
// suspect-commit attribution has something to be right about.

const TAX_RATE = 0.16;

// subtotal adds up the lines of an order, in cents.
function subtotal(items) {
  return items.reduce((sum, item) => sum + item.unitPrice * item.quantity, 0);
}

// withTax applies the sales tax to an amount already in cents.
function withTax(amount) {
  return amount * (1 + TAX_RATE);
}

module.exports = { subtotal, withTax, TAX_RATE };
