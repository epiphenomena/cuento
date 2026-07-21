// @ts-check
// Functional test of the REAL merge-programs flow (p11.5b). It drives the actual
// /programs page served by `cuento serve -dev` against the worker's fresh migrated db
// with a seeded admin (is_admin, hence TxnRead view + TxnWrite manage). It mirrors
// merge.spec.js (the account-merge flow): create two child programs through the inline
// form, then exercise the two-step merge -- open the merge form, pick source +
// destination, REVIEW (consequences preview with a Confirm control), then CONFIRM --
// after which the source is deactivated (it shows the inactive badge) and the
// destination survives active.
//
// Selectors come straight from program_merge_form.tmpl / programs.tmpl:
//   - Merge trigger:  button "Merge programs" (hx-get /programs/merge)
//   - form selects:   #pmg-src, #pmg-dst
//   - review button:  button "Review merge"
//   - confirm button: button "Confirm merge"

const { test, expect } = require('../fixtures');
const { saveAndReload } = require('../helpers');

test('programs: reviews consequences then confirms, deactivating the source', async ({ page, server }) => {
  // Log in once (admin => TxnWrite, hence manage).
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');

  await page.goto('/programs');
  await expect(page.getByRole('heading', { name: /programs/i })).toBeVisible();

  // Two child programs to merge (parent defaults to the seeded root).
  await page.getByRole('button', { name: /new program/i }).click();
  await expect(page.locator('#pf-name')).toBeVisible();
  await page.locator('#pf-name').fill('Merge Src E2E');
  await saveAndReload(page, { reloadPath: '/programs', formSelector: 'form#program-form' });
  await expect(page.getByRole('cell', { name: 'Merge Src E2E', exact: true })).toBeVisible();

  await page.getByRole('button', { name: /new program/i }).click();
  await page.locator('#pf-name').fill('Merge Dst E2E');
  await saveAndReload(page, { reloadPath: '/programs', formSelector: 'form#program-form' });
  await expect(page.getByRole('cell', { name: 'Merge Dst E2E', exact: true })).toBeVisible();

  // Open the merge form.
  await page.getByRole('button', { name: /merge programs/i }).click();
  await expect(page.locator('#pmg-src')).toBeVisible();

  // Pick source + destination by their option labels (the dotted hierarchy path;
  // both programs default under the seeded root "General").
  await page.locator('#pmg-src').selectOption({ label: 'General.Merge Src E2E' });
  await page.locator('#pmg-dst').selectOption({ label: 'General.Merge Dst E2E' });

  // Step 1: review -> consequences preview with a Confirm control.
  await page.getByRole('button', { name: /review merge/i }).click();
  await expect(page.getByRole('button', { name: /confirm merge/i })).toBeVisible();
  await expect(page.locator('.merge-consequences')).toBeVisible();

  // Step 2: confirm -> executes, redirects to /programs.
  await page.getByRole('button', { name: /confirm merge/i }).click();
  await page.waitForURL('**/programs');
  await page.waitForLoadState('load');

  // The source is deactivated: its row carries the inactive badge; the destination
  // survives and stays active (no inactive badge on its row).
  const srcRow = page.locator('tr.prog-row', { hasText: 'Merge Src E2E' });
  await expect(srcRow).toHaveClass(/prog-inactive/);
  const dstRow = page.locator('tr.prog-row', { hasText: 'Merge Dst E2E' });
  await expect(dstRow).not.toHaveClass(/prog-inactive/);
});
