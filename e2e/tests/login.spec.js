// @ts-check
// Functional test of the REAL login flow (pE.1). It drives the actual login page
// served by `cuento serve -dev` (templates/login.tmpl) against a fresh db with a
// seeded admin. Selectors come straight from login.tmpl:
//   - username field:  #username  (name="username")
//   - password field:  #password  (name="password")
//   - submit:          the form's <button type="submit">
//   - error marker:    <p role="alert"> rendered via {{t .Error}} (key auth.invalid)
// Successful login redirects (303) to "/", the authenticated landing (home.tmpl).

const { test, expect } = require('../fixtures');

test.describe('login', () => {
  test('rejects a wrong password with the uniform error, staying on /login', async ({ page, server }) => {
    await page.goto('/login');

    await page.locator('#username').fill(server.username);
    await page.locator('#password').fill('definitely-not-the-password');
    await page.getByRole('button', { name: /.+/ }).click();

    // Uniform auth error (no user enumeration): the alert is visible and we are
    // still on /login (not redirected to the authenticated landing).
    await expect(page.locator('p[role="alert"]')).toBeVisible();
    expect(new URL(page.url()).pathname).toBe('/login');
    // The password field is still present -> the form re-rendered, not the app.
    await expect(page.locator('#password')).toBeVisible();
  });

  test('rejects an unknown user with the same uniform error', async ({ page }) => {
    await page.goto('/login');

    await page.locator('#username').fill('no-such-user');
    await page.locator('#password').fill('whatever');
    await page.getByRole('button', { name: /.+/ }).click();

    await expect(page.locator('p[role="alert"]')).toBeVisible();
    expect(new URL(page.url()).pathname).toBe('/login');
  });

  test('logs in with valid admin credentials and lands authenticated', async ({ page, server }) => {
    await page.goto('/login');

    await page.locator('#username').fill(server.username);
    await page.locator('#password').fill(server.password);
    await page.getByRole('button', { name: /.+/ }).click();

    // Success: redirected off /login to the authenticated landing at "/".
    await page.waitForURL('**/');
    expect(new URL(page.url()).pathname).toBe('/');
    // No login form on the authenticated landing -> we are truly logged in.
    await expect(page.locator('#password')).toHaveCount(0);
  });
});
