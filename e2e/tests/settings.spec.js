// @ts-check
// Functional test of the p13.1 my-settings page. It drives the REAL /settings page
// served by `cuento serve -dev` against a fresh migrated db with a seeded admin. It
// logs in, opens Settings, switches the UI language to Spanish (and the amount
// display to DR/CR), saves, and asserts the change took effect on a SUBSEQUENT page:
// the nav chrome renders in Spanish. This proves the settings POST persists and the
// next render honors the new locale end to end.
//
// The settings form is a FULL-PAGE form, not an inline htmx swap: on save it
// 303-redirects to /settings?saved=1 (a DIFFERENT URL than the current /settings),
// so `page.waitForURL('**/settings?*')` is a real wait (never the no-op
// same-url pattern the shared helpers guard against). No page.waitForFunction is
// used (strict CSP: script-src 'self', no unsafe-eval) -- only locator/URL waits.
//
// Selectors come from settings.tmpl (#setting-locale, #setting-display) and
// base.tmpl (nav.app-nav).
//
// TEST-ISOLATION NOTE: the `server` fixture is WORKER-scoped -- one cuento server +
// one db shared across all tests in a worker (fixtures.js). Unlike the theme (a
// per-context cookie, so shell.spec's toggle stays local), a locale/display change
// is a DURABLE change to the SHARED admin user, so it would poison sibling tests in
// the same worker (e.g. txn-editor's English "New account" selector fails when the
// admin is left in es). So this test RESTORES the admin's settings to the seeded
// defaults (en / signed) before finishing, leaving the shared user as it found it.

const { test, expect } = require('../fixtures');

async function login(page, server) {
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');
}

test.describe('my settings', () => {
  test('switching locale to Spanish takes effect on the next render', async ({ page, server }) => {
    await login(page, server);

    // Open Settings (English by default for the seeded admin).
    await page.goto('/settings');
    await expect(page.locator('main#main h1')).toHaveText('Settings');

    // Switch the UI language to Spanish and the amount display to DR/CR.
    await page.locator('#setting-locale').selectOption('es');
    await page.locator('#setting-display').selectOption('dr_cr');

    // Save: full-page POST -> 303 to /settings?saved=1 (a real URL change).
    await page.getByRole('button', { name: /^save$/i }).click();
    await page.waitForURL('**/settings?**');

    // The redirected GET re-renders in Spanish: the saved notice and the es title.
    await expect(page.locator('main#main h1')).toHaveText('Ajustes');
    await expect(page.locator('html')).toHaveAttribute('lang', 'es');

    // The change is durable and applies to the CHROME on a subsequent, different
    // page: the nav renders in Spanish (Ajustes / Cuentas), proving locale is read
    // per-render from the stored setting.
    await page.goto('/accounts');
    const nav = page.locator('nav.app-nav');
    await expect(nav.getByRole('link', { name: 'Ajustes' })).toBeVisible();
    await expect(nav.getByRole('link', { name: 'Cuentas' })).toBeVisible();

    // Restore the shared admin to the seeded defaults (en / signed) so sibling
    // tests in this worker (worker-scoped server fixture) see the app in English.
    // Use VALUE-based / language-INDEPENDENT selectors: selectOption is value-keyed,
    // and the submit button is targeted by form+type (never by its label text) since
    // the current locale is es here and would be unknown after any mid-test failure.
    await page.goto('/settings');
    await page.locator('#setting-locale').selectOption('en');
    await page.locator('#setting-display').selectOption('signed');
    await page.locator('form.settings-form button[type="submit"]').click();
    await page.waitForURL('**/settings?**');
    await expect(page.locator('main#main h1')).toHaveText('Settings');
  });
});
