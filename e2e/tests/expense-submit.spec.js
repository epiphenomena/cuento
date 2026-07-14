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
const { openNewAccount, saveAccount } = require('../helpers');

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
  await openNewAccount(page);
  await page.locator('#af-type').selectOption('revenue');
  await expect(page.locator('form#account-form.e2e-settled')).toBeVisible();
  await page.locator('#af-name-en').fill(revName);
  await page.locator('#af-name-es').fill(`${revName} ES`);
  const acctSub = page.locator('input[name="sub_1"]');
  if (!(await acctSub.isChecked())) await acctSub.check();
  await saveAccount(page);
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

  // Add an UNBALANCED line via the auto-row grid: fill row 0 (a single revenue line,
  // nothing offsets it). The grid auto-appends a fresh trailing empty row.
  await expect(page.locator('form#expense-grid-form')).toBeVisible();
  await page.locator('#el-account-0').selectOption({ label: revName });
  await page.locator('#el-amount-0').fill('123.45');
  // The client auto-appends row 1 once row 0 is edited.
  await expect(page.locator('#el-account-1')).toBeVisible();

  // Save the whole grid (bulk replace-set) -> a redirect back to the detail page.
  const detailReloaded = page.waitForResponse(
    (r) => new URL(r.url()).pathname === `/expenses/${reportID}` && r.request().method() === 'GET',
  );
  await page.locator('#expense-save-lines').click();
  await detailReloaded;

  // The line shows in the grid (its account is selected in row 0).
  await expect(page.locator('#el-account-0')).toHaveValue(/\d+/);
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

