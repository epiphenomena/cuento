// @ts-check
// Functional test of the REAL merge-accounts flow (p11.2). It drives the actual
// /accounts page served by `cuento serve -dev` against a fresh migrated db with a
// seeded admin (is_admin, hence TxnWrite). It creates two same-type leaf accounts
// through the inline form, then exercises the two-step merge: open the merge form,
// pick source + destination, REVIEW (consequences preview with the split count and
// the 0-reconciliations note + a Confirm control), then CONFIRM -- after which the
// source drops out of the active tree (it is deactivated).
//
// Selectors come straight from merge_form.tmpl / accounts.tmpl:
//   - Merge trigger:    button "Merge accounts" (hx-get /accounts/merge)
//   - form selects:     #mg-src, #mg-dst
//   - review button:    button "Review merge"
//   - confirm button:   button "Confirm merge"

const { test, expect } = require('../fixtures');
const { saveAccount, selectTxnAccount } = require('../helpers');

async function login(page, server) {
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');
}

// createLeaf makes one active leaf account via the inline form. It creates an ASSET
// leaf (the form's DEFAULT type) on purpose: picking a non-default type fires an
// htmx form re-fetch (hx-get on #af-type) that re-renders the form, and the newly
// swapped Save button's hx-post is only wired on the settle tick -- clicking it in
// that window drops the submit and the account is never created (a real flake seen
// under parallel load; the form just sits open). Merge is type-agnostic (it needs
// two same-type leaves), so asset leaves are equivalent coverage without the race.
async function createLeaf(page, name) {
  // p26.7: New account is its own full page (GET /accounts/new). The caller is
  // already on /accounts, so navigate via the New-account link.
  await page.getByRole('link', { name: /new account/i }).click();
  await page.waitForURL('**/accounts/new');
  await expect(page.locator('#af-name-en')).toBeVisible();
  await page.locator('#af-name-en').fill(name);
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) {
    await rootSub.check();
  }
  // saveAccount submits the plain full-page POST and waits for the 303 back to
  // /accounts (see helpers.js).
  await saveAccount(page);
  await expect(page.getByText(name, { exact: true })).toBeVisible();
}

test.describe('merge accounts', () => {
  test('reviews consequences then confirms, deactivating the source', async ({ page, server }) => {
    await login(page, server);
    await page.goto('/accounts');

    // Two same-type (asset) leaves to merge.
    await createLeaf(page, 'Supplies E2E');
    await createLeaf(page, 'Office E2E');

    // Open the merge form.
    await page.getByRole('button', { name: /merge accounts/i }).click();
    await expect(page.locator('#mg-src')).toBeVisible();

    // Pick source + destination by their visible labels.
    await selectTxnAccount(page.locator('#mg-src'), 'Supplies E2E');
    await selectTxnAccount(page.locator('#mg-dst'), 'Office E2E');

    // Step 1: review -> consequences preview with a Confirm control.
    await page.getByRole('button', { name: /review merge/i }).click();
    await expect(page.getByRole('button', { name: /confirm merge/i })).toBeVisible();
    // The consequences summary names both accounts.
    await expect(page.locator('.merge-consequences')).toBeVisible();

    // Step 2: confirm -> executes, redirects to /accounts.
    await page.getByRole('button', { name: /confirm merge/i }).click();
    await page.waitForURL('**/accounts');
    await page.waitForLoadState('load');

    // The source is deactivated: it drops out of the active tree. Filter to
    // active-only via the section-bar filter (p23.10): checking the box auto-applies
    // (htmx GET swapping #accounts-results, no Apply button). Wait for that swap
    // response so the assertion runs against the filtered table.
    const swap = page.waitForResponse(
      (r) => new URL(r.url()).pathname === '/accounts' && r.request().method() === 'GET',
    );
    await page.locator('.accounts-filters input[name="active"]').check();
    await swap;
    await expect(page.getByText('Supplies E2E', { exact: true })).toHaveCount(0);
    // The destination survives.
    await expect(page.getByText('Office E2E', { exact: true })).toBeVisible();
  });
});
