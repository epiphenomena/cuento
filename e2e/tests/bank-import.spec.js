// @ts-check
// Functional test of the p17.2 bank-CSV import (upload + mapping + staging). It
// drives the REAL /import page served by `cuento serve -dev` against the worker's
// migrated db with the seeded admin (is_admin => TxnWrite). The admin creates its
// OWN asset account (worker-scoped db is shared across a worker's specs, so this
// spec never touches shared data beyond the seeded root subsidiary "Organization"),
// then uploads a small CSV via the multipart file input, maps the columns, picks the
// account + subsidiary, previews the parsed rows (htmx swaps the preview into
// #import-workspace), and stages them -- a duplicate line shows flagged in the result.
//
// The CSV is delivered as an in-memory Buffer via setInputFiles (no on-disk fixture,
// DATA RULE 11: hand-authored synthetic data). Strict CSP (script-src 'self') => NO
// page.waitForFunction; only locator / e2e-settled / waitForResponse waits. The
// preview + stage are htmx innerHTML swaps into #import-workspace, so we wait for the
// e2e-settled marker the page fixture stamps on the swap target.
//
// Selectors (import.tmpl / import_preview.tmpl):
//   - upload form:      form.import-upload-form #import-subsidiary/#import-account/#import-file
//   - workspace target: #import-workspace  (preview + result swap in here)
//   - preview rows:     tr.import-preview-row ; confirm button: form.import-confirm-form
//   - result summary:   p.import-result-summary[role=status]
//   - duplicate row:    tr.import-row-duplicate ; flag: span.import-dupe-flag

const { test, expect } = require('../fixtures');
const { openNewAccount, saveAccount } = require('../helpers');

// A per-run account name so parallel specs on the worker never collide.
function uniqueName() {
  return 'Bank E2E ' + Math.random().toString(36).slice(2, 8);
}

async function login(page, server) {
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');
}

// createAssetAccount makes an asset account mapped to the root subsidiary through
// the real inline form and returns its display name. Mirrors accounts.spec.js.
async function createAssetAccount(page) {
  const name = uniqueName();
  await openNewAccount(page);
  await page.locator('#af-name-en').fill(name);
  await page.locator('#af-name-es').fill(name);
  await page.locator('#af-type').selectOption('asset');
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) await rootSub.check();

  const reloaded = page.waitForResponse(
    (r) => new URL(r.url()).pathname === '/accounts' && r.request().method() === 'GET',
  );
  await page.getByRole('button', { name: /^save$/i }).click();
  await reloaded;
  await expect(page.getByText(name)).toBeVisible();
  return name;
}

// createExpenseAccount makes a leaf expense account (with a default functional class)
// mapped to the root subsidiary and returns its display name. The expense type change
// triggers an htmx form-swap that server-renders #af-func; see txn-editor.spec.js.
async function createExpenseAccount(page) {
  const name = uniqueName() + ' Exp';
  // p26.7: the create form is its own full-shell page; the expense type change still
  // re-fetches the form region in place (htmx hx-get on #af-type, HX-Target
  // #account-form). Wait for THAT GET swap before touching #af-func.
  await openNewAccount(page);
  const typeSwapped = page.waitForResponse(
    (r) => new URL(r.url()).pathname === '/accounts/new' && r.request().method() === 'GET',
  );
  await page.locator('#af-type').selectOption('expense');
  await typeSwapped;
  await expect(page.locator('#af-func')).toBeVisible();
  await page.locator('#af-name-en').fill(name);
  await page.locator('#af-name-es').fill(name);
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) await rootSub.check();
  await page.locator('#af-func').selectOption('program');

  // Save is a plain full-page POST -> 303 back to /accounts.
  await saveAccount(page);
  await expect(page.getByText(name)).toBeVisible();
  return name;
}

