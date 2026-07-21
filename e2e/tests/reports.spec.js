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
const { saveAndReload, openNewAccount, saveAccount, selectTxnAccount } = require('../helpers');

const TB = '/reports/trial_balance';
const BS = '/reports/balance_sheet';
const IS = '/reports/income_statement';
const AL = '/reports/account_ledger';
const FE = '/reports/functional_expenses';
const FA = '/reports/fund_activity';
const FS = '/reports/fund_statement';
const FP = '/reports/fund_period';
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

  // p26.76: for a light report (trial balance) the filter form lives in the SECOND-LEVEL
  // nav bar (the subnav), NOT an inline body fieldset — this is the experiment. Assert the
  // form is inside .app-subnav and drives the report from there.
  await expect(page.locator('nav.app-subnav form.report-params')).toBeVisible();
  await expect(page.locator('main#main form.report-params')).toHaveCount(0);

  // The subsidiary SCOPE selector is present on the report (every report is scoped,
  // D18): a <select name="scope"> with the marker class, listing at least the root.
  const scope = page.locator('select.report-scope-select[name="scope"]');
  await expect(scope).toBeVisible();
  await expect(scope.locator('option')).not.toHaveCount(0);

  // p26.90: the subnav filters AUTO-APPLY on change now (no Run button for JS users — it
  // lives in <noscript>). The dedicated apply-on-change / latest-wins / CSV-href behavior
  // test is below; here we confirm the same GET round trip via navigation (the no-JS path)
  // renders the table.
  await page.goto(`${TB}?asof=2026-06-30`);
  await expect(page.locator('table.report-table')).toBeVisible();

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
  expect(body).toContain(',');
});

// p26.90 APPLY-ON-CHANGE: the report filter form auto-applies on change (no Run button for
// JS users), swapping ONLY #report-results, keeping the CSV export href fresh, pushing the
// URL for persistence, and settling on the LATEST response when changes fire in quick
// succession (hx-sync="this:replace"). Drives the trial balance's AS-OF date control — a
// plain text input (rule 12) at the SCOPE BASE currency (USD), so the report renders WITHOUT
// any exchange-rate conversion (the fresh worker db has no rates; picking a non-base target
// currency would legitimately error, unrelated to this chrome). The as-of `change` fires on
// blur. READ-ONLY (opens a report; mutates nothing durable). Strict CSP (script-src 'self')
// => NO page.waitForFunction; only locator/URL/attribute waits.
test('reports: the filter form auto-applies on change (no Run), refreshes the CSV href, persists, latest-wins', async ({
  page,
  server,
}) => {
  await login(page, server);

  await page.goto(`${TB}?scope=1`);
  await expect(page.locator('#report-results table.report-table')).toBeVisible();

  // With JS active, the Run button is inside <noscript> — it is NOT a DOM element, so a JS
  // user has no submit button (apply-on-change is the whole interaction).
  await expect(page.locator('form.report-params button[type="submit"]')).toHaveCount(0);

  const asof = page.locator('nav.app-subnav form.report-params [name="asof"]');
  await expect(asof).toBeVisible();

  // --- change the as-of date (ONE deliberate change, no Run click): fill + blur fires
  // `change`, the hx-get swaps #report-results, and hx-push-url syncs the URL. Assert the
  // eventual state with a retrying expect (generous timeout). The CSV export href lives
  // INSIDE the swapped fragment, so it reflecting the new as-of proves the swap landed AND
  // the export link is recomputed from the current filter (never stale). Do NOT re-fire —
  // hx-sync="this:replace" would abort the in-flight request. */
  await asof.fill('2026-03-31');
  await asof.blur();
  await expect(page.locator('a.report-csv-link')).toHaveAttribute('href', /asof=2026-03-31/, { timeout: 15000 });
  await expect(page.locator('#report-results table.report-table')).toBeVisible();
  await expect(page).toHaveURL(/asof=2026-03-31/); // hx-push-url synced the query (persistence)

  // Persistence: a full reload replays the pushed filter (the report's ONLY persistence is
  // the query string; the server re-reads it and re-renders the same as-of).
  await page.reload();
  await expect(page).toHaveURL(/asof=2026-03-31/);
  await expect(page.locator('a.report-csv-link')).toHaveAttribute('href', /asof=2026-03-31/);

  // --- latest-wins: fire TWO as-of changes in quick succession (an intermediate date, then
  // the final one). hx-sync="this:replace" aborts the in-flight intermediate request when
  // the final change fires, so the results + URL + CSV href all settle on the LAST change,
  // never on a stale out-of-order intermediate response. ---
  const asof2 = page.locator('nav.app-subnav form.report-params [name="asof"]');
  await asof2.fill('2026-05-15'); // intermediate (should be aborted / never the final state)
  await asof2.blur();
  await asof2.fill('2026-06-30'); // final (wins)
  await asof2.blur();
  // The CSV href lives INSIDE the swapped #report-results, so it reflects whichever response
  // LANDED LAST — the discriminating check: it settles on the FINAL date (the aborted
  // intermediate never wins).
  await expect(page.locator('a.report-csv-link')).toHaveAttribute('href', /asof=2026-06-30/, { timeout: 15000 });
  await expect(page.locator('a.report-csv-link')).not.toHaveAttribute('href', /asof=2026-05-15/);
  await expect(page).toHaveURL(/asof=2026-06-30/); // hx-push-url, latest-wins
});

