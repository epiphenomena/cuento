package db_test

import (
	"database/sql"
	"strings"
	"testing"

	"cuento/internal/testutil"
)

// p08.1 adds the transaction core: payees, transactions, splits, their three
// *_versions twins, the six indexes named in Appendix A (plus the *_versions_entity
// twins), and four row-local triggers:
//
//	trg_splits_leaf_active_only         a split's account must be an active leaf
//	trg_accounts_no_children_over_splits a child cannot be inserted under an account that has splits
//	trg_splits_function_matches_type    functional_class present iff expense account (D21)
//	trg_splits_program_matches_type     program_id present iff revenue/expense account (D24)
//
// Zero-sum overall AND per fund, single-currency/subsidiary-per-txn, account-in-
// subsidiary, and fund scope are NOT triggers -- they are store (p08.2) + check
// (p08.3) invariants, deliberately absent here (Appendix A note). reconciliation_id
// is deferred to p16.1 -- it must appear on neither splits nor splits_versions.
//
// These tests exercise the migration via direct SQL on a migrated harness db
// (AGENTS testing conventions permit direct SQL for schema checks; the store path
// is p08.2). Written before 00010 exists -- each fails with "no such table" until
// the migration lands.
//
// The four split triggers cross-couple: to reach the ONE trigger under test a
// split must satisfy the other three. Type -> requirement matrix (D21/D24):
//
//	asset/liability/equity : functional_class NULL,     program_id NULL
//	revenue                : functional_class NULL,     program_id NOT NULL
//	expense                : functional_class NOT NULL, program_id NOT NULL
//
// Positive tests therefore set every column the other triggers demand; isolation
// tests pick the account type that satisfies the other triggers with NULLs.

// mkTxn inserts a minimal transaction (root subsidiary id 1, USD) and returns its
// id. It exists only so the split tests have a valid transaction_id to reference;
// the transaction itself is unbalanced (single split), which the schema permits --
// zero-sum is a store/check invariant (p08.2/p08.3), not a trigger.
func mkTxn(t *testing.T, sqldb *sql.DB) int64 {
	t.Helper()
	res, err := sqldb.Exec(
		`INSERT INTO transactions (date, subsidiary_id, currency) VALUES ('2025-01-01', 1, 'USD')`,
	)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			t.Fatalf("cannot seed a transaction because transactions is missing: %v", err)
		}
		t.Fatalf("seed transaction: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("transaction last insert id: %v", err)
	}
	return id
}

