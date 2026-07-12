// @ts-check
// Functional test of the REAL chart-of-accounts flow (p11.1). It drives the
// actual /accounts page served by `cuento serve -dev` against a fresh migrated db
// with a seeded admin (who is is_admin, hence TxnWrite). It logs in, creates an
// account through the real inline htmx form, asserts it appears in the tree, edits
// it, and proves a bad submit shows the localized field error inline (the p10.3
// form-error convention). Selectors come straight from account_form.tmpl /
// accounts.tmpl:
//   - New-account trigger:  button "New account" (hx-get /accounts/new)
//   - form fields:          #af-name-en, #af-name-es, #af-type, #af-currency
//   - subsidiary checklist: input[name="sub_1"] (the root "Organization")
//   - inline field error:   p.field-error (rendered {{t error.account.*}})

const { test, expect } = require('../fixtures');
const { saveAndReload } = require('../helpers');

// login signs the admin in and lands on the authenticated shell. Reused by every
// test here (no storageState wiring in this suite; a fresh login is cheap).
async function login(page, server) {
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');
}

test.describe('chart of accounts', () => {
  test('creates an account through the inline form and it appears in the tree', async ({ page, server }) => {
    await login(page, server);

    await page.goto('/accounts');
    await expect(page.getByRole('heading', { name: /chart of accounts/i })).toBeVisible();

    // Open the inline create form (htmx swaps #account-form).
    await page.getByRole('button', { name: /new account/i }).click();
    await expect(page.locator('#af-name-en')).toBeVisible();

    // Fill a valid create: en + es names, type asset, root subsidiary checked.
    await page.locator('#af-name-en').fill('Petty Cash E2E');
    await page.locator('#af-name-es').fill('Caja Chica E2E');
    await page.locator('#af-type').selectOption('asset');
    // The root subsidiary (id 1, "Organization") is pre-checked on a new account;
    // ensure it is checked so the store's >=1-subsidiary rule is satisfied.
    const rootSub = page.locator('input[name="sub_1"]');
    if (!(await rootSub.isChecked())) {
      await rootSub.check();
    }

    // Success is a server redirect to /accounts; the new account is in the tree.
    await saveAndReload(page, { reloadPath: '/accounts' });
    await expect(page.getByText('Petty Cash E2E')).toBeVisible();
  });

  test('a bad submit shows the localized field error inline', async ({ page, server }) => {
    await login(page, server);
    await page.goto('/accounts');

    await page.getByRole('button', { name: /new account/i }).click();
    await expect(page.locator('#af-name-en')).toBeVisible();

    // Leave the English name blank -> the store rejects with ErrNameRequired, which
    // the handler maps to error.account.name_required and re-renders the form region
    // at 422 (htmx swaps it in). The field error must appear inline.
    await page.locator('#af-name-en').fill('');
    const rootSub = page.locator('input[name="sub_1"]');
    if (!(await rootSub.isChecked())) {
      await rootSub.check();
    }
    await page.getByRole('button', { name: /^save$/i }).click();

    // The inline localized error is shown, and we did NOT navigate away.
    await expect(page.locator('p.field-error')).toBeVisible();
    await expect(page.locator('p.field-error')).toContainText(/english name is required/i);
    expect(new URL(page.url()).pathname).toBe('/accounts');
  });

  test('edits an existing account and the new name shows in the tree', async ({ page, server }) => {
    await login(page, server);
    await page.goto('/accounts');

    // Create one to edit.
    await page.getByRole('button', { name: /new account/i }).click();
    await page.locator('#af-name-en').fill('Editable E2E');
    await page.locator('#af-type').selectOption('asset');
    const rootSub = page.locator('input[name="sub_1"]');
    if (!(await rootSub.isChecked())) {
      await rootSub.check();
    }
    await saveAndReload(page, { reloadPath: '/accounts' });
    await expect(page.getByText('Editable E2E')).toBeVisible();

    // Open its edit form (the row's Edit button swaps #account-form).
    const row = page.locator('tr.acct-row', { hasText: 'Editable E2E' });
    await row.getByRole('button', { name: /^edit$/i }).click();
    await expect(page.locator('#af-name-en')).toHaveValue('Editable E2E');

    await page.locator('#af-name-en').fill('Renamed E2E');
    await saveAndReload(page, { reloadPath: '/accounts' });
    await expect(page.getByText('Renamed E2E')).toBeVisible();
    await expect(page.getByText('Editable E2E')).toHaveCount(0);
  });
});
