// @ts-check
// Functional test of the REAL programs-management flow (p11.5 + p29). It drives the
// actual /programs page served by `cuento serve -dev` against the worker's fresh
// migrated db with a seeded admin (is_admin, hence TxnRead view + TxnWrite manage).
//
// The whole flow is ONE test that logs in ONCE and exercises create -> edit ->
// blocked-deactivate sequentially. Keeping it to a single login matters: the
// worker-scoped fixture shares one server (and its login rate limiter, burst 5 per
// ip+user) across every spec on the worker, so each spec keeps its login count
// small.
//
// p29: the create/edit form moved to its OWN PAGE (GET /programs/new and
// /programs/{id}/edit are full shell pages, no longer an inline htmx swap atop the
// list). The list's New/Edit triggers are now plain <a> LINKS (page navigation).
// The form still uses hx-post, so a successful Save returns an HX-Redirect back to
// /programs. Selectors come straight from program_form.tmpl / programs.tmpl:
//   - New-program trigger: link "New program" -> /programs/new
//   - form fields:         #pf-name, #pf-name-es, #pf-description, #pf-parent
//   - guard message:       p.field-error (rendered {{t error.program.*}})

const { test, expect } = require('../fixtures');

// saveProgram clicks the own-page form's Save (hx-post -> HX-Redirect) and waits for
// the navigation back to the programs list.
async function saveProgram(page) {
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL('**/programs');
}

test('programs: create child with Spanish name + description, edit, blocked deactivate', async ({ page, server }) => {
  // Log in once (admin => TxnWrite, hence manage).
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');

  // --- the tree lists the seeded root program ("General") ---
  await page.goto('/programs');
  await expect(page.getByRole('heading', { name: /programs/i })).toBeVisible();
  await expect(page.getByRole('cell', { name: 'General', exact: true })).toBeVisible();

  // --- create a child program on its OWN PAGE, with a Spanish name + description ---
  await page.getByRole('link', { name: /new program/i }).click();
  await page.waitForURL('**/programs/new');
  await expect(page.locator('#pf-name')).toBeVisible();
  await page.locator('#pf-name').fill('Outreach E2E'); // parent defaults to root
  await page.locator('#pf-name-es').fill('Alcance E2E');
  await page.locator('#pf-description').fill('Community outreach E2E');
  await saveProgram(page);
  await expect(page.getByRole('cell', { name: /Outreach E2E/ })).toBeVisible();
  // The description shows on the list.
  await expect(page.getByText('Community outreach E2E')).toBeVisible();

  // --- edit via the OWN PAGE: the Spanish name + description round-trip, then rename ---
  const row = page.locator('tr.prog-row', { hasText: 'Outreach E2E' });
  await row.getByRole('link', { name: /^edit$/i }).click();
  await page.waitForURL(/\/programs\/\d+\/edit$/);
  await expect(page.locator('#pf-name')).toHaveValue('Outreach E2E');
  await expect(page.locator('#pf-name-es')).toHaveValue('Alcance E2E');
  await expect(page.locator('#pf-description')).toHaveValue('Community outreach E2E');
  await page.locator('#pf-name').fill('Renamed Program E2E');
  await saveProgram(page);
  await expect(page.getByRole('cell', { name: /Renamed Program E2E/ })).toBeVisible();
  await expect(page.getByRole('cell', { name: 'Outreach E2E', exact: true })).toHaveCount(0);

  // --- a blocked deactivate (active child) shows the localized guard message ---
  await page.getByRole('link', { name: /new program/i }).click();
  await page.waitForURL('**/programs/new');
  await page.locator('#pf-name').fill('Parent Program E2E');
  await saveProgram(page);

  await page.getByRole('link', { name: /new program/i }).click();
  await page.waitForURL('**/programs/new');
  await page.locator('#pf-name').fill('Kid Program E2E');
  await page.locator('#pf-parent').selectOption({ label: 'Parent Program E2E' });
  await saveProgram(page);

  // Deactivating the parent is blocked; the localized guard message appears and the
  // parent stays active (no execution) -- the store's no-trace discipline (D24).
  const parentRow = page.locator('tr.prog-row', { hasText: 'Parent Program E2E' }).first();
  await parentRow.getByRole('button', { name: /deactivate/i }).click();
  await expect(page.locator('p.field-error')).toBeVisible();
  await expect(page.locator('p.field-error')).toContainText(/active children/i);
});
