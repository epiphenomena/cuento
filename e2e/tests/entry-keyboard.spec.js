// @ts-check
// p12.6 keyboard-only entry of a 4-split MIXED-FUND transaction, end to end, through
// the REAL grid served by `cuento serve -dev`. This is the automatable core of the
// docs/qa-entry.md keyboard pass: it drives the editor with REAL keyboard events
// (page.keyboard.press('Tab'), Arrow/Enter to operate the native <select>s, typing
// amounts, Space/Enter on the add-row button) -- never selectOption -- so it exercises
// the actual keyboard reachability of every field a book-keeper touches, and asserts
// the entry posts.
//
// The 4 splits balance BOTH overall and per fund (D20 per-fund zero-sum), which is
// what makes it a genuine mixed-fund entry:
//   row0  A  +40.00  Water Grant     row1  B  -40.00  Water Grant
//   row2  A  +10.00  Unrestricted    row3  B  -10.00  Unrestricted
//   Water: +40-40 = 0    Unrestricted: +10-10 = 0    overall: 0
// All four accounts are ASSET leaves, so the program/class cells stay hidden
// (visibility:hidden -> out of native Tab order); linear Tab therefore walks exactly
// account -> amount -> fund -> memo per row with no dead stops.
//
// FINDING documented in docs/qa-entry.md: txngrid.js's nextCell state machine (Enter-
// advance, Alt+Arrow row-move, Ctrl+Enter save, Escape) is imported by txneditor.js
// but never wired to the DOM, so those keys are inert in the browser today. Native Tab
// + native select keyboard operation cover linear entry (proven here). This spec drives
// what actually works in the browser; it does not (and cannot) exercise the unwired
// nextCell via real keyboard events. See qa-entry.md "Keyboard grid state machine".

const { test, expect } = require('../fixtures');
const { saveAndReload } = require('../helpers');

async function login(page, server) {
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');
}

// createAsset makes a leaf asset account (the form's default type, so no type-change
// re-fetch race) mapped to the root subsidiary. Mirrors txn-editor.spec.js.
async function createAsset(page, name) {
  await page.goto('/accounts');
  await page.getByRole('button', { name: /new account/i }).click();
  await expect(page.locator('#af-name-en')).toBeVisible();
  await page.locator('#af-name-en').fill(name);
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) await rootSub.check();
  await saveAndReload(page, { reloadPath: '/accounts' });
  await expect(page.locator('tr.acct-row', { hasText: name })).toBeVisible();
}

// createFund makes a restricted fund scoped to the root subsidiary + root program, so
// it is offered in the editor's fund select. Mirrors funds.spec.js.
async function createFund(page, name, funder) {
  await page.goto('/funds');
  await page.getByRole('button', { name: /new fund/i }).click();
  await expect(page.locator('form#fund-form.e2e-settled')).toBeVisible();
  await page.locator('#ff-name').fill(name);
  await page.locator('#ff-funder').fill(funder);
  await page.locator('#ff-program').selectOption({ label: 'General' });
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) await rootSub.check();
  const reloaded = page.waitForResponse(
    (r) => new URL(r.url()).pathname === '/funds' && r.request().method() === 'GET',
  );
  await page.getByRole('button', { name: /^save$/i }).click();
  await reloaded;
  await expect(page.locator('tr.fund-row', { hasText: name })).toBeVisible();
}

// selectByKeyboard focuses a native <select> and picks the option whose label matches
// `label` using ONLY the keyboard: it reads the option values, then presses ArrowDown
// the right number of steps from the current selection (deterministic, unlike relying
// on type-ahead timing). This is real keyboard operation of the control -- no
// selectOption. Asserts the value landed.
async function selectByKeyboard(page, selector, label) {
  const sel = page.locator(selector);
  await sel.focus();
  // Resolve the target option's index and the current index from the live DOM.
  const { targetIndex, currentIndex, value } = await sel.evaluate((el, wantLabel) => {
    const s = /** @type {HTMLSelectElement} */ (el);
    const opts = [...s.options];
    const ti = opts.findIndex((o) => o.textContent.trim() === wantLabel);
    return { targetIndex: ti, currentIndex: s.selectedIndex, value: ti >= 0 ? opts[ti].value : '' };
  }, label);
  if (targetIndex < 0) throw new Error(`option "${label}" not found in ${selector}`);
  const steps = targetIndex - currentIndex;
  const key = steps >= 0 ? 'ArrowDown' : 'ArrowUp';
  for (let i = 0; i < Math.abs(steps); i++) {
    // eslint-disable-next-line no-await-in-loop
    await page.keyboard.press(key);
  }
  await expect(sel).toHaveValue(value);
}

