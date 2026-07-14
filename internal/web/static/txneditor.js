// p12.2 transaction editor -- DOM GLUE (trap 2: this is the thin, e2e-covered shim,
// NOT unit-tested; the pure logic it drives lives in txnamount.js / txnfund.js /
// txngrid.js and IS node-tested). Boring frontend (rule 12): a hand-written ES
// module, no framework, external (script-src 'self'), no inline handler.
//
// Responsibilities (all DISPLAY/UX; the server is the sole validator, trap 5):
//   - DR/CR twin columns -> normalize into the hidden signed amount_i field (trap 3,
//     the ONE mapping site is drcrToSigned in txnamount.js).
//   - live imbalance chips (overall + per fund) from fundImbalances (display only).
//   - fund apply-to-all (empty rows only) via applyFundToAll.
//   - show the program select only on R/E rows, the class select only on expense
//     rows, prefilled from the account's data-* defaults (server re-defaults).
//   - subsidiary re-filter: flag rows whose account left the sub (invalidRowsForSub).
//   - select-on-focus, add-row, keyboard grid. (The date input's shortcuts +
//     calendar popover are the shell-wide datefield.js, p23.4.)
//
// Guarded so importing under Node is side-effect free (no `document`).

import { parseAmountMinor, drcrToSigned, formatSignedMinor } from './txnamount.js';
import { fundImbalances, applyFundToAll } from './txnfund.js';
import { nextCell, invalidRowsForSubsidiary } from './txngrid.js';
import { allRowsEmpty, isRowEmpty } from './txnpayee.js';

