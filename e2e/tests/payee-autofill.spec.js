// @ts-check
// Functional test of the per-split DESCRIPTION autocomplete + per-row prefill (p26.19,
// the payee->description migration's entry-UI cutover). It drives the REAL
// /transactions/new grid served by `cuento serve -dev`. The old per-transaction payee
// autofill (a whole-grid template keyed off a picked payee) was REMOVED; each grid row's
// free-text description now autocompletes from prior splits and, on pick/commit, prefills
// THAT row from the matched split -- but ONLY when the row is otherwise empty
// (never-overwrites). Selectors come from transaction_form.tmpl.
//
// Precondition (a split WITH a description on a prior txn) is built THROUGH THE BROWSER:
// the first saved transaction types a per-split description; a later editor then
// autocompletes it, picks it, and the prior split's fields prefill that row.
//
// htmx timing (see txn-editor.spec): suggestions are filled by a manual debounced fetch
// on a STABLE (load-wired) input, and pick goes through a delegated mousedown + a manual
// prefill fetch (no hx-* trigger on the freshly-swapped <li>), so there is no settle-tick
// race. Strict CSP blocks page.waitForFunction; we use locator waits (auto-retry).

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
  if (!(await rootSub.isChecked())) {
    await rootSub.check();
  }
  await saveAccount(page);
  await expect(page.locator('tr.acct-row', { hasText: name })).toBeVisible();
}

// createExpense makes a leaf EXPENSE account (with a default functional class) in the root
// subsidiary -- the expense-line grid only offers R/E leaves, and a txn expense split needs
// a program + class (auto-defaulted from the account) to post.
async function createExpense(page, name) {
  await openNewAccount(page);
  const typeSwapped = page.waitForResponse(
    (r) => new URL(r.url()).pathname === '/accounts/new' && r.request().method() === 'GET',
  );
  await page.locator('#af-type').selectOption('expense');
  await typeSwapped;
  await expect(page.locator('#af-func')).toBeVisible();
  await page.locator('#af-name-en').fill(name);
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) await rootSub.check();
  await page.locator('#af-func').selectOption('program');
  await saveAccount(page);
  await expect(page.locator('tr.acct-row', { hasText: name })).toBeVisible();
}

