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
// usage half of this guard was completed in p08.2 and is exercised below via
// testRemoveSubBlockedBySplit (a split in that subsidiary also blocks removal).
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

	// p08.2 (completed): removing S is ALSO blocked while a split references the
	// account in a non-deleted txn of subsidiary S. Exercise the split half.
	testRemoveSubBlockedBySplit(t)
}

// testRemoveSubBlockedBySplit is the split-usage half of the guard, completed in
// p08.2: a leaf account with a split in subsidiary S cannot drop S.
func testRemoveSubBlockedBySplit(t *testing.T) {
	t.Helper()
	e := newTxnEnv(t)
	// Post a txn in US using `salaries` and `checking` (both mapped to US only).
	if _, err := e.s.PostTransaction(mutCtx(), e.balancedInput(100)); err != nil {
		t.Fatalf("PostTransaction: %v", err)
	}
	// Add a second sub to checking, then try to remove US: blocked by the split.
	subMX := newSub(t, e.s, rootID, "MX")
	if err := e.s.SetAccountSubsidiaries(mutCtx(), e.checking, []int64{e.subUS, subMX}); err != nil {
		t.Fatalf("widen checking subs: %v", err)
	}
	before := countChanges(t, e.d)
	err := e.s.SetAccountSubsidiaries(mutCtx(), e.checking, []int64{subMX})
	if !errors.Is(err, ErrSubInUseByChild) {
		t.Fatalf("remove US used by a split: err = %v, want ErrSubInUseByChild", err)
	}
	if n := countChanges(t, e.d); n != before {
		t.Errorf("changes = %d, want %d (rejected leaves no trace)", n, before)
	}
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

// treeName returns the resolved name for account id in a Tree result, failing if
// the account is absent.
func treeName(t *testing.T, rows []TreeRow, id int64) string {
	t.Helper()
	for _, r := range rows {
		if r.ID == id {
			return r.Name
		}
	}
	t.Fatalf("account %d not in tree", id)
	return ""
}

// TestTreeNameFallback: the resolved name in Tree follows the fallback chain
// requested-lang -> en -> any (deterministic), for both the nil-filter and the
// subFilter query paths (p05.3).
//
//   - requested lang present: that name wins (over en, proving precedence).
//   - requested lang absent, en present: the en name.
//   - both requested lang and en absent, only another lang present: that "any"
//     name, chosen deterministically (ORDER BY lang LIMIT 1).
//
// CreateAccount mandates an en name and there is no remove-name API, so the
// any-branch account has its en row raw-DELETEd after creation (raw SQL in tests
// is in-convention here; noted in the p05.3 commit body).
func TestTreeNameFallback(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	subA := newSub(t, s, rootID, "A")

	// requested-lang wins: has both fr and en; requesting fr yields the fr name.
	wantsFr, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD",
		Names: map[string]string{"en": "en-name", "fr": "fr-name"}, Subsidiaries: []int64{subA},
	})
	if err != nil {
		t.Fatalf("create wantsFr: %v", err)
	}

	// en fallback: no fr, has en; requesting fr yields the en name.
	enFallback, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD",
		Names: map[string]string{"en": "en-only"}, Subsidiaries: []int64{subA},
	})
	if err != nil {
		t.Fatalf("create enFallback: %v", err)
	}

	// any fallback: neither fr nor en (after deleting the mandated en row); only
	// es and de remain, so requesting fr must yield the deterministic first-by-lang
	// name ("de-name" < "es-name").
	anyFallback, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD",
		Names: map[string]string{"en": "en-doomed", "es": "es-name", "de": "de-name"}, Subsidiaries: []int64{subA},
	})
	if err != nil {
		t.Fatalf("create anyFallback: %v", err)
	}
	if _, err := d.Exec(`DELETE FROM account_names WHERE account_id = ? AND lang = 'en'`, anyFallback); err != nil {
		t.Fatalf("delete en name of anyFallback: %v", err)
	}

	check := func(t *testing.T, rows []TreeRow) {
		t.Helper()
		if got := treeName(t, rows, wantsFr); got != "fr-name" {
			t.Errorf("requested-lang: name = %q, want fr-name", got)
		}
		if got := treeName(t, rows, enFallback); got != "en-only" {
			t.Errorf("en fallback: name = %q, want en-only", got)
		}
		if got := treeName(t, rows, anyFallback); got != "de-name" {
			t.Errorf("any fallback: name = %q, want de-name (deterministic ORDER BY lang)", got)
		}
	}

	nilRows, err := s.Tree(context.Background(), "fr", nil)
	if err != nil {
		t.Fatalf("Tree(fr, nil): %v", err)
	}
	t.Run("nil filter", func(t *testing.T) { check(t, nilRows) })

	subRows, err := s.Tree(context.Background(), "fr", &subA)
	if err != nil {
		t.Fatalf("Tree(fr, &subA): %v", err)
	}
	t.Run("subsidiary filter", func(t *testing.T) { check(t, subRows) })
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

