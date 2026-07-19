package reports_test

// p31 FX conversion-details report tests. Every asserted number is the HAND-COMPUTED
// oracle the ExtendFX seam records (FXExpected), derived independently of the report's
// own code from the deterministic HNL rate schedule -- so the report is validated
// against a figure it did not itself produce. The golden files
// (testdata/fx_detail.{txt,csv}) are a committed, human-reviewable rendering;
// -update / `make golden` regenerate them deterministically (lang=en, as-of 2026-06-30,
// root scope, USD target).
//
// The scenario (ExtendFX): a Banco Lempira HNL bank in the USD-functional US sub holds
// a 150,000.00 HNL residual monetary balance whose closing-rate value (583,658 USD
// minor) differs from its transaction-date basis, recognizing a 461.74 USD FX LOSS in
// income. It is the ONLY item: the intercompany USD payable is correctly excluded (its
// FX effect is a translation adjustment routed to CTA, not income).

import (
	"bytes"
	"context"
	"testing"

	"cuento/internal/reports"
	"cuento/internal/testutil/fixture"
)

// fxGoldenParams: root scope, fixture as-of, USD target, lang en.
func fxGoldenParams(f *fixture.Fixture) reports.Params {
	return reports.Params{
		Scope:          reports.SubsidiaryID(f.IDs.Root),
		AsOf:           f.Expected.AsOf, // 2026-06-30
		TargetCurrency: "USD",
		Lang:           "en",
	}
}

// fxDetailReport fetches the registered FX conversion-details report from Default().
func fxDetailReport(t *testing.T) reports.Report {
	t.Helper()
	rep, ok := reports.Default().Get(reports.FXDetailReportID)
	if !ok {
		t.Fatalf("FX-detail report %q not registered in Default()", reports.FXDetailReportID)
	}
	return rep
}

// fxLabelAmount returns the gain/(loss)-column (col 5) minor amount for the row whose
// first cell is a LABEL matching key, and whether found -- used for the section/grand
// total rows.
func fxLabelAmount(t reports.Table, key string) (int64, bool) {
	for _, row := range t.Rows {
		if len(row.Cells) < 6 {
			continue
		}
		c := row.Cells[0]
		if c.Kind == reports.CellLabel && c.Text == key {
			return row.Cells[5].Minor, true
		}
	}
	return 0, false
}