test.describe('per-split description autocomplete + prefill', () => {
  test('the header payee field is GONE', async ({ page, server }) => {
    await login(page, server);
    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();
    // p26.19: the whole .txn-payee-field header block (native select + combo overlay)
    // was removed. Its inputs must not exist.
    await expect(page.locator('.txn-payee-field')).toHaveCount(0);
    await expect(page.locator('#txn-payee')).toHaveCount(0);
    await expect(page.locator('#txn-payee-name')).toHaveCount(0);
    // The per-row description input IS present on row 0.
    await expect(page.locator('#txn-desc-0')).toBeVisible();
  });

  test('a typed description autocompletes, picks, and prefills an empty row', async ({ page, server }) => {
    await login(page, server);
    await createAsset(page, 'Desc Checking');
    await createAsset(page, 'Desc Savings');

    // First transaction: a per-split description + a balanced transfer. Row 0 carries the
    // description "Autofill transfer" so a later entry can recall it.
    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();
    // p26.34: header = Checking (balancing); body row 0 = Savings 40 with desc + memo.
    await page.locator('#txn-main-account').selectOption({ label: 'Desc Checking' });
    await page.locator('#txn-account-0').selectOption({ label: 'Desc Savings' });
    await page.locator('#txn-amount-0').fill('40.00');
    await page.locator('#txn-desc-0').fill('Autofill transfer');
    await page.locator('#txn-memo-0').fill('first memo');
    await page.getByRole('button', { name: /^save$/i }).click();
    await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

    // Second entry: type a prefix of the description on row 0, the suggestion appears,
    // pick it, and the prior split's fields prefill THIS (empty) row.
    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();
    const desc0 = page.locator('#txn-desc-0');
    await desc0.click();
    await desc0.fill('Autofill');
    const suggestion = page.locator('#txn-desc-list-0 .desc-suggestion', { hasText: 'Autofill transfer' });
    await expect(suggestion).toBeVisible();
    await suggestion.click();

    // The row's description is the picked text and the matched split's fields prefilled:
    // the Desc Savings account (+40.00 magnitude in the signed field) and the memo.
    await expect(desc0).toHaveValue('Autofill transfer');
    await expect(page.locator('#txn-amount-0')).toHaveValue('40.00');
    await expect(page.locator('#txn-memo-0')).toHaveValue('first memo');
  });

  test('prefill does NOT overwrite a row the user has already typed', async ({ page, server }) => {
    await login(page, server);
    await createAsset(page, 'Guard Checking');
    await createAsset(page, 'Guard Savings');

    // Seed a split WITH a description on a prior transaction.
    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();
    await page.locator('#txn-main-account').selectOption({ label: 'Guard Checking' });
    await page.locator('#txn-account-0').selectOption({ label: 'Guard Savings' });
    await page.locator('#txn-amount-0').fill('15.00');
    await page.locator('#txn-desc-0').fill('Guard payment');
    await page.getByRole('button', { name: /^save$/i }).click();
    await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

    // New entry: TYPE an amount on row 0 FIRST (row is non-empty), then pick the
    // description -> the prefill must NOT clobber the typed amount.
    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();
    await page.locator('#txn-amount-0').fill('99.00'); // user input first
    const desc0 = page.locator('#txn-desc-0');
    await desc0.click();
    await desc0.fill('Guard');
    const suggestion = page.locator('#txn-desc-list-0 .desc-suggestion', { hasText: 'Guard payment' });
    await expect(suggestion).toBeVisible();
    await suggestion.click();

    // The typed amount survives (never-overwrites guard); it was NOT replaced by 15.00.
    await expect(page.locator('#txn-amount-0')).toHaveValue('99.00');
  });

  test('a new auto-appended row also autocompletes + prefills (clone contract)', async ({ page, server }) => {
    await login(page, server);
    await createAsset(page, 'Clone Checking');
    await createAsset(page, 'Clone Savings');

    // Seed a split with a description.
    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();
    await page.locator('#txn-main-account').selectOption({ label: 'Clone Checking' });
    await page.locator('#txn-account-0').selectOption({ label: 'Clone Savings' });
    await page.locator('#txn-amount-0').fill('22.00');
    await page.locator('#txn-desc-0').fill('Clone recall');
    await page.getByRole('button', { name: /^save$/i }).click();
    await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

    // New entry: fill row 0 to trigger the auto-append of row 1, then drive row 1's
    // (cloned) description input -> it must autocomplete + prefill just like a page-
    // rendered row (proves stripDescField + initDescField re-wired the clone).
    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();
    await page.locator('#txn-account-0').selectOption({ label: 'Clone Checking' });
    await expect(page.locator('#txn-desc-1')).toBeVisible(); // row 1 auto-appended
    const desc1 = page.locator('#txn-desc-1');
    await desc1.click();
    await desc1.fill('Clone');
    const suggestion = page.locator('#txn-desc-list-1 .desc-suggestion', { hasText: 'Clone recall' });
    await expect(suggestion).toBeVisible();
    await suggestion.click();

    await expect(desc1).toHaveValue('Clone recall');
    await expect(page.locator('#txn-amount-1')).toHaveValue('22.00');
  });

  test('the EXPENSE grid description input autocompletes + prefills (magnitude mode)', async ({ page, server }) => {
    await login(page, server); // admin passes ExpenseSubmit (can_submit OR admin)
    await createExpense(page, 'Exp Supplies');
    await createAsset(page, 'Exp Cash');

    // Seed a TRANSACTION split (suggest/prefill read splits, not report lines) on the
    // expense account, carrying a description the expense grid can later recall. The
    // expense row auto-defaults program + class from the account so the txn posts.
    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();
    // p26.34: header = Exp Cash (balancing asset); body row 0 = Exp Supplies (expense,
    // auto-defaults program/class) 33 with the description to recall.
    await page.locator('#txn-main-account').selectOption({ label: 'Exp Cash' });
    await page.locator('#txn-account-0').selectOption({ label: 'Exp Supplies' });
    await page.locator('#txn-amount-0').fill('33.00');
    await page.locator('#txn-desc-0').fill('Printer paper');
    await page.getByRole('button', { name: /^save$/i }).click();
    await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

    // Open an expense report's line grid, type the description prefix on line 0 -> the
    // suggestion appears; pick it -> the matched split's amount prefills as a POSITIVE
    // MAGNITUDE (the expense grid derives the sign from the account type server-side).
    await page.goto('/expenses');
    await page.getByRole('button', { name: /new expense report/i }).click();
    await page.waitForURL('**/expenses/*');
    await expect(page.locator('form#expense-grid-form')).toBeVisible();
    const desc0 = page.locator('#el-desc-0');
    await desc0.click();
    await desc0.fill('Printer');
    const suggestion = page.locator('#el-desc-list-0 .desc-suggestion', { hasText: 'Printer paper' });
    await expect(suggestion).toBeVisible();
    await suggestion.click();

    await expect(desc0).toHaveValue('Printer paper');
    // Magnitude mode: the stored -/+ sign is stripped; the amount is the positive figure.
    await expect(page.locator('#el-amount-0')).toHaveValue('33.00');
    // The recalled account (an offered R/E leaf) also filled.
    await expect(page.locator('#el-account-0')).toHaveValue(/\d+/);
  });
});
