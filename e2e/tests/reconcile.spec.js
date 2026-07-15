// @ts-check
// Functional test of the REAL reconciliation workspace (p16.3). It drives the actual
// /reconciliations flow served by `cuento serve -dev` against a fresh migrated db
// with a seeded admin (is_admin -> TxnRead view + TxnWrite act) and the seeded root
// subsidiary ("Organization", id 1). Worker-scoped db: the spec creates its OWN
// reconcilable account + splits through the UI (does not leak / assume fixture data).
//
// The whole flow logs in ONCE (the worker-scoped server shares one login rate
// limiter across the worker's specs) and then:
//   1. creates a RECONCILABLE checking account + an income account,
//   2. posts two deposits into checking (250.00 and 150.00) via the real editor,
//   3. starts a reconciliation from the list with statement date + ending balance
//      400.00 (opening 0, so a zero difference is reachable by clearing both),
//   4. TOGGLES the two splits and asserts the difference chip updates via a TARGETED
//      swap (NOT a full navigation) and reaches zero,
//   5. FINALIZES (enabled only at zero) and asserts the finalized recon shows.
//
// The TARGETED-swap proof (the p16.3 anti-jank requirement): the toggle button is a
// native <button hx-post ... hx-target="#recon-row-<id>"> that swaps ONLY its row and
// OOB-swaps the sticky #recon-summary. We install a window sentinel and assert it
// SURVIVES a toggle (no full reload) while the #recon-diff-chip text changes. Strict
// CSP rules out page.waitForFunction, so we wait on the DOM text (the diff chip) and
// the e2e-settled marker the page fixture stamps -- never a nav.
//
// Selectors come straight from reconciliations.tmpl / reconcile.tmpl / accounts.tmpl
// / transaction_form.tmpl.

const { test, expect } = require('../fixtures');
const { openNewAccount, saveAccount } = require('../helpers');

async function login(page, server) {
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');
}

// createAccount makes a leaf account of `type` mapped to the root subsidiary, with
// the reconcilable flag when asked. `type` 'asset' is the form default (no type-change
// re-fetch); other types trigger the htmx form swap, so we wait for it to settle.
async function createAccount(page, name, type, reconcilable) {
  await openNewAccount(page);
  if (type !== 'asset') {
    await page.locator('#af-type').selectOption(type);
    // The type change swaps the form; wait for the new form to settle again.
    await expect(page.locator('form#account-form.e2e-settled')).toBeVisible();
  }
  await page.locator('#af-name-en').fill(name);
  // Use USD so the account's default (statement) currency matches the transactions,
  // which the editor posts in the root subsidiary's base currency (USD, seeded).
  await page.locator('#af-currency').selectOption('USD');
  const rootSub = page.locator('input[name="sub_1"]');
  if (!(await rootSub.isChecked())) await rootSub.check();
  if (reconcilable) {
    const recon = page.locator('input[name="reconcilable"]');
    if (!(await recon.isChecked())) await recon.check();
  }
  await saveAccount(page);
  await expect(page.locator('tr.acct-row', { hasText: name })).toBeVisible();
}

// postDeposit posts a balanced 2-split deposit: checking DR +amount, income CR
// -amount, via the real editor grid. amount is a display string like '250.00'.
async function postDeposit(page, checkingName, incomeName, amount) {
  await page.goto('/transactions/new');
  await expect(page.locator('form#txn-form')).toBeVisible();
  await page.locator('#txn-account-0').selectOption({ label: checkingName });
  await page.locator('#txn-amount-0').fill(amount);
  await page.locator('#txn-account-1').selectOption({ label: incomeName });
  await page.locator('#txn-amount-1').fill(`-${amount}`);
  await page.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL((u) => /\/accounts\/\d+\/register/.test(u.pathname));
}

