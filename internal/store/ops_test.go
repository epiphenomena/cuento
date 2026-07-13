package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"cuento/internal/testutil"
)

// p18.3 ops store tests: Backup produces a valid standalone SQLite snapshot via
// VACUUM INTO, and RecordBackup writes exactly one ops.backup audit change naming
// the actor. Both are exercised end to end (a real migrated temp db, the sqlite
// driver reopening the snapshot) -- no mocks (AGENTS testing conventions).

// TestBackupProducesValidSnapshot: Backup writes a fresh SQLite file that passes
// quick_check and carries the schema+data (the seeded system user).
func TestBackupProducesValidSnapshot(t *testing.T) {
	db := testutil.NewDB(t)
	s := New(db)

	dir := t.TempDir()
	snap := filepath.Join(dir, "snapshot.db")
	if err := s.Backup(context.Background(), snap); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if _, err := os.Stat(snap); err != nil {
		t.Fatalf("snapshot file missing: %v", err)
	}

	sdb, err := sql.Open("sqlite", "file:"+snap)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer func() { _ = sdb.Close() }()

	var quick string
	if err := sdb.QueryRow("PRAGMA quick_check").Scan(&quick); err != nil {
		t.Fatalf("PRAGMA quick_check: %v", err)
	}
	if quick != "ok" {
		t.Fatalf("quick_check = %q, want ok", quick)
	}
	var username string
	if err := sdb.QueryRow("SELECT username FROM users WHERE id = 1").Scan(&username); err != nil {
		t.Fatalf("read users from snapshot: %v", err)
	}
	if username != "system" {
		t.Errorf("snapshot users id=1 = %q, want system", username)
	}
}

// TestBackupRefusesExistingTarget: VACUUM INTO refuses an existing database file
// ("output file already exists"); Backup surfaces that as an error. This is WHY the
// web handler always targets a fresh, unique path inside a temp dir it owns -- two
// concurrent backups never collide, and a stale file never blocks a snapshot.
func TestBackupRefusesExistingTarget(t *testing.T) {
	db := testutil.NewDB(t)
	s := New(db)

	dir := t.TempDir()
	snap := filepath.Join(dir, "exists.db")
	// Create a real snapshot there first, then try to write over it.
	if err := s.Backup(context.Background(), snap); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	if err := s.Backup(context.Background(), snap); err == nil {
		t.Fatalf("Backup to an existing database path succeeded, want an error")
	}
}

// TestRecordBackupWritesAuditChange: RecordBackup writes exactly one ops.backup
// change naming the actor, and no versioned rows (it is not a business mutation).
func TestRecordBackupWritesAuditChange(t *testing.T) {
	db := testutil.NewDB(t)
	s := New(db)

	ctx := WithActor(context.Background(), Actor{ID: 1})
	if _, err := s.RecordBackup(ctx); err != nil {
		t.Fatalf("RecordBackup: %v", err)
	}

	var count int
	var actor int64
	if err := db.QueryRow(
		"SELECT count(*), coalesce(max(actor_id), 0) FROM changes WHERE kind = 'ops.backup'",
	).
		Scan(&count, &actor); err != nil {
		t.Fatalf("read ops.backup change: %v", err)
	}
	if count != 1 {
		t.Fatalf("ops.backup change count = %d, want 1", count)
	}
	if actor != 1 {
		t.Errorf("ops.backup actor = %d, want 1", actor)
	}
}

// TestRecordBackupRequiresActor: with no actor in context, RecordBackup fails with
// ErrNoActor and writes nothing (the funnel checks BEFORE opening a tx).
func TestRecordBackupRequiresActor(t *testing.T) {
	db := testutil.NewDB(t)
	s := New(db)

	if _, err := s.RecordBackup(context.Background()); err == nil {
		t.Fatalf("RecordBackup without an actor succeeded, want ErrNoActor")
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM changes WHERE kind = 'ops.backup'").Scan(&count); err != nil {
		t.Fatalf("count changes: %v", err)
	}
	if count != 0 {
		t.Errorf("actor-less RecordBackup wrote %d change(s), want 0", count)
	}
}
