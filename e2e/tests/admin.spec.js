// @ts-check
// Functional test of the p13.2 admin surface. It drives the REAL /admin/users and
// /admin/currencies pages served by `cuento serve -dev` against the worker's fresh
// migrated db with the seeded admin (is_admin => Admin perm). In ONE test (one
// login, to stay under the login rate limiter shared per worker) it:
//   - creates a NEW operator through the inline create form,
//   - sets that user's txn_perm on the per-user detail page,
//   - grants a report group to that user,
//   - adds a currency,
// asserting each took effect.
//
// TEST-ISOLATION (the worker-scoped `server` shares one db across a worker's specs):
// this spec NEVER mutates the shared e2eadmin's perms/locale/grants -- it creates and
// mutates its OWN uniquely-named user, and adds a currency with a UNIQUE 3-letter
// code, so nothing durable leaks into sibling specs on the worker. Selectors are
// language-independent (ids, form selectors, data-* row attributes) so a mid-run
// locale change elsewhere could never break them. No page.waitForFunction (strict
// CSP: script-src 'self', no unsafe-eval) -- only locator/URL/response waits.
//
// Selectors come from admin_users.tmpl / admin_user_detail.tmpl / admin_currencies.tmpl:
//   - create form (inline swap): #uc-username, #uc-password, #uc-perm, button "Create user"
//   - user row:                  tr.user-row[data-username="..."]
//   - detail perm form:          form.txn-perm-form #ud-perm
//   - detail grants form:        form.grants-form input[name^="grant_"]
//   - currency add form:         form.currency-add-form #cc-code/#cc-name/#cc-symbol/#cc-exponent
//   - currency row:              tr.currency-row[data-code="..."]

const { test, expect } = require('../fixtures');

// A short unique suffix per test run so the created user + currency never collide
// with a sibling spec's data in the shared worker db (and survive a same-worker
// retry -- the currency upsert is idempotent, but a unique code also keeps rows
// distinct across specs).
function unique() {
  return Math.random().toString(36).slice(2, 8);
}

// threeLetterCode returns a random 3-uppercase-letter currency code (the store
// requires exactly [A-Z]{3}). Random keeps parallel specs from colliding.
function threeLetterCode() {
  const A = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ';
  let c = '';
  for (let i = 0; i < 3; i++) c += A[Math.floor(Math.random() * A.length)];
  return c;
}

async function login(page, server) {
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');
}

