// p27.2 budget-split entry grid -- DOM GLUE (rule 12: a hand-written external ES module,
// no framework, no inline handler, loaded under script-src 'self'). It mirrors the
// expense-report grid (expensegrid.js): the grid keeps exactly ONE trailing empty row --
// when the last row stops being empty a fresh empty row is appended; the server drops the
// trailing empty on the bulk save.
//
// A budget-split row is [description, date, account, fund, program, amount] (the DATE
// column is new vs the expense grid). The whole set saves as a REPLACE set. It ALSO
// consumes the `budget:cadence` event that budgetcadence.js fires: on Generate, the grid
// clones the CURRENT last-edited row across the generated dates (materialize-and-forget).
//
// The pure emptiness predicate (isRowEmpty) is the SAME tested one the txn/expense grids
// use (rowstate.js). Combos (account/fund/program) and the description autocomplete reuse
// the shared modules; date inputs reuse the global js-datefield enhancer (idempotent, so
// a cloned row is re-enhanced by stripping its data-df-wired flag + re-dispatching the
// global enhance scan). Guarded so importing under Node is side-effect free (no
// `document`).

import { isRowEmpty } from './rowstate.js';
import { formatAmountGrouped } from './txnamount.js';
import { initCombos, stripCombo, resyncCombos } from './combobox.js';
import { initDescField, stripDescField } from './descfield.js';

