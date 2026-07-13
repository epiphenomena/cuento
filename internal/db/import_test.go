package db_test

import (
	"database/sql"
	"strings"
	"testing"

	"cuento/internal/testutil"
)

// p17.1 adds the BANK-CSV-IMPORT staging schema (migration 00015): three
// OPERATIONAL tables and no *_versions twins.
//
//	mapping_profiles  saved column-mapping config (JSON) for a CSV layout
//	import_batches    one upload, bound to ONE account AND ONE subsidiary
//	import_rows       staged rows: raw JSON + parsed date/amount/payee/memo,
//	                  status pending|posted|discarded, dedupe_hash, and
//	                  posted_transaction_id (set when a row is posted)
//
// These are STAGING/reference tables like currencies/report_groups -- NOT
// bitemporal ledger tables. They deliberately have NO version twin and are NOT
// in the Z3/Z5 versioned-table union: the audit lives elsewhere (a posted row
// links to its real versioned transaction via posted_transaction_id; p17.3
// writes a `changes` row on discard). So there is no version wiring here.
//
// Dedupe-scoping decision (DECISIONS p17.1): dedupe_hash =
// sha256(account|date|amount|normalized payee+memo) is COMPUTED in p17.2; here
// it is just a column. Scoping is PER ACCOUNT, achieved by DENORMALIZING
// account_id onto import_rows plus a NON-UNIQUE index on
// (account_id, dedupe_hash). Non-unique on purpose: a legitimate idempotent
// re-upload (p17.3 TestReimportFlagsDuplicates) must be FLAGGED by a lookup, not
// crash at INSERT. The composite (account_id, dedupe_hash) -- not a bare
// dedupe_hash -- is what makes "same hash under a different account is allowed"
// hold.
//
// Direct-SQL schema tests (AGENTS testing conventions permit direct SQL for
// schema checks; the store/parser is p17.2/p17.3). They reuse mkAccount from
// transactions_splits_test.go (same db_test package). Seeded rows exist for
// user id 1 (system) and subsidiary id 1 (Organization).

// mkProfile inserts a mapping profile and returns its id.
func mkProfile(t *testing.T, sqldb *sql.DB, name string) int64 {
	t.Helper()
	res, err := sqldb.Exec(
		`INSERT INTO mapping_profiles (name, config) VALUES (?, '{}')`, name,
	)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			t.Fatalf("cannot seed mapping profile because mapping_profiles is missing: %v", err)
		}
		t.Fatalf("seed mapping profile: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("mapping profile last insert id: %v", err)
	}
	return id
}

// mkBatch inserts an import batch bound to acct (subsidiary 1, profile prof,
// uploaded by user 1) and returns its id.
func mkBatch(t *testing.T, sqldb *sql.DB, acct, prof int64) int64 {
	t.Helper()
	res, err := sqldb.Exec(
		`INSERT INTO import_batches (filename, account_id, subsidiary_id, profile_id, uploaded_by, uploaded_at)
		 VALUES ('statement.csv', ?, 1, ?, 1, '2025-01-01T00:00:00Z')`, acct, prof,
	)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			t.Fatalf("cannot seed import batch because import_batches is missing: %v", err)
		}
		t.Fatalf("seed import batch: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("import batch last insert id: %v", err)
	}
	return id
}

// TestImportTablesExist proves the three import tables exist, are queryable, and
// have NO version twins (they are non-versioned operational tables, p17.1).
func TestImportTablesExist(t *testing.T) {
	sqldb := testutil.NewDB(t)

	for _, tbl := range []string{"mapping_profiles", "import_batches", "import_rows"} {
		var n int
		if err := sqldb.QueryRow(`SELECT count(*) FROM ` + tbl).Scan(&n); err != nil {
			t.Errorf("query %s: %v", tbl, err)
		}
	}

	// No version twins: these are staging tables, not bitemporal ledger tables.
	for _, twin := range []string{"mapping_profiles_versions", "import_batches_versions", "import_rows_versions"} {
		if _, err := sqldb.Exec(`SELECT 1 FROM ` + twin + ` LIMIT 0`); err == nil {
			t.Errorf("%s exists; import staging tables must NOT be versioned", twin)
		}
	}
}