// TestFXDetailGolden runs the FX-detail report over the FX-seamed fixture at the pinned
// params, hand-verifies the single Banco Lempira item's cells against the independent
// oracle (native / remeasured / gain-loss), confirms it is the ONLY item (the
// intercompany USD payable is excluded), checks the section + grand totals equal the
// recognized income figure, and compares the rendered text + CSV to committed goldens.
func TestFXDetailGolden(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t) // MXN schedule
	f.ExtendFX(t)    // HNL schedule + the Lempira monetary item
	ctx := context.Background()

	rep := fxDetailReport(t)
	tk := reports.NewToolkit(f.Store, fxGoldenParams(f))
	table, err := rep.Run(ctx, tk, fxGoldenParams(f))
	if err != nil {
		t.Fatalf("run fx detail: %v", err)
	}

	// --- The account-name resolution: the Banco Lempira row must be present. Its name is
	// the fully-qualified per-lang path; find it by the leaf-name suffix rather than
	// pinning the whole path (the fixture's chart structure is not this test's oracle).
	var bankRow reports.Row
	var bankName string
	for _, row := range table.Rows {
		if row.Kind != reports.RowData || len(row.Cells) < 6 {
			continue
		}
		if row.Cells[0].Kind == reports.CellText && row.Cells[1].Text == "HNL" {
			bankRow = row
			bankName = row.Cells[0].Text
		}
	}
	if bankName == "" {
		t.Fatalf("no HNL data row (Banco Lempira) in the FX-detail table")
	}

	// --- The single item's cells vs the INDEPENDENT oracle (FXExpected, hand-computed).
	// Native (col 2) in HNL, remeasured-at-closing (col 4) in USD, FX gain/(loss) (col 5)
	// in USD. These equal the brief's literals 15_000_000 / 583_658 / -46_174.
	fx := f.Expected.FX
	if got := bankRow.Cells[2].Minor; got != fx.NativeHNL {
		t.Errorf("native HNL = %d, want %d (oracle NativeHNL)", got, fx.NativeHNL)
	}
	if bankRow.Cells[2].Currency != "HNL" {
		t.Errorf("native cell currency = %q, want HNL", bankRow.Cells[2].Currency)
	}
	if got := bankRow.Cells[4].Minor; got != fx.EndingUSDMinor {
		t.Errorf("remeasured USD = %d, want %d (oracle EndingUSDMinor)", got, fx.EndingUSDMinor)
	}
	if got := bankRow.Cells[5].Minor; got != fx.RemeasurementUSDMinor {
		t.Errorf("FX gain/(loss) USD = %d, want %d (oracle RemeasurementUSDMinor, a $%.2f loss)",
			got, fx.RemeasurementUSDMinor, float64(-fx.RemeasurementUSDMinor)/100)
	}
	// The gain/loss and remeasured columns are in the functional currency (USD).
	if bankRow.Cells[4].Currency != "USD" || bankRow.Cells[5].Currency != "USD" {
		t.Errorf("functional-currency cells = %q/%q, want USD/USD", bankRow.Cells[4].Currency, bankRow.Cells[5].Currency)
	}

	// Spell out the brief's exact literals (independent of the oracle struct) so a fixture
	// drift is visible here, not only in a golden diff.
	if bankRow.Cells[2].Minor != 15_000_000 || bankRow.Cells[4].Minor != 583_658 || bankRow.Cells[5].Minor != -46_174 {
		t.Errorf("Banco Lempira cells = native %d / remeasured %d / gain-loss %d, want 15,000,000 / 583,658 / -46,174",
			bankRow.Cells[2].Minor, bankRow.Cells[4].Minor, bankRow.Cells[5].Minor)
	}

	// --- It is the ONLY foreign-currency monetary item (the intercompany USD payable is
	// correctly excluded from the income path). Count DATA rows carrying a native money
	// cell (skip section headers -- their native column is blank).
	var itemRows int
	for _, row := range table.Rows {
		if row.Kind == reports.RowData && len(row.Cells) >= 3 &&
			row.Cells[2].Kind == reports.CellMoney && !row.Cells[2].Blank {
			itemRows++
		}
	}
	if itemRows != 1 {
		t.Errorf("FX-detail item rows = %d, want exactly 1 (the intercompany payable must be excluded)", itemRows)
	}

	// --- Section total (the US sub) and grand total (recognized in income) both equal the
	// single item's remeasurement loss.
	sect, ok := fxLabelAmount(table, "reports.fx_detail.total.subsidiary")
	if !ok {
		t.Fatalf("no section-total row")
	}
	if sect != fx.RemeasurementUSDMinor {
		t.Errorf("section total = %d, want %d", sect, fx.RemeasurementUSDMinor)
	}
	recognized, ok := fxLabelAmount(table, "reports.fx_detail.total.recognized")
	if !ok {
		t.Fatalf("no recognized-total row")
	}
	if recognized != fx.RemeasurementUSDMinor {
		t.Errorf("recognized-in-income total = %d, want %d", recognized, fx.RemeasurementUSDMinor)
	}

	// --- Golden artifacts.
	exps := goldenExps(t, f)
	exps["HNL"] = fxHNLExponent(t, f) // the FX report emits an HNL native column
	textDump := reports.DumpTable(table, goldenLocalize, exps)
	var csvBuf bytes.Buffer
	if err := reports.WriteCSV(&csvBuf, localizeLabels(table), goldenLocalize, exps); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	checkGolden(t, "fx_detail.txt", []byte(textDump))
	checkGolden(t, "fx_detail.csv", csvBuf.Bytes())
}

// fxHNLExponent returns HNL's minor-unit exponent for money formatting (goldenExps only
// loads USD/MXN; the FX report also emits an HNL native column).
func fxHNLExponent(t *testing.T, f *fixture.Fixture) int {
	t.Helper()
	c, err := f.Store.Currency(context.Background(), "HNL")
	if err != nil {
		t.Fatalf("currency HNL: %v", err)
	}
	return int(c.Exponent)
}

// TestFXDetailEmpty: WITHOUT the ExtendFX seam the base fixture carries no foreign-
// currency monetary balance in the income path (the MXN balances are held by the MXN-
// functional MX sub, so they have no FX exposure, and the intercompany USD is excluded),
// so the report is NOT an error -- it emits a single note row (the empty-state label), no
// item rows, and no total rows. ExtendRates is loaded so the toolkit's historical-basis
// conversion has a rate schedule (the compute reads dated rates even to reach an empty
// item set).
func TestFXDetailEmpty(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := fxDetailReport(t)

	p := reports.Params{Scope: reports.SubsidiaryID(f.IDs.Root), AsOf: f.Expected.AsOf, TargetCurrency: "USD", Lang: "en"}
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run fx detail (empty): %v", err)
	}

	if len(table.Rows) != 1 {
		t.Fatalf("empty FX-detail rows = %d, want exactly 1 (the note row)", len(table.Rows))
	}
	row := table.Rows[0]
	if row.Kind != reports.RowData || row.Cells[0].Kind != reports.CellLabel ||
		row.Cells[0].Text != "reports.fx_detail.empty" {
		t.Errorf("empty-state row = %+v, want a note label row (reports.fx_detail.empty)", row.Cells[0])
	}
	// No total rows on the empty report.
	for _, r := range table.Rows {
		if r.Kind == reports.RowSectionTotal || r.Kind == reports.RowTotal {
			t.Errorf("empty FX-detail emitted a total row: %+v", r)
		}
	}
}
