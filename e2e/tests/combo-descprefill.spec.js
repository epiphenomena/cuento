// @ts-check
// p26.31 REPRO/regression: the transaction editor + expense grid combobox / description
// prefill bugs the user reported. The existing txn-editor + payee-autofill specs already
// prove fresh-load typing/pick/gating work, so these assertions target the STATE-DEPENDENT
// paths reached AFTER a description prefill or an auto-append: (1) the account combo overlay
// stays typable after a prefill; (2) re-focusing a picked account re-opens the listbox;
// (3) the fund overlay reads "Unrestricted" (value 0) on load; (4) picking a description
// lands the prefill on the SAME row (not i+1). Selectors from transaction_form.tmpl /
// expense_detail.tmpl.

const { test, expect } = require('../fixtures');
const { openNewAccount, saveAccount } = require('../helpers');

async function installSettleMarker(page) {
  await page.addInitScript(() => {
    document.addEventListener('htmx:afterSettle', (e) => {
      const t = /** @type {any} */ (e.target);
      if (t && t.classList) t.classList.add('e2e-settled');
    });
  });
}

async function login(page, server) {
  await installSettleMarker(page);
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');
}

async function createAsset(page, name) {
  await openNewAccount(page);
  await page.locator('#af-name-en').fill(name);
  await page.locator('#af-type').selectOption('asset');
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) await rootSub.check();
  await saveAccount(page);
  await expect(page.locator('tr.acct-row', { hasText: name })).toBeVisible();
}

