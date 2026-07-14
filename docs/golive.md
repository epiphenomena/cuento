# Go-live: the two-step import deploy (D26)

The production cuento database is not hand-built — it is **produced by
`cmd/ledgerimport`** from the cleaned full-ledger CSV export plus a reviewed
mapping, then shipped. This is the D26 deployment path, rehearsed in p09.4. This
document is the runbook plus the list of best-guess mapping decisions a human must
review before cutover.

Real data never enters the repo (AGENTS rule 11): the source CSV and the two
mapping files (`mapping.json`, `mapping-accounts.csv`) live **gitignored** beside
the data under `fixtures/source/`. Only structural descriptions and aggregate
counts appear here.

## The two steps

**Step 1 — build the db and gate it.** Locally, against the gitignored source +
reviewed mapping:

```
ledgerimport build \
  -source fixtures/source/jrnl.csv \
  -map    fixtures/source/mapping-accounts.csv \
  -config fixtures/source/mapping.json \
  -o      cuento.db
cuento check --strict cuento.db
```

`ledgerimport build` migrates a fresh SQLite file, drives `internal/store` (so
every row passes the same invariants the app enforces, rule 2/7), and runs the
integrity suite itself — it exits non-zero on any Error-severity violation.
`cuento check --strict` then re-runs the suite promoting **warnings to errors**:
this is the human-review gate (below), not passable until the warning backlog
(currently the 990-code mapping) is worked or explicitly accepted.

**Step 2 — ship.** Ship the built `cuento.db` plus the `cuento` binary to the
host per `docs/deploy.md` (systemd unit + Litestream). The import is re-runnable
any time before cutover to pick up a newer CSV; a re-run is a fresh `-o` file
(the builder refuses to overwrite an existing db).

## Additive per-subsidiary import (scaffold + import-subsidiary)

`build` above imports every subsidiary at once into a fresh db. To bring
subsidiaries live **one at a time** — import the US side, prove/debug it in
production for a while, then import Honduras later into the same running db — use
the split path instead. It has the same invariants; it just separates the
subsidiary-independent reference data (created once) from each subsidiary's
transactions (added independently).

```
# Once: scaffold the reference db (subsidiaries, program tree, funds, the WHOLE
# account chart incl. FX Clearing + Opening Balances, and rates). No transactions.
ledgerimport scaffold -map ... -config ... -rates ... -o cuento.db
cuento check --strict cuento.db          # clean; 0 transactions

# Import ONE subsidiary's transactions additively (repeat later per subsidiary).
# ALWAYS back up first — the importer is row-by-row with no rollback; recovery is
# restore-from-backup, not transaction abort.
cp cuento.db cuento.db.bak
ledgerimport import-subsidiary -source ... -map ... -config ... -subsidiary UPLAM -o cuento.db
cuento check --strict cuento.db          # green -> keep; red -> cp cuento.db.bak cuento.db
# ... run/prove UPLAM live ...
ledgerimport import-subsidiary -source ... -map ... -config ... -subsidiary UPH -o cuento.db
```

`make scaffold-db` and `make import-sub IMPORT_SUB=UPLAM` wrap this with the
backup/restore safety net (stop the server first — it holds the db file open).

Semantics:
- **Scaffold is created once and only looked up afterward.** A per-subsidiary
  import creates NO shared entities; it resolves subsidiaries, programs, funds and
  accounts from the db and **fails loud** if the scaffold is missing or was built
  from a different mapping. Run `scaffold` before any `import-subsidiary`.
- **Re-import is refused.** Importing a subsidiary that already has transactions
  errors out. To redo a subsidiary, start over with a fresh `scaffold`.
- **Per-subsidiary reconciliation.** After each import the tool prints the native
  (per-currency, per-account-type) trial-balance totals to compare against the
  source books, and fails if a subsidiary's posted splits do not net to zero per
  currency. A cross-currency intercompany transfer still decomposes through FX
  Clearing (D3) even though its two legs import under different subsidiaries.
- **Consolidated reports show only the imported side(s)** until every subsidiary is
  imported — expected during the prove period, not a bug.

## Current rehearsal result (p09.4, best-guess mapping, non-strict)

`cuento check -db fixtures/sample.db`:

```
0 error(s), 151 warning(s)
```

Warning tally by rule:

| Rule | Count | Meaning |
|------|-------|---------|
| Z19  | 151   | active revenue/expense leaf with activity but no effective Form 990 code (D25) — every R/E leaf that carries splits. Expected: no 990 codes are mapped yet (see guesses). |

No other warning rule fires (Z17 intercompany and Z18 restricted-fund overspend
are vacuously clean: no accounts are flagged intercompany and no funds are
created yet). **Zero Error-severity violations** — the non-strict go-live goal is
met. `--strict` currently fails **only** on the 151 Z19 warnings; it passes once
990 codes are assigned (or the warnings are accepted).

### Build aggregates

- **3 subsidiaries** (1 root + 2 operating), **1 program** (the seeded root
  "General"), **0 funds**, **293 accounts** (232 leaf source accounts + 59 parent
  grouping placeholders + 2 synthetic import accounts).
