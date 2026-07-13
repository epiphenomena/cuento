// @ts-check
// Functional test of the REAL budget-management flow (p19.3). Drives the actual
// /budgets, /schedules, and budget-line pages served by `cuento serve -dev` against
// the worker's fresh migrated db (seeded admin = is_admin, hence TxnRead view +
// TxnWrite manage; seeded root subsidiary "Organization" id 1 + root program
// "General"). A fresh db has NO revenue/expense accounts and NO funds, so the flow
// first creates a revenue account (via /accounts) and a fund (via /funds), then the
// schedule + budget + line.
//
// The whole flow is ONE test that logs in ONCE (the worker-scoped fixture shares one
// server + login rate limiter across the worker). It exercises:
//   1. a KIND-SPECIFIC schedule: monthly, day-15, prev_business_day. The kind picker
//      SHOWS/HIDES field blocks via budgetkind.js (no server round-trip, so no htmx
//      settle race on the kind select); we assert the monthly block is visible.
//   2. a budget (name + period).
//   3. a budget line (sub / R-E account / fund / program / amount / that schedule)
//      whose fund/account options are sub-scoped -- the line form is an inline htmx
//      swap, so we wait for `e2e-settled` before driving it (fixtures.js settle
//      marker). The line then appears on the budget detail.
//
// Selectors come straight from schedule_form.tmpl / budgets.tmpl / budget_detail.tmpl
// / budget_line_form.tmpl.

const { test, expect } = require('../fixtures');
const { saveAndReload } = require('../helpers');

async function login(page, server) {
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');
}

