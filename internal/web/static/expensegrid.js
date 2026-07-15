// p25.4 / p26.4 expense-report line grid -- DOM GLUE (rule 12: a hand-written external ES
// module, no framework, no inline handler, loaded under script-src 'self'). It mirrors the
// transaction editor's auto-append (txneditor.js): the grid keeps exactly ONE trailing
// empty row -- when the last row stops being empty, a fresh empty row is appended; the
// server drops the trailing empty row on the bulk save (expenseLinesSave).
//
// There is NO balancing, NO DR/CR, NO functional-class here -- a plain account/amount/
// fund/program/memo grid whose whole set saves under one change (replace-set by line id,
// the hidden line_id_<i> round-trip). The pure emptiness predicate (isRowEmpty) is the
// SAME tested one the txn editor uses (rowstate.js); this module is the thin, e2e-covered
// glue. Guarded so importing under Node is side-effect free (no `document`).
//
// p26.4 adds:
//   - comboboxes on account/fund/program (the SAME widget as the txn grid) -- initCombos on
//     init + after an htmx swap; stripCombo(clone)+initCombos(clone) across addRow; a
//     resyncCombos after any programmatic select.value= (the sole-row reset).
//   - amount right-align (CSS) + reformat-on-blur (formatAmountGrouped, delegated on tbody).
//   - a per-row × delete button that RE-INDEXES the surviving rows so the name="_i" scheme
//     stays contiguous 0..n-1 (server handler unchanged); deleting the only/last row resets
//     it in place so the grid never drops below one trailing empty row.

import { isRowEmpty } from './rowstate.js';
import { formatAmountGrouped } from './txnamount.js';
import { initCombos, stripCombo, resyncCombos } from './combobox.js';
import { initDescField, stripDescField } from './descfield.js';

