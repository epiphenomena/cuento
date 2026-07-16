package main

import (
	"strings"
	"testing"
)

// TestRunAccountsEmitsTypTierRoots: the account skeleton derives a ONE-level tier
// from stmt + typ (p26.73: the stmt super-parent tier is no longer stored). A
// leaf's parent is its (stmt, typ) intermediate; that intermediate is a ROOT (nil
// parent); a blank-typ leaf is itself a root. The intermediate rows are emitted
// once each, with the UNION of their descendants' subsidiaries, and the explicit
// `parent` column is ignored.
func TestRunAccountsEmitsTypTierRoots(t *testing.T) {
	// Synthetic source (all invented, AGENTS rule 11):
	//   - Checking: stmt A, typ "Bank", US + MX  -> Bank(root) -> Checking
	//   - Savings : stmt A, typ "Bank", US        -> shares the asset Bank tier
	//   - Rent    : stmt E, typ "Bank", US        -> a SAME-typ-DIFFERENT-stmt case;
	//               its Bank tier must be the EXPENSE Bank, distinct from the asset Bank.
	//   - Cash    : stmt A, typ ""  , US          -> BLANK typ: leaf is itself a root.
	// The explicit `parent` column carries "IGNOREME" on every row to prove it is
	// not used for structure any more.
	src := strings.Join([]string{
		header,
		row("US", "A", "Bank", "Checking", "", "2025-01-01", "", "1", "m", "", "USD", "1.0", "10.00", "10.00", "0", "0", "IGNOREME"),
		row("MX", "A", "Bank", "Checking", "", "2025-01-02", "", "2", "m", "", "MXN", "1.0", "5.00", "5.00", "0", "0", "IGNOREME"),
		row("US", "A", "Bank", "Savings", "", "2025-01-03", "", "3", "m", "", "USD", "1.0", "7.00", "7.00", "0", "0", "IGNOREME"),
		row("US", "E", "Bank", "Rent", "", "2025-01-04", "", "4", "m", "", "USD", "1.0", "20.00", "20.00", "0", "0", "IGNOREME"),
		row("US", "A", "", "Cash", "", "2025-01-05", "", "5", "m", "", "USD", "1.0", "3.00", "3.00", "0", "0", "IGNOREME"),
	}, "\n") + "\n"

	var out strings.Builder
	if err := runAccounts(strings.NewReader(src), &out); err != nil {
		t.Fatalf("runAccounts: %v", err)
	}

	rows, err := ReadAccountMap(strings.NewReader(out.String()))
	if err != nil {
		t.Fatalf("emitted skeleton not parseable: %v\n%s", err, out.String())
	}
	byAcct := map[string]AccountMap{}
	for _, r := range rows {
		if _, dup := byAcct[r.SourceAcct]; dup {
			t.Errorf("duplicate row for %q", r.SourceAcct)
		}
		byAcct[r.SourceAcct] = r
	}

	// Leaf Checking -> asset Bank intermediate.
	chk := byAcct["Checking"]
	if chk.CuentoType != "asset" {
		t.Errorf("Checking type = %q, want asset", chk.CuentoType)
	}
	bankAsset := typParentKey("asset", "Bank")
	if chk.CuentoParent != bankAsset {
		t.Errorf("Checking parent = %q, want %q", chk.CuentoParent, bankAsset)
	}
	if len(chk.Subsidiaries) != 2 { // union US+MX carried on the leaf
		t.Errorf("Checking subs = %v, want US+MX", chk.Subsidiaries)
	}

	// Savings shares the SAME asset Bank intermediate as Checking.
	if byAcct["Savings"].CuentoParent != bankAsset {
		t.Errorf("Savings parent = %q, want %q", byAcct["Savings"].CuentoParent, bankAsset)
	}

	// The asset Bank intermediate exists once, type asset, is a ROOT (no parent),
	// subs = UNION of Checking(US,MX)+Savings(US) = {MX,US}.
	ai, ok := byAcct[bankAsset]
	if !ok {
		t.Fatalf("asset Bank intermediate not emitted; got %v", keysOf(byAcct))
	}
	if ai.CuentoType != "asset" {
		t.Errorf("asset Bank type = %q, want asset", ai.CuentoType)
	}
	if ai.CuentoParent != "" {
		t.Errorf("asset Bank parent = %q, want root (no parent)", ai.CuentoParent)
	}
	if ai.NameEN != "Bank" {
		t.Errorf("asset Bank name = %q, want Bank", ai.NameEN)
	}
	if len(ai.Subsidiaries) != 2 {
		t.Errorf("asset Bank subs = %v, want union US+MX", ai.Subsidiaries)
	}

	// SAME-typ-DIFFERENT-stmt: Rent (expense, typ Bank) has a DISTINCT expense Bank
	// intermediate, keyed by (stmt,typ) so it does not collapse into the asset Bank.
	bankExp := typParentKey("expense", "Bank")
	if byAcct["Rent"].CuentoParent != bankExp {
		t.Errorf("Rent parent = %q, want %q", byAcct["Rent"].CuentoParent, bankExp)
	}
	ei, ok := byAcct[bankExp]
	if !ok {
		t.Fatalf("expense Bank intermediate not emitted")
	}
	if ei.CuentoType != "expense" || ei.CuentoParent != "" {
		t.Errorf("expense Bank wrong: type=%q parent=%q (want expense, root)", ei.CuentoType, ei.CuentoParent)
	}
	if bankAsset == bankExp {
		t.Fatal("asset Bank and expense Bank collapsed to one key")
	}

	// BLANK typ: Cash is itself a ROOT (no intermediate tier, no stmt super-parent).
	if byAcct["Cash"].CuentoParent != "" {
		t.Errorf("Cash (blank typ) parent = %q, want root (no parent)", byAcct["Cash"].CuentoParent)
	}

	// No stmt-tier super-parent rows exist any more.
	for _, r := range rows {
		if strings.HasPrefix(r.SourceAcct, "::super:") {
			t.Errorf("stmt-tier super-parent %q still emitted", r.SourceAcct)
		}
	}

	// Exact row set: 4 leaves + 2 intermediates (asset Bank, expense Bank) = 6
	// distinct rows, none duplicated (the 2 stmt super-parents are gone).
	if len(rows) != 6 {
		t.Errorf("emitted %d rows, want 6: %v", len(rows), keysOf(byAcct))
	}
}

