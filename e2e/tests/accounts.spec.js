// @ts-check
// Functional test of the REAL chart-of-accounts flow (p11.1, p26.7). It drives the
// actual /accounts page served by `cuento serve -dev` against a fresh migrated db
// with a seeded admin (who is is_admin, hence TxnWrite). It logs in, creates an
// account through the real create form, asserts it appears in the tree, edits it,
// and proves a bad submit shows the localized field error (the p10.3 form-error
// convention).
//
// p26.7: the create/edit form moved OUT of the inline #account-form htmx swap onto
// dedicated full-shell pages. The New/Edit triggers are now plain links (a full-page
// navigation to GET /accounts/new and /accounts/{id}/edit), and Save is a plain POST
// that 303-redirects to /accounts on success or re-renders the WHOLE page at 422 with
// the field error + autofocus on failure. Selectors from account_form.tmpl /
// accounts.tmpl:
//   - New-account trigger:  link "New account" (-> /accounts/new)
//   - per-row Edit trigger:  link "Edit" (-> /accounts/{id}/edit)
//   - form fields:          #af-name-en, #af-name-es, #af-type, #af-currency
//   - subsidiary checklist: input[name="sub_1"] (the root "Organization")
//   - field error:          p.field-error (rendered {{t error.account.*}})

const { test, expect } = require('../fixtures');

// login signs the admin in and lands on the authenticated shell. Reused by every
// test here (no storageState wiring in this suite; a fresh login is cheap).
async function login(page, server) {
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');
}

