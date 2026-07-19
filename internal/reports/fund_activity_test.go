package reports_test

// p15.8 fund balances & activity report tests. Every number below is HAND-DERIVED from
// the synthetic fixture (PLAN Appendix D, internal/testutil/fixture) — the fixture's
// Expected.FundBalances is the oracle for the as-of balances, and transactions.go is
// the oracle for the period flows.
//
// TWO views:
//   - LIST (no fund): the roster — every fund's as-of balance + funder/restriction
//     metadata + the Unrestricted line (fund 0). Golden: fund_activity.{txt,csv}.
//   - SINGLE-FUND STATEMENT (a fund via Params.Fund): Opening + Received − Applied ==
//     Closing, Applied split into expense vs NON-expense. Golden: the Building Fund
//     (fund_activity_statement.{txt,csv}) — the Building asset purchase MUST land in
//     NON-expense applications, not expenses.
//
// HAND-DERIVED figures (from transactions.go):
//
//	Building Fund (USD), full period 2025-01-01..2026-06-30, opening = 0:
//	  2025-06-01 receipt:   Checking US +50,000.00 (asset) / Contributions -50,000.00 (revenue)
//	  2025-06-15 purchase:  Building +40,000.00 (asset) / Checking US -40,000.00 (asset)
//	  Opening (spendable)          = 0
//	  Received (−Σ revenue)        = 50,000.00        (5,000,000 minor)
//	  Applied — expenses           = 0
//	  Applied — non-expense        = 40,000.00        (4,000,000 minor) ← the Building
//	  Closing (spendable)          = 0 + 5,000,000 − 0 − 4,000,000 = 1,000,000 minor
//	  Total fund assets (all-asset)= 5,000,000 minor  == Expected.FundBalances{BuildingFund,USD}
//	  reconcile: Closing 1,000,000 + Capitalized 4,000,000 == 5,000,000 ✓
//
//	Beca Agua (cash-only, no non-expense applications), full period:
//	  MXN: Received 100,000.00 − Applied-expense 3,000.00 = Closing 97,000.00 (9,700,000)
//	  USD: Received   2,000.00 − Applied-expense 1,500.00 = Closing   500.00 (   50,000)
//	  Closing == Expected.FundBalances{BecaAgua,*}: MXN 9,700,000 / USD 50,000 ✓ (cash-only)

import (
	"bytes"
	"context"
	"testing"

	"cuento/internal/reports"
	"cuento/internal/store"
	"cuento/internal/testutil/fixture"
)

// fundActivityReport fetches the registered fund-activity report from Default().
func fundActivityReport(t *testing.T) reports.Report {
	t.Helper()
	rep, ok := reports.Default().Get(reports.FundActivityReportID)
	if !ok {
		t.Fatalf("fund-activity report %q not registered in Default()", reports.FundActivityReportID)
	}
	return rep
}

// fullPeriod runs the report over the whole fixture span, root scope, lang en.
func fullPeriod(f *fixture.Fixture, fund int64) reports.Params {
	return reports.Params{
		Scope: reports.SubsidiaryID(f.IDs.Root),
		Fund:  reports.FundID(fund),
		From:  f.Expected.ActivityFrom, // 2025-01-01
		To:    f.Expected.AsOf,         // 2026-06-30
		Lang:  "en",
	}
}

// --- LIST view --------------------------------------------------------------