// p26.4: the expense-report line grid overhaul. Drives the REAL grid to prove:
//   - the subsidiary picker AUTO-SUBMITS on change (no "Set subsidiary" button) and the
//     page re-scopes to the new sub;
//   - account/fund/program are comboboxes (type-to-filter + pick), including re-picking the
//     value-0 reset entry ("None") after a real pick (the p26.3 value-0 bug fix);
//   - the amount input reformats on blur (1000 -> 1,000.00);
//   - the × delete button removes a MIDDLE row (re-indexing the survivors so the save's
//     _i scheme stays contiguous) and, when the last data row is deleted, the grid never
//     drops below one trailing empty row.
test('expenses: subsidiary auto-set, account/fund/program combos, amount blur, delete-row (p26.4)', async ({
  page,
  server,
}) => {
  const suffix = unique();
  const subUser = `e2egrid_${suffix}`;
  const subPass = 'e2e-submitter-passw0rd';
  const revName = `Grid Revenue E2E ${suffix}`;
  const sub2Name = `Branch E2E ${suffix}`;

  // ===== PHASE 1: admin creates a 2nd subsidiary, a revenue account spanning BOTH subs,
  // and a submitter. =====
  await login(page, server.username, server.password);

  // A 2nd subsidiary so the report's subsidiary picker has a real choice to auto-switch to.
  // The New trigger hx-gets the form into #subsidiary-form; wait for the swap to SETTLE so
  // the form's own hx-post is wired before submitting (the settle race helpers.js documents).
  await page.goto('/admin/subsidiaries');
  await page.getByRole('button', { name: /new subsidiary/i }).click();
  await expect(page.locator('form#subsidiary-form.e2e-settled')).toBeVisible();
  await page.locator('#sf-name').fill(sub2Name);
  const subsReloaded = page.waitForResponse(
    (r) => new URL(r.url()).pathname === '/admin/subsidiaries' && r.request().method() === 'GET',
  );
  await page.locator('form#subsidiary-form button[type="submit"]').click();
  await subsReloaded;
  await expect(page.getByText(sub2Name)).toBeVisible();

  // A revenue leaf account that covers BOTH subsidiaries, so switching the report's sub
  // keeps the account offered in the grid.
  await openNewAccount(page);
  await page.locator('#af-type').selectOption('revenue');
  await expect(page.locator('form#account-form.e2e-settled')).toBeVisible();
  await page.locator('#af-name-en').fill(revName);
  await page.locator('#af-name-es').fill(`${revName} ES`);
  // Check every subsidiary checkbox so the account is a leaf in both subs.
  const subBoxes = page.locator('input[name^="sub_"]');
  const boxCount = await subBoxes.count();
  for (let i = 0; i < boxCount; i += 1) {
    const box = subBoxes.nth(i);
    if (!(await box.isChecked())) await box.check();
  }
  await saveAccount(page);
  await expect(page.getByText(revName)).toBeVisible();

  // Create the submitter user + grant can_submit_expenses.
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
  const row = page.locator(`tr.user-row[data-username="${subUser}"]`);
  await expect(row).toBeVisible();
  await row.getByRole('link', { name: /permissions/i }).click();
  await page.waitForURL('**/admin/users/*');
  const canSubmit = page.locator('form.can-submit-form input[name="can_submit_expenses"]');
  await expect(canSubmit).toBeVisible();
  await canSubmit.check();
  await page.locator('form.can-submit-form button[type="submit"]').click();
  await page.waitForURL('**/admin/users/*?saved**');

  await page.context().clearCookies();

  // ===== PHASE 2: the submitter drives the p26.4 grid =====
  await login(page, subUser, subPass);
  await page.goto('/expenses');
  await page.getByRole('button', { name: /new expense report/i }).click();
  await page.waitForURL('**/expenses/*');
  const reportID = Number(new URL(page.url()).pathname.split('/').pop());
  expect(reportID).toBeGreaterThan(0);

  // (a) SUBSIDIARY AUTO-SET: the "Set subsidiary" button is GONE; changing the picker
  // auto-submits and the page re-loads scoped to the chosen sub. Select the 2nd sub by
  // its (stored proper-noun) name -> a full navigation (HX-Redirect) back to the detail.
  await expect(page.locator('.expense-sub-form button:not([type="button"])')).toHaveCount(0);
  const afterSubSet = page.waitForResponse(
    (r) => new URL(r.url()).pathname === `/expenses/${reportID}` && r.request().method() === 'GET',
  );
  await page.locator('#er-sub').selectOption({ label: sub2Name });
  await afterSubSet;
  await expect(page.locator('#er-sub')).toHaveValue(/\d+/);
  // The picker now shows the branch as selected (its option text is the sub's name).
  await expect(
    page.locator('#er-sub option:checked'),
  ).toHaveText(sub2Name);

  // (e/f) ACCOUNT COMBO: type-to-filter the account and pick it via the overlay listbox.
  await expect(page.locator('form#expense-grid-form')).toBeVisible();
  const acctCell = page.locator('.el-row[data-row="0"] .el-account-cell');
  await acctCell.locator('.combo-text').click();
  await acctCell.locator('.combo-text').fill('grid');
  await expect(acctCell.locator('.combo-list')).toBeVisible();
  await acctCell.locator('.combo-option', { hasText: revName }).click();
  await expect(page.locator('#el-account-0')).toHaveValue(/\d+/);

  // (c) AMOUNT BLUR-REFORMAT: type a bare integer, blur -> grouped + 2-dp.
  await page.locator('#el-amount-0').fill('1000');
  await page.locator('#el-amount-0').blur();
  await expect(page.locator('#el-amount-0')).toHaveValue('1,000.00');

  // (e/f) PROGRAM COMBO + value-0 RE-PICK: pick "General" via the combo, then re-open and
  // pick the "None" (value 0) entry -- proving collectOptions now INCLUDES value 0 so a
  // reset-to-none is re-offerable after a real pick.
  const progCell = page.locator('.el-row[data-row="0"] .el-program-cell');
  await progCell.locator('.combo-text').click();
  await expect(progCell.locator('.combo-list')).toBeVisible();
  await progCell.locator('.combo-option', { hasText: 'General' }).click();
  await expect(page.locator('#el-program-0')).toHaveValue(/[1-9]\d*/);
  // Re-open: the value-0 "None" entry must be present and pickable (the p26.3 fix). Move
  // focus away first (a pick leaves the overlay focused, so a re-click would not re-fire
  // `focus`), then focus + type nothing to reopen the full list in original order.
  await page.locator('#el-memo-0').click();
  await progCell.locator('.combo-text').focus();
  await progCell.locator('.combo-text').fill('');
  await expect(progCell.locator('.combo-list')).toBeVisible();
  await progCell.locator('.combo-option').first().click(); // "None" is the first option (value 0)
  await expect(page.locator('#el-program-0')).toHaveValue('0');

  // The grid auto-appended a trailing empty row once row 0 became non-empty.
  await expect(page.locator('#el-account-1')).toBeVisible();

  // Fill row 1 (a second real line) and row 2 appears; fill row 2 so we have three data
  // rows + a trailing empty (row 3).
  await page.locator('.el-row[data-row="1"] .el-account-cell .combo-text').click();
  await page.locator('.el-row[data-row="1"] .el-account-cell .combo-option', { hasText: revName }).click();
  await page.locator('#el-amount-1').fill('20');
  await page.locator('#el-memo-1').fill('row-one-memo');
  await expect(page.locator('#el-account-2')).toBeVisible();
  await page.locator('.el-row[data-row="2"] .el-account-cell .combo-text').click();
  await page.locator('.el-row[data-row="2"] .el-account-cell .combo-option', { hasText: revName }).click();
  await page.locator('#el-amount-2').fill('30');
  await page.locator('#el-memo-2').fill('row-two-memo');
  await expect(page.locator('#el-account-3')).toBeVisible();

  // (p26.11) COLUMN ORDER: the delete-× cell is now the LAST cell in the row, AFTER the
  // error column. Assert both header and row cell order so a regression that moves it back
  // ahead of the error column is caught. Headers: account, amount, fund, program, memo,
  // error, then the (empty, aria-hidden) delete-action header.
  const headerCells = page.locator('#expense-grid-form thead th');
  await expect(headerCells).toHaveCount(7);
  await expect(headerCells.nth(5)).toHaveText(/error/i);
  await expect(headerCells.last()).toHaveClass(/el-delete-col/);
  // In a data row the error cell precedes the delete cell (the delete is the last td).
  const row0Cells = page.locator('.el-row[data-row="0"] > td, .el-row[data-row="0"] > th');
  await expect(row0Cells.last()).toHaveClass(/el-delete-cell/);
  await expect(page.locator('.el-row[data-row="0"] .el-row-error'))
    .not.toHaveClass(/el-delete-cell/);
  // The delete cell comes right after the error cell in DOM order.
  await expect(
    page.locator('.el-row[data-row="0"] .el-row-error + .el-delete-cell'),
  ).toHaveCount(1);

  // (d) DELETE A MIDDLE ROW: delete row 1 (memo "row-one-memo"). The survivors re-index to
  // contiguous 0..n-1, so row-two-memo shifts up to index 1 and the memo text follows.
  await page.locator('.el-row[data-row="1"] .el-delete').click();
  await expect(page.locator('#el-memo-1')).toHaveValue('row-two-memo');
  // The rows-count hidden field tracks the live row set (3 data rows became fewer + a
  // trailing empty). It is contiguous, so #el-account-<count-1> exists and beyond doesn't.
  const rowsCount = Number(await page.locator('#expense-rows-count').inputValue());
  await expect(page.locator(`#el-account-${rowsCount - 1}`)).toBeVisible();
  await expect(page.locator(`#el-account-${rowsCount}`)).toHaveCount(0);

  // (d) DELETE DOWN TO THE LAST DATA ROW: delete rows from the top until only the trailing
  // empty remains -- the grid must never drop below one row, re-adding a fresh empty.
  // Delete the current row 0 repeatedly; each delete re-indexes, so row 0 always exists.
  let guard = 0;
  while (guard < 10) {
    const remaining = Number(await page.locator('#expense-rows-count').inputValue());
    if (remaining <= 1) break;
    await page.locator('.el-row[data-row="0"] .el-delete').click();
    guard += 1;
  }
  // Exactly one row remains and it is EMPTY (a fresh trailing row): no account chosen.
  await expect(page.locator('.el-row')).toHaveCount(1);
  await expect(page.locator('#el-account-0')).toHaveValue('0');
  await expect(page.locator('#expense-rows-count')).toHaveValue('1');

  // (d) DELETE THE ONLY/LAST ROW (the resetRow branch): with exactly one trailing empty
  // row, clicking its × must RESET it in place (never drop to zero rows) -- the grid keeps
  // exactly one empty row. This is the "last -> re-add empty" invariant's fixed point.
  await page.locator('.el-row[data-row="0"] .el-delete').click();
  await expect(page.locator('.el-row')).toHaveCount(1);
  await expect(page.locator('#el-account-0')).toHaveValue('0');
  await expect(page.locator('#el-amount-0')).toHaveValue('');
  await expect(page.locator('#expense-rows-count')).toHaveValue('1');

  // (e) the fund select is ALSO enhanced into a combobox (its overlay input exists).
  await expect(page.locator('.el-row[data-row="0"] .el-fund-cell .combo-text')).toBeVisible();

  // Sanity: re-add a real line and SAVE + SUBMIT so the full round-trip still works after
  // all the client mutation.
  await page.locator('.el-row[data-row="0"] .el-account-cell .combo-text').click();
  await page.locator('.el-row[data-row="0"] .el-account-cell .combo-option', { hasText: revName }).click();
  await page.locator('#el-amount-0').fill('55');
  const detailReloaded = page.waitForResponse(
    (r) => new URL(r.url()).pathname === `/expenses/${reportID}` && r.request().method() === 'GET',
  );
  await page.locator('#expense-save-lines').click();
  await detailReloaded;
  await expect(page.locator('table.expense-lines-table')).toContainText(revName);
  await expect(page.locator('#el-amount-0')).toHaveValue('55.00');
});
