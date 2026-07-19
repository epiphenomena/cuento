package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"cuento/internal/ids"
	"cuento/internal/testutil"
)

// mutCtx is the actor-bearing context every mutating call in these tests uses
// (AGENTS: contexts carry the actor). System user id 1 exists from the p02.1 seed.
func mutCtx() context.Context {
	return WithActor(context.Background(), Actor{ID: 1})
}

// rootID is the seeded root subsidiary's id (p04.1 seeds id 1, NULL parent).
const rootID ids.SubsidiaryID = 1

// getSub reads a subsidiary's current live row directly through the store's
// GetSubsidiary read method (sqlc, rule 6) for assertions.
func getSub(t *testing.T, s *Store, id ids.SubsidiaryID) (parentID sql.NullInt64, name, baseCcy string, active int64) {
	t.Helper()
	row, err := s.GetSubsidiary(context.Background(), id)
	if err != nil {
		t.Fatalf("GetSubsidiary(%d): %v", id, err)
	}
	return row.ParentID, row.Name, row.BaseCurrency, row.Active
}

// latestVersion reads the newest subsidiaries_versions snapshot for an entity so
// tests can prove the snapshot mirrors the live row's NEW values. Raw SQL is
// acceptable in a test (as AssertVersioned itself does).
func latestVersion(t *testing.T, d *sql.DB, entityID ids.SubsidiaryID) (op, name, baseCcy string, active int64, parentID sql.NullInt64) {
	t.Helper()
	err := d.QueryRow(
		`SELECT op, name, base_currency, active, parent_id
		   FROM subsidiaries_versions
		  WHERE entity_id = ?
		  ORDER BY valid_from DESC, id DESC
		  LIMIT 1`, entityID,
	).Scan(&op, &name, &baseCcy, &active, &parentID)
	if err != nil {
		t.Fatalf("latestVersion(%d): %v", entityID, err)
	}
	return
}

// TestCreateSubsidiaryVersioned: creating a child appends a create-op version row
// and the snapshot matches the live row byte-for-byte (snapshot-from-live).
func TestCreateSubsidiaryVersioned(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	id, err := s.CreateSubsidiary(mutCtx(), CreateSubsidiaryInput{
		ParentID:     rootID,
		Name:         "West Branch",
		BaseCurrency: "USD",
		SortOrder:    0,
	})
	if err != nil {
		t.Fatalf("CreateSubsidiary: %v", err)
	}
	if id <= 0 {
		t.Fatalf("CreateSubsidiary returned id %d, want positive", id)
	}

	testutil.AssertVersioned(t, d, "subsidiaries", int64(id), "create")

	// Live row.
	parent, name, ccy, active := getSub(t, s, id)
	if !parent.Valid || parent.Int64 != int64(rootID) {
		t.Errorf("live parent = %+v, want %d", parent, rootID)
	}
	if name != "West Branch" || ccy != "USD" || active != 1 {
		t.Errorf("live row = (%q,%q,active=%d), want (West Branch,USD,1)", name, ccy, active)
	}

	// Snapshot mirrors the live row exactly.
	vOp, vName, vCcy, vActive, vParent := latestVersion(t, d, id)
	if vOp != "create" || vName != name || vCcy != ccy || vActive != active ||
		vParent.Int64 != parent.Int64 || vParent.Valid != parent.Valid {
		t.Errorf("snapshot (%s,%q,%q,%d,%+v) != live (%q,%q,%d,%+v)",
			vOp, vName, vCcy, vActive, vParent, name, ccy, active, parent)
	}
}

// TestCreateRejectsSecondRoot: creating a subsidiary with no parent is rejected
// with a typed error BEFORE relying on the schema trigger, and writes no change.
func TestCreateRejectsSecondRoot(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	before := countChanges(t, d)
	_, err := s.CreateSubsidiary(mutCtx(), CreateSubsidiaryInput{
		ParentID:     0, // no parent → would be a second root
		Name:         "Rogue Root",
		BaseCurrency: "USD",
	})
	if !errors.Is(err, ErrSecondRoot) {
		t.Fatalf("CreateSubsidiary(no parent): err = %v, want ErrSecondRoot", err)
	}
	if n := countChanges(t, d); n != before {
		t.Errorf("changes count = %d, want %d (rejected create leaves no trace)", n, before)
	}
}

