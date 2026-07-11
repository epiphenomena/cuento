package testutil_test

import (
	"testing"

	"cuento/internal/testutil"
)

// TestNewDBIsolated proves each NewDB(t) call yields an independent database:
// writing to one is invisible to the other. They are distinct temp files, so
// tests never leak state into one another through a shared handle.
func TestNewDBIsolated(t *testing.T) {
	a := testutil.NewDB(t)
	b := testutil.NewDB(t)

	if _, err := a.Exec(`CREATE TABLE marker (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create marker in a: %v", err)
	}
	if _, err := a.Exec(`INSERT INTO marker (id) VALUES (1)`); err != nil {
		t.Fatalf("insert marker in a: %v", err)
	}

	// The table must not exist in b: separate files, separate schemas.
	var name string
	err := b.QueryRow(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'marker'`,
	).Scan(&name)
	if err == nil {
		t.Fatalf("marker table visible in b (%q); databases are not isolated", name)
	}

	// And b must be writable independently — its own marker table with a
	// different row proves it is a live, separate database.
	if _, err := b.Exec(`CREATE TABLE marker (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create marker in b: %v", err)
	}
	if _, err := b.Exec(`INSERT INTO marker (id) VALUES (2)`); err != nil {
		t.Fatalf("insert marker in b: %v", err)
	}

	var got int
	if err := a.QueryRow(`SELECT id FROM marker`).Scan(&got); err != nil {
		t.Fatalf("read marker from a: %v", err)
	}
	if got != 1 {
		t.Errorf("a marker id = %d, want 1 (b's write leaked into a)", got)
	}
}
