package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"cuento/internal/testutil"
)

// Program operations (p07.1) mirror the subsidiary ops (p04.2) MINUS
// base_currency: a single-root tree with a seeded root ("General", id 1). Names
// are distinct from the subsidiary ones because package store is shared
// (ErrProgramSecondRoot vs ErrSecondRoot, ProgramTree vs SubTree, etc.).

// rootProgramID is the seeded root program's id (00008 seeds id 1, NULL parent).
const rootProgramID int64 = 1

// getProgram reads a program's current live row through the store read method.
func getProgram(t *testing.T, s *Store, id int64) (parentID sql.NullInt64, name string, active int64) {
	t.Helper()
	row, err := s.GetProgram(context.Background(), id)
	if err != nil {
		t.Fatalf("GetProgram(%d): %v", id, err)
	}
	return row.ParentID, row.Name, row.Active
}

// latestProgramVersion reads the newest programs_versions snapshot for an entity.
func latestProgramVersion(t *testing.T, d *sql.DB, entityID int64) (op, name string, active int64, parentID sql.NullInt64) {
	t.Helper()
	err := d.QueryRow(
		`SELECT op, name, active, parent_id
		   FROM programs_versions
		  WHERE entity_id = ?
		  ORDER BY valid_from DESC, id DESC
		  LIMIT 1`, entityID,
	).Scan(&op, &name, &active, &parentID)
	if err != nil {
		t.Fatalf("latestProgramVersion(%d): %v", entityID, err)
	}
	return
}

// TestCreateProgramVersioned: creating a child appends a create-op version row
// whose snapshot matches the live row (snapshot-from-live).
func TestCreateProgramVersioned(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	id, err := s.CreateProgram(mutCtx(), CreateProgramInput{
		ParentID: rootProgramID,
		Name:     "Youth Services",
	})
	if err != nil {
		t.Fatalf("CreateProgram: %v", err)
	}
	if id <= 0 {
		t.Fatalf("CreateProgram returned id %d, want positive", id)
	}

	testutil.AssertVersioned(t, d, "programs", id, "create")

	parent, name, active := getProgram(t, s, id)
	if !parent.Valid || parent.Int64 != rootProgramID {
		t.Errorf("live parent = %+v, want %d", parent, rootProgramID)
	}
	if name != "Youth Services" || active != 1 {
		t.Errorf("live row = (%q,active=%d), want (Youth Services,1)", name, active)
	}

	vOp, vName, vActive, vParent := latestProgramVersion(t, d, id)
	if vOp != "create" || vName != name || vActive != active ||
		vParent.Int64 != parent.Int64 || vParent.Valid != parent.Valid {
		t.Errorf("snapshot (%s,%q,%d,%+v) != live (%q,%d,%+v)",
			vOp, vName, vActive, vParent, name, active, parent)
	}
}

// TestCreateProgramRejectsSecondRoot: creating a program with no parent is
// rejected with a typed error BEFORE relying on the trigger, and writes no change.
func TestCreateProgramRejectsSecondRoot(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	before := countChanges(t, d)
	_, err := s.CreateProgram(mutCtx(), CreateProgramInput{
		ParentID: 0, // no parent -> would be a second root
		Name:     "Rogue Root",
	})
	if !errors.Is(err, ErrProgramSecondRoot) {
		t.Fatalf("CreateProgram(no parent): err = %v, want ErrProgramSecondRoot", err)
	}
	if n := countChanges(t, d); n != before {
		t.Errorf("changes count = %d, want %d (rejected create leaves no trace)", n, before)
	}
}

// TestCreateProgramRejectsMissingParent: a non-existent parent is a clean typed
// error and leaves no change-row trace.
func TestCreateProgramRejectsMissingParent(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	before := countChanges(t, d)
	_, err := s.CreateProgram(mutCtx(), CreateProgramInput{
		ParentID: 9999,
		Name:     "Orphan",
	})
	if !errors.Is(err, ErrProgramParentMissing) {
		t.Fatalf("CreateProgram(bad parent): err = %v, want ErrProgramParentMissing", err)
	}
	if n := countChanges(t, d); n != before {
		t.Errorf("changes count = %d, want %d (rejected create leaves no trace)", n, before)
	}
}

