'use strict';

const test = require('node:test');
const assert = require('node:assert');

const { subtotal, withTax } = require('../src/pricing');

test('subtotal adds the lines', () => {
  assert.strictEqual(
    subtotal([
      { unitPrice: 1200, quantity: 2 },
      { unitPrice: 350, quantity: 1 },
    ]),
    2750,
  );
});

test('an empty order costs nothing', () => {
  assert.strictEqual(subtotal([]), 0);
});

test('tax is applied on top', () => {
  assert.strictEqual(withTax(1000), 1160);
});