// p26.95 MISSING-RATE INLINE ERROR: converting a report to a target currency with NO
// exchange rate on file must show a CLEAN inline error in the results region, NOT a
// 500 (under apply-on-change a 5xx leaves htmx swapping nothing — a silent no-op). We
// first SEED a balanced USD posting (the fresh worker db has no rates), so converting
// to MXN (a seeded currency) needs a USD->MXN rate that does not exist and the report
// genuinely errors. The `change` on the currency <select> fires the hx-get; the swapped
// #report-results shows the error message and drops the table + CSV link. Strict CSP =>
// only locator/response waits.
test('reports: converting to a rate-less currency shows an inline error, not a 500', async ({
  page,
  server,
}) => {
  await login(page, server);

  // Seed a balanced USD transfer so the trial balance has non-zero USD figures to
  // convert (an EMPTY report converts nothing and would never hit the rate lookup).
  await createAsset(page, 'NoRate Checking');
  await createAsset(page, 'NoRate Savings');
  await page.goto('/transactions/new');
  await expect(page.locator('form#txn-form')).toBeVisible();
  await selectTxnAccount(page.locator('#txn-main-account'), 'NoRate Checking');
  await selectTxnAccount(page.locator('#txn-account-0'), 'NoRate Savings');
  await page.locator('#txn-amount-0').fill('42.00');
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

  // Open at the DEFAULT as-of (today) so the just-posted txn is in range and the report
  // has USD figures.
  await page.goto(`${TB}?scope=1`);
  await expect(page.locator('#report-results table.report-table')).toBeVisible();

  const ccy = page.locator('nav.app-subnav form.report-params select[name="currency"]');
  await expect(ccy).toBeVisible();

  // Watch the swap response: it must be 200 (a clean inline error), never 5xx.
  const respP = page.waitForResponse(
    (r) => r.url().includes('/reports/trial_balance') && r.request().method() === 'GET',
  );
  await ccy.selectOption('MXN'); // `change` fires the hx-get
  const resp = await respP;
  expect(resp.status()).toBe(200);

  // The results region now shows the inline no-rate error and NO table / CSV export.
  await expect(page.locator('#report-results .report-error')).toBeVisible({ timeout: 15000 });
  await expect(page.locator('#report-results a.report-csv-link')).toHaveCount(0);
  await expect(page.locator('#report-results table.report-table')).toHaveCount(0);
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

  // At least one group SECTION with a card grid renders (admin sees every group). p28.12
  // rewrote the index from a flat <ul> link list to the shared card grid (the same
  // .hub-section / .hub-cards markup the "All" landing uses), so both pages match.
  await expect(page.locator('section.hub-section').first()).toBeVisible();
  await expect(page.locator('ul.hub-cards').first()).toBeVisible();

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

  // p26.86: ALL reports now render their filters in the SECOND-LEVEL nav bar — the
  // income statement (a previously-inline dense report) included. Its filter form is in
  // the subnav, NOT the page body, and there is no visible "Filters" legend heading.
  await expect(page.locator('nav.app-subnav form.report-params')).toBeVisible();
  await expect(page.locator('main#main form.report-params')).toHaveCount(0);
  await expect(page.locator('form.report-params legend')).toHaveCount(0);

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
  expect(body).toContain(',');
});