// uploadCSV picks the target + file on /import and submits the upload; the map+preview
// fragment (p26.64) swaps into #import-workspace with the file's columns as headers.
async function uploadCSV(page, acctName, csv, filename) {
  await page.goto('/import');
  await expect(page.getByRole('heading', { name: /import bank csv/i })).toBeVisible();
  await page.locator('#import-subsidiary').selectOption('1');
  await page.locator('#import-account').selectOption({ label: acctName });
  await page.locator('#import-file').setInputFiles({
    name: filename,
    mimeType: 'text/csv',
    buffer: Buffer.from(csv, 'utf8'),
  });
  await page.locator('form.import-upload-form button[type="submit"]').click();
  // Wait for the workspace to be settled (htmx has swapped AND wired the fragment) so a
  // freshly-swapped role select is ready before we drive it.
  await expect(page.locator('#import-workspace.e2e-settled')).toBeVisible();
  await expect(page.locator('form.import-map-form')).toBeVisible();
}

// mapRole picks a role for one column via its "maps to" select; each change re-POSTs the
// map form and re-renders the fragment. The workspace's e2e-settled class persists
// across swaps, so we CLEAR it first and wait for htmx to re-stamp it after the swap --
// a deterministic "the new fragment has landed and is wired" signal (avoids the
// paint->settle wiring race the fixture documents).
async function mapRole(page, columnIndex, role) {
  await page.locator('#import-workspace').evaluate((el) => el.classList.remove('e2e-settled'));
  await page.locator('#import-role-' + columnIndex).selectOption(role);
  await expect(page.locator('#import-workspace.e2e-settled')).toBeVisible();
}

// p26.64: uploading shows the file's REAL columns as headers, and the amount MODE gates
// which "maps to" options each column offers (Amount in single mode; Debit + Credit in
// debit_credit mode). Server-rendered option-gating via an htmx re-POST (supersedes the
// p26.61 client-side field toggle, which is removed).
test('bank import: columns show as headers; amount-mode gates the maps-to options', async ({ page, server }) => {
  await login(page, server);
  const acctName = await createAssetAccount(page);
  const csv = 'date,amount,desc,memo\n2025-01-15,100.00,Acme,Invoice\n';
  await uploadCSV(page, acctName, csv, 'cols.csv');

  // The file's four columns appear as headers with a sample value.
  await expect(page.locator('th.import-map-col')).toHaveCount(4);
  await expect(page.locator('span.import-map-col-name').first()).toHaveText('date');
  await expect(page.locator('span.import-map-col-sample').first()).toHaveText('2025-01-15');

  // Default mode is single: the first column's "maps to" offers Amount, NOT Debit/Credit.
  await expect(page.locator('#import-map-mode')).toHaveValue('single');
  const opt0 = page.locator('#import-role-0 option');
  await expect(opt0.filter({ hasText: /^Amount$/ })).toHaveCount(1);
  await expect(opt0.filter({ hasText: /^Debit$/ })).toHaveCount(0);
  await expect(opt0.filter({ hasText: /^Credit$/ })).toHaveCount(0);

  // Switch to debit/credit mode: the re-POST re-renders options -> Debit + Credit
  // offered, Amount gone.
  const settled = page.waitForResponse(
    (r) => new URL(r.url()).pathname === '/import/preview' && r.request().method() === 'POST',
  );
  await page.locator('#import-map-mode').selectOption('debit_credit');
  await settled;
  await expect(page.locator('#import-workspace.e2e-settled')).toBeVisible();
  const opt0b = page.locator('#import-role-0 option');
  await expect(opt0b.filter({ hasText: /^Debit$/ })).toHaveCount(1);
  await expect(opt0b.filter({ hasText: /^Credit$/ })).toHaveCount(1);
  await expect(opt0b.filter({ hasText: /^Amount$/ })).toHaveCount(0);
});

