// @ts-check
// Functional test of the p20.3 reviewer queue -> convert / reject. Drives the REAL
// pages served by `cuento serve -dev` against the worker's fresh migrated db.
//
// The fixture seeds ONLY an admin (is_admin => TxnWrite), so the ADMIN is the reviewer.
// A pure submitter has NO ledger access, so the flow is phased (each phase one login,
// well under the per-worker login limiter):
//   PHASE 1 (admin): create an expense account (a report line + the posted txn need it),
//     a cash account (the reviewer's counter-side), and a submitter user with
//     can_submit_expenses (the p20.2 admin toggle -- how the submitter is seeded).
//   PHASE 2 (submitter): create TWO reports, add one expense line each, submit both.
//   PHASE 3 (admin/reviewer): open the queue, REVIEW & POST the first (the p12 editor
//     prefilled with the subsidiary LOCKED; add the cash counter-side to balance; post)
//     -> the report converts and links the real txn; then REJECT the second with a
//     reason -> it routes back. PHASE 4 (submitter): sees the rejection reason.
//
// TEST-ISOLATION (worker-scoped db shared across the worker's specs): uniquely-named
// accounts + submitter, so nothing durable leaks. Strict CSP (script-src 'self') => NO
// page.waitForFunction; only locator / e2e-settled / waitForResponse waits. The p12
// editor post is an hx-post returning HX-Redirect (a client navigation) -- wait for the
// destination GET response deterministically. RULE 11: all data synthetic.

const { test, expect } = require('../fixtures');
const { openNewAccount, saveAccount, selectTxnAccount } = require('../helpers');

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

// createLeafAccount makes a leaf account of the given type in the root subsidiary and
// returns its display name. For an expense type it also sets a default functional class
// (the type change triggers an htmx form-swap that server-renders #af-func). Mirrors the
// proven-stable createExpenseAccount in bank-import.spec.js: it inlines the
// waitForResponse+click (rather than saveAndReload) and waits for `form#account-form
// .e2e-settled` immediately before the click so the swapped-in form's Save hx-post is
// wired (the settle-race helpers.js documents).
async function createLeafAccount(page, type, name) {
  // p26.7: the create form is its own full-shell page (GET /accounts/new). Changing
  // the type still re-fetches the form in place as an htmx swap (hx-get on #af-type,
  // HX-Target #account-form -> the bare partial), so the type-swap + settle waits
  // below are unchanged; only the way we OPEN the form is now a navigation.
  await openNewAccount(page);
  // Wait for THAT GET swap to arrive before touching the swapped form, so the
  // following settle-wait can never resolve on the STALE pre-swap `.e2e-settled`
  // marker (the settle race helpers.js documents).
  const typeSwapped = page.waitForResponse(
    (r) => new URL(r.url()).pathname === '/accounts/new' && r.request().method() === 'GET',
  );
  await page.locator('#af-type').selectOption(type);
  await typeSwapped;
  if (type === 'expense') {
    await expect(page.locator('#af-func')).toBeVisible();
  }
  await page.locator('#af-name-en').fill(name);
  await page.locator('#af-name-es').fill(name);
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) await rootSub.check();
  if (type === 'expense') await page.locator('#af-func').selectOption('program');

  // Save is a plain full-page POST -> 303 back to /accounts.
  await saveAccount(page);
  await expect(page.getByText(name)).toBeVisible();
  return name;
}

// createSubmitter creates a txn_perm=none user and grants can_submit_expenses via the
// admin toggle, returning its credentials.
async function createSubmitter(page, username, password) {
  await page.goto('/admin/users');
  await page.getByRole('button', { name: /new user/i }).click();
  await expect(page.locator('form#user-create-form.e2e-settled')).toBeVisible();
  await page.locator('#uc-username').fill(username);
  await page.locator('#uc-password').fill(password);
  await page.locator('#uc-perm').selectOption('none');
  const usersReloaded = page.waitForResponse(
    (r) => new URL(r.url()).pathname === '/admin/users' && r.request().method() === 'GET',
  );
  await page.getByRole('button', { name: /create user/i }).click();
  await usersReloaded;

  const row = page.locator(`tr.user-row[data-username="${username}"]`);
  await expect(row).toBeVisible();
  // The users-list row link to the per-user detail page was renamed to "Edit" (users-list
  // cleanup); it still lands on /admin/users/{id} where the can-submit toggle lives.
  await row.getByRole('link', { name: /edit/i }).click();
  await page.waitForURL('**/admin/users/*');
  const canSubmit = page.locator('form.can-submit-form input[name="can_submit_expenses"]');
  await expect(canSubmit).toBeVisible();
  await canSubmit.check();
  await page.locator('form.can-submit-form button[type="submit"]').click();
  await page.waitForURL('**/admin/users/*?saved**');
}

