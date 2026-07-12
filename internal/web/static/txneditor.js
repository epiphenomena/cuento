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
//   - select-on-focus, date shortcuts (t / + / -), add-row, keyboard grid.
//
// Guarded so importing under Node is side-effect free (no `document`).

import { parseAmountMinor, drcrToSigned, formatSignedMinor } from './txnamount.js';
import { fundImbalances, applyFundToAll } from './txnfund.js';
import { nextCell, invalidRowsForSubsidiary } from './txngrid.js';
import { allRowsEmpty } from './txnpayee.js';

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
  function gateRow(row) {
    const i = row.dataset.row;
    const acctSel = form.querySelector(`#txn-account-${i}`);
    const progCell = row.querySelector('.txn-program-cell');
    const classCell = row.querySelector('.txn-class-cell');
    const progSel = form.querySelector(`#txn-program-${i}`);
    const classSel = form.querySelector(`#txn-class-${i}`);
    if (!acctSel) return;
    const opt = acctSel.selectedOptions[0];
    const type = opt ? opt.dataset.type : '';
    const isRE = type === 'revenue' || type === 'expense';
    const isExpense = type === 'expense';

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

  // --- add row ------------------------------------------------------------
  const addBtn = form.querySelector('#txn-add-row');
  if (addBtn) {
    addBtn.addEventListener('click', () => addRow());
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

  // --- date shortcuts (t / + / -) -----------------------------------------
  const dateInput = form.querySelector('#txn-date');
  if (dateInput) {
    dateInput.addEventListener('keydown', (evt) => {
      const k = evt.key;
      if (k === 't' || k === 'T') {
        evt.preventDefault();
        dateInput.value = shiftDate(today(), 0, form.dataset.dateFormat);
      } else if (k === '+') {
        evt.preventDefault();
        dateInput.value = shiftDate(parseDisplayDate(dateInput.value) || today(), 1, form.dataset.dateFormat);
      } else if (k === '-') {
        evt.preventDefault();
        dateInput.value = shiftDate(parseDisplayDate(dateInput.value) || today(), -1, form.dataset.dateFormat);
      }
    });
  }

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
}

// --- date helpers (glue-local; kept tiny) ---------------------------------
function today() {
  const d = new Date();
  return { y: d.getFullYear(), m: d.getMonth() + 1, d: d.getDate() };
}
function shiftDate(dt, days, fmt) {
  const d = new Date(dt.y, dt.m - 1, dt.d + days);
  const y = d.getFullYear();
  const m = String(d.getMonth() + 1).padStart(2, '0');
  const day = String(d.getDate()).padStart(2, '0');
  if (fmt === 'US') return `${m}/${day}/${y}`;
  if (fmt === 'EU') return `${day}/${m}/${y}`;
  return `${y}-${m}-${day}`;
}
function parseDisplayDate(s) {
  const iso = /^(\d{4})-(\d{2})-(\d{2})$/.exec(s);
  if (iso) return { y: +iso[1], m: +iso[2], d: +iso[3] };
  const parts = s.split(/[/.]/).map(Number);
  if (parts.length === 3 && parts.every((n) => !Number.isNaN(n))) {
    // Ambiguous US/EU; assume the value came from our own formatter.
    return null;
  }
  return null;
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
