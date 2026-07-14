# Ledger export format — `fixtures/source/jrnl.csv`

Structure reference for the historical import (D22, D26). This documents the **shape** of the
cleaned full-ledger CSV so `cmd/ledgerimport` (p09.3) and the production mapping (p09.4) can be
written. Per AGENTS rule 11 it contains **no data values** — no account names, donor names,
transaction descriptions, or amounts. Org-specific code vocabularies (the actual account paths,
donor/fund names, program/department codes, functional-class labels) live only in the gitignored
`fixtures/source/mapping.yaml`, never here or in code/tests.

The synthetic `testutil.Fixture` (Appendix D) — **not** this file — is what tests and goldens use.

## Provenance

The export is an already-double-entry general journal (one row per posting line / "split"),
covering roughly a decade of activity for a small multi-subsidiary nonprofit. It is not a raw
bank feed: accounts, debits/credits, currencies, exchange rates, functional classes, donors, and a
reconciliation flag are already present. The importer's job is to *convert* it into cuento's model,
not to re-key it.

## File format

- **Encoding:** UTF-8, no BOM.
- **Dialect:** RFC 4180 CSV, comma-delimited, double-quote quoting. Quoted fields contain both
  **embedded commas** and **embedded newlines** (memo text spans multiple physical lines), so a
  naive line-splitter miscounts — use a real CSV reader (Go `encoding/csv`, which handles this).
- **Header:** first row is the column header (see below).
- **Records:** under a proper CSV parse every data record has exactly **22 fields**. Physical line
  count (~130k) exceeds record count because of multi-line quoted memos.
- **Scale:** ~130k split records grouped into ~42k transactions; transaction dates span
  ~2016-07 to ~2026-07 (`YYYY-MM-DD`).

## Record model

Each row is one **split**. The `tid` column groups splits into a **transaction** (a posting).
Most transactions have 2 splits; some are large compound entries (up to ~188 splits). A handful of
`tid` groups have a single split (see Hazards). A transaction's splits share `dt`, `tid`, and
usually `currency`; each split carries its own account, amounts, class, donor, and cleared flag.

## Columns (22)

Order: `country, stmt, typ, acct, kat, dt, v, ndb, fndb, kls, klass, tid, desc, donor, currency, xrt, db, fdb, cr, fcr, clr, parent`

| Column | Role | Type / format | Notes |
|---|---|---|---|
| `country` | **Subsidiary** dimension | short code, 3 distinct | two operating entities + one consolidation marker (blank-currency rows). Maps to the subsidiary tree in mapping.yaml. |
| `stmt` | **Account super-type** | single char, 5 distinct | A / L / I / E / O = asset / liability / income(→revenue) / expense / other(→equity). Drives cuento `type`. |
| `typ` | Entry/posting type | short text, ~30 distinct | classification of the journal entry; informational for the mapping. |
| `acct` | **Account** | text path, up to ~110 chars, ~232 distinct | hierarchical account name/path (the leaf). Values are org data → mapping.yaml only. |
| `parent` | Account parent | text path, ~60 distinct | parent grouping for `acct`. **Superseded (p26.12):** the account tree is now DERIVED from `stmt`+`typ` (see "Account tree derivation"); this column is IGNORED for structure. |
| `kat` | **Program / department** dimension | short code, 8 distinct | ~83% populated. Maps to the program tree (D24) in mapping.yaml. |
| `dt` | Transaction date | `YYYY-MM-DD`, always 10 chars | already ISO; maps straight to `transactions.date`. |
| `kls` | **Functional class** | short text, 4 distinct (+blank) | maps to `program|management|fundraising` (D21); the 4th code is folded in mapping.yaml. |
| `klass` | Sub-class / detail | text, ~98 distinct | finer class/category text; informational. |
| `donor` | **Fund / restriction** | text, ~1968 distinct | ~41% populated → a present donor marks restricted activity (a fund, D20); blank = unrestricted. Values → mapping.yaml. |
| `currency` | Split currency | 3-letter ISO 4217, 2 real codes | the org's functional currency plus one local currency; blank on consolidation rows. |
| `tid` | **Transaction group id** | integer, ~42k distinct | groups splits into one posting. |
| `desc` | Memo / **split description** | free text, up to ~540 chars, may be multi-line | ~75% populated. Values (may contain PII) → never copied; each source row's `desc` becomes THAT split's per-split `description` (p26.16, payee→description migration) as well as its memo. No payees are minted. |
| `clr` | **Reconciliation** flag | single char (R / C / blank) | R = reconciled, C = cleared, blank = uncleared. Feeds the p16 reconciliation import. |
| `xrt` | Exchange rate | decimal (1.0 or ~6 dp) | relates the base and native amount pairs; 1.0 when the split is in the base currency. |

