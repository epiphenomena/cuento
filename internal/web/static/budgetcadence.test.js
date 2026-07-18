// p27.2 budget-cadence unit tests (node --test). Covers the PURE date-stepping core
// (cadenceDates + the month-end clamp in addMonthsISO + parseISO validation); the
// DOM glue is e2e-covered (budget-plan spec). The month-end clamp (Jan 31 + 1mo ->
// Feb 28/29, NOT a March rollover) is the load-bearing edge case.

const test = require('node:test');
const assert = require('node:assert');

let cadenceDates, addMonthsISO, parseISO, formatISO;
let INTERVAL_WEEKLY, INTERVAL_BIWEEKLY, INTERVAL_MONTHLY;
test.before(async () => {
  ({
    cadenceDates, addMonthsISO, parseISO, formatISO,
    INTERVAL_WEEKLY, INTERVAL_BIWEEKLY, INTERVAL_MONTHLY,
  } = await import('./budgetcadence.js'));
});

test('parseISO accepts a valid date and rejects malformed / impossible ones', () => {
  assert.deepEqual(parseISO('2026-03-15'), { y: 2026, m: 3, d: 15 });
  assert.equal(parseISO('2026-02-30'), null); // impossible day
  assert.equal(parseISO('2026-13-01'), null); // bad month
  assert.equal(parseISO('2026-3-5'), null); // not zero-padded
  assert.equal(parseISO('not-a-date'), null);
  assert.equal(parseISO(''), null);
  assert.equal(parseISO(undefined), null);
});

test('formatISO zero-pads', () => {
  assert.equal(formatISO({ y: 2026, m: 3, d: 5 }), '2026-03-05');
});

test('weekly steps +7 days across a month boundary', () => {
  assert.deepEqual(
    cadenceDates('2026-01-29', INTERVAL_WEEKLY, 3),
    ['2026-01-29', '2026-02-05', '2026-02-12'],
  );
});

test('biweekly steps +14 days', () => {
  assert.deepEqual(
    cadenceDates('2026-03-01', INTERVAL_BIWEEKLY, 3),
    ['2026-03-01', '2026-03-15', '2026-03-29'],
  );
});

test('monthly steps +1 calendar month, same day when it exists', () => {
  assert.deepEqual(
    cadenceDates('2026-03-15', INTERVAL_MONTHLY, 4),
    ['2026-03-15', '2026-04-15', '2026-05-15', '2026-06-15'],
  );
});

test('monthly CLAMPS the day to month-end (Jan 31 -> Feb 28, NOT March rollover)', () => {
  assert.deepEqual(
    cadenceDates('2026-01-31', INTERVAL_MONTHLY, 4),
    ['2026-01-31', '2026-02-28', '2026-03-31', '2026-04-30'],
  );
});

test('monthly clamp respects a leap February (2028)', () => {
  assert.equal(addMonthsISO('2028-01-31', 1), '2028-02-29');
});

test('monthly crosses a year boundary', () => {
  assert.deepEqual(
    cadenceDates('2026-11-30', INTERVAL_MONTHLY, 3),
    ['2026-11-30', '2026-12-30', '2027-01-30'],
  );
});

test('the first generated date is the start itself', () => {
  assert.deepEqual(cadenceDates('2026-06-01', INTERVAL_WEEKLY, 1), ['2026-06-01']);
});

test('cadenceDates returns [] for bad start, bad interval, or count < 1', () => {
  assert.deepEqual(cadenceDates('bad', INTERVAL_WEEKLY, 3), []);
  assert.deepEqual(cadenceDates('2026-01-01', 'daily', 3), []);
  assert.deepEqual(cadenceDates('2026-01-01', INTERVAL_WEEKLY, 0), []);
  assert.deepEqual(cadenceDates('2026-01-01', INTERVAL_WEEKLY, -2), []);
});
