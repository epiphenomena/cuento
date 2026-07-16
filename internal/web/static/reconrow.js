// p26.49 REUSABLE whole-row click-to-toggle for the reconciliation workspace -- a
// CSP-safe ES module (rule 12: external, no inline handler, script-src 'self'; NO
// hx-trigger filter expressions, which htmx eval's and the strict CSP would break).
//
// The workspace already carries a NATIVE <button class="recon-toggle"> per row (the
// p16.3 anti-jank toggle: hx-post the toggle action, hx-swap the row outerHTML +
// OOB-swap the sticky summary; Space activates the focused button). This module makes
// the ENTIRE <tr class="recon-row"> a click target that FORWARDS one synthetic click to
// that button -- so clicking anywhere on the row toggles cleared, while the button stays
// the sole focus/keyboard affordance (we add NO tabindex to the row).
//
// The design guarantees EXACTLY-ONCE (no double toggle):
//   - hx-post stays ONLY on the button (never moved to the row), so htmx fires once per
//     button click.
//   - A single DELEGATED listener on #recon-rows (NOT per-row: rows are swapped
//     outerHTML on toggle, so per-row handlers would die) inspects the click. It
//     forwards to the row's toggle ONLY when the click did not land on a genuinely
//     interactive child (a nested <a>/<button> -- e.g. the p26.50 Edit link, or the
//     toggle button itself). A click ON the button is excluded, so htmx's own handling
//     runs and the module does nothing (no second toggle).
//   - The forwarded synthetic click bubbles back to this same listener, but its target
//     is now the button -> excluded by the same rule. No loop, no double fire.
//   - A FINALIZED (read-only) recon renders NO .recon-toggle, so forwardTarget returns
//     null and the row never toggles.
//
// The pure decision (forwardTarget) is unit-tested (reconrow.test.js); the DOM glue is
// e2e-covered (reconcile.spec.js). Guarded so importing under Node is side-effect free.

// forwardTarget decides what a click on a recon row should forward to. Given the CLICKED
// element and a `closest(selector)` lookup + a `queryToggle()` that returns the row's
// toggle button (or null), it returns:
//   - null when the click landed on a genuinely interactive child (a nested <a>/<button>)
//     -- that element does ITS action, we must not also toggle;
//   - null when the row has no toggle (finalized / read-only) -- nothing to forward to;
//   - otherwise the row's toggle button, which the caller synthetically clicks.
// Kept pure (no DOM types) so it is testable in Node with a tiny stub.
export function forwardTarget(clicked, closest, queryToggle) {
  // A click on an interactive descendant (the toggle button, the Edit link, any nested
  // control) is handled by THAT element; do not also toggle the row.
  if (closest(clicked, 'a, button')) return null;
  // Otherwise forward to the row's toggle -- null when finalized (no toggle rendered).
  return queryToggle();
}

// -------------------------- DOM glue (browser only) ------------------------

// enhanceReconRows wires ONE delegated click listener on the #recon-rows tbody. It is
// idempotent (a dataset flag) so a re-init (htmx swap) does not stack listeners -- and
// crucially the listener lives on the STABLE tbody, not the swapped <tr>, so it survives
// every toggle's outerHTML row swap.
function enhanceReconRows(tbody) {
  if (tbody.dataset.reconRowsWired) return;
  tbody.dataset.reconRowsWired = '1';
  tbody.addEventListener('click', (e) => {
    const row = e.target.closest('.recon-row');
    if (!row) return;
    const target = forwardTarget(
      e.target,
      (el, sel) => el.closest(sel),
      () => row.querySelector('.recon-toggle'),
    );
    if (target) target.click();
  });
}

// Browser glue: enhance the workspace rows on load and after an htmx swap. The tbody
// (#recon-rows) is NOT itself swapped by a toggle (only individual <tr> are), so a single
// wiring persists; the afterSwap re-run only matters if a future flow replaces the whole
// table. Guarded for Node (import side-effect free).
if (typeof document !== 'undefined' && typeof document.addEventListener === 'function') {
  const initAll = () => {
    const tbody = document.getElementById('recon-rows');
    if (tbody) enhanceReconRows(tbody);
  };
  document.addEventListener('DOMContentLoaded', initAll);
  if (document.body) {
    document.body.addEventListener('htmx:afterSwap', initAll);
  }
}

export { enhanceReconRows };
