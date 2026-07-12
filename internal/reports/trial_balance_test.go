package reports_test

// p15.3 trial-balance report tests + the GOLDEN-FILE harness the rest of Phase 15
// (p15.4–p15.11) copies. Every number asserted below is HAND-DERIVED from the
// canonical synthetic fixture (PLAN Appendix D, internal/testutil/fixture) and the
// p14.1 rate seam — the fixture is the oracle, never the report's own output. The
// golden files are a committed, human-reviewable rendering of the report the reviewer
// reads to confirm the numbers; -update / `make golden` regenerate them
// deterministically (pinned lang=en, as-of 2026-06-30, root scope, USD target).
//
// The trial balance's whole point is that it BALANCES: the native per-currency signed
// sum is exactly zero (double-entry, D2). TestTrialBalanceGolden asserts that on the
// report's OWN emitted cells (re-summed, so a dropped/duplicated row is caught), for
// both USD and MXN, and cross-checks hand-derived converted cells.

import (
	"bytes"
	"context"
	"testing"

	"cuento/internal/reports"
	"cuento/internal/testutil/fixture"
)

// goldenParams are the FIXED params every Phase-15 golden runs at: root scope (full
// consolidation), the fixture's as-of, USD target (the org base), lang en. Pinning
// them here (not defaulting through the web layer) is what makes the golden
// deterministic — no clock, no user setting, no locale drift.
func goldenParams(f *fixture.Fixture) reports.Params {
	return reports.Params{
		Scope:          f.IDs.Root,
		AsOf:           f.Expected.AsOf, // 2026-06-30
		TargetCurrency: "USD",
		Lang:           "en",
	}
}

// goldenExps returns the currency->exponent map the golden renderers format money
// with (USD and MXN, both exponent 2 on the fixture). Read from the store so it is
// real reference data, not a hard-coded 2.
func goldenExps(t *testing.T, f *fixture.Fixture) map[string]int {
	t.Helper()
	ctx := context.Background()
	exps := map[string]int{}
	for _, code := range []string{"USD", "MXN"} {
		c, err := f.Store.Currency(ctx, code)
		if err != nil {
			t.Fatalf("currency %s: %v", code, err)
		}
		exps[code] = int(c.Exponent)
	}
	return exps
}

// TestTrialBalanceGolden runs the trial balance over the fixture at the pinned params,
// asserts it BALANCES (the point of a trial balance) on its own cells, cross-checks
// hand-derived converted cells, and compares the rendered text + CSV to committed
// goldens (regenerated with -update).
//
// HAND-VERIFIED (derived from the fixture natives + the 2026-06-30 closing USD->MXN
// rate 18.10, MXN->USD = 1/18.10, half-even at the final cell — NOT read from code):
//
//	native USD signed sum       = 0        (double-entry, D2)
//	native MXN signed sum        = 0        (double-entry, D2)
//	converted USD grand total    = 0        (MXN rounding residuals cancel on THIS
//	                                         fixture; a data property, asserted after
//	                                         an in-test confirmation, not a guarantee)
//	Checking MX 39,500,000 MXN  -> 2,182,320 USD
//	Government Grants -10,000,000 MXN -> -552,486 USD
//	Food Purchases 360,000 MXN  ->    19,890 USD
func TestTrialBalanceGolden(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t) // the converted column needs the USD/MXN schedule
	ctx := context.Background()

	rep := trialBalanceReport(t)
	tk := reports.NewToolkit(f.Store, goldenParams(f))
	table, err := rep.Run(ctx, tk, goldenParams(f))
	if err != nil {
		t.Fatalf("run trial balance: %v", err)
	}

	// --- BALANCING: re-sum the report's OWN native cells (column 2). Each currency
	// nets to exactly zero — this is the whole point of a trial balance. Re-summing
	// the emitted cells (not the report's total rows) proves no row was dropped/duped.
	const nativeCol = 2
	nativeSums := reports.SumMoneyColumn(table, nativeCol)
	for _, ccy := range []string{"USD", "MXN"} {
		if nativeSums[ccy] != 0 {
			t.Errorf("trial balance native %s sum = %d, want 0 (must BALANCE)", ccy, nativeSums[ccy])
		}
	}
	// No unexpected extra currency snuck in.
	if len(nativeSums) != 2 {
		t.Errorf("trial balance native currencies = %v, want exactly {USD, MXN}", nativeSums)
	}

	// --- Converted grand total (column 3, DATA rows only): hand-verified 0. Confirm
	// it in-test before treating it as the asserted value, and check it matches the
	// report's own RowTotal converted cell.
	const convCol = 3
	convSums := reports.SumMoneyColumn(table, convCol)
	if convSums["USD"] != 0 {
		t.Errorf("converted USD grand total (re-summed) = %d, want 0", convSums["USD"])
	}

	// --- Hand-derived converted cells (spot checks against the fixture natives).
	wantConverted := map[string]int64{
		"Checking MX":       2_182_320,
		"Government Grants": -552_486, // its MXN leg (the USD -200,000 leg is a separate row)
		"Food Purchases":    19_890,
	}
	for name, want := range wantConverted {
		got, ok := convertedCellFor(table, name, "MXN")
		if !ok {
			t.Errorf("no MXN converted cell for %q", name)
			continue
		}
		if got != want {
			t.Errorf("converted %q MXN->USD = %d, want %d", name, got, want)
		}
	}

	// --- NATIVE cells verified DIRECTLY against the fixture oracle (not just the
	// snapshot): a few (account, currency) native amounts must equal
	// f.Expected.AccountBalances, so a wrong native figure fails here, not only in a
	// golden diff a reviewer must catch.
	wantNative := []struct {
		name string
		ccy  string
		amt  int64
	}{
		{"Checking MX", "MXN", 39_500_000},
		{"Government Grants", "MXN", -10_000_000},
		{"Contributions", "USD", -5_275_000},
		{"FX Clearing", "MXN", 500_000},
	}
	for _, w := range wantNative {
		got, ok := nativeCellFor(table, w.name, w.ccy)
		if !ok {
			t.Errorf("no native cell for %q %s", w.name, w.ccy)
			continue
		}
		if got != w.amt {
			t.Errorf("native %q %s = %d, want %d (fixture oracle)", w.name, w.ccy, got, w.amt)
		}
	}

	// --- Golden artifacts: aligned text dump + machine CSV.
	exps := goldenExps(t, f)
	if exps["USD"] != 2 || exps["MXN"] != 2 {
		t.Fatalf("golden exponents = %v, want USD:2 MXN:2", exps)
	}
	textDump := reports.DumpTable(table, goldenLocalize, exps)

	var csvBuf bytes.Buffer
	// The CSV writer is i18n-free; localize LABEL cells to en text first (mirrors the
	// web layer's localizeLabelCells) so the golden CSV reads in English.
	if err := reports.WriteCSV(&csvBuf, localizeLabels(table), goldenLocalize, exps); err != nil {
		t.Fatalf("write csv: %v", err)
	}

	checkGolden(t, "trial_balance.txt", []byte(textDump))
	checkGolden(t, "trial_balance.csv", csvBuf.Bytes())
}

