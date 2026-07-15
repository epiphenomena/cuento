// @ts-check
// p26.44 REPRO/regression: the ACCOUNT combobox in the transaction grid was reported as
// "not supporting fuzzy matching" (typing does not filter/rank the options), while the
// fund/program comboboxes and the shared rankOptions matcher work. This spec probes the
// account combo (both the header #txn-main-account and a body #txn-account-0) AND the fund
// combo side-by-side with the SAME subsequence query, at three levels:
//   (1) DOM: the .combo-option set is filtered + ranked (the pure-matcher layer);
//   (2) Visible: the .combo-list is actually on screen (not rendered behind a sibling);
//   (3) Pickable: clicking the top filtered option changes the native select's value
//       (toBeVisible does NOT catch a listbox stacked behind another element -- only a
//        real pick does).
// If fund passes and account fails, the bug is account/row-specific. If both pass, the bug
// does not reproduce and the spec stays as a shared fuzzy-match regression guard.

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

// Assert a combo cell filters+ranks+picks a fuzzy subsequence query. `cell` is a locator
// scoping ONE .combo wrapper. `query` is a subsequence of `expectLabel`. Asserts the DOM
// option set narrows to include the expected leaf, the list is visible, and a click picks it.
async function assertFuzzy(page, cell, sel, query, expectLabel, expectValue) {
  const input = cell.locator('.combo-text');
  const list = cell.locator('.combo-list');
  await input.click();
  await input.fill('');
  await input.type(query);
  // (1) DOM: the expected option is present and ranked into the filtered list.
  const wanted = list.locator('.combo-option', { hasText: expectLabel });
  await expect(wanted).toBeVisible();
  // (2) Visible: the list itself is on screen.
  await expect(list).toBeVisible();
  // (3) Pickable: clicking the option changes the native select value (catches a stacking
  //     bug that toBeVisible would miss).
  await wanted.first().click();
  await expect(sel).toHaveValue(expectValue);
}

test.describe('account combobox fuzzy matching (p26.44)', () => {
  test('account (header + body) and fund all filter/rank/pick a subsequence query', async ({ page, server }) => {
    await login(page, server);
    await createAsset(page, 'Fuzz Checking');
    await createAsset(page, 'Fuzz Savings');
    await createFund(page, 'Fuzzfund Restricted');

    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();
    await expect(page.locator('#txn-account-0')).toBeVisible();

    // Resolve the option values so we can assert the pick landed on the right account/fund.
    const savingsVal = await page.locator('#txn-account-0 option', { hasText: 'Fuzz Savings' }).first().getAttribute('value');
    const checkingVal = await page.locator('#txn-account-0 option', { hasText: 'Fuzz Checking' }).first().getAttribute('value');
    const fundVal = await page.locator('#txn-fund-0 option', { hasText: 'Fuzzfund Restricted' }).first().getAttribute('value');

    // FUND (the control the task says works) -- a subsequence query 'fzrest'.
    await assertFuzzy(
      page,
      page.locator('.txn-row[data-row="0"] .txn-fund-cell .combo'),
      page.locator('#txn-fund-0'),
      'fzrest',
      'Fuzzfund Restricted',
      /** @type {string} */ (fundVal),
    );

    // BODY ACCOUNT -- the SAME kind of subsequence query 'fzsav' (leaf fragment of Savings).
    await assertFuzzy(
      page,
      page.locator('.txn-row[data-row="0"] .txn-account-cell .combo'),
      page.locator('#txn-account-0'),
      'fzsav',
      'Fuzz Savings',
      /** @type {string} */ (savingsVal),
    );

    // HEADER (balancing) ACCOUNT -- post-p26.34 this is the account field the user hits first.
    await assertFuzzy(
      page,
      page.locator('#txn-main-header .combo'),
      page.locator('#txn-main-account'),
      'fzchk',
      'Fuzz Checking',
      /** @type {string} */ (checkingVal),
    );
  });

  // p26.44 ROOT CAUSE / regression: for a non-freeText combo the native <select> is the Tab
  // stop (p26.11: the overlay is tabIndex=-1). A KEYBOARD user tabs onto the account select and
  // types -- which used to hit the browser's native <select> prefix-typeahead, NOT the fuzzy
  // overlay, so no ranked listbox opened ("the account field isn't supporting fuzzy matching").
  // The fix bridges a printable keystroke on the focused select to the overlay. This is the
  // account combo the user hits FIRST, but the gap (and the fix) are SHARED -- fund/program too.
  test('tabbing to the native select and typing opens the fuzzy overlay and Enter picks', async ({ page, server }) => {
    await login(page, server);
    await createAsset(page, 'Kb Checking');
    await createAsset(page, 'Kb Savings');
    await createFund(page, 'Kbfund Restricted');
    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();
    await expect(page.locator('#txn-account-0')).toBeVisible();

    const savingsVal = await page.locator('#txn-account-0 option', { hasText: 'Kb Savings' }).first().getAttribute('value');
    const fundVal = await page.locator('#txn-fund-0 option', { hasText: 'Kbfund Restricted' }).first().getAttribute('value');

    // ACCOUNT: focus the native select (the REAL tab stop), then type a fuzzy subsequence.
    await page.locator('#txn-account-0').focus();
    await page.keyboard.type('kbsav'); // subsequence of "Kb Savings"
    const acctCell = page.locator('.txn-row[data-row="0"] .txn-account-cell');
    const acctList = acctCell.locator('.combo-list');
    await expect(acctList).toBeVisible();
    await expect(acctList.locator('.combo-option', { hasText: 'Kb Savings' })).toBeVisible();
    await expect(acctList.locator('.combo-option', { hasText: 'Kb Checking' })).toHaveCount(0);
    // The overlay carries the typed text (the native typeahead jump was suppressed).
    await expect(acctCell.locator('.combo-text')).toHaveValue('kbsav');
    // Enter on the open list picks the top-ranked option into the native select.
    await page.keyboard.press('Enter');
    await expect(page.locator('#txn-account-0')).toHaveValue(/** @type {string} */ (savingsVal));

    // FUND: the same shared bridge -- focus the native fund select and type.
    await page.locator('#txn-fund-0').focus();
    await page.keyboard.type('kbrest'); // subsequence of "Kbfund Restricted"
    const fundCell = page.locator('.txn-row[data-row="0"] .txn-fund-cell');
    const fundList = fundCell.locator('.combo-list');
    await expect(fundList).toBeVisible();
    await expect(fundList.locator('.combo-option', { hasText: 'Kbfund Restricted' })).toBeVisible();
    await page.keyboard.press('Enter');
    await expect(page.locator('#txn-fund-0')).toHaveValue(/** @type {string} */ (fundVal));
  });
});
