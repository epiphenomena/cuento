// @ts-check
// Functional test of the p14.2 admin exchange-rate CSV upload (/admin/rates, Admin).
// It drives the REAL page served by `cuento serve -dev` against the worker's migrated
// db with the seeded admin (is_admin => Admin perm). An admin uploads a small rates
// CSV via a multipart file input and sees the imported-count confirmation.
//
// TEST-ISOLATION (the worker-scoped `server` shares one db across a worker's specs):
// this spec NEVER touches the seeded currencies or a shared date. It creates TWO of
// its OWN uniquely-coded currencies (the store requires exactly [A-Z]{3}), then
// uploads rates BETWEEN them on a random far-future date -- so no exchange_rates row,
// currency, or FK it writes can collide with or poison a sibling spec on the worker.
// The file is delivered as an in-memory Buffer via setInputFiles (no on-disk fixture).
// No page.waitForFunction (strict CSP: script-src 'self') -- only locator/URL/response
// waits. Selectors are ids / form/status classes from admin_rates.tmpl, so a mid-run
// locale change elsewhere never breaks them.
//
// Selectors (admin_rates.tmpl / admin_currencies.tmpl):
//   - currency add form: form.currency-add-form #cc-code/#cc-name/#cc-symbol/#cc-exponent
//   - rates upload form: form.rates-upload-form input#rates-file[type=file]
//   - success notice:    p.rates-imported[role=status]
//   - error notice:      p.rates-error[role=alert]

const { test, expect } = require('../fixtures');

// A random 3-uppercase-letter currency code; two of these give a unique pair that
// never collides with the seeded currencies or a sibling spec's codes.
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

// addCurrency adds one currency through the admin currencies form and waits for the
// list reload (the add redirects back to /admin/currencies).
async function addCurrency(page, code) {
  await page.goto('/admin/currencies');
  await page.locator('#cc-code').fill(code);
  await page.locator('#cc-name').fill(`E2E ${code}`);
  await page.locator('#cc-symbol').fill('¤');
  await page.locator('#cc-exponent').fill('2');
  const reloaded = page.waitForResponse(
    (r) => new URL(r.url()).pathname === '/admin/currencies' && r.request().method() === 'GET',
  );
  await page.locator('form.currency-add-form button[type="submit"]').click();
  await reloaded;
  await expect(page.locator(`tr.currency-row[data-code="${code}"]`)).toBeVisible();
}

test('admin: import an exchange-rate CSV and see the imported count', async ({ page, server }) => {
  await login(page, server);

  // Two OWN currencies so the imported rates FK cleanly and touch nothing shared.
  const base = threeLetterCode();
  let quote = threeLetterCode();
  while (quote === base) quote = threeLetterCode();
  await addCurrency(page, base);
  await addCurrency(page, quote);

  // A far-future, per-run date so the rate row never collides with a sibling spec.
  const year = 2900 + Math.floor(Math.random() * 90);
  const csv =
    'rate_date,base,quote,rate,source\n' +
    `${year}-01-01,${base},${quote},12.34,e2e\n` +
    `${year}-02-01,${base},${quote},13.57,e2e\n`;

  await page.goto('/admin/rates');
  await expect(page.locator('main#main h1')).toHaveText('Import exchange rates');

  await page.locator('input#rates-file').setInputFiles({
    name: 'rates.csv',
    mimeType: 'text/csv',
    buffer: Buffer.from(csv, 'utf8'),
  });
  await page.locator('form.rates-upload-form button[type="submit"]').click();

  // The imported-count confirmation appears (two rows).
  const notice = page.locator('p.rates-imported[role="status"]');
  await expect(notice).toBeVisible();
  await expect(notice).toHaveText(/2/);

  // A malformed upload (bad header) is rejected with an error, not a crash.
  await page.locator('input#rates-file').setInputFiles({
    name: 'bad.csv',
    mimeType: 'text/csv',
    buffer: Buffer.from('nope,base,quote,rate,source\n', 'utf8'),
  });
  await page.locator('form.rates-upload-form button[type="submit"]').click();
  await expect(page.locator('p.rates-error[role="alert"]')).toBeVisible();
});
