// @ts-check
// Functional test of the REAL transaction editor (p12.2). It drives the actual
// /transactions/new grid served by `cuento serve -dev` against a fresh migrated db
// with a seeded admin (is_admin -> TxnWrite). It logs in, creates two asset accounts
// through the inline chart-of-accounts form, opens the editor from an account
// register, enters a balanced 2-split transfer through the real grid (account
// comboboxes, signed amounts, fund selects), saves, and asserts the entry posted and
// appears in the destination account's register. The keyboard-only pass is p12.6.
// Selectors come from transaction_form.tmpl / register.tmpl / accounts.tmpl.

const { test, expect } = require('../fixtures');
const { openNewAccount, saveAccount } = require('../helpers');

// The htmx settle marker (`e2e-settled` on each htmx:afterSettle target) is installed
// centrally by the `page` fixture — see fixtures.js for why (hx-* triggers on a
// swapped-in node are wired on the settle tick, after it paints, so a synthetic
// action right after `toBeVisible()` can beat the wiring; the strict CSP rules out
// waitForFunction, so we mark the DOM). Waiting for `.e2e-settled` before driving a
// freshly-swapped hx-trigger makes it race-free.

async function login(page, server) {
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');
}

// createAsset makes a leaf asset account (the form's default type, so no type-change
// re-fetch) mapped to the root subsidiary. Waits for the form to settle before Save
// (so its hx-post is wired) and for the reload response (so the new row is in the SSR
// DOM) — see createLeaf in merge.spec.js for the full rationale.
async function createAsset(page, name) {
  await openNewAccount(page);
  await page.locator('#af-name-en').fill(name);
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) {
    await rootSub.check();
  }
  await saveAccount(page);
  await expect(page.locator('tr.acct-row', { hasText: name })).toBeVisible();
}

// createFund makes an active fund scoped to the root subsidiary + root program
// ("General") through the inline /funds form, so the txn editor's fund combo has a real
// option to filter (a fresh -dev db has none). Mirrors funds.spec.js.
async function createFund(page, name) {
  await page.goto('/funds');
  await page.getByRole('button', { name: /new fund/i }).click();
  await expect(page.locator('form#fund-form.e2e-settled')).toBeVisible();
  await page.locator('#ff-name').fill(name);
  await page.locator('#ff-program').selectOption({ label: 'General' });
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) await rootSub.check();
  const reloaded = page.waitForResponse(
    (r) => r.url().endsWith('/funds') && r.request().method() === 'GET',
  );
  await page.getByRole('button', { name: /^save$/i }).click();
  await reloaded;
  await expect(page.locator('tr.fund-row', { hasText: name })).toBeVisible();
}

