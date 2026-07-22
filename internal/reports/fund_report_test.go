package reports_test

import (
	"bytes"
	"context"
	"testing"

	"cuento/internal/reports"
	"cuento/internal/testutil/fixture"
)

// fundReportReport fetches the registered fund-report cover page from Default().
func fundReportReport(t *testing.T) reports.Report {
	t.Helper()
	rep, ok := reports.Default().Get(reports.FundReportReportID)
	if !ok {
		t.Fatalf("fund-report %q not registered in Default()", reports.FundReportReportID)
	}
	return rep
}

// statusAmt returns the STATUS-band money for a given line label + currency: it scans the
// rows for the first RowData whose col-0 LABEL == labelKey and col-3 currency == ccy, and
// returns its col-4 (Amount) minor value. Fails if not found.
func statusAmt(t *testing.T, table reports.Table, labelKey, ccy string) int64 {
	t.Helper()
	for _, r := range table.Rows {
		if r.Kind != reports.RowData || len(r.Cells) < 5 {
			continue
		}
		if r.Cells[0].Kind == reports.CellLabel && r.Cells[0].Text == labelKey && r.Cells[3].Text == ccy {
			return r.Cells[4].Minor
		}
	}
	t.Fatalf("status line %q (%s) not found", labelKey, ccy)
	return 0
}

// reconAmt returns the reconciliation RowTotal amount for a currency (col-4), which must
// equal the all-asset FundBalancesAsOf. Fails if not found.
func reconAmt(t *testing.T, table reports.Table, ccy string) int64 {
	t.Helper()
	for _, r := range table.Rows {
		if r.Kind == reports.RowTotal && len(r.Cells) >= 5 &&
			r.Cells[0].Kind == reports.CellLabel &&
			r.Cells[0].Text == "reports.fund_report.total_assets" && r.Cells[3].Text == ccy {
			return r.Cells[4].Minor
		}
	}
	t.Fatalf("reconciliation total (%s) not found", ccy)
	return 0
}

// reconLine returns the reconciliation-section money (col-4) for a labeled line (Closing /
// Capitalized) in a currency, and whether it was found (a Capitalized line is absent for a
// cash-only fund). It scans any row (subtotal) whose col-0 LABEL matches.
func reconLine(table reports.Table, labelKey, ccy string) (int64, bool) {
	for _, r := range table.Rows {
		if len(r.Cells) >= 5 && r.Cells[0].Kind == reports.CellLabel &&
			r.Cells[0].Text == labelKey && r.Cells[3].Text == ccy {
			return r.Cells[4].Minor, true
		}
	}
	return 0, false
}

