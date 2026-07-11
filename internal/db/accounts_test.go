package db_test

import (
	"strings"
	"testing"

	"cuento/internal/testutil"
)

// p05.1 introduces the accounts family: accounts + account_names +
// account_subsidiaries, their three *_versions twins, the static form990_lines
// reference table, and two row-local triggers (trg_accounts_parent_typeclass,
// trg_accounts_function_expense_only). These tests exercise the migration's
// schema, triggers, and seeded reference data via direct SQL on a migrated
// harness db (AGENTS testing conventions permit direct SQL for schema checks;
// the store path is p05.2). Written before 00005 exists — each fails with
// "no such table: accounts" (or form990_lines) until the migration lands.

// TestAccountsExpenseUnderAssetRejected proves trg_accounts_parent_typeclass
// rejects an expense child under an asset parent: for an A/L/E parent the child
// type must EQUAL the parent type.
func TestAccountsExpenseUnderAssetRejected(t *testing.T) {
	sqldb := testutil.NewDB(t)

	res, err := sqldb.Exec(
		`INSERT INTO accounts (parent_id, type, default_currency, created_at)
		 VALUES (NULL, 'asset', 'USD', '2025-01-01T00:00:00Z')`,
	)
	if err != nil {
		t.Fatalf("insert root asset account: %v", err)
	}
	parentID, _ := res.LastInsertId()

	_, err = sqldb.Exec(
		`INSERT INTO accounts (parent_id, type, default_currency, created_at)
		 VALUES (?, 'expense', 'USD', '2025-01-01T00:00:00Z')`, parentID,
	)
	if err == nil {
		t.Fatal("expense under asset succeeded; trg_accounts_parent_typeclass does not reject cross-type A/L/E children")
	}
	if strings.Contains(err.Error(), "no such table") {
		t.Fatalf("insert failed because the table is missing, not the typeclass trigger: %v", err)
	}
}

// TestAccountsAssetUnderAssetSucceeds proves an asset child under an asset
// parent is allowed (A/L/E child type == parent type).
func TestAccountsAssetUnderAssetSucceeds(t *testing.T) {
	sqldb := testutil.NewDB(t)

	res, err := sqldb.Exec(
		`INSERT INTO accounts (parent_id, type, default_currency, created_at)
		 VALUES (NULL, 'asset', 'USD', '2025-01-01T00:00:00Z')`,
	)
	if err != nil {
		t.Fatalf("insert root asset account: %v", err)
	}
	parentID, _ := res.LastInsertId()

	if _, err := sqldb.Exec(
		`INSERT INTO accounts (parent_id, type, default_currency, created_at)
		 VALUES (?, 'asset', 'USD', '2025-01-01T00:00:00Z')`, parentID,
	); err != nil {
		t.Fatalf("asset under asset rejected but should be allowed: %v", err)
	}
}

// TestAccountsRevenueUnderExpenseSucceeds proves revenue/expense interleave
// freely under an R/E parent (D11): a revenue child under an expense parent is
// allowed.
func TestAccountsRevenueUnderExpenseSucceeds(t *testing.T) {
	sqldb := testutil.NewDB(t)

	res, err := sqldb.Exec(
		`INSERT INTO accounts (parent_id, type, default_currency, created_at)
		 VALUES (NULL, 'expense', 'USD', '2025-01-01T00:00:00Z')`,
	)
	if err != nil {
		t.Fatalf("insert root expense account: %v", err)
	}
	parentID, _ := res.LastInsertId()

	if _, err := sqldb.Exec(
		`INSERT INTO accounts (parent_id, type, default_currency, created_at)
		 VALUES (?, 'revenue', 'USD', '2025-01-01T00:00:00Z')`, parentID,
	); err != nil {
		t.Fatalf("revenue under expense rejected but R/E interleave should be allowed: %v", err)
	}
}

// TestAccountsAssetUnderRevenueRejected covers the other typeclass edge: an
// A/L/E-typed child under an R/E parent must be rejected (child not in R/E).
func TestAccountsAssetUnderRevenueRejected(t *testing.T) {
	sqldb := testutil.NewDB(t)

	res, err := sqldb.Exec(
		`INSERT INTO accounts (parent_id, type, default_currency, created_at)
		 VALUES (NULL, 'revenue', 'USD', '2025-01-01T00:00:00Z')`,
	)
	if err != nil {
		t.Fatalf("insert root revenue account: %v", err)
	}
	parentID, _ := res.LastInsertId()

	_, err = sqldb.Exec(
		`INSERT INTO accounts (parent_id, type, default_currency, created_at)
		 VALUES (?, 'asset', 'USD', '2025-01-01T00:00:00Z')`, parentID,
	)
	if err == nil {
		t.Fatal("asset under revenue succeeded; an A/L/E child under an R/E parent must be rejected")
	}
	if strings.Contains(err.Error(), "no such table") {
		t.Fatalf("insert failed because the table is missing, not the typeclass trigger: %v", err)
	}
}