test('reconcile: start, toggle splits (targeted swap), reach zero, finalize', async ({
  page,
  server,
}) => {
  test.slow(); // this flow creates accounts + posts two transactions before reconciling
  await login(page, server);

  const checking = 'Recon Checking E2E';
  const income = 'Recon Income E2E';
  await createAccount(page, checking, 'asset', true);
  await createAccount(page, income, 'revenue', false);

  await postDeposit(page, checking, income, '250.00');
  await postDeposit(page, checking, income, '150.00');

  // --- the recon LIST shows the reconcilable account with a start form ---
  await page.goto('/reconciliations');
  await expect(
    page.getByRole('heading', { name: 'Reconciliations', exact: true }),
  ).toBeVisible();
  const acctRow = page.locator('tr.recon-list-row', { hasText: checking });
  await expect(acctRow).toBeVisible();

  // --- start a reconciliation: statement date + ending balance 400.00 ---
  await acctRow.locator('input[name="statement_date"]').fill('2026-03-31');
  await acctRow.locator('input[name="balance"]').fill('400.00');
  await acctRow.getByRole('button', { name: /start reconciliation/i }).click();
  // The start POSTs and redirects to the workspace.
  await page.waitForURL('**/reconciliations/*');
  await expect(page.getByRole('heading', { name: /reconcile/i })).toBeVisible();

  // The two deposit splits render, each with a cleared toggle; the summary is present.
  const toggles = page.locator('button.recon-toggle');
  await expect(toggles).toHaveCount(2);
  await expect(page.locator('#recon-summary')).toBeVisible();

  // Difference starts at 400.00 (statement 400 - opening 0 - cleared 0).
  const diff = page.locator('#recon-diff-chip');
  await expect(diff).toContainText('400.00');
  // Finalize is DISABLED at a nonzero difference.
  await expect(page.locator('#recon-finalize')).toBeDisabled();

  // --- TARGETED-SWAP PROOF: install a sentinel; a toggle must NOT reload the page ---
  await page.evaluate(() => {
    /** @type {any} */ (window).__reconSentinel = 'alive';
  });

  // Toggle the FIRST split via a CLICK. Its row swaps in place (hx-target) and the
  // summary OOB. We wait for the diff chip TEXT to change (a targeted swap updated
  // it), not a nav. After clearing one deposit the difference is 400 - 250 = 150.00.
  await toggles.first().click();
  await expect(diff).toContainText('150.00');
  // The sentinel survived -> no full-page reload happened (targeted swap only).
  const aliveAfterFirst = await page.evaluate(
    () => /** @type {any} */ (window).__reconSentinel,
  );
  expect(aliveAfterFirst).toBe('alive');

  // Toggle the SECOND split via the KEYBOARD (the "Space toggles the focused row"
  // affordance): FOCUS its toggle button, press Space -> the split clears. The toggle
  // swaps the whole row (destroying the focused button), so htmx's focus-restore-by-id
  // must bring focus back to the same-id button -- which is what makes REPEATED Space
  // work. Assert BOTH the flip (difference -> 0.00) AND that focus survived the swap.
  const secondToggle = page.locator('button.recon-toggle').nth(1);
  await secondToggle.focus();
  await page.keyboard.press('Space');
  await expect(diff).toContainText('0.00');
  await expect(page.locator('button.recon-toggle').nth(1)).toBeFocused();
  const aliveAfterSecond = await page.evaluate(
    () => /** @type {any} */ (window).__reconSentinel,
  );
  expect(aliveAfterSecond).toBe('alive');
  await expect(page.locator('#recon-finalize')).toBeEnabled();

  // --- FINALIZE (enabled only at zero); the finalized recon shows ---
  const workspaceURL = page.url();
  // Finalize is htmx-driven (hx-post): on success redirectAfterForm emits HX-Redirect
  // back to this workspace (now the finalized read-only view). The summary was just
  // OOB-swapped by the toggle, so wait for it to SETTLE (htmx wires the swapped-in
  // form's hx-post on the settle tick) before clicking -- else the submit is dropped
  // (see fixtures.js). Click DIRECTLY after the toggles (no reload) and wait for POST.
  await expect(page.locator('#recon-summary.e2e-settled')).toBeVisible();
  const finalized = page.waitForResponse(
    (r) => r.url().endsWith('/finalize') && r.request().method() === 'POST',
  );
  await page.getByRole('button', { name: /^finalize$/i }).click();
  await finalized;
  // The finalized recon renders read-only with the finalized note + a Reopen action.
  await expect(page.locator('.recon-finalized-note')).toBeVisible();
  await expect(page.getByRole('button', { name: /reopen/i })).toBeVisible();
  // No live toggles remain on a finalized recon (read-only).
  await expect(page.locator('button.recon-toggle')).toHaveCount(0);
  // We are on the same workspace URL (the finalized recon "shows").
  expect(page.url()).toBe(workspaceURL);

  // --- p16.4 HISTORY + STATEMENT REPORT: the finalized recon appears on the
  // /reconciliations history and its statement report renders the statement info +
  // included splits + opening/closing chain. ---
  await page.goto('/reconciliations');
  const historyRow = page.locator('tr.recon-history-row', { hasText: checking });
  await expect(historyRow.first()).toBeVisible();
  // The history row shows the statement date and links to the statement report.
  await expect(historyRow.first()).toContainText('2026-03-31');
  const statementLink = historyRow.first().locator('a.recon-history-link');
  await expect(statementLink).toBeVisible();

  // Open the statement report from the history link.
  await statementLink.click();
  await page.waitForURL('**/reports/reconciliation_statement**');
  const reportTable = page.locator('table.report-table');
  await expect(reportTable).toBeVisible();
  // Statement info: the account name + finalized status show in the preamble.
  await expect(reportTable).toContainText(checking);
  // The two cleared deposits are INCLUDED as split lines (their income counterpart is
  // NOT on this account, so exactly the two checking deposits appear as data lines).
  await expect(reportTable).toContainText('250.00');
  await expect(reportTable).toContainText('150.00');
  // Opening + Closing balance rows frame the statement (the chain). Statement balance
  // 400.00 == closing (opening 0 + cleared 400).
  await expect(reportTable).toContainText('400.00');
});
