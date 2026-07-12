package db_test

import (
	"database/sql"
	"strings"
	"testing"

	"cuento/internal/testutil"
)

// p16.1 adds reconciliations + its versions twin, splits.reconciliation_id
// (LIVE-ONLY, deliberately NOT on splits_versions -- see 00014's header note and
// DECISIONS p16.1: clearing a split is operational metadata, not an audited split
// version), and one row-local trigger:
//
//	trg_split_locked_when_finalized  a split cleared against a FINALIZED recon may
//	                                 not change amount/account_id/transaction_id/
//	                                 fund_id/reconciliation_id (D13).
//
// These tests exercise the migration via direct SQL (AGENTS testing conventions
// permit direct SQL for schema checks; the store lifecycle is p16.2). They reuse
// mkTxn / mkAccount from transactions_splits_test.go (same db_test package).

// mkRecon inserts a reconciliation on account acct with the given status and
// returns its id. statement_balance/currency are fixed (this table is exercised
// for the lock trigger and FK, not its own business rules -- those are p16.2).
func mkRecon(t *testing.T, sqldb *sql.DB, acct int64, status string) int64 {
	t.Helper()
	res, err := sqldb.Exec(
		`INSERT INTO reconciliations (account_id, statement_date, statement_balance, currency, status)
		 VALUES (?, '2025-01-31', 0, 'USD', ?)`, acct, status,
	)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			t.Fatalf("cannot seed reconciliation because reconciliations is missing: %v", err)
		}
		t.Fatalf("seed reconciliation (%s): %v", status, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("reconciliation last insert id: %v", err)
	}
	return id
}

