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

    // A localized nav label (the admin persona is en by default): the Settings and
    // Admin sections render for an admin user.
    const nav = page.locator('nav.app-nav');
    await expect(nav.getByRole('link', { name: 'Settings' })).toBeVisible();
    await expect(nav.getByRole('link', { name: 'Admin' })).toBeVisible();

    // p18.1: the footer surfaces the build version (the release binary bakes it
    // via -X main.version; the e2e binary is a plain `make build`, so it shows
    // "Version dev"). Assert the localized label + a non-empty version token by
    // pattern, never a hard-coded value, so the spec holds across build paths.
    await expect(page.locator('footer.app-footer')).toContainText(/Version\s+\S+/);

    // The Settings nav target is live (renders through the shell, AnyUser).
    await nav.getByRole('link', { name: 'Settings' }).click();
    await page.waitForURL('**/settings');
    await expect(page.locator('main#main h1')).toHaveText('Settings');
  });

  test('toggles the theme and persists it across a reload (SSR, no flash)', async ({ page, server }) => {
    await login(page, server);

    // Default theme is "auto" (server-side from the absent cookie / default).
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'auto');

    // Toggle to dark via the plain-form theme control (no JS).
    await page.locator('#theme-select').selectOption('dark');
    await page.locator('form.theme-control button[type="submit"]').click();

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
