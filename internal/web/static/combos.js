// p28.2 shell-wide combobox enhancer. The entry-grid editors (txneditor / expensegrid /
// budgetgrid) call initCombos themselves on load + per row-clone, but the OTHER account
// pickers -- merge (mg-src/mg-dst), the account-ledger report filter (rp-account), the
// account-form parent (af-parent), and the import target (import-account) -- reach the DOM
// via ordinary page renders and htmx swaps with no owning editor module. This tiny module
// (mirroring datefield.js) enhances EVERY `select.combo-input` shell-wide, so those pickers
// become the SAME fuzzy + hierarchy combobox the grids use.
//
// initCombos is idempotent (the :not([data-combo]) guard) and skips the payee's manual
// combo ([data-combo-manual]), so running it here in addition to the grid editors' own
// initCombos(form)/initCombos(clone) calls is safe -- a select is enhanced at most once, and
// a row clone (which is NOT an htmx swap) still relies on the editor's own re-enhance.
//
// Boring frontend (rule 12): a hand-written external ES module, no framework, no inline
// handler, loaded under script-src 'self'. Guarded so importing under Node is side-effect
// free (like datefield.js / combobox.js); the ranking logic it drives is node-tested in
// combofilter.test.js.

import { initCombos } from './combobox.js';

if (typeof document !== 'undefined' && typeof document.addEventListener === 'function') {
  document.addEventListener('DOMContentLoaded', () => initCombos(document));
  // Re-scan after every htmx swap (the params partial, the merge-form swap, the
  // account-form type re-render, etc.) so a freshly-swapped combo select is enhanced.
  document.addEventListener('htmx:afterSwap', () => initCombos(document));
}