// mkFund inserts a minimal fund and returns its id (for the fund_id lock test).
func mkFund(t *testing.T, sqldb *sql.DB) int64 {
	t.Helper()
	res, err := sqldb.Exec(`INSERT INTO funds (name, restriction) VALUES ('Recon Fund', 'purpose')`)
	if err != nil {
		t.Fatalf("seed fund: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("fund last insert id: %v", err)
	}
	return id
}

// mkClearedSplit inserts a plain asset split (fclass/program NULL, so the 00010
// triggers stay satisfied) cleared against recon, and returns its id.
func mkClearedSplit(t *testing.T, sqldb *sql.DB, txn, acct, recon int64) int64 {
	t.Helper()
	res, err := sqldb.Exec(
		`INSERT INTO splits (transaction_id, account_id, amount, reconciliation_id, position)
		 VALUES (?, ?, 100, ?, 0)`, txn, acct, recon,
	)
	if err != nil {
		t.Fatalf("seed cleared split: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("split last insert id: %v", err)
	}
	return id
}

// TestReconciliationsTablesExist proves the reconciliations table and its versions
// twin exist and are queryable, and that splits.reconciliation_id now exists while
// splits_versions.reconciliation_id does NOT (live-only decision, DECISIONS p16.1).
func TestReconciliationsTablesExist(t *testing.T) {
	sqldb := testutil.NewDB(t)

	for _, tbl := range []string{"reconciliations", "reconciliations_versions"} {
		var n int
		if err := sqldb.QueryRow(`SELECT count(*) FROM ` + tbl).Scan(&n); err != nil {
			t.Errorf("query %s: %v", tbl, err)
		}
	}

	// reconciliation_id is now a LIVE splits column (p16.1)...
	if _, err := sqldb.Exec(`SELECT reconciliation_id FROM splits LIMIT 0`); err != nil {
		t.Errorf("splits.reconciliation_id must exist after p16.1: %v", err)
	}
	// ...but stays OFF splits_versions (live-only: a toggle mints no split version).
	if _, err := sqldb.Exec(`SELECT reconciliation_id FROM splits_versions LIMIT 0`); err == nil {
		t.Error("splits_versions.reconciliation_id exists; reconciliation_id is live-only (must NOT be versioned)")
	}
}

// TestReconciliationIDFKValid proves the reconciliation_id FK: a split may
// reference a real reconciliation, and an insert referencing a non-existent one
// fails the FK (FKs are ON in the harness db).
func TestReconciliationIDFKValid(t *testing.T) {
	sqldb := testutil.NewDB(t)
	txn := mkTxn(t, sqldb)
	acct := mkAccount(t, sqldb, "asset")
	recon := mkRecon(t, sqldb, acct, "open")

	// Valid reference: accepted.
	if _, err := sqldb.Exec(
		`INSERT INTO splits (transaction_id, account_id, amount, reconciliation_id, position)
		 VALUES (?, ?, 100, ?, 0)`, txn, acct, recon,
	); err != nil {
		t.Fatalf("split referencing a valid reconciliation rejected but should be allowed: %v", err)
	}

	// Invalid reference: FK violation.
	_, err := sqldb.Exec(
		`INSERT INTO splits (transaction_id, account_id, amount, reconciliation_id, position)
		 VALUES (?, ?, 100, 999999, 1)`, txn, acct,
	)
	if err == nil {
		t.Fatal("split referencing a non-existent reconciliation succeeded; the reconciliation_id FK is not enforced")
	}
}

// TestSplitLockedWhenFinalized proves trg_split_locked_when_finalized rejects
// edits to a finalized recon's cleared split's financial fields: amount,
// account_id, transaction_id, fund_id. Each is its own subtest so a single failure
// is precise.
func TestSplitLockedWhenFinalized(t *testing.T) {
	// altAcct returns a SECOND active asset leaf so the account_id-change subtest
	// re-points to a valid leaf (otherwise trg_splits_leaf_active_only would abort
	// first, with the wrong message).
	cases := []struct {
		name   string
		update string // UPDATE ... WHERE id = ? (split id bound last)
		args   func(altAcct, altTxn, altFund int64) []any
	}{
		{"amount", `UPDATE splits SET amount = 200 WHERE id = ?`, func(_, _, _ int64) []any { return nil }},
		{"account_id", `UPDATE splits SET account_id = ? WHERE id = ?`, func(a, _, _ int64) []any { return []any{a} }},
		{"transaction_id", `UPDATE splits SET transaction_id = ? WHERE id = ?`, func(_, tx, _ int64) []any { return []any{tx} }},
		{"fund_id", `UPDATE splits SET fund_id = ? WHERE id = ?`, func(_, _, f int64) []any { return []any{f} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sqldb := testutil.NewDB(t)
			txn := mkTxn(t, sqldb)
			acct := mkAccount(t, sqldb, "asset")
			altAcct := mkAccount(t, sqldb, "asset")
			altTxn := mkTxn(t, sqldb)
			altFund := mkFund(t, sqldb)
			recon := mkRecon(t, sqldb, acct, "finalized")
			split := mkClearedSplit(t, sqldb, txn, acct, recon)

			args := append(tc.args(altAcct, altTxn, altFund), split)
			_, err := sqldb.Exec(tc.update, args...)
			if err == nil {
				t.Fatalf("UPDATE %s on a finalized-recon split succeeded; trg_split_locked_when_finalized did not fire", tc.name)
			}
			if !strings.Contains(err.Error(), "locked by a finalized reconciliation") {
				t.Fatalf("UPDATE %s aborted for the WRONG reason: %v", tc.name, err)
			}
		})
	}
}

// TestSplitEditableWhenOpen proves the SAME edits succeed while the recon is OPEN:
// the lock is strictly a finalized-recon guard (D13).
func TestSplitEditableWhenOpen(t *testing.T) {
	sqldb := testutil.NewDB(t)
	txn := mkTxn(t, sqldb)
	acct := mkAccount(t, sqldb, "asset")
	altAcct := mkAccount(t, sqldb, "asset")
	altTxn := mkTxn(t, sqldb)
	altFund := mkFund(t, sqldb)
	recon := mkRecon(t, sqldb, acct, "open")
	split := mkClearedSplit(t, sqldb, txn, acct, recon)

	// amount
	if _, err := sqldb.Exec(`UPDATE splits SET amount = 200 WHERE id = ?`, split); err != nil {
		t.Fatalf("amount edit on an OPEN-recon split rejected but should be allowed: %v", err)
	}
	// account_id (to another valid active asset leaf)
	if _, err := sqldb.Exec(`UPDATE splits SET account_id = ? WHERE id = ?`, altAcct, split); err != nil {
		t.Fatalf("account_id edit on an OPEN-recon split rejected but should be allowed: %v", err)
	}
	// transaction_id
	if _, err := sqldb.Exec(`UPDATE splits SET transaction_id = ? WHERE id = ?`, altTxn, split); err != nil {
		t.Fatalf("transaction_id edit on an OPEN-recon split rejected but should be allowed: %v", err)
	}
	// fund_id
	if _, err := sqldb.Exec(`UPDATE splits SET fund_id = ? WHERE id = ?`, altFund, split); err != nil {
		t.Fatalf("fund_id edit on an OPEN-recon split rejected but should be allowed: %v", err)
	}
}

// TestSplitMemoEditableWhenFinalized proves the lock guards ONLY the financial
// fields: memo (a non-financial column) is editable even on a finalized-recon
// split (matches the p16.2 store rule "memo/payee allowed").
func TestSplitMemoEditableWhenFinalized(t *testing.T) {
	sqldb := testutil.NewDB(t)
	txn := mkTxn(t, sqldb)
	acct := mkAccount(t, sqldb, "asset")
	recon := mkRecon(t, sqldb, acct, "finalized")
	split := mkClearedSplit(t, sqldb, txn, acct, recon)

	if _, err := sqldb.Exec(`UPDATE splits SET memo = 'note' WHERE id = ?`, split); err != nil {
		t.Fatalf("memo edit on a finalized-recon split rejected but should be allowed (only financial fields are locked): %v", err)
	}
}

// TestSplitCannotJoinFinalizedRecon proves the guard also blocks re-pointing
// reconciliation_id: an uncleared split cannot be moved INTO a finalized recon,
// and a cleared split cannot be moved OUT of one (the OR OLD/NEW branch).
func TestSplitCannotJoinFinalizedRecon(t *testing.T) {
	sqldb := testutil.NewDB(t)
	txn := mkTxn(t, sqldb)
	acct := mkAccount(t, sqldb, "asset")
	fin := mkRecon(t, sqldb, acct, "finalized")

	// An uncleared split (reconciliation_id NULL) cannot join a finalized recon.
	res, err := sqldb.Exec(
		`INSERT INTO splits (transaction_id, account_id, amount, position) VALUES (?, ?, 100, 0)`, txn, acct,
	)
	if err != nil {
		t.Fatalf("seed uncleared split: %v", err)
	}
	uncleared, _ := res.LastInsertId()
	if _, err := sqldb.Exec(`UPDATE splits SET reconciliation_id = ? WHERE id = ?`, fin, uncleared); err == nil {
		t.Fatal("moving an uncleared split INTO a finalized recon succeeded; the NEW-finalized guard did not fire")
	}

	// A split cleared in the finalized recon cannot be un-cleared (moved OUT).
	cleared := mkClearedSplit(t, sqldb, txn, acct, fin)
	if _, err := sqldb.Exec(`UPDATE splits SET reconciliation_id = NULL WHERE id = ?`, cleared); err == nil {
		t.Fatal("moving a split OUT of a finalized recon succeeded; the OLD-finalized guard did not fire")
	}
}