test.describe('combobox + description prefill row-targeting', () => {
  // Bug 3: the fund combo's value-0 default ("Unrestricted") must SHOW in the overlay on
  // load -- not a blank box. Per p26.22 the combo shows value-0 labels (currentLabel), so the
  // fund reads "Unrestricted"; resyncCombos (run by gateRow on load) must NOT blank it.
  test('fund overlay reads Unrestricted (value 0) on load', async ({ page, server }) => {
    await login(page, server);
    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();

    const fundOverlay = page.locator('.txn-row[data-row="0"] .txn-fund-cell .combo-text');
    await expect(fundOverlay).toBeVisible();
    // value-0 fund is "Unrestricted" -- the overlay must render that label, not blank.
    await expect(page.locator('#txn-fund-0')).toHaveValue('0');
    await expect(fundOverlay).toHaveValue(/unrestricted/i);
  });

  // Bug 4 (+ 1): the reported "next row down" scenario -- row 0 is ALREADY filled, so the
  // description is typed+picked on the TRAILING empty row (row 1). The prefill must land on
  // row 1 (the row whose description was picked), NOT leak to the freshly auto-appended
  // row 2; and row 1's account overlay must stay typable afterwards.
  test('description prefill on a trailing row lands on THAT row, whose account stays typable', async ({ page, server }) => {
    await login(page, server);
    await createAsset(page, 'Rowtgt Checking');
    await createAsset(page, 'Rowtgt Savings');

    // Seed a split carrying a description on a prior transaction.
    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();
    await page.locator('#txn-account-0').selectOption({ label: 'Rowtgt Savings' });
    await page.locator('#txn-amount-0').fill('40.00');
    await page.locator('#txn-desc-0').fill('Rowtarget recall');
    await page.locator('#txn-account-1').selectOption({ label: 'Rowtgt Checking' });
    await page.locator('#txn-amount-1').fill('-40.00');
    await page.getByRole('button', { name: /^save$/i }).click();
    await page.waitForURL('**/register**');

    // New entry: FILL row 0 first (so a trailing empty row 1 exists), then type+pick the
    // description on row 1 -- the "next row down" case the user hit.
    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();
    await page.locator('#txn-account-0').selectOption({ label: 'Rowtgt Checking' });
    await page.locator('#txn-amount-0').fill('-40.00');
    await expect(page.locator('#txn-desc-1')).toBeVisible(); // trailing row auto-appended

    const desc1 = page.locator('#txn-desc-1');
    await desc1.click();
    await desc1.fill('Rowtarget');
    const suggestion = page.locator('#txn-desc-list-1 .desc-suggestion', { hasText: 'Rowtarget recall' });
    await expect(suggestion).toBeVisible();
    await suggestion.click();

    // The prefill landed on ROW 1 (the row whose description was picked), NOT row 2.
    await expect(page.locator('#txn-amount-1')).toHaveValue('40.00');
    await expect(page.locator('#txn-account-1')).not.toHaveValue('0');
    // The newly auto-appended row 2 stays empty -- the prefill did NOT leak onto it.
    await expect(page.locator('#txn-account-2')).toHaveValue('0');
    await expect(page.locator('#txn-amount-2')).toHaveValue('');

    // Bug 1: after the prefill, row 1's account overlay must still accept typing.
    const acctCell1 = page.locator('.txn-row[data-row="1"] .txn-account-cell');
    const acctInput1 = acctCell1.locator('.combo-text');
    await acctInput1.click();
    await acctInput1.fill('savings');
    await expect(acctInput1).toHaveValue('savings');
    await expect(acctCell1.locator('.combo-list')).toBeVisible();
    await expect(acctCell1.locator('.combo-option', { hasText: 'Rowtgt Savings' })).toBeVisible();
  });

  // Bug 4 via the BLUR-commit path (type the full description and Tab away, no click-pick):
  // the async prefill resolves the row from the input at fetch time. On a filled row 0 with a
  // trailing row 1, commit the description on row 1 by blurring -> prefill must land on row 1.
  test('description blur-commit on a trailing row prefills THAT row (not the next)', async ({ page, server }) => {
    await login(page, server);
    await createAsset(page, 'Blur Checking');
    await createAsset(page, 'Blur Savings');

    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();
    await page.locator('#txn-account-0').selectOption({ label: 'Blur Savings' });
    await page.locator('#txn-amount-0').fill('18.00');
    await page.locator('#txn-desc-0').fill('Blur commit recall');
    await page.locator('#txn-account-1').selectOption({ label: 'Blur Checking' });
    await page.locator('#txn-amount-1').fill('-18.00');
    await page.getByRole('button', { name: /^save$/i }).click();
    await page.waitForURL('**/register**');

    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();
    await page.locator('#txn-account-0').selectOption({ label: 'Blur Checking' });
    await page.locator('#txn-amount-0').fill('-18.00');
    await expect(page.locator('#txn-desc-1')).toBeVisible();

    // Type the FULL description on row 1 and blur (Tab out) -- the commit-on-blur path.
    const desc1 = page.locator('#txn-desc-1');
    await desc1.click();
    await desc1.fill('Blur commit recall');
    await desc1.blur();

    // The prefill lands on row 1 (the committed row), not the auto-appended next row.
    await expect(page.locator('#txn-amount-1')).toHaveValue('18.00');
    await expect(page.locator('#txn-account-1')).not.toHaveValue('0');
    await expect(page.locator('#txn-account-2')).toHaveValue('0');
    await expect(page.locator('#txn-amount-2')).toHaveValue('');
  });

  // Bugs 1 + 2 (shared cause): the blur handler's setTimeout(close, 120) is NEVER cancelled.
  // When focus returns within 120ms -- a real user re-focusing a cell -- the stale timer
  // still fires: close() hides the just-reopened listbox (bug 2), and syncInputToSelection()
  // rewrites input.value to the selection label, WIPING the text the user just typed (bug 1:
  // "won't let me enter anything"). Playwright's sequential actions are >120ms apart, so the
  // timer fires harmlessly between them -- which is why the other specs pass. This probe
  // drives focus->blur->focus SYNCHRONOUSLY (all within the 120ms window), then types, then
  // waits past 120ms and asserts BOTH the typed text survived AND the listbox is still open.
  test('a fast re-focus does not wipe typed text or close the listbox (blur-timer race)', async ({ page, server }) => {
    await login(page, server);
    await createAsset(page, 'Race Checking');
    await createAsset(page, 'Race Savings');

    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();

    const cell0 = page.locator('.txn-row[data-row="0"] .txn-account-cell');
    const input0 = cell0.locator('.combo-text');
    const list0 = cell0.locator('.combo-list');

    // Synchronously focus -> blur (schedules the 120ms close) -> focus again (should cancel
    // the stale timer), then type a filter query and fire `input`. All inside one tick.
    await input0.evaluate((el) => {
      el.focus();
      el.blur();
      el.focus();
      el.value = 'race';
      el.dispatchEvent(new Event('input', { bubbles: true }));
    });

    // Wait PAST the 120ms blur timer. If it is not cancelled it will now fire, wiping the
    // typed text and closing the list.
    await page.waitForTimeout(220);

    // Bug 1: the typed text survived (the stale timer did NOT rewrite input.value).
    await expect(input0).toHaveValue('race');
    // Bug 2: the listbox is still open and filtered (the stale timer did NOT close it).
    await expect(list0).toBeVisible();
    await expect(list0.locator('.combo-option', { hasText: 'Race Savings' })).toBeVisible();
  });

  // The expense grid shares the same combobox/descfield code. Assert the fund overlay reads
  // Unrestricted on load there too (value-0 fund default) and the grid page is WIDE
  // (Shell.Wide, the quick win).
  test('expense grid: fund reads Unrestricted and the page is wide', async ({ page, server }) => {
    await login(page, server);
    await page.goto('/expenses');
    await page.getByRole('button', { name: /new expense report/i }).click();
    await page.waitForURL('**/expenses/*');
    await expect(page.locator('form#expense-grid-form')).toBeVisible();

    // Quick win: the detail page uses the wide shell (like the txn editor / register).
    await expect(page.locator('main.app-main-wide')).toHaveCount(1);

    const fundOverlay = page.locator('.el-row[data-row="0"] .el-fund-cell .combo-text');
    await expect(fundOverlay).toBeVisible();
    await expect(page.locator('#el-fund-0')).toHaveValue('0');
    await expect(fundOverlay).toHaveValue(/unrestricted/i);
  });
});
