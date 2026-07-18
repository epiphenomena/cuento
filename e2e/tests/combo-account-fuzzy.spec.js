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

// createChildAsset makes a leaf asset under an existing asset parent, so the child's
// hierarchy PATH is "Parent.Child" -- the label the p28.2 pickers fuzzy-rank on. Type
// stays "asset" (default) so no htmx form re-swap races the parent select.
async function createChildAsset(page, name, parentPath) {
  await openNewAccount(page);
  await page.locator('#af-name-en').fill(name);
  await expect(async () => {
    // The #af-parent option label is the parent's dotted PATH (p28.2); pick it by path.
    await page.locator('#af-parent').selectOption({ label: parentPath });
    await expect(page.locator('#af-parent')).not.toHaveValue('0');
  }).toPass({ timeout: 5000 });
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) await rootSub.check();
  await saveAccount(page);
  await expect(page.locator('tr.acct-row', { hasText: name })).toBeVisible();
}

// p28.2: the account pickers OUTSIDE the entry grids -- the merge source/destination
// (#mg-src/#mg-dst) and the account-ledger report filter (#rp-account) -- must be the
// SAME fuzzy + hierarchy combobox. Each option's label carries the dotted ancestor PATH
// (data-path), so a query like "hier.leaf" (a segment of "Hier Parent.Hier Leaf") ranks
// the child. This proves both the shell-wide combos.js enhancement (reachability) and the
// hierarchy path on the label.
test.describe('non-grid account pickers are fuzzy + hierarchy comboboxes (p28.2)', () => {
  test('merge source and account-ledger filter fuzzy-rank the account path', async ({ page, server }) => {
    await login(page, server);
    // A parent + a child leaf, so the leaf's path is "Hier Parent.Hier Leaf".
    await createAsset(page, 'Hier Parent');
    await createChildAsset(page, 'Hier Leaf', 'Hier Parent');
    // A sibling leaf so a query must actually RANK, not just be the only option.
    await createAsset(page, 'Other Leaf');

    // MERGE picker: open the merge form, fuzzy-query the child by a path subsequence.
    await page.goto('/accounts');
    await page.getByRole('button', { name: /merge accounts/i }).click();
    await expect(page.locator('#mg-src')).toBeVisible();
    // The overlay input sits over the native select inside a .combo wrapper.
    const srcCell = page.locator('#mg-src').locator('xpath=ancestor::div[contains(@class,"combo")][1]');
    const srcInput = srcCell.locator('.combo-text');
    const srcList = srcCell.locator('.combo-list');
    await srcInput.click();
    await srcInput.fill('');
    await srcInput.type('hipar.leaf'); // subsequence of "Hier Parent.Hier Leaf"
    const wanted = srcList.locator('.combo-option', { hasText: 'Hier Parent.Hier Leaf' });
    await expect(wanted).toBeVisible();
    await expect(srcList).toBeVisible();
    const leafVal = await page.locator('#mg-src option', { hasText: 'Hier Parent.Hier Leaf' }).first().getAttribute('value');
    await wanted.first().click();
    await expect(page.locator('#mg-src')).toHaveValue(/** @type {string} */ (leafVal));

    // ACCOUNT-LEDGER report filter: same combobox, same path label.
    await page.goto('/reports/account_ledger');
    await expect(page.locator('#rp-account')).toBeVisible();
    const rpCell = page.locator('#rp-account').locator('xpath=ancestor::div[contains(@class,"combo")][1]');
    const rpInput = rpCell.locator('.combo-text');
    const rpList = rpCell.locator('.combo-list');
    await rpInput.click();
    await rpInput.fill('');
    await rpInput.type('hipar.leaf');
    await expect(rpList.locator('.combo-option', { hasText: 'Hier Parent.Hier Leaf' })).toBeVisible();
  });
});

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

  // p26.74: the account selector groups its options by TYPE under <optgroup> labels
  // (Assets / Liabilities / …) in canonical statement order. optgroups flatten in
  // HTMLSelectElement.options, so the fuzzy overlay above still ranks every option
  // (the other tests in this file prove that); here we assert the native select's
  // grouping directly. Both the header (#txn-main-account) and a body row
  // (#txn-account-0) carry the groups; the value="0" placeholder stays outside them.
  test('the account select groups options by type under optgroups', async ({ page, server }) => {
    await login(page, server);
    // Fresh e2e db: create one asset + one expense so two type groups appear. Expense
    // needs a functional class (form default handles it); the create form's expense
    // path is covered elsewhere -- here we only need it to exist as an option.
    await createAsset(page, 'Grp Checking');
    await page.goto('/accounts/new');
    await page.locator('#af-name-en').fill('Grp Rent');
    await page.locator('#af-type').selectOption('expense');
    await expect(page.locator('#af-type')).toHaveValue('expense');
    await page.locator('#af-name-en').fill('Grp Rent');
    const rentSub = page.locator('input[name="sub_1"]');
    if (!(await rentSub.isChecked())) await rentSub.check();
    await page.getByRole('button', { name: /^save$/i }).click();
    await expect(page.locator('tr.acct-row', { hasText: 'Grp Rent' })).toBeVisible();

    await page.goto('/transactions/new');
    await expect(page.locator('#txn-account-0')).toBeVisible();

    for (const sel of ['#txn-main-account', '#txn-account-0']) {
      const select = page.locator(sel);
      // The Assets optgroup exists and holds the created asset.
      const assets = select.locator('optgroup[label="Assets"]');
      await expect(assets).toHaveCount(1);
      await expect(assets.locator('option', { hasText: 'Grp Checking' })).toHaveCount(1);
      // The created expense account surfaces under the Expenses group.
      const expenses = select.locator('optgroup[label="Expenses"]');
      await expect(expenses).toHaveCount(1);
      await expect(expenses.locator('option', { hasText: 'Grp Rent' })).toHaveCount(1);
      // The "Choose account" placeholder (value 0) is NOT inside any optgroup.
      await expect(select.locator('optgroup > option[value="0"]')).toHaveCount(0);
      await expect(select.locator('option[value="0"]')).toHaveCount(1);
    }
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
