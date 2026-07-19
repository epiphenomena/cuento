// p12.2 transaction editor -- DOM GLUE (trap 2: this is the thin, e2e-covered shim,
// NOT unit-tested; the pure logic it drives lives in txnamount.js / txnfund.js /
// txngrid.js and IS node-tested). Boring frontend (rule 12): a hand-written ES
// module, no framework, external (script-src 'self'), no inline handler.
//
// Responsibilities (all DISPLAY/UX; the server is the sole validator, trap 5):
//   - DR/CR twin columns -> normalize into the hidden signed amount_i field (trap 3,
//     the ONE mapping site is drcrToSigned in txnamount.js).
//   - live PER-FUND imbalance chips from fundImbalances (display only; p29.5 dropped the
//     always-zero overall Total chip -- the balancing main split makes it always 0).
//   - show the program select only on R/E rows, the class select only on expense
//     rows, prefilled from the account's data-* defaults (server re-defaults).
//   - subsidiary re-filter: flag rows whose account left the sub (invalidRowsForSub).
//   - select-on-focus, add-row, keyboard grid. (The date input's shortcuts +
//     calendar popover are the shell-wide datefield.js, p23.4.)
//
// Guarded so importing under Node is side-effect free (no `document`).

import { parseAmountMinor, drcrToSigned, formatSignedMinor } from './txnamount.js';
import { fundImbalances, chipLabel } from './txnfund.js';
import { nextCell, invalidRowsForSubsidiary } from './txngrid.js';
import { isRowEmpty } from './rowstate.js';
import { initCombos, stripCombo, resyncCombos } from './combobox.js';
import { initDescField, stripDescField } from './descfield.js';

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

  // fundNames builds the fund id -> NAME lookup from ANY row's fund <select> options
  // (all rows share the same option set). The names are proper nouns the server
  // already rendered into the options (stored data, rule 9 -- not a catalog key), so
  // the per-fund chip resolves to the fund NAME rather than its database id. The
  // value="0" (unrestricted) option is skipped; its localized label is stamped
  // separately on the form (data-chip-unrestricted).
  function fundNames() {
    const names = {};
    const sel = form.querySelector('.txn-fund');
    if (!sel) return names;
    for (const o of sel.options) {
      if (o.value && o.value !== '0') names[o.value] = (o.textContent || '').trim();
    }
    return names;
  }

  function recompute() {
    const rows = [...form.querySelectorAll('.txn-row')].map((row) => {
      const i = row.dataset.row;
      const fundSel = form.querySelector(`#txn-fund-${i}`);
      return { fund: fundSel ? fundSel.value.replace(/^0$/, '') : '', amount: rowAmount(i) };
    });
    const { total, perFund } = fundImbalances(rows);
    // Chip labels: the per-fund chips use the fund names read from the fund options
    // (rule 9) + the localized "Unrestricted" label (server-stamped on the form). Amount
    // formatting stays client-side. (p29.5 removed the overall Total chip -- a transaction
    // is enforced zero-sum overall too, so the balancing main split makes it always 0.)
    const unrestrictedLabel = form.dataset.chipUnrestricted || 'Unrestricted';

    const chips = form.querySelector('#txn-fund-chips');
    if (chips) {
      chips.textContent = '';
      const names = fundNames();
      Object.keys(perFund).forEach((k) => {
        const span = document.createElement('span');
        span.className = 'txn-fund-chip imbalanced';
        span.textContent = fmtChip(chipLabel(k, names, unrestrictedLabel), perFund[k]);
        chips.appendChild(span);
      });
    }
    // p26.34: the header main split's amount is the auto-balanced residual -(body sum).
    // Display-only preview -- the server recomputes the authoritative per-fund residual on
    // save (rules 3+12: money math in Go). Blank when the body is empty/zero.
    const mainAmt = form.querySelector('#txn-main-amount');
    if (mainAmt) {
      mainAmt.value = total === 0 ? '' : formatSignedMinor(-total, exp);
    }
  }

  function fmtChip(label, minor) {
    return `${label}: ${formatSignedMinor(minor, exp)}`;
  }

  // --- combined program/class gating per account (p26.41) -----------------
  // rowReveal is the SINGLE source of truth for which conditional cells a row shows,
  // derived from the chosen account's type. gateRow uses it to toggle visibility;
  // the keyboard grid's isVisible() (below) uses it to skip hidden cells. `i` is the
  // row's dataset.row index. The combined program/class control is a SINGLE cell shown on
  // R/E rows (the `progclass` reveal); its Admin/Fundraising "class" options are shown
  // only on EXPENSE rows.
  function rowReveal(i) {
    const acctSel = form.querySelector(`#txn-account-${i}`);
    const opt = acctSel ? acctSel.selectedOptions[0] : null;
    const type = opt ? opt.dataset.type : '';
    const isRE = type === 'revenue' || type === 'expense';
    const isExpense = type === 'expense';
    return { isRE, isExpense, progclass: isRE };
  }

  // programValue encodes a program id as the combined control's option value (p:<programID>),
  // mirroring the server's decodeProgClass. The c:<class> values are literal option values.
  function programValue(programID) {
    return `p:${programID}`;
  }

  function gateRow(row) {
    const i = row.dataset.row;
    const acctSel = form.querySelector(`#txn-account-${i}`);
    const pcCell = row.querySelector('.txn-progclass-cell');
    const pcSel = form.querySelector(`#txn-progclass-${i}`);
    const progCarrier = form.querySelector(`#txn-program-${i}`);
    if (!acctSel) return;
    const opt = acctSel.selectedOptions[0];
    const { isRE, isExpense } = rowReveal(i);

    if (pcCell) pcCell.style.visibility = isRE ? 'visible' : 'hidden';

    // Show/hide the two "class" entries (Admin / Fundraising, data-class="1") -- expense
    // rows offer them above the program tree; a revenue row offers programs ONLY (rule 7).
    if (pcSel) {
      [...pcSel.options].forEach((o) => {
        if (o.dataset.class === '1') o.hidden = !isExpense;
      });
    }

    if (isRE && pcSel && opt) {
      // Prefill default (never override a value the user / server round-trip already set).
      // A program pick is the default for BOTH revenue and expense; precedence (p26.5): the
      // account's own default_program wins; else the user's default_program; else root (D24).
      const isClassPick = pcSel.value.startsWith('c:');
      const isProgPick = pcSel.value.startsWith('p:');
      if (!isClassPick && !isProgPick) {
        const def = opt.dataset.defaultProgram;
        const userDef = form.dataset.userProgram;
        let prog = '';
        if (def && def !== '0') prog = def;
        else if (userDef && userDef !== '0') prog = userDef;
        else prog = form.dataset.rootProgram || '';
        // p26.39: an expense account with a default CLASS (management/fundraising) starts on
        // that class instead of a program; else default to the program node.
        const defClass = isExpense ? opt.dataset.defaultClass : '';
        if (defClass && defClass !== '' && defClass !== 'program') pcSel.value = `c:${defClass}`;
        else if (prog !== '') pcSel.value = programValue(prog);
        if (prog !== '' && progCarrier && (!progCarrier.value || progCarrier.value === '0')) {
          progCarrier.value = prog; // seed the hidden carrier so a later Admin pick keeps it
        }
      } else if (isProgPick && progCarrier) {
        // Keep the hidden carrier in step with a program pick (so switching to Admin keeps it).
        progCarrier.value = pcSel.value.slice('p:'.length);
      }
    }
    if (!isRE) {
      // A/L/E row: no program/class. Clear the combined pick AND the hidden carrier so the
      // server (which defaults program on R/E, forbids it elsewhere) gets a clean row.
      if (pcSel) pcSel.value = '';
      if (progCarrier) progCarrier.value = '';
    }
    // The combined select was set directly above -> refresh its combo overlay so the
    // visible label matches (no `change` fired by a .value= assignment).
    resyncCombos(row);
  }

  function gateAll() {
    form.querySelectorAll('.txn-row').forEach(gateRow);
  }

  // --- subsidiary re-filter (client display; server also re-filters) ------
  const subSel = form.querySelector('#txn-subsidiary');
  function markSubsidiaryConflicts() {
    if (!subSel) return;
    const sub = subSel.value;
    const accountSubs = {};
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
    // p26.32: each split is its own <tbody class="txn-row"> (two-row layout), so there is
    // no single wrapping tbody -- append the cloned tbody to the grid table itself.
    const table = form.querySelector('.txn-grid');
    const rows = form.querySelectorAll('.txn-row');
    const template = rows[rows.length - 1];
    if (!table || !template) return;
    const idx = rows.length;
    const clone = template.cloneNode(true);
    clone.dataset.row = String(idx);
    clone.classList.remove('sub-conflict');
    clone.removeAttribute('data-row-error');
    // Combobox clone contract (p26.2): the template's account cell carries an enhanced
    // combobox whose overlay listeners cloneNode does NOT copy (a dead wrapper). Strip it
    // back to a clean native <select> BEFORE the id/name rewrite so the overlay's own
    // nodes aren't re-indexed; initCombos(clone) below re-enhances the fresh select.
    stripCombo(clone);
    // p26.19: same clone contract for the description field -- its suggest/prefill
    // listeners were not copied by cloneNode, so strip the marker + empty the cloned
    // listbox BEFORE the id rewrite; initDescField(clone) below re-wires the fresh input.
    stripDescField(clone);
    // Rewrite every id/name suffix to the new index; clear values.
    clone.querySelectorAll('[id],[name]').forEach((el) => {
      if (el.id) el.id = el.id.replace(/-\d+$/, `-${idx}`);
      if (el.name) el.name = el.name.replace(/_\d+$/, `_${idx}`);
      if (el.tagName === 'INPUT') el.value = '';
      if (el.tagName === 'SELECT') el.selectedIndex = 0;
    });
    // p26.19: the description input's data-desc-container points at its per-row listbox id
    // (txn-desc-list-<i>) -- re-index it alongside the id/name suffixes so the clone's
    // input finds its OWN (re-indexed) listbox, not the template row's.
    clone.querySelectorAll('[data-desc-container]').forEach((el) => {
      el.dataset.descContainer = el.dataset.descContainer.replace(/-\d+$/, `-${idx}`);
    });
    const errCell = clone.querySelector('.txn-row-error');
    if (errCell) errCell.textContent = '';
    table.appendChild(clone);
    form.querySelector('#txn-rows-count').value = String(form.querySelectorAll('.txn-row').length);
    initCombos(clone, { onAdvance: advanceComboFocus }); // enhance the clone's clean account select
    initDescField(clone); // p26.19: re-wire the clone's clean description input
    wireRow(clone);
    gateRow(clone);
  }

  // --- delete row (p26.23) -------------------------------------------------
  // Mirrors the expense grid's per-row × (expensegrid.js): remove the row (or reset it in
  // place when it is the only row), re-index the survivors to a contiguous 0..n-1 so the
  // name="_<i>" scheme stays contiguous, update the rows-count, and re-assert the
  // one-trailing-empty invariant. The combobox/descfield enhancements are closure-bound to
  // the element (not the id), so a re-index does NOT need a strip+re-init; only the
  // id/name/data-* suffixes are rewritten. After a structural change we re-gate + recompute
  // + re-mark subsidiary conflicts so the chips and per-row state stay correct.
  function reindexRow(rowEl, idx) {
    rowEl.dataset.row = String(idx);
    if (rowEl.hasAttribute('data-row-error')) rowEl.setAttribute('data-row-error', String(idx));
    rowEl.querySelectorAll('[id],[name]').forEach((el) => {
      if (el.id) el.id = el.id.replace(/-\d+$/, `-${idx}`);
      if (el.name) el.name = el.name.replace(/_\d+$/, `_${idx}`);
    });
    // Keep the description input's listbox pointer (txn-desc-list-<i>) on its OWN row.
    rowEl.querySelectorAll('[data-desc-container]').forEach((el) => {
      el.dataset.descContainer = el.dataset.descContainer.replace(/-\d+$/, `-${idx}`);
    });
  }
  // resetRow clears one row's inputs/selects in place (used for the sole/last delete so the
  // grid never drops to zero rows), then resyncs its combos + re-gates it.
  function resetRow(rowEl) {
    rowEl.classList.remove('sub-conflict');
    rowEl.removeAttribute('data-row-error');
    rowEl.querySelectorAll('input').forEach((el) => {
      el.value = '';
    });
    rowEl.querySelectorAll('select').forEach((el) => {
      el.selectedIndex = 0;
    });
    const errCell = rowEl.querySelector('.txn-row-error');
    if (errCell) errCell.textContent = '';
    resyncCombos(rowEl); // the selects were set directly -> refresh their overlay text
    gateRow(rowEl); // re-hide program/class and re-apply defaults on the cleared row
  }
  function deleteRow(rowEl) {
    const rowEls = [...form.querySelectorAll('.txn-row')];
    if (rowEls.length <= 1) {
      resetRow(rowEl);
      form.querySelector('#txn-rows-count').value = String(form.querySelectorAll('.txn-row').length);
    } else {
      rowEl.remove();
      [...form.querySelectorAll('.txn-row')].forEach((r, i) => reindexRow(r, i));
      form.querySelector('#txn-rows-count').value = String(form.querySelectorAll('.txn-row').length);
      ensureTrailingEmptyRow();
    }
    markSubsidiaryConflicts();
    recompute();
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
    // p26.41: a combined program/class pick keeps the hidden program carrier in step so a
    // later switch to Admin/Fundraising retains the last-chosen program.
    const pcSel = form.querySelector(`#txn-progclass-${i}`);
    if (pcSel) {
      pcSel.addEventListener('change', () => {
        const progCarrier = form.querySelector(`#txn-program-${i}`);
        if (progCarrier && pcSel.value.startsWith('p:')) progCarrier.value = pcSel.value.slice('p:'.length);
      });
    }
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
        { field: 'desc', always: true },
        { field: 'account', always: true },
        { field: 'dr', always: true },
        { field: 'cr', always: true },
        { field: 'fund', always: true },
        { field: 'progclass', reveal: 'progclass' },
        { field: 'memo', always: true },
      ]
    : [
        { field: 'desc', always: true },
        { field: 'account', always: true },
        { field: 'amount', always: true },
        { field: 'fund', always: true },
        { field: 'progclass', reveal: 'progclass' },
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

  // advanceComboFocus (p28.3): after an Enter-pick from an OPEN combobox overlay, move
  // focus to the NEXT grid cell -- SKIPPING visibility:hidden cells (the progclass cell on
  // non-R/E rows), reusing the same gridCols model the keyboard traversal uses. The
  // overlay input carries no id, so the grid's keydown handler never sees it; this is the
  // combobox's dedicated onAdvance hook. `select` is the enhanced native <select> (its id
  // is txn-<field>-<i>), from which we derive the current row/col. Enter-pick from the LAST
  // visible cell just leaves focus on the picked select (no wrap).
  function advanceComboFocus(select) {
    if (!select || !select.id) return;
    // The HEADER main-account combo (#txn-main-account) has no row index; an Enter-pick
    // there advances to the first BODY row's first cell (its description), mirroring the
    // top-to-bottom entry flow (header account -> body rows). Without this the header combo
    // would keep the onAdvance no-op (its id doesn't match the per-row pattern below).
    if (select.id === 'txn-main-account') {
      const first = cellInput(0, 0);
      if (first && typeof first.focus === 'function') first.focus();
      return;
    }
    const m = /^txn-([a-z]+)-(\d+)$/.exec(select.id);
    if (!m) return;
    const rowIndex = Number(m[2]);
    let col = colOfField(m[1]);
    if (col < 0) return;
    for (col += 1; col < gridCols.length; col += 1) {
      if (!gridIsVisible(rowIndex, col)) continue;
      const target = cellInput(rowIndex, col);
      if (target && typeof target.focus === 'function') {
        target.focus();
        return;
      }
    }
  }

  // Swap two rows' VALUES field-by-field (ids stay stable -- the whole editor keys
  // off row index). Used by Alt+Arrow move-row. Copies every editable field plus the
  // hidden signed-amount and split-id sinks, then re-gates/recomputes.
  function swapRowValues(a, b) {
    const fields = drcr
      ? ['account', 'dr', 'cr', 'amount', 'fund', 'progclass', 'program', 'desc', 'memo', 'splitid']
      : ['account', 'amount', 'fund', 'progclass', 'program', 'desc', 'memo', 'splitid'];
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

  // --- client guard: a content-bearing row must have an account (trap 2) --------
  // A row that carries an amount / DR / CR / memo but no account (value "0") must NOT
  // post silently -- the server would reject it with ErrAccountMissing, but flagging it
  // client-side gives an immediate per-row error. Only the trailing EMPTY row is allowed
  // to have no account (it is dropped server-side). Returns true when at least one row
  // was flagged (submit should be blocked). Uses the SAME emptiness predicate as the
  // auto-append (isRowEmpty), so client and server agree on which row is droppable.
  function flagAccountlessRows() {
    const msg = form.dataset.accountMissingMsg || '';
    let flagged = false;
    let firstBad = -1;
    [...form.querySelectorAll('.txn-row')].forEach((row) => {
      const i = row.dataset.row;
      const acctSel = form.querySelector(`#txn-account-${i}`);
      const errCell = row.querySelector('.txn-row-error');
      const acct = acctSel ? acctSel.value : '';
      const empty = isRowEmpty(rowFieldValues(row));
      const accountless = (acct === '' || acct === '0');
      if (!empty && accountless) {
        row.setAttribute('data-row-error', i);
        if (errCell) {
          errCell.textContent = '';
          const span = document.createElement('span');
          span.className = 'field-error';
          span.setAttribute('role', 'alert');
          span.textContent = msg;
          errCell.appendChild(span);
        }
        if (firstBad < 0) firstBad = Number(i);
        flagged = true;
      } else if (row.getAttribute('data-row-error') === i && errCell && errCell.querySelector('.field-error')) {
        // Clear a stale client-set account error once the row gains an account or empties.
        row.removeAttribute('data-row-error');
        errCell.textContent = '';
      }
    });
    if (firstBad >= 0) {
      // Focus the ACCOUNT cell (the missing field), not column 0 -- which is now the
      // description column (p26.23 moved description to the first column).
      const cell = cellInput(firstBad, colOfField('account'));
      if (cell && typeof cell.focus === 'function') cell.focus();
    }
    return flagged;
  }

  // Native submit guard (no-JS / non-htmx path).
  form.addEventListener('submit', (evt) => {
    if (flagAccountlessRows()) evt.preventDefault();
  });
  // htmx submit guard: htmx fires its XHR from its OWN submit listener and does NOT
  // consult defaultPrevented, so a plain preventDefault above does not stop it. The
  // cancelable htmx:beforeRequest is the hook. Filter to THIS form's POST so the
  // subsidiary re-filter hx-get (and any GET) is untouched.
  form.addEventListener('htmx:beforeRequest', (evt) => {
    const cfg = evt.detail && evt.detail.requestConfig;
    const verb = cfg && cfg.verb ? String(cfg.verb).toLowerCase() : '';
    if (verb !== 'get' && flagAccountlessRows()) {
      evt.preventDefault(); // stops the request; the per-row error span stays visible
    }
  });

  const grid = form.querySelector('.txn-grid');
  if (grid) {
    grid.addEventListener('keydown', (evt) => {
      // Scope: only handle keys fired from a real grid input/select. The subsidiary/date
      // inputs live in .txn-main-header (outside .txn-grid), so they keep their own handlers.
      // The per-row description input (#txn-desc-<i>) IS a grid cell (Tab traverses it),
      // but while its suggestion listbox is open descfield.js stopPropagation's the
      // ↑/↓/Enter/Esc keys so they never reach this handler (pick != move/save/cancel).
      // Ignore keys with no mapped field (defensive).
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

    // Per-row delete (p26.23): the × button. type="button" so it never submits; delegated
    // so it survives addRow/re-index. Deleting the only/last row resets it in place.
    grid.addEventListener('click', (evt) => {
      const btn = evt.target.closest('.txn-delete');
      if (!btn) return;
      evt.preventDefault();
      const rowEl = btn.closest('.txn-row');
      if (rowEl) deleteRow(rowEl);
    });
  }

  // p26.19: the per-transaction payee autofill (p12.3/p26.3) was REMOVED. Its whole-grid
  // "fetch the payee's last transaction and fill every empty row" is replaced by the
  // per-ROW description prefill (descfield.js): each row's free-text description
  // autocompletes + prefills THAT row from the matched split. Wired below via
  // initDescField, alongside the account/fund/program combos.

  form.querySelectorAll('.txn-row').forEach(wireRow);
  if (subSel) subSel.addEventListener('change', markSubsidiaryConflicts);

  // p26.2: enhance the account combo on every row present on initial render / after a
  // whole-form htmx swap (subsidiary re-filter, 422 re-render). Idempotent. p28.3: pass
  // onAdvance so an Enter-pick from an open combo overlay advances to the next grid cell.
  initCombos(form, { onAdvance: advanceComboFocus });
  // p26.19: wire the per-row description autocomplete + prefill on every row (idempotent,
  // like initCombos -- covers initial render, htmx swaps, and clones via addRow).
  initDescField(form);

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
  // p26.35: a BOOSTED nav ("New transaction") swaps <body> then injects THIS module for the
  // first time in the document. The swap's htmx:afterSwap already fired (before the listener
  // above was attached) and DOMContentLoaded fired on the ORIGINAL page, so neither re-runs
  // -- the grid would stay un-enhanced. When the DOM is already parsed at module-eval time,
  // enhance immediately (idempotent via data-wired) so the boost-entered editor wires up.
  if (document.readyState !== 'loading') init();
}

export { initEditor };
