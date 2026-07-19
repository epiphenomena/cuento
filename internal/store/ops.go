package store

import (
	"context"
	"fmt"

	"cuento/internal/db/sqlc"
	"cuento/internal/ids"
)

// p18.3 ops: the admin ops page's audited backup action. Taking a database
// snapshot is not a business-table mutation (it writes no versioned row), but it
// IS an operator action the audit trail must record (rule 14): who took a backup,
// and when. RecordBackup writes exactly ONE changes row through the write funnel
// (kind "ops.backup", the acting admin as actor), with no accompanying live/
// version write -- the same shape the reconciliation boundary changes use. There
// is deliberately no *_versions coupling, so `cuento check` stays clean (no Z-rule
// ties changes to a version row; verified in p18.3).
//
// The snapshot ITSELF is produced by a VACUUM INTO on the raw handle (Backup,
// below): VACUUM cannot run inside a transaction, so it is a non-funnel read-side
// operation (like the config upserts, rule 2's sanctioned exception). The web
// handler runs the VACUUM first and only records the audit change on success, so a
// failed backup leaves no misleading audit trace (mirroring the funnel's "rejected
// ops leave NO audit row").

// RecordBackup writes the audit change for a completed backup snapshot: one
// changes row (kind "ops.backup") naming the actor in ctx. It returns the change
// id (unused today, kept for symmetry with the other entity ops). It performs NO
// other write -- the snapshot is a separate, non-transactional VACUUM INTO.
func (s *Store) RecordBackup(ctx context.Context) (ids.ChangeID, error) {
	return s.write(ctx, "ops.backup", "",
		func(_ context.Context, _ *sqlc.Queries, _ ids.ChangeID) error { return nil })
}

// Backup writes a consistent, standalone snapshot of the whole database to path
// via SQLite's `VACUUM INTO` (a built-in supported by modernc.org/sqlite). The
// result is a fresh, defragmented SQLite file safe to copy off-box; unlike copying
// the live file it is transactionally consistent even under concurrent writes.
//
// path MUST NOT already exist (VACUUM INTO refuses to overwrite); callers pass a
// unique path inside a temp dir they own and clean up. The path is a BOUND
// parameter (not string-concatenated), so rule 6 holds even for this const-SQL
// site. VACUUM cannot run inside a transaction, so this bypasses the write funnel
// deliberately (a read-side consistency operation); the AUDIT of the action is the
// separate RecordBackup call.
func (s *Store) Backup(ctx context.Context, path string) error {
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return fmt.Errorf("store.Backup: vacuum into: %w", err)
	}
	return nil
}
