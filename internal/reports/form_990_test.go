package reports_test

// p15.11 the 990 PACKAGE report tests. Every asserted number is HAND-DERIVED from the
// canonical synthetic fixture (PLAN Appendix D, internal/testutil/fixture) and CROSS-
// CHECKED against the sibling reports this package composes (p15.4 balance sheet, p15.5
// income statement, p15.7 functional expenses, p15.10 program statement) — the cross-
// checks are the whole point (the 990 numbers MUST reconcile to the reports they are
// drawn from). The golden files (testdata/form_990.{txt,csv}) are a committed, human-
// reviewable rendering; -update / `make golden` regenerate them deterministically
// (lang=en, root scope, period 2025-01-01..2026-06-30 = the fiscal year, USD target).
//
// The report is ONE Table with four labeled Part sections over a shared 3-column shape
// [Line/Account, Currency, Amount]:
//
//   - Part III  — program service summary: revenue + expense per program (General,
//     Educacion, Food Pantry — the p15.10 comparative set), NATIVE per currency, driven
//     by the identical ProgramActivity(RateNone) call so each group == p15.10's column.
//   - Part VIII — revenue by effective line: revenue accounts converted at the TXN-DATE
//     rate (p15.5's flow), rolled to effective Part VIII codes (Group990), Unmapped last.
//     Line total == p15.5 total revenue = 6,476,594.
//   - Part IX  — functional-expense line totals: p15.7 FunctionalMatrix(RateTxnDate,
//     p26.71 — the expense FLOW rate, matching Part VIII revenue and the income
//     statement), each line total == p15.7's line total; grand == p15.7 grand =
//     2,377,088 == the income statement's total expenses (GranNone).
//   - Part X   — balance sheet at year-end (To=2026-06-30): p15.4's path (BalancesAsOf +
//     restricted split + intercompany elimination), A == L + NA.
//
// HAND-VERIFIED (root scope, USD target, period 2025-01..2026-06, closing 18.10):
//
//	Part VIII  VIII.1f Contributions             5,275,000
//	           VIII.1e Government Grants            781,594  (=200,000 + 10,000,000/18.10)
//	           VIII.2  Program Service Fees         120,000
//	           (Unmapped) Event Income              300,000
//	           Total revenue                      6,476,594  (== p15.5)
//	Part IX    IX.7  1,650,000 · IX.11g 2,500 · IX.16 305,000 · IX.24e 419,588
//	           Total functional expenses          2,377,088  (== p15.7 grand, txn-date)
//	Part X     Total assets == Total L+NA (A = L + NA); with-restriction split == p15.4.

import (
	"bytes"
	"context"
	"testing"

	"cuento/internal/reports"
	"cuento/internal/store"
	"cuento/internal/testutil/fixture"
)

// f990GoldenParams: root scope, full fixture span (the fiscal year), USD target, en.
func f990GoldenParams(f *fixture.Fixture) reports.Params {
	return reports.Params{
		Scope:          reports.SubsidiaryID(f.IDs.Root),
		From:           f.Expected.ActivityFrom, // 2025-01-01
		To:             f.Expected.ActivityTo,   // 2026-06-30 (== Expected.AsOf, Part X year-end)
		TargetCurrency: "USD",
		Lang:           "en",
	}
}

func form990Report(t *testing.T) reports.Report {
	t.Helper()
	rep, ok := reports.Default().Get(reports.Form990ReportID)
	if !ok {
		t.Fatalf("990 package report %q not registered in Default()", reports.Form990ReportID)
	}
	return rep
}

// f990RowFor returns the full cell slice for the FIRST row whose first-cell string equals
// key (matches account/line TEXT and localized framework LABEL keys — the golden test
// pins en, but here we match the raw label key or the stored text).
func f990RowFor(t reports.Table, key string) ([]reports.Cell, bool) {
	for _, row := range t.Rows {
		if len(row.Cells) > 0 && row.Cells[0].Text == key {
			return row.Cells, true
		}
	}
	return nil, false
}

