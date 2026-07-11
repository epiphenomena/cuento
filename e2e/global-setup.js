// @ts-check
// Global setup: build bin/cuento ONCE for the whole test run (before any worker
// starts a server). `make e2e` also builds the binary, so this is belt-and-braces
// for a direct `npx playwright test` invocation; `make build` is idempotent and
// fast when the binary is up to date.

const { spawnSync } = require('node:child_process');
const path = require('node:path');

module.exports = async () => {
  const repoRoot = path.resolve(__dirname, '..');
  const res = spawnSync('make', ['build'], {
    cwd: repoRoot,
    stdio: 'inherit',
  });
  if (res.status !== 0) {
    throw new Error(`make build failed (exit ${res.status})`);
  }
};
