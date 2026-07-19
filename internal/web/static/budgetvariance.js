// p30.9 BUDGET-VARIANCE measure toggle -- a CSP-safe ES module (rule 12: external, no
// inline handler, script-src 'self'). The budget-variance report renders each grid cell
// with all THREE measures pre-formatted server-side into three spans
// (.bv-budgeted / .bv-actual / .bv-variance); this module lets the user switch which one
// shows INSTANTLY, client-side, with NO server round trip and NO money math (rule 10 --
// the values are already formatted; JS only shows/hides).
//
// Mechanism: a single table-level attribute `data-measure` on the <table.bv-table>
// selects which spans are visible (CSS in app.css shows the matching .bv-<measure> spans
// and hides the others). A button group (.bv-measure-toggle > .bv-measure-btn[data-measure])
// flips that attribute on click. Because only the ACTUAL span holds the drill link, the
// drill is live exactly when Actual is shown -- no link rewrapping.
//
// No-JS fallback: the server stamps data-measure="variance" and marks the Variance button
// aria-pressed; CSS shows the variance spans, and the plain buttons (no form, no href) do
// nothing. The report is fully usable without this module.
//
// The pure decision (applyMeasure) is unit-tested with `node --test`; the DOM wiring is
// e2e-covered (a click switches the displayed measure with no network request).

// The three valid measures, in button order. Exported so the test pins the set.
export const MEASURES = ['budgeted', 'actual', 'variance'];

// applyMeasure sets the chosen measure on the table and syncs the button group's pressed
// state -- the PURE state transition the DOM click handler delegates to. It:
//   - writes `table.dataset.measure = measure` (CSS then shows that measure's spans),
//   - sets aria-pressed="true" on the matching button and "false" on the others.
// An unknown measure is ignored (the table/buttons are left untouched) so a stray value
// can never blank the grid. Works on real DOM nodes and on the test's tiny stubs alike
// (it only touches .dataset and .setAttribute), so the logic is testable without a browser.
export function applyMeasure(table, buttons, measure) {
  if (!MEASURES.includes(measure)) return false;
  table.dataset.measure = measure;
  for (const btn of buttons) {
    btn.setAttribute('aria-pressed', btn.dataset.measure === measure ? 'true' : 'false');
  }
  return true;
}

// measureFromButton returns a button's target measure (its data-measure), or null if the
// element carries none -- the pure lookup the click handler uses to decide what to apply.
export function measureFromButton(btn) {
  const m = btn && btn.dataset ? btn.dataset.measure : null;
  return MEASURES.includes(m) ? m : null;
}

// --- DOM glue (guarded for Node so the pure exports import cleanly under `node --test`) ---

// wireToggle connects one table to its button group: each button click applies its
// measure. The initial state comes from the server-stamped data-measure (default
// "variance"); we re-assert it through applyMeasure so the button pressed-state is
// consistent even if the markup drifts.
function wireToggle(table, group) {
  const buttons = Array.from(group.querySelectorAll('.bv-measure-btn'));
  applyMeasure(table, buttons, table.dataset.measure || 'variance');
  for (const btn of buttons) {
    btn.addEventListener('click', () => {
      const m = measureFromButton(btn);
      if (m) applyMeasure(table, buttons, m);
    });
  }
}

// findToggle locates the .bv-measure-toggle group that belongs to a table. The toggle and
// the table are siblings inside #report-results (the toggle sits just above the table), so
// prefer the group within the same results wrapper, then fall back document-wide.
function findToggle(table) {
  const scoped = table.closest('#report-results');
  if (scoped) {
    const near = scoped.querySelector('.bv-measure-toggle');
    if (near) return near;
  }
  return document.querySelector('.bv-measure-toggle');
}

if (typeof document !== 'undefined' && typeof document.addEventListener === 'function') {
  const initAll = () => {
    document.querySelectorAll('table.bv-table').forEach((table) => {
      if (table.dataset.bvWired) return;
      const group = findToggle(table);
      if (!group) return;
      table.dataset.bvWired = '1';
      wireToggle(table, group);
    });
  };
  document.addEventListener('DOMContentLoaded', initAll);
  if (document.body) {
    document.body.addEventListener('htmx:afterSwap', initAll);
  }
}
