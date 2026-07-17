// p26.87 new-user password generator. On the admin create form the password field
// defaults to a strong RANDOM password the admin can copy to hand to the new user
// (instead of an empty field they must invent one for). A CSP-safe external ES module
// (rule 12: no inline script, script-src 'self'); the pure generator is unit-tested
// with `node --test` (pwgen.test.js).
//
// PROGRESSIVE ENHANCEMENT: with JS off the password field is a normal <input> the
// admin types into, and the server-rendered Regenerate button stays `hidden` + inert.
// With JS on this module fills the field, unhides the field (readable, so it can be
// copied) and the Regenerate button, and wires the button. The server is unchanged: it
// argon2id-hashes whatever value is submitted, generated or typed.

// CHARSET_GROUPS: four disjoint character classes. The generator draws a fixed count
// from EACH group (so every class is guaranteed present — a strong, policy-friendly
// password) and shuffles the result so the group order is not predictable. Ambiguous
// glyphs (0/O, 1/l/I) are excluded so a human can read the password off the screen and
// retype it without confusion.
const CHARSET_GROUPS = [
  'ABCDEFGHJKLMNPQRSTUVWXYZ', // upper (no I, O)
  'abcdefghijkmnpqrstuvwxyz', // lower (no l, o)
  '23456789', // digits (no 0, 1)
  '!@#$%^&*-_=+?', // symbols
];

// PER_GROUP is how many characters to draw from EACH group; total length is
// PER_GROUP * groups. 5 * 4 = 20 chars — comfortably above the ~16 the task asks for,
// with every character class represented.
const PER_GROUP = 5;

// randomInts fills a Uint32Array of length n with cryptographically strong random
// values via crypto.getRandomValues (Web Crypto — present in the browser AND in Node
// >= 19 as globalThis.crypto). It is injectable so the unit test can exercise the
// pure selection/shuffle logic without a real RNG; the default is the real CSPRNG.
export function randomInts(n, rng) {
  const out = new Uint32Array(n);
  if (rng) {
    rng(out);
  } else {
    globalThis.crypto.getRandomValues(out);
  }
  return out;
}

// pickUnbiased maps a 32-bit random value to [0, size) with rejection sampling so the
// distribution is uniform (a plain modulo would bias toward the low indices). It walks
// the supplied random pool starting at cursor, returning the chosen index and the next
// cursor; if the pool is exhausted it falls back to a modulo (bounded, never throws).
function pickUnbiased(pool, cursor, size) {
  const limit = Math.floor(0x100000000 / size) * size;
  let i = cursor;
  while (i < pool.length) {
    const v = pool[i];
    i += 1;
    if (v < limit) {
      return { index: v % size, cursor: i };
    }
  }
  // Pool exhausted (extremely unlikely with the generous sizing below): accept a
  // slightly-biased modulo of the last value rather than fail.
  return { index: pool[pool.length - 1] % size, cursor: i };
}

// generatePassword builds a strong random password: PER_GROUP characters drawn from
// each CHARSET_GROUP (guaranteeing every class), then Fisher–Yates shuffled so the
// group order is not exposed. `rng` is an optional crypto.getRandomValues-shaped
// function (see randomInts) for the unit test; production passes none and uses the
// real CSPRNG. Returns a string of length PER_GROUP * CHARSET_GROUPS.length.
export function generatePassword(rng) {
  const total = PER_GROUP * CHARSET_GROUPS.length;
  // Draw generously so rejection sampling and the shuffle both have entropy to spare
  // (each pick may reject a few values): total picks for the chars + total-1 for the
  // shuffle, times a safety factor.
  const pool = randomInts(total * 4 + 8, rng);
  let cursor = 0;

  const chars = [];
  for (const group of CHARSET_GROUPS) {
    for (let k = 0; k < PER_GROUP; k += 1) {
      const r = pickUnbiased(pool, cursor, group.length);
      cursor = r.cursor;
      chars.push(group[r.index]);
    }
  }

  // Fisher–Yates shuffle so the first PER_GROUP chars are not always uppercase, etc.
  for (let i = chars.length - 1; i > 0; i -= 1) {
    const r = pickUnbiased(pool, cursor, i + 1);
    cursor = r.cursor;
    const j = r.index;
    const tmp = chars[i];
    chars[i] = chars[j];
    chars[j] = tmp;
  }

  return chars.join('');
}

// enhanceCreateForm wires a create-form region: fill the password field with a fresh
// generated password, reveal it (type=text so the admin can read/copy it), and unhide
// + wire the Regenerate button (server-rendered `hidden` for the no-JS fallback).
// Idempotent via a dataset flag so a repeated htmx swap does not double-wire.
export function enhanceCreateForm(root) {
  if (!root || root.dataset.pwgenWired) return;
  const field = root.querySelector('#uc-password');
  const button = root.querySelector('.pwgen-regenerate');
  if (!field) return;
  root.dataset.pwgenWired = '1';

  const fill = () => {
    field.value = generatePassword();
  };

  // Reveal the value so the admin can copy it to hand to the user. type=password ->
  // type=text keeps the field a plain text input on submit (the server hashes the
  // value regardless).
  field.type = 'text';
  fill();

  if (button) {
    button.hidden = false;
    button.addEventListener('click', () => {
      fill();
      field.focus();
    });
  }
}

// Browser glue: enhance the create form on load AND after an htmx swap (the create
// form is swapped in via GET /admin/users/new, so it is not present on first parse).
// Guarded so importing under Node for the unit test is side-effect free.
if (typeof document !== 'undefined' && typeof document.addEventListener === 'function') {
  const initAll = () => {
    document.querySelectorAll('form.user-create-form').forEach((form) => {
      enhanceCreateForm(form);
    });
  };
  document.addEventListener('DOMContentLoaded', initAll);
  if (document.body) {
    document.body.addEventListener('htmx:afterSwap', initAll);
  }
}
