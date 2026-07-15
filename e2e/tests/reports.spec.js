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
const { saveAndReload, openNewAccount, saveAccount } = require('../helpers');

const TB = '/reports/trial_balance';
const BS = '/reports/balance_sheet';
const IS = '/reports/income_statement';
const AL = '/reports/account_ledger';
const FE = '/reports/functional_expenses';
const FA = '/reports/fund_activity';
const ABR = '/reports/activities_by_restriction';
const PS = '/reports/program_statement';
const F990 = '/reports/form_990';

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
  await openNewAccount(page);
  await page.locator('#af-name-en').fill(name);
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) {
    await rootSub.check();
  }
  await saveAccount(page);
  await expect(page.locator('tr.acct-row', { hasText: name })).toBeVisible();
}

// createChildAsset makes a leaf ASSET under an existing asset parent (making the parent
// a PLACEHOLDER). Type stays "asset" (its default) so no htmx form re-swap races the
// parent select; the parent select is retried until it sticks (the accounts.spec
// pattern). Used by the p26.26 trial-balance nested-tree test.
async function createChildAsset(page, name, parentName) {
  await page.goto('/accounts/new');
  await page.locator('#af-name-en').fill(name);
  await expect(async () => {
    await page.locator('#af-parent').selectOption({ label: parentName });
    await expect(page.locator('#af-parent')).not.toHaveValue('0');
  }).toPass({ timeout: 5000 });
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) await rootSub.check();
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL(/\/accounts$/);
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