test('budgets: create schedule + revenue account + fund + budget + line', async ({
  page,
  server,
}) => {
  await login(page, server);

  // --- prerequisite: a revenue leaf account (a budget line needs an R/E account) ---
  await page.goto('/accounts');
  await page.getByRole('button', { name: /new account/i }).click();
  await expect(page.locator('#af-name-en')).toBeVisible();
  // The type select re-fetches the form on change (hx-get); wait for the swap to
  // settle before filling the re-rendered fields.
  await page.locator('#af-type').selectOption('revenue');
  await expect(page.locator('form#account-form.e2e-settled')).toBeVisible();
  await page.locator('#af-name-en').fill('Grants Revenue E2E');
  await page.locator('#af-name-es').fill('Ingresos por Subvenciones E2E');
  const acctSub = page.locator('input[name="sub_1"]');
  if (!(await acctSub.isChecked())) await acctSub.check();
  await saveAndReload(page, { reloadPath: '/accounts' });
  await expect(page.getByText('Grants Revenue E2E')).toBeVisible();

  // --- prerequisite: a fund scoped to the root subsidiary ---
  await page.goto('/funds');
  await page.getByRole('button', { name: /new fund/i }).click();
  await expect(page.locator('form#fund-form.e2e-settled')).toBeVisible();
  await page.locator('#ff-name').fill('Program Fund E2E');
  const fundSub = page.locator('input[name="sub_1"]');
  if (!(await fundSub.isChecked())) await fundSub.check();
  let reloaded = page.waitForResponse(
    (r) => new URL(r.url()).pathname === '/funds' && r.request().method() === 'GET',
  );
  await page.getByRole('button', { name: /^save$/i }).click();
  await reloaded;
  await expect(page.locator('tr.fund-row', { hasText: 'Program Fund E2E' })).toBeVisible();

  // --- create a schedule: monthly, day 15, previous business day ---
  await page.goto('/schedules');
  await expect(page.getByRole('heading', { name: /schedules/i })).toBeVisible();
  await page.getByRole('button', { name: /new schedule/i }).click();
  await expect(page.locator('form#schedule-form.e2e-settled')).toBeVisible();
  await page.locator('#sf-name').fill('Monthly 15th E2E');
  // Default kind is monthly; the monthly block (with #sf-dom) is visible, the custom
  // block is hidden by budgetkind.js.
  await expect(page.locator('#sf-kind')).toHaveValue('monthly');
  await expect(page.locator('#sf-dom')).toBeVisible();
  await page.locator('#sf-dom').selectOption('15');
  await page.locator('#sf-weekend').selectOption('prev_business_day');
  await saveAndReload(page, { reloadPath: '/schedules', formSelector: 'form#schedule-form' });
  await expect(page.locator('tr.schedule-row', { hasText: 'Monthly 15th E2E' })).toBeVisible();

  // --- create a SEMIMONTHLY schedule: exercises the kind picker SWITCH + the
  //     kind-unique field names. Selecting 'semimonthly' hides the monthly block and
  //     shows the semimonthly block (budgetkind.js, no server round-trip); the two
  //     day-of-month selects submit under sm_day_of_month / day_of_month_2, NOT the
  //     monthly block's shared day_of_month, so the store accepts it (a shared name
  //     would read the hidden monthly block's "None" and 422). ---
  await page.getByRole('button', { name: /new schedule/i }).click();
  await expect(page.locator('form#schedule-form.e2e-settled')).toBeVisible();
  await page.locator('#sf-name').fill('Semimonthly E2E');
  await page.locator('#sf-kind').selectOption('semimonthly');
  // The switch is client-side (no swap): the monthly day select hides, the two
  // semimonthly day selects appear.
  await expect(page.locator('#sf-dom')).toBeHidden();
  await expect(page.locator('#sf-dom-a')).toBeVisible();
  await page.locator('#sf-dom-a').selectOption('15');
  await page.locator('#sf-dom-b').selectOption('-1'); // last day
  await saveAndReload(page, { reloadPath: '/schedules', formSelector: 'form#schedule-form' });
  await expect(page.locator('tr.schedule-row', { hasText: 'Semimonthly E2E' })).toBeVisible();

  // --- create a budget ---
  await page.goto('/budgets');
  await expect(page.getByRole('heading', { name: /budgets/i })).toBeVisible();
  await page.getByRole('button', { name: /new budget/i }).click();
  await expect(page.locator('form#budget-form.e2e-settled')).toBeVisible();
  await page.locator('#bf-name').fill('FY2025 E2E');
  await page.locator('#bf-start').fill('2025-01-01');
  await page.locator('#bf-end').fill('2025-12-31');
  // Save redirects to the budget DETAIL (/budgets/{id}); wait for that navigation.
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL('**/budgets/*');
  await expect(page.getByRole('heading', { name: 'FY2025 E2E' })).toBeVisible();

  // --- add a budget line (sub / R-E account / fund / program / amount / schedule) ---
  await page.getByRole('button', { name: /add line/i }).click();
  await expect(page.locator('form#line-form.e2e-settled')).toBeVisible();
  // The subsidiary defaults to the root; the account + fund options are scoped to it.
  await page.locator('#lf-account').selectOption({ label: 'Grants Revenue E2E' });
  await page.locator('#lf-fund').selectOption({ label: 'Program Fund E2E' });
  await page.locator('#lf-program').selectOption({ label: 'General' });
  await page.locator('#lf-amount').fill('500.00');
  await page.locator('#lf-schedule').selectOption({ label: 'Monthly 15th E2E' });
  // Save redirects back to the budget detail (same pathname); wait for the reload.
  const budgetPath = new URL(page.url()).pathname;
  reloaded = page.waitForResponse(
    (r) => new URL(r.url()).pathname === budgetPath && r.request().method() === 'GET',
  );
  await page.getByRole('button', { name: /^save$/i }).click();
  await reloaded;

  // The line appears on the budget detail with its account + fund + schedule.
  const row = page.locator('tr.budget-line-row', { hasText: 'Grants Revenue E2E' });
  await expect(row).toBeVisible();
  await expect(row).toContainText('Program Fund E2E');
  await expect(row).toContainText('Monthly 15th E2E');
});
