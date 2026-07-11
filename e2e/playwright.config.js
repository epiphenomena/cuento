// @ts-check
// Playwright configuration for cuento's functional tests (DECISIONS "Functional
// testing"). The server fixture (fixtures.js) manages the cuento server itself --
// each run needs a fresh temp db, a seeded admin, and a dynamically allocated port
// -- so Playwright's own `webServer` is deliberately NOT used. globalSetup builds
// the binary once; baseURL is set per-worker by the fixture.

const { defineConfig, devices } = require('@playwright/test');

module.exports = defineConfig({
  testDir: './tests',
  globalSetup: require.resolve('./global-setup.js'),
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  // Local functional tests: no retries. A flake should be seen, not hidden.
  retries: 0,
  reporter: 'list',
  timeout: 30_000,
  expect: { timeout: 5_000 },
  use: {
    // baseURL is injected per worker by the `server` fixture; a bad request with no
    // baseURL would fail loudly rather than silently hit the wrong host.
    trace: 'off',
    actionTimeout: 5_000,
    navigationTimeout: 10_000,
  },
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
});
