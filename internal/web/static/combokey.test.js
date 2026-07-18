// p28.3 unit test for the shared Enter/Tab key-decision helper (trap 2: pure, no
// `document`). It is the SINGLE source of truth for "when the suggestion list is open
// and an item is highlighted, Enter commits+advances (preventDefault) and Tab commits
// (native advance)"; anything else leaves both keys to their default behavior.

const test = require('node:test');
const assert = require('node:assert/strict');

let comboKeyAction;
test.before(async () => {
  ({ comboKeyAction } = await import('./combokey.js'));
});

test('Enter with an open list + a highlighted item commits, advances, and preventDefaults', () => {
  assert.deepEqual(comboKeyAction('Enter', { open: true, hasActive: true }), {
    commit: true,
    preventDefault: true,
    focusNext: true,
  });
});

test('Tab with an open list + a highlighted item commits but lets native Tab advance', () => {
  assert.deepEqual(comboKeyAction('Tab', { open: true, hasActive: true }), {
    commit: true,
    preventDefault: false,
    focusNext: false,
  });
});

test('Enter with a CLOSED list is not special (bubbles to grid save)', () => {
  assert.deepEqual(comboKeyAction('Enter', { open: false, hasActive: false }), {
    commit: false,
    preventDefault: false,
    focusNext: false,
  });
});

test('Tab with a CLOSED list is not special (native advance, no commit)', () => {
  assert.deepEqual(comboKeyAction('Tab', { open: false, hasActive: false }), {
    commit: false,
    preventDefault: false,
    focusNext: false,
  });
});

test('open list but NO highlighted item does not commit for either key', () => {
  assert.deepEqual(comboKeyAction('Enter', { open: true, hasActive: false }), {
    commit: false,
    preventDefault: false,
    focusNext: false,
  });
  assert.deepEqual(comboKeyAction('Tab', { open: true, hasActive: false }), {
    commit: false,
    preventDefault: false,
    focusNext: false,
  });
});

test('other keys are never special', () => {
  for (const k of ['ArrowDown', 'Escape', 'a', ' ']) {
    assert.deepEqual(comboKeyAction(k, { open: true, hasActive: true }), {
      commit: false,
      preventDefault: false,
      focusNext: false,
    });
  }
});

test('missing state is treated as closed (defensive)', () => {
  assert.deepEqual(comboKeyAction('Enter'), {
    commit: false,
    preventDefault: false,
    focusNext: false,
  });
});