// f990RowsFor returns every row whose first cell text equals key (a label like the
// section revenue/expense sub-line recurs per program/currency).
func f990RowsFor(t reports.Table, key string) [][]reports.Cell {
	var out [][]reports.Cell
	for _, row := range t.Rows {
		if len(row.Cells) > 0 && row.Cells[0].Text == key {
			out = append(out, row.Cells)
		}
	}
	return out
}

// TestForm990Golden runs the package over the fixture at the pinned params, hand-verifies
// each Part's key lines, and compares the rendered text + CSV to committed goldens.
func TestForm990Golden(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()

	rep := form990Report(t)
	p := f990GoldenParams(f)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run 990 package: %v", err)
	}

	// Shared 3-column shape.
	if len(table.Columns) != 3 {
		t.Fatalf("columns = %d, want 3 (Line + Currency + Amount)", len(table.Columns))
	}

	// --- Part VIII revenue lines (converted USD, txn-date; displayed +inflow).
	viii := map[string]int64{
		"1f — All other contributions and gifts": 5_275_000,
		"1e — Government grants (contributions)": 781_594,
		"2 — Program service revenue":            120_000,
	}
	for label, want := range viii {
		cells, ok := f990RowFor(table, label)
		if !ok {
			t.Errorf("Part VIII: no line %q", label)
			continue
		}
		if cells[2].Minor != want {
			t.Errorf("Part VIII %q = %d, want %d", label, cells[2].Minor, want)
		}
	}

	// --- Part IX line totals (== p15.7's per-line totals).
	ix := map[string]int64{
		"7 — Other salaries and wages":     1_650_000,
		"11g — Fees for services -- other": 2_500,
		"16 — Occupancy":                   305_000,
		"24e — All other expenses":         419_588,
	}
	for label, want := range ix {
		cells, ok := f990RowFor(table, label)
		if !ok {
			t.Errorf("Part IX: no line %q", label)
			continue
		}
		if cells[2].Minor != want {
			t.Errorf("Part IX %q = %d, want %d", label, cells[2].Minor, want)
		}
	}

	// The golden artifacts.
	exps := map[string]int{"USD": 2, "MXN": 2}
	got := reports.DumpTable(table, goldenLocalize, exps)
	checkGolden(t, "form_990.txt", []byte(got))

	var buf bytes.Buffer
	if err := reports.WriteCSV(&buf, localizeLabels(table), goldenLocalize, exps); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	checkGolden(t, "form_990.csv", buf.Bytes())
}

// TestForm990PartIXCrossCheckP157: every Part IX line total in the 990 package EQUALS the
// corresponding line total the p15.7 functional-expenses report emits (the Total column of
// its per-line subtotal row), and the grand totals agree. Run BOTH reports and compare —
// an identity, not a re-derivation.
func TestForm990PartIXCrossCheckP157(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	p := f990GoldenParams(f)

	nine, err := form990Report(t).Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run 990: %v", err)
	}
	fe, err := functionalExpensesReport(t).Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run functional expenses: %v", err)
	}

	// p15.7's per-line subtotal Total column (index 4) keyed by its IRS line label.
	for _, label := range []string{
		"7 — Other salaries and wages",
		"11g — Fees for services -- other",
		"16 — Occupancy",
		"24e — All other expenses",
	} {
		feCells, ok := feRowFor(fe, label)
		if !ok {
			t.Fatalf("p15.7 missing line %q", label)
		}
		feTotal := feCells[4].Minor // p15.7 Total column
		nineCells, ok := f990RowFor(nine, label)
		if !ok {
			t.Fatalf("990 Part IX missing line %q", label)
		}
		if nineCells[2].Minor != feTotal {
			t.Errorf("Part IX %q = %d, but p15.7 line total = %d", label, nineCells[2].Minor, feTotal)
		}
	}

	// Grand totals agree (p15.7 grand = 2,375,014).
	feGrand, ok := feRowFor(fe, "reports.functional_expenses.total")
	if !ok {
		t.Fatal("p15.7 grand-total row missing")
	}
	nineGrand, ok := f990RowFor(nine, "reports.form_990.ix.total")
	if !ok {
		t.Fatal("990 Part IX total row missing")
	}
	if nineGrand[2].Minor != feGrand[4].Minor {
		t.Errorf("Part IX grand = %d, p15.7 grand = %d", nineGrand[2].Minor, feGrand[4].Minor)
	}
	if nineGrand[2].Minor != 2_377_088 {
		t.Errorf("Part IX grand = %d, want 2,377,088 (fixture oracle, txn-date p26.71)", nineGrand[2].Minor)
	}
}

