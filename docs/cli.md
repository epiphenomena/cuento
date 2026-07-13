# cuento CLI reference

`cuento` is a single binary. The first argument selects a subcommand; everything
after it is that subcommand's flags and positionals. With no arguments it prints
usage and exits **2**.

```
usage: cuento <command> [flags]

commands:
  serve           run the HTTP server (auto-migrates on start; -dev relaxes cookie Secure)
  migrate         apply pending database migrations
  user            manage users (add|passwd|disable)
  check           run the ledger integrity suite ([-db PATH] [--strict])
  ratesync        fetch configured currency pairs from Yahoo Finance into exchange rates ([-db PATH])
  expense-report  maintenance over expense reports (reject <id> --reason ...) ([-db PATH])
```

One command is **long-running** — `serve`, the application server. The rest are
**operator / one-shot** commands (`migrate`, `user`, `check`, `ratesync`,
`expense-report`) that do their work and exit. All commands are exercised in
[docs/deploy.md](deploy.md); this file is the exhaustive per-flag reference.

Subcommands are verified against `cmd/cuento/main.go` (the dispatch switch) and
their source files:
`serve`/`migrate` → `main.go`, config → `config.go`, `user` → `user.go`,
`check` → `check.go`, `ratesync` → `ratesync.go`, `expense-report` →
`expense_report.go`.

---

## Database path resolution (`-db`) and the cwd caveat

Every command that touches the database accepts a `-db PATH` flag, but the
**default and resolution differ between `serve` and the operator commands** —
this is deliberate and worth understanding before you run anything against a
production db.

- **`serve`** — `-db` defaults to empty. When unset, the path is derived as
  `<data-dir>/cuento.db` (data dir from `CUENTO_DATA_DIR` / `-data-dir`, default
  `.`). `serve` resolves the data dir to an **absolute** path before opening the
  db, so the derived db path and the autocert cache are independent of the
  current working directory. An explicit `-db` is used verbatim (the e2e harness
  relies on a relative `-db` resolving to its own file).
- **Operator commands** (`migrate`, `user *`, `check`, `ratesync`,
  `expense-report reject`) — `-db` defaults to the literal `cuento.db` and is
  passed straight to the SQLite opener. It is **not** made absolute.

**cwd caveat (p06.4 / p01.1 quirk).** The SQLite opener percent-escapes the path
into a `file:` DSN, so even an *absolute* `-db /var/lib/cuento/cuento.db`
escapes its slashes and the driver resolves the result relative to the current
working directory. Practically this means:

- Always run a related set of commands (e.g. `migrate` then `user add`) from the
  **same working directory**, with the **same `-db` value** — otherwise
  `user add` may seed one physical file while `serve` opens another, and a
  correct password then fails to log in.
- In production the systemd units pin the working directory and pass an explicit
  `-db /var/lib/cuento/cuento.db`; the deploy walkthrough uses that path
  consistently, which is why it is correct despite the quirk.

This is a known pre-existing behavior of the db opener, not a per-command bug.

---

## Environment variables (serve only)

`serve` reads four `CUENTO_*` environment variables. Each mirrors a flag of the
same name; the flag **overrides** the env var when explicitly passed on the
command line (env is the base, flags win). When neither env nor flag sets a
knob, the package default applies. Resolution is a pure function
(`resolveConfig`) so the precedence matrix is unit-tested.

| Env var            | Flag         | Default  | Meaning |
|--------------------|--------------|----------|---------|
| `CUENTO_DATA_DIR`  | `-data-dir`  | `.`      | Directory holding the db, the autocert cert cache (`autocert/`), and Litestream's local state. Resolved to an absolute path at startup. |
| `CUENTO_ADDR`      | `-addr`      | `:8080`  | Plain-HTTP listen address. Used only when TLS is **off** (no domain, or `-dev`). |
| `CUENTO_DOMAIN`    | `-domain`    | (empty)  | TLS hostname. When set (and not `-dev`), serve HTTPS on `:443` via autocert plus a `:80` ACME/redirect listener. When empty, plain HTTP on `CUENTO_ADDR`. |
| `CUENTO_DEV`       | `-dev`       | `false`  | Development mode: the session cookie is **not** marked `Secure` (works over plain HTTP). Forces plain HTTP even if a domain leaks in from the environment. **Never set in production.** |

`CUENTO_DEV` is truthy for `1`, `true`, `yes`, `on` (any case); anything else
(including empty) is false. `-db` has **no** env twin by design.

Precedence, concretely: `CUENTO_ADDR=:9000` in a unit with `cuento serve` →
listens on `:9000`; `cuento serve -addr :7000` overrides to `:7000`;
`cuento serve` with nothing set → `:8080`.

---

## `cuento serve` — run the HTTP server