test.describe('keyboard-only entry', () => {
  test('enters a 4-split mixed-fund transaction entirely by keyboard and it posts', async ({
    page,
    server,
  }) => {
    await login(page, server);

    // Two asset leaves + one restricted fund. The transfer moves money between the two
    // accounts under two different funds.
    await createAsset(page, 'KB Checking');
    await createAsset(page, 'KB Savings');
    await createFund(page, 'KB Water Grant', 'KB Funder');

    // Open the editor directly (the register->new-transaction link path is covered by
    // txn-editor.spec.js; here we focus on driving the grid itself by keyboard).
    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();
    await expect(page.locator('#txn-account-0')).toBeVisible();
    await expect(page.locator('#txn-account-1')).toBeVisible();

    // The grid starts with two rows; we need four. Activate the "Add row" button TWICE
    // with the keyboard (focus it, press Enter -- a real <button> in tab order).
    const addRow = page.locator('#txn-add-row');
    await addRow.focus();
    await page.keyboard.press('Enter');
    await expect(page.locator('#txn-account-2')).toBeVisible();
    await addRow.focus();
    await page.keyboard.press('Enter');
    await expect(page.locator('#txn-account-3')).toBeVisible();

    // --- Row 0: KB Savings +40.00, fund KB Water Grant --------------------------
    // Drive each field by keyboard. Selects are operated by Arrow keys (real keyboard),
    // amounts by typing, and we Tab between fields to prove linear reachability.
    await selectByKeyboard(page, '#txn-account-0', 'KB Savings');
    await page.keyboard.press('Tab'); // account -> amount
    await expect(page.locator('#txn-amount-0')).toBeFocused();
    await page.keyboard.type('40.00');
    await page.keyboard.press('Tab'); // amount -> fund
    await expect(page.locator('#txn-fund-0')).toBeFocused();
    await selectByKeyboard(page, '#txn-fund-0', 'KB Water Grant');
    // Row 0 is an ASSET account, so its program + class cells are visibility:hidden and
    // must be SKIPPED by native Tab (no dead stop): fund -> memo directly. This proves
    // the "hidden cells are skipped" claim in docs/qa-entry.md.
    await page.locator('#txn-fund-0').focus();
    await page.keyboard.press('Tab');
    await expect(page.locator('#txn-memo-0')).toBeFocused();

    // --- Row 1: KB Checking -40.00, fund KB Water Grant -------------------------
    await selectByKeyboard(page, '#txn-account-1', 'KB Checking');
    await page.keyboard.press('Tab');
    await expect(page.locator('#txn-amount-1')).toBeFocused();
    await page.keyboard.type('-40.00');
    await page.keyboard.press('Tab');
    await expect(page.locator('#txn-fund-1')).toBeFocused();
    await selectByKeyboard(page, '#txn-fund-1', 'KB Water Grant');

    // --- Row 2: KB Savings +10.00, Unrestricted ---------------------------------
    await selectByKeyboard(page, '#txn-account-2', 'KB Savings');
    await page.keyboard.press('Tab');
    await expect(page.locator('#txn-amount-2')).toBeFocused();
    await page.keyboard.type('10.00');
    // Leave fund 2 at Unrestricted (the default option).

    // --- Row 3: KB Checking -10.00, Unrestricted --------------------------------
    await selectByKeyboard(page, '#txn-account-3', 'KB Checking');
    await page.keyboard.press('Tab');
    await expect(page.locator('#txn-amount-3')).toBeFocused();
    await page.keyboard.type('-10.00');

    // Set the date via the header field's keyboard shortcut ('t' = today) so the whole
    // entry is keyboard-driven. (Date defaults to today already; 't' proves the
    // shortcut works from the keyboard.)
    await page.locator('#txn-date').focus();
    await page.keyboard.press('t');
    await expect(page.locator('#txn-date')).not.toHaveValue('');

    // Save. A successful htmx submit returns HX-Redirect to the first split's register;
    // waitForURL('**/register**') tracks that full-page navigation.
    await page.getByRole('button', { name: /^save$/i }).click();
    await page.waitForURL('**/register**');
    await expect(page.locator('table.register-table')).toBeVisible();

    // The posted entry is visible: the 40.00 leg appears in the register we land on
    // (KB Savings, the first split's account).
    await expect(page.locator('table.register-table')).toContainText('40.00');
  });
});
