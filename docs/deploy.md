# Deploying cuento

cuento is one CGO-free binary plus one SQLite file. Production (D8) is a single
Google Compute Engine **e2-micro always-free VM** running the server under
systemd, with **Litestream** streaming the database to **Google Cloud Storage**
(free 5 GB) for disaster recovery, and TLS terminated in-process via `autocert`
(Let's Encrypt). No load balancer, no container, no managed database.

This document is a runnable walkthrough. Every hostname, bucket, and project name
below is a **placeholder** (`books.example.com`, `your-bucket`, `your-project`) —
substitute your own. It stores no secrets: GCS access uses the VM's attached
service account, and TLS certs are provisioned on first request.

The unit files referenced here live in [`deploy/`](../deploy):
`cuento.service`, `litestream.service`, `litestream.yml`, `ratesync.service`,
`ratesync.timer`. Every `cuento` subcommand and flag used below is documented in
[docs/cli.md](cli.md).

---

## 1. Create the always-free VM

The GCE always-free tier grants one `e2-micro` in one of three US regions:
`us-west1`, `us-central1`, or `us-east1`. Pick one and stay in it (the free tier
is region-bound).

```sh
gcloud compute instances create cuento \
  --project=your-project \
  --zone=us-central1-a \
  --machine-type=e2-micro \
  --image-family=debian-12 --image-project=debian-cloud \
  --boot-disk-size=30GB --boot-disk-type=pd-standard \
  --scopes=storage-rw
```

- **30 GB `pd-standard`** is the free-tier disk allowance (standard persistent
  disk, not SSD). Ample for the db plus WAL, autocert cache, and logs.
- **`--scopes=storage-rw`** lets the VM's default service account write to GCS
  via Application Default Credentials — this is why no key file appears in
  `litestream.yml`. (You can instead attach a dedicated service account with the
  `roles/storage.objectAdmin` role scoped to the one bucket.)

### Firewall: open 80 and 443

autocert needs inbound **80** (ACME http-01 challenge + the HTTPS redirect) and
**443** (HTTPS). Nothing else should be exposed; SSH goes through GCP's default
rules or IAP.

```sh
gcloud compute firewall-rules create cuento-web \
  --project=your-project \
  --allow=tcp:80,tcp:443 \
  --target-tags=cuento-web --direction=INGRESS
gcloud compute instances add-tags cuento --zone=us-central1-a --tags=cuento-web
```

Point your domain's A record at the VM's external IP (reserve a static IP so it
survives a stop/start) **before** first start, so autocert can validate it.

---

## 2. Create the GCS backup bucket

```sh
gcloud storage buckets create gs://your-bucket \
  --project=your-project --location=us-central1 \
  --uniform-bucket-level-access
```

Keep the bucket in the same region as the VM (egress within a region is free)
and well under 5 GB — for a db of this scale, a month of Litestream snapshots is
a few hundred MB at most.

---

## 3. Install the binaries

Build the release binary locally (`make release` → CGO-free linux/amd64,
`-trimpath`, version stamped from `git describe`) and copy it up, then install
Litestream from its release.

```sh
# local
make release                       # produces ./bin/cuento
gcloud compute scp bin/cuento cuento:/tmp/cuento --zone=us-central1-a

# on the VM
sudo install -m 0755 /tmp/cuento /usr/local/bin/cuento

# Litestream (external tool; see https://litestream.io for the current release)
curl -fsSL -o /tmp/litestream.deb \
  https://github.com/benbjohnson/litestream/releases/latest/download/litestream-linux-amd64.deb
sudo dpkg -i /tmp/litestream.deb   # installs /usr/local/bin/litestream
```

### Create the service user and data directory

The units run as a dedicated non-root `cuento` user. The data dir
(`/var/lib/cuento`, matching `CUENTO_DATA_DIR`) holds the SQLite db, the autocert
certificate cache (`autocert/`), and Litestream's local state.

```sh
sudo useradd --system --home-dir /var/lib/cuento --shell /usr/sbin/nologin cuento
sudo install -d -o cuento -g cuento -m 0750 /var/lib/cuento
```

---

## 4. First-run: migrate, create the admin, provision TLS

`cuento serve` **auto-migrates on start** (backing the db up first once schema
exists — AGENTS rule 4), so a fresh db needs no separate migrate step. But you do
need a first admin user, and `serve` refuses no one — it just logs a bootstrap
hint until a human user exists. Create the schema and the admin as the `cuento`
user so the file ownership is right:

```sh
# Create the schema explicitly (optional; serve would do it too).
sudo -u cuento cuento migrate -db /var/lib/cuento/cuento.db

# Create the first admin. The password is read from STDIN (never a flag/argv,
# so it never lands in the process list or shell history).
sudo -u cuento sh -c 'printf "%s\n" "$ADMIN_PW" | cuento user add admin --admin --display "Administrator" -db /var/lib/cuento/cuento.db'
```

Now install and start the server unit. Edit `CUENTO_DOMAIN` in
`deploy/cuento.service` to your real hostname first.

```sh
sudo cp deploy/cuento.service /etc/systemd/system/cuento.service
sudo systemctl daemon-reload
sudo systemctl enable --now cuento.service
journalctl -u cuento -f
```

On the **first HTTPS request** to `https://books.example.com`, autocert obtains a
Let's Encrypt certificate over the :80 ACME http-01 challenge and caches it in
`/var/lib/cuento/autocert/`. Subsequent restarts reuse the cache (so you don't
hit Let's Encrypt rate limits). Plain HTTP on :80 serves the challenge and
otherwise 301-redirects to HTTPS.

> **Config recap.** The server is configured entirely by `CUENTO_*` env vars in
> the unit, overridable by flags: `CUENTO_DATA_DIR` (data dir),
> `CUENTO_DOMAIN` (set ⇒ TLS on :443 + :80 redirect; unset ⇒ plain HTTP on
> `CUENTO_ADDR`), `CUENTO_ADDR`, `CUENTO_DEV` (**never** set in production —
> it disables the Secure cookie flag). Env is the base; a flag of the same name
> overrides it.

### Binding :80/:443 as non-root

`cuento.service` grants `CAP_NET_BIND_SERVICE` (Ambient + bounding set) with
`NoNewPrivileges=true`, so the non-root `cuento` user can bind the privileged
ports directly — no proxy needed. If your policy forbids that capability, drop
those two lines, run plain HTTP on a high `CUENTO_ADDR` (e.g. `:8080`, unset
`CUENTO_DOMAIN`), and put nginx/Caddy in front for TLS.

---

## 5. Litestream replication

Install the config and unit, then start replication:

```sh
sudo cp deploy/litestream.yml /etc/litestream.yml       # edit `bucket:` first
sudo cp deploy/litestream.service /etc/systemd/system/litestream.service
sudo systemctl daemon-reload
sudo systemctl enable --now litestream.service
litestream replicas /var/lib/cuento/cuento.db           # should list the gcs replica
```

Litestream reads the WAL (cuento runs in WAL journal mode) and streams frames to
GCS every ~10s, snapshotting daily with a 30-day retention window
(`snapshot-interval: 24h`, `retention: 720h` in `litestream.yml`). That keeps a
month of restore points comfortably inside the 5 GB free tier for a db this size.

---

## 6. Restore drill (rehearse this before you need it)

A backup you have never restored is a hope, not a backup. Practice the full
restore into a scratch path — it does not touch the live db:

```sh
# Restore the latest replicated state from GCS into a scratch file.
sudo -u cuento litestream restore \
  -config /etc/litestream.yml \
  -o /tmp/restore-check.db \
  /var/lib/cuento/cuento.db

# Verify the restored file is a sound cuento db: SQLite integrity + the ledger
# invariant suite in strict mode (errors AND warnings fail the drill).
sqlite3 /tmp/restore-check.db 'PRAGMA integrity_check;'   # expect: ok
sudo -u cuento cuento check -db /tmp/restore-check.db --strict
rm -f /tmp/restore-check.db
```

`cuento check --strict` runs the Z1–Z19 integrity rules (zero-sum per txn and per
fund, current==latest version, foreign-key check, tree acyclicity, …) and exits
non-zero on any error **or** warning — the same gate go-live uses (D26).

### Real disaster recovery

To recover onto a fresh (or the same) VM after data loss:

```sh
sudo systemctl stop cuento litestream          # stop writers first
sudo -u cuento litestream restore -config /etc/litestream.yml \
  -o /var/lib/cuento/cuento.db /var/lib/cuento/cuento.db
sudo -u cuento cuento check -db /var/lib/cuento/cuento.db --strict
sudo systemctl start cuento litestream
```

Restore to a **point in time** with `-timestamp 2026-07-01T12:00:00Z` (must be
within the retention window).

---

## 7. Exchange-rate sync timer

Install the rate-sync timer (the p14.2 deferred timer) so
`cuento ratesync` runs on a schedule (Mon–Fri 18:30 UTC):

```sh
sudo cp deploy/ratesync.service deploy/ratesync.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now ratesync.timer
systemctl list-timers ratesync                 # confirm the next run
sudo systemctl start ratesync.service          # optional: run it once now
```

---

## 8. Backup retention & operations summary

| Concern            | Setting                                                            |
|--------------------|-------------------------------------------------------------------|
| Replication RPO    | ~10 s (`sync-interval: 10s`)                                       |
| Snapshot cadence   | daily (`snapshot-interval: 24h`)                                   |
| Retention window   | 30 days (`retention: 720h`), checked every 12 h                   |
| Backup storage     | GCS `your-bucket`, same region as the VM, < 5 GB (free tier)       |
| Restore verified   | by the drill in §6 (`PRAGMA integrity_check` + `check --strict`)   |
| TLS certs          | autocert, cached in `/var/lib/cuento/autocert/`, auto-renewed      |

Routine checks:

- `systemctl status cuento litestream ratesync.timer`
- `journalctl -u cuento --since -1h`
- `litestream replicas /var/lib/cuento/cuento.db` (replication healthy)
- Run the restore drill (§6) periodically — monthly is reasonable.

Upgrades: build a new binary (`make release`), copy it over
`/usr/local/bin/cuento`, and `sudo systemctl restart cuento` — the auto-migrate
on start applies any new schema (backing up the db file first). Migrations are
**forward-only and idempotent** (AGENTS rule 4): restarting the new binary
re-runs the migrator, which applies only what is pending and is a no-op when the
schema is already current, so an upgrade never needs a manual `migrate` step and
a restart is safe to repeat. Litestream keeps replicating across the restart; no
Litestream action is needed for an upgrade.

---

## 9. Hosting the public demo (auto-resetting, synthetic)

The `cuento demo` subcommand generates a **fully synthetic**, richly-populated
database so prospective users can click around a live instance without any real
data. It is a fictional multi-subsidiary nonprofit ("Rio Verde Internacional")
with a full chart of accounts across all five types, restricted funds (including
a multi-currency capital campaign), several years of multi-currency
transactions, a sample budget with lines, expense reports in draft / submitted /
posted states, a finalized **and** an in-progress reconciliation, a bank-import
mapping profile with a staged batch, bilingual (en/es) account names, and three
demo logins across permission levels. Every value is invented (AGENTS rule 11),
so the result is **safe to host publicly**.

> **Never point the demo at real data.** The demo host runs its own unit
> (`cuento-demo.service`) with its own DEMO-ONLY data dir
> (`/var/lib/cuento-demo`). Do **not** run `cuento demo` against a data dir a
> real-data server uses, and do not co-host the demo and a real instance in the
> same data dir. The demo keeps the full production security posture — it does
> **not** set `CUENTO_DEV` (rule 13).

The generator is **deterministic** (fixed seed dates, no `time.Now`, no network):
repeat runs produce identical business data. The one non-reproducible surface is
the argon2id password salts on the seeded users — the passwords below are stable,
only the stored hashes differ run-to-run.

### Demo login credentials

| Username    | Password           | Role                                                        |
|-------------|--------------------|-------------------------------------------------------------|
| `admin`     | `demo-admin-2026`  | administrator (full access)                                 |
| `submitter` | `demo-submit-2026` | expense submitter (write, can submit)                       |
| `viewer`    | `demo-view-2026`   | read-only viewer                                            |
| `campdir`   | `demo-camp-2026`   | program-scoped viewer (financial reports, Educacion subtree only) |

These are DEMO-ONLY credentials for a throwaway, auto-resetting database — they
are printed by `cuento demo` on generation and defined once in
`internal/synth` so this table cannot drift from what the generator seeds.

### Generate, serve, auto-reset

Install the binary (§3) and create a **demo** service user + data dir, mirroring
the real one but under `/var/lib/cuento-demo`:

```sh
sudo useradd --system --home-dir /var/lib/cuento-demo --shell /usr/sbin/nologin cuento
sudo install -d -o cuento -g cuento -m 0750 /var/lib/cuento-demo
```

Generate the first demo db (as the `cuento` user so ownership is right), then
install and start the demo server unit (edit `CUENTO_DOMAIN` first):

```sh
sudo -u cuento cuento demo -o /var/lib/cuento-demo/cuento.db
sudo cp deploy/cuento-demo.service /etc/systemd/system/cuento-demo.service
sudo systemctl daemon-reload
sudo systemctl enable --now cuento-demo.service
```

**Auto-reset** so visitor edits never persist: install the reset timer. It is a
deliberately simple, robust **external regenerate-and-restart** (an in-process
file swap under a running server's open connection pool + live sessions is
fragile). The `cuento-demo-reset.service` one-shot stops the server, regenerates
the db from scratch (`cuento demo -o … -force`, overwriting the previous file and
its `-wal`/`-shm`), and starts the server again; `cuento-demo-reset.timer` fires
it hourly (tune `OnUnitActiveSec`):

```sh
sudo cp deploy/cuento-demo-reset.service deploy/cuento-demo-reset.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now cuento-demo-reset.timer
systemctl list-timers cuento-demo-reset        # confirm the next reset
sudo systemctl start cuento-demo-reset.service # optional: reset once now
```

Each reset is a sub-second server blip; between resets, visitors edit freely and
every hour the database returns to the pristine synthetic set. The demo does not
need Litestream (there is nothing to back up — it regenerates from code) and does
not need the ratesync timer (rates are seeded synthetically).

---

For the full list of subcommands and flags used above (`serve`, `migrate`,
`user add`, `check --strict`, `ratesync`, `demo`), see [docs/cli.md](cli.md).
