// @ts-check
// Functional test of the p15.3 TRIAL BALANCE report (the first real report) served
// by `cuento serve -dev` against the worker's fresh migrated db with the seeded
// admin (is_admin => reaches every report without a grant). In ONE test (one login,
// to stay under the login rate limiter shared per worker) it:
//   - opens the trial balance (/reports/trial_balance),
//   - sets the as-of / scope params and re-runs (the shared params form controls),
//   - asserts the shared PARAMS FORM is present, INCLUDING the subsidiary SCOPE
//     selector (rendered on every report, D18) and the report table,
//   - asserts a BALANCING total row is present (the point of a trial balance),
//   - fetches the CSV endpoint and asserts it returns CSV.
//
// TEST-ISOLATION (the worker-scoped `server` shares one db across a worker's specs):
// this spec is READ-ONLY -- it opens a report, changes URL params, and downloads its
// CSV, mutating nothing durable, so it never leaks state into sibling specs.
// Assertions are STRUCTURAL (the fresh worker db has no seeded ledger, so specific
// numbers would be brittle): the params form, scope selector, table shell, a total
// row, and a text/csv response. Selectors are language-independent (ids, name attrs,
// marker classes) so a mid-run locale change elsewhere could never break them. No
// page.waitForFunction (strict CSP: script-src 'self', no unsafe-eval) -- only
// locator/URL/response waits and a plain page.request fetch for the CSV.

const { test, expect } = require('../fixtures');
const { saveAndReload } = require('../helpers');

const TB = '/reports/trial_balance';
const BS = '/reports/balance_sheet';
const IS = '/reports/income_statement';
const AL = '/reports/account_ledger';
const FE = '/reports/functional_expenses';

async function login(page, server) {
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');
}

// createAsset makes a leaf asset account mapped to the root subsidiary (mirrors the
// txn-editor spec): waits for the inline form to settle before Save (hx-post wired)
// and for the reload response (the new row is in the SSR DOM).
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

test('reports: open the trial balance, set as-of/scope, see the balancing total, CSV downloads', async ({
  page,
  server,
}) => {
  await login(page, server);

  // --- open the report HTML page ---
  await page.goto(TB);

  // The shared params form is present.
  await expect(page.locator('form.report-params')).toBeVisible();

  // The subsidiary SCOPE selector is present on the report (every report is scoped,
  // D18): a <select name="scope"> with the marker class, listing at least the root.
  const scope = page.locator('select.report-scope-select[name="scope"]');
  await expect(scope).toBeVisible();
  await expect(scope.locator('option')).not.toHaveCount(0);

  // The as-of date control is present (trial balance is an as-of report). It is a
  // plain text input (never input[type=date], rule 12), named "asof".
  const asof = page.locator('form.report-params [name="asof"]');
  await expect(asof).toBeVisible();

  // Set an explicit as-of and the root scope, then re-run by navigating with params
  // (a GET form; navigating the URL is the same round trip the Run button makes).
  const rootScope = await scope.locator('option').first().getAttribute('value');
  await page.goto(`${TB}?asof=2026-06-30&scope=${rootScope}`);

  // The report TABLE renders with its column headers (account / currency / native /
  // converted come from the Table the report returns).
  await expect(page.locator('table.report-table')).toBeVisible();
  await expect(page.locator('table.report-table thead th')).not.toHaveCount(0);

  // A BALANCING total row is present -- the whole point of a trial balance. The
  // renderer marks the native total rows report-subtotal and the converted grand
  // total report-total; at least one total row must render (structural, not numeric).
  await expect(
    page.locator('table.report-table tr.report-subtotal, table.report-table tr.report-total'),
  ).not.toHaveCount(0);

  // The CSV export link is present and points at the .csv endpoint.
  const csvLink = page.locator('a.report-csv-link');
  await expect(csvLink).toBeVisible();

  // --- fetch the CSV endpoint and assert it returns CSV ---
  const resp = await page.request.get(`${TB}.csv?asof=2026-06-30&scope=${rootScope}`);
  expect(resp.status()).toBe(200);
  expect(resp.headers()['content-type']).toContain('text/csv');
  // A CSV body has at least the localized header row (comma-separated columns).
  const body = await resp.text();
  expect(body.split('\n')[0]).toContain(',');
});

