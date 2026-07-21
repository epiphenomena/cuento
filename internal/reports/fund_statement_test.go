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
// asserts its SHAPE by hand: it is a COLLAPSIBLE ACCOUNT TREE (Report.Tree true) -- the
// account dimension is the chart-of-accounts hierarchy, placeholder parents and leaf
// account headers as nested RowSubtotal rows (a parent's Indent is shallower than its
// subtree), each leaf's detail lines and per-currency subtotal one level DEEPER than the
// leaf header. It carries the Description and Memo columns per line, and each leaf's
// subtotal FOOTS to the sum of that account's line amounts (per currency). Golden:
// fund_statement.{txt,csv}.
//
// The Building Fund is single-currency (USD); its three splits live on THREE LEAF accounts
// (Contributions, Checking US, Building), so the statement has three leaf account headers
// (plus their placeholder ancestors in the tree).
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

	// TREE shape: the report is flagged Tree so the generic template + treetable.js wire
	// click-to-collapse from each row's Indent (data-depth). Report registration must say so.
	if !rep.Tree {
		t.Errorf("fund statement Report.Tree = false, want true (collapsible account tree)")
	}

	// Walk the rows verifying the collapsible account TREE. Every RowSubtotal is a header
	// (placeholder parent OR leaf account): first cell a non-empty TEXT account name. A
	// LEAF header is one whose NEXT row is deeper (a detail line at header.Indent+1); its
	// detail RowData lines follow (Description at col 1, Memo at col 2), then a
	// RowSectionTotal per currency that MUST foot to the leaf's line sum. Pre-order tree
	// invariant: a header's Indent is <= the following rows in its subtree, and a leaf's
	// detail sits exactly one level deeper.
	var leafHeaders int
	lineSum := map[string]int64{}
	curLeafIndent := -1 // indent of the leaf header we are inside (-1 = not in a leaf)
	for i, row := range table.Rows {
		switch row.Kind {
		case reports.RowSubtotal:
			// Any header row: non-empty TEXT account/placeholder name.
			if len(row.Cells) == 0 || row.Cells[0].Kind != reports.CellText || row.Cells[0].Text == "" {
				t.Errorf("header row has no TEXT account name: %+v", row.Cells)
			}
			// A LEAF account header is a parent whose next row is DEEPER detail (a RowData
			// line or RowSectionTotal), as opposed to a PLACEHOLDER parent whose next row is
			// another (deeper) RowSubtotal header.
			isLeafHeader := i+1 < len(table.Rows) &&
				table.Rows[i+1].Indent > row.Indent &&
				table.Rows[i+1].Kind != reports.RowSubtotal
			if isLeafHeader {
				leafHeaders++
				curLeafIndent = row.Indent
				lineSum = map[string]int64{}
			}
		case reports.RowData:
			if curLeafIndent < 0 {
				t.Errorf("detail line outside a leaf account section")
			}
			if row.Indent != curLeafIndent+1 {
				t.Errorf("detail line Indent = %d, want leaf header Indent+1 (%d)", row.Indent, curLeafIndent+1)
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
			// Leaf subtotal (one per currency): its Amount (col 4) must foot to the sum of
			// the leaf's line amounts in that currency, and sit at the detail depth.
			if row.Indent != curLeafIndent+1 {
				t.Errorf("leaf subtotal Indent = %d, want leaf header Indent+1 (%d)", row.Indent, curLeafIndent+1)
			}
			ccy := row.Cells[3].Text
			if got, want := row.Cells[4].Minor, lineSum[ccy]; got != want {
				t.Errorf("account subtotal %s = %d, want line sum %d (subtotal must foot)", ccy, got, want)
			}
		}
	}
	if leafHeaders == 0 {
		t.Fatalf("no leaf account headers found — report is not an account tree")
	}
	// The Building Fund touches exactly three LEAF accounts (Contributions, Checking, Building).
	if leafHeaders != 3 {
		t.Errorf("leaf account headers = %d, want 3 (Contributions, Checking US, Building)", leafHeaders)
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
