// p10.3 form-focus module. The form-error convention re-renders the form region as
// an htmx partial (422 swap) with `autofocus` stamped on the FIRST invalid field.
// Browsers only honor `autofocus` on parse, NOT on nodes inserted later (which is
// what an htmx swap does), so after a swap the first invalid field would not
// actually receive focus. This module closes that gap: on every htmx swap it moves
// focus to the first [autofocus] element inside the swapped region.
//
// Boring frontend (rule 12): a small hand-written ES module, NO framework, NO inline
// handler (it is an external file loaded under script-src 'self'); the pure helper
// is unit-tested with `node --test` (formfocus.test.js). It attaches htmx glue only
// in a real browser (guarded), so importing it under Node for the test is side
// -effect free.

// firstAutofocus returns the first element carrying the `autofocus` attribute within
// root (inclusive of root itself), or null. Pure and DOM-shaped only through
// `matches`/`querySelector`, so a tiny stub node drives it in `node --test` without
// jsdom (no dependency). Exported for the unit test.
export function firstAutofocus(root) {
  if (!root) return null;
  if (typeof root.matches === 'function' && root.matches('[autofocus]')) {
    return root;
  }
  if (typeof root.querySelector === 'function') {
    return root.querySelector('[autofocus]');
  }
  return null;
}

// focusFirstAutofocus finds the first [autofocus] in root and calls .focus() on it,
// returning the focused element (or null if none). Kept separate from the event glue
// so it is unit-testable with a stub whose focus() just records the call.
export function focusFirstAutofocus(root) {
  const el = firstAutofocus(root);
  if (el && typeof el.focus === 'function') {
    el.focus();
  }
  return el;
}

// Browser glue (thin, untested shim): after htmx swaps a fragment in, move focus to
// the first [autofocus] within the swapped target. Guarded so importing this module
// under Node (for the unit test) does nothing.
if (typeof document !== 'undefined' && typeof document.body !== 'undefined') {
  document.body.addEventListener('htmx:afterSwap', (evt) => {
    focusFirstAutofocus(evt.target);
  });
}
