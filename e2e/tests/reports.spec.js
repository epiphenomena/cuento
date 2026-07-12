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

const TB = '/reports/trial_balance';

async function login(page, server) {
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');
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
