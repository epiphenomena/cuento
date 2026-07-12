-- p05.2: account operations. All SQL for the store's account methods lives here
-- (rule 6). This copies the version-append convention established in
-- subsidiaries.sql (p04.2): the entity op does the live write inside the funnel's
-- fn, then appends a snapshot-from-live version row, so the version row is
-- byte-identical to the live row (Z3 can never diverge) and valid_from ==
-- changes.at BY CONSTRUCTION.
--
-- Two composite-key versioned tables appear here (account_names,
-- account_subsidiaries). Their version-append queries copy InsertSubsidiaryVersion's
-- shape verbatim, changing ONLY the entity_id/WHERE expression, per 00005 comments.
--
-- NOTE: keep every comment and identifier in this file PURE ASCII. sqlc v1.31.1
-- miscounts byte offsets when a query file contains multi-byte UTF-8, corrupting
-- the generated SQL for the WHOLE file (see docs/DECISIONS.md p04.2).

-- name: InsertAccount :one
-- Live insert of an account. Row-local invariants (parent typeclass,
-- functional-class-expense-only) are validated in the store BEFORE this runs and
-- backstopped by triggers. default_program_id is validated in the store as R/E-only
-- (D24). Returns the new id for the store to snapshot + return.
INSERT INTO accounts
  (parent_id, type, default_currency, functional_class, form990_code,
   default_program_id, intercompany, reconcilable, active, sort_order, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: GetAccount :one
-- Column order matches the accounts table's PHYSICAL order (default_program_id was
-- ADDed last by 00008), so sqlc maps the result to the sqlc.Account model rather
-- than a bespoke GetAccountRow -- accounts.go depends on GetAccount returning Account.
SELECT id, parent_id, type, default_currency, functional_class, form990_code,
       intercompany, reconcilable, active, sort_order, created_at, default_program_id
FROM accounts
WHERE id = ?;

-- name: UpdateAccount :exec
-- Live update: move (parent) / flags / default currency / functional default /
-- form990 code / default program / active / sort. The store reads the current row
-- (GetAccount), overrides the caller's fields, and writes the full desired state
-- here, keeping snapshot-from-live trivial. created_at is preserved
-- (read-modify-write), and so is default_program_id unless changed -- the store
-- carries cur.DefaultProgramID through, so an unrelated update never NULLs it.
UPDATE accounts
SET parent_id = ?, type = ?, default_currency = ?, functional_class = ?,
    form990_code = ?, default_program_id = ?, intercompany = ?, reconcilable = ?,
    active = ?, sort_order = ?, created_at = ?
WHERE id = ?;

-- name: InsertAccountVersion :exec
-- Snapshot-from-live version append for accounts (STANDARD single-column entity,
-- entity_id = accounts.id). Must run AFTER the live write. Snapshot column set
-- matches 00005_accounts.sql + the 00008 default_program_id ripple exactly:
-- adding default_program_id to accounts requires it here too, or Z3 (current ==
-- latest snapshot) diverges for accounts touched after p07.1. Params (plain
-- positional, each used once): op, change_id, entity_id -> generated fields Op,
-- ID (change_id), ID_2 (entity_id).
INSERT INTO accounts_versions
  (entity_id, change_id, valid_from, op, parent_id, type, default_currency,
   functional_class, form990_code, default_program_id, intercompany, reconcilable,
   active, sort_order, created_at)
SELECT a.id, c.id, c.at, ?, a.parent_id, a.type, a.default_currency,
       a.functional_class, a.form990_code, a.default_program_id, a.intercompany,
       a.reconcilable, a.active, a.sort_order, a.created_at
FROM accounts a, changes c
WHERE c.id = ? AND a.id = ?;

-- name: UpsertAccountName :exec
-- Insert or replace a single (account_id, lang) name. The store decides op
-- (create first time / update thereafter) via GetAccountName before this runs.
INSERT INTO account_names (account_id, lang, name)
VALUES (?, ?, ?)
ON CONFLICT (account_id, lang) DO UPDATE SET name = excluded.name;

-- name: GetAccountName :one
SELECT account_id, lang, name
FROM account_names
WHERE account_id = ? AND lang = ?;

-- name: InsertAccountNameVersion :exec
-- Snapshot-from-live version append for a COMPOSITE (account_id, lang) name.
-- entity_id = account_id; lang is both a snapshot column and part of the entity
-- identity (00005). Must run AFTER the live upsert. Params (positional): op,
-- change_id, account_id, lang -> generated fields Op, ID, AccountID, Lang.
INSERT INTO account_names_versions
  (entity_id, change_id, valid_from, op, lang, name)
