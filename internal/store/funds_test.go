package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"cuento/internal/ids"
	"cuento/internal/testutil"
)

// Fund operations (p07.3) -- funds are a restricted-fund SPLIT DIMENSION (D20).
// A fund scopes to ONE OR MORE subsidiaries via fund_subsidiaries (a FLAT set,
// NOT inherited and NO superset invariant -- unlike account_subsidiaries) and
// optionally to a program subtree. These tests copy the composite-membership
// versioning discipline the account tests (p05.2) established.

// newFundSub is a tiny helper: create a child subsidiary under the root and
// return its id (fund scope needs real subsidiaries; the seeded root id 1
// exists but child subs make multi-sub scoping tests clearer).
func newFundSub(t *testing.T, s *Store, name string) ids.SubsidiaryID {
	t.Helper()
	return newSub(t, s, rootID, name)
}

// getFund reads a fund's current live row for assertions.
func getFund(t *testing.T, s *Store, id ids.FundID) sqlcFund {
	t.Helper()
	row, err := s.GetFund(context.Background(), id)
	if err != nil {
		t.Fatalf("GetFund(%d): %v", id, err)
	}
	return sqlcFund{
		Name: row.Name, Funder: row.Funder, Purpose: row.Purpose,
		Restriction: row.Restriction, ProgramID: row.ProgramID,
		StartDate: row.StartDate, EndDate: row.EndDate, Notes: row.Notes,
		Active: row.Active,
	}
}

// sqlcFund mirrors the business columns for assertions without importing sqlc.
type sqlcFund struct {
	Name        string
	Funder      string
	Purpose     string
	Restriction string
	ProgramID   sql.NullInt64
	StartDate   sql.NullString
	EndDate     sql.NullString
	Notes       string
	Active      int64
}

// fundSubSet reads a fund's current subsidiary set as a map for assertions.
func fundSubSet(t *testing.T, d *sql.DB, fundID ids.FundID) map[int64]bool {
	t.Helper()
	rows, err := d.Query(`SELECT subsidiary_id FROM fund_subsidiaries WHERE fund_id = ?`, fundID)
	if err != nil {
		t.Fatalf("fundSubSet(%d): %v", fundID, err)
	}
	defer func() { _ = rows.Close() }()
	set := make(map[int64]bool)
	for rows.Next() {
		var sid int64
		if err := rows.Scan(&sid); err != nil {
			t.Fatalf("fundSubSet scan: %v", err)
		}
		set[sid] = true
	}
	return set
}

// TestCreateFundVersioned: creating a fund + its subsidiary memberships appends
// a create-op fund version AND a create-op membership version per subsidiary,
// ALL under ONE change (change count increments by exactly 1).
func TestCreateFundVersioned(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	subA := newFundSub(t, s, "A")
	subB := newFundSub(t, s, "B")

	before := countChanges(t, d)
	id, err := s.CreateFund(mutCtx(), CreateFundInput{
		Name:         "Youth Grant",
		Funder:       "Acme Foundation",
		Purpose:      "youth programs",
		Restriction:  "purpose",
		Subsidiaries: []ids.SubsidiaryID{subA, subB},
	})
	if err != nil {
		t.Fatalf("CreateFund: %v", err)
	}
	if id <= 0 {
		t.Fatalf("CreateFund returned id %d, want positive", id)
	}

	// Exactly ONE change for the whole fund + membership create.
	if n := countChanges(t, d); n != before+1 {
		t.Errorf("changes count = %d, want %d (fund + memberships under one change)", n, before+1)
	}

	testutil.AssertVersioned(t, d, "funds", int64(id), "create")
	testutil.AssertVersionedFundSub(t, d, int64(id), int64(subA), "create")
	testutil.AssertVersionedFundSub(t, d, int64(id), int64(subB), "create")

	// Live row matches inputs.
	f := getFund(t, s, id)
	if f.Name != "Youth Grant" || f.Funder != "Acme Foundation" ||
		f.Restriction != "purpose" || f.Active != 1 {
		t.Errorf("live fund = %+v, want (Youth Grant, Acme Foundation, purpose, active=1)", f)
	}
	if set := fundSubSet(t, d, id); !set[int64(subA)] || !set[int64(subB)] || len(set) != 2 {
		t.Errorf("fund sub set = %v, want {%d,%d}", set, subA, subB)
	}
}

