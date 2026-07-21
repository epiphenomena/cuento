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
// p29: the create/edit form moved to its OWN PAGE (GET /funds/new and
// /funds/{id}/edit are full shell pages, no longer an inline htmx swap atop the
// list). The list's New/Edit triggers are now plain <a> LINKS. The form still uses
// hx-post, so a successful Save returns an HX-Redirect back to /funds.
//
// Selectors come straight from fund_form.tmpl / funds.tmpl / fund_statement.tmpl:
//   - New-fund trigger:  link "New fund" -> /funds/new
//   - form fields:       #ff-name, #ff-name-es, #ff-funder, #ff-program, input[name="sub_1"]
//   - purpose/notes:     textareas (#ff-purpose, #ff-notes) as of p29
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

// openNewFund navigates to the create-fund OWN PAGE (p29) and waits for the form.
async function openNewFund(page) {
  await page.goto('/funds');
  await page.getByRole('link', { name: /new fund/i }).click();
  await page.waitForURL('**/funds/new');
  await expect(page.locator('#ff-name')).toBeVisible();
}

// saveFund submits the own-page form (hx-post -> HX-Redirect) and waits for the
// navigation back to the funds list.
async function saveFund(page) {
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL(/\/funds(\?|$)/);
}

test('funds: create with checklist + program, view statement, close and reopen', async ({
  page,
  server,
}) => {
  await login(page, server);

  // --- the funds workspace loads (empty to start) ---
  await page.goto('/funds');
  await expect(page.getByRole('heading', { name: /funds/i })).toBeVisible();

  // --- create a fund on its OWN PAGE (subsidiary checklist + program + Spanish name) ---
  await openNewFund(page);
  await page.locator('#ff-name').fill('Water Grant E2E');
  await page.locator('#ff-name-es').fill('Beca Agua E2E');
  await page.locator('#ff-funder').fill('Clean Water Fund E2E');
  // Purpose is a TEXTAREA as of p29 (more space).
  await page.locator('#ff-purpose').fill('Clean water access');
  // Program scope: the seeded root program "General".
  await page.locator('#ff-program').selectOption({ label: 'General' });
  // Subsidiary checklist: check the seeded root subsidiary (id 1).
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) {
    await rootSub.check();
  }
  await saveFund(page);

  // The fund appears with its name, funder, and scope (subsidiary + program).
  const row = page.locator('tr.fund-row', { hasText: 'Water Grant E2E' });
  await expect(row).toBeVisible();
  await expect(row).toContainText('Clean Water Fund E2E');
  await expect(row).toContainText('Organization'); // subsidiary scope chip
  await expect(row).toContainText('General'); // program scope chip

  // --- edit via the OWN PAGE: the Spanish name + textarea purpose round-trip ---
  await row.getByRole('link', { name: /^edit$/i }).click();
  await page.waitForURL(/\/funds\/\d+\/edit$/);
  await expect(page.locator('#ff-name')).toHaveValue('Water Grant E2E');
  await expect(page.locator('#ff-name-es')).toHaveValue('Beca Agua E2E');
  await expect(page.locator('#ff-purpose')).toHaveValue('Clean water access');
  await page.goto('/funds'); // leave the edit page without changes

  let reloaded;

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

// A closed fund is HIDDEN from fund CHOICE pickers (here: the fund-activity report's
// fund selector), EXCEPT when the report's "show inactive" checkbox is on. This is the
// picker-exclusion arm of the inactive-fund feature. Flow: create -> present in the
// report picker -> close -> absent by default -> "show inactive" reveals it (marked
// closed) -> reopen -> present again by default. Own fund so it is independent of the
// first test.
test('funds: closed fund is hidden from the report picker until "show inactive"', async ({
  page,
  server,
}) => {
  await login(page, server);

  // --- create a fund on its OWN PAGE ---
  await openNewFund(page);
  await page.locator('#ff-name').fill('Picker Grant E2E');
  await page.locator('#ff-funder').fill('Picker Fund E2E');
  await page.locator('#ff-program').selectOption({ label: 'General' });
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) {
    await rootSub.check();
  }
  await saveFund(page);
  await expect(page.locator('tr.fund-row', { hasText: 'Picker Grant E2E' })).toBeVisible();

  let reloaded;

  const FA = '/reports/fund_activity';
  const fundSelect = page.locator('select.report-fund-select[name="fund"]');
  const fundOption = fundSelect.locator('option', { hasText: 'Picker Grant E2E' });

  // --- while ACTIVE: the fund is an offered option in the report picker ---
  await page.goto(`${FA}?scope=1`);
  await expect(fundSelect).toBeVisible();
  await expect(fundOption).toHaveCount(1);

  // --- close the fund ---
  await page.goto('/funds');
  reloaded = page.waitForResponse(
    (r) => r.url().includes('/funds') && r.request().method() === 'GET',
  );
  await page
    .locator('tr.fund-row', { hasText: 'Picker Grant E2E' })
    .getByRole('button', { name: /^close$/i })
    .click();
  await reloaded;

  // --- closed: GONE from the report picker by default (unchecked "show inactive") ---
  await page.goto(`${FA}?scope=1`);
  await expect(fundSelect).toBeVisible();
  await expect(fundOption).toHaveCount(0);

  // --- check "show inactive": the closed fund reappears (marked closed) via the OOB
  //     re-render of #rp-fund. The checkbox rides the form's change trigger. ---
  const showInactive = page.locator('input[name="show_inactive"]');
  await showInactive.check();
  await expect(fundOption).toHaveCount(1);
  await expect(fundOption).toContainText('(closed)');

  // Loading the report with ?show_inactive=1 directly also offers it (persistence).
  await page.goto(`${FA}?scope=1&show_inactive=1`);
  await expect(fundOption).toHaveCount(1);

  // --- reopen (Reactivate): the fund is offered again by DEFAULT (no checkbox) ---
  await page.goto('/funds?closed=1');
  reloaded = page.waitForResponse(
    (r) => r.url().includes('/funds') && r.request().method() === 'GET',
  );
  await page
    .locator('tr.fund-row', { hasText: 'Picker Grant E2E' })
    .getByRole('button', { name: /^reopen$/i })
    .click();
  await reloaded;
  await page.goto(`${FA}?scope=1`);
  await expect(fundOption).toHaveCount(1);
  await expect(fundOption).not.toContainText('(closed)');
});
