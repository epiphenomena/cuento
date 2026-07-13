-- p08.2: transaction/split/payee operations (D2, D18, D20, D21, D24). All SQL for
-- the store's transaction methods lives here (rule 6). This copies the
-- version-append convention established in subsidiaries.sql (p04.2) and the
-- snapshot-from-live pattern: the entity op does the live write inside the
-- funnel's fn, then appends a snapshot-from-live version row, so each version row
-- is byte-identical to its live row (Z3 can never diverge) and valid_from ==
-- changes.at BY CONSTRUCTION.
--
-- transactions/splits use SOFT-DELETE only for the header (rule 14): DeleteTransaction
-- flips transactions.deleted and appends a transactions_versions op='delete'; the
-- splits are left in place (the as-of query excludes the txn by its own delete row).
-- An UpdateTransaction split REMOVAL is a hard live DELETE of that split row plus a
-- splits_versions op='delete' (captured BEFORE the live delete, snapshot-from-live).
--
-- Query names are DISTINCT across the whole sqlc package.
--
-- NOTE: keep every comment and identifier in this file PURE ASCII. sqlc v1.31.1
-- miscounts byte offsets when a query file contains multi-byte UTF-8, corrupting
-- the generated SQL for the WHOLE file (see docs/DECISIONS.md p04.2).

-- ---------------------------------------------------------------------------
-- payees (minimal; autocomplete is p12.3)
-- ---------------------------------------------------------------------------

-- name: InsertPayee :one
-- Live insert of a payee. name is UNIQUE COLLATE NOCASE (schema). Returns the id.
INSERT INTO payees (name, active)
VALUES (?, 1)
RETURNING id;

-- name: GetPayee :one
SELECT id, name, active FROM payees WHERE id = ?;

-- name: GetPayeeByName :one
-- Look up a payee by name for find-or-create (p12.3 create-on-save). payees.name is
-- UNIQUE COLLATE NOCASE, so the equality is case-insensitive: "Acme" finds "acme".
-- Returns no rows when the name is new (the caller then inserts).
SELECT id, name, active FROM payees WHERE name = ?;

-- name: ListPayees :many
-- Every payee (id -> name), for the register's payee-name lookup (p12.1). The
-- payee set is tiny; the store loads it once per page render into an id->name map
-- rather than joining per row. Ordered by id for determinism.
SELECT id, name, active FROM payees ORDER BY id;

-- name: SuggestPayees :many
-- Autocomplete ranking (p12.3): active payees whose name PREFIX-matches the query
-- (case-insensitive; payees.name is COLLATE NOCASE), ordered MOST-RECENT-FIRST by
-- the payee's latest NON-DELETED transaction date. Payees never used, or with only
-- deleted transactions, have a NULL max date and sort LAST, then by name for a
-- deterministic tail. The prefix is the caller-built LIKE pattern (query + '%'),
-- passed once; the store escapes LIKE metacharacters (% _) in the raw query before
-- appending the wildcard, so a literal % / _ in the query is not treated as a
-- wildcard (sqlc's parser rejects an explicit ESCAPE clause, so escaping is done in
-- Go against the default backslash-free LIKE -- see the store). Payees sharing the
-- same latest date (or all in the never-used/only-deleted tail) tiebreak by name
-- (COLLATE NOCASE), then id, for a deterministic order.
SELECT p.id, p.name,
       MAX(t.date) AS last_date
FROM payees p
LEFT JOIN transactions t
  ON t.payee_id = p.id AND t.deleted = 0
WHERE p.active = 1 AND p.name LIKE ?
GROUP BY p.id, p.name
ORDER BY (MAX(t.date) IS NULL), MAX(t.date) DESC, p.name COLLATE NOCASE, p.id;

-- name: LastTransactionForPayee :one
-- The id of a payee's LAST non-deleted transaction (p12.3 template autofill): the
-- greatest (date, id) among that payee's live transactions. Returns no rows when the
-- payee has no non-deleted transaction (never used / only deleted). The store then
-- reuses SplitsByTransaction to read its splits (the existing splits reader).
SELECT id, currency
FROM transactions
WHERE payee_id = ? AND deleted = 0
ORDER BY date DESC, id DESC
LIMIT 1;

-- name: InsertPayeeVersion :exec
-- Snapshot-from-live version append for payees (STANDARD single-id entity,
-- entity_id = payees.id). Runs AFTER the live insert. Snapshot column set matches
-- 00010_transactions_splits.sql exactly (name, active). Params (positional, each
-- used once): op, change_id, entity_id -> generated Op, ID (change_id), ID_2.
INSERT INTO payees_versions
  (entity_id, change_id, valid_from, op, name, active)
