package db_test

import (
	"strings"
	"testing"

	"cuento/internal/testutil"
)

// p04.1 introduces the FIRST versioned business table. These tests exercise the
// migration's schema, triggers, and audit-consistent seed via direct SQL on a
// migrated harness db (AGENTS testing conventions permit direct SQL for schema
// checks; the store path is p04.2). Written before 00004 exists — each fails
// with "no such table: subsidiaries" until the migration lands.

// TestSubsidiariesSingleRootEnforced proves trg_subsidiaries_single_root rejects
// a SECOND parent_id-IS-NULL insert. The second root uses a distinct name and a
// valid base_currency so the abort can only come from the single-root trigger —
// not from UNIQUE(name) or the currency FK.
func TestSubsidiariesSingleRootEnforced(t *testing.T) {
	sqldb := testutil.NewDB(t)

	// The seed already provides one root ('Organization'). A second root must be
	// rejected by the BEFORE INSERT trigger.
	_, err := sqldb.Exec(
		`INSERT INTO subsidiaries (parent_id, name, base_currency) VALUES (NULL, 'Second Root', 'USD')`,
	)
	if err == nil {
		t.Fatal("second NULL-parent insert succeeded; trg_subsidiaries_single_root does not enforce a single root")
	}

	// And BEFORE UPDATE: create a valid child under the root, then try to orphan
	// it into a second root. That, too, must be rejected.
	if _, err := sqldb.Exec(
		`INSERT INTO subsidiaries (parent_id, name, base_currency) VALUES (1, 'Child', 'USD')`,
	); err != nil {
		t.Fatalf("insert valid child under root: %v", err)
	}
	if _, err := sqldb.Exec(
		`UPDATE subsidiaries SET parent_id = NULL WHERE name = 'Child'`,
	); err == nil {
		t.Fatal("setting a second row's parent_id to NULL succeeded; the BEFORE UPDATE single-root guard is missing")
	}
}

// TestSubsidiariesCurrencyFK proves the base_currency FK to currencies is
// enforced. parent_id=1 (the seeded root) is valid and non-NULL, so the
// single-root trigger does not fire first — the abort must come from the FK.
func TestSubsidiariesCurrencyFK(t *testing.T) {
	sqldb := testutil.NewDB(t)

	_, err := sqldb.Exec(
		`INSERT INTO subsidiaries (parent_id, name, base_currency) VALUES (1, 'Bad Currency Sub', 'ZZZ')`,
	)
	if err == nil {
		t.Fatal("subsidiary with nonexistent base_currency inserted; the currencies FK is not enforced")
	}
	// Guard against a false pass: before the migration lands the abort is
	// "no such table", not a FK rejection — that is not the reason under test.
	if strings.Contains(err.Error(), "no such table") {
		t.Fatalf("insert failed because the table is missing, not the currency FK: %v", err)
	}
}

// TestSubsidiariesVersionsTableExists proves the versions twin exists and is
// queryable (AGENTS rule 5: every versioned table has a *_versions twin).
func TestSubsidiariesVersionsTableExists(t *testing.T) {
	sqldb := testutil.NewDB(t)

	var n int
	if err := sqldb.QueryRow(`SELECT count(*) FROM subsidiaries_versions`).Scan(&n); err != nil {
		t.Fatalf("query subsidiaries_versions: %v", err)
	}
}

// TestSeedRootVersionConsistency is the seed-version guard: it closes the gap
// that Z3/Z5 do not exist until p08.3. It asserts the seeded root (id 1) has
// EXACTLY ONE subsidiaries_versions row, op='create', every snapshot column
// equal to the live subsidiaries row, and a change_id that references a real
// changes row whose `at` equals the version's valid_from (rule 5:
// valid_from == changes.at). This is the audit-consistency contract every
// versioned-table seed must satisfy so Z3/Z5 hold with no seed special-casing.
func TestSeedRootVersionConsistency(t *testing.T) {
	sqldb := testutil.NewDB(t)

	// Exactly one version row for the root.
	var count int
	if err := sqldb.QueryRow(
		`SELECT count(*) FROM subsidiaries_versions WHERE entity_id = 1`,
	).Scan(&count); err != nil {
		t.Fatalf("count root version rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("root (id 1) has %d subsidiaries_versions rows, want exactly 1", count)
	}

	// The version row's op, its change_id, and its snapshot columns.
	var (
		op         string
		changeID   int64
		validFrom  string
		vParentID  *int64
		vName      string
		vBaseCurr  string
		vActive    int
		vSortOrder int
	)
	if err := sqldb.QueryRow(
		`SELECT op, change_id, valid_from, parent_id, name, base_currency, active, sort_order
		   FROM subsidiaries_versions WHERE entity_id = 1`,
	).Scan(&op, &changeID, &validFrom, &vParentID, &vName, &vBaseCurr, &vActive, &vSortOrder); err != nil {
		t.Fatalf("read root version row: %v", err)
	}
	if op != "create" {
		t.Errorf("root version op = %q, want create", op)
	}

	// The live subsidiaries row.
	var (
		lParentID  *int64
		lName      string
		lBaseCurr  string
		lActive    int
		lSortOrder int
	)
	if err := sqldb.QueryRow(
		`SELECT parent_id, name, base_currency, active, sort_order FROM subsidiaries WHERE id = 1`,
	).Scan(&lParentID, &lName, &lBaseCurr, &lActive, &lSortOrder); err != nil {
		t.Fatalf("read live root row: %v", err)
	}

	// Snapshot must match the live row column-for-column.
	if (vParentID == nil) != (lParentID == nil) || (vParentID != nil && *vParentID != *lParentID) {
		t.Errorf("version parent_id = %v, live parent_id = %v", vParentID, lParentID)
	}
	if vName != lName {
		t.Errorf("version name = %q, live name = %q", vName, lName)
	}
	if vBaseCurr != lBaseCurr {
		t.Errorf("version base_currency = %q, live base_currency = %q", vBaseCurr, lBaseCurr)
	}
	if vActive != lActive {
		t.Errorf("version active = %d, live active = %d", vActive, lActive)
	}
	if vSortOrder != lSortOrder {
		t.Errorf("version sort_order = %d, live sort_order = %d", vSortOrder, lSortOrder)
	}

	// change_id references a real changes row whose `at` equals valid_from
	// (rule 5: valid_from == changes.at). The `at` column is the changes
	// timestamp — not "valid_from".
	var changeAt string
	if err := sqldb.QueryRow(
		`SELECT at FROM changes WHERE id = ?`, changeID,
	).Scan(&changeAt); err != nil {
		t.Fatalf("root version change_id=%d references no changes row: %v", changeID, err)
	}
	if changeAt != validFrom {
		t.Errorf("changes.at = %q but version valid_from = %q (rule 5: they must be equal)", changeAt, validFrom)
	}
}
