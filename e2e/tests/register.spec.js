// @ts-check
// Functional test of the REAL account register (p12.1). It drives the actual
// /accounts/{id}/register page served by `cuento serve -dev` against a fresh
// migrated db with a seeded admin (is_admin -> TxnRead). It logs in, creates a
// reconcilable asset account through the inline chart-of-accounts form, follows the
// per-row "Register" link, and asserts the register page renders: the heading, the
// filter form, the column headers, and -- because the account is reconcilable --
// the recon column (a p16-wired placeholder gated on the reconcilable flag here).
// Selectors come from register.tmpl / accounts.tmpl.

const { test, expect } = require('../fixtures');
const { saveAndReload } = require('../helpers');

async function login(page, server) {
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');
}

test.describe('account register', () => {
  test('opens a reconcilable account register from the chart of accounts', async ({ page, server }) => {
    await login(page, server);
    await page.goto('/accounts');

    // Create a reconcilable asset account to open a register for.
    await page.getByRole('button', { name: /new account/i }).click();
    await page.locator('#af-name-en').fill('Checking E2E');
    await page.locator('#af-type').selectOption('asset');
    const rootSub = page.locator('input[name="sub_1"]');
    if (!(await rootSub.isChecked())) {
      await rootSub.check();
    }
    // Reconcilable flag -> the register renders the recon column.
    const recon = page.locator('input[name="reconcilable"]');
    if (!(await recon.isChecked())) {
      await recon.check();
    }
    await saveAndReload(page, { reloadPath: '/accounts' });

    // Follow the per-row Register link.
    const row = page.locator('tr.acct-row', { hasText: 'Checking E2E' });
    await row.getByRole('link', { name: /^register$/i }).click();
    await page.waitForURL('**/register');

    // The register page renders its heading, the filter form, and the columns.
    await expect(page.getByRole('heading', { name: /register/i })).toBeVisible();
    await expect(page.locator('form.filters')).toBeVisible();
    await expect(page.locator('table.register-table')).toBeVisible();
    await expect(page.locator('table.register-table thead')).toContainText(/amount/i);

    // Reconcilable -> the recon column header is present.
    await expect(page.locator('th[data-col="recon"]')).toBeVisible();

    // No transactions yet -> the empty-state row shows.
    await expect(page.locator('tr.reg-empty')).toBeVisible();
  });
});
