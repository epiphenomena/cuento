package db_test

import (
	"strings"
	"testing"

	"cuento/internal/testutil"
)

// p07.2 adds the funds dimension (D20): funds document grants/restricted gifts,
// scope to one or more subsidiaries via fund_subsidiaries (not inherited, Q1),
// and optionally to a program subtree. Every split carries fund_id (NULL =
// unrestricted); there is NO seeded "general fund" row. These tests exercise the
// 00009 migration's schema, CHECK constraints, FKs, and versions twins via direct
// SQL on a migrated harness db (AGENTS testing conventions permit direct SQL for
// schema checks; the store path is p07.3). Written before 00009 exists -- each
// fails with "no such table: funds" until the migration lands.

// TestFundsRestrictionCheck proves the restriction CHECK: 'purpose'/'time'/
// 'perpetual' are accepted; any other value is rejected.
func TestFundsRestrictionCheck(t *testing.T) {
	sqldb := testutil.NewDB(t)

	_, err := sqldb.Exec(
		`INSERT INTO funds (name, restriction) VALUES ('Bogus Fund', 'bogus')`,
	)
	if err == nil {
		t.Fatal("fund with restriction='bogus' inserted; the restriction CHECK is not enforced")
	}
	if strings.Contains(err.Error(), "no such table") {
		t.Fatalf("insert failed because the table is missing, not the restriction CHECK: %v", err)
	}

	for _, r := range []string{"purpose", "time", "perpetual"} {
		if _, err := sqldb.Exec(
			`INSERT INTO funds (name, restriction) VALUES (?, ?)`, "Fund "+r, r,
		); err != nil {
			t.Fatalf("fund with valid restriction=%q rejected: %v", r, err)
		}
	}
}

// TestFundsProgramFK proves program_id references programs(id): a nonexistent
// program id is rejected; NULL is allowed; the seeded root program (id 1) is a
// valid target.
func TestFundsProgramFK(t *testing.T) {
	sqldb := testutil.NewDB(t)

	_, err := sqldb.Exec(
		`INSERT INTO funds (name, restriction, program_id) VALUES ('Bad Program', 'purpose', 9999)`,
	)
	if err == nil {
		t.Fatal("fund with nonexistent program_id inserted; the programs FK is not enforced")
	}
	if strings.Contains(err.Error(), "no such table") {
		t.Fatalf("insert failed because a table is missing, not the FK: %v", err)
	}
	if strings.Contains(err.Error(), "no such column") {
		t.Fatalf("insert failed because program_id is missing, not the FK: %v", err)
	}

	// NULL program_id is allowed (the scope is optional, D20).
	if _, err := sqldb.Exec(
		`INSERT INTO funds (name, restriction, program_id) VALUES ('No Program', 'purpose', NULL)`,
	); err != nil {
		t.Fatalf("fund with NULL program_id rejected: %v", err)
	}

	// The seeded root program (id 1) is a valid target.
	if _, err := sqldb.Exec(
		`INSERT INTO funds (name, restriction, program_id) VALUES ('Root Program', 'purpose', 1)`,
	); err != nil {
		t.Fatalf("fund with valid program_id=1 rejected: %v", err)
	}
}