// TestProgramMoveRejectsCycle: moving a program under its own descendant (or
// itself) is rejected (ErrCycle) and leaves no change-row trace.
func TestProgramMoveRejectsCycle(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	// root -> a -> b. Moving a under b is a cycle.
	a, _ := s.CreateProgram(mutCtx(), CreateProgramInput{ParentID: rootProgramID, Name: "A"})
	b, _ := s.CreateProgram(mutCtx(), CreateProgramInput{ParentID: a, Name: "B"})

	before := countChanges(t, d)
	err := s.UpdateProgram(mutCtx(), a, UpdateProgramInput{ParentID: &b})
	if !errors.Is(err, ErrCycle) {
		t.Fatalf("move a under descendant b: err = %v, want ErrCycle", err)
	}
	if n := countChanges(t, d); n != before {
		t.Errorf("changes count = %d, want %d (rejected move leaves no trace)", n, before)
	}

	if err := s.UpdateProgram(mutCtx(), a, UpdateProgramInput{ParentID: &a}); !errors.Is(err, ErrCycle) {
		t.Errorf("move a under itself: err = %v, want ErrCycle", err)
	}
}

// TestProgramRootImmovable: the root program cannot be given a parent; it keeps
// NULL parent and the rejected call leaves no change-row trace. A rename of the
// root is still allowed.
func TestProgramRootImmovable(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	child, _ := s.CreateProgram(mutCtx(), CreateProgramInput{ParentID: rootProgramID, Name: "Child"})

	before := countChanges(t, d)
	err := s.UpdateProgram(mutCtx(), rootProgramID, UpdateProgramInput{ParentID: &child})
	if !errors.Is(err, ErrProgramRootImmovable) {
		t.Fatalf("give root a parent: err = %v, want ErrProgramRootImmovable", err)
	}
	if n := countChanges(t, d); n != before {
		t.Errorf("changes count = %d, want %d (rejected move leaves no trace)", n, before)
	}
	parent, _, _ := getProgram(t, s, rootProgramID)
	if parent.Valid {
		t.Errorf("root parent = %+v, want NULL", parent)
	}

	// The root can still be renamed (parent stays NULL).
	newName := "Programs"
	if err := s.UpdateProgram(mutCtx(), rootProgramID, UpdateProgramInput{Name: &newName}); err != nil {
		t.Fatalf("rename root program: %v", err)
	}
	testutil.AssertVersioned(t, d, "programs", rootProgramID, "update")
	parent, name, _ := getProgram(t, s, rootProgramID)
	if parent.Valid {
		t.Errorf("root parent = %+v after rename, want NULL", parent)
	}
	if name != "Programs" {
		t.Errorf("root name = %q, want Programs", name)
	}
}

// TestDeactivateProgramBlocksNewUse: deactivating a childless program sets
// active=0 and appends op='update' (NOT 'delete' -- history intact; the full
// split-based blocks-new-use assertion is p08). Here we assert the version row
// and active=0.
func TestDeactivateProgramBlocksNewUse(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	id, _ := s.CreateProgram(mutCtx(), CreateProgramInput{ParentID: rootProgramID, Name: "Doomed"})

	if err := s.DeactivateProgram(mutCtx(), id); err != nil {
		t.Fatalf("DeactivateProgram: %v", err)
	}

	testutil.AssertVersioned(t, d, "programs", id, "update")
	_, _, active := getProgram(t, s, id)
	if active != 0 {
		t.Errorf("live active = %d, want 0", active)
	}
	vOp, _, vActive, _ := latestProgramVersion(t, d, id)
	if vOp != "update" || vActive != 0 {
		t.Errorf("snapshot = (op=%s,active=%d), want (update,0)", vOp, vActive)
	}
}

// TestDeactivateProgramBlockedWithActiveChildren: deactivating a program with
// active children is rejected (ErrProgramHasActiveChildren) and leaves no trace.
func TestDeactivateProgramBlockedWithActiveChildren(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	parent, _ := s.CreateProgram(mutCtx(), CreateProgramInput{ParentID: rootProgramID, Name: "Parent"})
	_, _ = s.CreateProgram(mutCtx(), CreateProgramInput{ParentID: parent, Name: "Kid"})

	before := countChanges(t, d)
	err := s.DeactivateProgram(mutCtx(), parent)
	if !errors.Is(err, ErrProgramHasActiveChildren) {
		t.Fatalf("deactivate program with active child: err = %v, want ErrProgramHasActiveChildren", err)
	}
	if n := countChanges(t, d); n != before {
		t.Errorf("changes count = %d, want %d (rejected deactivate leaves no trace)", n, before)
	}
	_, _, active := getProgram(t, s, parent)
	if active != 1 {
		t.Errorf("parent active = %d, want 1 (unchanged)", active)
	}
}

