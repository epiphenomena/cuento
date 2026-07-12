// @ts-check
// Functional test of the REAL programs-management flow (p11.5). It drives the
// actual /programs page served by `cuento serve -dev` against the worker's fresh
// migrated db with a seeded admin (is_admin, hence TxnRead view + TxnWrite manage).
//
// The whole flow is ONE test that logs in ONCE and exercises create -> edit ->
// blocked-deactivate sequentially. Keeping it to a single login matters: the
// worker-scoped fixture shares one server (and its login rate limiter, burst 5 per
// ip+user) across every spec on the worker, so each spec keeps its login count
// small. Selectors come straight from program_form.tmpl / programs.tmpl:
//   - New-program trigger: button "New program" (hx-get /programs/new)
//   - form fields:         #pf-name, #pf-parent
//   - guard message:       p.field-error (rendered {{t error.program.*}})
//
// Programs are a DIMENSION (D24): a single seeded root ("General") exists, so every
// created program is a child; the root is immovable and deactivating a program with
// active children is blocked.

const { test, expect } = require('../fixtures');
const { saveAndReload } = require('../helpers');

test('programs: create child, rename, and blocked deactivate', async ({ page, server }) => {
  // Log in once (admin => TxnWrite, hence manage).
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');

  // --- the tree lists the seeded root program ("General") ---
  await page.goto('/programs');
  await expect(page.getByRole('heading', { name: /programs/i })).toBeVisible();
  await expect(page.getByRole('cell', { name: 'General', exact: true })).toBeVisible();

  // --- create a child program through the inline form ---
  await page.getByRole('button', { name: /new program/i }).click();
  await expect(page.locator('#pf-name')).toBeVisible();
  await page.locator('#pf-name').fill('Outreach E2E'); // parent defaults to root
  await saveAndReload(page, { reloadPath: '/programs', formSelector: 'form#program-form' });
  await expect(page.getByRole('cell', { name: 'Outreach E2E', exact: true })).toBeVisible();

  // --- rename it and the new name shows in the tree ---
  const row = page.locator('tr.prog-row', { hasText: 'Outreach E2E' });
  await row.getByRole('button', { name: /^edit$/i }).click();
  await expect(page.locator('#pf-name')).toHaveValue('Outreach E2E');
  await page.locator('#pf-name').fill('Renamed Program E2E');
  await saveAndReload(page, { reloadPath: '/programs', formSelector: 'form#program-form' });
  await expect(page.getByRole('cell', { name: 'Renamed Program E2E', exact: true })).toBeVisible();
  await expect(page.getByRole('cell', { name: 'Outreach E2E', exact: true })).toHaveCount(0);

  // --- a blocked deactivate (active child) shows the localized guard message ---
  await page.getByRole('button', { name: /new program/i }).click();
  await page.locator('#pf-name').fill('Parent Program E2E');
  await saveAndReload(page, { reloadPath: '/programs', formSelector: 'form#program-form' });

  await page.getByRole('button', { name: /new program/i }).click();
  await page.locator('#pf-name').fill('Kid Program E2E');
  await page.locator('#pf-parent').selectOption({ label: 'Parent Program E2E' });
  await saveAndReload(page, { reloadPath: '/programs', formSelector: 'form#program-form' });

  // Deactivating the parent is blocked; the localized guard message appears and the
  // parent stays active (no execution) -- the store's no-trace discipline (D24).
  const parentRow = page.locator('tr.prog-row', { hasText: 'Parent Program E2E' }).first();
  await parentRow.getByRole('button', { name: /deactivate/i }).click();
  await expect(page.locator('p.field-error')).toBeVisible();
  await expect(page.locator('p.field-error')).toContainText(/active children/i);
});
