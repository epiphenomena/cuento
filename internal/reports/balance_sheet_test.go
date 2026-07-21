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
//     is 16,243,500 and NOT the fund-0 figure 17,543,500 (they differ by the
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
		Scope:          reports.SubsidiaryID(f.IDs.Root),
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

// labelAmount returns the PRIMARY (selected as-of) converted minor amount for the
// DATA/subtotal/total row whose FIRST cell is a LABEL matching key, and whether it was
// found. In the p18 multi-period view column 1 (Cells[1]) is the selected as-of (the
// old single-column figure); the older year-end columns follow to the RIGHT, so the
// oracle reads the FIRST value cell, NOT the last.
func labelAmount(t reports.Table, key string) (int64, bool) {
	for _, row := range t.Rows {
		if len(row.Cells) < 2 {
			continue
		}
		c := row.Cells[0]
		if c.Kind == reports.CellLabel && c.Text == key {
			return row.Cells[1].Minor, true
		}
	}
	return 0, false
}

// nameAmount returns the PRIMARY (selected as-of) converted amount for the account row
// whose first cell is a TEXT cell equal to name (converted-only view). Reads the first
// value cell (Cells[1]) -- the selected as-of column (p18).
func nameAmount(t reports.Table, name string) (int64, bool) {
	for _, row := range t.Rows {
		if len(row.Cells) < 2 {
			continue
		}
		c := row.Cells[0]
		if c.Kind == reports.CellText && c.Text == name {
			return row.Cells[1].Minor, true
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
//	USD Assets      = 21,593,500  (CheckingUS 3,593,500 + Savings 2,000,000 +
//	                               Building 16,000,000; FX Clearing 974,000 is
//	                               EQUITY-class now; DueFromMX 1,000,000 ELIMINATED)
//	USD Liabilities =    300,000  (Credit Card 300,000; Due to Intl 1,000,000 ELIMINATED)
//	USD Net assets  = 21,293,500  (= Assets - Liabilities, the plug; FX Clearing's
//	                               974,000 debit is now a contra-equity component)
//	  with          =  1,050,000  (Beca Agua 50,000 + Building Fund MONETARY 1,000,000;
//	                               the Building 4,000,000 is DEPLOYED into a non-monetary
//	                               asset -> released from restriction, p-golive)
//	  without       = 20,243,500  (= 21,293,500 - 1,050,000; absorbs the 4,000,000 release)
//	  surplus       =  3,567,500  (NetIncome from inception, positive)
//	MXN Assets      = 40,140,000  (Checking MX 39,500,000 + Cash MXN 640,000;
//	                               FX Clearing 500,000 is EQUITY-class now)
//	MXN Liabilities =          0
//	MXN Net assets  = 40,140,000
//	  with          =  9,700,000  (Beca Agua; cash-only, nothing deployed to release)
//	  without       = 30,440,000
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
	//   Assets converted   = 21,593,500 USD + 40,140,000 MXN/18.10  (post-collapse;
	//                        FX Clearing is EQUITY-class now, out of the asset totals)
	//                      = 21,593,500 + 2,217,680 (2,217,679.56 -> even) = 23,811,180
	//   Liabilities        = 300,000 (USD only; Due-to eliminated)
	//   Net assets         = Assets - Liab = 23,511,180
	//   with               = 1,050,000 USD + 9,700,000 MXN/18.10  (Building Fund monetary
	//                        1,000,000; the Building 4,000,000 released, p-golive)
	//                      = 1,050,000 + 535,912 (535,911.60 -> 535,912) = 1,585,912
	//   without            = 23,511,180 - 1,585,912 = 21,925,268  (absorbs the release)
	//   surplus            = 3,567,500 USD + 9,140,000 MXN/18.10
	//                      = 3,567,500 + 504,972 (91,400.00/18.10 = 5,049.72) = 4,072,472
	//
	// The converted identity holds because Net assets is the plug of the converted
	// Assets/Liabilities (each currency converted once, then summed).
	wantConv := map[string]int64{
		"reports.balance_sheet.total.assets":                 23_811_180,
		"reports.balance_sheet.total.liabilities":            300_000,
		"reports.balance_sheet.total.net_assets":             23_511_180,
		"reports.balance_sheet.na.without":                   21_925_268,
		"reports.balance_sheet.na.with":                      1_585_912,
		"reports.balance_sheet.na.surplus_of_which":          4_072_472,
		"reports.balance_sheet.total.liabilities_net_assets": 23_811_180,
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

// TestBalanceSheetMultiPeriod (p18) verifies the multi-period column shape: the
// statement is a SERIES of as-of value columns, left to right -- column 1 is the
// selected as-of, then each prior December 31 strictly before it, back to the earliest
// posting date. On the fixture (as-of 2026-06-30, ledger from 2025-01-01) the series is
// EXACTLY [2026-06-30, 2025-12-31]: 2024-12-31 is before the earliest posting so the
// walk stops there (2026-12-31 is >= the as-of, so the current year-end is not repeated).
//
// It asserts: (1) the column headers are Line + those two ISO dates in order; (2) column
// 1 (the selected as-of) reproduces the OLD single-as-of converted figures exactly -- the
// same numbers TestBalanceSheetGolden hand-verifies; (3) the prior year-end column
// (2025-12-31) carries its own, DIFFERENT snapshot (the fixture posts activity in H1 2026,
// so at least the change-in-net-assets-to-date figure moves between the two columns).
func TestBalanceSheetMultiPeriod(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := balanceSheetReport(t)

	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, bsGoldenParams(f)), bsGoldenParams(f))
	if err != nil {
		t.Fatalf("run balance sheet: %v", err)
	}

	// (1) Columns: Line + two as-of date columns, in descending-date order.
	wantHeaders := []string{"reports.balance_sheet.col.line", "2026-06-30", "2025-12-31"}
	if len(table.Columns) != len(wantHeaders) {
		t.Fatalf("columns = %d, want %d %v; got headers %v",
			len(table.Columns), len(wantHeaders), wantHeaders, columnHeaderKeys(table))
	}
	for i, want := range wantHeaders {
		if table.Columns[i].HeaderKey != want {
			t.Errorf("column %d header = %q, want %q", i, table.Columns[i].HeaderKey, want)
		}
	}

	// (2) Column 1 (Cells[1], the selected as-of) == the old single-as-of figures. These
	// are exactly the converted numbers TestBalanceSheetGolden hand-derives from the oracle.
	wantPrimary := map[string]int64{
		"reports.balance_sheet.total.assets":                 23_811_180,
		"reports.balance_sheet.total.liabilities":            300_000,
		"reports.balance_sheet.total.net_assets":             23_511_180,
		"reports.balance_sheet.na.without":                   21_925_268,
		"reports.balance_sheet.na.with":                      1_585_912,
		"reports.balance_sheet.total.liabilities_net_assets": 23_811_180,
	}
	for key, want := range wantPrimary {
		got, ok := labelAmount(table, key) // labelAmount reads Cells[1] = the selected as-of
		if !ok {
			t.Errorf("no row for label %q", key)
			continue
		}
		if got != want {
			t.Errorf("primary-column %q = %d, want %d (must equal the old single-as-of figure)", key, got, want)
		}
	}

	// (3) The prior year-end column (2025-12-31, Cells[2]) is its OWN snapshot: the
	// change-in-net-assets-to-date at 2025-12-31 differs from the 2026-06-30 figure (the
	// fixture posts revenue/expense activity in H1 2026), so the two columns are distinct.
	priorSurplus, ok := labelColumn(table, "reports.balance_sheet.na.surplus_of_which", 2)
	if !ok {
		t.Fatalf("no surplus row for the prior year-end column")
	}
	primarySurplus, _ := labelColumn(table, "reports.balance_sheet.na.surplus_of_which", 1)
	if priorSurplus == primarySurplus {
		t.Errorf("prior year-end surplus (%d) == selected-as-of surplus (%d): the columns are not distinct snapshots",
			priorSurplus, primarySurplus)
	}
	// EVERY column must foot in CONVERTED space, both the balance-sheet identity
	// (A == L + NA) and the net-asset split (without + with == Total NA). The split
	// footing is the discriminator the residual-derivation fixes: converting without/with/
	// NA independently drifts a half-even cent at the older year-end's rate, so a naive
	// per-figure conversion breaks without + with == NA there (p15.5 "footing wins").
	for col := 1; col <= 2; col++ {
		assets, _ := labelColumn(table, "reports.balance_sheet.total.assets", col)
		lPlusNA, _ := labelColumn(table, "reports.balance_sheet.total.liabilities_net_assets", col)
		if assets != lPlusNA {
			t.Errorf("column %d identity broken: A %d != L+NA %d", col, assets, lPlusNA)
		}
		without, _ := labelColumn(table, "reports.balance_sheet.na.without", col)
		with, _ := labelColumn(table, "reports.balance_sheet.na.with", col)
		na, _ := labelColumn(table, "reports.balance_sheet.total.net_assets", col)
		if without+with != na {
			t.Errorf("column %d net-asset split does not foot: without %d + with %d = %d != Total NA %d",
				col, without, with, without+with, na)
		}
	}
}

// TestBalanceSheetMultiPeriodBeforeInception (p18 guard): an as-of date BEFORE the
// earliest posting (the fixture ledger starts 2025-01-01) must NOT walk year-ends back
// forever -- the first candidate prior year-end already precedes the earliest posting, so
// the walk stops immediately and the statement is the single selected as-of column. This
// exercises the "stop before min" break with a real fixture (no degenerate/negative loop).
func TestBalanceSheetMultiPeriodBeforeInception(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := balanceSheetReport(t)

	// As-of 2024-06-30 is before the earliest posting (2025-01-01); the prior year-end
	// candidate 2023-12-31 precedes the min, so no year-end columns are added.
	p := reports.Params{Scope: reports.SubsidiaryID(f.IDs.Root), AsOf: "2024-06-30", Lang: "en", TargetCurrency: "USD"}
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run balance sheet before inception: %v", err)
	}
	// Line + exactly one as-of value column (no year-end fan-out before the ledger starts).
	if len(table.Columns) != 2 {
		t.Errorf("before-inception columns = %d, want 2 (Line + single as-of); headers %v", len(table.Columns), columnHeaderKeys(table))
	}
	if len(table.Columns) >= 2 && table.Columns[1].HeaderKey != "2024-06-30" {
		t.Errorf("before-inception value column header = %q, want 2024-06-30", table.Columns[1].HeaderKey)
	}
}

// columnHeaderKeys returns the header keys of every column (for diagnostics).
func columnHeaderKeys(t reports.Table) []string {
	out := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		out[i] = c.HeaderKey
	}
	return out
}

