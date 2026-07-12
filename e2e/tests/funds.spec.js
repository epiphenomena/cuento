// @ts-check
// Functional test of the REAL funds-workspace flow (p12.5). It drives the actual
// /funds page served by `cuento serve -dev` against the worker's fresh migrated db
// with a seeded admin (is_admin, hence TxnRead view + TxnWrite manage) and the
// seeded root subsidiary ("Organization", id 1) + root program ("General").
//
// The whole flow is ONE test that logs in ONCE and exercises create (subsidiary
// checklist + program scope) -> the fund appears with its funder/scope -> its
// statement (opening/closing balances render) -> close (moves under the closed
// toggle) -> reopen. Keeping it to a single login matters: the worker-scoped fixture
// shares one server (and its login rate limiter) across every spec on the worker.
//
// Selectors come straight from fund_form.tmpl / funds.tmpl / fund_statement.tmpl:
//   - New-fund trigger:  button "New fund" (hx-get /funds/new)
//   - form fields:       #ff-name, #ff-funder, #ff-program, input[name="sub_1"]
//   - list row:          tr.fund-row (fund name links to the statement)
//   - toggle:            link "Show closed funds" / "Show active funds"
//   - statement:         table.fund-openclose (opening/closing per currency)

const { test, expect } = require('../fixtures');

async function login(page, server) {
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');
}

test('funds: create with checklist + program, view statement, close and reopen', async ({
  page,
  server,
}) => {
  await login(page, server);

  // --- the funds workspace loads (empty to start) ---
  await page.goto('/funds');
  await expect(page.getByRole('heading', { name: /funds/i })).toBeVisible();

  // --- create a fund through the inline form (subsidiary checklist + program) ---
  await page.getByRole('button', { name: /new fund/i }).click();
  // Wait for the New-fund form swap to SETTLE so htmx has wired the Save button's
  // hx-post before we click it (see the settle-marker note in fixtures.js).
  await expect(page.locator('form#fund-form.e2e-settled')).toBeVisible();
  await page.locator('#ff-name').fill('Water Grant E2E');
  await page.locator('#ff-funder').fill('Clean Water Fund E2E');
  // Program scope: the seeded root program "General".
  await page.locator('#ff-program').selectOption({ label: 'General' });
  // Subsidiary checklist: check the seeded root subsidiary (id 1).
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) {
    await rootSub.check();
  }
  // Save posts via hx-post; success returns an HX-Redirect back to GET /funds. We're
  // ALREADY on /funds, so waitForURL is a no-op that does NOT wait for the reload --
  // wait for the reload RESPONSE instead, which lands only after the write commits.
  let reloaded = page.waitForResponse(
    (r) => r.url().endsWith('/funds') && r.request().method() === 'GET',
  );
  await page.getByRole('button', { name: /^save$/i }).click();
  await reloaded;

  // The fund appears with its name, funder, and scope (subsidiary + program).
  const row = page.locator('tr.fund-row', { hasText: 'Water Grant E2E' });
  await expect(row).toBeVisible();
  await expect(row).toContainText('Clean Water Fund E2E');
  await expect(row).toContainText('Organization'); // subsidiary scope chip
  await expect(row).toContainText('General'); // program scope chip

  // --- open the fund's statement (opening/closing balances render) ---
  await row.getByRole('link', { name: 'Water Grant E2E' }).click();
  await page.waitForURL('**/funds/*');
  await expect(page.getByRole('heading', { name: /fund statement/i })).toBeVisible();
  // A fresh fund has no activity, so opening = closing = 0 is not yet present (no
  // currency rows); the statement table + empty note render.
  await expect(page.locator('table.fund-openclose')).toBeVisible();
  await expect(page.locator('table.fund-statement-table')).toBeVisible();

  // --- close the fund; it moves under the closed toggle ---
  await page.goto('/funds');
  const activeRow = page.locator('tr.fund-row', { hasText: 'Water Grant E2E' });
  reloaded = page.waitForResponse(
    (r) => r.url().includes('/funds') && r.request().method() === 'GET',
  );
  await activeRow.getByRole('button', { name: /^close$/i }).click();
  await reloaded;
  // The active list no longer shows it.
  await page.goto('/funds');
  await expect(page.locator('tr.fund-row', { hasText: 'Water Grant E2E' })).toHaveCount(0);
  // The closed toggle shows it.
  await page.getByRole('link', { name: /show closed funds/i }).click();
  await page.waitForURL('**/funds?closed=1');
  await expect(page.locator('tr.fund-row', { hasText: 'Water Grant E2E' })).toBeVisible();

  // --- reopen it; it returns to the active list ---
  const closedRow = page.locator('tr.fund-row', { hasText: 'Water Grant E2E' });
  reloaded = page.waitForResponse(
    (r) => r.url().includes('/funds') && r.request().method() === 'GET',
  );
  await closedRow.getByRole('button', { name: /^reopen$/i }).click();
  await reloaded;
  await page.goto('/funds');
  await expect(page.locator('tr.fund-row', { hasText: 'Water Grant E2E' })).toBeVisible();
});
