// Unit tests for the PURE column-collapse helpers (colcollapse.js). Run under `node --test`.
// These cover the program-column-tree decisions the DOM glue leans on: building the
// parent map, the strict-ancestor chain, whether a column is hidden under a collapsed set,
// the toggle set transition, and the visible-count the group-colspan shrink uses. The DOM
// wiring (hiding cells, injecting the disclosure, shrinking colspan) is e2e-covered.

import test from 'node:test';
import assert from 'node:assert/strict';

let buildParentMap, ancestors, columnHidden, toggle, visibleCount, clickToggles;
test.before(async () => {
  ({ buildParentMap, ancestors, columnHidden, toggle, visibleCount, clickToggles } =
    await import('./colcollapse.js'));
});

// A small program tree (matching the 10a matrix shape):
//   General (10)                      root, has children
//     Affiliates (11)                 child of General, has children
//       UPH (13)                      child of Affiliates, leaf
//     Educacion (12)                  child of General, leaf
//   Standalone (20)                   root, leaf
// Columns are emitted in tree pre-order.
const cols = [
  { program: '10', parent: '' },
  { program: '11', parent: '10' },
  { program: '13', parent: '11' },
  { program: '12', parent: '10' },
  { program: '20', parent: '' },
];
const allIDs = cols.map((c) => c.program);

test('buildParentMap: roots map to null, children to their parent id', () => {
  const m = buildParentMap(cols);
  assert.equal(m.get('10'), null);
  assert.equal(m.get('11'), '10');
  assert.equal(m.get('13'), '11');
  assert.equal(m.get('12'), '10');
  assert.equal(m.get('20'), null);
});

test('ancestors: strict chain nearest-first, excludes self, empty for a root', () => {
  const m = buildParentMap(cols);
  assert.deepEqual(ancestors(m, '13'), ['11', '10']); // UPH -> Affiliates -> General
  assert.deepEqual(ancestors(m, '11'), ['10']); // Affiliates -> General
  assert.deepEqual(ancestors(m, '10'), []); // General is a root
  assert.deepEqual(ancestors(m, '20'), []); // Standalone is a root
});

test('columnHidden: collapsing a parent hides its whole subtree, not itself', () => {
  const m = buildParentMap(cols);
  const collapsed = new Set(['10']); // collapse General
  assert.equal(columnHidden(m, collapsed, '10'), false); // General itself stays (rollup)
  assert.equal(columnHidden(m, collapsed, '11'), true); // Affiliates under General
  assert.equal(columnHidden(m, collapsed, '13'), true); // UPH under General (deep)
  assert.equal(columnHidden(m, collapsed, '12'), true); // Educacion under General
  assert.equal(columnHidden(m, collapsed, '20'), false); // Standalone unrelated
});

test('columnHidden: an intermediate collapse hides only its own subtree', () => {
  const m = buildParentMap(cols);
  const collapsed = new Set(['11']); // collapse Affiliates
  assert.equal(columnHidden(m, collapsed, '11'), false); // Affiliates itself stays
  assert.equal(columnHidden(m, collapsed, '13'), true); // UPH under Affiliates
  assert.equal(columnHidden(m, collapsed, '12'), false); // Educacion NOT under Affiliates
  assert.equal(columnHidden(m, collapsed, '10'), false); // General is an ancestor, not hidden
});

test('columnHidden: nested collapse -- expand parent keeps a collapsed grandchild hidden', () => {
  const m = buildParentMap(cols);
  // Both General and Affiliates collapsed: everything under General hidden.
  let collapsed = new Set(['10', '11']);
  assert.equal(columnHidden(m, collapsed, '11'), true); // Affiliates hidden by General
  assert.equal(columnHidden(m, collapsed, '13'), true); // UPH hidden
  // Expand General (remove it): Affiliates now visible (its rollup), but UPH stays hidden
  // because Affiliates is STILL collapsed -- nested independence.
  collapsed = toggle(collapsed, '10');
  assert.equal(columnHidden(m, collapsed, '11'), false); // Affiliates shows its rollup
  assert.equal(columnHidden(m, collapsed, '12'), false); // Educacion shows
  assert.equal(columnHidden(m, collapsed, '13'), true); // UPH still hidden under Affiliates
});

test('toggle: returns a NEW set flipping membership (pure)', () => {
  const base = new Set(['10']);
  const added = toggle(base, '11');
  assert.deepEqual([...added].sort(), ['10', '11']);
  assert.deepEqual([...base], ['10']); // original untouched
  const removed = toggle(added, '10');
  assert.deepEqual([...removed], ['11']);
});

test('visibleCount: counts columns not hidden -- drives the group colspan shrink', () => {
  const m = buildParentMap(cols);
  assert.equal(visibleCount(m, new Set(), allIDs), 5); // nothing collapsed: all 5 show
  // Collapse General: General(10) + Standalone(20) remain visible; its 3 descendants hide.
  assert.equal(visibleCount(m, new Set(['10']), allIDs), 2);
  // Collapse Affiliates only: hide UPH(13); the other 4 show.
  assert.equal(visibleCount(m, new Set(['11']), allIDs), 4);
});

test('clickToggles: a plain header click toggles; an interactive-element click does not', () => {
  assert.equal(clickToggles(false), true); // plain header/name click
  assert.equal(clickToggles(true), false); // click hit a link or the disclosure button
});