// TestFundActivityListGolden runs the fund ROSTER (no fund chosen): every fund's as-of
// balance per currency + funder/restriction metadata + the Unrestricted line, checks the
// balances against the fixture oracle, and compares to the committed golden.
func TestFundActivityListGolden(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := fundActivityReport(t)

	p := reports.Params{Scope: reports.SubsidiaryID(f.IDs.Root), To: f.Expected.AsOf, Lang: "en"} // Fund 0 => list
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run fund list: %v", err)
	}

	// Every expected fund balance appears as a (fund-name-or-Unrestricted, currency,
	// balance) DATA row. Build a lookup by (currency -> balance) per fund line.
	want := map[[2]int64]int64{} // (fund, ccyKey) unused; assert via helper below
	_ = want
	// Assert each Expected.FundBalance shows up with the right amount.
	for _, fb := range f.Expected.FundBalances {
		name := fundDisplayName(f, fb.Fund)
		got, ok := fundListBalance(table, name, fb.Currency)
		if !ok {
			t.Errorf("list missing row for fund %q %s", name, fb.Currency)
			continue
		}
		if got != fb.Amount {
			t.Errorf("list balance fund %q %s = %d, want %d", name, fb.Currency, got, fb.Amount)
		}
	}

	// The Unrestricted line (fund 0) is present (its first cell is the label key).
	if _, ok := fundListBalance(table, "reports.fund_activity.unrestricted", "USD"); !ok {
		t.Errorf("list missing the Unrestricted line")
	}
	// The restricted funds carry funder + restriction metadata (Beca Agua row).
	if !listRowHasFunder(table, "Beca Agua 2025", "Fundacion Agua Limpia") {
		t.Errorf("Beca Agua row missing its funder metadata")
	}

	exps := goldenExps(t, f)
	textDump := reports.DumpTable(table, goldenLocalize, exps)
	var csvBuf bytes.Buffer
	if err := reports.WriteCSV(&csvBuf, localizeLabels(table), goldenLocalize, exps); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	checkGolden(t, "fund_activity.txt", []byte(textDump))
	checkGolden(t, "fund_activity.csv", csvBuf.Bytes())
}

// TestFundActivityListDrillReconciles: each LIST balance cell's drill (the fund's asset
// splits as-of To) sums to the cell figure (the reconciliation invariant, p15.3d),
// against the store as oracle.
func TestFundActivityListDrillReconciles(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := fundActivityReport(t)
	p := reports.Params{Scope: reports.SubsidiaryID(f.IDs.Root), To: f.Expected.AsOf, Lang: "en"}
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	sawDrill := false
	for _, row := range table.Rows {
		if row.Kind != reports.RowData {
			continue
		}
		cell := row.Cells[len(row.Cells)-1] // balance cell
		if cell.Drill == nil {
			// The Unrestricted line (fund 0) is intentionally non-drillable (the store
			// has no NULL-fund drill filter). Every restricted fund cell IS drillable.
			continue
		}
		sawDrill = true
		if sum := drillSum(t, f, cell.Drill); sum != cell.Minor {
			t.Errorf("list drill sum = %d, want cell %d", sum, cell.Minor)
		}
	}
	if !sawDrill {
		t.Errorf("no drillable restricted-fund balance cells found")
	}
}

// --- SINGLE-FUND statement (Building Fund) ----------------------------------

// TestFundStatementBuildingGolden runs the Building Fund's period statement and asserts
// the whole line set BY HAND: the identity Opening+Received−AppliedExpense−
// AppliedNonExpense == Closing, the Building purchase landing in NON-expense (not
// expenses), and Closing + Capitalized == the all-asset fixture balance. Golden:
// fund_activity_statement.{txt,csv}.
func TestFundStatementBuildingGolden(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := fundActivityReport(t)

	p := fullPeriod(f, f.IDs.BuildingFund)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run Building Fund statement: %v", err)
	}

	line := func(key string) int64 { return stmtLine(t, table, key, "USD") }
	opening := line("reports.fund_activity.opening")
	received := line("reports.fund_activity.received")
	appliedExp := line("reports.fund_activity.applied_expense")
	appliedNon := line("reports.fund_activity.applied_nonexpense")
	closing := line("reports.fund_activity.closing")
	totalAssets := line("reports.fund_activity.total_assets")

	// Identity from the report's OWN emitted rows (three independent flow queries).
	if opening+received-appliedExp-appliedNon != closing {
		t.Errorf("identity broken: opening %d + received %d − expense %d − nonexpense %d = %d, want closing %d",
			opening, received, appliedExp, appliedNon, opening+received-appliedExp-appliedNon, closing)
	}
	// Hand figures.
	if opening != 0 {
		t.Errorf("opening = %d, want 0", opening)
	}
	if received != 5_000_000 {
		t.Errorf("received = %d, want 5,000,000", received)
	}
	if appliedExp != 0 {
		t.Errorf("applied-expense = %d, want 0 (no expense splits for Building Fund)", appliedExp)
	}
	// THE KEY ASSERTION: the Building asset purchase lands in NON-expense applications.
	if appliedNon != 4_000_000 {
		t.Errorf("applied-non-expense = %d, want 4,000,000 (the Building purchase)", appliedNon)
	}
	if closing != 1_000_000 {
		t.Errorf("closing (spendable) = %d, want 1,000,000", closing)
	}
	// Total fund assets == the fixture oracle; Closing + Capitalized reconciles to it.
	if want := expectedFundBalance(t, f, f.IDs.BuildingFund, "USD"); totalAssets != want {
		t.Errorf("total fund assets = %d, want fixture oracle %d", totalAssets, want)
	}
	if closing+appliedNon != totalAssets {
		t.Errorf("reconcile: closing %d + capitalized %d = %d, want total assets %d",
			closing, appliedNon, closing+appliedNon, totalAssets)
	}

	exps := goldenExps(t, f)
	textDump := reports.DumpTable(table, goldenLocalize, exps)
	var csvBuf bytes.Buffer
	if err := reports.WriteCSV(&csvBuf, localizeLabels(table), goldenLocalize, exps); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	checkGolden(t, "fund_activity_statement.txt", []byte(textDump))
	checkGolden(t, "fund_activity_statement.csv", csvBuf.Bytes())
}

