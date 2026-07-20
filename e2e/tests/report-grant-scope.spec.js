// @ts-check
// Functional test of the p27.4c program-subtree-scoped report grant, end to end
// against the REAL `cuento serve -dev` binary on the worker's fresh migrated db.
//
// It SEEDS two sibling child programs under the seeded root ("General") -- "Grant
// Scope Alpha" and "Grant Scope Beta" -- each with its own expense account and a
// program-tagged expense, then, as the admin:
//   - creates a fresh read-only operator,
//   - grants it the "financial" report group SCOPED to the Alpha program subtree via
//     the per-group program picker on the user detail page.
// Then, logged in AS that operator, it asserts the DATA-SCOPING axis (distinct from
// route reachability):
//   - the income_statement (a program-dimensioned report in "financial") shows ONLY
//     Alpha's expense account, NOT the sibling Beta's (row-filtered to the subtree);
//   - balance_sheet (a DEMOTED, non-program report in "financial") is
//     DENIED (403) -- a purely program-scoped grant cannot reach a report that has no
//     program dimension (p27.4b).
// Finally, back as admin, it CLEARS the scope (org-wide) and confirms the operator now
// sees BOTH programs' rows in the income statement and reaches the previously-denied
// report.
//
// TEST-ISOLATION: the worker db is shared, so every seeded program/account/user name
// is UNIQUE per run and the operator is a brand-new uniquely-named user; the admin's
// own perms are never touched. Selectors are language-independent (ids, name attrs,
// marker classes). No page.waitForFunction (strict CSP); only locator/URL/response
// waits and page.request fetches.

const { test, expect } = require('../fixtures');
const { saveAndReload, openNewAccount, saveAccount, selectTxnAccount } = require('../helpers');

const IS = '/reports/income_statement';
const DEMOTED = '/reports/balance_sheet'; // demoted (non-program) report in "financial"

function unique() {
  return Math.random().toString(36).slice(2, 8);
}

async function login(page, username, password) {
  await page.goto('/login');
  await page.locator('#username').fill(username);
  await page.locator('#password').fill(password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');
}

// createProgram makes a child program under the seeded root ("General").
async function createProgram(page, name) {
  await page.goto('/programs');
  await page.getByRole('button', { name: /new program/i }).click();
  await expect(page.locator('#pf-name')).toBeVisible();
  await page.locator('#pf-name').fill(name); // parent defaults to the root program
  await saveAndReload(page, { reloadPath: '/programs', formSelector: 'form#program-form' });
  await expect(page.locator('tr.prog-row', { hasText: name })).toBeVisible();
}

async function createAsset(page, name) {
  await openNewAccount(page);
  await page.locator('#af-name-en').fill(name);
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) await rootSub.check();
  await saveAccount(page);
  await expect(page.locator('tr.acct-row', { hasText: name })).toBeVisible();
}

async function createExpenseAccount(page, name) {
  await openNewAccount(page);
  await page.locator('#af-type').selectOption('expense');
  await expect(page.locator('#af-func')).toBeVisible();
  await page.locator('#af-name-en').fill(name);
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) await rootSub.check();
  await page.locator('#af-func').selectOption('program');
  await saveAccount(page);
  await expect(page.locator('tr.acct-row', { hasText: name })).toBeVisible();
}

// postExpense books DR <expenseAcct> / CR <cashAcct> for 80.00, tagging the expense
// split with <programLabel> (rule 7 R/E program dimension). It lands the txn.
async function postExpense(page, cashAcct, expenseAcct, programLabel) {
  await page.goto('/transactions/new');
  await expect(page.locator('form#txn-form')).toBeVisible();
  await selectTxnAccount(page.locator('#txn-main-account'), cashAcct);
  await selectTxnAccount(page.locator('#txn-account-0'), expenseAcct);
  await expect(page.locator('#txn-progclass-0')).toBeVisible();
  await page.locator('#txn-progclass-0').selectOption({ label: programLabel });
  await page.locator('#txn-amount-0').fill('80.00');
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));
}

