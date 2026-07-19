// p23.4 reusable date field. A single boring ES module (rule 12: hand-written, no
// framework, external under script-src 'self', no inline handler) that enhances
// EVERY `input.js-datefield` on any page: forgiving display parse/format, GnuCash
// keyboard shortcuts, and a hand-written calendar popover opened by a button (rule
// 10: never input[type=date]). Loaded once from the shell (base.tmpl) so it applies
// everywhere.
//
// Two layers, mirroring the txn editor's split (trap 2): the PURE date logic below
// (parse/format/shift/shortcut) is exported and node-tested (datefield.test.js); the
// DOM glue + calendar widget at the bottom is guarded so importing under Node is
// side-effect free, and is covered by e2e.
//
// The server (money.ParseDate, p23.3) is the sole authority — this client parse is a
// DISPLAY convenience only; a value it can't parse is left as typed for the server to
// validate/reject.

// --- pure date logic ------------------------------------------------------

// valid builds a {y,m,d} after range- and overflow-checking (Feb 30 -> the JS Date
// round-trips to Mar 2, so the components no longer match -> reject). Returns null on
// an impossible date. NB: a 2-digit year must already be expanded (JS maps years
// 0–99 to 1900–1999, which the round-trip would otherwise reject).
function valid(y, m, d) {
  if (m < 1 || m > 12 || d < 1 || d > 31) return null;
  const dt = new Date(y, m - 1, d);
  if (dt.getFullYear() !== y || dt.getMonth() !== m - 1 || dt.getDate() !== d) return null;
  return { y, m, d };
}

// expandYear widens a written year to four digits, strptime %y: 1–2 digits pivot
// (00–68 -> 2000s, 69–99 -> 1900s); 3–4 digits are taken as written. Mirrors the
// server (money.expandYear, p23.3).
function expandYear(y, field) {
  if (field.length <= 2) return y <= 68 ? 2000 + y : 1900 + y;
  return y;
}

// parseStrict matches only the FULL zero-padded rendering of one format, so US/EU
// full dates are read in their own order (not big-endian). Returns null otherwise.
function parseStrict(str, fmt) {
  let m;
  if (fmt === 'US') {
    m = /^(\d{2})\/(\d{2})\/(\d{4})$/.exec(str);
    return m ? valid(+m[3], +m[1], +m[2]) : null;
  }
  if (fmt === 'EU') {
    m = /^(\d{2})\/(\d{2})\/(\d{4})$/.exec(str);
    return m ? valid(+m[3], +m[2], +m[1]) : null;
  }
  m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(str);
  return m ? valid(+m[1], +m[2], +m[3]) : null;
}

// parseFlexible reads the short/partial numeric forms (p23.3): dash/slash/dot
// separated integers, most-significant first. 3 parts = Y-M-D; 2 parts = M-D with the
// year from `now`. Returns null if not such a form.
function parseFlexible(str, now) {
  const parts = str.split(/[-/.]/);
  if (parts.some((p) => p === '' || !/^\d+$/.test(p))) return null;
  const nums = parts.map(Number);
  if (nums.length === 3) return valid(expandYear(nums[0], parts[0]), nums[1], nums[2]);
  if (nums.length === 2) return valid(now.getFullYear(), nums[0], nums[1]);
  return null;
}

// parseDate is the client mirror of money.ParseDate: the active format's full layout
// first, then full ISO (always accepted), then the flexible short form. `now` (a JS
// Date) supplies an omitted year. Returns {y,m,d} or null.
function parseDate(str, fmt, now) {
  str = (str || '').trim();
  if (!str) return null;
  let d = parseStrict(str, fmt);
  if (d) return d;
  if (fmt !== 'ISO') {
    d = parseStrict(str, 'ISO');
    if (d) return d;
  }
  return parseFlexible(str, now);
}

// formatDate renders {y,m,d} in the active display format.
function formatDate(d, fmt) {
  const y = String(d.y).padStart(4, '0');
  const m = String(d.m).padStart(2, '0');
  const day = String(d.d).padStart(2, '0');
  if (fmt === 'US') return `${m}/${day}/${y}`;
  if (fmt === 'EU') return `${day}/${m}/${y}`;
  return `${y}-${m}-${day}`;
}

