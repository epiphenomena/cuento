// @ts-check
// Functional test of the REAL chart-of-accounts flow (p11.1, p26.7). It drives the
// actual /accounts page served by `cuento serve -dev` against a fresh migrated db
// with a seeded admin (who is is_admin, hence TxnWrite). It logs in, creates an
// account through the real create form, asserts it appears in the tree, edits it,
// and proves a bad submit shows the localized field error (the p10.3 form-error
// convention).
//
// p26.7: the create/edit form moved OUT of the inline #account-form htmx swap onto
// dedicated full-shell pages. The New/Edit triggers are now plain links (a full-page
// navigation to GET /accounts/new and /accounts/{id}/edit), and Save is a plain POST
// that 303-redirects to /accounts on success or re-renders the WHOLE page at 422 with
// the field error + autofocus on failure. Selectors from account_form.tmpl /
// accounts.tmpl:
//   - New-account trigger:  link "New account" (-> /accounts/new)
//   - per-row Edit trigger:  link "Edit" (-> /accounts/{id}/edit)
//   - form fields:          #af-name-en, #af-name-es, #af-type, #af-currency
//   - subsidiary checklist: input[name="sub_1"] (the root "Organization")
//   - field error:          p.field-error (rendered {{t error.account.*}})

const { test, expect } = require('../fixtures');

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
  test('creates an account through the form page and it appears in the tree', async ({ page, server }) => {
    await login(page, server);

    await page.goto('/accounts');
    await expect(page.getByRole('heading', { name: /chart of accounts/i })).toBeVisible();

    // Open the create form on its OWN page (a plain navigation, p26.7).
    await page.getByRole('link', { name: /new account/i }).click();
    await page.waitForURL('**/accounts/new');
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

    // Success is a server 303-redirect back to /accounts; the new account is in the tree.
    await page.getByRole('button', { name: /^save$/i }).click();
    await page.waitForURL('**/accounts');
    await expect(page.getByText('Petty Cash E2E')).toBeVisible();
  });

  test('a bad submit re-renders the page with the localized field error', async ({ page, server }) => {
    await login(page, server);
    await page.goto('/accounts');

    await page.getByRole('link', { name: /new account/i }).click();
    await page.waitForURL('**/accounts/new');
    await expect(page.locator('#af-name-en')).toBeVisible();

    // Leave the English name blank -> the store rejects with ErrNameRequired, which
    // the handler maps to error.account.name_required and re-renders the WHOLE page
    // at 422 with the field error + native autofocus on the invalid name input.
    await page.locator('#af-name-en').fill('');
    const rootSub = page.locator('input[name="sub_1"]');
    if (!(await rootSub.isChecked())) {
      await rootSub.check();
    }
    await page.getByRole('button', { name: /^save$/i }).click();

    // The localized error is shown; we stayed on the form (POST action is /accounts).
    await expect(page.locator('p.field-error')).toBeVisible();
    await expect(page.locator('p.field-error')).toContainText(/english name is required/i);
    // Autofocus landed on the first invalid control (native on a real page render).
    await expect(page.locator('#af-name-en')).toBeFocused();
  });

  test('edits an existing account and the new name shows in the tree', async ({ page, server }) => {
    await login(page, server);
    await page.goto('/accounts');

    // Create one to edit.
    await page.getByRole('link', { name: /new account/i }).click();
    await page.waitForURL('**/accounts/new');
    await page.locator('#af-name-en').fill('Editable E2E');
    await page.locator('#af-type').selectOption('asset');
    const rootSub = page.locator('input[name="sub_1"]');
    if (!(await rootSub.isChecked())) {
      await rootSub.check();
    }
    await page.getByRole('button', { name: /^save$/i }).click();
    await page.waitForURL('**/accounts');
    await expect(page.getByText('Editable E2E')).toBeVisible();

    // Open its edit page (the row's Edit link navigates to /accounts/{id}/edit).
    const row = page.locator('tr.acct-row', { hasText: 'Editable E2E' });
    await row.getByRole('link', { name: /^edit$/i }).click();
    await page.waitForURL('**/accounts/*/edit');
    await expect(page.locator('#af-name-en')).toHaveValue('Editable E2E');

    await page.locator('#af-name-en').fill('Renamed E2E');
    await page.getByRole('button', { name: /^save$/i }).click();
    await page.waitForURL('**/accounts');
    await expect(page.getByText('Renamed E2E')).toBeVisible();
    await expect(page.getByText('Editable E2E')).toHaveCount(0);
  });

  // p26.14: the subsidiary + active-only filters are remembered in the session, so
  // a fresh navigation back to /accounts restores the last-used selection instead
  // of resetting to defaults. Sets a real filter (sub -> the root subsidiary,
  // active-only checked), navigates away, and asserts both are restored on return.
  test('remembers and restores the filter selection across navigation', async ({ page, server }) => {
    await login(page, server);
    await page.goto('/accounts');

    // Set a deliberate filter: pick the root subsidiary and check "active only".
    // The form auto-applies on change (htmx GET), which saves it to the session.
    await page.locator('#sub-filter').selectOption('1');
    await page.locator('input[name="active"]').check();
    // Let the htmx change-fetch settle (it swaps #accounts-results).
    await expect(page.locator('#accounts-results')).toBeVisible();

    // Navigate away to another in-app page (same session), then come back to a
    // BARE /accounts with no query params -- a fresh nav that must restore.
    await page.goto('/funds');
    await page.goto('/accounts');

    // The saved selection is restored from the session.
    await expect(page.locator('#sub-filter')).toHaveValue('1');
    await expect(page.locator('input[name="active"]')).toBeChecked();
  });
});
