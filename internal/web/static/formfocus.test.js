// p10.3 unit test for the form-focus module (node --test). It exercises the PURE
// helpers (firstAutofocus / focusFirstAutofocus) with tiny stub nodes — no jsdom, no
// dependency, no browser. The browser glue in formfocus.js is guarded by a
// `typeof document` check, so importing the module here has no side effects.

const test = require('node:test');
const assert = require('node:assert/strict');

// The module is an ES module; load it via dynamic import from CommonJS test.
async function mod() {
  return import('./formfocus.js');
}

// stubEl builds a minimal element stub: matches('[autofocus]') reflects `auto`,
// querySelector returns the first descendant that matches, and focus() records that
// it was called (so we can assert focus landed on the right node).
function stubEl({ auto = false, children = [] } = {}) {
  const el = {
    auto,
    children,
    focused: false,
    matches(sel) {
      return sel === '[autofocus]' && this.auto === true;
    },
    querySelector(sel) {
      if (sel !== '[autofocus]') return null;
      for (const c of this.children) {
        if (c.matches('[autofocus]')) return c;
      }
      return null;
    },
    focus() {
      this.focused = true;
    },
  };
  return el;
}

test('firstAutofocus returns root itself when it carries autofocus', async () => {
  const { firstAutofocus } = await mod();
  const root = stubEl({ auto: true });
  assert.equal(firstAutofocus(root), root);
});

test('firstAutofocus finds the first autofocus descendant', async () => {
  const { firstAutofocus } = await mod();
  const wanted = stubEl({ auto: true });
  const root = stubEl({
    auto: false,
    children: [stubEl({ auto: false }), wanted, stubEl({ auto: true })],
  });
  assert.equal(firstAutofocus(root), wanted);
});

test('firstAutofocus returns null when nothing has autofocus', async () => {
  const { firstAutofocus } = await mod();
  const root = stubEl({ auto: false, children: [stubEl({ auto: false })] });
  assert.equal(firstAutofocus(root), null);
});

test('firstAutofocus handles a null root', async () => {
  const { firstAutofocus } = await mod();
  assert.equal(firstAutofocus(null), null);
});

test('focusFirstAutofocus focuses the first invalid field and returns it', async () => {
  const { focusFirstAutofocus } = await mod();
  const wanted = stubEl({ auto: true });
  const root = stubEl({ auto: false, children: [stubEl({ auto: false }), wanted] });
  const got = focusFirstAutofocus(root);
  assert.equal(got, wanted);
  assert.equal(wanted.focused, true);
});

test('focusFirstAutofocus is a no-op returning null when nothing matches', async () => {
  const { focusFirstAutofocus } = await mod();
  const root = stubEl({ auto: false, children: [] });
  assert.equal(focusFirstAutofocus(root), null);
});