// mkAccount inserts an account of the given type (root, USD) and returns its id.
// functional_class/program_id defaults are NULL, valid for A/L/E; callers set them
// on the split, not the account.
func mkAccount(t *testing.T, sqldb *sql.DB, typ string) int64 {
	t.Helper()
	res, err := sqldb.Exec(
		`INSERT INTO accounts (parent_id, type, default_currency, created_at)
		 VALUES (NULL, ?, 'USD', '2025-01-01T00:00:00Z')`, typ,
	)
	if err != nil {
		t.Fatalf("seed %s account: %v", typ, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("account last insert id: %v", err)
	}
	return id
}

// TestSplitsPlaceholderAccountRejected proves trg_splits_leaf_active_only rejects a
// split on a placeholder account (one that has a child): only leaf accounts hold
// splits (D11).
func TestSplitsPlaceholderAccountRejected(t *testing.T) {
	sqldb := testutil.NewDB(t)
	txn := mkTxn(t, sqldb)

	parent := mkAccount(t, sqldb, "asset") // will gain a child -> becomes a placeholder
	if _, err := sqldb.Exec(
		`INSERT INTO accounts (parent_id, type, default_currency, created_at)
		 VALUES (?, 'asset', 'USD', '2025-01-01T00:00:00Z')`, parent,
	); err != nil {
		t.Fatalf("seed child account: %v", err)
	}

	_, err := sqldb.Exec(
		`INSERT INTO splits (transaction_id, account_id, amount, position)
		 VALUES (?, ?, 100, 0)`, txn, parent,
	)
	if err == nil {
		t.Fatal("split on a placeholder (parent) account succeeded; trg_splits_leaf_active_only does not reject non-leaf accounts")
	}
	if strings.Contains(err.Error(), "no such table") {
		t.Fatalf("insert failed because a table is missing, not the leaf trigger: %v", err)
	}
}

// TestSplitsInactiveAccountRejected proves trg_splits_leaf_active_only rejects a
// split on an inactive (active=0) leaf account.
func TestSplitsInactiveAccountRejected(t *testing.T) {
	sqldb := testutil.NewDB(t)
	txn := mkTxn(t, sqldb)

	res, err := sqldb.Exec(
		`INSERT INTO accounts (parent_id, type, default_currency, active, created_at)
		 VALUES (NULL, 'asset', 'USD', 0, '2025-01-01T00:00:00Z')`,
	)
	if err != nil {
		t.Fatalf("seed inactive account: %v", err)
	}
	acct, _ := res.LastInsertId()

	_, err = sqldb.Exec(
		`INSERT INTO splits (transaction_id, account_id, amount, position)
		 VALUES (?, ?, 100, 0)`, txn, acct,
	)
	if err == nil {
		t.Fatal("split on an inactive account succeeded; trg_splits_leaf_active_only does not reject active=0 accounts")
	}
	if strings.Contains(err.Error(), "no such table") {
		t.Fatalf("insert failed because a table is missing, not the leaf-active trigger: %v", err)
	}
}

// TestSplitsActiveLeafAccepted proves the positive side: a split on an active leaf
// account is accepted. The account is an asset (so functional_class and program_id
// stay NULL and the other three triggers pass with a plain amount split).
func TestSplitsActiveLeafAccepted(t *testing.T) {
	sqldb := testutil.NewDB(t)
	txn := mkTxn(t, sqldb)
	acct := mkAccount(t, sqldb, "asset")

	if _, err := sqldb.Exec(
		`INSERT INTO splits (transaction_id, account_id, amount, position)
		 VALUES (?, ?, 100, 0)`, txn, acct,
	); err != nil {
		t.Fatalf("split on an active leaf asset account rejected but should be allowed: %v", err)
	}
}

// TestSplitsUpdateRepointToPlaceholderRejected proves the BEFORE UPDATE variant of
// trg_splits_leaf_active_only fires: re-pointing an existing split's account_id at a
// placeholder (an account with a child) is rejected, not just the INSERT path. The
// listed p08.1 tests are insert-only; this one covers the update trigger so p08.2/
// p16 know the leaf/active re-check fires on split UPDATE too.
func TestSplitsUpdateRepointToPlaceholderRejected(t *testing.T) {
	sqldb := testutil.NewDB(t)
	txn := mkTxn(t, sqldb)

	leaf := mkAccount(t, sqldb, "asset")
	res, err := sqldb.Exec(
		`INSERT INTO splits (transaction_id, account_id, amount, position)
		 VALUES (?, ?, 100, 0)`, txn, leaf,
	)
	if err != nil {
		t.Fatalf("seed split on active leaf: %v", err)
	}
	splitID, _ := res.LastInsertId()

	placeholder := mkAccount(t, sqldb, "asset")
	if _, err := sqldb.Exec(
		`INSERT INTO accounts (parent_id, type, default_currency, created_at)
		 VALUES (?, 'asset', 'USD', '2025-01-01T00:00:00Z')`, placeholder,
	); err != nil {
		t.Fatalf("seed child (making placeholder): %v", err)
	}

	if _, err := sqldb.Exec(
		`UPDATE splits SET account_id = ? WHERE id = ?`, placeholder, splitID,
	); err == nil {
		t.Fatal("re-pointing a split at a placeholder account succeeded; trg_splits_leaf_active_only_update does not fire on UPDATE")
	}
}

// TestAccountsNoChildrenOverSplits proves trg_accounts_no_children_over_splits: an
// account that already holds splits cannot gain a child (a placeholder that holds
// splits can't gain children).
func TestAccountsNoChildrenOverSplits(t *testing.T) {
	sqldb := testutil.NewDB(t)
	txn := mkTxn(t, sqldb)
	acct := mkAccount(t, sqldb, "asset")

	if _, err := sqldb.Exec(
		`INSERT INTO splits (transaction_id, account_id, amount, position)
		 VALUES (?, ?, 100, 0)`, txn, acct,
	); err != nil {
		t.Fatalf("seed split on leaf account: %v", err)
	}

	_, err := sqldb.Exec(
		`INSERT INTO accounts (parent_id, type, default_currency, created_at)
		 VALUES (?, 'asset', 'USD', '2025-01-01T00:00:00Z')`, acct,
	)
	if err == nil {
		t.Fatal("inserting a child under an account with splits succeeded; trg_accounts_no_children_over_splits does not reject it")
	}
	if strings.Contains(err.Error(), "no such table") {
		t.Fatalf("insert failed because a table is missing, not the no-children-over-splits trigger: %v", err)
	}
}

// TestSplitsAmountZeroRejected proves the amount <> 0 CHECK.
func TestSplitsAmountZeroRejected(t *testing.T) {
	sqldb := testutil.NewDB(t)
	txn := mkTxn(t, sqldb)
	acct := mkAccount(t, sqldb, "asset")

	_, err := sqldb.Exec(
		`INSERT INTO splits (transaction_id, account_id, amount, position)
		 VALUES (?, ?, 0, 0)`, txn, acct,
	)
	if err == nil {
		t.Fatal("split with amount=0 accepted; the amount <> 0 CHECK is missing")
	}
	if strings.Contains(err.Error(), "no such table") {
		t.Fatalf("insert failed because a table is missing, not the amount CHECK: %v", err)
	}
}

// TestSplitsExpenseRequiresFunction proves trg_splits_function_matches_type rejects
// an expense split with a NULL functional_class. program_id is set (root, id 1) so
// the program trigger is satisfied and only the function trigger can fire.
func TestSplitsExpenseRequiresFunction(t *testing.T) {
	sqldb := testutil.NewDB(t)
	txn := mkTxn(t, sqldb)
	acct := mkAccount(t, sqldb, "expense")

	_, err := sqldb.Exec(
		`INSERT INTO splits (transaction_id, account_id, amount, program_id, functional_class, position)
		 VALUES (?, ?, 100, 1, NULL, 0)`, txn, acct,
	)
	if err == nil {
		t.Fatal("expense split with NULL functional_class accepted; trg_splits_function_matches_type does not require a class on expense splits")
	}
	if strings.Contains(err.Error(), "no such table") {
		t.Fatalf("insert failed because a table is missing, not the function-matches-type trigger: %v", err)
	}
}

// TestSplitsNonExpenseFunctionRejected proves the other side of
// trg_splits_function_matches_type: a non-expense split carrying a functional_class
// is rejected. The account is an asset (so program_id stays NULL and the program
// trigger passes); only the function trigger can fire.
func TestSplitsNonExpenseFunctionRejected(t *testing.T) {
	sqldb := testutil.NewDB(t)
	txn := mkTxn(t, sqldb)
	acct := mkAccount(t, sqldb, "asset")

	_, err := sqldb.Exec(
		`INSERT INTO splits (transaction_id, account_id, amount, functional_class, position)
		 VALUES (?, ?, 100, 'program', 0)`, txn, acct,
	)
	if err == nil {
		t.Fatal("non-expense split with a functional_class accepted; trg_splits_function_matches_type does not require NULL on non-expense splits")
	}
	if strings.Contains(err.Error(), "no such table") {
		t.Fatalf("insert failed because a table is missing, not the function-matches-type trigger: %v", err)
	}
}

// TestSplitsExpenseWithFunctionAccepted proves the positive side: a valid expense
// split with a functional_class (and the required program_id) is accepted.
func TestSplitsExpenseWithFunctionAccepted(t *testing.T) {
	sqldb := testutil.NewDB(t)
	txn := mkTxn(t, sqldb)
	acct := mkAccount(t, sqldb, "expense")

	if _, err := sqldb.Exec(
		`INSERT INTO splits (transaction_id, account_id, amount, program_id, functional_class, position)
		 VALUES (?, ?, 100, 1, 'program', 0)`, txn, acct,
	); err != nil {
		t.Fatalf("valid expense split with a functional_class rejected but should be allowed: %v", err)
	}
}

// TestSplitsAssetNullFunctionAccepted proves a valid asset split with a NULL
// functional_class (and NULL program_id) is accepted.
func TestSplitsAssetNullFunctionAccepted(t *testing.T) {
	sqldb := testutil.NewDB(t)
	txn := mkTxn(t, sqldb)
	acct := mkAccount(t, sqldb, "asset")

	if _, err := sqldb.Exec(
		`INSERT INTO splits (transaction_id, account_id, amount, position)
		 VALUES (?, ?, 100, 0)`, txn, acct,
	); err != nil {
		t.Fatalf("valid asset split with NULL functional_class rejected but should be allowed: %v", err)
	}
}

// TestSplitsRERequiresProgram proves trg_splits_program_matches_type rejects a
// revenue/expense split with a NULL program_id. A revenue account is used so
// functional_class stays NULL and the function trigger passes; only the program
// trigger can fire.
func TestSplitsRERequiresProgram(t *testing.T) {
	sqldb := testutil.NewDB(t)
	txn := mkTxn(t, sqldb)
	acct := mkAccount(t, sqldb, "revenue")

	_, err := sqldb.Exec(
		`INSERT INTO splits (transaction_id, account_id, amount, program_id, position)
		 VALUES (?, ?, 100, NULL, 0)`, txn, acct,
	)
	if err == nil {
		t.Fatal("R/E split with NULL program_id accepted; trg_splits_program_matches_type does not require a program on R/E splits")
	}
	if strings.Contains(err.Error(), "no such table") {
		t.Fatalf("insert failed because a table is missing, not the program-matches-type trigger: %v", err)
	}
}

// TestSplitsBalanceSheetProgramRejected proves the other side: an A/L/E split
// carrying a program_id is rejected. The account is an asset (functional_class
// stays NULL); only the program trigger can fire.
func TestSplitsBalanceSheetProgramRejected(t *testing.T) {
	sqldb := testutil.NewDB(t)
	txn := mkTxn(t, sqldb)
	acct := mkAccount(t, sqldb, "asset")

	_, err := sqldb.Exec(
		`INSERT INTO splits (transaction_id, account_id, amount, program_id, position)
		 VALUES (?, ?, 100, 1, 0)`, txn, acct,
	)
	if err == nil {
		t.Fatal("A/L/E split with a program_id accepted; trg_splits_program_matches_type does not require NULL on non-R/E splits")
	}
	if strings.Contains(err.Error(), "no such table") {
		t.Fatalf("insert failed because a table is missing, not the program-matches-type trigger: %v", err)
	}
}

// TestSplitsREWithProgramAccepted proves the positive side: a valid revenue split
// with a program_id (and NULL functional_class) is accepted.
func TestSplitsREWithProgramAccepted(t *testing.T) {
	sqldb := testutil.NewDB(t)
	txn := mkTxn(t, sqldb)
	acct := mkAccount(t, sqldb, "revenue")

	if _, err := sqldb.Exec(
		`INSERT INTO splits (transaction_id, account_id, amount, program_id, position)
		 VALUES (?, ?, 100, 1, 0)`, txn, acct,
	); err != nil {
		t.Fatalf("valid revenue split with a program_id rejected but should be allowed: %v", err)
	}
}

// TestSplitsBalanceSheetNullProgramAccepted proves a valid A/L/E split with a NULL
// program_id is accepted.
func TestSplitsBalanceSheetNullProgramAccepted(t *testing.T) {
	sqldb := testutil.NewDB(t)
	txn := mkTxn(t, sqldb)
	acct := mkAccount(t, sqldb, "liability")

	if _, err := sqldb.Exec(
		`INSERT INTO splits (transaction_id, account_id, amount, position)
		 VALUES (?, ?, 100, 0)`, txn, acct,
	); err != nil {
		t.Fatalf("valid liability split with NULL program_id rejected but should be allowed: %v", err)
	}
}

// TestTxnCurrencyDeleteBlocked proves the transactions.currency FK is enforced:
// deleting a currency a transaction references is rejected. EUR is used (seeded,
// otherwise unreferenced) so the rejection is provably from this transaction, not a
// pre-existing seed reference like USD.
func TestTxnCurrencyDeleteBlocked(t *testing.T) {
	sqldb := testutil.NewDB(t)

	if _, err := sqldb.Exec(
		`INSERT INTO transactions (date, subsidiary_id, currency) VALUES ('2025-01-01', 1, 'EUR')`,
	); err != nil {
		if strings.Contains(err.Error(), "no such table") {
			t.Fatalf("cannot seed a transaction because transactions is missing: %v", err)
		}
		t.Fatalf("seed EUR transaction: %v", err)
	}

	if _, err := sqldb.Exec(`DELETE FROM currencies WHERE code = 'EUR'`); err == nil {
		t.Fatal("deleting a currency referenced by a transaction succeeded; the currency FK is not enforced")
	}
}

// TestTxnSubsidiaryDeleteBlocked proves the transactions.subsidiary_id FK: deleting
// a subsidiary a transaction references is rejected. A fresh child subsidiary is
// used so the rejection is provably from this transaction (the root is referenced
// elsewhere).
func TestTxnSubsidiaryDeleteBlocked(t *testing.T) {
	sqldb := testutil.NewDB(t)

	res, err := sqldb.Exec(
		`INSERT INTO subsidiaries (parent_id, name, base_currency) VALUES (1, 'Branch', 'USD')`,
	)
	if err != nil {
		t.Fatalf("seed child subsidiary: %v", err)
	}
	sub, _ := res.LastInsertId()

	if _, err := sqldb.Exec(
		`INSERT INTO transactions (date, subsidiary_id, currency) VALUES ('2025-01-01', ?, 'USD')`, sub,
	); err != nil {
		if strings.Contains(err.Error(), "no such table") {
			t.Fatalf("cannot seed a transaction because transactions is missing: %v", err)
		}
		t.Fatalf("seed transaction: %v", err)
	}

	if _, err := sqldb.Exec(`DELETE FROM subsidiaries WHERE id = ?`, sub); err == nil {
		t.Fatal("deleting a subsidiary referenced by a transaction succeeded; the subsidiary FK is not enforced")
	}
}

// TestTxnPayeeDeleteBlocked proves the transactions.payee_id FK: deleting a payee a
// transaction references is rejected.
func TestTxnPayeeDeleteBlocked(t *testing.T) {
	sqldb := testutil.NewDB(t)

	res, err := sqldb.Exec(`INSERT INTO payees (name) VALUES ('Acme')`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			t.Fatalf("cannot seed a payee because payees is missing: %v", err)
		}
		t.Fatalf("seed payee: %v", err)
	}
	payee, _ := res.LastInsertId()

	if _, err := sqldb.Exec(
		`INSERT INTO transactions (date, subsidiary_id, payee_id, currency)
		 VALUES ('2025-01-01', 1, ?, 'USD')`, payee,
	); err != nil {
		t.Fatalf("seed transaction: %v", err)
	}

	if _, err := sqldb.Exec(`DELETE FROM payees WHERE id = ?`, payee); err == nil {
		t.Fatal("deleting a payee referenced by a transaction succeeded; the payee FK is not enforced")
	}
}

