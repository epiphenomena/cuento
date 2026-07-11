package store

import (
	"context"
	"fmt"

	"cuento/internal/db/sqlc"
)

// Report groups are code-declared reference data (D10): the set lives in code
// (declared by the web layer with the report routes) and is synced to the db at
// startup. Because report_groups is NOT a versioned business table (no *_versions
// twin, no changes wiring -- Appendix A omits it), the sync is a plain idempotent
// upsert OUTSIDE the write funnel, exactly like currencies: rule 2 permits reads
// and reference-data upserts via sqlc without an actor or a changes row. The
// per-user read grants (user_report_grants) ARE versioned; their writers land in
// p13.2. This step only needs to READ grants (for permission enforcement) and
// UPSERT groups (for the startup sync).

// SyncReportGroups upserts the code-declared report groups into report_groups,
// idempotently (safe on every boot). names is the ordered set the code declares;
// each name's index becomes its sort key so the db reflects the declared order.
// Existing groups are refreshed, new ones inserted; nothing is deleted -- a group
// that disappears from code is left in place so historical grants keep their FK
// (pruning stale groups is a later concern once grant management exists, p13.2).
func (s *Store) SyncReportGroups(ctx context.Context, names []string) error {
	for i, name := range names {
		if err := s.q.UpsertReportGroup(ctx, sqlc.UpsertReportGroupParams{
			Name: name,
			Sort: int64(i),
		}); err != nil {
			return fmt.Errorf("store: sync report group %q: %w", name, err)
		}
	}
	return nil
}

// ReportGrants returns the report-group names a user has been granted read access
// to (D10). It is a read (rule 2 permits reads outside the funnel via sqlc), used
// by the permission-enforcement middleware ONLY for routes whose Perm is
// ReportGroup -- so the anonymous / non-report hot path never pays for it.
func (s *Store) ReportGrants(ctx context.Context, userID int64) ([]string, error) {
	names, err := s.q.ReportGrantsForUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("store: report grants for user %d: %w", userID, err)
	}
	return names, nil
}