function toJS(d) { return new Date(d.y, d.m - 1, d.d); }
function fromJS(dt) { return { y: dt.getFullYear(), m: dt.getMonth() + 1, d: dt.getDate() }; }
function todayOf(now) { return fromJS(now); }

// shiftDay moves n days (n may be negative).
function shiftDay(d, n) {
  const dt = toJS(d);
  dt.setDate(dt.getDate() + n);
  return fromJS(dt);
}

// shiftMonth moves n months, clamping the day to the target month's last day (so
// Jan 31 -> Feb 28, not Mar 3).
function shiftMonth(d, n) {
  const day = d.d;
  const dt = new Date(d.y, d.m - 1 + n, 1);
  const eom = new Date(dt.getFullYear(), dt.getMonth() + 1, 0).getDate();
  dt.setDate(Math.min(day, eom));
  return fromJS(dt);
}

// endOfMonth returns the last day of d's month.
function endOfMonth(d) { return fromJS(new Date(d.y, d.m, 0)); }

// applyShortcut maps a GnuCash-style key to a new formatted value, operating on the
// field's current value (or today when empty/unparsed):
//   [ ] -> previous / next month     - + -> previous / next day     h -> end of month
//   t   -> today
// '=' is an alias for '+' (owner: "make '=' same as '+'") — the unshifted key on the
// same physical cap, so a forward-day shift needs no Shift press.
// It returns the new string, or null when the key is not a shortcut OR must stay
// LITERAL: '-'/'+'/'=' are separators while a partial date is being typed (non-empty
// and not yet parseable), so they only shift a day once the field holds a complete
// date (or is empty). Mirrors the p23.3 server forms so shortcuts and typing agree.
function applyShortcut(key, value, fmt, now) {
  const cur = parseDate(value, fmt, now);
  const base = cur || todayOf(now);
  switch (key) {
    case 't':
      return formatDate(todayOf(now), fmt);
    case '[':
      return formatDate(shiftMonth(base, -1), fmt);
    case ']':
      return formatDate(shiftMonth(base, 1), fmt);
    case 'h':
      return formatDate(endOfMonth(base), fmt);
    case '-':
    case '+':
    case '=': {
      const v = (value || '').trim();
      if (v !== '' && cur === null) return null; // mid-typing -> literal separator
      const forward = key === '+' || key === '='; // '=' aliases '+' (same physical cap)
      return formatDate(shiftDay(base, forward ? 1 : -1), fmt);
    }
    default:
      return null;
  }
}

export {
  parseDate,
  formatDate,
  shiftDay,
  shiftMonth,
  endOfMonth,
  applyShortcut,
  expandYear,
};

// --- DOM glue + calendar widget (guarded for Node) ------------------------

// resolveFmt picks the display format for an input: its own data-date-format, else
// the nearest ancestor carrying one (the txn form does), else the body's (the shell
// stamps the per-user format there), else ISO.
function resolveFmt(input) {
  const own = input.getAttribute('data-date-format');
  if (own) return own;
  const near = input.closest('[data-date-format]');
  if (near && near !== input) return near.getAttribute('data-date-format');
  return (document.body && document.body.dataset.dateFormat) || 'ISO';
}

// calData reads the localized calendar labels the shell stamps on <body> from the
// i18n catalog (rule 9: the strings originate in the catalog, rendered server-side).
function calData() {
  const b = document.body ? document.body.dataset : {};
  return {
    months: (b.calMonths || '').split(',').filter(Boolean),
    weekdays: (b.calWeekdays || '').split(',').filter(Boolean),
    pick: b.calPick || 'Pick a date',
    prev: b.calPrev || 'Previous month',
    next: b.calNext || 'Next month',
    choose: b.calChoose || 'Choose month and year',
    prevYear: b.calPrevYear || 'Previous year',
    nextYear: b.calNextYear || 'Next year',
  };
}

