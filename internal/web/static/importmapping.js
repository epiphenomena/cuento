// p26.61 bank-import AMOUNT-MODE field toggle -- a tiny CSP-safe ES module (rule 12:
// external, no inline handler, script-src 'self') that SHOWS only the amount fields
// relevant to the chosen amount mode and HIDES the rest, with NO server round-trip.
//
// The mapping form renders every amount field, each wrapped in a `.field` tagged
// `data-amount-field="<modes>"` (space-separated list of the amount modes it belongs
// to): the single signed-amount column is `data-amount-field="single"`, the debit and
// credit columns are `data-amount-field="debit_credit"`. This module reads the chosen
// mode from `.import-amount-mode` and toggles `hidden` on each block accordingly.
//
// The SERVER stays the sole source of truth: bankimport.parseAmount switches on the
// mode and reads only the relevant columns (the others arrive as their defaults and
// are ignored), so the hide is not merely cosmetic -- a NO-JS client sees both sets,
// submits both, and the server uses only the ones matching the selected mode.
//
// Delegated on `document` (not bound to a single form node) and re-applied on
// DOMContentLoaded and htmx:afterSwap so it keeps working when the mapping form is
// swapped into a workspace region (p26.35's lesson: an injected <script> in an
// innerHTML swap does not execute, so the glue must live on the page that stays put
// and re-run after each swap).
//
// Guarded so importing under Node is side-effect free (no `document`).

// fieldVisible is the PURE decision (unit-testable): given the chosen amount mode and
// a block's space-separated mode list, should the block be visible?
export function fieldVisible(selectedMode, modeList) {
  if (!modeList) return false;
  return modeList.split(/\s+/).filter(Boolean).includes(selectedMode);
}

// applyAmountMode toggles every `[data-amount-field]` block under root to match the
// current value of root's amount-mode select. A root without a mode select is a no-op.
function applyAmountMode(root) {
  const modeSel = root.querySelector('.import-amount-mode');
  if (!modeSel) return;
  const mode = modeSel.value;
  root.querySelectorAll('[data-amount-field]').forEach((el) => {
    el.hidden = !fieldVisible(mode, el.getAttribute('data-amount-field'));
  });
}

// Browser glue: wire each mapping form's mode select once, apply the initial state,
// and re-apply after an htmx swap (the workspace/preview swaps re-render the form).
// Guarded for Node.
if (typeof document !== 'undefined' && typeof document.addEventListener === 'function') {
  const wire = (form) => {
    const modeSel = form.querySelector('.import-amount-mode');
    if (!modeSel) return;
    if (!form.dataset.amountModeWired) {
      form.dataset.amountModeWired = '1';
      modeSel.addEventListener('change', () => applyAmountMode(form));
    }
    applyAmountMode(form); // initial state (default or a reloaded profile's mode)
  };
  const initAll = () => {
    document.querySelectorAll('form.import-upload-form').forEach(wire);
  };
  document.addEventListener('DOMContentLoaded', initAll);
  if (document.body) {
    document.body.addEventListener('htmx:afterSwap', initAll);
  }
  // The DOM may already be parsed when this module first evaluates (e.g. injected by
  // a later boost); run once immediately, idempotent via the wired flag.
  if (document.readyState !== 'loading') {
    initAll();
  }
}

export { applyAmountMode };
