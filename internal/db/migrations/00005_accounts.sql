-- +goose Up
-- p05.1: accounts + names + subsidiary map + versions + triggers (D11, D21, D25).
-- Forward-only; never edit an applied migration; no Down (AGENTS rule 4).
--
-- This migration adds the accounts family. FK targets must pre-exist, so the
-- create order is: form990_lines (static reference) -> accounts -> account_names
-- -> account_subsidiaries -> the three *_versions twins.
--
-- default_program_id is DELIBERATELY OMITTED here: Appendix A lists it on
-- accounts, but it references the programs table which does not exist until
-- p07.1, which adds the column then (D24). Do not add it in this step.
--
-- Leaf/no-splits triggers (trg_accounts_no_children_over_splits) also arrive
-- later (p08.1) once `splits` exists. This step covers only the two row-local
-- invariants that need no other table: parent typeclass and function-expense-only.

-- ---------------------------------------------------------------------------
-- form990_lines: STATIC seeded reference (D25), like currencies. NOT a versioned
-- business table (absent from the Appendix A versions list): no *_versions twin,
-- no trigger, no changes wiring. Updated only by a future migration.
--
-- account_types is a CSV of the account types a line may be assigned to; the
-- p05.2 Set990CodeTypeMismatch guard checks an account's type against this set.
-- Q6 default: seed the FULL 990 line set (supersets 990-EZ) for the three parts
-- the reports need -- VIII (revenue), IX (expenses), X (balance sheet).
-- ---------------------------------------------------------------------------
CREATE TABLE form990_lines (
  code          TEXT PRIMARY KEY,        -- e.g. 'VIII.1e', 'IX.16', 'X.10'
  part          TEXT NOT NULL,           -- 'VIII' | 'IX' | 'X'
  line          TEXT NOT NULL,           -- the 990 line number within the part
  label         TEXT NOT NULL,
  account_types TEXT NOT NULL,           -- CSV of allowed account types
  sort          INTEGER NOT NULL
);

-- Part VIII -- Statement of Revenue. All revenue lines.
INSERT INTO form990_lines (code, part, line, label, account_types, sort) VALUES
  ('VIII.1a', 'VIII', '1a', 'Federated campaigns',                        'revenue', 100),
  ('VIII.1b', 'VIII', '1b', 'Membership dues',                            'revenue', 101),
  ('VIII.1c', 'VIII', '1c', 'Fundraising events',                         'revenue', 102),
  ('VIII.1d', 'VIII', '1d', 'Related organizations',                      'revenue', 103),
  ('VIII.1e', 'VIII', '1e', 'Government grants (contributions)',          'revenue', 104),
  ('VIII.1f', 'VIII', '1f', 'All other contributions and gifts',          'revenue', 105),
  ('VIII.2',  'VIII', '2',  'Program service revenue',                    'revenue', 110),
  ('VIII.3',  'VIII', '3',  'Investment income',                          'revenue', 120),
  ('VIII.4',  'VIII', '4',  'Income from investment of tax-exempt bond proceeds', 'revenue', 121),
  ('VIII.5',  'VIII', '5',  'Royalties',                                  'revenue', 122),
  ('VIII.6',  'VIII', '6',  'Net rental income',                          'revenue', 130),
  ('VIII.7',  'VIII', '7',  'Net gain or loss from sales of assets',      'revenue', 131),
  ('VIII.8',  'VIII', '8',  'Net income from fundraising events',         'revenue', 140),
  ('VIII.9',  'VIII', '9',  'Net income from gaming activities',          'revenue', 141),
  ('VIII.10', 'VIII', '10', 'Net income from sales of inventory',         'revenue', 142),
  ('VIII.11', 'VIII', '11', 'All other revenue (miscellaneous)',          'revenue', 150);