// enhance wires ONE grid form: combos, auto-append of a trailing empty row on edit,
// amount blur-reformat, and per-row delete + re-index.
function enhance(form) {
  const tbody = form.querySelector('#expense-rows');
  const count = form.querySelector('#expense-rows-count');
  if (!tbody || !count) return;

  const exp = 2; // expense amounts are 2-dp (like the txn grid); the server is authoritative.

  // userProgram is the submitter's default_program (p26.5); '' / '0' = unset. Unlike the
  // txn editor there is NO per-account default tier here, so it is the program prefill for
  // every NEW/empty row. applyUserProgram sets a row's program select to it (when set and
  // the row's program is still unset), then resyncs the combo overlay so the visible label
  // matches. Never overrides a server-rendered existing-line program.
  const userProgram = form.dataset.userProgram || '';
  function applyUserProgram(rowEl) {
    if (!userProgram || userProgram === '0') return;
    const sel = rowEl.querySelector('.el-program');
    if (!sel) return;
    if (!sel.value || sel.value === '0') {
      sel.value = userProgram;
      resyncCombos(rowEl);
    }
  }

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

  // reindexRow rewrites one row's id/name/data-row suffixes to `idx`, PRESERVING every
  // value (unlike addRow's clone, which clears). Combos are closure-bound (not id-bound),
  // so an enhanced row survives a re-index without re-enhancement.
  function reindexRow(rowEl, idx) {
    rowEl.dataset.row = String(idx);
    rowEl.querySelectorAll('[id],[name]').forEach((el) => {
      if (el.id) el.id = el.id.replace(/-\d+$/, `-${idx}`);
      if (el.name) el.name = el.name.replace(/_\d+$/, `_${idx}`);
    });
    // p26.19: keep the description input's data-desc-container (el-desc-list-<i>) pointing
    // at its OWN (re-indexed) listbox. The combos are closure-bound and survive a
    // re-index, but the descfield's container lookup is id-based, so re-index it too.
    rowEl.querySelectorAll('[data-desc-container]').forEach((el) => {
      el.dataset.descContainer = el.dataset.descContainer.replace(/-\d+$/, `-${idx}`);
    });
  }

  // syncCount rewrites #expense-rows-count from the live row set.
  function syncCount() {
    count.value = String(tbody.querySelectorAll('.el-row').length);
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
    // Combobox clone contract (p26.2): the template's account/fund/program cells carry
    // enhanced combos whose overlay listeners cloneNode does NOT copy (dead wrappers).
    // Strip them back to clean native <select>s BEFORE the id/name rewrite so the overlay's
    // own nodes aren't re-indexed; initCombos(clone) below re-enhances the fresh selects.
    stripCombo(clone);
    // p26.19: same clone contract for the description field (its listeners were not copied
    // by cloneNode) -- strip the marker + empty the cloned listbox BEFORE the id rewrite.
    stripDescField(clone);
    clone.querySelectorAll('[id],[name]').forEach((el) => {
      if (el.id) el.id = el.id.replace(/-\d+$/, `-${idx}`);
      if (el.name) el.name = el.name.replace(/_\d+$/, `_${idx}`);
      if (el.tagName === 'INPUT') el.value = ''; // clears amount/memo/description AND the hidden line_id
      if (el.tagName === 'SELECT') el.selectedIndex = 0;
    });
    // Re-index the description input's listbox pointer alongside the id/name suffixes.
    clone.querySelectorAll('[data-desc-container]').forEach((el) => {
      el.dataset.descContainer = el.dataset.descContainer.replace(/-\d+$/, `-${idx}`);
    });
    const errCell = clone.querySelector('.el-row-error');
    if (errCell) errCell.textContent = '';
    tbody.appendChild(clone);
    syncCount();
    initCombos(clone); // enhance the clone's clean selects
    initDescField(clone); // p26.19: re-wire the clone's clean description input
    applyUserProgram(clone); // p26.5: prefill the user's default program on the fresh row
  }

  // resetRow clears one row in place (values, selects, hidden line_id, error), then
  // resyncs its combos' overlays. Used for the sole/last delete so the grid never drops to
  // zero rows (a clone-based addRow can't run at zero rows, and an emptied row saves as
  // "no line" -- the server deletes the underlying line, which is the delete's intent).
  function resetRow(rowEl) {
    rowEl.querySelectorAll('input').forEach((el) => {
      el.value = '';
    });
    rowEl.querySelectorAll('select').forEach((el) => {
      el.selectedIndex = 0;
    });
    const errCell = rowEl.querySelector('.el-row-error');
    if (errCell) errCell.textContent = '';
    resyncCombos(rowEl); // the selects were set directly -> refresh their overlay text
    applyUserProgram(rowEl); // p26.5: re-seed the user's default program on the cleared row
  }

  // deleteRow removes the row (or resets it in place when it is the only row), then
  // re-indexes the survivors to a contiguous 0..n-1 and re-asserts the trailing-empty
  // invariant. The server handler is unchanged (it always sees a contiguous _i scheme).
  function deleteRow(rowEl) {
    const rows = [...tbody.querySelectorAll('.el-row')];
    if (rows.length <= 1) {
      resetRow(rowEl);
      syncCount();
      return;
    }
    rowEl.remove();
    tbody.querySelectorAll('.el-row').forEach((r, i) => reindexRow(r, i));
    syncCount();
    ensureTrailingEmptyRow();
  }

  // ensureTrailingEmptyRow grows a fresh empty row when the last row is no longer empty.
  function ensureTrailingEmptyRow() {
    const rows = [...tbody.querySelectorAll('.el-row')];
    const last = rows[rows.length - 1];
    if (last && !isRowEmpty(rowFieldValues(last))) addRow();
  }

  // Delegated on the tbody so they survive addRow/re-index without re-wiring each control.
  tbody.addEventListener('input', ensureTrailingEmptyRow);
  tbody.addEventListener('change', ensureTrailingEmptyRow);

  // Amount blur-reformat: reformat the just-left amount input (1000 -> 1,000.00). Delegated
  // (focusout bubbles; blur does not) so cloned rows are covered.
  tbody.addEventListener('focusout', (evt) => {
    const el = evt.target;
    if (el && el.classList && el.classList.contains('el-amount')) {
      el.value = formatAmountGrouped(el.value, exp);
    }
  });

  // Per-row delete (the × button). type="button" so it never submits; delegated so it
  // survives addRow/re-index.
  tbody.addEventListener('click', (evt) => {
    const btn = evt.target.closest('.el-delete');
    if (!btn) return;
    evt.preventDefault();
    const rowEl = btn.closest('.el-row');
    if (rowEl) deleteRow(rowEl);
  });

  // Client guard (p26.10): a content-bearing row (amount or memo) with NO account must
  // not post silently -- the server rejects it (account_not_offered), but flagging it
  // client-side gives immediate per-row feedback. Only the trailing EMPTY row may lack
  // an account. The form is a plain (non-htmx) POST, so a native preventDefault blocks
  // it. Uses the SAME emptiness predicate (isRowEmpty) as the auto-append.
  function flagAccountlessRows() {
    const msg = form.dataset.accountMissingMsg || '';
    let flagged = false;
    [...tbody.querySelectorAll('.el-row')].forEach((rowEl) => {
      const i = rowEl.dataset.row;
      const acctSel = form.querySelector(`#el-account-${i}`);
      const errCell = rowEl.querySelector('.el-row-error');
      const acct = acctSel ? acctSel.value : '';
      const empty = isRowEmpty(rowFieldValues(rowEl));
      const accountless = acct === '' || acct === '0';
      if (!empty && accountless) {
        if (errCell) {
          errCell.textContent = '';
          const span = document.createElement('span');
          span.className = 'field-error';
          span.setAttribute('role', 'alert');
          span.textContent = msg;
          errCell.appendChild(span);
        }
        flagged = true;
      } else if (errCell && errCell.querySelector('.field-error')) {
        errCell.textContent = ''; // clear a stale client-set error
      }
    });
    return flagged;
  }
  form.addEventListener('submit', (evt) => {
    if (flagAccountlessRows()) evt.preventDefault();
  });

  // Enhance every combo select present on initial render / after a whole-form htmx swap
  // (subsidiary re-scope, 422 re-render). Idempotent.
  initCombos(form);
  // p26.19: wire the per-line description autocomplete + prefill on every row (idempotent).
  initDescField(form);

  // p26.5: seed the user's default program on any EMPTY row present on load (the server's
  // starter empty row) so a fresh grid opens with the preferred program. Non-empty existing
  // lines keep their stored program (applyUserProgram only fills an unset program select).
  tbody.querySelectorAll('.el-row').forEach((rowEl) => {
    if (isRowEmpty(rowFieldValues(rowEl))) applyUserProgram(rowEl);
  });

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
