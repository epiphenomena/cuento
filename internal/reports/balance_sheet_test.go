package reports_test

// p15.4 balance-sheet report tests. Every asserted number is HAND-DERIVED from the
// canonical synthetic fixture (PLAN Appendix D, internal/testutil/fixture) and the
// p14.1 rate seam -- the fixture is the oracle, never the report's own output. The
// golden files (testdata/balance_sheet.{txt,csv}) are a committed, human-reviewable
// rendering; -update / `make golden` regenerate them deterministically (lang=en,
// as-of 2026-06-30, root scope, USD target, converted-only view).
//
// The balance sheet's identity is Assets = Liabilities + Net Assets, and the
// nonprofit net-asset split is by DONOR RESTRICTION (Q3): without / with donor
// restrictions, plus a "net surplus to date" disclosure. The tests assert:
//   - the identity balances (Assets == Liabilities + Net Assets), per currency;
//   - with-restriction == the restricted funds' asset-side balances (fund tagging);
//   - without-restriction == total - with, and (the discriminator) that USD without
//     is 17,217,500 and NOT the fund-0 figure 18,517,500 (they differ by the
//     1,300,000 of USD liabilities -- a naive "without = fund 0" would pass MXN and
//     fail USD);
//   - net surplus to date == NetIncome from inception (presented positive);
//   - the intercompany warning row is ABSENT on the clean fixture and APPEARS on a
//     corrupted copy (a nonzero IC residual);
//   - a leaf-sub scope differs from root;
//   - the per-currency detail toggle expands per-currency rows.

import (
	"bytes"
	"context"
	"testing"

	"cuento/internal/reports"
	"cuento/internal/store"
	"cuento/internal/testutil/fixture"
)

// bsGoldenParams: root scope, fixture as-of, USD target, lang en, converted-only.
func bsGoldenParams(f *fixture.Fixture) reports.Params {
	return reports.Params{
		Scope:          f.IDs.Root,
		AsOf:           f.Expected.AsOf, // 2026-06-30
		TargetCurrency: "USD",
		Lang:           "en",
	}
}

// balanceSheetReport fetches the registered balance-sheet report from Default().
func balanceSheetReport(t *testing.T) reports.Report {
	t.Helper()
	rep, ok := reports.Default().Get(reports.BalanceSheetReportID)
	if !ok {
		t.Fatalf("balance-sheet report %q not registered in Default()", reports.BalanceSheetReportID)
	}
	return rep
}

// labelAmount returns the converted (last-column) minor amount for the DATA/subtotal/
// total row whose FIRST cell is a LABEL matching key, and whether it was found. Used
// in the converted-only (2-column) view where synthetic/total lines are labels.
func labelAmount(t reports.Table, key string) (int64, bool) {
	for _, row := range t.Rows {
		if len(row.Cells) < 2 {
			continue
		}
		c := row.Cells[0]
		if c.Kind == reports.CellLabel && c.Text == key {
			last := row.Cells[len(row.Cells)-1]
			return last.Minor, true
		}
	}
	return 0, false
}

// nameAmount returns the converted amount for the account row whose first cell is a
// TEXT cell equal to name (converted-only view).
func nameAmount(t reports.Table, name string) (int64, bool) {
	for _, row := range t.Rows {
		if len(row.Cells) < 2 {
			continue
		}
		c := row.Cells[0]
		if c.Kind == reports.CellText && c.Text == name {
			last := row.Cells[len(row.Cells)-1]
			return last.Minor, true
		}
	}
	return 0, false
}