-- Part IX -- Statement of Functional Expenses. All expense lines.
INSERT INTO form990_lines (code, part, line, label, account_types, sort) VALUES
  ('IX.1',   'IX', '1',   'Grants to domestic organizations and governments', 'expense', 200),
  ('IX.2',   'IX', '2',   'Grants to domestic individuals',                    'expense', 201),
  ('IX.3',   'IX', '3',   'Grants to foreign organizations and individuals',   'expense', 202),
  ('IX.4',   'IX', '4',   'Benefits paid to or for members',                   'expense', 203),
  ('IX.5',   'IX', '5',   'Compensation of officers, directors, and key employees', 'expense', 210),
  ('IX.6',   'IX', '6',   'Compensation to disqualified persons',              'expense', 211),
  ('IX.7',   'IX', '7',   'Other salaries and wages',                          'expense', 212),
  ('IX.8',   'IX', '8',   'Pension plan accruals and contributions',           'expense', 213),
  ('IX.9',   'IX', '9',   'Other employee benefits',                           'expense', 214),
  ('IX.10',  'IX', '10',  'Payroll taxes',                                     'expense', 215),
  ('IX.11a', 'IX', '11a', 'Fees for services -- management',                   'expense', 220),
  ('IX.11b', 'IX', '11b', 'Fees for services -- legal',                        'expense', 221),
  ('IX.11c', 'IX', '11c', 'Fees for services -- accounting',                   'expense', 222),
  ('IX.11d', 'IX', '11d', 'Fees for services -- lobbying',                     'expense', 223),
  ('IX.11e', 'IX', '11e', 'Fees for services -- professional fundraising',     'expense', 224),
  ('IX.11f', 'IX', '11f', 'Fees for services -- investment management',        'expense', 225),
  ('IX.11g', 'IX', '11g', 'Fees for services -- other',                        'expense', 226),
  ('IX.12',  'IX', '12',  'Advertising and promotion',                         'expense', 230),
  ('IX.13',  'IX', '13',  'Office expenses',                                   'expense', 231),
  ('IX.14',  'IX', '14',  'Information technology',                            'expense', 232),
  ('IX.15',  'IX', '15',  'Royalties',                                         'expense', 233),
  ('IX.16',  'IX', '16',  'Occupancy',                                         'expense', 234),
  ('IX.17',  'IX', '17',  'Travel',                                            'expense', 235),
  ('IX.18',  'IX', '18',  'Payments of travel or entertainment for officials', 'expense', 236),
  ('IX.19',  'IX', '19',  'Conferences, conventions, and meetings',            'expense', 237),
  ('IX.20',  'IX', '20',  'Interest',                                          'expense', 238),
  ('IX.21',  'IX', '21',  'Payments to affiliates',                            'expense', 239),
  ('IX.22',  'IX', '22',  'Depreciation, depletion, and amortization',         'expense', 240),
  ('IX.23',  'IX', '23',  'Insurance',                                         'expense', 241),
  ('IX.24a', 'IX', '24a', 'Other expenses -- write-in a',                      'expense', 250),
  ('IX.24b', 'IX', '24b', 'Other expenses -- write-in b',                      'expense', 251),
  ('IX.24c', 'IX', '24c', 'Other expenses -- write-in c',                      'expense', 252),
  ('IX.24d', 'IX', '24d', 'Other expenses -- write-in d',                      'expense', 253),
  ('IX.24e', 'IX', '24e', 'All other expenses',                                'expense', 254);

-- Part X -- Balance Sheet. Asset lines, liability lines, and net-asset (equity)
-- lines. Some lines legitimately allow more than one account type (CSV).
INSERT INTO form990_lines (code, part, line, label, account_types, sort) VALUES
  ('X.1',  'X', '1',  'Cash -- non-interest-bearing',                    'asset',            300),
  ('X.2',  'X', '2',  'Savings and temporary cash investments',          'asset',            301),
  ('X.3',  'X', '3',  'Pledges and grants receivable',                   'asset',            302),
  ('X.4',  'X', '4',  'Accounts receivable',                             'asset',            303),
  ('X.5',  'X', '5',  'Receivables from current/former officers and key employees', 'asset', 304),
  ('X.6',  'X', '6',  'Receivables from other disqualified persons',      'asset',            305),
  ('X.7',  'X', '7',  'Notes and loans receivable',                       'asset',            306),
  ('X.8',  'X', '8',  'Inventories for sale or use',                      'asset',            307),
  ('X.9',  'X', '9',  'Prepaid expenses and deferred charges',            'asset',            308),
  ('X.10', 'X', '10', 'Land, buildings, and equipment',                   'asset',            309),
  ('X.11', 'X', '11', 'Investments -- publicly traded securities',        'asset',            310),
  ('X.12', 'X', '12', 'Investments -- other securities',                  'asset',            311),
  ('X.13', 'X', '13', 'Investments -- program-related',                   'asset',            312),
  ('X.14', 'X', '14', 'Intangible assets',                                'asset',            313),
  ('X.15', 'X', '15', 'Other assets',                                     'asset',            314),
  ('X.17', 'X', '17', 'Accounts payable and accrued expenses',            'liability',        320),
  ('X.18', 'X', '18', 'Grants payable',                                   'liability',        321),
  ('X.19', 'X', '19', 'Deferred revenue',                                 'liability',        322),
  ('X.20', 'X', '20', 'Tax-exempt bond liabilities',                      'liability',        323),
  ('X.21', 'X', '21', 'Escrow or custodial account liability',            'liability',        324),
  ('X.22', 'X', '22', 'Loans/payables to officers and key employees',     'liability',        325),
  ('X.23', 'X', '23', 'Secured mortgages and notes payable',              'liability',        326),
  ('X.24', 'X', '24', 'Unsecured notes and loans payable',                'liability',        327),
  ('X.25', 'X', '25', 'Other liabilities',                                'liability',        328),
  ('X.27', 'X', '27', 'Net assets without donor restrictions',            'equity',           330),
  ('X.28', 'X', '28', 'Net assets with donor restrictions',               'equity',           331),
  ('X.29', 'X', '29', 'Capital stock or trust principal',                 'equity',           332),
  ('X.30', 'X', '30', 'Paid-in or capital surplus',                       'equity',           333),
  ('X.31', 'X', '31', 'Retained earnings, endowment, or other funds',     'equity',           334);

