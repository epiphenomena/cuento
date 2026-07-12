// p12.2 transaction editor -- unit tests for the PURE fund logic (trap 2):
// per-fund imbalance computation (client-side DISPLAY only; the server revalidates,
// trap 5) and fund apply-to-all (fills EMPTY rows only). No `document` access.

const test = require('node:test');
const assert = require('node:assert/strict');

let fundImbalances, applyFundToAll;
test.before(async () => {
  ({ fundImbalances, applyFundToAll } = await import('./txnfund.js'));
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

test('applyFundToAll: fills only EMPTY rows, leaves set rows untouched', () => {
  const funds = ['', '3', ''];
  const out = applyFundToAll(funds, '7');
  assert.deepEqual(out, ['7', '3', '7']);
});

test('applyFundToAll: applying the empty (unrestricted) value fills empty rows too', () => {
  // The header select could apply "Unrestricted" (""), which still fills blanks;
  // but blanks are already "" -- so nothing observable changes. The contract is
  // "empty rows only", proven by the set row staying put.
  const funds = ['5', '', '5'];
  assert.deepEqual(applyFundToAll(funds, ''), ['5', '', '5']);
});
