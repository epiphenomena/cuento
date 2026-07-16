// p26.61 bank-import amount-mode field-toggle unit tests (node --test). Covers the
// PURE visibility decision (fieldVisible); the DOM glue is e2e-covered
// (bank-import.spec.js).

const test = require('node:test');
const assert = require('node:assert');

// Import the ES module under Node. The module is guarded so importing it is
// side-effect free (no `document`).
let fieldVisible;
test.before(async () => {
  ({ fieldVisible } = await import('./importmapping.js'));
});

test('fieldVisible shows the single column only in single mode', () => {
  assert.equal(fieldVisible('single', 'single'), true);
  assert.equal(fieldVisible('debit_credit', 'single'), false);
});

test('fieldVisible shows the debit/credit columns only in debit_credit mode', () => {
  assert.equal(fieldVisible('debit_credit', 'debit_credit'), true);
  assert.equal(fieldVisible('single', 'debit_credit'), false);
});

test('fieldVisible matches a mode in a space-separated list', () => {
  assert.equal(fieldVisible('single', 'single debit_credit'), true);
  assert.equal(fieldVisible('debit_credit', 'single debit_credit'), true);
});

test('fieldVisible handles an empty list', () => {
  assert.equal(fieldVisible('single', ''), false);
  assert.equal(fieldVisible('single', null), false);
});
