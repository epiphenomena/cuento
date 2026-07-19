// Unit tests for the PURE budget-variance measure-toggle helpers (budgetvariance.js).
// Run under `node --test`. These cover the state transition the DOM click handler
// delegates to: setting the table's data-measure and syncing the button pressed-state.
// The DOM wiring (event listeners, htmx re-init) is e2e-covered; only the pure decision
// lives here. Tiny stubs stand in for the table/buttons (applyMeasure only touches
// .dataset and .setAttribute), so no browser/jsdom is needed.

import test from 'node:test';
import assert from 'node:assert/strict';

let MEASURES, applyMeasure, measureFromButton;
test.before(async () => {
  ({ MEASURES, applyMeasure, measureFromButton } = await import('./budgetvariance.js'));
});

// A stub button: a data-measure and a recorded aria-pressed attribute.
function stubButton(measure) {
  return {
    dataset: { measure },
    pressed: null,
    setAttribute(name, value) {
      if (name === 'aria-pressed') this.pressed = value;
    },
  };
}

function stubTable(initial) {
  return { dataset: { measure: initial } };
}

test('MEASURES is exactly budgeted/actual/variance in button order', () => {
  assert.deepEqual(MEASURES, ['budgeted', 'actual', 'variance']);
});

test('applyMeasure sets the table measure and presses exactly the matching button', () => {
  const table = stubTable('variance');
  const buttons = MEASURES.map(stubButton);

  assert.equal(applyMeasure(table, buttons, 'actual'), true);
  assert.equal(table.dataset.measure, 'actual');
  // Only the "actual" button is pressed; the others are explicitly unpressed.
  assert.deepEqual(
    buttons.map((b) => b.pressed),
    ['false', 'true', 'false'],
  );
});

test('applyMeasure switches again cleanly (no stale pressed state)', () => {
  const table = stubTable('variance');
  const buttons = MEASURES.map(stubButton);

  applyMeasure(table, buttons, 'actual');
  applyMeasure(table, buttons, 'budgeted');
  assert.equal(table.dataset.measure, 'budgeted');
  assert.deepEqual(
    buttons.map((b) => b.pressed),
    ['true', 'false', 'false'],
  );
});

test('applyMeasure ignores an unknown measure (grid never blanks)', () => {
  const table = stubTable('variance');
  const buttons = MEASURES.map(stubButton);

  assert.equal(applyMeasure(table, buttons, 'bogus'), false);
  assert.equal(table.dataset.measure, 'variance'); // unchanged
  assert.deepEqual(
    buttons.map((b) => b.pressed),
    [null, null, null], // never touched
  );
});

test('measureFromButton returns a valid measure or null', () => {
  assert.equal(measureFromButton(stubButton('variance')), 'variance');
  assert.equal(measureFromButton(stubButton('bogus')), null);
  assert.equal(measureFromButton({ dataset: {} }), null);
  assert.equal(measureFromButton({}), null);
});
