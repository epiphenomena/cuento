package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"cuento/internal/testutil"
)

// newSub is a tiny helper: create a child subsidiary under a parent and return
// its id, failing the test on error. Accounts map to subsidiaries created via
// CreateSubsidiary (p04.2); the seeded root subsidiary id 1 exists.
func newSub(t *testing.T, s *Store, parent int64, name string) int64 {
	t.Helper()
	id, err := s.CreateSubsidiary(mutCtx(), CreateSubsidiaryInput{
		ParentID: parent, Name: name, BaseCurrency: "USD",
	})
	if err != nil {
		t.Fatalf("CreateSubsidiary(%s): %v", name, err)
	}
	return id
}

// enName is the minimal name set (en required) most account tests use.
func enName(n string) map[string]string { return map[string]string{"en": n} }

// getAccount reads an account's current live row directly for assertions.
func getAccount(t *testing.T, s *Store, id int64) (parent sql.NullInt64, typ, ccy string, active int64) {
	t.Helper()
	row, err := s.GetAccount(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAccount(%d): %v", id, err)
	}
	return row.ParentID, row.Type, row.DefaultCurrency, row.Active
}

// accountSubs returns the current subsidiary id set mapped to an account.
func accountSubs(t *testing.T, d *sql.DB, accountID int64) map[int64]bool {
	t.Helper()
	rows, err := d.Query(`SELECT subsidiary_id FROM account_subsidiaries WHERE account_id = ?`, accountID)
	if err != nil {
		t.Fatalf("accountSubs(%d): %v", accountID, err)
	}
	defer func() { _ = rows.Close() }()
	got := map[int64]bool{}
	for rows.Next() {
		var sid int64
		if err := rows.Scan(&sid); err != nil {
			t.Fatalf("scan sub: %v", err)
		}
		got[sid] = true
	}
	return got
}

// TestCreateAccountVersioned: creating an account under one change versions the
// account row, each name row, and each subsidiary-map row, INCLUDING the
// membership auto-propagated up the (ACCOUNT) ancestor chain (D18 superset). The
// propagation walks the account tree, not the subsidiary tree: assigning subA to
// a child grows the PARENT ACCOUNT's set (not subsidiary A's own ancestors).
func TestCreateAccountVersioned(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	subA := newSub(t, s, rootID, "A")

	// Parent account maps {root}; child (under parent) maps {subA}. Creating the
	// child must propagate subA up to the parent account (superset invariant).
	parent, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("Assets"), Subsidiaries: []int64{rootID},
	})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	id, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		ParentID:        &parent,
		Type:            "asset",
		DefaultCurrency: "USD",
		Names:           map[string]string{"en": "Cash", "es": "Efectivo"},
		Subsidiaries:    []int64{subA},
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if id <= 0 {
		t.Fatalf("CreateAccount returned id %d, want positive", id)
	}

	// Account row versioned op=create.
	testutil.AssertVersioned(t, d, "accounts", id, "create")
	// Both names versioned op=create.
	testutil.AssertVersionedName(t, d, id, "en", "create")
	testutil.AssertVersionedName(t, d, id, "es", "create")
	// The child's own membership versioned create.
	testutil.AssertVersionedSub(t, d, id, subA, "create")
	// The propagated PARENT-ACCOUNT membership versioned create.
	testutil.AssertVersionedSub(t, d, parent, subA, "create")

	// Live memberships: the child maps subA; the parent now also maps subA.
	if !accountSubs(t, d, id)[subA] {
		t.Errorf("child %d missing subA after create; got %v", id, accountSubs(t, d, id))
	}
	if !accountSubs(t, d, parent)[subA] {
		t.Errorf("parent %d missing propagated subA; got %v", parent, accountSubs(t, d, parent))
	}
}

// TestCreateRequiresAtLeastOneSub: an account with no subsidiaries is rejected
// with ErrNoSubsidiary and leaves no change-row trace.
func TestCreateRequiresAtLeastOneSub(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	before := countChanges(t, d)
	_, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type:            "asset",
		DefaultCurrency: "USD",
		Names:           enName("Cash"),
		Subsidiaries:    nil,
	})
	if !errors.Is(err, ErrNoSubsidiary) {
		t.Fatalf("CreateAccount(no subs): err = %v, want ErrNoSubsidiary", err)
	}
	if n := countChanges(t, d); n != before {
		t.Errorf("changes count = %d, want %d (rejected create leaves no trace)", n, before)
	}
}

