package db_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"cuento/internal/db"
)

// synthMigrations builds an in-memory migration set with real CREATE TABLE
// statements so backup/idempotency/latest are genuinely exercised. count
// controls how many sequential migrations exist (1 => only 00001, etc.).
func synthMigrations(count int) fstest.MapFS {
	m := fstest.MapFS{}
	if count >= 1 {
		m["00001_first.sql"] = &fstest.MapFile{Data: []byte(
			"-- +goose Up\nCREATE TABLE t1 (id INTEGER PRIMARY KEY);\n",
		)}
	}
	if count >= 2 {
		m["00002_second.sql"] = &fstest.MapFile{Data: []byte(
			"-- +goose Up\nCREATE TABLE t2 (id INTEGER PRIMARY KEY);\n",
		)}
	}
	if count >= 3 {
		m["00003_third.sql"] = &fstest.MapFile{Data: []byte(
			"-- +goose Up\nCREATE TABLE t3 (id INTEGER PRIMARY KEY);\n",
		)}
	}
	return m
}

// gooseVersion reads the current goose schema version straight from the
// goose_db_version table so tests don't depend on the runner's own reporting.
func gooseVersion(t *testing.T, path string) int64 {
	t.Helper()
	sqldb, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = sqldb.Close() })

	var v int64
	err = sqldb.QueryRow(`SELECT MAX(version_id) FROM goose_db_version`).Scan(&v)
	if err != nil {
		t.Fatalf("read goose version: %v", err)
	}
	return v
}

// bakFiles returns the set of *.bak files sitting next to the db path.
func bakFiles(t *testing.T, path string) []string {
	t.Helper()
	matches, err := filepath.Glob(path + ".pre-*.bak")
	if err != nil {
		t.Fatalf("glob backups: %v", err)
	}
	return matches
}

// TestMigrateFreshReachesLatest: migrating a brand-new db applies every
// available migration and leaves the goose version at the latest.
func TestMigrateFreshReachesLatest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")

	if err := db.MigrateFS(context.Background(), path, synthMigrations(3)); err != nil {
		t.Fatalf("MigrateFS: %v", err)
	}

	if got := gooseVersion(t, path); got != 3 {
		t.Errorf("goose version = %d, want 3 (latest)", got)
	}
}

// TestMigrateIdempotent: running migrate twice is a no-op the second time — no
// error, version unchanged, and (crucially) no spurious backup written.
func TestMigrateIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idem.db")
	mig := synthMigrations(2)

	if err := db.MigrateFS(context.Background(), path, mig); err != nil {
		t.Fatalf("first MigrateFS: %v", err)
	}
	v1 := gooseVersion(t, path)

	before := bakFiles(t, path)
	if err := db.MigrateFS(context.Background(), path, mig); err != nil {
		t.Fatalf("second MigrateFS: %v", err)
	}
	after := bakFiles(t, path)

	if v2 := gooseVersion(t, path); v2 != v1 {
		t.Errorf("version changed on no-op migrate: %d -> %d", v1, v2)
	}
	if len(after) != len(before) {
		t.Errorf("no-op migrate wrote a backup: before=%v after=%v", before, after)
	}
}

// TestMigrateBacksUpFile: before migrations apply to an EXISTING (non-brand-new)
// db, a backup copy <db>.pre-<version>.bak must exist; the backup step is
// SKIPPED for brand-new files.
func TestMigrateBacksUpFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.db")

	// First migrate to version 1 on a brand-new file: no backup expected.
	if err := db.MigrateFS(context.Background(), path, synthMigrations(1)); err != nil {
		t.Fatalf("first MigrateFS: %v", err)
	}
	if baks := bakFiles(t, path); len(baks) != 0 {
		t.Fatalf("backup written for brand-new db: %v", baks)
	}

	// Now migrate the existing (version 1) db forward to version 2: a
	// pre-1.bak snapshot of the pre-migration db must exist.
	if err := db.MigrateFS(context.Background(), path, synthMigrations(2)); err != nil {
		t.Fatalf("second MigrateFS: %v", err)
	}

	bak := path + ".pre-1.bak"
	if _, err := os.Stat(bak); err != nil {
		t.Fatalf("expected backup %s: %v", bak, err)
	}

	// The backup must be a valid SQLite copy of the pre-migration state: it has
	// t1 (from 00001) but not t2 (which was applied after the backup).
	bakdb, err := sql.Open("sqlite", "file:"+bak)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	t.Cleanup(func() { _ = bakdb.Close() })

	var name string
	err = bakdb.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='t1'`,
	).Scan(&name)
	if err != nil {
		t.Fatalf("backup missing t1 (pre-migration table): %v", err)
	}
	err = bakdb.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='t2'`,
	).Scan(&name)
	if err != sql.ErrNoRows {
		t.Fatalf("backup contains t2; it was taken after migrating, not before: err=%v", err)
	}
}