// TestProgramTree: depth-first (pre-order) traversal, children ordered by
// sort_order then id. sort_order is made to DISAGREE with id order so the test
// distinguishes sort_order ordering from mere id ordering.
func TestProgramTree(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	// root
	//   A (sort 1)
	//     A1
	//   B (sort 0)  <- lower sort_order but created AFTER A, so id(B) > id(A)
	a, _ := s.CreateProgram(mutCtx(), CreateProgramInput{ParentID: rootProgramID, Name: "A", SortOrder: 1})
	a1, _ := s.CreateProgram(mutCtx(), CreateProgramInput{ParentID: a, Name: "A1"})
	b, _ := s.CreateProgram(mutCtx(), CreateProgramInput{ParentID: rootProgramID, Name: "B", SortOrder: 0})

	tree, err := s.ProgramTree(context.Background())
	if err != nil {
		t.Fatalf("ProgramTree: %v", err)
	}

	gotIDs := make([]int64, len(tree))
	for i, row := range tree {
		gotIDs[i] = row.ID
	}
	// Pre-order with B (sort 0) before A (sort 1): root, B, A, A1.
	want := []int64{rootProgramID, b, a, a1}
	if len(gotIDs) != len(want) {
		t.Fatalf("ProgramTree returned %v, want %v", gotIDs, want)
	}
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Fatalf("ProgramTree order = %v, want %v (depth-first, sort_order then id)", gotIDs, want)
		}
	}
}

// TestProgramPaths (p29.13): id -> dotted ancestor path over the program tree. The
// seeded root ("General") is its bare name; a child joins its ancestors with ".".
func TestProgramPaths(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	//   General(root) - Education - K12
	educ, _ := s.CreateProgram(mutCtx(), CreateProgramInput{ParentID: rootProgramID, Name: "Education"})
	k12, _ := s.CreateProgram(mutCtx(), CreateProgramInput{ParentID: educ, Name: "K12"})

	paths, err := s.ProgramPaths(context.Background())
	if err != nil {
		t.Fatalf("ProgramPaths: %v", err)
	}
	want := map[int64]string{
		rootProgramID: "General",
		educ:          "General.Education",
		k12:           "General.Education.K12",
	}
	for id, w := range want {
		if got := paths[id]; got != w {
			t.Errorf("ProgramPaths[%d] = %q, want %q", id, got, w)
		}
	}
}

// TestProgramDescendants: self + transitive closure. Self must be present; a
// sibling subtree and an ancestor must be excluded.
func TestProgramDescendants(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	//   root - A - A1 - A11
	//        - B
	a, _ := s.CreateProgram(mutCtx(), CreateProgramInput{ParentID: rootProgramID, Name: "A"})
	a1, _ := s.CreateProgram(mutCtx(), CreateProgramInput{ParentID: a, Name: "A1"})
	a11, _ := s.CreateProgram(mutCtx(), CreateProgramInput{ParentID: a1, Name: "A11"})
	b, _ := s.CreateProgram(mutCtx(), CreateProgramInput{ParentID: rootProgramID, Name: "B"})

	rows, err := s.ProgramDescendants(context.Background(), a)
	if err != nil {
		t.Fatalf("ProgramDescendants(a): %v", err)
	}
	got := make(map[int64]bool, len(rows))
	for _, r := range rows {
		got[r.ID] = true
	}
	for _, id := range []int64{a, a1, a11} {
		if !got[id] {
			t.Errorf("ProgramDescendants(a) missing %d (self+closure): got %v", id, got)
		}
	}
	if got[b] {
		t.Errorf("ProgramDescendants(a) includes sibling b=%d, want excluded", b)
	}
	if got[rootProgramID] {
		t.Errorf("ProgramDescendants(a) includes ancestor root, want excluded")
	}
	if len(rows) != 3 {
		t.Errorf("ProgramDescendants(a) size = %d, want 3 (a,a1,a11)", len(rows))
	}
}