// accountFlags reads an account's current_cash / open_item live flags.
func accountFlags(t *testing.T, s *Store, id int64) (currentCash, openItem bool) {
	t.Helper()
	row, err := s.GetAccount(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAccount(%d): %v", id, err)
	}
	return row.CurrentCash != 0, row.OpenItem != 0
}

// TestCreateAccountFlagsVersioned (p27.1): creating an asset with current_cash +
// open_item persists both flags on the live row AND the latest version snapshot
// (so Z3 stays clean).
func TestCreateAccountFlagsVersioned(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	id, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("Receivable Cash"),
		Subsidiaries: []int64{rootID}, CurrentCash: true, OpenItem: true,
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	cc, oi := accountFlags(t, s, id)
	if !cc || !oi {
		t.Fatalf("live flags = (cc=%v, oi=%v), want both true", cc, oi)
	}
	testutil.AssertVersioned(t, d, "accounts", id, "create")
	// The version snapshot must carry the flags too (Z3 backstop).
	var vcc, voi int64
	if err := d.QueryRow(`SELECT current_cash, open_item FROM accounts_versions
		WHERE entity_id = ? ORDER BY valid_from DESC, id DESC LIMIT 1`, id).Scan(&vcc, &voi); err != nil {
		t.Fatalf("read version snapshot: %v", err)
	}
	if vcc != 1 || voi != 1 {
		t.Errorf("version snapshot flags = (cc=%d, oi=%d), want (1,1)", vcc, voi)
	}
}

// accountNotes reads an account's current live notes ("" when NULL).
func accountNotes(t *testing.T, s *Store, id int64) string {
	t.Helper()
	row, err := s.GetAccount(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAccount(%d): %v", id, err)
	}
	return row.Notes.String
}

// TestAccountNotesRoundTrip (p28.7): a notes value persists on the live row + the
// latest version snapshot (Z3), survives an UNRELATED no-op edit (currency-only,
// Notes left nil), and is cleared to NULL by an empty-string Notes.
func TestAccountNotesRoundTrip(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	note := "Synthetic reconcile monthly against the bank feed."
	id, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("Checking"),
		Subsidiaries: []int64{rootID}, Notes: &note,
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if got := accountNotes(t, s, id); got != note {
		t.Fatalf("notes after create = %q, want %q", got, note)
	}
	testutil.AssertVersioned(t, d, "accounts", id, "create")
	// The version snapshot must carry notes too (Z3 backstop).
	var vnotes sql.NullString
	if err := d.QueryRow(`SELECT notes FROM accounts_versions
		WHERE entity_id = ? ORDER BY valid_from DESC, id DESC LIMIT 1`, id).Scan(&vnotes); err != nil {
		t.Fatalf("read version snapshot notes: %v", err)
	}
	if vnotes.String != note {
		t.Errorf("version snapshot notes = %q, want %q", vnotes.String, note)
	}

	// An unrelated edit (Notes nil) must PRESERVE the note (the ripple: next := cur
	// carries it through).
	cur := "MXN"
	if err := s.UpdateAccount(mutCtx(), id, UpdateAccountInput{DefaultCurrency: &cur}); err != nil {
		t.Fatalf("no-op UpdateAccount: %v", err)
	}
	if got := accountNotes(t, s, id); got != note {
		t.Fatalf("notes after unrelated edit = %q, want preserved %q", got, note)
	}

	// An empty-string Notes clears it to NULL.
	empty := ""
	if err := s.UpdateAccount(mutCtx(), id, UpdateAccountInput{Notes: &empty}); err != nil {
		t.Fatalf("clear UpdateAccount: %v", err)
	}
	if got := accountNotes(t, s, id); got != "" {
		t.Fatalf("notes after clear = %q, want empty", got)
	}
}

