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
const { openNewAccount, saveAccount, selectTxnAccount } = require('../helpers');

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
  // "New fund" is a subnav link to its own /funds/new page (bc2dd5b subnav refactor), a
  // full-page navigation -- wait for the form itself, not an htmx settle marker.
  await page.getByRole('link', { name: /new fund/i }).click();
  await page.waitForURL('**/funds/new');
  await expect(page.locator('form#fund-form')).toBeVisible();
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
    await srcInput.type('hier.leaf'); // adjacency fragments of "Hier Parent.Hier Leaf"
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
    await rpInput.type('hier.leaf');
    await expect(rpList.locator('.combo-option', { hasText: 'Hier Parent.Hier Leaf' })).toBeVisible();
  });
});

// p28.3: when the combo/description suggestion list is OPEN with a HIGHLIGHTED item,
// BOTH Enter and Tab must (a) commit that item and (b) advance focus to the next field.
// This proves it on an ACCOUNT combo (Enter and Tab) and on the DESCRIPTION field (Enter).
test.describe('Enter and Tab select-and-advance (p28.3)', () => {
  test('account combo: Enter commits + advances to amount; Tab commits + advances too', async ({ page, server }) => {
    await login(page, server);
    await createAsset(page, 'Adv Checking');
    await createAsset(page, 'Adv Savings');

    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();
    await expect(page.locator('#txn-account-0')).toBeVisible();
    const savingsVal = await page.locator('#txn-account-0 option', { hasText: 'Adv Savings' }).first().getAttribute('value');

    // --- ENTER: focus the native account select (the tab stop), type a fuzzy query (the
    //     p26.44 bridge opens the overlay list highlighted), press Enter. It must commit the
    //     highlighted option AND move focus to the amount cell.
    await page.locator('#txn-account-0').focus();
    await page.keyboard.type('adv sav'); // adjacency fragments of "Adv Savings"
    const acctList = page.locator('.txn-row[data-row="0"] .txn-account-cell .combo-list');
    await expect(acctList).toBeVisible();
    await expect(acctList.locator('.combo-option', { hasText: 'Adv Savings' })).toBeVisible();
    await page.keyboard.press('Enter');
    // (a) committed:
    await expect(page.locator('#txn-account-0')).toHaveValue(/** @type {string} */ (savingsVal));
    // (b) advanced to the next field (amount):
    await expect(page.locator('#txn-amount-0')).toBeFocused();

    // --- TAB: do the same on the freshly auto-appended row 1, pressing Tab instead. Tab must
    //     commit the highlighted option (not leave the typed text uncommitted) AND advance
    //     focus off the account cell (native Tab -> amount).
    await expect(page.locator('#txn-account-1')).toBeVisible();
    await page.locator('#txn-account-1').focus();
    await page.keyboard.type('adv check'); // adjacency fragments of "Adv Checking"
    const acctList1 = page.locator('.txn-row[data-row="1"] .txn-account-cell .combo-list');
    await expect(acctList1).toBeVisible();
    await expect(acctList1.locator('.combo-option', { hasText: 'Adv Checking' })).toBeVisible();
    const checkingVal = await page.locator('#txn-account-1 option', { hasText: 'Adv Checking' }).first().getAttribute('value');
    await page.keyboard.press('Tab');
    // (a) Tab committed the highlight (did NOT revert to the old selection via the blur timer):
    await expect(page.locator('#txn-account-1')).toHaveValue(/** @type {string} */ (checkingVal));
    // (b) focus advanced off the account cell to the amount input:
    await expect(page.locator('#txn-amount-1')).toBeFocused();
  });

  // p28.5: the header main-account combo (#txn-main-account, no row index) advances an
  // Enter-pick to the first BODY cell (row 0 description) -- so the header account is no
  // longer a dead end for Enter (Tab already advanced it natively). Proves Enter-advance
  // covers the header combo, not just the per-row body combos.
  test('header main-account combo: Enter commits + advances to the first body cell', async ({ page, server }) => {
    await login(page, server);
    await createAsset(page, 'HdrAdv Checking');

    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();
    await page.locator('#txn-main-account').focus();
    await page.keyboard.type('hdr check'); // adjacency fragments of "HdrAdv Checking"
    const list = page.locator('#txn-main-header .combo-list');
    await expect(list).toBeVisible();
    await expect(list.locator('.combo-option', { hasText: 'HdrAdv Checking' })).toBeVisible();
    const val = await page.locator('#txn-main-account option', { hasText: 'HdrAdv Checking' }).first().getAttribute('value');
    await page.keyboard.press('Enter');
    // (a) committed:
    await expect(page.locator('#txn-main-account')).toHaveValue(/** @type {string} */ (val));
    // (b) advanced past the tabIndex=-1 main-amount to the first body cell (description):
    await expect(page.locator('#txn-desc-0')).toBeFocused();
  });

  // p28.5: a non-grid picker (the merge source combo) has NO onAdvance, so Enter uses the
  // GENERIC "focus the next tabbable" fallback -- landing on the merge DESTINATION combo's
  // tab stop (its native select). This exercises the fallback branch the expense/budget
  // grid combos also rely on (they, too, enhance without a grid onAdvance).
  test('non-grid combo: Enter commits + advances via the generic next-tabbable fallback', async ({ page, server }) => {
    await login(page, server);
    await createAsset(page, 'MrgAdv One');
    await createAsset(page, 'MrgAdv Two');

    await page.goto('/accounts');
    await page.getByRole('button', { name: /merge accounts/i }).click();
    await expect(page.locator('#mg-src')).toBeVisible();
    await page.locator('#mg-src').focus();
    await page.keyboard.type('mrg one'); // adjacency fragments of "MrgAdv One"
    const list = page.locator('#mg-src').locator('xpath=ancestor::div[contains(@class,"combo")][1]').locator('.combo-list');
    await expect(list).toBeVisible();
    await expect(list.locator('.combo-option', { hasText: 'MrgAdv One' })).toBeVisible();
    const val = await page.locator('#mg-src option', { hasText: 'MrgAdv One' }).first().getAttribute('value');
    await page.keyboard.press('Enter');
    // (a) committed the source pick:
    await expect(page.locator('#mg-src')).toHaveValue(/** @type {string} */ (val));
    // (b) advanced to the destination select (the next tab stop after the source combo):
    await expect(page.locator('#mg-dst')).toBeFocused();
  });

  test('description field: Enter commits the suggestion + advances to the account cell', async ({ page, server }) => {
    await login(page, server);
    await createAsset(page, 'DescAdv Checking');
    await createAsset(page, 'DescAdv Savings');

    // Seed a prior transaction whose body split carries a recallable description.
    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();
    await selectTxnAccount(page.locator('#txn-main-account'), 'DescAdv Checking');
    await selectTxnAccount(page.locator('#txn-account-0'), 'DescAdv Savings');
    await page.locator('#txn-amount-0').fill('40.00');
    await page.locator('#txn-desc-0').fill('DescAdvance recall');
    await page.getByRole('button', { name: /^save$/i }).click();
    await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

    // New entry: type a prefix on row 0's description, the suggestion opens highlighted,
    // press Enter -> commit the full description AND advance focus to the account cell.
    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();
    const desc0 = page.locator('#txn-desc-0');
    await desc0.click();
    await desc0.fill('DescAdvance');
    const suggestion = page.locator('#txn-desc-list-0 .desc-suggestion', { hasText: 'DescAdvance recall' });
    await expect(suggestion).toBeVisible();
    await page.keyboard.press('Enter');
    // (a) committed the full suggestion text into the description input:
    await expect(desc0).toHaveValue('DescAdvance recall');
    // (b) focus advanced to the row's account <select> (the cell after description):
    await expect(page.locator('#txn-account-0')).toBeFocused();
  });

  test('description field: Tab commits the suggestion + advances to the account cell', async ({ page, server }) => {
    await login(page, server);
    await createAsset(page, 'DescTab Checking');
    await createAsset(page, 'DescTab Savings');

    // Seed a prior transaction whose body split carries a recallable description.
    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();
    await selectTxnAccount(page.locator('#txn-main-account'), 'DescTab Checking');
    await selectTxnAccount(page.locator('#txn-account-0'), 'DescTab Savings');
    await page.locator('#txn-amount-0').fill('40.00');
    await page.locator('#txn-desc-0').fill('DescTab recall');
    await page.getByRole('button', { name: /^save$/i }).click();
    await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

    // New entry: type a prefix on row 0's description, the suggestion opens highlighted,
    // press Tab -> commit the full description (NOT leave the partial typed text) AND
    // advance focus to the account cell (native/grid Tab move after the commit).
    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();
    const desc0 = page.locator('#txn-desc-0');
    await desc0.click();
    await desc0.fill('DescTab');
    const suggestion = page.locator('#txn-desc-list-0 .desc-suggestion', { hasText: 'DescTab recall' });
    await expect(suggestion).toBeVisible();
    await page.keyboard.press('Tab');
    // (a) Tab committed the full suggestion text (not the partial "DescTab" typed):
    await expect(desc0).toHaveValue('DescTab recall');
    // (b) focus advanced to the row's account <select> (the cell after description):
    await expect(page.locator('#txn-account-0')).toBeFocused();
  });
});

