// @ts-check
// Functional test of payee autocomplete + autofill (p12.3). It drives the REAL
// /transactions/new grid served by `cuento serve -dev`. Because a fresh -dev db has
// no payees and there is no payee-management UI, the precondition (a payee WITH a
// prior transaction) is built THROUGH THE BROWSER via create-on-save: the first saved
// transaction types a new payee name, which the handler find-or-creates. A later
// editor then autocompletes that payee, picks it, and the prior transaction's splits
// prefill the grid. Selectors come from transaction_form.tmpl.
//
// htmx timing (see txn-editor.spec): a swapped-in node's hx-* triggers are wired on
// the SETTLE tick, after paint, so a synthetic action right after a swap can beat the
// wiring. We avoid that here by design -- the suggestion list is filled by an hx-get
// on a STABLE input (wired at load), and picking a suggestion goes through a delegated
// click listener + a manual fetch (no hx-* trigger on the freshly-swapped <li>). We
// still wait for the suggestion <li> to exist before clicking. Strict CSP blocks
// page.waitForFunction; we use locator waits (auto-retry) instead.

const { test, expect } = require('../fixtures');
const { openNewAccount, saveAccount } = require('../helpers');

async function installSettleMarker(page) {
  await page.addInitScript(() => {
    document.addEventListener('htmx:afterSettle', (e) => {
      const t = /** @type {any} */ (e.target);
      if (t && t.classList) t.classList.add('e2e-settled');
    });
  });
}

async function login(page, server) {
  await installSettleMarker(page);
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');
}

async function createAsset(page, name) {
  await openNewAccount(page);
  await page.locator('#af-name-en').fill(name);
  await page.locator('#af-type').selectOption('asset');
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) {
    await rootSub.check();
  }
  await saveAccount(page);
  await expect(page.locator('tr.acct-row', { hasText: name })).toBeVisible();
}

// enterTransfer fills a balanced 2-split transfer (savings +amt / checking -amt) on a
// blank editor and returns after the fields are set (does NOT save).
async function enterTransfer(page, savings, checking, amt) {
  await page.locator('#txn-account-0').selectOption({ label: savings });
  await page.locator('#txn-amount-0').fill(amt);
  await page.locator('#txn-account-1').selectOption({ label: checking });
  await page.locator('#txn-amount-1').fill(`-${amt}`);
}

test.describe('payee autocomplete + autofill', () => {
  test('creates a payee on save, then autofills its last transaction on a later entry', async ({ page, server }) => {
    await login(page, server);
    await createAsset(page, 'Auto Checking');
    await createAsset(page, 'Auto Savings');

    // First transaction: a typed NEW payee (create-on-save) + a balanced transfer.
    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();
    // p26.3: the payee is ONE combobox over #txn-payee (combo-input). The overlay input
    // is the .combo-text inside the payee field; the native select is the value sink.
    const payeeBox = page.locator('.txn-payee-field .combo-text');
    await expect(payeeBox).toBeVisible();
    await payeeBox.fill('Autofill Vendor');
    // A typed brand-new name must survive blur (freeText) and post via payee_name.
    // Blur onto the memo header input (a plain field, no combo overlay to intercept).
    await page.locator('#txn-memo').click();
    await expect(payeeBox).toHaveValue('Autofill Vendor');
    await expect(page.locator('#txn-payee-name')).toHaveValue('Autofill Vendor');
    await enterTransfer(page, 'Auto Savings', 'Auto Checking', '40.00');
    await page.locator('#txn-memo-0').fill('first memo');
    await page.getByRole('button', { name: /^save$/i }).click();
    await page.waitForURL('**/register**');

    // Second entry: fuzzy-filter the payee, pick it, and the prior splits prefill.
    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();
    const input = page.locator('.txn-payee-field .combo-text');
    await input.click();
    await input.fill('Auto'); // fuzzy match -> the option appears in the combo list
    const suggestion = page.locator('.txn-payee-field .combo-option', { hasText: 'Autofill Vendor' });
    await expect(suggestion).toBeVisible();
    await suggestion.click();

    // The grid now reflects the prior transaction: savings +40.00 on row 0 (memo too),
    // checking -40.00 on row 1. (allRowsEmpty was true, so autofill applied.)
    await expect(page.locator('#txn-amount-0')).toHaveValue('40.00');
    await expect(page.locator('#txn-memo-0')).toHaveValue('first memo');
    await expect(page.locator('#txn-amount-1')).toHaveValue('-40.00');
  });

  test('does NOT overwrite the grid when the user has already typed a row', async ({ page, server }) => {
    await login(page, server);
    await createAsset(page, 'Guard Checking');
    await createAsset(page, 'Guard Savings');

    // Seed a payee with a prior transaction (create-on-save).
    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();
    await page.locator('.txn-payee-field .combo-text').fill('Guard Vendor');
    await enterTransfer(page, 'Guard Savings', 'Guard Checking', '15.00');
    await page.getByRole('button', { name: /^save$/i }).click();
    await page.waitForURL('**/register**');

    // New entry: TYPE a row FIRST, then pick the payee -> autofill must NOT clobber.
    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();
    await page.locator('#txn-amount-0').fill('99.00'); // user input first
    const input = page.locator('.txn-payee-field .combo-text');
    await input.click();
    await input.fill('Guard');
    const suggestion = page.locator('.txn-payee-field .combo-option', { hasText: 'Guard Vendor' });
    await expect(suggestion).toBeVisible();
    await suggestion.click();

    // The typed amount survives (never-overwrites guard); it was NOT replaced by 15.00.
    await expect(page.locator('#txn-amount-0')).toHaveValue('99.00');
  });
});