// TestForm990PartVIIISumsToRevenue: the Part VIII revenue lines (incl. the Unmapped
// bucket) sum to the total-revenue line, which equals p15.5's total revenue (6,476,594).
func TestForm990PartVIIISumsToRevenue(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	p := f990GoldenParams(f)

	nine, err := form990Report(t).Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run 990: %v", err)
	}
	// Cross-check against the income statement's total-revenue figure.
	is, err := incomeStatementReport(t).Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run income statement: %v", err)
	}
	isRev, ok := isTotalFor(is, "reports.income_statement.total.revenue")
	if !ok {
		t.Fatal("income statement total-revenue row missing")
	}

	total, ok := f990RowFor(nine, "reports.form_990.viii.total")
	if !ok {
		t.Fatal("Part VIII total-revenue row missing")
	}
	if total[2].Minor != isRev {
		t.Errorf("Part VIII total = %d, p15.5 total revenue = %d", total[2].Minor, isRev)
	}
	if total[2].Minor != 6_476_594 {
		t.Errorf("Part VIII total = %d, want 6,476,594 (fixture oracle)", total[2].Minor)
	}

	// The line amounts themselves sum to the total (the report footed correctly).
	var sum int64
	inVIII := false
	for _, row := range nine.Rows {
		if len(row.Cells) == 0 {
			continue
		}
		first := row.Cells[0].Text
		if first == "reports.form_990.part.viii" {
			inVIII = true
			continue
		}
		if first == "reports.form_990.viii.total" {
			break // total row itself, stop
		}
		if inVIII && row.Kind == reports.RowData && len(row.Cells) >= 3 &&
			row.Cells[2].Kind == reports.CellMoney && !row.Cells[2].Blank {
			sum += row.Cells[2].Minor
		}
	}
	if sum != total[2].Minor {
		t.Errorf("Part VIII lines sum to %d, but the total row is %d", sum, total[2].Minor)
	}
}

