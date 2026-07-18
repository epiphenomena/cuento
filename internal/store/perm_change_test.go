package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"cuento/internal/testutil"
)

// TestPermChangeVersioned is the p13.2 versioning proof: a perm change and a
// report-group grant/revoke each append a version row whose change NAMES the acting
// admin (changes.actor_id), under the single write funnel (rule 5). It exercises
// both twins:
//
//   - users_versions   -- SetUserTxnPerm appends op='update' named the admin.
//   - user_report_grants_versions -- a grant -> revoke -> re-grant round trip walks
//     op create -> delete -> create, each named the admin.
//
// The op + change-existence are asserted by the AssertVersioned* helpers; the ACTOR
// (the point of "naming the acting admin") is asserted separately via
// LatestVersionActor / LatestGrantActor, since the op helpers do not check actor_id.
func TestPermChangeVersioned(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	seedCtx := WithActor(context.Background(), Actor{ID: 1})

	if err := s.SyncReportGroups(context.Background(), []string{"reports_x"}); err != nil {
		t.Fatalf("sync report groups: %v", err)
	}

	adminID, err := s.CreateUser(seedCtx, CreateUserInput{Username: "boss", DisplayName: "Boss", IsAdmin: true})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	targetID, err := s.CreateUser(seedCtx, CreateUserInput{Username: "target", DisplayName: "Target", TxnPerm: "none"})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}

	// Every admin action carries the ADMIN as the actor (this is what the audit
	// trail must name).
	adminCtx := WithActor(context.Background(), Actor{ID: adminID})

	// --- txn_perm change --------------------------------------------------------
	if err := s.SetUserTxnPerm(adminCtx, targetID, "write"); err != nil {
		t.Fatalf("SetUserTxnPerm: %v", err)
	}
	testutil.AssertVersioned(t, d, "users", targetID, "update")
	if got := testutil.LatestVersionActor(t, d, "users", targetID); got != adminID {
		t.Errorf("txn_perm change actor = %d, want admin %d", got, adminID)
	}
	// The live row reflects the change.
	if u, err := s.AdminUserByID(seedCtx, targetID); err != nil {
		t.Fatalf("AdminUserByID: %v", err)
	} else if u.TxnPerm != "write" {
		t.Errorf("txn_perm = %q, want write", u.TxnPerm)
	}

	// --- grant round trip: create -> delete -> create ---------------------------
	if err := s.GrantReportGroup(adminCtx, targetID, "reports_x", nil); err != nil {
		t.Fatalf("GrantReportGroup: %v", err)
	}
	testutil.AssertVersionedGrant(t, d, targetID, "reports_x", "create")
	if got := testutil.LatestGrantActor(t, d, targetID, "reports_x"); got != adminID {
		t.Errorf("grant actor = %d, want admin %d", got, adminID)
	}

	if err := s.RevokeReportGroup(adminCtx, targetID, "reports_x"); err != nil {
		t.Fatalf("RevokeReportGroup: %v", err)
	}
	testutil.AssertVersionedGrant(t, d, targetID, "reports_x", "delete")
	if got := testutil.LatestGrantActor(t, d, targetID, "reports_x"); got != adminID {
		t.Errorf("revoke actor = %d, want admin %d", got, adminID)
	}
	// The live grant is gone after a revoke.
	if gs, err := s.ReportGrants(seedCtx, targetID); err != nil {
		t.Fatalf("ReportGrants after revoke: %v", err)
	} else if len(gs) != 0 {
		t.Errorf("grants after revoke = %v, want empty", gs)
	}

	if err := s.GrantReportGroup(adminCtx, targetID, "reports_x", nil); err != nil {
		t.Fatalf("re-GrantReportGroup: %v", err)
	}
	testutil.AssertVersionedGrant(t, d, targetID, "reports_x", "create")
	if got := testutil.LatestGrantActor(t, d, targetID, "reports_x"); got != adminID {
		t.Errorf("re-grant actor = %d, want admin %d", got, adminID)
	}
	// The live grant is back.
	if gs, err := s.ReportGrants(seedCtx, targetID); err != nil {
		t.Fatalf("ReportGrants after re-grant: %v", err)
	} else if len(gs) != 1 || gs[0].Group != "reports_x" || gs[0].ProgramID != nil {
		t.Errorf("grants after re-grant = %v, want [reports_x unscoped]", gs)
	}
}