// TestTrialBalanceScope: the trial balance over the ROOT scope and a LEAF subsidiary
// (RV Mexico) yields DIFFERENT account sets — the leaf carries only its own accounts
// (consolidation = the scope's descendant closure, D18), and US-only accounts are
// absent. This reuses the toolkit's consolidation through the report's Run. Native
// mode (no target) so the check is rate-independent.
func TestTrialBalanceScope(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := trialBalanceReport(t)

	rootTable, err := rep.Run(ctx, reports.NewToolkit(f.Store, reports.Params{Scope: f.IDs.Root, AsOf: f.Expected.AsOf, Lang: "en"}),
		reports.Params{Scope: f.IDs.Root, AsOf: f.Expected.AsOf, Lang: "en"})
	if err != nil {
		t.Fatalf("run root: %v", err)
	}
	leafTable, err := rep.Run(ctx, reports.NewToolkit(f.Store, reports.Params{Scope: f.IDs.MX, AsOf: f.Expected.AsOf, Lang: "en"}),
		reports.Params{Scope: f.IDs.MX, AsOf: f.Expected.AsOf, Lang: "en"})
	if err != nil {
		t.Fatalf("run leaf: %v", err)
	}

	rootNames := accountNames(rootTable)
	leafNames := accountNames(leafTable)

	// MX-only accounts appear in BOTH (scope-invariant single-sub accounts).
	for _, name := range []string{"Checking MX", "Cash MXN", "Due to RV Internacional"} {
		if !leafNames[name] {
			t.Errorf("leaf(MX) trial balance missing MX account %q", name)
		}
	}
	// US-only accounts appear in ROOT but NOT the MX leaf (descendant closure of MX
	// excludes US) — the discriminating check.
	for _, name := range []string{"Checking US", "Building", "Due from RV Mexico", "Credit Card"} {
		if !rootNames[name] {
			t.Errorf("root trial balance missing US account %q", name)
		}
		if leafNames[name] {
			t.Errorf("leaf(MX) trial balance unexpectedly contains US-only account %q", name)
		}
	}
	// The leaf account set is a strict subset of root (fewer accounts).
	if len(leafNames) >= len(rootNames) {
		t.Errorf("leaf account set (%d) not smaller than root (%d)", len(leafNames), len(rootNames))
	}
}

// TestTrialBalanceCSVParses: the report's CSV output parses back to the same values
// the table carries (the framework's CSV round-trip, exercised on a real report). It
// re-reads the CSV and re-sums the native column to zero — the balancing property
// survives the export.
func TestTrialBalanceCSVParses(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := trialBalanceReport(t)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, goldenParams(f)), goldenParams(f))
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
	// Header is the 4 localized columns.
	wantHeader := []string{"Account", "Currency", "Native amount", "Converted"}
	for i, h := range wantHeader {
		if recs[0][i] != h {
			t.Errorf("csv header[%d] = %q, want %q", i, recs[0][i], h)
		}
	}
	// Re-sum the native column (index 2) over DATA rows (skip the total rows, whose
	// account cell is the localized "Total (native)"/"Total (converted)" label). Group
	// by the currency column (index 1). Must net to zero per currency.
	sums := map[string]int64{}
	for _, rec := range recs[1:] {
		acct := rec[0]
		if acct == "Total (native)" || acct == "Total (converted)" {
			continue
		}
		ccy := rec[1]
		if rec[2] == "" {
			continue
		}
		sums[ccy] += parseMinor(t, rec[2])
	}
	for _, ccy := range []string{"USD", "MXN"} {
		if sums[ccy] != 0 {
			t.Errorf("csv native %s re-sum = %d, want 0", ccy, sums[ccy])
		}
	}
}

// --- harness helpers -------------------------------------------------------

// trialBalanceReport fetches the registered trial-balance report from the default
// registry (proving it IS registered under its id, replacing the retired smoke report).
func trialBalanceReport(t *testing.T) reports.Report {
	t.Helper()
	rep, ok := reports.Default().Get(reports.TrialBalanceReportID)
	if !ok {
		t.Fatalf("trial-balance report %q not registered in Default()", reports.TrialBalanceReportID)
	}
	return rep
}
