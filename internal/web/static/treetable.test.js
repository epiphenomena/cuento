// Unit tests for the PURE tree-table helpers (treetable.js). Run under `node --test`.
// These cover the collapse/expand logic that the DOM glue leans on: the contiguous
// descendant range of a row, and which depth level "expand one level" should reveal
// next given the current collapsed set. The DOM wiring (hiding rows, buttons) is
// e2e-covered; only the pure decisions live here.

import test from 'node:test';
import assert from 'node:assert/strict';

let descendantRange, hasChildren, rowHidden, nextLevelToReveal, collapseAllSet, expandLevelSet;
test.before(async () => {
  ({ descendantRange, hasChildren, rowHidden, nextLevelToReveal, collapseAllSet, expandLevelSet } =
    await import('./treetable.js'));
});

// A small chart:  0: A(0) B(1) C(2) D(1) E(0)
//                 idx:  0    1    2    3    4
const depths = [0, 1, 2, 1, 0];

test('descendantRange: a parent returns the contiguous deeper block', () => {
  // A (idx 0) owns B,C,D (idx 1..3); E (idx 4) is depth 0 and stops it.
  assert.deepEqual(descendantRange(depths, 0), [1, 4]);
  // B (idx 1) owns only C (idx 2).
  assert.deepEqual(descendantRange(depths, 1), [2, 3]);
});

test('descendantRange: a leaf returns an empty range [i+1, i+1]', () => {
  assert.deepEqual(descendantRange(depths, 2), [3, 3]); // C leaf
  assert.deepEqual(descendantRange(depths, 4), [5, 5]); // E last, leaf
});

test('hasChildren: true only for rows with a deeper next row', () => {
  assert.equal(hasChildren(depths, 0), true); // A
  assert.equal(hasChildren(depths, 1), true); // B
  assert.equal(hasChildren(depths, 2), false); // C leaf
  assert.equal(hasChildren(depths, 3), false); // D leaf
  assert.equal(hasChildren(depths, 4), false); // E leaf
});

test('rowHidden: a row is hidden iff some ancestor is in the collapsed set', () => {
  // Collapse A (idx 0): its whole block B,C,D is hidden; E stays.
  const collapsed = new Set([0]);
  assert.equal(rowHidden(depths, collapsed, 0), false); // A itself visible
  assert.equal(rowHidden(depths, collapsed, 1), true); // B under A
  assert.equal(rowHidden(depths, collapsed, 2), true); // C under A
  assert.equal(rowHidden(depths, collapsed, 3), true); // D under A
  assert.equal(rowHidden(depths, collapsed, 4), false); // E sibling
});

test('rowHidden: a deep row hidden by an intermediate collapse, not just the root', () => {
  const collapsed = new Set([1]); // collapse B
  assert.equal(rowHidden(depths, collapsed, 2), true); // C under B
  assert.equal(rowHidden(depths, collapsed, 1), false); // B visible
  assert.equal(rowHidden(depths, collapsed, 0), false); // A visible
});

test('collapseAllSet: collapses every parent row', () => {
  const set = collapseAllSet(depths);
  assert.deepEqual([...set].sort((a, b) => a - b), [0, 1]); // A and B are the parents
});

test('nextLevelToReveal: after collapse-all, the shallowest hidden depth is 1', () => {
  const collapsed = collapseAllSet(depths); // {0,1}
  assert.equal(nextLevelToReveal(depths, collapsed), 1);
});

test('nextLevelToReveal: after revealing depth 1, the next is depth 2', () => {
  const collapsed = collapseAllSet(depths); // {0,1} -> only depth 0 visible
  const afterOne = expandLevelSet(depths, collapsed); // reveal depth-1 rows
  assert.equal(nextLevelToReveal(depths, afterOne), 2);
});

test('nextLevelToReveal: fully expanded returns null (nothing hidden)', () => {
  const collapsed = new Set(); // nothing collapsed -> all visible
  assert.equal(nextLevelToReveal(depths, collapsed), null);
});

test('expandLevelSet: reveals exactly the next hidden level (progressive)', () => {
  // Start collapsed-all: {0,1}. One expand reveals depth-1 rows: uncollapse the
  // depth-0 parents (A) so its direct children show, but keep B collapsed so depth-2
  // (C) stays hidden.
  const collapsed = collapseAllSet(depths); // {0,1}
  const afterOne = expandLevelSet(depths, collapsed);
  // Depth-1 rows (B idx1, D idx3) are now visible; depth-2 (C idx2) still hidden.
  assert.equal(rowHidden(depths, afterOne, 1), false);
  assert.equal(rowHidden(depths, afterOne, 3), false);
  assert.equal(rowHidden(depths, afterOne, 2), true);
  // A second expand reveals depth-2.
  const afterTwo = expandLevelSet(depths, afterOne);
  assert.equal(rowHidden(depths, afterTwo, 2), false);
});
