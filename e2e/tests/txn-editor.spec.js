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
const { openNewAccount, saveAccount, selectTxnAccount } = require('../helpers');

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
    await page.locator('main a.btn-primary', { hasText: /new transaction/i }).click();
    await page.waitForURL(/\/transactions\/new/);

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

    // p26.34: the MAIN (balancing) account is entered in the HEADER; its amount is the
    // auto-balanced residual of the body splits (the user never types it). Header account
    // = Editor Checking; ONE body row = Editor Savings 25.00 -> the header amount shows
    // -25.00 automatically. (Header presence is asserted separately below.)
    await expect(page.locator('#txn-main-account')).toBeVisible();
    await selectTxnAccount(page.locator('#txn-main-account'), 'Editor Checking');
    await selectTxnAccount(page.locator('#txn-account-0'), 'Editor Savings');
    await page.locator('#txn-amount-0').fill('25.00');
    // test (c): the header balancing amount auto-fills WITHOUT the user typing it.
    await expect(page.locator('#txn-main-amount')).toHaveValue('-25.00');

    // Save (a plain submit; success redirects to the first split's register -- the main).
    await page.getByRole('button', { name: /^save$/i }).click();

    // We land on a register; the transfer is posted. The 25.00 side appears in a register.
    await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));
    await expect(page.locator('table.register-table')).toBeVisible();
    await expect(page.locator('table.register-table')).toContainText('25.00');
  });

  // p26.34: the MAIN (header) description fuels descfield autocomplete for all splits -- it
  // must SUGGEST prior descriptions like a body row's desc field. (Prefill on the header is
  // intentionally a no-op: the header has no fund/program/amount cells to fill.)
  test('the header description field autocompletes prior descriptions', async ({ page, server }) => {
    await login(page, server);
    await createAsset(page, 'HdrDesc Checking');
    await createAsset(page, 'HdrDesc Savings');

    // Seed a prior split carrying a description (header = Checking, body row 0 = Savings 40
    // with the description to recall).
    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();
    await selectTxnAccount(page.locator('#txn-main-account'), 'HdrDesc Checking');
    await selectTxnAccount(page.locator('#txn-account-0'), 'HdrDesc Savings');
    await page.locator('#txn-amount-0').fill('40.00');
    await page.locator('#txn-desc-0').fill('Header recall demo');
    await page.getByRole('button', { name: /^save$/i }).click();
    await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

    // New entry: type a prefix into the HEADER description -> a suggestion appears.
    await page.goto('/transactions/new');
    await expect(page.locator('#txn-main-desc')).toBeVisible();

    // p26.36: the header reads date -> description -> memo. Assert the description field sits
    // AFTER the date and BEFORE the memo in document order (DOCUMENT_POSITION_FOLLOWING = 4).
    const order = await page.evaluate(() => {
      const date = document.querySelector('#txn-date');
      const desc = document.querySelector('#txn-main-desc');
      const memo = document.querySelector('#txn-memo');
      return {
        dateBeforeDesc: !!(date.compareDocumentPosition(desc) & Node.DOCUMENT_POSITION_FOLLOWING),
        descBeforeMemo: !!(desc.compareDocumentPosition(memo) & Node.DOCUMENT_POSITION_FOLLOWING),
      };
    });
    expect(order.dateBeforeDesc).toBe(true);
    expect(order.descBeforeMemo).toBe(true);

    const hdrDesc = page.locator('#txn-main-desc');
    await hdrDesc.click();
    await hdrDesc.fill('Header recall');
    await expect(
      page.locator('#txn-main-desc-list .desc-suggestion', { hasText: 'Header recall demo' }),
    ).toBeVisible();
  });

  // p26.41: the program select AND the functional-class select MERGED into ONE combined
  // control (#txn-progclass-<i>) shown only on R/E rows. Its values are ENCODED: c:<class>
  // (Admin/Fundraising) or p:<programID>. An expense account with a default class starts on
  // that class (c:management here); an asset row hides the whole control.
  test('shows the combined program/class control only on R/E rows, prefilled from the account default', async ({ page, server }) => {
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

    // Row 0: pick the ASSET account -> the combined control stays hidden.
    await selectTxnAccount(page.locator('#txn-account-0'), 'Editor Bank');
    await expect(page.locator('#txn-progclass-0')).toBeHidden();

    // Row 1: pick the EXPENSE account -> the combined control becomes visible and is
    // prefilled from the account's default class (c:management, "Admin").
    await selectTxnAccount(page.locator('#txn-account-1'), 'Editor Rent');
    await expect(page.locator('#txn-progclass-1')).toBeVisible();
    await expect(page.locator('#txn-progclass-1')).toHaveValue('c:management');
  });

  // p26.41 (SUPERSEDES p26.39): an expense split with NO account default class defaults to a
  // PROGRAM pick (p:<programID>) in the combined control, never blank. The two "class" entries
  // (Admin / Fundraising) are offered above the program tree on an expense row; a picking Admin
  // sets the class in ONE choice (no separate class select).
  test('an expense split defaults to a program pick, and offers Admin/Fundraising', async ({
    page,
    server,
  }) => {
    await login(page, server);
    await createAsset(page, 'Prog Bank');
    // An expense account WITHOUT a default functional class (skip the #af-func selection so
    // it stores none).
    await openNewAccount(page);
    await page.locator('#af-type').selectOption('expense');
    await expect(page.locator('#af-func')).toBeVisible();
    await page.locator('#af-name-en').fill('Prog Supplies');
    const rootSub = page.locator('input[name="sub_1"]');
    if (!(await rootSub.isChecked())) await rootSub.check();
    // Leave #af-func at its default (none) so the account carries no default class.
    await saveAccount(page);
    await expect(page.locator('tr.acct-row', { hasText: 'Prog Supplies' })).toBeVisible();

    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();
    await selectTxnAccount(page.locator('#txn-account-0'), 'Prog Supplies');
    const pc0 = page.locator('#txn-progclass-0');
    await expect(pc0).toBeVisible();
    // Defaults to a PROGRAM pick (p:<id>), never blank, even without an account default class.
    await expect(pc0).toHaveValue(/^p:\d+$/);

    // The two class alternates (Admin, Fundraising) are the first two options (data-class="1")
    // and are VISIBLE (not hidden) on an expense row; the program tree follows as p:<id>.
    const adminOpt = pc0.locator('option[value="c:management"]');
    const fundOpt = pc0.locator('option[value="c:fundraising"]');
    expect((await adminOpt.textContent()).trim()).toBe('Admin');
    expect((await fundOpt.textContent()).trim()).toBe('Fundraising');
    await expect(adminOpt).not.toHaveAttribute('hidden', /.*/);
    // Picking Admin sets the class in ONE choice (the combined control), no second select.
    await pc0.selectOption('c:management');
    await expect(pc0).toHaveValue('c:management');

    // A REVENUE row offers the program tree ONLY (no c:<class> entries) -- rule 7: revenue
    // splits carry a program but no functional class.
    // (Reuse the seeded grant-revenue kind is not available here; assert the class options are
    // HIDDEN once the row's account is not expense by re-picking the asset on this row instead.)
    await selectTxnAccount(page.locator('#txn-account-0'), 'Prog Bank');
    await expect(pc0).toBeHidden();
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
    await expect(page.locator('#txn-progclass-0')).toBeVisible();
    await expect(page.locator('#txn-progclass-0')).toHaveValue('c:management');

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
    // Asset pick -> combined program/class cell hidden (gating fired on the CLONED row's select).
    await expect(page.locator('#txn-progclass-1')).toBeHidden();

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

    // Pick the expense account via its combo so the combined program/class cell reveals
    // (R/E gating). #txn-progclass-0 is the combined control; #txn-program-0 is now the
    // HIDDEN program carrier (round-trip only).
    const acctCell = page.locator('.txn-row[data-row="0"] .txn-account-cell');
    await acctCell.locator('.combo-text').click();
    await acctCell.locator('.combo-text').fill('supplies');
    await acctCell.locator('.combo-option', { hasText: 'Combo Supplies' }).click();
    await expect(page.locator('#txn-progclass-0')).toBeVisible();

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

    // Combined program/class combo: filter to "General" (the seeded root program) and pick
    // it -> the native combined select gets the encoded p:<id> value AND the overlay shows
    // the program label. Picking a program node also seeds the hidden #txn-program-0 carrier.
    const pcCell = page.locator('.txn-row[data-row="0"] .txn-progclass-cell');
    const pcInput = pcCell.locator('.combo-text');
    await pcInput.click();
    await pcInput.fill('gene');
    // p29.13: program combo labels now carry the dotted PATH, so child programs read
    // "General.x" -- match the ROOT exactly (^General$) to avoid a substring collision
    // with any child a prior spec seeded into this worker's shared db.
    await expect(pcCell.locator('.combo-option', { hasText: /^General$/ })).toBeVisible();
    await pcCell.locator('.combo-option', { hasText: /^General$/ }).click();
    await expect(page.locator('#txn-progclass-0')).toHaveValue(/^p:\d+$/);
    await expect(page.locator('#txn-program-0')).toHaveValue(/\d+/);
    await expect(pcInput).toHaveValue(/General/);
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
    await page.locator('main a.btn-primary', { hasText: /new transaction/i }).click();
    await page.waitForURL(/\/transactions\/new/);
    // p26.34: main (Live Savings) in the header; Gone Checking -40 in the body row. The
    // header auto-balances to +40. Gone Checking (the to-be-deactivated account) is the
    // BODY split whose row must later render it as a selected unavailable option.
    await selectTxnAccount(page.locator('#txn-main-account'), 'Live Savings');
    await selectTxnAccount(page.locator('#txn-account-0'), 'Gone Checking');
    await page.locator('#txn-amount-0').fill('-40.00');
    await page.getByRole('button', { name: /^save$/i }).click();
    await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

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
    await page.waitForURL((u) => /\/transactions\/\d+\/edit/.test(u.pathname));
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

    // Column-order guard: p28 moved the error + delete to the END of ROW 1 (after amount).
    // The delete cell is the LAST td of the split's FIRST row, right after the error cell.
    const row0MainCells = page.locator('.txn-row[data-row="0"] .txn-row-main > td');
    await expect(row0MainCells.last()).toHaveClass(/txn-delete-cell/);
    await expect(
      page.locator('.txn-row[data-row="0"] .txn-row-error + .txn-delete-cell'),
    ).toHaveCount(1);
    // Row 2 now ends with the memo cell (error/delete gone); the memo spans to the edge.
    await expect(
      page.locator('.txn-row[data-row="0"] .txn-row-more > td').last(),
    ).toHaveClass(/txn-memo-cell/);

    // Build three data rows with distinguishable memos (auto-append grows the trailing row).
    await selectTxnAccount(page.locator('#txn-account-0'), 'Del Savings');
    await page.locator('#txn-memo-0').fill('row-zero-memo');
    await expect(page.locator('#txn-account-1')).toBeVisible();
    await selectTxnAccount(page.locator('#txn-account-1'), 'Del Checking');
    await page.locator('#txn-memo-1').fill('row-one-memo');
    await expect(page.locator('#txn-account-2')).toBeVisible();
    await selectTxnAccount(page.locator('#txn-account-2'), 'Del Savings');
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

  // p26.33: the top-nav "New transaction" action is the canonical entry point, and Cancel
  // returns to the account register the user came from (threaded via ?from=).
  test('the New-transaction nav button opens the editor, and Cancel returns to the origin register', async ({ page, server }) => {
    await login(page, server);
    await createAsset(page, 'Origin Checking');

    // p26.48: "New transaction" is a DISTINCT right-aligned button in the header (pulled
    // out of the nav-link list), perm-gated (admin has TxnWrite). It is not inside
    // .app-nav; it is .app-newtxn in the header.
    const navNew = page.locator('.app-header a.app-newtxn', { hasText: /new transaction/i });
    await expect(navNew).toHaveCount(1);
    // It must NOT appear as an inline nav link.
    await expect(page.locator('.app-nav a', { hasText: /new transaction/i })).toHaveCount(0);
    await navNew.click();
    await expect(page.locator('form#txn-form')).toBeVisible();

    // p26.35: the BOOSTED nav click must render the FULL shell (nav + editor JS), not the
    // bare partial. Assert the nav shell is still present AND the editor JS wired up -- the
    // account combobox overlay exists and accepts typing (proves htmx:afterSwap re-init ran,
    // identical to entering from a register).
    await expect(page.locator('.app-header a.app-newtxn', { hasText: /new transaction/i })).toHaveCount(1);
    const navCombo = page.locator('.txn-row[data-row="0"] .txn-account-cell .combo-text');
    await expect(navCombo).toBeVisible();
    await navCombo.click();
    await navCombo.fill('origin');
    await expect(
      page.locator('.txn-row[data-row="0"] .txn-account-cell .combo-list'),
    ).toBeVisible();
    await expect(
      page.locator('.txn-row[data-row="0"] .txn-account-cell .combo-option', {
        hasText: 'Origin Checking',
      }),
    ).toBeVisible();

    // From the bare nav entry (no origin) Cancel falls back to the chart of accounts.
    await expect(page.locator('.txn-submit a.btn-ghost')).toHaveAttribute('href', '/accounts');

    // Open the register for the account and use ITS new-transaction link: Cancel now
    // returns to that register (origin threaded through the `from` param, echoed in the
    // hidden #txn-origin field). Navigate back to the chart of accounts first (we are on
    // the editor after the nav-button check above).
    await page.goto('/accounts');
    const row = page.locator('tr.acct-row', { hasText: 'Origin Checking' });
    await row.getByRole('link', { name: 'Origin Checking' }).click();
    await page.waitForURL('**/register');
    const registerURL = new URL(page.url()).pathname;
    // The register's own in-page new-transaction button (scoped to <main> so it is not
    // the identically-labeled nav link).
    await page.locator('main a.btn-primary', { hasText: /new transaction/i }).click();
    await expect(page.locator('form#txn-form')).toBeVisible();
    await expect(page.locator('#txn-origin')).toHaveValue(registerURL);
    await expect(page.locator('.txn-submit a.btn-ghost')).toHaveAttribute('href', registerURL);
    // Cancel navigates back to that register.
    await page.locator('.txn-submit a.btn-ghost').click();
    await expect(page).toHaveURL(new RegExp(`${registerURL}$`));
  });

  // p26.109: the live per-fund imbalance chips must render the fund NAME (proper noun,
  // read from the fund <select> options) and the localized "Total" / "Unrestricted"
  // labels -- NOT a hardcoded English literal or a raw fund database id. This guards
  // the rule-9 leak the audit found (chips showed 'total'/'unrestricted' + the id).
  test('the per-fund imbalance chips show fund NAMES and localized labels, not ids', async ({
    page,
    server,
  }) => {
    await login(page, server);
    // Two named funds so >=2 fund groups are in play (per-fund chips appear only then).
    await createFund(page, 'Alpha Grant');
    await createFund(page, 'Beta Grant');
    await createAsset(page, 'Chip Checking');
    await createAsset(page, 'Chip Savings');

    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();

    // Row 0: fund = Alpha Grant, amount +100. Row 1: fund = Beta Grant, amount -60.
    // Overall = +40 (Total chip). Each fund group is nonzero -> a per-fund chip each.
    await selectTxnAccount(page.locator('#txn-account-0'), 'Chip Savings');
    await page.locator('#txn-fund-0').selectOption({ label: 'Alpha Grant' });
    await page.locator('#txn-amount-0').fill('100.00');
    await expect(page.locator('#txn-account-1')).toBeVisible();
    await selectTxnAccount(page.locator('#txn-account-1'), 'Chip Checking');
    await page.locator('#txn-fund-1').selectOption({ label: 'Beta Grant' });
    await page.locator('#txn-amount-1').fill('-60.00');

    // p29.5: the redundant always-zero overall Total chip was removed; only the per-fund
    // imbalance chips remain (this test's actual subject).
    await expect(page.locator('#txn-total-overall')).toHaveCount(0);

    // The per-fund chips render the fund NAMES, never the raw fund id.
    const chips = page.locator('#txn-fund-chips .txn-fund-chip');
    await expect(chips).toHaveCount(2);
    const chipText = (await chips.allTextContents()).join(' | ');
    expect(chipText).toContain('Alpha Grant:');
    expect(chipText).toContain('Beta Grant:');
    // No chip is labelled with a bare numeric id (e.g. "5: ...").
    for (const t of await chips.allTextContents()) {
      expect(t).not.toMatch(/^\d+:/);
    }
  });

  // p28.4/p29.5: the header/main split auto-balances the BODY -- its amount previews the
  // residual -(body sum) -- so a balanced entry always nets to zero overall. p29.5 REMOVED
  // the always-zero overall Total chip (redundant); this test now guards only the surviving
  // behavior: the main-split residual preview keeps up with the body.
  test('the main-split amount previews the balancing residual of the body', async ({
    page,
    server,
  }) => {
    await login(page, server);
    await createAsset(page, 'Bal Checking');
    await createAsset(page, 'Bal Savings');

    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();

    // Header (balancing) account + a single body leg: Savings +40. The header takes the
    // residual -40, so the OVERALL transaction is balanced (0) even though the body sum is 40.
    await selectTxnAccount(page.locator('#txn-main-account'), 'Bal Checking');
    await selectTxnAccount(page.locator('#txn-account-0'), 'Bal Savings');
    await page.locator('#txn-amount-0').fill('40.00');

    // The header main split's amount previews the -40 residual (proves the body IS balanced
    // by the main split).
    await expect(page.locator('#txn-main-amount')).toHaveValue(/40/);
    // The redundant overall Total chip is gone (only the per-fund chips remain).
    await expect(page.locator('#txn-total-overall')).toHaveCount(0);
  });

  // p26.37: opening a NEW transaction prefills the header (balancing) account -- from a
  // register it is THAT register's account; from the top nav it is the user's LAST-USED
  // header account (the position-0 account of their most recent transaction).
  test('the header account is prefilled from the register origin, then from the last-used account via the nav', async ({
    page,
    server,
  }) => {
    await login(page, server);
    await createAsset(page, 'Prefill Checking');
    await createAsset(page, 'Prefill Savings');

    // Enter from Prefill Checking's register -> the header account is prefilled to it.
    await page.goto('/accounts');
    await page
      .locator('tr.acct-row', { hasText: 'Prefill Checking' })
      .getByRole('link', { name: 'Prefill Checking' })
      .click();
    await page.waitForURL('**/register');
    await page.locator('main a.btn-primary', { hasText: /new transaction/i }).click();
    await expect(page.locator('form#txn-form')).toBeVisible();
    const checkingVal = await page
      .locator('#txn-main-account option', { hasText: 'Prefill Checking' })
      .getAttribute('value');
    await expect(page.locator('#txn-main-account')).toHaveValue(checkingVal);

    // Post a transaction whose HEADER account is Prefill Savings, so the last-used header
    // account becomes Savings.
    await page.goto('/transactions/new');
    await selectTxnAccount(page.locator('#txn-main-account'), 'Prefill Savings');
    await selectTxnAccount(page.locator('#txn-account-0'), 'Prefill Checking');
    await page.locator('#txn-amount-0').fill('50.00');
    await page.getByRole('button', { name: /^save$/i }).click();
    await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

    // Open a NEW transaction from the top nav (no register origin) -> the header account is
    // prefilled to the last-used account (Savings), not blank.
    await page.locator('.app-header a.app-newtxn', { hasText: /new transaction/i }).click();
    await expect(page.locator('form#txn-form')).toBeVisible();
    const savingsVal = await page
      .locator('#txn-main-account option', { hasText: 'Prefill Savings' })
      .getAttribute('value');
    await expect(page.locator('#txn-main-account')).toHaveValue(savingsVal);
    // The header combobox overlay shows the prefilled account label (JS synced on init).
    await expect(page.locator('#txn-main-account-wrap .combo-text, .txn-main-account-field .combo-text')).toHaveValue(
      /Prefill Savings/,
    );
  });
});
