// p26.19 per-split description field -- DOM GLUE (rule 12: hand-written external ES
// module, no framework, no inline handler, loaded under script-src 'self'). It replaces
// the removed per-transaction payee autofill: instead of ONE whole-grid template keyed
// off a picked payee, EACH grid row's free-text description autocompletes (GET
// /descriptions/suggest) and, on pick/commit, prefills THAT row's account/amount/fund/
// program/class/memo (GET /descriptions/prefill) -- but ONLY when the row is otherwise
// empty (the SAME never-overwrites decision as the old payee autofill, reusing the
// tested isRowEmpty predicate from rowstate.js).
//
// CONTRACT (so both entry grids reuse the SAME widget, mirroring combobox.js):
//   - initDescField(root): enhance every input.js-descfield:not([data-descfield]) under
//     `root`. Idempotent (the :not guard), so it is safe on load, after an htmx swap, and
//     after a row clone.
//   - stripDescField(root): for every enhanced descfield input under `root`, clear its
//     data-descfield marker and EMPTY its suggestion listbox container. Call this on a
//     freshly cloned row BEFORE the id/name rewrite (like stripCombo) so the clone --
//     whose overlay listeners cloneNode did NOT copy -- is rebuilt clean by initDescField.
//   - The input carries data-desc-container = the id STEM of its per-row suggestion
//     listbox (a sibling container the fetched <ul.desc-suggestions> lands in), and
//     data-desc-mode = the grid's amount display mode: "signed" | "drcr" | "magnitude"
//     (the expense grid, where the sign is derived from the account type).
//   - The <input> is NEVER moved; the listbox is a sibling. On PICK (keyboard/click) or
//     commit (blur/change with a non-empty value) it fetches the prefill and applies it
//     via applyPrefillToRow -- a PURE helper (node-tested) that returns the field ops,
//     which the glue then writes to the row's inputs + dispatches the events the parent
//     editor already listens for (account `change`, amount `input`) so gating / imbalance
//     chips / auto-append run for free.
//
// Guarded so importing under Node is side-effect free (no `document`), like combobox.js.

import { isRowEmpty } from './rowstate.js';

// --- PURE helpers (node-tested; no `document`) ------------------------------

// signedToMagnitude strips a leading '-' (and surrounding blanks) from a signed amount
// string, returning the positive magnitude text. Used by the expense grid, which stores
// a positive magnitude and derives the sign from the account type.
export function signedToMagnitude(amount) {
  if (typeof amount !== 'string') return '';
  return amount.trim().replace(/^-/, '').trim();
}

// applyPrefillToRow computes the field writes for prefilling ONE row from a prefill
// record, honoring the never-overwrites guard and the row's amount display mode. It is
// PURE: it takes the current row field values + the prefill data + the mode, and returns
// a plain object describing what to write (or null to write nothing). The DOM glue does
// the actual writes + event dispatch.
//
//   rowValues: { account, amount, dr, cr, memo } -- the row's current inputs (isRowEmpty).
//   prefill:   { found, account, amount, fund, program, class, memo } from the endpoint's
//              data-* attributes (amount is SIGNED, user-number-formatted).
//   mode:      'signed' (single amount_i), 'drcr' (twin dr/cr columns), or 'magnitude'
//              (expense grid: a positive amount input, sign derived server-side).
//
// Returns null when nothing should be applied (no match, or the row already carries user
// input). Otherwise an object with the fields to set: { account, fund, program, class,
// memo } always, plus EXACTLY ONE amount shape:
//   - signed:    { amount: '<signed>' }
//   - magnitude: { amount: '<magnitude>' }
//   - drcr:      { dr: '<mag>', cr: '' } when the signed amount is >= 0 (a debit), else
//                { dr: '', cr: '<mag>' } (a credit). A blank/zero amount clears both.
export function applyPrefillToRow(rowValues, prefill, mode) {
  if (!prefill || !prefill.found) return null;
  if (!isRowEmpty(rowValues)) return null; // never overwrite user input

  const out = {
    account: prefill.account || '',
    fund: prefill.fund || '',
    program: prefill.program || '',
    class: prefill.class || '',
    memo: prefill.memo || '',
  };
  const signed = typeof prefill.amount === 'string' ? prefill.amount.trim() : '';
  const mag = signedToMagnitude(signed);
  if (mode === 'drcr') {
    const isCredit = signed.startsWith('-');
    if (mag === '' || mag === '0' || /^0([.,]0+)?$/.test(mag)) {
      out.dr = '';
      out.cr = '';
    } else if (isCredit) {
      out.dr = '';
      out.cr = mag;
    } else {
      out.dr = mag;
      out.cr = '';
    }
  } else if (mode === 'magnitude') {
    out.amount = mag;
  } else {
    out.amount = signed;
  }
  return out;
}