// p26.65: MEMO is an OPTIONAL "maps to" role. A column mapped to Memo feeds the split's
// memo (it previews + imports); a file with no memo column mapped still imports cleanly
// (no memo, no error). Memo is mode-independent (offered in single mode).
test('bank import: a mapped memo column previews and imports; omitting it still works', async ({ page, server }) => {
  await login(page, server);
  const acctName = await createAssetAccount(page);
  const csv = 'date,amount,desc,memo\n2025-05-01,100.00,Acme,Invoice 9\n';
  await uploadCSV(page, acctName, csv, 'memo.csv');

  // Memo is offered as a "maps to" option (single mode -- it is mode-independent).
  const opt3 = page.locator('#import-role-3 option');
  await expect(opt3.filter({ hasText: /^Memo$/ })).toHaveCount(1);

  // Map date/amount/desc + the memo column.
  await mapRole(page, 0, 'date');
  await mapRole(page, 1, 'amount');
  await mapRole(page, 2, 'desc');
  await mapRole(page, 3, 'memo');

  // Preview renders and the memo cell carries the mapped text.
  await expect(page.locator('tr.import-preview-row')).toHaveCount(1);
  await expect(page.locator('td.import-cell-memo').first()).toHaveText('Invoice 9');

  // Stage: the result row shows the memo (it flowed through to the staged row).
  await page.locator('form.import-confirm-form button[type="submit"]').click();
  await expect(page.locator('p.import-result-summary[role="status"]')).toBeVisible();
  await expect(page.locator('tr.import-result-row td.import-cell-memo').first()).toHaveText('Invoice 9');

  // Now a SECOND import of the SAME file, this time leaving memo UNMAPPED: it imports
  // cleanly (memo empty, no validation error).
  await page.goto('/import');
  await page.locator('#import-subsidiary').selectOption('1');
  await page.locator('#import-account').selectOption({ label: acctName });
  await page.locator('#import-file').setInputFiles({
    name: 'nomemo.csv',
    mimeType: 'text/csv',
    buffer: Buffer.from('date,amount,desc,memo\n2025-06-01,75.00,Beta,Skip me\n', 'utf8'),
  });
  await page.locator('form.import-upload-form button[type="submit"]').click();
  await expect(page.locator('#import-workspace.e2e-settled')).toBeVisible();
  await mapRole(page, 0, 'date');
  await mapRole(page, 1, 'amount');
  await mapRole(page, 2, 'desc');
  // Leave column 3 (memo) as Ignore.
  await expect(page.locator('tr.import-preview-row')).toHaveCount(1);
  await expect(page.locator('td.import-cell-memo').first()).toHaveText('');
  await page.locator('form.import-confirm-form button[type="submit"]').click();
  await expect(page.locator('p.import-result-summary[role="status"]')).toBeVisible();
  await expect(page.locator('tr.import-result-row')).toHaveCount(1);
});

// p26.62: a FILE-level error (an empty CSV -> no readable rows) is rejected at
// /import/preview with a 422, and the error swaps into #import-workspace as a BARE
// fragment -- NOT a full shell page (which would nest a whole document, duplicating the
// page frame, the p26.35 class of bug). Assert the error shows in-place AND there is no
// second <header>/nav nested inside the workspace. (A bad column MAPPING on the new path
// is an in-fragment hint, not a 422 -- covered by the map+preview test below.)
test('bank import: a file-level error shows in place, no duplicate page frame', async ({ page, server }) => {
  await login(page, server);
  const acctName = await createAssetAccount(page);

  await page.goto('/import');
  await expect(page.getByRole('heading', { name: /import bank csv/i })).toBeVisible();
  await page.locator('#import-subsidiary').selectOption('1');
  await page.locator('#import-account').selectOption({ label: acctName });

  // An empty file -> no readable rows -> a file-level error (bare import-error 422).
  await page.locator('#import-file').setInputFiles({
    name: 'empty.csv',
    mimeType: 'text/csv',
    buffer: Buffer.from('', 'utf8'),
  });

  const previewResp = page.waitForResponse(
    (r) => new URL(r.url()).pathname === '/import/preview' && r.request().method() === 'POST',
  );
  await page.locator('form.import-upload-form button[type="submit"]').click();
  const resp = await previewResp;
  expect(resp.status()).toBe(422);

  // The error message shows inside the workspace (swapped in place).
  const workspace = page.locator('#import-workspace');
  await expect(workspace.locator('p.import-error[role="alert"]')).toBeVisible();

  // NO nested shell inside the workspace, exactly one app header on the page.
  await expect(workspace.locator('header')).toHaveCount(0);
  await expect(workspace.locator('nav.app-nav')).toHaveCount(0);
  await expect(page.locator('.app-header')).toHaveCount(1);
  await expect(page.locator('form.import-upload-form')).toHaveCount(1);
});

