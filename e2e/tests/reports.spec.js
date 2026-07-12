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
const FA = '/reports/fund_activity';
const ABR = '/reports/activities_by_restriction';

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

// p15.8 FUND BALANCES & ACTIVITY: the donor-restricted fund tracking / per-grant funder
// view (D20). It has TWO views selected by the report-specific FUND param: the LIST
// (fund roster with as-of balances + funder/restriction metadata) and the SINGLE-FUND
// STATEMENT (Opening + Received − Applied == Closing, Applied split into EXPENSE vs
// NON-EXPENSE applications). This spec SEEDS a restricted fund, a receipt into it, and a
// capital-asset PURCHASE from it (the non-expense application), then:
//   - opens the LIST, confirms the fund selector is present and the fund's roster row +
//     funder metadata render;
//   - picks the fund -> the single-fund STATEMENT with the applied SPLIT (the Received,
//     Applied — expenses, and Applied — non-expense lines) renders;
//   - the CSV endpoint returns text/csv.
//
// This test MUTATES (creates a fund, three accounts, two txns); names are unique so it
// never collides with sibling specs sharing the worker db, and it only ADDS rows (never
// asserts a global count). Strict CSP (script-src 'self') => NO page.waitForFunction;
// only locator/URL/response waits + a plain page.request fetch for the CSV. Selectors are
// the report/params marker classes, the fund-select name, and the localized line labels.

// createRevenueAccount makes a leaf REVENUE account mapped to the root subsidiary (the
// type change triggers an htmx form-swap, like the expense path in txn-editor.spec.js);
// a revenue split is the "Received" side of a fund receipt.
async function createRevenueAccount(page, name) {
  await page.goto('/accounts');
  await page.getByRole('button', { name: /new account/i }).click();
  await expect(page.locator('form#account-form.e2e-settled')).toBeVisible();
  await page.locator('#af-type').selectOption('revenue');
  await expect(page.locator('#af-name-en')).toBeVisible();
  await page.locator('#af-name-en').fill(name);
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) await rootSub.check();
  await saveAndReload(page, { reloadPath: '/accounts' });
  await expect(page.locator('tr.acct-row', { hasText: name })).toBeVisible();
}

// createExpenseAccount makes a leaf EXPENSE account (default functional class program)
// mapped to the root subsidiary. An expense split requires a functional class (rule 7),
// which prefills from this default on the txn form.
async function createExpenseAccount(page, name) {
  await page.goto('/accounts');
  await page.getByRole('button', { name: /new account/i }).click();
  await expect(page.locator('form#account-form.e2e-settled')).toBeVisible();
  await page.locator('#af-type').selectOption('expense');
  await expect(page.locator('#af-func')).toBeVisible();
  await page.locator('#af-name-en').fill(name);
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) await rootSub.check();
  await page.locator('#af-func').selectOption('program');
  await saveAndReload(page, { reloadPath: '/accounts' });
  await expect(page.locator('tr.acct-row', { hasText: name })).toBeVisible();
}

// createFund makes a restricted fund scoped to the root subsidiary via the /funds form.
async function createFund(page, name, funder) {
  await page.goto('/funds');
  await page.getByRole('button', { name: /new fund/i }).click();
  await expect(page.locator('form#fund-form.e2e-settled')).toBeVisible();
  await page.locator('#ff-name').fill(name);
  await page.locator('#ff-funder').fill(funder);
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) await rootSub.check();
  const reloaded = page.waitForResponse(
    (r) => r.url().endsWith('/funds') && r.request().method() === 'GET',
  );
  await page.getByRole('button', { name: /^save$/i }).click();
  await reloaded;
  await expect(page.locator('tr.fund-row', { hasText: name })).toBeVisible();
}

