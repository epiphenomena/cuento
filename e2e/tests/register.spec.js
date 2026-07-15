// @ts-check
// Functional test of the REAL account register (p12.1). It drives the actual
// /accounts/{id}/register page served by `cuento serve -dev` against a fresh
// migrated db with a seeded admin (is_admin -> TxnRead). It logs in, creates a
// reconcilable asset account through the inline chart-of-accounts form, follows the
// per-row "Register" link, and asserts the register page renders: the heading, the
// filter form, the column headers, and -- because the account is reconcilable --
// the recon column (a p16-wired placeholder gated on the reconcilable flag here).
// Selectors come from register.tmpl / accounts.tmpl.

const { test, expect } = require('../fixtures');
const { openNewAccount, saveAccount } = require('../helpers');

async function login(page, server) {
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');
}

test.describe('account register', () => {
  test('opens a reconcilable account register from the chart of accounts', async ({ page, server }) => {
    await login(page, server);

    // Create a reconcilable asset account to open a register for.
    await openNewAccount(page);
    await page.locator('#af-name-en').fill('Checking E2E');
    await page.locator('#af-type').selectOption('asset');
    const rootSub = page.locator('input[name="sub_1"]');
    if (!(await rootSub.isChecked())) {
      await rootSub.check();
    }
    // Reconcilable flag -> the register renders the recon column.
    const recon = page.locator('input[name="reconcilable"]');
    if (!(await recon.isChecked())) {
      await recon.check();
    }
    await saveAccount(page);

    // Open the register by clicking the account NAME (p25: the name is the register
    // link; the dedicated Register button was dropped).
    const row = page.locator('tr.acct-row', { hasText: 'Checking E2E' });
    await row.getByRole('link', { name: 'Checking E2E' }).click();
    await page.waitForURL('**/register');

    // The register page renders its heading, the filter form (now in the section
    // bar, p23.12), and the columns.
    await expect(page.getByRole('heading', { name: /register/i })).toBeVisible();
    await expect(page.locator('.app-subnav-controls form.subnav-filters')).toBeVisible();
    await expect(page.locator('table.register-table')).toBeVisible();
    await expect(page.locator('table.register-table thead')).toContainText(/amount/i);

    // Reconcilable -> the recon column header is present.
    await expect(page.locator('th[data-col="recon"]')).toBeVisible();

    // No transactions yet -> the empty-state row shows.
    await expect(page.locator('tr.reg-empty')).toBeVisible();

    // p23.12: changing a section-bar filter AUTO-APPLIES — an htmx GET to this
    // register carrying the filter param, swapping ONLY #register-results (no Apply
    // button, no full navigation). Drive a real subsidiary change and confirm the
    // GET fires with sub= and the results region re-renders in place.
    const registerUrl = page.url();
    const swap = page.waitForResponse(
      (r) => r.url().includes('/register?') && /[?&]sub=[1-9]/.test(r.url()) && r.status() === 200,
    );
    await page.locator('#reg-sub').selectOption({ index: 1 });
    await swap;
    // htmx swaps outerHTML in place: the URL is unchanged (no hx-push-url) and the
    // results table is still present (now scoped to the chosen subsidiary).
    await expect(page).toHaveURL(registerUrl);
    await expect(page.locator('#register-results table.register-table')).toBeVisible();
  });

  // p26.6: a PLACEHOLDER (parent) account's register rolls up the transactions of
  // its descendant leaf accounts, with a single combined running balance. Build a
  // parent "Cash P26" with two leaf children, post a transfer between the children,
  // then open the PARENT register and see both child rows + the running balance.
  test('parent-account register rolls up descendant transactions', async ({ page, server }) => {
    await login(page, server);

    // The parent placeholder (an asset with no parent, mapped to the root sub).
    await createAccount(page, { name: 'Cash P26', type: 'asset' });
    // Two leaf children under it (selecting the parent makes Cash P26 a placeholder).
    await createAccount(page, { name: 'BOA P26', type: 'asset', parent: 'Cash P26' });
    await createAccount(page, { name: 'WF P26', type: 'asset', parent: 'Cash P26' });

    // Post a balanced transfer BOA P26 -> WF P26 (DR WF 12.00 / CR BOA 12.00) via the
    // real editor, opened from the WF leaf register.
    await page.goto('/accounts');
    await page.locator('tr.acct-row', { hasText: 'WF P26' })
      .getByRole('link', { name: 'WF P26' }).click();
    await page.waitForURL('**/register');
    await page.locator('main a.btn-primary', { hasText: /new transaction/i }).click();
    await page.waitForURL(/\/transactions\/new/);
    await expect(page.locator('form#txn-form')).toBeVisible();
    // p26.1: the split <option> label is the dotted ancestor path (Parent.Leaf), so a
    // child account is selected by its full path, not its bare name.
    await page.locator('#txn-account-0').selectOption({ label: 'Cash P26.WF P26' });
    await page.locator('#txn-amount-0').fill('12.00');
    await page.locator('#txn-account-1').selectOption({ label: 'Cash P26.BOA P26' });
    await page.locator('#txn-amount-1').fill('-12.00');
    await page.locator('#txn-memo').fill('rollup transfer');
    await page.getByRole('button', { name: /^save$/i }).click();
    await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

    // Open the PARENT register. A leaf holds the splits; the parent register must show
    // BOTH descendant rows (one per child) rolled up, even though the parent itself
    // holds no splits.
    await page.goto('/accounts');
    await page.locator('tr.acct-row', { hasText: 'Cash P26' })
      .getByRole('link', { name: 'Cash P26' }).click();
    await page.waitForURL('**/register');

    const table = page.locator('table.register-table');
    await expect(table).toBeVisible();
    // Two rolled-up rows (the transfer appears once per child leaf).
    await expect(page.locator('tr.reg-row')).toHaveCount(2);
    // The transfer memo and both amounts are present.
    await expect(table).toContainText('rollup transfer');
    await expect(table).toContainText('12.00');
    // The combined running balance reaches 0.00 (BOA -12 then WF +12 net to zero) --
    // present in the running-balance column of the last merged row.
    await expect(table).toContainText('0.00');
  });

  // p26.9: transaction listings show the MOST RECENT transaction on TOP (reverse
  // chronological). Post two dated transactions on one account, then confirm the
  // FIRST register data row is the newer one and the running balance still reads
  // correctly (top row = latest cumulative balance).
  test('register lists newest transaction first', async ({ page, server }) => {
    await login(page, server);

    // Two plain asset accounts (a transfer avoids the required program/class on R/E
    // splits; the ordering behavior is identical).
    await createAccount(page, { name: 'Cash P269', type: 'asset' });
    await createAccount(page, { name: 'Savings P269', type: 'asset' });

    // Helper: post a Cash -> Savings transfer (Cash -amount / Savings +amount) with a
    // memo/date, entered from the Cash register.
    const postSpend = async (date, amount, memo) => {
      await page.goto('/accounts');
      await page.locator('tr.acct-row', { hasText: 'Cash P269' })
        .getByRole('link', { name: 'Cash P269' }).click();
      await page.waitForURL('**/register');
      await page.locator('main a.btn-primary', { hasText: /new transaction/i }).click();
      await page.waitForURL(/\/transactions\/new/);
      await expect(page.locator('form#txn-form')).toBeVisible();
      await page.locator('#txn-date').fill(date);
      // Filling row 0 auto-appends row 1 (p25.2), so select account 0 first.
      await page.locator('#txn-account-0').selectOption({ label: 'Cash P269' });
      await expect(page.locator('#txn-account-1')).toBeVisible();
      await page.locator('#txn-amount-0').fill('-' + amount);
      await page.locator('#txn-account-1').selectOption({ label: 'Savings P269' });
      await page.locator('#txn-amount-1').fill(amount);
      await page.locator('#txn-memo').fill(memo);
      await page.getByRole('button', { name: /^save$/i }).click();
      await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));
    };

    // Post the OLDER one first, then the NEWER one, so insertion order (split id)
    // differs from date order -- the display must sort by date DESC regardless.
    await postSpend('2025-01-10', '10.00', 'older spend P269');
    await postSpend('2025-06-20', '25.00', 'newer spend P269');

    // Open the Cash register and read the data rows in DOM order.
    await page.goto('/accounts');
    await page.locator('tr.acct-row', { hasText: 'Cash P269' })
      .getByRole('link', { name: 'Cash P269' }).click();
    await page.waitForURL('**/register');
    await expect(page.locator('tr.reg-row')).toHaveCount(2);

    // The FIRST (top) data row is the NEWER transaction (reverse chronological).
    await expect(page.locator('tr.reg-row').first().locator('.reg-memo'))
      .toHaveText('newer spend P269');
    // The LAST (bottom) row is the older transaction.
    await expect(page.locator('tr.reg-row').last().locator('.reg-memo'))
      .toHaveText('older spend P269');
    // Running balance is the ascending cumulative (oldest->this-row), so the TOP row
    // shows the latest balance (-35.00) and the bottom the earliest (-10.00).
    await expect(page.locator('tr.reg-row').first().locator('.reg-running'))
      .toContainText('35.00');
    await expect(page.locator('tr.reg-row').last().locator('.reg-running'))
      .toContainText('10.00');
  });
});

// createAccount opens the inline chart-of-accounts form and creates one account,
// optionally under a named parent (the #af-parent select), then reloads /accounts.
// Mirrors the p25.1 flow used across the register/txn specs.
async function createAccount(page, { name, type, parent }) {
  await openNewAccount(page);
  await page.locator('#af-name-en').fill(name);
  // Only change the type when it differs from the form default ('asset'): selecting
  // a non-default type re-fetches the form in place (htmx hx-get on #af-type,
  // HX-Target #account-form). Wait for THAT swap before touching later fields, else
  // the swap discards them. 'asset' is the default, so skip the change entirely.
  if (type && type !== 'asset') {
    const typeSwapped = page.waitForResponse(
      (r) => new URL(r.url()).pathname === '/accounts/new' && r.request().method() === 'GET',
    );
    await page.locator('#af-type').selectOption(type);
    await typeSwapped;
  }
  if (parent) {
    await page.locator('#af-parent').selectOption({ label: parent });
  }
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) {
    await rootSub.check();
  }
  await saveAccount(page);
  await expect(page.locator('tr.acct-row', { hasText: name })).toBeVisible();
}