// TestSplitAccountDeleteBlocked proves the splits.account_id FK: deleting an account
// a split references is rejected. The account is a fresh asset leaf only referenced
// by this split.
func TestSplitAccountDeleteBlocked(t *testing.T) {
	sqldb := testutil.NewDB(t)
	txn := mkTxn(t, sqldb)
	acct := mkAccount(t, sqldb, "asset")

	if _, err := sqldb.Exec(
		`INSERT INTO splits (transaction_id, account_id, amount, position)
		 VALUES (?, ?, 100, 0)`, txn, acct,
	); err != nil {
		t.Fatalf("seed split: %v", err)
	}

	if _, err := sqldb.Exec(`DELETE FROM accounts WHERE id = ?`, acct); err == nil {
		t.Fatal("deleting an account referenced by a split succeeded; the account FK is not enforced")
	}
}

// TestSplitFundDeleteBlocked proves the splits.fund_id FK: deleting a fund a split
// references is rejected. The fund is fresh and only this split references it.
func TestSplitFundDeleteBlocked(t *testing.T) {
	sqldb := testutil.NewDB(t)
	txn := mkTxn(t, sqldb)
	acct := mkAccount(t, sqldb, "asset")

	res, err := sqldb.Exec(`INSERT INTO funds (name, restriction) VALUES ('Grant', 'purpose')`)
	if err != nil {
		t.Fatalf("seed fund: %v", err)
	}
	fund, _ := res.LastInsertId()

	if _, err := sqldb.Exec(
		`INSERT INTO splits (transaction_id, account_id, amount, fund_id, position)
		 VALUES (?, ?, 100, ?, 0)`, txn, acct, fund,
	); err != nil {
		t.Fatalf("seed split with fund: %v", err)
	}

	if _, err := sqldb.Exec(`DELETE FROM funds WHERE id = ?`, fund); err == nil {
		t.Fatal("deleting a fund referenced by a split succeeded; the fund FK is not enforced")
	}
}