// TestFundReportBecaAguaGolden runs the Beca Agua cover page and asserts its whole SHAPE
// and figures BY HAND: the multi-currency (MXN + USD) STATUS band (Received / Spent /
// Remaining foot to the fixture oracle, per currency), the collapsible EXPENSES account
// tree (each leaf subtotal foots to the account's line sum), and the per-currency
// reconciliation line (Remaining == FundBalancesAsOf). Golden: fund_report.{txt,csv}.
//
// Beca Agua is the canonical multi-currency restricted fund: MXN 100,000.00 grant −
// 3,000.00 supplies, USD 2,000.00 grant − 1,500.00 supplies, cash-only (no capitalized
// asset), so Remaining == Received − Spent == the all-asset balance, per currency.
func TestFundReportBecaAguaGolden(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := fundReportReport(t)

	// No date range: the fund selector is the only param. The report spans all data
	// (LedgerDateRange), so Opening ~ 0 and Received − Spent == Remaining.
	p := reports.Params{
		Scope: reports.SubsidiaryID(f.IDs.Root),
		Fund:  reports.FundID(f.IDs.BecaAgua),
		Lang:  "en",
	}
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run Beca Agua fund report: %v", err)
	}

	// Six columns: Item, Description, Memo, Currency, Amount, % spent.
	if got := len(table.Columns); got != 6 {
		t.Fatalf("columns = %d, want 6 (Item, Description, Memo, Currency, Amount, %% spent)", got)
	}
	wantCols := []string{
		"reports.fund_report.col.item",
		"reports.fund_report.col.description",
		"reports.fund_report.col.memo",
		"reports.fund_report.col.currency",
		"reports.fund_report.col.amount",
		"reports.fund_report.col.pct",
	}
	for i, k := range wantCols {
		if table.Columns[i].HeaderKey != k {
			t.Errorf("column[%d] header = %q, want %q", i, table.Columns[i].HeaderKey, k)
		}
	}
	if !rep.Tree {
		t.Errorf("fund report Report.Tree = false, want true (collapsible account trees)")
	}

	// --- STATUS band figures per currency, hand-checked against the Beca Agua fixture ---
	for _, tc := range []struct {
		ccy             string
		received, spent int64
		remaining       int64
	}{
		{"MXN", 10_000_000, 300_000, 9_700_000},
		{"USD", 200_000, 150_000, 50_000},
	} {
		received := statusAmt(t, table, "reports.fund_report.received", tc.ccy)
		spent := statusAmt(t, table, "reports.fund_report.spent", tc.ccy)
		remaining := statusAmt(t, table, "reports.fund_report.remaining", tc.ccy)
		if received != tc.received {
			t.Errorf("%s received = %d, want %d", tc.ccy, received, tc.received)
		}
		if spent != tc.spent {
			t.Errorf("%s spent = %d, want %d", tc.ccy, spent, tc.spent)
		}
		if remaining != tc.remaining {
			t.Errorf("%s remaining = %d, want %d", tc.ccy, remaining, tc.remaining)
		}
		// Received − Spent == Remaining (cash-only fund, Opening ~ 0): the status band foots.
		if received-spent != remaining {
			t.Errorf("%s status does not foot: received %d − spent %d = %d, want remaining %d",
				tc.ccy, received, spent, received-spent, remaining)
		}
		// Reconciliation (the point of the deliverable): the FLOW-derived Closing (spendable)
		// must equal the BALANCE-derived Total fund assets -- two INDEPENDENT computations.
		// Beca Agua is cash-only, so it has NO Capitalized line and Closing == Total. Closing
		// itself is the flow figure Opening(0) + received − spent == remaining.
		reconTotal := reconAmt(t, table, tc.ccy)
		closing, hasClosing := reconLine(table, "reports.fund_report.closing", tc.ccy)
		if !hasClosing {
			t.Errorf("%s reconciliation has no Closing (spendable) line", tc.ccy)
		}
		if _, hasCap := reconLine(table, "reports.fund_report.capitalized", tc.ccy); hasCap {
			t.Errorf("%s cash-only fund should have NO Deployed/Capitalized line", tc.ccy)
		}
		// The independent identity: flow-derived Closing == balance-derived Total.
		if closing != reconTotal {
			t.Errorf("%s reconcile broken: Closing %d != Total fund assets %d", tc.ccy, closing, reconTotal)
		}
		if closing != received-spent {
			t.Errorf("%s Closing %d != received %d − spent %d", tc.ccy, closing, received, spent)
		}
		if reconTotal != remaining {
			t.Errorf("%s reconciliation total = %d, want remaining %d", tc.ccy, reconTotal, remaining)
		}
		// The fixture oracle (the fund_activity LIST figure) is the same as-of balance.
		if want := expectedFundBalance(t, f, f.IDs.BecaAgua, tc.ccy); remaining != want {
			t.Errorf("%s remaining %d != fixture oracle balance %d", tc.ccy, remaining, want)
		}
	}

	// --- EXPENSES account tree: each leaf subtotal foots to its line sum -------------
	// Walk the rows; within the EXPENSES section, a leaf account header (a RowSubtotal with
	// a TEXT name whose NEXT row is a deeper detail line) opens a leaf whose RowData lines
	// accumulate per currency and whose RowSectionTotal must foot to that sum. This proves
	// the collapsible expense-by-account tree is present and foots.
	const (
		secStatus   = "reports.fund_report.section.status"
		secReceipts = "reports.fund_report.section.receipts"
		secExpenses = "reports.fund_report.section.expenses"
		secAssets   = "reports.fund_report.section.assets"
		secRecon    = "reports.fund_report.section.reconciliation"
	)
	var curSection string
	var expenseLeaves int
	lineSum := map[string]int64{}
	inLeaf := false
	leafIndent := -1
	seenSection := map[string]bool{}
	for i, row := range table.Rows {
		// A section header is an Indent-0 RowSubtotal whose col-0 LABEL is a section key.
		if row.Indent == 0 && row.Kind == reports.RowSubtotal &&
			len(row.Cells) > 0 && row.Cells[0].Kind == reports.CellLabel {
			curSection = row.Cells[0].Text
			seenSection[curSection] = true
			inLeaf = false
			continue
		}
		if curSection != secExpenses {
			continue
		}
		switch row.Kind {
		case reports.RowSubtotal:
			// An account header (placeholder or leaf): TEXT account name at col 0.
			if len(row.Cells) == 0 || row.Cells[0].Kind != reports.CellText {
				continue
			}
			isLeaf := i+1 < len(table.Rows) &&
				table.Rows[i+1].Indent > row.Indent &&
				table.Rows[i+1].Kind != reports.RowSubtotal
			if isLeaf {
				expenseLeaves++
				inLeaf = true
				leafIndent = row.Indent
				lineSum = map[string]int64{}
			}
		case reports.RowData:
			if !inLeaf {
				continue
			}
			if row.Indent != leafIndent+1 {
				t.Errorf("expense detail line Indent = %d, want leaf+1 (%d)", row.Indent, leafIndent+1)
			}
			ccy := row.Cells[3].Text
			lineSum[ccy] += row.Cells[4].Minor
		case reports.RowSectionTotal:
			ccy := row.Cells[3].Text
			if got := row.Cells[4].Minor; got != lineSum[ccy] {
				t.Errorf("expense account subtotal %s = %d, want line sum %d (must foot)", ccy, got, lineSum[ccy])
			}
		}
	}
	if expenseLeaves == 0 {
		t.Errorf("no expense leaf accounts found — EXPENSES tree is empty (Beca Agua has supply spends)")
	}

	// All five sections render.
	for _, s := range []string{secStatus, secReceipts, secExpenses, secAssets, secRecon} {
		if !seenSection[s] {
			t.Errorf("section %q missing from the report", s)
		}
	}

	exps := goldenExps(t, f)
	textDump := reports.DumpTable(table, goldenLocalize, exps)
	var csvBuf bytes.Buffer
	if err := reports.WriteCSV(&csvBuf, localizeLabels(table), goldenLocalize, exps); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	checkGolden(t, "fund_report.txt", []byte(textDump))
	checkGolden(t, "fund_report.csv", csvBuf.Bytes())
}