-- ---------------------------------------------------------------------------
-- accounts: the live chart-of-accounts table (D11, D18, D19, D21, D25). All
-- rows are USER-CREATED (no seed), so unlike 00004 there is no seed-with-version
-- audit consistency to establish this step.
-- ---------------------------------------------------------------------------
CREATE TABLE accounts (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,                    -- D17
  parent_id        INTEGER REFERENCES accounts(id),                      -- NULL = root
  type             TEXT NOT NULL CHECK (type IN ('asset','liability','equity','revenue','expense')),
  default_currency TEXT NOT NULL REFERENCES currencies(code),
  functional_class TEXT CHECK (functional_class IN ('program','management','fundraising')),
                                                                         -- default class; non-NULL only on
                                                                         -- expense accounts (trg below, D21)
  form990_code     TEXT REFERENCES form990_lines(code),                  -- effective = own or nearest ancestor (D25)
  intercompany     INTEGER NOT NULL DEFAULT 0,                           -- D19
  reconcilable     INTEGER NOT NULL DEFAULT 0,
  active           INTEGER NOT NULL DEFAULT 1,
  sort_order       INTEGER NOT NULL DEFAULT 0,
  created_at       TEXT NOT NULL
);

CREATE TABLE account_names (
  account_id INTEGER NOT NULL REFERENCES accounts(id),
  lang       TEXT NOT NULL,
  name       TEXT NOT NULL,
  PRIMARY KEY (account_id, lang)
);

CREATE TABLE account_subsidiaries (
  account_id    INTEGER NOT NULL REFERENCES accounts(id),
  subsidiary_id INTEGER NOT NULL REFERENCES subsidiaries(id),
  PRIMARY KEY (account_id, subsidiary_id)
);
-- Invariant (store + Z12): parent's subsidiary set superset-of union of children's.

-- ---------------------------------------------------------------------------
-- Version twins (rule 5, D4, Appendix A versions pattern). Append-only; never
-- UPDATE/DELETE. No seed rows this step (accounts are all user-created), so no
-- seed-version consistency wiring. The version-append queries + AssertVersioned
-- extension land in p05.2; these tables define the exact snapshot shapes it must
-- write.
--
-- accounts_versions: STANDARD single-column entity (entity_id = accounts.id).
-- Snapshot column set (must match `accounts` business columns, id/audit
-- excluded), which p05.2's InsertAccountVersion must write exactly:
--     parent_id, type, default_currency, functional_class, form990_code,
--     intercompany, reconcilable, active, sort_order, created_at
-- ---------------------------------------------------------------------------
CREATE TABLE accounts_versions (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  entity_id        INTEGER NOT NULL,                                     -- accounts.id
  change_id        INTEGER NOT NULL REFERENCES changes(id),
  valid_from       TEXT NOT NULL,                                        -- equals changes.at (rule 5)
  op               TEXT NOT NULL CHECK (op IN ('create','update','delete')),
  -- full snapshot of accounts' business columns:
  parent_id        INTEGER,
  type             TEXT NOT NULL,
  default_currency TEXT NOT NULL,
  functional_class TEXT,
  form990_code     TEXT,
  intercompany     INTEGER NOT NULL,
  reconcilable     INTEGER NOT NULL,
  active           INTEGER NOT NULL,
  sort_order       INTEGER NOT NULL,
  created_at       TEXT NOT NULL
);
CREATE INDEX accounts_versions_entity ON accounts_versions(entity_id, valid_from);