SELECT an.account_id, c.id, c.at, ?, an.lang, an.name
FROM account_names an, changes c
WHERE c.id = ? AND an.account_id = ? AND an.lang = ?;

-- name: HasAccountSubsidiary :one
-- 1 if the account already maps the subsidiary. Propagation uses this to skip
-- ancestors that already hold the sub (no duplicate PK, no spurious version row).
SELECT COUNT(*) FROM account_subsidiaries
WHERE account_id = ? AND subsidiary_id = ?;

-- name: InsertAccountSubsidiary :exec
-- Add one (account_id, subsidiary_id) membership. Callers guard with
-- HasAccountSubsidiary first (membership is a set; the PK forbids duplicates).
INSERT INTO account_subsidiaries (account_id, subsidiary_id)
VALUES (?, ?);

-- name: DeleteAccountSubsidiary :exec
-- Remove one membership. For op=delete the version row is captured BEFORE this
-- runs (the live row must still exist to snapshot from) -- see the store's
-- removal-op ordering comment.
DELETE FROM account_subsidiaries
WHERE account_id = ? AND subsidiary_id = ?;

-- name: InsertAccountSubsidiaryVersion :exec
-- Snapshot-from-live version append for a COMPOSITE (account_id, subsidiary_id)
-- membership. entity_id = account_id; subsidiary_id is both a snapshot column and
-- part of the entity identity (00005). For op='create' this runs AFTER the live
-- insert; for op='delete' it runs BEFORE the live delete (the row must still
-- exist to snapshot). Params (positional): op, change_id, account_id,
-- subsidiary_id -> generated fields Op, ID, AccountID, SubsidiaryID.
INSERT INTO account_subsidiaries_versions
  (entity_id, change_id, valid_from, op, subsidiary_id)
SELECT asub.account_id, c.id, c.at, ?, asub.subsidiary_id
FROM account_subsidiaries asub, changes c
WHERE c.id = ? AND asub.account_id = ? AND asub.subsidiary_id = ?;

-- name: AccountSubsidiaries :many
-- The subsidiary id set an account currently maps (order-insensitive; the store
-- builds a set).
SELECT subsidiary_id FROM account_subsidiaries
WHERE account_id = ?
ORDER BY subsidiary_id;

-- name: AccountAncestors :many
-- Self + transitive ancestor chain of an account (walk parent_id upward).
-- Propagation adds an assigned subsidiary to every strict ancestor; the store
-- skips self.
WITH RECURSIVE anc(id, parent_id) AS (
  SELECT a.id, a.parent_id FROM accounts a WHERE a.id = ?
  UNION ALL
  SELECT a.id, a.parent_id FROM accounts a
  JOIN anc ON a.id = anc.parent_id
)
SELECT anc.id FROM anc;

-- name: AccountDescendants :many
-- Self + transitive closure of an account (walk children downward). The store's
-- move cycle check (new parent must not be self nor any descendant) relies on
-- self being included.
WITH RECURSIVE des(id) AS (
  SELECT a.id FROM accounts a WHERE a.id = ?
  UNION ALL
  SELECT a.id FROM accounts a
  JOIN des ON a.parent_id = des.id
)
SELECT des.id FROM des;