// TestCreateRequiresEnName: at least one name, and en is required.
func TestCreateRequiresEnName(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	before := countChanges(t, d)
	_, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type:            "asset",
		DefaultCurrency: "USD",
		Names:           map[string]string{"es": "Efectivo"}, // no en
		Subsidiaries:    []int64{rootID},
	})
	if !errors.Is(err, ErrNameRequired) {
		t.Fatalf("CreateAccount(no en name): err = %v, want ErrNameRequired", err)
	}
	if n := countChanges(t, d); n != before {
		t.Errorf("changes count = %d, want %d (rejected create leaves no trace)", n, before)
	}
}

// TestAccountMoveRejectsCycle: moving an account under its own descendant is
// rejected (ErrCycle) and leaves no change-row trace. (Named to avoid colliding
// with the subsidiary TestMoveRejectsCycle in the same package; the PLAN lists
// both under the generic name.)
func TestAccountMoveRejectsCycle(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	// asset parent P -> child C (both map to root).
	p, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("P"), Subsidiaries: []int64{rootID},
	})
	if err != nil {
		t.Fatalf("create P: %v", err)
	}
	c, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		ParentID: &p, Type: "asset", DefaultCurrency: "USD", Names: enName("C"), Subsidiaries: []int64{rootID},
	})
	if err != nil {
		t.Fatalf("create C: %v", err)
	}

	before := countChanges(t, d)
	err = s.UpdateAccount(mutCtx(), p, UpdateAccountInput{ParentID: ptr(c)})
	if !errors.Is(err, ErrCycle) {
		t.Fatalf("move P under descendant C: err = %v, want ErrCycle", err)
	}
	if n := countChanges(t, d); n != before {
		t.Errorf("changes count = %d, want %d (rejected move leaves no trace)", n, before)
	}

	// Move under self is also a cycle.
	if err := s.UpdateAccount(mutCtx(), p, UpdateAccountInput{ParentID: ptr(p)}); !errors.Is(err, ErrCycle) {
		t.Errorf("move P under itself: err = %v, want ErrCycle", err)
	}
}

// TestMoveRejectsCrossTypeClass: an asset account cannot move under a revenue
// parent (D11: A/L/E children must match parent type). Typed ErrCrossTypeClass,
// no trace.
func TestMoveRejectsCrossTypeClass(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	asset, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("Cash"), Subsidiaries: []int64{rootID},
	})
	if err != nil {
		t.Fatalf("create asset: %v", err)
	}
	rev, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "revenue", DefaultCurrency: "USD", Names: enName("Donations"), Subsidiaries: []int64{rootID},
	})
	if err != nil {
		t.Fatalf("create revenue: %v", err)
	}

	before := countChanges(t, d)
	err = s.UpdateAccount(mutCtx(), asset, UpdateAccountInput{ParentID: ptr(rev)})
	if !errors.Is(err, ErrCrossTypeClass) {
		t.Fatalf("move asset under revenue: err = %v, want ErrCrossTypeClass", err)
	}
	if n := countChanges(t, d); n != before {
		t.Errorf("changes count = %d, want %d (rejected move leaves no trace)", n, before)
	}
}

// TestMoveRejectsSubMismatch: a move is rejected when the new parent's subsidiary
// set does not cover the moving account's set (D18). Typed ErrSubMismatch, no
// trace.
func TestMoveRejectsSubMismatch(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	// Two independent subsidiaries under root.
	subA := newSub(t, s, rootID, "A")
	subB := newSub(t, s, rootID, "B")

	// mover maps to {A, root} (via propagation). newParent maps to {B, root}.
	// newParent's set does NOT cover mover's {A} -> mismatch.
	newParent, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("PB"), Subsidiaries: []int64{subB},
	})
	if err != nil {
		t.Fatalf("create newParent: %v", err)
	}
	mover, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("MA"), Subsidiaries: []int64{subA},
	})
	if err != nil {
		t.Fatalf("create mover: %v", err)
	}

	before := countChanges(t, d)
	err = s.UpdateAccount(mutCtx(), mover, UpdateAccountInput{ParentID: ptr(newParent)})
	if !errors.Is(err, ErrSubMismatch) {
		t.Fatalf("move under sub-incompatible parent: err = %v, want ErrSubMismatch", err)
	}
	if n := countChanges(t, d); n != before {
		t.Errorf("changes count = %d, want %d (rejected move leaves no trace)", n, before)
	}
}

