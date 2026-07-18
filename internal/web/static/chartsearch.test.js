// Unit tests for the PURE chart-search helpers (chartsearch.js). Run under
// `node --test`. The DOM glue (input listener, CSS classes, htmx re-apply) is
// e2e-covered; only the pure path-building + visible-set decisions live here.

import test from 'node:test';
import assert from 'node:assert/strict';

let rowPaths, visibleSet;
test.before(async () => {
  ({ rowPaths, visibleSet } = await import('./chartsearch.js'));
});

// A small chart mirroring the p26.74 grouping:
//   0 Assets(header, d0)
//   1   Cash(d1)
//   2   Checking(d2, under Cash)
//   3 Expenses(header, d0)
//   4   Salaries(d1)
const names = ['', 'Cash', 'Checking', '', 'Salaries'];
const depths = [0, 1, 2, 0, 1];
const header = [true, false, false, true, false];

test('rowPaths: accounts get dotted ancestor paths; headers skipped', () => {
  const paths = rowPaths(names, depths, header);
  assert.deepEqual(paths, ['', 'Cash', 'Cash.Checking', '', 'Salaries']);
});

test('visibleSet: empty query -> null (show everything)', () => {
  assert.equal(visibleSet(rowPaths(names, depths, header), depths, header, ''), null);
  assert.equal(visibleSet(rowPaths(names, depths, header), depths, header, '   '), null);
});

test('visibleSet: matching a leaf reveals it + ancestors + its type header', () => {
  const paths = rowPaths(names, depths, header);
  const vis = visibleSet(paths, depths, header, 'checking');
  // Checking(2) matches; Cash(1) is its ancestor; Assets(0) is the type header.
  assert.deepEqual([...vis].sort((a, b) => a - b), [0, 1, 2]);
  // Expenses block is entirely hidden.
  assert.ok(!vis.has(3));
  assert.ok(!vis.has(4));
});

test('visibleSet: matching a PARENT name reveals the whole subtree via path prefix', () => {
  const paths = rowPaths(names, depths, header);
  const vis = visibleSet(paths, depths, header, 'cash');
  // "cash" is a subsequence of both "Cash" and "Cash.Checking", so both show; the
  // Assets header shows because its block has visible rows.
  assert.ok(vis.has(0)); // Assets header
  assert.ok(vis.has(1)); // Cash
  assert.ok(vis.has(2)); // Cash.Checking
  assert.ok(!vis.has(4)); // Salaries unaffected
});

test('visibleSet: a non-matching query hides all account rows and headers', () => {
  const paths = rowPaths(names, depths, header);
  const vis = visibleSet(paths, depths, header, 'zzzzz');
  assert.equal(vis.size, 0);
});

test('visibleSet: a dotted path query ranks a child by Parent.Child', () => {
  const paths = rowPaths(names, depths, header);
  // "c.che" is a subsequence of "cash.checking".
  const vis = visibleSet(paths, depths, header, 'c.che');
  assert.ok(vis.has(2)); // Checking
  assert.ok(vis.has(1)); // Cash (ancestor)
  assert.ok(vis.has(0)); // Assets header
});
