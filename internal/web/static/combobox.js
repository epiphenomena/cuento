// p26.2 fuzzy-filter combobox -- DOM GLUE (rule 12: hand-written external ES module,
// no framework, no inline handler, script-src 'self'). The PURE ranking lives in
// combofilter.js (node-tested); this is the thin, e2e-covered shim that turns a native
// <select class="combo-input"> into a type-to-filter autocomplete.
//
// CONTRACT (so p26.3/p26.4 can reuse it for fund/program/payee + the expense grid):
//   - The native <select> STAYS the value sink and the no-JS fallback. It is never
//     hidden with display:none/visibility:hidden (that would break keyboard tests and
//     Playwright selectOption) -- the overlay input sits ON TOP of it, and the select
//     stays laid out + operable underneath.
//   - enhance(select): wraps the select in a .combo wrapper (sibling DOM, NOT inside the
//     option list), builds an overlay input + listbox, marks select[data-combo="1"].
//     Option labels come from each <option>'s data-path (else its text); the picked
//     value is the <option> value. Picking sets select.value and fires a bubbling
//     `change` on the select so existing per-row + delegated grid listeners run.
//   - initCombos(root): enhance every select.combo-input:not([data-combo]) under root.
//     Idempotent (the :not([data-combo]) guard), so it is safe to call on load, after an
//     htmx swap, and after a row clone.
//   - stripCombo(root): for every wrapped select under root, unwrap it (remove the
//     overlay) and clear data-combo, leaving a clean native select. Call this on a
//     freshly cloned row BEFORE re-indexing so the clone (whose overlay listeners are
//     dead -- cloneNode does not copy listeners) is rebuilt from its clean select.
//   - The <option> nodes are NEVER moved out of the select; the listbox is separate DOM
//     that references each option's value/label. So readers of selectedOptions[0].dataset
//     (program/class gating) and option[data-account-option] (subsidiary marking) are
//     undisturbed.
//
// Guarded so importing under Node is side-effect free (no `document`), like txneditor.js.

import { rankOptions } from './combofilter.js';

// optionLabel: the display label for an <option> -- its data-path (the dotted ancestor
// path from p26.1) if present, else its trimmed text.
function optionLabel(opt) {
  const p = opt.getAttribute('data-path');
  return (p != null && p !== '') ? p : (opt.textContent || '').trim();
}

// collectOptions returns the pickable options as { label, value, el } (skipping the
// placeholder "0"/"" option so the filtered list shows only real choices; the placeholder
// stays the select's cleared state).
function collectOptions(select) {
  return [...select.options]
    .filter((o) => o.value !== '' && o.value !== '0')
    .map((o) => ({ label: optionLabel(o), value: o.value, el: o }));
}