// p15.5 INCOME STATEMENT (statement of activities): open it, set a period + QUARTERLY
// granularity, see the R/E TREE (Revenue + Expenses section labels), the comparative
// period COLUMNS (more than the Line+Total pair), and the NET surplus/deficit line, then
// confirm the CSV returns. READ-ONLY (opens the report, changes URL params, downloads
// CSV -- mutates nothing durable). Assertions are STRUCTURAL (the fresh worker db has no
// seeded ledger, so numbers would be brittle): the params form + scope selector + the
// GRANULARITY select + the period from/to inputs, the section + net labels (localized en
// text present in the table), the comparative-column header count, a net (report-total)
// row, and a text/csv response. No page.waitForFunction (strict CSP) -- only locator/
// URL/response waits.
test('reports: open the income statement, set period + granularity, see the R/E tree + comparative columns + net line, CSV returns', async ({
  page,
  server,
}) => {
  await login(page, server);

  // --- open the income statement at the root scope, a fixed period, quarterly columns ---
  await page.goto(`${IS}?scope=1&from=2025-01-01&to=2026-06-30&granularity=quarter&currency=USD`);

  // The shared params form + the always-present subsidiary SCOPE selector (D18).
  await expect(page.locator('form.report-params')).toBeVisible();
  await expect(page.locator('select.report-scope-select[name="scope"]')).toBeVisible();

  // The GRANULARITY select is present (an income-statement comparative control), set to
  // "quarter" from the query round trip.
  const gran = page.locator('form.report-params select[name="granularity"]');
  await expect(gran).toBeVisible();
  await expect(gran).toHaveValue('quarter');

  // The period FROM/TO controls are present -- plain text inputs (never input[type=date],
  // rule 12), named "from"/"to".
  await expect(page.locator('form.report-params [name="from"]')).toBeVisible();
  await expect(page.locator('form.report-params [name="to"]')).toBeVisible();

  // The report table renders the R/E TREE: the Revenue and Expenses SECTION labels and
  // the NET surplus/deficit line are localized labels rendered verbatim; assert their
  // default-language (en) text is present (structural, not numeric).
  const table = page.locator('table.report-table');
  await expect(table).toBeVisible();
  await expect(table).toContainText('Revenue');
  await expect(table).toContainText('Expenses');
  await expect(table).toContainText('Net surplus');

  // The COMPARATIVE columns: quarterly over 18 months => 6 period columns + Line + Total
  // = 8 header cells. Assert at least 4 (structural: Line + >=2 periods + Total), which a
  // non-comparative single-column layout could not produce.
  const headers = page.locator('table.report-table thead th');
  expect(await headers.count()).toBeGreaterThanOrEqual(4);

  // A NET (report-total) row is present -- the surplus/deficit line, marked report-total.
  await expect(
    page.locator('table.report-table tr.report-total, table.report-table tr.report-subtotal'),
  ).not.toHaveCount(0);

  // --- the CSV export link is present and the endpoint returns text/csv ---
  await expect(page.locator('a.report-csv-link')).toBeVisible();
  const resp = await page.request.get(
    `${IS}.csv?scope=1&from=2025-01-01&to=2026-06-30&granularity=quarter&currency=USD`,
  );
  expect(resp.status()).toBe(200);
  expect(resp.headers()['content-type']).toContain('text/csv');
  const body = await resp.text();
  expect(body.split('\n')[0]).toContain(',');
});

// p15.3d DRILL-DOWN: seed a balanced transfer (so the trial balance has a non-zero
// balance to drill), open the trial balance, click a balance's DRILL link, land on
// the transaction list, and see rows -- each linking to the txn editor + history
// (p12.4). This test MUTATES (creates two accounts + one txn); names are unique so it
// does not collide with sibling specs sharing the worker db, and it only ever ADDS
// rows (never asserts a global count), so the addition is inert to other reads.
//
// Strict CSP (script-src 'self', no unsafe-eval) => NO page.waitForFunction; only
// locator/URL/response waits. Selectors are the drill's marker classes / ids and the
// register action-link names, language-independent where structural.
test('reports: drill a trial-balance figure to its transactions (each linking to the editor/history)', async ({
  page,
  server,
}) => {
  await login(page, server);

  // Two leaf asset accounts and a balanced transfer between them, so each account
  // carries a non-zero (drillable) trial-balance figure.
  await createAsset(page, 'Drill Checking');
  await createAsset(page, 'Drill Savings');

  await page.goto('/transactions/new');
  await expect(page.locator('form#txn-form')).toBeVisible();
  await page.locator('#txn-account-0').selectOption({ label: 'Drill Savings' });
  await page.locator('#txn-amount-0').fill('42.00');
  await page.locator('#txn-account-1').selectOption({ label: 'Drill Checking' });
  await page.locator('#txn-amount-1').fill('-42.00');
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL('**/register**');

  // Open the trial balance at the DEFAULT as-of (today), so the just-posted txn (its
  // date defaults to today) is included -- a pinned past as-of would exclude it and
  // leave every cell zero/absent.
  await page.goto(`${TB}?scope=1`);
  await expect(page.locator('table.report-table')).toBeVisible();

  // A native money cell renders as a DRILL link (the p15.3d retrofit). Click the
  // first one and land on the drill transaction list.
  const drillLink = page.locator('a.report-drill-link').first();
  await expect(drillLink).toBeVisible();
  await drillLink.click();
  await page.waitForURL('**/drill**');

  // The drill page lists the contributing transaction rows.
  await expect(page.locator('table.report-drill-table')).toBeVisible();
  const rows = page.locator('tr.drill-row');
  await expect(rows).not.toHaveCount(0);

  // Each row links to the txn editor and history (p12.4).
  const firstRow = rows.first();
  await expect(firstRow.getByRole('link', { name: /edit/i })).toBeVisible();
  await expect(firstRow.getByRole('link', { name: /history/i })).toBeVisible();

  // The reconciled figure header is shown (the signed native sum of the listed rows).
  await expect(page.locator('.report-drill-figure')).toBeVisible();
});

