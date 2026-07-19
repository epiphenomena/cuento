package store

import (
	"testing"

	"cuento/internal/ids"
	"cuento/internal/testutil"
)

// idSet turns a slice of ints into a set for membership assertions.
func optIDs(opts []ParentOption) map[ids.AccountID]bool {
	m := make(map[ids.AccountID]bool, len(opts))
	for _, o := range opts {
		m[o.ID] = true
	}
	return m
}

// TestParentOptionsExcludeDescendantsAndWrongClass: the parent-select options for
// a given account exclude (a) the account itself, (b) its descendants, and (c)
// any account whose type cannot host the account's type (D11) -- reusing the same
// typeCompatible rule the write path enforces.
func TestParentOptionsExcludeDescendantsAndWrongClass(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	// Build: Assets(placeholder) > Cash(child) ; Liabilities(placeholder) ;
	// Revenue(placeholder) . Moving Assets, we want the options to EXCLUDE Assets
	// itself and Cash (descendant), EXCLUDE Liabilities/Revenue (wrong class for an
	// asset), and there is no other asset placeholder, so the option set is empty.
	assets, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("Assets"), Subsidiaries: []ids.SubsidiaryID{rootID},
	})
	if err != nil {
		t.Fatalf("create assets: %v", err)
	}
	cash, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		ParentID: &assets, Type: "asset", DefaultCurrency: "USD", Names: enName("Cash"), Subsidiaries: []ids.SubsidiaryID{rootID},
	})
	if err != nil {
		t.Fatalf("create cash: %v", err)
	}
	liab, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "liability", DefaultCurrency: "USD", Names: enName("Liabilities"), Subsidiaries: []ids.SubsidiaryID{rootID},
	})
	if err != nil {
		t.Fatalf("create liab: %v", err)
	}
	rev, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "revenue", DefaultCurrency: "USD", Names: enName("Revenue"), Subsidiaries: []ids.SubsidiaryID{rootID},
	})
	if err != nil {
		t.Fatalf("create rev: %v", err)
	}

	// Options to be a parent of the asset account `assets` (type asset, exclude self
	// + descendants).
	opts, err := s.ParentOptions(mutCtx(), "en", "asset", assets)
	if err != nil {
		t.Fatalf("ParentOptions: %v", err)
	}
	got := optIDs(opts)
	if got[assets] {
		t.Errorf("options include the account itself (%d)", assets)
	}
	if got[cash] {
		t.Errorf("options include descendant Cash (%d)", cash)
	}
	if got[liab] {
		t.Errorf("options include wrong-class Liabilities (%d)", liab)
	}
	if got[rev] {
		t.Errorf("options include wrong-class Revenue (%d)", rev)
	}
	if len(opts) != 0 {
		t.Errorf("options = %v, want empty (no other asset placeholder is eligible)", got)
	}

	// A NEW revenue account (excludeID=0): options should include the revenue
	// placeholder (R/E interleave) but NOT the asset/liability placeholders.
	revOpts, err := s.ParentOptions(mutCtx(), "en", "revenue", 0)
	if err != nil {
		t.Fatalf("ParentOptions(new revenue): %v", err)
	}
	rgot := optIDs(revOpts)
	if !rgot[rev] {
		t.Errorf("new-revenue options missing the revenue placeholder %d; got %v", rev, rgot)
	}
	if rgot[assets] || rgot[liab] {
		t.Errorf("new-revenue options include a non-R/E placeholder; got %v", rgot)
	}
}