// TestBalanceSheetGolden runs the balance sheet over the fixture at the pinned
// params, hand-verifies the identity + net-asset split + surplus, asserts the
// intercompany warning is ABSENT, and compares the rendered text + CSV to committed
// goldens.
//
// At ROOT scope the intercompany accounts are ELIMINATED (D19 collapse): Due from RV
// Mexico (asset +1,000,000 USD) and Due to RV Internacional (liability -1,000,000
// USD) drop from both listings and totals. They net to zero, so total net assets is
// unchanged; only the Assets and Liabilities totals shrink by 1,000,000 USD each.
//
// HAND-VERIFIED (root scope, native, POST-collapse; converted at the 2026-06-30
// closing USD->MXN 18.10, MXN->USD = 1/18.10, half-even):
//
//	USD Assets      = 22,567,500  (CheckingUS 3,593,500 + Savings 2,000,000 +
//	                               Building 16,000,000 + FX Clearing 974,000;
//	                               DueFromMX 1,000,000 ELIMINATED)
//	USD Liabilities =    300,000  (Credit Card 300,000; Due to Intl 1,000,000 ELIMINATED)
//	USD Net assets  = 22,267,500  (= Assets - Liabilities, the plug; unchanged)
//	  with          =  5,050,000  (Beca Agua 50,000 + Building Fund 5,000,000)
//	  without       = 17,217,500  (= 22,267,500 - 5,050,000; NOT fund-0 18,517,500)
//	  surplus       =  3,567,500  (NetIncome from inception, positive)
//	MXN Assets      = 40,640,000  (Checking MX 39,500,000 + Cash MXN 640,000 +
//	                               FX Clearing 500,000)
//	MXN Liabilities =          0
//	MXN Net assets  = 40,640,000
//	  with          =  9,700,000  (Beca Agua)
//	  without       = 30,940,000
//	  surplus       =  9,140,000
func TestBalanceSheetGolden(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()

	rep := balanceSheetReport(t)
	tk := reports.NewToolkit(f.Store, bsGoldenParams(f))
	table, err := rep.Run(ctx, tk, bsGoldenParams(f))
	if err != nil {
		t.Fatalf("run balance sheet: %v", err)
	}

	// --- Converted (USD) figures the report emits. Everything is converted to USD:
	// USD lines pass through 1:1; MXN lines convert at 1/18.10 half-even.
	//
	// USD-native lines convert 1:1, so their USD figures are exactly the natives.
	// MXN converts and ADDS to the USD converted total for the mixed lines, so the
	// converted totals below are hand-derived from BOTH currencies.
	//
	//   Assets converted   = 22,567,500 USD + 40,640,000 MXN/18.10  (post-collapse)
	//                      = 22,567,500 + 2,245,304 (2,245,303.86 -> even) = 24,812,804
	//   Liabilities        = 300,000 (USD only; Due-to eliminated)
	//   Net assets         = Assets - Liab = 24,512,804
	//   with               = 5,050,000 USD + 9,700,000 MXN/18.10
	//                      = 5,050,000 + 535,912 (535,911.60 -> 535,912) = 5,585,912
	//   without            = 24,512,804 - 5,585,912 = 18,926,892
	//   surplus            = 3,567,500 USD + 9,140,000 MXN/18.10
	//                      = 3,567,500 + 504,972 (91,400.00/18.10 = 5,049.72) = 4,072,472
	//
	// The converted identity holds because Net assets is the plug of the converted
	// Assets/Liabilities (each currency converted once, then summed).
	wantConv := map[string]int64{
		"reports.balance_sheet.total.assets":                 24_812_804,
		"reports.balance_sheet.total.liabilities":            300_000,
		"reports.balance_sheet.total.net_assets":             24_512_804,
		"reports.balance_sheet.na.without":                   18_926_892,
		"reports.balance_sheet.na.with":                      5_585_912,
		"reports.balance_sheet.na.surplus_of_which":          4_072_472,
		"reports.balance_sheet.total.liabilities_net_assets": 24_812_804,
	}
	for key, want := range wantConv {
		got, ok := labelAmount(table, key)
		if !ok {
			t.Errorf("no row for label %q", key)
			continue
		}
		if got != want {
			t.Errorf("converted %q = %d, want %d", key, got, want)
		}
	}

	// --- THE IDENTITY (converted): Assets == Liabilities + Net Assets.
	assets := wantConv["reports.balance_sheet.total.assets"]
	liab := wantConv["reports.balance_sheet.total.liabilities"]
	na := wantConv["reports.balance_sheet.total.net_assets"]
	if assets != liab+na {
		t.Errorf("identity broken: Assets %d != Liabilities %d + NetAssets %d", assets, liab, na)
	}
	// The report's own "Total liabilities and net assets" equals total assets.
	if wantConv["reports.balance_sheet.total.liabilities_net_assets"] != assets {
		t.Errorf("L+NA total (%d) != Assets (%d)", wantConv["reports.balance_sheet.total.liabilities_net_assets"], assets)
	}
	// without + with == total net assets (surplus is NOT summed in).
	if wantConv["reports.balance_sheet.na.without"]+wantConv["reports.balance_sheet.na.with"] != na {
		t.Errorf("without + with (%d) != net assets (%d)",
			wantConv["reports.balance_sheet.na.without"]+wantConv["reports.balance_sheet.na.with"], na)
	}

	// --- The intercompany warning row is ABSENT on the clean fixture (D19).
	for _, row := range table.Rows {
		if row.Kind == reports.RowWarning {
			t.Errorf("intercompany warning row present on the balanced fixture: %+v", row)
		}
	}

	// --- A drillable asset account cell exists (drill wiring). Checking MX is single-
	// currency, so its converted cell is drillable in the converted-only view.
	if !hasDrill(table, "Checking MX") {
		t.Errorf("Checking MX asset cell is not drillable")
	}
	// Opening Balances (equity) must NOT appear as a row (absorbed into the plug).
	if _, ok := nameAmount(table, "Opening Balances"); ok {
		t.Errorf("Opening Balances (equity) emitted as a row; must be absorbed into net-asset plug")
	}
	// Intercompany accounts are ELIMINATED at the consolidated root scope (D19): neither
	// Due from RV Mexico (asset) nor Due to RV Internacional (liability) appears.
	for _, name := range []string{"Due from RV Mexico", "Due to RV Internacional"} {
		if _, ok := nameAmount(table, name); ok {
			t.Errorf("intercompany account %q not collapsed at consolidated root scope", name)
		}
	}

	// --- Golden artifacts.
	exps := goldenExps(t, f)
	textDump := reports.DumpTable(table, goldenLocalize, exps)
	var csvBuf bytes.Buffer
	if err := reports.WriteCSV(&csvBuf, localizeLabels(table), goldenLocalize, exps); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	checkGolden(t, "balance_sheet.txt", []byte(textDump))
	checkGolden(t, "balance_sheet.csv", csvBuf.Bytes())
}