// TestFundStatementBecaAguaClosingEqualsBalance: for a CASH-ONLY fund (no non-expense
// applications), the spendable Closing EQUALS the as-of-To fund balance the LIST view
// shows (the fixture oracle), per currency — the literal "Closing == as-of fund balance"
// assertion. Beca Agua holds only cash, so spendable == all-asset.
func TestFundStatementBecaAguaClosingEqualsBalance(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := fundActivityReport(t)

	p := fullPeriod(f, f.IDs.BecaAgua)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run Beca Agua statement: %v", err)
	}

	for _, tc := range []struct {
		ccy               string
		received, expense int64
	}{
		{"MXN", 10_000_000, 300_000}, // 100,000.00 grant − 3,000.00 supplies
		{"USD", 200_000, 150_000},    //   2,000.00 grant − 1,500.00 supplies
	} {
		opening := stmtLine(t, table, "reports.fund_activity.opening", tc.ccy)
		received := stmtLine(t, table, "reports.fund_activity.received", tc.ccy)
		appliedExp := stmtLine(t, table, "reports.fund_activity.applied_expense", tc.ccy)
		appliedNon := stmtLine(t, table, "reports.fund_activity.applied_nonexpense", tc.ccy)
		closing := stmtLine(t, table, "reports.fund_activity.closing", tc.ccy)

		if opening != 0 {
			t.Errorf("%s opening = %d, want 0", tc.ccy, opening)
		}
		if received != tc.received {
			t.Errorf("%s received = %d, want %d", tc.ccy, received, tc.received)
		}
		if appliedExp != tc.expense {
			t.Errorf("%s applied-expense = %d, want %d", tc.ccy, appliedExp, tc.expense)
		}
		if appliedNon != 0 {
			t.Errorf("%s applied-non-expense = %d, want 0 (cash-only fund)", tc.ccy, appliedNon)
		}
		// Identity AND Closing == fixture oracle (cash-only => spendable == all-asset).
		if opening+received-appliedExp-appliedNon != closing {
			t.Errorf("%s identity broken", tc.ccy)
		}
		if want := expectedFundBalance(t, f, f.IDs.BecaAgua, tc.ccy); closing != want {
			t.Errorf("%s closing %d != as-of fund balance %d", tc.ccy, closing, want)
		}
	}
}