SELECT p.id, c.id, c.at, ?, p.name, p.active
FROM payees p, changes c
WHERE c.id = ? AND p.id = ?;

-- ---------------------------------------------------------------------------
-- transactions
-- ---------------------------------------------------------------------------

-- name: InsertTransaction :one
-- Live insert of the transaction header (D18: exactly one subsidiary; D3: single
-- currency). deleted defaults to 0. Returns the new id.
INSERT INTO transactions (date, subsidiary_id, payee_id, memo, currency, deleted)
VALUES (?, ?, ?, ?, ?, 0)
RETURNING id;

-- name: GetTransaction :one
SELECT id, date, subsidiary_id, payee_id, memo, currency, deleted
FROM transactions
WHERE id = ?;

-- name: UpdateTransaction :exec
-- Live update of the header fields (date/payee/memo; subsidiary and currency may
-- also change on an edit). deleted is carried through by the store (never flipped
-- here -- soft-delete is its own query).
UPDATE transactions
SET date = ?, subsidiary_id = ?, payee_id = ?, memo = ?, currency = ?, deleted = ?
WHERE id = ?;

-- name: SoftDeleteTransaction :exec
-- Soft-delete (rule 14): flip the deleted flag. The row is never removed. The
-- store appends a transactions_versions op='delete' after this.
UPDATE transactions SET deleted = 1 WHERE id = ?;

-- name: InsertTransactionVersion :exec
-- Snapshot-from-live version append for transactions (STANDARD single-id entity).
-- Runs AFTER the live write. Snapshot column set matches 00010 exactly. Params
-- (positional): op, change_id, entity_id -> generated Op, ID (change_id), ID_2.
INSERT INTO transactions_versions
  (entity_id, change_id, valid_from, op, date, subsidiary_id, payee_id, memo, currency, deleted)
SELECT t.id, c.id, c.at, ?, t.date, t.subsidiary_id, t.payee_id, t.memo, t.currency, t.deleted
FROM transactions t, changes c
WHERE c.id = ? AND t.id = ?;

-- ---------------------------------------------------------------------------
-- splits
-- ---------------------------------------------------------------------------

-- name: InsertSplit :one
-- Live insert of one split line. amount is int64 minor units, net-debit sign
-- (D1/D2), CHECK amount <> 0. fund_id NULL == unrestricted (D20). program_id
-- required iff R/E account (trigger backstop); functional_class required iff
-- expense account (trigger backstop); the store DEFAULTS both before insert so the
-- triggers never fire on the happy path. Returns the new id.
INSERT INTO splits
  (transaction_id, account_id, amount, fund_id, program_id, functional_class, memo, position)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: UpdateSplit :exec
-- Live update of one split's business columns (replace-set diff: only splits that
-- actually changed reach this). id last.
UPDATE splits
SET account_id = ?, amount = ?, fund_id = ?, program_id = ?,
    functional_class = ?, memo = ?, position = ?
WHERE id = ?;

-- name: DeleteSplit :exec
-- Hard-delete one split row (the replace-set diff removes splits dropped from the
-- input). For op='delete' the version row is captured BEFORE this runs (the live
-- row must still exist to snapshot from) -- the removal-op ordering.
DELETE FROM splits WHERE id = ?;

-- name: SplitsByTransaction :many
-- The current live split set for one transaction, in display order. Selects the
-- full splits row (incl. reconciliation_id, added p16.1) so this maps to the
-- sqlc.Split model; consumers that ignore reconciliation_id are unaffected.
SELECT id, transaction_id, account_id, amount, fund_id, program_id,
       functional_class, memo, position, reconciliation_id
FROM splits
WHERE transaction_id = ?
ORDER BY position, id;

-- name: InsertSplitVersion :exec
-- Snapshot-from-live version append for splits (STANDARD single-id entity,
-- entity_id = splits.id). For op='create'/'update' this runs AFTER the live write;
-- for op='delete' it runs BEFORE the live DELETE (the row must still exist to
-- snapshot). Snapshot column set matches 00010 exactly. Params (positional): op,
-- change_id, entity_id -> generated Op, ID (change_id), ID_2.
INSERT INTO splits_versions
  (entity_id, change_id, valid_from, op, transaction_id, account_id, amount,
   fund_id, program_id, functional_class, memo, position)
SELECT s.id, c.id, c.at, ?, s.transaction_id, s.account_id, s.amount,
       s.fund_id, s.program_id, s.functional_class, s.memo, s.position
