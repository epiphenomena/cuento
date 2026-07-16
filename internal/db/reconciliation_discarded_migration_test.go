package db

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

func execT(t *testing.T, d *sql.DB, q string) {
	t.Helper()
	if _, err := d.Exec(q); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// TestMigrate23PreservesReconciliationsOnRebuild applies migrations 1..22 to a fresh db, seeds a
// reconciliation with a CLEARED split (the FK case the 00023 rebuild must
// preserve), then applies ONLY 00023 to exercise the INSERT...SELECT copy path
// with real rows. Asserts id preservation, the cleared-split FK survives, FK
// enforcement is re-enabled, foreign_key_check is empty, and 'discarded' is now
// accepted by the widened CHECK.
func TestMigrate23PreservesReconciliationsOnRebuild(t *testing.T) {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pop.db")
	sqldb, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer sqldb.Close()

	prov, err := goose.NewProvider(goose.DialectSQLite3, sqldb, sub)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prov.UpTo(context.Background(), 22); err != nil {
		t.Fatalf("up to 22: %v", err)
	}

	// USD (00003) + the root subsidiary id 1 (00004) are pre-seeded by the migrations.
	execT(t, sqldb, `INSERT INTO accounts (id,sort_order,type,active,default_currency,reconcilable,created_at) VALUES (10,1,'asset',1,'USD',1,'2026-01-01T00:00:00Z')`)
	execT(t, sqldb, `INSERT INTO transactions (id,date,subsidiary_id,currency,memo,deleted) VALUES (5,'2026-01-10',1,'USD','',0)`)
	execT(t, sqldb, `INSERT INTO reconciliations (id,account_id,statement_date,statement_balance,currency,status) VALUES (7,10,'2026-01-31',1000,'USD','open')`)
	execT(t, sqldb, `INSERT INTO splits (id,transaction_id,account_id,amount,position,memo,description,reconciliation_id) VALUES (9,5,10,1000,0,'','',7)`)

	if _, err := prov.Up(context.Background()); err != nil {
		t.Fatalf("apply 23: %v", err)
	}

	var acct int64
	if err := sqldb.QueryRow(`SELECT account_id FROM reconciliations WHERE id=7`).Scan(&acct); err != nil {
		t.Fatalf("recon id 7 not preserved after rebuild: %v", err)
	}
	var rid int64
	if err := sqldb.QueryRow(`SELECT reconciliation_id FROM splits WHERE id=9`).Scan(&rid); err != nil || rid != 7 {
		t.Fatalf("cleared split FK broken: reconciliation_id=%d err=%v, want 7", rid, err)
	}
	var fk int
	sqldb.QueryRow(`PRAGMA foreign_keys`).Scan(&fk)
	if fk != 1 {
		t.Errorf("foreign_keys=%d after migrate, want 1", fk)
	}
	rows, _ := sqldb.Query(`PRAGMA foreign_key_check`)
	defer rows.Close()
	if rows.Next() {
		t.Errorf("foreign_key_check reported a violation after the rebuild")
	}
	rows.Close()
	if _, err := sqldb.Exec(`UPDATE reconciliations SET status='discarded' WHERE id=7`); err != nil {
		t.Errorf("'discarded' not accepted after migrate: %v", err)
	}
}