test('bank import: upload, map columns, preview, stage; a duplicate row is flagged', async ({ page, server }) => {
  await login(page, server);
  const acctName = await createAssetAccount(page);

  // The second data line duplicates the first -> flagged within the batch.
  const csv =
    'when,total,who,note\n' +
    '2025-01-15,100.00,Acme,Invoice 5\n' +
    '2025-01-16,-42.50,Bob,Refund\n' +
    '2025-01-15,100.00,Acme,Invoice 5\n';
  await uploadCSV(page, acctName, csv, 'statement.csv');

  // No default mapping: the pending hint shows and there is no preview yet.
  await expect(page.locator('p.import-map-pending')).toBeVisible();
  await expect(page.locator('tr.import-preview-row')).toHaveCount(0);

  // Label the real columns: column 0 = Date, column 1 = Amount, column 2 = Description.
  await mapRole(page, 0, 'date');
  await mapRole(page, 1, 'amount');
  await mapRole(page, 2, 'desc');

  // Now the preview co-renders (3 rows).
  await expect(page.locator('tr.import-preview-row')).toHaveCount(3);

  // Stage: the confirm form (carrying the derived mapping + CSV base64) POSTs to /import.
  await page.locator('form.import-confirm-form button[type="submit"]').click();
  await expect(page.locator('p.import-result-summary[role="status"]')).toBeVisible();
  await expect(page.locator('tr.import-result-row')).toHaveCount(3);

  // Exactly one row is flagged a duplicate (the repeated Acme line).
  await expect(page.locator('tr.import-row-duplicate')).toHaveCount(1);
  await expect(page.locator('span.import-dupe-flag').first()).toBeVisible();
});

// p26.64: a bad column MAPPING (Amount pointed at a text column) shows an in-fragment
// error hint -- the selects stay, NO workspace-wiping 422, no nested shell.
test('bank import: a bad column mapping is an in-fragment hint, not a 422', async ({ page, server }) => {
  await login(page, server);
  const acctName = await createAssetAccount(page);
  const csv = 'date,amount,desc\n2025-01-15,100.00,Acme\n';
  await uploadCSV(page, acctName, csv, 'badmap.csv');

  await mapRole(page, 0, 'date');
  // Point Amount at the DESC column (text) -> every row fails to parse.
  await mapRole(page, 2, 'amount');

  // In-fragment error hint; the map form (selects) survives; no preview; no nested shell.
  const hint = page.locator('p.import-map-error[role="alert"]');
  await expect(hint).toBeVisible();
  // The message carries its row-number argument (not a dropped-arg artifact).
  await expect(hint).toContainText(/row\s*1/i);
  await expect(hint).not.toContainText(/MISSING|%!/);
  await expect(page.locator('form.import-map-form')).toBeVisible();
  await expect(page.locator('tr.import-preview-row')).toHaveCount(0);
  await expect(page.locator('#import-workspace header')).toHaveCount(0);
});