// TestFundReportFullySpentIndicator: the Building Fund received USD 5,000,000 and expensed
// 0 (it capitalized into the Building instead), so it is NOT fully spent and % spent is 0%.
// This pins the spent-status derivation (Spent = expense applications only; the capitalized
// Building is ASSETS held / Remaining, not Spent) and the fully-spent gate.
func TestFundReportFullySpentIndicator(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := fundReportReport(t)

	p := reports.Params{
		Scope: reports.SubsidiaryID(f.IDs.Root),
		Fund:  reports.FundID(f.IDs.BuildingFund),
		Lang:  "en",
	}
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run Building Fund report: %v", err)
	}

	received := statusAmt(t, table, "reports.fund_report.received", "USD")
	spent := statusAmt(t, table, "reports.fund_report.spent", "USD")
	remaining := statusAmt(t, table, "reports.fund_report.remaining", "USD")
	if received != 5_000_000 {
		t.Errorf("received = %d, want 5,000,000", received)
	}
	// KEY: the capitalized Building is NOT "spent" — Spent counts only expense applications.
	if spent != 0 {
		t.Errorf("spent = %d, want 0 (Building Fund has no expense splits; the Building is an asset held)", spent)
	}
	if remaining != 5_000_000 {
		t.Errorf("remaining = %d, want 5,000,000 (all held as assets: cash + Building)", remaining)
	}
	// % spent is 0% and the fully-spent line is ABSENT (spent < received).
	pctFound := false
	fullyFound := false
	for _, r := range table.Rows {
		if len(r.Cells) < 6 || r.Cells[0].Kind != reports.CellLabel {
			continue
		}
		switch r.Cells[0].Text {
		case "reports.fund_report.pct_spent":
			pctFound = true
			if r.Cells[5].Text != "0%" {
				t.Errorf("%% spent = %q, want %q", r.Cells[5].Text, "0%")
			}
		case "reports.fund_report.fully_spent":
			fullyFound = true
		}
	}
	if !pctFound {
		t.Errorf("no %% spent line found")
	}
	if fullyFound {
		t.Errorf("fully-spent line present, but Building Fund spent 0 of 5,000,000 (not fully spent)")
	}

	// RECONCILIATION identity (the deliverable, and the case that proves the reconciliation
	// is NOT tautological): the Building Fund capitalized 4,000,000 into the Building, so the
	// FLOW-derived Closing (spendable) 1,000,000 + Capitalized 4,000,000 == the BALANCE-
	// derived Total fund assets 5,000,000. Closing and Total are computed independently
	// (FundPeriodStatement flows vs. FundBalancesAsOf), so this asserts they agree.
	reconTotal := reconAmt(t, table, "USD")
	closing, hasClosing := reconLine(table, "reports.fund_report.closing", "USD")
	capd, hasCap := reconLine(table, "reports.fund_report.capitalized", "USD")
	if !hasClosing {
		t.Fatalf("reconciliation has no Closing line")
	}
	if !hasCap {
		t.Fatalf("reconciliation has no Deployed/Capitalized line (Building Fund capitalized 4,000,000)")
	}
	if closing != 1_000_000 {
		t.Errorf("Closing (spendable) = %d, want 1,000,000", closing)
	}
	if capd != 4_000_000 {
		t.Errorf("Deployed/Capitalized = %d, want 4,000,000 (the Building)", capd)
	}
	if closing+capd != reconTotal {
		t.Errorf("reconcile broken: Closing %d + Capitalized %d = %d, want Total %d",
			closing, capd, closing+capd, reconTotal)
	}
	if reconTotal != remaining {
		t.Errorf("reconciliation total %d != remaining %d", reconTotal, remaining)
	}
}

// TestFundReportNoFundEmpty: with no fund chosen (Fund == 0) the report renders just the
// header (the framework's nothing-to-show rule), so a bare hit is a clean 200.
func TestFundReportNoFundEmpty(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := fundReportReport(t)
	p := reports.Params{Scope: reports.SubsidiaryID(f.IDs.Root), Lang: "en"} // Fund 0
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(table.Rows) != 0 {
		t.Errorf("no-fund report has %d rows, want 0 (header only)", len(table.Rows))
	}
	if len(table.Columns) != 6 {
		t.Errorf("no-fund report columns = %d, want 6", len(table.Columns))
	}
}