-- account_names_versions: COMPOSITE entity (account_id, lang). The natural
-- identity is the (account_id, lang) pair, but the versions pattern keys on a
-- single entity_id column, so: entity_id = account_id, and `lang` is BOTH a
-- snapshot column AND part of the entity identity. p05.2's as-of/AssertVersioned
-- for names must therefore filter on (entity_id, lang), and its version-append
-- copies the InsertSubsidiaryVersion shape changing only the entity_id/WHERE.
-- Snapshot columns: lang, name. Index carries lang so per-(account,lang) as-of
-- lookups stay cheap.
CREATE TABLE account_names_versions (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  entity_id  INTEGER NOT NULL,                                           -- account_names.account_id
  change_id  INTEGER NOT NULL REFERENCES changes(id),
  valid_from TEXT NOT NULL,
  op         TEXT NOT NULL CHECK (op IN ('create','update','delete')),
  -- snapshot (lang is also part of the composite identity):
  lang       TEXT NOT NULL,
  name       TEXT NOT NULL
);
CREATE INDEX account_names_versions_entity ON account_names_versions(entity_id, lang, valid_from);

-- account_subsidiaries_versions: COMPOSITE entity (account_id, subsidiary_id).
-- entity_id = account_id; subsidiary_id is BOTH the snapshot column AND part of
-- the entity identity. Membership is a set: adding a subsidiary appends op
-- 'create', removing it appends op 'delete' (there is no 'update' for a pure
-- membership row). p05.2 must filter as-of/AssertVersioned on
-- (entity_id, subsidiary_id). Snapshot column: subsidiary_id.
CREATE TABLE account_subsidiaries_versions (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  entity_id     INTEGER NOT NULL,                                        -- account_subsidiaries.account_id
  change_id     INTEGER NOT NULL REFERENCES changes(id),
  valid_from    TEXT NOT NULL,
  op            TEXT NOT NULL CHECK (op IN ('create','update','delete')),
  -- snapshot (subsidiary_id is also part of the composite identity):
  subsidiary_id INTEGER NOT NULL
);
CREATE INDEX account_subsidiaries_versions_entity
  ON account_subsidiaries_versions(entity_id, subsidiary_id, valid_from);

-- ---------------------------------------------------------------------------
-- trg_accounts_parent_typeclass (D11): when an account has a parent --
--   * parent is A/L/E: child.type must EQUAL parent.type (balance sheet stays
--     clean; a placeholder holds one type of natural category);
--   * parent is R/E: child.type must be revenue or expense (they interleave
--     freely under an R/E parent).
-- Root accounts (NULL parent) allow any type: the WHEN clause guards on parent.
-- Two triggers, INSERT + UPDATE (moves), because SQLite triggers fire per-row
-- with no deferred constraint. A single WHERE...EXISTS covers all four edges:
-- cross-type A/L/E child, and a non-R/E child under an R/E parent.
-- +goose StatementBegin
CREATE TRIGGER trg_accounts_parent_typeclass_insert
BEFORE INSERT ON accounts
WHEN NEW.parent_id IS NOT NULL
BEGIN
  SELECT RAISE(ABORT, 'accounts: child type incompatible with parent type')
  WHERE EXISTS (
    SELECT 1 FROM accounts p
    WHERE p.id = NEW.parent_id
      AND (
        (p.type IN ('asset','liability','equity') AND NEW.type <> p.type)
        OR (p.type IN ('revenue','expense') AND NEW.type NOT IN ('revenue','expense'))
      )
  );
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER trg_accounts_parent_typeclass_update
BEFORE UPDATE ON accounts
WHEN NEW.parent_id IS NOT NULL
BEGIN
  SELECT RAISE(ABORT, 'accounts: child type incompatible with parent type')
  WHERE EXISTS (
    SELECT 1 FROM accounts p
    WHERE p.id = NEW.parent_id
      AND (
        (p.type IN ('asset','liability','equity') AND NEW.type <> p.type)
        OR (p.type IN ('revenue','expense') AND NEW.type NOT IN ('revenue','expense'))
      )
  );
END;
-- +goose StatementEnd

-- trg_accounts_function_expense_only (D21): a non-NULL functional_class (the
-- account's DEFAULT class) is allowed ONLY on type='expense' accounts. NULL is
-- always fine. INSERT + UPDATE, same reason as above.
-- +goose StatementBegin
CREATE TRIGGER trg_accounts_function_expense_only_insert
BEFORE INSERT ON accounts
WHEN NEW.functional_class IS NOT NULL AND NEW.type <> 'expense'
BEGIN
  SELECT RAISE(ABORT, 'accounts: functional_class is allowed only on expense accounts');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER trg_accounts_function_expense_only_update
BEFORE UPDATE ON accounts
WHEN NEW.functional_class IS NOT NULL AND NEW.type <> 'expense'
BEGIN
  SELECT RAISE(ABORT, 'accounts: functional_class is allowed only on expense accounts');
END;
-- +goose StatementEnd