// TestImportRowValidInsert proves a fully specified valid row inserts: it binds
// a batch, carries a pending status, parsed fields, a dedupe_hash, and a NULL
// posted_transaction_id.
func TestImportRowValidInsert(t *testing.T) {
	sqldb := testutil.NewDB(t)
	acct := mkAccount(t, sqldb, "asset")
	prof := mkProfile(t, sqldb, "bank")
	batch := mkBatch(t, sqldb, acct, prof)

	if _, err := sqldb.Exec(
		`INSERT INTO import_rows
		   (batch_id, account_id, raw_json, parsed_date, parsed_amount, parsed_payee, parsed_memo, status, dedupe_hash)
		 VALUES (?, ?, '{"c0":"x"}', '2025-01-15', 100, 'Payee', 'Memo', 'pending', 'hashaaa')`,
		batch, acct,
	); err != nil {
		t.Fatalf("valid import row insert rejected: %v", err)
	}
}

// TestImportRowBadStatusRejected proves the status CHECK admits exactly
// pending/posted/discarded and rejects anything else.
func TestImportRowBadStatusRejected(t *testing.T) {
	sqldb := testutil.NewDB(t)
	acct := mkAccount(t, sqldb, "asset")
	prof := mkProfile(t, sqldb, "bank")
	batch := mkBatch(t, sqldb, acct, prof)

	for _, ok := range []string{"pending", "posted", "discarded"} {
		if _, err := sqldb.Exec(
			`INSERT INTO import_rows (batch_id, account_id, raw_json, status, dedupe_hash)
			 VALUES (?, ?, '{}', ?, ?)`, batch, acct, ok, "h-"+ok,
		); err != nil {
			t.Fatalf("status %q should be accepted by the CHECK: %v", ok, err)
		}
	}

	_, err := sqldb.Exec(
		`INSERT INTO import_rows (batch_id, account_id, raw_json, status, dedupe_hash)
		 VALUES (?, ?, '{}', 'bogus', 'h-bogus')`, batch, acct,
	)
	if err == nil {
		t.Fatal("status 'bogus' accepted; the status CHECK(pending/posted/discarded) is missing or wrong")
	}
	if strings.Contains(err.Error(), "no such table") {
		t.Fatalf("insert failed because a table is missing, not the status CHECK: %v", err)
	}
}