FROM splits s, changes c
WHERE c.id = ? AND s.id = ?;

-- ---------------------------------------------------------------------------
-- validation reads (inside the funnel's fn, on the tx-bound queries)
-- ---------------------------------------------------------------------------

-- name: AccountIsLeaf :one
-- 1 when the account has NO children (a leaf; D11). A placeholder (>=1 child)
-- holds no splits. Used to raise ErrPlaceholderAccount before the trigger fires.
SELECT NOT EXISTS (SELECT 1 FROM accounts c WHERE c.parent_id = ?) AS is_leaf;

-- name: HasAccountSubsidiaryMap :one
-- 1 when account A is mapped to subsidiary S (D18). Used to raise
-- ErrAccountNotInSubsidiary: every split account must include the txn's subsidiary.
SELECT EXISTS (
  SELECT 1 FROM account_subsidiaries WHERE account_id = ? AND subsidiary_id = ?
) AS mapped;

-- name: HasFundSubsidiaryMap :one
-- 1 when fund F is scoped to subsidiary S (D20/Z13). Used to raise
-- ErrFundSubsidiaryScope: a txn's subsidiary must be in its split fund's set.
SELECT EXISTS (
  SELECT 1 FROM fund_subsidiaries WHERE fund_id = ? AND subsidiary_id = ?
) AS scoped;

-- name: RootProgram :one
-- The single root program (parent_id IS NULL; the seeded "General", D24). The
-- program-defaulting fallback for an R/E split with no account default. Looked up
-- rather than hardcoded id 1.
SELECT id, parent_id, name, active, sort_order
FROM programs
WHERE parent_id IS NULL;

-- name: ProgramSubtreeIDs :many
-- The program ids in the subtree rooted at R (self included). Used for the
-- fund-program scope check (D20): an R/E split tagged a fund whose program scope R
-- is set must carry a program inside R's subtree, else ErrFundProgramScope. Like
-- ProgramDescendants, the store builds a set and checks membership in Go (sqlc's
-- sqlite analyzer rejects a recursive-CTE alias in an outer EXISTS/WHERE).
WITH RECURSIVE subtree(id, parent_id, name, active, sort_order) AS (
  SELECT p0.id, p0.parent_id, p0.name, p0.active, p0.sort_order
  FROM programs p0 WHERE p0.id = ?
  UNION ALL
  SELECT p.id, p.parent_id, p.name, p.active, p.sort_order
  FROM programs p JOIN subtree ON p.parent_id = subtree.id
)
SELECT subtree.id FROM subtree;

-- ---------------------------------------------------------------------------
-- as-of reconstruction (D4/D5) -- resolved in Go (like Effective990Codes) to keep
-- each param single-use and avoid the sqlc numbered-param quirk on `at` reuse.
-- ---------------------------------------------------------------------------

-- name: TransactionVersionAsOf :one
-- The transaction header as of a time: the latest transactions_versions row with
-- valid_from <= at (op='delete' means the txn is absent -- the store excludes it).
-- Tiebreak (valid_from DESC, id DESC) matches AssertVersioned's append order.
SELECT op, date, subsidiary_id, payee_id, memo, currency, deleted
FROM transactions_versions
WHERE entity_id = ? AND valid_from <= ?
ORDER BY valid_from DESC, id DESC
LIMIT 1;

-- name: SplitVersionsAsOf :many
-- Every split version row for a transaction with valid_from <= at, ordered so the
-- store can take the FIRST row per entity_id (the latest snapshot for that split)
-- and drop op='delete'. transaction_id filters to this txn's splits. Tiebreak
-- matches AssertVersioned.
SELECT entity_id, op, transaction_id, account_id, amount, fund_id, program_id,
       functional_class, memo, position
FROM splits_versions
WHERE transaction_id = ? AND valid_from <= ?
ORDER BY entity_id, valid_from DESC, id DESC;

-- ---------------------------------------------------------------------------
-- history reconstruction (p12.4) -- the FULL version timeline of one transaction,
-- for /transactions/{id}/history. Unlike the AsOf queries above these fetch EVERY
-- version row (not a LIMIT-1 snapshot) so the store can walk consecutive snapshots
-- per entity and compute per-change diffs in Go (testable; SQL only orders). Each
-- row carries its change_id + the change's actor + timestamp (JOIN changes, and
-- users for the actor's display name), so the store groups rows into timeline
-- entries by change_id. Ordered by valid_from then id so an entity's snapshots are
-- consecutive in append order; the store re-groups by change_id afterwards.
-- ---------------------------------------------------------------------------

-- name: TransactionVersionHistory :many
-- Every transactions_versions row for one transaction, oldest first, with the
-- change's actor id, actor display name, and timestamp. Includes op='delete' rows
-- (a voided txn's history must still render). Params (positional): entity_id.
SELECT tv.change_id, tv.op, tv.valid_from,
       tv.date, tv.subsidiary_id, tv.payee_id, tv.memo, tv.currency, tv.deleted,
       c.actor_id, u.display_name AS actor_name, c.at
FROM transactions_versions tv
JOIN changes c ON c.id = tv.change_id
JOIN users u ON u.id = c.actor_id
WHERE tv.entity_id = ?
ORDER BY tv.valid_from, tv.id;

-- name: SplitVersionHistory :many
-- Every splits_versions row for one transaction's splits, oldest first, with the
-- change actor + timestamp. entity_id (the split id) lets the store group a split's
-- consecutive snapshots to diff them; change_id groups a row into its timeline
-- entry. Params (positional): transaction_id.
SELECT sv.entity_id, sv.change_id, sv.op, sv.valid_from,
       sv.account_id, sv.amount, sv.fund_id, sv.program_id,
       sv.functional_class, sv.memo, sv.position,
       c.actor_id, u.display_name AS actor_name, c.at
FROM splits_versions sv
JOIN changes c ON c.id = sv.change_id
JOIN users u ON u.id = c.actor_id
WHERE sv.transaction_id = ?
ORDER BY sv.valid_from, sv.id;

-- ---------------------------------------------------------------------------
-- deferred p08 guards (complete the p05.2 / p07.3 TODOs)
-- ---------------------------------------------------------------------------

-- name: SplitUsesAccountInSubsidiary :one
-- 1 when a split on account A belongs to a NON-DELETED transaction whose
-- subsidiary is S. Completes the p05.2 guard: removing subsidiary S from account A
-- is blocked while such a split exists (ErrSubInUseByChild extended to splits).
SELECT EXISTS (
  SELECT 1 FROM splits s
  JOIN transactions t ON t.id = s.transaction_id
  WHERE s.account_id = ? AND t.subsidiary_id = ? AND t.deleted = 0
) AS in_use;

-- name: SplitUsesFundInSubsidiary :one
-- 1 when a split with fund_id F belongs to a NON-DELETED transaction whose
-- subsidiary is S. Completes the p07.3 guard: removing subsidiary S from fund F is
-- blocked while such a split exists (ErrFundSubInUseBySplit).
SELECT EXISTS (
  SELECT 1 FROM splits s
  JOIN transactions t ON t.id = s.transaction_id
  WHERE s.fund_id = ? AND t.subsidiary_id = ? AND t.deleted = 0
) AS in_use;

-- ---------------------------------------------------------------------------
-- account merge (p08.5) -- repoint every split from a source account to a
-- destination account, versioning each moved split op='update'.
-- ---------------------------------------------------------------------------

-- name: SplitIdsByAccount :many
-- All split ids currently on an account, oldest first. Used by MergeAccount to
-- repoint each split individually and version it (snapshot-from-live). NOT
-- filtered by transaction.deleted: a merge clears the source account entirely so
-- its history reads coherently (even a soft-deleted txn's split moves), and Z2
-- (splits on active leaves) would otherwise still see splits stranded on the
-- deactivated source. Captured BEFORE any repoint write so the moved rows are not
-- confused with the destination's pre-existing splits.
SELECT id FROM splits WHERE account_id = ? ORDER BY id;

-- name: RepointSplitAccount :exec
-- Move ONE split to a new account_id (the merge repoint). The store versions the
-- split op='update' AFTER this so the snapshot-from-live row records account_id =
-- the destination. id last.
UPDATE splits SET account_id = ? WHERE id = ?;

-- name: CountReconciledSplitsForAccount :one
-- How many splits on an account carry a non-NULL reconciliation_id (p22.5). The
-- merge block-guard uses this: repointing a reconciled split to the destination
-- would leave it linked to a reconciliation on the SOURCE account (Z8 fires for an
-- open recon; the 00014 finalized-lock trigger ABORTs for a finalized one), so the
-- store refuses the merge when this count is > 0 (ErrMergeSourceReconciled). Full
-- recon repointing stays backlog; this closes the integrity hole cleanly.
SELECT COUNT(*) FROM splits WHERE account_id = ? AND reconciliation_id IS NOT NULL;