// submitReport (as the submitter) creates a report, adds one line on acctName (with an
// optional per-line description, p26.19), submits, and returns the numeric report id.
async function submitReport(page, acctName, amount, desc) {
  await page.goto('/expenses');
  await page.getByRole('button', { name: /new expense report/i }).click();
  await page.waitForURL('**/expenses/*');
  const reportID = Number(new URL(page.url()).pathname.split('/').pop());
  expect(reportID).toBeGreaterThan(0);

  // Fill row 0 of the auto-row grid, then bulk-save the line set.
  await expect(page.locator('form#expense-grid-form')).toBeVisible();
  await page.locator('#el-account-0').selectOption({ label: acctName });
  await page.locator('#el-amount-0').fill(amount);
  if (desc) await page.locator('#el-desc-0').fill(desc);
  await expect(page.locator('#el-account-1')).toBeVisible();
  const detailReloaded = page.waitForResponse(
    (r) => new URL(r.url()).pathname === `/expenses/${reportID}` && r.request().method() === 'GET',
  );
  await page.locator('#expense-save-lines').click();
  await detailReloaded;

  // p26.19: the per-line description persisted through the bulk save -- the reloaded
  // (still-editable) grid re-renders it into row 0's description input.
  if (desc) await expect(page.locator('#el-desc-0')).toHaveValue(desc);

  await page.locator('#expense-submit').click();
  await page.waitForURL(`**/expenses/${reportID}`);
  await expect(page.locator('.badge')).toContainText(/submitted/i);
  return reportID;
}