- **~42.1k source transaction groups → ~49.5k cuento transactions.** Of the
  source groups: ~34.6k are single-currency (one transaction each) and ~7.6k are
  multi-currency, **decomposed through the FX Clearing account** into paired
  single-currency transactions (D3) — this decomposition is what produces the
  extra ~7.4k transactions.
- **~1.0k source groups did not net to zero** in their single currency (opening
  balances / adjustments / export artifacts, ~1.0k of which are single-split
  groups). Their residual is **routed to the Opening Balances equity account per
  subsidiary and counted as a warning** in the build log — never force-balanced
  onto an existing split (docs/ledger-export.md hazard #4). Every produced
  transaction still balances overall and per fund, so `cuento check` is
  Error-clean.
- Two currencies posted (USD, HNL); the consolidation-marker rows (~200 splits)
  are skipped entirely.

## Mapping guesses to review before cutover

Every item below was a **best guess** made autonomously for the rehearsal. A
human must confirm or correct each before cutover. Descriptions are structural —
no real names/values.

1. **Subsidiary names (GUESS).** The root org and the two operating subsidiaries
   were given placeholder English names. Base currencies are anchored: root =
   USD, one operating entity = USD, the other = HNL (Honduran Lempira). Review
   the three names; the currency assignment follows the source and should stand.

2. **HNL currency seed (STRUCTURAL, committed).** The source posts in a local
   currency (HNL) that the p03.1 seed did not include; a new forward migration
   seeds it (ISO 4217 exponent 2). Not a guess about the data — a required
   prerequisite the rehearsal surfaced — but noted so the reviewer knows the
   currency table changed.

3. **Functional-class mapping (GUESS, D21).** The four source functional-class
   codes map to the fixed enum as: two program-like codes → `program`, an
   administration code → `management`, a development code → `fundraising`. Every
   expense split in the source carries one of these codes (100% coverage on
   non-consolidation rows), so no expense split relies on an account default.
   Confirm the four-way mapping — in particular which of the two program-like
   codes is truly a program activity vs. something else.

4. **Program handling (GUESS — simplest option, D24).** The source's ~7 program/
   department codes are **not** mapped to programs; all revenue/expense splits
   land on the seeded root program "General". This is the simplest Error-clean
   choice and loses the program dimension for 990 Part-VIII/IX-by-program
   reporting. **Alternative to consider:** map each source department code to a
   named program under "General" (the importer supports this via the config
   `programs` map + per-account `default_program`), which would restore
   program-level reporting. The importer now correctly applies a program only to
   revenue/expense splits, so enabling this is a config-only change.

5. **Funds = none (GUESS, D20).** No funds are created; all activity is
   unrestricted (NULL fund). The source marks a donor/restriction on ~41% of
   splits, so a large restricted-activity population exists but is **not** yet
   modelled as funds — funds are created only when the human names the donors
   and their subsidiary/program scopes. Until then, fund-level conservation (Z10)
   is vacuously satisfied (it collapses into the whole-transaction check Z1).

6. **Form 990 codes = unmapped (GUESS, D25).** No `form990_code` is set on any
   account. Every active R/E leaf with activity therefore has no effective 990
   code → the 151 Z19 warnings. Reports render these in an explicit "Unmapped"
   bucket rather than dropping them. Assign codes at ~10 natural parent accounts
   (codes are inheritable) to clear the warnings and enable the 990 reports.

7. **Account tree fixes (STRUCTURAL, GUESS on placeholders).** The source names
   59 distinct parent grouping paths that are **not** themselves posting accounts.
   Each was materialized as a **placeholder account** (holds no splits) whose
   type is its children's single type (the source's groupings are type-pure — no
   parent mixes families) and whose subsidiary set is the union of its children's
   subsidiaries (satisfying the parent ⊇ children superset invariant, Z12). One
   source leaf appears only under the consolidation marker and has no real
   subsidiary; it was mapped to the root subsidiary as a fallback and receives no
   splits. Review the placeholder names and that fallback account.

8. **Consolidation rows skipped (GUESS, hazard #3).** Rows carrying the
   consolidation-marker country (blank currency/amounts) are dropped entirely
   (`skip_countries`), not treated as elimination entries. Confirm these are
   non-postings.

9. **Imbalance strategy (STRUCTURAL, D22/hazard #4).** Single-currency source
   groups that do not net to zero (~1.0k) route their residual to the per-
   subsidiary Opening Balances equity account and are surfaced as counted
   warnings in the build log. No `opening_balance_typs` are configured, so every
   such residual is treated uniformly as an opening/adjustment posting to Opening
   Balances. Review the build-log warnings (local, gitignored — values stay in
   `fixtures/source/mapping-notes.md`) to confirm each is a genuine opening
   balance or adjustment and not a data error worth fixing at source.

10. **Payees = none (GUESS).** `payee_column` is empty, so no payees are created
    (the source's memo column is long/multi-line and would mint thousands of junk
    payees). If a cleaner payee-ish source column exists, set it in the config.

### Needs human decision (blocks `--strict`, not the non-strict goal)

- **990 code assignment** (item 6) is the only thing standing between the current
  build and a clean `cuento check --strict`. It is a mapping decision, not an
  importer defect.
- **Spreadsheet spot-check** (D26 gate): balances in the produced db must be
  reconciled against the current reporting spreadsheet before cutover. Not yet
  done — awaits human review.