// The income statement at TOTAL granularity (granularity=total / none) drops the single
// "Period" column and instead shows three FUNCTIONAL columns -- Admin | Fundraising |
// Program -- plus Total, mirroring the functional-expenses statement. Assert the CSV
// header carries those four column labels and no "Period" column, and that a data row
// foots (Admin + Fundraising + Program == Total) on the CSV.
test('reports: income statement at total granularity shows Admin/Fundraising/Program functional columns', async ({
  page,
  server,
}) => {
  await login(page, server);

  await page.goto(`${IS}?scope=1&from=2025-01-01&to=2026-06-30&granularity=total&currency=USD`);

  const table = page.locator('table.report-table');
  await expect(table).toBeVisible();

  // The functional column headers are present; the Period header is gone.
  const headerText = await page.locator('table.report-table thead').innerText();
  expect(headerText).toContain('Admin');
  expect(headerText).toContain('Fundraising');
  expect(headerText).toContain('Program');
  expect(headerText).toContain('Total');
  expect(headerText).not.toContain('Period');

  // CSV: header shape + a foot check on the "Total expenses" section-total row.
  const resp = await page.request.get(
    `${IS}.csv?scope=1&from=2025-01-01&to=2026-06-30&granularity=total&currency=USD`,
  );
  expect(resp.status()).toBe(200);
  const rows = (await resp.text()).trim().split('\n');
  const header = rows.find((r) => r.startsWith('Line,'));
  expect(header).toBe('Line,Admin,Fundraising,Program,Total');
  const expTotal = rows.find((r) => r.startsWith('Total expenses,'));
  expect(expTotal).toBeTruthy();
  const [, admin, fundraising, program, total] = expTotal
    .split(',')
    .map((c) => Math.round(parseFloat(c || '0') * 100));
  expect(admin + fundraising + program).toBe(total);
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
  await selectTxnAccount(page.locator('#txn-main-account'), 'Drill Checking');
  await selectTxnAccount(page.locator('#txn-account-0'), 'Drill Savings');
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
  await selectTxnAccount(page.locator('#txn-main-account'), 'TB Nest Counter');
  await selectTxnAccount(page.locator('#txn-account-0'), 'TB Nest Parent.TB Nest Child');
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

  // p26.55: clicking the parent's NAME (its first cell, not just the little arrow)
  // toggles the subtree. Click the parent row's name cell -> the child hides; click
  // again -> it reappears. This is the "click a parent's name collapses/expands its
  // subtree" case.
  const parentNameCell = parentRow.locator('td.tree-name-cell').first();
  await expect(parentNameCell).toBeVisible();
  await parentNameCell.click();
  await expect(childRow).toBeHidden();
  await parentNameCell.click();
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
  await selectTxnAccount(page.locator('#txn-main-account'), 'Ledger Savings');
  await selectTxnAccount(page.locator('#txn-account-0'), 'Ledger Checking');
  await page.locator('#txn-amount-0').fill('55.00');
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

  // --- open the account ledger; the account SELECTOR (report-specific control) is present ---
  await page.goto(AL);
  await expect(page.locator('form.report-params')).toBeVisible();
  await expect(page.locator('select.report-scope-select[name="scope"]')).toBeVisible();
  const acctSelect = page.locator('select.report-account-select[name="account"]');
  await expect(acctSelect).toBeVisible();

  // p26.90: pick the account + a wide period. The report AUTO-APPLIES on change now; the
  // account option value is its id, resolved via the account-ledger options — navigate the
  // equivalent GET (the no-JS round trip) so this render test stays deterministic. The
  // default To is today, which includes the just-posted (today-dated) txn.
  const acctVal = await acctSelect.locator('option', { hasText: 'Ledger Checking' }).getAttribute('value');
  await page.goto(`${AL}?account=${acctVal}&from=2025-01-01`);

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
  expect(body).toContain(',');
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
  await selectTxnAccount(page.locator('#txn-main-account'), 'FE Bank');
  await selectTxnAccount(page.locator('#txn-account-0'), 'FE Rent');
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
  expect(body).toContain(',');
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
  expect(body).toContain(',');
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

// createChildExpenseAccount makes a leaf EXPENSE account nested under an existing expense
// PARENT (parentName), so the program statement's collapsible account tree has a
// placeholder-parent subtotal (the parent) over an indented leaf (this child). The parent
// becomes a placeholder once it has a child, so its own row is a roll-up subtotal.
async function createChildExpenseAccount(page, name, parentName) {
  await openNewAccount(page);
  await page.locator('#af-type').selectOption('expense');
  await expect(page.locator('#af-func')).toBeVisible();
  await page.locator('#af-name-en').fill(name);
  await expect(async () => {
    await page.locator('#af-parent').selectOption({ label: parentName });
    await expect(page.locator('#af-parent')).not.toHaveValue('0');
  }).toPass({ timeout: 5000 });
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
  await selectTxnAccount(page.locator('#txn-main-account'), 'Stmt Cash E2E');
  await selectTxnAccount(page.locator('#txn-account-0'), 'Stmt Gift E2E');
  await page.locator('#txn-amount-0').fill('-100.00');
  await page.locator('#txn-fund-0').selectOption({ label: 'Stmt Fund E2E' });
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

  // Capital-asset PURCHASE applying the fund (the NON-EXPENSE application): DR Stmt
  // Building 40.00 (fund), CR Stmt Cash 40.00 (fund). Both asset splits, fund-tagged.
  await page.goto('/transactions/new');
  await expect(page.locator('form#txn-form')).toBeVisible();
  // Header = Stmt Cash (-40 residual, fund derived); body = Stmt Building +40 fund.
  await selectTxnAccount(page.locator('#txn-main-account'), 'Stmt Cash E2E');
  await selectTxnAccount(page.locator('#txn-account-0'), 'Stmt Building E2E');
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
  // p26.90: the report AUTO-APPLIES on change; navigate the equivalent GET (no-JS round
  // trip) for a deterministic render test. The fund option value is its id.
  const fundVal = await fundSelect.locator('option', { hasText: 'Stmt Fund E2E' }).getAttribute('value');
  await page.goto(`${FA}?scope=1&fund=${fundVal}&from=2025-01-01&to=2030-12-31`);

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
  expect(body).toContain(',');
});

// FUND STATEMENT (Report A): a full-detail, all-time LINE statement for ONE fund,
// grouped by account, with Date/Description/Memo/Amount per line + a per-account
// subtotal. Fund selector, NO date range. This spec seeds a restricted fund, an asset +
// a revenue account, and a receipt whose split carries a DESCRIPTION and a MEMO, then
// opens the report, picks the fund, and confirms the by-account line detail (the
// description and memo both render) + the account subtotal + the CSV export.
test('reports: open the fund statement, pick a fund, see by-account line detail with description + memo, CSV returns', async ({
  page,
  server,
}) => {
  await login(page, server);

  await createFund(page, 'FStmt Fund E2E', 'FStmt Donor E2E');
  await createAsset(page, 'FStmt Cash E2E');
  await createRevenueAccount(page, 'FStmt Gift E2E');

  // Receipt INTO the fund: DR FStmt Cash 100.00 (fund), CR FStmt Gift 100.00 (fund). The
  // body split carries a per-split DESCRIPTION and a MEMO -- the two per-line columns the
  // report shows distinctly.
  await page.goto('/transactions/new');
  await expect(page.locator('form#txn-form')).toBeVisible();
  await selectTxnAccount(page.locator('#txn-main-account'), 'FStmt Cash E2E');
  await selectTxnAccount(page.locator('#txn-account-0'), 'FStmt Gift E2E');
  await page.locator('#txn-amount-0').fill('-100.00');
  await page.locator('#txn-fund-0').selectOption({ label: 'FStmt Fund E2E' });
  await page.locator('#txn-desc-0').fill('Annual gala pledge');
  await page.locator('#txn-memo-0').fill('check 1042');
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

  // --- open the report; the report-specific FUND selector is present, and there is NO
  // date-range input (this report spans all data) ---
  await page.goto(`${FS}?scope=1`);
  await expect(page.locator('form.report-params')).toBeVisible();
  const fundSelect = page.locator('select.report-fund-select[name="fund"]');
  await expect(fundSelect).toBeVisible();
  await expect(page.locator('form.report-params [name="from"]')).toHaveCount(0);
  await expect(page.locator('form.report-params [name="to"]')).toHaveCount(0);

  // --- pick the fund -> the by-account line statement (navigate the auto-apply GET) ---
  const fundVal = await fundSelect
    .locator('option', { hasText: 'FStmt Fund E2E' })
    .getAttribute('value');
  await page.goto(`${FS}?scope=1&fund=${fundVal}`);

  const table = page.locator('table.report-table');
  await expect(table).toBeVisible();
  // Grouped by account: the section headers are the two touched accounts.
  await expect(table).toContainText('FStmt Cash E2E');
  await expect(table).toContainText('FStmt Gift E2E');
  // The per-line Description and Memo both render (the point of the report).
  await expect(table).toContainText('Annual gala pledge');
  await expect(table).toContainText('check 1042');
  // The account subtotal label is present (each account section closes with one).
  await expect(table).toContainText('Account subtotal');
  // The line date links to the txn editor (Cell.TxnID -> /transactions/{id}/edit).
  await expect(
    table.locator('a[href*="/transactions/"][href*="/edit"]').first(),
  ).toBeVisible();

  // --- the CSV export link is present and the endpoint returns text/csv ---
  await expect(page.locator('a.report-csv-link')).toBeVisible();
  const csvHref = await page.locator('a.report-csv-link').getAttribute('href');
  const resp = await page.request.get(csvHref);
  expect(resp.status()).toBe(200);
  expect(resp.headers()['content-type']).toContain('text/csv');
  const body = await resp.text();
  expect(body).toContain('Annual gala pledge');
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
  await selectTxnAccount(page.locator('#txn-main-account'), 'Restr Cash E2E');
  await selectTxnAccount(page.locator('#txn-account-0'), 'Restr Gift E2E');
  await page.locator('#txn-amount-0').fill('-100.00');
  await page.locator('#txn-fund-0').selectOption({ label: 'Restr Fund E2E' });
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

  // An UNRESTRICTED expense (no fund): DR Restr Cost 30.00, CR Restr Cash 30.00. Its
  // functional class + program prefill from the form defaults (expense splits need both).
  await page.goto('/transactions/new');
  await expect(page.locator('form#txn-form')).toBeVisible();
  // Header = Restr Cash (-30 residual); body = Restr Cost +30 (expense, class/program default).
  await selectTxnAccount(page.locator('#txn-main-account'), 'Restr Cash E2E');
  await selectTxnAccount(page.locator('#txn-account-0'), 'Restr Cost E2E');
  await page.locator('#txn-amount-0').fill('30.00');
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

  // --- open the statement over a wide period, root scope ---
  // p26.90: the report AUTO-APPLIES on change; navigate the equivalent GET (no-JS round
  // trip) with the wide period for a deterministic render test.
  await page.goto(`${ABR}?scope=1&from=2025-01-01&to=2030-12-31`);
  await expect(page.locator('form.report-params')).toBeVisible();
  await expect(page.locator('select.report-scope-select[name="scope"]')).toBeVisible();
  await expect(page.locator('form.report-params [name="from"]')).toBeVisible();

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
  expect(abrBody).toContain(',');
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

test('reports: open the program statement MATRIX, collapse a program COLUMN group by clicking its header, pick a single program, CSV returns', async ({
  page,
  server,
}) => {
  await login(page, server);

  // --- seed: a child program, a cash asset, a NESTED expense account (a placeholder parent
  // over a leaf, so the collapsible account tree has a subtotal), and a program-tagged
  // expense on the leaf ---
  await createProgram(page, 'PS Outreach E2E');
  await createAsset(page, 'PS Cash E2E');
  await createExpenseAccount(page, 'PS Expenses E2E'); // becomes a placeholder parent below
  await createChildExpenseAccount(page, 'PS Cost E2E', 'PS Expenses E2E'); // leaf under it

  // An expense: DR PS Cost 80.00 (fund none), CR PS Cash 80.00. The expense split needs a
  // program (rule 7 R/E dimension, D24); assign the seeded child program so the comparative
  // view has a non-empty program column.
  await page.goto('/transactions/new');
  await expect(page.locator('form#txn-form')).toBeVisible();
  await selectTxnAccount(page.locator('#txn-main-account'), 'PS Cash E2E');
  // The child leaf's split-option label is its DOTTED PATH (p26.1: parent.child).
  await selectTxnAccount(page.locator('#txn-account-0'), 'PS Expenses E2E.PS Cost E2E');
  // p26.41: the combined program/class control -- pick the program node by label (its value
  // is p:<id>, which decodes to program=PS Outreach + class=program on this expense row).
  await expect(page.locator('#txn-progclass-0')).toBeVisible();
  await page.locator('#txn-progclass-0').selectOption({ label: 'PS Outreach E2E' });
  await page.locator('#txn-amount-0').fill('80.00');
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

  // --- MATRIX view (p31-10a): account ROWS x (functional-class/program) COLUMNS. The
  // matrix is single-currency (converted), so currency is mandatory (USD). ---
  await page.goto(`${PS}?scope=1&from=2025-01-01&to=2030-12-31&currency=USD`);
  await expect(page.locator('form.report-params')).toBeVisible();
  await expect(page.locator('select.report-scope-select[name="scope"]')).toBeVisible();
  // The report-specific PROGRAM selector (default is EMPTY == all programs).
  const progSelect = page.locator('select.report-program-select[name="program"]');
  await expect(progSelect).toBeVisible();
  // The period FROM/TO controls are present -- plain text inputs (never input[type=date]).
  await expect(page.locator('form.report-params [name="from"]')).toBeVisible();
  await expect(page.locator('form.report-params [name="to"]')).toBeVisible();

  const table = page.locator('table.report-table');
  await expect(table).toBeVisible();
  // p31-10a: a STACKED (two-row) header — the top group row (.report-header-groups) spans the
  // functional-class / Program-services super-columns; the leaf row carries each column head.
  await expect(page.locator('table.report-table tr.report-header-groups')).toBeVisible();
  // The programs are now COLUMN headers (proper nouns, verbatim): "General" (the seeded root)
  // and its child "PS Outreach E2E" — NOT row headers.
  const generalHead = page.locator('thead th[data-program-group="1"]', { hasText: 'General' }).first();
  const childHead = page.locator('thead th[data-program]', { hasText: 'PS Outreach E2E' }).first();
  await expect(generalHead).toBeVisible();
  await expect(childHead).toBeVisible();
  // The child column declares data-program-parent (its ancestor chain includes General); the
  // parent (General) column declares data-program-group="1" (collapsible).
  await expect(childHead).toHaveAttribute('data-program-parent', /\d+/);
  // The section labels (localized en) + the net line. The seed posts only an EXPENSE (no
  // revenue receipt), so the Expenses section + Net line are present.
  await expect(table).toContainText('Expenses');
  await expect(table).toContainText('Net');
  // The expense account row is present.
  await expect(table).toContainText('PS Cost E2E');
  // A NET (report-total) row is present.
  await expect(page.locator('table.report-table tr.report-total')).not.toHaveCount(0);

  // --- p31-10b: CLICK-TO-COLLAPSE a program COLUMN group. Clicking the "General" parent
  // header hides its DESCENDANT program columns (here: the child "PS Outreach E2E"), leaving
  // General's rolled-up column visible; clicking again expands. colcollapse.js wires it (the
  // ▸/▾ affordance + event delegation, no server round trip). ---
  const childProgID = await childHead.getAttribute('data-program');
  // The whole child column (header + body <td>s) shares data-program; capture the set.
  const childCells = page.locator(`[data-program="${childProgID}"]`);
  await expect(childCells.first()).toBeVisible(); // expanded: the child column shows
  // A disclosure affordance is injected into the collapsible parent header.
  await expect(generalHead.locator('.col-toggle')).toHaveCount(1);

  // Click the General header -> its descendant (PS Outreach E2E) column hides.
  await generalHead.click();
  await expect(childHead).toHaveClass(/col-hidden/); // child column header hidden
  await expect(generalHead).not.toHaveClass(/col-hidden/); // parent rollup column stays
  // The parent's disclosure now reads collapsed (▸).
  await expect(generalHead.locator('.col-toggle')).toHaveClass(/is-collapsed/);

  // Click again -> the child column re-appears (expanded).
  await generalHead.click();
  await expect(childHead).not.toHaveClass(/col-hidden/);
  await expect(generalHead.locator('.col-toggle')).not.toHaveClass(/is-collapsed/);

  // --- pick the SINGLE program -> the statement SCOPED to that program's subtree ---
  // p26.90: the report AUTO-APPLIES on change; navigate the equivalent GET (no-JS round trip)
  // for a deterministic render test. The program option value is its id.
  const progVal = await progSelect.locator('option', { hasText: 'PS Outreach E2E' }).getAttribute('value');
  await page.goto(`${PS}?scope=1&program=${progVal}&from=2025-01-01&to=2030-12-31&currency=USD`);

  const single = page.locator('table.report-table');
  await expect(single).toBeVisible();
  await expect(single).toContainText('PS Cost E2E');
  await expect(single).toContainText('Net');

  // The currency selector is present and required (the matrix is single-currency converted).
  const ccy = page.locator('form.report-params select[name="currency"]');
  await expect(ccy).toBeVisible();
  await expect(ccy).toHaveValue('USD');

  // p26.54 sticky headers: the table scrolls inside .report-scroll (present on the page).
  await expect(page.locator('.report-scroll')).toBeVisible();

  // --- the CSV export link is present and the endpoint returns text/csv ---
  await expect(page.locator('a.report-csv-link')).toBeVisible();
  const csvHref = await page.locator('a.report-csv-link').getAttribute('href');
  const resp = await page.request.get(csvHref);
  expect(resp.status()).toBe(200);
  expect(resp.headers()['content-type']).toContain('text/csv');
  const body = await resp.text();
  expect(body).toContain(',');
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
  await selectTxnAccount(page.locator('#txn-main-account'), 'F990 Bank E2E');
  await selectTxnAccount(page.locator('#txn-account-0'), 'F990 Gift E2E');
  await page.locator('#txn-amount-0').fill('-90.00');
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

  // An expense: DR F990 Rent 30.00 (class management, program prefilled), CR F990 Bank 30.00.
  await page.goto('/transactions/new');
  await expect(page.locator('form#txn-form')).toBeVisible();
  // Header = F990 Bank (-30 residual); body = F990 Rent +30 (expense, class management).
  await selectTxnAccount(page.locator('#txn-main-account'), 'F990 Bank E2E');
  await selectTxnAccount(page.locator('#txn-account-0'), 'F990 Rent E2E');
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

  // Every section renders an explicit Unmapped bucket (never drop rows). At least one is
  // present (Part VIII's is non-empty here: the F990 Gift receipt has no 990 code). The
  // bucket label is "Unmapped — assign a 990 line" (reports.form_990.unmapped).
  await expect(table).toContainText('assign a 990 line');

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
  expect(body990).toContain(',');
});

const BV = '/reports/budget_variance';

// p30.9 BUDGET-VARIANCE REDESIGN: the report is a MONTHLY pivot with qualified row labels
// and a MEASURE TOGGLE (Budgeted / Actual / Variance) that switches which measure the grid
// shows INSTANTLY, client-side, with NO server round-trip. This spec SEEDS a budget plan
// with a projected revenue split + a matching posted revenue transaction (so the grid has
// both a budgeted and an actual figure to toggle between), opens the report, and:
//   - confirms the monthly grid renders with the qualified columns (Account/Fund/Program/
//     Currency + month columns + Total) and the three-button measure toggle;
//   - the default measure is Variance (its button pressed, the table's data-measure attr);
//   - clicking "Actual" flips the table's data-measure to "actual" with NO network request
//     (the whole point: all three values are already in the page, the JS only shows/hides).
//
// This test MUTATES (creates an account, a plan+split, a txn); names are unique so it never
// collides with sibling specs sharing the worker db, and it only ADDS rows. Strict CSP
// (script-src 'self') => NO page.waitForFunction; only locator/URL/attribute waits.
test('reports: budget variance renders the monthly grid + toggles the measure with no round trip', async ({
  page,
  server,
}) => {
  const suffix = Math.random().toString(36).slice(2, 8);
  const revName = `BV Gift E2E ${suffix}`;
  const cashName = `BV Cash E2E ${suffix}`;
  const planName = `BV Plan E2E ${suffix}`;

  await login(page, server);

  // --- seed: a revenue leaf + a cash asset (the receipt's two legs) ---
  await createRevenueAccount(page, revName);
  await createAsset(page, cashName);

  // --- a budget plan with ONE projected revenue split for that account (2026-02) ---
  await page.goto('/budget-plans');
  await page.locator('#new-budget-plan').click();
  await page.locator('#bpf-name').fill(planName);
  await page.locator('#budget-plan-create').click();
  await page.waitForURL('**/budget-plans/*');
  const planPath = new URL(page.url()).pathname;
  await page.locator('#bs-account-0').selectOption({ label: revName });
  await page.locator('#bs-date-0').fill('2026-02-15');
  await page.locator('#bs-amount-0').fill('500.00');
  await page.locator('#bs-program-0').selectOption({ label: 'General' }); // R/E needs a program
  const planReload = page.waitForResponse(
    (r) => new URL(r.url()).pathname === planPath && r.request().method() === 'GET',
  );
  await page.locator('#budget-save-splits').click();
  await planReload;

  // --- a matching posted revenue receipt (the ACTUAL side) in the plan span ---
  await page.goto('/transactions/new');
  await expect(page.locator('form#txn-form')).toBeVisible();
  await selectTxnAccount(page.locator('#txn-main-account'), cashName);
  await selectTxnAccount(page.locator('#txn-account-0'), revName);
  await page.locator('#txn-amount-0').fill('-320.00'); // a revenue credit (net-debit negative)
  await page.locator('#txn-date').fill('2026-02-20');
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));

  // --- open the budget variance report on the seeded plan over its span ---
  await page.goto(BV);
  const budgetSel = page.locator('select.report-budget-select[name="budget"]');
  await expect(budgetSel).toBeVisible();
  const planVal = await budgetSel.locator('option', { hasText: planName }).getAttribute('value');
  await page.goto(`${BV}?scope=1&budget=${planVal}&from=2026-01-01&to=2026-03-31`);

  // The monthly-pivot table renders with the qualified columns + month columns + Total.
  const table = page.locator('table.report-table.bv-table');
  await expect(table).toBeVisible();
  const headers = page.locator('table.report-table thead th');
  // Account | Fund | Program | Currency | >=3 months | Total => >= 6 columns (a flat
  // per-period layout could not produce the qualified + monthly grid).
  expect(await headers.count()).toBeGreaterThanOrEqual(6);
  await expect(page.locator('table.report-table thead')).toContainText('Total');
  // A qualified row label: the account name in the first column + its fund + program.
  await expect(table).toContainText(revName);
  await expect(table).toContainText('Unrestricted');
  await expect(table).toContainText('General');

  // --- the MEASURE TOGGLE: three buttons; Variance pressed by default (also the table attr) ---
  const toggle = page.locator('.bv-measure-toggle');
  await expect(toggle).toBeVisible();
  await expect(toggle.locator('.bv-measure-btn')).toHaveCount(3);
  await expect(table).toHaveAttribute('data-measure', 'variance');
  await expect(page.locator('.bv-measure-btn[data-measure="variance"]')).toHaveAttribute('aria-pressed', 'true');

  // --- click "Actual": the displayed measure switches with NO network request. We record any
  // request to the report endpoint during the click; the table's data-measure attribute flips
  // purely client-side (all three values are already in the page — the JS only shows/hides). ---
  let sawRequest = false;
  const onReq = (req) => {
    if (req.url().includes('/reports/budget_variance')) sawRequest = true;
  };
  page.on('request', onReq);
  await page.locator('.bv-measure-btn[data-measure="actual"]').click();
  await expect(table).toHaveAttribute('data-measure', 'actual'); // switched (client-side)
  await expect(page.locator('.bv-measure-btn[data-measure="actual"]')).toHaveAttribute('aria-pressed', 'true');
  await expect(page.locator('.bv-measure-btn[data-measure="variance"]')).toHaveAttribute('aria-pressed', 'false');
  page.off('request', onReq);
  expect(sawRequest).toBe(false); // no server round-trip: the switch is pure JS show/hide
});
