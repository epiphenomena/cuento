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

// ReportGroupNames returns every code-declared report group, in declared sort
// order (p13.2): the checkboxes the per-user admin page offers. A read.
func (s *Store) ReportGroupNames(ctx context.Context) ([]string, error) {
	rows, err := s.q.ListReportGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list report groups: %w", err)
	}
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r.Name)
	}
	return names, nil
}

// GrantReportGroup adds a read grant on group for user (p13.2), versioned as one
// op='create' user_report_grants_versions row under a change NAMING the acting
// admin (rule 5). user_report_grants_versions is a COMPOSITE-key twin (entity_id
// = user_id, snapshot group_name), so this mirrors account-subsidiary membership:
// guard with HasReportGrant (a re-grant is a no-op, no duplicate PK, no spurious
// version row), insert live, then snapshot-from-live. The system user (id 1) is
// refused (ErrSystemUser). A no-op re-grant still returns nil.
func (s *Store) GrantReportGroup(ctx context.Context, userID int64, group string) error {
	if userID == systemUserID {
		return ErrSystemUser
	}
	_, err := s.write(ctx, "user.grant", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			has, err := q.HasReportGrant(ctx, sqlc.HasReportGrantParams{UserID: userID, GroupName: group})
			if err != nil {
				return fmt.Errorf("check grant: %w", err)
			}
			if has > 0 {
				return nil // already granted -- no-op, no version row
			}
			if err := q.InsertReportGrant(ctx, sqlc.InsertReportGrantParams{UserID: userID, GroupName: group}); err != nil {
				return fmt.Errorf("insert grant: %w", err)
			}
			// Live-write FIRST, then snapshot-from-live (add order).
			if err := q.InsertReportGrantVersion(ctx, sqlc.InsertReportGrantVersionParams{
				Op: "create", ID: changeID, UserID: userID, GroupName: group,
			}); err != nil {
				return fmt.Errorf("version grant add: %w", err)
			}
			return nil
		})
	if err != nil {
		return fmt.Errorf("grant report group %q to user %d: %w", group, userID, err)
	}
	return nil
}

// RevokeReportGroup removes a read grant (p13.2), versioned as one op='delete'
// row naming the acting admin. REMOVAL INVERTS the write order: the version
// append is snapshot-from-live, so the delete-version row MUST be captured BEFORE
// the live row is deleted (the account-subsidiaries removeSub convention). A
// revoke of a grant the user does not hold is a no-op (no live row to snapshot, no
// version row). The system user (id 1) is refused (ErrSystemUser).
func (s *Store) RevokeReportGroup(ctx context.Context, userID int64, group string) error {
	if userID == systemUserID {
		return ErrSystemUser
	}
	_, err := s.write(ctx, "user.revoke", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			has, err := q.HasReportGrant(ctx, sqlc.HasReportGrantParams{UserID: userID, GroupName: group})
			if err != nil {
				return fmt.Errorf("check grant: %w", err)
			}
			if has == 0 {
				return nil // not granted -- no-op, no version row
			}
			// Snapshot-from-live BEFORE the live delete (removal order).
			if err := q.InsertReportGrantVersion(ctx, sqlc.InsertReportGrantVersionParams{
				Op: "delete", ID: changeID, UserID: userID, GroupName: group,
			}); err != nil {
				return fmt.Errorf("version grant remove: %w", err)
			}
			if err := q.DeleteReportGrant(ctx, sqlc.DeleteReportGrantParams{UserID: userID, GroupName: group}); err != nil {
				return fmt.Errorf("delete grant: %w", err)
			}
			return nil
		})
	if err != nil {
		return fmt.Errorf("revoke report group %q from user %d: %w", group, userID, err)
	}
	return nil
}
