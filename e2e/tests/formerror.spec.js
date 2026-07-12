// @ts-check
// Functional test of the p10.3 form-error convention against the REAL app served by
// `cuento serve -dev`. The demonstrator is the -dev-only /styleguide page (Public,
// no login needed); its form (templates/formdemo.tmpl, "demo-form" partial) posts to
// /styleguide via htmx (hx-post, hx-target="#demo-form", hx-swap="outerHTML").
//
// This proves the whole point of the step — the htmx TARGETED SWAP — end to end:
//   - an invalid submit swaps the 422 form-region partial IN PLACE (no full reload),
//   - the LOCALIZED field error is visible (rendered via {{t}}, not a raw key),
//   - focus lands on the FIRST invalid field after the swap,
//   - a valid submit swaps in the success message (still no reload).
//
// The 422 swap works only because base.tmpl's htmx-config meta adds {"code":"422",
// "swap":true} (htmx 2.x does NOT swap 4xx by default); focus-after-swap works via
// the formfocus.js module (browsers don't fire autofocus on inserted nodes). If
// either regressed, this spec fails.
//
// Selectors from formdemo.tmpl: #demo-form / #demo-name / #demo-email / the submit
// button; per-field error <p class="field-error" role="alert">.

const { test, expect } = require('../fixtures');

test.describe('form-error convention (htmx swap, 422, autofocus, i18n)', () => {
  test('an invalid submit swaps the 422 form partial in place without a full reload', async ({ page }) => {
    await page.goto('/styleguide');
    await expect(page.locator('#demo-form')).toBeVisible();

    // Sentinel on window: a full-page navigation (reload) would wipe it. A targeted
    // htmx swap keeps the same document, so it must survive — this is how we prove
    // "no full page reload".
    await page.evaluate(() => { /** @type {any} */ (window).__noReload = 1; });

    // Invalid: empty name (required) + a malformed email. First invalid field = name.
    await page.locator('#demo-name').fill('');
    await page.locator('#demo-email').fill('notanemail');
    await page.locator('#demo-form button[type="submit"]').click();

    // The localized field errors are visible (en catalog strings, not raw keys).
    await expect(page.locator('#demo-name-err')).toHaveText('This field is required.');
    await expect(page.locator('#demo-email-err')).toHaveText('Enter a valid email address.');
    await expect(page.locator('body')).not.toContainText('error.required');

    // No full reload: the sentinel survived the swap (a navigation would clear it).
    const survived = await page.evaluate(() => /** @type {any} */ (window).__noReload);
    expect(survived).toBe(1);

    // Focus landed on the FIRST invalid field (name) after the swap.
    await expect(page.locator('#demo-name')).toBeFocused();
  });

  test('a valid submit swaps in the success message (still no reload)', async ({ page }) => {
    await page.goto('/styleguide');
    await page.evaluate(() => { /** @type {any} */ (window).__noReload2 = 1; });

    await page.locator('#demo-name').fill('Ada');
    await page.locator('#demo-email').fill('ada@example.org');
    await page.locator('#demo-form button[type="submit"]').click();

    // The success alert swapped in; no field errors remain.
    await expect(page.locator('.alert-ok')).toHaveText('Looks good.');
    await expect(page.locator('#demo-name-err')).toHaveCount(0);

    // Still the same document (targeted swap, no reload).
    const survived = await page.evaluate(() => /** @type {any} */ (window).__noReload2);
    expect(survived).toBe(1);
  });
});
