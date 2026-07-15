// @ts-check
// Functional test of p12.4: edit / void / duplicate + the history panel. Drives the
// REAL app served by `cuento serve -dev` against a fresh migrated db with a seeded
// admin (is_admin -> TxnWrite). It creates two asset accounts, posts a balanced
// transfer through the real editor, then exercises the per-row register actions:
//   - HISTORY: after an edit, the timeline shows the changed field / split delta.
//   - VOID: the confirm flow removes the txn from the register.
//   - DUPLICATE: opens the editor prefilled as a NEW unsaved entry (posts to create).
// Selectors come from register.tmpl / transaction_form.tmpl / history.tmpl /
// void.tmpl. The per-row action links are plain full-page <a> navigations (no htmx
// swap), so they need no settle dance; the editor save is a plain submit.

const { test, expect } = require('../fixtures');
const { openNewAccount, saveAccount } = require('../helpers');

// The htmx settle marker is installed centrally by the `page` fixture (fixtures.js);
// the per-row p12.4 actions (edit/void/duplicate/history) are plain full-page <a>
// links, so only the new-account form swap needs the settle dance, handled by the
// shared saveAndReload helper.
async function login(page, server) {
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');
}

async function createAsset(page, name) {
  await openNewAccount(page);
  await page.locator('#af-name-en').fill(name);
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) {
    await rootSub.check();
  }
  await saveAccount(page);
  await expect(page.locator('tr.acct-row', { hasText: name })).toBeVisible();
}

// postTransfer opens the editor from the source account's register and posts a
// balanced 2-split transfer (DR dst amt / CR src amt), landing back on a register.
async function postTransfer(page, srcName, dstName, amt) {
  // p25.1: the account NAME is the register link (the dedicated "Register" link was
  // removed). Click the name link scoped to this account's row.
  const row = page.locator('tr.acct-row', { hasText: srcName });
  await row.getByRole('link', { name: srcName }).click();
  await page.waitForURL('**/register');
  await page.locator('main a.btn-primary', { hasText: /new transaction/i }).click();
  await page.waitForURL(/\/transactions\/new/);
  await expect(page.locator('form#txn-form')).toBeVisible();
  await page.locator('#txn-account-0').selectOption({ label: dstName });
  await page.locator('#txn-amount-0').fill(amt);
  await page.locator('#txn-account-1').selectOption({ label: srcName });
  await page.locator('#txn-amount-1').fill('-' + amt);
  await page.locator('#txn-memo').fill('original memo');
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));
}

test.describe('transaction history / void / duplicate', () => {
  test('edit then history shows the changed field', async ({ page, server }) => {
    await login(page, server);
    await createAsset(page, 'Hist Checking');
    await createAsset(page, 'Hist Savings');
    await postTransfer(page, 'Hist Checking', 'Hist Savings', '30.00');

    // Go to the source register and edit the transaction via the row action.
    await page.goto('/accounts');
    await page.locator('tr.acct-row', { hasText: 'Hist Checking' })
      .getByRole('link', { name: 'Hist Checking' }).click();
    await page.waitForURL('**/register');
    await page.locator('tr.reg-row').first()
      .getByRole('link', { name: /^edit$/i }).click();
    await page.waitForURL((u) => /\/transactions\/\d+\/edit/.test(u.pathname));
    await expect(page.locator('form#txn-form')).toBeVisible();

    // Change the memo and save.
    await page.locator('#txn-memo').fill('edited memo');
    await page.getByRole('button', { name: /^save$/i }).click();
    await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

    // Open the history for that row and assert the timeline shows both ops and the
    // edited memo value.
    await page.goto('/accounts');
    await page.locator('tr.acct-row', { hasText: 'Hist Checking' })
      .getByRole('link', { name: 'Hist Checking' }).click();
    await page.waitForURL('**/register');
    await page.locator('tr.reg-row').first()
      .getByRole('link', { name: /^history$/i }).click();
    await page.waitForURL('**/transactions/*/history');

    const timeline = page.locator('ol.history-timeline');
    await expect(timeline).toBeVisible();
    await expect(timeline).toContainText('Created');
    await expect(timeline).toContainText('Updated');
    await expect(timeline).toContainText('edited memo');
  });

  test('void with confirm removes the transaction from the register', async ({ page, server }) => {
    await login(page, server);
    await createAsset(page, 'Void Checking');
    await createAsset(page, 'Void Savings');
    await postTransfer(page, 'Void Checking', 'Void Savings', '40.00');

    // The transfer is in the source register.
    await page.goto('/accounts');
    await page.locator('tr.acct-row', { hasText: 'Void Checking' })
      .getByRole('link', { name: 'Void Checking' }).click();
    await page.waitForURL('**/register');
    await expect(page.locator('table.register-table')).toContainText('40.00');

    // Void via the row action -> the confirm-review page -> confirm.
    await page.locator('tr.reg-row').first()
      .getByRole('link', { name: /^void$/i }).click();
    await page.waitForURL('**/transactions/*/void');
    await expect(page.locator('table.void-lines')).toContainText('40.00');
    await page.getByRole('button', { name: /void this transaction/i }).click();

    // Back on the chart of accounts; the source register no longer shows the txn.
    await page.waitForURL('**/accounts');
    await page.locator('tr.acct-row', { hasText: 'Void Checking' })
      .getByRole('link', { name: 'Void Checking' }).click();
    await page.waitForURL('**/register');
    await expect(page.locator('table.register-table')).not.toContainText('40.00');
  });

  test('duplicate opens a prefilled new (unsaved) editor', async ({ page, server }) => {
    await login(page, server);
    await createAsset(page, 'Dup Checking');
    await createAsset(page, 'Dup Savings');
    await postTransfer(page, 'Dup Checking', 'Dup Savings', '55.00');

    await page.goto('/accounts');
    await page.locator('tr.acct-row', { hasText: 'Dup Checking' })
      .getByRole('link', { name: 'Dup Checking' }).click();
    await page.waitForURL('**/register');
    await page.locator('tr.reg-row').first()
      .getByRole('link', { name: /^duplicate$/i }).click();

    // The editor opens as a NEW entry (URL is /duplicate; the form is the create form
    // -- it will POST to /transactions). The source memo + amount are prefilled.
    await page.waitForURL('**/transactions/*/duplicate');
    const form = page.locator('form#txn-form');
    await expect(form).toBeVisible();
    await expect(page.locator('#txn-memo')).toHaveValue('original memo');
    // The prefilled amount survives (row 0 carries the source magnitude).
    await expect(page.locator('#txn-amount-0')).toHaveValue(/55\.00/);
    // It is a create: saving posts to /transactions and lands on a register (a NEW
    // second transaction now exists).
    await page.getByRole('button', { name: /^save$/i }).click();
    await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));
    await expect(page.locator('table.register-table')).toContainText('55.00');
  });
});