// TestForm990PartXCrossCheckP154: the Part X net-assets with/without split, the section
// totals, and the A = L + NA identity match the p15.4 balance sheet run as-of the same
// year-end (To). Run the actual p15.4 report (converted-only, USD) and compare.
func TestForm990PartXCrossCheckP154(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	p := f990GoldenParams(f)

	nine, err := form990Report(t).Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run 990: %v", err)
	}
	// p15.4 as-of the SAME year-end date, root scope, USD.
	bsP := reports.Params{Scope: reports.SubsidiaryID(f.IDs.Root), AsOf: p.To, TargetCurrency: "USD", Lang: "en"}
	bs, err := balanceSheetReport(t).Run(ctx, reports.NewToolkit(f.Store, bsP), bsP)
	if err != nil {
		t.Fatalf("run balance sheet: %v", err)
	}

	// The p15.4 balance-sheet lines (converted-only: [line, amount]; label in cell 0).
	bsAmt := func(key string) int64 {
		for _, row := range bs.Rows {
			if len(row.Cells) >= 2 && row.Cells[0].Kind == reports.CellLabel && row.Cells[0].Text == key {
				return row.Cells[1].Minor
			}
		}
		t.Fatalf("p15.4 missing %q", key)
		return 0
	}
	// The 990 Part X lines (converted: [line, currency, amount]).
	nineAmt := func(key string) int64 {
		cells, ok := f990RowFor(nine, key)
		if !ok {
			t.Fatalf("990 Part X missing %q", key)
		}
		return cells[2].Minor
	}

	pairs := []struct{ nineKey, bsKey string }{
		{"reports.form_990.x.na_without", "reports.balance_sheet.na.without"},
		{"reports.form_990.x.na_with", "reports.balance_sheet.na.with"},
		{"reports.form_990.x.total_assets", "reports.balance_sheet.total.assets"},
		{"reports.form_990.x.total_liabilities", "reports.balance_sheet.total.liabilities"},
		{"reports.form_990.x.total_net_assets", "reports.balance_sheet.total.net_assets"},
		{"reports.form_990.x.total_liabilities_net_assets", "reports.balance_sheet.total.liabilities_net_assets"},
	}
	for _, pr := range pairs {
		if got, want := nineAmt(pr.nineKey), bsAmt(pr.bsKey); got != want {
			t.Errorf("Part X %q = %d, p15.4 %q = %d", pr.nineKey, got, pr.bsKey, want)
		}
	}

	// A = L + NA (the identity).
	assets := nineAmt("reports.form_990.x.total_assets")
	lPlusNA := nineAmt("reports.form_990.x.total_liabilities_net_assets")
	if assets != lPlusNA {
		t.Errorf("Part X identity broken: assets %d != L+NA %d", assets, lPlusNA)
	}
	// Net assets = without + with.
	na := nineAmt("reports.form_990.x.total_net_assets")
	if got := nineAmt("reports.form_990.x.na_without") + nineAmt("reports.form_990.x.na_with"); got != na {
		t.Errorf("Part X net assets %d != without+with %d", na, got)
	}
}

// TestForm990PartIIICrossCheckP1510: each program's Part III revenue and expense figures
// (native, per currency) equal the corresponding program column in p15.10's comparative
// program statement — an identity, since both drive off the same ProgramActivity call.
func TestForm990PartIIICrossCheckP1510(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	p := f990GoldenParams(f)

	nine, err := form990Report(t).Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run 990: %v", err)
	}
	// p15.10 comparative (no program chosen): a column per program (General, Educacion,
	// Food Pantry). Section total rows carry the per-currency totals per program column.
	psP := reports.Params{Scope: reports.SubsidiaryID(f.IDs.Root), From: p.From, To: p.To, Lang: "en"}
	ps, err := psReport(t).Run(ctx, reports.NewToolkit(f.Store, psP), psP)
	if err != nil {
		t.Fatalf("run program statement: %v", err)
	}

	// p15.10 columns: [Account, Currency, General, Educacion, Food Pantry]. Its section
	// total rows ("Total revenue" / "Total expenses") carry the per-currency total per
	// program column. Map program name -> its column index (2 + tree pre-order position).
	progCol := map[string]int{"General": 2, "Educacion": 3, "Food Pantry": 4}

	// For each program, compare the 990 Part III revenue/expense per currency against the
	// p15.10 section-total column value for that program.
	for prog, col := range progCol {
		for _, sec := range []struct{ psKey, iiiKey string }{
			{"reports.program_statement.total.revenue", "reports.form_990.iii.revenue"},
			{"reports.program_statement.total.expenses", "reports.form_990.iii.expenses"},
		} {
			psByCcy := psTotalsByCurrency(ps, sec.psKey, col)
			nineByCcy := f990ProgramSectionByCurrency(nine, prog, sec.iiiKey)
			// Union the currency sets; an absent entry is 0. p15.10 emits a per-currency
			// row for EVERY currency present in ANY program column (a program without that
			// currency shows 0), whereas the 990 emits only the currencies a program
			// actually has — so the two agree with absent==0 on both sides.
			ccys := map[string]bool{}
			for c := range psByCcy {
				ccys[c] = true
			}
			for c := range nineByCcy {
				ccys[c] = true
			}
			for ccy := range ccys {
				if nineByCcy[ccy] != psByCcy[ccy] {
					t.Errorf("Part III %s %s %s = %d, p15.10 = %d", prog, sec.iiiKey, ccy, nineByCcy[ccy], psByCcy[ccy])
				}
			}
		}
	}
}