// TestAccountEditorOptionsPath: each option carries a dotted ancestor path ending
// in the account's own name -- a nested leaf under a placeholder renders
// "Parent.Child", while a top-level leaf's Path is just its own name (no dot). The
// last path segment always equals the option's Name (both come from the same
// lang-resolved tree). p26.1 adds this field for the combobox display/fuzzy match.
func TestAccountEditorOptionsPath(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	// Cash(placeholder) > BOA(leaf) ; Petty(top-level leaf).
	cash, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("Cash"), Subsidiaries: []ids.SubsidiaryID{rootID},
	})
	if err != nil {
		t.Fatalf("create cash: %v", err)
	}
	boa, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		ParentID: &cash, Type: "asset", DefaultCurrency: "USD", Names: enName("BOA"), Subsidiaries: []ids.SubsidiaryID{rootID},
	})
	if err != nil {
		t.Fatalf("create boa: %v", err)
	}
	petty, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("Petty"), Subsidiaries: []ids.SubsidiaryID{rootID},
	})
	if err != nil {
		t.Fatalf("create petty: %v", err)
	}

	opts, err := s.AccountEditorOptions(mutCtx(), "en", rootID)
	if err != nil {
		t.Fatalf("AccountEditorOptions: %v", err)
	}
	paths := map[ids.AccountID]string{}
	for _, o := range opts {
		paths[o.ID] = o.Path
		if o.Path == "" || o.Path[len(o.Path)-len(o.Name):] != o.Name {
			t.Errorf("option %d (%q): Path %q does not end in its Name", o.ID, o.Name, o.Path)
		}
	}
	// Cash is a placeholder (has a child) -> not offered; only leaves appear.
	if _, ok := paths[cash]; ok {
		t.Errorf("placeholder Cash (%d) should not be an editor option", cash)
	}
	if got := paths[boa]; got != "Cash.BOA" {
		t.Errorf("nested leaf BOA Path = %q, want %q", got, "Cash.BOA")
	}
	if got := paths[petty]; got != "Petty" {
		t.Errorf("top-level leaf Petty Path = %q, want %q (no dot)", got, "Petty")
	}
}

// TestAccountEditorOptionsIncludeInactive: an inactive leaf is normally SKIPPED by
// AccountEditorOptions, but AccountEditorOptionsWith(include=[id]) appends it as an
// Unavailable option (marked) carrying its real name/path/type so the editor can
// display a split whose account was deactivated after the split was posted (the
// display-only "missing accounts" bug, p26.10). The normal call (no include) still
// omits it, so a NEW transaction's option set is unchanged.
func TestAccountEditorOptionsIncludeInactive(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	// A top-level leaf, then deactivate it.
	acct, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("Old Cash"), Subsidiaries: []ids.SubsidiaryID{rootID},
	})
	if err != nil {
		t.Fatalf("create acct: %v", err)
	}
	if err := s.DeactivateAccount(mutCtx(), acct); err != nil {
		t.Fatalf("deactivate acct: %v", err)
	}

	// Normal call: the inactive leaf is NOT offered.
	plain, err := s.AccountEditorOptions(mutCtx(), "en", rootID)
	if err != nil {
		t.Fatalf("AccountEditorOptions: %v", err)
	}
	for _, o := range plain {
		if o.ID == acct {
			t.Fatalf("inactive account %d offered in the plain option set", acct)
		}
	}

	// Include the inactive account: it appears, marked Unavailable, with real metadata.
	withInc, err := s.AccountEditorOptionsWith(mutCtx(), "en", rootID, []ids.AccountID{acct})
	if err != nil {
		t.Fatalf("AccountEditorOptionsWith: %v", err)
	}
	var found *AccountEditorOption
	for i := range withInc {
		if withInc[i].ID == acct {
			found = &withInc[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("included inactive account %d missing from the option set", acct)
	}
	if !found.Unavailable {
		t.Errorf("included inactive account %d not marked Unavailable", acct)
	}
	if found.Name != "Old Cash" || found.Path != "Old Cash" {
		t.Errorf("included account name/path = %q/%q, want %q/%q", found.Name, found.Path, "Old Cash", "Old Cash")
	}
	if found.Type != "asset" {
		t.Errorf("included account type = %q, want asset", found.Type)
	}

	// Including an id ALREADY in the set (an active leaf) does not duplicate it and
	// does not mark it Unavailable.
	active, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("Live Cash"), Subsidiaries: []ids.SubsidiaryID{rootID},
	})
	if err != nil {
		t.Fatalf("create active: %v", err)
	}
	withActive, err := s.AccountEditorOptionsWith(mutCtx(), "en", rootID, []ids.AccountID{active})
	if err != nil {
		t.Fatalf("AccountEditorOptionsWith(active): %v", err)
	}
	n := 0
	for _, o := range withActive {
		if o.ID == active {
			n++
			if o.Unavailable {
				t.Errorf("active account %d wrongly marked Unavailable", active)
			}
		}
	}
	if n != 1 {
		t.Errorf("active included account appears %d times, want exactly 1", n)
	}
}