// labelColumn returns the minor amount in value-column `col` (1-based over the value
// columns; col 1 is the leftmost/selected as-of) for the row whose first cell is a LABEL
// matching key.
func labelColumn(t reports.Table, key string, col int) (int64, bool) {
	for _, row := range t.Rows {
		if len(row.Cells) <= col {
			continue
		}
		c := row.Cells[0]
		if c.Kind == reports.CellLabel && c.Text == key {
			return row.Cells[col].Minor, true
		}
	}
	return 0, false
}

// TestBalanceSheetFundFilter runs the Statement of Position NARROWED to the Building
// Fund (p15.4 fund selector) and hand-verifies that it presents that single fund's OWN
// position, that the identity A = L + NA holds for the fund (its net assets == its fund
// balance), and that a single-fund view has NO intercompany elimination.
//
// HAND-VERIFIED (Building Fund, USD-native, as-of 2026-06-30 — the fixture oracle):
// the fund is US-only, restricted ("purpose"), and its ledger is:
//   - Contributions (revenue) received     5,000,000 minor (a credit)
//   - Building purchase: Building +4,000,000 (asset) / Checking US -4,000,000 (asset)
//
// so its per-account balances are:
//
//	Checking US   +1,000,000  (asset)   ┐
//	Building      +4,000,000  (asset)   ┴ Assets total 5,000,000
//	Liabilities            0
//	Net assets (plug A-L)  5,000,000  == FundBalancesAsOf{BuildingFund,USD} (list view)
//	  with donor restrictions = 1,000,000  (the fund IS restricted; with == its MONETARY
//	                                         balance = Checking US cash. The Building
//	                                         4,000,000 is DEPLOYED into a non-monetary
//	                                         asset -> released from restriction, p-golive)
//	  without                 = 4,000,000  (= 5,000,000 - 1,000,000; the released Building)
//	  surplus to date         = 5,000,000  (the Contributions revenue, presented positive)
//
// This is the p-golive ARTICULATION oracle: with (1,000,000) + without (4,000,000) ==
// total net assets (5,000,000) unchanged, and the released amount (4,000,000) is exactly
// the Building the fund capitalized — restriction satisfied on acquisition.
func TestBalanceSheetFundFilter(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := balanceSheetReport(t)

	// USD-native (no target currency) so every figure is the exact fixture minor unit.
	p := reports.Params{
		Scope: reports.SubsidiaryID(f.IDs.Root),
		AsOf:  f.Expected.AsOf, // 2026-06-30
		Lang:  "en",
		Fund:  reports.FundID(f.IDs.BuildingFund),
	}
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run fund-filtered balance sheet: %v", err)
	}

	want := map[string]int64{
		"reports.balance_sheet.total.assets":                 5_000_000,
		"reports.balance_sheet.total.liabilities":            0,
		"reports.balance_sheet.total.net_assets":             5_000_000,
		"reports.balance_sheet.na.with":                      1_000_000,
		"reports.balance_sheet.na.without":                   4_000_000,
		"reports.balance_sheet.na.surplus_of_which":          5_000_000,
		"reports.balance_sheet.total.liabilities_net_assets": 5_000_000,
	}
	for key, w := range want {
		got, ok := labelAmount(table, key)
		if !ok {
			t.Errorf("no row for label %q", key)
			continue
		}
		if got != w {
			t.Errorf("fund-filtered %q = %d, want %d", key, got, w)
		}
	}

	// The identity holds for the single fund: Assets == Liabilities + Net Assets.
	if want["reports.balance_sheet.total.assets"] !=
		want["reports.balance_sheet.total.liabilities"]+want["reports.balance_sheet.total.net_assets"] {
		t.Errorf("fund identity broken: A %d != L %d + NA %d",
			want["reports.balance_sheet.total.assets"],
			want["reports.balance_sheet.total.liabilities"],
			want["reports.balance_sheet.total.net_assets"])
	}
	// without + with == net assets.
	if want["reports.balance_sheet.na.without"]+want["reports.balance_sheet.na.with"] !=
		want["reports.balance_sheet.total.net_assets"] {
		t.Errorf("without + with != net assets for the fund")
	}

	// The fund's own asset accounts appear as rows; unrelated MX accounts do NOT (the
	// Building Fund is US-only, holds only Checking US + Building).
	if _, ok := nameAmount(table, "Building"); !ok {
		t.Errorf("Building asset row missing from the Building Fund statement")
	}
	if _, ok := nameAmount(table, "Checking MX"); ok {
		t.Errorf("Checking MX (not a Building Fund account) leaked into the fund statement")
	}

	// NO intercompany warning/elimination for a single-fund view (not a consolidation).
	for _, row := range table.Rows {
		if row.Kind == reports.RowWarning {
			t.Errorf("intercompany warning row present on a single-fund statement: %+v", row)
		}
	}
	for _, name := range []string{"Due from RV Mexico", "Due to RV Internacional"} {
		if _, ok := nameAmount(table, name); ok {
			t.Errorf("intercompany account %q appeared in a single-fund view (should be absent — the fund holds none)", name)
		}
	}
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
	p := reports.Params{Scope: reports.SubsidiaryID(f.IDs.Root), AsOf: f.Expected.AsOf, Lang: "en", Detail: "currency"}
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
		{"reports.balance_sheet.total.assets", "USD", 21_593_500},     // post-collapse (DueFromMX eliminated); FX Clearing 974,000 now equity
		{"reports.balance_sheet.total.assets", "MXN", 40_140_000},     // FX Clearing 500,000 now equity
		{"reports.balance_sheet.total.liabilities", "USD", 300_000},   // post-collapse (DueToIntl eliminated)
		{"reports.balance_sheet.total.net_assets", "USD", 21_293_500}, // = assets - liabilities; FX Clearing contra-equity debit 974,000
		{"reports.balance_sheet.total.net_assets", "MXN", 40_140_000},
		{"reports.balance_sheet.na.with", "USD", 1_050_000},     // Beca Agua 50,000 + Building Fund MONETARY 1,000,000 (Building 4,000,000 released, p-golive)
		{"reports.balance_sheet.na.with", "MXN", 9_700_000},     // Beca Agua cash-only; nothing deployed
		{"reports.balance_sheet.na.without", "USD", 20_243_500}, // = net_assets 21,293,500 - with 1,050,000 (absorbs the 4,000,000 release)
		{"reports.balance_sheet.na.without", "MXN", 30_440_000},
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

	// THE DISCRIMINATOR, spelled out: USD without-restriction (20,243,500) is derived as
	// total net assets - with (fund tagging), NOT the fund-0 asset figure (17,543,500). A
	// "without = fund 0" bug would return 17,543,500 here.
	usdWithout, _ := native("reports.balance_sheet.na.without", "USD")
	if usdWithout == 17_543_500 {
		t.Errorf("USD without-restriction == fund-0 asset figure 17,543,500: 'without = fund 0' bug")
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

	rootP := reports.Params{Scope: reports.SubsidiaryID(f.IDs.Root), AsOf: f.Expected.AsOf, Lang: "en"}
	rootT, err := rep.Run(ctx, reports.NewToolkit(f.Store, rootP), rootP)
	if err != nil {
		t.Fatalf("run root: %v", err)
	}
	leafP := reports.Params{Scope: reports.SubsidiaryID(f.IDs.MX), AsOf: f.Expected.AsOf, Lang: "en"}
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
// intercompany accounts fail to net to zero, and the balance sheet surfaces the D19
// residual -- in the NATIVE view (no target) as the reconciling-difference line (RowWarning
// kind, p26.70: labeled honestly rather than flagged as an unexplained error; there is no
// single rate in native mode, so it cannot be split into a translation adjustment).
// Mirrors p15.2's IntercompanyNet corrupted-copy technique: you cannot post an unbalanced
// txn, and net-to-zero is a CROSS-transaction property, so post ONE extra BALANCED txn
// (DR Due-from +250,000 / CR Checking US -250,000) that has no mirroring Due-to credit --
// each txn stays zero-sum, but the consolidated intercompany net becomes +250,000 USD.
func TestBalanceSheetIntercompanyWarning(t *testing.T) {
	f := fixture.New(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	rep := balanceSheetReport(t)

	// Baseline (clean): no warning.
	p := reports.Params{Scope: reports.SubsidiaryID(f.IDs.Root), AsOf: f.Expected.AsOf, Lang: "en"}
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
		t.Fatalf("corrupted fixture: expected an intercompany reconciling row, got none")
	}
	// It is labeled as the reconciling-difference line (p26.70), not the old bare "do not
	// net to zero" warning key.
	if wr.Cells[0].Kind != reports.CellLabel || wr.Cells[0].Text != "reports.balance_sheet.na.ic_reconciling" {
		t.Errorf("native residual row label = %q, want reports.balance_sheet.na.ic_reconciling", wr.Cells[0].Text)
	}
	// The row's amount cell carries the +250,000 USD residual (native USD; no target).
	var found bool
	for _, c := range wr.Cells {
		if c.Kind == reports.CellMoney && !c.Blank && c.Minor == 250_000 {
			found = true
		}
	}
	if !found {
		t.Errorf("intercompany reconciling row does not carry the 250,000 USD residual: %+v", wr.Cells)
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
	p := reports.Params{Scope: reports.SubsidiaryID(f.IDs.Root), AsOf: "2025-12-31", Lang: "en"}
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
			// p18: the selected as-of is column 1 (Cells[1]); the nested-tree oracle reads
			// that primary column, not the oldest (last) year-end column.
			return rowInfo{found: true, kind: row.Kind, indent: row.Indent, amount: row.Cells[1].Minor}
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

// TestBalanceSheetCTASplit is the p26.70 reclassification: a MULTI-currency intercompany
// residual (a foreign-currency IC advance whose closing-rate value differs from its
// historical/transaction-date value) is presented in the CONVERTED view as a Cumulative
// Translation Adjustment line (closing − historical) plus a reconciling-difference line
// (historical), carved out of the without-restriction figure so the net-assets total and
// the balance-sheet identity are UNCHANGED. Asserts: (1) the bare "does not net to zero"
// warning is GONE (replaced by the two labeled lines); (2) A == L + NA still holds
// exactly; (3) CTA + reconciling reclassify the whole residual (their magnitudes equal
// the closing residual — no value lost, just relabeled); (4) the CTA equals closing −
// historical from the toolkit split; (5) without-restriction moved TOWARD its undistorted
// value (the directional check — the reclassification restores what the elimination
// distorted). Uses the corrupted-copy technique with an MXN IC advance so closing ≠
// historical (RULE 11: a store-level structural probe, not a golden).
func TestBalanceSheetCTASplit(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	rep := balanceSheetReport(t)
	p := reports.Params{Scope: reports.SubsidiaryID(f.IDs.Root), AsOf: f.Expected.AsOf, Lang: "en", TargetCurrency: "USD"}

	// Baseline (clean, converted): NO CTA / reconciling / warning rows.
	clean, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run clean: %v", err)
	}
	if _, ok := labelAmount(clean, "reports.balance_sheet.na.cta"); ok {
		t.Fatalf("clean fixture already has a CTA row")
	}
	cleanWithout, _ := labelAmount(clean, "reports.balance_sheet.na.without")

	// Corrupt with a FOREIGN-currency (MXN) unmirrored intercompany advance on the MX sub,
	// dated early (Feb 2025) so its transaction-date rate differs from the closing rate --
	// giving a nonzero translation component. DueToIntl (liability, IC-flagged) net-debit
	// +1,000,000 MXN, Cash MXN -1,000,000 MXN keeps the txn zero-sum.
	_, err = f.Store.PostTransaction(ctx, store.PostTransactionInput{
		Date:         "2025-02-15",
		SubsidiaryID: f.IDs.MX,
		Currency:     "MXN",
		Memo:         "test: unmirrored MXN intercompany advance",
		Splits: []store.SplitInput{
			{AccountID: f.IDs.DueToIntl, Amount: 1_000_000},
			{AccountID: f.IDs.CashMXN, Amount: -1_000_000},
		},
	})
	if err != nil {
		t.Fatalf("post corrupting MXN txn: %v", err)
	}

	// The toolkit split is the oracle for the two components.
	tk := reports.NewToolkit(f.Store, p)
	split, err := tk.IntercompanyResidualSplit(ctx, reports.Scope{Sub: reports.SubsidiaryID(f.IDs.Root)}, f.Expected.AsOf, "USD")
	if err != nil {
		t.Fatalf("residual split: %v", err)
	}
	if split.Closing == split.Historical {
		t.Fatalf("MXN advance produced no translation component (closing %d == historical %d) -- the scenario is not exercising CTA", split.Closing, split.Historical)
	}

	corr, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run corrupted: %v", err)
	}

	// (1) The bare warning is GONE (no RowWarning in the converted view; replaced by the
	// two labeled net-asset lines).
	if wr := warningRow(corr); wr != nil {
		t.Errorf("converted view still emits a bare intercompany warning row: %+v", wr.Cells)
	}

	cta, okC := labelAmount(corr, "reports.balance_sheet.na.cta")
	rec, okR := labelAmount(corr, "reports.balance_sheet.na.ic_reconciling")
	if !okC || !okR {
		t.Fatalf("converted view missing CTA (%v) or reconciling (%v) line", okC, okR)
	}

	// (4) The residual is a CONTRA-equity adjustment (the eliminated IC legs REDUCED the
	// plug), so each displayed line is the negation of its raw component: the CTA line
	// shows −(closing − historical) = historical − closing (the FX translation portion),
	// and the reconciling line shows −historical (the genuine imbalance). The raw split's
	// translation magnitude is |closing − historical|.
	if cta != split.Historical-split.Closing {
		t.Errorf("CTA line = %d, want historical-closing = %d (contra-equity FX component)", cta, split.Historical-split.Closing)
	}
	if rec != -split.Historical {
		t.Errorf("reconciling line = %d, want -historical = %d", rec, -split.Historical)
	}

	// (3) No value lost: CTA + reconciling reclassify the whole closing residual (their
	// sum is the negated closing residual -- the amount the elimination pulled out of the
	// plug), so |CTA + rec| == the old IntercompanyNet residual (converted at closing).
	if cta+rec != -split.Closing {
		t.Errorf("CTA + reconciling = %d, want -closing residual = %d (no value lost)", cta+rec, -split.Closing)
	}

	// (2) A == L + NA still holds EXACTLY (the reclassification does not touch the plug).
	assets, _ := labelAmount(corr, "reports.balance_sheet.total.assets")
	lPlusNA, _ := labelAmount(corr, "reports.balance_sheet.total.liabilities_net_assets")
	if assets != lPlusNA {
		t.Errorf("A (%d) != L+NA (%d): the CTA reclassification broke the identity", assets, lPlusNA)
	}

	// (5) Directional: without-restriction moved TOWARD its undistorted (clean) value --
	// the reclassification restores what the elimination distorted, it does not push
	// further away.
	corrWithout, _ := labelAmount(corr, "reports.balance_sheet.na.without")
	// Distorted (pre-reclassification) without = cleanWithout - closing (the elimination
	// shortfall). The shown without should be closer to cleanWithout than the distorted one.
	distorted := cleanWithout - split.Closing
	if abs64(corrWithout-cleanWithout) > abs64(distorted-cleanWithout) {
		t.Errorf("without-restriction moved AWAY from clean: shown %d, clean %d, distorted %d", corrWithout, cleanWithout, distorted)
	}
}

// TestBalanceSheetCTADetailNoSplit: the CTA reclassification is a CONVERTED-view
// presentation. In the per-currency NATIVE DETAIL view (Detail=="currency", even WITH a
// target) there is no single rate, so the split must NOT run — the residual is shown as
// the native reconciling-difference line PER NATIVE CURRENCY instead, and no (converted)
// CTA / reconciling-in-target rows appear, and the native asset/without figures are NOT
// polluted by a target-currency injection. Regression guard for the detail+target gate.
func TestBalanceSheetCTADetailNoSplit(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	rep := balanceSheetReport(t)

	// Detail=currency WITH a target (native + converted columns).
	p := reports.Params{Scope: reports.SubsidiaryID(f.IDs.Root), AsOf: f.Expected.AsOf, Lang: "en", TargetCurrency: "USD", Detail: "currency"}

	// The clean native without-USD figure (oracle for "not polluted").
	clean, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run clean detail: %v", err)
	}
	cleanNativeUSD := detailNative(clean, "reports.balance_sheet.na.without", "USD")

	// MXN unmirrored intercompany advance (a residual with a translation component).
	_, err = f.Store.PostTransaction(ctx, store.PostTransactionInput{
		Date:         "2025-02-15",
		SubsidiaryID: f.IDs.MX,
		Currency:     "MXN",
		Memo:         "test: MXN intercompany advance (detail view)",
		Splits: []store.SplitInput{
			{AccountID: f.IDs.DueToIntl, Amount: 1_000_000},
			{AccountID: f.IDs.CashMXN, Amount: -1_000_000},
		},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}

	corr, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run corrupted detail: %v", err)
	}

	// No converted CTA row in the detail view (the split did not run).
	if _, ok := labelAmount(corr, "reports.balance_sheet.na.cta"); ok {
		t.Errorf("detail view emitted a CTA row; the split must not run in per-currency mode")
	}
	// The reconciling line IS present, carrying the MXN 1,000,000 NATIVE residual (not a
	// USD figure).
	if mxn := detailNative(corr, "reports.balance_sheet.na.ic_reconciling", "MXN"); mxn != 1_000_000 {
		t.Errorf("detail reconciling native MXN = %d, want 1,000,000 (native residual, not converted USD)", mxn)
	}
	// The native without-USD figure is UNCHANGED (no target-currency injection polluted it;
	// the MXN residual only touches MXN lines/totals).
	if got := detailNative(corr, "reports.balance_sheet.na.without", "USD"); got != cleanNativeUSD {
		t.Errorf("detail native without-USD = %d, want %d (unchanged; no converted injection)", got, cleanNativeUSD)
	}
}

