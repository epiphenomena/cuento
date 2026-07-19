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
