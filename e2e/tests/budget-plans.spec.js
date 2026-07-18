// @ts-check
// Functional test of the p27.2 split-derived budget-plan flow. Drives the REAL
// /budget-plans pages served by `cuento serve -dev` against the worker's fresh migrated
// db (seeded admin = is_admin -> TxnRead view + TxnWrite manage; seeded root subsidiary
// "Organization" id 1 + root program "General").
//
// A fresh db has no R/E accounts and no open_item A/R account, so as admin the flow
// first creates: a revenue leaf (R/E -> program REQUIRED) and an open_item ASSET leaf
// (A/R -> program FORBIDDEN). Then a budget plan, then the split-entry GRID:
//   - an R/E split WITH a program saves fine;
//   - an A/L (open_item) split with NO program saves fine;
//   - the program rule is proven both ways: an R/E row with NO program is REJECTED
//     (error.budget_plan.program_required), and an A/L row WITH a program is REJECTED
//     (error.budget_plan.program_forbidden).
// Then the flat-CSV import appends a row and it appears.
//
// ONE login (worker-scoped fixture shares the login rate limiter). Selectors come from
// budget_plans.tmpl / budget_plan_form.tmpl / budget_plan_detail.tmpl. No
// page.waitForFunction (strict CSP). RULE 11: all data synthetic.

const { test, expect } = require('../fixtures');
const { openNewAccount, saveAccount } = require('../helpers');

function unique() {
  return Math.random().toString(36).slice(2, 8);
}

async function login(page, server) {
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');
}