// p26.63: a saved mapping profile round-trips (save on stage -> appears in the load
// list -> loading it restores the mapping) and can be DELETED (soft-delete; gone from
// the list). The profile is saved by checking "save this mapping" + naming it and
// staging an upload; then a fresh /import shows it, and the per-profile delete removes
// it.
test('bank import: save a mapping profile, then delete it', async ({ page, server }) => {
  await login(page, server);
  const acctName = await createAssetAccount(page);
  const profileName = 'E2E Profile ' + Math.random().toString(36).slice(2, 8);

  await page.goto('/import');
  await page.locator('#import-subsidiary').selectOption('1');
  await page.locator('#import-account').selectOption({ label: acctName });
  // Save this mapping as a reusable profile (checkbox + name on the upload form; they
  // carry into the confirm form and persist on stage).
  await page.locator('#import-save-profile').check();
  await page.locator('#import-profile-name').fill(profileName);

  const csv = 'date,amount,desc,memo\n2025-03-01,10.00,Acme,Note\n';
  await page.locator('#import-file').setInputFiles({
    name: 'mar.csv',
    mimeType: 'text/csv',
    buffer: Buffer.from(csv, 'utf8'),
  });
  await page.locator('form.import-upload-form button[type="submit"]').click();
  await expect(page.locator('#import-workspace.e2e-settled')).toBeVisible();

  // Map the columns (date/amount/desc) so the preview + confirm form appear.
  await mapRole(page, 0, 'date');
  await mapRole(page, 1, 'amount');
  await mapRole(page, 2, 'desc');
  await expect(page.locator('tr.import-preview-row')).toHaveCount(1);

  // Stage: this is what persists the named profile (with its derived column mapping).
  await page.locator('form.import-confirm-form button[type="submit"]').click();
  await expect(page.locator('p.import-result-summary[role="status"]')).toBeVisible();

  // Fresh upload page: the profile appears in the load list AND the manage section.
  await page.goto('/import');
  await expect(
    page.locator('#import-profile-id option').filter({ hasText: profileName }),
  ).toHaveCount(1);
  const item = page
    .locator('li.import-profile-item')
    .filter({ has: page.getByText(profileName, { exact: true }) });
  await expect(item).toHaveCount(1);

  // Loading it restores the mapping: select the profile, pick the account, upload a
  // same-shape CSV -> the per-column "maps to" selects come back PRE-SELECTED from the
  // saved profile (reverse-mapped), and the preview renders without re-mapping.
  await page.locator('#import-profile-id').selectOption({ label: profileName });
  await expect(page.locator('#import-profile-id')).toHaveValue(/\d+/);
  await page.locator('#import-subsidiary').selectOption('1');
  await page.locator('#import-account').selectOption({ label: acctName });
  await page.locator('#import-file').setInputFiles({
    name: 'again.csv',
    mimeType: 'text/csv',
    buffer: Buffer.from('date,amount,desc,memo\n2025-04-01,20.00,Beta,Note2\n', 'utf8'),
  });
  await page.locator('form.import-upload-form button[type="submit"]').click();
  await expect(page.locator('#import-workspace.e2e-settled')).toBeVisible();
  await expect(page.locator('#import-role-0')).toHaveValue('date');
  await expect(page.locator('#import-role-1')).toHaveValue('amount');
  await expect(page.locator('#import-role-2')).toHaveValue('desc');
  await expect(page.locator('tr.import-preview-row')).toHaveCount(1);

  // Back to a clean page to delete the profile.
  await page.goto('/import');
  const item2 = page
    .locator('li.import-profile-item')
    .filter({ has: page.getByText(profileName, { exact: true }) });
  await expect(item2).toHaveCount(1);

  // Delete it: the per-profile form POSTs to the soft-delete route and HX-Redirects to
  // /import; wait for the GET reload so the refreshed list is in the SSR DOM.
  const reloaded = page.waitForResponse(
    (r) => new URL(r.url()).pathname === '/import' && r.request().method() === 'GET',
  );
  await item2.locator('button.import-profile-delete').click();
  await reloaded;

  // Gone from both the load list and the manage section.
  await expect(
    page.locator('#import-profile-id option').filter({ hasText: profileName }),
  ).toHaveCount(0);
  await expect(
    page.locator('li.import-profile-item').filter({ has: page.getByText(profileName, { exact: true }) }),
  ).toHaveCount(0);
});