// p15.6 ACCOUNT LEDGER: seed a balanced transfer (so the chosen account has a real
// opening/closing balance and an in-range line), open the account ledger, pick the
// ACCOUNT (the report-specific selector) + a period, and see the opening/closing
// framing rows, the in-range LINE with its FUND column, and the line's LINK to the txn
// editor (p12.4) -- then confirm the CSV returns. This test MUTATES (creates two
// accounts + one txn); names are unique so it never collides with sibling specs sharing
// the worker db, and it only ADDS rows (never asserts a global count).
//
// Strict CSP (script-src 'self', no unsafe-eval) => NO page.waitForFunction; only
// locator/URL/response waits. Selectors are the account-select marker class, the
// framing-row kind classes, and the register/editor link href -- language-independent.
test('reports: open the account ledger, pick an account + range, see opening/lines/closing + fund column, CSV returns', async ({
  page,
  server,
}) => {
  await login(page, server);

  // Two leaf asset accounts and a balanced transfer, so the ledgered account carries a
  // real balance and an in-range line to print.
  await createAsset(page, 'Ledger Checking');
  await createAsset(page, 'Ledger Savings');

  await page.goto('/transactions/new');
  await expect(page.locator('form#txn-form')).toBeVisible();
  await page.locator('#txn-account-0').selectOption({ label: 'Ledger Checking' });
  await page.locator('#txn-amount-0').fill('55.00');
  await page.locator('#txn-account-1').selectOption({ label: 'Ledger Savings' });
  await page.locator('#txn-amount-1').fill('-55.00');
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL('**/register**');

  // --- open the account ledger; the account SELECTOR (report-specific control) is present ---
  await page.goto(AL);
  await expect(page.locator('form.report-params')).toBeVisible();
  await expect(page.locator('select.report-scope-select[name="scope"]')).toBeVisible();
  const acctSelect = page.locator('select.report-account-select[name="account"]');
  await expect(acctSelect).toBeVisible();

  // Pick the account and set a wide period, then Run (the GET form round trip). The
  // account's option value is its id; select by its (unique) label.
  await acctSelect.selectOption({ label: 'Ledger Checking' });
  await page.locator('form.report-params [name="from"]').fill('2025-01-01');
  // The default To is today, which includes the just-posted (today-dated) txn.
  await page.locator('form.report-params button[type="submit"]').click();
  await page.waitForURL('**/reports/account_ledger?**');

  // The report table renders with the OPENING (subtotal) and CLOSING (total) framing
  // rows and at least one in-range data line.
  const table = page.locator('table.report-table');
  await expect(table).toBeVisible();
  await expect(table.locator('tr.report-subtotal')).not.toHaveCount(0); // opening
  await expect(table.locator('tr.report-total')).not.toHaveCount(0); // closing
  // At least one in-range DATA line (report-row that is neither the opening subtotal nor
  // the closing total) carries the txn-editor link -- the in-range 55.00 posting.
  const dataLines = table.locator('tr.report-row:not(.report-subtotal):not(.report-total)');
  await expect(dataLines).not.toHaveCount(0);

  // The FUND column shows the seeded (unrestricted) split's "Unrestricted" label.
  await expect(table).toContainText('Unrestricted');

  // The in-range line LINKS to the transaction editor (Cell.TxnID -> /transactions/{id}/edit).
  await expect(table.locator('a[href*="/transactions/"][href*="/edit"]').first()).toBeVisible();

  // The opening/closing balance cells are DRILL links (as-of Drill on the framing rows).
  await expect(page.locator('a.report-drill-link').first()).toBeVisible();

  // --- the CSV export link is present and the endpoint returns text/csv ---
  await expect(page.locator('a.report-csv-link')).toBeVisible();
  const csvHref = await page.locator('a.report-csv-link').getAttribute('href');
  const resp = await page.request.get(csvHref);
  expect(resp.status()).toBe(200);
  expect(resp.headers()['content-type']).toContain('text/csv');
  const body = await resp.text();
  expect(body.split('\n')[0]).toContain(',');
});

