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
const { saveAndReload } = require('../helpers');

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
  await page.goto('/accounts');
  await page.getByRole('button', { name: /new account/i }).click();
  await expect(page.locator('#af-name-en')).toBeVisible();
  await page.locator('#af-name-en').fill(name);
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) {
    await rootSub.check();
  }
  await saveAndReload(page, { reloadPath: '/accounts' });
  await expect(page.locator('tr.acct-row', { hasText: name })).toBeVisible();
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
    await page.goto('/accounts');
    await page.getByRole('button', { name: /new account/i }).click();
    // The New-account form arrives via an htmx swap; wait for it to SETTLE (not just
    // appear) so htmx has wired #af-type's change→hx-get before we drive it — else
    // the type change fires into an unwired select and the re-fetch never happens.
    await expect(page.locator('form#account-form.e2e-settled')).toBeVisible();
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
    await saveAndReload(page, { reloadPath: '/accounts' });
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
    await page.goto('/accounts');
    await page.getByRole('button', { name: /new account/i }).click();
    await expect(page.locator('form#account-form.e2e-settled')).toBeVisible();
    await page.locator('#af-type').selectOption('expense');
    await expect(page.locator('#af-func')).toBeVisible();
    await page.locator('#af-name-en').fill('Combo Rent');
    const rootSub = page.locator('input[name="sub_1"]');
    if (!(await rootSub.isChecked())) await rootSub.check();
    await page.locator('#af-func').selectOption('management');
    await saveAndReload(page, { reloadPath: '/accounts' });
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
  });
});
