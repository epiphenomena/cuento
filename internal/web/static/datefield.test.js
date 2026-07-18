// p23.4 date field -- unit tests for the PURE date logic (trap 2). No `document`
// access: parse/format/shift/shortcut are pure functions, run for real under
// `node --test` (the Makefile globs internal/web/static/*.test.js). The DOM glue +
// calendar widget in datefield.js is guarded for Node and covered by e2e.

const test = require('node:test');
const assert = require('node:assert/strict');

let parseDate, formatDate, shiftDay, shiftMonth, endOfMonth, applyShortcut, expandYear;
test.before(async () => {
  ({ parseDate, formatDate, shiftDay, shiftMonth, endOfMonth, applyShortcut, expandYear } = await import('./datefield.js'));
});

// A fixed reference so implied-year forms are deterministic (matches the server's
// refNow in money/date_test.go): 2026-07-13.
const NOW = new Date(2026, 6, 13);

test('parseDate: full ISO/US/EU parse in their own order', () => {
  assert.deepEqual(parseDate('2025-03-04', 'ISO', NOW), { y: 2025, m: 3, d: 4 });
  assert.deepEqual(parseDate('03/04/2025', 'US', NOW), { y: 2025, m: 3, d: 4 });
  assert.deepEqual(parseDate('04/03/2025', 'EU', NOW), { y: 2025, m: 3, d: 4 });
});

test('parseDate: ISO is always accepted regardless of format', () => {
  for (const fmt of ['ISO', 'US', 'EU']) {
    assert.deepEqual(parseDate('2025-03-04', fmt, NOW), { y: 2025, m: 3, d: 4 });
  }
});

test('parseDate: flexible short forms are big-endian regardless of format', () => {
  assert.deepEqual(parseDate('26-6-1', 'ISO', NOW), { y: 2026, m: 6, d: 1 });
  assert.deepEqual(parseDate('26-06-01', 'US', NOW), { y: 2026, m: 6, d: 1 });
  assert.deepEqual(parseDate('26/6/1', 'EU', NOW), { y: 2026, m: 6, d: 1 });
  assert.deepEqual(parseDate('26.6.1', 'ISO', NOW), { y: 2026, m: 6, d: 1 });
});

test('parseDate: two-part form takes the reference year', () => {
  assert.deepEqual(parseDate('6-1', 'ISO', NOW), { y: 2026, m: 6, d: 1 });
  assert.deepEqual(parseDate('6/1', 'EU', NOW), { y: 2026, m: 6, d: 1 });
});

test('expandYear: strptime %y pivot', () => {
  assert.equal(expandYear(0, '0'), 2000);
  assert.equal(expandYear(68, '68'), 2068);
  assert.equal(expandYear(69, '69'), 1969);
  assert.equal(expandYear(98, '98'), 1998);
  assert.equal(expandYear(2026, '2026'), 2026);
});

test('parseDate: impossible / malformed -> null', () => {
  assert.equal(parseDate('', 'ISO', NOW), null);
  assert.equal(parseDate('not-a-date', 'ISO', NOW), null);
  assert.equal(parseDate('2026-2-30', 'ISO', NOW), null); // Feb 30
  assert.equal(parseDate('26-13-1', 'ISO', NOW), null); // month 13
  assert.equal(parseDate('1/2/3/4', 'ISO', NOW), null); // 4 parts
  assert.equal(parseDate('2025-02-29', 'ISO', NOW), null); // non-leap
});

test('formatDate: renders per format', () => {
  const d = { y: 2026, m: 6, d: 1 };
  assert.equal(formatDate(d, 'ISO'), '2026-06-01');
  assert.equal(formatDate(d, 'US'), '06/01/2026');
  assert.equal(formatDate(d, 'EU'), '01/06/2026');
});

test('shiftDay: crosses month/year boundaries', () => {
  assert.deepEqual(shiftDay({ y: 2026, m: 6, d: 30 }, 1), { y: 2026, m: 7, d: 1 });
  assert.deepEqual(shiftDay({ y: 2026, m: 1, d: 1 }, -1), { y: 2025, m: 12, d: 31 });
});

