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

test('bank import: upload, map, preview, stage; a duplicate row is flagged', async ({ page, server }) => {
  await login(page, server);
  const acctName = await createAssetAccount(page);

  await page.goto('/import');
  await expect(page.getByRole('heading', { name: /import bank csv/i })).toBeVisible();

  // Pick the target subsidiary (root "Organization", id 1) and the new account.
  await page.locator('#import-subsidiary').selectOption('1');
  await page.locator('#import-account').selectOption({ label: acctName });

  // Default mapping (date=0, amount=1, payee=2, memo=3, ISO, comma, header) matches
  // this CSV. The second line duplicates the first -> flagged within the batch.
  const csv =
    'date,amount,payee,memo\n' +
    '2025-01-15,100.00,Acme,Invoice 5\n' +
    '2025-01-16,-42.50,Bob,Refund\n' +
    '2025-01-15,100.00,Acme,Invoice 5\n';

  await page.locator('#import-file').setInputFiles({
    name: 'statement.csv',
    mimeType: 'text/csv',
    buffer: Buffer.from(csv, 'utf8'),
  });

  // Preview: the form POSTs multipart to /import/preview and swaps the preview into
  // #import-workspace. Wait for the swap to settle (e2e-settled on the target).
  await page.locator('form.import-upload-form button[type="submit"]').click();
  await expect(page.locator('#import-workspace.e2e-settled')).toBeVisible();
  await expect(page.locator('tr.import-preview-row')).toHaveCount(3);

  // Stage: the confirm form (carrying the CSV base64) POSTs to /import and swaps the
  // result in. The result summary shows the staged count and a flagged duplicate.
  await page.locator('form.import-confirm-form button[type="submit"]').click();
  await expect(page.locator('p.import-result-summary[role="status"]')).toBeVisible();
  await expect(page.locator('tr.import-result-row')).toHaveCount(3);

  // Exactly one row is flagged a duplicate (the repeated Acme line).
  await expect(page.locator('tr.import-row-duplicate')).toHaveCount(1);
  await expect(page.locator('span.import-dupe-flag').first()).toBeVisible();
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

  // Stage a 2-row statement.
  await page.goto('/import');
  await page.locator('#import-subsidiary').selectOption('1');
  await page.locator('#import-account').selectOption({ label: bankName });
  const csv =
    'date,amount,payee,memo\n' +
    '2025-02-10,-30.00,Landlord,Rent\n' +
    '2025-02-11,-15.00,Cafe,Coffee\n';
  await page.locator('#import-file').setInputFiles({
    name: 'feb.csv',
    mimeType: 'text/csv',
    buffer: Buffer.from(csv, 'utf8'),
  });
  await page.locator('form.import-upload-form button[type="submit"]').click();
  await expect(page.locator('#import-workspace.e2e-settled')).toBeVisible();
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

  // Add the counter split (the expense account, +30.00, class program) so it balances.
  await page.locator('#txn-account-1').selectOption({ label: expenseName });
  await page.locator('#txn-amount-1').fill('30.00');
  await page.locator('#txn-class-1').selectOption('program');

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
