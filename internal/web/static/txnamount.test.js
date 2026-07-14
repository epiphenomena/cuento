// p12.2 transaction editor -- unit tests for the PURE amount logic (trap 2). These
// modules have NO `document` access: amount parse/format and the DR/CR<->signed
// mapping are pure functions, exercised for real under `node --test` (the Makefile
// globs internal/web/static/*.test.js). Stubbing `document` here would be a red
// flag; there is none to stub.

const test = require('node:test');
const assert = require('node:assert/strict');

// The module is an ES module; load it via dynamic import from this CommonJS test
// (the same pattern formfocus.test.js uses). A top-level await keeps each test
// body synchronous.
let parseAmountMinor, formatSignedMinor, drcrToSigned, signedToDrCr, formatAmountGrouped;
test.before(async () => {
  ({ parseAmountMinor, formatSignedMinor, drcrToSigned, signedToDrCr, formatAmountGrouped } = await import(
    './txnamount.js'
  ));
});

test('parseAmountMinor: plain decimal at exponent 2', () => {
  assert.equal(parseAmountMinor('12.34', 2), 1234);
  assert.equal(parseAmountMinor('1,234.50', 2), 123450);
  assert.equal(parseAmountMinor('-5.00', 2), -500);
  assert.equal(parseAmountMinor('(5.00)', 2), -500);
});

test('parseAmountMinor: exponent 0 (no fraction)', () => {
  assert.equal(parseAmountMinor('42', 0), 42);
});

test('parseAmountMinor: empty and malformed return null', () => {
  assert.equal(parseAmountMinor('', 2), null);
  assert.equal(parseAmountMinor('   ', 2), null);
  assert.equal(parseAmountMinor('abc', 2), null);
  assert.equal(parseAmountMinor('1.234', 2), null); // too many fractional digits
});

test('formatSignedMinor: round-trips through parseAmountMinor', () => {
  for (const minor of [0, 1, -1, 1234, -1234, 999999, -50]) {
    const s = formatSignedMinor(minor, 2);
    assert.equal(parseAmountMinor(s, 2), minor, `round-trip ${minor} via ${s}`);
  }
});

test('drcrToSigned: DR is positive (net-debit, D2), CR is negative', () => {
  // one field filled at a time; the other blank.
  assert.equal(drcrToSigned('10.00', '', 2), 1000);
  assert.equal(drcrToSigned('', '10.00', 2), -1000);
});

test('drcrToSigned: both blank -> null (empty row)', () => {
  assert.equal(drcrToSigned('', '', 2), null);
});

test('drcrToSigned: malformed side -> null (invalid, caller keeps typing)', () => {
  assert.equal(drcrToSigned('x', '', 2), null);
});

test('signedToDrCr: positive -> debit column, negative -> credit column', () => {
  assert.deepEqual(signedToDrCr(1000, 2), { debit: '10.00', credit: '' });
  assert.deepEqual(signedToDrCr(-1000, 2), { debit: '', credit: '10.00' });
  assert.deepEqual(signedToDrCr(0, 2), { debit: '', credit: '' });
});

test('drcr<->signed is a single round-trip mapping (trap 3)', () => {
  for (const minor of [1234, -1234, 500, -500]) {
    const { debit, credit } = signedToDrCr(minor, 2);
    assert.equal(drcrToSigned(debit, credit, 2), minor, `drcr round-trip ${minor}`);
  }
});

// p26.4: formatAmountGrouped reformats a user-typed amount on blur -- parse it (reusing
// parseAmountMinor's US grouping/decimal handling), then re-emit it with thousands
// grouping and the currency's fraction width. Blank/unparseable input is left untouched
// (the user is mid-type / cleared the field); it never invents a value.
test('formatAmountGrouped: pads fraction + inserts thousands grouping', () => {
  assert.equal(formatAmountGrouped('1000', 2), '1,000.00');
  assert.equal(formatAmountGrouped('1234567.5', 2), '1,234,567.50');
  assert.equal(formatAmountGrouped('12.34', 2), '12.34');
  assert.equal(formatAmountGrouped('0', 2), '0.00');
});

test('formatAmountGrouped: already-grouped input round-trips', () => {
  assert.equal(formatAmountGrouped('1,000.00', 2), '1,000.00');
  assert.equal(formatAmountGrouped('  42.5  ', 2), '42.50');
});

test('formatAmountGrouped: exponent 0 has no fraction', () => {
  assert.equal(formatAmountGrouped('1000', 0), '1,000');
});

test('formatAmountGrouped: blank or unparseable is returned unchanged', () => {
  assert.equal(formatAmountGrouped('', 2), '');
  assert.equal(formatAmountGrouped('   ', 2), '   ');
  assert.equal(formatAmountGrouped('abc', 2), 'abc');
});
