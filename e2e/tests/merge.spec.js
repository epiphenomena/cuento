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

async function login(page, server) {
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');
}

// createLeaf makes one active leaf account of the given type via the inline form.
async function createLeaf(page, name, type) {
  await page.getByRole('button', { name: /new account/i }).click();
  await expect(page.locator('#af-name-en')).toBeVisible();
  await page.locator('#af-type').selectOption(type);
  await page.locator('#af-name-en').fill(name);
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) {
    await rootSub.check();
  }
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL('**/accounts');
  // The save responds with an htmx HX-Redirect; wait for the redirect-driven load
  // to fully settle before the next action, or a following goto/click can abort
  // the in-flight navigation (net::ERR_ABORTED).
  await page.waitForLoadState('load');
  await expect(page.getByText(name, { exact: true })).toBeVisible();
}

test.describe('merge accounts', () => {
  test('reviews consequences then confirms, deactivating the source', async ({ page, server }) => {
    await login(page, server);
    await page.goto('/accounts');

    // Two same-type expense leaves to merge.
    await createLeaf(page, 'Supplies E2E', 'expense');
    await createLeaf(page, 'Office E2E', 'expense');

    // Open the merge form.
    await page.getByRole('button', { name: /merge accounts/i }).click();
    await expect(page.locator('#mg-src')).toBeVisible();

    // Pick source + destination by their visible labels.
    await page.locator('#mg-src').selectOption({ label: 'Supplies E2E' });
    await page.locator('#mg-dst').selectOption({ label: 'Office E2E' });

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
    // active-only via the REAL filter form (a plain JS-free GET form) rather than a
    // raw page.goto right after the htmx redirect (which races the in-flight
    // navigation -> intermittent net::ERR_ABORTED). This also exercises the filter.
    await page.locator('form.filters input[name="active"]').check();
    await page.locator('form.filters button[type="submit"]').click();
    await page.waitForLoadState('load');
    await expect(page.getByText('Supplies E2E', { exact: true })).toHaveCount(0);
    // The destination survives.
    await expect(page.getByText('Office E2E', { exact: true })).toBeVisible();
  });
});
