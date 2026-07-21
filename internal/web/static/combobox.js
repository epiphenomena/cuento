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
//   - resyncCombos(root): after ANY code sets select.value directly (no `change` event),
//     refresh each enhanced overlay's visible text from its select's selection. Used by
//     apply-fund-to-all, gateRow's program prefill, and swap-row (p26.3). freeText combos
//     keep their typed text (they own their input via onInput).
//
// p26.3 GENERALIZATION (so payee + the p26.4 expense grid reuse the SAME widget):
//   enhance(select, opts) takes an optional opts:
//     - allowFreeText: the overlay is a real tab stop (the select drops out of tab order)
//       and a typed value that matches no option is KEPT (not reset to the selection) on
//       blur/close. This is the payee case: a typed brand-new name must survive.
//     - onInput(text): called on every keystroke (payee mirrors the typed text into the
//       hidden payee_name field + resets the id sink to 0).
//     - onPick({value, label}): called after a real option is picked and select.value is
//       set + `change` dispatched (payee autofills the grid from the chosen payee).
//   The account/fund/program call sites pass NO opts and behave exactly as p26.2.
//
// Guarded so importing under Node is side-effect free (no `document`), like txneditor.js.

import { rankOptions } from './combofilter.js';
import { comboKeyAction } from './combokey.js';

// optionLabel: the display label for an <option> -- its data-path (the dotted ancestor
// path from p26.1) if present, else its trimmed text.
function optionLabel(opt) {
  const p = opt.getAttribute('data-path');
  return (p != null && p !== '') ? p : (opt.textContent || '').trim();
}

// collectOptions returns the pickable options as { label, value, el }. p26.4 fix: the
// value="0" option ("Unrestricted" / "None" / account-none) IS a real, selectable choice
// -- keeping it in the list lets a user who picked a real option re-offer and re-pick the
// reset-to-none entry. Only the empty-value ("") placeholder (if any) is excluded, since
// that is not a meaningful selection. Picking the 0 option resets the select to its cleared
// state (currentLabel() renders it blank -- an empty box == cleared, which is correct).
function collectOptions(select) {
  return [...select.options]
    .filter((o) => o.value !== '')
    .map((o) => ({ label: optionLabel(o), value: o.value, el: o }));
}

