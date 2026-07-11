package db_test

import (
	"strings"
	"testing"

	"cuento/internal/testutil"
)

// p07.1 adds the programs dimension (D24): a single-root tree structurally
// identical to subsidiaries MINUS base_currency, plus its versions twin, the
// single-root trigger pair, and an audit-consistent seed of the root program
// "General". These tests exercise the migration's schema, triggers, and seed via
// direct SQL on a migrated harness db (AGENTS testing conventions permit direct
// SQL for schema checks; the store path is the programs.go methods). Written
// before 00008 exists -- each fails with "no such table: programs" until the
// migration lands.

// TestProgramsSingleRootEnforced proves trg_programs_single_root rejects a SECOND
// parent_id-IS-NULL insert (BEFORE INSERT) and rejects orphaning a child into a
// second root (BEFORE UPDATE). The second root uses a distinct name so the abort
// can only come from the single-root trigger, not UNIQUE(name).
func TestProgramsSingleRootEnforced(t *testing.T) {
	sqldb := testutil.NewDB(t)

	// The seed already provides one root ('General'). A second root must be
	// rejected by the BEFORE INSERT trigger.
	_, err := sqldb.Exec(
		`INSERT INTO programs (parent_id, name) VALUES (NULL, 'Second Root')`,
	)
	if err == nil {
		t.Fatal("second NULL-parent insert succeeded; trg_programs_single_root does not enforce a single root")
	}
	if strings.Contains(err.Error(), "no such table") {
		t.Fatalf("insert failed because the table is missing, not the single-root trigger: %v", err)
	}

	// And BEFORE UPDATE: create a valid child under the root, then try to orphan
	// it into a second root. That, too, must be rejected.
	if _, err := sqldb.Exec(
		`INSERT INTO programs (parent_id, name) VALUES (1, 'Child')`,
	); err != nil {
		t.Fatalf("insert valid child under root: %v", err)
	}
	if _, err := sqldb.Exec(
		`UPDATE programs SET parent_id = NULL WHERE name = 'Child'`,
	); err == nil {
		t.Fatal("setting a second row's parent_id to NULL succeeded; the BEFORE UPDATE single-root guard is missing")
	}
}

// TestProgramsVersionsTableExists proves the versions twin exists and is
// queryable (AGENTS rule 5: every versioned table has a *_versions twin).
func TestProgramsVersionsTableExists(t *testing.T) {
	sqldb := testutil.NewDB(t)

	var n int
	if err := sqldb.QueryRow(`SELECT count(*) FROM programs_versions`).Scan(&n); err != nil {
		t.Fatalf("query programs_versions: %v", err)
	}
}

// TestSeedRootProgramVersionConsistency is the seed-version guard, mirroring the
// subsidiary-seed guard (TestSeedRootVersionConsistency). It asserts the seeded
// root program (id 1) has EXACTLY ONE programs_versions row, op='create', every
// snapshot column equal to the live programs row, and a change_id referencing a
// real changes row whose `at` equals the version's valid_from (rule 5). This is
// the audit-consistency contract so Z3/Z5 hold with no seed special-casing.
func TestSeedRootProgramVersionConsistency(t *testing.T) {
	sqldb := testutil.NewDB(t)

	var count int
	if err := sqldb.QueryRow(
		`SELECT count(*) FROM programs_versions WHERE entity_id = 1`,
	).Scan(&count); err != nil {
		t.Fatalf("count root program version rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("root program (id 1) has %d programs_versions rows, want exactly 1", count)
	}

	var (
		op         string
		changeID   int64
		validFrom  string
		vParentID  *int64
		vName      string
		vActive    int
		vSortOrder int
	)
	if err := sqldb.QueryRow(
		`SELECT op, change_id, valid_from, parent_id, name, active, sort_order
		   FROM programs_versions WHERE entity_id = 1`,
	).Scan(&op, &changeID, &validFrom, &vParentID, &vName, &vActive, &vSortOrder); err != nil {
		t.Fatalf("read root program version row: %v", err)
	}
	if op != "create" {
		t.Errorf("root program version op = %q, want create", op)
	}

	var (
		lParentID  *int64
		lName      string
		lActive    int
		lSortOrder int
	)
	if err := sqldb.QueryRow(
		`SELECT parent_id, name, active, sort_order FROM programs WHERE id = 1`,
	).Scan(&lParentID, &lName, &lActive, &lSortOrder); err != nil {
		t.Fatalf("read live root program row: %v", err)
	}

	if (vParentID == nil) != (lParentID == nil) || (vParentID != nil && *vParentID != *lParentID) {
		t.Errorf("version parent_id = %v, live parent_id = %v", vParentID, lParentID)
	}
	if vName != lName {
		t.Errorf("version name = %q, live name = %q", vName, lName)
	}
	if vName != "General" {
		t.Errorf("root program name = %q, want General (D24 unallocated default)", vName)
	}
	if vActive != lActive {
		t.Errorf("version active = %d, live active = %d", vActive, lActive)
	}
	if vSortOrder != lSortOrder {
		t.Errorf("version sort_order = %d, live sort_order = %d", vSortOrder, lSortOrder)
	}

	var changeAt string
	if err := sqldb.QueryRow(
		`SELECT at FROM changes WHERE id = ?`, changeID,
	).Scan(&changeAt); err != nil {
		t.Fatalf("root program version change_id=%d references no changes row: %v", changeID, err)
	}
	if changeAt != validFrom {
		t.Errorf("changes.at = %q but version valid_from = %q (rule 5: they must be equal)", changeAt, validFrom)
	}
}

// TestAccountsDefaultProgramColumnAndFK proves the p07.1 ALTER added
// accounts.default_program_id and that it references programs(id): a nonexistent
// program id is rejected by the FK. parent NULL + revenue type keep the row-local
// triggers satisfied so the ONLY possible abort is the program FK.
func TestAccountsDefaultProgramColumnAndFK(t *testing.T) {
	sqldb := testutil.NewDB(t)

	_, err := sqldb.Exec(
		`INSERT INTO accounts (parent_id, type, default_currency, default_program_id, created_at)
		 VALUES (NULL, 'revenue', 'USD', 9999, '2025-01-01T00:00:00Z')`,
	)
	if err == nil {
		t.Fatal("account with nonexistent default_program_id inserted; the programs FK is not enforced")
	}
	if strings.Contains(err.Error(), "no such column") {
		t.Fatalf("insert failed because default_program_id is missing, not the FK: %v", err)
	}
	if strings.Contains(err.Error(), "no such table") {
		t.Fatalf("insert failed because a table is missing, not the FK: %v", err)
	}

	// Positive: the seeded root program (id 1) is a valid target.
	if _, err := sqldb.Exec(
		`INSERT INTO accounts (parent_id, type, default_currency, default_program_id, created_at)
		 VALUES (NULL, 'revenue', 'USD', 1, '2025-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("account with valid default_program_id=1 rejected: %v", err)
	}
}
