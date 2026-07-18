// p12.2 transaction editor -- unit tests for the PURE fund logic (trap 2):
// per-fund imbalance computation (client-side DISPLAY only; the server revalidates,
// trap 5). No `document` access. (p26.23 removed the fund apply-to-all helper + tests.)

const test = require('node:test');
const assert = require('node:assert/strict');

let fundImbalances;
let chipLabel;
let overallImbalance;
test.before(async () => {
  ({ fundImbalances, chipLabel, overallImbalance } = await import('./txnfund.js'));
});

// p28.4: with the main-split design the header/main split auto-balances the body, so a
// genuinely-balanced transaction (overall) is ALWAYS zero when the main split is present --
// the Total chip must render neutral, not red. overallImbalance folds the main split in.
test('overallImbalance: main present -> always 0 (the header split balances the body)', () => {
  // A nonzero body sum still nets to 0 overall because the header takes the residual.
  assert.equal(overallImbalance(4000, true), 0);
  assert.equal(overallImbalance(-4000, true), 0);
  assert.equal(overallImbalance(0, true), 0);
});

test('overallImbalance: flat grid (no main split) -> the body sum stands', () => {
  assert.equal(overallImbalance(4000, false), 4000);
  assert.equal(overallImbalance(0, false), 0);
  assert.equal(overallImbalance(-250, false), -250);
});

test('fundImbalances: balanced overall and per fund -> empty', () => {
  const rows = [
    { fund: '', amount: 1000 },
    { fund: '', amount: -1000 },
  ];
  const r = fundImbalances(rows);
  assert.equal(r.total, 0);
  assert.deepEqual(r.perFund, {}); // no nonzero fund groups
});

test('fundImbalances: overall imbalance reported', () => {
  const rows = [
    { fund: '', amount: 1000 },
    { fund: '', amount: -900 },
  ];
  const r = fundImbalances(rows);
  assert.equal(r.total, 100);
});

test('fundImbalances: per-fund groups keyed by fund id; unrestricted key ""', () => {
  // grant fund "7" balances; unrestricted "" balances; overall balances.
  const rows = [
    { fund: '7', amount: 600 },
    { fund: '7', amount: -600 },
    { fund: '', amount: 400 },
    { fund: '', amount: -400 },
  ];
  const r = fundImbalances(rows);
  assert.equal(r.total, 0);
  assert.deepEqual(r.perFund, {}); // all groups net zero -> nothing to show
});

test('fundImbalances: a single fund group out of balance is surfaced (D20)', () => {
  // overall zero but fund "7" nets +100 and unrestricted nets -100.
  const rows = [
    { fund: '7', amount: 700 },
    { fund: '7', amount: -600 },
    { fund: '', amount: 400 },
    { fund: '', amount: -500 },
  ];
  const r = fundImbalances(rows);
  assert.equal(r.total, 0);
  assert.deepEqual(r.perFund, { '7': 100, '': -100 });
});

test('fundImbalances: blank amounts ignored', () => {
  const rows = [
    { fund: '', amount: null },
    { fund: '', amount: 1000 },
    { fund: '', amount: -1000 },
  ];
  assert.equal(fundImbalances(rows).total, 0);
});

test('fundImbalances: fewer than 2 funds -> no per-fund chips even if a group nonzero', () => {
  // Only the unrestricted group present; per-fund chips appear only when >=2 funds
  // are in play (Appendix C). An overall imbalance is still reported via .total.
  const rows = [
    { fund: '', amount: 1000 },
    { fund: '', amount: -900 },
  ];
  const r = fundImbalances(rows);
  assert.equal(r.total, 100);
  assert.deepEqual(r.perFund, {}); // one fund only -> suppressed
});

test('chipLabel: unrestricted key "" resolves to the localized label, not the id', () => {
  const names = { 7: 'Building Fund' };
  assert.equal(chipLabel('', names, 'Unrestricted'), 'Unrestricted');
  assert.equal(chipLabel('', names, 'Sin restricción'), 'Sin restricción');
});

test('chipLabel: a known fund id resolves to its NAME, never the raw id', () => {
  const names = { 7: 'Building Fund', 12: 'Scholarship' };
  assert.equal(chipLabel('7', names, 'Unrestricted'), 'Building Fund');
  assert.equal(chipLabel('12', names, 'Unrestricted'), 'Scholarship');
});

test('chipLabel: an unknown id falls back to the raw id (chip stays visible, not blank)', () => {
  assert.equal(chipLabel('99', { 7: 'Building Fund' }, 'Unrestricted'), '99');
  assert.equal(chipLabel('5', {}, 'Unrestricted'), '5');
  assert.equal(chipLabel('5', null, 'Unrestricted'), '5');
});