test.describe('transaction editor', () => {
  test('enters a balanced 2-split transaction and it appears in the register', async ({ page, server }) => {
    await login(page, server);

    // Two leaf asset accounts to move money between.
    await createAsset(page, 'Editor Checking');
    await createAsset(page, 'Editor Savings');

    // Open the editor from Editor Checking's register (the everyday entry point). p25:
    // the account name is the register link (the Register button was dropped).
    const row = page.locator('tr.acct-row', { hasText: 'Editor Checking' });
    await row.getByRole('link', { name: 'Editor Checking' }).click();
    await page.waitForURL('**/register');
    await page.getByRole('link', { name: /new transaction/i }).click();
    await page.waitForURL('**/transactions/new');

    // The grid renders with its header and a single starter row (p25.2: it auto-appends
    // a fresh trailing row as each row is filled, no "Add row" button).
    await expect(page.locator('form#txn-form')).toBeVisible();
    await expect(page.locator('#txn-account-0')).toBeVisible();
    await expect(page.locator('#txn-account-1')).toHaveCount(0);

    // p26.23: the apply-fund control is GONE (removed with the payee -> per-split
    // description migration; the fund defaults to Unrestricted).
    await expect(page.locator('#txn-apply-fund')).toHaveCount(0);
    await expect(page.locator('#txn-apply-fund-btn')).toHaveCount(0);

    // p26.23: DESCRIPTION is the FIRST grid column (before Account). Assert the header
    // order and that the description cell is the FIRST cell of a split's first row.
    // p26.32: each split is a <tbody class="txn-row"> holding two <tr>; row 1
    // (.txn-row-main) carries description / account / amount, so the first td of the
    // split's first <tr> is the description cell.
    const headerCells = page.locator('.txn-grid thead th');
    await expect(headerCells.first()).toHaveText(/description/i);
    await expect(headerCells.nth(1)).toHaveText(/account/i);
    const row0MainCells = page.locator('.txn-row[data-row="0"] .txn-row-main > td');
    await expect(row0MainCells.first()).toHaveClass(/txn-desc-cell/);
    // And the description input still enhances (autocompletes) in its new position:
    // typing prefills from a prior split is covered in payee-autofill.spec; here we just
    // assert the input is present + typable as the first cell.
    await expect(page.locator('#txn-desc-0')).toBeVisible();

    // Fill a balanced transfer: DR Editor Savings 25.00, CR Editor Checking 25.00.
    // The account combobox is a real <select> (ARIA listbox enhancement is progressive
    // -- selectOption drives the underlying control). Amounts are the SIGNED column
    // (signed display mode; the admin's default). Filling row 0 grows row 1.
    await page.locator('#txn-account-0').selectOption({ label: 'Editor Savings' });
    await expect(page.locator('#txn-account-1')).toBeVisible();
    await page.locator('#txn-amount-0').fill('25.00');
    await page.locator('#txn-account-1').selectOption({ label: 'Editor Checking' });
    await page.locator('#txn-amount-1').fill('-25.00');

    // Save (a plain submit; success redirects to the first split's register).
    await page.getByRole('button', { name: /^save$/i }).click();

    // We land on a register; the transfer is posted. Navigate to Editor Savings'
    // register and assert a row with the 25.00 amount is present (the entry exists).
    await page.waitForURL('**/register**');
    await expect(page.locator('table.register-table')).toBeVisible();

    // The saved amount appears somewhere in the register table.
    await expect(page.locator('table.register-table')).toContainText('25.00');
  });

  test('shows the program/class selects only on R/E rows, prefilled from the account default', async ({ page, server }) => {
    await login(page, server);

    // A checking account, then an expense account with a default functional class
    // (Management & general).
    await createAsset(page, 'Editor Bank');
    await openNewAccount(page);
    // Choosing the expense type triggers an htmx form-swap (hx-get -> #account-form,
    // outerHTML) that server-renders the functional-class default field (#af-func is
    // gated by {{if .IsExpense}}). #af-func becoming visible is the swap's own signal.
    // The handler round-trips typed values (overlayFormValues), so name/sub survive.
    await page.locator('#af-type').selectOption('expense');
    await expect(page.locator('#af-func')).toBeVisible();
    await page.locator('#af-name-en').fill('Editor Rent');
    const rootSub = page.locator('input[name="sub_1"]');
    if (!(await rootSub.isChecked())) await rootSub.check();
    await page.locator('#af-func').selectOption('management');
    // #af-func visible means the expense re-fetch swapped in (old form gone), so
    // saveAndReload's `.e2e-settled` wait now tracks THIS form — it waits for the
    // re-rendered Save's hx-post to be wired, then for the reload response.
    await saveAccount(page);
    await expect(page.locator('tr.acct-row', { hasText: 'Editor Rent' })).toBeVisible();

    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();

    // Row 0: pick the ASSET account -> program + class selects stay hidden.
    await page.locator('#txn-account-0').selectOption({ label: 'Editor Bank' });
    await expect(page.locator('#txn-class-0')).toBeHidden();

    // Row 1: pick the EXPENSE account -> the class select becomes visible and is
    // prefilled from the account's default (management); the program select shows.
    await page.locator('#txn-account-1').selectOption({ label: 'Editor Rent' });
    await expect(page.locator('#txn-class-1')).toBeVisible();
    await expect(page.locator('#txn-class-1')).toHaveValue('management');
    await expect(page.locator('#txn-program-1')).toBeVisible();
  });

  // p26.2: the account select is enhanced into a fuzzy-filter combobox (combobox.js +
  // combofilter.js). The native <select> stays the value sink (the two tests above still
  // drive it via selectOption / arrow keys). This test drives the OVERLAY input: typing
  // filters the listbox, a pick sets the underlying select + fires gating, and the
  // cloned trailing row's combobox filters + picks correctly too.
  test('account combobox filters + picks by typing, and the cloned row does too', async ({ page, server }) => {
    await login(page, server);

    // A checking account and an expense account (whose pick must fire the class gating).
    await createAsset(page, 'Combo Checking');
    await openNewAccount(page);
    await page.locator('#af-type').selectOption('expense');
    await expect(page.locator('#af-func')).toBeVisible();
    await page.locator('#af-name-en').fill('Combo Rent');
    const rootSub = page.locator('input[name="sub_1"]');
    if (!(await rootSub.isChecked())) await rootSub.check();
    await page.locator('#af-func').selectOption('management');
    await saveAccount(page);
    await expect(page.locator('tr.acct-row', { hasText: 'Combo Rent' })).toBeVisible();

    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();

    // Row 0's account cell has a .combo overlay (the enhancement ran on load).
    const cell0 = page.locator('.txn-row[data-row="0"] .txn-account-cell');
    const input0 = cell0.locator('.combo-text');
    await expect(input0).toBeVisible();

    // Type a fuzzy leaf query -> the listbox filters to matching options only. "rent"
    // matches "Combo Rent" and excludes "Combo Checking".
    await input0.click();
    await input0.fill('rent');
    const list0 = cell0.locator('.combo-list');
    await expect(list0).toBeVisible();
    await expect(list0.locator('.combo-option', { hasText: 'Combo Rent' })).toBeVisible();
    await expect(list0.locator('.combo-option', { hasText: 'Combo Checking' })).toHaveCount(0);

    // Pick it -> the native select value is set AND the expense gating fires (class cell
    // shows, prefilled from the account default). This proves change bubbled + datasets
    // survived the enhancement.
    await list0.locator('.combo-option', { hasText: 'Combo Rent' }).click();
    await expect(page.locator('#txn-account-0')).toHaveValue(/\d+/);
    await expect(input0).toHaveValue(/Combo Rent/);
    await expect(page.locator('#txn-class-0')).toBeVisible();
    await expect(page.locator('#txn-class-0')).toHaveValue('management');

    // Picking in the last row grew a fresh trailing row (auto-append via the bubbled
    // change). Its account cell is ALSO an enhanced combobox (the clone contract).
    const cell1 = page.locator('.txn-row[data-row="1"] .txn-account-cell');
    const input1 = cell1.locator('.combo-text');
    await expect(input1).toBeVisible();
    await input1.click();
    await input1.fill('check');
    const list1 = cell1.locator('.combo-list');
    await expect(list1.locator('.combo-option', { hasText: 'Combo Checking' })).toBeVisible();
    await list1.locator('.combo-option', { hasText: 'Combo Checking' }).click();
    await expect(page.locator('#txn-account-1')).toHaveValue(/\d+/);
    await expect(input1).toHaveValue(/Combo Checking/);
    // Asset pick -> class cell hidden (gating fired on the CLONED row's select).
    await expect(page.locator('#txn-class-1')).toBeHidden();

    // p26.11: FOCUS RING on the account combo. The native <select> is the Tab stop but the
    // opaque overlay covers its native ring, so the ring is painted on the overlay via
    // `.combo:focus-within > .combo-text`. Focus the account cell and assert a real focus
    // outline is present (not "none"/"0px"). This guards the reported "no visible focus when
    // tabbing to the account cell" regression.
    const focusCell = page.locator('.txn-row[data-row="0"] .txn-account-cell');
    await expect(focusCell.locator('.combo')).toHaveClass(/combo/); // wrapper present
    // Focus the REAL Tab stop (the native <select>, which the overlay covers) -- this is the
    // exact reported scenario (tab onto the account cell) -- and assert the overlay carries a
    // real focus outline via `.combo:focus-within > .combo-text` (not "none"/"0px"). Focusing
    // the overlay alone would only prove the overlay-focus path, not the covered-select path.
    await page.locator('#txn-account-0').focus();
    const overlayOutline = await focusCell.locator('.combo-text').evaluate((el) => {
      const s = getComputedStyle(el);
      return { style: s.outlineStyle, width: s.outlineWidth };
    });
    expect(overlayOutline.style).not.toBe('none');
    expect(overlayOutline.width).not.toBe('0px');
  });

  // p26.3: the fund and program selects are ALSO enhanced into comboboxes. This drives
  // the overlay inputs: typing filters, a pick sets the underlying select. Program is
  // shown only on an R/E row (gated), so we pick an expense account first.
  test('fund and program combos filter + pick by typing', async ({ page, server }) => {
    await login(page, server);
    await createFund(page, 'Water Grant Combo');

    // An expense account so the program cell is revealed on its row.
    await openNewAccount(page);
    await page.locator('#af-type').selectOption('expense');
    await expect(page.locator('#af-func')).toBeVisible();
    await page.locator('#af-name-en').fill('Combo Supplies');
    const rootSub2 = page.locator('input[name="sub_1"]');
    if (!(await rootSub2.isChecked())) await rootSub2.check();
    await page.locator('#af-func').selectOption('management');
    await saveAccount(page);
    await expect(page.locator('tr.acct-row', { hasText: 'Combo Supplies' })).toBeVisible();

    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();

    // Pick the expense account via its combo so the program cell reveals (R/E gating).
    const acctCell = page.locator('.txn-row[data-row="0"] .txn-account-cell');
    await acctCell.locator('.combo-text').click();
    await acctCell.locator('.combo-text').fill('supplies');
    await acctCell.locator('.combo-option', { hasText: 'Combo Supplies' }).click();
    await expect(page.locator('#txn-program-0')).toBeVisible();

    // Fund combo: type-filter to the created fund and pick it -> the native select gets
    // its value AND the overlay shows the label.
    const fundCell = page.locator('.txn-row[data-row="0"] .txn-fund-cell');
    const fundInput = fundCell.locator('.combo-text');
    await expect(fundInput).toBeVisible();
    await fundInput.click();
    await fundInput.fill('water');
    await expect(fundCell.locator('.combo-option', { hasText: 'Water Grant Combo' })).toBeVisible();
    await fundCell.locator('.combo-option', { hasText: 'Water Grant Combo' }).click();
    await expect(page.locator('#txn-fund-0')).toHaveValue(/\d+/);
    await expect(fundInput).toHaveValue(/Water Grant Combo/);

    // Program combo: filter to "General" (the seeded root program) and pick it.
    const progCell = page.locator('.txn-row[data-row="0"] .txn-program-cell');
    const progInput = progCell.locator('.combo-text');
    await progInput.click();
    await progInput.fill('gene');
    await expect(progCell.locator('.combo-option', { hasText: 'General' })).toBeVisible();
    await progCell.locator('.combo-option', { hasText: 'General' }).click();
    await expect(page.locator('#txn-program-0')).toHaveValue(/\d+/);
    await expect(progInput).toHaveValue(/General/);
  });

  // p26.10: editing a transaction whose split references a now-INACTIVE account must
  // still DISPLAY that account (the real name, marked "(unavailable)") in the row's
  // account cell -- NOT a blank "Choose account" select (the reported "missing accounts"
  // bug). We post a transfer, deactivate one leg's account, then reopen the editor.
  test('edit shows a split whose account was deactivated (not a blank cell)', async ({ page, server }) => {
    await login(page, server);
    await createAsset(page, 'Gone Checking');
    await createAsset(page, 'Live Savings');

    // Post a balanced transfer using both accounts.
    await page.goto('/accounts');
    const row = page.locator('tr.acct-row', { hasText: 'Live Savings' });
    await row.getByRole('link', { name: 'Live Savings' }).click();
    await page.waitForURL('**/register');
    await page.getByRole('link', { name: /new transaction/i }).click();
    await page.waitForURL('**/transactions/new');
    await page.locator('#txn-account-0').selectOption({ label: 'Live Savings' });
    await expect(page.locator('#txn-account-1')).toBeVisible();
    await page.locator('#txn-amount-0').fill('40.00');
    await page.locator('#txn-account-1').selectOption({ label: 'Gone Checking' });
    await page.locator('#txn-amount-1').fill('-40.00');
    await page.getByRole('button', { name: /^save$/i }).click();
    await page.waitForURL('**/register**');

    // Deactivate Gone Checking from the accounts list (a plain POST form).
    await page.goto('/accounts');
    const goneRow = page.locator('tr.acct-row', { hasText: 'Gone Checking' });
    await goneRow.getByRole('button', { name: /deactivate/i }).click();
    await expect(page.locator('tr.acct-row', { hasText: 'Gone Checking' }).locator('.badge')).toBeVisible();

    // Reopen the transaction editor from Live Savings' register (the edit link).
    await page.goto('/accounts');
    await page.locator('tr.acct-row', { hasText: 'Live Savings' }).getByRole('link', { name: 'Live Savings' }).click();
    await page.waitForURL('**/register');
    await page.getByRole('link', { name: /edit/i }).first().click();
    await page.waitForURL('**/transactions/*/edit');
    await expect(page.locator('form#txn-form')).toBeVisible();

    // Every row's account select carries the full option list, so the injected
    // "(unavailable)" option appears in each. The point is that it is SELECTED in the
    // row whose split references it -- not left on the "Choose account" placeholder.
    const marked = page.locator('#txn-form option[data-unavailable="1"]', { hasText: 'Gone Checking' });
    await expect(marked.first()).toContainText('(unavailable)');

    // Exactly one row has that option SELECTED (the deactivated split's row).
    const goneCell = page.locator('.txn-account-cell', { has: page.locator('option[data-unavailable="1"]:checked') });
    await expect(goneCell).toHaveCount(1);

    // The combobox overlay for that row shows the real account label (not blank -- the
    // reported symptom). optionLabel reads data-path, into which the marker was appended.
    const overlay = goneCell.locator('.combo-text');
    await expect(overlay).toHaveValue(/Gone Checking.*unavailable/);
  });

  // p26.10 (client guard): a row that carries content (an amount) but NO account must
  // not silently post -- the save is blocked and a per-row error is shown. The htmx POST
  // must NOT fire (the transaction is not created).
  test('a content row with no account is blocked with an error, not posted', async ({ page, server }) => {
    await login(page, server);
    await createAsset(page, 'Guard Cash');

    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();

    // Row 0: an amount but leave the account on the "Choose account" placeholder (0).
    await page.locator('#txn-amount-0').fill('12.00');

    // Assert NO POST /transactions fires when we click Save.
    let posted = false;
    page.on('request', (req) => {
      if (req.method() === 'POST' && /\/transactions(\?|$)/.test(req.url())) posted = true;
    });
    await page.getByRole('button', { name: /^save$/i }).click();

    // A per-row error appears in the row's error cell, and we stay on the editor.
    await expect(page.locator('.txn-row[data-row="0"] .txn-row-error .field-error')).toBeVisible();
    await expect(page).toHaveURL(/\/transactions\/new/);
    // Give any (erroneous) request a beat to have fired, then assert none did.
    await page.waitForTimeout(300);
    expect(posted).toBe(false);
  });

  // p26.23: the transaction grid gains the per-row × delete affordance (the same one the
  // expense grid has). Deleting a MIDDLE row re-indexes the survivors (contiguous _i) so the
  // memo text follows; deleting down to the last data row never drops below one trailing
  // empty row; the delete cell is the trailing column, after the error column.
  test('the × deletes a row, re-indexes survivors, and the last-row reset keeps one empty row', async ({ page, server }) => {
    await login(page, server);
    await createAsset(page, 'Del Checking');
    await createAsset(page, 'Del Savings');

    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();

    // Column-order guard: the delete cell is the LAST td of the split's second row
    // (p26.32), right after the error cell.
    const row0MoreCells = page.locator('.txn-row[data-row="0"] .txn-row-more > td');
    await expect(row0MoreCells.last()).toHaveClass(/txn-delete-cell/);
    await expect(
      page.locator('.txn-row[data-row="0"] .txn-row-error + .txn-delete-cell'),
    ).toHaveCount(1);

    // Build three data rows with distinguishable memos (auto-append grows the trailing row).
    await page.locator('#txn-account-0').selectOption({ label: 'Del Savings' });
    await page.locator('#txn-memo-0').fill('row-zero-memo');
    await expect(page.locator('#txn-account-1')).toBeVisible();
    await page.locator('#txn-account-1').selectOption({ label: 'Del Checking' });
    await page.locator('#txn-memo-1').fill('row-one-memo');
    await expect(page.locator('#txn-account-2')).toBeVisible();
    await page.locator('#txn-account-2').selectOption({ label: 'Del Savings' });
    await page.locator('#txn-memo-2').fill('row-two-memo');
    await expect(page.locator('#txn-account-3')).toBeVisible();

    // Delete the MIDDLE row (row-one-memo). Survivors re-index, so row-two-memo shifts to
    // index 1 and the memo text follows.
    await page.locator('.txn-row[data-row="1"] .txn-delete').click();
    await expect(page.locator('#txn-memo-1')).toHaveValue('row-two-memo');
    const rowsCount = Number(await page.locator('#txn-rows-count').inputValue());
    await expect(page.locator(`#txn-account-${rowsCount - 1}`)).toBeVisible();
    await expect(page.locator(`#txn-account-${rowsCount}`)).toHaveCount(0);

    // Delete down to a single trailing empty row: repeatedly delete row 0.
    let guard = 0;
    while (guard < 10) {
      const remaining = Number(await page.locator('#txn-rows-count').inputValue());
      if (remaining <= 1) break;
      await page.locator('.txn-row[data-row="0"] .txn-delete').click();
      guard += 1;
    }
    await expect(page.locator('.txn-row')).toHaveCount(1);
    await expect(page.locator('#txn-account-0')).toHaveValue('0');
    await expect(page.locator('#txn-rows-count')).toHaveValue('1');

    // Deleting the ONLY/last row resets it in place (never drops to zero rows).
    await page.locator('#txn-memo-0').fill('leftover');
    await page.locator('.txn-row[data-row="0"] .txn-delete').click();
    await expect(page.locator('.txn-row')).toHaveCount(1);
    await expect(page.locator('#txn-account-0')).toHaveValue('0');
    await expect(page.locator('#txn-memo-0')).toHaveValue('');
    await expect(page.locator('#txn-rows-count')).toHaveValue('1');
  });
});
