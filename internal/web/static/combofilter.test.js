// p26.2 / p30.13 combobox -- unit tests for the PURE fuzzy ranking (trap 2). No `document`.
// rankOptions(query, options) ranks dotted-path option labels; the DOM glue in
// combobox.js drives it. Behavior under test: empty query preserves order; a non-empty
// query is TOKENIZED on segment/word separators and every fragment must be a CONTIGUOUS
// (adjacency) substring of the label; non-matches excluded; best first; dotted-path aware.

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

test('separator-only query -> original order (no fragments)', () => {
  assert.deepEqual(labels(rankOptions('...', ACCOUNTS)), labels(ACCOUNTS));
});

test('empty query returns a NEW array, does not mutate input', () => {
  const before = labels(ACCOUNTS);
  const r = rankOptions('', ACCOUNTS);
  assert.notEqual(r, ACCOUNTS);
  assert.deepEqual(labels(ACCOUNTS), before);
});

// ---- p30.13 owner acceptance set, all against a "Food Purchases" label ----

const FOOD = [{ label: 'Food Purchases', value: '1' }];

test('p30.13 acceptance: foo MATCHES Food Purchases (contiguous substring of "Food")', () => {
  assert.deepEqual(labels(rankOptions('foo', FOOD)), ['Food Purchases']);
});

test('p30.13 acceptance: pur MATCHES Food Purchases (contiguous substring of "Purchases")', () => {
  assert.deepEqual(labels(rankOptions('pur', FOOD)), ['Food Purchases']);
});

test('p30.13 acceptance: "f p" MATCHES (two fragments, each contiguous)', () => {
  assert.deepEqual(labels(rankOptions('f p', FOOD)), ['Food Purchases']);
});

test('p30.13 acceptance: "food purchases" MATCHES', () => {
  assert.deepEqual(labels(rankOptions('food purchases', FOOD)), ['Food Purchases']);
});

test('p30.13 acceptance: fp does NOT match (no contiguous "fp" anywhere)', () => {
  assert.deepEqual(rankOptions('fp', FOOD), []);
});

// ---- dotted-path / leaf regressions ----

test('dotted query c.boa ranks Cash.BOA highest', () => {
  const r = rankOptions('c.boa', ACCOUNTS);
  assert.ok(r.length > 0);
  assert.equal(r[0].label, 'Cash.BOA');
  // Cash.BOA.Payroll also matches (both "c" and "boa" are contiguous), but the exact-leaf
  // fit puts plain Cash.BOA first.
  assert.ok(labels(r).includes('Cash.BOA.Payroll'));
});

test('cash.boa (full segments) also matches Cash.BOA', () => {
  const r = rankOptions('cash.boa', ACCOUNTS);
  assert.equal(r[0].label, 'Cash.BOA');
});

test('leaf-only query boa matches the leaf segment of Cash.BOA', () => {
  const r = rankOptions('boa', ACCOUNTS);
  assert.equal(r[0].label, 'Cash.BOA');
  assert.ok(labels(r).includes('Cash.BOA.Payroll'));
});

test('leaf query boa ranks Cash.BOA above an account that only CONTAINS boa mid-word', () => {
  const opts = [
    { label: 'Global.Aboard Fund', value: '1' }, // "boa" mid-word in "Aboard"
    { label: 'Cash.BOA', value: '2' }, // "boa" is the boundary+leaf segment
  ];
  const r = rankOptions('boa', opts);
  assert.equal(r[0].label, 'Cash.BOA');
});

test('case-insensitive: BOA and boa rank identically', () => {
  assert.deepEqual(labels(rankOptions('BOA', ACCOUNTS)), labels(rankOptions('boa', ACCOUNTS)));
});

test('non-matches are EXCLUDED', () => {
  const r = rankOptions('boa', ACCOUNTS);
  // Nothing without a contiguous "boa" survives.
  assert.ok(!labels(r).includes('Accounts Receivable'));
  assert.ok(!labels(r).includes('Expenses.Rent'));
});

test('query matching nothing -> empty array', () => {
  assert.deepEqual(rankOptions('zzzq', ACCOUNTS), []);
});

test('prefix match beats a non-prefix boundary match', () => {
  const opts = [
    { label: 'Grants.Water.Restricted', value: '1' }, // "rest" boundary/leaf, not a prefix
    { label: 'Restricted Cash', value: '2' }, // "rest" is a true prefix
  ];
  const r = rankOptions('rest', opts);
  assert.equal(r[0].label, 'Restricted Cash');
});

test('adjacency: a gappy would-be subsequence no longer matches', () => {
  const opts = [
    { label: 'Rabbit Ordinary Analysis', value: '1' }, // r..o..a gappy, NOT contiguous "roa"
    { label: 'Road', value: '2' }, // "roa" contiguous
  ];
  const r = rankOptions('roa', opts);
  // Only the contiguous match survives; the gappy one is excluded entirely.
  assert.deepEqual(labels(r), ['Road']);
});

test('word-boundary match beats mid-word for a leaf query', () => {
  const opts = [
    { label: 'Subscriptions', value: '1' }, // no "rent"
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