// TestCreateRejectsMissingParent: a non-existent parent is a clean typed error.
func TestCreateRejectsMissingParent(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	before := countChanges(t, d)
	_, err := s.CreateSubsidiary(mutCtx(), CreateSubsidiaryInput{
		ParentID:     9999,
		Name:         "Orphan",
		BaseCurrency: "USD",
	})
	if !errors.Is(err, ErrParentMissing) {
		t.Fatalf("CreateSubsidiary(bad parent): err = %v, want ErrParentMissing", err)
	}
	if n := countChanges(t, d); n != before {
		t.Errorf("changes count = %d, want %d (rejected create leaves no trace)", n, before)
	}
}

// TestUpdateSubsidiaryVersioned: a rename+base-currency change appends an
// update-op version whose snapshot reflects the NEW values (proves the version
// append runs AFTER the live write).
func TestUpdateSubsidiaryVersioned(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	id, err := s.CreateSubsidiary(mutCtx(), CreateSubsidiaryInput{
		ParentID: rootID, Name: "Old Name", BaseCurrency: "USD",
	})
	if err != nil {
		t.Fatalf("CreateSubsidiary: %v", err)
	}

	newName := "New Name"
	newCcy := "EUR"
	if err := s.UpdateSubsidiary(mutCtx(), id, UpdateSubsidiaryInput{
		Name:         &newName,
		BaseCurrency: &newCcy,
	}); err != nil {
		t.Fatalf("UpdateSubsidiary: %v", err)
	}

	testutil.AssertVersioned(t, d, "subsidiaries", int64(id), "update")

	_, name, ccy, _ := getSub(t, s, id)
	if name != "New Name" || ccy != "EUR" {
		t.Errorf("live row after update = (%q,%q), want (New Name,EUR)", name, ccy)
	}
	vOp, vName, vCcy, _, _ := latestVersion(t, d, id)
	if vOp != "update" || vName != "New Name" || vCcy != "EUR" {
		t.Errorf("snapshot = (%s,%q,%q), want (update,New Name,EUR)", vOp, vName, vCcy)
	}
}

// TestUpdateMoveVersioned: a valid move (reparent) appends update with the new
// parent in the snapshot.
func TestUpdateMoveVersioned(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	a, _ := s.CreateSubsidiary(mutCtx(), CreateSubsidiaryInput{ParentID: rootID, Name: "A", BaseCurrency: "USD"})
	b, _ := s.CreateSubsidiary(mutCtx(), CreateSubsidiaryInput{ParentID: rootID, Name: "B", BaseCurrency: "USD"})

	if err := s.UpdateSubsidiary(mutCtx(), b, UpdateSubsidiaryInput{ParentID: &a}); err != nil {
		t.Fatalf("UpdateSubsidiary move: %v", err)
	}

	testutil.AssertVersioned(t, d, "subsidiaries", int64(b), "update")
	parent, _, _, _ := getSub(t, s, b)
	if !parent.Valid || parent.Int64 != int64(a) {
		t.Errorf("live parent of B = %+v, want %d", parent, a)
	}
	_, _, _, _, vParent := latestVersion(t, d, b)
	if !vParent.Valid || vParent.Int64 != int64(a) {
		t.Errorf("snapshot parent of B = %+v, want %d", vParent, a)
	}
}