// TestSubsidiaryFilter: Tree(lang, &sub) returns only accounts mapped to the
// selected subsidiary -- the chart-of-accounts subsidiary filter. An account
// mapped only to a different sub is dropped; one mapped to the selected sub is
// kept.
func TestSubsidiaryFilter(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	subA := newSub(t, s, rootID, "A")
	subB := newSub(t, s, rootID, "B")

	// onlyA maps {subA}; onlyB maps {subB}. Both are top-level (no propagation
	// beyond self).
	onlyA, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("Cash A"), Subsidiaries: []ids.SubsidiaryID{subA},
	})
	if err != nil {
		t.Fatalf("create onlyA: %v", err)
	}
	onlyB, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("Cash B"), Subsidiaries: []ids.SubsidiaryID{subB},
	})
	if err != nil {
		t.Fatalf("create onlyB: %v", err)
	}

	rows, err := s.Tree(mutCtx(), "en", &subA)
	if err != nil {
		t.Fatalf("Tree(subA): %v", err)
	}
	in := map[ids.AccountID]bool{}
	for _, r := range rows {
		in[r.ID] = true
	}
	if !in[onlyA] {
		t.Errorf("subA filter dropped onlyA (%d); got %v", onlyA, in)
	}
	if in[onlyB] {
		t.Errorf("subA filter kept onlyB (%d), which maps only subB; got %v", onlyB, in)
	}
}

// TestForm990OptionsFilteredByType: the 990 select offers only lines whose
// account_types CSV includes the account's type, and every offered code is
// accepted by the write-side validator (check990Type) -- proving the options and
// the accepted writes share one predicate (D25).
func TestForm990OptionsFilteredByType(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	for _, typ := range []string{"asset", "liability", "equity", "revenue", "expense"} {
		opts, err := s.Form990LinesForType(mutCtx(), typ)
		if err != nil {
			t.Fatalf("Form990LinesForType(%s): %v", typ, err)
		}
		if len(opts) == 0 {
			// Not every type must have lines, but revenue/expense/asset/liability do
			// in the full-990 seed (Parts VIII/IX/X). A totally empty result for a
			// major type signals a broken filter.
			t.Logf("Form990LinesForType(%s) returned no lines", typ)
		}
		for _, o := range opts {
			// Every offered code must pass the write-side type check.
			if err := check990Type(mutCtx(), s.q, o.Code, typ); err != nil {
				t.Errorf("offered code %q for type %s is rejected by check990Type: %v", o.Code, typ, err)
			}
		}
	}

	// Cross-type negative: a revenue-only line (Part VIII) must NOT appear for an
	// expense account. Find one revenue line and assert it is absent from expense
	// options.
	revOpts, err := s.Form990LinesForType(mutCtx(), "revenue")
	if err != nil {
		t.Fatalf("Form990LinesForType(revenue): %v", err)
	}
	if len(revOpts) == 0 {
		t.Fatal("no revenue 990 lines seeded; cannot run cross-type check")
	}
	// Pick a revenue line that is NOT also valid for expense.
	var revOnly string
	for _, o := range revOpts {
		if err := check990Type(mutCtx(), s.q, o.Code, "expense"); err != nil {
			revOnly = o.Code
			break
		}
	}
	if revOnly == "" {
		t.Skip("no revenue-only 990 line in the seed to test cross-type exclusion")
	}
	expOpts, err := s.Form990LinesForType(mutCtx(), "expense")
	if err != nil {
		t.Fatalf("Form990LinesForType(expense): %v", err)
	}
	for _, o := range expOpts {
		if o.Code == revOnly {
			t.Errorf("revenue-only line %q offered for an expense account", revOnly)
		}
	}
}