test.describe('chart of accounts', () => {
  test('creates an account through the form page and it appears in the tree', async ({ page, server }) => {
    await login(page, server);

    await page.goto('/accounts');
    await expect(page.getByRole('heading', { name: /chart of accounts/i })).toBeVisible();

    // Open the create form on its OWN page (a plain navigation, p26.7).
    await page.getByRole('link', { name: /new account/i }).click();
    await page.waitForURL('**/accounts/new');
    await expect(page.locator('#af-name-en')).toBeVisible();

    // Fill a valid create: en + es names, type asset, root subsidiary checked.
    await page.locator('#af-name-en').fill('Petty Cash E2E');
    await page.locator('#af-name-es').fill('Caja Chica E2E');
    await page.locator('#af-type').selectOption('asset');
    // p28.7/p28.8: a free-text note shows in the chart's Notes column (which replaced
    // the redundant per-row Type).
    await page.locator('#af-notes').fill('Reception drawer float E2E');
    // The root subsidiary (id 1, "Organization") is pre-checked on a new account;
    // ensure it is checked so the store's >=1-subsidiary rule is satisfied.
    const rootSub = page.locator('input[name="sub_1"]');
    if (!(await rootSub.isChecked())) {
      await rootSub.check();
    }

    // Success is a server 303-redirect back to /accounts; the new account is in the tree.
    await page.getByRole('button', { name: /^save$/i }).click();
    await page.waitForURL('**/accounts');
    await expect(page.getByText('Petty Cash E2E')).toBeVisible();
    // The note renders in the row's Notes cell.
    const pettyRow = page.locator('tr.acct-row', { hasText: 'Petty Cash E2E' });
    await expect(pettyRow.locator('.acct-notes')).toHaveText('Reception drawer float E2E');
  });

  test('a bad submit re-renders the page with the localized field error', async ({ page, server }) => {
    await login(page, server);
    await page.goto('/accounts');

    await page.getByRole('link', { name: /new account/i }).click();
    await page.waitForURL('**/accounts/new');
    await expect(page.locator('#af-name-en')).toBeVisible();

    // Leave the English name blank -> the store rejects with ErrNameRequired, which
    // the handler maps to error.account.name_required and re-renders the WHOLE page
    // at 422 with the field error + native autofocus on the invalid name input.
    await page.locator('#af-name-en').fill('');
    const rootSub = page.locator('input[name="sub_1"]');
    if (!(await rootSub.isChecked())) {
      await rootSub.check();
    }
    await page.getByRole('button', { name: /^save$/i }).click();

    // The localized error is shown; we stayed on the form (POST action is /accounts).
    await expect(page.locator('p.field-error')).toBeVisible();
    await expect(page.locator('p.field-error')).toContainText(/english name is required/i);
    // Autofocus landed on the first invalid control (native on a real page render).
    await expect(page.locator('#af-name-en')).toBeFocused();
  });

  test('edits an existing account and the new name shows in the tree', async ({ page, server }) => {
    await login(page, server);
    await page.goto('/accounts');

    // Create one to edit.
    await page.getByRole('link', { name: /new account/i }).click();
    await page.waitForURL('**/accounts/new');
    await page.locator('#af-name-en').fill('Editable E2E');
    await page.locator('#af-type').selectOption('asset');
    const rootSub = page.locator('input[name="sub_1"]');
    if (!(await rootSub.isChecked())) {
      await rootSub.check();
    }
    await page.getByRole('button', { name: /^save$/i }).click();
    await page.waitForURL('**/accounts');
    await expect(page.getByText('Editable E2E')).toBeVisible();

    // Open its edit page (the row's Edit link navigates to /accounts/{id}/edit).
    const row = page.locator('tr.acct-row', { hasText: 'Editable E2E' });
    await row.getByRole('link', { name: /^edit$/i }).click();
    await page.waitForURL('**/accounts/*/edit');
    await expect(page.locator('#af-name-en')).toHaveValue('Editable E2E');

    await page.locator('#af-name-en').fill('Renamed E2E');
    await page.getByRole('button', { name: /^save$/i }).click();
    await page.waitForURL('**/accounts');
    await expect(page.getByText('Renamed E2E')).toBeVisible();
    await expect(page.getByText('Editable E2E')).toHaveCount(0);
  });

  // p26.14: the subsidiary + active-only filters are remembered in the session, so
  // a fresh navigation back to /accounts restores the last-used selection instead
  // of resetting to defaults. Sets a real filter (sub -> the root subsidiary,
  // active-only checked), navigates away, and asserts both are restored on return.
  test('remembers and restores the filter selection across navigation', async ({ page, server }) => {
    await login(page, server);
    await page.goto('/accounts');

    // Set a deliberate filter: pick the root subsidiary and check "active only".
    // The form auto-applies on change (htmx GET), which saves it to the session.
    await page.locator('#sub-filter').selectOption('1');
    await page.locator('input[name="active"]').check();
    // Let the htmx change-fetch settle (it swaps #accounts-results).
    await expect(page.locator('#accounts-results')).toBeVisible();

    // Navigate away to another in-app page (same session), then come back to a
    // BARE /accounts with no query params -- a fresh nav that must restore.
    await page.goto('/funds');
    await page.goto('/accounts');

    // The saved selection is restored from the session.
    await expect(page.locator('#sub-filter')).toHaveValue('1');
    await expect(page.locator('input[name="active"]')).toBeChecked();
  });

  // p26.25: the reusable tree-table collapse/expand controls (treetable.js). Builds a
  // 3-deep asset chain (root -> child -> leaf), then drives the controls: collapse-all
  // leaves only depth-0 rows, expand-one-level reveals the next depth progressively,
  // and a per-row disclosure toggle hides/shows just that row's subtree.
  test('collapse/expand controls drive the accounts tree', async ({ page, server }) => {
    await login(page, server);

    // createAccount fills the standalone create form and returns to /accounts.
    // NOTE: a new form already defaults to type "asset", so we do NOT touch the type
    // select -- changing it re-fetches the whole form region via htmx (parent options
    // must be type-compatible), which REPLACES #af-parent and would race our parent
    // selection. Leaving type at its default keeps the form stable. We still retry the
    // parent selection until it sticks and confirm a non-zero parent before saving.
    async function createAccount(name, parentName) {
      await page.goto('/accounts/new');
      await page.locator('#af-name-en').fill(name);
      if (parentName) {
        await expect(async () => {
          await page.locator('#af-parent').selectOption({ label: parentName });
          await expect(page.locator('#af-parent')).not.toHaveValue('0');
        }).toPass({ timeout: 5000 });
      }
      const rootSub = page.locator('input[name="sub_1"]');
      if (!(await rootSub.isChecked())) await rootSub.check();
      await page.getByRole('button', { name: /^save$/i }).click();
      // Wait for the success redirect to the bare /accounts (not a 422 re-render,
      // whose URL would also end in /accounts), and confirm the row landed.
      await page.waitForURL(/\/accounts$/);
      await expect(page.locator('tr.acct-row', { hasText: name })).toBeVisible();
    }

    // p28.2: the #af-parent option label is now the account's dotted HIERARCHY path
    // (so the shared fuzzy combobox ranks "c.boa" -> "Cash.BOA"), so a non-root parent
    // is selected by its full path (a root's path is just its name).
    await createAccount('Tree Root E2E', null);
    await createAccount('Tree Child E2E', 'Tree Root E2E');
    await createAccount('Tree Leaf E2E', 'Tree Root E2E.Tree Child E2E');

    await page.goto('/accounts');
    // p26.74: an injected "Assets" TYPE HEADER now sits at depth 0 above the asset
    // roots (which shifted to depth 1); the chain is header(0) -> Tree Root E2E(1) ->
    // Child(2) -> Leaf(3). The header is a plain .acct-row.acct-type-header (no id, no
    // register link) and participates in collapse/expand as the block's top parent.
    const header = page.locator('tr.acct-type-header', { hasText: 'Assets' });
    const root = page.locator('tr.acct-row', { hasText: 'Tree Root E2E' });
    const child = page.locator('tr.acct-row', { hasText: 'Tree Child E2E' });
    const leaf = page.locator('tr.acct-row', { hasText: 'Tree Leaf E2E' });

    // The header + all three accounts are visible initially (the module fully expands).
    await expect(header).toBeVisible();
    await expect(root).toBeVisible();
    await expect(child).toBeVisible();
    await expect(leaf).toBeVisible();

    // Collapse all -> only the depth-0 TYPE HEADERS remain; every account hides.
    await page.locator('.tree-collapse-all').click();
    await expect(header).toBeVisible();
    await expect(root).toBeHidden();
    await expect(child).toBeHidden();
    await expect(leaf).toBeHidden();

    // Expand one level -> depth-1 (the asset roots, incl. Tree Root) show; deeper hidden.
    await page.locator('.tree-expand-level').click();
    await expect(root).toBeVisible();
    await expect(child).toBeHidden();
    await expect(leaf).toBeHidden();

    // Expand one more level -> depth-2 (child) shows, depth-3 (leaf) still hidden.
    await page.locator('.tree-expand-level').click();
    await expect(child).toBeVisible();
    await expect(leaf).toBeHidden();

    // Expand once more -> depth-3 (the leaf) shows; fully expanded now.
    await page.locator('.tree-expand-level').click();
    await expect(leaf).toBeVisible();

    // Collapsing the injected Assets HEADER hides its whole type subtree (p26.74:
    // collapse/expand works on a header). Its disclosure toggle lives in the name cell.
    await header.locator('.tree-toggle').click();
    await expect(root).toBeHidden();
    await expect(child).toBeHidden();
    await expect(leaf).toBeHidden();
    // Expanding the header again reveals its subtree (nothing beneath was collapsed).
    await header.locator('.tree-toggle').click();
    await expect(root).toBeVisible();
    await expect(child).toBeVisible();
    await expect(leaf).toBeVisible();

    // Per-row disclosure toggle still works: collapsing Tree Root hides its subtree.
    await root.locator('.tree-toggle').click();
    await expect(child).toBeHidden();
    await expect(leaf).toBeHidden();
    await root.locator('.tree-toggle').click();
    await expect(child).toBeVisible();
    await expect(leaf).toBeVisible();
  });

  // p26.74: the chart injects a display-only TYPE HEADER per type in canonical
  // statement order (Assets, Liabilities, Equity, Revenue, Expenses), with the real
  // accounts nested one level under each. The header is not an account: no register
  // link, no Edit/Deactivate. This drives the REAL page over the fixture (which has
  // all five types) plus a freshly-created asset nested under Assets.
  test('shows the five type headers with accounts nested under them', async ({ page, server }) => {
    await login(page, server);

    // The e2e db is FRESH (no fixture accounts), so create one account of each type
    // so every canonical section header appears. A type-change re-fetches the form
    // region (parent options), so wait for the type select to settle each time.
    async function createTyped(name, typ) {
      await page.goto('/accounts/new');
      await page.locator('#af-name-en').fill(name);
      if (typ !== 'asset') { // asset is the default; changing type re-fetches the form
        await page.locator('#af-type').selectOption(typ);
        await expect(page.locator('#af-type')).toHaveValue(typ);
        await page.locator('#af-name-en').fill(name); // re-fill in case the re-fetch cleared it
      }
      const rootSub = page.locator('input[name="sub_1"]');
      if (!(await rootSub.isChecked())) await rootSub.check();
      await page.getByRole('button', { name: /^save$/i }).click();
      await page.waitForURL(/\/accounts$/);
      await expect(page.locator('tr.acct-row', { hasText: name })).toBeVisible();
    }
    await createTyped('Grouped Asset E2E', 'asset');
    await createTyped('Grouped Liab E2E', 'liability');
    await createTyped('Grouped Equity E2E', 'equity');
    await createTyped('Grouped Rev E2E', 'revenue');
    await createTyped('Grouped Exp E2E', 'expense');

    await page.goto('/accounts');
    // Every canonical section header is present, in order, as a display-only row.
    for (const label of ['Assets', 'Liabilities', 'Equity', 'Revenue', 'Expenses']) {
      await expect(page.locator('tr.acct-type-header', { hasText: label })).toBeVisible();
    }
    // A type header carries no register link / Edit / Deactivate (it is not an account).
    const assets = page.locator('tr.acct-type-header', { hasText: 'Assets' });
    await expect(assets.locator('a')).toHaveCount(0);
    await expect(assets.locator('button[type="submit"]')).toHaveCount(0);

    // The created asset is present and (via data-depth) sits deeper than the Assets header.
    const acct = page.locator('tr.acct-row', { hasText: 'Grouped Asset E2E' });
    await expect(acct).toBeVisible();
    const headerDepth = Number(await assets.getAttribute('data-depth'));
    const acctDepth = Number(await acct.getAttribute('data-depth'));
    expect(headerDepth).toBe(0);
    expect(acctDepth).toBeGreaterThan(0);
  });

  // p27.1b: the shared account attributes current_cash + open_item. The two
  // checkboxes are type-gated server-side (current_cash asset-only; open_item
  // asset/liability-only) using the same htmx type-refetch machinery as the
  // functional-class / default-program regions. Creating an open_item ASSET shows
  // the A/R badge on the chart; switching type to equity hides both controls.
  test('current_cash + open_item flags gate by type and label A/R on the chart', async ({ page, server }) => {
    await login(page, server);

    await page.goto('/accounts/new');
    // On the default (asset) type both flag checkboxes are present.
    await expect(page.locator('input[name="current_cash"]')).toBeVisible();
    await expect(page.locator('input[name="open_item"]')).toBeVisible();

    // Create an open-item receivable asset that is also spendable cash.
    await page.locator('#af-name-en').fill('AR Cash E2E');
    await page.locator('input[name="current_cash"]').check();
    await page.locator('input[name="open_item"]').check();
    const rootSub = page.locator('input[name="sub_1"]');
    if (!(await rootSub.isChecked())) await rootSub.check();
    await page.getByRole('button', { name: /^save$/i }).click();
    await page.waitForURL(/\/accounts$/);

    // The chart shows the A/R badge next to the open_item asset's name, and (p28.8)
    // the current-cash indicator badge stays visible too.
    const row = page.locator('tr.acct-row', { hasText: 'AR Cash E2E' });
    await expect(row).toBeVisible();
    await expect(row.locator('.badge-open-item')).toHaveText('A/R');
    await expect(row.locator('.badge-current-cash')).toBeVisible();

    // Switching the new-account type to EQUITY hides BOTH flag controls (server-gated
    // via the htmx type re-fetch). Await the re-fetch response so the swap has settled
    // before asserting (the type select fires an hx-get to /accounts/new).
    await page.goto('/accounts/new');
    const toEquity = page.waitForResponse((r) => r.url().includes('/accounts/new') && r.request().method() === 'GET');
    await page.locator('#af-type').selectOption('equity');
    await toEquity;
    await expect(page.locator('#af-type')).toHaveValue('equity');
    await expect(page.locator('input[name="current_cash"]')).toHaveCount(0);
    await expect(page.locator('input[name="open_item"]')).toHaveCount(0);

    // Switching to LIABILITY shows open_item (payable) but NOT current_cash.
    const toLiability = page.waitForResponse((r) => r.url().includes('/accounts/new') && r.request().method() === 'GET');
    await page.locator('#af-type').selectOption('liability');
    await toLiability;
    await expect(page.locator('#af-type')).toHaveValue('liability');
    await expect(page.locator('input[name="open_item"]')).toBeVisible();
    await expect(page.locator('input[name="current_cash"]')).toHaveCount(0);
  });

  // p26.75: the account-type filter narrows the chart to one type's group; "All"
  // restores every type, and the selection persists across a bare navigation (like
  // the sub/active filters). Auto-applies on change via the same htmx filter form.
  test('the account-type filter narrows the chart and All restores it', async ({ page, server }) => {
    await login(page, server);

    // Two accounts of different types so filtering is observable.
    async function createTyped(name, typ) {
      await page.goto('/accounts/new');
      await page.locator('#af-name-en').fill(name);
      if (typ !== 'asset') {
        await page.locator('#af-type').selectOption(typ);
        await expect(page.locator('#af-type')).toHaveValue(typ);
        await page.locator('#af-name-en').fill(name);
      }
      const rootSub = page.locator('input[name="sub_1"]');
      if (!(await rootSub.isChecked())) await rootSub.check();
      await page.getByRole('button', { name: /^save$/i }).click();
      await page.waitForURL(/\/accounts$/);
      await expect(page.locator('tr.acct-row', { hasText: name })).toBeVisible();
    }
    await createTyped('TF Asset E2E', 'asset');
    await createTyped('TF Liab E2E', 'liability');

    await page.goto('/accounts');
    const asset = page.locator('tr.acct-row', { hasText: 'TF Asset E2E' });
    const liab = page.locator('tr.acct-row', { hasText: 'TF Liab E2E' });
    const liabHeader = page.locator('tr.acct-type-header', { hasText: 'Liabilities' });
    await expect(asset).toBeVisible();
    await expect(liab).toBeVisible();

    // Filter to Assets: the asset shows; the liability + its header disappear.
    await page.locator('#type-filter').selectOption('asset');
    await expect(page.locator('#accounts-results')).toBeVisible();
    await expect(asset).toBeVisible();
    await expect(liab).toHaveCount(0);
    await expect(liabHeader).toHaveCount(0);

    // Persist across a bare nav (away then back): the asset-only filter is remembered.
    await page.goto('/funds');
    await page.goto('/accounts');
    await expect(page.locator('#type-filter')).toHaveValue('asset');
    await expect(page.locator('tr.acct-row', { hasText: 'TF Liab E2E' })).toHaveCount(0);

    // "All" restores every type.
    await page.locator('#type-filter').selectOption('');
    await expect(page.locator('#accounts-results')).toBeVisible();
    await expect(page.locator('tr.acct-row', { hasText: 'TF Asset E2E' })).toBeVisible();
    await expect(page.locator('tr.acct-row', { hasText: 'TF Liab E2E' })).toBeVisible();
    await expect(page.locator('tr.acct-type-header', { hasText: 'Liabilities' })).toBeVisible();
  });

  // p28.9: the EPHEMERAL fuzzy search filters the chart rows client-side; unlike the
  // sub/active/type filters it is NOT remembered -- leaving and returning does NOT
  // restore the typed query, and the full tree is back.
  test('the chart search filters rows and is NOT remembered across navigation', async ({ page, server }) => {
    await login(page, server);

    // Two distinctly-named accounts so the search is observable.
    async function create(name) {
      await page.goto('/accounts/new');
      await page.locator('#af-name-en').fill(name);
      const rootSub = page.locator('input[name="sub_1"]');
      if (!(await rootSub.isChecked())) await rootSub.check();
      await page.getByRole('button', { name: /^save$/i }).click();
      await page.waitForURL(/\/accounts$/);
    }
    await create('Searchable Widget E2E');
    await create('Hidden Gadget E2E');

    await page.goto('/accounts');
    const widget = page.locator('tr.acct-row', { hasText: 'Searchable Widget E2E' });
    const gadget = page.locator('tr.acct-row', { hasText: 'Hidden Gadget E2E' });
    await expect(widget).toBeVisible();
    await expect(gadget).toBeVisible();

    // Type a query matching only the widget; the gadget row hides (CSS display:none).
    await page.locator('#acct-search').fill('widget');
    await expect(widget).toBeVisible();
    await expect(gadget).toBeHidden();

    // Clearing the box restores every row (treetable's state governs again).
    await page.locator('#acct-search').fill('');
    await expect(gadget).toBeVisible();

    // Re-filter, then navigate AWAY and BACK: the search must NOT be restored -- the
    // box is empty and all rows show (proving it is ephemeral, no session key).
    await page.locator('#acct-search').fill('widget');
    await expect(gadget).toBeHidden();
    await page.goto('/funds');
    await page.goto('/accounts');
    await expect(page.locator('#acct-search')).toHaveValue('');
    await expect(page.locator('tr.acct-row', { hasText: 'Searchable Widget E2E' })).toBeVisible();
    await expect(page.locator('tr.acct-row', { hasText: 'Hidden Gadget E2E' })).toBeVisible();
  });
});