// psTotalsByCurrency returns the p15.10 section-total row values (per currency) in the
// given program column index for the rows whose first cell is the section-total label.
func psTotalsByCurrency(t reports.Table, labelKey string, col int) map[string]int64 {
	out := map[string]int64{}
	for _, row := range t.Rows {
		if len(row.Cells) <= col {
			continue
		}
		if row.Cells[0].Kind == reports.CellLabel && row.Cells[0].Text == labelKey {
			ccy := row.Cells[1].Text // Currency column
			out[ccy] = row.Cells[col].Minor
		}
	}
	return out
}

// f990ProgramSectionByCurrency returns the 990 Part III revenue/expense sub-line values
// (per currency) for the named program group: the rows between that program's group-header
// row (first cell == program name) and the NEXT program-header (or the Unmapped bucket),
// whose first cell is the section label sectionKey.
func f990ProgramSectionByCurrency(t reports.Table, prog, sectionKey string) map[string]int64 {
	out := map[string]int64{}
	in := false
	for _, row := range t.Rows {
		if len(row.Cells) == 0 {
			continue
		}
		c0 := row.Cells[0]
		// A program group header is a TEXT cell equal to the program name.
		if c0.Kind == reports.CellText && c0.Text == prog {
			in = true
			continue
		}
		// Another program header, the Unmapped bucket, or a new Part section ends this group.
		if in && c0.Kind == reports.CellText && c0.Text != "" && c0.Text != prog {
			// A different program name row: stop.
			break
		}
		if in && c0.Kind == reports.CellLabel && c0.Text == "reports.form_990.unmapped" {
			break
		}
		if in && c0.Kind == reports.CellLabel && c0.Text == sectionKey {
			out[row.Cells[1].Text] = row.Cells[2].Minor
		}
	}
	return out
}

// TestForm990UnmappedBucketsPresent: EVERY Part renders an explicit Unmapped bucket line
// (never drops rows), even when empty on the fixture (Part IX/III/X), and Part VIII's is
// non-empty (Event Income has no effective 990 code).
func TestForm990UnmappedBucketsPresent(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	p := f990GoldenParams(f)
	table, err := form990Report(t).Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// Count Unmapped rows: one per Part = 4.
	unmapped := f990RowsFor(table, "reports.form_990.unmapped")
	if len(unmapped) != 4 {
		t.Errorf("Unmapped buckets = %d, want 4 (one per Part)", len(unmapped))
	}

	// Part VIII's Unmapped bucket is NON-empty: Event Income = 300,000 (positive inflow).
	var found bool
	for _, cells := range unmapped {
		if cells[2].Kind == reports.CellMoney && !cells[2].Blank && cells[2].Minor == 300_000 {
			found = true
		}
	}
	if !found {
		t.Errorf("Part VIII Unmapped bucket missing the 300,000 Event Income figure")
	}

	// The flag must surface the SPECIFIC unmapped account BY NAME so the preparer knows
	// exactly which account to map: an "Event Income" detail row (RowData) sits beneath the
	// Part VIII Unmapped flag, carrying its 300,000 (positive inflow) figure. The flag row
	// itself is an emphasized RowSubtotal (skipped by the section footing; the detail row is
	// the summed figure) — assert the account name is present with its amount.
	ei, ok := f990RowFor(table, "Event Income")
	if !ok {
		t.Fatal("Part VIII Unmapped: no 'Event Income' detail row (unmapped account not surfaced by name)")
	}
	if ei[2].Kind != reports.CellMoney || ei[2].Blank || ei[2].Minor != 300_000 {
		t.Errorf("Event Income detail amount = %+v, want money 300,000", ei[2])
	}
	// The Part VIII Unmapped flag row is an emphasized RowSubtotal carrying the bucket total.
	for _, row := range table.Rows {
		if len(row.Cells) > 0 && row.Cells[0].Kind == reports.CellLabel &&
			row.Cells[0].Text == "reports.form_990.unmapped" && row.Cells[2].Minor == 300_000 {
			if row.Kind != reports.RowSubtotal {
				t.Errorf("Part VIII Unmapped flag row kind = %v, want RowSubtotal (non-summed memo)", row.Kind)
			}
		}
	}
}

