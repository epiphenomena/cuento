// p31 10b PROGRAM-STATEMENT column-group collapse -- a CSP-safe ES module (rule 12:
// external, no inline handler, script-src 'self'). 10a rebuilt the program statement as an
// account-ROW x (functional-class/program)-COLUMN matrix; the program columns form a tree
// (programs.parent_id), and each PARENT program column already shows the rolled-up total of
// its descendants. This module lets the user CLICK a parent program's column header to hide
// its descendant program columns, leaving just the parent's rollup -- and click again to
// expand. No server round-trip: a pure display enhancement.
//
// ============================ DOM CONTRACT ============================
// The report template (10a) stamps FIXED-name data-* attributes the module reads:
//   - every program column's leaf header  <th data-program="ID">        (and every body
//     <td data-program="ID"> in that column position, tagged in 10b)
//   - a CHILD column's header carries       data-program-parent="PID"
//   - a PARENT program's SPAN cell (has children) carries data-program-group="1" -> collapsible;
//     it spans its subtree's columns (colspan=N) and its colspan must shrink as descendants hide.
//     (A parent appears TWICE in the nested header: this subtree SPAN cell, and its own leaf
//     data-column cell one row below -- both carry data-program so both hide with the subtree.)
//
// Model (mirrors treetable.js exactly). A single "collapsed" SET of program ids. A column
// (program id X) is HIDDEN iff some STRICT ancestor of X -- walking data-program-parent up
// the tree -- is in the collapsed set. Strict, so a clicked parent never hides ITSELF (its
// rollup stays); nested collapse composes for free (collapse General + UPH, expand General
// -> UPH shows its rollup but its own children stay hidden under UPH's own collapsed state).
//
// The PURE decisions (buildParentMap, ancestors, columnHidden, toggle, visibleCount) are
// unit-tested (colcollapse.test.js). The DOM wiring (hiding cells, shrinking the group
// colspan, the disclosure affordance) is e2e-covered. Guarded so importing under Node is
// side-effect free (no `document`).

// buildParentMap turns a list of {program, parent} column descriptors into a Map
// program-id -> parent-id (parent absent/"" for a root). The program-column tree the hidden
// decision walks. Ids are kept as their raw string form (they come from the DOM verbatim).
export function buildParentMap(cols) {
  const parent = new Map();
  for (const c of cols) {
    if (c.program == null || c.program === '') continue;
    parent.set(c.program, c.parent && c.parent !== '' ? c.parent : null);
  }
  return parent;
}

// ancestors returns the STRICT ancestor chain of program id (its parent, grandparent, ...)
// walking parentMap upward, EXCLUDING id itself. A cycle or a missing link terminates the
// walk (defensive: the data is a real tree, but never loop forever). Order: nearest first.
export function ancestors(parentMap, id) {
  const out = [];
  const seen = new Set([id]);
  let p = parentMap.get(id) || null;
  while (p != null && !seen.has(p)) {
    out.push(p);
    seen.add(p);
    p = parentMap.get(p) || null;
  }
  return out;
}

// columnHidden reports whether the column for program id is hidden: some STRICT ancestor of
// it is in the collapsed set. Strict (the clicked parent itself is never hidden -- it keeps
// showing its rollup), matching treetable's rowHidden (which requires the collapsed row to
// PRECEDE its descendants). A program not in the map (unknown id) is never hidden.
export function columnHidden(parentMap, collapsed, id) {
  for (const a of ancestors(parentMap, id)) {
    if (collapsed.has(a)) return true;
  }
  return false;
}

// toggle flips id's membership in the collapsed set, returning a NEW set (pure -- the caller
// swaps it in). Collapsing a parent hides its whole subtree; a descendant already collapsed
// keeps its own state, so expanding this parent later reveals its DIRECT children while a
// still-collapsed grandchild stays hidden (nested independence, like the row treetable).
export function toggle(collapsed, id) {
  const next = new Set(collapsed);
  if (next.has(id)) next.delete(id);
  else next.add(id);
  return next;
}

// visibleCount returns how many of the given program ids are currently VISIBLE (not hidden
// under the collapsed set). The DOM layer uses it (per parent, over that parent's subtree) to
// shrink each collapsible parent SPAN <th>'s colspan so the nested header stays aligned after
// columns hide.
export function visibleCount(parentMap, collapsed, ids) {
  let n = 0;
  for (const id of ids) {
    if (!columnHidden(parentMap, collapsed, id)) n++;
  }
  return n;
}

// clickToggles is the PURE decision (mirrors treetable's nameClickToggles): a click inside a
// collapsible group header toggles the subtree UNLESS it hit a genuine interactive element
// (a link, or the injected disclosure button, which has its own handler -- letting a header
// click ALSO fire would double-toggle). Returns true only for a "plain" header click.
export function clickToggles(insideInteractive) {
  return !insideInteractive;
}

// -------------------------- DOM glue (browser only) ------------------------

