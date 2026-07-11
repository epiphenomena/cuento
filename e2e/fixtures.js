// @ts-check
// Server fixture for the cuento functional tests (DECISIONS "Functional testing").
//
// Each test run gets its OWN cuento server, backed by a FRESH temporary SQLite
// database with a known seeded admin, listening on a dynamically allocated port.
// The fixture drives the REAL `cuento serve -dev` binary end to end -- it is not a
// mock. Per run it:
//
//   1. builds bin/cuento once (via globalSetup; here we just reuse the artifact),
//   2. makes a temp dir + db path,
//   3. `cuento migrate -db <db>` (creates the schema),
//   4. seeds admin `e2eadmin` by piping a password to
//      `cuento user add e2eadmin --admin -db <db>` on STDIN,
//   5. allocates a free TCP port in Node (listen on :0, read the assigned port,
//      close), then launches `cuento serve -dev -db <db> -addr 127.0.0.1:<port>`,
//   6. polls GET /healthz until 200, then exposes baseURL,
//   7. on teardown kills serve and removes the temp dir.
//
// -dev makes the session cookie non-Secure so Playwright over plain http works.
//
// The p06.4 DECISIONS quirk: db.Open url.PathEscape's the -db path, so an absolute
// path is cwd-sensitive across invocations. Every child process (migrate, user
// add, serve) is therefore spawned with the SAME cwd (the repo root) and the SAME
// -db value, so all three resolve to one physical file. Getting this wrong shows
// up as `auth.invalid` on a correct password (serve opened a different db).

const { test: base, expect } = require('@playwright/test');
const { spawn, spawnSync } = require('node:child_process');
const net = require('node:net');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const http = require('node:http');

const repoRoot = path.resolve(__dirname, '..');
const binary = path.join(repoRoot, 'bin', 'cuento');

// The admin credentials seeded into every run's db. Specs log in with these.
const ADMIN_USERNAME = 'e2eadmin';
const ADMIN_PASSWORD = 'e2e-admin-passw0rd';

// allocateFreePort opens a server on port 0 (the OS assigns a free port), reads
// the assigned port, closes the server, and resolves the port. There is a tiny
// TOCTOU window between close and serve binding it, acceptable for a local test
// harness where nothing else is grabbing 127.0.0.1 ports.
function allocateFreePort() {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.on('error', reject);
    srv.listen(0, '127.0.0.1', () => {
      const { port } = srv.address();
      srv.close((err) => (err ? reject(err) : resolve(port)));
    });
  });
}

// run executes a cuento subcommand synchronously from the repo root, optionally
// feeding stdin, and throws on non-zero exit (surfacing stderr for diagnosis).
function run(args, input) {
  const res = spawnSync(binary, args, {
    cwd: repoRoot,
    input: input === undefined ? undefined : input,
    encoding: 'utf8',
  });
  if (res.status !== 0) {
    throw new Error(
      `cuento ${args.join(' ')} failed (exit ${res.status}):\n${res.stderr || res.stdout}`,
    );
  }
  return res;
}

// waitForHealthz polls GET <baseURL>/healthz until it answers 200 or the deadline
// passes. /healthz is Public (no auth), so a 200 means the server is serving.
function waitForHealthz(baseURL, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  return new Promise((resolve, reject) => {
    const attempt = () => {
      const req = http.get(`${baseURL}/healthz`, (res) => {
        res.resume();
        if (res.statusCode === 200) {
          resolve();
        } else {
          retry(`healthz status ${res.statusCode}`);
        }
      });
      req.on('error', (err) => retry(err.message));
    };
    const retry = (why) => {
      if (Date.now() > deadline) {
        reject(new Error(`server not ready after ${timeoutMs}ms: ${why}`));
        return;
      }
      setTimeout(attempt, 100);
    };
    attempt();
  });
}

// The fixtures: `server` is worker-scoped (one cuento server per Playwright
// worker, shared across that worker's tests) and auto-set baseURL for the page.
const test = base.extend({
  // eslint-disable-next-line no-empty-pattern
  server: [
    async ({}, use, workerInfo) => {
      if (!fs.existsSync(binary)) {
        throw new Error(
          `bin/cuento not found at ${binary}; run \`make e2e\` (or \`make build\`) first`,
        );
      }

      const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'cuento-e2e-'));
      const dbPath = path.join(dir, 'cuento.db');

      // Schema, then seed the known admin (password piped on stdin, newline so the
      // CLI's single-line read terminates).
      run(['migrate', '-db', dbPath]);
      run(['user', 'add', ADMIN_USERNAME, '--admin', '-db', dbPath], `${ADMIN_PASSWORD}\n`);

      const port = await allocateFreePort();
      const baseURL = `http://127.0.0.1:${port}`;

      const proc = spawn(
        binary,
        ['serve', '-dev', '-db', dbPath, '-addr', `127.0.0.1:${port}`],
        { cwd: repoRoot, stdio: ['ignore', 'pipe', 'pipe'] },
      );

      // Capture server output so a startup failure is diagnosable.
      let log = '';
      proc.stdout.on('data', (d) => (log += d));
      proc.stderr.on('data', (d) => (log += d));

      let exited = false;
      proc.on('exit', () => (exited = true));

      try {
        await waitForHealthz(baseURL, 30_000);
      } catch (err) {
        proc.kill('SIGKILL');
        fs.rmSync(dir, { recursive: true, force: true });
        throw new Error(`${err.message}\nserver log:\n${log}${exited ? '\n(process exited)' : ''}`);
      }

      await use({
        baseURL,
        username: ADMIN_USERNAME,
        password: ADMIN_PASSWORD,
        dbPath,
        workerIndex: workerInfo.workerIndex,
      });

      // Teardown: stop serve, remove the temp db + dir.
      proc.kill('SIGTERM');
      await new Promise((resolve) => {
        if (exited) return resolve();
        const t = setTimeout(() => {
          proc.kill('SIGKILL');
          resolve();
        }, 5_000);
        proc.on('exit', () => {
          clearTimeout(t);
          resolve();
        });
      });
      fs.rmSync(dir, { recursive: true, force: true });
    },
    { scope: 'worker' },
  ],

  // Point every page at this worker's server automatically.
  baseURL: async ({ server }, use) => {
    await use(server.baseURL);
  },
});

module.exports = { test, expect, ADMIN_USERNAME, ADMIN_PASSWORD };