// TestImportRowFKsEnforced proves the batch_id and account_id FKs on import_rows,
// and the account_id/subsidiary_id/profile_id/uploaded_by FKs on import_batches,
// are enforced (FKs are ON in the harness db; Z4 relies on this).
func TestImportRowFKsEnforced(t *testing.T) {
	sqldb := testutil.NewDB(t)
	acct := mkAccount(t, sqldb, "asset")
	prof := mkProfile(t, sqldb, "bank")
	batch := mkBatch(t, sqldb, acct, prof)

	// import_rows.batch_id -> import_batches
	if _, err := sqldb.Exec(
		`INSERT INTO import_rows (batch_id, account_id, raw_json, status, dedupe_hash)
		 VALUES (999999, ?, '{}', 'pending', 'h1')`, acct,
	); err == nil {
		t.Fatal("import_rows referencing a non-existent batch succeeded; batch_id FK not enforced")
	}
	// import_rows.account_id -> accounts
	if _, err := sqldb.Exec(
		`INSERT INTO import_rows (batch_id, account_id, raw_json, status, dedupe_hash)
		 VALUES (?, 999999, '{}', 'pending', 'h2')`, batch,
	); err == nil {
		t.Fatal("import_rows referencing a non-existent account succeeded; account_id FK not enforced")
	}
	// import_rows.posted_transaction_id -> transactions (when set)
	if _, err := sqldb.Exec(
		`INSERT INTO import_rows (batch_id, account_id, raw_json, status, dedupe_hash, posted_transaction_id)
		 VALUES (?, ?, '{}', 'posted', 'h3', 999999)`, batch, acct,
	); err == nil {
		t.Fatal("import_rows referencing a non-existent transaction succeeded; posted_transaction_id FK not enforced")
	}

	// import_batches.account_id -> accounts
	if _, err := sqldb.Exec(
		`INSERT INTO import_batches (filename, account_id, subsidiary_id, profile_id, uploaded_by, uploaded_at)
		 VALUES ('x.csv', 999999, 1, ?, 1, '2025-01-01T00:00:00Z')`, prof,
	); err == nil {
		t.Fatal("import_batches referencing a non-existent account succeeded; account_id FK not enforced")
	}
	// import_batches.subsidiary_id -> subsidiaries
	if _, err := sqldb.Exec(
		`INSERT INTO import_batches (filename, account_id, subsidiary_id, profile_id, uploaded_by, uploaded_at)
		 VALUES ('x.csv', ?, 999999, ?, 1, '2025-01-01T00:00:00Z')`, acct, prof,
	); err == nil {
		t.Fatal("import_batches referencing a non-existent subsidiary succeeded; subsidiary_id FK not enforced")
	}
	// import_batches.profile_id -> mapping_profiles
	if _, err := sqldb.Exec(
		`INSERT INTO import_batches (filename, account_id, subsidiary_id, profile_id, uploaded_by, uploaded_at)
		 VALUES ('x.csv', ?, 1, 999999, 1, '2025-01-01T00:00:00Z')`, acct,
	); err == nil {
		t.Fatal("import_batches referencing a non-existent profile succeeded; profile_id FK not enforced")
	}
	// import_batches.uploaded_by -> users
	if _, err := sqldb.Exec(
		`INSERT INTO import_batches (filename, account_id, subsidiary_id, profile_id, uploaded_by, uploaded_at)
		 VALUES ('x.csv', ?, 1, ?, 999999, '2025-01-01T00:00:00Z')`, acct, prof,
	); err == nil {
		t.Fatal("import_batches referencing a non-existent user succeeded; uploaded_by FK not enforced")
	}
}