// TestDeactivateVersioned: deactivating a childless sub sets active=0 and appends
// op='update' (NOT 'delete' — the entity still exists).
func TestDeactivateVersioned(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	id, _ := s.CreateSubsidiary(mutCtx(), CreateSubsidiaryInput{ParentID: rootID, Name: "Doomed", BaseCurrency: "USD"})

	if err := s.DeactivateSubsidiary(mutCtx(), id); err != nil {
		t.Fatalf("DeactivateSubsidiary: %v", err)
	}

	testutil.AssertVersioned(t, d, "subsidiaries", int64(id), "update")
	_, _, _, active := getSub(t, s, id)
	if active != 0 {
		t.Errorf("live active = %d, want 0", active)
	}
	vOp, _, _, vActive, _ := latestVersion(t, d, id)
	if vOp != "update" || vActive != 0 {
		t.Errorf("snapshot = (op=%s,active=%d), want (update,0)", vOp, vActive)
	}
}

// TestMoveRejectsCycle: moving a subsidiary under its own descendant is rejected
// (typed ErrCycle) and leaves no change-row trace (validate-then-commit guard).
func TestMoveRejectsCycle(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	// root → a → b. Moving a under b is a cycle.
	a, _ := s.CreateSubsidiary(mutCtx(), CreateSubsidiaryInput{ParentID: rootID, Name: "A", BaseCurrency: "USD"})
	b, _ := s.CreateSubsidiary(mutCtx(), CreateSubsidiaryInput{ParentID: a, Name: "B", BaseCurrency: "USD"})

	before := countChanges(t, d)
	err := s.UpdateSubsidiary(mutCtx(), a, UpdateSubsidiaryInput{ParentID: &b})
	if !errors.Is(err, ErrCycle) {
		t.Fatalf("move a under descendant b: err = %v, want ErrCycle", err)
	}
	if n := countChanges(t, d); n != before {
		t.Errorf("changes count = %d, want %d (rejected move leaves no trace)", n, before)
	}

	// Also reject moving a node under itself.
	if err := s.UpdateSubsidiary(mutCtx(), a, UpdateSubsidiaryInput{ParentID: &a}); !errors.Is(err, ErrCycle) {
		t.Errorf("move a under itself: err = %v, want ErrCycle", err)
	}
}

// TestRootImmovable: the root cannot be given a parent; it keeps NULL parent, and
// the rejected call leaves no change-row trace.
func TestRootImmovable(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	child, _ := s.CreateSubsidiary(mutCtx(), CreateSubsidiaryInput{ParentID: rootID, Name: "Child", BaseCurrency: "USD"})

	before := countChanges(t, d)
	err := s.UpdateSubsidiary(mutCtx(), rootID, UpdateSubsidiaryInput{ParentID: &child})
	if !errors.Is(err, ErrRootImmovable) {
		t.Fatalf("give root a parent: err = %v, want ErrRootImmovable", err)
	}
	if n := countChanges(t, d); n != before {
		t.Errorf("changes count = %d, want %d (rejected move leaves no trace)", n, before)
	}
	parent, _, _, _ := getSub(t, s, rootID)
	if parent.Valid {
		t.Errorf("root parent = %+v, want NULL", parent)
	}
}

// TestRootRenameable: the root can still be renamed / rebased (parent stays NULL);
// UpdateSubsidiary must not reject legitimate root edits.
func TestRootRenameable(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	newName := "HQ"
	if err := s.UpdateSubsidiary(mutCtx(), rootID, UpdateSubsidiaryInput{Name: &newName}); err != nil {
		t.Fatalf("rename root: %v", err)
	}
	testutil.AssertVersioned(t, d, "subsidiaries", int64(rootID), "update")
	parent, name, _, _ := getSub(t, s, rootID)
	if parent.Valid {
		t.Errorf("root parent = %+v after rename, want NULL", parent)
	}
	if name != "HQ" {
		t.Errorf("root name = %q, want HQ", name)
	}
}