// TestRunAccountsTypCollidesWithLeafName: a `typ` value that equals a real leaf
// `acct` name does NOT collide — the intermediate is namespaced by its own
// synthetic key, so a leaf literally named "Bank" stays a separate row from the
// (asset,"Bank") intermediate tier.
func TestRunAccountsTypCollidesWithLeafName(t *testing.T) {
	src := strings.Join([]string{
		header,
		// A real leaf account literally named "Bank" (stmt A, typ "Cash").
		row("US", "A", "Cash", "Bank", "", "2025-01-01", "", "1", "m", "", "USD", "1.0", "10.00", "10.00", "0", "0", "x"),
		// A different leaf whose TYP is "Bank" — its intermediate is (asset,"Bank").
		row("US", "A", "Bank", "Checking", "", "2025-01-02", "", "2", "m", "", "USD", "1.0", "5.00", "5.00", "0", "0", "x"),
	}, "\n") + "\n"

	var out strings.Builder
	if err := runAccounts(strings.NewReader(src), &out); err != nil {
		t.Fatalf("runAccounts: %v", err)
	}
	rows, err := ReadAccountMap(strings.NewReader(out.String()))
	if err != nil {
		t.Fatalf("skeleton not parseable: %v", err)
	}
	byAcct := map[string]AccountMap{}
	for _, r := range rows {
		byAcct[r.SourceAcct] = r
	}

	// The real "Bank" leaf keeps its own key and is parented under the (asset,"Cash")
	// intermediate — NOT confused with the (asset,"Bank") intermediate.
	leaf, ok := byAcct["Bank"]
	if !ok {
		t.Fatalf("real leaf 'Bank' not emitted")
	}
	if leaf.CuentoParent != typParentKey("asset", "Cash") {
		t.Errorf("leaf Bank parent = %q, want %q", leaf.CuentoParent, typParentKey("asset", "Cash"))
	}
	// The (asset,"Bank") intermediate exists under its namespaced key, distinct from
	// the real leaf "Bank".
	inter := typParentKey("asset", "Bank")
	if inter == "Bank" {
		t.Fatal("intermediate key collided with the real leaf name 'Bank'")
	}
	if _, ok := byAcct[inter]; !ok {
		t.Errorf("(asset,Bank) intermediate not emitted under %q", inter)
	}
	if byAcct["Checking"].CuentoParent != inter {
		t.Errorf("Checking parent = %q, want %q", byAcct["Checking"].CuentoParent, inter)
	}
}

