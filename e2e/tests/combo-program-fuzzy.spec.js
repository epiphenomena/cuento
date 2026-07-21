// @ts-check
// p29.13: every PROGRAM selector is the same fuzzy + hierarchy combobox the account
// pickers are (p28.2). Each program option's label carries the dotted ancestor PATH
// (data-path, e.g. "General.Education"), so a query like "gen.edu" ranks the child by
// its hierarchy. This mirrors combo-account-fuzzy for programs: it creates a child
// program (so a real dotted path exists), then proves the REPORT program filter
// (#rp-program, a plain native <select> before p29.13) is now a combo-input that
// filters/ranks/picks by the path -- exercising both the shell-wide combos.js
// enhancement (reachability on a non-grid select) and the ProgramPaths hierarchy label.

const { test, expect } = require('../fixtures');
const { saveAndReload } = require('../helpers');

async function login(page, server) {
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');
}

// createChildProgram makes a child under the seeded root "General" so its path is
// "General.<name>" -- the label the p29.13 program pickers fuzzy-rank on.
async function createChildProgram(page, name) {
  await page.goto('/programs');
  await page.getByRole('button', { name: /new program/i }).click();
  await expect(page.locator('#pf-name')).toBeVisible();
  await page.locator('#pf-name').fill(name); // parent defaults to the root "General"
  await saveAndReload(page, { reloadPath: '/programs', formSelector: 'form#program-form' });
  await expect(page.getByRole('cell', { name, exact: true })).toBeVisible();
}

test.describe('program selectors are fuzzy + hierarchy comboboxes (p29.13)', () => {
  test('the report program filter fuzzy-ranks and picks a program by its dotted path', async ({ page, server }) => {
    await login(page, server);
    // A child + a sibling child, so a query must actually RANK, not be the only option.
    await createChildProgram(page, 'Edufuzz Program');
    await createChildProgram(page, 'Otherfuzz Program');

    // The program-statement report's program filter (#rp-program): a plain native
    // <select> before p29.13, now a combo-input enhanced shell-wide by combos.js.
    await page.goto('/reports/program_statement');
    await expect(page.locator('#rp-program')).toBeVisible();

    // The report program filter DROPS the implied root ("General.") segment, so a
    // direct child of the root carries just its own name on data-path.
    const path = 'Edufuzz Program';
    await expect(
      page.locator('#rp-program option', { hasText: 'Edufuzz Program' }).first(),
    ).toHaveAttribute('data-path', path);

    const cell = page.locator('#rp-program').locator('xpath=ancestor::div[contains(@class,"combo")][1]');
    const input = cell.locator('.combo-text');
    const list = cell.locator('.combo-list');
    await input.click();
    await input.fill('');
    await input.type('edufuzz'); // contiguous fragment of "Edufuzz Program"
    // (1) DOM: the child ranks into the filtered list, labeled by its path.
    const wanted = list.locator('.combo-option', { hasText: path });
    await expect(wanted).toBeVisible();
    // (2) Visible: the list is on screen (not stacked behind the select).
    await expect(list).toBeVisible();
    // (3) Pickable: clicking it sets the native select's value to the program id.
    const val = await page.locator('#rp-program option', { hasText: 'Edufuzz Program' }).first().getAttribute('value');
    await wanted.first().click();
    await expect(page.locator('#rp-program')).toHaveValue(/** @type {string} */ (val));
  });

  // The program filter default is EMPTY (blank box) and CLEARING it means "all
  // programs" (value 0). A user picks a program, then clears the box + tabs away -> the
  // select resets to 0 and the report reloads at "all" (scoped ONLY to this select via
  // data-empty-value; other combos still revert a cleared box to their selection).
  test('clearing the program filter resets to all (empty == program 0)', async ({ page, server }) => {
    await login(page, server);
    await createChildProgram(page, 'Clearme Program');

    await page.goto('/reports/program_statement');
    const select = page.locator('#rp-program');
    await expect(select).toBeVisible();
    // Default render: value 0 (all) and a BLANK overlay box (no "— all programs —" text).
    await expect(select).toHaveValue('0');
    const cell = select.locator('xpath=ancestor::div[contains(@class,"combo")][1]');
    const input = cell.locator('.combo-text');
    await expect(input).toHaveValue('');
    await expect(input).toHaveAttribute('placeholder', /programs/i);

    // Pick a real program via the overlay.
    const list = cell.locator('.combo-list');
    await input.click();
    await input.type('clearme');
    await list.locator('.combo-option', { hasText: 'Clearme Program' }).first().click();
    const val = await select.locator('option', { hasText: 'Clearme Program' }).first().getAttribute('value');
    await expect(select).toHaveValue(/** @type {string} */ (val));

    // Now CLEAR the box and blur (tab away) -> the select snaps back to 0 (all programs),
    // the overlay stays blank, and the report reloads (the URL drops to program=0 / absent).
    await input.click();
    await input.fill('');
    await input.blur();
    await expect(select).toHaveValue('0');
    await expect(input).toHaveValue('');
  });
});