// p15.7 FUNCTIONAL EXPENSES (IRS Form 990 Part IX): create an expense account with an
// effective Part IX 990 line (IX.16 Occupancy) and a functional class, post an expense
// transaction to it, then open the report and see the 990-LINE grouping row + the three
// functional-CLASS columns (Program / Management & general / Fundraising) + a Total
// column + the grand-total row, and confirm the CSV returns. This test MUTATES (creates
// two accounts + one txn); names are unique so it never collides with sibling specs
// sharing the worker db, and it only ADDS rows (never asserts a global count).
//
// Strict CSP (script-src 'self', no unsafe-eval) => NO page.waitForFunction; only
// locator/URL/response waits. Selectors are the report table marker classes, the class
// column header text (localized en), and the params-form control names — language-
// independent where structural.
test('reports: open the functional expenses (990 Part IX), see the 990-line rows + 3 class columns + total, CSV returns', async ({
  page,
  server,
}) => {
  await login(page, server);

  // An asset account to fund the expense, and an EXPENSE account carrying an effective
  // Part IX line (IX.16 Occupancy) and a default functional class (management).
  await createAsset(page, 'FE Bank');
  await page.goto('/accounts');
  await page.getByRole('button', { name: /new account/i }).click();
  await expect(page.locator('form#account-form.e2e-settled')).toBeVisible();
  // Choosing the expense type swaps in the expense form (the #af-func class field and the
  // type-filtered #af-990 line select), preserving the typed name/sub (overlayFormValues).
  await page.locator('#af-type').selectOption('expense');
  await expect(page.locator('#af-func')).toBeVisible();
  await page.locator('#af-name-en').fill('FE Rent');
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) await rootSub.check();
  await page.locator('#af-func').selectOption('management');
  // The 990 line select (#af-990) offers the expense (Part IX) lines; pick IX.16 Occupancy
  // so the account has an effective Part IX code and lands on its own 990 line.
  await page.locator('#af-990').selectOption('IX.16');
  await saveAndReload(page, { reloadPath: '/accounts' });
  await expect(page.locator('tr.acct-row', { hasText: 'FE Rent' })).toBeVisible();

  // Post an expense transaction: debit FE Rent (an expense, class prefilled management),
  // credit FE Bank. The expense split carries a functional class (rule 7), so it appears
  // in the 990 Part IX matrix.
  await page.goto('/transactions/new');
  await expect(page.locator('form#txn-form')).toBeVisible();
  await page.locator('#txn-account-0').selectOption({ label: 'FE Rent' });
  await expect(page.locator('#txn-class-0')).toBeVisible();
  await expect(page.locator('#txn-class-0')).toHaveValue('management');
  await page.locator('#txn-amount-0').fill('75.00');
  await page.locator('#txn-account-1').selectOption({ label: 'FE Bank' });
  await page.locator('#txn-amount-1').fill('-75.00');
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL('**/register**');

  // --- open the functional-expenses report at the root scope over a wide period ---
  await page.goto(`${FE}?scope=1&from=2025-01-01&to=2030-12-31&currency=USD`);

  // The shared params form + the always-present subsidiary SCOPE selector (D18).
  await expect(page.locator('form.report-params')).toBeVisible();
  await expect(page.locator('select.report-scope-select[name="scope"]')).toBeVisible();
  // The period FROM/TO controls are present -- plain text inputs (never input[type=date]).
  await expect(page.locator('form.report-params [name="from"]')).toBeVisible();
  await expect(page.locator('form.report-params [name="to"]')).toBeVisible();

  // The report table renders the 990 Part IX matrix: the three functional-CLASS column
  // headers (localized en) plus a Total column, and the IX.16 Occupancy 990-line row.
  const table = page.locator('table.report-table');
  await expect(table).toBeVisible();
  const headers = page.locator('table.report-table thead th');
  await expect(headers).toContainText(['Line', 'Program', 'Management & general', 'Fundraising', 'Total']);
  // The 990-LINE grouping row for IX.16 Occupancy (the effective line of FE Rent) is a
  // subtotal row; the account row (FE Rent) sits under it. The Occupancy line label is the
  // IRS-seeded stored text.
  await expect(table).toContainText('Occupancy');
  await expect(table).toContainText('FE Rent');
  // The grand-total row (Total functional expenses) is present, marked report-total.
  await expect(table).toContainText('Total functional expenses');
  await expect(page.locator('table.report-table tr.report-total')).not.toHaveCount(0);
  // At least one 990-line SUBTOTAL row (the grouping row) is present.
  await expect(page.locator('table.report-table tr.report-subtotal')).not.toHaveCount(0);

  // --- the CSV export link is present and the endpoint returns text/csv ---
  await expect(page.locator('a.report-csv-link')).toBeVisible();
  const resp = await page.request.get(`${FE}.csv?scope=1&from=2025-01-01&to=2030-12-31&currency=USD`);
  expect(resp.status()).toBe(200);
  expect(resp.headers()['content-type']).toContain('text/csv');
  const body = await resp.text();
  expect(body.split('\n')[0]).toContain(',');
});

