// p26.2 combobox -- unit tests for the PURE fuzzy ranking (trap 2). No `document`.
// rankOptions(query, options) ranks dotted-path option labels; the DOM glue in
// combobox.js drives it. Behavior under test: empty query preserves order; non-empty
// is a subsequence match, best first, non-matches excluded; dotted-path aware.

const test = require('node:test');
const assert = require('node:assert/strict');

let rankOptions;
test.before(async () => {
  ({ rankOptions } = await import('./combofilter.js'));
});

// A small realistic account option set (dotted ancestor paths, p26.1).
const ACCOUNTS = [
  { label: 'Cash.BOA', value: '10' },
  { label: 'Cash.Petty', value: '11' },
  { label: 'Cash.BOA.Payroll', value: '12' },
  { label: 'Accounts Receivable', value: '20' },
  { label: 'Expenses.Rent', value: '30' },
  { label: 'Expenses.Utilities.Water', value: '31' },
];

function labels(opts) {
  return opts.map((o) => o.label);
}

test('empty query -> all options in ORIGINAL order', () => {
  const r = rankOptions('', ACCOUNTS);
  assert.deepEqual(labels(r), labels(ACCOUNTS));
});

test('blank/whitespace query -> original order (treated as empty)', () => {
  assert.deepEqual(labels(rankOptions('   ', ACCOUNTS)), labels(ACCOUNTS));
});

test('empty query returns a NEW array, does not mutate input', () => {
  const before = labels(ACCOUNTS);
  const r = rankOptions('', ACCOUNTS);
  assert.notEqual(r, ACCOUNTS);
  assert.deepEqual(labels(ACCOUNTS), before);
});

test('dotted query c.boa ranks Cash.BOA highest', () => {
  const r = rankOptions('c.boa', ACCOUNTS);
  assert.ok(r.length > 0);
  assert.equal(r[0].label, 'Cash.BOA');
  // Cash.BOA.Payroll also matches c.boa as a subsequence, but the exact-leaf/prefix
  // fit puts plain Cash.BOA first.
  assert.ok(labels(r).includes('Cash.BOA.Payroll'));
});

test('leaf-only query boa matches the leaf segment of Cash.BOA', () => {
  const r = rankOptions('boa', ACCOUNTS);
  assert.equal(r[0].label, 'Cash.BOA');
  assert.ok(labels(r).includes('Cash.BOA.Payroll'));
});

test('case-insensitive: BOA and boa rank identically', () => {
  assert.deepEqual(labels(rankOptions('BOA', ACCOUNTS)), labels(rankOptions('boa', ACCOUNTS)));
});

test('non-matches are EXCLUDED', () => {
  const r = rankOptions('boa', ACCOUNTS);
  // Nothing without a b-o-a subsequence survives.
  assert.ok(!labels(r).includes('Accounts Receivable'));
  assert.ok(!labels(r).includes('Expenses.Rent'));
});

test('query matching nothing -> empty array', () => {
  assert.deepEqual(rankOptions('zzzq', ACCOUNTS), []);
});

test('prefix match beats scattered subsequence', () => {
  const opts = [
    { label: 'Grants.Water.Restricted', value: '1' }, // has "rest" scattered
    { label: 'Restricted Cash', value: '2' }, // "rest" is a true prefix
  ];
  const r = rankOptions('rest', opts);
  assert.equal(r[0].label, 'Restricted Cash');
});

test('contiguous run beats gappy subsequence for the same query', () => {
  const opts = [
    { label: 'Rabbit Ordinary Analysis', value: '1' }, // r..o..a gappy
    { label: 'Road', value: '2' }, // roa contiguous
  ];
  const r = rankOptions('roa', opts);
  assert.equal(r[0].label, 'Road');
});

test('word-boundary match beats mid-word for a leaf query', () => {
  const opts = [
    { label: 'Subscriptions', value: '1' }, // 'rent' not present; use 'rip'? keep simple
    { label: 'Expenses.Rent', value: '2' },
    { label: 'Parenting', value: '3' }, // contains "rent" mid-word
  ];
  const r = rankOptions('rent', opts);
  // Expenses.Rent (rent starts a segment / is the leaf) outranks Parenting (mid-word).
  assert.equal(r[0].label, 'Expenses.Rent');
});

test('deterministic tie-break by original index (stable)', () => {
  const opts = [
    { label: 'AX', value: '1' },
    { label: 'AX', value: '2' },
    { label: 'AX', value: '3' },
  ];
  const r = rankOptions('ax', opts);
  assert.deepEqual(r.map((o) => o.value), ['1', '2', '3']);
});
