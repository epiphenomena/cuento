// p26.87 unit test for the new-user password generator (node --test). It exercises the
// PURE generator (generatePassword) — length, character classes, and uniqueness across
// calls — with the real CSPRNG (globalThis.crypto, present in Node >= 19) and with an
// injected deterministic RNG. The browser glue in pwgen.js is guarded by a
// `typeof document` check, so importing the module here has no side effects.

const test = require('node:test');
const assert = require('node:assert/strict');

async function mod() {
  return import('./pwgen.js');
}

// The character classes the generator guarantees (mirrors CHARSET_GROUPS, minus the
// ambiguous glyphs it excludes). Kept here so the test independently pins the policy.
const UPPER = /[A-HJ-NP-Z]/; // no I, O
const LOWER = /[a-hj-km-np-z]/; // no l, o
const DIGIT = /[2-9]/; // no 0, 1
const SYMBOL = /[!@#$%^&*\-_=+?]/;

test('generatePassword: length is at least 16 (strong)', async () => {
  const { generatePassword } = await mod();
  const pw = generatePassword();
  assert.ok(pw.length >= 16, `expected >=16 chars, got ${pw.length}`);
});

test('generatePassword: contains every character class', async () => {
  const { generatePassword } = await mod();
  // Check many draws so a flaky single draw missing a class would surface.
  for (let i = 0; i < 200; i += 1) {
    const pw = generatePassword();
    assert.match(pw, UPPER, `no uppercase in ${pw}`);
    assert.match(pw, LOWER, `no lowercase in ${pw}`);
    assert.match(pw, DIGIT, `no digit in ${pw}`);
    assert.match(pw, SYMBOL, `no symbol in ${pw}`);
  }
});

test('generatePassword: excludes ambiguous glyphs (0 O 1 l I)', async () => {
  const { generatePassword } = await mod();
  for (let i = 0; i < 200; i += 1) {
    const pw = generatePassword();
    assert.doesNotMatch(pw, /[0O1lI]/, `ambiguous glyph in ${pw}`);
  }
});

test('generatePassword: distinct across calls (real CSPRNG)', async () => {
  const { generatePassword } = await mod();
  const seen = new Set();
  const n = 500;
  for (let i = 0; i < n; i += 1) {
    seen.add(generatePassword());
  }
  // With >=16 chars of real entropy, 500 draws should be all distinct.
  assert.equal(seen.size, n, 'generated passwords collided across calls');
});

test('generatePassword: honors an injected RNG (deterministic path)', async () => {
  const { generatePassword } = await mod();
  // A counter RNG fills the Uint32Array with an increasing sequence; two runs with the
  // same seed produce the same password, and it is still a valid strong password.
  const counterRng = () => {
    let c = 7;
    return (arr) => {
      for (let i = 0; i < arr.length; i += 1) {
        c = (c * 1103515245 + 12345) >>> 0;
        arr[i] = c;
      }
    };
  };
  const a = generatePassword(counterRng());
  const b = generatePassword(counterRng());
  assert.equal(a, b, 'same injected RNG seed should be deterministic');
  assert.match(a, UPPER);
  assert.match(a, LOWER);
  assert.match(a, DIGIT);
  assert.match(a, SYMBOL);
});
