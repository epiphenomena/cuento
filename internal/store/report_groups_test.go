package store

import (
	"context"
	"database/sql"
	"testing"

	"cuento/internal/testutil"
)

// TestSyncReportGroupsIdempotent proves the startup sync (D10) is a safe-every-boot
// upsert: running it twice with the same code-declared set leaves exactly that set
// (no duplicates, no error), and sort reflects the declared order. report_groups is
// reference data synced OUTSIDE the write funnel (no actor, no changes row) — like
// currencies — so the sync takes a plain context.
func TestSyncReportGroupsIdempotent(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	ctx := context.Background()

	names := []string{"grp_a", "grp_b"}
	if err := s.SyncReportGroups(ctx, names); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	// Second boot: idempotent, no error, no duplicate rows.
	if err := s.SyncReportGroups(ctx, names); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	got := reportGroupSorts(t, d)
	if len(got) != 2 || got["grp_a"] != 0 || got["grp_b"] != 1 {
		t.Fatalf("after sync, report_groups = %v, want {grp_a:0, grp_b:1}", got)
	}
}

// TestReportGrantsRoundTrip proves the grant-read the ReportGroup permission
// enforcement relies on: after the group is synced (so the FK resolves) and a
// grant is inserted (direct SQL — grant WRITERS land in p13.2, raw SQL in tests is
// in-convention, p05.3), ReportGrants returns exactly that group for the user and
// nothing for an ungranted user. This exercises the enforcement's real DB path,
// which p15's report routes will drive over HTTP through the permission matrix.
func TestReportGrantsRoundTrip(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	ctx := WithActor(context.Background(), Actor{ID: 1})

	if err := s.SyncReportGroups(context.Background(), []string{"reports_x"}); err != nil {
		t.Fatalf("sync: %v", err)
	}

	granted, err := s.CreateUser(ctx, CreateUserInput{Username: "granted", DisplayName: "Granted"})
	if err != nil {
		t.Fatalf("create granted user: %v", err)
	}
	ungranted, err := s.CreateUser(ctx, CreateUserInput{Username: "ungranted", DisplayName: "Ungranted"})
	if err != nil {
		t.Fatalf("create ungranted user: %v", err)
	}

	if _, err := d.ExecContext(ctx,
		`INSERT INTO user_report_grants (user_id, group_name) VALUES (?, ?)`,
		granted, "reports_x"); err != nil {
		t.Fatalf("insert grant: %v", err)
	}

	gs, err := s.ReportGrants(ctx, granted)
	if err != nil {
		t.Fatalf("ReportGrants(granted): %v", err)
	}
	if len(gs) != 1 || gs[0].Group != "reports_x" || gs[0].ProgramID != nil {
		t.Fatalf("ReportGrants(granted) = %v, want [reports_x unscoped]", gs)
	}

	none, err := s.ReportGrants(ctx, ungranted)
	if err != nil {
		t.Fatalf("ReportGrants(ungranted): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("ReportGrants(ungranted) = %v, want empty", none)
	}
}

// reportGroupSorts reads report_groups into a name->sort map via direct SQL (raw
// SQL in store tests is in-convention).
func reportGroupSorts(t *testing.T, d *sql.DB) map[string]int64 {
	t.Helper()
	rows, err := d.Query(`SELECT name, sort FROM report_groups`)
	if err != nil {
		t.Fatalf("query report_groups: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var name string
		var sort int64
		if err := rows.Scan(&name, &sort); err != nil {
			t.Fatalf("scan report_groups: %v", err)
		}
		out[name] = sort
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate report_groups: %v", err)
	}
	return out
}