// --- DOM glue (guarded; no self-init -- the parent editor calls initDescField) ---------

const DEBOUNCE_MS = 150;

// enhanceDescField wires ONE description input: debounced suggest into its row listbox,
// keyboard (↑/↓/Enter/Esc) + click pick, and prefill-on-pick/commit. `input` carries
// data-desc-container (listbox id stem) and data-desc-mode. `subOf()` reads the live
// subsidiary id; `rowValuesOf(input)` reads the row's current field values (for the
// never-overwrites guard); `writeRow(input, ops)` applies the prefill ops + dispatches
// the events the parent listens for.
function enhanceDescField(input, ctx) {
  if (!input || input.dataset.descfield === '1') return;
  input.dataset.descfield = '1';

  const list = ctx.listOf(input);
  if (!list) return;
  const mode = input.dataset.descMode || 'signed';

  let active = -1; // active <li> index within the current suggestion list
  let timer = null;

  function options() {
    return [...list.querySelectorAll('.desc-suggestion')];
  }
  function open() {
    list.hidden = false;
  }
  function close() {
    list.hidden = true;
    list.textContent = '';
    active = -1;
  }
  function renderActive() {
    options().forEach((li, idx) => {
      li.classList.toggle('active', idx === active);
      if (idx === active) li.setAttribute('aria-selected', 'true');
      else li.removeAttribute('aria-selected');
    });
  }

  function fetchSuggest() {
    const q = input.value;
    if (q.trim() === '') {
      close();
      return;
    }
    const url = `/descriptions/suggest?q=${encodeURIComponent(q)}&sub=${encodeURIComponent(ctx.subOf())}`;
    fetch(url, { headers: { 'HX-Request': 'true' } })
      .then((resp) => (resp.ok ? resp.text() : ''))
      .then((html) => {
        list.innerHTML = html;
        active = options().length > 0 ? 0 : -1;
        if (options().length > 0) {
          open();
          renderActive();
        } else {
          close();
        }
      })
      .catch(() => close());
  }

  // fetchPrefill applies the matched split's fields to THIS row when the row is empty.
  function fetchPrefill(desc) {
    if (desc.trim() === '') return;
    const url = `/descriptions/prefill?q=${encodeURIComponent(desc)}&sub=${encodeURIComponent(ctx.subOf())}`;
    fetch(url, { headers: { 'HX-Request': 'true' } })
      .then((resp) => (resp.ok ? resp.text() : ''))
      .then((html) => {
        if (!html) return;
        const doc = new DOMParser().parseFromString(html, 'text/html');
        const el = doc.querySelector('#desc-prefill');
        if (!el) return;
        const prefill = {
          found: el.dataset.found === '1',
          account: el.dataset.account || '',
          amount: el.dataset.amount || '',
          fund: el.dataset.fund || '',
          program: el.dataset.program || '',
          class: el.dataset.class || '',
          memo: el.dataset.memo || '',
        };
        const ops = applyPrefillToRow(ctx.rowValuesOf(input), prefill, mode);
        if (ops) ctx.writeRow(input, ops);
      })
      .catch(() => {});
  }

  function pick(li) {
    const desc = li.getAttribute('data-description') || li.textContent || '';
    input.value = desc;
    close();
    fetchPrefill(desc);
  }

  input.addEventListener('input', () => {
    if (timer) clearTimeout(timer);
    timer = setTimeout(fetchSuggest, DEBOUNCE_MS);
  });

  // Keyboard: handle ↑/↓/Enter/Esc ONLY while the listbox is open, and stopPropagation so
  // the .txn-grid delegated keydown (which maps this same input's id to a grid column)
  // never fires its own Enter=save / Escape=cancel / Arrow=move on our keys. When the
  // listbox is closed the keys bubble untouched, so grid Tab/arrow/add-row/save still work.
  input.addEventListener('keydown', (evt) => {
    const opts = options();
    const open = !list.hidden && opts.length > 0;
    if (evt.key === 'ArrowDown') {
      if (!open) return;
      evt.preventDefault();
      evt.stopPropagation();
      active = (active + 1) % opts.length;
      renderActive();
    } else if (evt.key === 'ArrowUp') {
      if (!open) return;
      evt.preventDefault();
      evt.stopPropagation();
      active = (active - 1 + opts.length) % opts.length;
      renderActive();
    } else if (evt.key === 'Enter') {
      if (!open || active < 0 || !opts[active]) return;
      evt.preventDefault();
      evt.stopPropagation();
      pick(opts[active]);
    } else if (evt.key === 'Escape') {
      if (!open) return;
      evt.preventDefault();
      evt.stopPropagation();
      close();
    }
  });

  // Click (mousedown so it beats the input's blur-close) picks a suggestion.
  list.addEventListener('mousedown', (evt) => {
    const li = evt.target.closest('.desc-suggestion');
    if (!li) return;
    evt.preventDefault();
    pick(li);
  });

  // Commit on blur: a typed description that matches an existing one still prefills an
  // empty row (the user may type the full text and Tab out without opening the list).
  input.addEventListener('blur', () => {
    const desc = input.value;
    setTimeout(() => {
      close();
      if (desc.trim() !== '') fetchPrefill(desc);
    }, DEBOUNCE_MS);
  });
}