### Amount columns (see "Amounts" below)

| Column | Meaning | Reliability |
|---|---|---|
| `db`, `cr` | debit / credit in one currency (each always ≥ 0; one of the pair is zero) | **authoritative** — 1–2 real decimals |
| `fdb`, `fcr` | debit / credit in the other currency of the base/native pair | **authoritative** — 1–2 real decimals |
| `ndb` | net-debit (`db − cr`), pre-computed | **derived, lossy** — carries binary-float rounding noise (up to ~18 decimal places); do NOT use for exact amounts |
| `fndb` | foreign net-debit, pre-computed | **derived, lossy** — same float-noise caveat |
| `v` | a pre-computed value column | **derived, lossy** — float noise; informational only |

## Amounts

- Numbers use a **`.` decimal point and no thousands separator**; real precision is **≤ 2 decimal
  places**. Convert to cuento's `int64` minor units by the currency's exponent (D1).
- **Use `db`/`cr` (and `fdb`/`fcr`) — never `ndb`/`fndb`/`v`** for exact values: the pre-computed
  net columns were produced by floating-point subtraction and carry rounding artifacts (10–18
  spurious decimals). The signed net-debit for a split is `db − cr` computed exactly from the
  authoritative columns.
- Each split carries **two** amount pairs — a base/functional-currency pair and a native-currency
  pair — related by `xrt`. When the split's `currency` equals the base currency the two pairs are
  equal and `xrt = 1`. Which pair is native vs base per row is resolved in p09.3/p09.4 against the
  `currency` column and `xrt`.

## Dimensional mapping (summary — details in mapping.yaml, p09.4)

| Source | → cuento |
|---|---|
| `country` | subsidiary (D18) |
| `stmt` | account `type` (A/L/I/E/O → asset/liability/revenue/expense/equity) |
| `acct` + `stmt` + `typ` | account tree + leaf; two-level parent chain (see below); `type` from `stmt`; subsidiaries from `country` set. (`parent` COLUMN superseded, p26.12) |
| `kat` | program (D24) |
| `kls` | functional class on expense splits (D21) |
| `donor` | fund (D20); blank → unrestricted |
| `currency` | transaction currency (D3) |
| `db`/`cr` (+ `fdb`/`fcr`, `xrt`) | split amount in minor units (D1) |
| `dt` | transaction date |
| `desc` | per-split `description` + memo (p26.16; no payees minted) |
| `clr` | reconciliation state (imported in the p16 pass) |

## Account tree derivation (`stmt` + `typ`, p26.12)

The account hierarchy is DERIVED from `stmt` and `typ` as a deterministic
**two-level chain**, not from the explicit `parent` column:

    <stmt super-parent>  ->  <(stmt,typ) intermediate>  ->  <leaf acct>

e.g. `stmt=A`, `typ="Bank"`, `acct="BOA Checking"` becomes **Assets → Bank → BOA
Checking**. The `accounts` skeleton (`cmd/ledgerimport/accounts.go`) synthesizes
the intermediate and super-parent rows into the reviewable account-mapping CSV, so
`build` (which sees only the reviewed CSV, no `typ`) creates the tree
parent-before-child with no extra logic.

- **stmt → super-parent name:** `A/L/I/E/O → Assets/Liabilities/Revenue/Expenses/
  Equity` (`stmtToSuperParent`), mirroring the report section headers. The
  super-parent's cuento `type` is the stmt's type (`stmtToType`).