-- name: CountChildAccountsWithSub :one
-- Number of DIRECT child accounts that still map a given subsidiary. Removing a
-- subsidiary from an account is blocked while this is > 0 (ErrSubInUseByChild):
-- the superset invariant (parent set superset-of union of children's) forbids a
-- parent dropping a sub a child still holds. Direct children suffice because the
-- invariant holds inductively.
SELECT COUNT(*) FROM accounts child
JOIN account_subsidiaries asub ON asub.account_id = child.id
WHERE child.parent_id = ? AND asub.subsidiary_id = ?;

-- name: GetForm990Line :one
-- A 990 line's allowed account types (CSV). The store splits account_types on
-- comma and checks membership (some lines allow more than one type), rejecting a
-- code whose CSV omits the account's type (Err990TypeMismatch).
SELECT code, part, line, label, account_types, sort
FROM form990_lines
WHERE code = ?;

-- name: ListForm990Lines :many
-- Every 990 line in report order (part, then line by sort). The chart-of-accounts
-- form (p11.1) offers, for an account of a given type, only the lines whose
-- account_types CSV includes that type; the CSV-membership filter runs in Go
-- (Form990LinesForType) reusing the same predicate as check990Type, so this query
-- stays a simple ordered fetch of the small static reference set (D25).
SELECT code, part, line, label, account_types, sort
FROM form990_lines
ORDER BY sort, code;

-- name: AccountTree :many
-- All accounts in DEPTH-FIRST (pre-order) order, resolving each account's name via
-- the fallback chain requested-lang -> en -> any (p05.3). Multiple roots are
-- allowed (accounts have no single-root constraint); each root subtree is emitted
-- in order. Path = zero-padded (sort_order, id) per level, so ORDER BY path is a
-- true pre-order traversal.
--
-- Name resolution is a COALESCE over correlated subqueries against account_names:
-- (1) the requested lang, (2) 'en' (D14), (3) any name, chosen deterministically
-- by ORDER BY lang LIMIT 1 so results are stable. The trailing '' keeps the column
-- non-null (an account with no names at all resolves to empty, and sqlc types the
-- column as a plain string), so the store's TreeRow mapping is unchanged.
WITH RECURSIVE tree(id, parent_id, type, active, sort_order, path) AS (
  SELECT a.id, a.parent_id, a.type, a.active, a.sort_order,
         printf('%020d.%020d', a.sort_order, a.id)
  FROM accounts a
  WHERE a.parent_id IS NULL
  UNION ALL
  SELECT a.id, a.parent_id, a.type, a.active, a.sort_order,
         t.path || '/' || printf('%020d.%020d', a.sort_order, a.id)
  FROM accounts a
  JOIN tree t ON a.parent_id = t.id
)
SELECT tree.id, tree.parent_id, tree.type, tree.active, tree.sort_order,
       CAST(COALESCE(
         (SELECT an1.name FROM account_names an1 WHERE an1.account_id = tree.id AND an1.lang = ?),
         (SELECT an2.name FROM account_names an2 WHERE an2.account_id = tree.id AND an2.lang = 'en'),
         (SELECT anx.name FROM account_names anx WHERE anx.account_id = tree.id ORDER BY anx.lang LIMIT 1),
         ''
       ) AS TEXT) AS name
FROM tree
ORDER BY tree.path;

-- name: AccountTreeBySubsidiary :many
-- Like AccountTree but restricted to accounts mapped to a specific subsidiary.
-- The filter is applied at output (the recursion still walks the whole tree so
-- paths stay well-defined); a mapped child under an unmapped parent is dropped,
-- matching "accounts mapped to subFilter". sqlc-sqlite's nullable-? handling is
-- awkward, so this is a separate query (mirrors SubTree/Descendants split).
--
-- Name resolution is the SAME requested-lang -> en -> any COALESCE chain as
-- AccountTree (p05.3); see that query's comment for the rationale of each branch
-- and the trailing '' (keeps the column non-null / a plain string in sqlc).
WITH RECURSIVE tree(id, parent_id, type, active, sort_order, path) AS (
  SELECT a.id, a.parent_id, a.type, a.active, a.sort_order,
         printf('%020d.%020d', a.sort_order, a.id)
  FROM accounts a
  WHERE a.parent_id IS NULL
  UNION ALL
  SELECT a.id, a.parent_id, a.type, a.active, a.sort_order,
         t.path || '/' || printf('%020d.%020d', a.sort_order, a.id)
  FROM accounts a
  JOIN tree t ON a.parent_id = t.id
)
SELECT tree.id, tree.parent_id, tree.type, tree.active, tree.sort_order,
       CAST(COALESCE(
         (SELECT an1.name FROM account_names an1 WHERE an1.account_id = tree.id AND an1.lang = ?),
         (SELECT an2.name FROM account_names an2 WHERE an2.account_id = tree.id AND an2.lang = 'en'),
         (SELECT anx.name FROM account_names anx WHERE anx.account_id = tree.id ORDER BY anx.lang LIMIT 1),
         ''
       ) AS TEXT) AS name
FROM tree
JOIN account_subsidiaries asub ON asub.account_id = tree.id AND asub.subsidiary_id = ?
ORDER BY tree.path;

-- name: AllAccountCodes :many
-- Flat (id, parent_id, own form990_code) for every account. Effective990Codes
-- resolves the nearest-ancestor code (D25) in Go from this: sqlc's sqlite analyzer
-- cannot resolve a recursive CTE column that is CARRIED (a self-referencing
-- COALESCE or a passed-through id) in the OUTPUT projection -- it reports the
-- carried column 'does not exist'. Rather than fight that limitation with an
-- awkward closure query, the store walks parent_id in memory (the chart is small;
-- one pass is O(n * depth)). Ordered by id for deterministic resolution.
SELECT id, parent_id, form990_code
FROM accounts
ORDER BY id;
