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
    // page: the top nav renders in Spanish (Cuentas / Más), proving locale is read
    // per-render from the stored setting. (p23.9 moved Settings into the hub — now the
    // p26.77 "All" landing — so the top-nav es labels are Cuentas + Todo.)
    await page.goto('/accounts');
    const nav = page.locator('nav.app-nav');
    await expect(nav.getByRole('link', { name: 'Cuentas' })).toBeVisible();
    await expect(nav.getByRole('link', { name: 'Todo', exact: true })).toBeVisible();

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

  // p26.5: the default-program setting round-trips AND reaches the txn editor form as
  // the data-user-program attribute (the client fallback tier gateRow uses to prefill a
  // new R/E row's program when the account has no default_program of its own). The e2e
  // db has only the seeded root program ("General", id 1). We pick it, save, assert the
  // reloaded select keeps it, and assert the txn form carries data-user-program="1".
  // Then restore to "none" (the seeded default) so the shared worker-scoped admin is
  // left as found (a durable change would poison sibling tests in this worker).
  test('default program round-trips and reaches the txn editor form', async ({ page, server }) => {
    await login(page, server);

    await page.goto('/settings');
    await expect(page.locator('main#main h1')).toHaveText('Settings');

    // Pick the seeded root program (value 1) as the default and save.
    await page.locator('#setting-program').selectOption('1');
    await page.locator('form.settings-form button[type="submit"]').click();
    await page.waitForURL('**/settings?**');

    // The reloaded select keeps the choice (persisted + echoed).
    await expect(page.locator('#setting-program')).toHaveValue('1');

    // The user default reaches the txn editor as data-user-program (gateRow's fallback
    // tier). A fresh -dev db has no accounts, so we assert the plumbing (the attribute)
    // rather than driving a full R/E row; the account-default -> user-default -> root
    // prefill precedence itself is covered by the R/E-prefill txn-editor spec plus this
    // attribute check (gateRow is DOM glue, exercised in-browser, not in the node suite).
    await page.goto('/transactions/new');
    await expect(page.locator('form#txn-form')).toHaveAttribute('data-user-program', '1');

    // Restore: clear the default program back to "none" (seeded default) for siblings.
    await page.goto('/settings');
    await page.locator('#setting-program').selectOption('');
    await page.locator('form.settings-form button[type="submit"]').click();
    await page.waitForURL('**/settings?**');
    await expect(page.locator('#setting-program')).toHaveValue('');
  });
});