// TestFundsDateGlobChecks proves the start_date/end_date GLOB CHECKs. The shape
// is the SAME as transactions.date (Appendix A): a plain YYYY-MM-DD digit shape,
// NULL allowed. Note the shape is deliberately loose (digit-count only, like
// transactions.date): 'not-a-date' and a wrong-digit-count string are rejected;
// a well-formed '2025-01-01' and NULL are accepted. (See the p07.2 DECISIONS
// note: the task listed '2025-13-99' as a rejection sample, but that string
// matches the authoritative loose transactions.date shape and IS accepted; the
// sample was swapped for a shape-invalid one to keep schema consistency.)
func TestFundsDateGlobChecks(t *testing.T) {
	sqldb := testutil.NewDB(t)

	// Shape-invalid strings are rejected on start_date...
	for _, bad := range []string{"not-a-date", "2025-1-1"} {
		if _, err := sqldb.Exec(
			`INSERT INTO funds (name, restriction, start_date) VALUES ('Bad Start', 'time', ?)`, bad,
		); err == nil {
			t.Fatalf("fund with start_date=%q inserted; the start_date GLOB CHECK is not enforced", bad)
		} else if strings.Contains(err.Error(), "no such table") {
			t.Fatalf("insert failed because the table is missing, not the start_date CHECK: %v", err)
		}
	}

	// ...and on end_date.
	for _, bad := range []string{"not-a-date", "2025-1-1"} {
		if _, err := sqldb.Exec(
			`INSERT INTO funds (name, restriction, end_date) VALUES ('Bad End', 'time', ?)`, bad,
		); err == nil {
			t.Fatalf("fund with end_date=%q inserted; the end_date GLOB CHECK is not enforced", bad)
		}
	}

	// A well-formed date on both is accepted.
	if _, err := sqldb.Exec(
		`INSERT INTO funds (name, restriction, start_date, end_date)
		 VALUES ('Good Dates', 'time', '2025-01-01', '2025-12-31')`,
	); err != nil {
		t.Fatalf("fund with well-formed dates rejected: %v", err)
	}

	// NULL on both is accepted (dates are optional).
	if _, err := sqldb.Exec(
		`INSERT INTO funds (name, restriction, start_date, end_date)
		 VALUES ('Null Dates', 'perpetual', NULL, NULL)`,
	); err != nil {
		t.Fatalf("fund with NULL dates rejected: %v", err)
	}
}

// TestFundSubsidiariesFK proves fund_subsidiaries references funds(id) and
// subsidiaries(id): referencing a nonexistent fund or subsidiary is rejected; a
// valid (fund, subsidiary) pair is accepted. A real fund is seeded first (needs a
// valid restriction); the valid subsidiary is the seeded root (id 1).
func TestFundSubsidiariesFK(t *testing.T) {
	sqldb := testutil.NewDB(t)

	res, err := sqldb.Exec(
		`INSERT INTO funds (name, restriction) VALUES ('Scoped Fund', 'purpose')`,
	)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			t.Fatalf("cannot seed a fund because funds is missing: %v", err)
		}
		t.Fatalf("seed fund: %v", err)
	}
	fundID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("fund last insert id: %v", err)
	}

	// A nonexistent fund_id is rejected by the funds FK.
	if _, err := sqldb.Exec(
		`INSERT INTO fund_subsidiaries (fund_id, subsidiary_id) VALUES (9999, 1)`,
	); err == nil {
		t.Fatal("fund_subsidiaries row with nonexistent fund_id inserted; the funds FK is not enforced")
	} else if strings.Contains(err.Error(), "no such table") {
		t.Fatalf("insert failed because fund_subsidiaries is missing, not the FK: %v", err)
	}

	// A nonexistent subsidiary_id is rejected by the subsidiaries FK.
	if _, err := sqldb.Exec(
		`INSERT INTO fund_subsidiaries (fund_id, subsidiary_id) VALUES (?, 9999)`, fundID,
	); err == nil {
		t.Fatal("fund_subsidiaries row with nonexistent subsidiary_id inserted; the subsidiaries FK is not enforced")
	}

	// A valid pair (the seeded fund + root subsidiary id 1) is accepted.
	if _, err := sqldb.Exec(
		`INSERT INTO fund_subsidiaries (fund_id, subsidiary_id) VALUES (?, 1)`, fundID,
	); err != nil {
		t.Fatalf("valid fund_subsidiaries pair rejected: %v", err)
	}
}

// TestFundsVersionsTablesExist proves both versions twins exist and are queryable
// (AGENTS rule 5: every versioned table has a *_versions twin). funds_versions is
// the standard single-id shape (entity_id = fund id); fund_subsidiaries_versions
// is the composite shape (entity_id = fund_id, snapshot subsidiary_id), like
// account_subsidiaries_versions.
func TestFundsVersionsTablesExist(t *testing.T) {
	sqldb := testutil.NewDB(t)

	var n int
	if err := sqldb.QueryRow(`SELECT count(*) FROM funds_versions`).Scan(&n); err != nil {
		t.Fatalf("query funds_versions: %v", err)
	}
	if err := sqldb.QueryRow(`SELECT count(*) FROM fund_subsidiaries_versions`).Scan(&n); err != nil {
		t.Fatalf("query fund_subsidiaries_versions: %v", err)
	}
}
