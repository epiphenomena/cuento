// @ts-check
// Functional test of the REAL subsidiaries-admin flow (p11.3). It drives the
// actual /admin/subsidiaries page served by `cuento serve -dev` against the
// worker's fresh migrated db with a seeded admin (is_admin, hence Admin perm).
//
// The whole flow is ONE test that logs in ONCE and exercises create -> edit ->
// blocked-deactivate sequentially. Keeping it to a single login matters: the
// worker-scoped fixture shares one server (and its login rate limiter, burst 5 per
// ip+user) across every spec on the worker, so each spec keeps its login count
// small. Selectors come straight from subsidiary_form.tmpl / subsidiaries.tmpl:
//   - New-subsidiary trigger: button "New subsidiary" (hx-get .../new)
//   - form fields:            #sf-name, #sf-parent, #sf-currency
//   - guard message:          p.field-error (rendered {{t error.subsidiary.*}})

const { test, expect } = require('../fixtures');

test('subsidiaries admin: create child, rename, and blocked deactivate', async ({ page, server }) => {
  // Log in once (admin => Admin perm).
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');

  // --- create a child subsidiary through the inline form ---
  await page.goto('/admin/subsidiaries');
  await expect(page.getByRole('heading', { name: /subsidiaries/i })).toBeVisible();
  // The seeded root ("Organization") is listed in the tree table.
  await expect(page.getByRole('cell', { name: 'Organization', exact: true })).toBeVisible();

  await page.getByRole('button', { name: /new subsidiary/i }).click();
  await expect(page.locator('#sf-name')).toBeVisible();
  await page.locator('#sf-name').fill('West Branch E2E');
  await page.locator('#sf-currency').selectOption('USD'); // parent defaults to root
  await page.getByRole('button', { name: /^save$/i }).click();

  await page.waitForURL('**/admin/subsidiaries');
  await expect(page.getByRole('cell', { name: 'West Branch E2E', exact: true })).toBeVisible();

  // --- rename it and the new name shows in the tree ---
  const row = page.locator('tr.sub-row', { hasText: 'West Branch E2E' });
  await row.getByRole('button', { name: /^edit$/i }).click();
  await expect(page.locator('#sf-name')).toHaveValue('West Branch E2E');
  await page.locator('#sf-name').fill('Renamed E2E');
  await page.getByRole('button', { name: /^save$/i }).click();

  await page.waitForURL('**/admin/subsidiaries');
  await expect(page.getByRole('cell', { name: 'Renamed E2E', exact: true })).toBeVisible();
  await expect(page.getByRole('cell', { name: 'West Branch E2E', exact: true })).toHaveCount(0);

  // --- a blocked deactivate (active child) shows the localized guard message ---
  await page.getByRole('button', { name: /new subsidiary/i }).click();
  await page.locator('#sf-name').fill('Parent E2E');
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL('**/admin/subsidiaries');

  await page.getByRole('button', { name: /new subsidiary/i }).click();
  await page.locator('#sf-name').fill('Kid E2E');
  await page.locator('#sf-parent').selectOption({ label: 'Parent E2E' });
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL('**/admin/subsidiaries');

  // Deactivating the parent is blocked; the localized guard message appears and the
  // parent stays active (no execution) -- the store's no-trace discipline.
  const parentRow = page.locator('tr.sub-row', { hasText: 'Parent E2E' }).first();
  await parentRow.getByRole('button', { name: /deactivate/i }).click();
  await expect(page.locator('p.field-error')).toBeVisible();
  await expect(page.locator('p.field-error')).toContainText(/active children/i);
});
