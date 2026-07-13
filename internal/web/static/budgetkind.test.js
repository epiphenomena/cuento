// p19.3 schedule-form kind-picker unit tests (node --test). Covers the PURE
// visibility decision (blockVisible); the DOM glue is e2e-covered (budgets.spec.js).

const test = require('node:test');
const assert = require('node:assert');

// Import the ES module under Node. The module is guarded so importing it is
// side-effect free (no `document`).
let blockVisible;
test.before(async () => {
  ({ blockVisible } = await import('./budgetkind.js'));
});

test('blockVisible matches a kind in the space-separated list', () => {
  assert.equal(blockVisible('monthly', 'monthly'), true);
  assert.equal(blockVisible('annual', 'onetime annual biweekly'), true);
  assert.equal(blockVisible('biweekly', 'onetime annual biweekly'), true);
});

test('blockVisible rejects a kind not in the list', () => {
  assert.equal(blockVisible('custom', 'monthly'), false);
  assert.equal(blockVisible('weekly', 'onetime annual biweekly'), false);
});

test('blockVisible handles an empty list', () => {
  assert.equal(blockVisible('monthly', ''), false);
  assert.equal(blockVisible('monthly', null), false);
});