function initEditor(form) {
  const exp = 2; // currency exponent; USD/MXN are 2. Amounts are display-only here.
  const drcr = form.dataset.drcr === '1';

  // --- amount normalization (trap 3) --------------------------------------
  function syncRowAmount(row) {
    if (!drcr) return;
    const i = row.dataset.row;
    const dr = form.querySelector(`#txn-dr-${i}`);
    const cr = form.querySelector(`#txn-cr-${i}`);
    const hidden = form.querySelector(`#txn-amount-${i}`);
    if (!dr || !cr || !hidden) return;
    const signed = drcrToSigned(dr.value, cr.value, exp);
    hidden.value = signed === null ? '' : formatSignedMinor(signed, exp);
    // Entering one clears the other (Appendix C).
    if (dr.value.trim() !== '') cr.value = '';
    else if (cr.value.trim() !== '') dr.value = '';
  }

  // --- live imbalance chips (display only) --------------------------------
  function rowAmount(i) {
    if (drcr) {
      const dr = form.querySelector(`#txn-dr-${i}`);
      const cr = form.querySelector(`#txn-cr-${i}`);
      return drcrToSigned(dr ? dr.value : '', cr ? cr.value : '', exp);
    }
    const a = form.querySelector(`#txn-amount-${i}`);
    return a ? parseAmountMinor(a.value, exp) : null;
  }

  function recompute() {
    const rows = [...form.querySelectorAll('.txn-row')].map((row) => {
      const i = row.dataset.row;
      const fundSel = form.querySelector(`#txn-fund-${i}`);
      return { fund: fundSel ? fundSel.value.replace(/^0$/, '') : '', amount: rowAmount(i) };
    });
    const { total, perFund } = fundImbalances(rows);
    const overall = form.querySelector('#txn-total-overall');
    if (overall) {
      overall.textContent = total === 0 ? '' : fmtChip('total', total);
      overall.classList.toggle('imbalanced', total !== 0);
    }
    const chips = form.querySelector('#txn-fund-chips');
    if (chips) {
      chips.textContent = '';
      Object.keys(perFund).forEach((k) => {
        const span = document.createElement('span');
        span.className = 'txn-fund-chip imbalanced';
        span.textContent = fmtChip(k || 'unrestricted', perFund[k]);
        chips.appendChild(span);
      });
    }
  }

  function fmtChip(label, minor) {
    return `${label}: ${formatSignedMinor(minor, exp)}`;
  }

  // --- program / class gating per account ---------------------------------
  // rowReveal is the SINGLE source of truth for which conditional cells a row shows,
  // derived from the chosen account's type. gateRow uses it to toggle visibility;
  // the keyboard grid's isVisible() (below) uses it to skip hidden cells. `i` is the
  // row's dataset.row index.
  function rowReveal(i) {
    const acctSel = form.querySelector(`#txn-account-${i}`);
    const opt = acctSel ? acctSel.selectedOptions[0] : null;
    const type = opt ? opt.dataset.type : '';
    const isRE = type === 'revenue' || type === 'expense';
    const isExpense = type === 'expense';
    return { isRE, isExpense, program: isRE, class: isExpense };
  }

  function gateRow(row) {
    const i = row.dataset.row;
    const acctSel = form.querySelector(`#txn-account-${i}`);
    const progCell = row.querySelector('.txn-program-cell');
    const classCell = row.querySelector('.txn-class-cell');
    const progSel = form.querySelector(`#txn-program-${i}`);
    const classSel = form.querySelector(`#txn-class-${i}`);
    if (!acctSel) return;
    const opt = acctSel.selectedOptions[0];
    const { isRE, isExpense } = rowReveal(i);

    if (progCell) progCell.style.visibility = isRE ? 'visible' : 'hidden';
    if (classCell) classCell.style.visibility = isExpense ? 'visible' : 'hidden';

    // Prefill defaults from the account data-* (server re-defaults authoritatively).
    if (isRE && progSel && opt) {
      const def = opt.dataset.defaultProgram;
      if ((!progSel.value || progSel.value === '0') && def && def !== '0') progSel.value = def;
      else if (!progSel.value || progSel.value === '0') progSel.value = form.dataset.rootProgram || '';
    }
    if (isExpense && classSel && opt) {
      const def = opt.dataset.defaultClass;
      if (!classSel.value && def) classSel.value = def;
    }
    if (!isExpense && classSel) classSel.value = '';
    if (!isRE && progSel) progSel.value = '';
  }

  function gateAll() {
    form.querySelectorAll('.txn-row').forEach(gateRow);
  }

  // --- fund apply-to-all (empty rows only) --------------------------------
  const applyBtn = form.querySelector('#txn-apply-fund-btn');
  if (applyBtn) {
    applyBtn.addEventListener('click', () => {
      const sel = form.querySelector('#txn-apply-fund');
      const value = sel ? sel.value : '';
      const selects = [...form.querySelectorAll('.txn-fund')];
      const current = selects.map((s) => (s.value === '0' ? '' : s.value));
      const next = applyFundToAll(current, value);
      selects.forEach((s, idx) => {
        s.value = next[idx] === '' ? '0' : next[idx];
      });
      recompute();
    });
  }

  // --- subsidiary re-filter (client display; server also re-filters) ------
  const subSel = form.querySelector('#txn-subsidiary');
  function markSubsidiaryConflicts() {
    if (!subSel) return;
    const sub = subSel.value;
    const accountSubs = {};
    form.querySelectorAll('#txn-account-0 option[data-account-option]').forEach(() => {});
    // Build account->subs from ANY row's option list (all rows share the option set).
    const first = form.querySelector('.txn-account');
    if (first) {
      first.querySelectorAll('option[data-account-option]').forEach((o) => {
        accountSubs[o.value] = (o.dataset.subsidiaries || '').split(',').filter(Boolean);
      });
    }
    const rows = [...form.querySelectorAll('.txn-row')].map((row) => {
      const acctSel = row.querySelector('.txn-account');
      return { account: acctSel ? acctSel.value.replace(/^0$/, '') : '' };
    });
    const bad = new Set(invalidRowsForSubsidiary(rows, sub, accountSubs));
    [...form.querySelectorAll('.txn-row')].forEach((row, idx) => {
      row.classList.toggle('sub-conflict', bad.has(idx));
    });
  }

  // --- add row (auto, p25.2) ----------------------------------------------
  // No "Add row" button: the grid keeps exactly one trailing empty row. When the last
  // row stops being empty (isRowEmpty, the tested predicate), append a fresh one; the
  // server drops the trailing empty row on submit. addRow is still the primitive the
  // keyboard Enter-on-last-cell and this auto-append both call.
  function rowFieldValues(rowEl) {
    const i = rowEl.dataset.row;
    const get = (f) => {
      const el = form.querySelector(`#txn-${f}-${i}`);
      return el ? el.value : '';
    };
    return { account: get('account'), amount: get('amount'), dr: get('dr'), cr: get('cr'), memo: get('memo') };
  }
  function ensureTrailingEmptyRow() {
    const rowEls = [...form.querySelectorAll('.txn-row')];
    const last = rowEls[rowEls.length - 1];
    if (last && !isRowEmpty(rowFieldValues(last))) addRow();
  }
  function addRow() {
    const tbody = form.querySelector('#txn-rows');
    const rows = form.querySelectorAll('.txn-row');
    const template = rows[rows.length - 1];
    if (!template) return;
    const idx = rows.length;
    const clone = template.cloneNode(true);
    clone.dataset.row = String(idx);
    clone.classList.remove('sub-conflict');
    clone.removeAttribute('data-row-error');
    // Rewrite every id/name suffix to the new index; clear values.
    clone.querySelectorAll('[id],[name]').forEach((el) => {
      if (el.id) el.id = el.id.replace(/-\d+$/, `-${idx}`);
      if (el.name) el.name = el.name.replace(/_\d+$/, `_${idx}`);
      if (el.tagName === 'INPUT') el.value = el.type === 'hidden' && el.name.startsWith('split_id') ? '' : '';
      if (el.tagName === 'SELECT') el.selectedIndex = 0;
    });
    const errCell = clone.querySelector('.txn-row-error');
    if (errCell) errCell.textContent = '';
    tbody.appendChild(clone);
    form.querySelector('#txn-rows-count').value = String(form.querySelectorAll('.txn-row').length);
    wireRow(clone);
    gateRow(clone);
  }

  // --- select-on-focus (Appendix C) ---------------------------------------
  form.addEventListener('focusin', (evt) => {
    const el = evt.target;
    if (el && (el.tagName === 'INPUT') && typeof el.select === 'function') {
      el.select();
    }
  });

  // --- date field ---------------------------------------------------------
  // The #txn-date input carries class js-datefield, so the shell-wide datefield.js
  // (p23.4) owns its flexible parse/format, the GnuCash shortcuts ([ ] - + h t) and
  // the calendar popover. Nothing to wire here.

  // --- per-row wiring -----------------------------------------------------
  function wireRow(row) {
    const i = row.dataset.row;
    const acctSel = form.querySelector(`#txn-account-${i}`);
    if (acctSel) {
      acctSel.addEventListener('change', () => {
        gateRow(row);
        markSubsidiaryConflicts();
        recompute();
      });
    }
    ['dr', 'cr', 'amount'].forEach((kind) => {
      const el = form.querySelector(`#txn-${kind}-${i}`);
      if (el) {
        el.addEventListener('input', () => {
          syncRowAmount(row);
          recompute();
        });
      }
    });
    const fundSel = form.querySelector(`#txn-fund-${i}`);
    if (fundSel) fundSel.addEventListener('change', recompute);
  }

  // --- keyboard grid (Appendix C, p12.6) ----------------------------------
  // Wire txngrid.js's pure state machine to the real grid. The column model is the
  // ordered list of editable cells per row, in DOM/tab order; it depends on the
  // display mode (DR/CR splits the amount into two visible columns). `field` is the
  // id stem (#txn-<field>-<i>); `always` marks cells shown on every row; program and
  // class are shown only on R/E / expense rows (rowReveal, the single source of
  // truth) and are the cells the traversal must skip on other rows.
  const gridCols = drcr
    ? [
        { field: 'account', always: true },
        { field: 'dr', always: true },
        { field: 'cr', always: true },
        { field: 'fund', always: true },
        { field: 'program', reveal: 'program' },
        { field: 'class', reveal: 'class' },
        { field: 'memo', always: true },
      ]
    : [
        { field: 'account', always: true },
        { field: 'amount', always: true },
        { field: 'fund', always: true },
        { field: 'program', reveal: 'program' },
        { field: 'class', reveal: 'class' },
        { field: 'memo', always: true },
      ];

  // cellInput returns the focusable input/select for (rowIndex, col), or null.
  function cellInput(rowIndex, col) {
    const spec = gridCols[col];
    if (!spec) return null;
    return form.querySelector(`#txn-${spec.field}-${rowIndex}`);
  }

  // colOfField maps a focused input's field stem to its column index.
  function colOfField(field) {
    return gridCols.findIndex((c) => c.field === field);
  }

  // gridIsVisible(rowIndex, col) -> is this cell a focus target for that row? Always
  // cells are; program/class follow rowReveal. Out-of-range cols are not visible.
  function gridIsVisible(rowIndex, col) {
    const spec = gridCols[col];
    if (!spec) return false;
    if (spec.always) return true;
    return !!rowReveal(rowIndex)[spec.reveal];
  }

  // Swap two rows' VALUES field-by-field (ids stay stable -- the whole editor keys
  // off row index). Used by Alt+Arrow move-row. Copies every editable field plus the
  // hidden signed-amount and split-id sinks, then re-gates/recomputes.
  function swapRowValues(a, b) {
    const fields = drcr
      ? ['account', 'dr', 'cr', 'amount', 'fund', 'program', 'class', 'memo', 'splitid']
      : ['account', 'amount', 'fund', 'program', 'class', 'memo', 'splitid'];
    fields.forEach((f) => {
      const ea = form.querySelector(`#txn-${f}-${a}`);
      const eb = form.querySelector(`#txn-${f}-${b}`);
      if (!ea || !eb) return;
      const tmp = ea.value;
      ea.value = eb.value;
      eb.value = tmp;
    });
    const rowA = form.querySelector(`.txn-row[data-row="${a}"]`);
    const rowB = form.querySelector(`.txn-row[data-row="${b}"]`);
    if (rowA) gateRow(rowA);
    if (rowB) gateRow(rowB);
    markSubsidiaryConflicts();
    recompute();
  }

  const submitBtn = form.querySelector('.txn-submit button[type="submit"]');
  const cancelLink = form.querySelector('.txn-submit a');

  const grid = form.querySelector('.txn-grid');
  if (grid) {
    grid.addEventListener('keydown', (evt) => {
      // Scope: only handle keys fired from a real grid input/select. The payee and
      // date inputs live in .txn-header (outside .txn-grid), so they keep their own
      // handlers untouched. Ignore keys with no mapped field (defensive).
      const el = evt.target;
      if (!el || !el.id) return;
      const m = /^txn-([a-z]+)-(\d+)$/.exec(el.id);
      if (!m) return;
      const col = colOfField(m[1]);
      if (col < 0) return; // e.g. #txn-splitid-i (hidden, not a grid column)
      const rowIndex = Number(m[2]);
      const rowEls = [...form.querySelectorAll('.txn-row')];
      const rows = rowEls.length;
      const gridShape = { rows, cols: gridCols.length };

      const mods = { ctrl: evt.ctrlKey || evt.metaKey, alt: evt.altKey };
      // Let a plain Enter be handled (advance/add-row); everything else that the
      // machine reports as 'none' falls through to native behavior (e.g. Arrow keys
      // operating a <select>, typing into an amount input).
      const { cell, action } = nextCell(
        gridShape,
        { row: rowIndex, col },
        evt.key,
        evt.shiftKey,
        mods,
        gridIsVisible,
      );
      if (action === 'none') return;

      if (action === 'save') {
        evt.preventDefault();
        if (submitBtn && typeof form.requestSubmit === 'function') form.requestSubmit(submitBtn);
        else if (submitBtn) submitBtn.click();
        return;
      }
      if (action === 'cancel') {
        evt.preventDefault();
        if (cancelLink) cancelLink.click();
        return;
      }
      if (action === 'add-row') {
        evt.preventDefault();
        addRow();
        const first = cellInput(rows, 0); // new row index == old row count
        if (first) first.focus();
        return;
      }
      if (action === 'move-row-down' || action === 'move-row-up') {
        evt.preventDefault();
        swapRowValues(rowIndex, cell.row);
        const moved = cellInput(cell.row, col);
        if (moved) moved.focus();
        return;
      }
      if (action === 'move') {
        // Boundary no-op (target == current cell): don't trap focus -- let native
        // Tab carry out of the grid to the Save controls.
        if (cell.row === rowIndex && cell.col === col) return;
        const target = cellInput(cell.row, cell.col);
        if (target) {
          evt.preventDefault();
          target.focus();
        }
      }
    });

    // Auto-append (p25.2): any edit that leaves the last row non-empty grows a fresh
    // trailing empty row. Delegated on the grid so it survives addRow without re-wiring.
    grid.addEventListener('input', ensureTrailingEmptyRow);
    grid.addEventListener('change', ensureTrailingEmptyRow);
  }

  // --- payee autocomplete + autofill (p12.3) ------------------------------
  // Progressive enhancement: hide the native <select> (the no-JS value sink) and
  // reveal the autocomplete input. Picking a suggestion writes the id into the select
  // (so submit is unchanged), then fetches the payee's template and applies it -- but
  // ONLY when every current row is empty (never-overwrites, allRowsEmpty; the pure
  // guard). The template fetch is triggered PROGRAMMATICALLY (not an hx-* trigger on a
  // freshly-swapped suggestion node) so it never races htmx's settle-tick wiring.
  const payeeSelect = form.querySelector('#txn-payee');
  const payeeAuto = form.querySelector('.txn-payee-autocomplete');
  const payeeInput = form.querySelector('#txn-payee-input');
  const payeeName = form.querySelector('#txn-payee-name');
  const payeeList = form.querySelector('#txn-payee-suggestions');
  if (payeeSelect && payeeAuto && payeeInput && payeeName && payeeList) {
    payeeSelect.style.display = 'none';
    payeeAuto.hidden = false;

    // Typing a name (without picking a suggestion) is a NEW payee: post the typed
    // name via payee_name and reset the id sink so the handler find-or-creates by
    // name (create-on-save). Picking a suggestion overrides both (setPayee).
    payeeInput.addEventListener('input', () => {
      payeeName.value = payeeInput.value;
      payeeSelect.value = '0';
    });

    function currentRowValues() {
      return [...form.querySelectorAll('.txn-row')].map((row) => {
        const i = row.dataset.row;
        const acct = form.querySelector(`#txn-account-${i}`);
        const amount = form.querySelector(`#txn-amount-${i}`);
        const dr = form.querySelector(`#txn-dr-${i}`);
        const cr = form.querySelector(`#txn-cr-${i}`);
        const memo = form.querySelector(`#txn-memo-${i}`);
        return {
          account: acct ? acct.value : '',
          amount: amount ? amount.value : '',
          dr: dr ? dr.value : '',
          cr: cr ? cr.value : '',
          memo: memo ? memo.value : '',
        };
      });
    }

    function setPayee(id, name) {
      // Ensure the select has an option for this id (it may be a payee not in the
      // initial option list), then select it so submit posts the chosen payee.
      let opt = payeeSelect.querySelector(`option[value="${id}"]`);
      if (!opt) {
        opt = document.createElement('option');
        opt.value = id;
        opt.textContent = name;
        payeeSelect.appendChild(opt);
      }
      payeeSelect.value = id;
      payeeInput.value = name;
      payeeName.value = name;
    }

    function clearSuggestions() {
      payeeList.textContent = '';
      payeeInput.setAttribute('aria-expanded', 'false');
    }

    function applyTemplateRows(html) {
      // The partial is #txn-rows content plus an oob notice; parse and take the rows.
      const tmp = document.createElement('tbody');
      tmp.innerHTML = html;
      const newRows = [...tmp.querySelectorAll('.txn-row')];
      if (newRows.length === 0) return; // nothing to apply (e.g. all dropped)
      const tbody = form.querySelector('#txn-rows');
      tbody.textContent = '';
      newRows.forEach((r) => tbody.appendChild(r));
      form.querySelector('#txn-rows-count').value = String(newRows.length);
      // Re-wire + re-gate the swapped-in rows (they are fresh nodes).
      form.querySelectorAll('.txn-row').forEach(wireRow);
      gateAll();
      markSubsidiaryConflicts();
      recompute();
      ensureTrailingEmptyRow(); // a payee template brings in filled rows -> keep a trailing empty
    }

    function fetchAndApplyTemplate(id) {
      // Never overwrite user input: apply only when the grid is entirely empty.
      if (!allRowsEmpty(currentRowValues())) return;
      const base = payeeAuto.dataset.templateUrl || '/payees';
      const sub = subSel ? subSel.value : '';
      const url = `${base}/${encodeURIComponent(id)}/template?sub=${encodeURIComponent(sub)}`;
      // A manual fetch (not htmx) so the request fires immediately on pick, dodging the
      // settle-tick wiring race; we apply the rows + the out-of-band notice ourselves.
      fetch(url, { headers: { 'HX-Request': 'true' } })
        .then((resp) => (resp.ok ? resp.text() : ''))
        .then((html) => {
          if (!html) return;
          const doc = new DOMParser().parseFromString(html, 'text/html');
          // The notice comes as an oob element; swap it into the live notice region.
          const oob = doc.querySelector('#txn-autofill-notice');
          const dest = form.querySelector('#txn-autofill-notice');
          if (oob && dest) dest.replaceWith(oob);
          applyTemplateRows(html);
        })
        .catch(() => {});
    }

    function pick(item) {
      if (!item) return;
      setPayee(item.dataset.payeeId, item.dataset.payeeName);
      clearSuggestions();
      fetchAndApplyTemplate(item.dataset.payeeId);
    }

    payeeList.addEventListener('click', (evt) => {
      const item = evt.target.closest('.txn-payee-suggestion');
      if (item) pick(item);
    });
    payeeInput.addEventListener('keydown', (evt) => {
      if (evt.key === 'Enter') {
        const first = payeeList.querySelector('.txn-payee-suggestion');
        if (first) {
          evt.preventDefault();
          pick(first);
        }
      } else if (evt.key === 'Escape') {
        clearSuggestions();
      }
    });
    // Reflect suggestion presence in aria-expanded after each htmx swap of the list.
    payeeList.addEventListener('htmx:afterSwap', () => {
      payeeInput.setAttribute('aria-expanded', payeeList.children.length > 0 ? 'true' : 'false');
    });
  }

  form.querySelectorAll('.txn-row').forEach(wireRow);
  if (subSel) subSel.addEventListener('change', markSubsidiaryConflicts);

  gateAll();
  markSubsidiaryConflicts();
  recompute();
  // Guarantee the one-trailing-empty-row invariant on every render (initial load, the
  // subsidiary re-filter swap, and a 422 re-render where the server dropped empties).
  ensureTrailingEmptyRow();
}

// Browser glue: initialize each editor form on load and after an htmx swap (the
// subsidiary re-filter / 422 re-render swaps the whole #txn-form). Guarded for Node.
if (typeof document !== 'undefined' && typeof document.addEventListener === 'function') {
  const init = () => {
    document.querySelectorAll('form#txn-form').forEach((f) => {
      if (!f.dataset.wired) {
        f.dataset.wired = '1';
        initEditor(f);
      }
    });
  };
  document.addEventListener('DOMContentLoaded', init);
  document.body && document.body.addEventListener('htmx:afterSwap', () => {
    // A swapped-in #txn-form is a fresh node; re-init it.
    document.querySelectorAll('form#txn-form').forEach((f) => {
      if (!f.dataset.wired) {
        f.dataset.wired = '1';
        initEditor(f);
      }
    });
  });
}

export { initEditor };