test('shiftMonth: clamps the day to the target month', () => {
  assert.deepEqual(shiftMonth({ y: 2026, m: 1, d: 31 }, 1), { y: 2026, m: 2, d: 28 });
  assert.deepEqual(shiftMonth({ y: 2026, m: 3, d: 15 }, -1), { y: 2026, m: 2, d: 15 });
  assert.deepEqual(shiftMonth({ y: 2026, m: 12, d: 10 }, 1), { y: 2027, m: 1, d: 10 });
});

test('endOfMonth: last day, leap-year aware', () => {
  assert.deepEqual(endOfMonth({ y: 2026, m: 2, d: 5 }), { y: 2026, m: 2, d: 28 });
  assert.deepEqual(endOfMonth({ y: 2024, m: 2, d: 5 }), { y: 2024, m: 2, d: 29 });
  assert.deepEqual(endOfMonth({ y: 2026, m: 7, d: 1 }), { y: 2026, m: 7, d: 31 });
});

test('applyShortcut: [ ] month, - + day, h end-of-month, t today', () => {
  // Base value 2026-06-15 in ISO.
  assert.equal(applyShortcut('[', '2026-06-15', 'ISO', NOW), '2026-05-15');
  assert.equal(applyShortcut(']', '2026-06-15', 'ISO', NOW), '2026-07-15');
  assert.equal(applyShortcut('-', '2026-06-15', 'ISO', NOW), '2026-06-14');
  assert.equal(applyShortcut('+', '2026-06-15', 'ISO', NOW), '2026-06-16');
  assert.equal(applyShortcut('h', '2026-06-15', 'ISO', NOW), '2026-06-30');
  assert.equal(applyShortcut('t', '2026-06-15', 'ISO', NOW), '2026-07-13'); // today = NOW
});

test('applyShortcut: empty field shifts from today', () => {
  assert.equal(applyShortcut('-', '', 'ISO', NOW), '2026-07-12');
  assert.equal(applyShortcut(']', '', 'ISO', NOW), '2026-08-13');
});

test('applyShortcut: - / + stay LITERAL while a partial date is being typed', () => {
  // "2026-06" is not yet a complete date -> '-' must be a literal separator (null),
  // so the user can finish typing "2026-06-01".
  assert.equal(applyShortcut('-', '2026-06', 'ISO', NOW), null);
  assert.equal(applyShortcut('+', '26-6', 'ISO', NOW), null);
  // But [ ] h t always act (they never appear mid-number); with an unparsed value
  // they operate on today (NOW), not the partial text.
  assert.equal(applyShortcut(']', '2026-06', 'ISO', NOW), '2026-08-13');
});

test('applyShortcut: "=" is an alias for "+" (shift a day forward)', () => {
  // The owner asked "make '=' same as '+'": '=' shifts a day FORWARD, identically
  // to '+', including the mid-typing-literal guard. Assert byte-for-byte parity.
  for (const value of ['2026-06-15', '', '06/15/2026']) {
    const fmt = value.includes('/') ? 'US' : 'ISO';
    assert.equal(applyShortcut('=', value, fmt, NOW), applyShortcut('+', value, fmt, NOW));
  }
  assert.equal(applyShortcut('=', '2026-06-15', 'ISO', NOW), '2026-06-16');
  // Mid-typing a partial date -> '=' stays a literal (null), exactly like '+'.
  assert.equal(applyShortcut('=', '2026-06', 'ISO', NOW), null);
  assert.equal(applyShortcut('=', '26-6', 'ISO', NOW), null);
});

test('applyShortcut: non-shortcut keys return null', () => {
  assert.equal(applyShortcut('a', '2026-06-15', 'ISO', NOW), null);
  assert.equal(applyShortcut('5', '2026-06-15', 'ISO', NOW), null);
});

test('applyShortcut: respects the display format', () => {
  assert.equal(applyShortcut('+', '06/15/2026', 'US', NOW), '06/16/2026');
  assert.equal(applyShortcut('[', '15/06/2026', 'EU', NOW), '15/05/2026');
});
