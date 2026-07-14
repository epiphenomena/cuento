// p25.4 expense-report line grid -- DOM GLUE (rule 12: a hand-written external ES
// module, no framework, no inline handler, loaded under script-src 'self'). It mirrors
// the transaction editor's auto-append (txneditor.js): the grid keeps exactly ONE
// trailing empty row -- when the last row stops being empty, a fresh empty row is
// appended; the server drops the trailing empty row on the bulk save (expenseLinesSave).
//
// There is NO balancing, NO DR/CR, NO functional-class here -- a plain account/amount/
// fund/program/memo grid whose whole set saves under one change (replace-set by line id,
// the hidden line_id_<i> round-trip). The pure emptiness predicate (isRowEmpty) is the
// SAME tested one the txn editor uses (txnpayee.js); this module is the thin, e2e-covered
// glue. Guarded so importing under Node is side-effect free (no `document`).

import { isRowEmpty } from './txnpayee.js';

// enhance wires ONE grid form: the auto-append of a trailing empty row on edit.
function enhance(form) {
  const tbody = form.querySelector('#expense-rows');
  const count = form.querySelector('#expense-rows-count');
  if (!tbody || !count) return;

  // rowFieldValues reads the emptiness-relevant fields of a row (account/amount/memo);
  // fund/program defaults alone do NOT make a row non-empty (isRowEmpty's contract).
  function rowFieldValues(rowEl) {
    const i = rowEl.dataset.row;
    const get = (f) => {
      const el = form.querySelector(`#el-${f}-${i}`);
      return el ? el.value : '';
    };
    return { account: get('account'), amount: get('amount'), memo: get('memo') };
  }

  // addRow clones the last row, rewrites its id/name suffixes to the new index, clears
  // inputs (including the hidden line_id so a new row INSERTS, ID 0, rather than updating
  // an existing line), resets selects to index 0, and appends it -- then bumps the count.
  function addRow() {
    const rows = tbody.querySelectorAll('.el-row');
    const template = rows[rows.length - 1];
    if (!template) return;
    const idx = rows.length;
    const clone = template.cloneNode(true);
    clone.dataset.row = String(idx);
    clone.querySelectorAll('[id],[name]').forEach((el) => {
      if (el.id) el.id = el.id.replace(/-\d+$/, `-${idx}`);
      if (el.name) el.name = el.name.replace(/_\d+$/, `_${idx}`);
      if (el.tagName === 'INPUT') el.value = ''; // clears amount/memo AND the hidden line_id
      if (el.tagName === 'SELECT') el.selectedIndex = 0;
    });
    const errCell = clone.querySelector('.el-row-error');
    if (errCell) errCell.textContent = '';
    tbody.appendChild(clone);
    count.value = String(tbody.querySelectorAll('.el-row').length);
  }

  // ensureTrailingEmptyRow grows a fresh empty row when the last row is no longer empty.
  function ensureTrailingEmptyRow() {
    const rows = [...tbody.querySelectorAll('.el-row')];
    const last = rows[rows.length - 1];
    if (last && !isRowEmpty(rowFieldValues(last))) addRow();
  }

  // Delegated on the tbody so it survives addRow without re-wiring each control.
  tbody.addEventListener('input', ensureTrailingEmptyRow);
  tbody.addEventListener('change', ensureTrailingEmptyRow);

  // Guarantee the one-trailing-empty-row invariant on load (and after a 422 re-render
  // where the server dropped empties).
  ensureTrailingEmptyRow();
}

// Browser glue: enhance each grid form once on load and after an htmx swap. Guarded for
// Node (no `document`), same pattern as txneditor.js.
if (typeof document !== 'undefined' && typeof document.addEventListener === 'function') {
  const init = () => {
    document.querySelectorAll('form#expense-grid-form').forEach((f) => {
      if (!f.dataset.wired) {
        f.dataset.wired = '1';
        enhance(f);
      }
    });
  };
  document.addEventListener('DOMContentLoaded', init);
  document.body && document.body.addEventListener('htmx:afterSwap', init);
}

export { enhance };
