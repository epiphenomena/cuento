// Unit tests for the pure split-row emptiness predicate (rowstate.js). Run under
// `node --test`. The predicate decides whether a grid row carries user input, which
// several grids use to gate auto-append / description-prefill / trailing-blank drop.

import test from 'node:test';
import assert from 'node:assert/strict';

let isRowEmpty;
test.before(async () => {
  ({ isRowEmpty } = await import('./rowstate.js'));
});

test('isRowEmpty: a blank row is empty (true)', () => {
  assert.equal(isRowEmpty({ account: '0', amount: '', memo: '' }), true);
});

test('isRowEmpty: missing keys default to empty', () => {
  assert.equal(isRowEmpty({}), true);
  assert.equal(isRowEmpty(null), true);
});

test('isRowEmpty: a chosen account makes a row non-empty -> false (no overwrite)', () => {
  assert.equal(isRowEmpty({ account: '5', amount: '', memo: '' }), false);
});

test('isRowEmpty: a typed signed amount blocks (non-empty)', () => {
  assert.equal(isRowEmpty({ account: '0', amount: '12.00', memo: '' }), false);
});

test('isRowEmpty: a typed DR or CR blocks (DR/CR display mode)', () => {
  assert.equal(isRowEmpty({ account: '0', dr: '10.00', cr: '' }), false);
  assert.equal(isRowEmpty({ account: '0', dr: '', cr: '10.00' }), false);
});

test('isRowEmpty: a typed memo blocks (non-empty)', () => {
  assert.equal(isRowEmpty({ account: '0', amount: '', memo: 'rent' }), false);
});

test('isRowEmpty: a default fund/program/class alone does NOT block (not user intent)', () => {
  assert.equal(isRowEmpty({ account: '0', amount: '', memo: '', fund: '3', program: '1', class: 'program' }), true);
});

test('isRowEmpty: whitespace-only values count as empty', () => {
  assert.equal(isRowEmpty({ account: '0', amount: '   ', memo: '  ' }), true);
});