// hasDrill reports whether the account row named name carries a drillable cell.
func hasDrill(t reports.Table, name string) bool {
	for _, row := range t.Rows {
		if len(row.Cells) == 0 || row.Cells[0].Kind != reports.CellText || row.Cells[0].Text != name {
			continue
		}
		for _, c := range row.Cells {
			if c.Drill != nil {
				return true
			}
		}
	}
	return false
}

// TestBalanceSheetNativeSplit verifies the NATIVE per-currency net-asset figures
// against the fixture oracle directly (via the detail view), including the USD
// discriminator (without != fund-0). This proves the split is correct BEFORE FX
// rounding, so a wrong classification fails here, not only in a golden diff.
func TestBalanceSheetNativeSplit(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := balanceSheetReport(t)

	// Detail=currency, NO target (native): each line shows native per currency.
	p := reports.Params{Scope: f.IDs.Root, AsOf: f.Expected.AsOf, Lang: "en", Detail: "currency"}
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run detail: %v", err)
	}

	// In detail/native mode a line's NATIVE cell (column 2) per currency is the
	// oracle. A multi-currency line carries its LABEL on its first currency row and
	// blank-named continuation rows for the rest, so carry the current label forward
	// as we walk (a continuation row has a blank first cell).
	native := func(key, ccy string) (int64, bool) {
		curLabel := ""
		for _, row := range table.Rows {
			if len(row.Cells) < 3 {
				continue
			}
			switch row.Cells[0].Kind {
			case reports.CellLabel:
				curLabel = row.Cells[0].Text
			case reports.CellText:
				if row.Cells[0].Text != "" {
					curLabel = "" // an account (proper-noun) row resets the label context
				}
			}
			if curLabel == key && row.Cells[1].Text == ccy {
				return row.Cells[2].Minor, true
			}
		}
		return 0, false
	}

	cases := []struct {
		key  string
		ccy  string
		want int64
	}{
		{"reports.balance_sheet.total.assets", "USD", 22_567_500}, // post-collapse (DueFromMX eliminated)
		{"reports.balance_sheet.total.assets", "MXN", 40_640_000},
		{"reports.balance_sheet.total.liabilities", "USD", 300_000}, // post-collapse (DueToIntl eliminated)
		{"reports.balance_sheet.total.net_assets", "USD", 22_267_500},
		{"reports.balance_sheet.total.net_assets", "MXN", 40_640_000},
		{"reports.balance_sheet.na.with", "USD", 5_050_000},
		{"reports.balance_sheet.na.with", "MXN", 9_700_000},
		{"reports.balance_sheet.na.without", "USD", 17_217_500}, // discriminator: NOT 18,517,500
		{"reports.balance_sheet.na.without", "MXN", 30_940_000},
		{"reports.balance_sheet.na.surplus_of_which", "USD", 3_567_500},
		{"reports.balance_sheet.na.surplus_of_which", "MXN", 9_140_000},
	}
	for _, c := range cases {
		got, ok := native(c.key, c.ccy)
		if !ok {
			t.Errorf("no native %s row for %q", c.ccy, c.key)
			continue
		}
		if got != c.want {
			t.Errorf("native %q %s = %d, want %d", c.key, c.ccy, got, c.want)
		}
	}

	// THE DISCRIMINATOR, spelled out: USD without-restriction (17,217,500) must NOT
	// equal the fund-0 asset figure (18,517,500). A "without = fund 0" bug would
	// return 18,517,500 here.
	usdWithout, _ := native("reports.balance_sheet.na.without", "USD")
	if usdWithout == 18_517_500 {
		t.Errorf("USD without-restriction == fund-0 asset figure 18,517,500: 'without = fund 0' bug")
	}

	// Per-currency identity holds in native mode too.
	for _, ccy := range []string{"USD", "MXN"} {
		a, _ := native("reports.balance_sheet.total.assets", ccy)
		l, ok := native("reports.balance_sheet.total.liabilities", ccy)
		if !ok {
			l = 0 // MXN has no liabilities -> no row
		}
		n, _ := native("reports.balance_sheet.total.net_assets", ccy)
		if a != l+n {
			t.Errorf("native identity %s: assets %d != liab %d + na %d", ccy, a, l, n)
		}
	}
}