// TestCreateFundRequiresAtLeastOneSub: CreateFund with zero subsidiaries is
// rejected (ErrFundNoSubsidiary) and leaves no change-row trace. Named with a
// Fund suffix because accounts (p05.2) already declared
// TestCreateRequiresAtLeastOneSub in this shared package.
func TestCreateFundRequiresAtLeastOneSub(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	before := countChanges(t, d)
	_, err := s.CreateFund(mutCtx(), CreateFundInput{
		Name:         "No Scope",
		Restriction:  "perpetual",
		Subsidiaries: nil,
	})
	if !errors.Is(err, ErrFundNoSubsidiary) {
		t.Fatalf("CreateFund(no subs): err = %v, want ErrFundNoSubsidiary", err)
	}
	if n := countChanges(t, d); n != before {
		t.Errorf("changes count = %d, want %d (rejected create leaves no trace)", n, before)
	}
}

// TestActiveFundsForSubsidiary: a fund scoped to {A,B} appears for A and for B,
// NOT for a third sub C; a CLOSED fund does not appear (D20/Q1 -- the txn
// editor's option source).
func TestActiveFundsForSubsidiary(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	subA := newFundSub(t, s, "A")
	subB := newFundSub(t, s, "B")
	subC := newFundSub(t, s, "C")

	ab, err := s.CreateFund(mutCtx(), CreateFundInput{
		Name: "AB Grant", Restriction: "purpose", Subsidiaries: []ids.SubsidiaryID{subA, subB},
	})
	if err != nil {
		t.Fatalf("CreateFund AB: %v", err)
	}
	// A closed fund scoped to A must not appear.
	closed, err := s.CreateFund(mutCtx(), CreateFundInput{
		Name: "Closed", Restriction: "time", Subsidiaries: []ids.SubsidiaryID{subA},
	})
	if err != nil {
		t.Fatalf("CreateFund closed: %v", err)
	}
	if err := s.CloseFund(mutCtx(), closed); err != nil {
		t.Fatalf("CloseFund: %v", err)
	}

	assertFundsForSub := func(sub ids.SubsidiaryID, wantIDs ...ids.FundID) {
		t.Helper()
		funds, err := s.ActiveFunds(context.Background(), int64(sub))
		if err != nil {
			t.Fatalf("ActiveFunds(%d): %v", sub, err)
		}
		got := make([]ids.FundID, len(funds))
		for i, f := range funds {
			got[i] = f.ID
		}
		if len(got) != len(wantIDs) {
			t.Fatalf("ActiveFunds(%d) = %v, want %v", sub, got, wantIDs)
		}
		for i := range wantIDs {
			if got[i] != wantIDs[i] {
				t.Fatalf("ActiveFunds(%d) = %v, want %v (deterministic order)", sub, got, wantIDs)
			}
		}
	}

	assertFundsForSub(subA, ab) // closed fund excluded
	assertFundsForSub(subB, ab)
	assertFundsForSub(subC) // no fund scoped to C
}

// TestProgramScopeStored: CreateFund with a program_id stores it and it is
// retrievable on the live row and in the version snapshot.
func TestProgramScopeStored(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	subA := newFundSub(t, s, "A")
	prog, err := s.CreateProgram(mutCtx(), CreateProgramInput{ParentID: rootProgramID, Name: "Grants"})
	if err != nil {
		t.Fatalf("CreateProgram: %v", err)
	}

	id, err := s.CreateFund(mutCtx(), CreateFundInput{
		Name: "Scoped", Restriction: "purpose",
		ProgramID: &prog, Subsidiaries: []ids.SubsidiaryID{subA},
	})
	if err != nil {
		t.Fatalf("CreateFund with program: %v", err)
	}

	f := getFund(t, s, id)
	if !f.ProgramID.Valid || f.ProgramID.Int64 != int64(prog) {
		t.Errorf("live program_id = %+v, want %d", f.ProgramID, prog)
	}

	// The version snapshot carries the program scope too.
	var vProg sql.NullInt64
	if err := d.QueryRow(
		`SELECT program_id FROM funds_versions
		  WHERE entity_id = ? ORDER BY valid_from DESC, id DESC LIMIT 1`, id,
	).Scan(&vProg); err != nil {
		t.Fatalf("read fund version program_id: %v", err)
	}
	if !vProg.Valid || vProg.Int64 != int64(prog) {
		t.Errorf("snapshot program_id = %+v, want %d", vProg, prog)
	}

	// A missing program is rejected cleanly with no trace.
	before := countChanges(t, d)
	bad := ids.ProgramID(9999)
	if _, err := s.CreateFund(mutCtx(), CreateFundInput{
		Name: "BadProg", Restriction: "purpose",
		ProgramID: &bad, Subsidiaries: []ids.SubsidiaryID{subA},
	}); !errors.Is(err, ErrFundProgramMissing) {
		t.Fatalf("CreateFund bad program: err = %v, want ErrFundProgramMissing", err)
	}
	if n := countChanges(t, d); n != before {
		t.Errorf("changes count = %d, want %d (rejected create leaves no trace)", n, before)
	}
}

