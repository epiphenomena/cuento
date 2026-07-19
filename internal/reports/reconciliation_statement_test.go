package reports_test

// p16.4 reconciliation statement report tests. Every number is HAND-DERIVED from the
// synthetic fixture's FINALIZED 2026-05-31 Checking US (USD) reconciliation, which the
// ExtendReconciliation seam builds (opt-in). The seam's own expectations
// (f.Expected.Reconciliation) are the oracle:
//
//	statement balance = 3,673,500 USD
//	opening           = 0          (first finalized recon on Checking US / USD)
//	cleared total     = 3,673,500  (opening + cleared == statement balance, the Z9 gate)
//	closing           = opening + cleared = 3,673,500 == statement balance ✓
//	included splits   = 17 cleared splits (ClearedCount); the two UNCLEARED txns
//	                    (MayRentTxn, JuneDonationTxn) are ABSENT.
//
// The report is NOT scoped and NOT converted: a reconciliation spans all funds AND
// subsidiaries (D13/D20), so the included set is identified by the recon id alone.

import (
	"bytes"
	"context"
	"testing"

	"cuento/internal/reports"
	"cuento/internal/testutil/fixture"
)

// reconStatementReport fetches the registered reconciliation-statement report from the
// default registry (proving it IS registered under its id, in the "reconciliation"
// group).
func reconStatementReport(t *testing.T) reports.Report {
	t.Helper()
	rep, ok := reports.Default().Get(reports.ReconciliationStatementReportID)
	if !ok {
		t.Fatalf("reconciliation-statement report %q not registered in Default()", reports.ReconciliationStatementReportID)
	}
	if rep.Group != "reconciliation" {
		t.Fatalf("report group = %q, want %q", rep.Group, "reconciliation")
	}
	return rep
}

// TestReconciliationStatementGolden runs the statement report over the fixture's
// finalized 2026-05-31 Checking US recon, asserts the opening/cleared/closing chain
// re-derived from the report's OWN emitted cells (closing == statement balance), the
// included-split count (== ClearedCount, the two uncleared txns absent), and compares
// the rendered text + CSV to committed goldens.
func TestReconciliationStatementGolden(t *testing.T) {
	f := fixture.New(t)
	f.ExtendReconciliation(t) // OPT-IN seam: builds the finalized recon.
	ctx := context.Background()

	rep := reconStatementReport(t)
	recon := f.Expected.Reconciliation
	p := reports.Params{Scope: reports.SubsidiaryID(f.IDs.Root), Reconciliation: recon.ID, Lang: "en"}
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run reconciliation statement: %v", err)
	}

	// --- The chain, re-derived from the report's OWN emitted DATA rows (the included
	// cleared splits): Σ(split amounts) must equal the cleared total, and opening(0) +
	// that Σ must equal the statement balance. Summing the emitted cells (not trusting a
	// single figure) catches a dropped/duplicated line.
	lineSum := reports.SumMoneyColumn(table, 3)["USD"]
	if lineSum != recon.StatementBalance {
		t.Errorf("Σ(included split amounts) = %d, want %d (== statement balance, opening 0)",
			lineSum, recon.StatementBalance)
	}
	if recon.Opening != 0 {
		t.Fatalf("fixture opening = %d, want 0 (test assumes the first finalized recon)", recon.Opening)
	}
	// opening(0) + cleared == closing == statement balance (3,673,500).
	if recon.Opening+lineSum != recon.StatementBalance {
		t.Errorf("chain broken: opening %d + Σ %d = %d, want statement balance %d",
			recon.Opening, lineSum, recon.Opening+lineSum, recon.StatementBalance)
	}
	if recon.StatementBalance != 3_673_500 {
		t.Errorf("statement balance = %d, want 3,673,500", recon.StatementBalance)
	}

	// --- Exactly ClearedCount (17) included split DATA rows, and the two UNCLEARED
	// transactions are absent.
	dataRows := statementDataRows(table)
	if len(dataRows) != recon.ClearedCount {
		t.Errorf("included split lines = %d, want %d (ClearedCount)", len(dataRows), recon.ClearedCount)
	}
	if recon.ClearedCount != 17 {
		t.Errorf("ClearedCount = %d, want 17", recon.ClearedCount)
	}

	// --- The report's own CLOSING row equals the statement balance (the asserted chain).
	closing := statementClosing(t, table, "USD")
	if closing != recon.StatementBalance {
		t.Errorf("report closing = %d, want statement balance %d", closing, recon.StatementBalance)
	}

	// --- Golden artifacts: aligned text dump + machine CSV.
	exps := goldenExps(t, f)
	textDump := reports.DumpTable(table, goldenLocalize, exps)

	var csvBuf bytes.Buffer
	if err := reports.WriteCSV(&csvBuf, localizeLabels(table), goldenLocalize, exps); err != nil {
		t.Fatalf("write csv: %v", err)
	}

	checkGolden(t, "reconciliation_statement.txt", []byte(textDump))
	checkGolden(t, "reconciliation_statement.csv", csvBuf.Bytes())
}

// TestReconciliationStatementNoRecon: with no reconciliation chosen (Reconciliation ==
// 0) the report returns an empty Table (the framework's nothing-to-show rule) so a bare
// hit renders 200 with just the params form.
func TestReconciliationStatementNoRecon(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := reconStatementReport(t)

	p := reports.Params{Scope: reports.SubsidiaryID(f.IDs.Root), Lang: "en"} // Reconciliation == 0
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run with no recon: %v", err)
	}
	if len(table.Rows) != 0 {
		t.Errorf("no-recon table has %d rows, want 0 (nothing-to-show)", len(table.Rows))
	}
	if len(table.Columns) == 0 {
		t.Errorf("no-recon table has no columns; the form still needs the header row")
	}
}

// statementDataRows returns the report's DATA rows (the included cleared split lines,
// skipping the info/opening/cleared-total/closing subtotal+total rows).
func statementDataRows(table reports.Table) []reports.Row {
	var out []reports.Row
	for _, r := range table.Rows {
		if r.Kind == reports.RowData {
			out = append(out, r)
		}
	}
	return out
}

// statementClosing returns the amount (col 3) of the report's grand-TOTAL (closing) row
// for the given currency.
func statementClosing(t *testing.T, table reports.Table, ccy string) int64 {
	t.Helper()
	for _, r := range table.Rows {
		if r.Kind != reports.RowTotal || len(r.Cells) < 4 {
			continue
		}
		c := r.Cells[3]
		if c.Kind == reports.CellMoney && !c.Blank && c.Currency == ccy {
			return c.Minor
		}
	}
	t.Fatalf("no closing (total) row found for currency %s", ccy)
	return 0
}
