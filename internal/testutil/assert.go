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

	var exists int
	err = db.QueryRow(`SELECT 1 FROM changes WHERE id = ?`, changeID.Int64).Scan(&exists)
	if err == sql.ErrNoRows {
		t.Fatalf("AssertVersioned: %s_versions.change_id=%d references no changes row", table, changeID.Int64)
	}
	if err != nil {
		t.Fatalf("AssertVersioned: verify change_id: %v", err)
	}
}