test('report grant scope: a program-subtree-scoped "financial" grant filters income-statement rows and denies a demoted report', async ({
  page,
  server,
}) => {
  const suffix = unique();
  const alphaProg = `GScope Alpha ${suffix}`;
  const betaProg = `GScope Beta ${suffix}`;
  const alphaAcct = `GScope Alpha Cost ${suffix}`;
  const betaAcct = `GScope Beta Cost ${suffix}`;
  const cash = `GScope Cash ${suffix}`;
  const operator = `gscope_${suffix}`;
  const operatorPw = 'gscope-op-passw0rd';

  await login(page, server.username, server.password);

  // --- seed: two sibling programs, a cash asset, an expense account per program, and a
  // program-tagged expense in each ---
  await createProgram(page, alphaProg);
  await createProgram(page, betaProg);
  await createAsset(page, cash);
  await createExpenseAccount(page, alphaAcct);
  await createExpenseAccount(page, betaAcct);
  await postExpense(page, cash, alphaAcct, alphaProg);
  await postExpense(page, cash, betaAcct, betaProg);

  // --- create a fresh read-only operator ---
  await page.goto('/admin/users');
  await page.getByRole('button', { name: /new user/i }).click();
  await expect(page.locator('form#user-create-form.e2e-settled')).toBeVisible();
  await page.locator('#uc-username').fill(operator);
  await page.locator('#uc-password').fill(operatorPw);
  await page.locator('#uc-perm').selectOption('read');
  const listReloaded = page.waitForResponse(
    (r) => new URL(r.url()).pathname === '/admin/users' && r.request().method() === 'GET',
  );
  await page.getByRole('button', { name: /create user/i }).click();
  await listReloaded;

  // --- grant "financial" SCOPED to the Alpha program subtree via the per-group picker ---
  const opRow = page.locator(`tr.user-row[data-username="${operator}"]`);
  await opRow.getByRole('link', { name: /^edit$/i }).click();
  await page.waitForURL('**/admin/users/*');

  const financialBox = page.locator('form.grants-form input[name="grant_financial"]');
  await expect(financialBox).toBeVisible();
  await financialBox.check();
  // The program picker for "financial" (a program-dimensioned group) is present; pick Alpha.
  const scopeSelect = page.locator('form.grants-form select[name="program_financial"]');
  await expect(scopeSelect).toBeVisible();
  await scopeSelect.selectOption({ label: alphaProg });
  await page.locator('form.grants-form button[type="submit"]').click();
  await page.waitForURL('**/admin/users/*?saved**');
  // The scope stuck: the current-scope hint shows Alpha, and the select re-renders selected.
  await expect(page.locator('form.grants-form select[name="program_financial"]')).toHaveValue(
    await scopeSelect.locator('option', { hasText: alphaProg }).getAttribute('value'),
  );

  // The demoted "funds" group has NO program picker (empty-coverage): assert it is absent.
  await expect(page.locator('form.grants-form select[name="program_funds"]')).toHaveCount(0);

  // --- log out the admin, log in as the scoped operator ---
  await page.locator('form.app-logout button[type="submit"]').click();
  await page.waitForURL('**/login**');
  await login(page, operator, operatorPw);

  // The income statement (program-dimensioned, "financial") shows ONLY Alpha's expense
  // account -- Beta (a sibling subtree) is filtered out of the rows.
  await page.goto(`${IS}?scope=1&from=2025-01-01&to=2030-12-31`);
  const table = page.locator('table.report-table');
  await expect(table).toBeVisible();
  await expect(table).toContainText(alphaAcct);
  await expect(table).not.toContainText(betaAcct);

  // balance_sheet is a DEMOTED (non-program) report in "financial": a purely
  // program-scoped grant CANNOT reach it -> 403.
  const denied = await page.request.get(`${DEMOTED}?scope=1&asof=2030-12-31`);
  expect(denied.status()).toBe(403);

  // --- back as admin: CLEAR the scope (org-wide) ---
  await page.locator('form.app-logout button[type="submit"]').click();
  await page.waitForURL('**/login**');
  await login(page, server.username, server.password);
  await page.goto('/admin/users');
  await page
    .locator(`tr.user-row[data-username="${operator}"]`)
    .getByRole('link', { name: /^edit$/i })
    .click();
  await page.waitForURL('**/admin/users/*');
  // Keep the box checked, reset the program scope to org-wide (the empty first option).
  await page.locator('form.grants-form select[name="program_financial"]').selectOption('');
  await page.locator('form.grants-form button[type="submit"]').click();
  await page.waitForURL('**/admin/users/*?saved**');

  // --- as the now-org-wide operator: BOTH programs' rows show, and the demoted report is
  // reachable ---
  await page.locator('form.app-logout button[type="submit"]').click();
  await page.waitForURL('**/login**');
  await login(page, operator, operatorPw);

  await page.goto(`${IS}?scope=1&from=2025-01-01&to=2030-12-31`);
  const orgTable = page.locator('table.report-table');
  await expect(orgTable).toBeVisible();
  await expect(orgTable).toContainText(alphaAcct);
  await expect(orgTable).toContainText(betaAcct);

  const restored = await page.request.get(`${DEMOTED}?scope=1&asof=2030-12-31`);
  expect(restored.status()).toBe(200);
});