// Select-text-on-focus + Description-label tooltip refinements. The tooltip lives on the
// txn form's Description label; select-on-focus is asserted on a NON-txn-form picker (the
// account-ledger report filter) where it is net-new (the txn grid already select()s inputs
// via txneditor.js's form-level focusin, so asserting there would pass pre-change).
test.describe('combobox select-on-focus + Description tooltip', () => {
  test('Description label carries an accessible help tooltip', async ({ page, server }) => {
    await login(page, server);
    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();
    const tip = page.locator('label[for="txn-main-desc"] .help-tip');
    await expect(tip).toHaveCount(1);
    // The explanatory text is on both the native title (hover) and aria-label (a11y).
    const title = await tip.getAttribute('title');
    const aria = await tip.getAttribute('aria-label');
    expect(title && title.length).toBeGreaterThan(0);
    expect(aria).toBe(title);
    // The nudge mentions autocomplete/recall (the reason for a short, consistent name).
    expect((title || '').toLowerCase()).toContain('autocomplete');
  });

  test('focusing a fuzzy combo selects its text so typing replaces it (report account filter)', async ({ page, server }) => {
    await login(page, server);
    await createAsset(page, 'SelFocus Checking');

    await page.goto('/reports/account_ledger');
    await expect(page.locator('#rp-account')).toBeVisible();
    const cell = page.locator('#rp-account').locator('xpath=ancestor::div[contains(@class,"combo")][1]');
    const input = cell.locator('.combo-text');
    // Pick an option so the overlay holds a non-empty label.
    await input.click();
    await input.fill('');
    await input.type('selfocus check');
    const list = cell.locator('.combo-list');
    await list.locator('.combo-option', { hasText: 'SelFocus Checking' }).first().click();
    // p12.12: the account picker label is now the type-rooted dotted path.
    await expect(input).toHaveValue('Asset.SelFocus Checking');

    // Blur then re-focus (click) so the focus handler runs with a populated box, then type a
    // SINGLE char via the keyboard. Select-on-focus must have highlighted the whole label so
    // the char REPLACES it (value == just the char), not appends (value == label + char).
    await page.locator('body').click({ position: { x: 5, y: 5 } });
    await input.click();
    await page.keyboard.type('x');
    await expect(input).toHaveValue('x');
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

    // FUND (the control the task says works) -- adjacency fragments 'fuzz rest'.
    await assertFuzzy(
      page,
      page.locator('.txn-row[data-row="0"] .txn-fund-cell .combo'),
      page.locator('#txn-fund-0'),
      'fuzz rest',
      'Fuzzfund Restricted',
      /** @type {string} */ (fundVal),
    );

    // BODY ACCOUNT -- the SAME kind of adjacency query 'fuzz sav' (leaf fragment of Savings).
    await assertFuzzy(
      page,
      page.locator('.txn-row[data-row="0"] .txn-account-cell .combo'),
      page.locator('#txn-account-0'),
      'fuzz sav',
      'Fuzz Savings',
      /** @type {string} */ (savingsVal),
    );

    // HEADER (balancing) ACCOUNT -- post-p26.34 this is the account field the user hits first.
    await assertFuzzy(
      page,
      page.locator('#txn-main-header .combo'),
      page.locator('#txn-main-account'),
      'fuzz check',
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
    await page.keyboard.type('kb sav'); // adjacency fragments of "Kb Savings"
    const acctCell = page.locator('.txn-row[data-row="0"] .txn-account-cell');
    const acctList = acctCell.locator('.combo-list');
    await expect(acctList).toBeVisible();
    await expect(acctList.locator('.combo-option', { hasText: 'Kb Savings' })).toBeVisible();
    await expect(acctList.locator('.combo-option', { hasText: 'Kb Checking' })).toHaveCount(0);
    // The overlay carries the typed text (the native typeahead jump was suppressed).
    await expect(acctCell.locator('.combo-text')).toHaveValue('kb sav');
    // Enter on the open list picks the top-ranked option into the native select.
    await page.keyboard.press('Enter');
    await expect(page.locator('#txn-account-0')).toHaveValue(/** @type {string} */ (savingsVal));

    // FUND: the same shared bridge -- focus the native fund select and type.
    await page.locator('#txn-fund-0').focus();
    await page.keyboard.type('kb rest'); // adjacency fragments of "Kbfund Restricted"
    const fundCell = page.locator('.txn-row[data-row="0"] .txn-fund-cell');
    const fundList = fundCell.locator('.combo-list');
    await expect(fundList).toBeVisible();
    await expect(fundList.locator('.combo-option', { hasText: 'Kbfund Restricted' })).toBeVisible();
    await page.keyboard.press('Enter');
    await expect(page.locator('#txn-fund-0')).toHaveValue(/** @type {string} */ (fundVal));
  });
});