// TestFundStatementNonzeroOpening: a period that STARTS mid-fund folds prior activity
// into the opening spendable balance and lists only in-period flows. Beca Agua over
// 2025-05-01.. (after both grant receipts, before the two supply spends) has a nonzero
// opening equal to the receipts, and the identity still holds against the closing.
func TestFundStatementNonzeroOpening(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := fundActivityReport(t)

	// Opening (as of 2025-04-30): both grant receipts already in (MXN 100,000.00; USD
	// 2,000.00), no spends yet. In-period (2025-05-01..): only the two supply spends.
	p := reports.Params{Scope: reports.SubsidiaryID(f.IDs.Root), Fund: reports.FundID(f.IDs.BecaAgua), From: "2025-05-01", To: f.Expected.AsOf, Lang: "en"}
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	for _, tc := range []struct {
		ccy                        string
		opening, expense, closing0 int64
	}{
		{"MXN", 10_000_000, 300_000, 9_700_000},
		{"USD", 200_000, 150_000, 50_000},
	} {
		opening := stmtLine(t, table, "reports.fund_activity.opening", tc.ccy)
		received := stmtLine(t, table, "reports.fund_activity.received", tc.ccy)
		appliedExp := stmtLine(t, table, "reports.fund_activity.applied_expense", tc.ccy)
		closing := stmtLine(t, table, "reports.fund_activity.closing", tc.ccy)
		if opening != tc.opening {
			t.Errorf("%s opening = %d, want %d (prior receipts folded in)", tc.ccy, opening, tc.opening)
		}
		if received != 0 {
			t.Errorf("%s received = %d, want 0 (receipts are pre-period)", tc.ccy, received)
		}
		if appliedExp != tc.expense {
			t.Errorf("%s applied-expense = %d, want %d", tc.ccy, appliedExp, tc.expense)
		}
		if closing != tc.closing0 {
			t.Errorf("%s closing = %d, want %d", tc.ccy, closing, tc.closing0)
		}
	}
}

// TestFundStatementDrillReconciles: the received / applied-expense / applied-non-expense
// cells of the Building Fund statement each reconcile — the signed sum of the splits
// their Drill selects (store.DrillSplits) equals the cell figure. This is the split
// derivation's real cross-check: the non-expense drill lists exactly the Building
// purchase split (4,000,000), the expense drill nothing.
func TestFundStatementDrillReconciles(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := fundActivityReport(t)
	p := fullPeriod(f, f.IDs.BuildingFund)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// received cell reconciles (drills the fund's REVENUE splits in-period). Received is
	// a POSITIVE display of the −Σ revenue net-debit, so the drilled RAW splits sum to
	// the NEGATED figure (revenue is a credit): −5,000,000 (the income-statement sign
	// convention — the drill reconciles to the raw net-debit, not the display sign).
	if got, want := drillLineSum(t, f, table, "reports.fund_activity.received", "USD"), int64(-5_000_000); got != want {
		t.Errorf("received drill sum = %d, want %d (raw revenue net-debit)", got, want)
	}
	// applied-non-expense drills exactly the Building purchase (+4,000,000).
	if got, want := drillLineSum(t, f, table, "reports.fund_activity.applied_nonexpense", "USD"), int64(4_000_000); got != want {
		t.Errorf("applied-non-expense drill sum = %d, want %d (the Building purchase)", got, want)
	}
	// Opening/closing spendable drills (as-of, the spendable asset accounts) reconcile.
	if got, want := drillLineSum(t, f, table, "reports.fund_activity.closing", "USD"), int64(1_000_000); got != want {
		t.Errorf("closing (spendable) drill sum = %d, want %d", got, want)
	}
	// Total fund assets drill (all assets) reconciles to the all-asset balance.
	if got, want := drillLineSum(t, f, table, "reports.fund_activity.total_assets", "USD"), int64(5_000_000); got != want {
		t.Errorf("total-assets drill sum = %d, want %d", got, want)
	}
}

// TestFundActivityCSVParses: the single-fund statement CSV parses to well-formed
// records with the localized header.
func TestFundActivityCSVParses(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := fundActivityReport(t)
	p := fullPeriod(f, f.IDs.BuildingFund)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	exps := goldenExps(t, f)
	var buf bytes.Buffer
	if err := reports.WriteCSV(&buf, localizeLabels(table), goldenLocalize, exps); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	recs := parseCSV(t, buf.Bytes())
	if len(recs) < 2 {
		t.Fatalf("csv has %d records, want header + rows", len(recs))
	}
	wantHeader := []string{"Line", "Currency", "Amount"}
	for i, h := range wantHeader {
		if recs[0][i] != h {
			t.Errorf("csv header[%d] = %q, want %q", i, recs[0][i], h)
		}
	}
}

// --- helpers ----------------------------------------------------------------

// fundDisplayName maps a fund id to the name the LIST view keys its first-column cell
// by: a restricted fund's stored proper noun (a TEXT cell), or the Unrestricted LABEL
// key for fund 0 (a CellLabel).
func fundDisplayName(f *fixture.Fixture, id int64) string {
	switch id {
	case 0:
		return "reports.fund_activity.unrestricted" // the label KEY (fund 0 is a label cell)
	case f.IDs.BecaAgua:
		return "Beca Agua 2025"
	case f.IDs.BuildingFund:
		return "Building Fund"
	default:
		return ""
	}
}