// TestAssignSubPropagatesToAncestors: adding subsidiary S to a leaf account
// silently adds S to every ancestor account up the chain (D18). Each newly-added
// ancestor membership is versioned op=create.
func TestAssignSubPropagatesToAncestors(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	// Subsidiary tree root -> A -> A1, and a second branch root -> B.
	subA := newSub(t, s, rootID, "A")
	subA1 := newSub(t, s, subA, "A1")

	// Account tree: gp (maps root) -> parent (maps root) -> leaf (maps root).
	gp, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("GP"), Subsidiaries: []int64{rootID},
	})
	if err != nil {
		t.Fatalf("create gp: %v", err)
	}
	parent, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		ParentID: &gp, Type: "asset", DefaultCurrency: "USD", Names: enName("Parent"), Subsidiaries: []int64{rootID},
	})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	leaf, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		ParentID: &parent, Type: "asset", DefaultCurrency: "USD", Names: enName("Leaf"), Subsidiaries: []int64{rootID},
	})
	if err != nil {
		t.Fatalf("create leaf: %v", err)
	}

	// Now add subA1 to the leaf. Propagation walks the ACCOUNT tree (not the
	// subsidiary tree): the leaf's ancestor accounts (parent, gp) must also gain
	// subA1 to keep parent-set superset-of union-of-children (D18). Subsidiary A
	// (A1's own tree-ancestor) is NOT auto-added -- report scoping consolidates
	// descendants at read time.
	_ = subA
	if err := s.SetAccountSubsidiaries(mutCtx(), leaf, []int64{rootID, subA1}); err != nil {
		t.Fatalf("SetAccountSubsidiaries: %v", err)
	}

	for _, acct := range []int64{leaf, parent, gp} {
		subs := accountSubs(t, d, acct)
		if !subs[subA1] {
			t.Errorf("account %d missing propagated sub A1; got %v", acct, subs)
		}
	}
	// Each ancestor gained the membership -> versioned create for the added ones.
	testutil.AssertVersionedSub(t, d, parent, subA1, "create")
	testutil.AssertVersionedSub(t, d, gp, subA1, "create")
}

// TestRemoveSubBlockedByChildOrSplits: removing a subsidiary from an account is
// blocked while a CHILD account still maps it (ErrSubInUseByChild). The split-
// usage half of this guard lands in p08 (splits table does not exist yet) and is
// deliberately deferred; see the TODO(p08) tag in SetAccountSubsidiaries.
func TestRemoveSubBlockedByChildOrSplits(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	subA := newSub(t, s, rootID, "A")

	// parent maps {A, root}; child maps {A, root} (child needs A too).
	parent, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("Parent"), Subsidiaries: []int64{subA},
	})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		ParentID: &parent, Type: "asset", DefaultCurrency: "USD", Names: enName("Child"), Subsidiaries: []int64{subA},
	})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	_ = child

	// Removing A from the parent must be blocked: the child still maps A.
	before := countChanges(t, d)
	err = s.SetAccountSubsidiaries(mutCtx(), parent, []int64{rootID})
	if !errors.Is(err, ErrSubInUseByChild) {
		t.Fatalf("remove sub still used by child: err = %v, want ErrSubInUseByChild", err)
	}
	if n := countChanges(t, d); n != before {
		t.Errorf("changes count = %d, want %d (rejected remove leaves no trace)", n, before)
	}
	// Parent still maps A.
	if !accountSubs(t, d, parent)[subA] {
		t.Errorf("parent lost sub A despite blocked removal")
	}

	// TODO(p08): once splits exist, removing S must ALSO be blocked while splits
	// reference the account in subsidiary S. That half of ErrSubInUseByChild's
	// guard is intentionally not implemented in p05.2 (no splits table yet).
}

// TestRemoveSubSucceedsWhenUnused: removing a subsidiary with no child using it
// succeeds, versions op=delete for that membership, and the live row is gone.
func TestRemoveSubSucceedsWhenUnused(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	subA := newSub(t, s, rootID, "A")
	acct, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("Solo"), Subsidiaries: []int64{subA},
	})
	if err != nil {
		t.Fatalf("create acct: %v", err)
	}
	// Currently maps {A} only (no account parent, so no propagation). Set to
	// {root}: A is removed (no children use it), root is added.
	if err := s.SetAccountSubsidiaries(mutCtx(), acct, []int64{rootID}); err != nil {
		t.Fatalf("SetAccountSubsidiaries remove: %v", err)
	}
	if accountSubs(t, d, acct)[subA] {
		t.Errorf("account still maps A after removal")
	}
	// The removal is versioned op=delete (snapshot captured before the live delete).
	testutil.AssertVersionedSub(t, d, acct, subA, "delete")
}