function enhance(form) {
  const table = form.querySelector('.budget-splits-table');
  const count = form.querySelector('#budget-rows-count');
  if (!table || !count) return;

  const exp = 2; // the server is authoritative on the exponent; 2-dp for display grouping.

  // rowFieldValues reads the emptiness-relevant fields (account/amount/date/description).
  // A row with only a fund/program default is still empty (isRowEmpty's contract); date
  // and description are content-bearing here so a cadence-generated dated row counts.
  function rowFieldValues(rowEl) {
    const i = rowEl.dataset.row;
    const get = (f) => {
      const el = form.querySelector(`#bs-${f}-${i}`);
      return el ? el.value : '';
    };
    return { account: get('account'), amount: get('amount'), memo: get('date') + get('desc') };
  }

  function syncCount() {
    count.value = String(table.querySelectorAll('.bs-row').length);
  }

  // reEnhanceDates strips the copied df-wired flag off a subtree's date inputs and asks
  // the global datefield enhancer to re-scan (it is idempotent, keyed on data-df-wired).
  function reEnhanceDates(root) {
    root.querySelectorAll('input.js-datefield').forEach((el) => {
      delete el.dataset.dfWired;
    });
    // The global datefield module re-scans on htmx:afterSwap; dispatch it so cloned date
    // inputs get keyboard/calendar enhancement. Idempotent for already-wired inputs.
    document.dispatchEvent(new Event('htmx:afterSwap'));
  }

  // reindexRow rewrites one row's id/name/data-row suffixes to `idx`, PRESERVING values.
  function reindexRow(rowEl, idx) {
    rowEl.dataset.row = String(idx);
    rowEl.querySelectorAll('[id],[name]').forEach((el) => {
      if (el.id) el.id = el.id.replace(/-\d+$/, `-${idx}`);
      if (el.name) el.name = el.name.replace(/_\d+$/, `_${idx}`);
    });
    rowEl.querySelectorAll('[data-desc-container]').forEach((el) => {
      el.dataset.descContainer = el.dataset.descContainer.replace(/-\d+$/, `-${idx}`);
    });
  }

  // cloneRow clones a template row into index `idx`, stripping combo/desc overlays and
  // rewriting suffixes. `clear` empties inputs + resets selects (a fresh trailing row);
  // when false the clone KEEPS the template's values (a cadence clone reuses account/
  // fund/program/amount/description and only the date is later overwritten). Returns the
  // appended clone.
  function cloneRow(template, idx, clear) {
    const clone = template.cloneNode(true);
    clone.dataset.row = String(idx);
    stripCombo(clone);
    stripDescField(clone);
    clone.querySelectorAll('[id],[name]').forEach((el) => {
      if (el.id) el.id = el.id.replace(/-\d+$/, `-${idx}`);
      if (el.name) el.name = el.name.replace(/_\d+$/, `_${idx}`);
      if (clear) {
        if (el.tagName === 'INPUT') el.value = '';
        if (el.tagName === 'SELECT') el.selectedIndex = 0;
      }
    });
    clone.querySelectorAll('[data-desc-container]').forEach((el) => {
      el.dataset.descContainer = el.dataset.descContainer.replace(/-\d+$/, `-${idx}`);
    });
    const errCell = clone.querySelector('.bs-row-error');
    if (errCell) errCell.textContent = '';
    table.appendChild(clone);
    initCombos(clone);
    initDescField(clone);
    reEnhanceDates(clone);
    return clone;
  }

  function addRow() {
    const rows = table.querySelectorAll('.bs-row');
    const template = rows[rows.length - 1];
    if (!template) return;
    cloneRow(template, rows.length, true);
    syncCount();
  }

  function resetRow(rowEl) {
    rowEl.querySelectorAll('input').forEach((el) => { el.value = ''; });
    rowEl.querySelectorAll('select').forEach((el) => { el.selectedIndex = 0; });
    const errCell = rowEl.querySelector('.bs-row-error');
    if (errCell) errCell.textContent = '';
    resyncCombos(rowEl);
  }

  function deleteRow(rowEl) {
    const rows = [...table.querySelectorAll('.bs-row')];
    if (rows.length <= 1) {
      resetRow(rowEl);
      syncCount();
      return;
    }
    rowEl.remove();
    table.querySelectorAll('.bs-row').forEach((r, i) => reindexRow(r, i));
    syncCount();
    ensureTrailingEmptyRow();
  }

  function ensureTrailingEmptyRow() {
    const rows = [...table.querySelectorAll('.bs-row')];
    const last = rows[rows.length - 1];
    if (last && !isRowEmpty(rowFieldValues(last))) addRow();
  }

  // lastContentRow returns the last NON-EMPTY row (the "current" row a cadence run clones
  // across dates), or null when the grid holds only the empty scaffold.
  function lastContentRow() {
    const rows = [...table.querySelectorAll('.bs-row')];
    for (let i = rows.length - 1; i >= 0; i -= 1) {
      if (!isRowEmpty(rowFieldValues(rows[i]))) return rows[i];
    }
    return null;
  }

  // setRowDate writes a row's date input (display value) directly.
  function setRowDate(rowEl, value) {
    const i = rowEl.dataset.row;
    const el = form.querySelector(`#bs-date-${i}`);
    if (el) el.value = value;
  }

  // applyCadence clones the current content row across `dates`: the FIRST date lands on
  // the current row itself; each subsequent date gets a fresh clone (same account/fund/
  // program/amount/description). Materialize-and-forget -- no stored schedule.
  function applyCadence(dates) {
    if (!dates || dates.length === 0) return;
    let base = lastContentRow();
    if (!base) {
      // No content row yet: use the first (empty) row as the base for the first date.
      base = table.querySelector('.bs-row');
      if (!base) return;
    }
    setRowDate(base, dates[0]);
    for (let i = 1; i < dates.length; i += 1) {
      const idx = table.querySelectorAll('.bs-row').length;
      const clone = cloneRow(base, idx, false);
      // The clone carries the base's line_id via cloneNode? There is no line_id here (a
      // replace-set grid), so nothing to reset beyond the date, which we overwrite:
      setRowDate(clone, dates[i]);
      resyncCombos(clone);
    }
    syncCount();
    ensureTrailingEmptyRow();
  }

  // Consume the cadence event (bubbles from the [data-cadence] control up to document).
  document.addEventListener('budget:cadence', (evt) => {
    if (evt && evt.detail && Array.isArray(evt.detail.dates)) applyCadence(evt.detail.dates);
  });

  table.addEventListener('input', ensureTrailingEmptyRow);
  table.addEventListener('change', ensureTrailingEmptyRow);

  table.addEventListener('focusout', (evt) => {
    const el = evt.target;
    if (el && el.classList && el.classList.contains('bs-amount')) {
      el.value = formatAmountGrouped(el.value, exp);
    }
  });

  table.addEventListener('click', (evt) => {
    const btn = evt.target.closest('.bs-delete');
    if (!btn) return;
    evt.preventDefault();
    const rowEl = btn.closest('.bs-row');
    if (rowEl) deleteRow(rowEl);
  });

  initCombos(form);
  initDescField(form);
  ensureTrailingEmptyRow();
}

if (typeof document !== 'undefined' && typeof document.addEventListener === 'function') {
  const init = () => {
    document.querySelectorAll('form#budget-grid-form').forEach((f) => {
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
