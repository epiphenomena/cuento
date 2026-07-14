// p12.2 transaction editor -- PURE amount logic (trap 2). Amount parse/format and
// the DR/CR<->signed (net-debit, D2) mapping live here as pure functions with NO
// `document` access, unit-tested under `node --test`. The client normalizes DR/CR
// into ONE signed net-debit field before submit (trap 3): this module is the ONE
// mapping site, so the server always receives signed amounts (the store contract is
// unchanged) and signed-mode entry works with no JS at all.
//
// Boring frontend (rule 12): a small hand-written ES module, no framework, external
// (loaded under script-src 'self'), no inline handler. The DOM glue that wires these
// into inputs lives in txneditor.js and is covered by e2e, not units.

// parseAmountMinor parses a user-entered amount string to int64-ish minor units at
// the given currency exponent, or null when empty/malformed. It is deliberately a
// small, format-agnostic parser (US-style '.' decimal, ',' grouping) matching the
// editor's amount inputs; the AUTHORITATIVE parse is the Go store (money.Parse) --
// this drives client-side live chips only, so being conservative (null on doubt) is
// correct: an unparseable row simply does not contribute to the display chips.
export function parseAmountMinor(s, exponent) {
  if (typeof s !== 'string') return null;
  let str = s.trim();
  if (str === '') return null;

  let neg = false;
  // Parentheses = negative (must be balanced).
  if (str.startsWith('(') && str.endsWith(')')) {
    neg = true;
    str = str.slice(1, -1).trim();
  } else if (str.startsWith('(') || str.endsWith(')')) {
    return null;
  }
  if (str.startsWith('-')) {
    if (neg) return null;
    neg = true;
    str = str.slice(1);
  } else if (str.startsWith('+')) {
    str = str.slice(1);
  }
  str = str.trim();
  if (str === '') return null;

  // Strip thousands grouping (commas). Correctness comes from the digits.
  str = str.replace(/,/g, '');

  let intPart = str;
  let fracPart = '';
  const dot = str.indexOf('.');
  if (dot >= 0) {
    intPart = str.slice(0, dot);
    fracPart = str.slice(dot + 1);
    if (fracPart.indexOf('.') >= 0) return null; // multiple decimals
    if (exponent === 0) return null; // no fractional digits for this currency
  }
  if (fracPart.length > exponent) return null;
  if (intPart === '' && fracPart === '') return null;
  if (!/^\d*$/.test(intPart) || !/^\d*$/.test(fracPart)) return null;

  const scale = Math.pow(10, exponent);
  const intVal = intPart === '' ? 0 : Number(intPart);
  // Right-pad the fractional part to the exponent width.
  const fracVal = fracPart === '' ? 0 : Number(fracPart) * Math.pow(10, exponent - fracPart.length);
  let minor = intVal * scale + fracVal;
  if (neg) minor = -minor;
  return minor;
}

// formatSignedMinor renders minor units to a signed decimal string (US style), the
// inverse of parseAmountMinor, so parse(format(x)) == x. Used to prefill the hidden
// signed field and the DR/CR columns' magnitudes.
export function formatSignedMinor(minor, exponent) {
  const neg = minor < 0;
  let mag = Math.abs(minor);
  const scale = Math.pow(10, exponent);
  const intPart = Math.floor(mag / scale);
  const fracPart = mag % scale;
  let body = String(intPart);
  if (exponent > 0) {
    body += '.' + String(fracPart).padStart(exponent, '0');
  }
  return neg ? '-' + body : body;
}

// formatAmountGrouped reformats a user-typed amount string on blur: it PARSES the input
// (reusing parseAmountMinor's US-style '.' decimal / ',' grouping) and re-emits it with
// thousands grouping and the currency's fraction width (e.g. `1000` -> `1,000.00`). A
// blank or unparseable string is returned UNCHANGED -- the user is mid-type or cleared the
// field, and this is a display convenience, not a validator (the Go store is authoritative,
// trap 5). It is US-format only, matching parseAmountMinor; a non-US user's number format is
// beyond the current client scope (see DECISIONS p26.4) -- the server still parses per the
// user's setting. Sign is preserved (parentheses collapse to a leading '-').
export function formatAmountGrouped(s, exponent) {
  const minor = parseAmountMinor(s, exponent);
  if (minor === null) return s; // blank / mid-type / malformed -> leave the user's text
  const body = formatSignedMinor(minor, exponent); // e.g. "-1000.00" (no grouping yet)
  const neg = body.startsWith('-');
  const bare = neg ? body.slice(1) : body;
  const dot = bare.indexOf('.');
  const intPart = dot >= 0 ? bare.slice(0, dot) : bare;
  const fracPart = dot >= 0 ? bare.slice(dot) : '';
  const grouped = intPart.replace(/\B(?=(\d{3})+(?!\d))/g, ',');
  return (neg ? '-' : '') + grouped + fracPart;
}

// drcrToSigned maps a (debit, credit) pair of magnitude strings to ONE signed
// net-debit minor amount (D2): debit is positive, credit is negative. Exactly one
// side is filled (the DOM glue clears the other on input). Both blank -> null (an
// empty row); a malformed filled side -> null (the user is mid-type). This is the
// ONE mapping site (trap 3): the client writes the result into the hidden signed
// field so the server never sees DR/CR.
export function drcrToSigned(debit, credit, exponent) {
  const d = (debit || '').trim();
  const c = (credit || '').trim();
  if (d === '' && c === '') return null;
  if (d !== '') {
    const m = parseAmountMinor(d, exponent);
    return m === null ? null : Math.abs(m);
  }
  const m = parseAmountMinor(c, exponent);
  return m === null ? null : -Math.abs(m);
}

// signedToDrCr splits a signed net-debit minor amount into the DR/CR twin columns:
// a positive amount is a debit (fills the debit column), a negative a credit. Zero
// leaves both blank. The inverse of drcrToSigned, used to render an existing txn's
// splits into DR/CR mode without a server round-trip.
export function signedToDrCr(minor, exponent) {
  if (!minor) return { debit: '', credit: '' };
  const mag = formatSignedMinor(Math.abs(minor), exponent);
  return minor > 0 ? { debit: mag, credit: '' } : { debit: '', credit: mag };
}
