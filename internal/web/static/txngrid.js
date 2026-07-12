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
// It returns { cell, action } where action is one of: 'move', 'add-row', 'save',
// 'cancel', 'move-row-down', 'move-row-up', 'none'. It never touches the DOM.
export function nextCell(grid, cell, key, shift, mods = {}) {
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

  // Alt+Arrow moves the whole row up/down.
  if (mods.alt && key === 'ArrowDown') {
    if (row >= rows - 1) return stay;
    return { cell: { row: row + 1, col }, action: 'move-row-down' };
  }
  if (mods.alt && key === 'ArrowUp') {
    if (row <= 0) return stay;
    return { cell: { row: row - 1, col }, action: 'move-row-up' };
  }

  if (key === 'Tab') {
    return shift ? retreat(grid, cell) : advance(grid, cell, false);
  }
  if (key === 'Enter') {
    // Enter advances like Tab, but on the LAST field of the LAST row it asks for a
    // new row (Appendix C) rather than wrapping/staying.
    if (row === rows - 1 && col === cols - 1) {
      return { cell: { row, col }, action: 'add-row' };
    }
    return advance(grid, cell, true);
  }
  return stay;
}

// advance moves forward one cell: next column, or the first column of the next row
// at a row boundary. On the very last cell of the grid it stays put (Tab) -- Enter's
// add-row case is handled by the caller before advance runs.
function advance(grid, cell, isEnter) {
  const { rows, cols } = grid;
  let { row, col } = cell;
  if (col < cols - 1) {
    return { cell: { row, col: col + 1 }, action: 'move' };
  }
  if (row < rows - 1) {
    return { cell: { row: row + 1, col: 0 }, action: 'move' };
  }
  // Last cell of the grid. Tab has nowhere to go (add-row is Enter-only, handled
  // by the caller), so stay -- but still report 'move' so focus logic is uniform.
  return { cell: { row, col }, action: isEnter ? 'add-row' : 'move' };
}

// retreat moves backward one cell (Shift+Tab): previous column, or the last column
// of the previous row at a boundary; before the first cell it stays put.
function retreat(grid, cell) {
  const { cols } = grid;
  let { row, col } = cell;
  if (col > 0) {
    return { cell: { row, col: col - 1 }, action: 'move' };
  }
  if (row > 0) {
    return { cell: { row: row - 1, col: cols - 1 }, action: 'move' };
  }
  return { cell: { row, col }, action: 'move' };
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