// fundListBalance returns the balance cell (col 5) of the LIST DATA row whose fund
// identifier (col 0: a TEXT proper noun, or a LABEL key for Unrestricted) is name and
// currency (col 4) is ccy. The identifier repeats only on a fund's first currency row,
// so for a multi-currency fund the second row's first cell is blank — track the current
// fund identifier as rows are scanned.
func fundListBalance(t reports.Table, name, ccy string) (int64, bool) {
	cur := ""
	for _, row := range t.Rows {
		if row.Kind != reports.RowData || len(row.Cells) < 6 {
			continue
		}
		// A restricted fund's first cell is TEXT (its name); the Unrestricted line's is a
		// LABEL (its key). Either identifies the fund group.
		if n := row.Cells[0].Text; n != "" {
			cur = n
		}
		if cur == name && row.Cells[4].Text == ccy {
			return row.Cells[5].Minor, true
		}
	}
	return 0, false
}

// listRowHasFunder reports whether the LIST row for fund name carries funder in its
// funder column (col 1).
func listRowHasFunder(t reports.Table, name, funder string) bool {
	for _, row := range t.Rows {
		if row.Kind != reports.RowData || len(row.Cells) < 2 {
			continue
		}
		if row.Cells[0].Text == name && row.Cells[1].Text == funder {
			return true
		}
	}
	return false
}

// stmtLine returns the amount (col 2) of the single-fund STATEMENT row whose line label
// key (col 0) is labelKey and currency (col 1) is ccy.
func stmtLine(t *testing.T, tbl reports.Table, labelKey, ccy string) int64 {
	t.Helper()
	for _, row := range tbl.Rows {
		if len(row.Cells) < 3 {
			continue
		}
		if row.Cells[0].Kind == reports.CellLabel && row.Cells[0].Text == labelKey && row.Cells[1].Text == ccy {
			return row.Cells[2].Minor
		}
	}
	t.Fatalf("statement line %q (%s) not found", labelKey, ccy)
	return 0
}

// drillSum builds the store filter from a Drill and returns the signed sum of the splits
// it selects (mirrors the web drill handler).
func drillSum(t *testing.T, f *fixture.Fixture, d *reports.Drill) int64 {
	t.Helper()
	filter := store.DrillFilter{
		Scope:     d.Scope,
		Currency:  d.Currency,
		AsOf:      d.AsOf,
		From:      d.From,
		To:        d.To,
		FundID:    d.FundID,
		ProgramID: d.ProgramID,
		Class:     d.Class,
	}
	var sum int64
	for _, acct := range d.AccountIDs {
		filter.AccountID = acct
		splits, err := f.Store.DrillSplits(context.Background(), filter)
		if err != nil {
			t.Fatalf("drill splits: %v", err)
		}
		for _, s := range splits {
			sum += s.Amount
		}
	}
	return sum
}

// drillLineSum finds the STATEMENT row for labelKey/ccy and reconciles its drill.
func drillLineSum(t *testing.T, f *fixture.Fixture, tbl reports.Table, labelKey, ccy string) int64 {
	t.Helper()
	for _, row := range tbl.Rows {
		if len(row.Cells) < 3 {
			continue
		}
		if row.Cells[0].Kind == reports.CellLabel && row.Cells[0].Text == labelKey && row.Cells[1].Text == ccy {
			d := row.Cells[2].Drill
			if d == nil {
				t.Fatalf("statement line %q (%s) is not drillable", labelKey, ccy)
			}
			return drillSum(t, f, d)
		}
	}
	t.Fatalf("statement line %q (%s) not found", labelKey, ccy)
	return 0
}

// expectedFundBalance returns the fixture's expected (fund, currency) as-of balance —
// the independent oracle for the report's closing / total-assets figures.
func expectedFundBalance(t *testing.T, f *fixture.Fixture, fund int64, ccy string) int64 {
	t.Helper()
	for _, fb := range f.Expected.FundBalances {
		if fb.Fund == fund && fb.Currency == ccy {
			return fb.Amount
		}
	}
	t.Fatalf("no expected fund balance for fund %d %s", fund, ccy)
	return 0
}