// TestCreateCurrentCashNonAsset (p27.1): current_cash on a non-asset account is
// rejected cleanly (ErrCurrentCashNotAsset) before the tx opens; no trace.
func TestCreateCurrentCashNonAsset(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	before := countChanges(t, d)
	_, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "liability", DefaultCurrency: "USD", Names: enName("Loan"),
		Subsidiaries: []int64{rootID}, CurrentCash: true,
	})
	if !errors.Is(err, ErrCurrentCashNotAsset) {
		t.Fatalf("current_cash on liability: err = %v, want ErrCurrentCashNotAsset", err)
	}
	if n := countChanges(t, d); n != before {
		t.Errorf("changes count = %d, want %d (rejected create leaves no trace)", n, before)
	}
}

// TestCreateOpenItemBadType (p27.1): open_item on a type outside {asset,liability}
// is rejected cleanly (ErrOpenItemBadType); no trace.
func TestCreateOpenItemBadType(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	before := countChanges(t, d)
	_, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "equity", DefaultCurrency: "USD", Names: enName("Opening"),
		Subsidiaries: []int64{rootID}, OpenItem: true,
	})
	if !errors.Is(err, ErrOpenItemBadType) {
		t.Fatalf("open_item on equity: err = %v, want ErrOpenItemBadType", err)
	}
	if n := countChanges(t, d); n != before {
		t.Errorf("changes count = %d, want %d (rejected create leaves no trace)", n, before)
	}
}

// TestUpdateAccountFlagsRoundTrip (p27.1): setting the flags via UpdateAccount
// persists them; a subsequent unrelated no-op edit (changing nothing about the
// flags) leaves them intact -- next := cur carries them through.
func TestUpdateAccountFlagsRoundTrip(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	id, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("Bank"),
		Subsidiaries: []int64{rootID},
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	// Set both flags.
	if err := s.UpdateAccount(mutCtx(), id, UpdateAccountInput{
		CurrentCash: ptr(true), OpenItem: ptr(true),
	}); err != nil {
		t.Fatalf("UpdateAccount set flags: %v", err)
	}
	if cc, oi := accountFlags(t, s, id); !cc || !oi {
		t.Fatalf("after set: flags = (cc=%v, oi=%v), want both true", cc, oi)
	}
	// An unrelated update (sort order) must NOT clear the flags.
	if err := s.UpdateAccount(mutCtx(), id, UpdateAccountInput{SortOrder: ptr(int64(5))}); err != nil {
		t.Fatalf("UpdateAccount unrelated: %v", err)
	}
	if cc, oi := accountFlags(t, s, id); !cc || !oi {
		t.Errorf("after unrelated update: flags = (cc=%v, oi=%v), want both true (carried through)", cc, oi)
	}
	// Clearing one flag works.
	if err := s.UpdateAccount(mutCtx(), id, UpdateAccountInput{CurrentCash: ptr(false)}); err != nil {
		t.Fatalf("UpdateAccount clear: %v", err)
	}
	if cc, oi := accountFlags(t, s, id); cc || !oi {
		t.Errorf("after clear current_cash: flags = (cc=%v, oi=%v), want (false,true)", cc, oi)
	}
}

// TestUpdateCurrentCashNonAsset (p27.1): setting current_cash on a non-asset
// account via UpdateAccount is rejected (ErrCurrentCashNotAsset).
func TestUpdateCurrentCashNonAsset(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	id, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "liability", DefaultCurrency: "USD", Names: enName("Payable"),
		Subsidiaries: []int64{rootID},
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	err = s.UpdateAccount(mutCtx(), id, UpdateAccountInput{CurrentCash: ptr(true)})
	if !errors.Is(err, ErrCurrentCashNotAsset) {
		t.Fatalf("current_cash on liability update: err = %v, want ErrCurrentCashNotAsset", err)
	}
	// open_item on a liability IS allowed (payable).
	if err := s.UpdateAccount(mutCtx(), id, UpdateAccountInput{OpenItem: ptr(true)}); err != nil {
		t.Fatalf("open_item on liability update: %v", err)
	}
}

// TestDeactivatePreservesFlags (p27.1): DeactivateAccount writes the full account
// row from cur; it must carry the flags through (else Z3 diverges).
func TestDeactivatePreservesFlags(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	id, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("Petty Cash"),
		Subsidiaries: []int64{rootID}, CurrentCash: true,
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if err := s.DeactivateAccount(mutCtx(), id); err != nil {
		t.Fatalf("DeactivateAccount: %v", err)
	}
	if cc, _ := accountFlags(t, s, id); !cc {
		t.Errorf("after deactivate: current_cash = false, want true (carried through)")
	}
}

// ptr returns a pointer to v — for the *int64/*string optional update fields.
func ptr[T any](v T) *T { return &v }