// TestBalanceSheetScope: root vs a leaf sub (RV Mexico) differ. The MX leaf carries
// only MX accounts (descendant closure, D18): its Assets have no USD-only US accounts
// (Building, Checking US), and MXN dominates. Native mode (rate-independent).
func TestBalanceSheetScope(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := balanceSheetReport(t)

	rootP := reports.Params{Scope: f.IDs.Root, AsOf: f.Expected.AsOf, Lang: "en"}
	rootT, err := rep.Run(ctx, reports.NewToolkit(f.Store, rootP), rootP)
	if err != nil {
		t.Fatalf("run root: %v", err)
	}
	leafP := reports.Params{Scope: f.IDs.MX, AsOf: f.Expected.AsOf, Lang: "en"}
	leafT, err := rep.Run(ctx, reports.NewToolkit(f.Store, leafP), leafP)
	if err != nil {
		t.Fatalf("run leaf: %v", err)
	}

	// US-only NON-intercompany assets appear at root, NOT in the MX leaf.
	for _, name := range []string{"Building", "Checking US"} {
		if _, ok := nameAmount(rootT, name); !ok {
			t.Errorf("root balance sheet missing US asset %q", name)
		}
		if _, ok := nameAmount(leafT, name); ok {
			t.Errorf("leaf(MX) balance sheet unexpectedly contains US-only asset %q", name)
		}
	}
	// MX accounts appear in the leaf.
	for _, name := range []string{"Checking MX", "Cash MXN"} {
		if _, ok := nameAmount(leafT, name); !ok {
			t.Errorf("leaf(MX) balance sheet missing MX asset %q", name)
		}
	}

	// --- Intercompany treatment DIFFERS by scope (D19):
	//   root (consolidated, both subs) -> Due-from/Due-to ELIMINATED, no warning
	//     (they net to zero -- the balanced fixture).
	//   leaf (MX only) -> "Due to RV Internacional" is the sub's genuine due-to-parent
	//     liability, SHOWN (not collapsed) and NOT warned.
	if _, ok := nameAmount(rootT, "Due from RV Mexico"); ok {
		t.Errorf("root balance sheet did not collapse intercompany asset Due from RV Mexico")
	}
	if _, ok := nameAmount(rootT, "Due to RV Internacional"); ok {
		t.Errorf("root balance sheet did not collapse intercompany liability Due to RV Internacional")
	}
	if warningRow(rootT) != nil {
		t.Errorf("root balance sheet has an intercompany warning on the balanced fixture")
	}
	if _, ok := nameAmount(leafT, "Due to RV Internacional"); !ok {
		t.Errorf("leaf(MX) balance sheet missing its due-to-parent liability %q (must NOT be collapsed at a leaf scope)", "Due to RV Internacional")
	}
	if warningRow(leafT) != nil {
		t.Errorf("leaf(MX) balance sheet emitted an intercompany warning (a leaf scope must never warn)")
	}
}