test('budget-plans: create plan, R/E + open-item splits, program rule, CSV import', async ({
  page,
  server,
}) => {
  const suffix = unique();
  const revName = `Plan Revenue E2E ${suffix}`;
  const arName = `Plan Receivable E2E ${suffix}`;
  const planName = `Cash-Flow Plan E2E ${suffix}`;

  await login(page, server);

  // --- prerequisite: a revenue leaf account (R/E -> program required) ---
  await openNewAccount(page);
  await page.locator('#af-type').selectOption('revenue');
  await expect(page.locator('form#account-form.e2e-settled')).toBeVisible();
  await page.locator('#af-name-en').fill(revName);
  await page.locator('#af-name-es').fill(`${revName} ES`);
  let acctSub = page.locator('input[name="sub_1"]');
  if (!(await acctSub.isChecked())) await acctSub.check();
  await saveAccount(page);
  await expect(page.getByText(revName)).toBeVisible();

  // --- prerequisite: an OPEN-ITEM asset leaf (A/R -> program forbidden) ---
  await openNewAccount(page);
  await page.locator('#af-type').selectOption('asset');
  await expect(page.locator('form#account-form.e2e-settled')).toBeVisible();
  await page.locator('#af-name-en').fill(arName);
  await page.locator('#af-name-es').fill(`${arName} ES`);
  // The open_item checkbox is gated to asset/liability (p27.1b); it is now visible.
  await page.locator('input[name="open_item"]').check();
  acctSub = page.locator('input[name="sub_1"]');
  if (!(await acctSub.isChecked())) await acctSub.check();
  await saveAccount(page);
  await expect(page.getByText(arName)).toBeVisible();

  // --- create a budget plan (name + subsidiary) ---
  await page.goto('/budget-plans');
  await expect(page.getByRole('heading', { name: /budget plans/i })).toBeVisible();
  await page.locator('#new-budget-plan').click();
  await page.locator('#bpf-name').fill(planName);
  // Save redirects to the plan detail (/budget-plans/{id}).
  await page.locator('#budget-plan-create').click();
  await page.waitForURL('**/budget-plans/*');
  await expect(page.getByRole('heading', { name: planName })).toBeVisible();
  const planPath = new URL(page.url()).pathname;

  // ===== program rule (both directions) =====
  // R/E row with NO program -> rejected (program_required).
  await page.locator('#bs-account-0').selectOption({ label: revName });
  await page.locator('#bs-date-0').fill('2026-03-15');
  await page.locator('#bs-amount-0').fill('400.00');

  // p28.20 clone-safe datefield: filling row 0 makes the grid auto-append a fresh
  // trailing row (budgetgrid.js cloneRow -> reEnhanceDates re-runs enhance() on the
  // cloned, already-enhanced date input). The shared enhancer must unwrap the stale
  // clone-copied wrap first, so EACH date cell still has exactly ONE calendar (pick)
  // button — no duplicate. Assert one date input, one wrap, one button per cell.
  await expect(page.locator('.bs-date-cell')).toHaveCount(2); // row 0 + the appended empty
  for (const cell of await page.locator('.bs-date-cell').all()) {
    await expect(cell.locator('input.js-datefield')).toHaveCount(1);
    await expect(cell.locator('.datefield-wrap')).toHaveCount(1);
    await expect(cell.locator('.datefield-pick')).toHaveCount(1);
  }

  // Leave program unset (value 0).
  await page.locator('#budget-save-splits').click();
  await expect(page.locator('.bs-row-error .field-error')).toBeVisible();

  // Now add a program to that R/E row -> saves.
  await page.locator('#bs-program-0').selectOption({ label: 'General' });
  let reload = page.waitForResponse(
    (r) => new URL(r.url()).pathname === planPath && r.request().method() === 'GET',
  );
  await page.locator('#budget-save-splits').click();
  await reload;
  await expect(page.locator('#bs-account-0')).toHaveValue(/\d+/);

  // A/L (open_item) row WITH a program -> rejected (program_forbidden). The saved R/E
  // row occupies index 0; the empty scaffold is a later index -- use the first empty
  // account select the grid auto-appended.
  // Re-open the detail so indices are deterministic (row 0 = the saved R/E split).
  await page.goto(planPath);
  await page.locator('#bs-account-1').selectOption({ label: arName });
  await page.locator('#bs-date-1').fill('2026-04-01');
  await page.locator('#bs-amount-1').fill('250.00');
  await page.locator('#bs-program-1').selectOption({ label: 'General' });
  await page.locator('#budget-save-splits').click();
  await expect(page.locator('.bs-row-error .field-error')).toBeVisible();

  // Clear the program on the A/L row -> saves (A/L carries none).
  await page.locator('#bs-program-1').selectOption({ value: '0' });
  reload = page.waitForResponse(
    (r) => new URL(r.url()).pathname === planPath && r.request().method() === 'GET',
  );
  await page.locator('#budget-save-splits').click();
  await reload;
  // Both splits are now persisted (two content rows + a trailing empty).
  await expect(page.locator('#bs-account-0')).toHaveValue(/\d+/);
  await expect(page.locator('#bs-account-1')).toHaveValue(/\d+/);

  // ===== flat-CSV import =====
  // Append one revenue row (with a program name) via the import. account resolves by
  // name; the row appears after the redirect.
  const csv = `description,date,account,fund,program,amount\nImported gift,2026-05-01,${revName},,General,150.00\n`;
  await page.locator('#budget-import-file').setInputFiles({
    name: 'budget.csv',
    mimeType: 'text/csv',
    buffer: Buffer.from(csv),
  });
  reload = page.waitForResponse(
    (r) => new URL(r.url()).pathname === planPath && r.request().method() === 'GET',
  );
  await page.locator('#budget-import-submit').click();
  await reload;
  // The imported row's description is prefilled somewhere in the grid.
  await expect(page.locator('input.bs-desc[value="Imported gift"]')).toBeVisible();

  // ===== plan management: rename + notes, then delete (p27.3c) =====
  // The "Manage plan" disclosure holds the rename/notes edit and the delete.
  await page.locator('details.plan-manage > summary').click();
  await page.locator('#plan-name').fill('Renamed Plan');
  await page.locator('#plan-notes').fill('updated notes');
  let renamed = page.waitForResponse(
    (r) => new URL(r.url()).pathname === planPath && r.request().method() === 'GET',
  );
  await page.locator('.plan-edit-form button[type="submit"]').click();
  await renamed;
  // The new name shows in the header.
  await expect(page.locator('h1')).toHaveText('Renamed Plan');

  // Delete removes the plan and redirects to the list; the renamed plan is gone.
  await page.locator('details.plan-manage > summary').click();
  await page.locator('#plan-delete-btn').click();
  await page.waitForURL('**/budget-plans');
  await expect(page.getByText('Renamed Plan')).toHaveCount(0);
});