// p15.12 REPORTS INDEX (/reports): the grant-filtered directory of reports, grouped by
// report group, each a link to /reports/{id}. The seeded admin is is_admin, so it sees
// EVERY group/report (the per-persona grant filtering is unit-tested in the Go layer).
// This spec confirms the end-to-end path: admin opens /reports, sees grouped report
// links, clicks the trial balance, and lands on that report. READ-ONLY (navigation
// only; mutates nothing durable). Strict CSP (script-src 'self') => NO
// page.waitForFunction; only locator/URL waits. Selectors are the index marker classes
// and an href (language-independent).
test('reports: open the index, see grouped report links, click one, land on the report', async ({
  page,
  server,
}) => {
  await login(page, server);

  // --- open the index page ---
  await page.goto('/reports');

  // At least one group SECTION with a report LIST renders (admin sees all four groups).
  await expect(page.locator('section.reports-group').first()).toBeVisible();
  await expect(page.locator('ul.reports-list').first()).toBeVisible();

  // The trial-balance report is a link (admin reaches the financial group). Its href is
  // the concrete report route -- clicking it lands on the real report (no dead link).
  const tbLink = page.locator('a[href="/reports/trial_balance"]');
  await expect(tbLink).toBeVisible();
  await tbLink.click();
  await page.waitForURL('**/reports/trial_balance');

  // The report page renders (its shared params form + table shell).
  await expect(page.locator('form.report-params')).toBeVisible();
  await expect(page.locator('table.report-table')).toBeVisible();
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
  await expect(table).toContainText('Change in net assets');

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
  await page.locator('#txn-main-account').selectOption({ label: 'Drill Checking' });
  await page.locator('#txn-account-0').selectOption({ label: 'Drill Savings' });
  await page.locator('#txn-amount-0').fill('42.00');
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

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

// p26.26 NESTED ACCOUNT TREE + collapse/expand controls on a REPORT page. Seeds a
// PARENT asset with a CHILD leaf carrying a balance (a balanced transfer to a counter
// account), so the trial balance nests the child under its placeholder parent. Then
// drives the reused p26.25 tree controls (treetable.js): collapse-all hides the child,
// expand-all restores it -- proving the control works on a report table, not only the
// chart of accounts. MUTATES (three accounts + one txn); names are unique so it never
// collides with sibling worker-db specs and it only ADDS rows. Strict CSP => NO
// page.waitForFunction; only locator waits.
test('reports: the trial balance nests accounts and the tree collapse/expand controls work', async ({
  page,
  server,
}) => {
  await login(page, server);

  // A placeholder parent asset, a child leaf under it, and a counter leaf for the
  // balancing side of a transfer.
  await createAsset(page, 'TB Nest Parent');
  await createChildAsset(page, 'TB Nest Child', 'TB Nest Parent');
  await createAsset(page, 'TB Nest Counter');

  // Balanced transfer: the child gets +75, the counter -75 (so the child -- and thus
  // its parent's rolled-up subtotal -- carries a non-zero, tree-nesting balance).
  await page.goto('/transactions/new');
  await expect(page.locator('form#txn-form')).toBeVisible();
  // The child's split-option label is its DOTTED PATH (p26.1: parent.child); the
  // top-level counter's path is just its name.
  await page.locator('#txn-main-account').selectOption({ label: 'TB Nest Counter' });
  await page.locator('#txn-account-0').selectOption({ label: 'TB Nest Parent.TB Nest Child' });
  await page.locator('#txn-amount-0').fill('75.00');
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

  // Open the trial balance at the default as-of (today) so the just-posted txn counts.
  await page.goto(`${TB}?scope=1`);
  const table = page.locator('table.report-table.tree-table');
  await expect(table).toBeVisible();

  // The parent placeholder row (a SUBTOTAL carrying the rolled-up subtotal) and the
  // child leaf row are both present initially (the module fully expands on load).
  const parentRow = table.locator('tr.report-row', { hasText: 'TB Nest Parent' });
  const childRow = table.locator('tr.report-row', { hasText: 'TB Nest Child' });
  await expect(parentRow).toBeVisible();
  await expect(childRow).toBeVisible();
  // The parent row is the depth-0 root; the child sits one level deeper (data-depth).
  await expect(parentRow).toHaveAttribute('data-depth', '0');
  await expect(childRow).toHaveAttribute('data-depth', '1');

  // The tree controls are present and REVEALED by treetable.js (they ship `hidden`).
  const collapseAll = page.locator('.report-controls .tree-collapse-all');
  await expect(collapseAll).toBeVisible();

  // Collapse all -> only depth-0 rows remain; the child hides but the parent stays.
  await collapseAll.click();
  await expect(parentRow).toBeVisible();
  await expect(childRow).toBeHidden();

  // Expand all -> the child reappears.
  await page.locator('.report-controls .tree-expand-all').click();
  await expect(childRow).toBeVisible();
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
  await page.locator('#txn-main-account').selectOption({ label: 'Ledger Savings' });
  await page.locator('#txn-account-0').selectOption({ label: 'Ledger Checking' });
  await page.locator('#txn-amount-0').fill('55.00');
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

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
  await openNewAccount(page);
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
  await saveAccount(page);
  await expect(page.locator('tr.acct-row', { hasText: 'FE Rent' })).toBeVisible();

  // Post an expense transaction: debit FE Rent (an expense, class prefilled management),
  // credit FE Bank. The expense split carries a functional class (rule 7), so it appears
  // in the 990 Part IX matrix.
  await page.goto('/transactions/new');
  await expect(page.locator('form#txn-form')).toBeVisible();
  await page.locator('#txn-main-account').selectOption({ label: 'FE Bank' });
  await page.locator('#txn-account-0').selectOption({ label: 'FE Rent' });
  await expect(page.locator('#txn-progclass-0')).toBeVisible();
  await expect(page.locator('#txn-progclass-0')).toHaveValue('c:management');
  await page.locator('#txn-amount-0').fill('75.00');
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

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
  await expect(table).toContainText('change in net assets to date');

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
  await openNewAccount(page);
  await page.locator('#af-type').selectOption('revenue');
  // #af-program (the IsRE default-program select) exists ONLY on the revenue/expense
  // form, so waiting for it confirms the type-change hx-get swap landed (the old asset
  // form is gone) BEFORE we fill — #af-name-en is on both forms, so waiting for it can
  // let the fill race the in-flight swap and be lost under parallel load. saveAndReload
  // then waits for the new form to settle so Save's hx-post is wired.
  await expect(page.locator('#af-program')).toBeVisible();
  await page.locator('#af-name-en').fill(name);
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) await rootSub.check();
  await saveAccount(page);
  await expect(page.locator('tr.acct-row', { hasText: name })).toBeVisible();
}

// createExpenseAccount makes a leaf EXPENSE account (default functional class program)
// mapped to the root subsidiary. An expense split requires a functional class (rule 7),
// which prefills from this default on the txn form.
async function createExpenseAccount(page, name) {
  await openNewAccount(page);
  await page.locator('#af-type').selectOption('expense');
  await expect(page.locator('#af-func')).toBeVisible();
  await page.locator('#af-name-en').fill(name);
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) await rootSub.check();
  await page.locator('#af-func').selectOption('program');
  await saveAccount(page);
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
  // p26.34: Stmt Cash is the header (balancing) account; its fund is DERIVED from the body
  // (single fund), so it lands fund-tagged too. Body = Stmt Gift -100, fund Stmt Fund.
  await page.locator('#txn-main-account').selectOption({ label: 'Stmt Cash E2E' });
  await page.locator('#txn-account-0').selectOption({ label: 'Stmt Gift E2E' });
  await page.locator('#txn-amount-0').fill('-100.00');
  await page.locator('#txn-fund-0').selectOption({ label: 'Stmt Fund E2E' });
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

  // Capital-asset PURCHASE applying the fund (the NON-EXPENSE application): DR Stmt
  // Building 40.00 (fund), CR Stmt Cash 40.00 (fund). Both asset splits, fund-tagged.
  await page.goto('/transactions/new');
  await expect(page.locator('form#txn-form')).toBeVisible();
  // Header = Stmt Cash (-40 residual, fund derived); body = Stmt Building +40 fund.
  await page.locator('#txn-main-account').selectOption({ label: 'Stmt Cash E2E' });
  await page.locator('#txn-account-0').selectOption({ label: 'Stmt Building E2E' });
  await page.locator('#txn-amount-0').fill('40.00');
  await page.locator('#txn-fund-0').selectOption({ label: 'Stmt Fund E2E' });
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

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
  // p26.34: Restr Cash is the header (+100 residual, fund derived from the body).
  await page.locator('#txn-main-account').selectOption({ label: 'Restr Cash E2E' });
  await page.locator('#txn-account-0').selectOption({ label: 'Restr Gift E2E' });
  await page.locator('#txn-amount-0').fill('-100.00');
  await page.locator('#txn-fund-0').selectOption({ label: 'Restr Fund E2E' });
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

  // An UNRESTRICTED expense (no fund): DR Restr Cost 30.00, CR Restr Cash 30.00. Its
  // functional class + program prefill from the form defaults (expense splits need both).
  await page.goto('/transactions/new');
  await expect(page.locator('form#txn-form')).toBeVisible();
  // Header = Restr Cash (-30 residual); body = Restr Cost +30 (expense, class/program default).
  await page.locator('#txn-main-account').selectOption({ label: 'Restr Cash E2E' });
  await page.locator('#txn-account-0').selectOption({ label: 'Restr Cost E2E' });
  await page.locator('#txn-amount-0').fill('30.00');
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

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

// p15.10 PROGRAM STATEMENT: the DECISION-MAKER view of revenue/expense per PROGRAM (D24),
// the source p15.11 draws 990 Part III from. TWO views selected by the report-specific
// PROGRAM param: the COMPARATIVE side-by-side (every program a column: Account | Currency |
// <program...>, the root column == org total, no separate Total column) and the SINGLE
// SUBTREE (?program=, one program rolled up: Account | Currency | Amount). This spec SEEDS a
// child program + an expense account + a program-tagged expense, then:
//   - opens the COMPARATIVE view, confirms the program selector + the comparative program
//     columns (>=3 headers: Account + Currency + >=1 program) + the Revenue/Expenses/Net
//     section labels + a net (report-total) row;
//   - picks the single program -> the subtree statement (Account | Currency | Amount = 3
//     columns) renders;
//   - the CSV endpoint returns text/csv.
//
// This test MUTATES (creates a program, two accounts, one txn); names are unique so it never
// collides with sibling specs sharing the worker db, and it only ADDS rows (never asserts a
// global count). Strict CSP (script-src 'self') => NO page.waitForFunction; only locator/
// URL/response waits + a plain page.request fetch for the CSV. Selectors are the report/
// params marker classes, the program-select name, and the localized section labels.

// createProgram makes a child program under the seeded root ("General") via the /programs
// form, so the comparative program statement has more than the single root column.
async function createProgram(page, name) {
  await page.goto('/programs');
  await page.getByRole('button', { name: /new program/i }).click();
  await expect(page.locator('#pf-name')).toBeVisible();
  await page.locator('#pf-name').fill(name); // parent defaults to the root program
  await saveAndReload(page, { reloadPath: '/programs', formSelector: 'form#program-form' });
  await expect(page.locator('tr.prog-row', { hasText: name })).toBeVisible();
}

test('reports: open the program statement (comparative), see program columns + accounts + net, pick a single program, CSV returns', async ({
  page,
  server,
}) => {
  await login(page, server);

  // --- seed: a child program, a cash asset, an expense account, and a program-tagged expense ---
  await createProgram(page, 'PS Outreach E2E');
  await createAsset(page, 'PS Cash E2E');
  await createExpenseAccount(page, 'PS Cost E2E'); // default functional class program

  // An expense: DR PS Cost 80.00 (fund none), CR PS Cash 80.00. The expense split needs a
  // program (rule 7 R/E dimension, D24); assign the seeded child program so the comparative
  // view has a non-empty program column.
  await page.goto('/transactions/new');
  await expect(page.locator('form#txn-form')).toBeVisible();
  await page.locator('#txn-main-account').selectOption({ label: 'PS Cash E2E' });
  await page.locator('#txn-account-0').selectOption({ label: 'PS Cost E2E' });
  // p26.41: the combined program/class control -- pick the program node by label (its value
  // is p:<id>, which decodes to program=PS Outreach + class=program on this expense row).
  await expect(page.locator('#txn-progclass-0')).toBeVisible();
  await page.locator('#txn-progclass-0').selectOption({ label: 'PS Outreach E2E' });
  await page.locator('#txn-amount-0').fill('80.00');
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

  // --- COMPARATIVE view (no program chosen): the program selector + program columns ---
  await page.goto(`${PS}?scope=1&from=2025-01-01&to=2030-12-31`);
  await expect(page.locator('form.report-params')).toBeVisible();
  await expect(page.locator('select.report-scope-select[name="scope"]')).toBeVisible();
  // The report-specific PROGRAM selector (comparative default is "— all programs —").
  const progSelect = page.locator('select.report-program-select[name="program"]');
  await expect(progSelect).toBeVisible();
  // The period FROM/TO controls are present -- plain text inputs (never input[type=date]).
  await expect(page.locator('form.report-params [name="from"]')).toBeVisible();
  await expect(page.locator('form.report-params [name="to"]')).toBeVisible();

  const table = page.locator('table.report-table');
  await expect(table).toBeVisible();
  // The COMPARATIVE columns: Account + Currency + >=1 program column => >=3 header cells (a
  // single-column layout could not produce this side-by-side comparison).
  const headers = page.locator('table.report-table thead th');
  expect(await headers.count()).toBeGreaterThanOrEqual(3);
  // The child program is a column header (a stored proper noun rendered verbatim).
  await expect(page.locator('table.report-table thead')).toContainText('PS Outreach E2E');
  // The section labels (localized en) + the net-per-program line.
  await expect(table).toContainText('Revenue');
  await expect(table).toContainText('Expenses');
  await expect(table).toContainText('Net');
  // The expense account row is present.
  await expect(table).toContainText('PS Cost E2E');
  // A NET (report-total) row is present -- the net-per-program line, marked report-total.
  await expect(page.locator('table.report-table tr.report-total')).not.toHaveCount(0);
  // The account cells are DRILL links (program×account drill).
  await expect(page.locator('a.report-drill-link').first()).toBeVisible();

  // --- pick the SINGLE program -> the subtree statement (Account | Currency | Amount) ---
  await progSelect.selectOption({ label: 'PS Outreach E2E' });
  await page.locator('form.report-params [name="from"]').fill('2025-01-01');
  await page.locator('form.report-params [name="to"]').fill('2030-12-31');
  await page.locator('form.report-params button[type="submit"]').click();
  await page.waitForURL('**/reports/program_statement?**');

  const single = page.locator('table.report-table');
  await expect(single).toBeVisible();
  // Exactly THREE columns in the single view (Account, Currency, Amount).
  await expect(page.locator('table.report-table thead th')).toHaveCount(3);
  await expect(single).toContainText('PS Cost E2E');
  await expect(single).toContainText('Net');

  // --- the CSV export link is present and the endpoint returns text/csv ---
  await expect(page.locator('a.report-csv-link')).toBeVisible();
  const csvHref = await page.locator('a.report-csv-link').getAttribute('href');
  const resp = await page.request.get(csvHref);
  expect(resp.status()).toBe(200);
  expect(resp.headers()['content-type']).toContain('text/csv');
  const body = await resp.text();
  expect(body.split('\n')[0]).toContain(',');
});

// p15.11 the 990 PACKAGE: the year-end IRS Form 990 filing package, one labeled section
// per Part in a SINGLE Table (Part III program service summary from p15.10, Part VIII
// revenue from p15.5, Part IX functional-expense totals from p15.7, Part X balance sheet
// from p15.4). EVERY section renders an explicit (Unmapped) bucket rather than dropping
// rows. This spec SEEDS one program-tagged expense (with an effective Part IX 990 line),
// one fund-free revenue receipt (an UNMAPPED revenue -> the Part VIII Unmapped bucket),
// and the asset/liability rows those postings imply, then:
//   - opens the report over the fiscal year (from/to = period param), root scope, USD;
//   - confirms the params form + scope selector + period from/to controls;
//   - sees the FOUR Part section headers (localized en), the (Unmapped) bucket, the Part
//     VIII/IX total rows, and a balancing Part X (report-total) row;
//   - the CSV endpoint returns text/csv.
//
// This test MUTATES (creates three accounts + two txns); names are unique so it never
// collides with sibling specs sharing the worker db, and it only ADDS rows (never asserts
// a global count). Strict CSP (script-src 'self') => NO page.waitForFunction; only
// locator/URL/response waits + a plain page.request fetch for the CSV.
test('reports: open the 990 package, see the four Parts + Unmapped buckets + totals, CSV returns', async ({
  page,
  server,
}) => {
  await login(page, server);

  // --- seed: an expense account carrying an effective Part IX line (IX.16 Occupancy) +
  // functional class, a cash asset, and an UNMAPPED revenue account (no 990 code -> the
  // Part VIII Unmapped bucket). ---
  await createAsset(page, 'F990 Bank E2E');
  await createRevenueAccount(page, 'F990 Gift E2E'); // no 990 code -> Unmapped (Part VIII)

  await openNewAccount(page);
  await page.locator('#af-type').selectOption('expense');
  await expect(page.locator('#af-func')).toBeVisible();
  await page.locator('#af-name-en').fill('F990 Rent E2E');
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) await rootSub.check();
  await page.locator('#af-func').selectOption('management');
  await page.locator('#af-990').selectOption('IX.16'); // effective Part IX line
  await saveAccount(page);
  await expect(page.locator('tr.acct-row', { hasText: 'F990 Rent E2E' })).toBeVisible();

  // A revenue receipt: DR F990 Bank 90.00, CR F990 Gift 90.00 (the revenue split carries a
  // program, prefilled from the seeded root -> Part III; F990 Gift has no 990 code -> Part
  // VIII Unmapped).
  await page.goto('/transactions/new');
  await expect(page.locator('form#txn-form')).toBeVisible();
  // p26.34: header = F990 Bank (+90 residual); body = F990 Gift -90 (revenue, program).
  await page.locator('#txn-main-account').selectOption({ label: 'F990 Bank E2E' });
  await page.locator('#txn-account-0').selectOption({ label: 'F990 Gift E2E' });
  await page.locator('#txn-amount-0').fill('-90.00');
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

  // An expense: DR F990 Rent 30.00 (class management, program prefilled), CR F990 Bank 30.00.
  await page.goto('/transactions/new');
  await expect(page.locator('form#txn-form')).toBeVisible();
  // Header = F990 Bank (-30 residual); body = F990 Rent +30 (expense, class management).
  await page.locator('#txn-main-account').selectOption({ label: 'F990 Bank E2E' });
  await page.locator('#txn-account-0').selectOption({ label: 'F990 Rent E2E' });
  await expect(page.locator('#txn-progclass-0')).toHaveValue('c:management');
  await page.locator('#txn-amount-0').fill('30.00');
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

  // --- open the 990 package over the fiscal year (from/to), root scope, USD ---
  await page.goto(`${F990}?scope=1&from=2025-01-01&to=2030-12-31&currency=USD`);

  // The shared params form + the always-present subsidiary SCOPE selector (D18).
  await expect(page.locator('form.report-params')).toBeVisible();
  await expect(page.locator('select.report-scope-select[name="scope"]')).toBeVisible();
  // The period FROM/TO controls (the fiscal year is the period param) -- plain text inputs.
  await expect(page.locator('form.report-params [name="from"]')).toBeVisible();
  await expect(page.locator('form.report-params [name="to"]')).toBeVisible();

  // The report table renders the FOUR Part section headers (localized en) in one Table.
  const table = page.locator('table.report-table');
  await expect(table).toBeVisible();
  await expect(table).toContainText('Part III');
  await expect(table).toContainText('Part VIII');
  await expect(table).toContainText('Part IX');
  await expect(table).toContainText('Part X');

  // Every section renders an explicit (Unmapped) bucket (never drop rows). At least one is
  // present (Part VIII's is non-empty here: the F990 Gift receipt has no 990 code).
  await expect(table).toContainText('(Unmapped)');

  // The Part VIII / Part IX total rows + the Part X balancing identity row.
  await expect(table).toContainText('Total revenue');
  await expect(table).toContainText('Total functional expenses');
  await expect(table).toContainText('Total liabilities and net assets');
  // A balancing (report-total) row is present (Part VIII/IX/X totals are report-total).
  await expect(page.locator('table.report-table tr.report-total')).not.toHaveCount(0);
  // The Occupancy Part IX line (IX.16, the effective line of F990 Rent) renders.
  await expect(table).toContainText('Occupancy');
  // A drillable figure is present (each amount line drills to its accounts' splits).
  await expect(page.locator('a.report-drill-link').first()).toBeVisible();

  // --- the CSV export link is present and the endpoint returns text/csv ---
  await expect(page.locator('a.report-csv-link')).toBeVisible();
  const resp990 = await page.request.get(`${F990}.csv?scope=1&from=2025-01-01&to=2030-12-31&currency=USD`);
  expect(resp990.status()).toBe(200);
  expect(resp990.headers()['content-type']).toContain('text/csv');
  const body990 = await resp990.text();
  expect(body990.split('\n')[0]).toContain(',');
});