// TestAccountDefaultProgramREOnly: a default_program_id set on an A/L/E account is
// rejected (ErrDefaultProgramNotRE, D24 -- meaningful only on R/E accounts); on an
// R/E account it is accepted and versioned, and survives an unrelated update
// (read-modify-write must carry it -- the accounts_versions default_program_id
// ripple). A rejected set leaves no change-row trace.
func TestAccountDefaultProgramREOnly(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	subA := newSub(t, s, rootID, "A")

	// Rejected on an asset account.
	rootProg := rootProgramID
	before := countChanges(t, d)
	_, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type:             "asset",
		DefaultCurrency:  "USD",
		Names:            enName("Cash"),
		Subsidiaries:     []int64{subA},
		DefaultProgramID: &rootProg,
	})
	if !errors.Is(err, ErrDefaultProgramNotRE) {
		t.Fatalf("default program on asset account: err = %v, want ErrDefaultProgramNotRE", err)
	}
	if n := countChanges(t, d); n != before {
		t.Errorf("changes count = %d, want %d (rejected create leaves no trace)", n, before)
	}

	// Accepted on a revenue account, and versioned.
	prog, _ := s.CreateProgram(mutCtx(), CreateProgramInput{ParentID: rootProgramID, Name: "Grants"})
	rev, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type:             "revenue",
		DefaultCurrency:  "USD",
		Names:            enName("Donations"),
		Subsidiaries:     []int64{subA},
		DefaultProgramID: &prog,
	})
	if err != nil {
		t.Fatalf("CreateAccount revenue with default program: %v", err)
	}
	testutil.AssertVersioned(t, d, "accounts", rev, "create")

	if got := accountDefaultProgram(t, d, rev); !got.Valid || got.Int64 != prog {
		t.Errorf("live default_program_id = %+v, want %d", got, prog)
	}
	// Snapshot must carry it too.
	if got := latestAccountVersionDefaultProgram(t, d, rev); !got.Valid || got.Int64 != prog {
		t.Errorf("snapshot default_program_id = %+v, want %d", got, prog)
	}

	// An unrelated update must NOT drop default_program_id (read-modify-write
	// carries cur.DefaultProgramID -- the silent-NULL regression guard).
	newSort := int64(7)
	if err := s.UpdateAccount(mutCtx(), rev, UpdateAccountInput{SortOrder: &newSort}); err != nil {
		t.Fatalf("UpdateAccount (unrelated field): %v", err)
	}
	if got := accountDefaultProgram(t, d, rev); !got.Valid || got.Int64 != prog {
		t.Errorf("default_program_id after unrelated update = %+v, want %d (must survive)", got, prog)
	}
	if got := latestAccountVersionDefaultProgram(t, d, rev); !got.Valid || got.Int64 != prog {
		t.Errorf("snapshot default_program_id after update = %+v, want %d", got, prog)
	}

	// Setting a default program on the revenue account via UpdateAccount to an
	// inactive program is rejected (must be active, D24).
	inactive, _ := s.CreateProgram(mutCtx(), CreateProgramInput{ParentID: rootProgramID, Name: "Old"})
	if err := s.DeactivateProgram(mutCtx(), inactive); err != nil {
		t.Fatalf("DeactivateProgram: %v", err)
	}
	before = countChanges(t, d)
	if err := s.UpdateAccount(mutCtx(), rev, UpdateAccountInput{DefaultProgramID: &inactive}); !errors.Is(err, ErrDefaultProgramInactive) {
		t.Fatalf("set inactive default program: err = %v, want ErrDefaultProgramInactive", err)
	}
	if n := countChanges(t, d); n != before {
		t.Errorf("changes count = %d, want %d (rejected update leaves no trace)", n, before)
	}

	// UpdateAccount also rejects a default program on a non-R/E account (the
	// reject holds on BOTH entry points, not just create).
	asset, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("Bank"), Subsidiaries: []int64{subA},
	})
	if err != nil {
		t.Fatalf("create asset account: %v", err)
	}
	before = countChanges(t, d)
	if err := s.UpdateAccount(mutCtx(), asset, UpdateAccountInput{DefaultProgramID: &prog}); !errors.Is(err, ErrDefaultProgramNotRE) {
		t.Fatalf("UpdateAccount default program on asset: err = %v, want ErrDefaultProgramNotRE", err)
	}
	if n := countChanges(t, d); n != before {
		t.Errorf("changes count = %d, want %d (rejected update leaves no trace)", n, before)
	}

	// A non-nil 0 CLEARS the default program (back to NULL) on the revenue acct.
	clear := int64(0)
	if err := s.UpdateAccount(mutCtx(), rev, UpdateAccountInput{DefaultProgramID: &clear}); err != nil {
		t.Fatalf("UpdateAccount clear default program: %v", err)
	}
	if got := accountDefaultProgram(t, d, rev); got.Valid {
		t.Errorf("default_program_id after clear = %+v, want NULL", got)
	}
}

// accountDefaultProgram reads an account's live default_program_id directly.
func accountDefaultProgram(t *testing.T, d *sql.DB, accountID int64) sql.NullInt64 {
	t.Helper()
	var v sql.NullInt64
	if err := d.QueryRow(`SELECT default_program_id FROM accounts WHERE id = ?`, accountID).Scan(&v); err != nil {
		t.Fatalf("accountDefaultProgram(%d): %v", accountID, err)
	}
	return v
}

// latestAccountVersionDefaultProgram reads the newest accounts_versions snapshot's
// default_program_id for an account (proves the ripple carried it).
func latestAccountVersionDefaultProgram(t *testing.T, d *sql.DB, accountID int64) sql.NullInt64 {
	t.Helper()
	var v sql.NullInt64
	if err := d.QueryRow(
		`SELECT default_program_id FROM accounts_versions
		  WHERE entity_id = ? ORDER BY valid_from DESC, id DESC LIMIT 1`, accountID,
	).Scan(&v); err != nil {
		t.Fatalf("latestAccountVersionDefaultProgram(%d): %v", accountID, err)
	}
	return v
}
