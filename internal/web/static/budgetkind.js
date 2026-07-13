// p19.3 schedule-form KIND PICKER -- a tiny CSP-safe ES module (rule 12: external,
// no inline handler, script-src 'self') that SHOWS the kind-specific field block(s)
// for the chosen schedule kind and HIDES the rest, with NO server round-trip. Doing
// the reveal client-side (rather than an hx-get re-fetch on the kind select) avoids
// the htmx settle-race on the kind select entirely (DECISIONS p19.3): the form is
// fully rendered once and only visibility toggles.
//
// The form renders every kind's fields, each wrapped in a `.kind-block` tagged
// `data-kind-field="<kinds>"` (space-separated). The weekend-policy block is tagged
// `data-kind-weekend="<kinds>"` (shown only for the day-of-month kinds). This module
// reads the selected kind from `.schedule-kind-select` and toggles `hidden` on each
// block accordingly. The SERVER remains the sole validator of field consistency
// (ErrScheduleInvalid); this is display only.
//
// This module is loaded by the LIBRARY PAGE (schedules.tmpl), NOT inside the
// swapped-in schedule-form partial: a <script> injected via htmx innerHTML does not
// execute, so it must live on the page that stays put. It then self-inits on
// DOMContentLoaded and re-runs on htmx:afterSwap when the form is swapped in.
//
// Guarded so importing under Node is side-effect free (no `document`).

// applyKind is the PURE decision (unit-testable): given the chosen kind and a block's
// space-separated kind list, should the block be visible?
export function blockVisible(selectedKind, kindList) {
  if (!kindList) return false;
  return kindList.split(/\s+/).filter(Boolean).includes(selectedKind);
}

function initScheduleForm(form) {
  const kindSel = form.querySelector('.schedule-kind-select');
  if (!kindSel) return;

  function apply() {
    const kind = kindSel.value;
    form.querySelectorAll('[data-kind-field]').forEach((el) => {
      el.hidden = !blockVisible(kind, el.getAttribute('data-kind-field'));
    });
    form.querySelectorAll('[data-kind-weekend]').forEach((el) => {
      el.hidden = !blockVisible(kind, el.getAttribute('data-kind-weekend'));
    });
  }

  kindSel.addEventListener('change', apply);
  apply(); // initial state (edit prefill or the default kind)
}

// Browser glue: initialize each schedule form on load and after an htmx swap (the
// library page swaps the form into #schedule-form; a 422 re-render swaps it too).
// Guarded for Node.
if (typeof document !== 'undefined' && typeof document.addEventListener === 'function') {
  const initAll = () => {
    document.querySelectorAll('form#schedule-form').forEach((f) => {
      if (!f.dataset.kindWired) {
        f.dataset.kindWired = '1';
        initScheduleForm(f);
      }
    });
  };
  document.addEventListener('DOMContentLoaded', initAll);
  if (document.body) {
    document.body.addEventListener('htmx:afterSwap', initAll);
  }
}

export { initScheduleForm };
