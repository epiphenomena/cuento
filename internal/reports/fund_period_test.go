package reports_test

import (
	"bytes"
	"context"
	"testing"

	"cuento/internal/reports"
	"cuento/internal/testutil/fixture"
)

// fundPeriodReport fetches the registered fund-activity-by-period report from Default().
func fundPeriodReport(t *testing.T) reports.Report {
	t.Helper()
	rep, ok := reports.Default().Get(reports.FundPeriodReportID)
	if !ok {
		t.Fatalf("fund-period report %q not registered in Default()", reports.FundPeriodReportID)
	}
	return rep
}

// TestFundPeriodMatrixFootsAndGolden runs Beca Agua's account x period matrix at QUARTER
// granularity and asserts the two invariants that make it a matrix: every row's period
// columns SUM to its Total column (footing, mirroring the income-statement footing test),
// and each row is one (account, currency) the fund touches. Golden: fund_period.{txt,csv}.
//
// Beca Agua is multi-currency (MXN + USD) and cash-only, so it exercises the per-currency
// rows and the period bucketing. No date range is passed -- the report spans all data via
// LedgerDateRange -- so the granularity is the only comparative control.
func TestFundPeriodMatrixFootsAndGolden(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := fundPeriodReport(t)

	p := reports.Params{
		Scope:       reports.SubsidiaryID(f.IDs.Root),
		Fund:        reports.FundID(f.IDs.BecaAgua),
		Granularity: reports.GranQuarter,
		Lang:        "en",
	}
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run fund period: %v", err)
	}

	// Columns: Account, Currency, one per quarter, then Total (>= 4 columns, and the last
	// column is the Total). The first two are the row labels; the middle are period
	// columns; the last is Total.
	if got := len(table.Columns); got < 4 {
		t.Fatalf("columns = %d, want >= 4 (Account, Currency, >=1 period, Total)", got)
	}
	if k := table.Columns[0].HeaderKey; k != "reports.fund_period.col.account" {
		t.Errorf("column[0] = %q, want the Account header", k)
	}
	if k := table.Columns[len(table.Columns)-1].HeaderKey; k != "reports.fund_period.col.total" {
		t.Errorf("last column = %q, want the Total header", k)
	}
	nperiods := len(table.Columns) - 3 // minus Account, Currency, Total
	if nperiods < 1 {
		t.Fatalf("period columns = %d, want >= 1", nperiods)
	}

	// FOOTING: for every data row, the sum of the period-column cells equals the Total
	// cell (int64 addition, native currency -- exact). Also confirm each row carries a
	// (account, currency) label and at least one row exists.
	var dataRows int
	for _, row := range table.Rows {
		if row.Kind != reports.RowData {
			continue
		}
		dataRows++
		if len(row.Cells) != len(table.Columns) {
			t.Fatalf("row has %d cells, want %d (columns)", len(row.Cells), len(table.Columns))
		}
		if row.Cells[0].Kind != reports.CellText || row.Cells[0].Text == "" {
			t.Errorf("row account cell is not a non-empty TEXT: %+v", row.Cells[0])
		}
		if row.Cells[1].Text == "" {
			t.Errorf("row currency cell is empty")
		}
		var sum int64
		for i := 2; i < 2+nperiods; i++ {
			sum += row.Cells[i].Minor
		}
		totalCell := row.Cells[len(row.Cells)-1]
		if sum != totalCell.Minor {
			t.Errorf("row %q %s: period columns sum to %d, want Total %d (matrix must foot)",
				row.Cells[0].Text, row.Cells[1].Text, sum, totalCell.Minor)
		}
	}
	if dataRows == 0 {
		t.Fatalf("no data rows — the fund touches at least one account")
	}

	// Beca Agua's Received cash (Checking) reconciles to the fixture oracle: its whole-
	// window Total per currency equals the fund's closing cash balance (cash-only fund).
	// MXN 9,700,000 / USD 50,000 are the fixture's FundBalances for Beca Agua.
	assertRowTotal(t, table, "MXN", 9_700_000)
	assertRowTotal(t, table, "USD", 50_000)

	exps := goldenExps(t, f)
	textDump := reports.DumpTable(table, goldenLocalize, exps)
	var csvBuf bytes.Buffer
	if err := reports.WriteCSV(&csvBuf, localizeLabels(table), goldenLocalize, exps); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	checkGolden(t, "fund_period.txt", []byte(textDump))
	checkGolden(t, "fund_period.csv", csvBuf.Bytes())
}

// assertRowTotal checks that SOME (account, currency) row's whole-window Total equals
// want for the given currency -- the fund's cash-account row carries its closing cash
// balance (the fixture oracle), since a cash-only fund's cash inflow net of outflow is
// exactly that balance. This reconciles the matrix's Total column to the fixture.
func assertRowTotal(t *testing.T, table reports.Table, ccy string, want int64) {
	t.Helper()
	var found bool
	for _, row := range table.Rows {
		if row.Kind != reports.RowData || row.Cells[1].Text != ccy {
			continue
		}
		tot := row.Cells[len(row.Cells)-1].Minor
		if tot == want {
			found = true
		}
	}
	if !found {
		t.Errorf("no %s row with Total == %d (the fund's cash balance) in the matrix", ccy, want)
	}
}

// TestFundPeriodTotalGranularitySingleColumn: GranNone collapses the matrix to a single
// Total column (no period breakdown), so the report reads as a plain per-account total.
func TestFundPeriodTotalGranularitySingleColumn(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := fundPeriodReport(t)
	p := reports.Params{
		Scope:       reports.SubsidiaryID(f.IDs.Root),
		Fund:        reports.FundID(f.IDs.BecaAgua),
		Granularity: reports.GranNone,
		Lang:        "en",
	}
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// GranNone collapses to a SINGLE Total column: Account, Currency, Total = 3 columns
	// (no redundant per-period column).
	if got := len(table.Columns); got != 3 {
		t.Errorf("GranNone columns = %d, want 3 (Account, Currency, Total)", got)
	}
	if k := table.Columns[len(table.Columns)-1].HeaderKey; k != "reports.fund_period.col.total" {
		t.Errorf("GranNone last column = %q, want the Total header", k)
	}
	// Every data row carries its whole-window Total in the last cell (col 2).
	for _, row := range table.Rows {
		if row.Kind != reports.RowData {
			continue
		}
		if len(row.Cells) != 3 {
			t.Errorf("GranNone row has %d cells, want 3", len(row.Cells))
		}
	}
}

// TestFundPeriodNoFundEmpty: with no fund chosen the report renders just the header.
func TestFundPeriodNoFundEmpty(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := fundPeriodReport(t)
	p := reports.Params{Scope: reports.SubsidiaryID(f.IDs.Root), Lang: "en"} // Fund 0
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(table.Rows) != 0 {
		t.Errorf("no-fund matrix has %d rows, want 0 (header only)", len(table.Rows))
	}
}