// TestForm990PartVIIIDrillReconciles: a SINGLE-native-currency Part VIII revenue LINE's
// drill (spanning its mapped accounts) reconciles — the signed sum of the drilled NATIVE
// splits (looping the account set, per the drill route) equals the line's PRE-conversion
// native figure. VIII.1f Contributions is single-account, single-currency USD, so its
// converted figure == native. It also asserts a MULTI-native-currency line (VIII.1e,
// IX.24e) is left NON-drillable (the p15.7 rule — no single currency-filtered drill
// reconciles a figure summed across USD+MXN).
func TestForm990PartVIIIDrillReconciles(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	p := f990GoldenParams(f)
	table, err := form990Report(t).Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	cells, ok := f990RowFor(table, "1f — All other contributions and gifts")
	if !ok {
		t.Fatal("Part VIII VIII.1f line missing")
	}
	cell := cells[2]
	if cell.Drill == nil {
		t.Fatal("VIII.1f amount cell is not drillable")
	}
	d := cell.Drill
	if d.Mode != reports.DrillPeriod || d.From != p.From || d.To != p.To {
		t.Errorf("VIII.1f drill = %+v, want DrillPeriod over the fiscal year", d)
	}
	// Reconcile: sum the drilled native splits across the account set. Contributions is
	// USD 1:1, so the native sum (-5,275,000, net-debit) negates to the displayed 5,275,000.
	var sum int64
	for _, acct := range d.AccountIDs {
		filter := store.DrillFilter{Scope: d.Scope, AccountID: acct, Currency: d.Currency, From: d.From, To: d.To}
		splits, err := f.Store.DrillSplits(ctx, filter)
		if err != nil {
			t.Fatalf("drill splits: %v", err)
		}
		for _, s := range splits {
			sum += s.Amount
		}
	}
	if -sum != cell.Minor {
		t.Errorf("VIII.1f drill sum = %d (negated %d), want cell figure %d", sum, -sum, cell.Minor)
	}
	if -sum != 5_275_000 {
		t.Errorf("VIII.1f native (negated) = %d, want 5,275,000 (fixture oracle)", -sum)
	}

	// A MULTI-native-currency line is NOT drillable (the p15.7 rule): VIII.1e Government
	// Grants spans USD (200,000) + MXN (10,000,000), and Part IX's IX.24e spans USD+MXN —
	// a single currency-filtered drill cannot reconcile a figure summed across currencies.
	for _, label := range []string{
		"1e — Government grants (contributions)",
		"24e — All other expenses",
	} {
		cells, ok := f990RowFor(table, label)
		if !ok {
			t.Fatalf("multi-currency line %q missing", label)
		}
		if cells[2].Drill != nil {
			t.Errorf("multi-native-currency line %q is drillable; must not be (p15.7 rule)", label)
		}
	}
}