// initDescField enhances every not-yet-enhanced description input under `root`
// (idempotent). The parent editor calls this on load, after an htmx swap, and after a
// row clone -- exactly like initCombos.
function initDescField(root) {
  const scope = root || (typeof document !== 'undefined' ? document : null);
  if (!scope || typeof scope.querySelectorAll !== 'function') return;
  scope.querySelectorAll('input.js-descfield:not([data-descfield])').forEach((input) => {
    const ctx = contextFor(input);
    if (ctx) enhanceDescField(input, ctx);
  });
}

// stripDescField clears the enhanced marker + empties the listbox on every descfield
// input under `root`, so a cloned (dead-listener) row is rebuilt clean by initDescField.
function stripDescField(root) {
  if (!root || typeof root.querySelectorAll !== 'function') return;
  const inputs = [];
  if (root.matches && root.matches('input.js-descfield')) inputs.push(root);
  root.querySelectorAll('input.js-descfield').forEach((i) => inputs.push(i));
  inputs.forEach((input) => {
    delete input.dataset.descfield;
    const stem = input.dataset.descContainer;
    const doc = input.ownerDocument;
    const list = stem && doc ? doc.getElementById(stem) : null;
    if (list) {
      list.textContent = '';
      list.hidden = true;
    }
  });
}