// p15.4 BALANCE SHEET: open it, see the A/L/Net-assets sections + the by-restriction
// net-asset split lines + a balancing (grand-total) row, toggle the per-currency
// DETAIL, and confirm the CSV returns. READ-ONLY (opens the report, changes URL
// params, downloads CSV -- mutates nothing durable). Assertions are STRUCTURAL (the
// fresh worker db has no seeded ledger, so numbers would be brittle): the section
// labels (localized text present anywhere on the page), the net-asset split labels,
// the params form + scope selector + the DETAIL toggle, a grand-total row, the
// per-currency column shape after the toggle, and a text/csv response. No
// page.waitForFunction (strict CSP) -- only locator/URL/response waits.
test('reports: open the balance sheet, see the sections + net-asset split + a balancing total, toggle detail, CSV returns', async ({
  page,
  server,
}) => {
  await login(page, server);

  // --- open the balance sheet at the root scope, a fixed as-of ---
  await page.goto(`${BS}?scope=1&asof=2026-06-30`);

  // The shared params form + the always-present subsidiary SCOPE selector (D18).
  await expect(page.locator('form.report-params')).toBeVisible();
  await expect(page.locator('select.report-scope-select[name="scope"]')).toBeVisible();

  // The per-currency DETAIL toggle is present (a balance-sheet-only control).
  const detail = page.locator('select.report-detail-select[name="detail"]');
  await expect(detail).toBeVisible();

  // The report table renders with the three SECTIONS and the by-restriction net-asset
  // split lines. These are localized labels rendered verbatim in the first column;
  // assert their default-language (en) text is present on the page (structural, not
  // numeric). The section + split labels come straight from the report's Table.
  const table = page.locator('table.report-table');
  await expect(table).toBeVisible();
  await expect(table).toContainText('Assets');
  await expect(table).toContainText('Liabilities');
  await expect(table).toContainText('Net assets');
  await expect(table).toContainText('Net assets without donor restrictions');
  await expect(table).toContainText('Net assets with donor restrictions');
  await expect(table).toContainText('net surplus to date');

  // A BALANCING grand-total row is present -- the identity's right-hand side (Total
  // liabilities and net assets == total assets). The renderer marks it report-total.
  await expect(
    page.locator('table.report-table tr.report-total, table.report-table tr.report-subtotal'),
  ).not.toHaveCount(0);

  // --- toggle the per-currency DETAIL view (navigate with detail=currency, the same
  // GET round trip the Run button makes) and confirm the extra Currency column ---
  await page.goto(`${BS}?scope=1&asof=2026-06-30&detail=currency`);
  await expect(page.locator('table.report-table')).toBeVisible();
  // The detail view has 4 columns (Line / Currency / Native / Converted) vs 2 in the
  // converted-only view; assert at least 4 header cells (structural).
  const headers = page.locator('table.report-table thead th');
  expect(await headers.count()).toBeGreaterThanOrEqual(4);
  // The detail select now shows the "currency" option selected.
  await expect(page.locator('select.report-detail-select[name="detail"]')).toHaveValue('currency');

  // --- the CSV export link is present and the endpoint returns text/csv ---
  await expect(page.locator('a.report-csv-link')).toBeVisible();
  const resp = await page.request.get(`${BS}.csv?scope=1&asof=2026-06-30`);
  expect(resp.status()).toBe(200);
  expect(resp.headers()['content-type']).toContain('text/csv');
  const body = await resp.text();
  expect(body.split('\n')[0]).toContain(',');
});
