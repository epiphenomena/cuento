// @ts-check
// Shared helpers for the cuento functional tests.

const { expect } = require('@playwright/test');

// saveAndReload clicks the inline form's Save button and waits DETERMINISTICALLY for
// the post-save reload, fixing a suite-wide flake class:
//
//   - The create/edit forms are delivered INLINE as an htmx swap into a list page
//     (chart of accounts, programs, subsidiaries). A successful Save returns an htmx
//     HX-Redirect back to that SAME list URL. Because the page is ALREADY on that URL,
//     `page.waitForURL(sameUrl)` resolves IMMEDIATELY without waiting for the reload,
//     so a following `expect(row).toBeVisible()` races the reload and, under parallel
//     CPU load, loses (5s expect timeout). We wait for the reload RESPONSE instead —
//     set up BEFORE the click (or the response is missed) and matched by PATHNAME (an
//     `endsWith` check would let a future `/accounts?active=1` slip through).
//   - htmx wires the swapped-in form's Save `hx-post` on the settle tick, AFTER the
//     form paints; clicking Save in that window drops the submit (proven at merge's
//     expense-account case). So we first wait for the form to be `e2e-settled` (the
//     marker the `page` fixture stamps on every htmx:afterSettle target). By the
//     invariant "waitForURL-is-a-no-op ⟺ the form arrived as an inline swap", every
//     vulnerable site IS a swapped form, so this wait is always valid here (it only
//     risks hanging on full-page-loaded forms, which are never vulnerable sites).
//
// reloadPath is the list pathname the Save redirects back to (e.g. '/accounts').
// formSelector is the inline form's selector (defaults to the account form).
async function saveAndReload(page, { reloadPath, formSelector = 'form#account-form' }) {
  await expect(page.locator(`${formSelector}.e2e-settled`)).toBeVisible();
  const reloaded = page.waitForResponse(
    (r) => new URL(r.url()).pathname === reloadPath && r.request().method() === 'GET',
  );
  await page.getByRole('button', { name: /^save$/i }).click();
  await reloaded;
}

// openNewAccount navigates to the create-account page (p26.7). The New-account
// trigger on /accounts is now a plain LINK to GET /accounts/new (its own full-shell
// page), not an inline htmx swap into #account-form. Many specs create an account as
// fixture setup; this centralizes the changed mechanics (link click + navigation
// wait) so the field-filling stays inline in each spec. Waits for #af-name-en so the
// form is ready to fill.
async function openNewAccount(page) {
  await page.goto('/accounts');
  await page.getByRole('link', { name: /new account/i }).click();
  await page.waitForURL('**/accounts/new');
  await expect(page.locator('#af-name-en')).toBeVisible();
}

// saveAccount submits the create/edit form (p26.7). Save is a plain full-page POST
// that 303-redirects to /accounts on success (PRG), so we wait for that navigation.
// (A validation failure re-renders the SAME page at 422; for create the POST action
// is also /accounts so this still resolves — the caller's follow-up assertion catches
// a real failure, matching the old saveAndReload contract.)
async function saveAccount(page) {
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL('**/accounts');
}

// selectTxnAccount picks an account option in a transaction-editor account select
// (#txn-main-account / #txn-account-N) by its account NAME, tolerant of the account
// -type prefix the option text now carries. The transaction form renders each option
// as "<Type> · <dotted path>" (e.g. "Asset · Cash.BOA") so the type is visible and
// fuzzy-filterable; Playwright's selectOption({label}) is an EXACT match and would no
// longer find a bare name. We locate the option whose text ends with "· <name>"
// (the last path segment equals the account's own name), read its real value (the
// account id), and select by value — so specs keep naming accounts, not type prefixes.
async function selectTxnAccount(locator, name) {
  const esc = name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  const opt = locator.locator('option').filter({ hasText: new RegExp(`·\\s+${esc}$`) });
  const value = await opt.first().getAttribute('value');
  await locator.selectOption(value);
}

module.exports = { saveAndReload, openNewAccount, saveAccount, selectTxnAccount };
