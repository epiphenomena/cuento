// p12.2 transaction editor -- unit tests for the PURE keyboard-grid state machine
// and the PURE subsidiary re-filter (trap 2). NO `document` access: the machine
// takes (grid shape, current cell, key) and returns the next cell + an action; the
// re-filter takes option metadata and the current rows and returns which rows are
// now invalid. The ARIA combobox wiring is thin DOM glue, covered by e2e, not here.

const test = require('node:test');
const assert = require('node:assert/strict');

let nextCell, invalidRowsForSubsidiary;
test.before(async () => {
  ({ nextCell, invalidRowsForSubsidiary } = await import('./txngrid.js'));
});

// A grid with `cols` editable columns per row and `rows` rows. The machine is
// column-index agnostic (the DOM glue maps indices to real inputs).
const grid = { rows: 3, cols: 4 };

test('Tab advances to the next column, same row', () => {
  assert.deepEqual(nextCell(grid, { row: 0, col: 0 }, 'Tab', false), {
    cell: { row: 0, col: 1 },
    action: 'move',
  });
});

test('Shift+Tab retreats to the previous column', () => {
  assert.deepEqual(nextCell(grid, { row: 0, col: 1 }, 'Tab', true), {
    cell: { row: 0, col: 0 },
    action: 'move',
  });
});

test('Tab on the last column wraps to the first column of the next row', () => {
  assert.deepEqual(nextCell(grid, { row: 0, col: 3 }, 'Tab', false), {
    cell: { row: 1, col: 0 },
    action: 'move',
  });
});

test('Enter moves to the next field like Tab within a row', () => {
  assert.deepEqual(nextCell(grid, { row: 0, col: 0 }, 'Enter', false), {
    cell: { row: 0, col: 1 },
    action: 'move',
  });
});

test('Enter on the LAST field of the LAST row requests a new row (Appendix C)', () => {
  assert.deepEqual(nextCell(grid, { row: 2, col: 3 }, 'Enter', false), {
    cell: { row: 2, col: 3 },
    action: 'add-row',
  });
});

test('Enter on the last field of a NON-last row moves to the next row start', () => {
  assert.deepEqual(nextCell(grid, { row: 0, col: 3 }, 'Enter', false), {
    cell: { row: 1, col: 0 },
    action: 'move',
  });
});

test('Ctrl+Enter saves the transaction', () => {
  assert.deepEqual(nextCell(grid, { row: 0, col: 0 }, 'Enter', false, { ctrl: true }), {
    cell: { row: 0, col: 0 },
    action: 'save',
  });
});

test('Escape cancels', () => {
  assert.deepEqual(nextCell(grid, { row: 1, col: 2 }, 'Escape', false), {
    cell: { row: 1, col: 2 },
    action: 'cancel',
  });
});

test('Alt+ArrowDown / Alt+ArrowUp move rows', () => {
  assert.deepEqual(nextCell(grid, { row: 0, col: 1 }, 'ArrowDown', false, { alt: true }), {
    cell: { row: 1, col: 1 },
    action: 'move-row-down',
  });
  assert.deepEqual(nextCell(grid, { row: 2, col: 1 }, 'ArrowUp', false, { alt: true }), {
    cell: { row: 1, col: 1 },
    action: 'move-row-up',
  });
});

test('Alt+ArrowUp on the first row is a no-op (stays put)', () => {
  assert.deepEqual(nextCell(grid, { row: 0, col: 1 }, 'ArrowUp', false, { alt: true }), {
    cell: { row: 0, col: 1 },
    action: 'none',
  });
});

test('Shift+Tab on the very first cell stays put (no wrap before the grid)', () => {
  assert.deepEqual(nextCell(grid, { row: 0, col: 0 }, 'Tab', true), {
    cell: { row: 0, col: 0 },
    action: 'move',
  });
});

test('an unrelated key is a no-op', () => {
  assert.deepEqual(nextCell(grid, { row: 1, col: 1 }, 'a', false), {
    cell: { row: 1, col: 1 },
    action: 'none',
  });
});

// --- subsidiary re-filter (pure) -----------------------------------------

// Each account option carries the set of subsidiary ids it is valid for (the
// server stamps data-subsidiaries on the <option>). Changing the header subsidiary
// re-computes which chosen accounts are now out of scope -> those rows get a
// per-row error (never silent-clear, Appendix C / trap 5's client display).
const accountSubs = {
  '10': ['1', '2'], // Checking US: subs 1,2
  '11': ['2'], // Cash MX: sub 2 only
  '12': ['1'], // Building: sub 1 only
};

test('invalidRowsForSubsidiary: rows whose account is out of the new sub are flagged', () => {
  const rows = [{ account: '10' }, { account: '11' }, { account: '12' }];
  // Switch to subsidiary "1": account 11 (sub 2 only) is now invalid.
  assert.deepEqual(invalidRowsForSubsidiary(rows, '1', accountSubs), [1]);
});

test('invalidRowsForSubsidiary: no invalid rows when all accounts cover the sub', () => {
  const rows = [{ account: '10' }, { account: '12' }];
  assert.deepEqual(invalidRowsForSubsidiary(rows, '1', accountSubs), []);
});

test('invalidRowsForSubsidiary: empty account rows are never flagged', () => {
  const rows = [{ account: '' }, { account: '11' }];
  assert.deepEqual(invalidRowsForSubsidiary(rows, '1', accountSubs), [1]);
});

test('invalidRowsForSubsidiary: unknown account id is flagged (defensive)', () => {
  const rows = [{ account: '999' }];
  assert.deepEqual(invalidRowsForSubsidiary(rows, '1', accountSubs), [0]);
});