// TestRunAccountsLeafSpansTypsSubsetSubs: the SAME leaf `acct` recurring under
// DIFFERENT `typ` values (typ is a per-journal-entry classification, so one account
// records under many typs) keeps its first-sighting parent, but the chosen
// intermediate must carry a SUPERSET of the leaf's full subsidiary union (rule 7 /
// Z12: parent subs superset of children -- else the go-live build's CreateAccount /
// cuento check reject the chain). The leaf appears as (A,Bank,US) then (A,Transfer,
// MX); it must stay parented under (asset,Bank) whose subs include BOTH US and MX.
func TestRunAccountsLeafSpansTypsSubsetSubs(t *testing.T) {
	src := strings.Join([]string{
		header,
		row("US", "A", "Bank", "Checking", "", "2025-01-01", "", "1", "m", "", "USD", "1.0", "10.00", "10.00", "0", "0", "x"),
		row("MX", "A", "Transfer", "Checking", "", "2025-01-02", "", "2", "m", "", "MXN", "1.0", "5.00", "5.00", "0", "0", "x"),
	}, "\n") + "\n"

	var out strings.Builder
	if err := runAccounts(strings.NewReader(src), &out); err != nil {
		t.Fatalf("runAccounts: %v", err)
	}
	rows, err := ReadAccountMap(strings.NewReader(out.String()))
	if err != nil {
		t.Fatalf("skeleton not parseable: %v", err)
	}
	byAcct := map[string]AccountMap{}
	for _, r := range rows {
		byAcct[r.SourceAcct] = r
	}

	leaf := byAcct["Checking"]
	if leaf.CuentoParent != typParentKey("asset", "Bank") { // first sighting wins
		t.Errorf("Checking parent = %q, want %q (first sighting)", leaf.CuentoParent, typParentKey("asset", "Bank"))
	}
	if len(leaf.Subsidiaries) != 2 { // full union US+MX on the leaf
		t.Fatalf("Checking subs = %v, want US+MX", leaf.Subsidiaries)
	}

	// The chosen intermediate AND the super-parent must be supersets of the leaf's subs.
	subset := func(child, parent []string) bool {
		set := map[string]bool{}
		for _, s := range parent {
			set[s] = true
		}
		for _, s := range child {
			if !set[s] {
				return false
			}
		}
		return true
	}
	inter := byAcct[typParentKey("asset", "Bank")]
	if inter.CuentoParent != "" {
		t.Errorf("intermediate parent = %q, want root (no parent)", inter.CuentoParent)
	}
	if !subset(leaf.Subsidiaries, inter.Subsidiaries) {
		t.Errorf("intermediate subs %v are not a superset of leaf subs %v", inter.Subsidiaries, leaf.Subsidiaries)
	}
}

// keysOf returns a map's keys (test-only, for failure messages).
func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
