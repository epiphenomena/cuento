// Unit tests for the PURE recon-row forward decision (reconrow.js). Run under
// `node --test`. These cover the double-toggle-avoidance logic: a row-level click
// forwards to the toggle button ONLY when it did not land on an interactive child and a
// toggle exists (non-finalized). The DOM wiring (delegated listener, synthetic click) is
// e2e-covered (reconcile.spec.js); only the pure decision lives here.

import test from 'node:test';
import assert from 'node:assert/strict';

let forwardTarget;
test.before(async () => {
  ({ forwardTarget } = await import('./reconrow.js'));
});

// A stubbed toggle button (the object identity is what forwardTarget returns).
const toggle = { tag: 'button', className: 'recon-toggle' };

// closestStub builds a closest(el, sel) that reports whether `el` matches/ancestors an
// interactive selector, driven by the element's `interactive` flag.
function closestStub(el, sel) {
  if (sel === 'a, button') return el.interactive ? el : null;
  return null;
}

test('forwards to the toggle when a blank cell is clicked (non-finalized)', () => {
  const cell = { interactive: false };
  assert.equal(
    forwardTarget(cell, closestStub, () => toggle),
    toggle,
  );
});

test('does NOT forward when the toggle button itself is clicked (no double toggle)', () => {
  const btn = { interactive: true };
  assert.equal(
    forwardTarget(btn, closestStub, () => toggle),
    null,
  );
});

test('does NOT forward when an interactive child (Edit link) is clicked', () => {
  const link = { interactive: true }; // an <a>
  assert.equal(
    forwardTarget(link, closestStub, () => toggle),
    null,
  );
});

test('does NOT forward when the row has no toggle (finalized / read-only)', () => {
  const cell = { interactive: false };
  assert.equal(
    forwardTarget(cell, closestStub, () => null),
    null,
  );
});
