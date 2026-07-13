// @ts-check
// Functional test of the p20.2 submitter workspace. Drives the REAL /expenses pages
// served by `cuento serve -dev` against the worker's fresh migrated db.
//
// A pure SUBMITTER (can_submit_expenses, txn_perm=none) has NO way to create accounts
// (that is TxnWrite, the ledger boundary the submitter is 403 on), and the fixture
// seeds ONLY an admin. So the flow is two-phase, ONE login each (well under the
// per-worker login limiter):
//   PHASE 1 (as admin): create a revenue account (a report line needs an R/E account),
//     create the submitter user, and toggle its can_submit_expenses on the user-detail
//     page (the p20.1-deferred admin toggle -- this is how the submitter is seeded).
//   PHASE 2 (as the submitter): create a report, add an UNBALANCED line, submit, see
//     "Submitted"; then the reviewer's rejection is simulated via the CLI seam
//     (server.rejectReport -> the `expense-report reject` verb, the CLI face of the
//     p20.1 store method -- the reviewer WEB queue is p20.3), the reason shows, and the
//     submitter RESUBMITS, flipping the status back.
//
// TEST-ISOLATION (worker-scoped db shared across the worker's specs): this spec never
// mutates the shared e2eadmin's perms/locale; it creates its OWN uniquely-named
// submitter + a uniquely-named revenue account, so nothing durable leaks. Selectors
// are language-independent (ids, form/data-* selectors). No page.waitForFunction
// (strict CSP: script-src 'self') -- only locator/URL/response waits. RULE 11: all
// data is synthetic.

const { test, expect } = require('../fixtures');
const { saveAndReload } = require('../helpers');

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

test('expenses: submitter drafts an unbalanced report, submits, sees a rejection reason, resubmits', async ({
  page,
  server,
}) => {
  const suffix = unique();
  const subUser = `e2esubmitter_${suffix}`;
  const subPass = 'e2e-submitter-passw0rd';
  const revName = `Grants Revenue E2E ${suffix}`;

  // ===== PHASE 1: admin sets up the account + the submitter =====
  await login(page, server.username, server.password);

  // A revenue leaf account in the root subsidiary (a report line needs an R/E account).
  await page.goto('/accounts');
  await page.getByRole('button', { name: /new account/i }).click();
  await expect(page.locator('#af-name-en')).toBeVisible();
  await page.locator('#af-type').selectOption('revenue');
  await expect(page.locator('form#account-form.e2e-settled')).toBeVisible();
  await page.locator('#af-name-en').fill(revName);
  await page.locator('#af-name-es').fill(`${revName} ES`);
  const acctSub = page.locator('input[name="sub_1"]');
  if (!(await acctSub.isChecked())) await acctSub.check();
  await saveAndReload(page, { reloadPath: '/accounts' });
  await expect(page.getByText(revName)).toBeVisible();

  // Create the submitter user (txn_perm=none, no admin).
  await page.goto('/admin/users');
  await page.getByRole('button', { name: /new user/i }).click();
  await expect(page.locator('form#user-create-form.e2e-settled')).toBeVisible();
  await page.locator('#uc-username').fill(subUser);
  await page.locator('#uc-password').fill(subPass);
  await page.locator('#uc-perm').selectOption('none');
  const usersReloaded = page.waitForResponse(
    (r) => new URL(r.url()).pathname === '/admin/users' && r.request().method() === 'GET',
  );
  await page.getByRole('button', { name: /create user/i }).click();
  await usersReloaded;

  // Grant can_submit_expenses on the user-detail page (the p20.1-deferred admin toggle).
  const row = page.locator(`tr.user-row[data-username="${subUser}"]`);
  await expect(row).toBeVisible();
  await row.getByRole('link', { name: /permissions/i }).click();
  await page.waitForURL('**/admin/users/*');
  const canSubmit = page.locator('form.can-submit-form input[name="can_submit_expenses"]');
  await expect(canSubmit).toBeVisible();
  await canSubmit.check();
  await page.locator('form.can-submit-form button[type="submit"]').click();
  await page.waitForURL('**/admin/users/*?saved**');
  await expect(page.locator('form.can-submit-form input[name="can_submit_expenses"]')).toBeChecked();

  // Drop the admin session so we can log in as the submitter. Clearing cookies is
  // sufficient (the next /login mints a fresh session); no logout POST is needed.
  await page.context().clearCookies();

  // ===== PHASE 2: the submitter drives the workspace =====
  await login(page, subUser, subPass);

  // The submitter sees ONLY the Expenses section (no accounts/reports/admin). p24: the
  // Expenses parent (top nav) and the "My expenses" section-bar child both point to
  // /expenses; the review queue is hidden (no TxnWrite) — that absence is the access
  // boundary that matters here.
  await page.goto('/expenses');
  await expect(page.locator('main#main h1')).toHaveText(/my expense reports/i);
  await expect(page.locator('nav a[href="/accounts"]')).toHaveCount(0);
  await expect(page.locator('nav a[href="/reports"]')).toHaveCount(0);
  await expect(page.locator('nav a[href="/admin"]')).toHaveCount(0);
  await expect(page.locator('nav a[href="/expenses"]').first()).toBeVisible();
  await expect(page.locator('nav a[href="/expenses/review"]')).toHaveCount(0);

  // Create a new report (root subsidiary is the only option).
  await page.getByRole('button', { name: /new expense report/i }).click();
  await page.waitForURL('**/expenses/*');
  const reportURL = page.url();
  const reportID = Number(new URL(reportURL).pathname.split('/').pop());
  expect(reportID).toBeGreaterThan(0);

  // Add an UNBALANCED line: a single revenue line (nothing offsets it).
  await page.getByRole('button', { name: /new line/i }).click();
  await expect(page.locator('form#expense-line-form.e2e-settled')).toBeVisible();
  await page.locator('#elf-account').selectOption({ label: revName });
  await page.locator('#elf-amount').fill('123.45');
  const detailReloaded = page.waitForResponse(
    (r) => new URL(r.url()).pathname === `/expenses/${reportID}` && r.request().method() === 'GET',
  );
  await page.locator('form#expense-line-form button[type="submit"]').click();
  await detailReloaded;

  // The line shows in the table.
  await expect(page.locator('table.expense-lines-table')).toContainText(revName);

  // Submit -- the report need NOT balance.
  await page.locator('#expense-submit').click();
  await page.waitForURL(`**/expenses/${reportID}`);
  await expect(page.locator('.badge')).toContainText(/submitted/i);
  // Read-only now: no add-line trigger, no submit button.
  await expect(page.locator('#expense-submit')).toHaveCount(0);
  await expect(page.locator('#expense-readonly')).toBeVisible();

  // Simulate the reviewer rejecting the report (CLI seam; the WEB reviewer queue is p20.3).
  const reason = `Please attach the receipt (${suffix}).`;
  server.rejectReport(reportID, reason);

  // Reload the detail: the reviewer's reason shows + a resubmit affordance appears.
  await page.reload();
  await expect(page.locator('#expense-review-notes')).toHaveText(reason);
  await expect(page.locator('.badge')).toContainText(/rejected/i);
  await expect(page.locator('#expense-resubmit')).toBeVisible();

  // Resubmit -- the status flips back to submitted.
  await page.locator('#expense-resubmit').click();
  await page.waitForURL(`**/expenses/${reportID}`);
  await expect(page.locator('.badge')).toContainText(/submitted/i);
});
