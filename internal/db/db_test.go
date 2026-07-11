package db_test

import (
	"path/filepath"
	"strings"
	"testing"

	"cuento/internal/db"
)

// TestOpenSetsPragmas verifies the per-connection pragmas Open configures are
// actually in effect on a pooled connection: WAL journaling, foreign-key
// enforcement, a positive busy timeout, and NORMAL synchronous. journal_mode
// reads back lowercase ("wal"); the rest read back as integers.
func TestOpenSetsPragmas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pragmas.db")
	sqldb, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = sqldb.Close() })

	var journalMode string
	if err := sqldb.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Errorf("journal_mode = %q, want wal", journalMode)
	}

	var foreignKeys int
	if err := sqldb.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("query foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Errorf("foreign_keys = %d, want 1", foreignKeys)
	}

	var busyTimeout int
	if err := sqldb.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("query busy_timeout: %v", err)
	}
	if busyTimeout <= 0 {
		t.Errorf("busy_timeout = %d, want > 0", busyTimeout)
	}

	var synchronous int
	if err := sqldb.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil {
		t.Fatalf("query synchronous: %v", err)
	}
	if synchronous != 1 { // 1 = NORMAL, appropriate for WAL.
		t.Errorf("synchronous = %d, want 1 (NORMAL)", synchronous)
	}
}

// TestForeignKeysEnforced proves foreign_keys is ON for real work on pooled
// connections: a child row referencing a missing parent must be rejected.
func TestForeignKeysEnforced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fk.db")
	sqldb, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = sqldb.Close() })

	if _, err := sqldb.Exec(`CREATE TABLE parent (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	if _, err := sqldb.Exec(`CREATE TABLE child (
		id        INTEGER PRIMARY KEY,
		parent_id INTEGER NOT NULL REFERENCES parent(id)
	)`); err != nil {
		t.Fatalf("create child: %v", err)
	}

	// No parent with id 999 exists; the insert must fail on the FK constraint.
	if _, err := sqldb.Exec(`INSERT INTO child (id, parent_id) VALUES (1, 999)`); err == nil {
		t.Fatal("insert violating foreign key succeeded; foreign_keys is not enforced")
	}
}
