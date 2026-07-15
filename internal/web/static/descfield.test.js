// p26.19 per-split description prefill -- unit test for the PURE apply helper (trap 2:
// no `document`). applyPrefillToRow honors the never-overwrites guard (reusing the tested
// isRowEmpty) and shapes the amount per the row's display mode (signed / drcr /
// magnitude). signedToMagnitude strips the sign for the expense grid.

const test = require('node:test');
const assert = require('node:assert/strict');

let applyPrefillToRow, signedToMagnitude;
test.before(async () => {
  ({ applyPrefillToRow, signedToMagnitude } = await import('./descfield.js'));
});

const found = {
  found: true,
  account: '7',
  amount: '40.00',
  fund: '3',
  program: '2',
  class: 'program',
  memo: 'Office rent',
};

test('applyPrefillToRow: signed mode fills every field, amount signed', () => {
  const ops = applyPrefillToRow({ account: '0', amount: '', memo: '' }, found, 'signed');
  assert.deepEqual(ops, {
    account: '7',
    fund: '3',
    program: '2',
    class: 'program',
    memo: 'Office rent',
    amount: '40.00',
  });
});

test('applyPrefillToRow: negative signed amount stays signed in signed mode', () => {
  const ops = applyPrefillToRow({ account: '0' }, { ...found, amount: '-40.00' }, 'signed');
  assert.equal(ops.amount, '-40.00');
});

test('applyPrefillToRow: magnitude mode (expense grid) strips the sign', () => {
  const pos = applyPrefillToRow({ account: '0' }, { ...found, amount: '40.00' }, 'magnitude');
  assert.equal(pos.amount, '40.00');
  const neg = applyPrefillToRow({ account: '0' }, { ...found, amount: '-40.00' }, 'magnitude');
  assert.equal(neg.amount, '40.00');
  assert.equal('dr' in neg, false);
  assert.equal('cr' in neg, false);
});

test('applyPrefillToRow: drcr mode -- positive is a debit, negative a credit', () => {
  const dr = applyPrefillToRow({ account: '0' }, { ...found, amount: '40.00' }, 'drcr');
  assert.deepEqual({ dr: dr.dr, cr: dr.cr }, { dr: '40.00', cr: '' });
  const cr = applyPrefillToRow({ account: '0' }, { ...found, amount: '-40.00' }, 'drcr');
  assert.deepEqual({ dr: cr.dr, cr: cr.cr }, { dr: '', cr: '40.00' });
  assert.equal('amount' in dr, false);
});

test('applyPrefillToRow: drcr mode -- a zero amount clears both columns', () => {
  const z = applyPrefillToRow({ account: '0' }, { ...found, amount: '0.00' }, 'drcr');
  assert.deepEqual({ dr: z.dr, cr: z.cr }, { dr: '', cr: '' });
});

test('applyPrefillToRow: never overwrites a non-empty row (typed amount)', () => {
  assert.equal(applyPrefillToRow({ account: '0', amount: '99.00' }, found, 'signed'), null);
});

test('applyPrefillToRow: never overwrites a row with a chosen account', () => {
  assert.equal(applyPrefillToRow({ account: '5', amount: '' }, found, 'signed'), null);
});

test('applyPrefillToRow: a default fund/program alone does NOT block the fill', () => {
  // fund/program defaults are not user intent (isRowEmpty ignores them), so the row is
  // still empty and prefill applies.
  const ops = applyPrefillToRow({ account: '0', amount: '', fund: '3', program: '2', memo: '' }, found, 'signed');
  assert.ok(ops);
  assert.equal(ops.memo, 'Office rent');
});

test('applyPrefillToRow: no match (found=false) or null -> nothing to apply', () => {
  assert.equal(applyPrefillToRow({ account: '0' }, { found: false }, 'signed'), null);
  assert.equal(applyPrefillToRow({ account: '0' }, null, 'signed'), null);
});

test('applyPrefillToRow: 0-id fund/program pass through (the glue maps "0"/"" to the none option)', () => {
  const ops = applyPrefillToRow({ account: '0' }, { ...found, fund: '0', program: '0', class: '' }, 'signed');
  assert.equal(ops.fund, '0');
  assert.equal(ops.program, '0');
  assert.equal(ops.class, '');
});

test('signedToMagnitude: strips a leading minus and trims', () => {
  assert.equal(signedToMagnitude('-40.00'), '40.00');
  assert.equal(signedToMagnitude('  40.00 '), '40.00');
  assert.equal(signedToMagnitude(''), '');
  assert.equal(signedToMagnitude(null), '');
});