// TestDeactivate: DeactivateAccount sets active=0 and appends op='update' (NOT
// 'delete' — the entity persists).
func TestDeactivate(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	acct, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("Doomed"), Subsidiaries: []int64{rootID},
	})
	if err != nil {
		t.Fatalf("create acct: %v", err)
	}
	if err := s.DeactivateAccount(mutCtx(), acct); err != nil {
		t.Fatalf("DeactivateAccount: %v", err)
	}
	_, _, _, active := getAccount(t, s, acct)
	if active != 0 {
		t.Errorf("live active = %d, want 0", active)
	}
	testutil.AssertVersioned(t, d, "accounts", acct, "update")
}

// TestTreeOrdering: Tree returns accounts in tree (pre-order) order, resolving
// the requested lang's name via a plain join (empty when absent; en->any fallback
// is p05.3 and NOT built here).
func TestTreeOrdering(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	// asset A (sort 1) with child A1; asset B (sort 0). Pre-order with sort_order
	// then id: B(0) before A(1); A1 nests under A.
	a, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("A"), Subsidiaries: []int64{rootID}, SortOrder: 1,
	})
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	a1, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		ParentID: &a, Type: "asset", DefaultCurrency: "USD", Names: enName("A1"), Subsidiaries: []int64{rootID},
	})
	if err != nil {
		t.Fatalf("create A1: %v", err)
	}
	b, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("B"), Subsidiaries: []int64{rootID}, SortOrder: 0,
	})
	if err != nil {
		t.Fatalf("create B: %v", err)
	}

	rows, err := s.Tree(context.Background(), "en", nil)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	got := make([]int64, len(rows))
	for i, r := range rows {
		got[i] = r.ID
	}
	want := []int64{b, a, a1}
	if len(got) != len(want) {
		t.Fatalf("Tree order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Tree order = %v, want %v (pre-order, sort_order then id)", got, want)
		}
	}
	// Name resolved for lang en.
	for _, r := range rows {
		if r.ID == b && r.Name != "B" {
			t.Errorf("Tree name for B = %q, want B", r.Name)
		}
	}
}

// TestTreeSubsidiaryFilter: Tree filtered to a subsidiary returns only accounts
// mapped to it.
func TestTreeSubsidiaryFilter(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	subA := newSub(t, s, rootID, "A")
	subB := newSub(t, s, rootID, "B")

	inA, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("InA"), Subsidiaries: []int64{subA},
	})
	if err != nil {
		t.Fatalf("create inA: %v", err)
	}
	inB, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("InB"), Subsidiaries: []int64{subB},
	})
	if err != nil {
		t.Fatalf("create inB: %v", err)
	}

	rows, err := s.Tree(context.Background(), "en", &subA)
	if err != nil {
		t.Fatalf("Tree(subFilter A): %v", err)
	}
	got := map[int64]bool{}
	for _, r := range rows {
		got[r.ID] = true
	}
	if !got[inA] {
		t.Errorf("Tree filtered to A missing inA")
	}
	if got[inB] {
		t.Errorf("Tree filtered to A includes inB (mapped only to B)")
	}
}

// TestAccountNameAsOf: rename a name, then query the OLD name as of an earlier
// time via the versions table. Proves point-in-time works on the composite
// (account_id, lang) key.
func TestAccountNameAsOf(t *testing.T) {
	d := testutil.NewDB(t)

	// A mutable clock so the two writes get distinct valid_from timestamps.
	clk := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := New(d, WithClock(func() time.Time { return clk }))

	acct, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("Old Name"), Subsidiaries: []int64{rootID},
	})
	if err != nil {
		t.Fatalf("create acct: %v", err)
	}
	tOld := clk // timestamp at which the name is still "Old Name"

	// Advance the clock and rename.
	clk = clk.Add(time.Hour)
	if err := s.SetAccountName(mutCtx(), acct, "en", "New Name"); err != nil {
		t.Fatalf("SetAccountName: %v", err)
	}
	testutil.AssertVersionedName(t, d, acct, "en", "update")

	// Current name is "New Name".
	var cur string
	if err := d.QueryRow(`SELECT name FROM account_names WHERE account_id=? AND lang='en'`, acct).Scan(&cur); err != nil {
		t.Fatalf("read current name: %v", err)
	}
	if cur != "New Name" {
		t.Errorf("current name = %q, want New Name", cur)
	}

	// As-of tOld: the latest version row with valid_from <= tOld, not op=delete.
	asof := nameAsOf(t, d, acct, "en", tOld.Format(time.RFC3339Nano))
	if asof != "Old Name" {
		t.Errorf("name as-of %v = %q, want Old Name", tOld, asof)
	}
}