// TestDedupeHashScopedPerAccount is the KEY test. dedupe_hash uniqueness is
// scoped PER ACCOUNT via the denormalized account_id column:
//
//   - the SAME dedupe_hash under the SAME account is DETECTED as a duplicate by
//     the (account_id, dedupe_hash) lookup -- ACROSS batches (a re-upload is a
//     new batch), which is what makes p17.3's idempotent-reimport flagging work;
//   - the SAME dedupe_hash under a DIFFERENT account is ALLOWED and is NOT seen
//     as a duplicate by the first account's lookup (two banks can legitimately
//     produce identical-looking rows).
//
// The index is deliberately NON-UNIQUE: detection is a LOOKUP, so a legitimate
// re-upload is FLAGGED rather than crashing at INSERT (p17.2/p17.3 flag, never
// error). This test asserts exactly that scoping.
func TestDedupeHashScopedPerAccount(t *testing.T) {
	sqldb := testutil.NewDB(t)
	prof := mkProfile(t, sqldb, "bank")

	acctA := mkAccount(t, sqldb, "asset")
	acctB := mkAccount(t, sqldb, "asset")

	const hash = "sha256-identical-looking-row"

	// First upload of account A: stage a row with the hash.
	batchA1 := mkBatch(t, sqldb, acctA, prof)
	if _, err := sqldb.Exec(
		`INSERT INTO import_rows (batch_id, account_id, raw_json, status, dedupe_hash)
		 VALUES (?, ?, '{}', 'pending', ?)`, batchA1, acctA, hash,
	); err != nil {
		t.Fatalf("first row for account A rejected: %v", err)
	}

	// The dedupe lookup a staging flow runs before inserting a candidate row for
	// account A: it MUST find the prior row (duplicate detected), even though the
	// re-upload is a DIFFERENT batch.
	var dupA int
	if err := sqldb.QueryRow(
		`SELECT count(*) FROM import_rows WHERE account_id = ? AND dedupe_hash = ?`, acctA, hash,
	).Scan(&dupA); err != nil {
		t.Fatalf("dedupe lookup (account A): %v", err)
	}
	if dupA != 1 {
		t.Fatalf("dedupe lookup for account A found %d prior rows; want 1 (duplicate must be detectable across batches)", dupA)
	}

	// The index is NON-UNIQUE by decision: a re-upload of the SAME row on the SAME
	// account (a NEW batch) must be able to INSERT without crashing -- p17.2/p17.3
	// FLAG the duplicate via the lookup above, they do NOT rely on a UNIQUE
	// constraint erroring at INSERT. (A UNIQUE(account_id, dedupe_hash) would make
	// this INSERT fail -- this assertion is what locks the non-unique decision in.)
	batchA2 := mkBatch(t, sqldb, acctA, prof)
	if _, err := sqldb.Exec(
		`INSERT INTO import_rows (batch_id, account_id, raw_json, status, dedupe_hash)
		 VALUES (?, ?, '{}', 'pending', ?)`, batchA2, acctA, hash,
	); err != nil {
		t.Fatalf("re-uploading the SAME hash on the SAME account (new batch) failed at INSERT; the dedupe index must be NON-UNIQUE so duplicates are flagged, not crashed: %v", err)
	}
	// The lookup now sees BOTH rows -- the duplicate is detected, across batches.
	if err := sqldb.QueryRow(
		`SELECT count(*) FROM import_rows WHERE account_id = ? AND dedupe_hash = ?`, acctA, hash,
	).Scan(&dupA); err != nil {
		t.Fatalf("dedupe lookup (account A, after re-upload): %v", err)
	}
	if dupA != 2 {
		t.Fatalf("dedupe lookup for account A found %d rows after a same-account re-upload; want 2 (both staged, duplicate flagged by lookup)", dupA)
	}

	// The SAME hash under a DIFFERENT account (B) must be allowed to INSERT: the
	// index is (account_id, dedupe_hash), not a bare dedupe_hash, so it does not
	// collide across accounts.
	batchB := mkBatch(t, sqldb, acctB, prof)
	if _, err := sqldb.Exec(
		`INSERT INTO import_rows (batch_id, account_id, raw_json, status, dedupe_hash)
		 VALUES (?, ?, '{}', 'pending', ?)`, batchB, acctB, hash,
	); err != nil {
		t.Fatalf("same hash under a DIFFERENT account rejected; dedupe is not scoped per account: %v", err)
	}

	// And account A's lookup still sees exactly its OWN two rows for that hash --
	// account B's identical-looking row does NOT leak into account A's dedupe
	// scope (the scope is (account_id, dedupe_hash), not a bare hash).
	if err := sqldb.QueryRow(
		`SELECT count(*) FROM import_rows WHERE account_id = ? AND dedupe_hash = ?`, acctA, hash,
	).Scan(&dupA); err != nil {
		t.Fatalf("dedupe lookup (account A, after B insert): %v", err)
	}
	if dupA != 2 {
		t.Fatalf("account A dedupe scope saw %d rows after account B inserted an identical hash; want 2 (scoping leaked across accounts)", dupA)
	}
	// ...and account B's scope sees exactly its own one row.
	var dupB int
	if err := sqldb.QueryRow(
		`SELECT count(*) FROM import_rows WHERE account_id = ? AND dedupe_hash = ?`, acctB, hash,
	).Scan(&dupB); err != nil {
		t.Fatalf("dedupe lookup (account B): %v", err)
	}
	if dupB != 1 {
		t.Fatalf("account B dedupe scope saw %d rows; want 1 (its own row only)", dupB)
	}
}