function enhance(select, opts) {
  if (!select || select.dataset.combo === '1') return;
  select.dataset.combo = '1';
  const freeText = !!(opts && opts.allowFreeText);
  const onInputCb = opts && typeof opts.onInput === 'function' ? opts.onInput : null;
  const onPickCb = opts && typeof opts.onPick === 'function' ? opts.onPick : null;
  // p28.3: onAdvance(select) moves focus to the NEXT field after an Enter-pick from an
  // OPEN highlighted list. The entry grids pass their tested next-cell mover (which skips
  // visibility:hidden cells); non-grid combos pass nothing and simply commit without a
  // programmatic advance (Enter with the list open still commits; there is just no "next
  // field" to jump to). Tab never uses this -- native Tab does its own advancing.
  const onAdvanceCb = opts && typeof opts.onAdvance === 'function' ? opts.onAdvance : null;

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
  // An optional data-placeholder on the select (the report PROGRAM filter's
  // "— all programs —") is mirrored onto the overlay so a blank box (the empty == all
  // default) still names its meaning. Purely cosmetic; other combos set no placeholder.
  if (select.dataset.placeholder) input.placeholder = select.dataset.placeholder;
  // data-empty-value scopes "a cleared/blank input means THIS option" to the select that
  // opts in (the report program filter: blank == all programs, value 0). Absent on the
  // fund/account/payee combos, so their "empty reverts to the current selection" behavior
  // is unchanged.
  const emptyValue = select.dataset.emptyValue;
  // Tab order: by default the native <select> underneath is the tab stop (keeps the
  // grid's Tab order account->amount and the keyboard e2e intact); the overlay is
  // reachable by click/focus only. For a freeText combo (payee) the overlay IS the
  // tab stop -- a keyboard user must be able to type a brand-new value -- so the select
  // drops out of tab order (it stays the value sink + no-JS fallback).
  if (freeText) {
    input.tabIndex = 0;
    select.tabIndex = -1;
  } else {
    input.tabIndex = -1;
  }

  const list = document.createElement('ul');
  list.className = 'combo-list';
  list.setAttribute('role', 'listbox');
  list.hidden = true;

  wrap.appendChild(input);
  wrap.appendChild(list);

  let items = []; // current filtered [{label,value}], in listbox order
  let active = -1; // active item index within `items`
  let blurTimer = null; // pending deferred close (blur), cancelled on re-focus

  function currentLabel() {
    const opt = select.selectedOptions[0];
    // value="0" is a REAL selectable default (fund "Unrestricted", program "None",
    // account "Choose account") -- show its label so the box isn't blank. Only the
    // empty-value placeholder (value="") is treated as "no label". Focus always opens
    // the list with an EMPTY query (not input.value), so showing a label here never
    // turns into a stray filter.
    if (!opt || opt.value === '') return '';
    return optionLabel(opt);
  }

  function syncInputToSelection() {
    // freeText combos own their input text (a typed brand-new value must survive a
    // blur/close even though the select is still 0). Only overwrite the box from the
    // selection when there IS a real selection; otherwise leave the typed text alone.
    if (freeText) {
      const lbl = currentLabel();
      if (lbl !== '') input.value = lbl;
      return;
    }
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
    // The picked option's label (before syncInputToSelection reconciles the box).
    const opt = select.selectedOptions[0];
    const label = opt ? optionLabel(opt) : '';
    input.value = label; // real pick -> show the chosen label (freeText or not)
    close();
    // Bubble so the grid-delegated change listener (auto-append) AND the per-row change
    // listener (gating + subsidiary marking) both fire.
    select.dispatchEvent(new Event('change', { bubbles: true }));
    if (onPickCb) onPickCb({ value, label });
  }

  // Typing filters/reorders. An empty box shows the full list in original order.
  input.addEventListener('input', () => {
    open(input.value);
    if (onInputCb) onInputCb(input.value);
  });

  // Opening on focus/click shows the full list so the control feels like a dropdown.
  input.addEventListener('focus', () => {
    // Cancel any pending blur-close: a re-focus within the blur's 120ms window (a real
    // user tabbing back to the cell) would otherwise let the stale timer fire AFTER this
    // focus, closing the just-opened list AND rewriting input.value from the selection --
    // which reads as "the box won't let me type" (my keystrokes keep getting wiped) and
    // "re-focus doesn't reopen the list". Clearing it here makes focus authoritative.
    if (blurTimer) { clearTimeout(blurTimer); blurTimer = null; }
    syncInputToSelection();
    // Select-on-focus: highlight the current label so typing immediately REPLACES it and
    // starts a fresh fuzzy search (rather than appending to / editing within the old text).
    // Runs AFTER syncInputToSelection has written the box, so the selection covers the label
    // that is actually shown. Harmless for the p26.44 keyboard bridge (it focuses THEN
    // overwrites input.value and moves the caret to the end) and for the txn-grid combos
    // (txneditor.js's form-level focusin already select()s them -- a second select() is a
    // no-op). freeText combos also benefit: a typed-new value is selected so a re-type
    // replaces it cleanly.
    if (typeof input.select === 'function') input.select();
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
    } else if (evt.key === 'Enter' || evt.key === 'Tab') {
      // p28.3: unified select-and-advance. When the list is OPEN with a highlighted item,
      // BOTH Enter and Tab COMMIT that item; Enter also advances focus (and preventDefaults
      // so it neither submits nor bubbles to a grid Enter=save), while Tab lets NATIVE Tab
      // advance (no preventDefault -- committing first is enough). When the list is closed
      // / nothing highlighted, neither key is special: Enter/Tab fall through to native
      // (a closed-list Enter still reaches the grid's save handler).
      const open = !list.hidden && items.length > 0;
      const { commit, preventDefault, focusNext } = comboKeyAction(evt.key, {
        open,
        hasActive: active >= 0 && !!items[active],
      });
      if (commit) {
        if (preventDefault) evt.preventDefault();
        pick(items[active].value); // pick() closes the list + sets the selection
        // Enter: jump to the next field. Tab: skip (native Tab already advances -- and the
        // pick's input.value write means the deferred blur reconcile keeps the committed
        // label, so Tab no longer reverts the highlight to the old selection).
        if (focusNext) {
          // p28.5: the entry grids pass onAdvance (skip-hidden next-cell). Any other site
          // (the txn HEADER main-account, the expense/budget grids, a plain merge/report
          // picker) has none -> fall back to focusing the next tabbable after the select,
          // mirroring what native Tab would do, so Enter advances everywhere Tab does.
          if (onAdvanceCb) onAdvanceCb(select);
          else focusNextTabbable(select);
        }
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
    // The timer id is kept so a re-focus within the window can cancel it (see `focus`).
    if (blurTimer) clearTimeout(blurTimer);
    blurTimer = setTimeout(() => {
      blurTimer = null;
      close();
      // empty == the opt-in "empty value" (report program filter: blank -> program 0 ==
      // all). A cleared box does NOT move select.value on its own, so without this a
      // click-away would REVERT the box to the old selection's label (and never post 0).
      // Reset the select here and fire `change` so the report's form (hx-trigger="change")
      // reloads at "all". Gated on data-empty-value + non-freeText, so only the program
      // filter opts in. syncInputToSelection then leaves the box blank (value-0 has no label).
      if (!freeText && emptyValue != null && input.value.trim() === '' && select.value !== emptyValue) {
        select.value = emptyValue;
        select.dispatchEvent(new Event('change', { bubbles: true }));
      }
      syncInputToSelection();
    }, 120);
  });

  // p26.44: KEYBOARD ENTRY bridge. For a non-freeText combo the native <select> is the Tab
  // stop (p26.11: the overlay is tabIndex=-1, reachable by click/focus only) -- so a user who
  // TABS to the account/fund/program cell and starts TYPING hits the browser's native <select>
  // prefix-typeahead (a raw first-letter jump), NOT this fuzzy overlay: no ranked listbox ever
  // opens ("the account field isn't supporting fuzzy matching"). Bridge it: a PRINTABLE key on
  // the focused select is redirected to the overlay -- prevent the native typeahead, move focus
  // to the overlay input, seed the char, and open the filtered list. Only bare single-character
  // keys (no Ctrl/Meta/Alt) are intercepted, so Tab/Enter/Escape/Arrows (grid nav, the store's
  // keyboard model in txngrid.js) and programmatic select-focus/selectOption are untouched.
  if (!freeText) {
    select.addEventListener('keydown', (evt) => {
      if (evt.ctrlKey || evt.metaKey || evt.altKey) return;
      if (evt.key == null || evt.key.length !== 1) return; // printable single char only
      evt.preventDefault(); // suppress the native <select> typeahead jump
      // Focus FIRST (a focusin handler may select() the input's contents), THEN seed the
      // char and move the caret to the end -- so the next natively-typed char appends rather
      // than replacing a selected seed (the select-on-focus wipe that dropped the first key).
      input.focus();
      input.value = evt.key;
      if (typeof input.setSelectionRange === 'function') {
        input.setSelectionRange(input.value.length, input.value.length);
      }
      open(input.value);
      if (onInputCb) onInputCb(input.value);
    });
  }

  // If some other code (keyboard e2e, no-JS fallback path, another module) changes the
  // native select, keep the input label in sync.
  select.addEventListener('change', syncInputToSelection);

  syncInputToSelection();
  // Init seeding (freeText): on a 422 re-render of a typed-new value the select is 0 but
  // the caller holds the typed text (opts.initialText) -> show it so the box isn't blank.
  if (freeText && input.value === '' && opts && opts.initialText) {
    input.value = opts.initialText;
  }
}

// focusNextTabbable (p28.5) moves focus to the next tab-order element AFTER `from`,
// mirroring what native Tab would do -- used as the generic Enter-advance when a combo has
// no site-specific onAdvance (the txn header main-account, the expense/budget grids, a
// plain merge/report picker). It walks the document's tabbable elements in DOM order and
// focuses the first one positioned after `from`, skipping tabIndex<0, disabled, and
// display/visibility-hidden nodes (the combo overlay input is tabIndex=-1, so it is skipped
// -- Enter lands on the SAME cell native Tab from the select would). No-op if none follows.
function focusNextTabbable(from) {
  const doc = from && from.ownerDocument;
  if (!doc || typeof doc.querySelectorAll !== 'function') return;
  const sel = 'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]';
  const all = [...doc.querySelectorAll(sel)].filter((el) => {
    if (el === from) return true; // keep the anchor so we can find its position
    if (el.tabIndex < 0) return false;
    if (el.disabled) return false;
    const view = doc.defaultView;
    if (view && typeof view.getComputedStyle === 'function') {
      const cs = view.getComputedStyle(el);
      if (cs.visibility === 'hidden' || cs.display === 'none') return false;
    }
    return true;
  });
  const idx = all.indexOf(from);
  if (idx < 0 || idx + 1 >= all.length) return;
  const next = all[idx + 1];
  if (next && typeof next.focus === 'function') next.focus();
}

// initCombos enhances every not-yet-enhanced combo select under `root` (idempotent).
// Plain (no-opts) enhancement -- account/fund/program. The payee combo is enhanced
// separately by txneditor.js with its freeText opts, so it is skipped here via a
// data-combo-manual marker.
function initCombos(root, opts) {
  const scope = root || (typeof document !== 'undefined' ? document : null);
  if (!scope || typeof scope.querySelectorAll !== 'function') return;
  scope
    .querySelectorAll('select.combo-input:not([data-combo]):not([data-combo-manual])')
    .forEach((sel) => enhance(sel, opts));
}

// resyncCombos refreshes each enhanced overlay's visible text from its select's current
// selection. Call after ANY code sets select.value WITHOUT dispatching `change` (setting
// .value fires no event, and the overlay only auto-syncs on `change`): apply-fund-to-all,
// gateRow's program prefill, swap-row. freeText combos are skipped (they own their text).
function resyncCombos(root) {
  const scope = root || (typeof document !== 'undefined' ? document : null);
  if (!scope || typeof scope.querySelectorAll !== 'function') return;
  scope.querySelectorAll('.combo').forEach((wrap) => {
    const select = wrap.querySelector('select.combo-input');
    const input = wrap.querySelector('.combo-text');
    if (!select || !input || select.dataset.comboManual === '1') return;
    // Mirror currentLabel (p26.22): value="0" is a REAL selectable default (fund
    // "Unrestricted", program "None", account "Choose account"), so show its label -- only
    // the empty-value placeholder (value="") is blank. Blanking value-0 here re-hid the fund
    // "Unrestricted" that syncInputToSelection had set, because gateRow calls resyncCombos on
    // load (the reported "fund does not display Unrestricted").
    const opt = select.selectedOptions[0];
    input.value = opt && opt.value !== '' ? optionLabel(opt) : '';
  });
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

export { enhance, initCombos, stripCombo, resyncCombos };