// nameAsOf reconstructs an account name at time T from account_names_versions
// (D4): the row with the greatest valid_from <= at for that (account_id, lang),
// excluded if op='delete'. Raw SQL is fine in a test.
func nameAsOf(t *testing.T, d *sql.DB, accountID int64, lang, at string) string {
	t.Helper()
	var name, op string
	err := d.QueryRow(
		`SELECT name, op FROM account_names_versions
		  WHERE entity_id = ? AND lang = ? AND valid_from <= ?
		  ORDER BY valid_from DESC, id DESC LIMIT 1`,
		accountID, lang, at,
	).Scan(&name, &op)
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("nameAsOf: no version <= %s for (%d,%s)", at, accountID, lang)
	}
	if err != nil {
		t.Fatalf("nameAsOf: %v", err)
	}
	if op == "delete" {
		return ""
	}
	return name
}

// TestEffective990Inherited: a form990_code set on a parent resolves for all
// descendants; a child's OWN code wins over the inherited one (D25).
func TestEffective990Inherited(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	// Expense parent with code IX.16 (Occupancy). Child with no code inherits it;
	// grandchild with its own code IX.17 (Travel) wins.
	code := "IX.16"
	parent, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "expense", DefaultCurrency: "USD", Names: enName("Occupancy"),
		Subsidiaries: []int64{rootID}, Form990Code: &code,
	})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		ParentID: &parent, Type: "expense", DefaultCurrency: "USD", Names: enName("Rent"),
		Subsidiaries: []int64{rootID},
	})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	own := "IX.17"
	grand, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		ParentID: &child, Type: "expense", DefaultCurrency: "USD", Names: enName("Travel"),
		Subsidiaries: []int64{rootID}, Form990Code: &own,
	})
	if err != nil {
		t.Fatalf("create grand: %v", err)
	}

	eff, err := s.Effective990Codes(context.Background())
	if err != nil {
		t.Fatalf("Effective990Codes: %v", err)
	}
	if eff[parent] != "IX.16" {
		t.Errorf("effective code(parent) = %q, want IX.16", eff[parent])
	}
	if eff[child] != "IX.16" {
		t.Errorf("effective code(child, inherited) = %q, want IX.16", eff[child])
	}
	if eff[grand] != "IX.17" {
		t.Errorf("effective code(grand, own) = %q, want IX.17 (own wins)", eff[grand])
	}
}

// TestSet990CodeTypeMismatch: assigning a revenue 990 line to an expense account
// is rejected (Err990TypeMismatch) against form990_lines.account_types; no trace.
func TestSet990CodeTypeMismatch(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	acct, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "expense", DefaultCurrency: "USD", Names: enName("Rent"), Subsidiaries: []int64{rootID},
	})
	if err != nil {
		t.Fatalf("create acct: %v", err)
	}

	before := countChanges(t, d)
	// VIII.2 is a revenue line; the account is expense -> mismatch.
	rev := "VIII.2"
	err = s.UpdateAccount(mutCtx(), acct, UpdateAccountInput{Form990Code: &rev})
	if !errors.Is(err, Err990TypeMismatch) {
		t.Fatalf("assign revenue line to expense account: err = %v, want Err990TypeMismatch", err)
	}
	if n := countChanges(t, d); n != before {
		t.Errorf("changes count = %d, want %d (rejected update leaves no trace)", n, before)
	}
}

// TestCreateFunctionalClassNonExpense: a functional_class on a non-expense
// account is rejected cleanly (ErrFunctionalClassNotExpense) before hitting the
// trigger; no trace.
func TestCreateFunctionalClassNonExpense(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	before := countChanges(t, d)
	fc := "program"
	_, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("Cash"),
		Subsidiaries: []int64{rootID}, FunctionalClass: &fc,
	})
	if !errors.Is(err, ErrFunctionalClassNotExpense) {
		t.Fatalf("functional class on asset: err = %v, want ErrFunctionalClassNotExpense", err)
	}
	if n := countChanges(t, d); n != before {
		t.Errorf("changes count = %d, want %d (rejected create leaves no trace)", n, before)
	}
}

// ptr returns a pointer to v — for the *int64/*string optional update fields.
func ptr[T any](v T) *T { return &v }