test('admin: create a user, set its permission, grant a report group, add a currency', async ({ page, server }) => {
  await login(page, server);

  const username = `e2euser_${unique()}`;

  // --- create a user through the inline create form ---
  await page.goto('/admin/users');
  await expect(page.locator('main#main h1')).toHaveText('Users');

  // p26.88: a discoverability hint tells the admin that report-group access +
  // expense-submit are set on the user's OWN page after creation (they are not on the
  // create form). It renders on page load near the create form.
  await expect(page.locator('.user-grants-hint')).toContainText(/report-group access and expense-submit/i);

  await page.getByRole('button', { name: /new user/i }).click();
  // Wait for the swapped-in form to settle (htmx wires hx-post on the settle tick).
  await expect(page.locator('form#user-create-form.e2e-settled')).toBeVisible();
  await page.locator('#uc-username').fill(username);

  // p26.87: the password field is PRE-FILLED with a generated strong password on load
  // (pwgen.js self-inits on the htmx swap). It is non-empty and readable (type=text so
  // the admin can copy it). Capture the first value, click Regenerate, and confirm the
  // value CHANGED — the generator produced a fresh password.
  const pwField = page.locator('#uc-password');
  await expect(pwField).toHaveValue(/.+/);
  await expect(pwField).toHaveAttribute('type', 'text');
  const firstPw = await pwField.inputValue();
  expect(firstPw.length).toBeGreaterThanOrEqual(16);
  await page.getByRole('button', { name: /regenerate/i }).click();
  const secondPw = await pwField.inputValue();
  expect(secondPw).not.toEqual(firstPw);
  expect(secondPw.length).toBeGreaterThanOrEqual(16);
  // Use the GENERATED password to create the user (the whole point — the admin hands
  // this to the new user); we log in with it at the end of the test.
  const generatedPw = secondPw;

  await page.locator('#uc-perm').selectOption('none');

  // The inline create redirects (HX-Redirect) back to /admin/users; wait for that
  // GET response deterministically (set up BEFORE the click), matched by pathname --
  // the same-url reload pattern the shared helper guards against.
  const listReloaded = page.waitForResponse(
    (r) => new URL(r.url()).pathname === '/admin/users' && r.request().method() === 'GET',
  );
  await page.getByRole('button', { name: /create user/i }).click();
  await listReloaded;

  // The new operator appears in the list.
  const row = page.locator(`tr.user-row[data-username="${username}"]`);
  await expect(row).toBeVisible();
  // Its txn_perm cell starts at "No access" (perm none).
  await expect(row.locator('.user-perm')).toHaveText(/no access/i);

  // --- set the user's txn_perm on the per-user detail page ---
  await row.getByRole('link', { name: /^edit$/i }).click();
  await page.waitForURL('**/admin/users/*');
  await expect(page.locator('#ud-perm')).toBeVisible();
  await page.locator('#ud-perm').selectOption('write');
  // The txn-perm form is a full-page POST -> 303 to .../{id}?saved=1 (a real URL
  // change), so waitForURL is a genuine wait (never the same-url no-op).
  await page.locator('form.txn-perm-form button[type="submit"]').click();
  await page.waitForURL('**/admin/users/*?saved**');
  // The select now reflects "write" on the re-rendered page.
  await expect(page.locator('#ud-perm')).toHaveValue('write');

  // --- grant a report group to the user ---
  // The per-user page lists a checkbox per report group. Check the first one and save.
  const grantBox = page.locator('form.grants-form input[type="checkbox"]').first();
  await expect(grantBox).toBeVisible();
  await grantBox.check();
  await page.locator('form.grants-form button[type="submit"]').click();
  await page.waitForURL('**/admin/users/*?saved**');
  // The grant persisted: the box is checked on the fresh render.
  await expect(page.locator('form.grants-form input[type="checkbox"]').first()).toBeChecked();

  // The list reflects the perm change on the user's row. (Report grants are no
  // longer shown on the list row — they are edited on the per-user page.)
  await page.goto('/admin/users');
  const listRow = page.locator(`tr.user-row[data-username="${username}"]`);
  await expect(listRow.locator('.user-perm')).toHaveText(/read and write/i);

  // --- add a currency ---
  const code = threeLetterCode();
  await page.goto('/admin/currencies');
  await expect(page.locator('main#main h1')).toHaveText('Currencies');
  await page.locator('#cc-code').fill(code);
  await page.locator('#cc-name').fill(`E2E ${code}`);
  await page.locator('#cc-symbol').fill('¤');
  await page.locator('#cc-exponent').fill('2');

  const curReloaded = page.waitForResponse(
    (r) => new URL(r.url()).pathname === '/admin/currencies' && r.request().method() === 'GET',
  );
  await page.locator('form.currency-add-form button[type="submit"]').click();
  await curReloaded;

  // The currency appears in the list, active.
  const curRow = page.locator(`tr.currency-row[data-code="${code}"]`);
  await expect(curRow).toBeVisible();
  await expect(curRow.locator('.currency-status')).toHaveText(/active/i);

  // --- p26.87: the GENERATED password actually works: log out the admin and log in as
  // the new user with the password the generator produced. The new username is unique
  // so its login shares NO rate-limit bucket with the admin (the limiter keys on
  // ip+username), keeping this second login well under the per-key burst. ---
  await page.locator('form.app-logout button[type="submit"]').click();
  await page.waitForURL('**/login**');
  await page.locator('#username').fill(username);
  await page.locator('#password').fill(generatedPw);
  await page.getByRole('button', { name: /.+/ }).click();
  // A successful login lands on the authenticated landing page (not back on /login).
  await page.waitForURL('**/');
  await expect(page).not.toHaveURL(/\/login/);
});