// contextFor builds the per-input ctx (list lookup, subsidiary, row values, row writer)
// from the input's DOM neighborhood. It is grid-agnostic: it derives the row element, the
// live subsidiary select, and the row's field inputs by convention so BOTH the txn grid
// (#txn-*-<i>) and the expense grid (#el-*-<i>) reuse it. Returns null if no owning
// document (defensive; the Node guard already prevents this path).
function contextFor(input) {
  const doc = input.ownerDocument;
  if (!doc) return null;
  const form = input.closest('form');
  const row = input.closest('tr');

  function listOf(el) {
    const stem = el.dataset.descContainer;
    return stem ? doc.getElementById(stem) : null;
  }
  function subOf() {
    // The txn editor's subsidiary select lives IN the form; the expense grid's picker
    // (#er-sub) is a SIBLING form, so fall back to a document-wide lookup. A locked sub
    // posts via a hidden [name="subsidiary"], covered by the same selector.
    const sel = '#txn-subsidiary, #er-sub, [name="subsidiary"]';
    let sub = form ? form.querySelector(sel) : null;
    if (!sub) sub = doc.querySelector(sel);
    return sub ? sub.value : '';
  }
  // rowValuesOf reads the emptiness-relevant fields (account/amount/dr/cr/memo) of the
  // input's row, by class (both grids share .txn-account/.el-account etc. naming pairs).
  function rowValuesOf(el) {
    const r = el.closest('tr');
    if (!r) return {};
    const pick = (sels) => {
      for (const s of sels) {
        const node = r.querySelector(s);
        if (node) return node.value;
      }
      return '';
    };
    return {
      account: pick(['.txn-account', '.el-account']),
      amount: pick(['.txn-amount', '.el-amount']),
      dr: pick(['.txn-dr']),
      cr: pick(['.txn-cr']),
      memo: pick(['.txn-memo', '.el-memo']),
    };
  }
  // writeRow applies the prefill ops to the row's inputs and dispatches the events the
  // parent editor already listens for (account `change`, amount `input`), so gating,
  // imbalance chips, subsidiary marking, and auto-append all run without descfield
  // reaching into the parent module.
  function writeRow(el, ops) {
    const r = el.closest('tr');
    if (!r) return;
    const set = (sels, value) => {
      for (const s of sels) {
        const node = r.querySelector(s);
        if (node) {
          node.value = value;
          return node;
        }
      }
      return null;
    };
    if ('memo' in ops) set(['.txn-memo', '.el-memo'], ops.memo);
    if ('fund' in ops) set(['.txn-fund', '.el-fund'], ops.fund === '' ? '0' : ops.fund);
    if ('program' in ops) set(['.txn-program', '.el-program'], ops.program === '' ? '0' : ops.program);
    if ('class' in ops) set(['.txn-class'], ops.class);
    // Amount shapes: signed/magnitude write the single amount input; drcr writes the twins.
    if ('amount' in ops) {
      const amtEl = set(['.txn-amount', '.el-amount'], ops.amount);
      if (amtEl) amtEl.dispatchEvent(new Event('input', { bubbles: true }));
    }
    if ('dr' in ops || 'cr' in ops) {
      const drEl = set(['.txn-dr'], ops.dr || '');
      const crEl = set(['.txn-cr'], ops.cr || '');
      const t = drEl || crEl;
      if (t) t.dispatchEvent(new Event('input', { bubbles: true }));
    }
    // Account LAST + with `change` so the parent's per-row gating (program/class reveal)
    // sees the already-set program/class values and refreshes the combo overlays.
    const acctEl = set(['.txn-account', '.el-account'], ops.account === '' ? '0' : ops.account);
    if (acctEl) acctEl.dispatchEvent(new Event('change', { bubbles: true }));
    // Refresh the fund/program combo overlays we set directly (they only auto-sync on
    // `change`, which we did NOT dispatch for them). Reuse the parent's resyncCombos via
    // the row scope; combobox.js is safe to import here (guarded).
    if (row && typeof doc.defaultView !== 'undefined') {
      resyncRowCombos(r);
    }
  }

  return { listOf, subOf, rowValuesOf, writeRow };
}

// resyncRowCombos refreshes each enhanced combo overlay's text in one row from its
// select's current selection (fund/program were set by value= with no `change`). Inlined
// (not importing combobox.resyncCombos) to keep descfield's import surface minimal and
// avoid a cycle; it mirrors that function's per-wrapper logic.
function resyncRowCombos(row) {
  row.querySelectorAll('.combo').forEach((wrap) => {
    const select = wrap.querySelector('select.combo-input');
    const inp = wrap.querySelector('.combo-text');
    if (!select || !inp || select.dataset.comboManual === '1') return;
    // Mirror combobox.currentLabel / resyncCombos (p26.22): value="0" is a REAL selectable
    // default (fund "Unrestricted", program "None") -- show its label, blank only value="".
    const opt = select.selectedOptions[0];
    let label = '';
    if (opt && opt.value !== '') {
      const p = opt.getAttribute('data-path');
      label = p != null && p !== '' ? p : (opt.textContent || '').trim();
    }
    inp.value = label;
  });
}

export { initDescField, stripDescField, enhanceDescField };
