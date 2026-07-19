package store

import (
	"context"
	"database/sql"
	"fmt"

	"cuento/internal/db/sqlc"
	"cuento/internal/ids"
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

// ReportGrant is one report-group grant a user holds (D10, p27.4): the group name
// plus an OPTIONAL program-subtree scope. ProgramID nil == unscoped (org-wide, the
// pre-p27.4 behavior); a non-nil ProgramID scopes the grant to that program node
// AND all its descendants (hierarchical, resolved via ProgramSubtree at report
// time) -- a program-dimensioned report then returns only that subtree's rows.
type ReportGrant struct {
	Group string
	// ProgramID is the granted program-subtree ROOT (nil = unscoped). It is a
	// pointer so nil is distinguishable from program id 0 (which is never a real
	// program).
	ProgramID *ids.ProgramID
}

// ReportGrants returns the report-group grants a user holds -- each a group name
// plus its optional program-subtree scope (D10, p27.4). It is a read (rule 2
// permits reads outside the funnel via sqlc), used by the permission-enforcement
// middleware ONLY for routes whose Perm is ReportGroup (and by the report row-scope
// resolver) -- so the anonymous / non-report hot path never pays for it.
func (s *Store) ReportGrants(ctx context.Context, userID ids.UserID) ([]ReportGrant, error) {
	rows, err := s.q.ReportGrantsForUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("store: report grants for user %d: %w", userID, err)
	}
	out := make([]ReportGrant, 0, len(rows))
	for _, r := range rows {
		g := ReportGrant{Group: r.GroupName}
		g.ProgramID = ids.Ptr[ids.ProgramID](r.ProgramID)
		out = append(out, g)
	}
	return out, nil
}

// ProgramSubtree returns the ids of a program plus its transitive descendants (self
// included, D24) -- the set a program-scoped report grant filters rows to (p27.4).
// A read reusing the ProgramDescendants recursive CTE; the write-closure-only
// ProgramSubtreeIDs (transactions.sql) is not reachable outside a write, so this is
// the read-path analogue for the web layer's grant-scope resolution.
func (s *Store) ProgramSubtree(ctx context.Context, id ids.ProgramID) ([]ids.ProgramID, error) {
	rows, err := s.q.ProgramDescendants(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("store: program subtree of %d: %w", id, err)
	}
	out := make([]ids.ProgramID, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out, nil
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

// GrantReportGroup adds (or re-scopes) a read grant on group for user, with an
// OPTIONAL program-subtree scope (programID nil = unscoped, org-wide -- the pre-p27.4
// behavior). Versioned under a change NAMING the acting admin (rule 5).
// user_report_grants_versions is a COMPOSITE-key twin (entity_id = user_id, snapshot
// group_name + program_id); the live key is (user_id, group_name) with program_id a
// mutable ATTRIBUTE, so:
//   - no existing grant -> insert live, then snapshot-from-live (op='create').
//   - existing grant, SAME scope -> no-op (no duplicate PK, no spurious version row).
//   - existing grant, DIFFERENT scope -> a scope CHANGE, handled AS revoke+grant in
//     ONE change (preserving the no-'update' membership convention): delete-version
//     (snapshot the OLD scope) BEFORE the live delete, then insert the NEW scope and
//     its create-version. Doing both legs in one write keeps the change atomic and the
//     version trail complete (old scope captured, new scope snapshotted).
//
// The system user (id 1) is refused (ErrSystemUser). programID, when non-nil, is the
// granted program-subtree root (self + descendants covered at report time).
func (s *Store) GrantReportGroup(ctx context.Context, userID ids.UserID, group string, programID *ids.ProgramID) error {
	if userID == systemUserID {
		return ErrSystemUser
	}
	want := sql.NullInt64{}
	if programID != nil {
		want = sql.NullInt64{Int64: int64(*programID), Valid: true}
	}
	_, err := s.write(ctx, "user.grant", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			has, err := q.HasReportGrant(ctx, sqlc.HasReportGrantParams{UserID: userID, GroupName: group})
			if err != nil {
				return fmt.Errorf("check grant: %w", err)
			}
			if has > 0 {
				cur, err := q.GetReportGrantScope(ctx, sqlc.GetReportGrantScopeParams{UserID: userID, GroupName: group})
				if err != nil {
					return fmt.Errorf("get grant scope: %w", err)
				}
				if cur == want {
					return nil // already granted with the same scope -- no-op, no version row
				}
				// Scope CHANGE: revoke the old scope (snapshot-from-live BEFORE the delete),
				// then re-grant with the new scope below.
				if err := q.InsertReportGrantVersion(ctx, sqlc.InsertReportGrantVersionParams{
					Op: "delete", ID: changeID, UserID: userID, GroupName: group,
				}); err != nil {
					return fmt.Errorf("version grant rescope-remove: %w", err)
				}
				if err := q.DeleteReportGrant(ctx, sqlc.DeleteReportGrantParams{UserID: userID, GroupName: group}); err != nil {
					return fmt.Errorf("delete grant (rescope): %w", err)
				}
			}
			if err := q.InsertReportGrant(ctx, sqlc.InsertReportGrantParams{UserID: userID, GroupName: group, ProgramID: want}); err != nil {
				return fmt.Errorf("insert grant: %w", err)
			}
			// Live-write FIRST, then snapshot-from-live (add order); the snapshot reads
			// program_id from the just-inserted live row.
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
func (s *Store) RevokeReportGroup(ctx context.Context, userID ids.UserID, group string) error {
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