function enhance(select) {
  if (!select || select.dataset.combo === '1') return;
  select.dataset.combo = '1';

  // Wrapper is a sibling container the overlay lives in; the select is moved inside it so
  // the input can be absolutely positioned over the select's box.
  const wrap = document.createElement('div');
  wrap.className = 'combo';
  select.parentNode.insertBefore(wrap, select);
  wrap.appendChild(select);

  const input = document.createElement('input');
  input.type = 'text';
  input.className = 'combo-text';
  input.autocomplete = 'off';
  input.setAttribute('role', 'combobox');
  input.setAttribute('aria-autocomplete', 'list');
  input.setAttribute('aria-expanded', 'false');
  // Not a sequential tab stop: the native <select> underneath is the tab stop (keeps the
  // grid's Tab order account->amount and the keyboard e2e intact). Clicking or focusing
  // the select still reveals/uses the input via the wiring below.
  input.tabIndex = -1;

  const list = document.createElement('ul');
  list.className = 'combo-list';
  list.setAttribute('role', 'listbox');
  list.hidden = true;

  wrap.appendChild(input);
  wrap.appendChild(list);

  let items = []; // current filtered [{label,value}], in listbox order
  let active = -1; // active item index within `items`

  function currentLabel() {
    const opt = select.selectedOptions[0];
    if (!opt || opt.value === '' || opt.value === '0') return '';
    return optionLabel(opt);
  }

  function syncInputToSelection() {
    input.value = currentLabel();
  }

  function close() {
    list.hidden = true;
    list.textContent = '';
    input.setAttribute('aria-expanded', 'false');
    active = -1;
    items = [];
  }

  function renderList() {
    list.textContent = '';
    items.forEach((it, idx) => {
      const li = document.createElement('li');
      li.className = 'combo-option';
      li.setAttribute('role', 'option');
      li.dataset.value = it.value;
      li.textContent = it.label;
      if (idx === active) {
        li.classList.add('active');
        li.setAttribute('aria-selected', 'true');
      }
      list.appendChild(li);
    });
    list.hidden = items.length === 0;
    input.setAttribute('aria-expanded', items.length > 0 ? 'true' : 'false');
  }

  function open(query) {
    const all = collectOptions(select);
    items = rankOptions(query, all).map((o) => ({ label: o.label, value: o.value }));
    active = items.length > 0 ? 0 : -1;
    renderList();
  }

  function pick(value) {
    select.value = value;
    syncInputToSelection();
    close();
    // Bubble so the grid-delegated change listener (auto-append) AND the per-row change
    // listener (gating + subsidiary marking) both fire.
    select.dispatchEvent(new Event('change', { bubbles: true }));
  }

  // Typing filters/reorders. An empty box shows the full list in original order.
  input.addEventListener('input', () => open(input.value));

  // Opening on focus/click shows the full list so the control feels like a dropdown.
  input.addEventListener('focus', () => {
    syncInputToSelection();
    open('');
  });

  input.addEventListener('keydown', (evt) => {
    if (evt.key === 'ArrowDown') {
      evt.preventDefault();
      if (list.hidden) { open(input.value); return; }
      if (items.length) { active = (active + 1) % items.length; renderList(); }
    } else if (evt.key === 'ArrowUp') {
      evt.preventDefault();
      if (items.length) { active = (active - 1 + items.length) % items.length; renderList(); }
    } else if (evt.key === 'Enter') {
      if (!list.hidden && active >= 0 && items[active]) {
        evt.preventDefault();
        pick(items[active].value);
      }
    } else if (evt.key === 'Escape') {
      if (!list.hidden) { evt.preventDefault(); close(); syncInputToSelection(); }
    }
  });

  list.addEventListener('mousedown', (evt) => {
    // mousedown (not click) so the pick happens before the input's blur closes the list.
    const li = evt.target.closest('.combo-option');
    if (!li) return;
    evt.preventDefault();
    pick(li.dataset.value);
  });

  input.addEventListener('blur', () => {
    // Defer so a list mousedown can win; then reconcile the input text to the selection.
    setTimeout(() => { close(); syncInputToSelection(); }, 120);
  });

  // If some other code (keyboard e2e, no-JS fallback path, another module) changes the
  // native select, keep the input label in sync.
  select.addEventListener('change', syncInputToSelection);

  syncInputToSelection();
}

// initCombos enhances every not-yet-enhanced combo select under `root` (idempotent).
function initCombos(root) {
  const scope = root || (typeof document !== 'undefined' ? document : null);
  if (!scope || typeof scope.querySelectorAll !== 'function') return;
  scope.querySelectorAll('select.combo-input:not([data-combo])').forEach(enhance);
}

// stripCombo unwraps any enhanced combo under `root`, restoring a clean native select and
// clearing data-combo so initCombos can re-enhance it. Used before a cloned row is
// re-indexed so a stale (dead-listener) overlay never rides along in the clone.
function stripCombo(root) {
  if (!root || typeof root.querySelectorAll !== 'function') return;
  // A cloned select still has data-combo="1" and sits inside a cloned .combo wrapper.
  const wraps = [];
  if (root.classList && root.classList.contains('combo')) wraps.push(root);
  root.querySelectorAll('.combo').forEach((w) => wraps.push(w));
  wraps.forEach((wrap) => {
    const select = wrap.querySelector('select.combo-input');
    if (!select) { wrap.remove(); return; }
    delete select.dataset.combo;
    wrap.parentNode.insertBefore(select, wrap);
    wrap.remove();
  });
  // Defensive: also clear the flag on any wrapper-less enhanced select (shouldn't happen).
  root.querySelectorAll('select.combo-input[data-combo]').forEach((s) => {
    delete s.dataset.combo;
  });
}

export { enhance, initCombos, stripCombo };
