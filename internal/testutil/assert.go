package testutil

import (
	"database/sql"
	"fmt"
	"testing"
)

// AssertVersioned asserts the versioning contract (AGENTS rule 5, D4) for one
// entity: after a store mutation, the entity's *_versions table holds a latest
// snapshot row whose op matches wantOp and whose change_id references a real
// changes row.
//
// It is TEST tooling and is therefore exempt from AGENTS rule 6 (SQL via sqlc):
// it parameterizes the table name (`<table>_versions`) so a single helper serves
// every versioned table, which sqlc cannot express. It is never on the app's
// query path.
//
// Contract, for entity entityID in <table>_versions:
//   - at least one version row exists (else the mutation was not versioned);
//   - the row with the greatest valid_from (ties broken by greatest id — the
//     append order) has op == wantOp;
//   - that row's change_id is non-NULL and names an existing changes row.
//
// No versioned business table exists until p04.1 (subsidiaries), which is this
// helper's first real exercise. table is the base name, e.g. "subsidiaries"
// (the helper appends "_versions"); wantOp is one of create/update/delete.
func AssertVersioned(t *testing.T, db *sql.DB, table string, entityID int64, wantOp string) {
	t.Helper()

	// Latest snapshot for the entity: max(valid_from), then max(id) as the
	// append-order tiebreaker for same-instant versions.
	q := fmt.Sprintf(
		`SELECT op, change_id
		   FROM %s_versions
		  WHERE entity_id = ?
		  ORDER BY valid_from DESC, id DESC
		  LIMIT 1`, table,
	)

	var (
		gotOp    string
		changeID sql.NullInt64
	)
	err := db.QueryRow(q, entityID).Scan(&gotOp, &changeID)
	if err == sql.ErrNoRows {
		t.Fatalf("AssertVersioned: no %s_versions row for entity_id=%d (mutation was not versioned)", table, entityID)
	}
	if err != nil {
		t.Fatalf("AssertVersioned: query %s_versions: %v", table, err)
	}

	if gotOp != wantOp {
		t.Errorf("AssertVersioned: latest %s_versions op for entity_id=%d = %q, want %q", table, entityID, gotOp, wantOp)
	}
	if !changeID.Valid {
		t.Fatalf("AssertVersioned: latest %s_versions row for entity_id=%d has NULL change_id", table, entityID)
	}

	assertChangeExists(t, db, "AssertVersioned", changeID)
}

// AssertVersionedName asserts the versioning contract for one composite-key
// account name (account_id, lang). account_names_versions keys entity_id on
// account_id and carries `lang` as both a snapshot column and part of the
// entity identity (p05.1), so the as-of/latest lookup must filter on the pair.
// The latest snapshot for (accountID, lang) must have op == wantOp and a
// change_id naming an existing changes row.
func AssertVersionedName(t *testing.T, db *sql.DB, accountID int64, lang, wantOp string) {
	t.Helper()

	var (
		gotOp    string
		changeID sql.NullInt64
	)
	err := db.QueryRow(
		`SELECT op, change_id
		   FROM account_names_versions
		  WHERE entity_id = ? AND lang = ?
		  ORDER BY valid_from DESC, id DESC
		  LIMIT 1`, accountID, lang,
	).Scan(&gotOp, &changeID)
	if err == sql.ErrNoRows {
		t.Fatalf("AssertVersionedName: no account_names_versions row for (account_id=%d, lang=%q) (name mutation was not versioned)", accountID, lang)
	}
	if err != nil {
		t.Fatalf("AssertVersionedName: query account_names_versions: %v", err)
	}
	if gotOp != wantOp {
		t.Errorf("AssertVersionedName: latest op for (account_id=%d, lang=%q) = %q, want %q", accountID, lang, gotOp, wantOp)
	}
	if !changeID.Valid {
		t.Fatalf("AssertVersionedName: latest row for (account_id=%d, lang=%q) has NULL change_id", accountID, lang)
	}
	assertChangeExists(t, db, "AssertVersionedName", changeID)
}

// AssertVersionedSub asserts the versioning contract for one composite-key
// account-subsidiary membership (account_id, subsidiary_id).
// account_subsidiaries_versions keys entity_id on account_id and carries
// subsidiary_id as both a snapshot column and part of the entity identity
// (p05.1). Membership is a set: an add is op='create', a removal op='delete';
// the latest snapshot for (accountID, subID) must have op == wantOp and a
// change_id naming an existing changes row.
func AssertVersionedSub(t *testing.T, db *sql.DB, accountID, subID int64, wantOp string) {
	t.Helper()

	var (
		gotOp    string
		changeID sql.NullInt64
	)
	err := db.QueryRow(
		`SELECT op, change_id
		   FROM account_subsidiaries_versions
		  WHERE entity_id = ? AND subsidiary_id = ?
		  ORDER BY valid_from DESC, id DESC
		  LIMIT 1`, accountID, subID,
	).Scan(&gotOp, &changeID)
	if err == sql.ErrNoRows {
		t.Fatalf("AssertVersionedSub: no account_subsidiaries_versions row for (account_id=%d, subsidiary_id=%d) (membership mutation was not versioned)", accountID, subID)
	}
	if err != nil {
		t.Fatalf("AssertVersionedSub: query account_subsidiaries_versions: %v", err)
	}
	if gotOp != wantOp {
		t.Errorf("AssertVersionedSub: latest op for (account_id=%d, subsidiary_id=%d) = %q, want %q", accountID, subID, gotOp, wantOp)
	}
	if !changeID.Valid {
		t.Fatalf("AssertVersionedSub: latest row for (account_id=%d, subsidiary_id=%d) has NULL change_id", accountID, subID)
	}
	assertChangeExists(t, db, "AssertVersionedSub", changeID)
}

// assertChangeExists verifies a version row's change_id references a real
// changes row — the shared tail of every AssertVersioned* helper.
func assertChangeExists(t *testing.T, db *sql.DB, who string, changeID sql.NullInt64) {
	t.Helper()
	var exists int
	err := db.QueryRow(`SELECT 1 FROM changes WHERE id = ?`, changeID.Int64).Scan(&exists)
	if err == sql.ErrNoRows {
		t.Fatalf("%s: change_id=%d references no changes row", who, changeID.Int64)
	}
	if err != nil {
		t.Fatalf("%s: verify change_id: %v", who, err)
	}
}
