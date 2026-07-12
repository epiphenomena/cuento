// p12.2 transaction editor -- PURE keyboard-grid state machine and subsidiary
// re-filter (trap 2). NO `document` access: nextCell maps (grid shape, current
// cell, key, modifiers) -> the next cell + an action name; invalidRowsForSubsidiary
// maps (rows, sub, account->subs metadata) -> the indices of rows now out of scope.
// The DOM glue in txneditor.js translates cell indices to real inputs and actions to
// side effects; that glue is covered by e2e, not units. Keeping the logic pure is
// the point (a node test that stubbed `document` would be a red flag).

// nextCell is the grid state machine (Appendix C keys). `grid` is { rows, cols };
// `cell` is { row, col } (0-based); `key` is a KeyboardEvent.key; `shift` is the
// Shift modifier; `mods` carries { ctrl, alt } (meta treated as ctrl by the glue).
// `isVisible(row, col) -> bool` reports whether a cell is a focusable target for
// that row (p12.6): the editor hides the program/class cells on non-R/E rows, so
// advance/retreat/Enter must SKIP hidden cells rather than land focus in a hole.
// It defaults to "everything visible", preserving the original behavior and tests.
// Returns { cell, action } where action is one of: 'move', 'add-row', 'save',
// 'cancel', 'move-row-down', 'move-row-up', 'none'. It never touches the DOM.
export function nextCell(grid, cell, key, shift, mods = {}, isVisible = allVisible) {
  const { rows, cols } = grid;
  const { row, col } = cell;
  const stay = { cell: { row, col }, action: 'none' };

  // Ctrl/Cmd+Enter saves regardless of position.
  if (key === 'Enter' && mods.ctrl) {
    return { cell: { row, col }, action: 'save' };
  }
  if (key === 'Escape') {
    return { cell: { row, col }, action: 'cancel' };
  }

  // Alt+Arrow moves the whole row up/down (visibility is irrelevant: a whole row
  // moves, and the focused column stays the same).
  if (mods.alt && key === 'ArrowDown') {
    if (row >= rows - 1) return stay;
    return { cell: { row: row + 1, col }, action: 'move-row-down' };
  }
  if (mods.alt && key === 'ArrowUp') {
    if (row <= 0) return stay;
    return { cell: { row: row - 1, col }, action: 'move-row-up' };
  }

  if (key === 'Tab') {
    return shift ? retreat(grid, cell, isVisible) : advance(grid, cell, false, isVisible);
  }
  if (key === 'Enter') {
    // Enter advances like Tab, but when there is no visible cell forward (the last
    // visible cell of the last row) it asks for a new row (Appendix C) rather than
    // wrapping/staying. advance reports that via action 'add-row' when isEnter.
    return advance(grid, cell, true, isVisible);
  }
  return stay;
}

// allVisible is the default visibility predicate: every cell is a focus target.
function allVisible() {
  return true;
}

// advance moves forward one cell, SKIPPING hidden cells in the forward direction
// until it finds a visible one or runs off the end of the grid. Each step strictly
// increases the (row, col) position, so the scan always terminates. On the last
// visible cell of the grid it stays put; for Tab that reports 'move' (native focus
// carries out of the grid), for Enter it reports 'add-row' (Appendix C).
function advance(grid, cell, isEnter, isVisible) {
  const { rows, cols } = grid;
  let { row, col } = cell;
  for (;;) {
    if (col < cols - 1) {
      col += 1;
    } else if (row < rows - 1) {
      row += 1;
      col = 0;
    } else {
      // No cell forward of here: the last position in the grid.
      return { cell: { row: cell.row, col: cell.col }, action: isEnter ? 'add-row' : 'move' };
    }
    if (isVisible(row, col)) {
      return { cell: { row, col }, action: 'move' };
    }
  }
}

// retreat moves backward one cell (Shift+Tab), SKIPPING hidden cells in the
// backward direction until a visible one or the first cell of the grid. Each step
// strictly decreases the position, so the scan always terminates.
function retreat(grid, cell, isVisible) {
  const { cols } = grid;
  let { row, col } = cell;
  for (;;) {
    if (col > 0) {
      col -= 1;
    } else if (row > 0) {
      row -= 1;
      col = cols - 1;
    } else {
      // Before the first cell of the grid: stay put.
      return { cell: { row: cell.row, col: cell.col }, action: 'move' };
    }
    if (isVisible(row, col)) {
      return { cell: { row, col }, action: 'move' };
    }
  }
}

// invalidRowsForSubsidiary returns the indices of rows whose chosen account is NOT
// mapped to `sub` (the newly selected header subsidiary). `rows` is [{ account }]
// (account = the option value, "" = empty), `accountSubs` maps account id -> array
// of subsidiary id strings it is valid for. Empty-account rows are never flagged; an
// unknown account id is flagged defensively. This drives the per-row error on a
// subsidiary switch (Appendix C: never silent-clear) -- pure display; the server
// re-validates with ErrAccountNotInSubsidiary (trap 5).
export function invalidRowsForSubsidiary(rows, sub, accountSubs) {
  const bad = [];
  rows.forEach((r, i) => {
    const acct = r.account;
    if (acct === '' || acct === null || acct === undefined) return;
    const subs = accountSubs[acct];
    if (!subs || subs.indexOf(sub) < 0) bad.push(i);
  });
  return bad;
}