// detailNative returns the NATIVE (column-2) minor for the synthetic-label row `key` at
// currency `ccy` in the per-currency detail view (a multi-currency line carries its label
// on its first currency row and blank-named continuations, so carry the label forward).
func detailNative(t reports.Table, key, ccy string) int64 {
	curLabel := ""
	for _, row := range t.Rows {
		if len(row.Cells) < 3 {
			continue
		}
		switch row.Cells[0].Kind {
		case reports.CellLabel:
			curLabel = row.Cells[0].Text
		case reports.CellText:
			if row.Cells[0].Text != "" {
				curLabel = ""
			}
		}
		if curLabel == key && row.Cells[1].Text == ccy {
			return row.Cells[2].Minor
		}
	}
	return 0
}

// abs64 returns the absolute value of an int64.
func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
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

	// p18: the converted-only view is Line + one value column per as-of date (>= 2
	// value columns on the fixture: 2026-06-30 + 2025-12-31), so >= 3 columns total.
	// The per-currency detail view stays a single as-of, 4 columns.
	if len(convT.Columns) < 3 {
		t.Errorf("converted-only columns = %d, want >= 3 (Line + multi-period value columns)", len(convT.Columns))
	}
	if convT.Columns[0].HeaderKey != "reports.balance_sheet.col.line" {
		t.Errorf("converted-only column 0 = %q, want the line column", convT.Columns[0].HeaderKey)
	}
	if len(detT.Columns) != 4 {
		t.Errorf("detail columns = %d, want 4 (single as-of, per-currency)", len(detT.Columns))
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