// TestGrantProgramScope (p27.4) proves a report grant carries an optional
// program-subtree scope: a scoped grant round-trips its program_id, a re-grant with
// the SAME scope is a no-op, and a re-grant with a DIFFERENT scope RE-SCOPES the
// grant (revoke+grant in one change: op create -> delete -> create). The versioned
// snapshot carries program_id, so a scope change leaves an auditable trail and the
// live scope always matches the latest snapshot (Z3, exercised by the ledger check
// integration tests + the demo anti-drift).
func TestGrantProgramScope(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	ctx := WithActor(context.Background(), Actor{ID: 1})

	if err := s.SyncReportGroups(context.Background(), []string{"reports_x"}); err != nil {
		t.Fatalf("sync report groups: %v", err)
	}
	// Two sibling programs under the seeded root (id 1) to scope to.
	progA, err := s.CreateProgram(ctx, CreateProgramInput{ParentID: 1, Name: "Alpha"})
	if err != nil {
		t.Fatalf("create program A: %v", err)
	}
	progB, err := s.CreateProgram(ctx, CreateProgramInput{ParentID: 1, Name: "Beta"})
	if err != nil {
		t.Fatalf("create program B: %v", err)
	}
	target, err := s.CreateUser(ctx, CreateUserInput{Username: "director", DisplayName: "Director"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Grant SCOPED to program A: the live scope round-trips.
	if err := s.GrantReportGroup(ctx, target, "reports_x", &progA); err != nil {
		t.Fatalf("GrantReportGroup scoped: %v", err)
	}
	testutil.AssertVersionedGrant(t, d, target, "reports_x", "create")
	if gs, err := s.ReportGrants(ctx, target); err != nil {
		t.Fatalf("ReportGrants: %v", err)
	} else if len(gs) != 1 || gs[0].ProgramID == nil || *gs[0].ProgramID != progA {
		t.Fatalf("scoped grant = %v, want scope=program A (%d)", gs, progA)
	}

	// Re-grant with the SAME scope: a no-op (no new version row).
	before := grantVersionCount(t, d, target, "reports_x")
	if err := s.GrantReportGroup(ctx, target, "reports_x", &progA); err != nil {
		t.Fatalf("re-grant same scope: %v", err)
	}
	if after := grantVersionCount(t, d, target, "reports_x"); after != before {
		t.Errorf("version rows after same-scope re-grant = %d, want unchanged %d", after, before)
	}

	// Re-scope to program B: revoke+grant in one change -> the live scope is now B, and a
	// delete THEN a create version row were appended (the auditable re-scope trail).
	if err := s.GrantReportGroup(ctx, target, "reports_x", &progB); err != nil {
		t.Fatalf("re-scope to B: %v", err)
	}
	testutil.AssertVersionedGrant(t, d, target, "reports_x", "create") // latest op is the new create
	if gs, err := s.ReportGrants(ctx, target); err != nil {
		t.Fatalf("ReportGrants after re-scope: %v", err)
	} else if len(gs) != 1 || gs[0].ProgramID == nil || *gs[0].ProgramID != progB {
		t.Fatalf("re-scoped grant = %v, want scope=program B (%d)", gs, progB)
	}
	if after := grantVersionCount(t, d, target, "reports_x"); after != before+2 {
		t.Errorf("version rows after re-scope = %d, want +2 (delete+create) over %d", after, before)
	}

	// Re-grant UNSCOPED (from scoped): another re-scope to NULL.
	if err := s.GrantReportGroup(ctx, target, "reports_x", nil); err != nil {
		t.Fatalf("re-scope to unscoped: %v", err)
	}
	if gs, err := s.ReportGrants(ctx, target); err != nil {
		t.Fatalf("ReportGrants after unscope: %v", err)
	} else if len(gs) != 1 || gs[0].ProgramID != nil {
		t.Fatalf("unscoped re-grant = %v, want scope=nil", gs)
	}
}

// grantVersionCount returns the number of user_report_grants_versions rows for a
// (user, group) -- a raw read (in-convention for tests, p05.3) so the re-scope trail
// can be counted.
func grantVersionCount(t *testing.T, d *sql.DB, userID int64, group string) int {
	t.Helper()
	var n int
	if err := d.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM user_report_grants_versions WHERE entity_id = ? AND group_name = ?`,
		userID, group).Scan(&n); err != nil {
		t.Fatalf("grantVersionCount: %v", err)
	}
	return n
}

// TestLastAdminGuard proves the DisableUser guard (p13.2): the last ENABLED admin
// cannot be disabled (ErrLastAdmin), the system user can never be disabled
// (ErrSystemUser), and a non-last admin CAN be disabled. It also confirms the guard
// keys on OTHER-enabled-admin count, not on non-system user count (CountHumanUsers):
// adding a non-admin operator does NOT unlock disabling the sole admin.
func TestLastAdminGuard(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	ctx := WithActor(context.Background(), Actor{ID: 1})

	adminA, err := s.CreateUser(ctx, CreateUserInput{Username: "admin_a", DisplayName: "A", IsAdmin: true})
	if err != nil {
		t.Fatalf("create admin_a: %v", err)
	}
	// A non-admin operator: its existence must NOT let the sole admin be disabled.
	if _, err := s.CreateUser(ctx, CreateUserInput{Username: "viewer", DisplayName: "V", TxnPerm: "read"}); err != nil {
		t.Fatalf("create viewer: %v", err)
	}

	if err := s.DisableUser(ctx, adminA); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("disable sole admin = %v, want ErrLastAdmin", err)
	}
	if err := s.DisableUser(ctx, systemUserID); !errors.Is(err, ErrSystemUser) {
		t.Fatalf("disable system user = %v, want ErrSystemUser", err)
	}

	// A SECOND admin unlocks disabling the first.
	adminB, err := s.CreateUser(ctx, CreateUserInput{Username: "admin_b", DisplayName: "B", IsAdmin: true})
	if err != nil {
		t.Fatalf("create admin_b: %v", err)
	}
	if err := s.DisableUser(ctx, adminA); err != nil {
		t.Fatalf("disable admin_a with a second admin present: %v", err)
	}
	// Now adminB is the last enabled admin -- it cannot be disabled.
	if err := s.DisableUser(ctx, adminB); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("disable now-sole admin_b = %v, want ErrLastAdmin", err)
	}
}