// TestSplitProgramDeleteBlocked proves the splits.program_id FK: deleting a program
// a split references is rejected. A fresh child program is used so the rejection is
// provably from this split (the root program is referenced elsewhere).
func TestSplitProgramDeleteBlocked(t *testing.T) {
	sqldb := testutil.NewDB(t)
	txn := mkTxn(t, sqldb)
	acct := mkAccount(t, sqldb, "revenue")

	res, err := sqldb.Exec(`INSERT INTO programs (parent_id, name) VALUES (1, 'Outreach')`)
	if err != nil {
		t.Fatalf("seed child program: %v", err)
	}
	prog, _ := res.LastInsertId()

	if _, err := sqldb.Exec(
		`INSERT INTO splits (transaction_id, account_id, amount, program_id, position)
		 VALUES (?, ?, 100, ?, 0)`, txn, acct, prog,
	); err != nil {
		t.Fatalf("seed split with program: %v", err)
	}

	if _, err := sqldb.Exec(`DELETE FROM programs WHERE id = ?`, prog); err == nil {
		t.Fatal("deleting a program referenced by a split succeeded; the program FK is not enforced")
	}
}

// TestTransactionsSplitsIndexesExist proves the six named indexes (Appendix A) and
// the three *_versions_entity indexes exist (queried from sqlite_master).
func TestTransactionsSplitsIndexesExist(t *testing.T) {
	sqldb := testutil.NewDB(t)

	for _, idx := range []string{
		"splits_account", "splits_txn", "splits_fund", "splits_program",
		"txn_date", "txn_sub",
		"payees_versions_entity", "transactions_versions_entity", "splits_versions_entity",
	} {
		var name string
		err := sqldb.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, idx,
		).Scan(&name)
		if err != nil {
			t.Errorf("index %q not found in sqlite_master: %v", idx, err)
		}
	}
}

// TestTransactionsSplitsVersionsTablesExist proves the three *_versions twins exist
// and are queryable (AGENTS rule 5). reconciliation_id must NOT appear on
// splits_versions -- it is deferred to p16.1.
func TestTransactionsSplitsVersionsTablesExist(t *testing.T) {
	sqldb := testutil.NewDB(t)

	for _, tbl := range []string{
		"payees_versions", "transactions_versions", "splits_versions",
	} {
		var n int
		if err := sqldb.QueryRow(`SELECT count(*) FROM ` + tbl).Scan(&n); err != nil {
			t.Errorf("query %s: %v", tbl, err)
		}
	}

	// reconciliation_id is deferred to p16.1: it must be absent from both splits
	// and splits_versions. A SELECT of the column must fail with "no such column".
	if _, err := sqldb.Exec(`SELECT reconciliation_id FROM splits LIMIT 0`); err == nil {
		t.Error("splits.reconciliation_id exists; it must be deferred to p16.1")
	}
	if _, err := sqldb.Exec(`SELECT reconciliation_id FROM splits_versions LIMIT 0`); err == nil {
		t.Error("splits_versions.reconciliation_id exists; it must be deferred to p16.1")
	}
}