test('reports: open the fund report (list), pick a fund, see its period statement with the applied split, CSV returns', async ({
  page,
  server,
}) => {
  await login(page, server);

  // --- seed: a restricted fund, its cash + a capital (Building) asset, a revenue line ---
  await createFund(page, 'Stmt Fund E2E', 'Anonymous Donor E2E');
  await createAsset(page, 'Stmt Cash E2E');
  await createAsset(page, 'Stmt Building E2E');
  await createRevenueAccount(page, 'Stmt Gift E2E');

  // Receipt INTO the fund: DR Stmt Cash 100.00 (fund), CR Stmt Gift 100.00 (fund). Both
  // splits tagged the fund so the txn nets to zero WITHIN the fund (D20). The revenue
  // split's program prefills from the seeded root program.
  await page.goto('/transactions/new');
  await expect(page.locator('form#txn-form')).toBeVisible();
  await page.locator('#txn-account-0').selectOption({ label: 'Stmt Cash E2E' });
  await page.locator('#txn-amount-0').fill('100.00');
  await page.locator('#txn-fund-0').selectOption({ label: 'Stmt Fund E2E' });
  await page.locator('#txn-account-1').selectOption({ label: 'Stmt Gift E2E' });
  await page.locator('#txn-amount-1').fill('-100.00');
  await page.locator('#txn-fund-1').selectOption({ label: 'Stmt Fund E2E' });
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL('**/register**');

  // Capital-asset PURCHASE applying the fund (the NON-EXPENSE application): DR Stmt
  // Building 40.00 (fund), CR Stmt Cash 40.00 (fund). Both asset splits, fund-tagged.
  await page.goto('/transactions/new');
  await expect(page.locator('form#txn-form')).toBeVisible();
  await page.locator('#txn-account-0').selectOption({ label: 'Stmt Building E2E' });
  await page.locator('#txn-amount-0').fill('40.00');
  await page.locator('#txn-fund-0').selectOption({ label: 'Stmt Fund E2E' });
  await page.locator('#txn-account-1').selectOption({ label: 'Stmt Cash E2E' });
  await page.locator('#txn-amount-1').fill('-40.00');
  await page.locator('#txn-fund-1').selectOption({ label: 'Stmt Fund E2E' });
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL('**/register**');

  // --- LIST view: the fund roster with the report-specific FUND selector ---
  await page.goto(`${FA}?scope=1`);
  await expect(page.locator('form.report-params')).toBeVisible();
  await expect(page.locator('select.report-scope-select[name="scope"]')).toBeVisible();
  const fundSelect = page.locator('select.report-fund-select[name="fund"]');
  await expect(fundSelect).toBeVisible();
  // The roster shows the fund row with its funder metadata (a restricted fund line).
  const listTable = page.locator('table.report-table');
  await expect(listTable).toBeVisible();
  await expect(listTable).toContainText('Stmt Fund E2E');
  await expect(listTable).toContainText('Anonymous Donor E2E');
  // The Unrestricted line is always present (fund 0).
  await expect(listTable).toContainText('Unrestricted');

  // --- pick the fund -> the SINGLE-FUND STATEMENT with the applied split ---
  await fundSelect.selectOption({ label: 'Stmt Fund E2E' });
  await page.locator('form.report-params [name="from"]').fill('2025-01-01');
  await page.locator('form.report-params [name="to"]').fill('2030-12-31');
  await page.locator('form.report-params button[type="submit"]').click();
  await page.waitForURL('**/reports/fund_activity?**');

  // The statement renders Opening/Received/Applied(expense + NON-expense)/Closing +
  // the total-fund-assets reconciliation line. Assert the localized line labels (en)
  // are present -- the applied SPLIT is the point of the report.
  const stmt = page.locator('table.report-table');
  await expect(stmt).toBeVisible();
  await expect(stmt).toContainText('Opening');
  await expect(stmt).toContainText('Received');
  await expect(stmt).toContainText('Applied — expenses');
  await expect(stmt).toContainText('Applied — non-expense');
  await expect(stmt).toContainText('Closing');
  await expect(stmt).toContainText('Total fund assets');
  // Opening/Closing (spendable) and Total assets are framing rows (subtotal/total).
  await expect(
    page.locator('table.report-table tr.report-subtotal, table.report-table tr.report-total'),
  ).not.toHaveCount(0);
  // A drillable figure is present (the reconciliation invariant is unit-tested; here we
  // confirm the statement cells render as drill links).
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

test('reports: open the activities-by-restriction statement, see the two restriction columns + released line + change in net assets, CSV returns', async ({
  page,
  server,
}) => {
  await login(page, server);

  // --- seed: a restricted fund + a receipt INTO it (With-donor-restriction support) and
  // an unrestricted expense (a Without-restriction expense). The receipt makes the fund
  // hold spendable resources; the point of THIS spec is the STRUCTURE (the two columns +
  // released + change), so a receipt + an unrestricted expense renders every row.
  await createFund(page, 'Restr Fund E2E', 'Restr Donor E2E');
  await createAsset(page, 'Restr Cash E2E');
  await createRevenueAccount(page, 'Restr Gift E2E');
  await createExpenseAccount(page, 'Restr Cost E2E');

  // Receipt INTO the fund: DR Restr Cash 100.00 (fund), CR Restr Gift 100.00 (fund).
  await page.goto('/transactions/new');
  await expect(page.locator('form#txn-form')).toBeVisible();
  await page.locator('#txn-account-0').selectOption({ label: 'Restr Cash E2E' });
  await page.locator('#txn-amount-0').fill('100.00');
  await page.locator('#txn-fund-0').selectOption({ label: 'Restr Fund E2E' });
  await page.locator('#txn-account-1').selectOption({ label: 'Restr Gift E2E' });
  await page.locator('#txn-amount-1').fill('-100.00');
  await page.locator('#txn-fund-1').selectOption({ label: 'Restr Fund E2E' });
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL('**/register**');

  // An UNRESTRICTED expense (no fund): DR Restr Cost 30.00, CR Restr Cash 30.00. Its
  // functional class + program prefill from the form defaults (expense splits need both).
  await page.goto('/transactions/new');
  await expect(page.locator('form#txn-form')).toBeVisible();
  await page.locator('#txn-account-0').selectOption({ label: 'Restr Cost E2E' });
  await page.locator('#txn-amount-0').fill('30.00');
  await page.locator('#txn-account-1').selectOption({ label: 'Restr Cash E2E' });
  await page.locator('#txn-amount-1').fill('-30.00');
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL('**/register**');

  // --- open the statement over a wide period, root scope ---
  await page.goto(`${ABR}?scope=1`);
  await expect(page.locator('form.report-params')).toBeVisible();
  await expect(page.locator('select.report-scope-select[name="scope"]')).toBeVisible();
  await page.locator('form.report-params [name="from"]').fill('2025-01-01');
  await page.locator('form.report-params [name="to"]').fill('2030-12-31');
  await page.locator('form.report-params button[type="submit"]').click();
  await page.waitForURL('**/reports/activities_by_restriction?**');

  const abrStmt = page.locator('table.report-table');
  await expect(abrStmt).toBeVisible();
  // The two restriction COLUMNS (localized en headers) + the Total column.
  await expect(abrStmt).toContainText('Without donor restrictions');
  await expect(abrStmt).toContainText('With donor restrictions');
  // The signature line ROWS: revenue, the DERIVED released line, expenses, and change.
  await expect(abrStmt).toContainText('Revenue and support');
  await expect(abrStmt).toContainText('Net assets released from restrictions');
  await expect(abrStmt).toContainText('Expenses');
  await expect(abrStmt).toContainText('Change in net assets');
  // The Change-in-net-assets line is a grand-total row (kind marker).
  await expect(page.locator('table.report-table tr.report-total')).not.toHaveCount(0);
  // A drillable figure is present (reconciliation is unit-tested; here we confirm cells
  // render as drill links — e.g. the released fund-set drill).
  await expect(page.locator('a.report-drill-link').first()).toBeVisible();

  // --- the CSV export returns text/csv ---
  await expect(page.locator('a.report-csv-link')).toBeVisible();
  const abrCsvHref = await page.locator('a.report-csv-link').getAttribute('href');
  const abrResp = await page.request.get(abrCsvHref);
  expect(abrResp.status()).toBe(200);
  expect(abrResp.headers()['content-type']).toContain('text/csv');
  const abrBody = await abrResp.text();
  expect(abrBody.split('\n')[0]).toContain(',');
});
