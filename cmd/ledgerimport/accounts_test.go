package main

import (
	"strings"
	"testing"
)

// TestRunAccountsEmitsTwoLevelChain: the account skeleton derives a two-level
// parent chain from stmt + typ. A leaf's parent is its (stmt, typ) intermediate;
// the intermediate's parent is the stmt super-parent (Assets/…/Expenses). The
// intermediate + super-parent rows are emitted once each, with the UNION of their
// descendants' subsidiaries, and the explicit `parent` column is ignored.
func TestRunAccountsEmitsTwoLevelChain(t *testing.T) {
	// Synthetic source (all invented, AGENTS rule 11):
	//   - Checking: stmt A, typ "Bank", US + MX  -> Assets -> Bank -> Checking
	//   - Savings : stmt A, typ "Bank", US        -> shares the Assets->Bank tier
	//   - Rent    : stmt E, typ "Bank", US        -> a SAME-typ-DIFFERENT-stmt case;
	//               its Bank tier must be Expenses->Bank, distinct from Assets->Bank.
	//   - Cash    : stmt A, typ ""  , US          -> BLANK typ: leaf directly under Assets.
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

	// Leaf Checking -> Assets->Bank intermediate.
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

	// Savings shares the SAME Assets->Bank intermediate as Checking.
	if byAcct["Savings"].CuentoParent != bankAsset {
		t.Errorf("Savings parent = %q, want %q", byAcct["Savings"].CuentoParent, bankAsset)
	}

	// The Assets->Bank intermediate exists once, type asset, parent = super-parent
	// "Assets", subs = UNION of Checking(US,MX)+Savings(US) = {MX,US}.
	ai, ok := byAcct[bankAsset]
	if !ok {
		t.Fatalf("Assets->Bank intermediate not emitted; got %v", keysOf(byAcct))
	}
	if ai.CuentoType != "asset" {
		t.Errorf("Assets->Bank type = %q, want asset", ai.CuentoType)
	}
	if ai.CuentoParent != superParentKey("Assets") {
		t.Errorf("Assets->Bank parent = %q, want %q", ai.CuentoParent, superParentKey("Assets"))
	}
	if ai.NameEN != "Bank" {
		t.Errorf("Assets->Bank name = %q, want Bank", ai.NameEN)
	}
	if len(ai.Subsidiaries) != 2 {
		t.Errorf("Assets->Bank subs = %v, want union US+MX", ai.Subsidiaries)
	}

	// SAME-typ-DIFFERENT-stmt: Rent (expense, typ Bank) has a DISTINCT Expenses->Bank
	// intermediate, keyed by (stmt,typ) so it does not collapse into Assets->Bank.
	bankExp := typParentKey("expense", "Bank")
	if byAcct["Rent"].CuentoParent != bankExp {
		t.Errorf("Rent parent = %q, want %q", byAcct["Rent"].CuentoParent, bankExp)
	}
	ei, ok := byAcct[bankExp]
	if !ok {
		t.Fatalf("Expenses->Bank intermediate not emitted")
	}
	if ei.CuentoType != "expense" || ei.CuentoParent != superParentKey("Expenses") {
		t.Errorf("Expenses->Bank wrong: type=%q parent=%q", ei.CuentoType, ei.CuentoParent)
	}
	if bankAsset == bankExp {
		t.Fatal("Assets->Bank and Expenses->Bank collapsed to one key")
	}

	// BLANK typ: Cash is parented DIRECTLY under the Assets super-parent (no tier).
	if byAcct["Cash"].CuentoParent != superParentKey("Assets") {
		t.Errorf("Cash (blank typ) parent = %q, want super-parent %q",
			byAcct["Cash"].CuentoParent, superParentKey("Assets"))
	}

	// Super-parents Assets + Expenses exist once, top-level (no parent), typed.
	assets, ok := byAcct[superParentKey("Assets")]
	if !ok {
		t.Fatalf("Assets super-parent not emitted")
	}
	if assets.CuentoParent != "" {
		t.Errorf("Assets super-parent has parent %q, want top-level", assets.CuentoParent)
	}
	if assets.CuentoType != "asset" || assets.NameEN != "Assets" {
		t.Errorf("Assets super-parent wrong: type=%q name=%q", assets.CuentoType, assets.NameEN)
	}
	if len(assets.Subsidiaries) != 2 { // union of everything asset (US+MX)
		t.Errorf("Assets super-parent subs = %v, want union US+MX", assets.Subsidiaries)
	}
	exp := byAcct[superParentKey("Expenses")]
	if exp.CuentoType != "expense" || exp.CuentoParent != "" {
		t.Errorf("Expenses super-parent wrong: type=%q parent=%q", exp.CuentoType, exp.CuentoParent)
	}

	// Exact row set: 4 leaves + 2 intermediates (Assets->Bank, Expenses->Bank)
	// + 2 super-parents (Assets, Expenses) = 8 distinct rows, none duplicated.
	if len(rows) != 8 {
		t.Errorf("emitted %d rows, want 8: %v", len(rows), keysOf(byAcct))
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
// records under many typs) keeps its first-sighting parent chain, but the chosen
// intermediate + super-parent must carry a SUPERSET of the leaf's full subsidiary
// union (rule 7 / Z12: parent subs superset of children -- else the go-live build's
// CreateAccount / cuento check reject the chain). The leaf appears as (A,Bank,US)
// then (A,Transfer,MX); it must stay parented under (asset,Bank) whose subs include
// BOTH US and MX.
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
	if !subset(leaf.Subsidiaries, inter.Subsidiaries) {
		t.Errorf("intermediate subs %v are not a superset of leaf subs %v", inter.Subsidiaries, leaf.Subsidiaries)
	}
	super := byAcct[superParentKey("Assets")]
	if !subset(leaf.Subsidiaries, super.Subsidiaries) {
		t.Errorf("super-parent subs %v are not a superset of leaf subs %v", super.Subsidiaries, leaf.Subsidiaries)
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