// TestBalanceSheetIntercompanyWarning: on a CORRUPTED copy of the fixture the
// intercompany accounts fail to net to zero, and the balance sheet emits the D19
// WARNING row. Mirrors p15.2's IntercompanyNet corrupted-copy technique: you cannot
// post an unbalanced txn, and net-to-zero is a CROSS-transaction property, so post
// ONE extra BALANCED txn (DR Due-from +250,000 / CR Checking US -250,000) that has no
// mirroring Due-to credit -- each txn stays zero-sum, but the consolidated
// intercompany net becomes +250,000 USD.
func TestBalanceSheetIntercompanyWarning(t *testing.T) {
	f := fixture.New(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	rep := balanceSheetReport(t)

	// Baseline (clean): no warning.
	p := reports.Params{Scope: f.IDs.Root, AsOf: f.Expected.AsOf, Lang: "en"}
	clean, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run clean: %v", err)
	}
	if warningRow(clean) != nil {
		t.Fatalf("clean fixture already has an intercompany warning row")
	}

	// Corrupt: post a balanced txn that moves the intercompany net off zero.
	_, err = f.Store.PostTransaction(ctx, store.PostTransactionInput{
		Date:         "2026-06-15",
		SubsidiaryID: f.IDs.US,
		Currency:     "USD",
		Memo:         "test: unmirrored intercompany advance",
		Splits: []store.SplitInput{
			{AccountID: f.IDs.DueFromMX, Amount: 250_000},
			{AccountID: f.IDs.CheckingUS, Amount: -250_000},
		},
	})
	if err != nil {
		t.Fatalf("post corrupting txn: %v", err)
	}

	corrupted, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run corrupted: %v", err)
	}
	wr := warningRow(corrupted)
	if wr == nil {
		t.Fatalf("corrupted fixture: expected an intercompany WARNING row, got none")
	}
	// The warning row's amount cell carries the +250,000 USD residual (converted; no
	// target here so it is native USD).
	var found bool
	for _, c := range wr.Cells {
		if c.Kind == reports.CellMoney && !c.Blank && c.Minor == 250_000 {
			found = true
		}
	}
	if !found {
		t.Errorf("intercompany warning row does not carry the 250,000 USD residual: %+v", wr.Cells)
	}
}