// TestForm990Empty: a period with no activity yields a table with headers and the four
// section headers + Unmapped buckets, but no data lines (the framework nothing-to-show).
func TestForm990Empty(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	p := f990GoldenParams(f)
	p.From = "2020-01-01"
	p.To = "2020-12-31"
	table, err := form990Report(t).Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run empty: %v", err)
	}
	// Still renders the four Part section headers.
	for _, key := range []string{
		"reports.form_990.part.iii", "reports.form_990.part.viii",
		"reports.form_990.part.ix", "reports.form_990.part.x",
	} {
		if _, ok := f990RowFor(table, key); !ok {
			t.Errorf("empty run missing section header %q", key)
		}
	}
	// The Unmapped buckets are still present (the mechanism).
	if got := len(f990RowsFor(table, "reports.form_990.unmapped")); got != 4 {
		t.Errorf("empty run Unmapped buckets = %d, want 4", got)
	}
}

// TestForm990GrantProgramScope (p27.4b): a program-scoped report grant filters the 990
// package's PROGRAM-DIMENSIONED parts (III program services, VIII revenue, IX functional
// expenses -- all R/E, all filtered by the toolkit) to the granted subtree, and SUPPRESSES
// Part X (the balance sheet -- assets/liabilities/net-assets carry NO program, D24, so it
// cannot be program-filtered; computing it would ship org-wide balances). Scoped to
// Educacion (leaf subtree = {Educacion}): the General-direct Salaries line (IX.7) and
// Contributions line (VIII.1f) vanish, the Part IX grand total shrinks, and Part X is gone.
func TestForm990GrantProgramScope(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := form990Report(t)

	base := f990GoldenParams(f)
	baseT, err := rep.Run(ctx, reports.NewToolkit(f.Store, base), base)
	if err != nil {
		t.Fatalf("run unscoped: %v", err)
	}
	// Baseline: Part X present, Salaries line present (sibling present).
	if _, ok := f990RowFor(baseT, "reports.form_990.part.x"); !ok {
		t.Fatalf("unscoped 990 missing Part X header")
	}
	if _, ok := f990RowFor(baseT, "7 — Other salaries and wages"); !ok {
		t.Fatalf("unscoped 990 missing Part IX Salaries line (sibling present)")
	}
	baseIX, ok := f990RowFor(baseT, "reports.form_990.ix.total")
	if !ok {
		t.Fatalf("unscoped: no Part IX total")
	}

	p := base
	p.ProgramScope = []reports.ProgramID{reports.ProgramID(f.IDs.Educacion)}
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run scoped: %v", err)
	}

	// Part X (balance sheet, no program dimension) is SUPPRESSED entirely under a scope.
	if _, ok := f990RowFor(table, "reports.form_990.part.x"); ok {
		t.Errorf("scoped 990 still renders Part X (balance sheet has no program dimension; must be suppressed)")
	}
	// Parts III/VIII/IX remain (they ARE program-filterable).
	for _, key := range []string{
		"reports.form_990.part.iii", "reports.form_990.part.viii", "reports.form_990.part.ix",
	} {
		if _, ok := f990RowFor(table, key); !ok {
			t.Errorf("scoped 990 dropped program-dimensioned section header %q", key)
		}
	}
	// General-direct lines vanish: Salaries (IX.7) and All-other-contributions (VIII.1f).
	if _, ok := f990RowFor(table, "7 — Other salaries and wages"); ok {
		t.Errorf("scoped 990 leaks General-direct Salaries line (IX.7)")
	}
	if _, ok := f990RowFor(table, "1f — All other contributions and gifts"); ok {
		t.Errorf("scoped 990 leaks General-direct Contributions line (VIII.1f)")
	}
	// The rolled Part IX grand total reflects only Educacion's expenses -- strictly less.
	scopedIX, ok := f990RowFor(table, "reports.form_990.ix.total")
	if !ok {
		t.Fatalf("scoped: no Part IX total")
	}
	if scopedIX[2].Minor >= baseIX[2].Minor {
		t.Errorf("scoped Part IX total %d >= org-wide %d; a sibling subtree leaked into the rollup",
			scopedIX[2].Minor, baseIX[2].Minor)
	}
	if scopedIX[2].Minor <= 0 {
		t.Errorf("scoped Part IX total %d; Educacion's Program Supplies should remain", scopedIX[2].Minor)
	}
}
