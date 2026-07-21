package reports_test

import (
	"bytes"
	"context"
	"testing"

	"cuento/internal/reports"
	"cuento/internal/testutil/fixture"
)

// fundStatementReport fetches the registered fund-statement report from Default().
func fundStatementReport(t *testing.T) reports.Report {
	t.Helper()
	rep, ok := reports.Default().Get(reports.FundStatementReportID)
	if !ok {
		t.Fatalf("fund-statement report %q not registered in Default()", reports.FundStatementReportID)
	}
	return rep
}

// TestFundStatementLineDetailGolden runs the Building Fund's all-time line statement and
// asserts its SHAPE by hand: it is grouped by ACCOUNT (a subtotal-kind header row per
// account, then that account's lines, then a section-total subtotal per currency), it
// carries the Description and Memo columns per line, and each account's subtotal FOOTS to
// the sum of that account's line amounts (per currency). Golden: fund_statement.{txt,csv}.
//
// The Building Fund is single-currency (USD); its three splits live on THREE accounts
// (Contributions, Checking US, Building), so the statement has three account sections.
func TestFundStatementLineDetailGolden(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := fundStatementReport(t)

	// No date range: the fund selector is the only param. The report spans all data.
	p := reports.Params{
		Scope: reports.SubsidiaryID(f.IDs.Root),
		Fund:  reports.FundID(f.IDs.BuildingFund),
		Lang:  "en",
	}
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run fund statement: %v", err)
	}

	// Columns: Date, Description, Memo, Currency, Amount, Balance (6). The Description and
	// Memo columns are the point of this report (per-line detail, not an aggregate).
	if got := len(table.Columns); got != 6 {
		t.Fatalf("columns = %d, want 6 (Date, Description, Memo, Currency, Amount, Balance)", got)
	}
	wantCols := []string{
		"reports.fund_statement.col.date",
		"reports.fund_statement.col.description",
		"reports.fund_statement.col.memo",
		"reports.fund_statement.col.currency",
		"reports.fund_statement.col.amount",
		"reports.fund_statement.col.balance",
	}
	for i, k := range wantCols {
		if table.Columns[i].HeaderKey != k {
			t.Errorf("column[%d] header = %q, want %q", i, table.Columns[i].HeaderKey, k)
		}
	}

	// Walk the rows verifying the by-account grouping: a header (RowSubtotal) whose first
	// cell is a TEXT account name opens each section; RowData lines follow (each with a
	// Description cell at col 1 and a Memo cell at col 2); a RowSectionTotal subtotal per
	// currency closes it and MUST equal the sum of that section's line amounts.
	var sections int
	var sawAccountHeader bool

	// Accumulate line sums per section, keyed by currency, and check each subtotal row.
	lineSum := map[string]int64{}
	inSection := false
	for _, row := range table.Rows {
		switch row.Kind {
		case reports.RowSubtotal:
			// Account section header: first cell is the account name (TEXT, non-empty).
			if len(row.Cells) == 0 || row.Cells[0].Kind != reports.CellText || row.Cells[0].Text == "" {
				t.Errorf("account header row has no TEXT account name: %+v", row.Cells)
			}
			sections++
			sawAccountHeader = true
			inSection = true
			lineSum = map[string]int64{}
		case reports.RowData:
			if !inSection {
				t.Errorf("data row outside an account section")
			}
			if len(row.Cells) < 6 {
				t.Fatalf("data row has %d cells, want 6", len(row.Cells))
			}
			// Description (col 1) and Memo (col 2) are TEXT cells (the per-line detail).
			if row.Cells[1].Kind != reports.CellText {
				t.Errorf("description cell is not TEXT: %+v", row.Cells[1])
			}
			if row.Cells[2].Kind != reports.CellText {
				t.Errorf("memo cell is not TEXT: %+v", row.Cells[2])
			}
			ccy := row.Cells[3].Text
			lineSum[ccy] += row.Cells[4].Minor
		case reports.RowSectionTotal:
			// Account subtotal (one per currency): its Amount (col 4) must foot to the sum
			// of the section's line amounts in that currency.
			ccy := row.Cells[3].Text
			if got, want := row.Cells[4].Minor, lineSum[ccy]; got != want {
				t.Errorf("account subtotal %s = %d, want line sum %d (subtotal must foot)", ccy, got, want)
			}
		}
	}
	if !sawAccountHeader {
		t.Fatalf("no account section headers found — report is not grouped by account")
	}
	// The Building Fund touches exactly three accounts (Contributions, Checking, Building).
	if sections != 3 {
		t.Errorf("account sections = %d, want 3 (Contributions, Checking US, Building)", sections)
	}

	exps := goldenExps(t, f)
	textDump := reports.DumpTable(table, goldenLocalize, exps)
	var csvBuf bytes.Buffer
	if err := reports.WriteCSV(&csvBuf, localizeLabels(table), goldenLocalize, exps); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	checkGolden(t, "fund_statement.txt", []byte(textDump))
	checkGolden(t, "fund_statement.csv", csvBuf.Bytes())
}

// TestFundStatementNoFundEmpty: with no fund chosen (Fund == 0) the report renders just
// the header (the framework's nothing-to-show rule), so a bare hit is a clean 200.
func TestFundStatementNoFundEmpty(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := fundStatementReport(t)
	p := reports.Params{Scope: reports.SubsidiaryID(f.IDs.Root), Lang: "en"} // Fund 0
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(table.Rows) != 0 {
		t.Errorf("no-fund statement has %d rows, want 0 (header only)", len(table.Rows))
	}
	if len(table.Columns) != 6 {
		t.Errorf("no-fund statement columns = %d, want 6", len(table.Columns))
	}
}
