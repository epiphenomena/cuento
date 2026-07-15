// p26.25 REUSABLE collapse/expand for a depth-ordered tree table -- a CSP-safe ES
// module (rule 12: external, no inline handler, script-src 'self'). Enhances a table
// whose nodes are rendered as a FLAT list of <tr> in pre-order tree traversal, each
// carrying `data-depth="N"` (root = 0). A row's DESCENDANTS are the contiguous
// following rows with depth > its own (until a row with depth <= its own). No server
// round-trip: this is a pure display enhancement.
//
// ============================ DOM CONTRACT (for reuse) ======================
// A caller wires this by handing enhanceTree() the TABLE and a CONTROLS container:
//
//   enhanceTree(tableEl, controlsEl)
//
//   - tableEl: a <table> whose data rows are <tr data-depth="N"> inside <tbody>.
//     Rows must appear in pre-order (a parent immediately precedes its subtree).
//     "Has children" is derived from the depth sequence alone (the next row is
//     deeper) -- NO per-row marker attribute is required. This keeps the contract
//     minimal so report tables can reuse it with only data-depth.
//   - controlsEl: a container holding the (optionally hidden) control buttons:
//       .tree-collapse-all   -> collapse to depth-0 rows only
//       .tree-expand-level   -> reveal the next-deepest currently-hidden level
//       .tree-expand-all     -> reveal everything
//     Buttons may be rendered with the `hidden` attribute (NO-JS fallback: the full
//     tree shows and inert buttons stay hidden); this module removes `hidden` on init.
//
// Each parent row (a row that HAS descendants) gets a clickable ▸/▾ disclosure
// affordance injected into its first cell; clicking it collapses/expands THAT row's
// subtree. State is a single "collapsed set" of row indices: a row is hidden iff any
// ancestor is collapsed. Expanding a parent reveals its DIRECT children; a child that
// is itself collapsed stays collapsed (its own state persists).
// ===========================================================================
//
// Guarded so importing under Node is side-effect free (no `document`). The PURE
// decisions below (descendantRange, hasChildren, rowHidden, collapseAllSet,
// expandLevelSet, nextLevelToReveal) are unit-tested (treetable.test.js).

// descendantRange returns [start, end): the half-open index range of row i's
// descendants -- the contiguous following rows deeper than depths[i]. A leaf yields
// an empty range [i+1, i+1].
export function descendantRange(depths, i) {
  const own = depths[i];
  let end = i + 1;
  while (end < depths.length && depths[end] > own) end++;
  return [i + 1, end];
}

// hasChildren reports whether row i has at least one descendant (the next row is
// deeper). This is the SOLE "is a parent" test -- no marker attribute needed.
export function hasChildren(depths, i) {
  return i + 1 < depths.length && depths[i + 1] > depths[i];
}

// rowHidden reports whether row i is hidden: some ANCESTOR of i is in the collapsed
// set. An ancestor of i is a row j < i whose descendant range contains i and whose
// depth is shallower. Walking the collapsed set is fine (small charts); we test each
// collapsed parent's range for containment.
export function rowHidden(depths, collapsed, i) {
  for (const p of collapsed) {
    if (p >= i) continue; // a parent precedes its descendants
    const [start, end] = descendantRange(depths, p);
    if (i >= start && i < end) return true;
  }
  return false;
}

// collapseAllSet returns the set of ALL parent rows -- collapsing every parent hides
// everything below depth 0 (only roots remain visible).
export function collapseAllSet(depths) {
  const set = new Set();
  for (let i = 0; i < depths.length; i++) {
    if (hasChildren(depths, i)) set.add(i);
  }
  return set;
}

// nextLevelToReveal returns the shallowest depth that is currently HIDDEN (the next
// level "expand one level" would reveal), or null when nothing is hidden (fully
// expanded). It is the min depth over rows that are hidden under the collapsed set.
export function nextLevelToReveal(depths, collapsed) {
  let best = null;
  for (let i = 0; i < depths.length; i++) {
    if (rowHidden(depths, collapsed, i)) {
      if (best === null || depths[i] < best) best = depths[i];
    }
  }
  return best;
}

// expandLevelSet returns a NEW collapsed set that reveals one more level: uncollapse
// every parent sitting at depth (target - 1), where target is the shallowest hidden
// depth. That makes the target-depth rows visible while any deeper collapsed parents
// keep their subtrees hidden (progressive reveal). If nothing is hidden it is a no-op.
export function expandLevelSet(depths, collapsed) {
  const target = nextLevelToReveal(depths, collapsed);
  if (target === null) return new Set(collapsed);
  const next = new Set(collapsed);
  for (const p of collapsed) {
    if (depths[p] === target - 1) next.delete(p);
  }
  return next;
}

// -------------------------- DOM glue (browser only) ------------------------

function enhanceTree(table, controls) {
  const rows = Array.from(table.querySelectorAll('tbody > tr[data-depth]'));
  if (rows.length === 0) return;
  const depths = rows.map((tr) => Number(tr.getAttribute('data-depth')) || 0);

  const collapsed = new Set(); // indices of collapsed parent rows

  // Inject a disclosure toggle into each parent row's first cell.
  const toggles = new Array(rows.length).fill(null);
  rows.forEach((tr, i) => {
    if (!hasChildren(depths, i)) return;
    const cell = tr.querySelector('td, th');
    if (!cell) return;
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'tree-toggle';
    btn.setAttribute('aria-expanded', 'true');
    btn.addEventListener('click', () => {
      if (collapsed.has(i)) collapsed.delete(i);
      else collapsed.add(i);
      render();
    });
    cell.insertBefore(btn, cell.firstChild);
    toggles[i] = btn;
  });

  // render applies the collapsed set to the DOM: hide hidden rows, set each toggle's
  // glyph/aria to reflect whether ITS row is collapsed.
  function render() {
    rows.forEach((tr, i) => {
      tr.hidden = rowHidden(depths, collapsed, i);
    });
    toggles.forEach((btn, i) => {
      if (!btn) return;
      const isCollapsed = collapsed.has(i);
      btn.setAttribute('aria-expanded', isCollapsed ? 'false' : 'true');
      btn.classList.toggle('is-collapsed', isCollapsed);
    });
  }

  function wire(sel, fn) {
    if (!controls) return;
    const el = controls.querySelector(sel);
    if (!el) return;
    el.hidden = false; // reveal the button now that JS is live
    el.addEventListener('click', fn);
  }

  wire('.tree-collapse-all', () => {
    collapseAllSet(depths).forEach((p) => collapsed.add(p));
    render();
  });
  wire('.tree-expand-level', () => {
    const next = expandLevelSet(depths, collapsed);
    collapsed.clear();
    next.forEach((p) => collapsed.add(p));
    render();
  });
  wire('.tree-expand-all', () => {
    collapsed.clear();
    render();
  });

  render();
}

// Browser glue: enhance the accounts tree on load and after the htmx filter swap
// (which replaces #accounts-results, table + controls together). Idempotent via a
// dataset flag on the table. Guarded for Node.
if (typeof document !== 'undefined' && typeof document.addEventListener === 'function') {
  const initAll = () => {
    document.querySelectorAll('table.tree-table').forEach((table) => {
      if (table.dataset.treeWired) return;
      table.dataset.treeWired = '1';
      const results = table.closest('#accounts-results') || document;
      const controls = results.querySelector
        ? results.querySelector('.accounts-controls')
        : null;
      enhanceTree(table, controls);
    });
  };
  document.addEventListener('DOMContentLoaded', initAll);
  if (document.body) {
    document.body.addEventListener('htmx:afterSwap', initAll);
  }
}

export { enhanceTree };
