// Split-row emptiness predicate -- PURE guard logic (trap 2: no `document` access;
// unit-tested under `node --test`). A row is "empty" when it carries no user input,
// which several grids use to decide whether it is safe to auto-append a fresh row, to
// prefill a row from a matched description (descfield.js), or to drop a trailing blank
// row on save. Keeping the never-overwrites decision here (a pure predicate over the
// row's field values) is the point: a row that the user has typed into is never
// clobbered. (Formerly txnpayee.js, when this gated payee autofill; the payee entity
// is retired as of p26.20 but the emptiness predicate is still the shared contract.)

// isRowEmpty reports whether one row carries no user input. The row is a plain object
// of the row's editable field values: { account, amount, dr, cr, memo } (any subset;
// missing keys count as empty). A row is empty when it has no account chosen ("0"/""),
// no amount (signed or DR/CR), and no memo. Fund/program/class defaults alone do NOT
// make a row non-empty (they are auto-defaulted, not user intent) -- only a chosen
// account, a typed amount, or a typed memo counts as user input.
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