// enhance wires one date input: keyboard shortcuts, a blur-time reformat to the
// canonical display form, and a calendar popover behind a pick button. Idempotent.
function enhance(input) {
  if (input.dataset.dfWired) return;
  // Clone-safe (p28.20): a grid that clones an ALREADY-enhanced row (budgetgrid.js)
  // copies the surrounding `.datefield-wrap` span + pick button + popover DOM (but
  // NOT their listeners — cloneNode drops those, so the copied button is dead) and
  // deletes dfWired to force a re-enhance. Left as-is that would build a SECOND wrap
  // nested in the first -> a duplicate calendar button. So if this input already sits
  // in a wrap, lift it out and drop the stale wrap first, then build exactly one
  // fresh wrap/button/popover below. Invariant: one input -> one wrap -> one button,
  // however many times enhance runs or the row is cloned.
  const stale = input.closest('.datefield-wrap');
  if (stale && stale.parentNode) {
    stale.parentNode.insertBefore(input, stale);
    stale.remove();
  }
  input.dataset.dfWired = '1';
  const fmt = resolveFmt(input);
  const labels = calData();

  // Keyboard shortcuts (skip when a modifier is held so browser/grid combos pass).
  input.addEventListener('keydown', (evt) => {
    if (evt.ctrlKey || evt.metaKey || evt.altKey) return;
    const next = applyShortcut(evt.key, input.value, fmt, new Date());
    if (next !== null) {
      evt.preventDefault();
      input.value = next;
      if (typeof input.select === 'function') input.select();
      input.dispatchEvent(new Event('input', { bubbles: true }));
    }
  });

  // p29.3 select-all on focus, so the whole value can be replaced by typing.
  // GOTCHA: a plain focus->select() is undone by the click's mouseup, which
  // collapses the selection to the caret — so select-all would work on Tab-in but
  // NOT on a mouse click. A one-shot flag armed on focus lets us preventDefault the
  // FIRST mouseup after focus (keeping the whole-value selection), then disarm so
  // later clicks inside the field position the caret normally.
  let selectArmed = false;
  input.addEventListener('focus', () => {
    selectArmed = true;
    if (typeof input.select === 'function') input.select();
  });
  input.addEventListener('mouseup', (evt) => {
    if (!selectArmed) return;
    selectArmed = false;
    evt.preventDefault(); // don't let this click's mouseup collapse the select-all
  });

  // Reformat a parseable value to the canonical display form on blur (so "6-1"
  // becomes the full date); leave an unparseable value for the server to reject.
  input.addEventListener('blur', () => {
    selectArmed = false;
    const d = parseDate(input.value, fmt, new Date());
    if (d) input.value = formatDate(d, fmt);
  });

  // Wrap the input so the popover can anchor to it; add the pick button.
  const wrap = document.createElement('span');
  wrap.className = 'datefield-wrap';
  input.parentNode.insertBefore(wrap, input);
  wrap.appendChild(input);

  const btn = document.createElement('button');
  btn.type = 'button';
  btn.className = 'datefield-pick';
  btn.setAttribute('aria-label', labels.pick);
  btn.setAttribute('aria-haspopup', 'dialog');
  btn.textContent = '\u{1F4C5}'; // 📅 — an icon glyph, not translatable text
  wrap.appendChild(btn);

  const pop = document.createElement('div');
  pop.className = 'datefield-popover';
  pop.setAttribute('role', 'dialog');
  pop.hidden = true;
  wrap.appendChild(pop);

  let view = null; // {y,m} of the displayed month
  let mode = 'day'; // 'day' grid or 'month' (the p29.2 month+year navigator)

  function close() {
    pop.hidden = true;
    btn.setAttribute('aria-expanded', 'false');
  }
  function open() {
    const cur = parseDate(input.value, fmt, new Date()) || todayOf(new Date());
    view = { y: cur.y, m: cur.m };
    mode = 'day';
    render(cur);
    pop.hidden = false;
    btn.setAttribute('aria-expanded', 'true');
    const sel = pop.querySelector('.datefield-day[aria-current="date"]') || pop.querySelector('.datefield-day');
    if (sel) sel.focus();
  }

  // render draws the popover for the current `mode`: the day grid, or the p29.2
  // month+year navigator. `selected` is the currently-picked date (highlighted in the
  // day view; its month is highlighted in the month view).
  function render(selected) {
    if (mode === 'month') { renderMonthView(selected); return; }
    renderDayView(selected);
  }

  function renderDayView(selected) {
    pop.textContent = '';
    // Header: ‹ prev month, a "Month Year" BUTTON (opens the navigator), next month ›.
    const head = document.createElement('div');
    head.className = 'datefield-head';
    const prev = navButton('‹', labels.prev, () => { view = shiftMonth({ y: view.y, m: view.m, d: 1 }, -1); render(selected); focusGrid(); });
    const next = navButton('›', labels.next, () => { view = shiftMonth({ y: view.y, m: view.m, d: 1 }, 1); render(selected); focusGrid(); });
    const title = document.createElement('button');
    title.type = 'button';
    title.className = 'datefield-title';
    title.setAttribute('aria-label', labels.choose);
    const monthName = labels.months[view.m - 1] || String(view.m);
    title.textContent = `${monthName} ${view.y}`;
    title.addEventListener('click', () => { mode = 'month'; render(selected); focusMonthGrid(); });
    head.append(prev, title, next);
    pop.appendChild(head);

    // Weekday header row + day grid (Sunday-first, matching weekdays list order).
    const grid = document.createElement('div');
    grid.className = 'datefield-grid';
    labels.weekdays.forEach((w) => {
      const wd = document.createElement('span');
      wd.className = 'datefield-weekday';
      wd.textContent = w;
      grid.appendChild(wd);
    });
    const first = new Date(view.y, view.m - 1, 1);
    const lead = first.getDay(); // 0=Sun
    const days = new Date(view.y, view.m, 0).getDate();
    for (let i = 0; i < lead; i++) {
      const blank = document.createElement('span');
      blank.className = 'datefield-blank';
      grid.appendChild(blank);
    }
    for (let day = 1; day <= days; day++) {
      const b = document.createElement('button');
      b.type = 'button';
      b.className = 'datefield-day';
      b.textContent = String(day);
      b.dataset.day = String(day);
      if (selected && selected.y === view.y && selected.m === view.m && selected.d === day) {
        b.setAttribute('aria-current', 'date');
      }
      b.addEventListener('click', () => {
        input.value = formatDate({ y: view.y, m: view.m, d: day }, fmt);
        input.dispatchEvent(new Event('input', { bubbles: true }));
        close();
        input.focus();
      });
      grid.appendChild(b);
    }
    pop.appendChild(grid);
  }

  function focusGrid() {
    const d = pop.querySelector('.datefield-day');
    if (d) d.focus();
  }

  // renderMonthView draws the p29.2 month+year navigator: a ‹ year › header plus a
  // grid of the 12 localized month names. Picking a month returns to the day grid on
  // that month/year (via shiftMonth to keep the day-clamp consistent).
  function renderMonthView(selected) {
    pop.textContent = '';
    const head = document.createElement('div');
    head.className = 'datefield-head';
    const prev = navButton('‹', labels.prevYear, () => { view = shiftMonth({ y: view.y, m: view.m, d: 1 }, -12); render(selected); focusMonthGrid(); });
    const next = navButton('›', labels.nextYear, () => { view = shiftMonth({ y: view.y, m: view.m, d: 1 }, 12); render(selected); focusMonthGrid(); });
    const title = document.createElement('span');
    title.className = 'datefield-title';
    title.textContent = String(view.y);
    head.append(prev, title, next);
    pop.appendChild(head);

    const grid = document.createElement('div');
    grid.className = 'datefield-months';
    for (let m = 1; m <= 12; m++) {
      const b = document.createElement('button');
      b.type = 'button';
      b.className = 'datefield-month';
      b.textContent = labels.months[m - 1] || String(m);
      b.dataset.month = String(m);
      if (view.m === m) b.setAttribute('aria-current', 'true');
      b.addEventListener('click', () => pickMonth(m, selected));
      grid.appendChild(b);
    }
    pop.appendChild(grid);
  }

  // pickMonth moves `view` to month m of the displayed year, switches back to the day
  // grid, and re-renders there (shiftMonth from the current view keeps the day-clamp).
  function pickMonth(m, selected) {
    view = shiftMonth({ y: view.y, m: view.m, d: 1 }, m - view.m);
    view = { y: view.y, m: view.m };
    mode = 'day';
    render(selected);
    focusGrid();
  }

  function focusMonthGrid() {
    const cur = pop.querySelector('.datefield-month[aria-current="true"]') || pop.querySelector('.datefield-month');
    if (cur) cur.focus();
  }

  function navButton(glyph, label, onClick) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'datefield-nav';
    b.setAttribute('aria-label', label);
    b.textContent = glyph;
    b.addEventListener('click', onClick);
    return b;
  }

  btn.addEventListener('click', () => (pop.hidden ? open() : close()));

  // NB: "click outside closes" is NOT wired per-field (that would leak one
  // document listener per enhanced input, accumulating on every htmx:afterSwap
  // that re-creates #txn-date). A single delegated document listener at module
  // scope (below) closes any open popover instead.

  // Arrow-key navigation across the day grid + Escape to close.
  pop.addEventListener('keydown', (evt) => {
    if (evt.key === 'Escape') {
      close();
      btn.focus();
      return;
    }
    // Month navigator (p29.2): arrows walk the 12-month grid (3 cols), Enter picks.
    const monthCell = evt.target.closest('.datefield-month');
    if (monthCell) {
      if (evt.key === 'Enter' || evt.key === ' ') return; // native button activation picks
      const mDelta = { ArrowLeft: -1, ArrowRight: 1, ArrowUp: -3, ArrowDown: 3 }[evt.key];
      if (mDelta === undefined) return;
      evt.preventDefault();
      const want = Number(monthCell.dataset.month) + mDelta;
      if (want < 1 || want > 12) return; // stay within the year (the ‹ year › nav steps)
      const cell = pop.querySelector(`.datefield-month[data-month="${want}"]`);
      if (cell) cell.focus();
      return;
    }
    const day = evt.target.closest('.datefield-day');
    if (!day) return;
    const delta = { ArrowLeft: -1, ArrowRight: 1, ArrowUp: -7, ArrowDown: 7 }[evt.key];
    if (delta === undefined) return;
    evt.preventDefault();
    const target = shiftDay({ y: view.y, m: view.m, d: Number(day.dataset.day) }, delta);
    if (target.y !== view.y || target.m !== view.m) {
      view = { y: target.y, m: target.m };
      render(target);
    }
    const want = pop.querySelector(`.datefield-day[data-day="${target.d}"]`);
    if (want) want.focus();
  });

}