test('expenses review: reviewer posts one report (converts) and rejects another', async ({
  page,
  server,
}) => {
  const suffix = unique();
  const subUser = `e2ereviewsub_${suffix}`;
  const subPass = 'e2e-review-passw0rd';
  const expName = `Travel Exp E2E ${suffix}`;
  const cashName = `Cash E2E ${suffix}`;

  // ===== PHASE 1: admin seeds the chart + the submitter =====
  await login(page, server.username, server.password);
  await createLeafAccount(page, 'expense', expName);
  await createLeafAccount(page, 'asset', cashName);
  await createSubmitter(page, subUser, subPass);
  await page.context().clearCookies();

  // ===== PHASE 2: the submitter files two reports =====
  await login(page, subUser, subPass);
  const postDesc = `Conference travel ${suffix}`;
  const postID = await submitReport(page, expName, '40.00', postDesc); // this one gets posted
  const rejectID = await submitReport(page, expName, '15.00'); // this one gets rejected
  await page.context().clearCookies();

  // ===== PHASE 3: the admin/reviewer works the queue =====
  await login(page, server.username, server.password);
  await page.goto('/expenses/review');
  await expect(page.getByRole('heading', { name: /expense review/i })).toBeVisible();
  // Both submitted reports are pending.
  await expect(page.locator('tr.expreview-row')).toHaveCount(2);

  // Review & post the FIRST report -> the p12 editor, subsidiary locked.
  const postRow = page.locator(`tr.expreview-row[data-report-id="${postID}"]`);
  await postRow.locator('a.expreview-post').click();
  await page.waitForURL(`**/expenses/review/${postID}`);
  await expect(page.locator('form#txn-form')).toBeVisible();
  await expect(page.locator('#txn-subsidiary')).toBeDisabled();
  // CONSISTENCY with the normal txn editor: review reuses the SAME transaction-form grid,
  // and the MAIN split (the pay-from / counter-side leg) is the FIRST row (main split =
  // first split, p26.34). Row 0 is that main split -- it carries the report's HEADER
  // description + memo (the report's overall description/memo, the same way split0 does in
  // the normal editor), with a blank account here (this sub has no default AP) for the
  // reviewer to fill. Row 1 is the prefilled expense LINE (its account + its per-line
  // description).
  // The report's header description defaults to the submitter's display name (which falls
  // back to the username when unset), and lands on the main split (row 0). The report's
  // header MEMO (defaulting to the localized "Expense report") also rides the main split, so
  // the main split carries BOTH the description AND the memo (the task's consistency point).
  await expect(page.locator('#txn-desc-0')).toHaveValue(subUser);
  await expect(page.locator('#txn-memo-0')).not.toHaveValue('');
  await expect(page.locator('#txn-account-1')).toHaveValue(/\d+/);
  // p26.19: the line's description was carried into the review editor row (prefillExpenseRows
  // -> description_1 now that the main split leads at row 0), so it round-trips into the
  // CREATED split on convert.
  await expect(page.locator('#txn-desc-1')).toHaveValue(postDesc);

  // Fill the MAIN (row 0) pay-from split with the cash counter-side (-40.00) so the txn
  // balances -- the main split is where the balancing leg belongs.
  await selectTxnAccount(page.locator('#txn-account-0'), cashName);
  await page.locator('#txn-amount-0').fill('-40.00');

  // Post: hx-post -> HX-Redirect to the created txn's history. Wait for that GET.
  const historyReloaded = page.waitForResponse(
    (r) => /^\/transactions\/\d+\/history$/.test(new URL(r.url()).pathname) && r.request().method() === 'GET',
  );
  await page.locator('form#txn-form button[type="submit"]').click();
  await historyReloaded;
  // The txn history page renders (the report converted to a real transaction).
  await expect(page.locator('main#main')).toBeVisible();
  // p26.19: the line's description reached the CREATED split -- the txn history's
  // create change shows the split's description field. This closes the line->split leg
  // (the row->split round-trip) end-to-end, not just the line->row prefill above.
  await expect(page.locator('main#main')).toContainText(postDesc);

  // Back on the queue: the posted report is now history (converted) with a txn link,
  // and only the second report remains pending.
  await page.goto('/expenses/review');
  await expect(page.locator('tr.expreview-row').filter({ has: page.locator('a.expreview-post') })).toHaveCount(1);
  await expect(page.locator(`tr.expreview-row[data-report-id="${postID}"] a.expreview-view-txn`)).toBeVisible();

  // p26.27: the queue row offers ONLY the "Review" link now -- the reject-with-reason
  // form moved onto the review PAGE (no per-row reject form on the queue anymore).
  await expect(page.locator('form.expreview-reject-form')).toHaveCount(0);
  const rejectRow = page.locator(`tr.expreview-row[data-report-id="${rejectID}"]`);
  await expect(rejectRow.locator('a.expreview-post')).toBeVisible();

  // Open the SECOND report's review page -> the p12 editor + a reject region.
  await rejectRow.locator('a.expreview-post').click();
  await page.waitForURL(`**/expenses/review/${rejectID}`);
  await expect(page.locator('form#txn-form')).toBeVisible();
  await expect(page.locator('form.expreview-reject-form')).toBeVisible();

  // A BLANK reason -> the review page re-renders at 422 with the reject-reason error
  // (the report stays submitted, the editor + reject form still shown).
  const rejected422 = page.waitForResponse(
    (r) => new URL(r.url()).pathname === `/expenses/review/reject/${rejectID}` && r.request().method() === 'POST',
  );
  await page.locator('form.expreview-reject-form button.expreview-reject-btn').click();
  const resp422 = await rejected422;
  expect(resp422.status()).toBe(422);
  await expect(page.locator('.expreview-reject-error')).toBeVisible();
  await expect(page.locator('form#txn-form')).toBeVisible();

  // Now reject WITH a reason from the review page (routes it back to the submitter);
  // a successful reject 303-redirects back to the queue.
  const reason = `Missing itemized receipt (${suffix}).`;
  await page.locator('form.expreview-reject-form input.expreview-reject-reason').fill(reason);
  const queueReloaded = page.waitForResponse(
    (r) => new URL(r.url()).pathname === '/expenses/review' && r.request().method() === 'GET',
  );
  await page.locator('form.expreview-reject-form button.expreview-reject-btn').click();
  await page.waitForURL('**/expenses/review');
  await queueReloaded;
  // No pending reports remain (both are actioned).
  await expect(page.locator('tr.expreview-row').filter({ has: page.locator('a.expreview-post') })).toHaveCount(0);

  // ===== PHASE 4: the submitter sees the rejection reason =====
  await page.context().clearCookies();
  await login(page, subUser, subPass);
  await page.goto(`/expenses/${rejectID}`);
  await expect(page.locator('#expense-review-notes')).toHaveText(reason);
  await expect(page.locator('.badge')).toContainText(/rejected/i);
});
