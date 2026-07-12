// p12.3 payee autofill -- PURE guard logic (trap 2: no `document` access; unit-tested
// under `node --test`). The DOM glue in txneditor.js fetches the template partial and,
// gated by allRowsEmpty, swaps its rows into the grid. Keeping the never-overwrites
// decision here (a pure predicate over the current rows' field values) is the point:
// the client applies a payee template ONLY when EVERY split row is empty, so a user
// who has typed anything is never clobbered (Appendix C / p12.3).

// allRowsEmpty reports whether EVERY split row is empty -- the precondition for
// applying a payee autofill template. Each row is a plain object of the row's editable
// field values: { account, amount, dr, cr, fund, program, class, memo } (any subset;
// missing keys count as empty). A row is empty when it has no account chosen ("0"/""),
// no amount (signed or DR/CR), and no memo. Fund/program/class defaults alone do NOT
// make a row non-empty (they are auto-defaulted, not user intent) -- only a chosen
// account, a typed amount, or a typed memo counts as user input. An empty rows array
// is vacuously empty (true), so autofill applies to a freshly-opened blank grid.
export function allRowsEmpty(rows) {
  if (!Array.isArray(rows)) return false;
  return rows.every(isRowEmpty);
}

// isRowEmpty reports whether one row carries no user input (see allRowsEmpty).
export function isRowEmpty(row) {
  if (!row) return true;
  if (hasValue(row.account) && row.account !== '0') return false; // an account is chosen
  if (hasValue(row.amount)) return false; // a signed amount typed
  if (hasValue(row.dr)) return false; // a debit typed
  if (hasValue(row.cr)) return false; // a credit typed
  if (hasValue(row.memo)) return false; // a memo typed
  return true;
}

// hasValue reports whether v is a non-blank string (trimmed). null/undefined and
// whitespace-only count as blank.
function hasValue(v) {
  return typeof v === 'string' ? v.trim() !== '' : v !== null && v !== undefined && v !== '';
}