// enhanceMatrix wires column-collapse on ONE program-statement table. It reads the program
// column headers, builds the parent map, injects a disclosure affordance into each
// collapsible parent header, and re-renders (hide descendant cells + shrink the group
// colspan) on every toggle.
function enhanceMatrix(table) {
  const heads = Array.from(table.querySelectorAll('thead th[data-program]'));
  if (heads.length === 0) return; // not a program matrix -- self-guard, harmless
  const cols = heads.map((th) => ({
    program: th.getAttribute('data-program'),
    parent: th.getAttribute('data-program-parent'),
  }));
  const parentMap = buildParentMap(cols);
  const allIDs = cols.map((c) => c.program);
  // Unique program ids (a PARENT appears twice in the nested header: its subtree SPAN cell and
  // its own leaf column cell). Dedupe for the visible-count math so a parent isn't counted
  // twice when shrinking a span.
  const uniqueIDs = [...new Set(allIDs)];
  // The collapsible parent SPAN cells (data-program-group="1"): each one's colspan must track
  // the visible column count of its OWN subtree, so ancestors shrink as descendants hide.
  const spanCells = heads.filter((th) => th.getAttribute('data-program-group') === '1');
  const spanFull = new Map(); // th -> its full (all-expanded) colspan
  for (const th of spanCells) spanFull.set(th, Number(th.getAttribute('colspan')) || 1);
  // subtreeIDs(pid): pid plus every program whose ancestor chain includes pid (its subtree).
  function subtreeIDs(pid) {
    return uniqueIDs.filter((id) => id === pid || ancestors(parentMap, id).includes(pid));
  }

  let collapsed = new Set();

  // render applies the collapsed set to the DOM: hide every cell (th + td) of a hidden
  // program column by its data-program, shrink each parent span's colspan to its visible
  // subtree count, and sync each parent header's ▸/▾ affordance + aria to whether ITS column
  // is collapsed.
  function render() {
    for (const id of uniqueIDs) {
      const hidden = columnHidden(parentMap, collapsed, id);
      table.querySelectorAll(`[data-program="${cssEscape(id)}"]`).forEach((cell) => {
        cell.classList.toggle('col-hidden', hidden);
      });
    }
    for (const th of spanCells) {
      const pid = th.getAttribute('data-program');
      const vis = visibleCount(parentMap, collapsed, subtreeIDs(pid));
      th.setAttribute('colspan', String(Math.max(1, vis || spanFull.get(th))));
    }
    heads.forEach((th, i) => {
      const btn = th.querySelector('.col-toggle');
      if (!btn) return;
      const isCollapsed = collapsed.has(allIDs[i]);
      btn.setAttribute('aria-expanded', isCollapsed ? 'false' : 'true');
      btn.classList.toggle('is-collapsed', isCollapsed);
    });
  }

  function toggleID(id) {
    collapsed = toggle(collapsed, id);
    render();
  }

  // Inject a disclosure toggle into each COLLAPSIBLE (data-program-group) header and make the
  // whole header clickable -- a click on the program name toggles the subtree too, unless it
  // lands on a genuine link or the button itself (which fires its own handler; a header click
  // there would double-toggle). Enter/Space come free from the injected <button>.
  heads.forEach((th, i) => {
    if (th.getAttribute('data-program-group') !== '1') return;
    const id = allIDs[i];
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'col-toggle';
    btn.setAttribute('aria-expanded', 'true');
    btn.setAttribute('aria-label', th.textContent);
    btn.addEventListener('click', (ev) => {
      ev.stopPropagation(); // its own handler owns the toggle; don't also fire the header click
      toggleID(id);
    });
    th.insertBefore(btn, th.firstChild);
    th.classList.add('col-group-header');
    th.addEventListener('click', (ev) => {
      const insideInteractive = !!(ev.target.closest && ev.target.closest('a, button'));
      if (clickToggles(insideInteractive)) toggleID(id);
    });
  });

  render();
}

// cssEscape quotes a data-program value for a CSS attribute selector. The ids are numeric
// strings, so CSS.escape is belt-and-suspenders (and lets the pure logic stay string-based);
// fall back to the raw value when CSS.escape is unavailable (older test shims).
function cssEscape(v) {
  if (typeof CSS !== 'undefined' && typeof CSS.escape === 'function') return CSS.escape(v);
  return v;
}

// Browser glue: enhance every program-statement matrix on load and after an htmx swap (a
// filter change replaces #report-results, re-rendering the table fresh -- like treetable,
// state does NOT persist across a swap). Idempotent via a dataset flag. Guarded for Node.
if (typeof document !== 'undefined' && typeof document.addEventListener === 'function') {
  const initAll = () => {
    document.querySelectorAll('table.report-table').forEach((table) => {
      if (table.dataset.colWired) return;
      if (!table.querySelector('thead th[data-program-group]')) return; // no collapsible cols
      table.dataset.colWired = '1';
      enhanceMatrix(table);
    });
  };
  document.addEventListener('DOMContentLoaded', initAll);
  if (document.body) {
    document.body.addEventListener('htmx:afterSwap', initAll);
  }
}

export { enhanceMatrix };