// TestAccountsFunctionalClassOnNonExpenseRejected proves
// trg_accounts_function_expense_only rejects a non-NULL functional_class on a
// non-expense account (D21: default class is expense-only). The value 'program'
// is a valid enum member, so only the trigger — not the column CHECK — can
// reject; that isolates the trigger under test.
func TestAccountsFunctionalClassOnNonExpenseRejected(t *testing.T) {
	sqldb := testutil.NewDB(t)

	_, err := sqldb.Exec(
		`INSERT INTO accounts (parent_id, type, default_currency, functional_class, created_at)
		 VALUES (NULL, 'asset', 'USD', 'program', '2025-01-01T00:00:00Z')`,
	)
	if err == nil {
		t.Fatal("functional_class on a non-expense account succeeded; trg_accounts_function_expense_only does not enforce expense-only")
	}
	if strings.Contains(err.Error(), "no such table") {
		t.Fatalf("insert failed because the table is missing, not the function-expense-only trigger: %v", err)
	}
}

// TestAccountsFunctionalClassOnExpenseAllowed proves a non-NULL functional_class
// IS allowed on an expense account (the positive side of expense-only).
func TestAccountsFunctionalClassOnExpenseAllowed(t *testing.T) {
	sqldb := testutil.NewDB(t)

	if _, err := sqldb.Exec(
		`INSERT INTO accounts (parent_id, type, default_currency, functional_class, created_at)
		 VALUES (NULL, 'expense', 'USD', 'management', '2025-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("functional_class on an expense account rejected but should be allowed: %v", err)
	}
}

// TestAccountsFunctionalClassEnumGuard proves the column CHECK rejects a
// functional_class outside the enum. The account is expense-typed so the
// expense-only trigger is satisfied and the ONLY possible abort is the CHECK.
func TestAccountsFunctionalClassEnumGuard(t *testing.T) {
	sqldb := testutil.NewDB(t)

	_, err := sqldb.Exec(
		`INSERT INTO accounts (parent_id, type, default_currency, functional_class, created_at)
		 VALUES (NULL, 'expense', 'USD', 'bogus', '2025-01-01T00:00:00Z')`,
	)
	if err == nil {
		t.Fatal("functional_class outside the enum accepted; the CHECK constraint is missing")
	}
	if strings.Contains(err.Error(), "no such table") {
		t.Fatalf("insert failed because the table is missing, not the enum CHECK: %v", err)
	}
}

// TestForm990LinesSeeded spot-checks the seeded reference rows the fixture
// (Appendix D) references: their part and account_types must be correct, or the
// p05.2 Set990CodeTypeMismatch check and the fixture's 990 rollups will be wrong.
func TestForm990LinesSeeded(t *testing.T) {
	sqldb := testutil.NewDB(t)

	cases := []struct {
		code         string
		wantPart     string
		wantAccTypes string // exact account_types CSV
	}{
		{"VIII.1e", "VIII", "revenue"}, // Government grants (revenue)
		{"VIII.1f", "VIII", "revenue"}, // All other contributions (revenue)
		{"VIII.2", "VIII", "revenue"},  // Program service revenue (revenue)
		{"IX.7", "IX", "expense"},      // Other salaries and wages (chosen salaries line)
		{"IX.16", "IX", "expense"},     // Occupancy (expense)
		{"IX.24e", "IX", "expense"},    // All other expenses (chosen bank-fees line)
		{"X.10", "X", "asset"},         // Land, buildings, and equipment (asset)
	}
	for _, c := range cases {
		var part, accTypes string
		err := sqldb.QueryRow(
			`SELECT part, account_types FROM form990_lines WHERE code = ?`, c.code,
		).Scan(&part, &accTypes)
		if err != nil {
			t.Errorf("form990_lines code %q not seeded: %v", c.code, err)
			continue
		}
		if part != c.wantPart {
			t.Errorf("form990_lines %q part = %q, want %q", c.code, part, c.wantPart)
		}
		if accTypes != c.wantAccTypes {
			t.Errorf("form990_lines %q account_types = %q, want %q", c.code, accTypes, c.wantAccTypes)
		}
	}
}

// TestAccountsForm990CodeFK proves accounts.form990_code references
// form990_lines: a nonexistent code is rejected by the FK.
func TestAccountsForm990CodeFK(t *testing.T) {
	sqldb := testutil.NewDB(t)

	_, err := sqldb.Exec(
		`INSERT INTO accounts (parent_id, type, default_currency, form990_code, created_at)
		 VALUES (NULL, 'revenue', 'USD', 'ZZZ.999', '2025-01-01T00:00:00Z')`,
	)
	if err == nil {
		t.Fatal("account with nonexistent form990_code inserted; the form990_lines FK is not enforced")
	}
	if strings.Contains(err.Error(), "no such table") {
		t.Fatalf("insert failed because the table is missing, not the form990_lines FK: %v", err)
	}
}

// TestAccountsVersionsTablesExist proves all three *_versions twins exist and are
// queryable (AGENTS rule 5: every versioned table has a *_versions twin). The
// version-append queries + AssertVersioned extension land in p05.2.
func TestAccountsVersionsTablesExist(t *testing.T) {
	sqldb := testutil.NewDB(t)

	for _, tbl := range []string{
		"accounts_versions",
		"account_names_versions",
		"account_subsidiaries_versions",
	} {
		var n int
		if err := sqldb.QueryRow(`SELECT count(*) FROM ` + tbl).Scan(&n); err != nil {
			t.Errorf("query %s: %v", tbl, err)
		}
	}
}