// TestReopenAudited: CloseFund then ReopenFund each append an op='update'
// version row (audited), and the live active flag toggles 0 then 1.
func TestReopenAudited(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	subA := newFundSub(t, s, "A")
	id, err := s.CreateFund(mutCtx(), CreateFundInput{
		Name: "Toggler", Restriction: "time", Subsidiaries: []ids.SubsidiaryID{subA},
	})
	if err != nil {
		t.Fatalf("CreateFund: %v", err)
	}

	if err := s.CloseFund(mutCtx(), id); err != nil {
		t.Fatalf("CloseFund: %v", err)
	}
	testutil.AssertVersioned(t, d, "funds", int64(id), "update")
	if f := getFund(t, s, id); f.Active != 0 {
		t.Errorf("after close active = %d, want 0", f.Active)
	}

	if err := s.ReopenFund(mutCtx(), id); err != nil {
		t.Fatalf("ReopenFund: %v", err)
	}
	testutil.AssertVersioned(t, d, "funds", int64(id), "update")
	if f := getFund(t, s, id); f.Active != 1 {
		t.Errorf("after reopen active = %d, want 1", f.Active)
	}

	// Both toggles are op='update' (never 'delete'): count the update versions.
	var updates int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM funds_versions WHERE entity_id = ? AND op = 'update'`, id,
	).Scan(&updates); err != nil {
		t.Fatalf("count update versions: %v", err)
	}
	if updates != 2 {
		t.Errorf("update version rows = %d, want 2 (close + reopen audited)", updates)
	}
}

// TestUpdateFundSubsetChange: UpdateFund's subsidiary-set diff adds new
// memberships with op='create' and removes dropped ones with op='delete' (the
// composite-membership versioning convention), plus a plain field update, all
// under one change per call. Still >=1 sub after the change.
func TestUpdateFundSubsetChange(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	subA := newFundSub(t, s, "A")
	subB := newFundSub(t, s, "B")
	subC := newFundSub(t, s, "C")

	id, err := s.CreateFund(mutCtx(), CreateFundInput{
		Name: "Grant", Restriction: "purpose", Subsidiaries: []ids.SubsidiaryID{subA, subB},
	})
	if err != nil {
		t.Fatalf("CreateFund: %v", err)
	}

	// Change set {A,B} -> {A,C}: adds C (create), removes B (delete), keeps A.
	newName := "Renamed Grant"
	if err := s.UpdateFund(mutCtx(), id, UpdateFundInput{
		Name:         &newName,
		Subsidiaries: []ids.SubsidiaryID{subA, subC},
	}); err != nil {
		t.Fatalf("UpdateFund: %v", err)
	}

	set := fundSubSet(t, d, id)
	if !set[int64(subA)] || !set[int64(subC)] || set[int64(subB)] || len(set) != 2 {
		t.Errorf("live sub set = %v, want {%d,%d}", set, subA, subC)
	}
	// Membership version ops: C created, B deleted, A still create (untouched).
	testutil.AssertVersionedFundSub(t, d, int64(id), int64(subC), "create")
	testutil.AssertVersionedFundSub(t, d, int64(id), int64(subB), "delete")
	testutil.AssertVersionedFundSub(t, d, int64(id), int64(subA), "create")

	if f := getFund(t, s, id); f.Name != "Renamed Grant" {
		t.Errorf("live name = %q, want Renamed Grant", f.Name)
	}
	testutil.AssertVersioned(t, d, "funds", int64(id), "update")

	// Narrowing to empty is rejected (>=1 must remain), no trace.
	before := countChanges(t, d)
	if err := s.UpdateFund(mutCtx(), id, UpdateFundInput{Subsidiaries: []ids.SubsidiaryID{}}); !errors.Is(err, ErrFundNoSubsidiary) {
		t.Fatalf("UpdateFund to empty set: err = %v, want ErrFundNoSubsidiary", err)
	}
	if n := countChanges(t, d); n != before {
		t.Errorf("changes count = %d, want %d (rejected update leaves no trace)", n, before)
	}
}

// TestCloseFundBlocksNewUse: the full "close blocks NEW use in a transaction"
// assertion is enforced at transaction-post time in p08 (splits do not exist
// yet). TAGGED here: assert only that CloseFund is versioned and sets active=0
// NOW; the post-time block is proven in p08.2.
func TestCloseFundBlocksNewUse(t *testing.T) {
	// p08.2 (completed): the post-time block is proven by TestPostInactiveFundRejected
	// (PostTransaction rejects a split tagged with a closed fund). Here, assert only
	// the close itself is versioned and sets active=0.
	d := testutil.NewDB(t)
	s := New(d)

	subA := newFundSub(t, s, "A")
	id, err := s.CreateFund(mutCtx(), CreateFundInput{
		Name: "ToClose", Restriction: "purpose", Subsidiaries: []ids.SubsidiaryID{subA},
	})
	if err != nil {
		t.Fatalf("CreateFund: %v", err)
	}
	if err := s.CloseFund(mutCtx(), id); err != nil {
		t.Fatalf("CloseFund: %v", err)
	}
	testutil.AssertVersioned(t, d, "funds", int64(id), "update")
	if f := getFund(t, s, id); f.Active != 0 {
		t.Errorf("after close active = %d, want 0", f.Active)
	}
}

// TestNarrowSubsBlockedBySplits: narrowing a fund's subsidiary set (removing a
// sub S) is blocked (ErrFundSubInUseBySplit) while a split tagged the fund lives
// in a non-deleted transaction of subsidiary S (the p08.2-completed guard).
func TestNarrowSubsBlockedBySplits(t *testing.T) {
	e := newTxnEnv(t)
	// A fund scoped to {US, MX}; post a txn in US tagging the fund.
	subMX := newSub(t, e.s, rootID, "MX")
	fund := mkFund(t, e.s, "Grant", []ids.SubsidiaryID{e.subUS, subMX}, nil)
	in := e.balancedInput(100)
	in.Splits[0].FundID = &fund
	in.Splits[1].FundID = &fund
	if _, err := e.s.PostTransaction(mutCtx(), in); err != nil {
		t.Fatalf("PostTransaction: %v", err)
	}

	// Removing US (which has the split) must be blocked.
	before := countChanges(t, e.d)
	err := e.s.UpdateFund(mutCtx(), fund, UpdateFundInput{Subsidiaries: []ids.SubsidiaryID{subMX}})
	if !errors.Is(err, ErrFundSubInUseBySplit) {
		t.Fatalf("narrow away US: err = %v, want ErrFundSubInUseBySplit", err)
	}
	if n := countChanges(t, e.d); n != before {
		t.Errorf("changes = %d, want %d (rejected leaves no trace)", n, before)
	}

	// Removing MX (no split there) succeeds.
	if err := e.s.UpdateFund(mutCtx(), fund, UpdateFundInput{Subsidiaries: []ids.SubsidiaryID{e.subUS}}); err != nil {
		t.Fatalf("narrow away MX (unused): %v", err)
	}
}

// TestFundSpanishNameSurvivesClose: name_es round-trips through create/update and
// SURVIVES CloseFund (setFundActive does a full read-modify-write; dropping the new
// column would silently blank the Spanish name -- the advisor's wipe risk).
func TestFundSpanishNameSurvivesClose(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	sub := newFundSub(t, s, "US")

	id, err := s.CreateFund(mutCtx(), CreateFundInput{
		Name:         "Ford Grant",
		NameES:       "Beca Ford",
		Restriction:  "purpose",
		Subsidiaries: []ids.SubsidiaryID{sub},
	})
	if err != nil {
		t.Fatalf("CreateFund: %v", err)
	}

	row, err := s.GetFund(context.Background(), id)
	if err != nil {
		t.Fatalf("GetFund: %v", err)
	}
	if row.NameEs != "Beca Ford" {
		t.Fatalf("after create: name_es=%q", row.NameEs)
	}

	// Update only the funder; name_es must be preserved.
	funder := "Ford Foundation"
	if err := s.UpdateFund(mutCtx(), id, UpdateFundInput{Funder: &funder}); err != nil {
		t.Fatalf("UpdateFund: %v", err)
	}
	row, _ = s.GetFund(context.Background(), id)
	if row.NameEs != "Beca Ford" {
		t.Fatalf("after update: name_es=%q", row.NameEs)
	}

	// Close must NOT blank name_es.
	if err := s.CloseFund(mutCtx(), id); err != nil {
		t.Fatalf("CloseFund: %v", err)
	}
	row, _ = s.GetFund(context.Background(), id)
	if row.NameEs != "Beca Ford" {
		t.Fatalf("after close (wipe bug): name_es=%q", row.NameEs)
	}
	if row.Active != 0 {
		t.Fatalf("expected closed (active=0)")
	}

	// Latest version snapshot carries name_es (Z3).
	var vNameEs string
	if err := d.QueryRow(
		`SELECT name_es FROM funds_versions
		  WHERE entity_id = ? ORDER BY valid_from DESC, id DESC LIMIT 1`, id,
	).Scan(&vNameEs); err != nil {
		t.Fatalf("latest fund version: %v", err)
	}
	if vNameEs != "Beca Ford" {
		t.Fatalf("latest version snapshot: name_es=%q", vNameEs)
	}
}
