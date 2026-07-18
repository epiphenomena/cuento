// p28.2 shell-wide combobox enhancer. The entry-grid editors (txneditor / expensegrid /
// budgetgrid) enhance their OWN combos on load + per row-clone, but the OTHER account
// pickers -- merge (mg-src/mg-dst), the account-ledger report filter (rp-account), the
// account-form parent (af-parent), and the import target (import-account) -- reach the DOM
// via ordinary page renders and htmx swaps with no owning editor module. This tiny module
// (mirroring datefield.js) enhances every NON-GRID `select.combo-input` shell-wide, so those
// pickers become the SAME fuzzy + hierarchy combobox the grids use.
//
// enhance() is idempotent (the data-combo guard) and this pass also skips the payee's manual
// combo ([data-combo-manual]) and any combo inside a grid form (the editor owns those). So it
// is safe alongside the editors' own enhancement -- a select is enhanced at most once, and a
// row clone (which is NOT an htmx swap) still relies on its editor's re-enhance.
//
// Boring frontend (rule 12): a hand-written external ES module, no framework, no inline
// handler, loaded under script-src 'self'. Guarded so importing under Node is side-effect
// free (like datefield.js / combobox.js); the ranking logic it drives is node-tested in
// combofilter.test.js.

import { enhance } from './combobox.js';

// The three entry-grid editors (txneditor / expensegrid / budgetgrid) enhance their OWN
// combos with an onAdvance hook (p28.3: an Enter-pick advances to the next grid cell). If
// this shell-wide pass enhanced those first (enhance is first-wins/idempotent), the grid
// combos would lose that hook. So skip any combo inside a grid form; the editors own them.
const GRID_FORMS = ['#txn-form', '#expense-grid-form', '#budget-grid-form'];

function enhanceNonGridCombos() {
  if (typeof document.querySelectorAll !== 'function') return;
  document
    .querySelectorAll('select.combo-input:not([data-combo]):not([data-combo-manual])')
    .forEach((sel) => {
      if (GRID_FORMS.some((f) => sel.closest(f))) return; // grid-owned; its editor enhances it
      enhance(sel); // no onAdvance: a non-grid picker has no "next grid cell" to jump to
    });
}

if (typeof document !== 'undefined' && typeof document.addEventListener === 'function') {
  document.addEventListener('DOMContentLoaded', enhanceNonGridCombos);
  // Re-scan after every htmx swap (the params partial, the merge-form swap, the
  // account-form type re-render, etc.) so a freshly-swapped combo select is enhanced.
  document.addEventListener('htmx:afterSwap', enhanceNonGridCombos);
}