Long-running. Auto-migrates the db on start (backing it up first once schema
exists), opens the pooled handle, syncs report groups, logs a bootstrap hint if
no human users exist yet, then serves until `SIGINT`/`SIGTERM` and shuts down
gracefully.

**Synopsis**

```
cuento serve [-data-dir DIR] [-addr ADDR] [-domain HOST] [-db PATH] [-dev]
```

**Flags** (from `newServeFlags` in `main.go`)

| Flag         | Default  | Meaning |
|--------------|----------|---------|
| `-data-dir`  | (empty → `.`) | Data directory; env `CUENTO_DATA_DIR`. See table above. |
| `-addr`      | (empty → `:8080`) | Plain-HTTP listen address when not serving TLS; env `CUENTO_ADDR`. |
| `-domain`    | (empty)  | TLS hostname; if set, HTTPS on `:443` + `:80` redirect via autocert; env `CUENTO_DOMAIN`. |
| `-db`        | (empty → `<data-dir>/cuento.db`) | Explicit SQLite path override. No env twin. |
| `-dev`       | `false`  | Dev mode: session cookie not marked `Secure` (plain HTTP); env `CUENTO_DEV`. |

**TLS vs plain HTTP.** TLS is used exactly when a domain is configured **and**
`-dev` is off. In TLS mode, certificates are provisioned on demand by autocert
on the **first HTTPS request** and cached under `<data-dir>/autocert/`; `:80`
serves the ACME http-01 challenge and 301-redirects everything else to HTTPS.
Otherwise serve runs plain HTTP on `-addr`.

**Health check.** `GET /healthz` is public (no auth) and reports the build
version; the e2e harness and any external monitor poll it for readiness.

**Exit codes.** 0 on graceful shutdown; non-zero (via `log.Fatalf`) on a startup
or listener error.

**Examples**

```sh
# Production: TLS via autocert on :443 (usually driven by the systemd unit's env).
CUENTO_DATA_DIR=/var/lib/cuento CUENTO_DOMAIN=books.example.com cuento serve

# Local development: plain HTTP, non-Secure cookie (this is what `make run` does).
cuento serve -dev -addr 127.0.0.1:8080 -db ./cuento.db
```

---

## `cuento migrate` — apply pending migrations

Operator / one-shot. Applies any pending embedded (goose, forward-only)
migrations to the configured db, backing the file up first once it already
carries schema (AGENTS rule 4). Idempotent: re-running with nothing pending is a
clean no-op. `serve` runs the same migration on start, so this command is
optional but handy for a first-run or a controlled pre-upgrade step.

**Synopsis**

```
cuento migrate [-db PATH]
```

**Flags**

| Flag  | Default    | Meaning |
|-------|------------|---------|
| `-db` | `cuento.db` | Path to the SQLite database file (created if absent). |

**Exit codes.** 0 on success; non-zero (`log.Fatalf`) on error.

**Example**

```sh
cuento migrate -db /var/lib/cuento/cuento.db
```

---

## `cuento user` — manage users

Operator / one-shot. Dispatches to `add`, `passwd`, or `disable`. All three run
as the seeded **system actor** (user id 1) and write through the store's audited,
versioned write funnel. The db must already have its schema (run `migrate` or
`serve` first).

```
usage: cuento user <command> [flags] <username>

  add <username> [--admin] [--display "Name"]   create a user (password read from stdin)
  passwd <username>                             set a user's password (read from stdin)
  disable <username>                            disable a user (cannot log in)
```

**Password input — stdin, never a flag.** `add` and `passwd` read the new
password as a single line from **stdin** (a prompt is printed to stderr only when
stdin is an interactive terminal; a piped line is read verbatim otherwise). This
keeps the secret out of the process list and shell history. The trailing newline
is stripped; an **empty** password is rejected.

**Flag placement.** Flags may appear before or after the username
(`user add carol --admin` and `user add --admin carol` both work), because the
parser re-parses around each positional.

### `cuento user add <username>`

Creates a user. `txn_perm` defaults to `none` (admins imply all permissions;
per-permission management is the admin UI). An empty `--display` falls back to
the username.

| Flag        | Default    | Meaning |
|-------------|------------|---------|
| `--admin`   | `false`    | Grant admin (implies all permissions). |
| `--display` | (username) | Display name. |
| `-db`       | `cuento.db` | Path to the SQLite database file. |

```sh
printf '%s\n' "$ADMIN_PW" | cuento user add admin --admin --display "Administrator" -db /var/lib/cuento/cuento.db
```

### `cuento user passwd <username>`

Sets a user's password (versioned `update`; the snapshot never contains the
hash, AGENTS rule 5). Fails cleanly if the username does not exist.

| Flag  | Default    | Meaning |
|-------|------------|---------|
| `-db` | `cuento.db` | Path to the SQLite database file. |

```sh
printf '%s\n' "$NEW_PW" | cuento user passwd carol -db /var/lib/cuento/cuento.db
```

### `cuento user disable <username>`

