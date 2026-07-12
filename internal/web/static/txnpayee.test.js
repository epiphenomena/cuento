// p12.3 payee autofill -- unit test for the PURE never-overwrites guard (trap 2). This
// IS TestAutofillNeverOverwrites: the client applies a payee template ONLY when every
// split row is empty. No `document` access.

const test = require('node:test');
const assert = require('node:assert/strict');

let allRowsEmpty, isRowEmpty;
test.before(async () => {
  ({ allRowsEmpty, isRowEmpty } = await import('./txnpayee.js'));
});

test('allRowsEmpty: two blank rows -> true (autofill may apply)', () => {
  const rows = [
    { account: '0', amount: '', fund: '0', memo: '' },
    { account: '0', amount: '', fund: '0', memo: '' },
  ];
  assert.equal(allRowsEmpty(rows), true);
});

test('allRowsEmpty: an empty array is vacuously empty (fresh grid)', () => {
  assert.equal(allRowsEmpty([]), true);
});

test('allRowsEmpty: a chosen account makes a row non-empty -> false (no overwrite)', () => {
  const rows = [
    { account: '5', amount: '', memo: '' },
    { account: '0', amount: '', memo: '' },
  ];
  assert.equal(allRowsEmpty(rows), false);
});

test('allRowsEmpty: a typed signed amount blocks autofill', () => {
  const rows = [{ account: '0', amount: '25.00', memo: '' }];
  assert.equal(allRowsEmpty(rows), false);
});

test('allRowsEmpty: a typed DR or CR blocks autofill (DR/CR display mode)', () => {
  assert.equal(allRowsEmpty([{ account: '0', dr: '10.00', cr: '' }]), false);
  assert.equal(allRowsEmpty([{ account: '0', dr: '', cr: '10.00' }]), false);
});

test('allRowsEmpty: a typed memo blocks autofill', () => {
  assert.equal(allRowsEmpty([{ account: '0', amount: '', memo: 'rent' }]), false);
});

test('allRowsEmpty: a default fund/program/class alone does NOT block (not user intent)', () => {
  // A blank row can carry an auto-defaulted fund/program/class; that is not user input,
  // so the row is still empty and autofill applies.
  const rows = [{ account: '0', amount: '', fund: '7', program: '1', class: 'management', memo: '' }];
  assert.equal(allRowsEmpty(rows), true);
});

test('allRowsEmpty: whitespace-only values count as empty', () => {
  assert.equal(allRowsEmpty([{ account: '0', amount: '   ', memo: '  ' }]), true);
});

test('isRowEmpty: missing keys default to empty', () => {
  assert.equal(isRowEmpty({}), true);
  assert.equal(isRowEmpty(null), true);
});

test('allRowsEmpty: non-array input is not treated as empty', () => {
  assert.equal(allRowsEmpty(null), false);
  assert.equal(allRowsEmpty(undefined), false);
});
