package sqlc_test

import (
	"strings"
	"testing"

	"cuento/internal/testutil"
)

// Schema-level tests for p06.1's migration (00006). They deliberately use RAW
// SQL through the migrated *sql.DB rather than the store: they assert the
// SCHEMA (FKs, CHECKs, the backfilled system-user version row) exists and
// behaves, independent of any store surface. Written first, they fail with a
// genuine "no such table/column" before the migration exists.

// TestGrantFKUserRejected proves user_report_grants.user_id enforces its FK to
// users(id): a grant referencing a non-existent user is rejected (foreign_keys
// is ON via db.Open). Seeds a valid report_group first so ONLY the user FK is
// in question.
func TestGrantFKUserRejected(t *testing.T) {
	d := testutil.NewDB(t)

	if _, err := d.Exec(`INSERT INTO report_groups (name) VALUES ('financials')`); err != nil {
		t.Fatalf("seed report_group: %v", err)
	}

	_, err := d.Exec(
		`INSERT INTO user_report_grants (user_id, group_name) VALUES (?, 'financials')`,
		9999, // no such user
	)
	if err == nil {
		t.Fatal("grant with dangling user_id succeeded, want FK violation")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Fatalf("error = %v, want a FOREIGN KEY constraint failure", err)
	}
}

// TestGrantFKGroupRejected proves user_report_grants.group_name enforces its FK
// to report_groups(name): a grant referencing a non-existent group is rejected.
func TestGrantFKGroupRejected(t *testing.T) {
	d := testutil.NewDB(t)

	// user 1 (system) exists from the 00002 seed; the group does not.
	_, err := d.Exec(
		`INSERT INTO user_report_grants (user_id, group_name) VALUES (1, 'nonexistent-group')`,
	)
	if err == nil {
		t.Fatal("grant with dangling group_name succeeded, want FK violation")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Fatalf("error = %v, want a FOREIGN KEY constraint failure", err)
	}
}

// TestGrantValidInserts proves a grant with BOTH a real user and a real report
// group inserts cleanly (the happy path the FK tests bound).
func TestGrantValidInserts(t *testing.T) {
	d := testutil.NewDB(t)

	if _, err := d.Exec(`INSERT INTO report_groups (name) VALUES ('financials')`); err != nil {
		t.Fatalf("seed report_group: %v", err)
	}
	if _, err := d.Exec(
		`INSERT INTO user_report_grants (user_id, group_name) VALUES (1, 'financials')`,
	); err != nil {
		t.Fatalf("valid grant insert: %v", err)
	}

	var n int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM user_report_grants WHERE user_id = 1 AND group_name = 'financials'`,
	).Scan(&n); err != nil {
		t.Fatalf("count grants: %v", err)
	}
	if n != 1 {
		t.Errorf("grant count = %d, want 1", n)
	}
}

// TestTxnPermCheckRejectsOutOfEnum proves the users.txn_perm CHECK rejects a
// value outside {none,read,write}.
func TestTxnPermCheckRejectsOutOfEnum(t *testing.T) {
	d := testutil.NewDB(t)

	_, err := d.Exec(
		`INSERT INTO users (username, display_name, created_at, txn_perm)
		 VALUES ('bad', 'Bad', '2026-07-11T00:00:00Z', 'superuser')`,
	)
	if err == nil {
		t.Fatal("insert with txn_perm='superuser' succeeded, want CHECK violation")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "check") {
		t.Fatalf("error = %v, want a CHECK constraint failure", err)
	}
}

// TestSystemUserBackfilledVersion proves the migration backfilled a
// users_versions op='create' row for the system user (id 1) — the p04.1-deferred
// instance, so future Z3 (current == latest version) holds. It also proves the
// snapshot carries the non-secret business columns and, crucially, that
// users_versions has NO password_hash column at all (rule 5).
func TestSystemUserBackfilledVersion(t *testing.T) {
	d := testutil.NewDB(t)

	var (
		op        string
		username  string
		validFrom string
		changeID  int64
	)
	err := d.QueryRow(
		`SELECT op, username, valid_from, change_id
		   FROM users_versions
		  WHERE entity_id = 1
		  ORDER BY valid_from DESC, id DESC
		  LIMIT 1`,
	).Scan(&op, &username, &validFrom, &changeID)
	if err != nil {
		t.Fatalf("read system user version row: %v", err)
	}
	if op != "create" {
		t.Errorf("system user version op = %q, want %q", op, "create")
	}
	if username != "system" {
		t.Errorf("system user version username = %q, want %q", username, "system")
	}
	if validFrom != "1970-01-01T00:00:00Z" {
		t.Errorf("system user version valid_from = %q, want the epoch seed", validFrom)
	}

	// valid_from must equal the backfill change's `at` (rule 5 / D4).
	var changeAt string
	if err := d.QueryRow(`SELECT at FROM changes WHERE id = ?`, changeID).Scan(&changeAt); err != nil {
		t.Fatalf("read backfill change: %v", err)
	}
	if changeAt != validFrom {
		t.Errorf("valid_from %q != changes.at %q", validFrom, changeAt)
	}

	// users_versions must NOT have a password_hash column (rule 5): the audit
	// trail can never carry the secret. PRAGMA table_info enumerates the columns.
	rows, err := d.Query(`PRAGMA table_info(users_versions)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info(users_versions): %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notnull   int
			dfltValue any
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		if name == "password_hash" {
			t.Fatal("users_versions has a password_hash column; rule 5 forbids the hash in the audit trail")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table_info: %v", err)
	}
}
