// Package testutil holds shared test helpers. NewDB(t) hands a test a migrated,
// isolated SQLite database; later steps add Fixture(t), AssertVersioned, and
// golden helpers here.
package testutil

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"cuento/internal/db"
)

// NewDB returns a migrated, temp-file SQLite database for a test. Each call
// creates a distinct file under the test's own t.TempDir(), so calling NewDB
// twice in one test yields two independent databases (no shared state). The
// handle is closed automatically via t.Cleanup when the test finishes; the temp
// dir (and its files) are removed by the testing package.
//
// It uses db.Open (the only place pragmas are set) then db.Migrate, so a
// harness database is configured and versioned exactly like production.
func NewDB(t *testing.T) *sql.DB {
	t.Helper()

	// A unique file name per call keeps repeated NewDB calls isolated even
	// within one test (t.TempDir returns the same dir across calls in a test).
	f, err := os.CreateTemp(t.TempDir(), "cuento-*.db")
	if err != nil {
		t.Fatalf("testutil.NewDB: temp file: %v", err)
	}
	path := f.Name()
	_ = f.Close() // db.Open/db.Migrate open by path; we only needed a unique name.

	if err := db.Migrate(context.Background(), path); err != nil {
		t.Fatalf("testutil.NewDB: migrate: %v", err)
	}

	sqldb, err := db.Open(path)
	if err != nil {
		t.Fatalf("testutil.NewDB: open: %v", err)
	}
	t.Cleanup(func() { _ = sqldb.Close() })

	return sqldb
}
