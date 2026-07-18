// @ts-check
// p18.4 hardening sweep — the CSP-clean click-through. The strict Content-Security-
// Policy (internal/web/middleware.go) is `script-src 'self'; style-src 'self'` with
// NO 'unsafe-inline': an inline <script>, an inline event handler (onclick=...), an
// inline <style> block, or a `style="..."` attribute would all be REFUSED by the
// browser and logged as a Content-Security-Policy violation. Chromium reports these
// on the console ("Refused to apply inline style ..." / "Refused to execute inline
// script ...") and, for blocked scripts, may also raise a SecurityPolicyViolation
// event. This spec logs in and walks EVERY main -dev page built across phases
// 10–18 (plus the -dev-only styleguide gallery and an htmx-swapped partial, the two
// likeliest places a stray inline style/handler hides), asserting ZERO CSP
// violations surface. It proves the whole UI honors the boring-frontend rule
// (AGENTS rule 12): no inline script/style slipped in anywhere.
//
// Detector design: page.on('console') + page.on('pageerror') survive full
// navigations (a window-array via addInitScript resets on every load, so it is not
// relied on alone). We match the browser's own CSP-refusal wording plus the
// SecurityPolicyViolation error, so a real leak on any visited page is captured no
// matter which page raised it.

const { test, expect } = require('../fixtures');

// cspViolationText matches Chromium's CSP-refusal console messages and the
// SecurityPolicyViolation error text. Kept broad on purpose: any of these strings
// means the strict CSP blocked an inline resource — exactly what must never happen.
const cspViolationText =
  /Content Security Policy|Refused to (execute|apply|load)|SecurityPolicyViolation/i;

// attachCSPDetector wires console + pageerror listeners that push any CSP-violation
// message into `violations`. It is installed before navigation so nothing is missed.
function attachCSPDetector(page, violations) {
  page.on('console', (msg) => {
    const text = msg.text();
    if (cspViolationText.test(text)) violations.push(`console: ${text}`);
  });
  page.on('pageerror', (err) => {
    const text = String(err);
    if (cspViolationText.test(text)) violations.push(`pageerror: ${text}`);
  });
}

async function login(page, server) {
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');
}

test.describe('CSP console clean across the -dev UI', () => {
  test('main pages, styleguide, and an htmx swap raise no CSP violation', async ({ page, server }) => {
    /** @type {string[]} */
    const violations = [];
    attachCSPDetector(page, violations);

    // Login first (the admin persona sees every nav section).
    await login(page, server);

    // Walk the main app pages an admin reaches (the nav targets built across phases
    // 10–18) plus the -dev-only styleguide gallery (the component showcase, the most
    // inline-style-prone page). Each is a full navigation; the detector persists
    // across all of them. A trailing networkidle wait lets any deferred inline
    // resource attempt (and thus any refusal) surface before we move on.
    const pages = [
      '/',
      '/accounts',
      '/funds',
      '/programs',
      '/reconciliations',
      // Budget-plan management (p27.2/p27.3): the plan list. The plan detail loads
      // external ES modules (budgetgrid/budgetcadence) — visiting exercises script-src,
      // and the page must carry no inline style/script under the strict CSP. (The old
      // schedule-based /budgets + /schedules pages were retired in p27.3.)
      '/budget-plans',
      '/import',
      '/reports',
      // A concrete report DETAIL: report templates align columns/totals, a classic
      // hiding place for a stray inline style. trial_balance is the id reports.spec.js
      // navigates (known-good) and it renders on the empty e2e db.
      '/reports/trial_balance',
      // The transaction EDITOR: the daily-use split grid with conditional
      // program/class selects (p12.2) — the most inline-style-prone page in the app.
      // A plain GET renders the full grid, so visiting is enough to exercise style-src.
      '/transactions/new',
      '/settings',
      '/admin',
      '/admin/users',
      '/admin/subsidiaries',
      '/admin/currencies',
      '/admin/org',
      '/admin/rates',
      '/admin/ops',
      '/styleguide',
    ];
    for (const path of pages) {
      await page.goto(path);
      await expect(page.locator('body')).toBeVisible();
      // Give htmx-boosted links / deferred styles a beat to attempt loading so any
      // CSP refusal is logged before the assertion.
      await page.waitForLoadState('networkidle');
    }

    // Trigger an htmx SWAP: the chart-of-accounts "merge" form arrives as a partial
    // swapped into #account-form (not a full navigation). A swapped-in fragment can
    // carry inline style a static page load never renders, so exercising one closes
    // that gap. (New/Edit moved to their own full pages in p26.7; Merge stays inline
    // and is the remaining inline swap on this page.) The form settles (fixtures
    // stamps e2e-settled) with no CSP violation if the partial is inline-free.
    await page.goto('/accounts');
    await page.getByRole('button', { name: /merge/i }).click();
    await expect(page.locator('form#account-form.e2e-settled')).toBeVisible();

    expect(violations, `CSP violations detected:\n${violations.join('\n')}`).toEqual([]);
  });
});
