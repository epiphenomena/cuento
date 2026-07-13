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
  await page.goto('/accounts');
  await page.getByRole('button', { name: /new account/i }).click();
  await expect(page.locator('#af-name-en')).toBeVisible();
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