// closeOnOutsideClick is the SINGLE delegated document listener (registered once, in
// the init block) that closes any open date popover when the click lands outside its
// wrapper — replacing the per-field listener that would leak on every htmx swap.
function closeOnOutsideClick(evt) {
  // p29.1: an in-popover control that RE-RENDERS the popover (‹/› month nav, the
  // p29.2 month/year title) empties `pop` (render() does `pop.textContent = ''`),
  // detaching the very button that was clicked BEFORE this bubbled listener runs.
  // Its `evt.target` is then a disconnected node, so `!wrap.contains(evt.target)`
  // would wrongly read as an outside click and close the popover (the month-nav
  // bug: the month re-rendered and instantly closed). A click whose target has left
  // the document was consumed by such a control — treat it as in-popover, not
  // outside, and do NOT close. One guard covers every re-rendering control.
  if (evt.target && !evt.target.isConnected) return;
  document.querySelectorAll('.datefield-popover:not([hidden])').forEach((pop) => {
    const wrap = pop.closest('.datefield-wrap');
    if (wrap && !wrap.contains(evt.target)) {
      pop.hidden = true;
      const btn = wrap.querySelector('.datefield-pick');
      if (btn) btn.setAttribute('aria-expanded', 'false');
    }
  });
}

function enhanceAll(root) {
  (root || document).querySelectorAll('input.js-datefield').forEach(enhance);
}

if (typeof document !== 'undefined' && typeof document.addEventListener === 'function') {
  document.addEventListener('DOMContentLoaded', () => enhanceAll());
  // Re-scan after htmx swaps (the txn form re-render, filter swaps, etc.).
  document.addEventListener('htmx:afterSwap', () => enhanceAll());
  // ONE document-level "click outside closes" listener for all popovers.
  document.addEventListener('click', closeOnOutsideClick);
}
