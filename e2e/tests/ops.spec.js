// @ts-check
// Functional test of the p18.3 admin ops page. It drives the REAL /admin/ops page
// served by `cuento serve -dev` against the worker's fresh migrated db with the
// seeded admin (is_admin => Admin perm). In ONE test (one login, to stay under the
// login rate limiter shared per worker) it:
//   - opens /admin/ops and sees the build info (a version + the Go runtime version),
//   - sees the integrity-check SECTION render its result,
//   - takes a backup snapshot and asserts the download is a 200 octet-stream whose
//     bytes begin with the SQLite magic header.
//
// TEST-ISOLATION (the worker-scoped `server` shares one db across a worker's specs):
// this spec is READ-ONLY except for the backup, which only appends an ops.backup
// audit change (no business data), so nothing durable leaks into sibling specs.
// Crucially it does NOT assert the integrity check is CLEAN: sibling specs on the
// same worker legitimately post transactions and create restricted funds / R/E
// leaves that raise advisory warnings (Z18/Z19), so cleanliness is run-order
// dependent. Instead it asserts the check SECTION renders SOME result (the clean
// notice OR a violations list). Selectors are language-independent (ids, classes,
// data-*), so a mid-run locale change elsewhere could never break them. No
// page.waitForFunction (strict CSP: script-src 'self', no unsafe-eval) -- only
// locator/URL/response/request waits.
//
// Selectors come from ops.tmpl:
//   - build info:   .ops-version, .ops-go-version
//   - check result: .ops-check-clean OR .ops-violations (either is a rendered result)
//   - backup form:  form.ops-backup-form (POST /admin/ops/backup)

const { test, expect } = require('../fixtures');

// The SQLite file header: 16 bytes, "SQLite format 3" then a single NUL byte.
const SQLITE_MAGIC = Buffer.from('SQLite format 3\0', 'latin1');

async function login(page, server) {
  await page.goto('/login');
  await page.locator('#username').fill(server.username);
  await page.locator('#password').fill(server.password);
  await page.getByRole('button', { name: /.+/ }).click();
  await page.waitForURL('**/');
}

test('ops: build info + integrity check render, and the backup downloads a SQLite file', async ({ page, server }) => {
  await login(page, server);

  await page.goto('/admin/ops');

  // --- build info ---
  // The running version (whatever `make e2e` stamped) and the Go runtime version are
  // both shown; assert each element is present and non-empty.
  await expect(page.locator('.ops-version')).not.toBeEmpty();
  const goVersion = page.locator('.ops-go-version');
  await expect(goVersion).toBeVisible();
  await expect(goVersion).toHaveText(/^go\d/); // e.g. "go1.24.0"

  // --- integrity check ---
  // The check SECTION renders SOME result: on a clean db the "no violations" notice,
  // otherwise a violations list. We assert either is present (never "clean" alone --
  // sibling specs on the shared worker db may raise advisory warnings).
  await expect(page.locator('.ops-check-clean, .ops-violations').first()).toBeVisible();

  // --- backup snapshot ---
  // POST the backup form via the browser's request context (same session cookie +
  // matching Origin, so the cross-origin guard passes). The response is the snapshot
  // itself: a 200 octet-stream attachment whose bytes begin with the SQLite magic.
  await expect(page.locator('form.ops-backup-form')).toBeVisible();
  const resp = await page.request.post('/admin/ops/backup');
  expect(resp.status()).toBe(200);
  expect(resp.headers()['content-type']).toBe('application/octet-stream');
  expect(resp.headers()['content-disposition']).toContain('attachment');
  const body = await resp.body();
  expect(body.length).toBeGreaterThan(0);
  expect(body.subarray(0, SQLITE_MAGIC.length).equals(SQLITE_MAGIC)).toBe(true);
});
