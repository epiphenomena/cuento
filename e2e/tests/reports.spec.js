// @ts-check
// Functional test of the p15.1 reports FRAMEWORK. It drives the REAL report page
// served by `cuento serve -dev` against the worker's fresh migrated db with the
// seeded admin (is_admin => reaches every report without a grant). In ONE test (one
// login, to stay under the login rate limiter shared per worker) it:
//   - opens the framework smoke report (/reports/_smoke),
//   - asserts the shared PARAMS FORM is present, INCLUDING the subsidiary SCOPE
//     selector (rendered on every report, D18) and the report table,
//   - fetches the CSV endpoint and asserts it returns CSV.
//
// TEST-ISOLATION (the worker-scoped `server` shares one db across a worker's specs):
// this spec is READ-ONLY -- it opens a report and downloads its CSV, mutating
// nothing durable, so it never leaks state into sibling specs. Selectors are
// language-independent (ids, name attrs, marker classes) so a mid-run locale change
// elsewhere could never break them. No page.waitForFunction (strict CSP:
// script-src 'self', no unsafe-eval) -- only locator/URL/response waits and a plain
// page.request fetch for the CSV.
//
// The smoke report is a THROWAWAY placeholder proving the framework end to end; when
// p15.3+ land real reports this spec still validates the framework (scope selector +
// table + CSV) against whichever report id it targets.

const { test, expect } = require('../fixtures');

const SMOKE = '/reports/_smoke';

async function login(page, server) {
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');
}

test('reports: open the smoke report, see the params form + scope selector + table, CSV downloads', async ({
  page,
  server,
}) => {
  await login(page, server);

  // --- open the report HTML page ---
  await page.goto(SMOKE);

  // The shared params form is present.
  await expect(page.locator('form.report-params')).toBeVisible();

  // The subsidiary SCOPE selector is present on the report (the p15.1 invariant:
  // every report is scoped). It is a <select name="scope"> with the marker class,
  // and lists at least the root subsidiary (a migrated db always has one).
  const scope = page.locator('select.report-scope-select[name="scope"]');
  await expect(scope).toBeVisible();
  await expect(scope.locator('option')).not.toHaveCount(0);

  // The report TABLE renders (typed cells / rows come from the Table the report
  // returns; with no seeded ledger data it still renders the table shell + headers).
  await expect(page.locator('table.report-table')).toBeVisible();
  await expect(page.locator('table.report-table thead th')).not.toHaveCount(0);

  // The CSV export link is present and points at the .csv endpoint.
  const csvLink = page.locator('a.report-csv-link');
  await expect(csvLink).toBeVisible();

  // --- fetch the CSV endpoint and assert it returns CSV ---
  const resp = await page.request.get(`${SMOKE}.csv`);
  expect(resp.status()).toBe(200);
  expect(resp.headers()['content-type']).toContain('text/csv');
  // A CSV body has at least the localized header row (comma-separated columns).
  const body = await resp.text();
  expect(body.split('\n')[0]).toContain(',');
});
