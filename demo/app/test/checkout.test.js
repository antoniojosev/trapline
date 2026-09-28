'use strict';

const test = require('node:test');
const assert = require('node:assert');

const { total } = require('../src/checkout');

const ORDER = {
  id: 'ord_4412',
  items: [
    { sku: 'cafe-500g', unitPrice: 1200, quantity: 2 },
    { sku: 'filtros-x100', unitPrice: 350, quantity: 1 },
  ],
};

test('an order with no discount code', () => {
  assert.strictEqual(total(ORDER), 3190);
});

test('an order with a live discount code', () => {
  assert.strictEqual(total({ ...ORDER, discountCode: 'WELCOME10' }), 2871);
});

test('the bigger campaign takes more off', () => {
  assert.strictEqual(total({ ...ORDER, discountCode: 'BLACKFRIDAY' }), 2393);
});