- **Intermediates are keyed by `(cuento-type, typ)`**, not `typ` alone, so the same
  `typ` under two different `stmt` supertypes produces two distinct tiers (e.g. an
  asset "Bank" tier ≠ an expense "Bank" tier). This also enforces **type
  consistency**: a leaf always nests under an intermediate of its OWN stmt type
  (D26: prefer the leaf's own stmt).
- **Synthetic parent rows are namespaced** (`::super:` / `::typ:` prefixes in the
  CSV `source_acct` key), so a real leaf `acct` that happens to match a `typ` value
  or a super-parent name never collides with a synthetic tier. The human-facing
  `name_en`/`name_es` are the clean names ("Assets", the raw `typ`).
- **Subsidiary unions (two-pass):** each synthetic parent carries the UNION of the
  subsidiaries of the leaves ACTUALLY parented under it (rule 7 / Z12: a parent's
  subsidiary set ⊇ every child's). This matters because `typ` is documented as a
  **per-journal-entry** classification, so one `acct` recurs under many `typ` values
  and accrues countries from all of them; the derivation fixes each leaf's parent
  chain at its **first sighting** (file order — deterministic) but then propagates
  the leaf's FULL country union up that chain, so a leaf spanning several typs never
  leaves a country only on itself. **Caveat for review:** because `typ` is
  per-journal-entry (not an account attribute), which `typ` names a leaf's parent is
  semantically arbitrary; the go-live human review should confirm the resulting tier
  names make sense (the task's `stmt=A,typ=Bank` example instead treats `typ` as an
  account subtype — the two readings are surfaced here).
- **Edge cases:** a blank `typ` parents the leaf DIRECTLY under the super-parent
  (the intermediate tier is skipped); a `typ` whose text equals the super-parent's
  own name collapses the tier (no self-parent); a blank/unknown `stmt` leaves the
  leaf top-level for the human to place in review; a leaf `acct` literally NAMED like
  a super-parent or a `typ` value coexists with the synthetic tier without collision
  (the synthetic rows use the reserved `::super:`/`::typ:` keys, the leaf keeps its
  own name), and stands as an ordinary leaf under its stmt/typ chain.

The explicit `parent` column is **ignored** for structure (simplest correct
behavior — the stmt/typ chain is authoritative; the go-live mapping gets human
review either way).

## Hazards & quirks (for p09.3 parser and p09.4 mapping)

1. **Multi-currency transactions.** ~7.5k `tid` groups span more than one currency. cuento
   transactions are single-currency (D3); these must be **decomposed into paired transactions
   through an FX Clearing account** at import, using `xrt`. This is the main structural
   transformation.
2. **Float-noise net columns.** `ndb`/`fndb`/`v` are unusable for exact math (see Amounts). Recompute
   the net-debit from `db`/`cr`.
3. **Consolidation rows.** A small set of rows carry the consolidation `country` marker and a blank
   `currency` (and blank amounts). Decide in mapping.yaml whether to skip them or treat them as
   elimination context — they are not ordinary postings.
4. **Single-split and non-balancing transactions.** A minority of `tid` groups have one split, and a
   small fraction of single-currency groups do not net to zero on `db − cr` (opening balances,
   adjustments, or export artifacts). The importer must reconcile each produced transaction to
   zero (overall and per fund, D2/D20) — surface non-balancing source groups for human review
   rather than silently forcing them.
5. **Large compound entries.** Some transactions have many dozens of splits (up to ~188); the
   importer and any preview UI must not assume small split counts.
6. **Multi-line, long memos.** `desc` may be several hundred characters and contain newlines; carry
   it verbatim into the memo, and never echo its contents anywhere versioned.
7. **Functional-class and account-type coverage.** `kls`/`kat`/`donor` are partially populated;
   R/E splits still need a program (default to the root, D24), expense splits need a functional
   class (default from the account, D21), and unmapped source accounts must be caught by the
   mapping review, not defaulted silently.
8. **Long history.** ~10 years of dates; opening balances predate the earliest activity and are
   posted via `Equity:Opening Balances` per subsidiary (D22).

## Downstream

- **p09.3** builds `cmd/ledgerimport`: `accounts` emits a reviewable `mapping.yaml` skeleton from
  the distinct `acct`/`parent`/`country`/`kat`/`donor`/`kls` values; `build` converts rows →
  subsidiaries, programs, funds, accounts, opening balances, and transactions (single- and
  multi-currency-via-FX-clearing) per the mapping — each split carries its source row's `desc` as
  its per-split `description` (p26.16); no payees are minted. Parsers are unit-tested against **synthetic**
  lines shaped like this spec — never against real values.
- **p09.4** iterates `fixtures/source/mapping.yaml` (gitignored, beside the data) with the human
  until `ledgerimport build → cuento check --strict` is clean and spot-checked balances match the
  current reporting spreadsheet (the go-live gate, D26).