// p17.3 review queue -> post: upload+stage two rows, open the review queue, "edit &
// post" the first pending row (the editor opens prefilled with the subsidiary LOCKED;
// a counter-split is added to balance, then posted), and discard the second with a
// reason. The posted row shows posted (and the txn is in the account register); the
// discarded row shows discarded.
test('bank import: review queue -> edit&post one row and discard another', async ({ page, server }) => {
  await login(page, server);
  const bankName = await createAssetAccount(page);
  // A counter account (expense) to balance the posted transaction against.
  const expenseName = await createExpenseAccount(page);

  // Stage a 2-row statement: upload, label the columns, then confirm.
  const csv =
    'date,amount,desc,memo\n' +
    '2025-02-10,-30.00,Landlord,Rent\n' +
    '2025-02-11,-15.00,Cafe,Coffee\n';
  await uploadCSV(page, bankName, csv, 'feb.csv');
  await mapRole(page, 0, 'date');
  await mapRole(page, 1, 'amount');
  await mapRole(page, 2, 'desc');
  await expect(page.locator('tr.import-preview-row')).toHaveCount(2);
  await page.locator('form.import-confirm-form button[type="submit"]').click();
  await expect(page.locator('p.import-result-summary[role="status"]')).toBeVisible();

  // Go to the review queue via the result's review link.
  await page.locator('a.import-review-link').click();
  await page.waitForURL('**/import/batches/**');
  await expect(page.getByRole('heading', { name: /review import/i })).toBeVisible();
  await expect(page.locator('tr.import-queue-row')).toHaveCount(2);
  // Progress starts at 0 posted, 0 discarded, 2 pending.
  await expect(page.locator('p.import-progress')).toContainText('pending');

  // "Edit & post" the first pending row.
  await page.locator('tr.import-queue-row').first().locator('a.import-edit-post').click();
  await page.waitForURL('**/import/rows/**/edit');
  await expect(page.locator('form#txn-form')).toBeVisible();

  // The subsidiary is LOCKED (disabled) and the bank line is prefilled in row 0.
  await expect(page.locator('#txn-subsidiary')).toBeDisabled();
  await expect(page.locator('#txn-account-0')).toHaveValue(/\d+/);

  // Add the counter split (the expense account, +30.00) so it balances. p26.41: picking the
  // expense account auto-defaults the combined program/class control to a program pick
  // (p:<id>), which decodes to class=program server-side -- no separate class choice needed.
  await page.locator('#txn-account-1').selectOption({ label: expenseName });
  await page.locator('#txn-amount-1').fill('30.00');
  await expect(page.locator('#txn-progclass-1')).toHaveValue(/^p:\d+$/);

  // Post: the editor submits via hx-post and gets an HX-Redirect back to the batch
  // queue (a client navigation). Wait for the queue GET reload response so the posted
  // row is in the SSR DOM before asserting (deterministic, not URL-timing luck).
  const postReloaded = page.waitForResponse(
    (r) => /^\/import\/batches\/\d+$/.test(new URL(r.url()).pathname) && r.request().method() === 'GET',
  );
  await page.locator('form#txn-form button[type="submit"]').click();
  await postReloaded;
  await expect(page.locator('tr.import-row-posted')).toHaveCount(1);

  // The remaining pending row: discard with a reason. The page is ALREADY at
  // /import/batches/{id} (post redirected here), so waitForURL would be a no-op and
  // race the POST->303->GET reload; wait for the reload RESPONSE instead (the
  // helpers.js flake class), set up BEFORE the click and matched by pathname.
  const pending = page.locator('tr.import-queue-row').filter({ has: page.locator('a.import-edit-post') });
  await expect(pending).toHaveCount(1);
  await pending.locator('input.import-discard-reason').fill('personal, not the org');
  const discardReloaded = page.waitForResponse(
    (r) => /^\/import\/batches\/\d+$/.test(new URL(r.url()).pathname) && r.request().method() === 'GET',
  );
  await pending.locator('button.import-discard-btn').click();
  await discardReloaded;
  await expect(page.locator('tr.import-row-discarded')).toHaveCount(1);

  // Progress now reflects 1 posted, 1 discarded, 0 pending.
  await expect(page.locator('tr.import-queue-row').filter({ has: page.locator('a.import-edit-post') })).toHaveCount(0);
});
