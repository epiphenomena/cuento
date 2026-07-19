// @ts-check
// Functional tests for the shared date-field calendar popover (static/datefield.js).
// The PURE date logic is node-tested (datefield.test.js); this drives the REAL DOM
// behavior the node tests can't see — the month-nav bug (p29.1), the month/year
// navigator (p29.2), and select-all-on-focus (p29.3). It opens a real date picker on
// the income-statement report filter (#rp-from) served by `cuento serve -dev`.
//
// No page.waitForFunction / selection introspection (strict CSP blocks eval-based
// probes) — everything is asserted via visible DOM (title text, popover visibility)
// and behavior (typing replaces a selected value).

const { test, expect } = require('../fixtures');

async function login(page, server) {
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');
}

// openPicker navigates to the income-statement report (which renders #rp-from as a
// js-datefield with a known value) and opens its calendar popover, returning the
// popover + input locators.
async function openPicker(page) {
  await page.goto('/reports/income_statement');
  const input = page.locator('#rp-from');
  await expect(input).toBeVisible();
  // Seed a known month so the title/day grid are deterministic (2026-03).
  await input.fill('2026-03-15');
  const wrap = page.locator('.datefield-wrap', { has: input });
  await wrap.locator('.datefield-pick').click();
  const pop = wrap.locator('.datefield-popover');
  await expect(pop).toBeVisible();
  return { input, wrap, pop };
}

test('datefield: prev/next month arrows walk both directions and keep the popover open', async ({
  page,
  server,
}) => {
  await login(page, server);
  const { pop } = await openPicker(page);
  const title = pop.locator('.datefield-title');
  await expect(title).toHaveText('March 2026');

  // Forward: › steps to the next month, popover STAYS open (the p29.1 bug closed it).
  await pop.locator('.datefield-nav').last().click();
  await expect(pop).toBeVisible();
  await expect(title).toHaveText('April 2026');
  await pop.locator('.datefield-nav').last().click();
  await expect(pop).toBeVisible();
  await expect(title).toHaveText('May 2026');

  // Backward: ‹ steps back, crossing the year boundary, popover stays open.
  const prev = () => pop.locator('.datefield-nav').first();
  await prev().click();
  await expect(title).toHaveText('April 2026');
  for (let i = 0; i < 4; i++) await prev().click();
  await expect(pop).toBeVisible();
  await expect(title).toHaveText('December 2025');
});

test('datefield: the month/year title opens a navigator to step the year and pick a month', async ({
  page,
  server,
}) => {
  await login(page, server);
  const { pop } = await openPicker(page); // March 2026
  const title = pop.locator('.datefield-title');
  await expect(title).toHaveText('March 2026');

  // Click the title -> the month navigator opens (year label + 12 month buttons).
  await title.click();
  await expect(pop).toBeVisible();
  await expect(pop.locator('.datefield-months')).toBeVisible();
  await expect(pop.locator('.datefield-month')).toHaveCount(12);
  await expect(pop.locator('.datefield-title')).toHaveText('2026');

  // Step the year forward once via the ‹ year › nav (the header now steps YEARS).
  await pop.locator('.datefield-nav').last().click();
  await expect(pop.locator('.datefield-title')).toHaveText('2027');

  // Pick September -> back to the day grid on September 2027.
  await pop.locator('.datefield-month', { hasText: 'September' }).click();
  await expect(pop.locator('.datefield-months')).toHaveCount(0);
  await expect(pop.locator('.datefield-grid')).toBeVisible();
  await expect(pop.locator('.datefield-title')).toHaveText('September 2027');
  await expect(pop).toBeVisible();

  // Keyboard access (like the day grid): arrows walk the month grid, Enter picks,
  // Escape closes. Self-discriminating: if arrow nav were dead, Enter would pick
  // January (the focused start), not May.
  await title.click(); // reopen the navigator on Sept 2027
  await pop.locator('.datefield-month[data-month="1"]').focus(); // January
  await page.keyboard.press('ArrowRight'); // -> February (+1)
  await page.keyboard.press('ArrowDown'); // -> May (+3, the grid is 3 columns)
  await page.keyboard.press('Enter'); // native button activation picks May
  await expect(pop.locator('.datefield-grid')).toBeVisible();
  await expect(pop.locator('.datefield-title')).toHaveText('May 2027');

  // Escape from the navigator closes the whole popover.
  await title.click();
  await expect(pop.locator('.datefield-months')).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(pop).toBeHidden();
});

// NB: this asserts the select-all-on-focus FEATURE (typing replaces the whole value)
// for both a mouse click-in and a keyboard focus-in. It cannot isolate the mouseup
// guard specifically: synthetic Playwright clicks don't reproduce the native mouseup
// that collapses the selection (dispatched events are untrusted, so the default caret
// placement never runs), so the mouse case would pass with or without the guard in
// this environment. The guard is verified manually in a real browser. Removing the
// focus->select() entirely, however, DOES break both sub-cases here (typing appends).
test('datefield: focusing a date field selects all, so typing replaces the value (mouse + keyboard)', async ({
  page,
  server,
}) => {
  await login(page, server);
  await page.goto('/reports/income_statement');

  // --- mouse click-in: the click's mouseup must NOT collapse the select-all, so a
  // full typed date REPLACES the seeded value rather than inserting into it. ---
  const from = page.locator('#rp-from');
  const to = page.locator('#rp-to');
  await from.fill('2026-03-15');
  await to.fill('2026-06-30');
  // Move focus off both fields so the next focus/click really fires the focus event.
  await page.getByRole('heading').first().click();

  await from.click(); // real mouse click (mousedown+mouseup) into the field
  await page.keyboard.type('2024-01-02');
  await expect(from).toHaveValue('2024-01-02'); // replaced, not appended/spliced

  // --- keyboard focus-in (the Tab path): focus fires, select() runs, no mouseup, so
  // the whole value is selected and typing replaces it. ---
  await page.getByRole('heading').first().click(); // move focus away again
  await to.focus();
  await page.keyboard.type('2025-05-05');
  await expect(to).toHaveValue('2025-05-05');
});