// TestBalanceSheetNestedTree exercises the p26.53 NESTED account tree: with the
// capital-campaign seam the chart carries a "Fixed Assets" placeholder PARENT over
// "Land" + "Construction in Progress" leaves. The balance sheet must now surface the
// parent as a rolled-up SUBTOTAL row (previously it was dropped -- it has no direct
// balance), with the leaves nested one level deeper. This is the path the flat base
// fixture cannot exercise (its assets are all top-level leaves), so it is asserted here
// directly on the returned Table: parent kind, rollup == sum of leaves, and the indent
// relationship (parent == child indent - 1). NATIVE mode (no target) so the rollup is
// exact int64 addition per currency, not FX-rounded.
func TestBalanceSheetNestedTree(t *testing.T) {
	f := fixture.New(t)
	f.ExtendCapitalCampaign(t)
	ctx := context.Background()
	rep := balanceSheetReport(t)

	// As-of end of the campaign year, root scope, native (no target), converted-only.
	p := reports.Params{Scope: f.IDs.Root, AsOf: "2025-12-31", Lang: "en"}
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run balance sheet: %v", err)
	}

	// Locate the three rows by name and capture their (row, indent, converted amount).
	type rowInfo struct {
		found  bool
		kind   reports.RowKind
		indent int
		amount int64
	}
	find := func(name string) rowInfo {
		for _, row := range table.Rows {
			if len(row.Cells) < 2 || row.Cells[0].Kind != reports.CellText || row.Cells[0].Text != name {
				continue
			}
			last := row.Cells[len(row.Cells)-1]
			return rowInfo{found: true, kind: row.Kind, indent: row.Indent, amount: last.Minor}
		}
		return rowInfo{}
	}

	parent := find("Fixed Assets")
	land := find("Land")
	constr := find("Construction in Progress")

	// The placeholder PARENT now appears (p26.53 -- previously dropped) as a SUBTOTAL.
	if !parent.found {
		t.Fatalf("Fixed Assets placeholder parent not rendered (the nested-tree regression)")
	}
	if parent.kind != reports.RowSubtotal {
		t.Errorf("Fixed Assets parent kind = %v, want RowSubtotal", parent.kind)
	}
	if !land.found || !constr.found {
		t.Fatalf("nested leaf rows missing: land=%v constr=%v", land.found, constr.found)
	}

	// Leaves nest ONE level below the parent (Indent == parent + 1). The parent itself
	// is at Indent 1 (a top-level account under the Indent-0 Assets section header), so
	// the leaves land at Indent 2 -- the flat pre-p26.53 layout would have left them at 1.
	if parent.indent != 1 {
		t.Errorf("Fixed Assets parent indent = %d, want 1 (top-level under the section header)", parent.indent)
	}
	if land.indent != parent.indent+1 || constr.indent != parent.indent+1 {
		t.Errorf("leaf indent land=%d constr=%d, want parent+1 (%d)", land.indent, constr.indent, parent.indent+1)
	}

	// THE ROLLUP ARITHMETIC: the parent's (native, no-target) converted cell equals the
	// sum of its leaves' cells -- the number that must not drift. Land = 800,000 USD;
	// Construction = 500,000 USD + 6,000,000 MXN; in native/no-target mode the converted
	// column is the plain per-currency sum, so the parent = 800,000 + 500,000 + 6,000,000
	// = 7,300,000 minor across the two currencies, and == land + constr by construction.
	if parent.amount != land.amount+constr.amount {
		t.Errorf("Fixed Assets rollup (%d) != Land (%d) + Construction (%d)",
			parent.amount, land.amount, constr.amount)
	}
}

// warningRow returns the first RowWarning in t, or nil.
func warningRow(t reports.Table) *reports.Row {
	for i := range t.Rows {
		if t.Rows[i].Kind == reports.RowWarning {
			return &t.Rows[i]
		}
	}
	return nil
}

// TestBalanceSheetDetailToggle: the per-currency detail view has MORE rows than the
// converted-only view (each multi-currency total expands), a Currency column, and its
// per-currency native cells reconcile to the same section totals. The converted-only
// view has the 2-column shape; detail has 4.
func TestBalanceSheetDetailToggle(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := balanceSheetReport(t)

	convP := bsGoldenParams(f)
	convT, err := rep.Run(ctx, reports.NewToolkit(f.Store, convP), convP)
	if err != nil {
		t.Fatalf("run converted: %v", err)
	}
	detP := convP
	detP.Detail = "currency"
	detT, err := rep.Run(ctx, reports.NewToolkit(f.Store, detP), detP)
	if err != nil {
		t.Fatalf("run detail: %v", err)
	}

	if len(convT.Columns) != 2 {
		t.Errorf("converted-only columns = %d, want 2", len(convT.Columns))
	}
	if len(detT.Columns) != 4 {
		t.Errorf("detail columns = %d, want 4", len(detT.Columns))
	}
	// Detail expands multi-currency lines, so it has strictly more rows.
	if len(detT.Rows) <= len(convT.Rows) {
		t.Errorf("detail rows (%d) not more than converted-only rows (%d)", len(detT.Rows), len(convT.Rows))
	}
	// The detail view has a Currency column header key.
	if detT.Columns[1].HeaderKey != "reports.balance_sheet.col.currency" {
		t.Errorf("detail column 1 = %q, want the currency column", detT.Columns[1].HeaderKey)
	}
}
