// @ts-check
// Functional test of the p10.2 authenticated shell. It drives the REAL app served
// by `cuento serve -dev`: logs in with the seeded admin (reusing the harness
// fixture), asserts the authenticated shell renders (nav landmarks + a localized
// nav label), toggles the theme and asserts <html data-theme> changes AND persists
// across a reload (the theme cookie is set server-side, so the reload re-renders
// the chosen theme SSR — no flash), and checks the login page localizes to Spanish
// via the language switcher.
//
// Selectors come from the templates:
//   - login:   #username / #password / submit button      (login.tmpl)
//   - nav:     <nav class="app-nav" aria-label="Primary">  (base.tmpl shell-nav)
//   - theme:   <select id="theme-select"> + its form's submit (theme-control)
//   - lang:    <nav class="lang-switch"> links ?lang=<code>  (login.tmpl)

const { test, expect } = require('../fixtures');

// login is a small helper: it drives the real login form and waits for the
// authenticated landing at "/".
async function login(page, server) {
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');
}

test.describe('authenticated shell', () => {
  test('renders the shell landmarks and a localized nav after login', async ({ page, server }) => {
    await login(page, server);

    // Semantic landmarks: header/nav/main/footer are all present.
    await expect(page.locator('header.app-header')).toBeVisible();
    await expect(page.locator('nav.app-nav')).toBeVisible();
    await expect(page.locator('main#main')).toBeVisible();
    await expect(page.locator('footer.app-footer')).toBeVisible();

    // p23.9/p26.77: the top nav is lean (Accounts + All + role items); Settings/Admin
    // and every other destination live on the "All" landing as perm-gated cards.
    const nav = page.locator('nav.app-nav');
    await expect(nav.getByRole('link', { name: 'Accounts' })).toBeVisible();
    await expect(nav.getByRole('link', { name: 'All', exact: true })).toBeVisible();

    // p26.48: "New transaction" is a DISTINCT right-aligned button in the header, NOT an
    // inline nav link. It reuses the .btn/.btn-primary tokens and routes to the full
    // editor (boost-safe). Assert it exists, is not in .app-nav, is right-aligned
    // (margin-left:auto -> its left edge is right of the nav's right edge), and routes.
    await expect(nav.getByRole('link', { name: /new transaction/i })).toHaveCount(0);
    const newTxn = page.locator('header.app-header a.app-newtxn', { hasText: /new transaction/i });
    await expect(newTxn).toBeVisible();
    await expect(newTxn).toHaveClass(/btn-primary/);
    const navBox = await nav.boundingBox();
    const btnBox = await newTxn.boundingBox();
    expect(btnBox.x).toBeGreaterThanOrEqual(navBox.x + navBox.width - 1);
    await newTxn.click();
    await page.waitForURL('**/transactions/new');
    await expect(page.locator('form#txn-form')).toBeVisible();
    // The full editor shell rendered (nav present + the distinct button re-rendered).
    await expect(page.locator('header.app-header a.app-newtxn')).toHaveCount(1);
    await page.goto('/');

    // p18.1: the footer surfaces the build version (the release binary bakes it
    // via -X main.version; the e2e binary is a plain `make build`, so it shows
    // "Version dev"). Assert the localized label + a non-empty version token by
    // pattern, never a hard-coded value, so the spec holds across build paths.
    await expect(page.locator('footer.app-footer')).toContainText(/Version\s+\S+/);

    // The All landing lists cards for every destination the admin can reach — the
    // ledger (Accounts), an admin sub-page (Users), a granted report (Trial balance),
    // and Settings — grouped into labeled sections. Settings is a live target.
    await nav.getByRole('link', { name: 'All', exact: true }).click();
    await page.waitForURL('**/more');
    const main = page.locator('main#main');
    await expect(main.locator('.hub-section-title')).not.toHaveCount(0);
    await expect(main.locator('a.hub-card-link[href="/accounts"]')).toBeVisible();
    await expect(main.locator('a.hub-card-link[href="/admin/users"]')).toBeVisible();
    await expect(main.locator('a.hub-card-link[href="/reports/trial_balance"]')).toBeVisible();
    await expect(main.locator('a.hub-card-link[href="/settings"]')).toBeVisible();
    await main.locator('a.hub-card-link[href="/settings"]').click();
    await page.waitForURL('**/settings');
    await expect(page.locator('main#main h1')).toHaveText('Settings');
  });

  test('toggles the theme and persists it across a reload (SSR, no flash)', async ({ page, server }) => {
    await login(page, server);

    // Default theme is "auto" (server-side from the absent cookie / default).
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'auto');

    // Toggle to dark via the Settings page (p23.1 consolidated the theme control
    // there and removed the header form). Saving POSTs /settings, which writes the
    // theme cookie and 303-redirects back.
    await page.goto('/settings');
    await page.locator('#setting-theme').selectOption('dark');
    await page.locator('form.settings-form button[type="submit"]').click();

    // After the POST/redirect the page re-renders with data-theme="dark" applied
    // server-side (read from the cookie), so it is present on first paint.
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark');

    // Persist across a full reload: the cookie is re-read SSR, so no client round
    // trip and no flash to the default theme.
    await page.reload();
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark');
  });

  test('login page localizes to Spanish via the language switcher', async ({ page }) => {
    await page.goto('/login');

    // English by default.
    await expect(page.locator('h1')).toHaveText('Sign in');

    // Click the Español switcher link (?lang=es); the login page re-renders in es.
    await page.locator('nav.lang-switch').getByRole('link', { name: 'Español' }).click();
    await expect(page.locator('h1')).toHaveText('Iniciar sesion');
    await expect(page.locator('html')).toHaveAttribute('lang', 'es');
  });
});