// TestDeactivateBlockedWithActiveChildren: deactivating a sub with active children
// is rejected (typed ErrHasActiveChildren) and leaves no change-row trace.
func TestDeactivateBlockedWithActiveChildren(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	parent, _ := s.CreateSubsidiary(mutCtx(), CreateSubsidiaryInput{ParentID: rootID, Name: "Parent", BaseCurrency: "USD"})
	_, _ = s.CreateSubsidiary(mutCtx(), CreateSubsidiaryInput{ParentID: parent, Name: "Kid", BaseCurrency: "USD"})

	before := countChanges(t, d)
	err := s.DeactivateSubsidiary(mutCtx(), parent)
	if !errors.Is(err, ErrHasActiveChildren) {
		t.Fatalf("deactivate parent with active child: err = %v, want ErrHasActiveChildren", err)
	}
	if n := countChanges(t, d); n != before {
		t.Errorf("changes count = %d, want %d (rejected deactivate leaves no trace)", n, before)
	}
	_, _, _, active := getSub(t, s, parent)
	if active != 1 {
		t.Errorf("parent active = %d, want 1 (unchanged)", active)
	}
}

// TestSubTree: depth-first (pre-order) traversal, children ordered by sort_order
// then id. sort_order is deliberately made to DISAGREE with id order so the test
// distinguishes sort_order ordering from mere id ordering.
func TestSubTree(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	// Build (root already exists):
	//   root
	//   ├── A (sort 1)
	//   │   └── A1
	//   └── B (sort 0)   ← lower sort_order but created AFTER A, so id(B) > id(A)
	a, _ := s.CreateSubsidiary(mutCtx(), CreateSubsidiaryInput{ParentID: rootID, Name: "A", BaseCurrency: "USD", SortOrder: 1})
	a1, _ := s.CreateSubsidiary(mutCtx(), CreateSubsidiaryInput{ParentID: a, Name: "A1", BaseCurrency: "USD"})
	b, _ := s.CreateSubsidiary(mutCtx(), CreateSubsidiaryInput{ParentID: rootID, Name: "B", BaseCurrency: "USD", SortOrder: 0})

	tree, err := s.SubTree(context.Background())
	if err != nil {
		t.Fatalf("SubTree: %v", err)
	}

	gotIDs := make([]ids.SubsidiaryID, len(tree))
	for i, row := range tree {
		gotIDs[i] = row.ID
	}
	// Pre-order with B (sort 0) before A (sort 1): root, B, A, A1.
	want := []ids.SubsidiaryID{rootID, b, a, a1}
	if len(gotIDs) != len(want) {
		t.Fatalf("SubTree returned %v, want %v", gotIDs, want)
	}
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Fatalf("SubTree order = %v, want %v (depth-first, sort_order then id)", gotIDs, want)
		}
	}
}

// TestDescendants: self + transitive closure. Self must be present (the cycle
// check depends on it); a sibling subtree must be excluded.
func TestDescendants(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	//   root ── A ── A1 ── A11
	//        └─ B
	a, _ := s.CreateSubsidiary(mutCtx(), CreateSubsidiaryInput{ParentID: rootID, Name: "A", BaseCurrency: "USD"})
	a1, _ := s.CreateSubsidiary(mutCtx(), CreateSubsidiaryInput{ParentID: a, Name: "A1", BaseCurrency: "USD"})
	a11, _ := s.CreateSubsidiary(mutCtx(), CreateSubsidiaryInput{ParentID: a1, Name: "A11", BaseCurrency: "USD"})
	b, _ := s.CreateSubsidiary(mutCtx(), CreateSubsidiaryInput{ParentID: rootID, Name: "B", BaseCurrency: "USD"})

	rows, err := s.Descendants(context.Background(), a)
	if err != nil {
		t.Fatalf("Descendants(a): %v", err)
	}
	got := make(map[ids.SubsidiaryID]bool, len(rows))
	for _, r := range rows {
		got[r.ID] = true
	}
	for _, id := range []ids.SubsidiaryID{a, a1, a11} {
		if !got[id] {
			t.Errorf("Descendants(a) missing %d (self+closure): got %v", id, got)
		}
	}
	if got[b] {
		t.Errorf("Descendants(a) includes sibling b=%d, want excluded", b)
	}
	if got[rootID] {
		t.Errorf("Descendants(a) includes ancestor root, want excluded")
	}
	if len(rows) != 3 {
		t.Errorf("Descendants(a) size = %d, want 3 (a,a1,a11)", len(rows))
	}
}