Disables a user (sets `disabled_at`; a disabled user cannot log in). Versioned
`update`; `disabled_at` is recorded in the audit snapshot.

| Flag  | Default    | Meaning |
|-------|------------|---------|
| `-db` | `cuento.db` | Path to the SQLite database file. |

```sh
cuento user disable carol -db /var/lib/cuento/cuento.db
```

**Exit codes.** 0 on success; non-zero (`log.Fatalf`) on any error — missing
username argument, empty password, unknown user, store error.

---

## `cuento check` — ledger integrity suite

Operator / one-shot. Opens the db, runs the ledger integrity suite (the
**Z1–Z19** invariant rules — zero-sum per transaction and per fund,
`current == latest version`, foreign-key check, tree acyclicity, and the rest),
prints every violation as `SEVERITY RULE: detail` in a deterministic order
(errors before warnings), then a summary line, and exits with a code that
reflects the result. This is the same gate go-live and the restore drill use.

**Synopsis**

```
cuento check [-db PATH] [--strict]
```

**Flags**

| Flag       | Default    | Meaning |
|------------|------------|---------|
| `-db`      | `cuento.db` | Path to the SQLite database file. |
| `--strict` | `false`    | Treat warnings as failures (non-zero exit on any warning). |

**Exit codes** (the one command with *designed* exit-code semantics):

| Result | Exit |
|--------|------|
| Any **error**-severity violation | 1 |
| Any violation (error **or** warning) when `--strict` | 1 |
| Clean, or only warnings without `--strict` | 0 |

An operational (db open / query) failure exits non-zero via `log.Fatalf`;
ledger violations exit non-zero **without** a spurious log line (the printed
violations are the message).

**Examples**

```sh
cuento check -db /var/lib/cuento/cuento.db            # clean → "check: clean (no violations)", exit 0
cuento check -db /tmp/restore-check.db --strict       # restore drill: warnings also fail
```

---

## `cuento ratesync` — fetch exchange rates

Operator / one-shot. Derives the currency pairs to fetch (the org base currency —
the root subsidiary's `base_currency` — against every **active** currency, minus
the identity pair), fetches them "as of today" from Yahoo Finance, writes them as
**one** change under the system actor, and prints a one-line summary. It takes no
env config of its own; point it at the same db `serve` uses. In production the
`ratesync.timer` runs it on a schedule (Mon–Fri 18:30 UTC).

**Synopsis**

```
cuento ratesync [-db PATH]
```

**Flags**

| Flag  | Default    | Meaning |
|-------|------------|---------|
| `-db` | `cuento.db` | Path to the SQLite database file. |

**Behavior notes.** A source (network) error aborts **before** any write (no
partial batch). An empty fetch is a clean no-op. An empty subsidiary tree (no
root, so no base currency) is a clear error, not a panic.

**Exit codes.** 0 on success; non-zero (`log.Fatalf`) on a fetch or store error.

**Example**

```sh
cuento ratesync -db /var/lib/cuento/cuento.db     # → "ratesync: imported N rate(s)"
```

---

## `cuento expense-report` — expense-report maintenance

Operator / one-shot **maintenance / seed** verb over the expense-report store
methods. Currently exposes one sub-subcommand, `reject`. It runs through the
write funnel as the system actor, so the rejection is a real audited, versioned
change — the CLI face of the same `store.RejectExpenseReport` the Go tests and
the reviewer web queue use.

```
usage: cuento expense-report <command> [flags]

  reject <id> --reason "<text>" [-db PATH]   reject a submitted expense report (maintenance/seed)
```

### `cuento expense-report reject <id> --reason "<text>"`

Rejects a **submitted** expense report by numeric id. The report must be in the
submitted state and `--reason` must be non-empty (both enforced by the store).
Exactly one `<id>` is required. `--reason` may be placed before or after the id.

| Flag       | Default    | Meaning |
|------------|------------|---------|
| `--reason` | (empty)    | The rejection reason (**required**, non-empty). |
| `-db`      | `cuento.db` | Path to the SQLite database file. |

**Exit codes.** 0 on success; non-zero (`log.Fatalf`) on a bad/missing id,
missing reason, wrong report state, or store error.

**Example**

```sh
cuento expense-report reject 42 --reason "duplicate submission" -db /var/lib/cuento/cuento.db
```

---

## Exit-code summary

| Situation | Exit |
|-----------|------|
| No subcommand, or unknown subcommand (usage printed to stderr) | 2 |
| `serve` / `migrate` / `user` / `ratesync` / `expense-report` error | non-zero (`log.Fatalf` → 1) |
| `check` with error violations, or any violation under `--strict` | 1 |
| Any command, success | 0 |

See [docs/deploy.md](deploy.md) for how these commands fit into a full VM
deployment (first-run migrate + admin, the ratesync timer, and the restore
drill's `check --strict`).
